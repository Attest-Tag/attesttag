package app

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"attesttag/internal/review"
)

// grep over the pull request's head.
//
// find_code is GitHub's code search, which indexes default branches only and allows a handful of
// searches a review: asked where something the pull request uses is defined, it answered from a
// branch the definition was not on yet, or not at all, and the finder called the thing undefined.
// grep answers from the head itself. The first grep of a run downloads the head once, as GitHub's
// archive of that commit, keeps the text files worth searching in memory, and every grep after it —
// finder passes and verifier alike — searches that copy. The copy goes when the run does.
//
// The archive is the one read of a review's that Proxy.Do cannot make. Do reads a body whole into a
// string, capped at proxyMaxRead, and follows no redirect: both right for an answer a model is shown,
// and both wrong here, where GitHub answers with a redirect to its download host and the archive of
// a large repository is many times that cap. So its two requests are sent here, from the proxy's own
// parts: the URL passes the pull request client's allowlist (reviewRoutes), the connection is matched
// and the review_read token minted and injected by the proxy, both requests are audited like any
// other, and the second — to the host the redirect names — carries no credential at all. The URL
// GitHub redirects to is signed for that one download; the installation token is for GitHub's API
// and is not another host's to be shown.
const (
	// Greps one review may run, finder passes and verifier together. A search over the whole head is
	// cheap next to a model call, but a pass that greps forty times is looping, not reading.
	reviewGrepMaxCalls = 40
	// Matching lines one grep shows unless it asks, the most it may ask for, and how much of each.
	reviewGrepDefault   = 40
	reviewGrepMax       = 100
	reviewGrepLineChars = 200
	reviewGrepPattern   = 500
	// How long the download and indexing of one head may take.
	reviewArchiveWall = 2 * time.Minute
)

// reviewIndexCaps bound one head's copy. A repository past any of them is searched as far as it was
// read, and every answer says the search was partial. A field each so a test can shrink them.
type reviewIndexCaps struct {
	download int64 // bytes of the archive read off the wire, compressed
	kept     int64 // bytes of text kept
	files    int   // files kept
	unpacked int64 // bytes read out of the archive, kept or not: what a small archive that unpacks to a great deal costs to walk
	fileMax  int64 // the largest file kept; anything larger is data, not code somebody reads
}

var reviewIndexLimits = reviewIndexCaps{download: 120 << 20, kept: 80 << 20, files: 60_000, unpacked: 1 << 30, fileMax: 512 << 10}

// reviewGrepSkipDirs are directories whose files nobody writes by hand or reads to learn the code:
// installed packages and build output. vendor/ is among reviewGeneratedPaths already.
var reviewGrepSkipDirs = []string{"node_modules", "dist", "build"}

// reviewHeadIndex is a head's text files, by path.
type reviewHeadIndex struct {
	sha   string
	files []*reviewIndexedFile
	// partial says why not every text file is in it, or "" when every one is.
	partial string
}

// reviewIndexedFile is one file of the copy, raw as committed with CRLF read as LF so its line
// numbers are read_file's. Shown lines are masked (line): the mask is worked out for a file the
// first time one of its lines is shown, and only the lines it changed are kept.
type reviewIndexedFile struct {
	path  string
	text  string
	once  sync.Once
	masks map[int]string
}

// line is line n of the file as a model may be shown it: masked, so a credential the copy holds
// never reaches one. raw is that line as the search found it.
func (f *reviewIndexedFile) line(n int, raw string) string {
	f.once.Do(func() {
		masked, hit := maskText(f.text)
		if len(hit) == 0 {
			return
		}
		lines := strings.Split(masked, "\n")
		f.masks = map[int]string{}
		for _, h := range hit {
			if h-1 < len(lines) {
				f.masks[h] = lines[h-1]
			}
		}
	})
	if s, ok := f.masks[n]; ok {
		return s
	}
	return raw
}

// reviewGrepIndex is a run's copy of its head and what grep has spent: built by the first grep that
// needs it, shared by every pass after it, and dropped when the run ends (release).
type reviewGrepIndex struct {
	mu       sync.Mutex // held while the copy is built, so a second grep waits for the first's download
	index    *reviewHeadIndex
	err      error
	released bool

	calls sync.Mutex
	n     int
}

// get returns the head's copy, building it with build the first time. A build that failed because
// its caller's time ran out is not remembered — the next grep may have time — and any other failure
// is, so a repository that cannot be downloaded is tried once a run.
func (g *reviewGrepIndex) get(ctx context.Context, build func(context.Context) (*reviewHeadIndex, error)) (*reviewHeadIndex, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case g.released:
		return nil, errors.New("this review has finished")
	case g.index != nil || g.err != nil:
		return g.index, g.err
	}
	idx, err := build(ctx)
	if err != nil && ctx.Err() != nil {
		return nil, err
	}
	g.index, g.err = idx, err
	return idx, err
}

// release drops the copy: a large repository's text is tens of megabytes, and nothing reads it after
// the run.
func (g *reviewGrepIndex) release() {
	g.mu.Lock()
	g.index, g.err, g.released = nil, nil, true
	g.mu.Unlock()
}

// spend counts one grep, and reports false once the run has had its share.
func (g *reviewGrepIndex) spend() bool {
	g.calls.Lock()
	defer g.calls.Unlock()
	if g.n >= reviewGrepMaxCalls {
		return false
	}
	g.n++
	return true
}

// ---- the download ----

// Archive opens the gzipped tar archive of the repository at commit sha. GitHub answers with a
// redirect to its download host, which is followed once, without the token (see the top of this
// file); what the caller reads of the body is its own to bound. The body is the caller's to close.
func (c *reviewGitHub) Archive(ctx context.Context, sha string) (io.ReadCloser, error) {
	if !commitSHA.MatchString(sha) {
		return nil, fmt.Errorf("an archive is read at a full commit id, not %q", truncate(sha, 70))
	}
	u, err := url.Parse(c.base + "/repos/" + c.repo + "/tarball/" + sha)
	if err != nil {
		return nil, err
	}
	if err := c.allow("GET", u); err != nil {
		return nil, err
	}
	p := c.proxy
	audit := ProxyAudit{Requester: "github-review", Method: "GET", Host: u.Hostname(), Path: u.Path}
	if err := checkURL(u); err != nil {
		return nil, err
	}
	conn, why := p.MatchNamed(c.acc, "GET", u, c.conn.Name)
	if why == "" && (conn == nil || conn.CredType != "github_app") {
		why = "blocked by the proxy: a review_read request has to go through a GitHub App connection, whose token can be narrowed to it"
	}
	if why != "" {
		audit.Blocked = why
		p.store.LogProxy(ctx, c.orgID, audit)
		return nil, errors.New(why)
	}
	hr, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	hr.Header.Set("User-Agent", "attesttag/0.2 (+proxy)")
	hr.Header.Set("Accept", "application/vnd.github+json")
	hr.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if err := p.inject(ctx, c.orgID, conn, hr, audit, githubPurposeReviewRead); err != nil {
		audit.Blocked = "credential error: " + err.Error()
		p.store.LogProxy(ctx, c.orgID, audit)
		return nil, fmt.Errorf("credential error for %s: %w", conn.Name, err)
	}
	audit.ConnectionID = conn.ID
	// The redirect is read rather than followed, so the request that follows it is built here, and
	// built bare.
	first := &http.Client{Transport: p.client.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	start := time.Now()
	resp, err := first.Do(hr)
	if err != nil {
		err = safeTransportErr("GET", u.Hostname(), err)
		audit.Blocked = "request failed: " + err.Error()
		p.store.LogProxy(ctx, c.orgID, audit)
		return nil, err
	}
	audit.Status, audit.MS = resp.StatusCode, time.Since(start).Milliseconds()
	p.store.LogProxy(ctx, c.orgID, audit)
	what := "GET tarball/" + shortSHA(sha)
	switch {
	case resp.StatusCode == http.StatusOK:
		return resp.Body, nil
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		loc := resp.Header.Get("Location")
		resp.Body.Close()
		next, err := u.Parse(loc)
		if err != nil || loc == "" {
			return nil, fmt.Errorf("GitHub answered %s with a redirect to nowhere", what)
		}
		return c.download(ctx, u, next, what)
	}
	return nil, archiveRefusal(resp, what)
}

// reviewArchiveHosts are where GitHub sends an archive download: its download host, or the API's own.
var reviewArchiveHosts = []string{"codeload.github.com"}

// download follows the archive's redirect to next: a host GitHub serves archives from, over https,
// with no credential and no further redirect.
func (c *reviewGitHub) download(ctx context.Context, from, next *url.URL, what string) (io.ReadCloser, error) {
	host := strings.ToLower(strings.TrimRight(next.Hostname(), "."))
	if next.Scheme != "https" || (!slices.Contains(reviewArchiveHosts, host) && !strings.EqualFold(host, from.Hostname())) {
		return nil, fmt.Errorf("GitHub answered %s with a redirect to %s, which is not where GitHub serves archives from", what, truncate(host, 80))
	}
	if err := publicURL(next); err != nil {
		return nil, err
	}
	p := c.proxy
	audit := ProxyAudit{Requester: "github-review", Method: "GET", Host: next.Hostname(), Path: next.Path}
	hr, err := http.NewRequestWithContext(ctx, "GET", next.String(), nil)
	if err != nil {
		return nil, err
	}
	hr.Header.Set("User-Agent", "attesttag/0.2 (+proxy)")
	bare := &http.Client{Transport: p.client.Transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("an archive download is not redirected twice")
	}}
	start := time.Now()
	resp, err := bare.Do(hr)
	if err != nil {
		// Never the URL itself: for a private repository it carries the download's signature.
		err = safeTransportErr("GET", next.Hostname(), err)
		audit.Blocked = "request failed: " + err.Error()
		p.store.LogProxy(ctx, c.orgID, audit)
		return nil, err
	}
	audit.Status, audit.MS = resp.StatusCode, time.Since(start).Milliseconds()
	p.store.LogProxy(ctx, c.orgID, audit)
	if resp.StatusCode != http.StatusOK {
		return nil, archiveRefusal(resp, what)
	}
	return resp.Body, nil
}

// archiveRefusal is an answer that is not an archive, as the errors the rest of the client returns:
// a spent rate limit to wait out, or GitHub's status and reason.
func archiveRefusal(resp *http.Response, what string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if wait := retryAfterHint(resp.Header, time.Now()); wait > 0 && resp.StatusCode >= 400 {
		return &githubRetryError{Status: resp.StatusCode, Wait: wait}
	}
	return &githubAPIError{Status: resp.StatusCode, Message: strings.TrimPrefix(ghMessage(string(body)), ": "), What: what}
}

// ---- the copy ----

// errArchiveCap is the archive running past the bytes a review downloads.
var errArchiveCap = errors.New("archive cap")

// capReader reads at most n bytes of r and then fails with over, so a cap reads as an error the
// caller can tell from a broken download.
type capReader struct {
	r    io.Reader
	n    int64
	over error
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.n <= 0 {
		// At the cap: past it only if there is more to read. A stream exactly the cap's size ends.
		var one [1]byte
		if k, err := io.ReadFull(c.r, one[:]); k == 0 && err == io.EOF {
			return 0, io.EOF
		}
		return 0, c.over
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	k, err := c.r.Read(p)
	c.n -= int64(k)
	return k, err
}

var errUnpackedCap = errors.New("unpacked cap")

// buildHeadIndex reads a repository archive — gzipped or not, GitHub's top directory stripped — into
// the text files keep accepts, within caps. It stops at the first cap it meets and says which in
// partial. A download that breaks after some files were read is a partial copy rather than none; one
// that breaks before any is an error.
func buildHeadIndex(ctx context.Context, sha string, body io.Reader, caps reviewIndexCaps, keep func(string) bool) (*reviewHeadIndex, error) {
	idx := &reviewHeadIndex{sha: sha}
	wire := bufio.NewReaderSize(&capReader{r: body, n: caps.download, over: errArchiveCap}, 64<<10)
	var src io.Reader = wire
	if magic, _ := wire.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(wire)
		if err != nil {
			return nil, fmt.Errorf("the archive is not one GitHub would send: %w", err)
		}
		defer gz.Close()
		src = gz
	}
	tr := tar.NewReader(&capReader{r: src, n: caps.unpacked, over: errUnpackedCap})
	var kept int64
	stop := func(why string) { idx.partial = why }
	for {
		if err := ctx.Err(); err != nil {
			if len(idx.files) == 0 {
				return nil, err
			}
			stop("the time for reading it ran out")
			break
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			why := archiveStop(err, caps)
			if len(idx.files) == 0 && !errors.Is(err, errArchiveCap) && !errors.Is(err, errUnpackedCap) {
				return nil, fmt.Errorf("the archive could not be read: %s", why)
			}
			stop(why)
			break
		}
		// Files only: no directory, link or GitHub's own header carries text of the commit's.
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != 0 {
			continue
		}
		_, p, ok := strings.Cut(hdr.Name, "/")
		if !ok || p == "" || hdr.Size > caps.fileMax || !utf8.ValidString(p) || slices.Contains(strings.Split(p, "/"), "..") || !keep(p) {
			continue
		}
		buf := make([]byte, hdr.Size)
		if _, err := io.ReadFull(tr, buf); err != nil {
			stop(archiveStop(err, caps))
			break
		}
		if !reviewTextual(buf) || reviewGeneratedText(buf) {
			continue
		}
		if kept+int64(len(buf)) > caps.kept {
			stop(fmt.Sprintf("it holds more text than the %d MB a review keeps", caps.kept>>20))
			break
		}
		if len(idx.files) >= caps.files {
			stop(fmt.Sprintf("it holds more than the %d files a review keeps", caps.files))
			break
		}
		kept += int64(len(buf))
		idx.files = append(idx.files, &reviewIndexedFile{path: p, text: strings.ReplaceAll(string(buf), "\r\n", "\n")})
	}
	slices.SortFunc(idx.files, func(a, b *reviewIndexedFile) int { return strings.Compare(a.path, b.path) })
	return idx, nil
}

// archiveStop says in words why reading an archive stopped.
func archiveStop(err error, caps reviewIndexCaps) string {
	switch {
	case errors.Is(err, errArchiveCap):
		return fmt.Sprintf("its archive is larger than the %d MB a review downloads", caps.download>>20)
	case errors.Is(err, errUnpackedCap):
		return fmt.Sprintf("its archive unpacks to more than the %d MB a review reads through", caps.unpacked>>20)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "the time for reading it ran out"
	}
	return "the download broke off"
}

// reviewTextual reports whether a file's bytes are text: no NUL in its first 8 KB, which is git's
// own test, and UTF-8 there, allowing a character cut at the end of the sample.
func reviewTextual(b []byte) bool {
	head := b[:min(len(b), 8000)]
	if bytes.IndexByte(head, 0) >= 0 {
		return false
	}
	for i := 0; i < utf8.UTFMax && len(head) > 0 && !utf8.Valid(head); i++ {
		head = head[:len(head)-1]
	}
	return utf8.Valid(head)
}

// reviewGeneratedText reports a generated file by the header its generator wrote in its first lines,
// the convention reviewGenerated reads in a diff.
func reviewGeneratedText(b []byte) bool {
	for i, rest := 0, b; i < 5 && len(rest) > 0; i++ {
		line, more, _ := bytes.Cut(rest, []byte("\n"))
		if generatedHeader.Match(line) {
			return true
		}
		rest = more
	}
	return false
}

// grepKeeps says whether a head file belongs in the copy grep searches: what a review leaves out of
// the changed files it reads — the team's ignored paths, generated and vendored code, lockfiles —
// and installed packages, build output and credential files, which are never what a definition is
// looked up in.
func (r *reviewRun) grepKeeps(p string) bool {
	if r.spec.Settings.IgnoresPath(p) || review.MatchAny(reviewGeneratedPaths, p) || review.MatchAny(reviewLockfiles, p) || reviewSecretPath(p) {
		return false
	}
	dirs := strings.Split(p, "/")
	for _, d := range dirs[:len(dirs)-1] {
		if slices.Contains(reviewGrepSkipDirs, d) {
			return false
		}
	}
	return true
}

// headIndex is the run's copy of its head, downloaded and read by the first grep that asks.
func (r *reviewRun) headIndex(ctx context.Context) (*reviewHeadIndex, error) {
	return r.grepIdx.get(ctx, func(ctx context.Context) (*reviewHeadIndex, error) {
		ctx, cancel := context.WithTimeout(ctx, reviewArchiveWall)
		defer cancel()
		body, err := r.gh.Archive(ctx, r.head)
		if err != nil {
			return nil, err
		}
		defer body.Close()
		return buildHeadIndex(ctx, r.head, body, reviewIndexLimits, r.grepKeeps)
	})
}

// ---- the search ----

// reviewGrepArgs are grep's arguments, read leniently: a model writes "true" and "40" as text often
// enough that refusing them would cost it a round.
type reviewGrepArgs struct {
	Pattern    string          `json:"pattern"`
	Literal    json.RawMessage `json:"literal"`
	Path       string          `json:"path"`
	MaxResults json.RawMessage `json:"max_results"`
}

func (a reviewGrepArgs) literal() bool {
	switch strings.ToLower(strings.Trim(strings.TrimSpace(string(a.Literal)), `"`)) {
	case "true", "1", "yes":
		return true
	}
	return false
}

func (a reviewGrepArgs) limit() int {
	n, err := strconv.Atoi(strings.Trim(strings.TrimSpace(string(a.MaxResults)), `"`))
	if err != nil || n <= 0 {
		return reviewGrepDefault
	}
	return min(n, reviewGrepMax)
}

// grep is the grep tool: a regular expression over every text file of the head, line by line.
func (r *reviewRun) grep(ctx context.Context, raw string) (string, error) {
	var a reviewGrepArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return "", errors.New("the arguments are not a JSON object of the fields this tool takes")
	}
	pat := a.Pattern
	switch {
	case strings.TrimSpace(pat) == "":
		return "", errors.New("say what to search for in pattern")
	case len(pat) > reviewGrepPattern:
		return "", fmt.Errorf("a pattern is at most %d characters", reviewGrepPattern)
	}
	if a.literal() {
		pat = regexp.QuoteMeta(pat)
	}
	// Multi-line mode, so ^ and $ mean the start and end of a line, as they do in grep.
	re, err := regexp.Compile("(?m)" + pat)
	if err != nil {
		return "", fmt.Errorf("the pattern is not a regular expression Go's RE2 reads (%s); set literal to true to search for it as plain text",
			truncate(strings.TrimPrefix(err.Error(), "error parsing regexp: "), 160))
	}
	if !r.grepIdx.spend() {
		return "", fmt.Errorf("this review has run its %d greps; read the files you have found with read_file instead", reviewGrepMaxCalls)
	}
	idx, err := r.headIndex(ctx)
	if err != nil {
		return "", fmt.Errorf("grep could not read the pull request's head (%s). find_code searches the default branch, and read_file reads any file at the head",
			truncate(err.Error(), 200))
	}
	return idx.search(ctx, r.repo, re, a.Pattern, strings.TrimSpace(a.Path), a.limit()), nil
}

// grepGlob reports whether file p is in what a grep's path asked for: a directory or a file named
// whole ("src/auth"), a glob over the whole path when it has a slash ("src/**/*.go"), and otherwise
// a glob over the file's name at any depth ("*.ts"), the way a pattern in .gitignore is read.
func grepGlob(glob, p string) bool {
	glob = strings.TrimPrefix(strings.TrimPrefix(glob, "./"), "/")
	switch {
	case glob == "":
		return true
	case !strings.ContainsAny(glob, "*?"):
		dir := strings.TrimSuffix(glob, "/")
		return p == dir || strings.HasPrefix(p, dir+"/")
	case strings.Contains(strings.TrimSuffix(glob, "/"), "/"):
		return review.Match(glob, p)
	}
	return review.Match(glob, path.Base(p)) || review.Match(glob, p)
}

// search runs re over every file under glob and writes what grep answers: a header, then path:line:
// text for each matching line up to limit and what fits in a tool result, then how many more there
// were and in how many files. A line is counted once however many times it matches.
func (ix *reviewHeadIndex) search(ctx context.Context, repo string, re *regexp.Regexp, pattern, glob string, limit int) string {
	var shown []string
	size, total, files, moreLines := 0, 0, 0, 0
	moreFiles := map[string]bool{}
	searched := 0
	budget := reviewToolOutputChars - 1200 // room for the header and the notes after the lines
	for _, f := range ix.files {
		if !grepGlob(glob, f.path) {
			continue
		}
		if searched++; searched%256 == 0 && ctx.Err() != nil {
			break
		}
		hit := false
		grepLines(f.text, re, func(n int, line string) {
			total++
			hit = true
			if len(shown) >= limit || size >= budget {
				moreLines++
				moreFiles[f.path] = true
				return
			}
			text, _ := cutRunes(strings.TrimSpace(f.line(n, line)), reviewGrepLineChars)
			s := fmt.Sprintf("%s:%d: %s", untrusted(f.path), n, untrusted(text))
			shown = append(shown, s)
			size += len(s) + 1
		})
		if hit {
			files++
		}
	}
	where := fmt.Sprintf("%s @ %s (the pull request's head)", repo, shortSHA(ix.sha))
	q, _ := cutRunes(oneLine(pattern), 100)
	in := ""
	if glob != "" {
		in, _ = cutRunes(glob, 100)
		in = " under " + untrusted(in)
	}
	var b strings.Builder
	if total == 0 {
		fmt.Fprintf(&b, "No line of %s%s matches %s (%d files searched).\n", where, in, untrusted(strconv.Quote(q)), searched)
	} else {
		fmt.Fprintf(&b, "%s%s: %d matching lines in %d files for %s, %d shown.\n", where, in, total, files, untrusted(strconv.Quote(q)), len(shown))
		b.WriteString(strings.Join(shown, "\n") + "\n")
		if moreLines > 0 {
			fmt.Fprintf(&b, "%d more matches in %d files: narrow it with path or a more specific pattern.\n", moreLines, len(moreFiles))
		}
	}
	if ctx.Err() != nil {
		b.WriteString("The search stopped early: the time for this pass ran out.\n")
	}
	if ix.partial != "" {
		fmt.Fprintf(&b, "Only part of the repository was searched: %s. A file not listed may still match — read_file reads any file, and find_code searches the default branch whole.\n", ix.partial)
	}
	if total == 0 && glob != "" && searched == 0 {
		b.WriteString("No text file is under that path; leave path out to search everything.\n")
	}
	return b.String()
}

// grepLines calls hit with the number and text of every line of text that re matches, once a line.
// re may match across a newline; the line it starts on is the one counted. Nothing after a final
// newline is a line, so a pattern that matches nothing at all does not invent one there.
func grepLines(text string, re *regexp.Regexp, hit func(n int, line string)) {
	pos, n := 0, 1
	for pos < len(text) {
		loc := re.FindStringIndex(text[pos:])
		if loc == nil {
			return
		}
		m := pos + loc[0]
		n += strings.Count(text[pos:m], "\n")
		from := strings.LastIndexByte(text[:m], '\n') + 1
		to := strings.IndexByte(text[m:], '\n')
		if to < 0 {
			hit(n, text[from:])
			return
		}
		to += m
		hit(n, text[from:to])
		pos, n = to+1, n+1
	}
}
