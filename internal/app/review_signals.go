package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"attesttag/internal/review"
)

// A live review's signals on its pull request, beside the review itself: what GitHub shows somebody
// glancing at the pull request, without opening anything, about a review that is under way or done.
//
//   - A reaction on the pull request: eyes while a review of it runs, a rocket once the review is
//     posted. GitHub has no tick among its reactions, so the rocket is the "done" and the check run
//     below is the tick. A run that ends any other way only takes the eyes off, and the check says
//     how it ended. A review of a pull request reviewed before takes the last review's rocket off as
//     it starts, so the two never show at once.
//   - A check run, "attest_tag review", among the pull request's checks: in progress while the review
//     runs, then completed — success once it is posted, with the score and the open findings, and
//     neutral, skipped or cancelled when it is not. Never failure: like the review, the check is
//     advisory, a failed check reads as the code failing, and it would block a merge wherever
//     somebody made it required. GitHub counts success, neutral and skipped as passing.
//
// Both are Go's, from what the database holds, and both are best effort: a signal GitHub refuses is
// logged and the review goes on, since a review that did not get its eyes is still a review. Neither
// is made in shadow, which writes nothing to GitHub at all, nor for a try, which is not the pull
// request's review. The start's signals are made beside the work, never in front of it, as its
// announcement in a chat channel is (noticeReviewStarted), and the end's wait for them, so the eyes
// never go back on after the review is done.
//
// The check run needs the App's Checks permission, which an App made for fix jobs has not got and
// the rest of code review does not need. An installation that has not granted it gets the reactions
// and no check run (reviewChecksGranted), and GitHub is asked for nothing it would refuse: the check
// run's token is minted for checks alone (githubPurposeReviewChecks), so asking for one the
// installation lacks could only ever cost the check.
//
// A run put back — GitHub asking for a wait, the instance stopping — leaves its signals up for the
// attempt that picks it up, which finds them rather than putting up a second set: the check run by
// the run it belongs to, whose id it carries as its external id, and the eyes by putting them on
// again, which GitHub answers with the App's own already there. Reactions are never listed: GitHub
// lists a pull request's reactions only to a token that can read issues, which code review does not
// ask for, and putting one on gives the id that taking it off needs.
// A run that ends where no lane sees it end — cancelled in the queue, retired by the sweep — leaves
// a check run in progress on a pull request that is, by then, closed or long since moved on.

const (
	// reviewCheckName is the check run's name among a pull request's checks.
	reviewCheckName = "attest_tag review"
	// reviewSignalWait bounds a start's or an end's signals — a few calls to GitHub, on a context the
	// review's own cancellation cannot reach — so a GitHub that hangs holds the lane by no more.
	reviewSignalWait = 20 * time.Second
	// reviewReactionWorking and reviewReactionDone are the reactions: a review under way, and posted.
	reviewReactionWorking = "eyes"
	reviewReactionDone    = "rocket"
)

// reviewSignals is a claimed live review's signals on its pull request, on its hold. The start's
// goroutine writes eyes and check before it closes done; the lane reads them only after.
type reviewSignals struct {
	// gh is the signals' own client, since the start's requests are made beside the work's.
	gh *reviewGitHub
	// sha is the commit the check run reports on: the head the run reviews.
	sha string
	// repo and number are the pull request; score and summaryID are its review as it stood when the
	// run was claimed, which a run answered from an earlier review says stands. A summary comment
	// says an earlier review was posted, and so may have left its rocket.
	repo      string
	number    int
	score     int
	summaryID int64
	// checks is the installation having granted Checks, read and write.
	checks bool
	// again is an earlier attempt of the run having been claimed, which may have put signals up.
	again bool
	// done is closed once the start's signals are up or given up on; nil while none were tried.
	done chan struct{}
	// eyes and check are what the start put up, 0 for what it did not.
	eyes, check int64
	// end is how a posted review's check ends, which the poster sets; nil leaves it to the run's
	// ending (reviewCheckEnding).
	end *reviewCheckEnd
}

// reviewCheckEnd is a check run's completion: its conclusion and text, and where its details are.
type reviewCheckEnd struct {
	conclusion, title, summary, details string
}

// reviewChecksGranted reports whether an installation granted what the check run needs: Checks, read
// and write. One whose grant was never recorded has not, as far as anything here can tell, and is
// asked for nothing it may refuse; its owner accepting the App's new permissions records the grant
// (the installation's new_permissions_accepted delivery).
func reviewChecksGranted(permissions string) bool {
	var have map[string]string
	if json.Unmarshal([]byte(permissions), &have) != nil {
		return false
	}
	return have["checks"] == "write" || have["checks"] == "admin"
}

// reviewSignalsFor is a claimed run's signals, or nil for a run that makes none. Its result goes
// where publishReview sends it — live, or a run that posted before its repository went to shadow
// finishing what it started — and anything recorded rather than posted signals nothing. sha is the
// commit the run reviews.
func (b *Bot) reviewSignalsFor(ctx context.Context, h *reviewHold, pr *ReviewPR, plan reviewPlan, sha string) *reviewSignals {
	r := h.run
	if r.Kind != "review" || (plan.post != review.ModeLive && r.GitHubReviewID == 0) || !commitSHA.MatchString(sha) {
		return nil
	}
	gh, err := b.reviewClient(r.OrgID, r.InstallationID, pr.Repo, pr.Number)
	if err != nil {
		slog.Warn("code review: no client for the review's signals", "run", r.PublicID, "err", err)
		return nil
	}
	s := &reviewSignals{gh: gh, sha: sha, repo: pr.Repo, number: pr.Number, score: pr.Score, summaryID: pr.SummaryCommentID,
		again: r.Attempts > 1 || r.Error != ""}
	if g, err := b.store.GitHubInstall(ctx, r.InstallationID); err != nil {
		slog.Warn("code review: the installation's grant was not read; no check run", "run", r.PublicID, "err", err)
	} else if g != nil && g.OrgID == r.OrgID {
		s.checks = reviewChecksGranted(g.Permissions)
	}
	return s
}

// reviewAppID is the App's own id, which its check runs are told apart from another App's by.
func (b *Bot) reviewAppID() string {
	if b.proxy == nil || b.proxy.ghApp == nil {
		return ""
	}
	return b.proxy.ghApp.id
}

// reviewSummaryURL is a pull request's summary comment; "" before it has one.
func reviewSummaryURL(repo string, number int, commentID int64) string {
	if commentID <= 0 || !validGitHubRepo(repo) || number <= 0 {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d#issuecomment-%d", repo, number, commentID)
}

// signalReviewStarted puts a claimed run's start signals up — the eyes, and the check run in
// progress — beside the work rather than in front of it. specs are the types the run reviews as.
func (b *Bot) signalReviewStarted(lane context.Context, h *reviewHold, specs []reviewTypeSpec) {
	s := h.signals
	if s == nil || s.done != nil {
		return
	}
	keys := make([]string, 0, len(specs))
	types := make([]review.Type, 0, len(specs))
	for _, sp := range specs {
		keys, types = append(keys, sp.Key), append(types, sp.Type)
	}
	h.mu.Lock()
	runID := h.run.PublicID
	h.mu.Unlock()
	done := make(chan struct{})
	s.done = done
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(lane), reviewSignalWait)
		defer cancel()
		b.putSignalsUp(ctx, s, runID, review.RenderContext{Repo: s.repo, PR: s.number, Types: types}, keys)
	}()
}

func (b *Bot) putSignalsUp(ctx context.Context, s *reviewSignals, runID string, rctx review.RenderContext, keys []string) {
	if s.summaryID != 0 {
		// The last review's rocket comes off: the pull request is being reviewed again.
		b.takeReactionOff(ctx, s, runID, reviewReactionDone, 0)
	}
	if id, err := s.gh.ReactToPull(ctx, reviewReactionWorking); err != nil {
		// The installation may not allow it, as a command's eyes may not be (review_commands.go).
		slog.Debug("code review: no eyes on the pull request", "run", runID, "err", err)
	} else {
		s.eyes = id
	}
	if !s.checks {
		return
	}
	var id int64
	var err error
	if s.again {
		if id, err = s.gh.FindCheckRun(ctx, s.sha, reviewCheckName, runID, b.reviewAppID()); err != nil {
			slog.Warn("code review: an earlier attempt's check run was not looked for", "run", runID, "err", err)
			return
		}
	}
	title, summary := review.CheckStart(rctx, s.sha, keys)
	in := reviewCheckRun{Status: "in_progress", Output: &reviewCheckOutput{Title: title, Summary: summary}}
	if id != 0 {
		err = s.gh.UpdateCheckRun(ctx, id, in)
	} else {
		in.Name, in.HeadSHA, in.ExternalID, in.StartedAt = reviewCheckName, s.sha, runID, time.Now().UTC().Format(time.RFC3339)
		id, err = s.gh.CreateCheckRun(ctx, in)
	}
	if err != nil {
		slog.Warn("code review: the check run was not started", "run", runID, "err", err)
		return
	}
	s.check = id
}

// signalRunEnded takes a run's signals down once its ending is recorded: the eyes off, the rocket on
// for a review that stands — posted, or answered from an earlier one of the same commits — and the
// check run completed. A run put back has no ending yet, and leaves its signals up for its next
// attempt. One whose signals never went up — in this attempt or, as far as can be told, any before
// it — has none to take down.
func (b *Bot) signalRunEnded(lane context.Context, h *reviewHold) {
	s, end := h.signals, h.ending
	if s == nil || end == nil {
		return
	}
	if s.done != nil {
		<-s.done
	} else if !s.again {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(lane), reviewSignalWait)
	defer cancel()
	r := h.run
	// A run answered from an earlier review of the same commits ends noop once it has started; one
	// that ends noop before it starts — a label whose types were reviewed already — answered nothing.
	stands := end.Status == "posted" || (end.Status == "noop" && s.done != nil)
	if s.eyes != 0 || s.again {
		// An earlier attempt's eyes are found by putting them on again (takeReactionOff).
		b.takeReactionOff(ctx, s, r.PublicID, reviewReactionWorking, s.eyes)
	}
	if stands {
		if _, err := s.gh.ReactToPull(ctx, reviewReactionDone); err != nil {
			slog.Debug("code review: no rocket on the pull request", "run", r.PublicID, "err", err)
		}
	}
	if !s.checks {
		return
	}
	id := s.check
	if id == 0 && s.again {
		found, err := s.gh.FindCheckRun(ctx, s.sha, reviewCheckName, r.PublicID, b.reviewAppID())
		if err != nil {
			slog.Warn("code review: the run's check run was not looked for", "run", r.PublicID, "err", err)
			return
		}
		id = found
	}
	c := s.end
	if c == nil {
		c = b.reviewCheckEnding(s, *end, stands)
	}
	in := reviewCheckRun{Status: "completed", Conclusion: c.conclusion, CompletedAt: time.Now().UTC().Format(time.RFC3339),
		DetailsURL: c.details, Output: &reviewCheckOutput{Title: c.title, Summary: c.summary}}
	var err error
	switch {
	case id != 0:
		err = s.gh.UpdateCheckRun(ctx, id, in)
	case stands:
		// The start's never went up: the result goes among the checks all the same.
		in.Name, in.HeadSHA, in.ExternalID = reviewCheckName, s.sha, r.PublicID
		_, err = s.gh.CreateCheckRun(ctx, in)
	}
	if err != nil {
		slog.Warn("code review: the check run was not completed", "run", r.PublicID, "status", end.Status, "err", err)
	}
}

// takeReactionOff takes the App's reaction of one kind off the pull request: id when the client put
// it on, else the one GitHub answers putting it on again with — the App's already there, or, when
// there was none, the one just put on, so taking it off leaves the pull request as it was.
func (b *Bot) takeReactionOff(ctx context.Context, s *reviewSignals, runID, content string, id int64) {
	if id == 0 {
		var err error
		if id, err = s.gh.ReactToPull(ctx, content); err != nil {
			slog.Debug("code review: the App's reaction was not found to take off", "run", runID, "reaction", content, "err", err)
			return
		}
	}
	if err := s.gh.DeletePullReaction(ctx, id); err != nil {
		slog.Debug("code review: the App's reaction was not taken off", "run", runID, "reaction", content, "err", err)
	}
}

// reviewCheckEnding is how the check of a run the poster said nothing for ends: answered from an
// earlier review, whose score stands, or not posted at all — failed, cancelled, set aside for a newer
// head, or stopped before it ran.
func (b *Bot) reviewCheckEnding(s *reviewSignals, end ReviewRunResult, stands bool) *reviewCheckEnd {
	summaryURL := reviewSummaryURL(s.repo, s.number, s.summaryID)
	if stands {
		title, summary := review.CheckStanding(s.sha, s.score, summaryURL, review.RenderContext{Repo: s.repo, PR: s.number})
		return &reviewCheckEnd{conclusion: "success", title: title, summary: summary, details: summaryURL}
	}
	reason, _, _ := strings.Cut(end.Error, ":")
	title, summary := review.CheckEnded(end.Status, s.sha, review.FailReason(strings.TrimSpace(reason)), b.reviewSlug())
	conclusion := "skipped"
	switch end.Status {
	case "failed":
		conclusion = "neutral"
	case "cancelled":
		conclusion = "cancelled"
	}
	return &reviewCheckEnd{conclusion: conclusion, title: title, summary: summary}
}
