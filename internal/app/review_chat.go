package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"attesttag/internal/review"
)

// A review asked for in a channel: "@bot review acme/web#12", which the model turns into a call to
// github_start_review. It is the console's Start review (startReview in review_api.go) with the
// channel standing where the console's permissions stood, and the channel's own access is what
// decides it. The repository has to be one this channel is granted in Access bundles — the list
// every other github_ tool reads from (repoTargets) — so a channel can have reviewed only what it
// could already read; a repository it is not granted is refused by name, and nothing is queued.
//
// Beyond that it is held to what a review asked for anywhere is held to: code review on, the
// organisation's plan, the repository's App connection in the review tree, the money and the
// throttles (enqueueReview). The repository's own mode decides where the result goes. Somebody in a
// channel cannot make a shadow repository post live; they may ask for a review recorded in shadow
// on a live one. A review starts only when a person typed the request: not on a turn a forwarded
// mail or a routine started, and not in the console's Playground, which never acts for real.

// reviewFromChat is Agent.startReview: it queues the review and says so, or says why not, in words
// the model passes on. A refusal is an error, so the model does not report it as started.
func (b *Bot) reviewFromChat(ctx context.Context, c *Call, repoArg string, pr int, typesArg []string, shadow bool) (string, error) {
	switch {
	case c == nil || c.OrgID == 0:
		return "", errors.New("code review is started from a channel of a connected workspace")
	case c.offline():
		return "", errors.New("the Playground does not start real reviews: ask in the channel itself")
	case !c.HumanTurn:
		return "", errors.New("a review is started only when somebody in the channel asks for one, not on a turn a forwarded mail or a routine started")
	case b.cfg.codeReviewOff():
		return "", errors.New("code review is not on in this deployment")
	case b.review == nil || b.proxy == nil || !b.reviewApp().configured():
		return "", errors.New("code review is not set up in this deployment: it needs the GitHub App")
	case pr <= 0:
		return "", errors.New("say which pull request: its number")
	}
	if c.Access == nil {
		return "", errors.New("no repositories are connected in this channel. An admin adds one in the console under Access bundles › Repositories")
	}
	// The channel's grant first, before anything about the organisation's review set-up is said:
	// a channel that cannot read a repository learns nothing here about how it is reviewed.
	conn, err := repoConnection(c, repoArg)
	if err != nil {
		return "", err
	}
	repo, err := reviewRepoName(conn.Repo)
	if err != nil {
		return "", err
	}
	if s := b.reviewPlanGate(ctx, c.OrgID); s != nil {
		return "", errors.New(s.Detail)
	}
	t, err := b.reviewTreeIndex(ctx, c.OrgID)
	if err != nil {
		return "", err
	}
	installation := t.installationFor(repo)
	if installation == 0 {
		return "", fmt.Errorf("%s is connected in this channel but is not reviewed: an admin adds its GitHub App connection under Automation › Reviews in the console", repo)
	}
	types, err := b.chatReviewTypes(ctx, c.OrgID, typesArg)
	if err != nil {
		return "", err
	}
	gh, err := b.reviewClient(c.OrgID, installation, repo, pr)
	if err != nil {
		return "", err
	}
	pull, err := gh.Pull(ctx)
	if err != nil {
		var api *githubAPIError
		if errors.As(err, &api) && api.Status == 404 {
			return "", fmt.Errorf("GitHub has no pull request %s#%d, or the App cannot see it", repo, pr)
		}
		return "", err
	}
	if pull.Number == 0 {
		pull.Number = pr
	}
	var post review.Mode
	if shadow {
		post = review.ModeShadow
	}
	where := fmt.Sprintf("%s#%d", repo, pr)
	if last, ok := b.reviewSameState(ctx, c.OrgID, installation, repo, pull, types, post, false); ok {
		score := "no score"
		if last.Score >= 0 {
			score = fmt.Sprintf("confidence %d/5", last.Score)
		}
		out := fmt.Sprintf("%s was already reviewed at %s with these types and settings (%s), so nothing new was queued: "+
			"push a new commit for a fresh review, or ask with other types.", where, shortSHA(last.HeadSHA), score)
		if u := b.reviewConsoleURL(ctx, last.PublicID); u != "" {
			out += " That review: " + u
		}
		return out, nil
	}
	// One request per person per thread: the model calling twice in a turn, or somebody asking again
	// for the same head, is the run already queued (the dedupe key holds the head and the types too).
	ref := "chat:" + c.TeamID + ":" + c.Channel + ":" + cmp.Or(c.ThreadTS, c.MessageTS, newPublicID()) + ":" + c.UserID
	run, err := b.enqueueReview(ctx, c.OrgID, repo, pr, reviewRequest{InstallationID: installation, Trigger: "chat",
		TriggerRef: ref, RequestedBy: "chat:" + c.TeamID + "/" + c.UserID, Types: types, Post: post,
		BypassFilters: true, Pull: pull})
	var skip *reviewSkip
	switch {
	case errors.As(err, &skip):
		return "", fmt.Errorf("the review of %s did not start: %s", where, skip.Detail)
	case errors.Is(err, errReviewLiveRefused):
		return "", fmt.Errorf("the review of %s did not start: %w", where, err)
	case err != nil:
		return "", err
	}
	b.auditChatReview(ctx, c, repo, pr, run, pull.Head.SHA, types, string(post))
	return b.chatReviewQueued(ctx, c.OrgID, installation, repo, pull, run, types, post), nil
}

// chatReviewQueued says what was queued: which types under which rule and where the result will go,
// as the gate just planned it, and where to follow it.
func (b *Bot) chatReviewQueued(ctx context.Context, orgID, installation int64, repo string, pull *githubPull, run *ReviewRun,
	types []string, post review.Mode) string {
	what, dest := "", "posted on the pull request"
	if eff, s, err := b.reviewEffective(ctx, orgID, installation, repo); err == nil && s == nil {
		if plan, s, err := planReview(eff, pull, types, post, false); err == nil && s == nil {
			what = " for " + strings.Join(plan.keys, ", ")
			if plan.post != review.ModeLive {
				dest = "recorded in shadow in the console; nothing is posted on GitHub"
			}
		}
	}
	out := fmt.Sprintf("Queued a review of %s#%d (%s) at %s%s. When it is done it will be %s. https://github.com/%s/pull/%d",
		repo, pull.Number, pull.Title, shortSHA(pull.Head.SHA), what, dest, repo, pull.Number)
	if u := b.reviewConsoleURL(ctx, run.PublicID); u != "" {
		out += " · In the console: " + u
	}
	return out
}

// chatReviewTypes is the review types somebody named, as keys, each one checked to exist and be on.
// The model may pass a type's name ("Security") as readily as its key ("security").
func (b *Bot) chatReviewTypes(ctx context.Context, orgID int64, named []string) ([]string, error) {
	if len(named) == 0 {
		return nil, nil
	}
	keys, err := b.reviewTypeKeys(ctx, orgID)
	if err != nil {
		return nil, err
	}
	var out, unknown []string
	for _, n := range named {
		k := strings.ToLower(strings.TrimSpace(n))
		k = strings.ReplaceAll(k, " ", "-")
		switch {
		case k == "":
		case slices.Contains(keys, k):
			if !slices.Contains(out, k) {
				out = append(out, k)
			}
		default:
			unknown = append(unknown, n)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("no review type %s here, or it is turned off; the ones that are on: %s",
			strings.Join(unknown, ", "), strings.Join(keys, ", "))
	}
	return out, nil
}

// auditChatReview records the review a person asked for in a channel, as the console's start is
// recorded (review.started), with the person's chat id as the actor.
func (b *Bot) auditChatReview(ctx context.Context, c *Call, repo string, pr int, run *ReviewRun, head string, types []string, post string) {
	e := AuditEvent{TargetKind: "pull_request", TargetID: fmt.Sprintf("%s#%d", repo, pr),
		TargetName: fmt.Sprintf("%s#%d", repo, pr), Details: auditDetails(map[string]any{"run": run.PublicID, "head": head,
			"types": types, "post": post, "channel": c.Channel, "via": "chat"})}
	if c.SL != nil {
		b.auditSlack(ctx, c.SL, c.UserID, "review.started", e)
		return
	}
	e.ActorName = c.UserID
	b.auditSystem(ctx, c.OrgID, "review.started", e)
}
