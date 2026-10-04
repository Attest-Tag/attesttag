package app

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
)

// The catch-up: pull requests GitHub told this deployment about while it was not listening.
//
// GitHub never redelivers a webhook by itself. A delivery that met a deployment being replaced, a
// database having a bad minute or a full inbox is listed under the App's Recent Deliveries for
// somebody to press Redeliver on, and is otherwise gone — and for a pull request that opened then,
// that is a review nobody gets and nobody hears is missing. So every ten minutes, behind the leader
// lease, the deployment asks GitHub instead: for each repository an organisation reviews, its open
// pull requests, most recently updated first, against what the reviewer knows of them.
//
//   - A pull request opened after the repository started being reviewed, which has never had a
//     review run, is what its "opened" delivery would have been. It goes through the gate as one,
//     under the trigger catchup and with the opening's dedupe key (reviewKeyTrigger), so a delivery
//     that does arrive — still in the inbox, or redelivered by hand — finds the run and queues no
//     second one. "Started being reviewed" is the newest change on the repository's chain of
//     settings (reviewChainSince): turning a repository on is never a reason to review the pull
//     requests already open on it, which no delivery would have reviewed either.
//   - A pull request whose head is not the head stored for it was pushed to. It is what a
//     synchronize delivery would have been (reviewHeadMoved): a posted summary's footer says the
//     head moved, for nothing, and a repository reviewed on every push gets its debounced review.
//
// It never resurrects a pull request the gate turned away on purpose. One with a skip reason on the
// head it still has — a bot's, an excluded author's, one the money stopped — is left alone, unless
// the reason was a fact the missed delivery would have changed: a draft since made ready, a pull
// request closed and since reopened (reviewSkipLapsed).
//
// Each organisation gets a share of each pass — reviewCatchupReposPerOrg listings and
// reviewCatchupPRsPerOrg pull requests acted on — so one with a hundred repositories, or a backlog
// after a long outage, neither runs its installation out of GitHub's rate limit nor holds up the
// next organisation; what is left over is logged and waits for the next pass. It runs only beside
// the webhook: on a deployment with no secret nothing is reviewed automatically, and a pass every
// ten minutes would make it so by the back door. And it recovers pull requests only. A command or a
// reply written while the deployment was down stays unanswered, and its author can write it again.

const (
	reviewCatchupEvery = 10 * time.Minute
	// The first pass after an instance takes the lease waits this long, so what the inbox already
	// holds from before a restart is dispatched first: the cheap path, which lists nothing.
	reviewCatchupFirst = time.Minute
	// One page of a listing each, which is a GitHub request and a token per repository.
	reviewCatchupReposPerOrg = 40
	// Pull requests acted on — put through the gate, or treated as pushed to — per organisation.
	reviewCatchupPRsPerOrg = 50
)

// reviewCatchupOn reports whether this process runs the catch-up: what the lane needs, and the
// webhook, without which nothing is reviewed as it opens and so nothing can have been missed.
func (b *Bot) reviewCatchupOn() bool { return b.reviewLaneOn() && b.ghHook.configured() }

// reviewCatchupLoop runs a pass every reviewCatchupEvery for as long as this instance holds the
// "review-catchup" lease (bot.go).
func (b *Bot) reviewCatchupLoop(ctx context.Context) {
	wait := reviewCatchupFirst
	for pass := 0; ; pass++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = reviewCatchupEvery
		b.reviewCatchup(ctx, pass)
	}
}

// reviewCatchup is one pass over every reviewed connection, an organisation at a time. pass turns
// which of an organisation's repositories are listed first when it has more than its share, so none
// is left out of every pass.
func (b *Bot) reviewCatchup(ctx context.Context, pass int) {
	conns, err := b.store.reviewCatchupConnections(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("code review catch-up: the reviewed connections could not be read", "err", err)
		}
		return
	}
	for len(conns) > 0 && ctx.Err() == nil {
		n := 1
		for n < len(conns) && conns[n].OrgID == conns[0].OrgID {
			n++
		}
		b.reviewCatchupOrg(ctx, conns[0].OrgID, conns[:n], pass)
		conns = conns[n:]
	}
}

// reviewCatchupRepo is one repository the catch-up lists, and the installation that reaches it.
type reviewCatchupRepo struct {
	installation int64
	repo         string
}

// reviewCatchupTally is what one organisation's pass came to, for the log.
type reviewCatchupTally struct {
	listed, queued, skipped, moved, failed, deferred, unlisted int
}

func (b *Bot) reviewCatchupOrg(ctx context.Context, orgID int64, conns []reviewCatchupConn, pass int) {
	// An organisation whose plan has no code review is passed over before anything is listed: the
	// gate would skip every pull request it found, and the listing is GitHub's rate limit spent on
	// nothing. That passes over the free footer re-render for a push it missed as well, which is left
	// to the next push's delivery or to the pass after the plan is back (review_plan.go).
	if b.reviewPlanGate(ctx, orgID) != nil {
		return
	}
	repos, err := b.reviewCatchupRepos(ctx, orgID, conns)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("code review catch-up: the repositories could not be read", "org", orgID, "err", err)
		}
		return
	}
	var t reviewCatchupTally
	if len(repos) > reviewCatchupReposPerOrg {
		start := pass * reviewCatchupReposPerOrg % len(repos)
		repos = slices.Concat(repos[start:], repos[:start])
		t.unlisted = len(repos) - reviewCatchupReposPerOrg
		repos = repos[:reviewCatchupReposPerOrg]
	}
	left := reviewCatchupPRsPerOrg
	for i, r := range repos {
		if ctx.Err() != nil {
			return
		}
		if stop := b.reviewCatchupRepo(ctx, orgID, r, &left, &t); stop {
			t.unlisted += len(repos) - i - 1
			break
		}
	}
	if t.queued+t.skipped+t.moved+t.failed > 0 {
		slog.Info("code review catch-up", "org", orgID, "repos_listed", t.listed, "queued", t.queued,
			"skipped_by_the_gate", t.skipped, "pushed_to", t.moved, "failed", t.failed)
	}
	if t.deferred+t.unlisted > 0 {
		slog.Info("code review catch-up stopped at this organisation's share of the pass; the rest wait for the next one",
			"org", orgID, "pull_requests_deferred", t.deferred, "repos_not_listed", t.unlisted)
	}
}

// reviewCatchupRepos is every repository the catch-up looks at for an organisation, with the
// installation that reaches it: the ones its App connections name, the ones somebody set something
// on under a connection, and the ones a review has run on through an installation — which is how a
// repository the App reaches without anybody having connected it is known at all. Only installations
// in conns, which are reviewed and active, are kept; the order is stable, for the rotation.
func (b *Bot) reviewCatchupRepos(ctx context.Context, orgID int64, conns []reviewCatchupConn) ([]reviewCatchupRepo, error) {
	reviewed := map[int64]bool{}
	for _, c := range conns {
		reviewed[c.InstallationID] = true
	}
	var out []reviewCatchupRepo
	add := func(installation int64, repo string) {
		r := reviewCatchupRepo{installation, strings.ToLower(strings.TrimSpace(repo))}
		if reviewed[installation] && validGitHubRepo(r.repo) && !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	all, err := b.store.AllConnections(ctx, orgID)
	if err != nil {
		return nil, err
	}
	for _, c := range all {
		if c.CredType == "github_app" {
			add(c.GitHubInstallationID, c.Repo)
		}
	}
	tree, err := b.store.ReviewSettingsTree(ctx, orgID)
	if err != nil {
		return nil, err
	}
	byID := map[int64]*ReviewSetting{}
	for _, n := range tree {
		byID[n.ID] = n
	}
	for _, n := range tree {
		if n.Kind != reviewKindRepo {
			continue
		}
		p := byID[n.ParentID]
		if p != nil && p.Kind == reviewKindGroup {
			p = byID[p.ParentID]
		}
		if p != nil && p.Kind == reviewKindConnection {
			add(p.InstallationID, n.Repo)
		}
	}
	for _, c := range conns {
		repos, err := b.store.reviewReposOfInstallation(ctx, orgID, c.InstallationID)
		if err != nil {
			return nil, err
		}
		for _, repo := range repos {
			add(c.InstallationID, repo)
		}
	}
	slices.SortFunc(out, func(a, b reviewCatchupRepo) int {
		return cmp.Or(strings.Compare(a.repo, b.repo), cmp.Compare(a.installation, b.installation))
	})
	return out, nil
}

// reviewCatchupRepo compares one repository's open pull requests with what is stored of them and
// acts on what the deployment missed, while left lasts. It reports whether the organisation's pass
// should stop here: GitHub asked for a wait, which every later listing through the App would meet.
func (b *Bot) reviewCatchupRepo(ctx context.Context, orgID int64, r reviewCatchupRepo, left *int, t *reviewCatchupTally) bool {
	warn := func(what string, err error) {
		if ctx.Err() == nil {
			slog.Warn("code review catch-up: "+what, "org", orgID, "repo", r.repo, "installation", r.installation, "err", err)
		}
	}
	if _, s, err := b.reviewEffective(ctx, orgID, r.installation, r.repo); err != nil {
		warn("the settings could not be read", err)
		return false
	} else if s != nil {
		return false // off, or reached through another installation: nothing here was missed
	}
	chain, err := b.store.ReviewSettingsChain(ctx, orgID, r.installation, r.repo)
	if err != nil {
		warn("the settings could not be read", err)
		return false
	}
	since, ok := reviewChainSince(chain)
	if !ok {
		warn("the settings carry no time they changed", errors.New("unreadable updated_at"))
		return false
	}
	pulls, _, err := b.reviewOpenPulls(ctx, orgID, r.installation, r.repo, 1)
	if err != nil {
		var wait *githubRetryError
		var api *githubAPIError
		var mint *installTokenError
		switch {
		case errors.As(err, &wait):
			slog.Info("code review catch-up: GitHub asked for a wait; this organisation's pass stops here", "org", orgID,
				"repo", r.repo, "wait", wait.Wait)
			return true
		case errors.As(err, &api) && api.Status == http.StatusNotFound, errors.As(err, &mint) && mint.status == http.StatusUnprocessableEntity:
			// A repository the installation no longer reaches — taken out of its selection, renamed,
			// deleted — which a review ran on once. Asked about every pass; not news every pass.
			slog.Debug("code review catch-up: the installation does not reach this repository", "org", orgID, "repo", r.repo, "err", err)
		default:
			warn("the open pull requests could not be listed", err)
		}
		return false
	}
	t.listed++
	prs, err := b.store.ReviewPRsOfRepo(ctx, orgID, r.repo)
	if err != nil {
		warn("the stored pull requests could not be read", err)
		return false
	}
	runs, err := b.store.latestReviewRunsOfRepo(ctx, orgID, r.repo)
	if err != nil {
		warn("the stored reviews could not be read", err)
		return false
	}
	for _, p := range pulls {
		if ctx.Err() != nil {
			return true
		}
		row := prs[p.Number]
		act := reviewCatchupAction(row, row != nil && runs[row.ID] != nil, p, since)
		if act == "" {
			continue
		}
		if *left <= 0 {
			t.deferred++
			continue
		}
		*left--
		req := reviewRequest{InstallationID: r.installation, TriggerRef: "catchup", RequestedBy: "github:" + p.User.Login, Pull: p.pull()}
		if act == "push" {
			if err := b.reviewHeadMoved(ctx, orgID, r.repo, req); err != nil {
				t.failed++
				warn("a push it missed could not be recorded", err)
			} else {
				t.moved++
			}
			continue
		}
		req.Trigger = "catchup"
		_, err := b.enqueueReview(ctx, orgID, r.repo, p.Number, req)
		var skip *reviewSkip
		switch {
		case errors.As(err, &skip):
			t.skipped++ // the reason is on the pull request, as an opening's would be
		case err != nil:
			t.failed++
			warn("a pull request it missed could not be queued", err)
		default:
			t.queued++
		}
	}
	return false
}

// reviewCatchupAction is what the catch-up does about one open pull request, given what is stored
// of it and whether it has ever had a review run: "open" to put it through the gate as its opening
// would have, "push" to treat it as pushed to, "" for nothing.
func reviewCatchupAction(row *ReviewPR, reviewed bool, p githubPullItem, since time.Time) string {
	if !reviewed && (row == nil || row.SkipReason == "" || reviewSkipLapsed(row.SkipReason, p)) {
		// A time GitHub wrote that does not read as one is never "after": the side to err on is the
		// review nobody asked for not happening.
		if opened, err := time.Parse(time.RFC3339, p.CreatedAt); err == nil && !opened.Before(since) {
			return "open"
		}
	}
	if row != nil && row.HeadSHA != "" && commitSHA.MatchString(p.Head.SHA) && !strings.EqualFold(row.HeadSHA, p.Head.SHA) {
		return "push"
	}
	return ""
}

// reviewSkipLapsed reports whether the gate's reason for not reviewing a pull request was a fact it
// has since stopped being — a draft made ready, a pull request closed and opened again — which is
// what the delivery the catch-up stands in for would have said. Every other reason was the gate's
// judgement of the pull request as it still is.
func reviewSkipLapsed(reason string, p githubPullItem) bool {
	switch reason {
	case "draft":
		return !p.Draft
	case "closed":
		return true // it is in the list of open ones
	}
	return false
}

// reviewChainSince is when a repository started being reviewed as it is now, as near as what is
// stored can say: the newest change on its chain of settings. Every write that can turn a repository
// on marks a node of that chain as updated — adding or restoring its connection, a mode set at any
// level, putting it in a group or taking it out, removing its own row (DeleteReviewRepo). So do
// writes that turn nothing on, like a new instruction, and they move the line later than it need
// be: a pull request missed in the minutes before somebody edited the settings is then left for a
// person to ask for. That is the side to err on; the other one reviews pull requests nobody asked
// to have reviewed.
func reviewChainSince(chain []*ReviewSetting) (time.Time, bool) {
	var since time.Time
	for _, n := range chain {
		at, err := time.Parse(time.DateTime, n.UpdatedAt)
		if err != nil {
			return time.Time{}, false
		}
		if at.After(since) {
			since = at
		}
	}
	return since, len(chain) > 0
}
