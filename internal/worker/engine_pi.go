package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"attesttag/internal/app"
)

// piEngine drives the pi coding agent (@mariozechner/pi-coding-agent) headless: --mode json
// prints every session event as a JSON line. It is the smaller of the two engines — seven tools,
// a short system prompt, no sub-agents — and it compacts its own conversation once it nears the
// window it is given, which is what keeps a long job's turns from each re-sending everything.
//
// pi has no turn cap of its own, so the engine counts turns and stops it at the job's, and no
// spend cap, which the job's llmProxy enforces for every engine alike.
// Docs: https://github.com/badlogic/pi-mono/tree/main/packages/coding-agent/docs
type piEngine struct {
	bin       string
	llm       app.JobLLMSecret
	maxRounds int
}

func (e *piEngine) Name() string { return "pi" }

// piProvider is the provider name the job's model is configured under in models.json.
const piProvider = "attest"

// piTools are pi's built-in tools, all of which a fix job uses.
const piTools = "read,bash,edit,write,grep,find,ls"

// errTurnCap is why the engine stopped the agent: it used the job's turns.
var errTurnCap = errors.New("turn cap")

// writeConfig puts the job's model and settings where pi reads them: models.json names one
// OpenAI-compatible provider (the job's proxy) with the one model, and settings.json keeps
// compaction on against the window qwen is given too.
func (e *piEngine) writeConfig(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	models := map[string]any{"providers": map[string]any{piProvider: map[string]any{
		"baseUrl": e.llm.BaseURL, "api": "openai-completions", "apiKey": "PI_ATTEST_API_KEY",
		// The system prompt goes as a system message and no reasoning_effort is sent: neither
		// is understood by every model an OpenAI-compatible endpoint serves.
		"compat": map[string]any{"supportsDeveloperRole": false, "supportsReasoningEffort": false},
		"models": []map[string]any{{"id": e.llm.Model, "name": e.llm.Model, "contextWindow": qwenContextWindow, "maxTokens": 32_000}},
	}}}
	settings := map[string]any{
		"quietStartup": true,
		"compaction":   map[string]any{"enabled": true, "reserveTokens": 16_384, "keepRecentTokens": 24_000},
		"retry":        map[string]any{"enabled": true, "maxRetries": 3},
	}
	for name, v := range map[string]any{"models.json": models, "settings.json": settings} {
		raw, _ := json.MarshalIndent(v, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (e *piEngine) Run(ctx context.Context, ws *Workspace, b Brief) (EngineResult, error) {
	home := filepath.Join(ws.JobDir, "home")
	agentDir := filepath.Join(home, ".pi", "agent")
	if err := e.writeConfig(agentDir); err != nil {
		return EngineResult{Stopped: "error"}, stepErr("engine_error", "pi settings: "+err.Error())
	}
	stop := ws.EngineStop
	if stop.IsZero() {
		stop = ws.Deadline.Add(-2 * time.Minute)
	}
	remaining := time.Until(stop)
	if remaining < 2*time.Minute {
		return EngineResult{Stopped: "timeout"}, nil
	}
	briefPath := filepath.Join(ws.JobDir, "brief.md")
	os.WriteFile(briefPath, []byte(briefText(b)), 0o600)
	// The brief goes in as an attached file rather than an argument: it runs to tens of
	// kilobytes, and one argument is capped at 128 KB on Linux.
	argv := append(qwenArgv(e.bin), "--mode", "json", "--no-session", "--provider", piProvider, "--model", e.llm.Model,
		"--tools", piTools, "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files",
		"@"+briefPath, "Do the job the attached brief describes, then reply with the SUMMARY: it asks for.")
	env := append(append([]string{}, ws.Env...), "PI_CODING_AGENT_DIR="+agentDir, "PI_ATTEST_API_KEY="+e.llm.APIKey,
		"PI_SKIP_VERSION_CHECK=1", "PI_OFFLINE=1", "PI_TELEMETRY=0")
	rctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	st := &piStream{rep: ws.Reporter, maxRounds: e.maxRounds, capped: func() { cancel(errTurnCap) }}
	grantSandbox(home, briefPath)
	ws.Reporter.Log(fmt.Sprintf("pi on %s, up to %d turns, %s wall time", e.llm.Model, e.maxRounds, remaining.Truncate(time.Minute)))
	out, err := runCmd(rctx, cmdSpec{Dir: ws.RepoDir, Argv: argv, Env: env, Timeout: remaining + time.Minute, OnLine: st.line, Head: 8 << 10, Tail: 32 << 10, Sandbox: true})
	res := EngineResult{Summary: st.summaryText(), Usage: st.usage, Rounds: st.turns, LogTail: lastLines(ws.Scrub.Clean(out.Output), 32<<10)}
	switch {
	case ctx.Err() != nil:
		res.Stopped = "cancelled"
		if context.Cause(ctx) == errDeadline {
			res.Stopped = "timeout"
		}
	case errors.Is(context.Cause(rctx), errTurnCap):
		res.Stopped = "max_rounds"
	case out.TimedOut:
		res.Stopped = "timeout"
	case out.Code == 0 && st.failed == "":
		res.Stopped = "finished"
	default:
		res.Stopped = "error"
		msg := st.failed
		if msg == "" {
			msg = lastLines(ws.Scrub.Clean(out.Output), 600)
		}
		if err != nil && msg == "" {
			msg = err.Error()
		}
		if res.Summary == "" || st.failed != "" {
			res.Summary = fmt.Sprintf("pi exited with code %d: %s", out.Code, msg)
		}
	}
	if res.Summary == "" {
		res.Summary = "The engine finished without a summary (stopped: " + res.Stopped + ")."
	}
	if !ws.Metered {
		ws.Reporter.Usage(res.Usage)
	}
	return res, nil
}

// piStream reads pi's JSON lines: turn_start counts turns, tool_execution_start names tools,
// message_end of an assistant message carries its text and usage, and an assistant message that
// ended in error says why the run failed.
type piStream struct {
	rep       *Reporter
	maxRounds int
	capped    func()
	turns     int
	tools     int
	usage     app.JobUsage
	last      string
	failed    string
}

func (s *piStream) line(l string) {
	if !strings.HasPrefix(l, "{") {
		return
	}
	var ev struct {
		Type     string          `json:"type"`
		ToolName string          `json:"toolName"`
		Args     json.RawMessage `json:"args"`
		Message  *struct {
			Role       string            `json:"role"`
			Content    []json.RawMessage `json:"content"`
			StopReason string            `json:"stopReason"`
			Error      string            `json:"errorMessage"`
			Usage      *struct {
				Input      int `json:"input"`
				Output     int `json:"output"`
				CacheRead  int `json:"cacheRead"`
				CacheWrite int `json:"cacheWrite"`
				Cost       *struct {
					Total float64 `json:"total"`
				} `json:"cost"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal([]byte(l), &ev) != nil {
		return
	}
	switch ev.Type {
	case "turn_start":
		s.turns++
		if s.turns%5 == 0 {
			s.rep.Log(fmt.Sprintf("pi: turn %d of at most %d", s.turns, s.maxRounds))
		}
		if s.maxRounds > 0 && s.turns > s.maxRounds && s.capped != nil {
			s.capped()
		}
	case "tool_execution_start":
		s.tools++
		s.rep.Log("pi: " + strings.TrimSpace(ev.ToolName+" "+cut(string(ev.Args), 160)))
	case "message_end":
		m := ev.Message
		if m == nil || m.Role != "assistant" {
			return
		}
		if m.Usage != nil {
			// pi's input excludes what it read from cache; the job's In includes it, as
			// OpenAI-shaped usage does.
			s.usage.In += m.Usage.Input + m.Usage.CacheRead + m.Usage.CacheWrite
			s.usage.Cached += m.Usage.CacheRead
			s.usage.Out += m.Usage.Output
			if m.Usage.Cost != nil {
				s.usage.CostUSD += m.Usage.Cost.Total
			}
		}
		if m.StopReason == "error" || m.StopReason == "aborted" {
			s.failed = nonEmptyStr(m.Error, m.StopReason)
		}
		var parts []string
		for _, c := range m.Content {
			var p struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(c, &p) == nil && p.Type == "text" && strings.TrimSpace(p.Text) != "" {
				parts = append(parts, p.Text)
			}
		}
		if t := strings.Join(parts, "\n"); t != "" {
			s.last = t
		}
	}
}

func (s *piStream) summaryText() string {
	if i := strings.Index(s.last, "SUMMARY:"); i >= 0 {
		return strings.TrimSpace(s.last[i+len("SUMMARY:"):])
	}
	return strings.TrimSpace(s.last)
}
