package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParseAllowRules(t *testing.T) {
	rules, err := parseAllowRules(`[" Creating tasks in ClickUp is approved. ", "", "Posting to #eng is fine"]`)
	if err != nil || len(rules) != 2 || rules[0] != "Creating tasks in ClickUp is approved." {
		t.Fatalf("got %v, %v", rules, err)
	}
	if rules, err := parseAllowRules(""); err != nil || len(rules) != 0 {
		t.Errorf("empty should be an empty list: %v %v", rules, err)
	}
	if _, err := parseAllowRules(`not json`); err == nil {
		t.Error("bad json accepted")
	}
	if _, err := parseAllowRules(`["` + strings.Repeat("x", 1025) + `"]`); err == nil {
		t.Error("over-long rule accepted")
	}
	long := `["` + strings.Repeat(`r","`, 50) + `r"]` // 51 rules
	if _, err := parseAllowRules(long); err == nil {
		t.Error("51 rules accepted")
	}
}

func TestParseAllowDecision(t *testing.T) {
	cases := []struct {
		in   string
		rule int
		ok   bool
	}{
		{`{"approved": true, "rule": 2, "why": "covered"}`, 2, true},
		{"```json\n{\"approved\":true,\"rule\":1}\n```", 1, true},
		{`Sure: {"approved": true, "rule": 1}`, 1, true},
		{`{"approved": false, "rule": 0, "why": "different service"}`, 0, false},
		{`yes`, 0, false},
		{``, 0, false},
	}
	for _, c := range cases {
		rule, ok := parseAllowDecision(c.in)
		if rule != c.rule || ok != c.ok {
			t.Errorf("parseAllowDecision(%q) = %d,%v want %d,%v", c.in, rule, ok, c.rule, c.ok)
		}
	}
}

// The checker is only consulted when rules exist, its answer must name a rule in range, and
// any failure means "ask a human".
func TestAllowedByRule(t *testing.T) {
	var calls atomic.Int32
	answer := `{"approved": true, "rule": 2, "why": "creating a task"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "x", "object": "chat.completion", "created": 1, "model": "m",
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": answer}}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	}))
	defer srv.Close()
	ctx := context.Background()
	st, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := Config{LLMBaseURL: srv.URL, LLMKey: "test", Model: "m"}
	a := &Agent{llm: NewLLM(cfg), store: st, settings: newSettingsCache(st, cfg)}

	// No rules anywhere: no model call, not approved.
	c := &Call{OrgID: orgID, TeamID: "T1", Channel: "C1", Access: &Access{}}
	if _, ok := a.allowedByRule(ctx, c, "HTTP POST https://api.clickup.com/task", ""); ok || calls.Load() != 0 {
		t.Fatalf("approved without rules (calls=%d)", calls.Load())
	}

	// Workspace rule from Settings plus a channel rule: the checker picks rule 2 (the channel's).
	st.PutSetting(ctx, orgID, "allow_rules", `["Reading anything is fine."]`)
	a.settings.Invalidate(orgID)
	c.Access.AllowRules = []string{"Creating tasks in ClickUp is expected and approved."}
	rule, ok := a.allowedByRule(ctx, c, "HTTP POST https://api.clickup.com/api/v2/list/1/task via connection \"ClickUp\"", "")
	if !ok || rule != c.Access.AllowRules[0] || calls.Load() != 1 {
		t.Fatalf("want channel rule approved, got %q %v (calls=%d)", rule, ok, calls.Load())
	}

	// A rule number out of range is a no.
	answer = `{"approved": true, "rule": 7}`
	if _, ok := a.allowedByRule(ctx, c, "HTTP DELETE https://api.clickup.com/api/v2/task/1", ""); ok {
		t.Error("out-of-range rule approved")
	}
	// So is a refusal.
	answer = `{"approved": false, "rule": 0, "why": "deleting is not creating"}`
	if _, ok := a.allowedByRule(ctx, c, "HTTP DELETE https://api.clickup.com/api/v2/task/1", ""); ok {
		t.Error("refusal treated as approval")
	}
}
