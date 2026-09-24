package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Storage for one person's own notes. The organisation's memory lives in `memories` and is read
// by scope; these are read by owner, and the two never meet. What makes that structural rather
// than remembered is below: every method takes a personalKey, so a query cannot be written with
// three strings that happened to be in scope, and there is no method that returns more than one
// person's rows. There is deliberately no AllPersonalMemories.

// personalKey is who a note belongs to. The fields are unexported and there are exactly two
// constructors: (*Call).personalKey, which is a person typing in Slack on this turn, and
// slackSelves, which the console reaches only after resolving the caller's own proved Slack
// identity. Everything else in the process can hold a Call with a real-looking user id on it --
// a routine runs as whoever wrote it, a preview as whoever opened the console -- and none of
// them can build one of these.
type personalKey struct {
	orgID  int64
	teamID string
	owner  string
}

func (k personalKey) ok() bool { return k.orgID != 0 && k.teamID != "" && k.owner != "" }

type PersonalMemory struct {
	ID                   int64
	OrgID                int64
	TeamID, Owner        string
	Text                 string
	CreatedAt, UpdatedAt string
}

const personalMemoryCols = `id, org_id, team_id, owner, text, created_at, coalesce(updated_at,'')`

func scanPersonalMemory(row interface{ Scan(...any) error }) (*PersonalMemory, error) {
	var v PersonalMemory
	if err := row.Scan(&v.ID, &v.OrgID, &v.TeamID, &v.Owner, &v.Text, &v.CreatedAt, &v.UpdatedAt); err != nil {
		return nil, err
	}
	return &v, nil
}

// errNoOwner is what an unowned key gets, everywhere, rather than an empty result. An empty list
// reads as "you have nothing saved", which is a different and wrong answer, and the caller that
// believed it would go on to write a row nobody owns.
var errNoOwner = errors.New("there is nobody on this turn to keep a private note for")

func (s *Store) AddPersonalMemory(ctx context.Context, k personalKey, text string) (int64, error) {
	if !k.ok() {
		return 0, errNoOwner
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, errors.New("text is required")
	}
	n, err := s.CountPersonalMemories(ctx, k)
	if err != nil {
		return 0, err
	}
	if n >= maxPersonalNotesPerUser {
		return 0, errPersonalCap
	}
	if s.countRows(ctx, "personal_memories", k.orgID) >= maxPersonalNotesPerOrg {
		return 0, errOrgCap
	}
	var id int64
	err = s.db.QueryRowContext(ctx, `insert into personal_memories (org_id, team_id, owner, text)
		values (?, ?, ?, ?) returning id`, k.orgID, k.teamID, k.owner, text).Scan(&id)
	return id, err
}

func (s *Store) PersonalMemories(ctx context.Context, k personalKey) ([]*PersonalMemory, error) {
	if !k.ok() {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `select `+personalMemoryCols+` from personal_memories
		where org_id=? and team_id=? and owner=? order by id`, k.orgID, k.teamID, k.owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*PersonalMemory{}
	for rows.Next() {
		v, err := scanPersonalMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// PersonalMemoryByID reports nil, nil when this person has no note with that id -- including when
// somebody else does. A caller cannot tell the two apart, which is the point: an id that is not
// yours is missing, not forbidden.
func (s *Store) PersonalMemoryByID(ctx context.Context, k personalKey, id int64) (*PersonalMemory, error) {
	if !k.ok() {
		return nil, nil
	}
	v, err := scanPersonalMemory(s.db.QueryRowContext(ctx, `select `+personalMemoryCols+` from personal_memories
		where org_id=? and team_id=? and owner=? and id=?`, k.orgID, k.teamID, k.owner, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return v, err
}

func (s *Store) UpdatePersonalMemory(ctx context.Context, k personalKey, id int64, text string) (bool, error) {
	if !k.ok() {
		return false, errNoOwner
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return false, errors.New("text is required")
	}
	res, err := s.db.ExecContext(ctx, `update personal_memories set text=?, updated_at=?
		where org_id=? and team_id=? and owner=? and id=?`, text, now(), k.orgID, k.teamID, k.owner, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeletePersonalMemory reports whether it found one, unlike DeleteMemory, which returns only an
// error: a personal delete has to be able to answer 404 for an id that is not the caller's.
//
// Deletion is by id and there is no delete-by-substring here on purpose. The shared ForgetMemory
// takes a needle from the model and has already had to be taught that % and _ are wildcards; a
// private list is short enough to enumerate, an id has no wildcard surface at all, and the blast
// radius if that reasoning were ever wrong is the only copy of something nobody else can see.
func (s *Store) DeletePersonalMemory(ctx context.Context, k personalKey, id int64) (bool, error) {
	if !k.ok() {
		return false, errNoOwner
	}
	res, err := s.db.ExecContext(ctx, `delete from personal_memories
		where org_id=? and team_id=? and owner=? and id=?`, k.orgID, k.teamID, k.owner, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) CountPersonalMemories(ctx context.Context, k personalKey) (int, error) {
	if !k.ok() {
		return 0, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx, `select count(*) from personal_memories
		where org_id=? and team_id=? and owner=?`, k.orgID, k.teamID, k.owner).Scan(&n)
	return n, err
}

// DeletePersonalMemoriesForTeam drops a workspace's notes, for the day a workspace is genuinely
// removed. Nothing calls it yet, and that is a decision rather than an omission: disconnecting a
// workspace does not remove it. disconnectTeam revokes the token and keeps the row "so the
// console can still explain why that workspace went quiet", and an uninstall or a revoked token
// in Slack lands in the same place -- all of them reversible by reinstalling. Wiring deletion to
// any of those would destroy the only copy of something nobody else can see, on an event that
// routinely undoes itself.
//
// So this is the primitive, ready and tested, for a path that actually deletes a workspace. The
// precedent when that path exists is DeleteUserConnectionsFor, which drops a connection's
// per-person sign-ins when the connection itself is deleted.
func (s *Store) DeletePersonalMemoriesForTeam(ctx context.Context, orgID int64, teamID string) error {
	if orgID == 0 || teamID == "" {
		return fmt.Errorf("a workspace is needed to remove its notes")
	}
	_, err := s.db.ExecContext(ctx, `delete from personal_memories where org_id=? and team_id=?`, orgID, teamID)
	return err
}
