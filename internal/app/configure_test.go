package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLocalBaseURL(t *testing.T) {
	cases := map[string]string{
		"":               "",
		":8090":          "http://localhost:8090",
		"0.0.0.0:8080":   "http://localhost:8080",
		"[::]:8080":      "http://localhost:8080",
		"127.0.0.1:8090": "http://127.0.0.1:8090",
		"bot.local:9000": "http://bot.local:9000",
		"8090":           "http://localhost:8090",
		" :8090 ":        "http://localhost:8090",
	}
	for in, want := range cases {
		if got := localBaseURL(in); got != want {
			t.Errorf("localBaseURL(%q)=%q want %q", in, got, want)
		}
	}
}

// Without ADMIN_BASE_URL the footer must still carry a Configure link, pointing at the
// process's own listen address; with it set, that wins.
func TestConfigureURLFallback(t *testing.T) {
	fixedMasterKey(t) // the link's MAC runs under a key derived from MASTER_KEY
	t.Setenv("ADMIN_BASE_URL", "")
	a := &Agent{cfg: Config{HealthAddr: ":8090"}}
	got := a.configureURL(context.Background(), 1, "T1", "C1")
	if !strings.HasPrefix(got, "http://localhost:8090/configure/T1/C1?t=") {
		t.Errorf("fallback configureURL=%q", got)
	}
	footer := a.footer(context.Background(), &Call{Kind: "mention", TeamID: "T1", Channel: "C1"}, "qwen/qwen3.5-flash", Usage{In: 1234, Out: 340, CostUSD: 0.0012})
	if !strings.HasPrefix(footer, "basic · 1.2k in · 340 out · $0.0012 · <http://localhost:8090/configure/T1/C1?t=") || !strings.HasSuffix(footer, "|Configure>") {
		t.Errorf("footer=%q", footer)
	}
	t.Setenv("ADMIN_BASE_URL", "https://bot.example.com/")
	if got := a.configureURL(context.Background(), 1, "T1", "C1"); !strings.HasPrefix(got, "https://bot.example.com/configure/T1/C1?t=") {
		t.Errorf("explicit configureURL=%q", got)
	}
	t.Setenv("ADMIN_BASE_URL", "")
	if got := (&Agent{}).configureURL(context.Background(), 1, "T1", "C1"); got != "" {
		t.Errorf("no address and no base url should give no link, got %q", got)
	}
}

// The Configure link in a reply footer is shared by everyone who can read the channel. A
// personal link is DMed to one person and opens their own settings, so the two must never be
// interchangeable and the name on a personal one must not be editable.
func TestConfigureTokenCarriesThePerson(t *testing.T) {
	fixedMasterKey(t)
	now := time.Now()

	// A channel link names nobody, and that is what keeps the personal tab off it.
	shared := mintConfigureToken("T1", "C1", "", 7, now)
	epoch, user, ok := verifyConfigureToken("T1", "C1", shared, now)
	if !ok || epoch != 7 || user != "" {
		t.Fatalf("shared link: epoch=%d user=%q ok=%v", epoch, user, ok)
	}

	// A personal link names one person and survives the round trip.
	mine := mintConfigureToken("T1", "C1", "USAM", 7, now)
	epoch, user, ok = verifyConfigureToken("T1", "C1", mine, now)
	if !ok || epoch != 7 || user != "USAM" {
		t.Fatalf("personal link: epoch=%d user=%q ok=%v", epoch, user, ok)
	}

	// Rewriting the name is the attack this exists to stop: one person's link must not open
	// another's settings, and the id is inside the MAC rather than beside it.
	parts := strings.Split(mine, ".")
	parts[3] = "UPRIYA"
	if _, _, ok := verifyConfigureToken("T1", "C1", strings.Join(parts, "."), now); ok {
		t.Fatal("a personal link rewritten to another person was accepted")
	}
	// Nor may it be downgraded into a shared one, or promoted out of one.
	if _, _, ok := verifyConfigureToken("T1", "C1", strings.Replace(mine, "v2.", "v1.", 1), now); ok {
		t.Fatal("a personal link passed as a channel link was accepted")
	}
	if _, _, ok := verifyConfigureToken("T1", "C1", strings.Replace(shared, "v1.", "v2.", 1), now); ok {
		t.Fatal("a channel link passed as a personal link was accepted")
	}
	// And the existing guards still hold on the new shape.
	if _, _, ok := verifyConfigureToken("T2", "C1", mine, now); ok {
		t.Fatal("a personal link opened another workspace")
	}
	if _, _, ok := verifyConfigureToken("T1", "C1", mine, now.Add(configureLinkTTL+time.Minute)); ok {
		t.Fatal("an expired personal link still opened")
	}
}

// The Configure page is open to everyone who can see the channel, so the model dropdown is a
// menu an admin wrote and not a text field. Anything off that menu falls back rather than
// quietly putting the channel on a model nobody chose to pay for.
func TestChannelModelsAreAMenuNotAField(t *testing.T) {
	offered := parseModelList("anthropic/claude-opus-5, openai/gpt-5-mini")
	if len(offered) != 2 || offered[0] != "anthropic/claude-opus-5" {
		t.Fatalf("parseModelList = %v", offered)
	}
	// Order is the admin's, and blanks and repeats drop out.
	if got := parseModelList("b,,a\nb , a "); strings.Join(got, "|") != "b|a" {
		t.Errorf("parseModelList kept duplicates or blanks: %v", got)
	}

	// otherModel is what keeps the dropdown honest when a channel is already on something the
	// menu no longer offers: it is shown as selected rather than silently reading as Default.
	if got := otherModel("openai/o9", offered); got != "openai/o9" {
		t.Errorf("a model set from the console vanished from the dropdown: %q", got)
	}
	for _, onMenu := range []string{"", "heavy", "openai/gpt-5-mini"} {
		if got := otherModel(onMenu, offered); got != "" {
			t.Errorf("otherModel(%q) = %q, want nothing: it is already on the menu", onMenu, got)
		}
	}
}

// The cap on that menu is the server's, not only the picker's. The check used to sit in
// validateWorkerSetting, which PUT /api/settings calls for worker_ keys alone, so a list of any
// length saved from a script or an old console tab went straight in.
func TestTheChannelModelListIsCappedWhereItIsSaved(t *testing.T) {
	b, mux, st := identityBot(t)
	_, org, token := signedUp(t, b, mux, st, "founder@example.com")
	models := make([]string, maxChannelModels+1)
	for i := range models {
		models[i] = fmt.Sprintf("vendor/model-%d", i)
	}
	save := func(list []string) (int, map[string]any) {
		return authReq(t, mux, "PUT", "/api/settings", map[string]string{"channel_models": strings.Join(list, ",")}, token)
	}

	if code, body := save(models); code != 400 {
		t.Fatalf("%d models were saved (%d %v); at most %d may be offered", len(models), code, body, maxChannelModels)
	}
	if kv, _ := st.AllSettings(context.Background(), org); kv["channel_models"] != "" {
		t.Errorf("the refused list was stored anyway: %q", kv["channel_models"])
	}
	if code, body := save(models[:maxChannelModels]); code != 200 {
		t.Errorf("a list of exactly %d was refused: %d %v", maxChannelModels, code, body)
	}
	// Counted the way the dropdown draws it, after repeats drop out.
	if code, body := save(append(models[:maxChannelModels:maxChannelModels], models[0])); code != 200 {
		t.Errorf("a repeat pushed a list of %d over the cap: %d %v", maxChannelModels, code, body)
	}
}
