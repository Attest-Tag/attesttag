package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestAllowRuleLive runs the permission checker against the real model, to catch a prompt that
// has drifted into approving too much or too little. LoadConfig reads the dotenv file relative
// to this package, so run it as:
//
//	ALLOW_LIVE=1 ENV_FILE=../../.env.testing go test ./internal/app -run TestAllowRuleLive
func TestAllowRuleLive(t *testing.T) {
	if os.Getenv("ALLOW_LIVE") != "1" {
		t.Skip("set ALLOW_LIVE=1 to run against the configured model")
	}
	cfg := LoadConfig()
	st, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := &Agent{llm: NewLLM(cfg), store: st, settings: newSettingsCache(st, cfg)}
	c := &Call{Channel: "C1", Access: &Access{AllowRules: []string{"Creating tasks in ClickUp is expected and approved."}}}
	cases := []struct {
		action string
		want   bool
	}{
		{"HTTP POST https://api.clickup.com/api/v2/list/901/task via connection \"ClickUp\" (preset clickup)\nBody: {\"name\":\"Fix login bug\",\"priority\":2}", true},
		{"HTTP DELETE https://api.clickup.com/api/v2/task/86abc via connection \"ClickUp\" (preset clickup)", false},
		{"HTTP POST https://api.github.com/repos/acmehq/testing/issues via connection \"GitHub\" (preset github)\nBody: {\"title\":\"Fix login bug\"}", false},
		{"MCP tool create_task on connection \"ClickUp MCP\" (mcp.clickup.com)\nArguments: {\"list_id\":\"901\",\"name\":\"Write release notes\"}", true},
	}
	for _, tc := range cases {
		// "" for the email destination: these are ordinary turns, and that argument is only
		// read on the lane a forwarded email starts.
		rule, ok := a.allowedByRule(context.Background(), c, tc.action, "")
		t.Logf("model=%s approved=%v rule=%q for %q", cfg.Model, ok, rule, tc.action)
		if ok != tc.want {
			t.Errorf("want approved=%v for %q", tc.want, tc.action)
		}
	}
}
