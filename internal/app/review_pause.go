package app

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"attesttag/internal/review"
)

// What a push brings to the pause on automatic reviews (reviewAutoPaused). The ceiling is there for
// a branch pushed to all day, and two kinds of push are not that:
//
//   - one that leaves the pull request's own diff as its last review read it — every changed file's
//     added and removed lines the same, review.PatchHash — which is how merging the base branch in,
//     or a rebase, looks. It is not reviewed, since there is nothing new of the pull request's to
//     read, and it is not counted: a busy base branch must not use up a pull request's reviews.
//   - one after somebody with access to the pull request said in a finding's thread that it is fixed
//     ("fixed in <sha>", "fixed", "addressed"): the review is how that claim gets checked, so while the
//     ceiling holds the pause, each such claim lets one push through. A claim that comes after the
//     push it is about — push, then reply — queues that review itself (reviewClaimResume). The review
//     carries the claim (reviewOptions.Claim), which is what spends it, and is not counted either.

// reviewSkipUnchanged is the gate's reason for a push that left the pull request's own diff alone.
const reviewSkipUnchanged = "unchanged_diff"

func reviewUnchangedSkip() *reviewSkip {
	return &reviewSkip{reviewSkipUnchanged, "this push left the pull request's own changes as they were when last reviewed " +
		"(the base branch merged in, or a rebase), so there is nothing new to review"}
}

// reviewPauseCeiling is the settings' auto_pause_after for a repository, for what a command says of
// it: the built-in one when the settings cannot be read.
func (b *Bot) reviewPauseCeiling(ctx context.Context, orgID, installationID int64, repo string) int {
	eff, s, err := b.reviewEffective(ctx, orgID, installationID, repo)
	if err != nil || s != nil {
		return reviewAutoPauseAfter
	}
	return eff.AutoPause()
}

// reviewNotPausedText and reviewResumedText are the answers to `@… resume`, under a ceiling of after
// automatic reviews, 0 being none.
func reviewNotPausedText(after int) string {
	if after == 0 {
		return "Automatic reviews of this pull request were not paused, and do not pause by themselves on this repository."
	}
	return fmt.Sprintf("Automatic reviews of this pull request were not paused. They pause by themselves after %d; "+
		"the count starts again from now.", after)
}

func reviewResumedText(after int, slug string) string {
	if after == 0 {
		return fmt.Sprintf("Resumed: this pull request is reviewed by itself again. `@%s review` reviews the head now.", slug)
	}
	return fmt.Sprintf("Resumed: this pull request is reviewed by itself again, up to %d more times before they pause. "+
		"`@%s review` reviews the head now.", after, slug)
}

// reviewDiffUnchanged reports whether the pull request's changed files are, file for file, what its
// last review read: the same files, each with the same added and removed lines. A file with no patch
// to compare — a binary one, a diff GitHub would not show — or a list GitHub cut short cannot be told
// apart, and counts as changed.
func (b *Bot) reviewDiffUnchanged(ctx context.Context, gh *reviewGitHub, pr *ReviewPR) (bool, error) {
	if pr.LastReviewedSHA == "" {
		return false, nil
	}
	var was map[string]string
	if json.Unmarshal([]byte(pr.FileHashes), &was) != nil || len(was) == 0 { // written only by finishReviewRun
		return false, nil
	}
	files, err := gh.PullFiles(ctx)
	if err != nil {
		return false, err
	}
	if files.Truncated || len(files.Files) != len(was) {
		return false, nil
	}
	for _, f := range files.Files {
		if h := review.PatchHash(f); h == "" || was[f.Path] != h {
			return false, nil
		}
	}
	return true, nil
}

// reviewClaimReplies is how many of a pull request's latest answered replies reviewFixClaim reads.
const reviewClaimReplies = 20

// reviewPublicIDShape is a public id (newPublicID): what a run's claim names, and what a pattern built
// from one may hold.
var reviewPublicIDShape = regexp.MustCompile(`^[0-9a-f]{32}$`)

// reviewClaim is a claim of a fix that may let a push through: the reply run that recorded it, and
// who made it.
type reviewClaim struct {
	run, login string
}

// reviewFixClaim is a fix claimed in one of the pull request's finding threads that no review has
// carried yet, by somebody with access to it: a member or collaborator of the repository, the
// author of a pull request from the repository's own branches, or on a private repository anybody
// who can comment at all. The claim must still stand on its finding — still open, still claimed —
// so a claim the next review already settled (closed as fixed, or found still there, which clears
// it) lets nothing through. Zero when there is none.
func (b *Bot) reviewFixClaim(ctx context.Context, orgID int64, pr *ReviewPR) (reviewClaim, error) {
	runs, err := b.store.reviewReplyRuns(ctx, orgID, pr.ID, reviewClaimReplies)
	if err != nil {
		return reviewClaim{}, err
	}
	for _, rr := range runs { // newest first
		ck, ok := replyCheckpointFrom(rr)
		if !ok || !ck.Applied || ck.Outcome != "claimed" {
			continue
		}
		var req reviewReplyRequest
		json.Unmarshal([]byte(rr.RequestJSON), &req) // written only by queueReviewReply
		login := cmp.Or(ck.Login, req.Login)
		author := login != "" && strings.EqualFold(login, pr.AuthorLogin) && !pr.IsFork
		if !reviewMember(req.Association) && !author && !pr.IsPrivate {
			continue
		}
		f, err := b.store.ReviewFindingByPublicID(ctx, orgID, pr.ID, req.Finding)
		if err != nil {
			return reviewClaim{}, err
		}
		if f == nil || f.ClaimedFixedSHA == "" || (f.Status != review.FindingOpen && f.Status != review.FindingDisputed) {
			continue
		}
		taken, err := b.store.reviewClaimTaken(ctx, orgID, pr.ID, rr.PublicID)
		if err != nil {
			return reviewClaim{}, err
		}
		if !taken {
			return reviewClaim{run: rr.PublicID, login: login}, nil
		}
	}
	return reviewClaim{}, nil
}

// carryClaim records on a claimed run the fix claim that let it through the pause, fenced on its
// lease like every write the lane makes for it.
func (b *Bot) carryClaim(ctx context.Context, h *reviewHold, opts reviewOptions) error {
	raw, err := json.Marshal(opts)
	if err != nil {
		return err
	}
	return h.write(func(r *ReviewRun) error {
		if err := b.store.setReviewRunRequest(ctx, r, string(raw)); err != nil {
			return err
		}
		r.RequestJSON = string(raw)
		return nil
	})
}

// reviewClaimResume queues the review a fix claimed in a finding's thread is waiting for, when the
// push it is about came first: the ceiling paused the pull request's automatic reviews, its head has
// moved past the last one reviewed, and a claim no review has carried stands. It is asked when the
// summary follows a reply that changed a finding, and queues a push's review of the head, which the
// gate then lets through on that claim; the debounce leaves room for the push still to come.
func (b *Bot) reviewClaimResume(ctx context.Context, r *ReviewRun, pr *ReviewPR) {
	if !pr.Paused || !pr.PausedAuto || pr.State != "open" || !commitSHA.MatchString(pr.HeadSHA) || pr.HeadSHA == pr.LastReviewedSHA {
		return
	}
	c, err := b.reviewFixClaim(ctx, r.OrgID, pr)
	if err != nil || c.run == "" {
		if err != nil {
			slog.Warn("code review: fix claims not read; the next push asks again", "org", r.OrgID, "pr", pr.ID, "err", err)
		}
		return
	}
	req := reviewRequest{InstallationID: r.InstallationID, Trigger: "push", TriggerRef: "claim:" + c.run,
		NotBefore: time.Now().Add(reviewPushDebounce)}
	if c.login != "" {
		req.RequestedBy = "github:" + c.login
	}
	_, err = b.enqueueReview(ctx, r.OrgID, pr.Repo, pr.Number, req)
	var skip *reviewSkip
	if err != nil && !errors.As(err, &skip) {
		slog.Warn("code review: the review a fix claim asked for was not queued; the next push asks again", "org", r.OrgID,
			"pr", pr.ID, "err", err)
	}
}
