package app

// Does the model actually reach for run_js? This asks the real model with the real tool
// definitions and looks at which tool it picks, so it catches a description that reads well to a
// person but does not pull the model in — and the opposite failure, a tool so eagerly described
// that it fires on "what's 2+2".
//
//	go test ./internal/app -run TestSandboxToolChoice -eval -v

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"attesttag/internal/sandbox"

	"github.com/joho/godotenv"
	"github.com/openai/openai-go/v3"
)

func toolChoiceAgent(t *testing.T) (*Agent, *LLM, Config) {
	t.Helper()
	// Overload so the dotenv key wins over a stale one exported in the shell.
	godotenv.Overload("../../.env.testing")
	cfg := LoadConfig()
	if cfg.LLMKey == "" {
		t.Skip("no model key")
	}
	a := &Agent{cfg: cfg, tools: map[string]Tool{}}
	a.registerSlackTools()
	a.registerWebTools()
	a.registerMemoryTools()
	a.registerDocTools()
	a.registerRoutineTools()
	a.registerArtifactTools()
	a.registerSandboxTools()
	return a, NewLLM(cfg), cfg
}

func TestSandboxToolChoice(t *testing.T) {
	if !*runEvals {
		t.Skip("pass -eval to run live evals")
	}
	a, llm, cfg := toolChoiceAgent(t)

	// A connected service, so http_request is on the table too and the choice is a real one.
	call := &Call{Channel: "C1", Access: &Access{Rules: []Rule{{Conn: &Connection{
		Name: "ClickUp", AllowedHosts: []string{"api.clickup.com"}, Writes: "confirm",
	}}}}}
	defs := make([]openai.ChatCompletionToolUnionParam, 0, len(a.order)+1)
	for _, n := range a.order {
		defs = append(defs, a.tools[n].def())
	}
	defs = append(defs, a.httpTool(call).def())
	t.Logf("model=%s tools=%v", cfg.Model, a.order)

	const system = `You are an AI teammate inside a Slack workspace, talking in the #ops channel. ` +
		`Tools: use them whenever the answer depends on workspace content, documents, or the web rather than guessing. ` +
		`When an answer turns on a calculation — a total, a count, an average, a percentile, a top-N, a date difference, ` +
		`parsing rows out of a blob — compute it with run_js over more than a couple of numbers, passing the data in as ` +
		`its input argument rather than working it out in your head; a single sum you can state exactly needs no tool.` +
		"\nConnected services in this channel (use http_request or the named tools): api.clickup.com (ClickUp)."

	cases := []struct {
		name   string
		prompt string
		want   string // tool that must be called, "" = run_js must NOT be called
	}{
		{"percentile", "Deal sizes closed last month: 4200, 15000, 900, 32000, 7600, 1200, 88000, 5400, 2300, 19000, 640, 44000. What's the median and the 90th percentile?", "run_js"},
		{"grouping", "Here are yesterday's tickets as JSON: [{\"assignee\":\"ana\",\"mins\":45},{\"assignee\":\"bo\",\"mins\":12},{\"assignee\":\"ana\",\"mins\":30},{\"assignee\":\"cy\",\"mins\":95},{\"assignee\":\"bo\",\"mins\":60}]. Who spent the most time, and what's each person's average?", "run_js"},
		{"date_math", "Our contract started 2024-11-08 and auto-renews after 18 months. How many days from today until the renewal date?", "run_js"},
		{"docs_still_wins", "Above what amount does a refund need Head of Finance approval? Check our docs.", "search_docs"},
		{"trivial_arithmetic_stays_in_head", "what's 17*23?", ""},
		{"paging_prefers_the_sandbox", "Our ClickUp list 900000000002 has around 800 tasks, returned 100 per page. How many of them are unassigned?", "run_js"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := llm.client.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
				Model: cfg.Model,
				Messages: []openai.ChatCompletionMessageParamUnion{
					openai.SystemMessage(system),
					openai.UserMessage(c.prompt),
				},
				Tools: defs,
			})
			if err != nil {
				t.Fatalf("model call: %v", err)
			}
			if len(resp.Choices) == 0 {
				t.Fatal("no choices")
			}
			var called []string
			for _, tc := range resp.Choices[0].Message.ToolCalls {
				called = append(called, tc.Function.Name)
			}
			t.Logf("called=%v", called)
			// Close the loop: whatever the model wrote must actually run, or the tool is
			// only theoretically useful.
			for _, tc := range resp.Choices[0].Message.ToolCalls {
				if tc.Function.Name != "run_js" {
					continue
				}
				var p struct{ Code, Input string }
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &p); err != nil {
					t.Fatalf("arguments were not JSON: %v", err)
				}
				if c.name == "paging_prefers_the_sandbox" {
					if !strings.Contains(p.Code, "fetch(") {
						t.Errorf("the script does not use fetch, so it cannot page:\n%s", p.Code)
					}
					t.Logf("code:\n%s", p.Code)
					continue // no live API to run it against
				}
				out, err := sandbox.Run(context.Background(), p.Code, p.Input, sandbox.Limits{})
				if err != nil {
					t.Errorf("the model's script failed: %v\ncode:\n%s", err, p.Code)
					continue
				}
				t.Logf("script output:\n%s", out)
			}
			switch c.want {
			case "":
				if slicesContains(called, "run_js") {
					t.Errorf("run_js fired on a trivial sum; the description is pulling too hard")
				}
			default:
				if !slicesContains(called, c.want) {
					t.Errorf("want %s, model called %v; answer was %q", c.want, called,
						strings.TrimSpace(resp.Choices[0].Message.Content))
				}
			}
		})
	}
}

func slicesContains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
