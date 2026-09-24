package app

import (
	"context"
	"strings"
	"testing"
)

func TestFmtTokensAndCost(t *testing.T) {
	for n, want := range map[int]string{0: "0", 340: "340", 1234: "1.2k", 2000: "2k", 15234: "15k", 1_300_000: "1.3M"} {
		if got := fmtTokens(n); got != want {
			t.Errorf("fmtTokens(%d)=%q want %q", n, got, want)
		}
	}
	for usd, want := range map[float64]string{0.00005: "<$0.0001", 0.0012: "$0.0012", 0.0456: "$0.046", 1.5: "$1.50"} {
		if got := fmtCost(usd); got != want {
			t.Errorf("fmtCost(%v)=%q want %q", usd, got, want)
		}
	}
}

func TestFooterLine(t *testing.T) {
	a := &Agent{}
	c := &Call{Kind: "mention", Channel: "C1"}
	got := a.footer(context.Background(), c, "qwen/qwen3.5-flash", Usage{In: 1234, Out: 340, CostUSD: 0.0012})
	if got != "basic · 1.2k in · 340 out · $0.0012" {
		t.Errorf("footer=%q", got)
	}
	if got := a.footer(context.Background(), c, "qwen/qwen3.5-flash", Usage{}); got != "basic" {
		t.Errorf("empty usage footer=%q", got)
	}
	if got := a.footer(context.Background(), &Call{Kind: "dm"}, "m", Usage{In: 1}); got != "" {
		t.Errorf("dm footer=%q", got)
	}
}

// The footer is read by everyone in the channel, so it names the tier and never the provider's
// model id. Each of the three tiers must be told apart from the other two.
func TestFooterNamesTheTierNotTheModel(t *testing.T) {
	st := Settings{Model: "z-ai/glm-5.3-flash", HeavyModel: "z-ai/glm-5.3"}
	for model, want := range map[string]string{
		"z-ai/glm-5.3-flash": "basic",
		"z-ai/glm-5.3":       "advanced",
		"qwen/qwen3.5-max":   "custom", // an id an admin pinned to this channel by name
	} {
		if got := modelLabel(st, model); got != want {
			t.Errorf("modelLabel(%q) = %q, want %q", model, got, want)
		}
	}

	ctx := context.Background()
	store := testStore(t)
	a := &Agent{store: store, settings: newSettingsCache(store, Config{Model: st.Model, HeavyModel: st.HeavyModel})}
	c := &Call{Kind: "mention", Channel: "C1", OrgID: 1}
	for _, model := range []string{st.Model, st.HeavyModel, "qwen/qwen3.5-max"} {
		got := a.footer(ctx, c, model, Usage{In: 1234, Out: 340})
		if strings.Contains(got, "glm") || strings.Contains(got, "qwen") || strings.Contains(got, "/") {
			t.Errorf("footer leaked the model id for %q: %q", model, got)
		}
	}
}

// The cost is the one part of the footer an organisation can silence. Turning it off must leave
// the model and the tokens alone, and must not leak across organisations.
func TestFooterShowCost(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	a := &Agent{store: st, settings: newSettingsCache(st, Config{})}
	us := Usage{In: 1234, Out: 340, CostUSD: 0.0012}

	c := &Call{Kind: "mention", Channel: "C1", OrgID: 1}
	if got := a.footer(ctx, c, "qwen/qwen3.5-flash", us); got != "basic · 1.2k in · 340 out · $0.0012" {
		t.Errorf("default footer=%q", got)
	}
	if err := st.PutSetting(ctx, 1, "show_cost", "0"); err != nil {
		t.Fatalf("put setting: %v", err)
	}
	a.settings.Invalidate(1)
	if got := a.footer(ctx, c, "qwen/qwen3.5-flash", us); got != "basic · 1.2k in · 340 out" {
		t.Errorf("cost hidden footer=%q", got)
	}
	if got := a.footer(ctx, &Call{Kind: "mention", Channel: "C1", OrgID: 2}, "qwen/qwen3.5-flash", us); got != "basic · 1.2k in · 340 out · $0.0012" {
		t.Errorf("other org footer=%q", got)
	}
}
