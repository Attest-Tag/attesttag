package app

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"attesttag/internal/review"
)

// The review lane: what turns "a pull request was opened on a reviewed repository" into a review
// on GitHub, or a review recorded in the console in shadow mode.
//
// It has three parts, in the order a review goes through them:
//
//   - The gate (enqueueReview). Every request — a webhook, a command on the pull request, and the
//     console's Start review when it calls it too — comes through one entry point, which decides
//     from the settings tree, the branch rules and the pull request itself whether a review runs at
//     all, records why on the pull request when it does not (review_prs.skip_reason), checks the
//     throttles and the money, and queues the run.
//   - The lane (runReviewLane). Two workers per instance, outside the interactive in-flight cap: a
//     review is minutes of model calls nobody is waiting on in a chat, and must never take a turn's
//     place. Each claims a run under a fenced lease that also holds its pull request
//     (store_review_runs.go), renews it while it works, and checks the gate, the money and the
//     throttles again — settings change and money is spent while a run waits, and only a run that
//     goes on to the model counts as a review of the day. A run whose lease is taken over stops,
//     and writes nothing more: the fence on every write is what makes that true even for a lane
//     that stalled without noticing.
//   - The engine (review_engine.go) and then the poster (review_post.go). The engine's result is
//     checkpointed before anything is posted, so a run requeued after its model work — GitHub's
//     rate limit, a lane that died — posts from what was stored and never pays twice.
//
// Money is held before a run starts, not counted after: each run reserves its max_usd against the
// account's credit, the account's monthly budget, code review's own monthly budget and its daily
// cap, less what the organisation's other running runs are holding — budgetRoom's shape for fix
// jobs (jobs.go), because budgetOK's "is there anything left" lets forty reviews start on the last
// dollar. A review the money stops is recorded as skipped for "budget", the admins' alert channel
// hears about it once an hour, and a live pull request gets one short note a day saying so.

const (
	reviewLaneWorkers  = 2
	reviewLaneIdlePoll = 2 * time.Second
	// A push review waits this long for the next push. A branch being worked on is pushed in
	// bursts — a fix, its test, the lint the test found — and reviewing each commit of a burst is
	// paying three times for the review of the last one.
	reviewPushDebounce = 90 * time.Second
	// Reviews per pull request and per repository in a day, counted as runs that went on to the
	// model (reviewRunsStarted). Commands and console starts count with automatic ones: each is a
	// review somebody pays for, and the reason for a cap is a loop — a bot that keeps pushing, a
	// team member who keeps asking — that nobody is watching. A push whose run stood aside for a
	// newer one spent nothing and is not counted, or a busy branch would use up its day on runs
	// that never ran and its last head would go unreviewed.
	reviewRunsPerPRDay   = 8
	reviewRunsPerRepoDay = 40
	// Automatic reviews of one pull request — on opening, on a push, found by the catch-up — before
	// they pause by themselves. A branch that is pushed to all day on a repository reviewed on every
	// push is paying for a review of each push whether anybody reads them or not; past five, the
	// next push is more likely the sixth of a long fix than a change somebody wants judged, and
	// `@… resume` (or any review somebody asks for, which still runs) costs a person one comment.
	// The daily caps above stay the backstop for everything else.
	reviewAutoPauseAfter = 5
	// reviewEngineVersion is in every cache key. Bump it when the engine's judgement changes, so a
	// pull request reviewed by the old one is not answered from its result.
	reviewEngineVersion = "2"
	// The sticky summary's marker id. One summary per pull request, and the marker's MAC already
	// binds the organisation, the repository and the number, so the id itself needs to say nothing.
	reviewSummaryID = "summary"
	// A label's review waits this long before it is claimed, as a push's does: labels go on in
	// bursts, and one set as the pull request was opened is delivered beside the opening, whose own
	// review already runs the label's types — waiting lets that one be claimed first, and the
	// label's then finds its types covered and stands aside for nothing (processReview).
	reviewLabelWait = reviewPushDebounce
	// How long a write after the work may take, on a context the work's cancellation cannot reach.
	reviewWriteTimeout = 10 * time.Second
)

// reviewRunTouchEvery is how often a lane renews its lease: a third of it, as the GitHub inbox
// does. A variable only so a test can watch a lease lapse without waiting; nothing else assigns it.
var reviewRunTouchEvery = reviewRunLease / 3

// The scopes a review can be asked for. Whole reads the pull request as a first review would;
// since_last also reads all of it, but holds a new minor finding to the files that changed since
// the last review (review_checks.go), which is what a review of a push is for.
const (
	reviewScopeWhole     = "whole"
	reviewScopeSinceLast = "since_last"
)

// reviewRequest is one request for a review, whoever made it. A command or the console's Start
// review fills in what a webhook cannot: who asked, which types, where to post.
type reviewRequest struct {
	// InstallationID is the GitHub App installation the repository is reached through: the
	// delivery's, or the console's connection's.
	InstallationID int64
	Trigger        string // open | push | command | catchup | console | api | label
	// TriggerRef identifies what caused it — a delivery, a comment, a console request — and is
	// what tells two requests from a person apart, so the same comment redelivered queues one run.
	TriggerRef  string
	RequestedBy string
	// Types are review type keys a person named. They replace the matching branch rule's types for
	// this run; empty runs the rule's.
	Types []string
	// Post overrides the repository's mode: shadow records a review on a live repository without
	// posting it, and live posts on a shadow one — only with AllowLive, which the caller sets
	// for somebody who may turn posting on (an admin), since posting is what live is.
	Post      review.Mode
	AllowLive bool
	// Scope is reviewScopeWhole or reviewScopeSinceLast; empty is the trigger's own default.
	Scope string
	// BypassFilters is a person asking: the "when" setting, drafts and the author filters are for
	// reviews nobody asked for. Fork policy, the throttles and the money still apply.
	BypassFilters bool
	// Full is a review from scratch (`@… full review`): it is not answered from an earlier review of
	// the same code, and the finder is not told what is already open, so it looks again rather than
	// around what was found. What Go does with the result is unchanged — a finding already open is
	// not posted twice, and one a person argued away is never raised again: a full review is a
	// second look, not a way to re-open an argument the thread settled.
	Full bool
	// Pull is what the trigger already says about the pull request; nil reads it from GitHub.
	Pull *githubPull
	// NotBefore delays the run, for the push debounce.
	NotBefore time.Time
	// Inline is a review type as somebody is editing it in the console, not as it is saved: Reviews ›
	// Types › Try on a PR. The run is a try (kind "try"), not a review of the pull request — it runs
	// that one type, is recorded in shadow whatever the repository says, and leaves the pull
	// request's own review untouched (see reviewTry).
	Inline *ReviewType
	// Label is the label a review of the types it added was queued for (reviewLabeled): Types are
	// those, and the rule the run records names the label beside it. BotLabel says a bot put it on —
	// a labeler workflow, not a person — which makes the review one the pause counts and holds
	// (reviewCountsTowardsPause).
	Label    string
	BotLabel bool
}

// reviewOptions is what a queued run remembers of its request (review_runs.request_json).
type reviewOptions struct {
	Post      review.Mode `json:"post,omitempty"`
	AllowLive bool        `json:"allow_live,omitempty"`
	Scope     string      `json:"scope,omitempty"`
	Types     []string    `json:"types,omitempty"` // named by a person
	Full      bool        `json:"full,omitempty"`
	// Inline is the type a try runs, carried with the run so a try resumed after a crash runs the
	// text that was tried rather than whatever has been saved since.
	Inline *ReviewType `json:"inline,omitempty"`
	// Label is reviewRequest.Label: the types named are a label's. BotLabel is reviewRequest.BotLabel.
	Label    string `json:"label,omitempty"`
	BotLabel bool   `json:"bot_label,omitempty"`
	// Title, Base and Head are the pull request as the request found it: what its chat message is
	// drawn with, and its branch rule matched on, by whoever ends the run with no read of GitHub in
	// hand — the queue's cancel, the sweep (noticeStartOrphaned). Each claim reads them afresh.
	Title string `json:"title,omitempty"`
	Base  string `json:"base,omitempty"`
	Head  string `json:"head,omitempty"`
}

// A try (reviewRequest.Inline) is a run of a review type that has not been saved, on a pull request
// somebody picked, to see what it would find before it can find anything for real. Everything that
// makes a review safe to run still applies — the gate, fork policy, the money and the throttles —
// and three things make it unlike a review:
//
//   - it is recorded in shadow, always: nothing it finds is posted, whatever the repository's mode,
//     and nobody may ask otherwise;
//   - it starts from nothing: it is not told what earlier reviews found, nothing it finds is dropped
//     as a duplicate of a real finding — which would also mark that finding as seen by it — and it is
//     never answered from an earlier review's result;
//   - it is not the pull request's review: its findings are not ones anybody was shown, so a later
//     review neither repeats them nor holds back for them, and neither the summary, the score, the
//     status command nor the head last reviewed moves because of it.
//
// It is kept apart by its kind, "try", which every read of a pull request's review ignores
// (kind='review'), and by the checks on reviewTry below. Its findings are its run's, for the console
// to show.
func reviewTry(r *ReviewRun) bool { return r.Kind == "try" }

// reviewInlineSpec is a tried type as the engine runs it. Its version is 0, which no saved version
// is: a run that records {key, 0} ran text that was never saved.
func reviewInlineSpec(t *ReviewType) reviewTypeSpec {
	s := reviewTypeOfRow(t)
	s.Version = 0
	return s
}

func reviewOptionsOf(r *ReviewRun) reviewOptions {
	var o reviewOptions
	json.Unmarshal([]byte(cmp.Or(r.RequestJSON, "{}")), &o) // written only by enqueueReview
	return o
}

// reviewSkip is the gate saying no: the reason is a short code, stored on the pull request for the
// status command and the console to explain, and Detail the sentence for a log or a refusal.
type reviewSkip struct {
	Reason string
	Detail string
}

func (e *reviewSkip) Error() string { return "review skipped (" + e.Reason + "): " + e.Detail }

// errReviewLiveRefused is a request to post live on a repository that only records, from somebody
// the caller did not vouch for.
var errReviewLiveRefused = errors.New("posting a review live on a repository in shadow mode needs an admin: only an admin may turn posting on")

// automaticTrigger reports whether nobody asked for a review with this trigger: the pull request
// opened, or was pushed to, or the catch-up found it, or a label was put on it that adds a review
// type (reviewLabeled). Each is held to the filters a review nobody asked for is — the "when"
// setting, drafts, the authors to skip, forks.
func automaticTrigger(t string) bool {
	return t == "open" || t == "push" || t == "catchup" || t == "label"
}

// reviewPauseCounts reports whether a review with this trigger counts towards the pause after
// reviewAutoPauseAfter, and is held back by one: the reviews that come of the pull request simply
// moving. A label a person put on is a deliberate act on one pull request and is not counted, though
// a pause holds it back all the same (reviewLabeled) — the pause says the pull request is not
// reviewed again by itself, and a label is not somebody asking on it.
func reviewPauseCounts(t string) bool { return t == "open" || t == "push" || t == "catchup" }

// reviewCountsTowardsPause is reviewPauseCounts for one request: its trigger's, or a label a bot put
// on, which no person chose — a labeler workflow that puts labels back as a branch changes would
// otherwise buy a review of their types past the pause on every push.
func reviewCountsTowardsPause(trigger string, botLabel bool) bool {
	return reviewPauseCounts(trigger) || (trigger == "label" && botLabel)
}

// ---- the gate ----

// reviewEffective is the settings a repository will be reviewed under, through one installation, or
// the gate's reason why it will not be: the installation is not in the review tree (or no longer
// the organisation's), the repository was removed from code review, the organisation's own App
// connection for the repository is under another installation, or the repository is off.
func (b *Bot) reviewEffective(ctx context.Context, orgID, installationID int64, repo string) (review.Effective, *reviewSkip, error) {
	var none review.Effective
	if ok, err := b.store.ReviewedInstallation(ctx, orgID, installationID); err != nil {
		return none, nil, err
	} else if !ok {
		return none, &reviewSkip{"not_reviewed", "this installation is not in the organisation's review settings"}, nil
	}
	chain, err := b.store.ReviewSettingsChain(ctx, orgID, installationID, repo)
	if err != nil {
		return none, nil, err
	}
	if chain == nil {
		return none, &reviewSkip{"not_reviewed", "this installation is not in the organisation's review settings"}, nil
	}
	if reviewRepoRemoved(chain) {
		return none, &reviewSkip{"removed", repo + " was removed from code review"}, nil
	}
	levels, err := reviewLevels(chain)
	if err != nil {
		return none, nil, err
	}
	// A repository has one owner and so one installation of the App. The organisation's own App
	// connection naming it under another one is a repository that moved accounts: whichever of the
	// two is stale, reviewing it under this one's settings is a guess, and a guess here posts.
	conns, err := b.store.AllConnections(ctx, orgID)
	if err != nil {
		return none, nil, err
	}
	for _, c := range conns {
		if c.CredType == "github_app" && c.GitHubInstallationID > 0 && c.GitHubInstallationID != installationID &&
			strings.EqualFold(c.Repo, repo) {
			return none, &reviewSkip{"installation_mismatch", fmt.Sprintf("%s is connected through another installation of the App", repo)}, nil
		}
	}
	eff := review.Resolve(levels)
	if eff.Mode == review.ModeOff || eff.Mode == "" {
		return none, &reviewSkip{"off", "review is off for this repository"}, nil
	}
	return eff, nil, nil
}

// reviewPlan is what a review of one pull request will be: the settings with the branch rule's
// overrides, the rule's label, the type keys and where the result goes.
type reviewPlan struct {
	eff   review.Effective
	label string
	keys  []string
	named bool
	post  review.Mode
	// labels are the pull request's labels whose label rules added types (review.LabelTypes), which
	// the rule's label names after it ("any → main +label:perf") and the cache key holds: the same
	// commits with another label's types are another review.
	labels []string
	// The branch rule's own types and label, before any label added to them, for
	// withoutLabelTypes to start again from.
	ruleKeys  []string
	ruleLabel string
	// What GitHub said of the pull request when the plan was made, for the channel's announcement
	// of the review (review_notify.go): its title and its branches.
	title, base, head string
}

// planReview applies the branch rule the pull request falls under, or the types a person named
// instead, and decides where the result goes. It is the same decision at enqueue and at claim, so
// a rule edited while a run waits is the rule the run is done under.
//
// The rule is matched on the pull request's own branches, which every caller has read — from the
// delivery, the listing or GitHub (enqueueReview) — and a pull request with no base branch is an
// error, not the fallback: an empty base matches only the rule that matches everything, so a review
// asked for on a pull request into main would run as "any → any", with the fallback's types,
// and nothing would say why. Label rules then add their types for the pull request's labels, after
// the rule's own, unless a person named the types: naming them replaces the rule's set, and a
// label's with it.
func planReview(eff review.Effective, pull *githubPull, named []string, post review.Mode, allowLive bool) (reviewPlan, *reviewSkip, error) {
	if pull.Base.Ref == "" {
		return reviewPlan{}, nil, fmt.Errorf("GitHub gave no base branch for pull request #%d, so no branch rule can be matched", pull.Number)
	}
	_, rule, ok := review.MatchRule(eff.BranchRules, pull.Base.Ref, pull.Head.Ref)
	if !ok {
		return reviewPlan{}, &reviewSkip{"no_rule", "no branch rule matches " + pull.Head.Ref + " → " + pull.Base.Ref}, nil
	}
	p := reviewPlan{eff: eff.WithRule(rule), label: rule.String(), keys: rule.Types, title: pull.Title, base: pull.Base.Ref,
		head: pull.Head.Ref, ruleKeys: rule.Types, ruleLabel: rule.String()}
	if len(named) > 0 {
		// The types a person named replace the rule's, and nothing else of it: its strictness, model,
		// money and destination are still the ones the review runs under, so it is still the rule the
		// run records and the summary names.
		p.keys, p.named = named, true
	} else {
		add, labels := review.LabelTypes(eff.BranchRules, pull.Base.Ref, pull.Head.Ref, pull.LabelNames(), p.keys)
		p.keys = append(slices.Clone(p.keys), add...)
		for _, l := range labels {
			p.withLabel(l)
		}
	}
	p.post = p.eff.Mode
	switch post {
	case "":
	case review.ModeShadow:
		p.post = post
	case review.ModeLive:
		if p.eff.Mode != review.ModeLive && !allowLive {
			return reviewPlan{}, nil, errReviewLiveRefused
		}
		p.post = post
	default:
		return reviewPlan{}, nil, fmt.Errorf("a review is posted live or recorded in shadow, not %q", post)
	}
	return p, nil, nil
}

// withLabel records that label's rule added types to the plan: the rule's label says so after it,
// as "+label:<name>", and the cache key counts it. Labels are one line of at most fifty characters
// at GitHub; this one is held to a line here as well, since it is written into the summary's footer.
func (p *reviewPlan) withLabel(label string) {
	label = oneLine(label)
	if label == "" || slices.ContainsFunc(p.labels, func(l string) bool { return strings.EqualFold(l, label) }) {
		return
	}
	p.labels = append(p.labels, label)
	p.label += " +label:" + label
}

// withoutLabelTypes takes out of a plan its labels made the types in done, and the labels that then
// add nothing: the label rules are applied again to the pull request's labels as if the branch rule
// had chosen those types as well, so the rule's label and the cache key name only the labels whose
// types this review still runs. A plan whose types a person named has no label's in it.
func (p *reviewPlan) withoutLabelTypes(rules []review.BranchRule, pull *githubPull, done []string) {
	if p.named || len(p.labels) == 0 || len(done) == 0 {
		return
	}
	add, labels := review.LabelTypes(rules, pull.Base.Ref, pull.Head.Ref, pull.LabelNames(), slices.Concat(p.ruleKeys, done))
	p.keys, p.label, p.labels = slices.Concat(p.ruleKeys, add), p.ruleLabel, nil
	for _, l := range labels {
		p.withLabel(l)
	}
}

// reviewDestination is where a review of pull is published when nobody asks for anywhere else: the
// repository's mode with the branch rule its branches fall under applied, as planReview decides it,
// since a rule may post live on a repository in shadow or record in shadow on one that is live.
// Everything else the bot says on a pull request follows it — a command's answer and its reaction, a
// reply in a finding's thread, a summary rendered again — so a pull request whose review is recorded
// in shadow hears nothing from the bot at all, and one whose review is posted is answered. A pull
// request no rule matches is not reviewed, and takes the repository's own mode.
func reviewDestination(eff review.Effective, pull *githubPull) review.Mode {
	if _, rule, ok := review.MatchRule(eff.BranchRules, pull.Base.Ref, pull.Head.Ref); ok {
		return eff.WithRule(rule).Mode
	}
	return eff.Mode
}

// reviewDestinationByBranch reports whether a pull request's branches decide reviewDestination: some
// branch rule sends a review somewhere other than the repository's own mode. When none does, the
// mode is the answer, and the pull request need not be read to find it.
func reviewDestinationByBranch(eff review.Effective) bool {
	return slices.ContainsFunc(eff.BranchRules, func(r review.BranchRule) bool { return eff.WithRule(r).Mode != eff.Mode })
}

// reviewClient is the GitHub client for one pull request of one installation's repository.
func (b *Bot) reviewClient(orgID, installationID int64, repo string, pr int) (*reviewGitHub, error) {
	conn, err := b.proxy.reviewConnection(installationID, repo)
	if err != nil {
		return nil, err
	}
	gh, err := newReviewGitHub(b.proxy, orgID, conn, repo, pr)
	if err != nil {
		return nil, err
	}
	gh.base = b.review.base
	return gh, nil
}

// reviewPRFacts is what a read of the pull request says about it, for review_prs.
func reviewPRFacts(repo string, p *githubPull) ReviewPRFacts {
	fork, private := p.IsFork(), p.Base.Repo != nil && p.Base.Repo.Private
	state := "open"
	switch {
	case p.Merged:
		state = "merged"
	case p.State == "closed":
		state = "closed"
	}
	head := p.Head.SHA
	if !commitSHA.MatchString(head) {
		head = ""
	}
	return ReviewPRFacts{Repo: repo, Number: p.Number, State: state, IsFork: &fork, IsPrivate: &private,
		AuthorLogin: p.User.Login, HeadSHA: head}
}

// enqueueReview is the one way into the review lane: a pull request opened or pushed to, a command
// on it (review_commands.go), the console's Start review (review_api.go), or the catch-up finding one
// whose delivery this deployment never got (review_catchup.go). It applies the gate, in order,
// recording on the pull request why it stopped wherever it does:
//
//  1. the installation is in the organisation's review tree, and no App connection of the
//     organisation names the repository under another installation;
//  2. the repository's effective mode is not off;
//  3. the organisation has code review here: CODE_REVIEW and its plan (review_plan.go);
//  4. a branch rule matches (the list always ends in one that does), or a person named types;
//  5. for a review nobody asked for: the "when" setting allows this trigger, the pull request is
//     not a draft unless drafts are on, its author is not a bot and not on the exclude list;
//  6. a fork's pull request is reviewed only when a person asked, and not even then with forks off;
//  7. for an opening, a push, the catch-up or a label a bot put on: the pull request's automatic
//     reviews are not paused, by somebody or by having had reviewAutoPauseAfter of them
//     (reviewAutoPaused) — asked once the request is known not to be one already queued or done;
//  8. on the organisation's own model key, the key's reviews switch is on;
//  9. at least one of the review types it would get is on;
//  10. the money: max_usd reserved against credit, the month, review's month and review's day;
//  11. fewer than reviewRunsPerPRDay reviews in the last day on the pull request, and fewer than
//     reviewRunsPerRepoDay on the repository.
//
// The claim checks 3 and 7 to 11 again, and only a run that passes them there is a review of the
// day. Whether any changed file is left to review once the ignored ones are set aside needs the file
// list, which is the engine's to read: a run with none ends skipped. A request with the dedupe key
// of one already queued or done returns that run, and nothing is checked or reserved for it —
// unless that run answered nothing (cancelled, skipped, failed, superseded, or a label's that stood
// aside), when the request is a new one.
//
// A *reviewSkip is the gate's no; any other error is a failure to find out, which the dispatcher
// tries again — errReviewSpendUnknown among them: a review is not refused for money nobody could
// count.
func (b *Bot) enqueueReview(ctx context.Context, orgID int64, repo string, pr int, req reviewRequest) (*ReviewRun, error) {
	switch {
	case b.review == nil:
		return nil, errors.New("code review is not running in this process")
	case req.InstallationID <= 0 || !validGitHubRepo(repo) || pr <= 0:
		return nil, fmt.Errorf("a review needs an installation, an owner/name repository and a pull request, not %d, %q and %d", req.InstallationID, repo, pr)
	case !slices.Contains(reviewRunTriggers, req.Trigger):
		return nil, fmt.Errorf("%w: trigger %q", ErrReviewRunInvalid, req.Trigger)
	case req.Scope != "" && req.Scope != reviewScopeWhole && req.Scope != reviewScopeSinceLast:
		return nil, fmt.Errorf("a review's scope is %s or %s, not %q", reviewScopeWhole, reviewScopeSinceLast, req.Scope)
	}
	kind := "review"
	if req.Inline != nil {
		// Recorded, never posted, whoever asked: see reviewTry.
		kind, req.Post, req.AllowLive, req.Types = "try", review.ModeShadow, false, []string{req.Inline.Key}
	}
	pull := req.Pull
	if pull == nil || pull.Base.Ref == "" {
		// Read when the trigger did not say, or said without the branches the rule is matched on.
		gh, err := b.reviewClient(orgID, req.InstallationID, repo, pr)
		if err != nil {
			return nil, err
		}
		if pull, err = gh.Pull(ctx); err != nil {
			return nil, err
		}
	}
	if pull.Number == 0 {
		pull.Number = pr
	}
	facts := reviewPRFacts(repo, pull)
	head := facts.HeadSHA
	ownHead := req.Inline != nil || req.Trigger == "label"
	if ownHead {
		// A try is not the pull request's review, and the head it read is not one a push was handled
		// for: moving the stored head here would hide a push this deployment missed from the catch-up
		// (reviewCatchupAction), and with it the summary saying that head was not reviewed. A label's
		// delivery is the same, like a comment's: it describes the head, and is not a push.
		facts.HeadSHA = ""
	}
	row, err := b.store.UpsertReviewPR(ctx, orgID, facts)
	if err != nil {
		return nil, err
	}
	if !ownHead {
		head = row.HeadSHA
	}
	skip := func(s *reviewSkip) (*ReviewRun, error) {
		// A try refused is said to whoever tried, in the console; why the pull request's own review
		// did not run is not something it may overwrite.
		if req.Inline == nil {
			if err := b.store.SetReviewPRSkipReason(ctx, orgID, row.ID, s.Reason); err != nil {
				return nil, err
			}
		}
		slog.Info("code review skipped", "org", orgID, "repo", repo, "pr", pr, "trigger", req.Trigger, "reason", s.Reason, "why", s.Detail)
		return nil, s
	}
	if row.State != "open" {
		return skip(&reviewSkip{"closed", "the pull request is " + row.State})
	}
	eff, s, err := b.reviewEffective(ctx, orgID, req.InstallationID, repo)
	if err != nil {
		return nil, err
	}
	if s != nil {
		return skip(s)
	}
	// The three refusals somebody has to act on are told to the pull request's channel too
	// (noticeReviewSkipped); a try's are the console's alone.
	tell := func(e review.Effective, s *reviewSkip) {
		if req.Inline == nil {
			b.noticeReviewSkipped(ctx, row, e, pull, head, s)
		}
	}
	if s := b.reviewPlanGate(ctx, orgID); s != nil {
		// Recorded whatever else would have stopped it, so the console says why nothing is reviewed;
		// told only where the plan is all that stands between the pull request and a review. A draft,
		// a bot's bump, a push to a repository reviewed on open: the gate would let none of them
		// through on any plan, and a channel told of each would hear "plan" all day.
		if p, ps, err := planReview(eff, pull, req.Types, req.Post, req.AllowLive); err == nil && ps == nil {
			if fs, _ := reviewFilterSkip(req, p, pull, b.reviewByUs(pull.User)); fs == nil {
				tell(p.eff, s)
			}
		}
		return skip(s)
	}
	plan, s, err := planReview(eff, pull, req.Types, req.Post, req.AllowLive)
	if err != nil {
		return nil, err
	}
	if s != nil {
		return skip(s)
	}
	plan.withLabel(req.Label)
	automatic := automaticTrigger(req.Trigger) && !req.BypassFilters
	if s, record := reviewFilterSkip(req, plan, pull, b.reviewByUs(pull.User)); s != nil {
		if !record {
			return nil, s
		}
		return skip(s)
	}
	// What would stop the engine before it spent anything stops the request here instead, before any
	// money is held for it or the money's refusal is told to anybody.
	if s, err := reviewOwnKeyGate(b.settings.Get(ctx, orgID)); err != nil {
		return nil, err
	} else if s != nil {
		return skip(s)
	}
	specs, _, err := resolveReviewTypes(ctx, b.store, orgID, plan.keys)
	if err != nil {
		return nil, err
	}
	if req.Inline != nil {
		specs = []reviewTypeSpec{reviewInlineSpec(req.Inline)}
	}
	if len(specs) == 0 {
		return skip(&reviewSkip{"types_off", "every review type this pull request would get is turned off or does not exist"})
	}
	scope := req.Scope
	if scope == "" {
		scope = reviewScopeWhole
		if req.Trigger == "push" && row.LastReviewedSHA != "" {
			scope = reviewScopeSinceLast
		}
	}
	// A full review is told apart by its key, so the once-a-day check (reviewFullRunsSince) can find
	// it, and keeps the mark when the key moves on below.
	prefix := reviewKeyTrigger(req.Trigger)
	if req.Full {
		prefix = "full:" + prefix
	}
	key := reviewDedupeKey(req.Trigger, req.TriggerRef, head, plan.keys, scope, plan.post)
	if req.Full {
		key = "full:" + key
	}
	for range 10 {
		existing, err := b.store.ReviewRunByDedupeKey(ctx, orgID, row.ID, key)
		if err != nil || existing == nil {
			if err != nil {
				return nil, err
			}
			break
		}
		switch {
		case slices.Contains([]string{"cancelled", "skipped", "failed", "superseded"}, existing.Status),
			existing.Status == "noop" && existing.CacheKey == "":
			// That request answered nothing, and the pull request is being asked again — reopened
			// after it was closed, made ready after the money ran out, force-pushed back to a head
			// whose run stood aside for a push since, or labelled again after the label's review stood
			// aside (a noop the cache did not answer: reviewLabelsAtClaim). The key moves on from the
			// run that holds it, so the same delivery sent twice still finds one run, the newer.
			key = prefix + ":" + reviewHash(key, existing.PublicID)[:40]
			continue
		}
		return existing, nil
	}
	// The pause after the dedupe: a request that is one already queued or done — the same delivery
	// under a new id, a ready_for_review at a head reviewed already, the catch-up beside the opening's
	// delivery — asks for no review, and at the ceiling would otherwise pause the pull request for a
	// sixth review nobody was going to run.
	if automatic && reviewCountsTowardsPause(req.Trigger, req.BotLabel) && req.Inline == nil {
		if s, err := b.reviewAutoPaused(ctx, orgID, req.InstallationID, row, req.Trigger); err != nil {
			return nil, err
		} else if s != nil {
			tell(plan.eff, s)
			return skip(s)
		}
	}
	maxUSD := plan.eff.MaxUSD
	if err := b.reviewBudgetRoom(ctx, orgID, maxUSD, 0); err != nil {
		if errors.Is(err, errReviewSpendUnknown) {
			return nil, err
		}
		// A command is answered on the pull request by its own reply, which says the same thing, a
		// console start in the console and a channel's request in the channel; the daily note would be
		// the same news twice. A try is not the pull request's review at all, and its refusal is the
		// console's answer and nothing more.
		note := plan.post
		if req.Trigger == "command" || req.Trigger == "console" || req.Trigger == "chat" {
			note = ""
		}
		if req.Inline == nil {
			b.reviewBudgetStopped(ctx, orgID, req.InstallationID, row, note, err)
		}
		s := &reviewSkip{"budget", err.Error()}
		tell(plan.eff, s)
		return skip(s)
	}
	// Nothing is counted here: a run queued now is counted when it starts, if it does.
	if s, err := b.reviewThrottled(ctx, orgID, row, 0); err != nil {
		return nil, err
	} else if s != nil {
		b.auditSystem(ctx, orgID, "review.skipped", reviewAuditEvent(row, "", map[string]any{"reason": "throttle", "trigger": req.Trigger}))
		return skip(s)
	}
	o := reviewOptions{Post: req.Post, AllowLive: req.AllowLive, Scope: scope, Full: req.Full, Inline: req.Inline, Label: req.Label,
		BotLabel: req.BotLabel, Title: cutAtRune(pull.Title, 512), Base: pull.Base.Ref, Head: pull.Head.Ref}
	if plan.named {
		o.Types = plan.keys
	}
	opts, _ := json.Marshal(o)
	run, _, err := b.store.EnqueueReviewRun(ctx, orgID, ReviewRunRequest{ReviewPRID: row.ID, InstallationID: req.InstallationID,
		Kind: kind, DedupeKey: key, Trigger: req.Trigger, TriggerRef: req.TriggerRef, RequestedBy: req.RequestedBy,
		HeadSHA: head, BaseSHA: pull.Base.SHA, Types: reviewRunTypes(specs), RuleLabel: plan.label, ConfigHash: plan.eff.Hash(),
		ReservedUSD: maxUSD, NotBefore: req.NotBefore, RequestJSON: string(opts)})
	if err != nil {
		return nil, err
	}
	if req.Inline == nil {
		if err := b.store.SetReviewPRSkipReason(ctx, orgID, row.ID, ""); err != nil {
			return nil, err
		}
	}
	slog.Info("code review queued", "org", orgID, "repo", repo, "pr", pr, "run", run.PublicID, "kind", kind, "trigger", req.Trigger,
		"types", plan.keys, "post", plan.post)
	return run, nil
}

// reviewFilterSkip is the gate's filters on a planned review, steps 5 and 6 of enqueueReview: for one
// nobody asked for the "when" setting, drafts, bots and excluded authors, and for any the forks.
// record is whether the reason goes on the pull request. The plan's refusal asks them too, to tell
// the channel only of a pull request they would have let through.
//
// ownApp is a pull request this App opened — a fix job's — which is not passed over as a bot's: a
// bot's pull request is skipped because it is a dependency bump nobody needs read, and a fix job's is
// code a model wrote, which is what most needs a review. It opens as a draft, so it is reviewed once
// somebody marks it ready, as anybody's draft is; the authors' list still applies to it. So is a bot
// the settings name (review_bots): one team's bots open dependency bumps, another's a coding agent's
// changes, and only the team knows which of its bots are which.
func reviewFilterSkip(req reviewRequest, plan reviewPlan, pull *githubPull, ownApp bool) (s *reviewSkip, record bool) {
	automatic := automaticTrigger(req.Trigger) && !req.BypassFilters
	if automatic {
		switch t := plan.eff.Trigger; {
		case req.Trigger == "push" && t != review.TriggerPush:
			// Not recorded: every push to a repository that reviews on open only would write this
			// over the reason that mattered.
			return &reviewSkip{"trigger", "this repository is not reviewed on every push"}, false
		case req.Trigger == "label" && t == review.TriggerCommand:
			// Nor this: a label added is not a request for the pull request's review, whose reason for
			// not running is the one a person asking about it wants to read.
			return &reviewSkip{"trigger", "this repository is reviewed only when somebody asks"}, false
		case t == review.TriggerCommand:
			return &reviewSkip{"trigger", "this repository is reviewed only when somebody asks"}, true
		case pull.Draft && !plan.eff.Drafts:
			return &reviewSkip{"draft", "draft pull requests are not reviewed automatically here"}, true
		case strings.EqualFold(pull.User.Type, "Bot") && !ownApp && !plan.eff.ReviewsBot(pull.User.Login):
			return &reviewSkip{"bot", pull.User.Login + " is a bot, and not on this repository's list of bots to review"}, true
		case plan.eff.ExcludesAuthor(pull.User.Login):
			return &reviewSkip{"excluded_author", pull.User.Login + " is on this repository's list of authors not reviewed automatically"}, true
		}
	}
	if pull.IsFork() && (automatic || plan.eff.Forks == review.ForksOff) {
		// A fork's code is a stranger's, and reviewing it spends the organisation's money: a member
		// asks for it, or nobody does.
		return &reviewSkip{"fork", "a pull request from a fork is reviewed only when a member asks for it"}, true
	}
	return nil, false
}

// reviewDedupeKey is what makes two requests one. A review nobody asked for is one per head, types,
// scope and destination: the same delivery sent twice, or a body resent under a new delivery id —
// the id is a header, which the signature does not cover — queues one review. One a person asked
// for is one per thing they did: the comment, the console request.
func reviewDedupeKey(trigger, ref, head string, keys []string, scope string, post review.Mode) string {
	if automaticTrigger(trigger) {
		return reviewKeyTrigger(trigger) + ":" + reviewHash(head, strings.Join(keys, ","), scope, string(post))[:40]
	}
	return trigger + ":" + reviewHash(cmp.Or(ref, newPublicID()))[:40]
}

// reviewKeyTrigger is the trigger a dedupe key is written under. The catch-up's is the opening's:
// what it queues is the review the pull request's "opened" delivery would have queued, and the two
// can both arrive — the catch-up finding a pull request whose delivery is still in the inbox, or
// somebody pressing Redeliver on the one that failed — and must come to one run, not two reviews
// of one head that differ only in who noticed first.
func reviewKeyTrigger(trigger string) string {
	if trigger == "catchup" {
		return "open"
	}
	return trigger
}

func reviewRunTypes(specs []reviewTypeSpec) []ReviewRunType {
	out := make([]ReviewRunType, 0, len(specs))
	for _, s := range specs {
		out = append(out, ReviewRunType{Key: s.Key, Version: s.Version})
	}
	return out
}

// reviewAuditEvent is a review's audit row: the pull request it is about, as people name one. The
// actor is the system's (auditSystem), like every other unattended write.
func reviewAuditEvent(pr *ReviewPR, runID string, details map[string]any) AuditEvent {
	if runID != "" {
		details["run"] = runID
	}
	return AuditEvent{TargetKind: "pull_request", TargetID: fmt.Sprintf("%s#%d", pr.Repo, pr.Number),
		TargetName: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), Details: auditDetails(details)}
}

// reviewAutoPaused is the pause at the gate, for a review nobody asked for: a pull request somebody
// paused, or one that has had reviewAutoPauseAfter automatic reviews, which this request then pauses
// — the one write that says so, conditional on it not being paused yet, so of two requests racing
// one pauses it, audits it and has the summary's footer say so, rendered again for nothing. Nothing
// else is posted: the footer is where a reader of the pull request looks for why a push was not
// reviewed, and a comment saying it would be the notification nobody wanted five of already.
func (b *Bot) reviewAutoPaused(ctx context.Context, orgID, installationID int64, pr *ReviewPR, trigger string) (*reviewSkip, error) {
	if pr.Paused {
		return &reviewSkip{"paused", "automatic reviews of this pull request are paused"}, nil
	}
	if pr.AutoReviews < reviewAutoPauseAfter {
		return nil, nil
	}
	paused, err := b.store.pauseReviewPR(ctx, orgID, pr.ID, true)
	if err != nil {
		return nil, err
	}
	pr.Paused = true
	if paused {
		pr.PausedAuto = true
		b.auditSystem(ctx, orgID, "review.paused", reviewAuditEvent(pr, "", map[string]any{"after": pr.AutoReviews, "by": "code review"}))
		if err := b.queueReviewResync(ctx, orgID, installationID, pr, trigger, "paused:"+newPublicID()); err != nil {
			slog.Warn("code review: the summary was not queued to say the reviews are paused", "org", orgID, "pr", pr.ID, "err", err)
		}
		slog.Info("code review: automatic reviews paused", "org", orgID, "repo", pr.Repo, "pr", pr.Number, "after", pr.AutoReviews)
	}
	return &reviewSkip{"paused", fmt.Sprintf("automatic reviews of this pull request paused after %d", reviewAutoPauseAfter)}, nil
}

// reviewOwnKeyGate is the organisation's own model key at the gate: a review on it needs the key's
// reviews switch, which is off until an admin turns it on (errReviewsOffOwnKey says why), and a
// stored key the deployment will not use stops every model call. A key whose row could not be read
// is a failure to find out, not a refusal.
func reviewOwnKeyGate(st Settings) (*reviewSkip, error) {
	k := st.OwnKey
	switch {
	case k.Unknown:
		return nil, k.refusal()
	case k.Present && !k.Allowed:
		return &reviewSkip{"own_key_blocked", k.refusal().Error()}, nil
	case k.Active() && !k.Ref.Reviews:
		return &reviewSkip{"own_key_off", errReviewsOffOwnKey.Error()}, nil
	}
	return nil, nil
}

// reviewThrottled is the throttles: how many reviews the pull request and its repository have had
// in the last day, other than except, against reviewRunsPerPRDay and reviewRunsPerRepoDay.
func (b *Bot) reviewThrottled(ctx context.Context, orgID int64, pr *ReviewPR, except int64) (*reviewSkip, error) {
	nPR, nRepo, err := b.store.reviewRunsStarted(ctx, orgID, pr.ID, pr.Repo, nowMinus(24*time.Hour), except)
	switch {
	case err != nil:
		return nil, err
	case nPR >= reviewRunsPerPRDay:
		return &reviewSkip{"throttle", fmt.Sprintf("this pull request has had %d reviews in the last day", nPR)}, nil
	case nRepo >= reviewRunsPerRepoDay:
		return &reviewSkip{"throttle", fmt.Sprintf("this repository has had %d reviews in the last day", nRepo)}, nil
	}
	return nil, nil
}

// ---- money ----

// errReviewSpendUnknown is a money check that could not be made: billing facts or the spend could
// not be read. It is not the money saying no — nothing is recorded or told for it — and the request
// is tried again: at the gate the delivery is, at the claim the run is.
var errReviewSpendUnknown = errors.New("spend accounting is unavailable, so no review has been started")

// reviewMoneyRefusal is the money saying no, and whether the account's own limits — its credit or
// its monthly budget, which code review shares with everything else — are what said it.
type reviewMoneyRefusal struct {
	text    string
	account bool
}

func (e *reviewMoneyRefusal) Error() string { return e.text }

// reviewBudgetRoom refuses a review the money could not absorb: need dollars on top of what the
// organisation's other running reviews hold, against every limit that applies. except is the run
// asking at claim, whose own reservation is need. A refusal is a *reviewMoneyRefusal; a check that
// could not be made is errReviewSpendUnknown.
func (b *Bot) reviewBudgetRoom(ctx context.Context, orgID int64, need float64, except int64) error {
	st := b.settings.Get(ctx, orgID)
	// On the organisation's own key nothing is drawn from credit, as for fix jobs; its budgets
	// still apply, measured on its own key's spend.
	ownKey := st.OwnKey.Active()
	unknown := func(err error) error { return fmt.Errorf("%w: %v", errReviewSpendUnknown, err) }
	if st.BillingUnknown && !ownKey {
		return errReviewSpendUnknown
	}
	held, err := b.store.reviewReservedUSD(ctx, orgID, except)
	if err != nil {
		return unknown(err)
	}
	total := need + held
	if (st.CreditEnforced || st.AllowanceActive) && !ownKey {
		bal, metered, err := b.store.SpendableCredit(ctx, orgID)
		if err != nil {
			return unknown(err)
		}
		if metered && microsToUSD(bal) < total {
			return &reviewMoneyRefusal{account: true, text: fmt.Sprintf("this account has %s of credit left, less than the $%.2f a review may spend on top of the $%.2f running reviews hold",
				creditAmount(bal), need, held)}
		}
	}
	if budget := st.EffectiveBudget(); budget > 0 {
		spent, err := budgetSpend(ctx, b.store, orgID, st)
		if err != nil {
			return unknown(err)
		}
		if spent+total > budget {
			return &reviewMoneyRefusal{account: true, text: fmt.Sprintf("the account has $%.2f of its $%.2f monthly budget left, less than the $%.2f a review may spend on top of the $%.2f running reviews hold",
				max(budget-spent, 0), budget, need, held)}
		}
	}
	owner := keyOwnerPlatform
	if ownKey {
		owner = keyOwnerOrg
	}
	for _, c := range []struct {
		cap   float64
		since string
		what  string
	}{
		{st.ReviewMonthlyBudgetUSD, time.Now().UTC().Format("2006-01") + "-01 00:00:00", "monthly code review budget"},
		{st.ReviewDailyUSD, today(), "daily code review cap"},
	} {
		if c.cap <= 0 {
			continue
		}
		spent, err := b.store.reviewSpendSince(ctx, orgID, owner, c.since)
		if err != nil {
			return unknown(err)
		}
		if spent+total > c.cap {
			return &reviewMoneyRefusal{text: fmt.Sprintf("code review has $%.2f of its $%.2f %s left, less than the $%.2f a review may spend on top of the $%.2f running reviews hold",
				max(c.cap-spent, 0), c.cap, c.what, need, held)}
		}
	}
	return nil
}

// reviewBudgetStopped is what a review the money stopped leaves behind: the reason on the pull
// request, an audit row, the hourly alert to the admins, and on a live repository one note a day
// on the pull request, so its author is not left waiting for a review that is not coming. The note
// says why in general terms only: a public repository's readers are not told what anybody spends.
func (b *Bot) reviewBudgetStopped(ctx context.Context, orgID, installationID int64, pr *ReviewPR, post review.Mode, why error) {
	if err := b.store.SetReviewPRSkipReason(ctx, orgID, pr.ID, "budget"); err != nil {
		slog.Warn("code review: skip reason not recorded", "org", orgID, "pr", pr.ID, "err", err)
	}
	b.auditSystem(ctx, orgID, "review.skipped", reviewAuditEvent(pr, "", map[string]any{"reason": "budget"}))
	if b.agent != nil {
		// Where to change it, in the console's words: code review's two caps sit beside the account's
		// monthly budget — on Billing where the deployment sells plans, on Models where it does not,
		// which is where that budget is (settings-page.tsx).
		where := "Settings › Models"
		if b.cfg.BillingEnabled() {
			where = "Settings › Billing"
		}
		text := ":moneybag: Code review stopped: " + why.Error() + ". Code review's own monthly budget and daily cap " +
			"are under " + where + " › Code review budget, beside the account's monthly budget."
		var mr *reviewMoneyRefusal
		if errors.As(why, &mr) && mr.account {
			if hint := b.settings.Get(ctx, orgID).raiseBudgetHint(); hint != "" {
				text += " " + hint
			}
		}
		b.agent.alert(ctx, orgID, "review-budget", text)
	}
	if post != review.ModeLive || !b.store.AlertOnce(ctx, orgID, fmt.Sprintf("review-budget-note:%d", pr.ID), 24*time.Hour) {
		return
	}
	gh, err := b.reviewClient(orgID, installationID, pr.Repo, pr.Number)
	if err == nil {
		_, err = gh.CreateIssueComment(ctx, "<sub>attest_tag did not review this pull request: the organisation's code review budget "+
			"is spent for now. An admin of its attest_tag account can raise it.</sub>")
	}
	if err != nil {
		slog.Warn("code review: budget note not posted", "org", orgID, "repo", pr.Repo, "pr", pr.Number, "err", err)
	}
}

// ---- routing ----

// githubPullRequestPayload is the part of a pull_request delivery the router reads.
type githubPullRequestPayload struct {
	Action      string      `json:"action"`
	Number      int         `json:"number"`
	Label       githubLabel `json:"label"` // the label a "labeled" delivery is about
	PullRequest githubPull  `json:"pull_request"`
	Repository  struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Sender githubUser `json:"sender"`
}

// reviewPullRequestEvent routes a pull_request delivery: opened, reopened and ready for review go
// through the gate to a review; a push moves the stored head, re-renders the summary's footer for
// free, and on a repository reviewed on every push queues a review after the debounce; a label that
// adds review types queues a review of those (reviewLabeled); closing the pull request or turning it
// back into a draft cancels its runs. Whoever sent it — a person, a merge bot, a workflow's commit
// — the head it describes is the pull request's head.
func (b *Bot) reviewPullRequestEvent(ctx context.Context, d *githubDelivery, raw []byte) error {
	var p githubPullRequestPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	repo, n := p.Repository.FullName, cmp.Or(p.Number, p.PullRequest.Number)
	if !validGitHubRepo(repo) || n <= 0 {
		slog.Warn("GitHub pull_request delivery names no repository or number", "delivery", d.ID, "org", d.OrgID)
		return nil
	}
	pull := &p.PullRequest
	pull.Number = n
	req := reviewRequest{InstallationID: d.InstallationID, TriggerRef: "delivery:" + d.ID, RequestedBy: "github:" + pull.User.Login, Pull: pull}
	queue := func(trigger string) error {
		req.Trigger = trigger
		_, err := b.enqueueReview(ctx, d.OrgID, repo, n, req)
		var skip *reviewSkip
		if errors.As(err, &skip) {
			return nil // the reason is on the pull request; there is nothing to retry
		}
		return err
	}
	switch p.Action {
	case "opened", "reopened", "ready_for_review":
		return queue("open")
	case "synchronize":
		return b.reviewHeadMoved(ctx, d.OrgID, repo, req)
	case "labeled":
		return b.reviewLabeled(ctx, d.OrgID, repo, req, p.Label.Name, p.Sender)
	case "closed", "converted_to_draft":
		row, err := b.store.UpsertReviewPR(ctx, d.OrgID, reviewPRFacts(repo, pull))
		if err != nil {
			return err
		}
		why := "the pull request was closed"
		if p.Action == "converted_to_draft" {
			why = "the pull request went back to draft"
		}
		if n, err := b.store.CancelReviewRuns(ctx, d.OrgID, row.ID, why); err != nil {
			return err
		} else if n > 0 {
			slog.Info("code review runs cancelled", "org", d.OrgID, "repo", repo, "pr", row.Number, "runs", n, "why", why)
		}
		if strings.HasPrefix(row.NotifyLast, reviewNoticeStart+":") {
			// A run cancelled in the queue — put back after its start was announced — or whose lane
			// died has nobody left to put its message back; one still running is its lane's to.
			b.noticeStartOrphaned(ctx, d.OrgID, row.ID)
		}
		if p.Action == "closed" && pull.Merged {
			b.reviewMerged(ctx, d, repo, pull, row)
		}
		return nil
	}
	slog.Debug("pull_request delivery with nothing for code review to do", "delivery", d.ID, "action", p.Action)
	return nil
}

// reviewMerged tells the pull request's channel it was merged, when its settings name one — with
// the branch rule its branches meet applied, as a review of it would have been told. Only a
// repository still reviewed here announces anything: its delivery got this far only because it is
// (ReviewModeAt), and the settings are read again in case that changed while it waited.
func (b *Bot) reviewMerged(ctx context.Context, d *githubDelivery, repo string, pull *githubPull, row *ReviewPR) {
	eff, s, err := b.reviewEffective(ctx, d.OrgID, d.InstallationID, repo)
	if err != nil || s != nil {
		if err != nil {
			slog.Warn("code review: the settings could not be read; the merge is not announced", "org", d.OrgID, "repo", repo,
				"pr", row.Number, "err", err)
		}
		return
	}
	by := ""
	if pull.MergedBy != nil {
		by = pull.MergedBy.Login
	}
	b.notifyReviewChannel(ctx, row, reviewNoticeEffective(eff, pull), reviewNotice{kind: reviewNoticeMerged, before: -1,
		title: pull.Title, base: pull.Base.Ref, head: pull.Head.Ref, mergedBy: by})
}

// reviewLabeled is a label put on a pull request. A label rule may add review types for it on top of
// the branch rule's (review.LabelTypes), and a pull request reviewed before the label went on has
// not had them: those are reviewed now, on the head as it is, and nothing else — the branch rule's
// own types were reviewed, or not, for reasons of their own, and paying for them again because a
// label went on would be a full review nobody asked for. A type already reviewed at this head, or
// queued to be, is not asked for again, so a label set as the pull request was opened — whose
// opening already runs its types — costs nothing; the claim checks again (reviewLabelsAtClaim),
// since the opening may be queued beside this. It is a review nobody asked for, held to the filters
// such a review is (automaticTrigger), and is run under the label rules as they are when it starts:
// a label taken off, or a rule changed so the label adds nothing, while it waits ends it having
// spent nothing. It is the review of whoever put the label on, who is recorded as having asked for
// it. A pull request whose automatic reviews are paused gets none, and one a person's label asks for
// is not counted towards the pause; a bot's is, since no person chose it (reviewCountsTowardsPause).
func (b *Bot) reviewLabeled(ctx context.Context, orgID int64, repo string, req reviewRequest, label string, sender githubUser) error {
	pull := req.Pull
	if strings.TrimSpace(label) == "" || !commitSHA.MatchString(pull.Head.SHA) || pull.Base.Ref == "" {
		return nil
	}
	if sender.Login != "" {
		req.RequestedBy = "github:" + sender.Login
	}
	req.BotLabel = b.reviewFromBot(sender)
	eff, s, err := b.reviewEffective(ctx, orgID, req.InstallationID, repo)
	if err != nil || s != nil {
		return err // not reviewed here: an opening's own delivery records why
	}
	_, rule, ok := review.MatchRule(eff.BranchRules, pull.Base.Ref, pull.Head.Ref)
	if !ok {
		return nil
	}
	add, _ := review.LabelTypes(eff.BranchRules, pull.Base.Ref, pull.Head.Ref, []string{label}, rule.Types)
	if len(add) == 0 {
		return nil // no label rule names it here, or the branch rule runs its types already
	}
	if row, err := b.store.ReviewPRByNumber(ctx, orgID, repo, pull.Number); err != nil {
		return err
	} else if row != nil {
		if row.Paused {
			return nil // `status` and the footer already say why nothing is reviewed by itself
		}
		covered, err := b.store.reviewTypesAtHead(ctx, orgID, row.ID, pull.Head.SHA, 0)
		if err != nil {
			return err
		}
		add = slices.DeleteFunc(add, func(t string) bool { return slices.Contains(covered, t) })
	}
	if len(add) == 0 {
		return nil
	}
	req.Trigger, req.Types, req.Label, req.NotBefore = "label", add, label, time.Now().Add(reviewLabelWait)
	_, err = b.enqueueReview(ctx, orgID, repo, pull.Number, req)
	var skip *reviewSkip
	if errors.As(err, &skip) {
		return nil // the reason is on the pull request, or there was none worth recording
	}
	return err
}

// reviewHeadMoved is a push to a pull request, told by its synchronize delivery or found by the
// catch-up (review_catchup.go): req carries the pull request as it is now. The stored head moves;
// the summary already on the pull request says which head it describes, and that is now a
// different head from the pull request's, so the footer says so at once, for nothing — rendered
// again from what is stored, with no model and no read of the code. A repository reviewed on every
// push also gets its review, once the push debounce has passed without another.
func (b *Bot) reviewHeadMoved(ctx context.Context, orgID int64, repo string, req reviewRequest) error {
	pull := req.Pull
	row, err := b.store.UpsertReviewPR(ctx, orgID, reviewPRFacts(repo, pull))
	if err != nil {
		return err
	}
	if row.LastReviewedSHA != "" && row.HeadSHA != "" {
		if _, _, err := b.store.EnqueueReviewRun(ctx, orgID, ReviewRunRequest{ReviewPRID: row.ID,
			InstallationID: req.InstallationID, Kind: "resync", DedupeKey: "resync:" + row.HeadSHA, Trigger: "push",
			TriggerRef: req.TriggerRef, HeadSHA: row.HeadSHA}); err != nil {
			return err
		}
	}
	req.Trigger, req.NotBefore = "push", time.Now().Add(reviewPushDebounce)
	_, err = b.enqueueReview(ctx, orgID, repo, pull.Number, req)
	var skip *reviewSkip
	if errors.As(err, &skip) {
		return nil // the reason is on the pull request, or there was none worth recording
	}
	return err
}

// ---- the lane ----

// reviewLaneOn reports whether this process can run reviews at all: it needs the engine and the
// App its tokens are minted from, on a deployment that has code review (CODE_REVIEW not off).
func (b *Bot) reviewLaneOn() bool {
	return b.review != nil && b.proxy != nil && b.proxy.ghApp != nil && !b.cfg.codeReviewOff()
}

// runReviewLane runs this instance's review workers until ctx ends. Not behind the leader lease:
// every claim takes its run under a lease of its own, so instances share the queue.
func (b *Bot) runReviewLane(ctx context.Context) {
	var wg sync.WaitGroup
	for range reviewLaneWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if !b.nextReviewRun(ctx) {
					select {
					case <-ctx.Done():
						return
					case <-time.After(reviewLaneIdlePoll):
					}
				}
			}
		}()
	}
	// Runs whose lane died with their attempts spent are nobody's work any more, and would read as
	// running in the console for ever. Retiring them, and putting back the message of any whose start
	// was announced, is this loop's only job.
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-tick.C:
			b.sweepReviewLane(ctx)
		}
	}
}

// sweepReviewLane retires the runs no lane will ever finish (sweepReviewRuns), and puts back the
// message of each review among them whose start was announced: its lane died before it could.
func (b *Bot) sweepReviewLane(ctx context.Context) {
	retired, err := b.store.sweepReviewRuns(ctx)
	if err != nil {
		slog.Warn("code review sweep", "err", err)
	}
	for _, r := range retired {
		if r.Kind == "review" && ctx.Err() == nil {
			b.noticeStartOrphaned(ctx, r.OrgID, r.ReviewPRID)
		}
	}
}

// reviewHold is one claimed run and the lease it is held under. The lease value moves with every
// renewal, and every write the lane makes for the run is fenced on it, so the renewals and the
// writes take turns under one lock; a write or a renewal that finds the lease gone marks the run
// lost, and nothing is written for it again.
type reviewHold struct {
	b           *Bot
	mu          sync.Mutex
	run         *ReviewRun
	lost        atomic.Bool
	cancelAsked atomic.Bool
	// ended is the run finished or put back by this lane: the lease is released, and a renewal
	// still in flight finding it gone is not news.
	ended atomic.Bool
	// notice is where a review run is announced and what of, once processReview has planned it
	// (review_notify.go); nil before, and for every other kind of run. noticed is closed when the
	// announcement of its start, made beside the work, is done, and startSaid — written by that
	// announcement before it closes noticed, read only after — is whether the message says this run
	// is under way. ending is how the run ended, once endReviewRun has recorded it, for nextReviewRun
	// to tell. All but startSaid are the lane goroutine's alone.
	notice    *reviewNoticeRun
	noticed   chan struct{}
	startSaid bool
	ending    *ReviewRunResult
	// signals are a live review's reactions and check run on its pull request (review_signals.go),
	// once processReview has planned it; nil for every other run.
	signals *reviewSignals
}

// write runs one fenced write, or refuses it once the lease is known to be lost.
func (h *reviewHold) write(fn func(r *ReviewRun) error) error {
	if h.lost.Load() {
		return errLeaseLost
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	err := fn(h.run)
	if errors.Is(err, errLeaseLost) {
		h.lost.Store(true)
	}
	return err
}

// touch renews the lease, which is also how the lane hears that somebody asked the run to stop. The
// poster calls it before each write to GitHub too: a lane that stalled past its lease — a long GC
// pause, a laptop lid — finds out there, before it posts what another lane is posting.
func (h *reviewHold) touch(ctx context.Context) error {
	return h.write(func(r *ReviewRun) error {
		cancel, err := h.b.store.touchReviewRun(ctx, r)
		if cancel {
			h.cancelAsked.Store(true)
		}
		return err
	})
}

// nextReviewRun claims one run and sees it through. It reports whether there was one.
func (b *Bot) nextReviewRun(ctx context.Context) bool {
	r, err := b.store.claimReviewRun(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("code review claim", "err", err)
		}
		return false
	}
	if r == nil {
		return false
	}
	if r.Attempts > 1 {
		slog.Warn("code review run resumed after an interrupted attempt", "run", r.PublicID, "attempt", r.Attempts)
	}
	h := &reviewHold{b: b, run: r}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop, held := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(held)
		t := time.NewTicker(reviewRunTouchEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if h.ended.Load() {
					return
				}
				// Not runCtx: a renewal the cancellation interrupted after the row was written would
				// leave the held lease value behind the database's.
				err := h.touch(context.WithoutCancel(ctx))
				switch {
				case h.ended.Load():
					return
				case errors.Is(err, errLeaseLost):
					slog.Warn("code review lease lost; another lane has the run", "run", r.PublicID)
					cancel()
					return
				case err != nil:
					slog.Warn("code review lease", "run", r.PublicID, "err", err)
				case h.cancelAsked.Load():
					cancel()
				}
			}
		}
	}()
	switch r.Kind {
	case "review", "try":
		b.processReview(runCtx, ctx, h)
	case "resync":
		b.processResync(runCtx, ctx, h)
	case "reply":
		b.processReply(runCtx, ctx, h)
	default:
		b.endReviewRun(ctx, h, ReviewRunResult{Status: "failed", Score: -1, Error: "the lane does not run " + r.Kind + " runs yet"})
	}
	close(stop)
	<-held
	// How the run ended is told here, once its work is done and its pull request let go, rather than
	// where it is recorded: what follows the record there — a command answered from an earlier review,
	// the model failure's alert — is not to wait on a chat platform that is slow or down, nor on GitHub.
	b.signalRunEnded(ctx, h)
	if h.ending != nil {
		b.noticeRunEnded(ctx, h, *h.ending)
	}
	// Its start's announcement does not outlive the run: one put back must leave notify_last as the
	// start left it for the next claim to find, not race it.
	h.waitNotice()
	return true
}

// endReviewRun records how a run ended, unless its lease is gone — then the run is another lane's,
// and so is its ending — and reports whether it was recorded. lane is the lane's own context: the
// work's may be cancelled by now. Every ending the lane makes passes here, and is kept on the hold
// for nextReviewRun to tell the run's channel (noticeRunEnded) once the run is over; the endings
// made elsewhere — cancelled in the queue, retired by the sweep — are told where they are made
// (noticeStartOrphaned), so no way of ending a review leaves its message saying "Reviewing…".
func (b *Bot) endReviewRun(lane context.Context, h *reviewHold, res ReviewRunResult) bool {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(lane), reviewWriteTimeout)
	defer cancel()
	err := h.write(func(r *ReviewRun) error {
		err := b.store.finishReviewRun(wctx, r, res)
		h.ended.Store(err == nil)
		return err
	})
	switch {
	case errors.Is(err, errLeaseLost):
		slog.Warn("code review run ended after its lease was lost; nothing recorded", "run", h.run.PublicID, "status", res.Status)
	case err != nil:
		slog.Warn("code review run end not recorded", "run", h.run.PublicID, "status", res.Status, "err", err)
	default:
		slog.Info("code review run ended", "org", h.run.OrgID, "repo", h.run.Repo, "pr", h.run.PRNumber, "run", h.run.PublicID,
			"kind", h.run.Kind, "status", res.Status, "cost_usd", res.CostUSD, "error", res.Error)
		h.ending = &res
	}
	return err == nil
}

// requeueReview puts a run back to be claimed from notBefore. spend says whether this attempt
// counts against the run's: GitHub asking for a wait, or the instance shutting down, is not the
// run failing.
func (b *Bot) requeueReview(lane context.Context, h *reviewHold, notBefore time.Time, why string, spend bool) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(lane), reviewWriteTimeout)
	defer cancel()
	if err := h.write(func(r *ReviewRun) error {
		err := b.store.requeueReviewRun(wctx, r, notBefore, why, spend)
		h.ended.Store(err == nil)
		return err
	}); err != nil {
		if !errors.Is(err, errLeaseLost) {
			slog.Warn("code review run not requeued", "run", h.run.PublicID, "err", err)
		}
		return
	}
	slog.Info("code review run put back", "run", h.run.PublicID, "until", notBefore.UTC().Format(time.RFC3339), "attempt_spent", spend, "why", why)
}

// interrupted ends a run whose work stopped short, the way its stopping deserves, and reports
// whether it had: a lost lease writes nothing, a cancel asked for ends it cancelled, and the
// instance shutting down puts it back for another to pick up, with no attempt spent.
func (b *Bot) interrupted(work, lane context.Context, h *reviewHold) bool {
	switch {
	case h.lost.Load():
		return true
	case h.cancelAsked.Load():
		b.endReviewRun(lane, h, ReviewRunResult{Status: "cancelled", Score: -1, Error: "cancelled while running"})
		return true
	case lane.Err() != nil:
		b.requeueReview(lane, h, time.Now(), "the instance running it shut down", false)
		return true
	case work.Err() != nil:
		// The work's context ended with neither: the renewal found the lease gone without the
		// write to say so having run yet.
		return true
	}
	return false
}

// reviewCheckpoint is review_runs.outcome_json: what the summary is rendered from, beyond the
// findings, and what the run's ending records once it is posted — and, for the console, what the
// review did not keep and why (Drops), and why a context repository it was given was not read.
type reviewCheckpoint struct {
	Summary       string              `json:"summary"`
	Risk          string              `json:"risk"`
	Types         []reviewTypeRunJSON `json:"types"`
	FullCoverage  bool                `json:"full_coverage"`
	Injection     bool                `json:"injection"`
	FileHashes    map[string]string   `json:"file_hashes"`
	ContextRepos  []string            `json:"context_repos,omitempty"`
	DefaultSHAs   map[string]string   `json:"default_shas,omitempty"`
	Private       bool                `json:"private"`
	FilesReviewed int                 `json:"files_reviewed"`
	NotReviewed   []reviewNotReviewed `json:"not_reviewed"`
	Candidates    int                 `json:"candidates"`
	Dropped       int                 `json:"dropped"`
	Drops         []reviewDrop        `json:"drops,omitempty"`
	ContextNotes  []string            `json:"context_notes,omitempty"`
	Skills        []reviewSkillRecord `json:"skills,omitempty"`
	Trace         []reviewTraceStep   `json:"trace,omitempty"`
	Kept          int                 `json:"kept"`
	// Resolved is what the run decided about earlier findings, and the answer each gets in its
	// thread, with whether it has been posted: a run put back after posting some answers posts the
	// rest, and never one twice.
	Resolved     []reviewResolvedJSON `json:"resolved,omitempty"`
	Model        string               `json:"model"`
	TokensIn     int64                `json:"tokens_in"`
	TokensOut    int64                `json:"tokens_out"`
	TokensCached int64                `json:"tokens_cached"`
	CostUSD      float64              `json:"cost_usd"`

	// RepoRules are the rules the run read from the repository's instruction files, which its
	// findings cite as C-ids and their comments quote (review.RenderContext.RepoRules).
	RepoRules []review.RepoRule `json:"repo_rules,omitempty"`
}

// reviewResolvedJSON is one earlier finding as a run left it (review_resolve.go).
type reviewResolvedJSON struct {
	Finding string `json:"finding"`           // its public id
	Status  string `json:"status,omitempty"`  // fixed or outdated, or open for one back; "" still standing
	Verdict string `json:"verdict,omitempty"` // the resolution check's, when one ran
	Reason  string `json:"reason,omitempty"`  // why it was closed or opened, or what the check said
	Moved   string `json:"moved,omitempty"`   // where it is at the head, path:line, when it moved
	Kept    string `json:"kept,omitempty"`    // why a check's "fixed" did not close it
	// Reply is the answer its thread gets — "Fixed in", "Back at", or "Still present at" for a claim
	// the check refuted — already sanitised; Replied that it was posted, or that GitHub will never
	// take it, or that the finding no longer stands as the run left it.
	Reply   string `json:"reply,omitempty"`
	Replied bool   `json:"replied,omitempty"`
	// Carried is the public id of the earlier run that decided this and never posted its answer,
	// which this one posts instead (carryResolvedAnswers).
	Carried string `json:"carried,omitempty"`
}

// reviewResolvedOf is a run's resolutions as its checkpoint keeps them. Only a finding with an inline
// comment has a thread to answer in, and the answers are Go's words around the check's reason, held
// to the poster's sanitiser like every reply.
func reviewResolvedOf(out *reviewOutcome, repo string) []reviewResolvedJSON {
	var list []reviewResolvedJSON
	for _, res := range out.Resolutions {
		e := reviewResolvedJSON{Finding: res.F.PublicID, Status: string(res.Status), Verdict: res.Verdict, Reason: res.Reason,
			Kept: res.Kept}
		if res.Status == "" {
			e.Reason = res.Said
		}
		if a := res.At; a != nil {
			e.Moved = fmt.Sprintf("%s:%d", a.Path, a.Line)
		}
		if res.F.GitHubCommentID > 0 {
			head := "`" + shortSHA(out.HeadSHA) + "`"
			switch {
			case res.Status == review.FindingFixed:
				e.Reply = "Fixed in " + head + "."
			case res.Status == review.FindingOpen:
				// Its thread was told it was fixed: it is told the fix is gone.
				e.Reply = "Back at " + head + ": the code this was about is here again."
				if res.Said != "" {
					e.Reply = "Back at " + head + ": " + res.Said
				}
			case res.Status == "" && res.Refuted:
				e.Reply = "Still present at " + head + "."
				if res.Said != "" {
					e.Reply = "Still present at " + head + ": " + res.Said
				}
			}
			if e.Reply != "" {
				e.Reply = review.Sanitize(e.Reply, review.SanitizeOptions{AllowedRepos: []string{repo}, MaxLen: reviewAnswerMaxLen})
			}
		}
		list = append(list, e)
	}
	return list
}

type reviewTypeRunJSON struct {
	Key     string `json:"key"`
	Summary string `json:"summary,omitempty"`
	Skipped string `json:"skipped,omitempty"`
}

func checkpointOf(out *reviewOutcome, skipped []review.TypeRun) *reviewCheckpoint {
	c := &reviewCheckpoint{Summary: out.Summary, Risk: out.Risk, FullCoverage: out.FullCoverage, Injection: out.InjectionDetected,
		FileHashes: out.FileHashes, ContextRepos: out.ContextRepos, DefaultSHAs: out.DefaultSHAs, Private: out.Private,
		FilesReviewed: out.FilesReviewed, Candidates: out.Candidates, Dropped: len(out.Dropped), Kept: len(out.Findings),
		Drops: out.Dropped, ContextNotes: out.ContextNotes, Skills: out.Skills, Trace: out.Trace,
		Model: out.Model, TokensIn: int64(out.Total.In), TokensOut: int64(out.Total.Out), TokensCached: int64(out.Total.CachedIn),
		CostUSD: out.Total.CostUSD, NotReviewed: []reviewNotReviewed{}}
	c.RepoRules = out.RepoRules
	for _, t := range append(slices.Clone(out.TypeRuns), skipped...) {
		c.Types = append(c.Types, reviewTypeRunJSON{Key: t.Key, Summary: t.Summary, Skipped: t.Skipped})
	}
	for _, f := range out.NotReviewed {
		c.NotReviewed = append(c.NotReviewed, reviewNotReviewed{Path: f.Path, Reason: f.Reason})
	}
	return c
}

func checkpointFrom(r *ReviewRun) (*reviewCheckpoint, bool) {
	if r.OutcomeJSON == "" || r.OutcomeJSON == "{}" {
		return nil, false
	}
	var c reviewCheckpoint
	if err := json.Unmarshal([]byte(r.OutcomeJSON), &c); err != nil {
		return nil, false
	}
	return &c, true
}

// result is the run's ending as review_runs records it, posted or recorded.
func (c *reviewCheckpoint) result(status string, score int) ReviewRunResult {
	nr := make([]review.NotReviewedFile, 0, len(c.NotReviewed))
	for _, f := range c.NotReviewed {
		nr = append(nr, review.NotReviewedFile{Path: f.Path, Reason: f.Reason})
	}
	return ReviewRunResult{Status: status, FilesReviewed: c.FilesReviewed, NotReviewed: nr, Candidates: c.Candidates,
		Dropped: c.Dropped, Kept: c.Kept, Score: score, Summary: c.Summary, Risk: c.Risk, Model: c.Model,
		TokensIn: c.TokensIn, TokensOut: c.TokensOut, TokensCached: c.TokensCached, CostUSD: c.CostUSD}
}

// reviewCacheKey is what makes two reviews the same review: the commits, the instruction files
// read at the base, the commits the context repositories were read at, the settings (with the
// rule applied), each type at its version, the engine, where the result goes, how much of the
// pull request was asked for, and what the types' skills said. A second request matching all of it
// is answered from the first.
func reviewCacheKey(out *reviewOutcome, configHash string, specs []reviewTypeSpec, post review.Mode, scope string, labels []string) string {
	ctxRepos := make([]string, 0, len(out.DefaultSHAs))
	for repo, sha := range out.DefaultSHAs {
		ctxRepos = append(ctxRepos, strings.ToLower(repo)+"@"+sha)
	}
	slices.Sort(ctxRepos)
	types := make([]string, 0, len(specs))
	for _, s := range specs {
		types = append(types, s.Key+"@"+strconv.Itoa(s.Version))
	}
	parts := []string{out.HeadSHA, out.BaseSHA, out.InstructionsHash, strings.Join(ctxRepos, ","), configHash,
		strings.Join(types, ","), reviewEngineVersion, string(post), scope}
	if len(labels) > 0 {
		// Only when a label added something, so a review of a pull request with no label rule in
		// play keeps the key it had before labels were part of it.
		ls := make([]string, 0, len(labels))
		for _, l := range labels {
			ls = append(ls, strings.ToLower(l))
		}
		slices.Sort(ls)
		parts = append(parts, "labels:"+strings.Join(ls, ","))
	}
	if out.SkillsHash != "" {
		// Likewise only when a type read skills: what they said, at the commits they were read at.
		parts = append(parts, "skills:"+out.SkillsHash)
	}
	return reviewHash(parts...)
}

// processReview runs one claimed review: the gate again, the money again, the engine, the
// checkpoint, and the poster. work is the run's context, cancelled when its lease is lost or a
// cancel is asked for; lane is the lane's, cancelled only when the instance stops.
func (b *Bot) processReview(work, lane context.Context, h *reviewHold) {
	r := h.run
	end := func(status, why string) {
		b.endReviewRun(lane, h, ReviewRunResult{Status: status, Score: -1, Error: why})
	}
	opts := reviewOptionsOf(r)
	if reviewTry(r) {
		if opts.Inline == nil {
			end("failed", "internal: a try carries the type it tries, and this one carries none")
			return
		}
		opts.Post, opts.AllowLive, opts.Types = review.ModeShadow, false, []string{opts.Inline.Key}
	}
	pr, err := b.store.ReviewPR(work, r.OrgID, r.ReviewPRID)
	if err != nil || pr == nil {
		if !b.interrupted(work, lane, h) {
			end("failed", fmt.Sprintf("the pull request could not be read: %v", err))
		}
		return
	}
	if pr.State != "open" {
		end("cancelled", "the pull request is "+pr.State)
		return
	}
	gh, err := b.reviewClient(r.OrgID, r.InstallationID, pr.Repo, pr.Number)
	if err != nil {
		end("failed", err.Error())
		return
	}
	// The gate, again: what was true when the run was queued may not be now.
	skipped := func(s *reviewSkip) {
		if !reviewTry(r) {
			if err := b.store.SetReviewPRSkipReason(work, r.OrgID, pr.ID, s.Reason); err != nil {
				slog.Warn("code review: skip reason not recorded", "run", r.PublicID, "err", err)
			}
		}
		end("skipped", s.Reason+": "+s.Detail)
	}
	eff, s, err := b.reviewEffective(work, r.OrgID, r.InstallationID, pr.Repo)
	if err == nil && s != nil {
		skipped(s)
		return
	}
	var pull *githubPull
	if err == nil {
		pull, err = gh.Pull(work)
	}
	if err != nil {
		b.reviewRunError(work, lane, h, err)
		return
	}
	if pull.State != "open" {
		end("cancelled", "the pull request was closed")
		return
	}
	plan, s, err := planReview(eff, pull, opts.Types, opts.Post, opts.AllowLive)
	if err != nil {
		end("failed", err.Error())
		return
	}
	if s != nil {
		skipped(s)
		return
	}
	plan.withLabel(opts.Label)
	if !reviewTry(r) {
		// From here the run knows its channel, and every way it ends is told there (noticeRunEnded).
		h.notice = &reviewNoticeRun{eff: plan.eff, title: plan.title, base: plan.base, head: plan.head,
			sha: cmp.Or(pull.Head.SHA, r.HeadSHA), shadow: plan.post != review.ModeLive}
	}
	ck, resumed := checkpointFrom(r)
	if !reviewTry(r) {
		// The commit the check run reports on is the one reviewed: the head now, which the engine
		// reads, or for a run resumed into its post, the head its stored findings are of.
		sha := cmp.Or(pull.Head.SHA, r.HeadSHA)
		if resumed {
			sha = r.HeadSHA
		}
		h.signals = b.reviewSignalsFor(work, h, pr, plan, sha)
	}
	if resumed {
		// The model work is done and stored: post it, under the types it was done with.
		plan.keys = nil
		for _, t := range r.Types {
			plan.keys = append(plan.keys, t.Key)
		}
	} else if !reviewTry(r) {
		if noop, err := b.reviewLabelsAtClaim(work, r, pr, eff, pull, opts, &plan); err != nil {
			b.reviewRunError(work, lane, h, err)
			return
		} else if noop != "" {
			end("noop", noop)
			return
		}
	}
	specs, skippedTypes, err := resolveReviewTypes(work, b.store, r.OrgID, plan.keys)
	if err != nil {
		b.reviewRunError(work, lane, h, err)
		return
	}
	if reviewTry(r) {
		specs, skippedTypes = []reviewTypeSpec{reviewInlineSpec(opts.Inline)}, nil
	}
	var out *reviewOutcome
	if !resumed {
		if r.Trigger == "push" && pull.Head.SHA != r.HeadSHA {
			// Pushed to again while this waited out its debounce. It stands aside only for a run that
			// will review the newer head; when that push queued none — throttled, refused for the
			// money, or the same as a run already done — this one reviews the head as it is now.
			newer, err := b.store.newerReviewQueued(work, r.OrgID, pr.ID, r.ID)
			if err != nil {
				b.reviewRunError(work, lane, h, err)
				return
			}
			if newer {
				end("superseded", "the pull request was pushed to again; the newer head has a run of its own")
				return
			}
		}
		if s := b.reviewPlanGate(work, r.OrgID); s != nil {
			skipped(s)
			return
		}
		switch {
		case reviewTry(r):
		case reviewCountsTowardsPause(r.Trigger, opts.BotLabel):
			// Queued before the pull request had its fill — a burst of pushes, the catch-up beside a
			// delivery — and claimed after: it is the one past the pause.
			if s, err := b.reviewAutoPaused(work, r.OrgID, r.InstallationID, pr, r.Trigger); err != nil {
				b.reviewRunError(work, lane, h, err)
				return
			} else if s != nil {
				skipped(s)
				return
			}
		case r.Trigger == "label" && pr.Paused:
			// Paused while a person's label waited: not counted, and not run by itself either.
			skipped(&reviewSkip{"paused", "automatic reviews of this pull request are paused"})
			return
		}
		if s, err := reviewOwnKeyGate(b.settings.Get(work, r.OrgID)); err != nil {
			// The key's row could not be read: no telling yet whether it may be spent.
			b.retryOrFail(work, lane, h, err, review.FailModel)
			return
		} else if s != nil {
			skipped(s)
			return
		}
		if len(specs) == 0 {
			skipped(&reviewSkip{"types_off", "every review type this pull request would get is turned off or does not exist"})
			return
		}
		// What the run may spend now, which is what the settings say now: a max_usd raised while it
		// waited is held in full, not at what was reserved when it was queued.
		need := max(r.ReservedUSD, plan.eff.MaxUSD)
		if err := b.reviewBudgetRoom(work, r.OrgID, need, r.ID); err != nil {
			if errors.Is(err, errReviewSpendUnknown) {
				b.retryOrFail(work, lane, h, err, review.FailInternal)
				return
			}
			if !reviewTry(r) {
				b.reviewBudgetStopped(work, r.OrgID, r.InstallationID, pr, plan.post, err)
			}
			end("skipped", "budget: "+err.Error())
			return
		}
		// The throttles count this run from here: it is going to the model.
		if s, err := b.reviewThrottled(work, r.OrgID, pr, r.ID); err != nil {
			b.reviewRunError(work, lane, h, err)
			return
		} else if s != nil {
			b.auditSystem(lane, r.OrgID, "review.skipped", reviewAuditEvent(pr, r.PublicID, map[string]any{"reason": "throttle", "trigger": r.Trigger}))
			skipped(s)
			return
		}
		if need > r.ReservedUSD {
			// Held in full where every other run's check reads it, not only in this one's.
			if err := h.write(func(r *ReviewRun) error { return b.store.setReviewRunReserved(work, r, need) }); err != nil {
				b.reviewRunError(work, lane, h, err)
				return
			}
		}
		// Through the gate, the money held: the review is under way, and its channel and its pull
		// request may say so.
		b.noticeReviewStarted(lane, h, pr, specs)
		b.signalReviewStarted(lane, h, specs)
		var ok bool
		if out, ck, ok = b.reviewWork(work, lane, h, pr, pull, plan, specs, skippedTypes, opts); !ok {
			return
		}
	}
	b.publishReview(work, lane, h, pr, gh, plan, specs, ck, out)
}

// reviewLabelsAtClaim applies the label rules again at the claim, to the pull request as GitHub has it
// now, for a run with no work done yet. A label's own review is worked out afresh, as planReview
// works out every other: the label taken off while it waited, or its rule changed so it adds nothing
// beyond the branch rule's types, leaves nothing to do; and what it adds that another review of this
// head runs already, or is queued to — the opening's, delivered beside the label — is left to that
// one, the rest reviewed. The other way round, an opening, push or catch-up claimed after a label's
// own review of the same head ran leaves out the label's types it reviewed: a late "opened" delivery
// or the catch-up standing in for one would otherwise pay for them twice. noop is why a run with
// nothing left ends; it is "" when the run goes on, with plan's types narrowed.
func (b *Bot) reviewLabelsAtClaim(ctx context.Context, r *ReviewRun, pr *ReviewPR, eff review.Effective, pull *githubPull,
	opts reviewOptions, plan *reviewPlan) (noop string, err error) {
	switch r.Trigger {
	case "label":
		_, rule, _ := review.MatchRule(eff.BranchRules, pull.Base.Ref, pull.Head.Ref) // planReview matched one
		on := slices.ContainsFunc(pull.LabelNames(), func(l string) bool {
			return strings.EqualFold(strings.TrimSpace(l), strings.TrimSpace(opts.Label))
		})
		add, _ := review.LabelTypes(eff.BranchRules, pull.Base.Ref, pull.Head.Ref, []string{opts.Label}, rule.Types)
		if !on || len(add) == 0 {
			return "the label is no longer on the pull request, or no longer adds a review type", nil
		}
		covered, err := b.store.reviewTypesAtHead(ctx, r.OrgID, pr.ID, pull.Head.SHA, r.ID)
		if err != nil {
			return "", err
		}
		if add = slices.DeleteFunc(add, func(t string) bool { return slices.Contains(covered, t) }); len(add) == 0 {
			return "another review of this head runs the review types the label adds", nil
		}
		plan.keys = add
	case "open", "push", "catchup":
		if len(plan.labels) == 0 {
			return "", nil
		}
		done, err := b.store.reviewLabelTypesReviewed(ctx, r.OrgID, pr.ID, pull.Head.SHA, r.ID)
		if err != nil {
			return "", err
		}
		plan.withoutLabelTypes(eff.BranchRules, pull, done)
	}
	return "", nil
}

// reviewWork runs the engine for a claimed run and checkpoints what it found. ok false means the
// run has been ended, requeued or lost, and there is nothing to post.
func (b *Bot) reviewWork(work, lane context.Context, h *reviewHold, pr *ReviewPR, pull *githubPull, plan reviewPlan,
	specs []reviewTypeSpec, skippedTypes []review.TypeRun, opts reviewOptions) (*reviewOutcome, *reviewCheckpoint, bool) {
	r := h.run
	// What a new finding would repeat is what somebody was shown: a finding a shadow run recorded,
	// or one of a run that ended before it posted, was said to nobody on GitHub, and a live review
	// that dropped its twin as "already open" would leave the problem unsaid.
	prior, err := b.store.reviewFindingsSaid(work, r.OrgID, pr.ID, plan.post != review.ModeLive, r.ID)
	if err != nil {
		b.reviewRunError(work, lane, h, err)
		return nil, nil, false
	}
	if reviewTry(r) {
		prior = nil // a try starts from nothing (reviewTry)
	}
	configHash := plan.eff.Hash()
	r.Types, r.RuleLabel, r.ConfigHash = reviewRunTypes(specs), plan.label, configHash
	spec := reviewSpec{OrgID: r.OrgID, InstallationID: r.InstallationID, Repo: pr.Repo, PR: pr.Number, Pull: pull,
		Settings: plan.eff, Types: specs, Prior: prior, FromScratch: opts.Full}
	if opts.Scope == reviewScopeSinceLast && pr.LastReviewedSHA != "" {
		spec.LastReviewedSHA = pr.LastReviewedSHA
		json.Unmarshal([]byte(pr.FileHashes), &spec.PriorFileHashes) // written only by finishReviewRun
	}
	var cached *ReviewRun
	spec.Cached = func(out *reviewOutcome) bool {
		r.CacheKey = reviewCacheKey(out, configHash, specs, plan.post, opts.Scope, plan.labels)
		if opts.Full || reviewTry(r) {
			// Recorded all the same, so a later review of the same code is answered from this one.
			return false
		}
		hit, err := b.store.reviewCachedRun(work, r.OrgID, pr.ID, r.CacheKey, r.ID)
		if hit == nil && err == nil && opts.Scope != reviewScopeSinceLast && pr.LastReviewedSHA == out.HeadSHA {
			// The head was last reviewed by a review of what changed since the one before — a push's,
			// or a command's after a push — and that review left the pull request's findings whole:
			// the ones it held back were already open from the review before. A request for all of it
			// on that head again is the same review, and answering it from that one is what keeps
			// `@… review` asked twice from paying twice.
			hit, err = b.store.reviewCachedRun(work, r.OrgID, pr.ID, reviewCacheKey(out, configHash, specs, plan.post, reviewScopeSinceLast,
				plan.labels), r.ID)
		}
		if err != nil {
			slog.Warn("code review: cache lookup failed; reviewing", "run", r.PublicID, "err", err)
		}
		cached = hit
		return hit != nil
	}
	out, err := b.review.Run(work, spec)
	// The spend is recorded before anything else, whatever became of the run: the tokens are
	// bought, a run that failed after spending is the one whose spend most needs recording, and a
	// lane that lost its lease still spent what it spent.
	b.logReviewUsage(lane, r, pr, out)
	if out != nil {
		r.HeadSHA, r.BaseSHA = cmp.Or(out.HeadSHA, r.HeadSHA), cmp.Or(out.BaseSHA, r.BaseSHA)
	}
	switch {
	case errors.Is(err, errReviewCached):
		if b.endReviewRun(lane, h, ReviewRunResult{Status: "noop", Score: -1, Error: "already reviewed: the same commits under the same settings"}) &&
			r.Trigger == "command" && cached != nil {
			// Somebody asked on the pull request and is waiting for an answer there: the review they
			// asked for exists, and saying so is free.
			b.answerCachedCommand(lane, r, pr, cached)
		}
		return nil, nil, false
	case err != nil:
		b.reviewRunError(work, lane, h, err)
		return nil, nil, false
	case h.lost.Load() || h.cancelAsked.Load():
		b.interrupted(work, lane, h)
		return nil, nil, false
	}
	// A review that finished is checkpointed even when the instance is stopping: the checkpoint's
	// write does not ride on the work's context, and the money is spent either way. The post that
	// follows is what is left for another lane, if this one is stopped before it.
	if out.Reviewable == 0 {
		if !reviewTry(r) {
			if err := b.store.SetReviewPRSkipReason(work, r.OrgID, pr.ID, "nothing_to_review"); err != nil {
				slog.Warn("code review: skip reason not recorded", "run", r.PublicID, "err", err)
			}
		}
		b.auditSystem(lane, r.OrgID, "review.skipped", reviewAuditEvent(pr, r.PublicID, map[string]any{"reason": "nothing_to_review"}))
		b.endReviewRun(lane, h, ReviewRunResult{Status: "skipped", Score: -1, Error: "nothing_to_review: every changed file is ignored, binary or generated"})
		return nil, nil, false
	}
	ck := checkpointOf(out, skippedTypes)
	ck.Resolved = reviewResolvedOf(out, pr.Repo)
	if plan.post == review.ModeLive && !reviewTry(r) {
		b.carryResolvedAnswers(work, r, pr.ID, ck)
	}
	raw, err := json.Marshal(ck)
	if err != nil {
		b.reviewRunError(work, lane, h, err)
		return nil, nil, false
	}
	var found []*ReviewFinding
	for _, f := range out.Findings {
		found = append(found, &ReviewFinding{Finding: f.Finding, AnchorSHA: out.HeadSHA, CodeHash: f.CodeHash,
			Placement: f.Placement, Place: f.Where, Kind: f.Kind, Fingerprint: f.Fingerprint, VerifierConfidence: f.VerifierConfidence,
			Snippet: f.Snippet})
	}
	var seen []string
	for _, d := range out.Dropped {
		if d.Reason == "duplicate" && d.DuplicateOf != "" {
			seen = append(seen, d.DuplicateOf)
		}
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(lane), reviewWriteTimeout)
	defer cancel()
	if err := h.write(func(r *ReviewRun) error {
		return b.store.saveReviewOutcome(wctx, r, string(raw), found, seen, out.Resolutions)
	}); err != nil {
		if errors.Is(err, errLeaseLost) {
			slog.Warn("code review lease lost before the findings were stored; another lane has the run, and this one stores and posts nothing",
				"run", r.PublicID)
		} else {
			b.reviewRunError(work, lane, h, err)
		}
		return nil, nil, false
	}
	for _, res := range out.Resolutions {
		if res.StatusApplied {
			b.auditFindingChange(lane, r.OrgID, pr, res.F, string(res.F.Status), string(res.Status), reviewResolvedBy)
		}
		// Once a finding a day, as the thread path's kept findings are: the same claim is checked
		// again at every push.
		if res.Kept != "" && b.store.AlertOnce(lane, r.OrgID, "review-check-kept:"+res.F.PublicID, 24*time.Hour) {
			b.auditSystem(lane, r.OrgID, "review.finding_kept", reviewAuditEvent(pr, r.PublicID, map[string]any{
				"finding": res.F.PublicID, "title": res.F.Title, "severity": string(res.F.Severity), "status": string(res.F.Status),
				"by": reviewResolvedBy, "why": res.Kept}))
		}
	}
	return out, ck, true
}

// reviewCarryRuns is how many of a pull request's latest runs carryResolvedAnswers reads.
const reviewCarryRuns = 10

// carryResolvedAnswers adds to a live run's checkpoint the thread answers earlier reviews decided on
// and never posted, for its poster to post. The findings were closed, opened again or had their claim
// refuted with that run's checkpoint, and nothing else would ever say so in their threads: the run
// was superseded, failed or cancelled after storing them, or GitHub failed the answer. Only the
// newest word on each finding counts — a later run that posted it, or decided otherwise about it,
// settles it — and a shadow run's are never carried: shadow says nothing on GitHub, then or later.
// What carried answers say still stands is checked again when they are posted.
func (b *Bot) carryResolvedAnswers(ctx context.Context, r *ReviewRun, prID int64, ck *reviewCheckpoint) {
	runs, err := b.store.ReviewRunsForPR(ctx, r.OrgID, prID, reviewCarryRuns)
	if err != nil {
		slog.Warn("code review: earlier runs not read; their unposted answers wait for the next review", "run", r.PublicID, "err", err)
		return
	}
	seen := map[string]bool{}
	for _, e := range ck.Resolved {
		seen[e.Finding] = true
	}
	for _, run := range runs { // newest first
		if run.ID >= r.ID || run.Kind != "review" || !slices.Contains(reviewRunEnds, run.Status) || run.Status == "shadow" {
			continue
		}
		old, ok := checkpointFrom(run)
		if !ok {
			continue
		}
		for _, e := range old.Resolved {
			if seen[e.Finding] {
				continue
			}
			seen[e.Finding] = true
			if e.Reply != "" && !e.Replied {
				e.Carried = cmp.Or(e.Carried, run.PublicID)
				ck.Resolved = append(ck.Resolved, e)
			}
		}
	}
}

// logReviewUsage records what a run's model calls cost, filed under the repository and the pull
// request so Activity names it, and charged to the pull request's author as "github:<login>": a
// GitHub user is not a member of anything here, and must never become a seat.
func (b *Bot) logReviewUsage(lane context.Context, r *ReviewRun, pr *ReviewPR, out *reviewOutcome) {
	if out == nil {
		return
	}
	b.logReviewSpend(lane, r, pr, pr.AuthorLogin, out.Usage)
}

// logReviewSpend records usage by model under the pull request, charged to login: the author for a
// review, whoever replied for an answer in a thread — the one whose words it was spent on.
func (b *Bot) logReviewSpend(lane context.Context, r *ReviewRun, pr *ReviewPR, login string, usage map[string]Usage) {
	models := make([]string, 0, len(usage))
	for m := range usage {
		models = append(models, m)
	}
	slices.Sort(models)
	for _, m := range models {
		u := usage[m]
		if u.In == 0 && u.Out == 0 && u.CostUSD == 0 {
			continue
		}
		b.store.LogUsageBy(lane, r.OrgID, "", "github:"+strings.ToLower(pr.Repo), fmt.Sprintf("pr:%d", pr.Number),
			"github:"+login, m, u)
	}
}

// reviewRunError ends or requeues a run on an error, by its kind: GitHub asking for a wait puts the
// run back for when it said, with no attempt spent; anything else fails the run, with the kind of
// failure the summary may name, while its error text stays in the console. A model failure is also
// the admins' to hear about, once an hour: the engine has already tried the provider twice, and a
// key at its limit (402) fails every pull request's review the same way until somebody acts.
func (b *Bot) reviewRunError(work, lane context.Context, h *reviewHold, err error) {
	if b.interrupted(work, lane, h) {
		return
	}
	var wait *githubRetryError
	if errors.As(err, &wait) {
		b.requeueReview(lane, h, time.Now().Add(max(wait.Wait, time.Second)), err.Error(), false)
		return
	}
	reason := review.FailInternal
	var rf *reviewFailure
	if errors.As(err, &rf) {
		reason = rf.Reason
	}
	if !b.endReviewRun(lane, h, ReviewRunResult{Status: "failed", Score: -1, Error: string(reason) + ": " + err.Error()}) {
		return
	}
	if reason == review.FailModel && b.agent != nil {
		r := h.run
		b.agent.alert(lane, r.OrgID, "review-model", fmt.Sprintf(":warning: Code review of %s#%d failed: the model did not answer (%s). "+
			"Reviews will fail the same way until the model key or the provider is put right; the run is in the console.",
			r.Repo, r.PRNumber, truncate(err.Error(), 300)))
	}
}

// retryOrFail puts a run back to be tried a minute later while it has attempts left, and fails it
// with reason when it has none: for what another attempt may not hit — GitHub refusing a post,
// the spend that could not be read.
func (b *Bot) retryOrFail(work, lane context.Context, h *reviewHold, err error, reason review.FailReason) {
	if b.interrupted(work, lane, h) || errors.Is(err, errLeaseLost) {
		return
	}
	if h.run.Attempts < reviewRunMaxAttempts {
		slog.Warn("code review run failed; trying again", "run", h.run.PublicID, "attempt", h.run.Attempts, "err", err)
		b.requeueReview(lane, h, time.Now().Add(time.Minute), err.Error(), true)
		return
	}
	b.endReviewRun(lane, h, ReviewRunResult{Status: "failed", Score: -1, Error: string(reason) + ": " + err.Error()})
}
