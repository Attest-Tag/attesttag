package app

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"attesttag/internal/review"
)

// GitHub I/O for code review: everything a review reads from and writes to one pull request.
//
// A review runs on behalf of nobody in particular — a pull request was opened, somebody typed a
// command — and posts without anybody pressing Confirm, so the guarantees a person's confirmation
// gives elsewhere are made here instead, in code:
//
//   - Every request goes through Proxy.Do, so it is audited and its credential is injected like any
//     other, and it names the one connection the review was given and a token purpose: reads mint
//     a read-only token, writes one that can write pull requests and nothing else (github_token.go).
//   - Every URL is checked against reviewRoutes before it is sent: this repository, this pull
//     request, these endpoints. A review cannot be steered — by the code it reads, or by a bug in
//     the engine above — into reading another repository, merging, or deleting anything. An issue
//     comment's own URL does not name its pull request, so one is only edited when the review
//     created it or adopted it as its own, and only reacted to when it is known to be on this pull
//     request. An inline comment's reactions URL does not name it either, and is held to the same:
//     only a comment this client listed on this pull request may be reacted to.
//   - Only a review is ever posted, and only as a COMMENT: never an approval or a request for
//     changes, which would turn an advisory reader into a gate somebody could talk into opening.
//   - Beside it, the App's own signals (review_signals.go): reactions on the pull request itself,
//     taken off again only when the client put them on, and one check
//     run per review, written under a token for checks alone and updated only when the client
//     created it or found it as the App's, by the run it belongs to. A check is never concluded as
//     a failure, for the reason a review is never a request for changes.
//
// Reads are raw (ProxyRequest.Raw): the bytes are parsed here, line numbers have to match the head
// exactly, and redact on a JSON page can take the document apart. Nothing this client returns has
// been redacted, so whatever of it reaches a model must be masked first, line by line.
const (
	reviewGitHubBase = "https://api.github.com"
	// GitHub lists at most 3,000 files for a pull request, a hundred to a page. The byte cap is what
	// one review fetches of them: patches of a release-sized pull request run to megabytes, and a
	// Cloud Run instance with a gigabyte has better uses for it.
	reviewFilesPerPage  = 100
	reviewFilesMax      = 3000
	reviewFilesMaxBytes = 4 << 20
	// A list of reviews or comments is read to find our own among them. Ten pages is a thousand,
	// beyond which a pull request is a discussion forum and the stored ids are what is relied on.
	reviewListPages = 10
	// GitHub refuses a comment body longer than this (in characters), with a 422 for the whole
	// review; refusing it here says which one.
	reviewCommentMaxLen = 65536
)

// reviewGitHub is one review's client for one pull request. It is not safe to share between
// pull requests by design: the repository and the number are the allowlist.
type reviewGitHub struct {
	proxy *Proxy
	orgID int64
	conn  *Connection
	acc   *Access
	base  string // reviewGitHubBase; a field so a test can say where GitHub is
	repo  string // owner/name, as the connection holds it
	pr    int

	mu   sync.Mutex
	onPR map[int64]bool // issue comments known to be on this pull request
	ours map[int64]bool // issue comments this review may edit: created by it, or adopted
	// inline are review comments known to be on this pull request — listed from it — which is
	// what a reaction to one needs, its URL naming no pull request. A separate set from onPR:
	// GitHub numbers issue comments and review comments apart, and one id can be both.
	inline map[int64]bool
	// threads are review threads known to be on this pull request, by their GraphQL node ids: listed
	// from it (ReviewThreads), or stored from a thread delivery about it (KnowThread). A node id names
	// its thread across all of GitHub, so resolving one takes knowing it is this pull request's.
	threads map[string]bool
	// rootThreads is the pull request's threads by the id of the comment that opened each, once they
	// have been listed (ThreadOf): a run that closes several findings lists them once.
	rootThreads map[int64]githubThread
	// reactions are the App's own reactions on the pull request itself, which this client put on,
	// and the only ones it may take off. checks are the App's own check runs, created by this client
	// or found by the run they belong to, and the only ones it may update.
	reactions map[int64]bool
	checks    map[int64]bool
}

// newReviewGitHub makes the client for pull request pr of repo, reached through conn. conn must be
// an App connection scoped to that repository (reviewConnection makes one): a pasted token posts
// as whoever pasted it, and a token minted for the whole installation reaches every repository in
// it.
func newReviewGitHub(p *Proxy, orgID int64, conn *Connection, repo string, pr int) (*reviewGitHub, error) {
	switch {
	case p == nil || conn == nil:
		return nil, errors.New("code review needs a proxy and a connection")
	case conn.CredType != "github_app":
		return nil, fmt.Errorf("code review reaches GitHub only through the GitHub App, and %s is not an App connection", conn.Name)
	case !validGitHubRepo(repo) || !strings.EqualFold(conn.Repo, repo):
		return nil, fmt.Errorf("the connection for a review must be scoped to its repository %q, not %q", repo, conn.Repo)
	case pr <= 0:
		return nil, errors.New("a review needs a pull request number")
	}
	return &reviewGitHub{proxy: p, orgID: orgID, conn: conn, base: reviewGitHubBase, repo: conn.Repo, pr: pr,
		acc:  &Access{Rules: []Rule{{Conn: conn}}},
		onPR: map[int64]bool{}, ours: map[int64]bool{}, inline: map[int64]bool{}, threads: map[string]bool{},
		reactions: map[int64]bool{}, checks: map[int64]bool{}}, nil
}

// reviewConnection is the connection a review reaches a repository through: the installation the
// delivery came from, narrowed to that one repository. It is built for the run and never stored —
// a repository the installation gains is reviewed under its connection's settings without anybody
// connecting it first (store_review.go), so there may be no stored row to borrow — and it is the
// same shape as installAccess's, with the repository set so every token it mints is scoped to it.
// Whether the installation is this organisation's is checked where every token is minted
// (installBelongsTo), not here.
func (p *Proxy) reviewConnection(installationID int64, repo string) (*Connection, error) {
	if installationID <= 0 || !validGitHubRepo(repo) {
		return nil, fmt.Errorf("a review connection needs an installation and an owner/name repository, not %d and %q", installationID, repo)
	}
	plain, err := json.Marshal(&Secret{InstallationID: installationID})
	if err != nil {
		return nil, err
	}
	enc, err := p.sealer.Seal(plain)
	if err != nil {
		return nil, err
	}
	return &Connection{Name: "github-review", Preset: "github", CredType: "github_app", Repo: repo,
		AllowedHosts: []string{"api.github.com"}, GitHubInstallationID: installationID, Status: "active", secretEnc: enc}, nil
}

func validGitHubRepo(repo string) bool {
	if !repoRe.MatchString(repo) {
		return false
	}
	name := repo[strings.IndexByte(repo, '/')+1:]
	return name != "." && name != ".."
}

// KnowIssueComment records an issue comment as one on this pull request — the command a delivery
// for it carried, say — so it may be reacted to.
func (c *reviewGitHub) KnowIssueComment(id int64) {
	c.mu.Lock()
	c.onPR[id] = true
	c.mu.Unlock()
}

// KnowReviewComment records an inline comment as one on this pull request — the reply a delivery for
// it carried, say — so it may be reacted to.
func (c *reviewGitHub) KnowReviewComment(id int64) {
	c.mu.Lock()
	c.inline[id] = true
	c.mu.Unlock()
}

// KnowThread records a review thread as one on this pull request — the thread node id a
// pull_request_review_thread delivery about it carried, stored on the finding — so it may be
// resolved without listing them all first.
func (c *reviewGitHub) KnowThread(nodeID string) {
	if !githubNodeID.MatchString(nodeID) {
		return
	}
	c.mu.Lock()
	c.threads[nodeID] = true
	c.mu.Unlock()
}

// AdoptIssueComment records an issue comment as the review's own, which it may then edit: the
// summary id stored on review_prs, or a comment found by its marker. Whoever calls it has already
// checked what the marker cannot prove on its own — that the bot wrote the comment it is on
// (review.VerifiedMarker).
func (c *reviewGitHub) AdoptIssueComment(id int64) {
	c.mu.Lock()
	c.onPR[id], c.ours[id] = true, true
	c.mu.Unlock()
}

// ---- the allowlist ----

// reviewRoute is one request the client may make: a method and a path below /repos/{owner}/{name}/,
// in which {n} is the client's own pull request, {id} a GitHub id, {sha} a full commit id and
// {path} the rest of a file path. query lists the parameters it may carry. comment says what an
// {id} has to be: "pr", an issue comment known to be on this pull request; "ours", one this review
// may edit; "inline", a review comment listed from this pull request; "reaction", a reaction the
// client put on the pull request; or "check", one of the App's own check runs.
type reviewRoute struct {
	method, pattern string
	query           []string
	comment         string
}

var reviewRoutes = []reviewRoute{
	{"GET", "pulls/{n}", nil, ""},
	{"GET", "pulls/{n}/files", []string{"per_page", "page"}, ""},
	{"GET", "git/trees/{sha}", []string{"recursive"}, ""},
	{"GET", "contents/{path}", []string{"ref"}, ""},
	{"GET", "pulls/{n}/reviews", []string{"per_page", "page"}, ""},
	{"GET", "pulls/{n}/comments", []string{"per_page", "page"}, ""},
	{"GET", "issues/{n}/comments", []string{"per_page", "page"}, ""},
	{"GET", "pulls/{n}/reviews/{id}/comments", []string{"per_page", "page"}, ""},
	{"POST", "pulls/{n}/reviews", nil, ""},
	{"POST", "pulls/{n}/comments/{id}/replies", nil, ""},
	{"POST", "issues/{n}/comments", nil, ""},
	{"PATCH", "issues/comments/{id}", nil, "ours"},
	{"POST", "issues/comments/{id}/reactions", nil, "pr"},
	{"POST", "pulls/comments/{id}/reactions", nil, "inline"},
	{"POST", "issues/{n}/reactions", nil, ""},
	{"DELETE", "issues/{n}/reactions/{id}", nil, "reaction"},
	// Whether somebody may have a fix pushed to the pull request's branch (review_fix.go): their
	// permission on this repository, a read every installation token may make (Metadata).
	{"GET", "collaborators/{login}/permission", nil, ""},
}

// reviewCheckRoutes are the review's check run, and go out under the token for checks alone
// (githubPurposeReviewChecks) — never under one that can write pull requests, nor the other way
// round. A check run belongs to a commit, not to a pull request, so the repository is all the path
// can be held to; what the client writes there is its own, made by CreateCheckRun.
var reviewCheckRoutes = []reviewRoute{
	{"GET", "commits/{sha}/check-runs", []string{"check_name", "app_id", "filter", "per_page"}, ""},
	{"POST", "check-runs", nil, ""},
	{"PATCH", "check-runs/{id}", nil, "check"},
}

var commitSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// githubUserLogin is a GitHub user's login as GitHub allows one: letters, digits and single
// hyphens, starting with a letter or digit, at most 39 characters. Nothing else may be a path
// segment of a request about a user — no "..", no slash, no "[bot]".
var githubUserLogin = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9]|-[A-Za-z0-9]){0,38}$`)

// allow reports why u may not be requested with method, or nil when it may.
func (c *reviewGitHub) allow(method string, u *url.URL) error {
	_, err := c.route(method, u)
	return err
}

// route checks u against the allowlist and returns the token purpose the request goes out under:
// a check run's own (reviewCheckRoutes), or "" for one that follows from the method (reviewSend).
func (c *reviewGitHub) route(method string, u *url.URL) (purpose string, err error) {
	base, err := url.Parse(c.base)
	if err != nil {
		return "", err
	}
	refuse := func(why string) error {
		return fmt.Errorf("code review may not %s %s: %s", method, u.Path, why)
	}
	if u.Scheme != base.Scheme || !strings.EqualFold(u.Host, base.Host) {
		return "", refuse("not GitHub's API")
	}
	segs := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	if len(segs) < 4 || segs[0] != "repos" || !strings.EqualFold(segs[1]+"/"+segs[2], c.repo) {
		return "", refuse("not this review's repository")
	}
	rest := segs[3:]
	for _, table := range []struct {
		routes  []reviewRoute
		purpose string
	}{{reviewRoutes, ""}, {reviewCheckRoutes, githubPurposeReviewChecks}} {
		for _, r := range table.routes {
			if r.method != method {
				continue
			}
			id, ok := c.matchRoute(r.pattern, rest)
			if !ok {
				continue
			}
			for k := range u.Query() {
				if !slices.Contains(r.query, k) {
					return "", refuse("it does not take ?" + k)
				}
			}
			c.mu.Lock()
			known, own, inline, reaction, check := c.onPR[id], c.ours[id], c.inline[id], c.reactions[id], c.checks[id]
			c.mu.Unlock()
			switch {
			case r.comment == "ours" && !own:
				return "", refuse("that comment is not one this review wrote")
			case r.comment == "pr" && !known:
				return "", refuse("that comment is not known to be on this pull request")
			case r.comment == "inline" && !inline:
				return "", refuse("that review comment is not known to be on this pull request")
			case r.comment == "reaction" && !reaction:
				return "", refuse("that reaction is not one this review put on this pull request")
			case r.comment == "check" && !check:
				return "", refuse("that check run is not one this review made")
			}
			return table.purpose, nil
		}
	}
	return "", refuse("not a request code review makes")
}

// matchRoute matches path segments against a pattern, returning the {id} it named, if any.
func (c *reviewGitHub) matchRoute(pattern string, segs []string) (int64, bool) {
	pat := strings.Split(pattern, "/")
	var id int64
	for i, p := range pat {
		if p == "{path}" {
			// The rest of the path, at least one segment, none empty or a dot segment.
			if i >= len(segs) {
				return 0, false
			}
			for _, s := range segs[i:] {
				if dec, err := url.PathUnescape(s); err != nil || s == "" || dec == "." || dec == ".." || strings.Contains(dec, "/") {
					return 0, false
				}
			}
			return id, true
		}
		if i >= len(segs) {
			return 0, false
		}
		s := segs[i]
		switch p {
		case "{n}":
			if s != strconv.Itoa(c.pr) {
				return 0, false
			}
		case "{id}":
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil || n <= 0 || strconv.FormatInt(n, 10) != s {
				return 0, false
			}
			id = n
		case "{sha}":
			if !commitSHA.MatchString(s) {
				return 0, false
			}
		case "{login}":
			if !githubUserLogin.MatchString(s) {
				return 0, false
			}
		default:
			if s != p {
				return 0, false
			}
		}
	}
	return id, len(segs) == len(pat)
}

// ---- errors ----

// githubRetryError is GitHub saying "not now" — a spent rate limit, primary or secondary — with
// how long to wait. It is not a failure of the review: the lane puts the run back to be claimed
// once Wait has passed, and spends no attempt on it.
type githubRetryError struct {
	Status int
	Wait   time.Duration
	// Why is said instead of the rate limit, for a wait that is not GitHub's to ask for: the head
	// of the pull request moving while it was read (readPull).
	Why string
}

func (e *githubRetryError) Error() string {
	if e.Why != "" {
		return fmt.Sprintf("%s; trying again in %s", e.Why, fmtDuration(e.Wait))
	}
	return fmt.Sprintf("GitHub's rate limit is spent (%d); it asks for %s before the next request", e.Status, fmtDuration(e.Wait))
}

// githubAPIError is any other answer of 400 or more, with GitHub's own reason for it. Status is
// what the engine acts on — a 422 on a review is an anchor GitHub refused, a 404 on a file is a
// file that is not there — and Message is for review_runs.error and the console.
type githubAPIError struct {
	Status  int
	Message string
	What    string // "GET pulls/7/files"
}

func (e *githubAPIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("GitHub answered %d to %s", e.Status, e.What)
	}
	return fmt.Sprintf("GitHub answered %d to %s: %s", e.Status, e.What, e.Message)
}

// isGitHubStatus reports whether err is GitHub answering with status.
func isGitHubStatus(err error, status int) bool {
	var e *githubAPIError
	return errors.As(err, &e) && e.Status == status
}

// ---- the request ----

// do sends one request below the repository: rel is the path after /repos/{owner}/{name}/. The
// purpose follows from the route and the method, in this one place: a check run's routes mint
// review_checks, any other read review_read and any other write review_post, so no helper can write
// with a token that was meant to read, or reach checks with one meant for pull requests.
func (c *reviewGitHub) do(ctx context.Context, method, rel string, q url.Values, body any, accept string, maxBytes int) (*ProxyResponse, error) {
	u, err := url.Parse(c.base + "/repos/" + c.repo + "/" + rel)
	if err != nil {
		return nil, err
	}
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}
	purpose, err := c.route(method, u)
	if err != nil {
		return nil, err
	}
	var raw string
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		raw = string(b)
	}
	what := method + " " + truncate(rel, 120)
	if purpose != "" {
		return reviewSendAs(ctx, c.proxy, c.orgID, c.acc, c.conn, purpose, method, u.String(), what, raw, accept, maxBytes)
	}
	return reviewSend(ctx, c.proxy, c.orgID, c.acc, c.conn, method, u.String(), what, raw, accept, maxBytes)
}

// reviewSend sends one of code review's requests to GitHub through the proxy, for a pull request's
// client and a context repository's reader alike, once the caller has checked the URL against its
// own allowlist — a pull request's routes, or a repository's four reads. What they share is here,
// so it cannot drift between them: the headers, the token purpose that follows from the method, and
// GitHub's answer turned into the two errors the lane acts on. what names the request in an error.
func reviewSend(ctx context.Context, p *Proxy, orgID int64, acc *Access, conn *Connection, method, u, what, body, accept string,
	maxBytes int) (*ProxyResponse, error) {
	purpose := githubPurposeReviewRead
	if method != "GET" {
		purpose = githubPurposeReviewPost
	}
	return reviewSendAs(ctx, p, orgID, acc, conn, purpose, method, u, what, body, accept, maxBytes)
}

// reviewSendAs is reviewSend under a purpose its caller names: GraphQL reads and writes alike with a
// POST, so its caller says which of the two each operation is (reviewGitHub.graphql).
func reviewSendAs(ctx context.Context, p *Proxy, orgID int64, acc *Access, conn *Connection, purpose, method, u, what, body,
	accept string, maxBytes int) (*ProxyResponse, error) {
	req := ProxyRequest{Method: method, URL: u, Connection: conn.Name, Raw: true, MaxBytes: maxBytes, Body: body, Purpose: purpose,
		Headers: map[string]string{"Accept": cmp.Or(accept, "application/vnd.github+json"), "X-GitHub-Api-Version": "2022-11-28"}}
	if purpose == githubPurposeReviewPost {
		// The review poster is the one caller whose markers reach GitHub: it signs them.
		req.ReviewPoster = true
	}
	// confirmed: these are the review's own requests, which a person turned on for this repository
	// in the console, and every one of them has passed its caller's allowlist.
	resp, err := p.Do(ctx, orgID, acc, req, ProxyAudit{Requester: "github-review"}, true)
	if err != nil {
		return nil, err
	}
	if resp.Status >= 400 {
		// Decided by the wait GitHub sent rather than the status, as ghResult.limited is: GitHub
		// answers a spent limit with 403 as often as with 429, and a 403 with no wait is a refusal.
		if resp.RetryAfter > 0 {
			return nil, &githubRetryError{Status: resp.Status, Wait: resp.RetryAfter}
		}
		return nil, &githubAPIError{Status: resp.Status, Message: strings.TrimPrefix(ghMessage(resp.Body), ": "), What: what}
	}
	return resp, nil
}

// getJSON reads one page into out. A page cut short by maxBytes is an error rather than half a
// document.
func (c *reviewGitHub) getJSON(ctx context.Context, rel string, q url.Values, maxBytes int, out any) error {
	resp, err := c.do(ctx, "GET", rel, q, nil, "", maxBytes)
	if err != nil {
		return err
	}
	if resp.Truncated {
		return fmt.Errorf("GitHub's answer to %s is larger than code review reads", truncate(rel, 120))
	}
	return json.Unmarshal([]byte(resp.Body), out)
}

// listAll reads every page of a list endpoint, up to reviewListPages. more says GitHub had more
// than that.
func listAll[T any](ctx context.Context, c *reviewGitHub, rel string) (items []T, more bool, err error) {
	for page := 1; page <= reviewListPages; page++ {
		var batch []T
		q := url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if err := c.getJSON(ctx, rel, q, proxyMaxRead, &batch); err != nil {
			return items, false, err
		}
		items = append(items, batch...)
		if len(batch) < 100 {
			return items, false, nil
		}
	}
	return items, true, nil
}

// ---- reads ----

// githubPull is the part of GET /pulls/{n} a review reads. Title and body are the author's, so
// untrusted, and go nowhere near the verifier.
type githubPull struct {
	Number            int           `json:"number"`
	State             string        `json:"state"` // open, closed
	Draft             bool          `json:"draft"`
	Merged            bool          `json:"merged"`
	MergedBy          *githubUser   `json:"merged_by"` // who merged it; nil until it is merged
	Title             string        `json:"title"`
	Body              string        `json:"body"`
	User              githubUser    `json:"user"`
	AuthorAssociation string        `json:"author_association"`
	Head              githubPullRef `json:"head"`
	Base              githubPullRef `json:"base"`
	ChangedFiles      int           `json:"changed_files"`
	Additions         int           `json:"additions"`
	Deletions         int           `json:"deletions"`
	// Labels are what label rules add review types for (review.LabelTypes). Somebody with triage
	// rights put each on; their names choose types and are recorded beside the rule, and never reach
	// a model.
	Labels []githubLabel `json:"labels"`
}

// githubLabel is a label on a pull request, as a delivery, a read of it and a listing all carry it.
type githubLabel struct {
	Name string `json:"name"`
}

// LabelNames are the pull request's labels by name.
func (p *githubPull) LabelNames() []string { return githubLabelNames(p.Labels) }

func githubLabelNames(labels []githubLabel) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l.Name != "" {
			out = append(out, l.Name)
		}
	}
	return out
}

type githubUser struct {
	Login string `json:"login"`
	Type  string `json:"type"` // User, Bot, Organization
}

type githubPullRef struct {
	SHA  string `json:"sha"`
	Ref  string `json:"ref"`
	Repo *struct {
		FullName      string `json:"full_name"`
		Private       bool   `json:"private"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repo"` // nil when the fork it came from has been deleted
}

// IsFork reports whether the pull request comes from another repository. A head whose repository
// is gone counts as one: it was not this repository's branch.
func (p *githubPull) IsFork() bool {
	return p.Head.Repo == nil || p.Base.Repo == nil || !strings.EqualFold(p.Head.Repo.FullName, p.Base.Repo.FullName)
}

// WritePermission reports whether login may push to this repository: GitHub's own answer, its
// legacy base permission admin or write — what maintain is read as too — counting every grant, the
// organisation's and the teams' included. A comment's author_association says none of that: an
// organisation member may only read, and a member whose membership is private reads as NONE.
func (c *reviewGitHub) WritePermission(ctx context.Context, login string) (bool, error) {
	if !githubUserLogin.MatchString(login) {
		return false, nil
	}
	var p struct {
		Permission string `json:"permission"`
	}
	if err := c.getJSON(ctx, "collaborators/"+login+"/permission", nil, 64<<10, &p); err != nil {
		if isGitHubStatus(err, 404) {
			return false, nil // not somebody GitHub knows on this repository
		}
		return false, err
	}
	return p.Permission == "admin" || p.Permission == "write", nil
}

// Pull reads the pull request itself.
func (c *reviewGitHub) Pull(ctx context.Context) (*githubPull, error) {
	var p githubPull
	if err := c.getJSON(ctx, fmt.Sprintf("pulls/%d", c.pr), nil, proxyMaxRead, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// reviewPullFiles is the pull request's changed files, as GitHub lists them against the merge base.
type reviewPullFiles struct {
	Files []review.File
	// Truncated says there are more than these: GitHub's 3,000, or more bytes than one review
	// fetches. The review then cannot claim to have seen every changed line.
	Truncated bool
}

// PullFiles reads the changed files, page by page, until GitHub has no more or a cap is reached.
// The files are as GitHub sent them; File.Parse is the caller's, which decides what a file that
// will not parse means for the review.
func (c *reviewGitHub) PullFiles(ctx context.Context) (*reviewPullFiles, error) {
	out := &reviewPullFiles{}
	used := 0
	for page := 1; len(out.Files) < reviewFilesMax; page++ {
		left := reviewFilesMaxBytes - used
		if left <= 0 {
			out.Truncated = true
			break
		}
		q := url.Values{"per_page": {strconv.Itoa(reviewFilesPerPage)}, "page": {strconv.Itoa(page)}}
		resp, err := c.do(ctx, "GET", fmt.Sprintf("pulls/%d/files", c.pr), q, nil, "", left)
		if err != nil {
			return nil, err
		}
		if resp.Truncated {
			// The page that crossed the byte cap is half a JSON document, and is dropped whole.
			out.Truncated = true
			break
		}
		used += len(resp.Body)
		var batch []review.File
		if err := json.Unmarshal([]byte(resp.Body), &batch); err != nil {
			return nil, fmt.Errorf("reading the pull request's files: %w", err)
		}
		out.Files = append(out.Files, batch...)
		if len(batch) < reviewFilesPerPage {
			return out, nil
		}
	}
	if len(out.Files) >= reviewFilesMax {
		// GitHub lists no more than this however many there are, so a full last page says nothing
		// about whether it was the last.
		out.Files, out.Truncated = out.Files[:reviewFilesMax], true
	}
	return out, nil
}

// reviewTreeEntry is one entry of a recursive tree: a file ("blob"), a directory ("tree") or a
// submodule ("commit").
type reviewTreeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

// Tree lists the repository at a commit, recursively — the repo map. Only a full commit id is
// taken: a branch name is a different tree tomorrow, and the map is cached by what it was read at.
// truncated is GitHub's own flag for a tree too large to list in full.
func (c *reviewGitHub) Tree(ctx context.Context, sha string) (entries []reviewTreeEntry, truncated bool, err error) {
	var t struct {
		Truncated bool              `json:"truncated"`
		Tree      []reviewTreeEntry `json:"tree"`
	}
	if err := c.getJSON(ctx, "git/trees/"+sha, url.Values{"recursive": {"1"}}, proxyMaxRead, &t); err != nil {
		return nil, false, err
	}
	return t.Tree, t.Truncated, nil
}

// FileAt reads one file's bytes at ref, as they are: GitHub's raw media type, read raw, so the
// line numbers are the head's own and match the diff's. truncated says the file is longer than
// maxBytes (0 means githubRawMax) and only its start came back. A file that is not there is a
// githubAPIError with Status 404.
func (c *reviewGitHub) FileAt(ctx context.Context, path, ref string, maxBytes int) (content string, truncated bool, err error) {
	path = strings.TrimLeft(path, "/")
	if path == "" || strings.TrimSpace(ref) == "" {
		return "", false, errors.New("a file read needs a path and a ref")
	}
	if maxBytes <= 0 {
		maxBytes = githubRawMax
	}
	resp, err := c.do(ctx, "GET", "contents/"+pathEscapeSegments(path), url.Values{"ref": {ref}}, nil,
		"application/vnd.github.raw", maxBytes)
	if err != nil {
		return "", false, err
	}
	return resp.Body, resp.Truncated, nil
}

// githubReview is a review object on a pull request.
type githubReview struct {
	ID          int64      `json:"id"`
	NodeID      string     `json:"node_id"`
	Body        string     `json:"body"`
	State       string     `json:"state"`
	CommitID    string     `json:"commit_id"`
	User        githubUser `json:"user"`
	SubmittedAt string     `json:"submitted_at"`
}

// githubComment is an issue comment or a review comment; the fields after HTMLURL are a review
// comment's own.
type githubComment struct {
	ID                  int64      `json:"id"`
	NodeID              string     `json:"node_id"`
	Body                string     `json:"body"`
	User                githubUser `json:"user"`
	AuthorAssociation   string     `json:"author_association"`
	CreatedAt           string     `json:"created_at"`
	UpdatedAt           string     `json:"updated_at"`
	HTMLURL             string     `json:"html_url"`
	PullRequestReviewID int64      `json:"pull_request_review_id,omitempty"`
	InReplyToID         int64      `json:"in_reply_to_id,omitempty"`
	Path                string     `json:"path,omitempty"`
	Line                int        `json:"line,omitempty"`
	Side                string     `json:"side,omitempty"`
	CommitID            string     `json:"commit_id,omitempty"`
}

// Reviews lists the pull request's reviews, to find one a run posted before it could record it.
func (c *reviewGitHub) Reviews(ctx context.Context) ([]githubReview, bool, error) {
	return listAll[githubReview](ctx, c, fmt.Sprintf("pulls/%d/reviews", c.pr))
}

// ReviewComments lists the inline comments on the pull request's diff, and records each as on it.
func (c *reviewGitHub) ReviewComments(ctx context.Context) ([]githubComment, bool, error) {
	list, more, err := listAll[githubComment](ctx, c, fmt.Sprintf("pulls/%d/comments", c.pr))
	c.knowInline(list)
	return list, more, err
}

// ReviewCommentsOf lists the inline comments of one review on the pull request — what a review
// submitted from pending brought with it — and records each as on it.
func (c *reviewGitHub) ReviewCommentsOf(ctx context.Context, reviewID int64) ([]githubComment, bool, error) {
	if reviewID <= 0 {
		return nil, false, errors.New("a review's comments need the review's id")
	}
	list, more, err := listAll[githubComment](ctx, c, fmt.Sprintf("pulls/%d/reviews/%d/comments", c.pr, reviewID))
	c.knowInline(list)
	return list, more, err
}

func (c *reviewGitHub) knowInline(list []githubComment) {
	c.mu.Lock()
	for _, cm := range list {
		c.inline[cm.ID] = true
	}
	c.mu.Unlock()
}

// IssueComments lists the pull request's conversation, and records each comment as on it.
func (c *reviewGitHub) IssueComments(ctx context.Context) ([]githubComment, bool, error) {
	list, more, err := listAll[githubComment](ctx, c, fmt.Sprintf("issues/%d/comments", c.pr))
	c.mu.Lock()
	for _, cm := range list {
		c.onPR[cm.ID] = true
	}
	c.mu.Unlock()
	return list, more, err
}

// ---- writes ----

// reviewInlineComment is one comment of a review, in the shape POST /pulls/{n}/reviews takes.
// Whether its lines are ones GitHub will accept is the engine's to check (review.ValidAnchor)
// before it gets here; this client checks only that it is well-formed.
type reviewInlineComment struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Side      string `json:"side"`
	StartLine int    `json:"start_line,omitempty"`
	StartSide string `json:"start_side,omitempty"`
	Body      string `json:"body"`
}

func (ic reviewInlineComment) check() error {
	side := func(s string) bool { return s == string(review.Right) || s == string(review.Left) }
	switch {
	case ic.Path == "" || ic.Line <= 0 || !side(ic.Side):
		return fmt.Errorf("an inline comment needs a path, a line and a side, not %q:%d %q", ic.Path, ic.Line, ic.Side)
	case ic.StartSide != "" && (ic.StartLine == 0 || !side(ic.StartSide)):
		return fmt.Errorf("start_side %q needs a start line and a side", ic.StartSide)
	case ic.StartLine < 0 || ic.StartLine > 0 && ic.StartLine >= ic.Line && cmp.Or(ic.StartSide, ic.Side) == ic.Side:
		// On one side a range runs downwards, and GitHub refuses one that starts where it ends.
		return fmt.Errorf("an inline comment's start line %d must come before its line %d", ic.StartLine, ic.Line)
	}
	return checkCommentBody(ic.Body)
}

func checkCommentBody(body string) error {
	if strings.TrimSpace(body) == "" {
		return errors.New("a comment needs a body")
	}
	if utf8.RuneCountInString(body) > reviewCommentMaxLen {
		return fmt.Errorf("a comment body is longer than GitHub's %d characters", reviewCommentMaxLen)
	}
	return nil
}

// PostReview posts one review of commitID: body (the run's marker) and its inline comments, as a
// COMMENT. commitID is the head this run reviewed, not the pull request's head now: GitHub places
// each comment against that commit's diff, which is the one its line numbers were checked against.
func (c *reviewGitHub) PostReview(ctx context.Context, commitID, body string, comments []reviewInlineComment) (*githubReview, error) {
	if !commitSHA.MatchString(commitID) {
		return nil, fmt.Errorf("a review is posted against a full commit id, not %q", truncate(commitID, 70))
	}
	if err := checkCommentBody(body); err != nil {
		return nil, err
	}
	for _, ic := range comments {
		if err := ic.check(); err != nil {
			return nil, err
		}
	}
	if comments == nil {
		comments = []reviewInlineComment{}
	}
	in := map[string]any{"commit_id": commitID, "body": body, "event": "COMMENT", "comments": comments}
	resp, err := c.do(ctx, "POST", fmt.Sprintf("pulls/%d/reviews", c.pr), nil, in, "", proxyMaxRead)
	if err != nil {
		return nil, err
	}
	var out githubReview
	return &out, json.Unmarshal([]byte(resp.Body), &out)
}

// ReplyToReviewComment answers in the thread of an inline comment. The pull request is in the URL,
// so GitHub itself refuses a comment id from anywhere else.
func (c *reviewGitHub) ReplyToReviewComment(ctx context.Context, commentID int64, body string) (*githubComment, error) {
	if err := checkCommentBody(body); err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, "POST", fmt.Sprintf("pulls/%d/comments/%d/replies", c.pr, commentID), nil,
		map[string]string{"body": body}, "", proxyMaxRead)
	if err != nil {
		return nil, err
	}
	var out githubComment
	return &out, json.Unmarshal([]byte(resp.Body), &out)
}

// CreateIssueComment posts on the pull request's conversation — the summary, the first time — and
// adopts the new comment, so the same review can edit it.
func (c *reviewGitHub) CreateIssueComment(ctx context.Context, body string) (*githubComment, error) {
	if err := checkCommentBody(body); err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, "POST", fmt.Sprintf("issues/%d/comments", c.pr), nil, map[string]string{"body": body}, "", proxyMaxRead)
	if err != nil {
		return nil, err
	}
	var out githubComment
	if err := json.Unmarshal([]byte(resp.Body), &out); err != nil {
		return nil, err
	}
	if out.ID > 0 {
		c.AdoptIssueComment(out.ID)
	}
	return &out, nil
}

// EditIssueComment rewrites one of the review's own issue comments: the summary, in place.
func (c *reviewGitHub) EditIssueComment(ctx context.Context, id int64, body string) (*githubComment, error) {
	if err := checkCommentBody(body); err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, "PATCH", fmt.Sprintf("issues/comments/%d", id), nil, map[string]string{"body": body}, "", proxyMaxRead)
	if err != nil {
		return nil, err
	}
	var out githubComment
	return &out, json.Unmarshal([]byte(resp.Body), &out)
}

// githubReactions are the reactions GitHub has. Anything else is a 422.
var githubReactions = []string{"+1", "-1", "laugh", "confused", "heart", "hooray", "rocket", "eyes"}

// ReactToIssueComment reacts to a comment on the pull request — eyes on a command it is acting on.
func (c *reviewGitHub) ReactToIssueComment(ctx context.Context, id int64, content string) error {
	return c.react(ctx, fmt.Sprintf("issues/comments/%d/reactions", id), content)
}

// ReactToReviewComment reacts to an inline comment on the pull request's diff — +1 on a reply in a
// finding's thread that only said thanks. The comment must be known to be on this pull request
// first — listed from it (ReviewComments) or named by a delivery for it (KnowReviewComment) — since
// its URL does not name one.
func (c *reviewGitHub) ReactToReviewComment(ctx context.Context, id int64, content string) error {
	return c.react(ctx, fmt.Sprintf("pulls/comments/%d/reactions", id), content)
}

func (c *reviewGitHub) react(ctx context.Context, rel, content string) error {
	if !slices.Contains(githubReactions, content) {
		return fmt.Errorf("%q is not a GitHub reaction", truncate(content, 20))
	}
	_, err := c.do(ctx, "POST", rel, nil, map[string]string{"content": content}, "", proxyMaxRead)
	return err
}

// githubReaction is one reaction, as GitHub lists and creates them.
type githubReaction struct {
	ID      int64      `json:"id"`
	Content string     `json:"content"`
	User    githubUser `json:"user"`
}

// ReactToPull puts a reaction on the pull request itself and returns its id. GitHub answers a
// reaction the App already has there with that one, so putting it on twice leaves one — and putting
// it on is how the id of the App's own is learnt: listing a pull request's reactions takes a token
// that can read issues, which code review does not ask for. Either way it is the App's own, and the
// client may take it off again.
func (c *reviewGitHub) ReactToPull(ctx context.Context, content string) (int64, error) {
	if !slices.Contains(githubReactions, content) {
		return 0, fmt.Errorf("%q is not a GitHub reaction", truncate(content, 20))
	}
	resp, err := c.do(ctx, "POST", fmt.Sprintf("issues/%d/reactions", c.pr), nil, map[string]string{"content": content}, "", proxyMaxRead)
	if err != nil {
		return 0, err
	}
	var out githubReaction
	if err := json.Unmarshal([]byte(resp.Body), &out); err != nil {
		return 0, err
	}
	if out.ID <= 0 {
		return 0, errors.New("GitHub answered a reaction without its id")
	}
	c.mu.Lock()
	c.reactions[out.ID] = true
	c.mu.Unlock()
	return out.ID, nil
}

// DeletePullReaction takes one of the App's own reactions off the pull request. One already gone is
// the outcome asked for.
func (c *reviewGitHub) DeletePullReaction(ctx context.Context, id int64) error {
	_, err := c.do(ctx, "DELETE", fmt.Sprintf("issues/%d/reactions/%d", c.pr, id), nil, nil, "", proxyMaxRead)
	if isGitHubStatus(err, 404) {
		return nil
	}
	return err
}

// reviewCheckRun is a check run as POST /check-runs takes it, and PATCH /check-runs/{id} the fields
// after HeadSHA of it.
type reviewCheckRun struct {
	Name        string             `json:"name,omitempty"`
	HeadSHA     string             `json:"head_sha,omitempty"`
	ExternalID  string             `json:"external_id,omitempty"`
	Status      string             `json:"status,omitempty"`
	Conclusion  string             `json:"conclusion,omitempty"`
	StartedAt   string             `json:"started_at,omitempty"`
	CompletedAt string             `json:"completed_at,omitempty"`
	DetailsURL  string             `json:"details_url,omitempty"`
	Output      *reviewCheckOutput `json:"output,omitempty"`
}

type reviewCheckOutput struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

// reviewCheckConclusions are the conclusions a review's check may end with — none of them failure or
// action_required, which would read as the code failing and block a merge wherever the check is
// required. GitHub counts success, neutral and skipped as passing.
var reviewCheckConclusions = []string{"success", "neutral", "skipped", "cancelled"}

// reviewCheckTitleMax bounds a check's title, which GitHub shows on one line beside its name, and
// reviewCheckSummaryMax its summary, which GitHub refuses past this many characters.
const (
	reviewCheckTitleMax   = 120
	reviewCheckSummaryMax = 65535
)

func (in reviewCheckRun) check(create bool) error {
	switch {
	case create && (in.Name == "" || !commitSHA.MatchString(in.HeadSHA)):
		return fmt.Errorf("a check run needs a name and a full commit id, not %q and %q", truncate(in.Name, 60), truncate(in.HeadSHA, 70))
	case !create && (in.Name != "" || in.HeadSHA != ""):
		return errors.New("a check run's name and commit are set when it is made, not changed after")
	case in.Status != "in_progress" && in.Status != "completed":
		return fmt.Errorf("a review's check run is in_progress or completed, not %q", truncate(in.Status, 30))
	case (in.Status == "completed") != (in.Conclusion != ""):
		return errors.New("a check run has a conclusion when, and only when, it is completed")
	case in.Conclusion != "" && !slices.Contains(reviewCheckConclusions, in.Conclusion):
		return fmt.Errorf("a review's check run does not conclude %q", truncate(in.Conclusion, 30))
	case in.DetailsURL != "" && !strings.HasPrefix(in.DetailsURL, "https://github.com/"):
		return errors.New("a check run's details are on GitHub")
	case in.Output == nil:
		return nil
	case strings.TrimSpace(in.Output.Title) == "" || utf8.RuneCountInString(in.Output.Title) > reviewCheckTitleMax:
		return fmt.Errorf("a check run's title is 1 to %d characters", reviewCheckTitleMax)
	case strings.TrimSpace(in.Output.Summary) == "" || utf8.RuneCountInString(in.Output.Summary) > reviewCheckSummaryMax:
		return fmt.Errorf("a check run's summary is 1 to %d characters", reviewCheckSummaryMax)
	}
	return nil
}

// CreateCheckRun starts a check run on a commit of the repository and returns its id, which the
// client may then update.
func (c *reviewGitHub) CreateCheckRun(ctx context.Context, in reviewCheckRun) (int64, error) {
	if err := in.check(true); err != nil {
		return 0, err
	}
	resp, err := c.do(ctx, "POST", "check-runs", nil, in, "", proxyMaxRead)
	if err != nil {
		return 0, err
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(resp.Body), &out); err != nil {
		return 0, err
	}
	if out.ID <= 0 {
		return 0, errors.New("GitHub answered a new check run without its id")
	}
	c.mu.Lock()
	c.checks[out.ID] = true
	c.mu.Unlock()
	return out.ID, nil
}

// UpdateCheckRun moves one of the client's own check runs on: its text while it runs, or how it
// ended.
func (c *reviewGitHub) UpdateCheckRun(ctx context.Context, id int64, in reviewCheckRun) error {
	if err := in.check(false); err != nil {
		return err
	}
	_, err := c.do(ctx, "PATCH", fmt.Sprintf("check-runs/%d", id), nil, in, "", proxyMaxRead)
	return err
}

// FindCheckRun finds the check run named name on sha that the App made for externalID — a run put
// back after its start made one — and records it as the client's own; 0 when there is none. appID
// is the App's id: another App may name a check the same, and its runs are not this one's to touch.
func (c *reviewGitHub) FindCheckRun(ctx context.Context, sha, name, externalID, appID string) (int64, error) {
	if !commitSHA.MatchString(sha) || name == "" || externalID == "" || appID == "" {
		return 0, errors.New("a check run is found by its commit, its name, its run and the App's id")
	}
	var out struct {
		CheckRuns []struct {
			ID         int64  `json:"id"`
			ExternalID string `json:"external_id"`
			App        struct {
				ID int64 `json:"id"`
			} `json:"app"`
		} `json:"check_runs"`
	}
	q := url.Values{"check_name": {name}, "app_id": {appID}, "filter": {"all"}, "per_page": {"100"}}
	if err := c.getJSON(ctx, "commits/"+sha+"/check-runs", q, proxyMaxRead, &out); err != nil {
		return 0, err
	}
	for _, cr := range out.CheckRuns {
		if cr.ID > 0 && cr.ExternalID == externalID && strconv.FormatInt(cr.App.ID, 10) == appID {
			c.mu.Lock()
			c.checks[cr.ID] = true
			c.mu.Unlock()
			return cr.ID, nil
		}
	}
	return 0, nil
}

// ---- review threads (GraphQL) ----

// Resolving a review thread is a GraphQL mutation — REST has no endpoint for it — and finding the
// thread an inline comment opened is a GraphQL query: a REST comment's node_id is the comment's, not
// its thread's. POST /graphql is one URL for every operation GitHub has, so it is allowlisted here by
// what is sent rather than where: exactly one of these two documents, both Go's own constants, with
// variables naming this client's repository and pull request, or a thread already known to be on it.
// No text from a model, a pull request or a comment ever reaches either.
const (
	reviewThreadsQuery = `query($owner: String!, $name: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $after) {
        nodes { id isResolved comments(first: 1) { nodes { fullDatabaseId } } }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`
	reviewResolveThreadMutation = `mutation($threadId: ID!) {
  resolveReviewThread(input: {threadId: $threadId}) { thread { id isResolved } }
}`
)

// githubNodeID is a GraphQL node id as GitHub writes one ("PRRT_kwDO…"), and githubCursor a page's
// cursor: both opaque, and both held to what they look like so a variable carries nothing else.
var (
	githubNodeID = regexp.MustCompile(`^[A-Za-z0-9_=-]{4,200}$`)
	githubCursor = regexp.MustCompile(`^[A-Za-z0-9+/=_:.-]{1,300}$`)
)

// githubGraphQLError is GitHub's GraphQL answering 200 with errors instead of data: its type
// (FORBIDDEN, NOT_FOUND, RATE_LIMITED, …) and its message.
type githubGraphQLError struct {
	Type    string
	Message string
}

func (e *githubGraphQLError) Error() string {
	return fmt.Sprintf("GitHub's GraphQL API answered %s: %s", cmp.Or(e.Type, "an error"), truncate(e.Message, 300))
}

// githubPermissionDenied reports whether err is GitHub refusing a request for want of a permission:
// a GraphQL FORBIDDEN, the "not accessible by integration" an installation token gets for anything
// its grant does not cover, or a REST 403 with no wait in it.
func githubPermissionDenied(err error) bool {
	var gq *githubGraphQLError
	if errors.As(err, &gq) {
		return gq.Type == "FORBIDDEN" || strings.Contains(strings.ToLower(gq.Message), "not accessible by integration")
	}
	var api *githubAPIError
	return errors.As(err, &api) && api.Status == 403
}

// allowGraphQL reports why query with vars may not be sent, or nil when it may: see the constants
// above for what may.
func (c *reviewGitHub) allowGraphQL(query string, vars map[string]any) error {
	refuse := func(why string) error { return fmt.Errorf("code review may not send that GraphQL request: %s", why) }
	owner, name, _ := strings.Cut(c.repo, "/")
	keys := slices.Sorted(maps.Keys(vars))
	switch query {
	case reviewThreadsQuery:
		after, hasAfter := vars["after"]
		want := []string{"name", "number", "owner"}
		if hasAfter {
			want = []string{"after", "name", "number", "owner"}
		}
		switch {
		case !slices.Equal(keys, want):
			return refuse("the threads query takes owner, name, number and after")
		case vars["owner"] != owner || vars["name"] != name:
			return refuse("not this review's repository")
		case vars["number"] != c.pr:
			return refuse("not this review's pull request")
		}
		if cur, ok := after.(string); hasAfter && after != nil && (!ok || !githubCursor.MatchString(cur)) {
			return refuse("a page cursor is GitHub's, as GitHub wrote it")
		}
		return nil
	case reviewResolveThreadMutation:
		id, _ := vars["threadId"].(string)
		c.mu.Lock()
		known := c.threads[id]
		c.mu.Unlock()
		switch {
		case !slices.Equal(keys, []string{"threadId"}) || !githubNodeID.MatchString(id):
			return refuse("resolving takes one thread id")
		case !known:
			return refuse("that thread is not known to be on this pull request")
		}
		return nil
	}
	return refuse("not an operation code review makes")
}

// graphql sends one of the two operations to GitHub's GraphQL API and decodes its data into out. The
// threads query reads, with review_read; the mutation writes, with review_post — GitHub does not
// document which permission resolveReviewThread wants of an App, and pull_requests:write is the
// narrowest that could be it (reviewResolveThread says what a refusal does). A 200 carrying errors is
// a githubGraphQLError.
func (c *reviewGitHub) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	if err := c.allowGraphQL(query, vars); err != nil {
		return err
	}
	u, err := url.Parse(c.base + "/graphql")
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	purpose, what := githubPurposeReviewRead, "POST graphql reviewThreads"
	if query == reviewResolveThreadMutation {
		purpose, what = githubPurposeReviewPost, "POST graphql resolveReviewThread"
	}
	resp, err := reviewSendAs(ctx, c.proxy, c.orgID, c.acc, c.conn, purpose, "POST", u.String(), what, string(body), "", proxyMaxRead)
	if err != nil {
		return err
	}
	if resp.Truncated {
		return fmt.Errorf("GitHub's answer to %s is larger than code review reads", what)
	}
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal([]byte(resp.Body), &env); err != nil {
		return fmt.Errorf("reading GitHub's answer to %s: %w", what, err)
	}
	if len(env.Errors) > 0 {
		return &githubGraphQLError{Type: env.Errors[0].Type, Message: env.Errors[0].Message}
	}
	return json.Unmarshal(env.Data, out)
}

// githubThread is one review thread on the pull request: its node id, whether it is resolved, and the
// database id of its first comment — the inline comment that opened it, which is what a finding
// records (github_comment_id). That id is read as fullDatabaseId, a BigInt: review comments' REST ids
// are past what GraphQL's 32-bit databaseId holds, and GitHub answers that field for such a comment
// with null and an error, which would fail the whole listing — a human's thread among them enough
// to leave every finding's thread unresolved.
type githubThread struct {
	ID       string
	Resolved bool
	RootID   int64
}

// ReviewThreads lists the pull request's review threads, a hundred to a page up to reviewListPages,
// and records each as on it. more says there were more than that.
func (c *reviewGitHub) ReviewThreads(ctx context.Context) (threads []githubThread, more bool, err error) {
	owner, name, _ := strings.Cut(c.repo, "/")
	var after any
	for range reviewListPages {
		vars := map[string]any{"owner": owner, "name": name, "number": c.pr}
		if after != nil {
			vars["after"] = after
		}
		var data struct {
			Repository *struct {
				PullRequest *struct {
					ReviewThreads struct {
						Nodes []struct {
							ID         string `json:"id"`
							IsResolved bool   `json:"isResolved"`
							Comments   struct {
								Nodes []struct {
									FullDatabaseID githubBigInt `json:"fullDatabaseId"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		if err := c.graphql(ctx, reviewThreadsQuery, vars, &data); err != nil {
			return threads, false, err
		}
		if data.Repository == nil || data.Repository.PullRequest == nil {
			return threads, false, &githubGraphQLError{Type: "NOT_FOUND", Message: "no such pull request"}
		}
		page := data.Repository.PullRequest.ReviewThreads
		c.mu.Lock()
		for _, n := range page.Nodes {
			if !githubNodeID.MatchString(n.ID) {
				continue
			}
			c.threads[n.ID] = true
			t := githubThread{ID: n.ID, Resolved: n.IsResolved}
			if len(n.Comments.Nodes) > 0 {
				t.RootID = int64(n.Comments.Nodes[0].FullDatabaseID)
			}
			threads = append(threads, t)
		}
		c.mu.Unlock()
		if !page.PageInfo.HasNextPage || page.PageInfo.EndCursor == "" {
			return threads, false, nil
		}
		after = page.PageInfo.EndCursor
	}
	return threads, true, nil
}

// githubBigInt is GraphQL's BigInt as GitHub writes it — a string of digits, since the value may
// not fit the 53 bits a JSON number is safe to — or a plain number, or null, which reads as 0: no
// comment, which no finding's thread is looked up by.
type githubBigInt int64

func (n *githubBigInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" || s == "" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("GitHub wrote a BigInt that is not one: %w", err)
	}
	*n = githubBigInt(v)
	return nil
}

// ThreadOf is the review thread the inline comment rootID opened, from the pull request's threads,
// listed once per client. ok is false when no thread starts with that comment — deleted, or past the
// pages ReviewThreads reads.
func (c *reviewGitHub) ThreadOf(ctx context.Context, rootID int64) (t githubThread, ok bool, err error) {
	c.mu.Lock()
	listed := c.rootThreads != nil
	t, ok = c.rootThreads[rootID]
	c.mu.Unlock()
	if listed {
		return t, ok, nil
	}
	threads, _, err := c.ReviewThreads(ctx)
	if err != nil {
		return githubThread{}, false, err
	}
	byRoot := map[int64]githubThread{}
	for _, th := range threads {
		if th.RootID > 0 {
			byRoot[th.RootID] = th
		}
	}
	c.mu.Lock()
	c.rootThreads = byRoot
	c.mu.Unlock()
	t, ok = byRoot[rootID]
	return t, ok, nil
}

// ResolveThread resolves one review thread on the pull request — the one a finding's inline comment
// opened, which must be known to be on it (ReviewThreads, KnowThread). Resolving one already resolved
// changes nothing at GitHub.
func (c *reviewGitHub) ResolveThread(ctx context.Context, threadID string) error {
	var data struct {
		ResolveReviewThread *struct {
			Thread struct {
				ID         string `json:"id"`
				IsResolved bool   `json:"isResolved"`
			} `json:"thread"`
		} `json:"resolveReviewThread"`
	}
	if err := c.graphql(ctx, reviewResolveThreadMutation, map[string]any{"threadId": threadID}, &data); err != nil {
		return err
	}
	if data.ResolveReviewThread == nil || !data.ResolveReviewThread.Thread.IsResolved {
		return &githubGraphQLError{Message: "the thread was not resolved"}
	}
	return nil
}
