package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Resolving a finding's thread on GitHub once the finding is closed: fixed by a push, which the
// re-review's "Fixed in" answers in the thread (postResolutionReplies), or withdrawn on a reply in it
// (replyFinish). The thread then folds away on the pull request as it would if a person had resolved
// it, and the conversation reads as settled where it is. Never the other way: a finding that comes
// back after a fix was undone is answered "Back at", and its thread is left as it is — unresolving
// somebody's conversation is a person's call, not the reviewer's — and so is one a person unresolves
// after we resolved it (reviewThreadEvent ignores it, as it ignores our own resolving).
//
// The thread is found by its node id: the one a pull_request_review_thread delivery stored on the
// finding, or else the one GitHub's GraphQL lists for the pull request whose first comment is the
// finding's inline comment, which is stored for next time. A REST comment's node id is the comment's,
// not its thread's.
//
// Best effort, after the answer is posted: a thread that could not be resolved costs the pull request
// a tidy conversation and nothing else, so it never fails, holds up or requeues the run. Which
// permission GitHub wants of an App for resolveReviewThread is not documented; the mutation goes out
// with the review's post token, pull_requests:write, the narrowest that could be it. If GitHub refuses
// it for want of a permission, that is logged once a day per installation and remembered against the
// installation — the console's connection card says that resolving threads needs a permission the
// installation's token lacks, and the answers in the threads still go out — until a thread of it is
// resolved again, which clears it.

// reviewThreadsRefusedEvery is how often a refusal is logged, per installation.
const reviewThreadsRefusedEvery = 24 * time.Hour

// reviewThreadsRefusedKey is the AlertOnce key a refusal is logged under, once a day; its time is the
// first refusal of the day's window. reviewThreadsRefusedLastKey is written at every refusal, so its
// time is when GitHub last refused, which is what the console's connection card says: the log's key
// would show a refusal five minutes ago as one nearly a day old.
func reviewThreadsRefusedKey(installationID int64) string {
	return fmt.Sprintf("review-threads-refused:%d", installationID)
}

func reviewThreadsRefusedLastKey(installationID int64) string {
	return reviewThreadsRefusedKey(installationID) + ":last"
}

// reviewResolveThread resolves the review thread of f's inline comment on run r's pull request, and
// audits it (review.thread_resolved), with why it was closed: "fixed" or "withdrawn". The caller has
// renewed the run's lease just before.
func (b *Bot) reviewResolveThread(ctx context.Context, r *ReviewRun, gh *reviewGitHub, pr *ReviewPR, f *ReviewFinding, why string) {
	if f.GitHubCommentID <= 0 {
		return // never posted inline: there is no thread
	}
	thread := f.ThreadNodeID
	if thread != "" {
		gh.KnowThread(thread)
	} else {
		t, ok, err := gh.ThreadOf(ctx, f.GitHubCommentID)
		if err != nil {
			b.reviewThreadNotResolved(ctx, r, f, err)
			return
		}
		if !ok {
			slog.Info("code review: no thread on the pull request starts with the finding's comment; nothing resolved", "run", r.PublicID,
				"finding", f.PublicID, "comment", f.GitHubCommentID)
			return
		}
		thread = t.ID
		if err := b.store.SetReviewFindingThread(ctx, r.OrgID, f.ID, thread); err != nil {
			slog.Warn("code review: a finding's thread id was not recorded", "run", r.PublicID, "finding", f.PublicID, "err", err)
		}
		if t.Resolved {
			return // a person resolved it already: there is nothing to do, and nothing of ours to audit
		}
	}
	if err := gh.ResolveThread(ctx, thread); err != nil {
		b.reviewThreadNotResolved(ctx, r, f, err)
		return
	}
	// It works for this installation now — its owner accepted what it lacked — and the card stops
	// saying otherwise.
	b.store.ClearAlert(ctx, r.OrgID, reviewThreadsRefusedKey(r.InstallationID))
	b.store.ClearAlert(ctx, r.OrgID, reviewThreadsRefusedLastKey(r.InstallationID))
	b.auditSystem(ctx, r.OrgID, "review.thread_resolved", reviewAuditEvent(pr, r.PublicID, map[string]any{
		"finding": f.PublicID, "title": f.Title, "thread": thread, "why": why}))
}

// reviewThreadNotResolved logs a thread GitHub would not resolve, or find: a refusal for want of a
// permission once a day per installation, remembered for the console; anything else each time, since
// it is about this one call.
func (b *Bot) reviewThreadNotResolved(ctx context.Context, r *ReviewRun, f *ReviewFinding, err error) {
	if !githubPermissionDenied(err) {
		slog.Warn("code review: a finding's thread was not resolved", "run", r.PublicID, "finding", f.PublicID, "err", err)
		return
	}
	b.store.AlertOnce(ctx, r.OrgID, reviewThreadsRefusedLastKey(r.InstallationID), 0) // a window of 0 always writes now
	if b.store.AlertOnce(ctx, r.OrgID, reviewThreadsRefusedKey(r.InstallationID), reviewThreadsRefusedEvery) {
		slog.Warn("code review: GitHub refused to resolve a review thread with the token code review posts with "+
			"(pull_requests:write); the answers in the threads still go out, and the threads stay open",
			"org", r.OrgID, "installation", r.InstallationID, "run", r.PublicID, "err", err)
	}
}
