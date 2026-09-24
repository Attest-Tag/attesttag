package app

import (
	"context"
	"database/sql"
	"slices"
	"time"
)

// The store side of a second factor: the sealed TOTP secret, the recovery codes, and the
// half-finished sign-ins waiting on a code. Sealing and verifying live in the app (totp.go,
// account.go); nothing here can read a secret it holds.

// TOTPEnrolment is one account's second factor as the database has it.
type TOTPEnrolment struct {
	SecretEnc []byte
	Confirmed bool
	LastStep  int64
}

// StartTOTP records a new, unconfirmed secret, replacing any enrolment that never finished.
// A confirmed one is left alone: turning off a working second factor is its own deliberate act
// (DisableTOTP), never a side effect of opening the enrolment panel again.
func (s *Store) StartTOTP(ctx context.Context, userID int64, secretEnc []byte) error {
	if _, err := s.db.ExecContext(ctx, `delete from user_totp where user_id=? and confirmed_at is null`, userID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `insert into user_totp (user_id, secret_enc, created_at) values (?, ?, ?)
		on conflict(user_id) do nothing`, userID, secretEnc, now())
	return err
}

func (s *Store) TOTPEnrolment(ctx context.Context, userID int64) (*TOTPEnrolment, error) {
	var e TOTPEnrolment
	var confirmed sql.NullString
	err := s.db.QueryRowContext(ctx, `select secret_enc, confirmed_at, coalesce(last_step,0)
		from user_totp where user_id=?`, userID).Scan(&e.SecretEnc, &confirmed, &e.LastStep)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	e.Confirmed = confirmed.Valid && confirmed.String != ""
	return &e, err
}

// ConfirmTOTP turns an enrolment into a second factor, spending the step that proved it.
func (s *Store) ConfirmTOTP(ctx context.Context, userID, step int64) error {
	_, err := s.db.ExecContext(ctx, `update user_totp set confirmed_at=?, last_step=? where user_id=?`,
		now(), step, userID)
	return err
}

// SpendTOTPStep remembers the window a code came from, so the same code cannot be used twice.
func (s *Store) SpendTOTPStep(ctx context.Context, userID, step int64) error {
	_, err := s.db.ExecContext(ctx, `update user_totp set last_step=? where user_id=? and last_step<?`,
		step, userID, step)
	return err
}

// DisableTOTP takes the second factor off an account, recovery codes and all.
func (s *Store) DisableTOTP(ctx context.Context, userID int64) error {
	if _, err := s.db.ExecContext(ctx, `delete from user_totp where user_id=?`, userID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `delete from user_recovery_codes where user_id=?`, userID)
	return err
}

// TwoFactorUsers answers, for a list of accounts, which of them hold a confirmed second factor.
// One query rather than one per row: the Users tab shows this beside every member.
func (s *Store) TwoFactorUsers(ctx context.Context) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, `select user_id from user_totp where confirmed_at is not null`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ---- recovery codes ----

// PutRecoveryCodes replaces the set: issuing new ones retires every old one, used or not, so
// there is never a second list in circulation that somebody has forgotten about.
func (s *Store) PutRecoveryCodes(ctx context.Context, userID int64, hashes []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `delete from user_recovery_codes where user_id=?`, userID); err != nil {
		return err
	}
	for _, h := range hashes {
		if _, err := tx.ExecContext(ctx, `insert into user_recovery_codes (user_id, code_hash) values (?, ?)`, userID, h); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SpendRecoveryCode marks one code used and says whether it was there to spend. The update is
// the test — `used_at is null` in the statement rather than a read followed by a write — so two
// sign-ins racing on the same code cannot both win it.
func (s *Store) SpendRecoveryCode(ctx context.Context, userID int64, hash string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `update user_recovery_codes set used_at=?
		where user_id=? and code_hash=? and used_at is null`, now(), userID, hash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// RecoveryCodesLeft is what the console shows: how many are still worth keeping.
func (s *Store) RecoveryCodesLeft(ctx context.Context, userID int64) int {
	var n int
	s.db.QueryRowContext(ctx, `select count(*) from user_recovery_codes where user_id=? and used_at is null`, userID).Scan(&n)
	return n
}

// ---- login challenges ----

const (
	challengeTTL   = 5 * time.Minute
	challengeTries = 6 // wrong codes before the challenge is burned and they start again
)

// NewLoginChallenge issues the one-shot token that stands between a correct password and a
// session. Old challenges for the same account go with it: only the sign-in in front of you is
// live, so a challenge left open on another machine cannot be finished later.
//
// The challenge remembers how the first factor was proved, because the session the code opens is
// that sign-in's and the organisation's policy is asked about it by name. It used to remember
// Slack or nothing, which made every single sign-on with a second factor a password sign-in — and
// refused outright by an organisation that accepts single sign-on only.
func (s *Store) NewLoginChallenge(ctx context.Context, userID int64, methods ...string) (string, error) {
	if _, err := s.db.ExecContext(ctx, `delete from login_challenges where user_id=? or expires_at<=?`, userID, now()); err != nil {
		return "", err
	}
	tok := randomToken()
	via := "password"
	if len(methods) > 0 && slices.Contains([]string{"slack", ProviderMicrosoft, "sso"}, methods[0]) {
		via = methods[0]
	}
	_, err := s.db.ExecContext(ctx, `insert into login_challenges (token, user_id, expires_at, via) values (?, ?, ?, ?)`,
		tok, userID, time.Now().Add(challengeTTL).UTC().Format(time.DateTime), via)
	return tok, err
}

// LoginChallenge resolves a live challenge to the account it belongs to, counting the attempt.
// It returns 0 for a token that is unknown, expired, or has been guessed at too often — the
// caller cannot tell those apart, and neither can anybody trying them.
func (s *Store) LoginChallenge(ctx context.Context, tok string) (int64, error) {
	if tok == "" {
		return 0, nil
	}
	var userID int64
	var tries int
	err := s.db.QueryRowContext(ctx, `select user_id, tries from login_challenges where token=? and expires_at>?`,
		tok, now()).Scan(&userID, &tries)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if tries >= challengeTries {
		s.DeleteLoginChallenge(ctx, tok)
		return 0, nil
	}
	s.db.ExecContext(ctx, `update login_challenges set tries=tries+1 where token=?`, tok)
	return userID, nil
}

func (s *Store) DeleteLoginChallenge(ctx context.Context, tok string) {
	s.db.ExecContext(ctx, `delete from login_challenges where token=?`, tok)
}

func (s *Store) loginChallengeVia(ctx context.Context, tok string) (string, error) {
	var via string
	err := s.db.QueryRowContext(ctx, `select via from login_challenges where token=? and expires_at>?`, tok, now()).Scan(&via)
	return via, err
}
