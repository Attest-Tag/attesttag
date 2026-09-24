package app

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The model writes the search terms; the channel's grants decide which repositories are
// searched. A query is therefore not allowed to carry its own repo:, org:, user: or owner:,
// because the credential answering the search can usually see far more than the one repository
// its connection names — a classic token sees the whole account. Everything else the model
// writes narrows the search inside the scope we set, and is its business.
func TestStripRepoScopeRemovesOnlyTheScopeQualifiers(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"parseTimeout", "parseTimeout"},
		{"org:other-co parseTimeout", "parseTimeout"},
		{"parseTimeout repo:other-co/secrets", "parseTimeout"},
		{"user:someone parseTimeout owner:acme", "parseTimeout"},
		{"-repo:other-co/secrets parseTimeout", "parseTimeout"},
		{"ORG:Other-Co parseTimeout", "parseTimeout"},          // the qualifier is not case sensitive
		{"org : other-co parseTimeout", "parseTimeout"},        // nor is it spacing sensitive
		{"is:pr is:open label:bug", "is:pr is:open label:bug"}, // filters that narrow, not scope
		{"path:internal/auth language:go", "path:internal/auth language:go"},
		{"fork:true handleLogin", "fork:true handleLogin"},
		{"org:a org:b org:c", ""}, // nothing but scope leaves nothing to search for
	} {
		if got := stripRepoScope(tc.in); got != tc.want {
			t.Errorf("stripRepoScope(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// repoTargets is the whole isolation boundary: it answers out of Access.Repos() and nothing
// else, so a repository the channel was not granted cannot be reached by naming it.
func TestRepoTargetsAnswersOnlyFromWhatTheChannelHolds(t *testing.T) {
	api := &Connection{ID: 1, Name: "api", Preset: "github", Repo: "acme/api"}
	web := &Connection{ID: 2, Name: "web", Preset: "github", Repo: "acme/web"}
	granted := &Call{Access: &Access{Rules: []Rule{{Conn: api, Rank: 2}, {Conn: web, Rank: 1}}}}

	// No repository named: every repository the channel holds, narrowest grant first.
	all, err := repoTargets(granted, "")
	if err != nil {
		t.Fatalf("repoTargets with no name: %v", err)
	}
	if got := strings.Join(repoNames(all), ","); got != "acme/api,acme/web" {
		t.Errorf("repoTargets(\"\") = %s, want acme/api,acme/web in grant order", got)
	}

	// One named: exactly that one, however it was spelled.
	for _, spelled := range []string{"acme/web", "ACME/WEB", "https://github.com/acme/web", "git@github.com:acme/web.git"} {
		one, err := repoTargets(granted, spelled)
		if err != nil {
			t.Errorf("repoTargets(%q): %v", spelled, err)
			continue
		}
		if len(one) != 1 || one[0].Repo != "acme/web" {
			t.Errorf("repoTargets(%q) = %v, want just acme/web", spelled, repoNames(one))
		}
	}

	// One the channel does not hold: refused, and the refusal names what it does hold rather
	// than leaving the model to read an empty result as an absence of code.
	_, err = repoTargets(granted, "other-co/secrets")
	if err == nil {
		t.Fatal("repoTargets reached a repository the channel was not granted")
	}
	if !strings.Contains(err.Error(), "acme/api") || !strings.Contains(err.Error(), "acme/web") {
		t.Errorf("refusal %q does not say which repositories are reachable", err)
	}

	// A channel with no repositories says so, rather than searching nothing in silence.
	if _, err := repoTargets(&Call{Access: &Access{}}, ""); err == nil {
		t.Error("repoTargets found repositories in a channel that has none")
	}
}

// A search spanning more repositories than the fan-out allows has to say it was partial.
// Reporting the cap is the difference between "not found here" and "not found", and only one
// of those is true.
func TestCapTargetsReportsWhatItLeftOut(t *testing.T) {
	var many []*Connection
	for i := 0; i < repoFanout+3; i++ {
		many = append(many, &Connection{Repo: "acme/r" + string(rune('a'+i))})
	}
	got, capped := capTargets(many)
	if !capped || len(got) != repoFanout {
		t.Fatalf("capTargets kept %d capped=%v, want %d capped", len(got), capped, repoFanout)
	}
	note := moreRepos(many, capped)
	if !strings.Contains(note, "Name a repository") {
		t.Errorf("the cap note %q does not tell the model how to look further", note)
	}

	few := many[:2]
	if got, capped := capTargets(few); capped || len(got) != 2 {
		t.Errorf("capTargets trimmed a set that fits: %d capped=%v", len(got), capped)
	}
	if note := moreRepos(few, false); note != "" {
		t.Errorf("an uncapped search still claimed to be partial: %q", note)
	}
}

// A rate limit is not a refusal. GitHub says when to come back in two different ways and answers
// a spent budget with 403 as often as 429, so the wait has to be read off the headers rather
// than inferred from the status — otherwise the model reads "slow down" as "you may not do
// this" and stops asking altogether.
func TestRetryAfterHintReadsBothWaysGitHubSaysWait(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	h := func(kv ...string) http.Header {
		out := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			out.Set(kv[i], kv[i+1])
		}
		return out
	}
	for _, tc := range []struct {
		name string
		hdr  http.Header
		want time.Duration
	}{
		{"plain seconds", h("Retry-After", "40"), 40 * time.Second},
		{"an http date", h("Retry-After", now.Add(30*time.Second).Format(http.TimeFormat)), 30 * time.Second},
		{"a date already past", h("Retry-After", now.Add(-time.Hour).Format(http.TimeFormat)), time.Second},
		{"nothing at all", h(), 0},
		{"garbage", h("Retry-After", "soon"), 0},
		// The reset epoch rides along on every response, so it only means wait once the budget
		// is actually spent. Reading it otherwise would stall every successful call.
		{"budget left", h("X-RateLimit-Remaining", "17", "X-RateLimit-Reset", "99999999999"), 0},
	} {
		if got := retryAfterHint(tc.hdr, now); got != tc.want {
			t.Errorf("%s: retryAfterHint = %v, want %v", tc.name, got, tc.want)
		}
	}

	// A spent budget does say wait, and the wait is measured from now rather than from the
	// caller's clock argument, because that is the header's own meaning.
	spent := h("X-RateLimit-Remaining", "0", "X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(45*time.Second).Unix(), 10))
	if got := retryAfterHint(spent, now); got < 40*time.Second || got > 46*time.Second {
		t.Errorf("a spent budget gave %v, want about 45s", got)
	}

	// And the sentence it turns into tells the model what to do instead of just what went wrong.
	msg := ghResult{status: 403, retry: 40 * time.Second}.limited("code search")
	if msg == nil || !strings.Contains(msg.Error(), "name one repository") {
		t.Errorf("the rate-limit message %v does not offer a way forward", msg)
	}
	if (ghResult{status: 403}).limited("code search") != nil {
		t.Error("a 403 carrying no wait was reported as a rate limit; it is a refusal")
	}
}
