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

// The poster: what a finished review leaves on GitHub. Go writes every word of it — the model has
// no tool that writes — and there are two things, and after a re-review a third:
//
//   - one review object per run, posted against the commit the run reviewed (commit_id), as a
//     COMMENT, its body nothing but the run's signed marker and its inline comments rendered by
//     review.RenderFinding from the stored findings. A run with nothing to say inline posts a review
//     object only when the pull request has none of the App's yet (postPlainReview), its body one
//     line on how the review came out: GitHub lists as a pull request's reviewers those who left a
//     review, and the App belongs there once it has reviewed — but a review on every push saying
//     there is nothing new inline would be a notification about nothing.
//   - the sticky summary: one conversation comment per pull request, created at the first result
//     and edited in place after that, rendered by review.RenderSummary from what the database
//     holds — every finding in every status, so the score moves the moment a finding does.
//   - one short answer in the thread of each earlier finding a re-review found fixed, found back
//     after a fix was undone, or claimed fixed and found still there (postResolutionReplies) — best
//     effort, after the summary, and carried by the next review when it does not go out.
//
// Posting is the step that cannot be taken back, so it is guarded three ways. The pull request is
// read again first: a closed one gets nothing, and a head that moved while the review ran is
// handled rather than ignored (below). The lease is renewed before each write, so a lane that lost
// the run while it was busy finds out before it posts what another lane is posting. And a review
// that was posted by a run that died before it could record it is found by its marker and adopted,
// not posted again — the marker is an HMAC over the organisation, the pull request and the run, and
// is only believed on a review the App itself wrote.
//
// A head that moved: the first automatic review of a pull request still posts, against its own
// commit — GitHub accepts a review of an older commit and shows its comments against that diff —
// because restarting on every push is how a busy branch never gets reviewed at all. Findings in
// files whose change is no longer the one reviewed (review.PatchHash) are kept off the diff and
// listed in the summary as possibly outdated, and the footer names the new head. A review somebody
// asked for, or one of a push, is superseded instead, but only when a newer run is already queued
// for the pull request: otherwise it is the newest answer there is, and posts the same way.
//
// GitHub answers a review whose anchors it will not take with a 422 that does not say which. The
// files are read again and every anchor checked against them; if any moved, those go to the
// summary and the review is tried once more. If nothing moved, or the second try is refused too,
// nothing is posted inline and every finding is in the summary — a review that says less is better
// than none. A rate limit puts the run back for when GitHub said, and since its findings were
// checkpointed before any of this, the next attempt only posts.

// reviewBotLogin is the login GitHub gives this App's own comments: its slug and "[bot]".
func (b *Bot) reviewBotLogin() string {
	if b.proxy == nil || b.proxy.ghApp == nil || b.proxy.ghApp.slug == "" {
		return ""
	}
	return b.proxy.ghApp.slug + "[bot]"
}

// reviewByUs reports whether a comment or review is the App's own. A marker that verifies on a
// comment somebody else wrote is a copy — quoted, pasted — and is not ours to adopt or edit.
func (b *Bot) reviewByUs(u githubUser) bool {
	login := b.reviewBotLogin()
	return login != "" && strings.EqualFold(u.Login, login)
}

// reviewRenderContext is what rendering needs for one run's comments and summary. private is
// whether the repository is private now, which is what decides what a summary may say, not what it
// was when the run read it: a repository made public since is shown no cost, and none of the
// context repositories a private one was allowed to cite — which of them are public is not known
// here, so none is linked.
//
// ConsoleURL is left empty: the console has no page for a run yet, and a "details" link to its
// not-found page would be on every summary.
func (b *Bot) reviewRenderContext(ctx context.Context, r *ReviewRun, pr *ReviewPR, eff review.Effective,
	specs []reviewTypeSpec, ck *reviewCheckpoint, private bool) review.RenderContext {
	types := make([]review.Type, 0, len(specs))
	for _, s := range specs {
		types = append(types, s.Type)
	}
	slug := ""
	if b.proxy != nil && b.proxy.ghApp != nil {
		slug = b.proxy.ghApp.slug
	}
	rctx := review.RenderContext{Repo: pr.Repo, PR: pr.Number, HeadSHA: r.HeadSHA, BaseSHA: r.BaseSHA,
		DefaultSHAs: ck.DefaultSHAs, AllowedRepos: ck.ContextRepos, Slug: slug, CommentHeader: eff.CommentHeader,
		Types: types, RepoRules: ck.RepoRules, PublicRepo: !private, ShowCost: b.settings.Get(ctx, r.OrgID).ShowCost,
		MarkerKey: derivedKey("review-marker"), OrgID: r.OrgID}
	if !private && ck.Private {
		rctx.DefaultSHAs, rctx.AllowedRepos = nil, nil
	}
	// The fix box goes where a tick can be acted on (review_fix.go): fixes allowed for the
	// repository, a worker that can run for the organisation, and a branch of this repository, which
	// a fork's is not.
	rctx.FixBox = eff.Fixes && !pr.IsFork && b.reviewFixAvailable(ctx, r.OrgID) == nil
	return rctx
}

func reviewMarkerScope(rctx review.RenderContext) review.MarkerScope {
	return review.MarkerScope{OrgID: rctx.OrgID, Repo: rctx.Repo, PR: rctx.PR}
}

// reviewSummaryState is the sticky summary as the database has it: every finding on the pull
// request that was said where this summary is — on GitHub for a live one, and in the console too
// for a shadow one (reviewFindingsSaid) — with run r's own, and what r came to. A note is listed
// under its own heading and not scored. It returns the score too, the same Score the rendering
// computes, for review_prs and the run to record.
func (b *Bot) reviewSummaryState(ctx context.Context, r *ReviewRun, pr *ReviewPR, ck *reviewCheckpoint, headNow string,
	reviews int, shadow bool) (review.SummaryState, int, error) {
	all, err := b.store.reviewFindingsSaid(ctx, r.OrgID, pr.ID, shadow, r.ID)
	if reviewTry(r) {
		// A try is scored on what it found and nothing else: it is not the pull request's review.
		all, err = b.store.reviewRunFindings(ctx, r.OrgID, r.ID)
	}
	if err != nil {
		return review.SummaryState{}, -1, err
	}
	st := review.SummaryState{ReviewID: reviewSummaryID, Summary: ck.Summary, FullCoverage: ck.FullCoverage,
		InjectionDetected: ck.Injection, Reviews: reviews, ReviewedSHA: r.HeadSHA, HeadSHA: headNow, Rule: r.RuleLabel,
		CostUSD: ck.CostUSD, Paused: pr.Paused && !reviewTry(r)}
	if st.Paused && pr.PausedAuto {
		st.PausedAfter = reviewAutoPauseAfter
	}
	for _, t := range ck.Types {
		st.Types = append(st.Types, review.TypeRun{Key: t.Key, Summary: t.Summary, Skipped: t.Skipped, Auto: t.Auto})
	}
	st.Skills = reviewSkillsRead(ck)
	for _, f := range ck.NotReviewed {
		st.NotReviewed = append(st.NotReviewed, review.NotReviewedFile{Path: f.Path, Reason: f.Reason})
	}
	var open []review.Finding
	for _, f := range all {
		note := f.Kind == reviewKindNote
		sf := review.SummaryFinding{Finding: f.Finding, ID: f.PublicID, Status: f.Status, Placement: f.Placement,
			Place: f.Place, Note: note, PossiblyOutdated: f.PossiblyOutdated, ClaimedFixedSHA: f.ClaimedFixedSHA,
			Snippet: f.Snippet, AnchorSHA: f.AnchorSHA}
		if f.Status == review.FindingFixed || f.Status == review.FindingOutdated {
			sf.ResolvedIn = reviewResolvedSHA(f.StatusReason)
		}
		if f.GitHubCommentID > 0 {
			sf.CommentURL = fmt.Sprintf("https://github.com/%s/pull/%d#discussion_r%d", pr.Repo, pr.Number, f.GitHubCommentID)
		}
		st.Findings = append(st.Findings, sf)
		if (f.Status == review.FindingOpen || f.Status == review.FindingDisputed) && !f.PreExisting && !note {
			open = append(open, f.Finding)
		}
	}
	return st, review.Score(open, ck.FullCoverage, ck.Injection), nil
}

// publishReview posts what a run found, or records it without a word to GitHub in shadow mode, and
// ends the run. out is the engine's outcome when it ran in this attempt, nil when the run resumed
// from its checkpoint; only the suggestions' replaced lines are read from it.
func (b *Bot) publishReview(work, lane context.Context, h *reviewHold, pr *ReviewPR, gh *reviewGitHub, plan reviewPlan,
	specs []reviewTypeSpec, ck *reviewCheckpoint, out *reviewOutcome) {
	r := h.run
	if len(derivedKey("review-marker")) == 0 {
		b.endReviewRun(lane, h, ReviewRunResult{Status: "failed", Score: -1,
			Error: "internal: no key to sign the review's markers with (MASTER_KEY)"})
		return
	}
	// A run that already has a review on GitHub — posted by an earlier attempt, before the
	// repository was switched to shadow — finishes what it started: a review with no summary
	// beside it would be the one state nobody could read.
	shadow := (plan.post != review.ModeLive || reviewTry(r)) && r.GitHubReviewID == 0
	finish := func(status string, reviewID, summaryID int64, headNow string, inline int) {
		_, score, err := b.reviewSummaryState(lane, r, pr, ck, headNow, pr.ReviewsCount+1, shadow)
		if err != nil {
			b.reviewRunError(work, lane, h, err)
			return
		}
		res := ck.result(status, score)
		res.GitHubReviewID = reviewID
		if !reviewTry(r) {
			// A try leaves the pull request as it found it: the head last reviewed, the file hashes
			// the next review compares against and the score are the pull request's review's.
			res.Reviewed = &ReviewPRReviewed{SHA: r.HeadSHA, FileHashes: reviewHashesJSON(ck.FileHashes), Score: score,
				Automatic: reviewCountsTowardsPause(r.Trigger, reviewOptionsOf(r).BotLabel)}
		}
		if !b.endReviewRun(lane, h, res) {
			return
		}
		if status == "posted" {
			b.auditSystem(lane, r.OrgID, "review.posted", reviewAuditEvent(pr, r.PublicID, map[string]any{
				"head": r.HeadSHA, "review_id": reviewID, "summary_comment_id": summaryID, "inline": inline, "score": score}))
		}
		if !reviewTry(r) {
			// Last, once the review is on GitHub or in the console and recorded: the channel hears of
			// what happened, and nothing it does can undo that (review_notify.go). After the start's
			// announcement, which must not land over it.
			fixed := 0
			for _, e := range ck.Resolved {
				if e.Status == string(review.FindingFixed) {
					fixed++
				}
			}
			h.waitNotice()
			b.notifyReviewChannel(lane, pr, plan.eff, reviewNotice{kind: reviewNoticeReview, run: r, again: pr.ReviewsCount > 0,
				before: pr.Score, fixed: fixed, title: plan.title, base: plan.base, head: plan.head, started: h.startSaid})
		}
	}
	if shadow {
		finish("shadow", 0, 0, r.HeadSHA, 0)
		return
	}

	pull, err := gh.Pull(work)
	if err != nil {
		b.reviewPostFailed(work, lane, h, err)
		return
	}
	if pull.State != "open" {
		b.endReviewRun(lane, h, ReviewRunResult{Status: "cancelled", Score: -1, Error: "the pull request was closed before the review was posted"})
		return
	}
	// Rendered for the repository as it is now: the checkpoint may be from before it was made public.
	rctx := b.reviewRenderContext(lane, r, pr, plan.eff, specs, ck, pull.Base.Repo != nil && pull.Base.Repo.Private)
	mine, err := b.store.reviewRunFindings(work, r.OrgID, r.ID)
	if err != nil {
		b.reviewRunError(work, lane, h, err)
		return
	}
	headNow := pull.Head.SHA
	// Only before the review object is up: once it is, its comments sit where they sit, and what is
	// left to post is the summary, which names the new head either way.
	if headNow != r.HeadSHA && r.GitHubReviewID == 0 {
		first := (r.Trigger == "open" || r.Trigger == "catchup") && pr.LastReviewedSHA == ""
		if !first {
			newer, err := b.store.newerReviewQueued(work, r.OrgID, pr.ID, r.ID)
			if err != nil {
				b.reviewRunError(work, lane, h, err)
				return
			}
			if newer {
				// Its findings were never posted, and ending it superseded retires them
				// (finishReviewRun), so the newer run is free to raise them again. What it decided
				// about earlier findings is stored, though, and their threads are on GitHub: "Fixed
				// in" the head it reviewed is true whatever the newer head is, so it is said now.
				// One GitHub does not take now is carried by the next review (carryResolvedAnswers).
				if err := b.postResolutionReplies(work, lane, h, gh, pr, ck); err != nil {
					slog.Warn("code review: a superseded run's answers were not all posted; the next review carries the rest",
						"run", r.PublicID, "err", err)
				}
				b.endReviewRun(lane, h, ReviewRunResult{Status: "superseded", Score: -1,
					Error: "the pull request moved to " + shortSHA(headNow) + " and a newer review of it is queued"})
				return
			}
		}
		if err := b.holdBackMoved(work, gh, r, ck, mine); err != nil {
			b.reviewPostFailed(work, lane, h, err)
			return
		}
	}
	replaced := map[string]string{}
	if out != nil {
		for _, f := range out.Findings {
			replaced[f.Fingerprint] = f.ReplacedLines
		}
	}
	var inline []*ReviewFinding
	for _, f := range mine {
		if f.Placement == review.PlacementInline && f.Status == review.FindingOpen {
			inline = append(inline, f)
		}
	}
	reviewID := r.GitHubReviewID
	if reviewID == 0 && len(inline) > 0 {
		if reviewID, err = b.postInlineReview(work, h, gh, rctx, inline, replaced); err != nil {
			b.reviewPostFailed(work, lane, h, err)
			return
		}
	}
	if reviewID != 0 {
		b.recordCommentIDs(work, gh, rctx, reviewID, mine)
	}
	state, _, err := b.reviewSummaryState(work, r, pr, ck, headNow, pr.ReviewsCount+1, false)
	if err != nil {
		b.reviewRunError(work, lane, h, err)
		return
	}
	summaryID, err := b.upsertReviewSummary(work, h, gh, pr, review.RenderSummary(state, rctx), true)
	if err != nil {
		b.reviewPostFailed(work, lane, h, err)
		return
	}
	summaryURL := reviewSummaryURL(pr.Repo, pr.Number, summaryID)
	if reviewID == 0 {
		// Best effort past what has to wait: the summary is up, and a review that is up must not fail
		// over the one line that lists the App among the pull request's reviewers.
		plain, err := b.postPlainReview(work, h, gh, rctx, reviewPlainNote(state, summaryURL))
		var wait *githubRetryError
		switch {
		case errors.As(err, &wait), errors.Is(err, errLeaseLost):
			b.reviewPostFailed(work, lane, h, err)
			return
		case err != nil:
			slog.Warn("code review: no review object posted to list the App among the reviewers", "run", r.PublicID, "err", err)
		default:
			reviewID = plain
		}
	}
	if s := h.signals; s != nil {
		title, text := review.RenderCheck(state, rctx, summaryURL)
		s.end = &reviewCheckEnd{conclusion: "success", title: title, summary: text, details: summaryURL}
	}
	// After the summary, so the score that moved is on the pull request before anybody is told why;
	// only a wait GitHub asked for, or the run no longer being this lane's, holds the run up here.
	if err := b.postResolutionReplies(work, lane, h, gh, pr, ck); err != nil {
		b.reviewPostFailed(work, lane, h, err)
		return
	}
	posted := 0
	for _, f := range inline {
		if f.Placement == review.PlacementInline {
			posted++
		}
	}
	finish("posted", reviewID, summaryID, headNow, posted)
}

// postResolutionReplies answers in the thread of each earlier finding the run closed as fixed or
// opened again, and of each claimed fixed that its check found still there (reviewResolvedOf), with
// those an earlier run decided and never posted (carryResolvedAnswers): once each. An answer is
// posted only while the finding still stands as the run left it — a person may have resolved it since
// — and the thread has answers left (reviewThreadAnswers). Each one posted is recorded in the
// checkpoint before the next, so a run put back by a rate limit posts the rest and none twice; one
// GitHub refuses for good, a locked thread or a deleted comment, is given up on, not retried.
//
// The answers are the run's last word, after the review and the summary are up: a failure here must
// not fail a run whose review is on GitHub, which would leave the pull request's score and file hashes
// unrecorded and, with no inline review, retire findings the summary showed. So only a wait GitHub
// asked for, or the run being lost, is returned; anything else is logged, and an answer that did not
// go out is left unposted in the checkpoint for the next live review to carry.
func (b *Bot) postResolutionReplies(work, lane context.Context, h *reviewHold, gh *reviewGitHub, pr *ReviewPR, ck *reviewCheckpoint) error {
	if !slices.ContainsFunc(ck.Resolved, func(e reviewResolvedJSON) bool { return e.Reply != "" && !e.Replied }) {
		return nil
	}
	r := h.run
	all, err := b.store.ReviewFindings(work, r.OrgID, pr.ID)
	if err != nil {
		slog.Warn("code review: findings not read; the thread answers wait for the next review", "run", r.PublicID, "err", err)
		return nil
	}
	byID := map[string]*ReviewFinding{}
	for _, f := range all {
		byID[f.PublicID] = f
	}
	for i := range ck.Resolved {
		e := &ck.Resolved[i]
		if e.Reply == "" || e.Replied {
			continue
		}
		f := byID[e.Finding]
		stands := f != nil && (f.Status == review.FindingStatus(e.Status) ||
			(e.Status == "" && (f.Status == review.FindingOpen || f.Status == review.FindingDisputed)))
		if stands && f.GitHubCommentID > 0 && f.BotReplies < reviewThreadAnswers {
			if err := h.touch(work); err != nil {
				return err
			}
			_, err := gh.ReplyToReviewComment(work, f.GitHubCommentID, e.Reply)
			var api *githubAPIError
			var wait *githubRetryError
			switch {
			case errors.As(err, &wait):
				return err
			case errors.As(err, &api) && api.Status < 500:
				slog.Warn("code review: GitHub refused a finding's resolution answer; not tried again", "run", r.PublicID,
					"finding", f.PublicID, "err", err)
			case err != nil:
				slog.Warn("code review: a finding's resolution answer did not go out; the next review carries it", "run", r.PublicID,
					"finding", f.PublicID, "err", err)
				continue
			default:
				wctx, cancel := context.WithTimeout(context.WithoutCancel(lane), reviewWriteTimeout)
				if err := b.store.addReviewFindingBotReply(wctx, r.OrgID, f.ID); err != nil {
					slog.Warn("code review: the thread's answer count was not recorded", "run", r.PublicID, "err", err)
				}
				cancel()
				b.auditSystem(lane, r.OrgID, "review.replied", reviewAuditEvent(pr, r.PublicID, map[string]any{
					"finding": f.PublicID, "class": "resolution", "outcome": cmp.Or(e.Status, "still_present")}))
			}
		}
		if stands && f.GitHubCommentID > 0 && e.Status == string(review.FindingFixed) {
			// After its answer, or in its place when the thread has had its answers: a fixed finding's
			// conversation is settled, and resolving it folds it away (review_thread_resolve.go).
			if err := h.touch(work); err != nil {
				return err
			}
			b.reviewResolveThread(work, r, gh, pr, f, "fixed")
		}
		e.Replied = true
		if err := b.saveReviewRunCheckpoint(lane, h, ck); err != nil {
			if errors.Is(err, errLeaseLost) {
				return err
			}
			// What was posted is not recorded: nothing more is posted on top of it, and the rest wait
			// for the next review.
			slog.Warn("code review: an answer posted was not recorded; the rest wait for the next review", "run", r.PublicID, "err", err)
			return nil
		}
	}
	return nil
}

// saveReviewRunCheckpoint writes a review run's checkpoint again, fenced on its lease, once something
// in it has moved on since the model work: an answer posted.
func (b *Bot) saveReviewRunCheckpoint(lane context.Context, h *reviewHold, ck *reviewCheckpoint) error {
	raw, err := json.Marshal(ck)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(lane), reviewWriteTimeout)
	defer cancel()
	return h.write(func(r *ReviewRun) error { return b.store.saveReviewCheckpoint(wctx, r, string(raw), nil, nil) })
}

// holdBackMoved keeps off the diff the inline findings of files whose change is no longer the one
// that was reviewed, now that the head has moved: their lines may say something else at the new
// head, and a comment on a line that changed under it reads as a comment on the new code.
func (b *Bot) holdBackMoved(ctx context.Context, gh *reviewGitHub, r *ReviewRun, ck *reviewCheckpoint, mine []*ReviewFinding) error {
	files, err := gh.PullFiles(ctx)
	if err != nil {
		return err
	}
	now := map[string]string{}
	for _, f := range files.Files {
		now[f.Path] = review.PatchHash(f)
	}
	for _, f := range mine {
		if f.Placement != review.PlacementInline {
			continue
		}
		// "" on either side cannot be told apart, so it is counted as changed (review.PatchHash).
		if was, is := ck.FileHashes[f.Path], now[f.Path]; was == "" || is == "" || was != is {
			if err := b.store.setReviewFindingPlacement(ctx, r.OrgID, f.ID, string(review.PlacementSummary), true); err != nil {
				return err
			}
			f.Placement, f.PossiblyOutdated = review.PlacementSummary, true
		}
	}
	return nil
}

// reviewInlineComments renders findings as the comments of one review.
func reviewInlineComments(rctx review.RenderContext, fs []*ReviewFinding, replaced map[string]string) []reviewInlineComment {
	out := make([]reviewInlineComment, 0, len(fs))
	for _, f := range fs {
		c := rctx
		c.FindingID, c.VerifierConfidence, c.ReplacedLines = f.PublicID, f.VerifierConfidence, replaced[f.Fingerprint]
		ic := reviewInlineComment{Path: f.Path, Line: f.Line, Side: string(f.Side), Body: review.RenderFinding(f.Finding, c)}
		if f.StartLine > 0 && f.StartLine != f.Line {
			ic.StartLine, ic.StartSide = f.StartLine, string(f.Side)
		}
		out = append(out, ic)
	}
	return out
}

// postInlineReview posts the run's one review object, or adopts it if an earlier attempt posted it
// and died before recording it, and records its id the moment GitHub gives it. It returns 0 when
// GitHub would take no inline comment at all, having moved every finding to the summary.
func (b *Bot) postInlineReview(ctx context.Context, h *reviewHold, gh *reviewGitHub, rctx review.RenderContext,
	inline []*ReviewFinding, replaced map[string]string) (int64, error) {
	r := h.run
	body := review.Marker(rctx.MarkerKey, reviewMarkerScope(rctx), review.MarkerRun, r.PublicID)
	post := func(fs []*ReviewFinding) (int64, error) {
		// The marker check and the lease check both come immediately before the post: this is the
		// last moment another lane's review of the same run can be seen, and this lane's loss of it.
		if err := h.touch(ctx); err != nil {
			return 0, err
		}
		if id, err := b.adoptReview(ctx, gh, rctx, r.PublicID); err != nil || id != 0 {
			return id, err
		}
		rv, err := gh.PostReview(ctx, r.HeadSHA, body, reviewInlineComments(rctx, fs, replaced))
		if err != nil {
			return 0, err
		}
		return rv.ID, nil
	}
	toSummary := func(fs []*ReviewFinding) error {
		for _, f := range fs {
			if err := b.store.setReviewFindingPlacement(ctx, r.OrgID, f.ID, string(review.PlacementSummary), f.PossiblyOutdated); err != nil {
				return err
			}
			f.Placement = review.PlacementSummary
		}
		return nil
	}
	id, err := post(inline)
	if isGitHubStatus(err, 422) {
		slog.Warn("code review: GitHub refused the review's anchors; checking them again", "run", r.PublicID, "err", err)
		moved, keep, rerr := b.revalidateAnchors(ctx, gh, inline)
		if rerr != nil {
			return 0, rerr
		}
		if err := toSummary(moved); err != nil {
			return 0, err
		}
		id, err = 0, nil
		if len(moved) > 0 && len(keep) > 0 {
			id, err = post(keep)
		}
		if len(moved) == 0 || isGitHubStatus(err, 422) {
			// Nothing GitHub said can be put right from here: every finding goes to the summary,
			// and no review object is posted.
			slog.Warn("code review: posting no inline comments; every finding is in the summary", "run", r.PublicID)
			return 0, toSummary(keep)
		}
	}
	if err != nil || id == 0 {
		return id, err
	}
	return b.recordReviewID(ctx, h, id)
}

// postPlainReview posts a review with no inline comments, its body a line on how the review came
// out, when the pull request has no review of the App's yet: GitHub lists as reviewers only those who
// left one, and a run whose findings all went to the summary leaves none otherwise. A review of this
// run's already there — an attempt that died before recording it — is adopted; one of another run's
// means the App is listed already, and nothing is posted. It returns the review's id, 0 for none.
func (b *Bot) postPlainReview(ctx context.Context, h *reviewHold, gh *reviewGitHub, rctx review.RenderContext, note string) (int64, error) {
	r := h.run
	if err := h.touch(ctx); err != nil {
		return 0, err
	}
	reviews, _, err := gh.Reviews(ctx)
	if err != nil {
		return 0, err
	}
	listed := false
	for _, rv := range reviews {
		if !b.reviewByUs(rv.User) {
			continue
		}
		if id, ok := review.VerifiedMarker(rctx.MarkerKey, reviewMarkerScope(rctx), rv.Body, review.MarkerRun); ok && id == r.PublicID {
			slog.Info("code review: adopted a review an earlier attempt posted", "run", r.PublicID, "review", rv.ID)
			return b.recordReviewID(ctx, h, rv.ID)
		}
		listed = true
	}
	if listed {
		return 0, nil
	}
	body := review.Marker(rctx.MarkerKey, reviewMarkerScope(rctx), review.MarkerRun, r.PublicID) + "\n" + note
	rv, err := gh.PostReview(ctx, r.HeadSHA, body, nil)
	if err != nil {
		return 0, err
	}
	return b.recordReviewID(ctx, h, rv.ID)
}

// recordReviewID records the run's review object the moment GitHub gives its id. Not on ctx: the
// review is on GitHub and paid for, and a cancel heard a moment after the post must not lose the one
// record that stops the next attempt posting it again.
func (b *Bot) recordReviewID(ctx context.Context, h *reviewHold, id int64) (int64, error) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reviewWriteTimeout)
	defer cancel()
	err := h.write(func(r *ReviewRun) error { return b.store.setReviewRunGitHubReview(wctx, r, id) })
	return id, err
}

// reviewPlainNote is the one line of a review with no inline comments: the commit reviewed, the score
// the summary shows, and where the findings are, linked when the summary comment is known.
func reviewPlainNote(st review.SummaryState, summaryURL string) string {
	var open []review.Finding
	for _, f := range st.Findings {
		if (f.Status == "" || f.Status == review.FindingOpen || f.Status == review.FindingDisputed) && !f.Note && !f.PreExisting {
			open = append(open, f.Finding)
		}
	}
	where := "the summary comment"
	if summaryURL != "" {
		where = "the [summary comment](" + summaryURL + ")"
	}
	line := "Reviewed"
	if sha := shortSHA(st.ReviewedSHA); sha != "" {
		line += " `" + sha + "`"
	}
	line += fmt.Sprintf(" · Confidence %d/5 (advisory). ", review.Score(open, st.FullCoverage, st.InjectionDetected))
	switch len(open) {
	case 0:
		return line + "Nothing to comment on in the diff; " + where + " has the review."
	case 1:
		return line + "No comment on the diff: its one open finding is in " + where + "."
	}
	return line + fmt.Sprintf("No comments on the diff: its %d open findings are in %s.", len(open), where)
}

// adoptReview finds a review the App posted for this run, by its marker; 0 when there is none.
func (b *Bot) adoptReview(ctx context.Context, gh *reviewGitHub, rctx review.RenderContext, runID string) (int64, error) {
	reviews, _, err := gh.Reviews(ctx)
	if err != nil {
		return 0, err
	}
	for _, rv := range reviews {
		if !b.reviewByUs(rv.User) {
			continue
		}
		if id, ok := review.VerifiedMarker(rctx.MarkerKey, reviewMarkerScope(rctx), rv.Body, review.MarkerRun); ok && id == runID {
			slog.Info("code review: adopted a review an earlier attempt posted", "run", runID, "review", rv.ID)
			return rv.ID, nil
		}
	}
	return 0, nil
}

// revalidateAnchors reads the pull request's files again and checks every inline finding's anchor
// against them, the way the engine did before posting: GitHub's 422 says some anchor is wrong and
// not which.
func (b *Bot) revalidateAnchors(ctx context.Context, gh *reviewGitHub, inline []*ReviewFinding) (moved, keep []*ReviewFinding, err error) {
	files, err := gh.PullFiles(ctx)
	if err != nil {
		return nil, nil, err
	}
	hunks := map[string][]review.Hunk{}
	for _, f := range files.Files {
		if f.Parse() == nil {
			hunks[f.Path] = f.Hunks
		}
	}
	for _, f := range inline {
		if review.ValidAnchor(hunks[f.Path], f.Side, f.StartLine, f.Line) {
			keep = append(keep, f)
		} else {
			moved = append(moved, f)
		}
	}
	return moved, keep, nil
}

// recordCommentIDs records the inline comment each finding became, by the finding's marker on it,
// which is how a reply in its thread finds the finding later. Best effort: a finding left without
// its comment id costs the reply its context, not the review its post.
func (b *Bot) recordCommentIDs(ctx context.Context, gh *reviewGitHub, rctx review.RenderContext, reviewID int64, mine []*ReviewFinding) {
	comments, _, err := gh.ReviewComments(ctx)
	if err != nil {
		slog.Warn("code review: inline comment ids not read", "repo", rctx.Repo, "pr", rctx.PR, "err", err)
		return
	}
	byID := map[string]*ReviewFinding{}
	for _, f := range mine {
		byID[f.PublicID] = f
	}
	for _, c := range comments {
		if c.PullRequestReviewID != reviewID || !b.reviewByUs(c.User) {
			continue
		}
		id, ok := review.VerifiedMarker(rctx.MarkerKey, reviewMarkerScope(rctx), c.Body, review.MarkerFinding)
		f := byID[id]
		if !ok || f == nil || f.GitHubCommentID == c.ID {
			continue
		}
		if err := b.store.SetReviewFindingComment(ctx, f.OrgID, f.ID, c.ID); err != nil {
			slog.Warn("code review: inline comment id not recorded", "finding", f.PublicID, "err", err)
			continue
		}
		f.GitHubCommentID = c.ID
	}
}

// upsertReviewSummary edits the pull request's summary comment in place, or makes it when create
// says this is a review's result and there is none. The comment is the stored one, or else the
// App's own comment carrying the summary's marker — the post of a run that died before it could
// record the id. One somebody deleted is made again. It returns the comment's id, 0 when there is
// none and create was false.
func (b *Bot) upsertReviewSummary(ctx context.Context, h *reviewHold, gh *reviewGitHub, pr *ReviewPR, body string, create bool) (int64, error) {
	id := pr.SummaryCommentID
	if id == 0 {
		comments, _, err := gh.IssueComments(ctx)
		if err != nil {
			return 0, err
		}
		key := derivedKey("review-marker")
		scope := review.MarkerScope{OrgID: pr.OrgID, Repo: pr.Repo, PR: pr.Number}
		for _, c := range comments {
			if mid, ok := review.VerifiedMarker(key, scope, c.Body, review.MarkerReview); ok && mid == reviewSummaryID && b.reviewByUs(c.User) {
				id = c.ID
				if err := b.store.SetReviewPRSummaryComment(ctx, pr.OrgID, pr.ID, id); err != nil {
					return 0, err
				}
				break
			}
		}
	}
	if id != 0 {
		gh.AdoptIssueComment(id)
		if err := h.touch(ctx); err != nil {
			return 0, err
		}
		_, err := gh.EditIssueComment(ctx, id, body)
		if err == nil {
			pr.SummaryCommentID = id
			return id, nil
		}
		if !isGitHubStatus(err, 404) {
			return 0, err
		}
		id = 0 // somebody deleted it: the summary starts again below
	}
	if !create {
		return 0, nil
	}
	if err := h.touch(ctx); err != nil {
		return 0, err
	}
	c, err := gh.CreateIssueComment(ctx, body)
	if err != nil {
		return 0, err
	}
	if err := b.store.SetReviewPRSummaryComment(ctx, pr.OrgID, pr.ID, c.ID); err != nil {
		return 0, err
	}
	pr.SummaryCommentID = c.ID
	return c.ID, nil
}

// reviewPostFailed handles an error from GitHub while posting. The findings are checkpointed, so
// another attempt costs only the post: a wait GitHub asked for is waited out with no attempt spent,
// anything else is tried again a minute later while attempts last, and then the run fails.
func (b *Bot) reviewPostFailed(work, lane context.Context, h *reviewHold, err error) {
	if b.interrupted(work, lane, h) || errors.Is(err, errLeaseLost) {
		return
	}
	var wait *githubRetryError
	if errors.As(err, &wait) {
		b.requeueReview(lane, h, time.Now().Add(max(wait.Wait, time.Second)), err.Error(), false)
		return
	}
	b.retryOrFail(work, lane, h, err, review.FailGitHub)
}

// reviewHashesJSON is review_prs.file_hashes. json.Marshal sorts a map's keys, so the same hashes
// are always the same text.
func reviewHashesJSON(m map[string]string) string {
	if m == nil {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// processResync renders a pull request's summary again from what is stored, with no model and no
// read of the code: a push moved the head, and the footer says which head was reviewed and which
// was not; or a finding changed — withdrawn or downgraded after a reply, claimed fixed, resolved by
// a person — and the score with it. The score is recomputed in Go from the findings as they stand
// and recorded on the pull request whatever else happens, shadow mode included, since the console
// and the status command read it there. Only a summary that exists is edited — a resync never makes
// one — and only on a pull request whose review is posted now (reviewDestination): its branches can
// keep it in shadow on a live repository, or post it on one in shadow.
func (b *Bot) processResync(work, lane context.Context, h *reviewHold) {
	r := h.run
	noop := func(why string) { b.endReviewRun(lane, h, ReviewRunResult{Status: "noop", Score: -1, Error: why}) }
	pr, err := b.store.ReviewPR(work, r.OrgID, r.ReviewPRID)
	if err != nil || pr == nil {
		b.reviewRunError(work, lane, h, fmt.Errorf("the pull request could not be read: %v", err))
		return
	}
	last, err := b.store.latestReviewOutcome(work, r.OrgID, pr.ID)
	if err != nil {
		b.reviewRunError(work, lane, h, err)
		return
	}
	ck, ok := (*reviewCheckpoint)(nil), false
	if last != nil {
		ck, ok = checkpointFrom(last)
	}
	if !ok {
		noop("no finished review to render again")
		return
	}
	if _, score, err := b.reviewSummaryState(work, last, pr, ck, pr.HeadSHA, pr.ReviewsCount, last.Status == "shadow"); err != nil {
		b.reviewRunError(work, lane, h, err)
		return
	} else if score != pr.Score {
		if err := h.write(func(r *ReviewRun) error { return b.store.setReviewPRScore(work, r, score) }); err != nil {
			b.reviewRunError(work, lane, h, err)
			return
		}
		pr.Score = score
	}
	if last.Status != "posted" || pr.SummaryCommentID == 0 {
		noop("no posted summary to render again")
		return
	}
	eff, s, err := b.reviewEffective(work, r.OrgID, r.InstallationID, pr.Repo)
	if err != nil {
		b.reviewRunError(work, lane, h, err)
		return
	}
	if s != nil {
		noop("the repository is not posting reviews now")
		return
	}
	gh, err := b.reviewClient(r.OrgID, r.InstallationID, pr.Repo, pr.Number)
	if err != nil {
		b.reviewRunError(work, lane, h, err)
		return
	}
	// The branches are read only when a rule could send this pull request's review somewhere other
	// than the repository's mode: a resync runs on every push, and most trees have no such rule.
	dest := eff.Mode
	if reviewDestinationByBranch(eff) {
		pull, err := gh.Pull(work)
		if err != nil {
			b.reviewRunError(work, lane, h, err)
			return
		}
		dest = reviewDestination(eff, pull)
	}
	if dest != review.ModeLive {
		noop("this pull request's review is not posted now")
		return
	}
	keys := make([]string, 0, len(last.Types))
	for _, t := range last.Types {
		keys = append(keys, t.Key)
	}
	specs, _, err := resolveReviewTypes(work, b.store, r.OrgID, keys)
	if err != nil {
		b.reviewRunError(work, lane, h, err)
		return
	}
	// The repository's visibility as the last delivery said it, not as the checkpoint has it: a
	// resync renders on every push, for as long as the pull request is open.
	rctx := b.reviewRenderContext(lane, last, pr, eff, specs, ck, pr.IsPrivate)
	state, _, err := b.reviewSummaryState(work, last, pr, ck, pr.HeadSHA, pr.ReviewsCount, false)
	if err != nil {
		b.reviewRunError(work, lane, h, err)
		return
	}
	id, err := b.upsertReviewSummary(work, h, gh, pr, review.RenderSummary(state, rctx), false)
	if err != nil {
		b.reviewPostFailed(work, lane, h, err)
		return
	}
	if id == 0 {
		noop("the summary comment is gone from the pull request")
		return
	}
	b.endReviewRun(lane, h, ReviewRunResult{Status: "posted", Score: -1, Summary: "summary re-rendered for head " + shortSHA(pr.HeadSHA)})
}
