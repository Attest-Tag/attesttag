package app

import (
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

// Commands: a comment on a pull request that starts with the App's handle — `@attesttag review`,
// `@attesttag status` — is somebody asking the reviewer for something by name. The dispatcher hands
// every issue_comment delivery here; review.ParseCommand decides whether it is addressed to us at
// all (the first unquoted line, the mention ending at a space, so a development App's handle never
// answers for production), and everything after that is decided here, in this order:
//
//   - Who is asking. A bot is never answered — the likeliest bot is this App, and an echo of its own
//     comment read as a command is how a reviewer starts talking to itself. A person must be an
//     owner, member or collaborator of the repository (author_association, GitHub's own word on the
//     delivery): a review spends the organisation's money, and on a public repository anybody can
//     comment. A stranger's command is ignored on a private repository, where only invited people
//     can comment anyway, and answered with one polite line a day per pull request on a public one,
//     so it does not look broken and cannot be used to make the bot talk. Either is audited once a
//     day per pull request and only logged after that, so a script commenting on a public
//     repository cannot push the organisation's own events out of its audit log.
//   - How often. Ten commands an hour per person, across instances (countThrottle), on top of the
//     review lane's own caps per pull request and per repository.
//   - Whether the organisation has code review at all (CODE_REVIEW and its plan, review_plan.go).
//     Without it a command from its own people is answered with one line a day per pull request
//     saying so, in words that name no plan — a public repository's readers are not told what the
//     organisation buys — and nothing else is done for it.
//   - What it asks. review queues a review through the lane's one entry point (enqueueReview), past
//     the filters a review nobody asked for is held to — the "when" setting, drafts, the author
//     list — but not past fork policy, the throttles or the money. full review does the same from
//     scratch, for members of the organisation only and once per commit a day. pause and resume
//     stop and start the pull request's automatic reviews, for members of the organisation too.
//     status and help are answered at once from what is stored, with no model; a free-form question
//     is not answered yet, and is pointed at help at most once an hour.
//
// A command is picked up visibly: an eyes reaction on it, which GitHub may refuse (the permission is
// not every installation's) without anything else failing. Every answer is Go's own text, passed
// through review.Sanitize like the review's, and posted with the review's post token; every command
// is audited (review.command), with what came of it.
//
// On a pull request whose review is recorded in shadow a command is acted on and nothing is written
// to GitHub — no reaction, no answer, no refusal. Shadow is where every connection starts and the
// promise it makes (guide/security.md) is that it writes not a word to GitHub, which is what lets a
// team add a connection before anybody has decided the bot may speak there; somebody typing the
// bot's name on a pull request is not that decision. Where a review goes is decided per pull request
// (reviewDestination): a branch rule can post live on a repository in shadow, and record in shadow
// on one that is live, and a command is answered exactly where the review is posted. A review asked
// for in shadow is recorded in the console, and the audit log says what each command came to and
// that nothing was said on GitHub.

const (
	reviewCommandsPerHour = 10
	// How often each kind of short answer may be given on one pull request: a stranger's refusal a
	// day, the pointer to help an hour. Both are the same sentence every time, and a thread full of
	// it is noise a person could make the bot produce on purpose.
	reviewRefusalEvery  = 24 * time.Hour
	reviewQuestionEvery = time.Hour
	// A full review of one commit, at most once in this.
	reviewFullEvery = 24 * time.Hour
	// How often an organisation whose plan has no code review is told so on one pull request.
	reviewPlanNoteEvery = 24 * time.Hour
	// How long a command's answer may be, in characters. The help is the longest, at a few hundred.
	reviewAnswerMaxLen = 4000
)

// githubIssueCommentPayload is the part of an issue_comment delivery a command needs.
type githubIssueCommentPayload struct {
	Action string `json:"action"`
	Issue  struct {
		Number int `json:"number"`
		// PullRequest is set only when the issue is a pull request.
		PullRequest json.RawMessage `json:"pull_request"`
	} `json:"issue"`
	Comment    githubComment `json:"comment"`
	Repository struct {
		FullName string `json:"full_name"`
		Private  bool   `json:"private"`
	} `json:"repository"`
	Sender githubUser `json:"sender"`
}

// reviewMember reports whether GitHub's author_association makes somebody one of the repository's
// own people: an owner, a member of the organisation that owns it, or a collaborator invited to it.
func reviewMember(assoc string) bool {
	switch strings.ToUpper(assoc) {
	case "OWNER", "MEMBER", "COLLABORATOR":
		return true
	}
	return false
}

// reviewConfirmedAssociation is a comment author's association, raised when GitHub under-reports it.
// Webhook payloads — and the API, read with the App's own token — give "NONE" for a member of an
// organisation whose membership is private, since an App without the Members permission cannot see
// it: a member's "remember" in a thread was refused while their command on the same pull request,
// whose delivery happened to say MEMBER, was obeyed. On a private repository that is settled
// without asking: nobody can comment on one without access to it, which is what COLLABORATOR
// means here. On a public one the comment is read back, and kept as delivered unless GitHub then
// says member. It only ever raises, and a failed read keeps the delivery's, so a stranger is never
// made a member by an error. inline picks the pull request's review comments over its
// conversation's.
func reviewConfirmedAssociation(ctx context.Context, gh *reviewGitHub, private, inline bool, commentID int64, given string) string {
	if reviewMember(given) {
		return given
	}
	if private {
		return "COLLABORATOR"
	}
	if gh == nil || commentID <= 0 {
		return given
	}
	list := gh.IssueComments
	if inline {
		list = gh.ReviewComments
	}
	comments, _, err := list(ctx)
	if err != nil {
		return given
	}
	for _, c := range comments {
		if c.ID == commentID {
			if reviewMember(c.AuthorAssociation) {
				return c.AuthorAssociation
			}
			break
		}
	}
	return given
}

// reviewOrgMember is the narrower half: the owner, or a member of the organisation. A collaborator
// is invited to one repository, and a full review — paid for again on code already reviewed — is
// the organisation's call.
func reviewOrgMember(assoc string) bool {
	switch strings.ToUpper(assoc) {
	case "OWNER", "MEMBER":
		return true
	}
	return false
}

// reviewSlug is the App's handle, the name a command is addressed to; "" when no App is configured.
func (b *Bot) reviewSlug() string {
	if b.proxy == nil || b.proxy.ghApp == nil {
		return ""
	}
	return b.proxy.ghApp.slug
}

// reviewFromBot reports whether u is a bot — GitHub's word for it, a "[bot]" login, or this App —
// whose words the reviewer never answers. The receipt filter has already dropped comment events a
// bot sent; this is the same check on the comment's author, which a delivery says separately.
func (b *Bot) reviewFromBot(u githubUser) bool {
	return strings.EqualFold(u.Type, "Bot") || strings.HasSuffix(strings.ToLower(u.Login), "[bot]") || b.reviewByUs(u)
}

// reviewIssueCommentEvent routes an issue_comment delivery: a new comment on a pull request that is
// a command to this App is answered; everything else — an edit, a deletion, a comment on an issue,
// one not addressed to us — is finished with nothing done. An edit is not a new command: answering
// edits would let one comment be replayed as many commands as it was edited.
func (b *Bot) reviewIssueCommentEvent(ctx context.Context, d *githubDelivery, raw []byte) error {
	var p githubIssueCommentPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	pr := string(p.Issue.PullRequest)
	if p.Action != "created" || pr == "" || pr == "null" || p.Comment.ID <= 0 || p.Issue.Number <= 0 ||
		!validGitHubRepo(p.Repository.FullName) {
		return nil
	}
	if b.reviewFromBot(p.Sender) || b.reviewFromBot(p.Comment.User) {
		return nil
	}
	slug := b.reviewSlug()
	if slug == "" {
		slog.Warn("code review: a comment arrived but no GitHub App slug is configured to read commands against", "delivery", d.ID)
		return nil
	}
	cmd, ok := review.ParseCommand(p.Comment.Body, slug)
	if !ok {
		return nil
	}
	return b.reviewCommand(ctx, d, &p, cmd, slug)
}

// reviewCommandCall is one command being answered.
type reviewCommandCall struct {
	b     *Bot
	d     *githubDelivery
	p     *githubIssueCommentPayload
	cmd   review.Command
	slug  string
	repo  string
	n     int
	login string
	gh    *reviewGitHub
	// pull is the pull request as read for this command, once it has been read (pullRequest).
	pull *githubPull
	// quiet is a pull request whose review is recorded in shadow (reviewDestination): the command is
	// acted on, and nothing is written to GitHub.
	quiet bool
}

// audit records the command and what came of it. The actor is the GitHub user who wrote it, by
// login, through the system: nobody signed in to the console did anything. In shadow the outcome
// says that nothing was said on GitHub, since "answered" with a silent thread beside it would be the
// audit log contradicting what people can see.
func (c *reviewCommandCall) audit(ctx context.Context, outcome string, extra map[string]any) {
	if c.quiet {
		outcome += " (shadow: nothing posted on GitHub)"
	}
	details := map[string]any{"verb": string(c.cmd.Verb), "comment": c.p.Comment.ID, "by": "github:" + c.login,
		"association": c.p.Comment.AuthorAssociation, "outcome": outcome}
	if c.quiet {
		details["shadow"] = true
	}
	if len(c.cmd.Types) > 0 {
		details["types"] = c.cmd.Types
	}
	for k, v := range extra {
		details[k] = v
	}
	e := AuditEvent{OrgID: c.d.OrgID, Via: viaSystem, Action: "review.command", ActorName: "github:" + c.login,
		TargetKind: "pull_request", TargetID: fmt.Sprintf("%s#%d", strings.ToLower(c.repo), c.n),
		TargetName: fmt.Sprintf("%s#%d", c.repo, c.n), Details: auditDetails(details)}
	c.b.record(ctx, e)
}

// answer posts one of the bot's short answers on the pull request. The text is Go's, with type
// names and keys a team chose in it, and goes through the same sanitiser as everything the review
// posts. A refusal from GitHub — no permission to comment on this installation, the pull request
// locked — is logged and not retried: another attempt would be refused the same way, and the
// command has been dealt with. A rate limit or a failure to reach GitHub is returned, so the
// delivery is tried again.
func (c *reviewCommandCall) answer(ctx context.Context, text string) error {
	if c.quiet {
		slog.Info("code review: a command is not answered on a repository in shadow mode", "org", c.d.OrgID, "repo", c.repo,
			"pr", c.n, "comment", c.p.Comment.ID, "verb", c.cmd.Verb)
		return nil
	}
	body := review.Sanitize(text, review.SanitizeOptions{AllowedRepos: []string{c.repo}, MaxLen: reviewAnswerMaxLen})
	_, err := c.gh.CreateIssueComment(ctx, body)
	var api *githubAPIError
	if errors.As(err, &api) && api.Status < 500 {
		slog.Warn("code review: a command's answer was refused by GitHub", "org", c.d.OrgID, "repo", c.repo, "pr", c.n,
			"comment", c.p.Comment.ID, "err", err)
		return nil
	}
	return err
}

// reviewCommand answers one command. Errors are failures to find out or to reach GitHub, which the
// dispatcher tries again; everything the command itself came to is an answer, or deliberate silence.
func (b *Bot) reviewCommand(ctx context.Context, d *githubDelivery, p *githubIssueCommentPayload, cmd review.Command, slug string) error {
	c := &reviewCommandCall{b: b, d: d, p: p, cmd: cmd, slug: slug, repo: p.Repository.FullName, n: p.Issue.Number,
		login: p.Comment.User.Login}
	if c.login == "" {
		c.login = p.Sender.Login
	}
	// Not reviewed here — the installation is not in the review tree, the repository is off or is
	// another installation's — and so nothing here answers for it.
	eff, s, err := b.reviewEffective(ctx, d.OrgID, d.InstallationID, c.repo)
	if err != nil {
		return err
	}
	if s != nil {
		slog.Info("code review: a command on a repository not reviewed here", "org", d.OrgID, "repo", c.repo, "reason", s.Reason)
		return nil
	}
	if c.gh, err = b.reviewClient(d.OrgID, d.InstallationID, c.repo, c.n); err != nil {
		return err
	}
	c.gh.KnowIssueComment(p.Comment.ID)

	p.Comment.AuthorAssociation = reviewConfirmedAssociation(ctx, c.gh, p.Repository.Private, false, p.Comment.ID, p.Comment.AuthorAssociation)
	if !reviewMember(p.Comment.AuthorAssociation) {
		// Once a day per pull request, before anything is read or written for it: past that a
		// stranger's command is a log line, whatever it says and however many of them there are.
		kind, outcome := "review-refusal", "refused: not a member of the repository"
		if p.Repository.Private {
			kind, outcome = "review-ignored", "ignored: not a member of the repository"
		}
		if !b.store.AlertOnce(ctx, d.OrgID, fmt.Sprintf("%s:%s#%d", kind, strings.ToLower(c.repo), c.n), reviewRefusalEvery) {
			slog.Info("code review: a command from somebody who is not a member, again within the day; not audited",
				"org", d.OrgID, "repo", c.repo, "pr", c.n, "login", c.login, "comment", p.Comment.ID)
			return nil
		}
		if p.Repository.Private {
			c.audit(ctx, outcome, nil)
			return nil
		}
		if err := c.where(ctx, eff); err != nil {
			return err
		}
		c.audit(ctx, outcome, nil)
		return c.answer(ctx, "Thanks for asking. Reviews by attest_tag on this repository are started by its members and collaborators.")
	}
	if s := b.reviewPlanGate(ctx, d.OrgID); s != nil {
		// Once a day per pull request, and nothing else: no reaction, no review, no status. The words
		// are the gate's for any reason it cannot run, so the plan is never named on the pull request.
		if !b.store.AlertOnce(ctx, d.OrgID, fmt.Sprintf("review-plan:%s#%d", strings.ToLower(c.repo), c.n), reviewPlanNoteEvery) {
			slog.Info("code review: a command on an organisation without code review, again within the day; not answered",
				"org", d.OrgID, "repo", c.repo, "pr", c.n, "comment", p.Comment.ID)
			return nil
		}
		if err := c.where(ctx, eff); err != nil {
			return err
		}
		c.audit(ctx, "refused: "+s.Detail, nil)
		return c.answer(ctx, reviewSkipSentence(s.Reason))
	}
	if ok, _, err := b.store.countThrottle(ctx, fmt.Sprintf("review-command:%d:%s", d.OrgID, strings.ToLower(c.login)),
		reviewCommandsPerHour, time.Hour); err != nil {
		return err
	} else if !ok {
		c.audit(ctx, "throttled", nil)
		slog.Info("code review: commands throttled", "org", d.OrgID, "login", c.login, "repo", c.repo, "pr", c.n)
		return nil
	}
	if err := c.where(ctx, eff); err != nil {
		return err
	}
	// Picked up. A reaction GitHub refuses — the installation may not have granted it — costs the
	// person the signal and nothing else.
	if !c.quiet {
		if err := c.gh.ReactToIssueComment(ctx, p.Comment.ID, "eyes"); err != nil {
			slog.Debug("code review: no eyes reaction on a command", "repo", c.repo, "pr", c.n, "err", err)
		}
	}

	switch cmd.Verb {
	case review.VerbHelp:
		keys, err := b.reviewTypeKeys(ctx, d.OrgID)
		if err != nil {
			return err
		}
		c.audit(ctx, "answered", nil)
		return c.answer(ctx, reviewHelpText(slug, keys))
	case review.VerbStatus:
		text, err := b.reviewStatusText(ctx, d.OrgID, c.repo, c.n, slug)
		if err != nil {
			return err
		}
		c.audit(ctx, "answered", nil)
		return c.answer(ctx, text)
	case review.VerbQuestion:
		if !b.store.AlertOnce(ctx, d.OrgID, fmt.Sprintf("review-question:%s#%d", strings.ToLower(c.repo), c.n), reviewQuestionEvery) {
			c.audit(ctx, "ignored: a question was pointed at help within the hour", nil)
			return nil
		}
		c.audit(ctx, "answered", nil)
		return c.answer(ctx, fmt.Sprintf("Questions in your own words are not answered yet. "+
			"`@%s help` lists what attest_tag does on a pull request.", slug))
	case review.VerbReview, review.VerbFullReview:
		return c.review(ctx)
	case review.VerbPause, review.VerbResume:
		return c.pause(ctx, cmd.Verb == review.VerbPause)
	case review.VerbFix:
		return c.fix(ctx)
	}
	return nil
}

// pause answers pause and resume: the pull request's automatic reviews — on opening, on a push,
// found by the catch-up — stopped, or started again with a fresh count towards the next pause
// (reviewAutoPaused). A review somebody asks for runs either way. Members of the organisation only,
// as for a full review: on a public repository anybody can comment, and switching a team's reviews
// off is the team's call. The summary's footer follows, rendered again for nothing.
func (c *reviewCommandCall) pause(ctx context.Context, pause bool) error {
	b, d := c.b, c.d
	verb := "resume"
	if pause {
		verb = "pause"
	}
	if !reviewOrgMember(c.p.Comment.AuthorAssociation) {
		c.audit(ctx, "refused: pausing and resuming automatic reviews is for members of the organisation", nil)
		return c.answer(ctx, fmt.Sprintf("`@%s %s` is for members of the organisation that owns this repository.", c.slug, verb))
	}
	pull, err := c.pullRequest(ctx)
	if err != nil {
		return err
	}
	// Not its head: the stored head is the last one a push was handled for, and moving it from a
	// comment's delivery would hide a push this deployment missed from the catch-up.
	facts := reviewPRFacts(c.repo, pull)
	facts.HeadSHA = ""
	row, err := b.store.UpsertReviewPR(ctx, d.OrgID, facts)
	if err != nil {
		return err
	}
	resync := func() {
		if err := b.queueReviewResync(ctx, d.OrgID, d.InstallationID, row, "command", fmt.Sprintf("comment:%d", c.p.Comment.ID)); err != nil {
			slog.Warn("code review: the summary was not queued to follow a pause", "org", d.OrgID, "repo", c.repo, "pr", c.n, "err", err)
		}
	}
	if pause {
		paused, err := b.store.pauseReviewPR(ctx, d.OrgID, row.ID, false)
		if err != nil {
			return err
		}
		if !paused {
			c.audit(ctx, "already paused", nil)
			return c.answer(ctx, fmt.Sprintf("Automatic reviews of this pull request are already paused. `@%s resume` starts them again.", c.slug))
		}
		c.audit(ctx, "paused", nil)
		resync()
		return c.answer(ctx, fmt.Sprintf("Paused: this pull request is not reviewed again by itself until `@%s resume`. "+
			"`@%s review` still reviews it whenever somebody asks.", c.slug, c.slug))
	}
	if err := b.store.resumeReviewPR(ctx, d.OrgID, row.ID); err != nil {
		return err
	}
	c.audit(ctx, "resumed", map[string]any{"was_paused": row.Paused, "auto_reviews": row.AutoReviews})
	if !row.Paused {
		return c.answer(ctx, fmt.Sprintf("Automatic reviews of this pull request were not paused. They pause by themselves after %d; "+
			"the count starts again from now.", reviewAutoPauseAfter))
	}
	resync()
	return c.answer(ctx, fmt.Sprintf("Resumed: this pull request is reviewed by itself again, up to %d more times before they pause. "+
		"`@%s review` reviews the head now.", reviewAutoPauseAfter, c.slug))
}

// where decides whether anything is written to this pull request: whether its review is posted
// (reviewDestination). The pull request is read for it only when a branch rule could change the
// answer; otherwise the repository's mode is the answer.
func (c *reviewCommandCall) where(ctx context.Context, eff review.Effective) error {
	c.quiet = eff.Mode != review.ModeLive
	if !reviewDestinationByBranch(eff) {
		return nil
	}
	pull, err := c.pullRequest(ctx)
	if err != nil {
		return err
	}
	c.quiet = reviewDestination(eff, pull) != review.ModeLive
	return nil
}

// pullRequest reads the pull request the command is on, once.
func (c *reviewCommandCall) pullRequest(ctx context.Context) (*githubPull, error) {
	if c.pull == nil {
		pull, err := c.gh.Pull(ctx)
		if err != nil {
			return nil, err
		}
		if pull.Number == 0 {
			pull.Number = c.n
		}
		c.pull = pull
	}
	return c.pull, nil
}

// review answers review and full review: the types named, the once-a-day rule for a full review,
// then the lane's gate.
func (c *reviewCommandCall) review(ctx context.Context) error {
	b, d, full := c.b, c.d, c.cmd.Verb == review.VerbFullReview
	if full && !reviewOrgMember(c.p.Comment.AuthorAssociation) {
		c.audit(ctx, "refused: a full review is for members of the organisation", nil)
		return c.answer(ctx, fmt.Sprintf("`@%s full review` is for members of the organisation that owns this repository. "+
			"`@%s review` reviews the head.", c.slug, c.slug))
	}
	if len(c.cmd.Types) > 0 {
		keys, err := b.reviewTypeKeys(ctx, d.OrgID)
		if err != nil {
			return err
		}
		var unknown []string
		for _, k := range c.cmd.Types {
			if !slices.Contains(keys, k) {
				unknown = append(unknown, "`"+k+"`")
			}
		}
		if len(unknown) > 0 {
			c.audit(ctx, "refused: unknown review types", map[string]any{"unknown": unknown})
			what := "There is no review type " + strings.Join(unknown, " or ") + " here, or it is turned off."
			return c.answer(ctx, what+" The review types here: "+reviewKeyList(keys)+".")
		}
	}
	pull, err := c.pullRequest(ctx)
	if err != nil {
		return err
	}
	row, err := b.store.ReviewPRByNumber(ctx, d.OrgID, c.repo, c.n)
	if err != nil {
		return err
	}
	scope := ""
	switch {
	case full:
		scope = reviewScopeWhole
		if row != nil {
			done, err := b.store.reviewFullRunsSince(ctx, d.OrgID, row.ID, pull.Head.SHA, nowMinus(reviewFullEvery))
			if err != nil {
				return err
			}
			if done > 0 {
				c.audit(ctx, "refused: a full review of this commit ran in the last day", map[string]any{"head": pull.Head.SHA})
				return c.answer(ctx, fmt.Sprintf("`%s` has had a full review in the last day. A push gets a new review with `@%s review`.",
					shortSHA(pull.Head.SHA), c.slug))
			}
		}
	case row != nil && row.LastReviewedSHA != "" && row.LastReviewedSHA != pull.Head.SHA:
		// A review of a head that moved on since the last one: like a push's, it holds a new minor
		// finding to what changed since. On the head already reviewed it stays whole, which is what
		// lets it be answered from that review.
		scope = reviewScopeSinceLast
	}
	run, err := b.enqueueReview(ctx, d.OrgID, c.repo, c.n, reviewRequest{InstallationID: d.InstallationID, Trigger: "command",
		TriggerRef: fmt.Sprintf("comment:%d", c.p.Comment.ID), RequestedBy: "github:" + c.login, Types: c.cmd.Types,
		Scope: scope, BypassFilters: true, Full: full, Pull: pull})
	var skip *reviewSkip
	if errors.As(err, &skip) {
		c.audit(ctx, "not reviewed: "+skip.Reason, nil)
		return c.answer(ctx, reviewSkipSentence(skip.Reason))
	}
	if err != nil {
		return err
	}
	// The review is the answer, and the eyes say it is coming.
	c.audit(ctx, "queued", map[string]any{"run": run.PublicID, "head": pull.Head.SHA})
	return nil
}

// answerCachedCommand tells the person who asked for a review that it already exists: the same
// commits, types and settings were reviewed, so the lane answered from that review and spent
// nothing. Without this, a command on a reviewed head would be met with silence, which reads as
// broken. Only a posted review is pointed at — the cache key holds where a review went, so a
// repeat of a shadow review is itself in shadow, where nothing is written. Best effort: the run is
// already recorded as a noop.
func (b *Bot) answerCachedCommand(lane context.Context, r *ReviewRun, pr *ReviewPR, cached *ReviewRun) {
	if cached.Status != "posted" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(lane), reviewWriteTimeout)
	defer cancel()
	gh, err := b.reviewClient(r.OrgID, r.InstallationID, pr.Repo, pr.Number)
	if err == nil {
		text := fmt.Sprintf("Already reviewed `%s` with the same review types and settings, so there is nothing new to say: "+
			"the summary on this pull request is current. `@%s full review` looks again from scratch.",
			shortSHA(cached.HeadSHA), b.reviewSlug())
		opt := review.SanitizeOptions{AllowedRepos: []string{pr.Repo}, MaxLen: reviewAnswerMaxLen}
		_, err = gh.CreateIssueComment(ctx, review.Sanitize(text, opt))
	}
	if err != nil {
		slog.Warn("code review: the answer to a command on a reviewed head was not posted", "run", r.PublicID, "err", err)
	}
}

// reviewSkipSentence is the gate's reason (review_prs.skip_reason) as a sentence for the pull
// request: a command's answer, or the status. It says what kind of reason it was and no more: a
// public repository's readers are not told what the organisation spends, which model key it uses,
// or how its settings are laid out.
func reviewSkipSentence(reason string) string {
	switch reason {
	case "closed":
		return "This pull request is closed, so it was not reviewed."
	case "fork":
		return "Pull requests from forks are not reviewed on this repository."
	case "budget":
		return "attest_tag did not review this pull request: the organisation's code review budget is spent for now. " +
			"An admin of its attest_tag account can raise it."
	case "throttle":
		return "This pull request, or this repository, has had as many reviews as it gets in a day. Ask again tomorrow."
	case "types_off":
		return "Every review type this would run is turned off, so it was not reviewed."
	case "own_key_off", "own_key_blocked":
		return "Code review cannot run for this organisation right now; an admin of its attest_tag account can see why in the console."
	case "trigger":
		return "This repository is reviewed when somebody asks, not when a pull request opens."
	case "draft":
		return "Draft pull requests are not reviewed here until somebody asks."
	case "bot", "excluded_author":
		return "Pull requests by this author are not reviewed here until somebody asks."
	case "nothing_to_review":
		return "Every changed file is ignored, binary or generated, so there was nothing to review."
	case "no_rule":
		return "No branch rule covers this pull request's branches."
	case "paused":
		return "Automatic reviews of this pull request are paused; a member of the organisation can resume them, and a review anybody asks for still runs."
	case "plan":
		return "attest_tag code review is not turned on for this organisation's account, so nothing here is reviewed. " +
			"An admin of its attest_tag account can see why in the console."
	}
	return "This pull request was not reviewed (" + reason + ")."
}

// reviewTypeKeys are the review types a command may name in this organisation, in the order they
// are offered: the built-ins, less any it turned off, then its own.
func (b *Bot) reviewTypeKeys(ctx context.Context, orgID int64) ([]string, error) {
	rows, err := b.store.ReviewTypes(ctx, orgID)
	if err != nil {
		return nil, err
	}
	byKey := map[string]*ReviewType{}
	for _, t := range rows {
		byKey[t.Key] = t
	}
	var keys []string
	for _, t := range review.BuiltinTypes() {
		if row := byKey[t.Key]; row == nil || row.Enabled {
			keys = append(keys, t.Key)
		}
		delete(byKey, t.Key)
	}
	for _, t := range rows {
		if byKey[t.Key] != nil && t.Enabled {
			keys = append(keys, t.Key)
		}
	}
	return keys, nil
}

func reviewKeyList(keys []string) string {
	quoted := make([]string, len(keys))
	for i, k := range keys {
		quoted[i] = "`" + k + "`"
	}
	return strings.Join(quoted, ", ")
}

// reviewHelpText is the answer to help, and to a bare mention.
func reviewHelpText(slug string, keys []string) string {
	var b strings.Builder
	b.WriteString("**attest_tag code review** answers these, at the start of a comment:\n\n")
	fmt.Fprintf(&b, "- `@%s review` reviews the pull request's head. `@%s review security` runs the review types "+
		"you name instead of the ones its branch rule picks.\n", slug, slug)
	fmt.Fprintf(&b, "- `@%s full review` reviews it again from scratch, even a commit already reviewed: "+
		"for members of the organisation, once per commit a day.\n", slug)
	fmt.Fprintf(&b, "- `@%s status` says what the score is and why, from what is stored.\n", slug)
	fmt.Fprintf(&b, "- `@%s pause` stops the reviews nobody asks for on this pull request, on opening and on a push, "+
		"and `@%s resume` starts them again: for members of the organisation. They pause by themselves after %d, "+
		"and a review somebody asks for still runs.\n", slug, slug, reviewAutoPauseAfter)
	fmt.Fprintf(&b, "- `@%s fix` has attest_tag fix the open findings and push the commit to this pull request's branch; "+
		"`@%s fix p0 p1` fixes those severities only. For people who can push to the repository, when its review settings allow fixes.\n", slug, slug)
	fmt.Fprintf(&b, "- `@%s help` is this.\n\n", slug)
	b.WriteString("Reply in a finding's thread to dispute it or ask about it, and you get a verdict; " +
		"say it is fixed, and the summary records your claim.")
	fmt.Fprintf(&b, " Reply `@%s fix` there, or tick the fix box on the finding, and it is fixed on the pull request.\n\n", slug)
	b.WriteString("Review types here: " + reviewKeyList(keys) + ".")
	return b.String()
}

// reviewStatusText is the answer to status: the pull request's review as the database has it, with
// no model and no read of GitHub — the score and what it is made of, which commit was reviewed and
// whether the head has moved since, what is on its way, and why the last request was not reviewed.
//
// A review recorded in shadow is said to exist and nothing more. Recording one on a pull request
// that is otherwise posted — Start review with "record only", a branch rule that keeps one branch
// in shadow — is somebody's decision not to say it here, and its score and its P0 count, on a public
// repository, would tell anybody who asks that a problem is open and unfixed.
func (b *Bot) reviewStatusText(ctx context.Context, orgID int64, repo string, n int, slug string) (string, error) {
	pr, err := b.store.ReviewPRByNumber(ctx, orgID, repo, n)
	if err != nil {
		return "", err
	}
	var lines []string
	add := func(format string, a ...any) { lines = append(lines, fmt.Sprintf(format, a...)) }
	lines = append(lines, "**attest_tag status**")
	if pr == nil {
		add("Not reviewed yet. `@%s review` asks for a review.", slug)
		return strings.Join(lines, "\n\n"), nil
	}
	last, st, score, err := b.reviewStanding(ctx, orgID, pr)
	if err != nil {
		return "", err
	}
	switch {
	case last == nil:
		add("Not reviewed yet. `@%s review` asks for a review.", slug)
	case last.Status == "shadow":
		add("A review was recorded in the attest_tag console; it is not posted here.")
	default:
		add("Confidence **%d/5** (advisory).", score)
		bySev := map[review.Severity]int{}
		var claimed, disputed, withdrawn, fixed int
		for _, f := range st.Findings {
			switch {
			case f.Note || f.PreExisting:
			case f.Status == review.FindingOpen || f.Status == review.FindingDisputed:
				bySev[f.Severity]++
				if f.ClaimedFixedSHA != "" {
					claimed++
				}
				if f.Status == review.FindingDisputed {
					disputed++
				}
			case f.Status == review.FindingWithdrawn:
				withdrawn++
			case f.Status == review.FindingFixed:
				fixed++
			}
		}
		var parts []string
		for _, sev := range []review.Severity{review.P0, review.P1, review.P2} {
			if bySev[sev] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", bySev[sev], sev))
			}
		}
		open := "none"
		if len(parts) > 0 {
			open = strings.Join(parts, ", ")
		}
		var extra []string
		for _, x := range []struct {
			n    int
			what string
		}{{claimed, "claimed fixed"}, {disputed, "disputed"}, {withdrawn, "withdrawn"}, {fixed, "fixed"}} {
			if x.n > 0 {
				extra = append(extra, fmt.Sprintf("%d %s", x.n, x.what))
			}
		}
		if len(extra) > 0 {
			open += " (" + strings.Join(extra, ", ") + ")"
		}
		add("Open findings: %s.", open)
		var why []string
		if !st.FullCoverage {
			why = append(why, "not every changed line was reviewed")
		}
		if st.InjectionDetected {
			why = append(why, "text in the diff spoke to the reviewer")
		}
		if len(why) > 0 && score == 4 {
			add("The score is at most 4 because %s.", strings.Join(why, ", and "))
		}
		head := "the head"
		if pr.HeadSHA != "" && pr.HeadSHA != last.HeadSHA {
			head = fmt.Sprintf("the head is now `%s`, not reviewed — `@%s review` reviews it", shortSHA(pr.HeadSHA), slug)
		}
		add("Last reviewed `%s`; %s.", shortSHA(last.HeadSHA), head)
		// The rule the pull request fell under, and the types when a person named them instead of its
		// own; a run from before rules were always recorded has types and no rule.
		if last.RuleLabel != "" {
			add("Branch rule: %s.", last.RuleLabel)
		}
		if len(reviewOptionsOf(last).Types) > 0 || (last.RuleLabel == "" && len(last.Types) > 0) {
			keys := make([]string, 0, len(last.Types))
			for _, t := range last.Types {
				keys = append(keys, t.Key)
			}
			add("Review types: %s, as asked.", reviewKeyList(keys))
		}
	}
	active, err := b.store.reviewActiveRun(ctx, orgID, pr.ID)
	if err != nil {
		return "", err
	}
	if active != nil {
		add("A review is %s.", active.Status)
	}
	if pr.Paused {
		why := "a member paused them"
		if pr.PausedAuto {
			why = fmt.Sprintf("they pause by themselves after %d", reviewAutoPauseAfter)
		}
		add("Automatic reviews are paused: %s. `@%s resume` starts them again; `@%s review` reviews the head now.", why, slug, slug)
	}
	if pr.SkipReason != "" && !(pr.SkipReason == "paused" && pr.Paused) {
		add("Why the last request was not reviewed: %s", reviewSkipSentence(pr.SkipReason))
	}
	return strings.Join(lines, "\n\n"), nil
}

// reviewStanding is a pull request's review as the database has it now: its last finished review,
// the summary state rendered from the findings said where that review's summary is, and the score
// computed from them in Go — what the summary would show if it were rendered this moment. last is
// nil before any review finished.
func (b *Bot) reviewStanding(ctx context.Context, orgID int64, pr *ReviewPR) (last *ReviewRun, st review.SummaryState, score int, err error) {
	last, err = b.store.latestReviewOutcome(ctx, orgID, pr.ID)
	if err != nil || last == nil {
		return nil, st, -1, err
	}
	ck, ok := checkpointFrom(last)
	if !ok {
		return nil, st, -1, nil
	}
	st, score, err = b.reviewSummaryState(ctx, last, pr, ck, pr.HeadSHA, pr.ReviewsCount, last.Status == "shadow")
	return last, st, score, err
}
