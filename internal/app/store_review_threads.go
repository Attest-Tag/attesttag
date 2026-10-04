package app

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"attesttag/internal/review"
)

// The statements behind code review's conversation on a pull request: the commands people write to
// the bot (review_commands.go) and the replies in a finding's thread (review_replies.go). What a
// reply changes about a finding is written here, each naming the organisation; the run that answers
// a reply holds its pull request's lease like any other run, and its own checkpoint is fenced on it
// (saveReviewCheckpoint), so a lane that lost the run never writes its answer twice.

// ReviewFindingByPublicID is one finding of one pull request by its public id; nil when it is not
// this organisation's, or not on that pull request.
func (s *Store) ReviewFindingByPublicID(ctx context.Context, orgID, prID int64, publicID string) (*ReviewFinding, error) {
	if publicID == "" {
		return nil, nil
	}
	return s.reviewFindingWhere(ctx, orgID, `review_pr_id=? and public_id=?`, prID, publicID)
}

// SetReviewFindingSeverity lowers or raises a finding's severity — a reply argued it down — with why
// and on whose word. The fingerprint leaves severity out, so the finding stays the same finding.
func (s *Store) SetReviewFindingSeverity(ctx context.Context, orgID, id int64, sev review.Severity, reason, by string) error {
	if !sev.Valid() {
		return ErrReviewRunInvalid
	}
	res, err := s.db.ExecContext(ctx, `update review_findings set severity=?, status_reason=?, status_by=?, updated_at=?
		where org_id=? and id=?`, string(sev), truncate(strings.TrimSpace(reason), 600), truncate(by, 200), now(), orgID, id)
	return reviewFindingWritten(res, err)
}

// SetReviewFindingClaim records a reply's "fixed in <sha>": the commit it names, or the head when
// it names none, and who said so. Nothing is closed on it — the summary lists the finding as claimed
// fixed and it still counts until the next review of a newer head checks it (review_resolve.go).
func (s *Store) SetReviewFindingClaim(ctx context.Context, orgID, id int64, sha, by string) error {
	res, err := s.db.ExecContext(ctx, `update review_findings set claimed_fixed_sha=?, claimed_by=?, updated_at=?
		where org_id=? and id=?`, truncate(sha, 64), truncate(by, 200), now(), orgID, id)
	return reviewFindingWritten(res, err)
}

// addReviewFindingBotReply counts one more answer by the bot in a finding's thread, which is capped.
func (s *Store) addReviewFindingBotReply(ctx context.Context, orgID, id int64) error {
	res, err := s.db.ExecContext(ctx, `update review_findings set bot_replies=bot_replies+1, updated_at=?
		where org_id=? and id=?`, now(), orgID, id)
	return reviewFindingWritten(res, err)
}

// reviewFullRunsSince counts the full reviews asked for on a pull request's head since a time in the
// stored format: the ones that are queued, running or came to something, since a full review that
// failed or was refused answered nothing and the person may ask again. A full review is known by its
// dedupe key (enqueueReview), which is the only thing on the row that says so.
func (s *Store) reviewFullRunsSince(ctx context.Context, orgID, prID int64, head, since string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `select count(*) from review_runs
		where org_id=? and review_pr_id=? and kind='review' and dedupe_key like 'full:%' and head_sha=? and created_at>=?
		  and status not in ('cancelled','skipped','failed','superseded')`, orgID, prID, head, since).Scan(&n)
	return n, err
}

// reviewActiveRun is the newest review of a pull request still queued or running, or nil: what the
// status command says is on its way.
func (s *Store) reviewActiveRun(ctx context.Context, orgID, prID int64) (*ReviewRun, error) {
	r, err := scanReviewRun(s.db.QueryRowContext(ctx, `select `+reviewRunCols+` from review_runs
		where org_id=? and review_pr_id=? and kind='review' and status in ('queued','running') and cancel=0
		order by id desc limit 1`, orgID, prID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// reviewRepliesStarted counts the reply runs on a pull request that got as far as the model since a
// time in the stored format, other than except: the per-day cap on replies, which is also the cap on
// what a thread nobody is watching — two bots, a person arguing for sport — can spend.
func (s *Store) reviewRepliesStarted(ctx context.Context, orgID, prID int64, since string, except int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `select count(*) from review_runs
		where org_id=? and review_pr_id=? and kind='reply' and outcome_json<>'{}' and started_at>=? and id<>?`,
		orgID, prID, since, except).Scan(&n)
	return n, err
}

// reviewReplyRuns lists a pull request's reply runs that got as far as the model, newest first, at
// most limit: what earlier replies in a finding's thread came to, which is how a second pushback is
// told from a first.
func (s *Store) reviewReplyRuns(ctx context.Context, orgID, prID int64, limit int) ([]*ReviewRun, error) {
	rows, err := s.db.QueryContext(ctx, `select `+reviewRunCols+` from review_runs
		where org_id=? and review_pr_id=? and kind='reply' and outcome_json<>'{}' order by id desc limit ?`, orgID, prID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ReviewRun
	for rows.Next() {
		r, err := scanReviewRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// setReviewPRScore records the score a resync computed from the findings as they stand now — a
// withdrawal, a downgrade, a thread a person resolved — so the pull request's row, which the console
// and the status command read, moves with the summary. Fenced on the lease the run holds the pull
// request under.
func (s *Store) setReviewPRScore(ctx context.Context, r *ReviewRun, score int) error {
	if score < -1 || score > 5 {
		return ErrReviewRunInvalid
	}
	res, err := s.db.ExecContext(ctx, `update review_prs set score=?, updated_at=? where org_id=? and id=? and lease_until=?`,
		score, now(), r.OrgID, r.ReviewPRID, r.Lease)
	return fencedWrite(res, err)
}

// proposeLearnedReviewRule adds a rule learned from a reply to a review type, as proposed: it reaches
// no prompt until somebody approves it in the console. The type is the organisation's row — its copy
// of a built-in, made here if there is none yet (copy-on-write), or its own type. A rule already
// learned from the same comment is not added twice: a reply run that is resumed after it proposed
// the rule must not propose it again. It returns the type the rule went into and whether it was
// added; ErrReviewTypeInvalid means the type has no room, or the rule is not one a type can hold.
func (s *Store) proposeLearnedReviewRule(ctx context.Context, orgID int64, typeKey, text, fromURL, by string) (*ReviewType, bool, error) {
	for attempt := 0; ; attempt++ {
		t, err := s.ReviewTypeByKey(ctx, orgID, typeKey)
		if err != nil {
			return nil, false, err
		}
		if t == nil {
			bt, ok := review.BuiltinType(typeKey)
			if !ok {
				return nil, false, ErrReviewTypeNotFound
			}
			if t, err = s.CopyBuiltinReviewType(ctx, orgID, reviewTypeRowOfBuiltin(bt), by); err != nil {
				return nil, false, err
			}
		}
		if fromURL != "" && slices.ContainsFunc(t.Rules, func(r ReviewTypeRule) bool { return r.FromCommentURL == fromURL }) {
			return t, false, nil
		}
		edit := *t
		edit.Rules = append(slices.Clone(t.Rules), ReviewTypeRule{Text: text, Enabled: true, Source: review.RuleLearned,
			Status: "proposed", FromCommentURL: truncate(fromURL, 500)})
		saved, err := s.SaveReviewType(ctx, orgID, &edit, by)
		if errors.Is(err, ErrReviewTypeStale) && attempt == 0 {
			continue // somebody saved the type in the same moment: once more, on top of what they saved
		}
		if err != nil {
			return nil, false, err
		}
		return saved, true, nil
	}
}

// reviewTypeRowOfBuiltin is a built-in type in the shape storage keeps a type in, for the
// organisation's copy-on-write copy of it.
func reviewTypeRowOfBuiltin(t review.Type) *ReviewType {
	row := &ReviewType{Key: t.Key, Name: t.Name, Purpose: t.Purpose, PathGlobs: slices.Clone(t.PathGlobs),
		Strictness: string(t.Strictness), InlineMinSeverity: string(t.InlineMinSeverity), Enabled: true,
		Skills: slices.Clone(t.Skills)}
	for _, r := range t.Rules {
		row.Rules = append(row.Rules, ReviewTypeRule{Text: r.Text, SeverityCap: string(r.SeverityCap),
			PathGlobs: slices.Clone(r.PathGlobs), ExampleBad: r.ExampleBad, ExampleGood: r.ExampleGood, Enabled: !r.Off,
			Source: review.RuleBuiltin, Status: "active"})
	}
	return row
}
