package app

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"attesttag/internal/review"
)

// Fixes on the pull request. A review finding is a problem somebody now has to go and fix; this lets
// them have it fixed where they read it. Somebody with write access to the repository asks, three
// ways, and a fix job commits the change onto the pull request's own branch (JobModePR) — one more
// commit, never forced, nothing new to merge:
//
//   - a tick in the finding comment's fix box (review.FixBoxText), which GitHub delivers as an edit
//     of the bot's own comment. GitHub never delivers a reaction, which is why a thumbs-up cannot be
//     the request: there is nothing to receive.
//   - a reply in the finding's thread that starts `@slug fix`, which fixes that finding;
//   - `@slug fix` on the conversation, which fixes every open finding said there, or the severities
//     it names (`@slug fix p0 p1`).
//
// What decides, in this order: the pull request must be one whose review is posted (shadow writes
// nothing to GitHub, and a push is the largest write there is), on an organisation with code review,
// with fixes allowed for the repository (review.Settings.Fixes) and a fix worker that can run for the
// organisation; open, from a branch of this repository — a fork's branch is not the App's to push to
// — that is not the repository's default branch nor the one it merges into; asked for by somebody
// GitHub says may push to the repository (WritePermission), since what the job does is push, and an
// organisation member who may only read must not gain a write through the bot; at most one job at a
// time on a pull request and reviewFixPerPRDay a day, and reviewFixPerHour requests a person an hour.
// Every refusal is answered where it was asked, in Go's words, which name no plan, budget or
// setting a public repository's readers have no business knowing.
//
// The job is a fix job like any other — the worker, the checks before and after, the budget, the
// console's Jobs page — with three differences: it pushes as the App, through the installation the
// review came through, whatever stored connection lends it its recipe; a change that breaks a check
// that passed before it is not pushed (worker pushToPR); and it answers on the pull request
// (reviewFixEnded): a reply with the commit and what the checks said, and a review of the new head,
// so the findings it fixed are closed by the same re-review a person's push gets (review_resolve.go).
// A job asked for on GitHub has no chat thread; its spend is logged where the review's is.

const (
	reviewFixPerPRDay    = 6  // fix jobs one pull request may start in a day
	reviewFixPerHour     = 5  // fix jobs one person may start in an hour, across pull requests
	reviewFixAsksPerHour = 20 // requests one person may make in an hour before they are not even read
	reviewFixFindingsMax = 10 // findings one job is asked to fix, most severe first
	// How long a request is remembered, so that one delivered twice starts one job.
	reviewFixSeenFor = 30 * 24 * time.Hour
	// How much of the job's diff a refusal to push shows, so it can be applied by hand. GitHub takes
	// 65,536 characters; the rest of the answer needs a few hundred.
	reviewFixDiffMax = 30_000
)

// reviewFixAsk is one request for a fix, however it was made.
type reviewFixAsk struct {
	d     *githubDelivery
	repo  string
	n     int
	login string
	via   string // box, thread or command
	// ref is what makes two deliveries of one request the same request: the comment that asked, or
	// for a ticked box the edit's own delivery.
	ref string
	// finding is the one a box or a thread is about; nil for a command on the conversation, which
	// fixes the open findings of severities (all of them when empty).
	finding    *ReviewFinding
	severities []review.Severity
	// note is what the asker wrote after the command, which the job is given as their words.
	note string
	// thread is the finding's inline comment, whose thread is answered; zero answers on the
	// conversation. react is the comment that asked — a reply, a command, or for a box the finding's
	// own comment — which gets the eyes; reacted says the command handler already put them on.
	thread      int64
	react       int64
	reactInline bool
	reacted     bool
	askedURL    string
}

// ---- deliveries ----

// githubReviewCommentEdit is the part of a pull_request_review_comment "edited" delivery a ticked box
// needs: the comment as it is now, and its body as it was.
type githubReviewCommentEdit struct {
	githubReviewCommentPayload
	Changes struct {
		Body *struct {
			From string `json:"from"`
		} `json:"body"`
	} `json:"changes"`
}

// reviewFixBoxEvent is an edit of an inline comment: if it ticked the fix box on one of this
// organisation's findings, the finding is fixed. Every other edit — the bot's own, a box unticked,
// somebody rewriting the text — asks for nothing.
func (b *Bot) reviewFixBoxEvent(ctx context.Context, d *githubDelivery, raw []byte) error {
	var p githubReviewCommentEdit
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	repo := p.Repository.FullName
	if p.Action != "edited" || p.Changes.Body == nil || !validGitHubRepo(repo) || p.PullRequest.Number <= 0 ||
		b.reviewFromBot(p.Sender) || !b.reviewByUs(p.Comment.User) || !review.FixBoxTicked(p.Changes.Body.From, p.Comment.Body) {
		return nil
	}
	f, err := b.reviewFixFindingOn(ctx, d, repo, p.PullRequest.Number, p.Comment.ID)
	if err != nil || f == nil {
		return err
	}
	return b.reviewFix(ctx, reviewFixAsk{d: d, repo: repo, n: p.PullRequest.Number, login: p.Sender.Login, via: "box",
		ref: fmt.Sprintf("box:%d:%s", p.Comment.ID, d.ID), finding: f, thread: p.Comment.ID, react: p.Comment.ID,
		reactInline: true, askedURL: p.Comment.HTMLURL})
}

// reviewFixThreadCommand reports whether a new inline comment is `@slug fix` in a finding's thread,
// and if it is, acts on it. A comment that is anything else is left to the reply flow.
func (b *Bot) reviewFixThreadCommand(ctx context.Context, d *githubDelivery, p *githubReviewCommentPayload) (bool, error) {
	c := p.Comment
	slug := b.reviewSlug()
	if slug == "" || c.InReplyToID <= 0 {
		return false, nil
	}
	cmd, ok := review.ParseCommand(c.Body, slug)
	if !ok || cmd.Verb != review.VerbFix {
		return false, nil
	}
	f, err := b.reviewFixFindingOn(ctx, d, p.Repository.FullName, p.PullRequest.Number, c.InReplyToID)
	if err != nil {
		return true, err
	}
	if f == nil {
		return true, nil // not under one of our findings: nothing here was asked of the reviewer
	}
	return true, b.reviewFix(ctx, reviewFixAsk{d: d, repo: p.Repository.FullName, n: p.PullRequest.Number, login: c.User.Login,
		via: "thread", ref: fmt.Sprintf("comment:%d", c.ID), finding: f, note: fixNote(cmd.Text), thread: c.InReplyToID,
		react: c.ID, reactInline: true, askedURL: c.HTMLURL})
}

// fix answers `@slug fix` on the conversation: every open finding said on the pull request, or those
// of the severities named.
func (c *reviewCommandCall) fix(ctx context.Context) error {
	if c.quiet {
		// Recorded in shadow: nothing is written to GitHub, and a push is the largest write there is.
		c.audit(ctx, "ignored: fixes are not pushed to a pull request reviewed in shadow", nil)
		return nil
	}
	c.audit(ctx, "passed on to a fix", nil)
	return c.b.reviewFix(ctx, reviewFixAsk{d: c.d, repo: c.repo, n: c.n, login: c.login, via: "command",
		ref: fmt.Sprintf("comment:%d", c.p.Comment.ID), severities: c.cmd.Severities, note: fixNote(c.cmd.Text),
		react: c.p.Comment.ID, reacted: true, askedURL: c.p.Comment.HTMLURL})
}

// fixNote is the asker's own words after the command — "fix this, keep the old name" — without the
// command itself, for the brief.
func fixNote(text string) string {
	line, rest, _ := strings.Cut(strings.TrimSpace(text), "\n")
	words := strings.Fields(line)
	i := 0
	for i < len(words) {
		w := strings.ToLower(strings.TrimRight(words[i], ".!,:"))
		if w != "fix" && w != "please" && w != "this" && w != "it" && !review.Severity(strings.ToUpper(w)).Valid() {
			break
		}
		i++
	}
	return strings.TrimSpace(strings.Join(words[i:], " ") + "\n" + rest)
}

// reviewFixFindingOn is the organisation's finding posted as inline comment id on pull request n of
// repo, or nil when the comment is not one: a finding is fixed only on the pull request it is on.
func (b *Bot) reviewFixFindingOn(ctx context.Context, d *githubDelivery, repo string, n int, id int64) (*ReviewFinding, error) {
	if id <= 0 {
		return nil, nil
	}
	f, err := b.store.ReviewFindingByComment(ctx, d.OrgID, id)
	if err != nil || f == nil {
		return nil, err
	}
	pr, err := b.store.ReviewPR(ctx, d.OrgID, f.ReviewPRID)
	if err != nil {
		return nil, err
	}
	if pr == nil || !strings.EqualFold(pr.Repo, repo) || pr.Number != n {
		slog.Warn("code review: a fix names a finding of another pull request; ignored", "org", d.OrgID, "repo", repo, "pr", n, "comment", id)
		return nil, nil
	}
	return f, nil
}

// ---- the request ----

// reviewFix acts on one request. What it needs to read from GitHub — the pull request, the asker's
// permission — is read first, and a failure to read is returned for the delivery to be tried again;
// from the moment the request is marked as seen everything is answered on the pull request instead,
// so a delivery tried again never starts a second job or says the same thing twice.
func (b *Bot) reviewFix(ctx context.Context, a reviewFixAsk) error {
	orgID := a.d.OrgID
	// Before anything is read: on a public repository anybody can reply in a thread, and each request
	// costs reads of GitHub before its asker can be refused.
	if ok, _, err := b.store.countThrottle(ctx, fmt.Sprintf("review-fix-ask:%d:%s", orgID, strings.ToLower(a.login)),
		reviewFixAsksPerHour, time.Hour); err != nil {
		return err
	} else if !ok {
		slog.Info("code review: fix requests throttled; not read", "org", orgID, "repo", a.repo, "pr", a.n, "login", a.login)
		return nil
	}
	eff, s, err := b.reviewEffective(ctx, orgID, a.d.InstallationID, a.repo)
	if err != nil {
		return err
	}
	if s != nil {
		slog.Info("code review: a fix asked for on a repository not reviewed here", "org", orgID, "repo", a.repo, "reason", s.Reason)
		return nil
	}
	gh, err := b.reviewClient(orgID, a.d.InstallationID, a.repo, a.n)
	if err != nil {
		return err
	}
	pull, err := gh.Pull(ctx)
	if err != nil {
		return err
	}
	if pull.Number == 0 {
		pull.Number = a.n
	}
	pr, err := b.store.ReviewPRByNumber(ctx, orgID, a.repo, a.n)
	if err != nil {
		return err
	}
	if reviewDestination(eff, pull) != review.ModeLive {
		// Recorded in shadow: nothing is written to GitHub, a push least of all.
		b.reviewFixAudit(ctx, orgID, a, "ignored: the pull request's review is not posted", nil)
		return nil
	}
	may, err := gh.WritePermission(ctx, a.login)
	if err != nil {
		return err
	}
	if !b.store.AlertOnce(ctx, orgID, "review-fix:"+a.ref, reviewFixSeenFor) {
		return nil // this request, delivered again
	}

	if a.thread > 0 {
		gh.KnowReviewComment(a.thread)
	}
	if a.react > 0 {
		if a.reactInline {
			gh.KnowReviewComment(a.react)
		} else {
			gh.KnowIssueComment(a.react)
		}
	}
	refuse := func(outcome, text string) error {
		b.reviewFixAudit(ctx, orgID, a, "refused: "+outcome, nil)
		a.say(ctx, gh, text)
		return nil
	}
	if !may {
		// Said once a day to each person on a pull request: anybody can reply on a public repository,
		// and the same refusal in every thread is noise somebody could make the bot produce.
		if !b.store.AlertOnce(ctx, orgID, fmt.Sprintf("review-fix-refused:%s#%d:%s", strings.ToLower(a.repo), a.n,
			strings.ToLower(a.login)), reviewRefusalEvery) {
			b.reviewFixAudit(ctx, orgID, a, "ignored: no write access, told already today", nil)
			return nil
		}
		return refuse("no write access", "Only people who can push to this repository can ask attest_tag to push a fix to it.")
	}
	if s := b.reviewPlanGate(ctx, orgID); s != nil {
		return refuse(s.Detail, reviewSkipSentence(s.Reason))
	}
	if !eff.Fixes {
		return refuse("fixes are off for the repository", "Fixes from the pull request are turned off for this repository.")
	}
	if err := b.reviewFixAvailable(ctx, orgID); err != nil {
		b.reviewFixAudit(ctx, orgID, a, "refused: the fix worker cannot run", map[string]any{"why": err.Error()})
		a.say(ctx, gh, "attest_tag cannot run a fix for this organisation right now. An admin of its attest_tag account can see why in the console.")
		return nil
	}
	head, base := pull.Head.Ref, pull.Base.Ref
	switch {
	case pull.State != "open" || pull.Merged:
		return refuse("the pull request is closed", "This pull request is closed, so there is no branch to fix.")
	case pull.IsFork():
		return refuse("the pull request is from a fork",
			"This pull request comes from a fork, and attest_tag pushes only to this repository's own branches. "+
				"A finding's suggested change, where it has one, can still be committed from its comment.")
	case head == "" || head == base || (pull.Base.Repo != nil && head == pull.Base.Repo.DefaultBranch):
		return refuse("the pull request's branch is the default branch",
			"This pull request's branch is the repository's default branch, which attest_tag does not push to.")
	case pr == nil:
		return refuse("not reviewed", "This pull request has not been reviewed here, so there is no finding to fix.")
	}
	fs, err := b.reviewFixFindings(ctx, orgID, pr, a)
	if err != nil {
		slog.Warn("code review: the findings to fix could not be read", "org", orgID, "repo", a.repo, "pr", a.n, "err", err)
		return nil // marked as seen: tried again it would be dropped, so it is not returned
	}
	if len(fs) == 0 {
		if a.finding != nil {
			return refuse("the finding is not open", fmt.Sprintf("This finding is %s, so there is nothing to fix.", findingStatusWords(a.finding.Status)))
		}
		return refuse("no open findings", "There is no open finding on this pull request to fix.")
	}
	channel, thread := jobGitHubChannel+strings.ToLower(a.repo), fmt.Sprintf("pr:%d", a.n)
	if active, err := b.store.ActiveJobsInThread(ctx, orgID, "", channel, thread); err == nil && len(active) > 0 {
		return refuse("a fix job is running", fmt.Sprintf("Fix job #%d is still working on this pull request. Ask again once it has answered.", active[0].ID))
	}
	if n, err := b.store.JobsInThreadSince(ctx, orgID, "", channel, thread, time.Now().Add(-24*time.Hour)); err == nil && n >= reviewFixPerPRDay {
		return refuse("the pull request's fixes for the day", fmt.Sprintf("This pull request has had %d fix jobs in the last day, as many as it gets. Ask again tomorrow.", n))
	}
	if ok, _, err := b.store.countThrottle(ctx, fmt.Sprintf("review-fix:%d:%s", orgID, strings.ToLower(a.login)), reviewFixPerHour, time.Hour); err == nil && !ok {
		return refuse("the asker's fixes for the hour", "You have asked for as many fixes as one person gets in an hour. Ask again later.")
	}

	spec := b.reviewFixSpec(ctx, orgID, a, pull, fs)
	job, err := b.jobs.Dispatch(ctx, DispatchRequest{OrgID: orgID, Spec: spec, Approval: "github"})
	if err != nil {
		if job != nil {
			// The job exists and was finished as failed, which reviewFixEnded has answered.
			return nil
		}
		b.reviewFixAudit(ctx, orgID, a, "refused: the job did not start", map[string]any{"why": err.Error()})
		a.say(ctx, gh, "attest_tag could not start a fix job for this organisation right now. An admin of its attest_tag account can see why in the console.")
		return nil
	}
	if !a.reacted && a.react > 0 {
		var err error
		if a.reactInline {
			err = gh.ReactToReviewComment(ctx, a.react, "eyes")
		} else {
			err = gh.ReactToIssueComment(ctx, a.react, "eyes")
		}
		if err != nil {
			slog.Debug("code review: no eyes reaction on a fix request", "repo", a.repo, "pr", a.n, "err", err)
		}
	}
	what := "this finding"
	if a.finding == nil {
		what = findingsWords(len(fs))
	}
	a.say(ctx, gh, fmt.Sprintf("Working on it: fix job #%d is fixing %s and will push a commit to `%s`, then answer here.", job.ID, what, head))
	ids := make([]string, len(fs))
	for i, f := range fs {
		ids[i] = f.PublicID
	}
	b.reviewFixAudit(ctx, orgID, a, "started", map[string]any{"job": job.ID, "findings": ids, "branch": head})
	slog.Info("code review: a fix job started on a pull request", "org", orgID, "repo", a.repo, "pr", a.n, "job", job.ID,
		"via", a.via, "by", a.login, "findings", len(fs))
	return nil
}

// reviewFixAvailable says why this deployment cannot run a fix job for the organisation, or nil: no
// worker at all, or no model credential it may hand one (JobRunner.credentialPolicy).
func (b *Bot) reviewFixAvailable(ctx context.Context, orgID int64) error {
	if b.jobs == nil || !b.jobs.Enabled() {
		return errors.New("the fix worker is switched off (WORKER_MODE)")
	}
	return b.jobs.credentialPolicy(ctx, orgID)
}

// reviewFixFindings is what the job is asked to fix: the finding a box or a thread is about, while it
// still stands, or the open findings said on the pull request of the severities a command named,
// most severe first, at most reviewFixFindingsMax. A note is not a problem and a pre-existing one is
// not the pull request's, so a command passes over both; a finding somebody points at is theirs to
// ask about.
func (b *Bot) reviewFixFindings(ctx context.Context, orgID int64, pr *ReviewPR, a reviewFixAsk) ([]*ReviewFinding, error) {
	standing := func(f *ReviewFinding) bool {
		return f.Status == review.FindingOpen || f.Status == review.FindingDisputed
	}
	if a.finding != nil {
		f, err := b.store.ReviewFindingByComment(ctx, orgID, a.finding.GitHubCommentID)
		if err != nil || f == nil || !standing(f) {
			return nil, err
		}
		return []*ReviewFinding{f}, nil
	}
	said, err := b.store.reviewFindingsSaid(ctx, orgID, pr.ID, false, 0)
	if err != nil {
		return nil, err
	}
	var out []*ReviewFinding
	for _, f := range said {
		if f.Status != review.FindingOpen || f.Kind == reviewKindNote || f.PreExisting {
			continue
		}
		if len(a.severities) > 0 && !slices.Contains(a.severities, f.Severity) {
			continue
		}
		out = append(out, f)
	}
	slices.SortStableFunc(out, func(x, y *ReviewFinding) int { return cmp.Compare(x.Severity, y.Severity) })
	if len(out) > reviewFixFindingsMax {
		out = out[:reviewFixFindingsMax]
	}
	return out, nil
}

// reviewFixSpec is the job's brief: the findings as the review stored them — what is wrong, where,
// the code it quoted, the change it suggested — and the asker's own words. The pull request's title
// and description are not in it: they are the author's case, and the findings are what to fix.
func (b *Bot) reviewFixSpec(ctx context.Context, orgID int64, a reviewFixAsk, pull *githubPull, fs []*ReviewFinding) JobSpec {
	title := "Fix " + findingsWords(len(fs)) + " from code review"
	if len(fs) == 1 {
		title = "Fix: " + fs[0].Title
	}
	var req strings.Builder
	fmt.Fprintf(&req, "attest_tag's code review of pull request #%d (%s into %s) raised the %s below. Fix %s on the pull request's branch, at the code each one points to.\n",
		a.n, pull.Head.Ref, pull.Base.Ref, pluralWord(len(fs), "finding", "findings"), pluralWord(len(fs), "it", "them"))
	var evidence strings.Builder
	var files []string
	for i, f := range fs {
		fmt.Fprintf(&req, "\n%d. %s · %s\n   At %s, as reviewed at %s.\n", i+1, f.Severity, f.Title, lineRef(f.Path, f.StartLine, f.Line), shortSHA(f.AnchorSHA))
		if sc := strings.TrimSpace(f.Scenario); sc != "" {
			req.WriteString("   " + strings.ReplaceAll(sc, "\n", "\n   ") + "\n")
		}
		if s := f.Suggestion; s != nil && strings.TrimSpace(s.Code) != "" {
			fmt.Fprintf(&req, "   The review suggested replacing %s with:\n   ```\n   %s\n   ```\n",
				lineRef(f.Path, s.StartLine, s.Line), strings.ReplaceAll(s.Code, "\n", "\n   "))
		}
		if !slices.Contains(files, f.Path) && f.Path != "" {
			files = append(files, f.Path)
		}
		for _, e := range f.Evidence {
			if e.Repo != "" && !strings.EqualFold(e.Repo, a.repo) {
				continue
			}
			fmt.Fprintf(&evidence, "Finding %d, %s:\n%s\n\n", i+1, lineRef(e.Path, e.StartLine, e.EndLine), strings.TrimSpace(e.Quote))
		}
	}
	if a.note != "" {
		fmt.Fprintf(&req, "\n@%s, who asked for the fix, added: %s\n", a.login, a.note)
	}
	link := cmp.Or(a.askedURL, fmt.Sprintf("https://github.com/%s/pull/%d", a.repo, a.n))
	spec := JobSpec{Repo: a.repo, Title: truncate(title, 80), Kind: jobKindBugfix,
		Requirement: truncate(redact(req.String()), 8000), Evidence: truncate(redact(evidence.String()), 8000),
		Acceptance: []string{
			pluralWord(len(fs), "the finding above is fixed", "each finding above is fixed") + " at the code it points to",
			"nothing the findings do not need is changed: the rest of the pull request is its author's",
			"the repository's checks pass, or fail no more than they did before the change",
		},
		FilesHint: cleanList(files, 20, 200), ThreadLink: link,
		Requester: "github:" + a.login, RequesterName: a.login,
		Channel: jobGitHubChannel + strings.ToLower(a.repo), ThreadTS: fmt.Sprintf("pr:%d", a.n),
		Mode: JobModePR, BaseBranch: pull.Head.Ref, Branch: pull.Head.Ref, HeadSHA: pull.Head.SHA,
		PR: &JobPRRef{Number: a.n, Base: pull.Base.Ref, URL: fmt.Sprintf("https://github.com/%s/pull/%d", a.repo, a.n),
			InstallationID: a.d.InstallationID, Thread: a.thread, AskedBy: a.login, AskedURL: a.askedURL}}
	for _, f := range fs {
		spec.PR.Findings = append(spec.PR.Findings, f.PublicID)
	}
	if conn := b.storedRepoConnection(ctx, orgID, a.repo); conn != nil {
		spec.ConnectionID = conn.ID
	}
	return spec
}

// storedRepoConnection is the organisation's stored connection for repo, when it has one that is
// working: what an admin said about building and testing it (its recipe, its test command) and the
// worker image earlier jobs found it needs. The job pushes through the App whatever this is
// (jobPushConnection); nil runs it on what the worker detects.
func (b *Bot) storedRepoConnection(ctx context.Context, orgID int64, repo string) *Connection {
	conns, err := b.store.AllConnections(ctx, orgID)
	if err != nil {
		return nil
	}
	var best *Connection
	for _, c := range conns {
		if c.Preset != "github" || c.Status != "active" || !strings.EqualFold(c.Repo, repo) {
			continue
		}
		// One somebody told how to build the repository first, then one an earlier job worked out.
		if best == nil || (c.Recipe.SetByAdmin() && !best.Recipe.SetByAdmin()) || (best.Recipe == nil && c.Recipe != nil) {
			best = c
		}
	}
	return best
}

// ---- the end ----

// reviewFixEnded answers on the pull request when a fix job asked for there ends: what was pushed and
// what the checks said, or why nothing was. A push is followed by a review of the new head — the
// re-review that closes the findings it fixed — asked for whatever the repository's "when" setting
// says, as a command's is, since somebody asked for this commit. Best effort: the job's outcome is
// stored whatever GitHub says, and the console shows it.
func (b *Bot) reviewFixEnded(ctx context.Context, j *Job, res *JobResult) {
	var spec JobSpec
	if b.review == nil || json.Unmarshal([]byte(j.Spec), &spec) != nil || spec.Mode != JobModePR || spec.PR == nil || res == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reviewWriteTimeout)
	defer cancel()
	inst, n, repo := spec.PR.InstallationID, spec.PR.Number, spec.Repo
	eff, s, err := b.reviewEffective(ctx, j.OrgID, inst, repo)
	if err != nil || s != nil {
		slog.Warn("code review: a fix job's end is not answered on a repository no longer reviewed here", "job", j.ID, "repo", repo, "err", err)
		return
	}
	gh, err := b.reviewClient(j.OrgID, inst, repo, n)
	if err != nil {
		slog.Warn("code review: a fix job's end is not answered", "job", j.ID, "err", err)
		return
	}
	pull, err := gh.Pull(ctx)
	if err != nil {
		slog.Warn("code review: a fix job's end is not answered", "job", j.ID, "err", err)
		return
	}
	if reviewDestination(eff, pull) != review.ModeLive {
		slog.Info("code review: a fix job ended on a pull request now reviewed in shadow; nothing said there", "job", j.ID, "repo", repo, "pr", n)
		return
	}
	a := reviewFixAsk{repo: repo, n: n, thread: spec.PR.Thread}
	if a.thread > 0 {
		gh.KnowReviewComment(a.thread)
	}
	diff := ""
	if j.Status != JobSucceeded && res.Error.Code == "checks_broken" {
		diff, _, _, _ = b.store.JobFile(ctx, j.OrgID, j.ID, "diff")
	}
	a.say(ctx, gh, reviewFixReport(j, res, spec, b.reviewSlug(), diff))
	b.reviewFixAudit(ctx, j.OrgID, reviewFixAsk{repo: repo, n: n, login: spec.PR.AskedBy}, "job "+j.Status,
		map[string]any{"job": j.ID, "head": res.HeadSHA})
	if j.Status != JobSucceeded {
		return
	}
	row, err := b.store.ReviewPRByNumber(ctx, j.OrgID, repo, n)
	scope := ""
	if err == nil && row != nil && row.LastReviewedSHA != "" {
		scope = reviewScopeSinceLast
	}
	_, err = b.enqueueReview(ctx, j.OrgID, repo, n, reviewRequest{InstallationID: inst, Trigger: "fix",
		TriggerRef: fmt.Sprintf("job:%d", j.ID), RequestedBy: "github:" + spec.PR.AskedBy, Scope: scope, BypassFilters: true})
	var skip *reviewSkip
	if err != nil && !errors.As(err, &skip) {
		slog.Warn("code review: the review after a fix was not queued", "job", j.ID, "repo", repo, "pr", n, "err", err)
	}
}

// reviewFixReport is the answer when a fix job ends, in Go's words around the engine's summary,
// which say what was pushed, what the checks said and how to try again.
func reviewFixReport(j *Job, res *JobResult, spec JobSpec, slug, diff string) string {
	var b strings.Builder
	what := "this finding"
	if spec.PR.Thread == 0 {
		what = findingsWords(len(spec.PR.Findings))
	}
	again := "Tick the box again, or reply `@" + slug + " fix`, to try again."
	if spec.PR.Thread == 0 {
		again = "`@" + slug + " fix` tries again."
	}
	switch j.Status {
	case JobSucceeded:
		sha := res.HeadSHA
		fmt.Fprintf(&b, "Pushed [`%s`](https://github.com/%s/commit/%s) to `%s`, fixing %s.", shortSHA(sha), spec.Repo, sha, spec.Branch, what)
		if sum := strings.TrimSpace(res.Summary); sum != "" {
			b.WriteString("\n\n" + truncate(sum, 1500))
		}
		if checks := fixChecksLine(res); checks != "" {
			b.WriteString("\n\n" + checks)
		}
		if res.Note != "" {
			b.WriteString("\n\n> Note: " + truncate(oneLine(res.Note), 500) + ".")
		}
		b.WriteString("\n\nA review of the new head follows, and closes what the commit fixed.")
	case JobCancelled:
		fmt.Fprintf(&b, "Fix job #%d was cancelled before it pushed anything. %s", j.ID, again)
	case JobTimeout:
		fmt.Fprintf(&b, "Fix job #%d ran out of time before it could push a fix. %s", j.ID, again)
	default:
		fmt.Fprintf(&b, "attest_tag could not fix %s: %s. %s", what, fixFailureWords(res, spec.Branch), again)
		if d := strings.TrimSpace(diff); d != "" && len(d) <= reviewFixDiffMax {
			b.WriteString("\n\n<details><summary>The change it made, not pushed</summary>\n\n```diff\n" + d + "\n```\n\n</details>")
		}
	}
	spend := ""
	if t := jobTokensWords(j.TokensIn, j.TokensCached, j.TokensOut); t != "" {
		spend = " " + jobCostWords(j) + t + "."
	}
	fmt.Fprintf(&b, "\n\n<sub>attest_tag fix job #%d, asked for by @%s.%s</sub>", j.ID, spec.PR.AskedBy, spend)
	return b.String()
}

// fixFailureWords is why a fix job pushed nothing, as a clause. The worker's own words are kept only
// for what it decided — a broken check, a branch that moved — and what else went wrong is named by
// kind: a public repository's readers are not shown the worker's logs.
func fixFailureWords(res *JobResult, branch string) string {
	switch res.Error.Code {
	case "checks_broken":
		return truncate(oneLine(res.Error.Message), 300)
	case "branch_moved":
		return fmt.Sprintf("somebody pushed to `%s` while the job worked, and the change conflicts with what they pushed, so nothing was pushed", branch)
	case "empty_diff":
		return "the job found nothing to change — the finding may already be fixed at the head"
	case "push_failed":
		return fmt.Sprintf("GitHub refused the push to `%s`, which may be a protected branch", branch)
	case "base_branch_missing", "clone_failed":
		return fmt.Sprintf("the job could not check out `%s`", branch)
	case "dispatch_failed", "credential_unavailable", "sandbox_unavailable":
		return "the fix worker could not start; an admin of its attest_tag account can see why in the console"
	}
	return "the job stopped with an error; an admin of its attest_tag account can see it in the console"
}

// fixChecksLine is what the repository's own checks said after the change, as one line.
func fixChecksLine(res *JobResult) string {
	var parts []string
	for _, c := range []struct {
		name string
		run  JobTestRun
	}{{"build", res.Build.After}, {"tests", res.Tests.After}, {"lint", res.Lint.After}} {
		switch r := c.run; {
		case !r.Ran:
		case r.Killed != "":
			parts = append(parts, c.name+" did not finish")
		case r.OK && r.Passed > 0:
			parts = append(parts, fmt.Sprintf("%s passed (%d)", c.name, r.Passed))
		case r.OK:
			parts = append(parts, c.name+" passed")
		case r.Failed > 0:
			parts = append(parts, fmt.Sprintf("%s failed (%d)", c.name, r.Failed))
		default:
			parts = append(parts, c.name+" failed")
		}
	}
	if len(parts) == 0 {
		return "No build or test command was found to check the change; check it before merging."
	}
	return "Checks after the change: " + strings.Join(parts, " · ") + "."
}

// ---- helpers ----

// say answers the asker where they asked: in the finding's thread, or on the conversation. GitHub
// refusing — a locked conversation, a permission the installation never granted — is logged; the
// request has been dealt with either way.
func (a reviewFixAsk) say(ctx context.Context, gh *reviewGitHub, text string) {
	body := review.Sanitize(text, review.SanitizeOptions{AllowedRepos: []string{a.repo}, MaxLen: reviewCommentMaxLen - 1000})
	var err error
	if a.thread > 0 {
		_, err = gh.ReplyToReviewComment(ctx, a.thread, body)
	} else {
		_, err = gh.CreateIssueComment(ctx, body)
	}
	if err != nil {
		slog.Warn("code review: a fix answer was not posted", "repo", a.repo, "pr", a.n, "err", err)
	}
}

// reviewFixAudit records what came of a fix request (review.fix), by the GitHub login that asked.
func (b *Bot) reviewFixAudit(ctx context.Context, orgID int64, a reviewFixAsk, outcome string, extra map[string]any) {
	details := map[string]any{"by": "github:" + a.login, "outcome": outcome}
	if a.via != "" {
		details["via"] = a.via
	}
	if a.finding != nil {
		details["finding"] = a.finding.PublicID
	}
	if len(a.severities) > 0 {
		details["severities"] = a.severities
	}
	for k, v := range extra {
		details[k] = v
	}
	e := reviewAuditEvent(&ReviewPR{Repo: strings.ToLower(a.repo), Number: a.n}, "", details)
	e.OrgID, e.Via, e.Action, e.ActorName = orgID, viaSystem, "review.fix", "github:"+a.login
	b.record(ctx, e)
}

func findingsWords(n int) string {
	if n == 1 {
		return "1 finding"
	}
	return fmt.Sprintf("%d findings", n)
}

func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// findingStatusWords is a finding that no longer stands, as the rest of a sentence.
func findingStatusWords(s review.FindingStatus) string {
	switch s {
	case review.FindingFixed:
		return "already fixed"
	case review.FindingWithdrawn:
		return "withdrawn"
	case review.FindingResolved:
		return "resolved"
	case review.FindingOutdated:
		return "outdated: its code is gone"
	case review.FindingAcknowledged:
		return "acknowledged as a known risk"
	}
	return "no longer open"
}

// lineRef is a place in a file as people write it: path:12, or path:12-14.
func lineRef(path string, start, end int) string {
	switch {
	case end <= 0 && start <= 0:
		return path
	case start <= 0 || start == end:
		return fmt.Sprintf("%s:%d", path, max(start, end))
	case end <= 0:
		return fmt.Sprintf("%s:%d", path, start)
	}
	return fmt.Sprintf("%s:%d-%d", path, start, end)
}
