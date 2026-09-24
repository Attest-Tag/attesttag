package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// githubToolAgent wires the GitHub tools onto repoTestBot's fake API: two repositories, each its
// own connection with its own token, which is how a channel that was granted two of them looks.
func githubToolAgent(t *testing.T, fn fakeAPI) (*Agent, *Call) {
	t.Helper()
	b := repoTestBot(t, fn)
	a := &Agent{proxy: b.proxy, store: b.store}
	conn := func(id int64, name, repo string) *Connection {
		enc, err := b.sealSecret(&Secret{Token: "tok-" + name})
		if err != nil {
			t.Fatal(err)
		}
		return &Connection{ID: id, Name: name, Preset: "github", CredType: "bearer", Repo: repo,
			AllowedHosts: []string{"api.github.com"}, Status: "active", secretEnc: enc}
	}
	c := &Call{OrgID: orgID, Channel: "C1", Access: &Access{
		Rules: []Rule{{Conn: conn(1, "api", "acme/api"), Rank: 2}, {Conn: conn(2, "web", "acme/web"), Rank: 2}}}}
	return a, c
}

const ghBase = "https://api.github.com"

// The point of the whole file. A model that writes org: or repo: into its query is asking to
// search somewhere it was not granted, and the credential answering would often oblige — a
// classic token sees the entire account. So the query that goes on the wire carries this
// channel's repositories and only those, one request each under that repository's own token.
func TestGitHubFindCodeSearchesOnlyTheChannelsRepositories(t *testing.T) {
	type sent struct{ q, auth string }
	var calls []sent
	a, c := githubToolAgent(t, func(r *http.Request) (int, string) {
		q, _ := url.QueryUnescape(r.URL.Query().Get("q"))
		calls = append(calls, sent{q: q, auth: r.Header.Get("Authorization")})
		return 200, `{"total_count":1,"items":[{"path":"internal/auth/session.go","html_url":"https://github.com/x"}]}`
	})

	out, err := a.githubFindCode(context.Background(), c, ghBase, "org:other-co repo:other-co/secrets parseTimeout", "", 10)
	if err != nil {
		t.Fatalf("githubFindCode: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("made %d searches, want one per granted repository", len(calls))
	}
	for _, got := range calls {
		if strings.Contains(got.q, "other-co") {
			t.Errorf("the model's own scope reached GitHub: q=%q", got.q)
		}
		if !strings.Contains(got.q, "parseTimeout") {
			t.Errorf("the search terms were lost: q=%q", got.q)
		}
	}
	if !strings.Contains(calls[0].q, "repo:acme/api") || !strings.Contains(calls[1].q, "repo:acme/web") {
		t.Errorf("searches were not scoped to the granted repositories: %+v", calls)
	}
	// Each repository answers under its own credential. Sending acme/web's question with
	// acme/api's fine-grained token returns nothing and reads as an absence of code.
	if calls[0].auth != "Bearer tok-api" || calls[1].auth != "Bearer tok-web" {
		t.Errorf("a repository was searched under another's token: %+v", calls)
	}
	if !strings.Contains(out, "acme/api") || !strings.Contains(out, "internal/auth/session.go") {
		t.Errorf("result does not say where the match is:\n%s", out)
	}
}

// Naming a repository the channel does not hold is refused before anything leaves the building,
// and the refusal says what is reachable instead.
func TestGitHubFindCodeRefusesARepositoryTheChannelLacks(t *testing.T) {
	called := false
	a, c := githubToolAgent(t, func(*http.Request) (int, string) { called = true; return 200, `{}` })
	_, err := a.githubFindCode(context.Background(), c, ghBase, "parseTimeout", "other-co/secrets", 10)
	if err == nil {
		t.Fatal("a repository outside the channel's grants was searched")
	}
	if called {
		t.Error("a request went out for a repository the channel was not granted")
	}
	if !strings.Contains(err.Error(), "acme/api") {
		t.Errorf("refusal %q does not name what is reachable", err)
	}
}

// Code search is not open to every kind of token. When every repository refuses, say that
// plainly and point at the tool that always works — a model told "no results" concludes the
// code is not there and stops looking.
func TestGitHubFindCodeSaysWhenTheCredentialCannotSearch(t *testing.T) {
	a, c := githubToolAgent(t, func(*http.Request) (int, string) {
		return 403, `{"message":"Resource not accessible by integration"}`
	})
	_, err := a.githubFindCode(context.Background(), c, ghBase, "parseTimeout", "", 10)
	if err == nil || !strings.Contains(err.Error(), "github_find_file") {
		t.Errorf("err = %v, want a refusal pointing at github_find_file", err)
	}
}

// A spent budget is a wait, not a refusal, and it stops the fan-out rather than spending the
// next minute's allowance on the remaining repositories.
func TestGitHubFindCodeStopsOnASpentRateLimit(t *testing.T) {
	calls := 0
	b := repoTestBot(t, nil)
	b.proxy.client.Transport = headerAPI(func(r *http.Request) (int, string, http.Header) {
		calls++
		// What a spent budget actually looks like: GitHub says 403, and the headers are how it
		// says "wait", which is why the wait rather than the status decides.
		return 403, `{"message":"API rate limit exceeded"}`, http.Header{
			"X-Ratelimit-Remaining": []string{"0"},
			"X-Ratelimit-Reset":     []string{strconv.FormatInt(time.Now().Add(40*time.Second).Unix(), 10)},
		}
	})
	a := &Agent{proxy: b.proxy, store: b.store}
	enc, _ := b.sealSecret(&Secret{Token: "tok"})
	mk := func(id int64, name, repo string) Rule {
		return Rule{Conn: &Connection{ID: id, Name: name, Preset: "github", CredType: "bearer", Repo: repo,
			AllowedHosts: []string{"api.github.com"}, Status: "active", secretEnc: enc}, Rank: 2}
	}
	c := &Call{OrgID: orgID, Channel: "C1", Access: &Access{Rules: []Rule{mk(1, "api", "acme/api"), mk(2, "web", "acme/web")}}}

	_, err := a.githubFindCode(context.Background(), c, ghBase, "parseTimeout", "", 10)
	if err == nil || !strings.Contains(err.Error(), "try again in") {
		t.Fatalf("err = %v, want a wait rather than a refusal", err)
	}
	if calls != 1 {
		t.Errorf("made %d calls after being rate limited, want 1", calls)
	}
}

// headerAPI is fakeAPI for the cases where the headers are the point: a rate limit is told apart
// from a refusal by what the response says about coming back, not by its status.
type headerAPI func(*http.Request) (int, string, http.Header)

func (f headerAPI) RoundTrip(r *http.Request) (*http.Response, error) {
	status, body, h := f(r)
	if h == nil {
		h = http.Header{}
	}
	h.Set("Content-Type", "application/json")
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: h, Request: r}, nil
}

// github_find_file needs nothing but read access to the contents, which is why it is the half of
// this that works on every credential. It costs two calls per repository — the default branch,
// then the tree — and the tree is cached so the second question is free.
func TestGitHubFindFileWalksTheTreeAndCachesIt(t *testing.T) {
	paths := 0
	a, c := githubToolAgent(t, func(r *http.Request) (int, string) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/repos/acme/api"), strings.HasSuffix(r.URL.Path, "/repos/acme/web"):
			return 200, `{"default_branch":"main"}`
		case strings.Contains(r.URL.Path, "/git/trees/"):
			paths++
			return 200, `{"truncated":false,"tree":[
				{"path":"internal/auth/session.go","type":"blob"},
				{"path":"internal/auth","type":"tree"},
				{"path":"README.md","type":"blob"}]}`
		}
		return 404, `{}`
	})

	out, err := a.githubFindFile(context.Background(), c, ghBase, "auth/session", "", 20)
	if err != nil {
		t.Fatalf("githubFindFile: %v", err)
	}
	if !strings.Contains(out, "internal/auth/session.go") {
		t.Errorf("the match is missing:\n%s", out)
	}
	// A directory is not a file: only blobs are offered, or github_read_file is sent at a path
	// that cannot be read.
	if strings.Contains(out, "internal/auth\t") || strings.Count(out, "internal/auth") != 2 {
		t.Errorf("a directory was listed as a file:\n%s", out)
	}
	if paths != 2 {
		t.Fatalf("fetched %d trees, want one per repository", paths)
	}
	if _, err := a.githubFindFile(context.Background(), c, ghBase, "README", "", 20); err != nil {
		t.Fatal(err)
	}
	if paths != 2 {
		t.Errorf("the second question re-fetched the trees (%d); they are cached", paths)
	}
}

// GitHub truncates the tree of a very large repository. Saying so is the difference between
// "not in this repository" and "not in the part of it we could see".
func TestGitHubFindFileReportsATruncatedTree(t *testing.T) {
	a, c := githubToolAgent(t, func(r *http.Request) (int, string) {
		if strings.Contains(r.URL.Path, "/git/trees/") {
			return 200, `{"truncated":true,"tree":[{"path":"a.go","type":"blob"}]}`
		}
		return 200, `{"default_branch":"main"}`
	})
	out, err := a.githubFindFile(context.Background(), c, ghBase, "nothing-matches-this", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "truncated") {
		t.Errorf("a partial file list was reported as a complete one:\n%s", out)
	}
}

// Reading a file asks for the raw bytes rather than GitHub's base64 envelope, and goes out under
// the named repository's own credential.
func TestGitHubReadFileAsksForRawUnderTheRightCredential(t *testing.T) {
	var accept, auth, path string
	a, c := githubToolAgent(t, func(r *http.Request) (int, string) {
		accept, auth, path = r.Header.Get("Accept"), r.Header.Get("Authorization"), r.URL.Path
		return 200, "package auth\n"
	})
	out, err := a.githubReadFile(context.Background(), c, ghBase, "acme/web", "/internal/auth/session.go", "")
	if err != nil {
		t.Fatalf("githubReadFile: %v", err)
	}
	if accept != "application/vnd.github.raw" {
		t.Errorf("Accept = %q, want the raw media type", accept)
	}
	if auth != "Bearer tok-web" {
		t.Errorf("Authorization = %q, want acme/web's own token", auth)
	}
	if path != "/repos/acme/web/contents/internal/auth/session.go" {
		t.Errorf("path = %q", path)
	}
	if !strings.Contains(out, "package auth") || !strings.Contains(out, "acme/web") {
		t.Errorf("the file came back without saying where from:\n%s", out)
	}
}

// With two repositories connected there is no sensible default, so ask rather than read the
// wrong file. With one there is nothing else it could mean.
func TestGitHubReadFileNeedsARepositoryOnlyWhenThereIsAChoice(t *testing.T) {
	a, c := githubToolAgent(t, func(*http.Request) (int, string) { return 200, "x" })
	if _, err := a.githubReadFile(context.Background(), c, ghBase, "", "README.md", ""); err == nil ||
		!strings.Contains(err.Error(), "which repository") {
		t.Errorf("err = %v, want a request to say which repository", err)
	}
	c.Access.Rules = c.Access.Rules[:1]
	if _, err := a.githubReadFile(context.Background(), c, ghBase, "", "README.md", ""); err != nil {
		t.Errorf("with one repository connected, the name should be optional: %v", err)
	}
}

// The issue search carries the same scoping as the code search, which it did not before: its
// query went to GitHub exactly as the model wrote it.
func TestGitHubSearchIssuesIsScopedToo(t *testing.T) {
	var queries []string
	a, c := githubToolAgent(t, func(r *http.Request) (int, string) {
		q, _ := url.QueryUnescape(r.URL.Query().Get("q"))
		queries = append(queries, q)
		return 200, `{"items":[{"number":7,"title":"Login times out","state":"open","html_url":"https://github.com/x/7"}]}`
	})
	out, err := a.githubSearchIssues(context.Background(), c, ghBase, "org:other-co is:pr is:open label:bug", "", 10)
	if err != nil {
		t.Fatalf("githubSearchIssues: %v", err)
	}
	for _, q := range queries {
		if strings.Contains(q, "other-co") {
			t.Errorf("the model's own scope reached GitHub: q=%q", q)
		}
		if !strings.Contains(q, "is:pr") || !strings.Contains(q, "label:bug") {
			t.Errorf("filters that narrow the search were stripped along with the scope: q=%q", q)
		}
	}
	if !strings.Contains(out, "acme/api#7") {
		t.Errorf("result does not identify the issue by repository:\n%s", out)
	}
}

// The fan-out is capped, and a capped search says so: a model that reads a partial answer as a
// complete one reports "nowhere in the codebase" after looking in eight of thirty places.
func TestGitHubFindCodeReportsACappedFanOut(t *testing.T) {
	b := repoTestBot(t, func(*http.Request) (int, string) { return 200, `{"total_count":0,"items":[]}` })
	a := &Agent{proxy: b.proxy, store: b.store}
	enc, _ := b.sealSecret(&Secret{Token: "tok"})
	var rules []Rule
	for i := 0; i < repoFanout+4; i++ {
		rules = append(rules, Rule{Rank: 2, Conn: &Connection{ID: int64(i + 1), Name: fmt.Sprintf("r%d", i),
			Preset: "github", CredType: "bearer", Repo: fmt.Sprintf("acme/r%d", i),
			AllowedHosts: []string{"api.github.com"}, Status: "active", secretEnc: enc}})
	}
	c := &Call{OrgID: orgID, Channel: "C1", Access: &Access{Rules: rules}}

	out, err := a.githubFindCode(context.Background(), c, ghBase, "parseTimeout", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Name a repository") {
		t.Errorf("a partial search did not say so:\n%s", out)
	}
}

// The pack the model is offered has to actually contain the new tools, with the arguments the
// tool descriptions promise. A tool nobody can call is worse than one that does not exist.
func TestGitHubPackOffersTheSearchTools(t *testing.T) {
	a := &Agent{}
	want := map[string][]string{
		"github_find_code":      {"query"},
		"github_find_file":      {"name"},
		"github_read_file":      {"path"},
		"github_search":         {"query"},
		"github_get_pr":         {"repo", "number"},
		"github_recent_commits": {"repo"},
		"github_comment":        {"repo", "number", "body"},
	}
	got := map[string]bool{}
	for _, tool := range a.packs("github", "api.github.com") {
		got[tool.Name] = true
		req, _ := json.Marshal(tool.Params["required"])
		for _, r := range want[tool.Name] {
			if !strings.Contains(string(req), `"`+r+`"`) {
				t.Errorf("%s does not require %q; required=%s", tool.Name, r, req)
			}
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("the github pack is missing %s", name)
		}
	}
}

// GitHub refuses an issue search that names neither issues nor pull requests (422, "Query must
// include 'is:issue' or 'is:pull-request'"). "What is open" and a plain keyword name neither, so
// every repository refused, each was skipped, and the model was told "Nothing matches" — and said
// there was nothing open. Such a search now asks for each kind in turn, and a refusal reads as one.
func TestGitHubSearchIssuesAsksForBothWhenTheQueryNamesNeither(t *testing.T) {
	var sent []string
	a, c := githubToolAgent(t, func(r *http.Request) (int, string) {
		q := r.URL.Query().Get("q")
		sent = append(sent, q)
		switch {
		case r.URL.Query().Has("advanced_search") || strings.Contains(q, " OR "):
			// What a fine-grained token gets for the advanced "or": nothing, and no error.
			return 200, `{"items":[]}`
		case strings.Contains(q, "is:issue"):
			return 200, `{"items":[{"number":3,"title":"Checkout times out","state":"open","html_url":"https://github.com/acme/api/issues/3"}]}`
		case strings.Contains(q, "is:pr"):
			return 200, `{"items":[{"number":4,"title":"Queue requests when budget is exhausted","state":"open","html_url":"https://github.com/acme/api/pull/4"}]}`
		}
		return 422, `{"message":"Query must include 'is:issue' or 'is:pull-request'"}`
	})

	out, err := a.githubSearchIssues(context.Background(), c, ghBase, "is:open", "", 10)
	if err != nil || !strings.Contains(out, "acme/api#3") || !strings.Contains(out, "acme/api#4") {
		t.Fatalf("a search for what is open found %q (%v), want the open issue and the open pull request", out, err)
	}
	for _, q := range sent {
		if !namesIssueType(q) {
			t.Errorf("a search went to GitHub naming neither kind: q=%q", q)
		}
	}

	// One that names its type goes as it was written, once per repository.
	sent = nil
	if _, err := a.githubSearchIssues(context.Background(), c, ghBase, "is:pr is:open", "", 10); err != nil {
		t.Fatal(err)
	}
	for _, q := range sent {
		if strings.Contains(q, "is:issue") {
			t.Errorf("a search for pull requests was also asked for issues: q=%q", q)
		}
	}

	// And a search GitHub refused says so, with GitHub's reason, instead of "Nothing matches" —
	// here, a token that may read pull requests but not issues, which GitHub answers with a
	// "Validation Failed" whose reason is in its first error.
	b, c2 := githubToolAgent(t, func(r *http.Request) (int, string) {
		if strings.Contains(r.URL.Query().Get("q"), "is:issue") {
			return 422, `{"message":"Validation Failed","errors":[{"message":"The listed users and repositories cannot be searched either because the resources do not exist or you do not have permission to view them."}]}`
		}
		return 200, `{"items":[{"number":4,"title":"Queue requests when budget is exhausted","state":"open","html_url":"https://github.com/acme/api/pull/4"}]}`
	})
	out, err = b.githubSearchIssues(context.Background(), c2, ghBase, "is:open", "", 10)
	if err != nil || !strings.Contains(out, "acme/api#4") || !strings.Contains(out, "issues: GitHub returned 422") || !strings.Contains(out, "do not have permission") {
		t.Errorf("a half-refused search answered %q, %v; want the pull request and why issues were not searched", out, err)
	}
	if _, err := b.githubSearchIssues(context.Background(), c2, ghBase, "is:issue", "", 10); err == nil || !strings.Contains(err.Error(), "do not have permission") {
		t.Errorf("a refused search answered %v; want an error naming GitHub's reason", err)
	}
}
