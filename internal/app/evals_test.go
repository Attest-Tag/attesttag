package app

// Live evals against the running bot. Skipped unless -eval is passed.
//   SELF_TEST=1 ./attesttag &   then   go test -run TestEvals -eval -v ./...

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/joho/godotenv"
	"github.com/slack-go/slack"
)

var runEvals = flag.Bool("eval", false, "run live evals against the bot")

type evalCase struct {
	Name        string   `json:"name"`
	Prompt      string   `json:"prompt"`
	ExpectTools []string `json:"expect_tools"`
	ExpectRegex string   `json:"expect_regex"`
	ForbidRegex string   `json:"forbid_regex"`
}

func blockText(m slack.Message) string {
	var parts []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if t, ok := x["text"].(string); ok && (x["type"] == "markdown" || x["type"] == "mrkdwn" || x["type"] == "plain_text" || x["type"] == "text") {
				parts = append(parts, t)
			}
			for _, vv := range x {
				walk(vv)
			}
		case []any:
			for _, vv := range x {
				walk(vv)
			}
		}
	}
	raw, _ := json.Marshal(m.Blocks)
	var any_ any
	json.Unmarshal(raw, &any_)
	walk(any_)
	if len(parts) == 0 {
		return m.Text
	}
	return strings.Join(parts, " ")
}

func TestEvals(t *testing.T) {
	if !*runEvals {
		t.Skip("pass -eval to run live evals")
	}
	godotenv.Load("../../.env.testing") // local testing creds, not the deployed .env.prod
	channel := os.Getenv("EVAL_CHANNEL")
	if channel == "" {
		channel = "CEVAL00001"
	}
	api := slack.New(os.Getenv("SLACK_BOT_TOKEN"))
	store, err := OpenStore(env("DB_PATH", "../../attesttag.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw, err := os.ReadFile("../../evals/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []evalCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	ask := func(prompt string) (root string, reply string, tools []string) {
		_, ts, err := api.PostMessageContext(ctx, channel, slack.MsgOptionText("[selftest] "+prompt, false))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		deadline := time.Now().Add(120 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(3 * time.Second)
			msgs, _, _, err := api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{ChannelID: channel, Timestamp: ts})
			if err != nil {
				continue
			}
			for _, m := range msgs {
				if m.Timestamp == ts {
					continue
				}
				txt := blockText(m)
				if strings.TrimSpace(txt) == "" || strings.Contains(m.Text, "[selftest]") {
					continue
				}
				// wait until the bot logged the turn (usage row) so tool_calls are complete
				var n int
				store.db.QueryRow(`select count(*) from usage where thread_ts=?`, ts).Scan(&n)
				var handled int
				store.db.QueryRow(`select count(*) from turns where thread_ts=? and role='assistant'`, ts).Scan(&handled)
				if n == 0 && handled == 0 && !strings.HasPrefix(prompt, "!") {
					break
				}
				rows, _ := store.db.Query(`select name from tool_calls where thread_ts=? order by id`, ts)
				for rows.Next() {
					var name string
					rows.Scan(&name)
					tools = append(tools, name)
				}
				rows.Close()
				return ts, txt, tools
			}
		}
		return ts, "", nil
	}

	// Index the eval organisation's own documents, docs/org-<id>/ — !ingest reads only the folder
	// of the organisation it is asked in, never the rest of docs/.
	ask("!ingest")

	var attached int
	store.db.QueryRow(`select count(*) from scope_bundles sb join scopes s on s.id=sb.scope_id where s.slack_id=?`, channel).Scan(&attached)
	// repo_* cases need a connected repository in the eval channel and WORKER_MODE on.
	var repos int
	store.db.QueryRow(`select count(*) from scope_connections sc join scopes s on s.id=sc.scope_id join connections c on c.id=sc.connection_id
		where s.slack_id=? and c.repo<>'' and c.status='active'`, channel).Scan(&repos)
	// calendar_* cases need the Google connection in the eval channel, and the eval user signed
	// into it: it is a per-person credential, so an attached connection is necessary and not
	// sufficient. A run without the consent gets the Connect card instead of an answer.
	var calendars int
	store.db.QueryRow(`select count(*) from scope_connections sc join scopes s on s.id=sc.scope_id join connections c on c.id=sc.connection_id
		where s.slack_id=? and c.preset='google' and c.status='active'`, channel).Scan(&calendars)
	pass := 0
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			if strings.HasPrefix(c.Name, "connection_") && attached == 0 {
				t.Skip("no bundle attached to the eval channel")
			}
			if strings.HasPrefix(c.Name, "repo_") && (repos == 0 || env("WORKER_MODE", "off") == "off") {
				t.Skip("no repository attached to the eval channel, or WORKER_MODE=off")
			}
			if strings.HasPrefix(c.Name, "calendar_") && calendars == 0 {
				t.Skip("no Google connection attached to the eval channel")
			}
			_, reply, tools := ask(c.Prompt)
			if reply == "" {
				t.Fatalf("no reply")
			}
			ok := true
			for _, want := range c.ExpectTools {
				found := false
				for _, got := range tools {
					if got == want {
						found = true
					}
				}
				if !found {
					ok = false
					t.Errorf("expected tool %s, got %v", want, tools)
				}
			}
			flat := strings.ReplaceAll(reply, "\n", " ")
			if re := regexp.MustCompile(c.ExpectRegex); !re.MatchString(flat) {
				ok = false
				t.Errorf("reply did not match %q:\n%s", c.ExpectRegex, reply)
			}
			if c.ForbidRegex != "" {
				if re := regexp.MustCompile(c.ForbidRegex); re.MatchString(flat) {
					ok = false
					t.Errorf("reply matched forbidden %q:\n%s", c.ForbidRegex, reply)
				}
			}
			if ok {
				pass++
			}
			t.Logf("tools=%v reply=%s", tools, truncate(strings.ReplaceAll(reply, "\n", " "), 200))
		})
	}
	fmt.Printf("\nEVALS: %d/%d passed\n", pass, len(cases))
}
