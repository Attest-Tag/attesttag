package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"attesttag/internal/review"
)

// Code review's announcements in a chat channel: the team hears about its pull requests where it
// already talks, without anybody watching the console or GitHub's notifications.
//
// The channel is a setting (review.Settings.Notify), inherited like any single value, and a branch
// rule may send its pull requests to another one or to none. Four kinds of event are announced, and
// the settings may leave any of them out (review.Settings.NotifyOn): a review starting, once its run
// is claimed and through the gate; every review that finishes — posted, or recorded in shadow, which
// the channel may hear about because it is the organisation's own and never GitHub; a review that
// failed, or that did not run for a reason the team needs to hear; and the pull request being
// merged. Nothing here calls a model: every word is Go's, from what the database holds.
//
// Low noise is the point, so a pull request gets ONE message in the channel. It is posted at the
// first announcement and edited in place from then on to say how the pull request stands now — its
// branches, the commit last reviewed, the score, what is open and the worst three of it, and what is
// happening to it — and each later event is also one short reply in that message's thread, so the
// channel's main view gets one line per pull request and the history is a click away. A start is the
// exception: it only edits the message to "Reviewing `abc1234`…", the open findings still listed,
// since the reply that matters is the result's, minutes later, and two replies a review would be the
// noise this is built against. Where the message is lives on review_prs (notify_team,
// notify_channel, notify_ts). A merge is news only of a pull request the channel has heard of or a
// review has read: the dependency bumps and drafts the gate skips by design would otherwise be a
// message each, saying nothing but that they were merged.
//
// A failure puts the message back to how the pull request stood before the run — its last review's
// findings, which are still what is open — with a line saying the review failed and which kind of
// failure it was, never the error, which is the deployment's (review.FailReason); the reply says the
// same. A review that did not run is said only where people would otherwise wait for one that is not
// coming — the money stopped it, the pull request's automatic reviews are paused, the organisation's
// plan has no code review (reviewSkipMatters) — and once per reason until something else is said, so
// a paused branch pushed to all day is told once. A draft, a bot's pull request, an excluded author,
// nothing left once the ignored files are set aside: that is the gate doing what it was set to, and
// says nothing. Except after a start: a run whose start was announced always puts the message back
// when it ends, quietly when its ending is not news (reviewNoticeWanted), so the message never stays
// on "Reviewing…" — whether the lane ends it (noticeRunEnded) or it ends in the queue or in the sweep,
// where no lane is left to (noticeStartOrphaned). The plan's refusal is told only of a pull request
// nothing else in the gate would have stopped: on a plan with no code review every draft and every
// bot's bump would otherwise say "plan".
//
// Nothing is posted in a channel shared with another organisation — Slack Connect, an Enterprise
// Grid share — or one the platform cannot say is not: the message carries a repository's name,
// branches, file paths and the titles of its findings, shadow ones GitHub never saw included, and
// another company's members have no business reading them (the Configure link is withheld from such
// a channel for the same reason, sharedConfigureURL). The settings refuse such a channel when they
// are saved; this is the check for one shared since.
//
// Announcing never fails the thing announced, nor holds it up. A review's start is announced beside
// its work, never in front of it (noticeReviewStarted); its result is on GitHub, or in the console,
// before the channel hears of it; and a post or an edit the chat platform refuses is logged, audited
// (review.notify_failed) and told to the admins' alert channel once an hour; the next event tries
// again, and a message somebody deleted is posted afresh by the next event that is news — a quiet
// put-back never posts one. Everything in it that came from a pull
// request or a model — titles, branch names, finding titles — is held to one line, cut short and
// defused (reviewChatText): a title is the author's to write, and must not ping a channel or carry a
// link of its own into it.

const (
	// reviewNoticeTop is how many open findings the message names; the rest are counted.
	reviewNoticeTop = 3
	// reviewNoticeWait bounds one announcement, a few calls to the chat platform, on a context the
	// review's own cancellation cannot reach: the review is over by then.
	reviewNoticeWait = 20 * time.Second
	// reviewNoticeStartWait bounds a start's, which runs beside the review and which everything the
	// run says after it waits for (reviewHold.waitNotice): a platform that hangs must not hold a
	// review's result back by more than this.
	reviewNoticeStartWait = 8 * time.Second
	// The kinds of event announced. end is a run whose start was announced ending in a way that is
	// not news — answered from an earlier review, cancelled, superseded — which only puts the
	// message back.
	reviewNoticeStart  = "start"
	reviewNoticeReview = "review"
	reviewNoticeFail   = "fail"
	reviewNoticeSkip   = "skip"
	reviewNoticeEnd    = "end"
	reviewNoticeMerged = "merged"
)

// reviewNotice is one event a pull request's channel hears about.
type reviewNotice struct {
	kind string
	// run is the review the event is about; nil for a merge, and for a review the gate stopped before
	// it had a run. Everything but a finished review is drawn on the pull request's last review, if it
	// had one: what was open before the run is what is still open.
	run *ReviewRun
	// again is a review of a pull request reviewed before, before is its score then (-1 none), and
	// fixed how many earlier findings this review found fixed: the reply's "k fixed, score a → b".
	again         bool
	before, fixed int
	// What GitHub says of the pull request now: its title and branches, and who merged it.
	title, base, head, mergedBy string
	// For a start, a failure and a skip: the commit the review is of, the names of the types it runs
	// (a start's), and whether it is recorded in shadow.
	sha    string
	types  []string
	shadow bool
	// why is a failure's reason in the channel's words (reviewFailWhy); reason is a skip's code, the
	// gate's (reviewSkip.Reason).
	why, reason string
	// quiet draws the message as the pull request stands and says nothing of the event: no line in
	// the message and no reply (reviewNoticeWanted).
	quiet bool
	// started is a run's ending whose hold saw its start announced (reviewHold.startSaid), which
	// notify_last may no longer say: another run's announcement, made as the lease passed between
	// them, can record its own over it after this start's edit landed.
	started bool
}

// key is what notify_last records for the event, so it is not announced twice: a merge delivered
// again, a run put back and claimed again announcing its start. A skip's is its reason alone, so a
// pull request paused or out of money is told so once, not at every push, until something else is
// said of it.
func (n reviewNotice) key() string {
	switch n.kind {
	case reviewNoticeMerged:
		return reviewNoticeMerged
	case reviewNoticeSkip:
		return "skip:" + n.reason
	case reviewNoticeStart, reviewNoticeEnd:
		return n.kind + ":" + n.run.PublicID
	case reviewNoticeFail:
		return "failed:" + n.run.PublicID
	}
	return "run:" + n.run.PublicID
}

// reviewNoticeWanted is whether n is announced, under settings eff, on pr as it stands — and whether
// only quietly: the message put back to how the pull request stands, with no word of n and no reply.
// That is what is left of an event the channel is not told of, or one not worth a word, when the
// message says this run's review is under way: it must not go on saying so. It says so when
// notify_last is this run's start, or when the run's hold saw that start announced (n.started) and
// notify_last names no other run's — whose own ending puts the message back, and whose "Reviewing…"
// this run's must not wipe while it is true. A skip is news when the team would otherwise wait for a
// review that is not coming (reviewSkipMatters), or when the message said one was coming.
func reviewNoticeWanted(eff review.Effective, pr *ReviewPR, n reviewNotice) (quiet, ok bool) {
	reviewing := false
	if n.run != nil {
		reviewing = pr.NotifyLast == (reviewNotice{kind: reviewNoticeStart, run: n.run}).key() ||
			n.started && !strings.HasPrefix(pr.NotifyLast, reviewNoticeStart+":")
	}
	switch n.kind {
	case reviewNoticeStart:
		return false, eff.Notifies(review.NotifyStarted)
	case reviewNoticeMerged:
		return false, eff.Notifies(review.NotifyMerged)
	case reviewNoticeReview:
		if eff.Notifies(review.NotifyFinished) {
			return false, true
		}
	case reviewNoticeFail:
		if eff.Notifies(review.NotifyFailed) {
			return false, true
		}
	case reviewNoticeSkip:
		if eff.Notifies(review.NotifyFailed) && (reviewSkipMatters(n.reason) || reviewing) {
			return false, true
		}
	}
	return true, reviewing
}

// notifyReviewChannel announces n for pr in the channel eff names — the settings with the pull
// request's branch rule applied — if it names one and is told of n's event. It never returns an
// error: see the top of the file. It reports whether the message carries n, put there now or by an
// earlier announcement of it, which only a start's hold asks (reviewHold.startSaid).
func (b *Bot) notifyReviewChannel(ctx context.Context, pr *ReviewPR, eff review.Effective, n reviewNotice) bool {
	ch := eff.Notify
	if !ch.Set() || pr == nil {
		return false
	}
	wait := reviewNoticeWait
	if n.kind == reviewNoticeStart {
		wait = reviewNoticeStartWait
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wait)
	defer cancel()
	cur, err := b.store.ReviewPR(ctx, pr.OrgID, pr.ID)
	if err != nil || cur == nil {
		b.reviewNoticeFailed(ctx, pr, ch, n, fmt.Errorf("the pull request could not be read: %v", err))
		return false
	}
	if cur.NotifyLast == n.key() {
		return true // said already: a merge delivered again, a start claimed again
	}
	quiet, ok := reviewNoticeWanted(eff, cur, n)
	if !ok || quiet && (cur.NotifyTS == "" || cur.NotifyTeam != ch.Team || cur.NotifyChannel != ch.Channel) {
		// Nothing to say, or nothing to put back: a quiet word never posts a message of its own.
		return false
	}
	n.quiet = quiet
	if n.kind == reviewNoticeStart && cur.State != "open" {
		return false // closed or merged as it was claimed: its run is being cancelled, and reviews nothing
	}
	if n.kind == reviewNoticeMerged && cur.NotifyTS == "" {
		// Never announced, and — when no review ever finished on it either — nothing to say but that
		// it was merged: recorded, so a redelivery is as quiet, and not posted.
		last, err := b.store.latestReviewOutcome(ctx, cur.OrgID, cur.ID)
		if err == nil && last == nil {
			if err := b.store.setReviewPRNotified(ctx, cur.OrgID, cur.ID, n.key()); err != nil {
				slog.Warn("code review: a merge left unannounced was not recorded", "org", cur.OrgID, "pr", cur.ID, "err", err)
			}
			return false
		}
	}
	posted, err := b.postReviewNotice(ctx, cur, ch, n)
	if err != nil {
		b.reviewNoticeFailed(ctx, cur, ch, n, err)
		return false
	}
	if err := b.store.setReviewPRNotified(ctx, cur.OrgID, cur.ID, n.key()); err != nil {
		slog.Warn("code review: the announcement was made and not recorded", "org", cur.OrgID, "pr", cur.ID, "err", err)
	}
	runID := ""
	if n.run != nil {
		runID = n.run.PublicID
	}
	b.auditSystem(ctx, cur.OrgID, "review.notified", reviewAuditEvent(cur, runID, map[string]any{
		"event": n.kind, "team": ch.Team, "channel": ch.Channel, "message": posted}))
	return true
}

// reviewNoticeFailed is an announcement that did not go out: the log, the audit log, and the alert
// channel once an hour, in the organisation's words rather than the platform's error alone.
func (b *Bot) reviewNoticeFailed(ctx context.Context, pr *ReviewPR, ch review.NotifyChannel, n reviewNotice, err error) {
	slog.Warn("code review: the channel was not told", "org", pr.OrgID, "repo", pr.Repo, "pr", pr.Number, "event", n.kind,
		"team", ch.Team, "channel", ch.Channel, "err", err)
	runID := ""
	if n.run != nil {
		runID = n.run.PublicID
	}
	b.auditSystem(ctx, pr.OrgID, "review.notify_failed", reviewAuditEvent(pr, runID, map[string]any{
		"event": n.kind, "team": ch.Team, "channel": ch.Channel, "error": truncate(err.Error(), 300)}))
	if b.agent != nil {
		b.agent.alert(ctx, pr.OrgID, "review-notify", fmt.Sprintf(":warning: Code review could not post about %s#%d in <#%s>: %s. "+
			"The next review or merge tries again; check that the bot is still in that channel and its workspace is connected.",
			pr.Repo, pr.Number, ch.Channel, truncate(err.Error(), 200)))
	}
}

// postReviewNotice edits the pull request's message to how it stands now and replies under it, or —
// the first time, in a channel other than the one it was posted in, or after somebody deleted it —
// posts that as the message, and then says nothing more: a message new to the channel is the news.
// A quiet word whose message is gone posts nothing: there is nothing left to put back, and a message
// of its own would tell the channel what it chose not to hear. It reports which it did.
func (b *Bot) postReviewNotice(ctx context.Context, pr *ReviewPR, ch review.NotifyChannel, n reviewNotice) (string, error) {
	msg, err := b.reviewNoticeText(ctx, pr, n)
	if err != nil {
		return "", err
	}
	sl, err := b.slacks.For(ctx, ch.Team)
	if err != nil {
		return "", err
	}
	// The setting was checked against the organisation when it was saved; a workspace connected to
	// another organisation since — or one whose owner is not known — is not this one's to post in.
	if sl.OrgID != pr.OrgID {
		return "", fmt.Errorf("workspace %s is not connected to this organisation", ch.Team)
	}
	if sl.IsSharedExternally(ctx, ch.Channel) {
		return "", errors.New("the channel is shared with another organisation (Slack Connect), or could not be checked, " +
			"and a repository's findings are not posted where another company reads them")
	}
	if pr.NotifyTS != "" && pr.NotifyTeam == ch.Team && pr.NotifyChannel == ch.Channel {
		err := sl.UpdateMarkdown(ctx, ch.Channel, pr.NotifyTS, msg.root, "")
		if err == nil {
			if err := reviewNoticeReply(ctx, sl, ch, pr.NotifyTS, msg); err != nil {
				return "", fmt.Errorf("the message was brought up to date, and the reply in its thread was not posted: %w", err)
			}
			b.reviewNoticeMergedSince(ctx, sl, pr, ch, n, msg, pr.NotifyTS)
			return msg.done(), nil
		}
		if !chatMessageGone(err) {
			return "", err
		}
		if n.quiet {
			return "gone", nil
		}
		slog.Info("code review: the pull request's message is gone from the channel; posting it again", "org", pr.OrgID,
			"pr", pr.ID, "channel", ch.Channel, "err", err)
	}
	ts, err := sl.PostMarkdown(ctx, ch.Channel, "", msg.root, "")
	if err != nil {
		return "", err
	}
	took, err := b.store.setReviewPRNotifyRoot(ctx, pr.OrgID, pr.ID, ch.Team, ch.Channel, ts, pr.NotifyTS)
	if err == nil && took {
		b.reviewNoticeMergedSince(ctx, sl, pr, ch, n, msg, ts)
		return "posted", nil
	}
	// Unrecorded, the message would be orphaned in the channel and the next event would post another:
	// it goes. When another event posted first, that one is the pull request's message, and this
	// event is a reply in it.
	if derr := sl.t.deleteMessage(ctx, ch.Channel, ts); derr != nil {
		slog.Warn("code review: a duplicate announcement could not be taken back", "channel", ch.Channel, "ts", ts, "err", derr)
	}
	if err != nil {
		return "", err
	}
	now, err := b.store.ReviewPR(ctx, pr.OrgID, pr.ID)
	if err != nil || now == nil {
		return "", fmt.Errorf("the pull request could not be read again: %v", err)
	}
	if now.NotifyTS == "" || now.NotifyTeam != ch.Team || now.NotifyChannel != ch.Channel {
		return "", errors.New("another announcement of this pull request was being posted elsewhere at the same moment")
	}
	// Drawn again from the pull request as the winner left it: the text this event drew before is
	// older than the winner's, and the winner may have been the merge, whose line it lacks.
	if msg, err = b.reviewNoticeText(ctx, now, n); err != nil {
		return "", err
	}
	if err := sl.UpdateMarkdown(ctx, ch.Channel, now.NotifyTS, msg.root, ""); err != nil {
		return "", err
	}
	if err := reviewNoticeReply(ctx, sl, ch, now.NotifyTS, msg); err != nil {
		return "", err
	}
	b.reviewNoticeMergedSince(ctx, sl, now, ch, n, msg, now.NotifyTS)
	return msg.done(), nil
}

// reviewNoticeReply posts msg's reply in the thread of the message at ts, when it has one: a start
// and a quiet word have none.
func reviewNoticeReply(ctx context.Context, sl *Chat, ch review.NotifyChannel, ts string, msg reviewNoticeMsg) error {
	if msg.reply == "" {
		return nil
	}
	_, err := sl.PostMarkdown(ctx, ch.Channel, ts, msg.reply, "")
	return err
}

// reviewNoticeMergedSince puts the merge back in a message an announcement has just edited with
// text drawn before the merge was recorded: a review ending as its pull request is merged reads the
// pull request open, and its edit can land after the merge's own. The pull request is read once more,
// and only a message that should say merged and does not is edited again. Best effort: the next event
// draws it right anyway.
func (b *Bot) reviewNoticeMergedSince(ctx context.Context, sl *Chat, pr *ReviewPR, ch review.NotifyChannel, n reviewNotice,
	msg reviewNoticeMsg, ts string) {
	if msg.merged {
		return
	}
	now, err := b.store.ReviewPR(ctx, pr.OrgID, pr.ID)
	if err != nil || now == nil || now.State != "merged" || now.NotifyTS != ts {
		return
	}
	again, err := b.reviewNoticeText(ctx, now, n)
	if err == nil {
		err = sl.UpdateMarkdown(ctx, ch.Channel, ts, again.root, "")
	}
	if err != nil {
		slog.Warn("code review: the message could not be brought up to the merge", "org", pr.OrgID, "pr", pr.ID, "err", err)
	}
}

// reviewChannelShared reports whether the platform says a channel is shared with another
// organisation, for the settings to refuse it when it is saved. Unlike IsSharedExternally it answers
// no when it cannot tell — a workspace whose client cannot be built, a lookup that fails — since a
// save is no time to stop an admin over the platform's hiccup: the post's own check
// (postReviewNotice) is the guard, and refuses whatever it cannot clear.
func (b *Bot) reviewChannelShared(ctx context.Context, n review.NotifyChannel) bool {
	sl, err := b.slacks.For(ctx, n.Team)
	if err != nil || sl == nil || sl.t == nil {
		return false
	}
	ci, err := sl.conv(ctx, n.Channel)
	return err == nil && (ci.IsExtShared || ci.IsShared)
}

// chatMessageGone reports whether a platform refused an edit because the message is not there any
// more — somebody deleted it — rather than for a reason another attempt might not meet. Only then is
// the message posted again; anything else is a failure, retried at the next event, rather than a
// second message beside the first.
func chatMessageGone(err error) bool {
	s := err.Error()
	return strings.Contains(s, "message_not_found") || strings.Contains(s, "thread_not_found") ||
		strings.Contains(s, "Teams answered 404")
}

// reviewNoticeMsg is one announcement drawn: the pull request's message as it should read now, the
// reply that says what happened ("" for none), and whether the message says the pull request was
// merged.
type reviewNoticeMsg struct {
	root, reply string
	merged      bool
}

// done is what an edit of the message did, for the audit log.
func (m reviewNoticeMsg) done() string {
	if m.reply == "" {
		return "edited"
	}
	return "edited and replied"
}

// reviewNoticeText draws n for pr. Whether the pull request was merged is the pull request's, not the
// event's: a review that ends after the merge — a shadow run already with the model, a live one
// already posting — is announced on a message that must go on saying it was merged. Who merged it is
// only the merge's to say; a later edit leaves the name to that reply in the thread.
func (b *Bot) reviewNoticeText(ctx context.Context, pr *ReviewPR, n reviewNotice) (reviewNoticeMsg, error) {
	var none reviewNoticeMsg
	// A finished review is drawn on the run as it ended, read back: the lane's copy of it still says
	// running. Anything else on the last review that was posted or recorded — for a run still going,
	// or one that came to nothing, that is how the pull request stands.
	var last *ReviewRun
	var err error
	if n.kind == reviewNoticeReview {
		last, err = b.store.ReviewRun(ctx, pr.OrgID, n.run.ID)
	} else {
		last, err = b.store.latestReviewOutcome(ctx, pr.OrgID, pr.ID)
	}
	if err != nil {
		return none, err
	}
	if n.kind == reviewNoticeReview && last == nil {
		return none, fmt.Errorf("review run %s could not be read back", n.run.PublicID)
	}
	v := reviewNoticeView{Repo: pr.Repo, Number: pr.Number, Title: n.title, Base: n.base, Head: n.head, Score: pr.Score,
		SummaryCommentID: pr.SummaryCommentID, Merged: n.kind == reviewNoticeMerged || pr.State == "merged"}
	if n.kind == reviewNoticeMerged {
		v.MergedBy = n.mergedBy
	}
	if last != nil {
		v.ReviewedSHA, v.Shadow = last.HeadSHA, last.Status == "shadow"
		said, err := b.store.reviewFindingsSaid(ctx, pr.OrgID, pr.ID, v.Shadow, last.ID)
		if err != nil {
			return none, err
		}
		for _, f := range said {
			if (f.Status == review.FindingOpen || f.Status == review.FindingDisputed) && !f.PreExisting && f.Kind != reviewKindNote {
				v.Open = append(v.Open, f)
			}
		}
		v.ConsoleURL = b.reviewConsoleURL(ctx, last.PublicID)
	}
	if !n.quiet && !(n.kind == reviewNoticeStart && v.Merged) {
		// A start drawn again over a merge that landed as it was posted (reviewNoticeMergedSince) is
		// a review that will not happen: the merge cancels it, and its line would outlive it.
		v.Activity = reviewNoticeActivity(pr, n)
	}
	root, reply := reviewNoticeRoot(v), ""
	switch {
	case n.quiet || n.kind == reviewNoticeStart || n.kind == reviewNoticeEnd:
		// The message alone: see the top of the file.
	case n.kind == reviewNoticeFail || n.kind == reviewNoticeSkip:
		reply = v.Activity
		if u := b.reviewConsoleURL(ctx, runPublicID(n.run)); u != "" && n.kind == reviewNoticeFail {
			// The error itself is the console's to show, not the channel's.
			reply += " [In the console](" + u + ")"
		}
	case n.kind == reviewNoticeMerged:
		state := "merged without a review"
		if last != nil {
			state = reviewOpenLine(v.Open, "still open")
		}
		reply = "Merged into " + reviewChatCode(n.base)
		if by := reviewChatText(n.mergedBy, 60); by != "" {
			reply += " by " + by
		}
		reply += " — " + state + "."
	default:
		verb := "Reviewed"
		if n.again {
			verb = "Re-reviewed"
		}
		score := "no score"
		if pr.Score >= 0 {
			score = fmt.Sprintf("score %d", pr.Score)
			if n.again && n.before >= 0 && n.before != pr.Score {
				score = fmt.Sprintf("score %d → %d", n.before, pr.Score)
			}
		}
		reply = fmt.Sprintf("%s %s: %d fixed, %d open, %s", verb, reviewChatCode(shortSHA(last.HeadSHA)), n.fixed, len(v.Open), score)
		if v.Shadow {
			reply += " (shadow)"
		}
		reply += "."
	}
	return reviewNoticeMsg{root: root, reply: reply, merged: v.Merged}, nil
}

// reviewNoticeActivity is the message's line for what is happening to the pull request: a review
// under way — "Reviewing `abc1234` (General, Security)…", "in shadow" where it is recorded rather
// than posted — or the run that failed or did not run, and why. Type names are a team's to write,
// and held to a line like any other text that is not Go's.
func reviewNoticeActivity(pr *ReviewPR, n reviewNotice) string {
	at := ""
	if sha := shortSHA(cmp.Or(n.sha, pr.HeadSHA)); sha != "" {
		at = " " + reviewChatCode(sha)
	}
	what := "Review"
	if n.shadow {
		what = "Shadow review"
	}
	if at != "" {
		what += " of" + at
	}
	switch n.kind {
	case reviewNoticeStart:
		var names []string
		for _, t := range n.types {
			if name := reviewChatText(t, 40); name != "" {
				names = append(names, name)
			}
		}
		line := "Reviewing" + at
		if len(names) > 0 {
			line += " (" + strings.Join(names, ", ") + ")"
		}
		if n.shadow {
			line += " in shadow"
		}
		return line + "…"
	case reviewNoticeFail:
		return what + " failed: " + n.why + "."
	case reviewNoticeSkip:
		return what + " skipped: " + reviewSkipWhy(pr, n.reason) + "."
	}
	return ""
}

// reviewSkipMatters reports whether a review the gate stopped for reason is news to the pull
// request's channel by itself: somebody is waiting for a review that is not coming, and only a person
// can change that — raise the budget, resume the pull request, move the plan. Every other reason is
// the settings doing what they were set to.
func reviewSkipMatters(reason string) bool {
	return reason == "budget" || reason == "paused" || reason == "plan"
}

// reviewSkipWhy is a skip's reason in the channel's words. The money's own refusal names amounts,
// which are the admins' to read in their alert channel, not everybody's here.
func reviewSkipWhy(pr *ReviewPR, reason string) string {
	switch reason {
	case "budget":
		return "the organisation's code review budget is spent for now"
	case "paused":
		if pr.PausedAuto {
			return fmt.Sprintf("automatic reviews of this pull request paused after %d; one somebody asks for still runs", reviewAutoPauseAfter)
		}
		return "automatic reviews of this pull request are paused; one somebody asks for still runs"
	case "plan":
		return "this organisation's plan does not include code review"
	case "nothing_to_review":
		return "every changed file is ignored, binary or generated"
	}
	return "the console says why"
}

// reviewFailWhy is a failed run's reason in the channel's words: the kind of failure its recorded
// error starts with (reviewRunError), never the error, which is the deployment's — a provider's
// reply, a request id — and stays in the console.
func reviewFailWhy(errText string) string {
	reason, _, _ := strings.Cut(errText, ":")
	return review.FailReason(strings.TrimSpace(reason)).Text()
}

// runPublicID is r's public id, "" for none: a review the gate stopped never had a run.
func runPublicID(r *ReviewRun) string {
	if r == nil {
		return ""
	}
	return r.PublicID
}

// reviewConsoleURL is a run's page in the console, for a channel to link to; "" without a public
// https origin, since the fallback is the process's own listen address, which in a channel is a link
// to nowhere for everybody but whoever runs the bot.
func (b *Bot) reviewConsoleURL(ctx context.Context, runID string) string {
	if runID == "" {
		return ""
	}
	base := publicBaseURL(ctx, b.store, b.cfg)
	if !strings.HasPrefix(base, "https://") {
		return ""
	}
	return strings.TrimRight(base, "/") + "/admin/reviews/?tab=history&run=" + runID
}

// reviewNoticeEffective is eff as a pull request on pull's branches is announced under: with the
// branch rule they meet applied, as its review would be (planReview), since a rule may send its pull
// requests to a channel of their own or to none.
func reviewNoticeEffective(eff review.Effective, pull *githubPull) review.Effective {
	if _, rule, ok := review.MatchRule(eff.BranchRules, pull.Base.Ref, pull.Head.Ref); ok {
		return eff.WithRule(rule)
	}
	return eff
}

// ---- the lane's side ----

// reviewNoticeRun is what a claimed run's announcements need beyond the run, once processReview has
// planned it: the settings it is announced under, with its branch rule applied, what GitHub said of
// the pull request, and the commit it reviews and where its result goes.
type reviewNoticeRun struct {
	eff               review.Effective
	title, base, head string
	sha               string
	shadow            bool
}

// noticeReviewStarted tells the channel a claimed run is under way, beside the work rather than in
// front of it: a review is minutes of model calls, and a chat platform that is slow or down must not
// hold a second of them. Everything the run says after it waits for it (reviewHold.waitNotice), so
// the message never goes back to "Reviewing…" after the run's own result. The run and the pull
// request are copied — the work goes on changing the lane's — the run under the hold's lock, which
// the lease's renewal writes it under.
func (b *Bot) noticeReviewStarted(lane context.Context, h *reviewHold, pr *ReviewPR, specs []reviewTypeSpec) {
	t := h.notice
	if t == nil || h.run.Kind != "review" || !t.eff.Notify.Set() || !t.eff.Notifies(review.NotifyStarted) || h.noticed != nil {
		return
	}
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, cmp.Or(s.Name, s.Key))
	}
	h.mu.Lock()
	run := *h.run
	h.mu.Unlock()
	row, done := *pr, make(chan struct{})
	h.noticed = done
	go func() {
		defer close(done)
		h.startSaid = b.notifyReviewChannel(lane, &row, t.eff, reviewNotice{kind: reviewNoticeStart, run: &run, before: -1,
			title: t.title, base: t.base, head: t.head, sha: t.sha, types: names, shadow: t.shadow})
	}()
}

// waitNotice waits for the run's start to be announced, if one is on its way. It is bounded by
// reviewNoticeStartWait.
func (h *reviewHold) waitNotice() {
	if h.noticed != nil {
		<-h.noticed
	}
}

// noticeRunEnded tells the channel how a review run ended, once that is recorded and the run is over:
// a failure, a skip, or — after its start was announced — an ending that only puts the message back
// (reviewNoticeWanted). A run posted or recorded is publishReview's to announce, with what it found.
// One that ended before this attempt planned it has no channel of its own to tell — which one depends
// on its branches — and never said it had started; but an attempt before it may have, before the run
// was put back, and that message is put back the way one the lane never saw end is.
func (b *Bot) noticeRunEnded(lane context.Context, h *reviewHold, res ReviewRunResult) {
	if h.run.Kind != "review" || res.Status == "posted" || res.Status == "shadow" {
		return
	}
	h.waitNotice()
	r, t := h.run, h.notice
	if t == nil {
		b.noticeStartOrphaned(lane, r.OrgID, r.ReviewPRID)
		return
	}
	n := reviewEndNotice(r, res.Status, res.Error)
	n.title, n.base, n.head, n.sha, n.shadow, n.started = t.title, t.base, t.head, cmp.Or(t.sha, r.HeadSHA), t.shadow, h.startSaid
	b.notifyReviewChannel(lane, &ReviewPR{OrgID: r.OrgID, ID: r.ReviewPRID, Repo: r.Repo, Number: r.PRNumber}, t.eff, n)
}

// reviewEndNotice is how a run's ending is told: a failure by its kind, a skip by its reason, and any
// other ending — cancelled, superseded, answered from an earlier review — an end, which only puts the
// message back.
func reviewEndNotice(r *ReviewRun, status, errText string) reviewNotice {
	n := reviewNotice{kind: reviewNoticeEnd, run: r, before: -1, sha: r.HeadSHA}
	switch status {
	case "failed":
		n.kind, n.why = reviewNoticeFail, reviewFailWhy(errText)
	case "skipped":
		n.kind = reviewNoticeSkip
		n.reason, _, _ = strings.Cut(errText, ":")
	}
	return n
}

// noticeStartOrphaned puts a pull request's message back when it still says a review is under way
// and that review is over, ended where no lane told its channel: put back after its start was
// announced — GitHub asking for a wait, a retry, the instance stopping — and then ended before an
// attempt planned it again (the pull request closed, the repository turned off, GitHub failing the
// read), cancelled in the queue as its pull request closed, or retired by the sweep once its lane died
// on its last attempt. None of those has read GitHub, nor needs to: the message is where review_prs
// says it is, and is drawn with the title and branches the run was asked for under (reviewOptions).
// The settings as they are now decide only whether a failure is a word or a quiet put-back, and only
// while they still send the pull request to that channel; one they no longer do has it put back, and
// hears nothing. A run still queued or running says how it ends itself.
func (b *Bot) noticeStartOrphaned(ctx context.Context, orgID, prID int64) {
	pr, err := b.store.ReviewPR(ctx, orgID, prID)
	if err != nil || pr == nil || pr.NotifyTS == "" {
		return
	}
	id, ok := strings.CutPrefix(pr.NotifyLast, reviewNoticeStart+":")
	if !ok {
		return
	}
	run, err := b.store.ReviewRunByPublicID(ctx, orgID, id)
	if err != nil || run == nil || run.ReviewPRID != pr.ID || run.Kind != "review" || !slices.Contains(reviewRunEnds, run.Status) {
		return
	}
	o := reviewOptionsOf(run)
	ch := review.NotifyChannel{Team: pr.NotifyTeam, Channel: pr.NotifyChannel}
	eff := review.Effective{Notify: ch, NotifyOn: []review.NotifyEvent{}}
	if now, s, err := b.reviewEffective(ctx, orgID, run.InstallationID, pr.Repo); err == nil && s == nil {
		at := &githubPull{Base: githubPullRef{Ref: o.Base}, Head: githubPullRef{Ref: o.Head}}
		if e := reviewNoticeEffective(now, at); e.Notify == ch {
			eff = e
		}
	}
	n := reviewEndNotice(run, run.Status, run.Error)
	n.title, n.base, n.head, n.shadow = o.Title, o.Base, o.Head, cmp.Or(o.Post, eff.Mode) != review.ModeLive
	b.notifyReviewChannel(ctx, pr, eff, n)
}

// noticeReviewSkipped tells a pull request's channel the gate stopped a review it would have had,
// where that is news (reviewSkipMatters). There is no run: the message says which head was not
// reviewed and why. eff has the pull request's branch rule applied.
func (b *Bot) noticeReviewSkipped(ctx context.Context, pr *ReviewPR, eff review.Effective, pull *githubPull, sha string, s *reviewSkip) {
	if !reviewSkipMatters(s.Reason) {
		return
	}
	b.notifyReviewChannel(ctx, pr, eff, reviewNotice{kind: reviewNoticeSkip, reason: s.Reason, before: -1, title: pull.Title,
		base: pull.Base.Ref, head: pull.Head.Ref, sha: sha, shadow: eff.Mode != review.ModeLive})
}

// reviewNoticeView is what a pull request's message is drawn from.
type reviewNoticeView struct {
	Repo              string
	Number            int
	Title, Base, Head string
	ReviewedSHA       string // "" when it was never reviewed
	Score             int
	Shadow            bool
	// Activity is what is happening to the pull request now, a line of its own: a review under
	// way, or the last one failed or not run (reviewNoticeActivity). "" says nothing.
	Activity         string
	Open             []*ReviewFinding
	SummaryCommentID int64
	ConsoleURL       string
	Merged           bool
	MergedBy         string
}

// reviewNoticeRoot renders the message: the pull request and its title, its branches and how its
// review stands, what is happening to it, what is open and the worst three of it, and where to read
// more. Markdown, in the subset Slack's markdown block and Teams both draw. The activity is the
// third line, where a channel that folds a long message still shows it.
func reviewNoticeRoot(v reviewNoticeView) string {
	pull := fmt.Sprintf("https://github.com/%s/pull/%d", v.Repo, v.Number)
	var sb strings.Builder
	fmt.Fprintf(&sb, "**[%s#%d](%s)**", v.Repo, v.Number, pull)
	if t := reviewChatText(v.Title, 120); t != "" {
		sb.WriteString(" " + t)
	}
	sb.WriteString("\n" + reviewChatCode(cmp.Or(v.Head, "?")) + " → " + reviewChatCode(cmp.Or(v.Base, "?")))
	if v.ReviewedSHA == "" {
		sb.WriteString(" · not reviewed")
	} else {
		sb.WriteString(" · last reviewed " + reviewChatCode(shortSHA(v.ReviewedSHA)))
		if v.Score >= 0 {
			fmt.Fprintf(&sb, " · Confidence %d/5", v.Score)
		}
		if v.Shadow {
			sb.WriteString(" · shadow")
		}
	}
	if v.Activity != "" {
		sb.WriteString("\n" + v.Activity)
	}
	if v.ReviewedSHA != "" {
		sb.WriteString("\n" + reviewOpenLine(v.Open, "open"))
		open := slices.Clone(v.Open)
		slices.SortStableFunc(open, func(a, b *ReviewFinding) int { return strings.Compare(string(a.Severity), string(b.Severity)) })
		for _, f := range open[:min(len(open), reviewNoticeTop)] {
			at := fmt.Sprintf("%s:%d", f.Path, f.Line)
			fmt.Fprintf(&sb, "\n• **%s** %s · [%s](https://github.com/%s/blob/%s/%s#L%d)", f.Severity, reviewChatText(f.Title, 100),
				reviewChatText(at, 120), v.Repo, cmp.Or(f.AnchorSHA, v.ReviewedSHA), reviewPathURL(f.Path), f.Line)
		}
		if len(open) > reviewNoticeTop {
			fmt.Fprintf(&sb, "\n…and %d more", len(open)-reviewNoticeTop)
		}
	}
	var links []string
	if v.SummaryCommentID > 0 {
		links = append(links, fmt.Sprintf("[Summary on GitHub](%s#issuecomment-%d)", pull, v.SummaryCommentID))
	}
	if v.ConsoleURL != "" {
		links = append(links, fmt.Sprintf("[In the console](%s)", v.ConsoleURL))
	}
	if len(links) > 0 {
		sb.WriteString("\n" + strings.Join(links, " · "))
	}
	if v.Merged {
		sb.WriteString("\nMerged into " + reviewChatCode(cmp.Or(v.Base, "?")))
		if by := reviewChatText(v.MergedBy, 60); by != "" {
			sb.WriteString(" by " + by)
		}
		sb.WriteString(".")
	}
	return sb.String()
}

// reviewOpenLine counts open findings by severity, worst first: "1 P1, 2 P2 open".
func reviewOpenLine(open []*ReviewFinding, what string) string {
	if len(open) == 0 {
		if what == "open" {
			return "No open findings"
		}
		return "nothing open"
	}
	var parts []string
	for _, s := range []review.Severity{review.P0, review.P1, review.P2} {
		if n := len(slices.DeleteFunc(slices.Clone(open), func(f *ReviewFinding) bool { return f.Severity != s })); n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, s))
		}
	}
	return strings.Join(parts, ", ") + " " + what
}

// reviewChatText is text a pull request or a model wrote, made safe to put in a channel: one line,
// at most max characters, and nothing in it that a chat platform would read as more than text —
// no <…> markup (a <!channel> ping, a link whose label lies), no @ that could become a mention,
// and no [label](url) of its own.
func reviewChatText(s string, max int) string {
	s = strings.TrimSpace(oneLine(s))
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max-1]) + "…"
	}
	s = escapeMrkdwn(s)
	// A word joiner after every @ keeps "@here" and "@someone" reading as written and pinging
	// nobody, on either platform, and one inside "](" breaks a [label](url) without touching the
	// "[WIP]" a title usually has brackets for.
	return strings.NewReplacer("@", "@\u2060", "](", "]\u2060(", "`", "'").Replace(s)
}

// reviewChatCode is a branch name or a commit as code. A branch is the author's to name, and a
// backtick in one would end the code span early, so it is held to reviewChatText first.
func reviewChatCode(s string) string { return "`" + reviewChatText(s, 80) + "`" }

// reviewPathURL is a repository path as the tail of a blob link: each segment escaped, the slashes
// kept.
func reviewPathURL(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
