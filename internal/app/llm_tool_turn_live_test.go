package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
)

// TestToolTurnLive sends every request shape a turn makes to real models (LLM_LIVE=1), so an
// endpoint that refuses one of them is found here rather than by a routine in somebody's channel:
//
//   - round 0 with a named tool forced, as a question the agent searches for first is sent;
//   - round 0 with a free choice between the tools;
//   - the ordinary next round: the tool results, the tools still offered, a free choice;
//   - the landing round: the same, told the budget is spent (outOfRoundsNote) with tool_choice
//     "none" (landingChoice);
//   - that round again with the tools gone (noToolsNote), which is how the agent repeats a landing
//     whose provider ignored "none" and wrote nothing alongside the calls.
//
// LLM_LIVE_MODELS is a comma-separated list and defaults to the configured model. A request that
// fails is a failure of the endpoint. A provider that ignores tool_choice is only logged — the
// agent recovers from that — and an answer that is empty or never used the tool result is a
// failure of the model, reported as "answer:" so the two are easy to tell apart.
func TestToolTurnLive(t *testing.T) {
	if os.Getenv("LLM_LIVE") != "1" {
		t.Skip("set LLM_LIVE=1 (and LLM_LIVE_MODELS=vendor/a,vendor/b) to call real models")
	}
	cfg := LoadConfig()
	l := NewLLM(cfg)
	for _, model := range liveModels(cfg) {
		t.Run(model, func(t *testing.T) {
			t.Parallel()
			liveToolTurn(t, l, model)
		})
	}
}

// TestAgentLandingLive runs the agent's own loop on real models (LLM_LIVE=1) with a budget of two
// rounds, so the second is a landing: a tool result in hand, tool_choice "none" or whatever the
// endpoint takes instead, and an answer that has to come out of it. It is the path a routine
// that spends its budget takes, end to end, with Slack stubbed.
func TestAgentLandingLive(t *testing.T) {
	if os.Getenv("LLM_LIVE") != "1" {
		t.Skip("set LLM_LIVE=1 (and LLM_LIVE_MODELS=vendor/a,vendor/b) to call real models")
	}
	base := LoadConfig()
	for i, model := range liveModels(base) {
		t.Run(model, func(t *testing.T) {
			t.Parallel()
			cfg := base
			cfg.Model, cfg.MaxToolRounds, cfg.TurnMaxMinutes = model, 2, 12
			a, _, _, st, sl := diggingFixture(t, cfg)
			a.llm = NewLLM(cfg)
			weather := Tool{Name: "get_weather", Desc: "Current weather for one city.",
				Params: schema(map[string]any{"city": map[string]any{"type": "string"}}, "city"),
				Run:    func(context.Context, *Call, json.RawMessage) (string, error) { return liveWeather, nil }}
			a.tools, a.order = map[string]Tool{"get_weather": weather}, []string{"get_weather"}

			ctx := context.Background()
			thread := fmt.Sprintf("1700000000.%06d", 900+i)
			sess, err := st.EnsureSession(ctx, "T1", "C1", thread, "channel", "")
			if err != nil {
				t.Fatal(err)
			}
			c := &Call{TeamID: "T1", OrgID: 1, SL: sl, Channel: "C1", ThreadTS: thread, UserID: "U1",
				Text: "What is the weather in Kathmandu right now?", Kind: "channel", Session: sess,
				Streamer: sl.NewStreamer("C1", thread, "U1")}
			if err := a.Run(ctx, c); err != nil {
				t.Fatalf("request: run: %v", truncate(err.Error(), 600))
			}
			if !strings.Contains(c.FinalText, "17") {
				t.Errorf("answer: never used the tool result: %q", truncate(c.FinalText, 300))
			}
			t.Logf("ok  rounds=%d cost=$%.5f in=%d out=%d answer=%q", c.roundsUsed, c.usage.CostUSD, c.usage.In, c.usage.Out, truncate(firstLine(c.FinalText), 120))
		})
	}
}

// liveModels is LLM_LIVE_MODELS, comma-separated, or the configured model.
func liveModels(cfg Config) []string {
	var out []string
	for _, m := range strings.Split(env("LLM_LIVE_MODELS", cfg.Model), ",") {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	return out
}

// The tool result carries numbers no model would guess, so an answer that repeats them read it.
const liveWeather = `{"city":"Kathmandu","temp_c":17.3,"conditions":"light rain","wind_kph":9}`

func liveToolTurn(t *testing.T, l *LLM, model string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	str := map[string]any{"type": "string"}
	tools := []openai.ChatCompletionToolUnionParam{
		Tool{Name: "get_weather", Desc: "Current weather for one city.", Params: map[string]any{
			"type": "object", "properties": map[string]any{"city": str}, "required": []string{"city"},
		}}.def(),
		Tool{Name: "search_docs", Desc: "Search the organisation's documents.", Params: map[string]any{
			"type": "object", "properties": map[string]any{"query": str}, "required": []string{"query"},
		}}.def(),
	}
	msgs := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage("You answer questions for a team. Use a tool whenever one can answer; never guess live data."),
		openai.UserMessage("What is the weather in Kathmandu right now?"),
	}
	var spent Usage
	chat := func(step string, m []openai.ChatCompletionMessageParamUnion, tl []openai.ChatCompletionToolUnionParam, choice string) openai.ChatCompletionMessage {
		t.Helper()
		resp, us, err := l.Chat(ctx, model, m, tl, choice)
		if err != nil {
			t.Fatalf("request: %s: %v", step, truncate(err.Error(), 600))
		}
		spent.add(us)
		if len(resp.Choices) == 0 {
			t.Fatalf("request: %s: no choices", step)
		}
		return resp.Choices[0].Message
	}
	var notes []string

	forced := chat("forced tool", msgs, tools, "get_weather")
	if !calls(forced, "get_weather") {
		notes = append(notes, fmt.Sprintf("forced: ignored (calls %v)", callNames(forced.ToolCalls)))
	}
	free := chat("free choice", msgs, tools, "")
	if len(free.ToolCalls) == 0 {
		notes = append(notes, "auto: answered without a tool")
	}

	turn := forced
	if len(turn.ToolCalls) == 0 {
		turn = free
	}
	if len(turn.ToolCalls) == 0 {
		t.Fatalf("answer: never called a tool, forced or free; answered %q", truncate(firstLine(stripThinking(free.Content)), 160))
	}
	after := append(msgs[:len(msgs):len(msgs)], turn.ToParam())
	for _, tc := range turn.ToolCalls {
		after = append(after, openai.ToolMessage(liveWeather, tc.ID))
	}

	next := chat("next round", after, tools, "")
	if len(next.ToolCalls) > 0 {
		notes = append(notes, fmt.Sprintf("next: called again %v", callNames(next.ToolCalls)))
	} else {
		checkAnswer(t, "next round", next)
	}

	landing := append(after[:len(after):len(after)], openai.UserMessage(outOfRoundsNote))
	land := chat("landing (tool_choice none)", landing, tools, toolChoiceNone)
	switch {
	case len(land.ToolCalls) == 0:
		checkAnswer(t, "landing", land)
	case stripThinking(land.Content) != "":
		notes = append(notes, fmt.Sprintf("none: ignored (calls %v), answered alongside", callNames(land.ToolCalls)))
		checkAnswer(t, "landing", land)
	default:
		notes = append(notes, fmt.Sprintf("none: ignored (calls %v)", callNames(land.ToolCalls)))
	}

	bare := chat("landing without tools", append(landing[:len(landing):len(landing)], openai.UserMessage(noToolsNote)), nil, "")
	checkAnswer(t, "landing without tools", bare)

	var refused []string
	for _, choice := range []string{"get_weather", toolChoiceNone} {
		if _, ok := l.autoOnly.Load(autoOnlyKey(model, choice)); ok {
			refused = append(refused, choice)
		}
	}
	t.Logf("ok  refused=%v cost=$%.5f in=%d out=%d %s", refused, spent.CostUSD, spent.In, spent.Out, strings.Join(notes, "; "))
}

func calls(m openai.ChatCompletionMessage, name string) bool {
	for _, tc := range m.ToolCalls {
		if tc.Function.Name == name {
			return true
		}
	}
	return false
}

func checkAnswer(t *testing.T, step string, m openai.ChatCompletionMessage) {
	t.Helper()
	text := stripThinking(m.Content)
	switch {
	case strings.TrimSpace(text) == "":
		t.Errorf("answer: %s: empty", step)
	case !strings.Contains(text, "17"):
		t.Errorf("answer: %s: never used the tool result: %q", step, truncate(firstLine(text), 160))
	}
}
