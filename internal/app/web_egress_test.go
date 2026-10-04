package app

import (
	"context"
	"testing"
)

// restrict_web_egress is the operator's lever for a deployment that connects sensitive services and
// reads untrusted content: with it on, fetch_url and web_search are gone from every turn, so an
// instruction hidden in a web page, a GitHub issue or a document cannot carry a connection's result
// out to a host of its choosing. Off by default, and it never touches reading the repository.
func TestRestrictWebEgressRemovesWebTools(t *testing.T) {
	fixedMasterKey(t)
	st := testStore(t)
	ctx := context.Background()
	org, err := st.CreateOrg(ctx, "Org", 0)
	if err != nil {
		t.Fatal(err)
	}
	newAgent := func() *Agent {
		a := &Agent{tools: map[string]Tool{}, store: st, settings: newSettingsCache(st, Config{})}
		for _, n := range []string{"fetch_url", "web_search", "github_read_file"} {
			a.tools[n] = Tool{Name: n}
		}
		return a
	}
	c := &Call{OrgID: org.ID, TeamID: "T1", Channel: "C1", ThreadTS: "1", UserID: "U1", Kind: "channel"}

	out := newAgent().toolsFor(ctx, c)
	if _, ok := out["fetch_url"]; !ok {
		t.Fatal("fetch_url is missing by default")
	}
	if _, ok := out["web_search"]; !ok {
		t.Fatal("web_search is missing by default")
	}

	if err := st.PutSetting(ctx, org.ID, "restrict_web_egress", "1"); err != nil {
		t.Fatal(err)
	}
	out = newAgent().toolsFor(ctx, c) // a fresh cache reads the new value
	if _, ok := out["fetch_url"]; ok {
		t.Error("fetch_url offered despite restrict_web_egress")
	}
	if _, ok := out["web_search"]; ok {
		t.Error("web_search offered despite restrict_web_egress")
	}
	if _, ok := out["github_read_file"]; !ok {
		t.Error("restrict_web_egress removed the repository read too")
	}
}
