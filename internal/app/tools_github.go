package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Searching code across the repositories a channel can reach.
//
// The question this exists for is "fix the login timeout bug", said by somebody who never names
// a repository. Answering it means looking in every repository the channel was granted and
// saying which one it is — which the pack could not do at all before: it could search issues,
// read a pull request and list commits, and it could not read a single line of code.
//
// Two rules shape everything here.
//
// The model supplies search terms; the channel's grants supply the repositories. They are never
// the same decision, so a repo:, org: or user: qualifier written by the model is stripped before
// ours is appended (stripRepoScope). The credential a search runs under can usually see far more
// than the one repository its connection names — a classic token sees the whole account — so
// without that, a model that wrote org:somebody-else would be answered.
//
// And each repository is searched under its own connection's credential, one request apiece,
// rather than one query listing them all. A fine-grained token scoped to acme/api returns
// nothing for acme/web, so a single query naming both would quietly answer for whichever
// repository the matched credential happened to cover, and report the silence from the other as
// an absence of results.

const (
	// repoFanout caps how many repositories one question touches. Code search allows ten
	// requests a minute and this spends one per repository, so a channel granted thirty of them
	// would burn the whole minute on a single question. The cap is reported rather than hidden:
	// the model is told to name a repository when it wants to look further.
	repoFanout = 8

	// githubTreeTTL is how long a repository's file list is held. Long enough that "where is X"
	// followed by "and where is Y" costs one fetch, short enough that a file added this morning
	// is findable this afternoon.
	githubTreeTTL = 10 * time.Minute

	// githubRawMax caps a single file read. Well above any source file worth reading and well
	// below anything that would crowd out the conversation.
	githubRawMax = 256 << 10
)

// cachedTree is one repository's file list at a point in time.
type cachedTree struct {
	at        time.Time
	branch    string
	paths     []string
	truncated bool
}

// githubTrees holds file lists per organisation+repository. Keyed by organisation as well as
// repository for the reason the ClickUp cache is (clickupLists): two tenants can be granted
// repositories with the same owner/name, and one tenant's file list is not the other's to see.
type githubTrees struct {
	mu sync.Mutex
	m  map[string]cachedTree
}

// scopeQualifierRe matches the qualifiers that choose which repositories a search covers.
// Deliberately not fork:, language:, path: or in: — those narrow a search inside the scope we
// set, which is the model's business. These four choose the scope itself, which is not.
var scopeQualifierRe = regexp.MustCompile(`(?i)(^|\s)-?(repo|org|user|owner)\s*:\s*[^\s]+`)

// stripRepoScope removes the model's own repository scoping from a query and tidies what is
// left. Whatever it wrote about which repositories to search is not an instruction we take.
func stripRepoScope(q string) string {
	return strings.Join(strings.Fields(scopeQualifierRe.ReplaceAllString(q, " ")), " ")
}

// repoTargets is the list of repositories a GitHub search may touch on this turn, narrowest
// grant first. want names one of them; empty means all of them. Anything not in this list is
// not reachable from this channel, and saying so by name is more use than a silent empty result.
func repoTargets(c *Call, want string) ([]*Connection, error) {
	all := c.Access.Repos()
	if len(all) == 0 {
		return nil, fmt.Errorf("no repositories are connected in this channel. An admin adds one in the console under Access bundles › Repositories")
	}
	if want = strings.TrimSpace(want); want == "" {
		return all, nil
	}
	repo, err := normalizeRepo(want)
	if err != nil {
		return nil, err
	}
	for _, conn := range all {
		if strings.EqualFold(conn.Repo, repo) {
			return []*Connection{conn}, nil
		}
	}
	return nil, fmt.Errorf("%s is not connected in this channel. The ones that are: %s", repo, strings.Join(repoNames(all), ", "))
}

func repoNames(conns []*Connection) []string {
	out := make([]string, len(conns))
	for i, c := range conns {
		out[i] = c.Repo
	}
	return out
}

// capTargets trims the fan-out and says whether it had to.
func capTargets(targets []*Connection) ([]*Connection, bool) {
	if len(targets) <= repoFanout {
		return targets, false
	}
	return targets[:repoFanout], true
}

// moreRepos is the line appended when the fan-out was capped, so a model that came up empty
// knows the search was partial rather than conclusive.
func moreRepos(targets []*Connection, capped bool) string {
	if !capped {
		return ""
	}
	return fmt.Sprintf("\n\n[searched the first %d of %d repositories: %s. Name a repository to look in one of the others.]",
		repoFanout, len(targets), strings.Join(repoNames(targets[:repoFanout]), ", "))
}

// ghResult is what a GitHub call came back as, apart from its body: the status, and how long to
// wait when the status was a spent rate limit.
type ghResult struct {
	status int
	retry  time.Duration
}

// limited turns a spent budget into a sentence a model can act on, and nil into nothing.
// Decided by the wait GitHub sent rather than by the status, because GitHub answers a rate limit
// with 403 as often as with 429 — and a 403 carrying no wait really is a refusal, which has to
// go on reading as one.
func (r ghResult) limited(what string) error {
	if r.retry <= 0 {
		return nil
	}
	return fmt.Errorf("GitHub's rate limit for %s is spent; try again in %s, or name one repository to spend less of it",
		what, fmtDuration(r.retry.Round(time.Second)))
}

// getJSONFrom is getJSON with a named credential: on GitHub every repository is its own
// connection, and which one answers decides what the search can see.
func (a *Agent) getJSONFrom(ctx context.Context, c *Call, conn *Connection, url string, out any) (ghResult, error) {
	resp, err := a.proxy.Do(ctx, c.OrgID, c.Access,
		ProxyRequest{Method: "GET", URL: url, Connection: conn.Name, MaxBytes: proxyMaxRead}, callAudit(c), true)
	if err != nil {
		return ghResult{}, err
	}
	r := ghResult{status: resp.Status, retry: resp.RetryAfter}
	if resp.Status >= 400 {
		if err := r.limited("GitHub"); err != nil {
			return r, err
		}
		return r, fmt.Errorf("GitHub returned %d for %s%s", resp.Status, conn.Repo, ghMessage(resp.Body))
	}
	return r, json.Unmarshal([]byte(resp.Body), out)
}

// ghMessage is GitHub's own reason for refusing a request, as ": reason", or "" when the body
// carries none. A bare "422" told the model nothing it could change; the reason says what to —
// and for a search that reason is in the first of its errors, under a message that only says
// "Validation Failed" (a token that may not read a repository's issues gets exactly that).
func ghMessage(body string) string {
	var e struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal([]byte(body), &e) != nil || strings.TrimSpace(e.Message) == "" {
		return ""
	}
	msg := e.Message
	if len(e.Errors) > 0 && strings.TrimSpace(e.Errors[0].Message) != "" {
		msg += ": " + e.Errors[0].Message
	}
	return ": " + truncate(oneLine(msg), 300)
}

// namesIssueType reports whether an issue search says what it is looking for. GitHub's refuses
// one that does not, with a 422: "Query must include 'is:issue' or 'is:pull-request'".
func namesIssueType(q string) bool {
	for _, f := range strings.Fields(strings.ToLower(q)) {
		switch f {
		case "is:issue", "is:pr", "is:pull-request", "type:issue", "type:pr", "type:pull-request":
			return true
		}
	}
	return false
}

// githubDefaultBranch asks the repository which branch it considers its own. Cached with the
// file list, because the two are always wanted together.
func (a *Agent) githubDefaultBranch(ctx context.Context, c *Call, conn *Connection, base string) (string, error) {
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if _, err := a.getJSONFrom(ctx, c, conn, base+"/repos/"+conn.Repo, &repo); err != nil {
		return "", err
	}
	if repo.DefaultBranch == "" {
		return "", fmt.Errorf("%s did not name a default branch", conn.Repo)
	}
	return repo.DefaultBranch, nil
}

// githubTree is every path in a repository's default branch, in one call. This is the tool that
// answers "which repository is this?" without a search index: names and directory structure say
// a great deal about where a feature lives, and unlike code search it needs nothing beyond read
// access to the contents, so it works on every credential there is.
func (a *Agent) githubTree(ctx context.Context, c *Call, conn *Connection, base string) (cachedTree, error) {
	key := strconv.FormatInt(c.OrgID, 10) + "|" + strings.ToLower(conn.Repo)
	a.trees.mu.Lock()
	if hit, ok := a.trees.m[key]; ok && time.Since(hit.at) < githubTreeTTL {
		a.trees.mu.Unlock()
		return hit, nil
	}
	a.trees.mu.Unlock()

	branch, err := a.githubDefaultBranch(ctx, c, conn, base)
	if err != nil {
		return cachedTree{}, err
	}
	var tree struct {
		Truncated bool `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
	}
	if _, err := a.getJSONFrom(ctx, c, conn,
		fmt.Sprintf("%s/repos/%s/git/trees/%s?recursive=1", base, conn.Repo, url.PathEscape(branch)), &tree); err != nil {
		return cachedTree{}, err
	}
	out := cachedTree{at: time.Now(), branch: branch, truncated: tree.Truncated}
	for _, e := range tree.Tree {
		if e.Type == "blob" {
			out.paths = append(out.paths, e.Path)
		}
	}
	a.trees.mu.Lock()
	if a.trees.m == nil {
		a.trees.m = map[string]cachedTree{}
	}
	a.trees.m[key] = out
	a.trees.mu.Unlock()
	return out, nil
}

// githubFindCode searches the text of the code itself, one request per repository.
func (a *Agent) githubFindCode(ctx context.Context, c *Call, base, query, want string, limit int) (string, error) {
	terms := stripRepoScope(query)
	if terms == "" {
		return "", fmt.Errorf("say what to search for. The repositories are chosen by what this channel is granted, not by the query")
	}
	all, err := repoTargets(c, want)
	if err != nil {
		return "", err
	}
	targets, capped := capTargets(all)

	var b strings.Builder
	n, refused := 0, 0
	for _, conn := range targets {
		var res struct {
			TotalCount int `json:"total_count"`
			Items      []struct {
				Path string `json:"path"`
				HTML string `json:"html_url"`
			} `json:"items"`
		}
		q := terms + " repo:" + conn.Repo
		r, err := a.getJSONFrom(ctx, c, conn,
			fmt.Sprintf("%s/search/code?q=%s&per_page=%d", base, url.QueryEscape(q), limit), &res)
		if err != nil {
			// A spent budget stops the fan-out there and says so. Carrying on would spend the
			// next minute's allowance too and still answer nothing.
			if limitErr := r.limited("code search"); limitErr != nil {
				if n > 0 {
					break
				}
				return "", limitErr
			}
			// 403 otherwise is usually the credential rather than the query: GitHub is particular
			// about which kinds of token may search code. Count them and explain once at the end,
			// so a model does not read "no results" as "the code is not there".
			if r.status == 403 || r.status == 422 {
				refused++
			}
			continue
		}
		for _, it := range res.Items {
			fmt.Fprintf(&b, "%s\t%s\t%s\n", conn.Repo, it.Path, it.HTML)
			if n++; n >= limit {
				break
			}
		}
		if n >= limit {
			break
		}
	}
	if refused == len(targets) && refused > 0 {
		return "", fmt.Errorf("GitHub would not search the code with this channel's credential (%d refused). "+
			"Code search is not open to every kind of token; use github_find_file to find files by name and path instead", refused)
	}
	if n == 0 {
		return fmt.Sprintf("No code matches %q in %s.%s Only the default branch is searched, and only files under 384 KB.",
			terms, strings.Join(repoNames(targets), ", "), moreRepos(all, capped)), nil
	}
	return fmt.Sprintf("repository\tpath\turl (%d shown)\n%s%s", n, b.String(), moreRepos(all, capped)), nil
}

// githubFindFile matches a fragment against every path in each repository.
func (a *Agent) githubFindFile(ctx context.Context, c *Call, base, name, want string, limit int) (string, error) {
	if name = strings.TrimSpace(name); name == "" {
		return "", fmt.Errorf("say what file or path fragment to look for")
	}
	all, err := repoTargets(c, want)
	if err != nil {
		return "", err
	}
	targets, capped := capTargets(all)

	var b strings.Builder
	n, partial := 0, []string{}
	needle := strings.ToLower(name)
	for _, conn := range targets {
		tree, err := a.githubTree(ctx, c, conn, base)
		if err != nil {
			continue
		}
		if tree.truncated {
			partial = append(partial, conn.Repo)
		}
		for _, p := range tree.paths {
			if !strings.Contains(strings.ToLower(p), needle) {
				continue
			}
			fmt.Fprintf(&b, "%s\t%s\n", conn.Repo, p)
			if n++; n >= limit {
				break
			}
		}
		if n >= limit {
			break
		}
	}
	tail := moreRepos(all, capped)
	if len(partial) > 0 {
		tail += fmt.Sprintf("\n\n[GitHub truncated the file list for %s: it is too large to list in full, so this may be incomplete]",
			strings.Join(partial, ", "))
	}
	if n == 0 {
		return fmt.Sprintf("No file matches %q in %s.%s", name, strings.Join(repoNames(targets), ", "), tail), nil
	}
	return fmt.Sprintf("repository\tpath (%d shown)\n%s%s", n, b.String(), tail), nil
}

// githubReadFile returns a file's text. With one repository connected the repo argument may be
// left out, because in that channel there is nothing else it could mean.
func (a *Agent) githubReadFile(ctx context.Context, c *Call, base, want, path, ref string) (string, error) {
	if path = strings.TrimLeft(strings.TrimSpace(path), "/"); path == "" {
		return "", fmt.Errorf("say which file to read")
	}
	conn, err := repoConnection(c, want)
	if err != nil {
		return "", err
	}
	u := fmt.Sprintf("%s/repos/%s/contents/%s", base, conn.Repo, pathEscapeSegments(path))
	if ref = strings.TrimSpace(ref); ref != "" {
		u += "?ref=" + url.QueryEscape(ref)
	}
	resp, err := a.proxy.Do(ctx, c.OrgID, c.Access, ProxyRequest{Method: "GET", URL: u, Connection: conn.Name,
		Headers: map[string]string{"Accept": "application/vnd.github.raw"}, MaxBytes: githubRawMax}, callAudit(c), true)
	if err != nil {
		return "", err
	}
	if err := (ghResult{status: resp.Status, retry: resp.RetryAfter}).limited("GitHub"); err != nil {
		return "", err
	}
	switch {
	case resp.Status == 404:
		return "", fmt.Errorf("%s has no file at %s. Find the path with github_find_file first", conn.Repo, path)
	case resp.Status == 403:
		return "", fmt.Errorf("GitHub refused %s in %s (403): the file may be too large to read this way", path, conn.Repo)
	case resp.Status >= 400:
		return "", fmt.Errorf("GitHub returned %d for %s in %s", resp.Status, path, conn.Repo)
	}
	return fmt.Sprintf("%s %s\n\n%s", conn.Repo, path, resp.Body), nil
}

// pathEscapeSegments escapes a repository path one segment at a time, so the slashes that
// separate directories survive and everything else that needs escaping gets it.
func pathEscapeSegments(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// githubSearchIssues is the old github_search, with the channel's repositories imposed on the
// query rather than trusted from it.
func (a *Agent) githubSearchIssues(ctx context.Context, c *Call, base, query, want string, limit int) (string, error) {
	terms := stripRepoScope(query)
	all, err := repoTargets(c, want)
	if err != nil {
		return "", err
	}
	targets, capped := capTargets(all)

	// "What is open", or a plain keyword, names neither issues nor pull requests, and GitHub
	// refuses a search that does not. Every repository answered 422, each was skipped, and the
	// model was told "Nothing matches" — so it said there was nothing open. Such a search is asked
	// twice, for issues and for pull requests; one that names its type goes as written. Not as one
	// "(is:issue OR is:pr)" in the advanced syntax: GitHub takes that from a fine-grained token and
	// then finds nothing at all.
	kinds := []string{""}
	if !namesIssueType(terms) {
		kinds = []string{" is:issue", " is:pr"}
	}

	var b strings.Builder
	var refused []string
	n := 0
repos:
	for _, conn := range targets {
		for _, kind := range kinds {
			var res struct {
				Items []struct {
					Number  int    `json:"number"`
					Title   string `json:"title"`
					State   string `json:"state"`
					HTML    string `json:"html_url"`
					Updated string `json:"updated_at"`
				} `json:"items"`
			}
			q := strings.TrimSpace(terms + kind + " repo:" + conn.Repo)
			r, err := a.getJSONFrom(ctx, c, conn,
				fmt.Sprintf("%s/search/issues?q=%s&per_page=%d&sort=updated", base, url.QueryEscape(q), limit), &res)
			if err != nil {
				if limitErr := r.limited("issue search"); limitErr != nil {
					if n > 0 {
						break repos
					}
					return "", limitErr
				}
				if kind != "" {
					err = fmt.Errorf("%s: %w", map[string]string{" is:issue": "issues", " is:pr": "pull requests"}[kind], err)
				}
				refused = append(refused, err.Error())
				continue
			}
			for _, it := range res.Items {
				fmt.Fprintf(&b, "%s#%d\t%s\t%s\t%s\n", conn.Repo, it.Number, it.State, it.Title, it.HTML)
				if n++; n >= limit {
					break repos
				}
			}
		}
	}
	// A search GitHub refused is not a search that found nothing, and must not read as one.
	if n == 0 && len(refused) > 0 {
		return "", fmt.Errorf("GitHub refused the search, so this says nothing about what exists: %s", strings.Join(refused, "; "))
	}
	tail := moreRepos(all, capped)
	if len(refused) > 0 {
		tail += "\n\n[not searched, GitHub refused: " + strings.Join(refused, "; ") + "]"
	}
	if n == 0 {
		return fmt.Sprintf("Nothing matches %q in %s.%s", terms, strings.Join(repoNames(targets), ", "), tail), nil
	}
	return fmt.Sprintf("issue\tstate\ttitle\turl (%d shown)\n%s%s", n, b.String(), tail), nil
}

// repoConnection resolves one repository to the connection holding its credential, so a call
// about acme/web goes out under acme/web's token rather than under whichever repository
// connection happened to rank first for api.github.com. With one repository connected the name
// may be left out: in that channel there is nothing else it could mean.
func repoConnection(c *Call, want string) (*Connection, error) {
	targets, err := repoTargets(c, want)
	if err != nil {
		return nil, err
	}
	if len(targets) > 1 {
		return nil, fmt.Errorf("say which repository: %s", strings.Join(repoNames(targets), ", "))
	}
	return targets[0], nil
}
