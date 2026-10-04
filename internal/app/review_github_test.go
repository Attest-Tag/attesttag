package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"attesttag/internal/review"
)

// The review client is the only thing in code review that talks to GitHub, and it does so without
// anybody pressing Confirm. These pin what it may say, under which token, and what it does with
// what comes back. GitHub is faked twice over: the App's token endpoint behind oauthHTTPClient,
// and the API behind the proxy's own transport, served by an http.ServeMux through httptest.

const fakeInstallation = int64(4242)

// fakeGitHub is GitHub as the review client sees it. Every token it mints is remembered with the
// permissions it was minted for, so a test can say which purpose a request went out under.
type fakeGitHub struct {
	mux *http.ServeMux

	mu       sync.Mutex
	mints    []mintRequest
	perms    map[string]string // token → permissionList
	requests []string          // "METHOD /path?query", in order
}

type mintRequest struct {
	Permissions  map[string]string `json:"permissions"`
	Repositories []string          `json:"repositories"`
}

func (f *fakeGitHub) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func (f *fakeGitHub) mintCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.mints)
}

// permsOf is the permission set of the token a request carried.
func (f *fakeGitHub) permsOf(r *http.Request) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.perms[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
}

// reviewProxyFixture is a proxy with this deployment's App configured, an installation bound to
// orgID, and both halves of GitHub faked.
func reviewProxyFixture(t *testing.T) (*Proxy, *fakeGitHub) {
	t.Helper()
	// Named rather than generated: NewSealer writes a fresh key into a dotenv file when the
	// environment has none.
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 7)
	}
	t.Setenv("MASTER_KEY", base64.StdEncoding.EncodeToString(key))
	st := testStore(t)
	sealer, err := NewSealer()
	if err != nil {
		t.Fatal(err)
	}
	p := NewProxy(sealer, st)
	_, b64, _ := testAppKey(t)
	app := &githubApp{id: "1234567", slug: "attesttag", clientID: "cid", clientSecret: "csecret"}
	if app.key, err = parseGitHubAppKey(b64, ""); err != nil {
		t.Fatal(err)
	}
	p.ghApp = app
	seedInstall(t, st, orgID, fakeInstallation, "acme")

	f := &fakeGitHub{mux: http.NewServeMux(), perms: map[string]string{}}
	saved := oauthHTTPClient
	oauthHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		if r.Method != "POST" || r.URL.Path != fmt.Sprintf("/app/installations/%d/access_tokens", fakeInstallation) {
			rec.WriteHeader(404)
			return rec.Result(), nil
		}
		var m mintRequest
		json.NewDecoder(r.Body).Decode(&m)
		f.mu.Lock()
		f.mints = append(f.mints, m)
		tok := fmt.Sprintf("ghs_minted%d", len(f.mints))
		f.perms[tok] = permissionList(m.Permissions)
		f.mu.Unlock()
		rec.WriteHeader(201)
		fmt.Fprintf(rec, `{"token":%q,"expires_at":%q}`, tok, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		return rec.Result(), nil
	})}
	t.Cleanup(func() { oauthHTTPClient = saved })
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
		f.mu.Unlock()
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, r)
		return rec.Result(), nil
	})
	return p, f
}

// reviewGitHubFixture is the client for acme/web#7.
func reviewGitHubFixture(t *testing.T) (*reviewGitHub, *fakeGitHub) {
	t.Helper()
	p, f := reviewProxyFixture(t)
	conn, err := p.reviewConnection(fakeInstallation, "acme/web")
	if err != nil {
		t.Fatal(err)
	}
	c, err := newReviewGitHub(p, orgID, conn, "acme/web", 7)
	if err != nil {
		t.Fatal(err)
	}
	return c, f
}

const (
	readPerms = "contents:read,pull_requests:read"
	postPerms = "pull_requests:write"
)

// A pull request's files come a hundred to a page, and the client reads until GitHub runs out —
// under the read-only token, scoped to the one repository.
func TestReviewGitHubPaginatesThePullRequestsFiles(t *testing.T) {
	c, f := reviewGitHubFixture(t)
	f.mux.HandleFunc("GET /repos/acme/web/pulls/7/files", func(w http.ResponseWriter, r *http.Request) {
		if got := f.permsOf(r); got != readPerms {
			t.Errorf("files read with a token for %q, want %q", got, readPerms)
		}
		if r.URL.Query().Get("per_page") != "100" {
			t.Errorf("per_page = %q", r.URL.Query().Get("per_page"))
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		n := map[int]int{1: 100, 2: 100, 3: 50}[page]
		var files []map[string]any
		for i := range n {
			files = append(files, map[string]any{"filename": fmt.Sprintf("src/f%d_%d.go", page, i), "status": "modified",
				"additions": 1, "deletions": 0, "patch": "@@ -1,1 +1,2 @@\n context\n+added"})
		}
		json.NewEncoder(w).Encode(files)
	})
	got, err := c.PullFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 250 || got.Truncated {
		t.Fatalf("read %d files (truncated %v), want all 250", len(got.Files), got.Truncated)
	}
	if got.Files[249].Path != "src/f3_49.go" || got.Files[0].Additions != 1 {
		t.Errorf("files did not decode straight into review.File: %+v / %+v", got.Files[0], got.Files[249])
	}
	if err := got.Files[0].Parse(); err != nil || len(got.Files[0].Hunks) != 1 {
		t.Errorf("a decoded patch does not parse: %v", err)
	}
	if n := len(f.sent()); n != 3 {
		t.Errorf("made %d requests for three pages", n)
	}
	if len(f.mints) != 1 || !slices.Equal(f.mints[0].Repositories, []string{"web"}) {
		t.Errorf("the token was not scoped to the repository: %+v", f.mints)
	}
}

// GitHub lists 3,000 files at most, and a review fetches 4 MiB of them at most. Past either the
// review says it did not see everything — which caps its score — rather than claiming it did.
func TestReviewGitHubPullFilesStopsAtItsCaps(t *testing.T) {
	t.Run("files", func(t *testing.T) {
		c, f := reviewGitHubFixture(t)
		f.mux.HandleFunc("GET /repos/acme/web/pulls/7/files", func(w http.ResponseWriter, r *http.Request) {
			var files []map[string]any
			for i := range 100 {
				files = append(files, map[string]any{"filename": fmt.Sprintf("f%d", i), "status": "added"})
			}
			json.NewEncoder(w).Encode(files)
		})
		got, err := c.PullFiles(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Files) != reviewFilesMax || !got.Truncated {
			t.Errorf("read %d files (truncated %v), want %d and truncated", len(got.Files), got.Truncated, reviewFilesMax)
		}
		if n := len(f.sent()); n != reviewFilesMax/reviewFilesPerPage {
			t.Errorf("made %d requests, want %d", n, reviewFilesMax/reviewFilesPerPage)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		c, f := reviewGitHubFixture(t)
		big := "@@ -0,0 +1,1 @@\n+" + strings.Repeat("x", 30<<10)
		f.mux.HandleFunc("GET /repos/acme/web/pulls/7/files", func(w http.ResponseWriter, r *http.Request) {
			var files []map[string]any
			for i := range 100 {
				files = append(files, map[string]any{"filename": fmt.Sprintf("f%d", i), "status": "added", "additions": 1, "patch": big})
			}
			json.NewEncoder(w).Encode(files)
		})
		got, err := c.PullFiles(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// Each page is about 3 MiB: the first fits, the second crosses the cap and is dropped whole.
		if len(got.Files) != 100 || !got.Truncated {
			t.Errorf("read %d files (truncated %v), want the first page and truncated", len(got.Files), got.Truncated)
		}
	})
}

// A file is read as it is. redact folds a private key into one line, which is right for a prompt
// and wrong for a review: every line after it would sit one comment-anchor off from the diff's.
// The bare BEGIN/END pair is assembled here so no scanner reads this file as holding a key.
func TestReviewGitHubRawReadsKeepLineNumbers(t *testing.T) {
	c, f := reviewGitHubFixture(t)
	begin, end := "-----BEGIN "+"RSA PRIVATE KEY-----", "-----END "+"RSA PRIVATE KEY-----"
	file := "package config\n\nconst key = `\n" + begin + "\nnot-a-real-key-0001\nnot-a-real-key-0002\n" + end + "`\n\nfunc after() {}\n"
	const head = "0123456789abcdef0123456789abcdef01234567"
	f.mux.HandleFunc("GET /repos/acme/web/contents/internal/config/key.go", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ref") != head || r.Header.Get("Accept") != "application/vnd.github.raw" {
			t.Errorf("raw read asked for ref %q with Accept %q", r.URL.Query().Get("ref"), r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "application/vnd.github.raw; charset=utf-8")
		io.WriteString(w, file)
	})
	patch := "@@ -0,0 +1,9 @@\n+package config\n+\n+const key = `\n+" + begin + "\n+not-a-real-key-0001\n+not-a-real-key-0002\n+" + end + "`\n+\n+func after() {}"
	f.mux.HandleFunc("GET /repos/acme/web/pulls/7/files", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{{"filename": "internal/config/key.go", "status": "added", "additions": 9, "patch": patch}})
	})

	// The test means something only because redaction would have moved the line.
	if strings.Count(redact(file), "\n") == strings.Count(file, "\n") {
		t.Fatal("the fixture does not exercise the multi-line redaction")
	}
	got, truncated, err := c.FileAt(context.Background(), "internal/config/key.go", head, 0)
	if err != nil || truncated {
		t.Fatalf("FileAt: %v (truncated %v)", err, truncated)
	}
	if got != file {
		t.Fatalf("the raw read changed the file:\n%q\nwant\n%q", got, file)
	}
	if lines := strings.Split(got, "\n"); lines[8] != "func after() {}" {
		t.Errorf("line 9 is %q", lines[8])
	}

	// The same key inside a patch, which arrives as one JSON string: redacted, the hunk would no
	// longer add up to its header and the file could not be reviewed at all.
	files, err := c.PullFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fl := files.Files[0]
	if err := fl.Parse(); err != nil {
		t.Fatalf("the patch does not parse after a raw read: %v", err)
	}
	last := fl.Hunks[0].Lines[len(fl.Hunks[0].Lines)-1]
	if last.New != 9 || last.Text != "func after() {}" {
		t.Errorf("the last added line is R%d %q, want R9", last.New, last.Text)
	}
	if !review.ValidAnchor(fl.Hunks, review.Right, 0, 9) {
		t.Error("a comment on the line after the key would be refused")
	}
}

// GitHub's "not now" is a wait, not a failure: the run goes back to the queue for as long as
// GitHub asked, and spends nothing doing so. Anything else at 400 or more is GitHub's refusal,
// with its own reason.
func TestReviewGitHubSurfacesRateLimitsAsAWait(t *testing.T) {
	c, f := reviewGitHubFixture(t)
	answer := 0
	f.mux.HandleFunc("GET /repos/acme/web/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		answer++
		switch answer {
		case 1:
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(429)
			io.WriteString(w, `{"message":"You have exceeded a secondary rate limit."}`)
		case 2:
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(90*time.Second).Unix(), 10))
			w.WriteHeader(403)
			io.WriteString(w, `{"message":"API rate limit exceeded"}`)
		case 3:
			w.WriteHeader(404)
			io.WriteString(w, `{"message":"Not Found"}`)
		default:
			io.WriteString(w, `{"number":7,"state":"open","head":{"sha":"abc","repo":{"full_name":"someone/web"}},"base":{"sha":"def","repo":{"full_name":"acme/web","private":true}},"user":{"login":"octocat","type":"User"}}`)
		}
	})
	ctx := context.Background()
	var wait *githubRetryError
	if _, err := c.Pull(ctx); !errors.As(err, &wait) || wait.Wait != 30*time.Second || wait.Status != 429 {
		t.Errorf("a 429 with Retry-After: %v", err)
	}
	if _, err := c.Pull(ctx); !errors.As(err, &wait) || wait.Wait < 60*time.Second || wait.Status != 403 {
		t.Errorf("a spent primary limit: %v", err)
	}
	_, err := c.Pull(ctx)
	if errors.As(err, &wait) || !isGitHubStatus(err, 404) || !strings.Contains(err.Error(), "Not Found") {
		t.Errorf("a plain 404 should be GitHub's refusal, not a wait: %v", err)
	}
	pr, err := c.Pull(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !pr.IsFork() || pr.User.Login != "octocat" || pr.Base.Repo == nil || !pr.Base.Repo.Private {
		t.Errorf("pull request decoded as %+v", pr)
	}
}

// The allowlist is checked before anything leaves: this repository, this pull request, these
// endpoints, nothing else — whatever a bug or the code under review talks the engine into.
func TestReviewGitHubRefusesEverythingOffItsAllowlist(t *testing.T) {
	c, f := reviewGitHubFixture(t)
	ctx := context.Background()
	for _, tc := range []struct {
		method, rel string
		q           url.Values
	}{
		{"GET", "pulls/8", nil},                               // another pull request
		{"GET", "../../acme/api/pulls/7", nil},                // another repository, by dot segments
		{"PUT", "pulls/7/merge", nil},                         // merging
		{"POST", "pulls/7/reviews/1/events", nil},             // submitting a pending review as anything
		{"DELETE", "issues/comments/5", nil},                  // deleting
		{"PATCH", "issues/comments/5", nil},                   // editing a comment that is not ours
		{"POST", "issues/comments/6/reactions", nil},          // reacting to a comment not on this PR
		{"POST", "pulls/comments/6/reactions", nil},           // reacting to an inline comment not known on this PR
		{"GET", "pulls/8/reviews/1/comments", nil},            // another pull request's review
		{"POST", "pulls/7/reviews/1/comments", nil},           // adding to somebody's pending review
		{"PATCH", "pulls/comments/5", nil},                    // editing an inline comment
		{"GET", "git/trees/main", nil},                        // a tree by branch, not by commit
		{"GET", "contents/a/../../secrets", nil},              // a file path that climbs
		{"GET", "pulls/7/files", url.Values{"q": {"x"}}},      // a parameter it does not take
		{"GET", "issues/7/comments/extra", nil},               // a longer path than any route
		{"POST", "issues/7/comments", url.Values{"x": {"1"}}}, // a write with a query
	} {
		if _, err := c.do(ctx, tc.method, tc.rel, tc.q, nil, "", 0); err == nil || !strings.Contains(err.Error(), "code review may not") {
			t.Errorf("%s %s: %v", tc.method, tc.rel, err)
		}
	}
	for _, raw := range []string{
		"https://api.github.com/repos/acme/api/pulls/7",       // another repository outright
		"https://api.github.com/repos/acme/web-api/pulls/7",   // a repository with our name as a prefix
		"https://evil.example/repos/acme/web/pulls/7",         // another host
		"http://api.github.com/repos/acme/web/pulls/7",        // another scheme
		"https://api.github.com/repos/acme/web/pulls/07",      // the number written another way
		"https://api.github.com/repos/acme/web/issues/7/lock", // locking the conversation
	} {
		u, _ := url.Parse(raw)
		if err := c.allow("GET", u); err == nil {
			t.Errorf("GET %s was allowed", raw)
		}
	}
	if sent := f.sent(); len(sent) != 0 {
		t.Fatalf("refused requests reached GitHub: %v", sent)
	}
	if f.mintCount() != 0 {
		t.Error("a token was minted for requests that were all refused")
	}

	// What the review itself makes known opens exactly those doors.
	f.mux.HandleFunc("PATCH /repos/acme/web/issues/comments/5", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":5}`)
	})
	f.mux.HandleFunc("POST /repos/acme/web/issues/comments/6/reactions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		io.WriteString(w, `{"id":1,"content":"eyes"}`)
	})
	c.AdoptIssueComment(5)
	c.KnowIssueComment(6)
	if _, err := c.EditIssueComment(ctx, 5, "edited"); err != nil {
		t.Errorf("editing an adopted comment: %v", err)
	}
	if err := c.ReactToIssueComment(ctx, 6, "eyes"); err != nil {
		t.Errorf("reacting to a known comment: %v", err)
	}
	if _, err := c.EditIssueComment(ctx, 6, "edited"); err == nil {
		t.Error("a comment merely known to be on the PR was edited")
	}
	if err := c.ReactToIssueComment(ctx, 6, "<img>"); err == nil {
		t.Error("a reaction GitHub does not have was sent")
	}
	// An inline comment is reacted to once it is known to be on this pull request, and its id
	// being known as an issue comment does not count: GitHub numbers the two apart.
	f.mux.HandleFunc("POST /repos/acme/web/pulls/comments/6/reactions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		io.WriteString(w, `{"id":2,"content":"+1"}`)
	})
	if err := c.ReactToReviewComment(ctx, 6, "+1"); err == nil {
		t.Error("an inline comment known only as an issue comment's id was reacted to")
	}
	c.KnowReviewComment(6)
	if err := c.ReactToReviewComment(ctx, 6, "+1"); err != nil {
		t.Errorf("reacting to a known inline comment: %v", err)
	}
}

// A review is one COMMENT, posted against the commit that was reviewed, under the token that can
// write pull requests and nothing more; its marker goes through, since the poster signs it.
func TestReviewGitHubPostsACommentReviewUnderThePostToken(t *testing.T) {
	c, f := reviewGitHubFixture(t)
	const head = "0123456789abcdef0123456789abcdef01234567"
	var posted map[string]any
	f.mux.HandleFunc("POST /repos/acme/web/pulls/7/reviews", func(w http.ResponseWriter, r *http.Request) {
		if got := f.permsOf(r); got != postPerms {
			t.Errorf("review posted with a token for %q, want %q", got, postPerms)
		}
		json.NewDecoder(r.Body).Decode(&posted)
		io.WriteString(w, `{"id":99,"state":"COMMENTED","commit_id":"`+head+`"}`)
	})
	f.mux.HandleFunc("POST /repos/acme/web/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		io.WriteString(w, `{"id":555,"body":"summary"}`)
	})
	f.mux.HandleFunc("PATCH /repos/acme/web/issues/comments/555", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":555,"body":"summary, edited"}`)
	})
	f.mux.HandleFunc("POST /repos/acme/web/pulls/7/comments/31/replies", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		io.WriteString(w, `{"id":32,"in_reply_to_id":31}`)
	})
	ctx := context.Background()
	marker := "<!-- attest_tag:run=0123456789abcdef0123456789abcdef.0123456789abcdef -->"
	rv, err := c.PostReview(ctx, head, marker, []reviewInlineComment{
		{Path: "src/totals.ts", Line: 52, Side: "RIGHT", StartLine: 40, Body: "**P1 · Older totals overwrite newer ones**"},
		{Path: "src/auth.ts", Line: 12, Side: "LEFT", Body: "**P0 · Removed auth check**"},
	})
	if err != nil || rv.ID != 99 {
		t.Fatalf("PostReview: %+v %v", rv, err)
	}
	if posted["event"] != "COMMENT" || posted["commit_id"] != head || posted["body"] != marker {
		t.Errorf("posted %v", posted)
	}
	comments, _ := posted["comments"].([]any)
	if len(comments) != 2 {
		t.Fatalf("posted comments %v", posted["comments"])
	}
	first := comments[0].(map[string]any)
	if first["start_line"] != float64(40) || first["line"] != float64(52) || first["side"] != "RIGHT" {
		t.Errorf("first comment %v", first)
	}
	if _, ok := comments[1].(map[string]any)["start_line"]; ok {
		t.Error("a one-line comment carried a start_line")
	}

	for _, bad := range []struct {
		commit string
		ic     reviewInlineComment
	}{
		{"main", reviewInlineComment{Path: "a", Line: 1, Side: "RIGHT", Body: "x"}},
		{head, reviewInlineComment{Path: "a", Line: 5, Side: "RIGHT", StartLine: 5, Body: "x"}},
		{head, reviewInlineComment{Path: "a", Line: 5, Side: "MIDDLE", Body: "x"}},
		{head, reviewInlineComment{Path: "a", Line: 5, Side: "RIGHT", Body: strings.Repeat("x", reviewCommentMaxLen+1)}},
	} {
		if _, err := c.PostReview(ctx, bad.commit, marker, []reviewInlineComment{bad.ic}); err == nil {
			t.Errorf("posted %q %+v", bad.commit, bad.ic)
		}
	}

	sc, err := c.CreateIssueComment(ctx, "summary")
	if err != nil || sc.ID != 555 {
		t.Fatalf("CreateIssueComment: %+v %v", sc, err)
	}
	if _, err := c.EditIssueComment(ctx, 555, "summary, edited"); err != nil {
		t.Errorf("the review could not edit the comment it just made: %v", err)
	}
	if reply, err := c.ReplyToReviewComment(ctx, 31, "Withdrawn."); err != nil || reply.InReplyToID != 31 {
		t.Errorf("ReplyToReviewComment: %+v %v", reply, err)
	}
	for _, m := range f.mints {
		if m.Permissions["contents"] != "" && m.Permissions["pull_requests"] == "write" {
			t.Errorf("a write token was minted with contents too: %v", m.Permissions)
		}
	}
}

// The lists are read page by page, and the conversation's comments become ones the review may
// react to — never ones it may edit, which takes AdoptIssueComment.
func TestReviewGitHubListsPagesOfComments(t *testing.T) {
	c, f := reviewGitHubFixture(t)
	f.mux.HandleFunc("GET /repos/acme/web/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		n := map[int]int{1: 100, 2: 3}[page]
		var out []map[string]any
		for i := range n {
			out = append(out, map[string]any{"id": page*1000 + i, "body": "hi", "user": map[string]any{"login": "octocat", "type": "User"}})
		}
		json.NewEncoder(w).Encode(out)
	})
	f.mux.HandleFunc("GET /repos/acme/web/pulls/7/reviews", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"id":1,"body":"x","commit_id":"abc","user":{"login":"attesttag[bot]","type":"Bot"}}]`)
	})
	ctx := context.Background()
	list, more, err := c.IssueComments(ctx)
	if err != nil || more || len(list) != 103 || list[102].ID != 2002 {
		t.Fatalf("IssueComments: %d comments, more %v, %v", len(list), more, err)
	}
	reviews, _, err := c.Reviews(ctx)
	if err != nil || len(reviews) != 1 || reviews[0].User.Type != "Bot" {
		t.Fatalf("Reviews: %+v %v", reviews, err)
	}
	f.mux.HandleFunc("POST /repos/acme/web/issues/comments/1000/reactions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		io.WriteString(w, `{}`)
	})
	if err := c.ReactToIssueComment(ctx, 1000, "+1"); err != nil {
		t.Errorf("reacting to a listed comment: %v", err)
	}
	if _, err := c.EditIssueComment(ctx, 1000, "mine now"); err == nil {
		t.Error("listing a comment made it the review's to edit")
	}
}

// The client is for one repository through the App, and refuses to be built any other way.
func TestReviewGitHubNeedsAnAppConnectionForItsRepository(t *testing.T) {
	p, _ := reviewProxyFixture(t)
	conn, err := p.reviewConnection(fakeInstallation, "acme/web")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newReviewGitHub(p, orgID, conn, "acme/api", 7); err == nil {
		t.Error("a client for acme/api was built on acme/web's connection")
	}
	pasted := *conn
	pasted.CredType = "bearer"
	if _, err := newReviewGitHub(p, orgID, &pasted, "acme/web", 7); err == nil {
		t.Error("a client was built on a pasted token")
	}
	if _, err := newReviewGitHub(p, orgID, conn, "acme/web", 0); err == nil {
		t.Error("a client was built for no pull request")
	}
	if _, err := p.reviewConnection(fakeInstallation, "acme/.."); err == nil {
		t.Error("a connection was built for a repository named ..")
	}
}
