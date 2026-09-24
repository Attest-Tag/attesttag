package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"attesttag/internal/app"
)

// qwenEngine runs Qwen Code (github.com/QwenLM/qwen-code) headlessly against the clone. It was
// chosen because its unattended mode carries turn, tool-call and wall-time caps with distinct
// exit codes, streams JSON, needs no sandbox, and talks to OpenRouter through plain OpenAI-style
// environment variables. It sees the model key and nothing else secret: the repository token is
// not in its environment, so a `git push` from inside it cannot authenticate.
// Docs: https://qwenlm.github.io/qwen-code-docs/en/users/features/headless/
type qwenEngine struct {
	bin       string
	llm       app.JobLLMSecret
	maxRounds int
}

func (e *qwenEngine) Name() string { return "qwen_code" }

// Exit codes documented for headless runs.
const (
	qwenExitTurns   = 53
	qwenExitBudget  = 55
	qwenExitSignal  = 130
	qwenSettingsDir = ".qwen"
)

func (e *qwenEngine) writeSettings(home string) error {
	dir := filepath.Join(home, qwenSettingsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	settings := map[string]any{
		"general":   map[string]any{"enableAutoUpdate": false, "checkpointing": map[string]any{"enabled": false}},
		"privacy":   map[string]any{"usageStatisticsEnabled": false},
		"telemetry": map[string]any{"enabled": false},
		"modelProviders": map[string]any{"openai": []map[string]any{{
			"id": e.llm.Model, "name": e.llm.Model, "baseUrl": e.llm.BaseURL, "envKey": "OPENAI_API_KEY",
		}}},
	}
	raw, _ := json.MarshalIndent(settings, "", "  ")
	return os.WriteFile(filepath.Join(dir, "settings.json"), raw, 0o600)
}

func (e *qwenEngine) Run(ctx context.Context, ws *Workspace, b Brief) (EngineResult, error) {
	home := filepath.Join(ws.JobDir, "home")
	if err := e.writeSettings(home); err != nil {
		return EngineResult{Stopped: "error"}, stepErr("engine_error", "qwen settings: "+err.Error())
	}
	remaining := time.Until(ws.Deadline) - 2*time.Minute
	if remaining < 2*time.Minute {
		return EngineResult{Stopped: "timeout"}, nil
	}
	prompt := briefText(b)
	os.WriteFile(filepath.Join(ws.JobDir, "brief.md"), []byte(prompt), 0o600)
	argv := []string{e.bin, "-p", prompt, "--yolo", "--output-format", "stream-json",
		"--max-session-turns", strconv.Itoa(e.maxRounds), "--max-tool-calls", strconv.Itoa(e.maxRounds * 3),
		// qwen wants whole seconds or a single unit ("5m"); Go's "40m0s" form is rejected.
		"--max-wall-time", strconv.Itoa(int(remaining.Seconds()))}
	env := append(append([]string{}, ws.Env...),
		"OPENAI_BASE_URL="+e.llm.BaseURL, "OPENAI_API_KEY="+e.llm.APIKey, "OPENAI_MODEL="+e.llm.Model,
		"QWEN_CODE_UNATTENDED_RETRY=1", "QWEN_CODE_NO_TELEMETRY=1", "QWEN_CODE_SUPPRESS_YOLO_WARNING=1")
	st := &qwenStream{rep: ws.Reporter, maxRounds: e.maxRounds}
	grantSandbox(home, filepath.Join(ws.JobDir, "brief.md")) // written just now, by the worker
	ws.Reporter.Log(fmt.Sprintf("qwen code on %s, up to %d turns, %s wall time", e.llm.Model, e.maxRounds, remaining.Truncate(time.Minute)))
	out, err := runCmd(ctx, cmdSpec{Dir: ws.RepoDir, Argv: argv, Env: env, Timeout: remaining + time.Minute, OnLine: st.line, Head: 8 << 10, Tail: 32 << 10, Sandbox: true})
	res := EngineResult{Summary: st.summaryText(), Usage: st.usage, Rounds: st.turns, LogTail: lastLines(ws.Scrub.Clean(out.Output), 32<<10)}
	switch {
	case ctx.Err() != nil:
		res.Stopped = "cancelled"
		if context.Cause(ctx) == errDeadline {
			res.Stopped = "timeout"
		}
	case out.TimedOut:
		res.Stopped = "timeout"
	case out.Code == 0:
		res.Stopped = "finished"
	case out.Code == qwenExitTurns:
		res.Stopped = "max_rounds"
	case out.Code == qwenExitBudget:
		res.Stopped = "budget"
	case out.Code == qwenExitSignal:
		res.Stopped = "cancelled"
	default:
		res.Stopped = "error"
		msg := lastLines(ws.Scrub.Clean(out.Output), 600)
		if err != nil && msg == "" {
			msg = err.Error()
		}
		if res.Summary == "" {
			res.Summary = "qwen exited with code " + strconv.Itoa(out.Code) + ": " + msg
		}
	}
	if res.Summary == "" {
		res.Summary = "The engine finished without a summary (stopped: " + res.Stopped + ")."
	}
	ws.Reporter.Usage(res.Usage)
	return res, nil
}

// qwenStream reads the stream-json lines. The shapes are matched loosely on purpose: the CLI's
// event names have moved between versions, and the harness only needs turns, tool names, the
// final text and token counts.
type qwenStream struct {
	rep       *Reporter
	maxRounds int
	turns     int
	tools     int
	usage     app.JobUsage
	last      string
	final     string
	seenUsage bool
}

func jstr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

func first(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v
		}
	}
	return nil
}

// textOf pulls assistant text out of the common shapes: content as a string, content as an
// array of {type:text,text}, or a nested message.
func textOf(m map[string]any) string {
	if msg, ok := m["message"].(map[string]any); ok {
		if t := textOf(msg); t != "" {
			return t
		}
	}
	switch c := first(m, "content", "text", "result", "response").(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			if pm, ok := p.(map[string]any); ok {
				if s, ok := pm["text"].(string); ok && s != "" {
					parts = append(parts, s)
				}
			} else if s, ok := p.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// toolUsesOf lists the tool calls in an assistant message as "name {input…}" lines.
func toolUsesOf(m map[string]any) []string {
	if msg, ok := m["message"].(map[string]any); ok {
		if t := toolUsesOf(msg); len(t) > 0 {
			return t
		}
	}
	parts, ok := m["content"].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, p := range parts {
		pm, ok := p.(map[string]any)
		if !ok || jstr(pm["type"]) != "tool_use" {
			continue
		}
		name := jstr(first(pm, "name", "tool_name"))
		out = append(out, strings.TrimSpace(name+" "+cut(jstr(first(pm, "input", "args", "arguments")), 160)))
	}
	return out
}

func (s *qwenStream) readUsage(v any) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	num := func(keys ...string) int {
		for _, k := range keys {
			if f, ok := m[k].(float64); ok {
				return int(f)
			}
		}
		return 0
	}
	in := num("input_tokens", "prompt_tokens", "inputTokens", "promptTokens", "input")
	out := num("output_tokens", "completion_tokens", "outputTokens", "candidatesTokens", "output")
	if in > 0 || out > 0 {
		s.usage.In, s.usage.Out, s.seenUsage = in, out, true
	}
	for _, k := range []string{"models", "stats", "usage", "metadata"} {
		if sub, ok := m[k]; ok {
			s.readUsage(sub)
		}
	}
	// per-model maps: {"model-id": {"tokens": {...}}}
	for _, v := range m {
		if sub, ok := v.(map[string]any); ok {
			if t, ok := sub["tokens"].(map[string]any); ok {
				s.readUsage(t)
			}
		}
	}
}

func (s *qwenStream) line(l string) {
	var ev map[string]any
	if err := json.Unmarshal([]byte(l), &ev); err != nil {
		return
	}
	typ := strings.ToLower(jstr(first(ev, "type", "event", "kind")))
	role := strings.ToLower(jstr(first(ev, "role")))
	if msg, ok := ev["message"].(map[string]any); ok && role == "" {
		role = strings.ToLower(jstr(msg["role"]))
	}
	switch {
	case strings.Contains(typ, "tool_use") || strings.Contains(typ, "tool_call") || typ == "tool":
		s.tools++
		name := jstr(first(ev, "name", "tool_name", "toolName"))
		if name == "" {
			if t, ok := ev["tool"].(map[string]any); ok {
				name = jstr(first(t, "name"))
			}
		}
		arg := jstr(first(ev, "input", "args", "arguments", "parameters", "params"))
		s.rep.Log("qwen: " + strings.TrimSpace(name+" "+cut(arg, 160)))
	case typ == "assistant" || (typ == "message" && role == "assistant") || (typ == "content" && role != "user"):
		// One assistant message per model reply; tool calls ride inside its content array
		// ({"type":"tool_use","name","input"}), thinking-only chunks are not a turn.
		text, tools := textOf(ev), toolUsesOf(ev)
		if text == "" && len(tools) == 0 {
			return
		}
		s.turns++
		for _, t := range tools {
			s.tools++
			s.rep.Log("qwen: " + t)
		}
		if text != "" {
			s.last = text
		}
		if s.turns%5 == 0 {
			s.rep.Log(fmt.Sprintf("qwen: turn %d of at most %d", s.turns, s.maxRounds))
		}
	case typ == "result" || typ == "final" || typ == "done":
		if t := textOf(ev); t != "" {
			s.final = t
		}
		s.readUsage(ev)
	case typ == "error":
		s.rep.Warn("qwen: " + cut(jstr(first(ev, "message", "error", "content")), 300))
	case typ == "usage" || typ == "stats":
		s.readUsage(ev)
	}
}

func (s *qwenStream) summaryText() string {
	for _, t := range []string{s.final, s.last} {
		if i := strings.Index(t, "SUMMARY:"); i >= 0 {
			return strings.TrimSpace(t[i+len("SUMMARY:"):])
		}
	}
	if s.final != "" {
		return strings.TrimSpace(s.final)
	}
	return strings.TrimSpace(s.last)
}
