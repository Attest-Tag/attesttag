package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Access requests: a write somebody else has to say yes to.
//
// This is deliberately not pending_writes. That table is the fast in-thread path — one payload,
// five minutes, answered by whoever is in the thread — and three separate call sites clear it by
// (channel, thread_ts): confirmFlow's "cancel", cancelPressed, and finishStopped. A request that
// waits a week for a named person cannot share a table whose contents anyone can drop by typing
// "no", and its five-minute window must not become conditional on a column. So: its own table,
// its own claim, and pending_writes left exactly as it was.

// accessRequestTTL is how long a request waits for an answer. ACCESS_TTL overrides it, which is
// only really useful for watching the expiry sweep run without waiting a week.
const accessRequestTTL = 7 * 24 * time.Hour

// AccessRequest is one ask, and the record of what was decided about it. The calls are stored as
// raw JSON in exactly the shape pending_writes.request has always held, so one replay function
// serves both paths and a held write can become a step here without being rewritten.
type AccessRequest struct {
	ID                   int64
	OrgID                int64  // whose credentials an approval would spend
	TeamID               string // the connected workspace it came from; the card is answered there
	Channel, ThreadTS    string // origin: where the answer goes back
	Requester, Approver  string
	Approvers            []string // who could answer when the card went out
	What, Why, Ask       string
	Calls                []json.RawMessage
	Status, OriginTS     string
	DecidedBy, DecidedAt string
	Reason, Result       string
	CreatedAt, ExpiresAt string
	SelfApproved         bool  // one person played both parts; recorded so a reader can tell
	RoleID               int64 // the tier this was routed to; execution runs under its bundle
}

// AccessCard is where one approver's copy of the card landed. A request goes to everyone who can
// answer it, so a decision has to rewrite every copy — otherwise the others keep live buttons for
// something already settled.
type AccessCard struct {
	Approver, Channel, TS string
}

func accessTTL() time.Duration {
	if d, err := time.ParseDuration(env("ACCESS_TTL", "")); err == nil && d > 0 {
		return d
	}
	return accessRequestTTL
}

func (s *Store) AddAccessRequest(ctx context.Context, r *AccessRequest) error {
	approvers, _ := json.Marshal(nonNil(r.Approvers))
	calls, _ := json.Marshal(r.Calls)
	r.ExpiresAt = time.Now().UTC().Add(accessTTL()).Format(time.DateTime)
	err := s.db.QueryRowContext(ctx, `insert into access_requests
		(org_id, team_id, channel, thread_ts, requester, approver, approvers, what, why, ask, calls, expires_at, role_id)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) returning id`,
		r.OrgID, r.TeamID, r.Channel, r.ThreadTS, r.Requester, r.Approver, string(approvers),
		r.What, r.Why, r.Ask, string(calls), r.ExpiresAt, r.RoleID).Scan(&r.ID)
	return err
}

const accessCols = `id, coalesce(org_id,0), coalesce(team_id,''), channel, thread_ts, requester, coalesce(approver,''), coalesce(approvers,'[]'),
	coalesce(what,''), coalesce(why,''), coalesce(ask,''), coalesce(calls,'[]'), status,
	coalesce(origin_ts,''), coalesce(decided_by,''), coalesce(decided_at,''),
	coalesce(reason,''), coalesce(result,''), created_at, expires_at, coalesce(self_approved,0), coalesce(role_id,0)`

func scanAccess(row interface{ Scan(...any) error }) (*AccessRequest, error) {
	var r AccessRequest
	var approvers, calls string
	if err := row.Scan(&r.ID, &r.OrgID, &r.TeamID, &r.Channel, &r.ThreadTS, &r.Requester, &r.Approver, &approvers,
		&r.What, &r.Why, &r.Ask, &calls, &r.Status, &r.OriginTS, &r.DecidedBy, &r.DecidedAt,
		&r.Reason, &r.Result, &r.CreatedAt, &r.ExpiresAt, &r.SelfApproved, &r.RoleID); err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(approvers), &r.Approvers)
	json.Unmarshal([]byte(calls), &r.Calls)
	return &r, nil
}

func (s *Store) AccessRequest(ctx context.Context, orgID, id int64) (*AccessRequest, error) {
	r, err := scanAccess(s.db.QueryRowContext(ctx, `select `+accessCols+` from access_requests where org_id=? and id=?`, orgID, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// TakeAccessRequest claims one request for execution — what an Approve button carries. A request
// that expired, was already answered, or belongs to the person pressing comes back nil.
//
// The requester <> ? predicate is the last of three places self-approval is refused, and the only
// one a future call site cannot forget: it is in the statement itself. allowSelf drops it — that
// is a deliberate setting for a workspace testing the flow with one person, and it is recorded on
// the row so a later reader can tell the difference between two people agreeing and one.
func (s *Store) TakeAccessRequest(ctx context.Context, orgID, id int64, by string, allowSelf bool) (*AccessRequest, error) {
	q := `update access_requests set status='approved', decided_by=?, decided_at=?, self_approved=?
		where org_id=? and id=? and status='pending' and expires_at > ?`
	args := []any{by, now(), 0, orgID, id, now()}
	if allowSelf {
		args[2] = 1
	} else {
		q += ` and requester <> ?`
		args = append(args, by)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil
	}
	return s.AccessRequest(ctx, orgID, id)
}

// DenyAccessRequest is the same claim in the other direction, so two approvers cannot both answer.
func (s *Store) DenyAccessRequest(ctx context.Context, orgID, id int64, by, reason string) (*AccessRequest, error) {
	res, err := s.db.ExecContext(ctx, `update access_requests
		set status='denied', decided_by=?, decided_at=?, reason=?
		where org_id=? and id=? and status='pending'`, by, now(), reason, orgID, id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil
	}
	return s.AccessRequest(ctx, orgID, id)
}

// SetAccessReason fills in a reason after the fact — the approver pressed Deny and then typed why.
func (s *Store) SetAccessReason(ctx context.Context, orgID, id int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `update access_requests set reason=? where org_id=? and id=?`, reason, orgID, id)
	return err
}

func (s *Store) FinishAccessRequest(ctx context.Context, orgID, id int64, status, result, reason string) error {
	_, err := s.db.ExecContext(ctx, `update access_requests set status=?, result=?, reason=? where org_id=? and id=?`,
		status, result, reason, orgID, id)
	return err
}

func (s *Store) SetAccessOriginTS(ctx context.Context, orgID, id int64, ts string) error {
	_, err := s.db.ExecContext(ctx, `update access_requests set origin_ts=? where org_id=? and id=?`, ts, orgID, id)
	return err
}

// CancelAccessRequests withdraws the caller's own open requests in a thread. Only their own: one
// person saying "cancel" must not drop somebody else's ask that happens to share the thread.
func (s *Store) CancelAccessRequests(ctx context.Context, teamID, channel, threadTS, requester string) ([]AccessRequest, error) {
	return s.claimAccess(ctx, `select id, org_id from access_requests
		where team_id=? and channel=? and thread_ts=? and requester=? and status='pending'`,
		"cancelled", teamID, channel, threadTS, requester)
}

// ExpireAccessRequests closes out everything nobody answered in time and returns the rows it moved,
// so the sweep can tell the people who were waiting.
func (s *Store) ExpireAccessRequests(ctx context.Context) ([]AccessRequest, error) {
	return s.claimAccess(ctx, `select id, org_id from access_requests
		where status='pending' and expires_at <= ?`, "expired", now())
}

// claimAccess moves rows to a terminal status one at a time, each guarded on status='pending', and
// returns only the ones this caller actually moved. Cloud Run overlaps the old and new revision
// during a traffic migration, so "one instance" is never a guarantee that a sweep runs alone — the
// guard is what stops two of them telling the same person twice.
func (s *Store) claimAccess(ctx context.Context, q, status string, args ...any) ([]AccessRequest, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	type claim struct{ id, org int64 }
	claims := []claim{}
	for rows.Next() {
		var c claim
		if err := rows.Scan(&c.id, &c.org); err != nil {
			rows.Close()
			return nil, err
		}
		claims = append(claims, c)
	}
	rows.Close()
	out := []AccessRequest{}
	for _, c := range claims {
		res, err := s.db.ExecContext(ctx, `update access_requests set status=?, decided_at=?
			where org_id=? and id=? and status='pending'`, status, now(), c.org, c.id)
		if err != nil {
			return out, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // somebody answered it between the select and here
		}
		if r, err := s.AccessRequest(ctx, c.org, c.id); err == nil && r != nil {
			out = append(out, *r)
		}
	}
	return out, nil
}

// OpenAccessRequestCount is the abuse guard: an approver's DMs are a shared resource, so one
// person cannot queue up an unbounded number of asks.
func (s *Store) OpenAccessRequestCount(ctx context.Context, orgID int64, requester string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `select count(*) from access_requests
		where org_id=? and requester=? and status='pending' and expires_at > ?`, orgID, requester, now()).Scan(&n)
	return n, err
}

// AccessRequests lists newest first, optionally filtered to one status, for the console.
func (s *Store) AccessRequests(ctx context.Context, orgID int64, status string, limit int) ([]AccessRequest, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q, args := `select `+accessCols+` from access_requests where org_id=?`, []any{orgID}
	if status != "" {
		q += ` and status=?`
		args = append(args, status)
	}
	q += ` order by id desc limit ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AccessRequest{}
	for rows.Next() {
		r, err := scanAccess(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// ---- where each approver's copy of the card landed ----

func (s *Store) AddAccessCard(ctx context.Context, orgID, id int64, approver, channel, ts string) error {
	_, err := s.db.ExecContext(ctx, `insert into access_request_cards (org_id, request_id, approver, channel, ts)
		values (?, ?, ?, ?, ?) on conflict(request_id, approver) do update set channel=excluded.channel, ts=excluded.ts`,
		orgID, id, approver, channel, ts)
	return err
}

func (s *Store) AccessCards(ctx context.Context, orgID, id int64) ([]AccessCard, error) {
	rows, err := s.db.QueryContext(ctx, `select approver, channel, ts from access_request_cards where org_id=? and request_id=?`, orgID, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AccessCard{}
	for rows.Next() {
		var c AccessCard
		if err := rows.Scan(&c.Approver, &c.Channel, &c.TS); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// AwaitingDenyReason finds a request this person denied a moment ago and has not yet explained.
// Bounded deliberately: it is a short window right after the press, so an ordinary DM sent later
// is never swallowed as a reason.
// denyReasonWindow is how long after a denial the next thing the denier types is taken as the
// reason for it. Long enough to finish a sentence, short enough that an unrelated message an
// hour later is not filed as one.
const denyReasonWindow = 10 * time.Minute

func (s *Store) AwaitingDenyReason(ctx context.Context, orgID int64, by string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `select id from access_requests
		where org_id=? and decided_by=? and status='denied' and coalesce(reason,'')=''
		and decided_at > ? order by id desc limit 1`, orgID, by, nowMinus(denyReasonWindow)).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

// ---- approval roles ----
//
// An approver is not a name on a list: they hold a role, and the role lists what they may grant
// from. Rank orders the tiers, so a super admin covers everything a plain approver can and a
// request nobody's own tier covers can escalate to one that does.

type ApprovalRole struct {
	ID   int64
	Name string
	Rank int
	// What its members may grant from: whole bundles, and connections picked on their own —
	// recorded the way a channel's access is, in approval_role_bundles and
	// approval_role_connections. Being listed here is the whole authorisation: the tier may
	// grant every active connection it names, directly or through a bundle.
	BundleIDs     []int64
	ConnectionIDs []int64
	Members       []string         // Slack user ids or emails, as entered
	Resolved      []ApprovalMember // the same entries with the workspace each one was resolved in
}

// ApprovalMember is one entry of a tier. Slack user ids are unique within a workspace, and an
// email is whatever that workspace's admin says it is — so an approver is a person *in a
// workspace*, and a press from any other workspace does not match them. TeamID is empty for a
// row written before that was recorded; tiers() decides what such a row is still worth.
type ApprovalMember struct {
	Ref, TeamID, SlackUserID string
}

func (s *Store) ApprovalRoles(ctx context.Context, orgID int64) ([]ApprovalRole, error) {
	rows, err := s.db.QueryContext(ctx, `select id, name, rank from approval_roles where org_id=? order by rank, name`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ApprovalRole{}
	for rows.Next() {
		var r ApprovalRole
		if err := rows.Scan(&r.ID, &r.Name, &r.Rank); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Resolved, _ = s.approvalMembers(ctx, orgID, out[i].ID)
		out[i].Members = memberRefs(out[i].Resolved)
		out[i].BundleIDs, out[i].ConnectionIDs, _ = s.roleGrantIDs(ctx, orgID, out[i].ID)
	}
	return out, nil
}

// memberRefs is the entries as entered, once each: an email resolved in two workspaces is two
// rows but one thing the admin typed.
func memberRefs(ms []ApprovalMember) []string {
	out := []string{}
	for _, m := range ms {
		out = appendOnce(out, m.Ref)
	}
	return out
}

func (s *Store) approvalMembers(ctx context.Context, orgID, roleID int64) ([]ApprovalMember, error) {
	rows, err := s.db.QueryContext(ctx, `select ref, coalesce(team_id,''), coalesce(slack_user_id,'') from approval_members
		where org_id=? and role_id=? order by ref, team_id`, orgID, roleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ApprovalMember{}
	for rows.Next() {
		var m ApprovalMember
		if err := rows.Scan(&m.Ref, &m.TeamID, &m.SlackUserID); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) ApprovalRole(ctx context.Context, orgID, id int64) (*ApprovalRole, error) {
	var r ApprovalRole
	err := s.db.QueryRowContext(ctx, `select id, name, rank from approval_roles where org_id=? and id=?`, orgID, id).
		Scan(&r.ID, &r.Name, &r.Rank)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Resolved, _ = s.approvalMembers(ctx, orgID, r.ID)
	r.Members = memberRefs(r.Resolved)
	r.BundleIDs, r.ConnectionIDs, _ = s.roleGrantIDs(ctx, orgID, r.ID)
	return &r, nil
}

// roleGrantIDs is what a tier may grant from: its bundles, then its one-off connections.
func (s *Store) roleGrantIDs(ctx context.Context, orgID, roleID int64) (bundles, conns []int64, err error) {
	if bundles, err = s.idList(ctx, `select bundle_id from approval_role_bundles where org_id=? and role_id=? order by bundle_id`, orgID, roleID); err != nil {
		return nil, nil, err
	}
	conns, err = s.idList(ctx, `select connection_id from approval_role_connections where org_id=? and role_id=? order by connection_id`, orgID, roleID)
	return bundles, conns, err
}

// idList runs a one-column query of ids. Never nil, so a caller can range over it and JSON
// says [] rather than null.
func (s *Store) idList(ctx context.Context, q string, args ...any) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// AddApprovalRole writes the tier and whatever it was created granting from. Each grant is
// checked to be this organisation's the same way the console's own add is.
func (s *Store) AddApprovalRole(ctx context.Context, orgID int64, r *ApprovalRole) error {
	err := s.db.QueryRowContext(ctx, `insert into approval_roles (org_id, name, rank) values (?, ?, ?) returning id`, orgID, r.Name, r.Rank).Scan(&r.ID)
	if err != nil {
		return err
	}
	for _, id := range r.BundleIDs {
		if err := s.AttachRoleBundle(ctx, orgID, r.ID, id); err != nil {
			return err
		}
	}
	for _, id := range r.ConnectionIDs {
		if err := s.AttachRoleConnection(ctx, orgID, r.ID, id); err != nil {
			return err
		}
	}
	return nil
}

// UpdateApprovalRole renames or re-ranks a tier. What it grants from changes through
// AttachRoleBundle and friends, one grant at a time, so a stale form cannot wipe the list.
func (s *Store) UpdateApprovalRole(ctx context.Context, orgID int64, r *ApprovalRole) error {
	_, err := s.db.ExecContext(ctx, `update approval_roles set name=?, rank=? where org_id=? and id=?`,
		r.Name, r.Rank, orgID, r.ID)
	return err
}

func (s *Store) DeleteApprovalRole(ctx context.Context, orgID, id int64) error {
	for _, q := range []string{
		`delete from approval_members where org_id=? and role_id=?`,
		`delete from approval_role_bundles where org_id=? and role_id=?`,
		`delete from approval_role_connections where org_id=? and role_id=?`,
	} {
		if _, err := s.db.ExecContext(ctx, q, orgID, id); err != nil {
			return err
		}
	}
	_, err := s.db.ExecContext(ctx, `delete from approval_roles where org_id=? and id=?`, orgID, id)
	return err
}

// AttachRoleBundle lets a tier grant from a whole bundle. Like AttachBundle for a scope, it is
// one statement with an existence check on both sides rather than a read-then-write, so neither
// id can belong to somebody else — and there is no ownership check a caller could forget.
func (s *Store) AttachRoleBundle(ctx context.Context, orgID, roleID, bundleID int64) error {
	res, err := s.db.ExecContext(ctx, `insert into approval_role_bundles (org_id, role_id, bundle_id)
		select ?, ?, ? where exists(select 1 from approval_roles where id=? and org_id=?)
		            and exists(select 1 from bundles where id=? and org_id=?) on conflict do nothing`,
		orgID, roleID, bundleID, roleID, orgID, bundleID, orgID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Either it was already there, or one of the two is not ours. Re-read to tell them
		// apart rather than reporting a duplicate as a failure.
		var ok int
		s.db.QueryRowContext(ctx, `select count(*) from approval_role_bundles where org_id=? and role_id=? and bundle_id=?`,
			orgID, roleID, bundleID).Scan(&ok)
		if ok == 0 {
			return errors.New("no such approval role or bundle")
		}
	}
	return nil
}

func (s *Store) DetachRoleBundle(ctx context.Context, orgID, roleID, bundleID int64) error {
	_, err := s.db.ExecContext(ctx, `delete from approval_role_bundles where org_id=? and role_id=? and bundle_id=?`, orgID, roleID, bundleID)
	return err
}

// AttachRoleConnection lets a tier grant one connection without the rest of its bundle.
func (s *Store) AttachRoleConnection(ctx context.Context, orgID, roleID, connID int64) error {
	res, err := s.db.ExecContext(ctx, `insert into approval_role_connections (org_id, role_id, connection_id)
		select ?, ?, ? where exists(select 1 from approval_roles where id=? and org_id=?)
		            and exists(select 1 from connections where id=? and org_id=?) on conflict do nothing`,
		orgID, roleID, connID, roleID, orgID, connID, orgID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var ok int
		s.db.QueryRowContext(ctx, `select count(*) from approval_role_connections where org_id=? and role_id=? and connection_id=?`,
			orgID, roleID, connID).Scan(&ok)
		if ok == 0 {
			return errors.New("no such approval role or connection")
		}
	}
	return nil
}

func (s *Store) DetachRoleConnection(ctx context.Context, orgID, roleID, connID int64) error {
	_, err := s.db.ExecContext(ctx, `delete from approval_role_connections where org_id=? and role_id=? and connection_id=?`, orgID, roleID, connID)
	return err
}

// AddApprovalMember records an entry that has not been tied to a workspace. A bare Slack user
// id is accepted as it is; an email written this way only counts while the organisation has a
// single workspace to resolve it in (see tiers). The console resolves entries at the time they
// are added and uses AddResolvedApprovalMember instead.
func (s *Store) AddApprovalMember(ctx context.Context, orgID, roleID int64, ref string) error {
	_, err := s.db.ExecContext(ctx,
		`insert into approval_members (org_id, role_id, ref) values (?, ?, ?) on conflict(org_id, role_id, ref) do nothing`, orgID, roleID, ref)
	return err
}

// AddResolvedApprovalMember records an approver as a person in one workspace. The uniqueness
// key is still (org, role, ref): an email resolved in a second workspace updates the row rather
// than adding one, so the console's picture of "who is on this tier" stays one line per entry.
func (s *Store) AddResolvedApprovalMember(ctx context.Context, orgID, roleID int64, ref, teamID, slackUserID string) error {
	_, err := s.db.ExecContext(ctx,
		`insert into approval_members (org_id, role_id, ref, team_id, slack_user_id) values (?, ?, ?, ?, ?)
		 on conflict(org_id, role_id, ref) do update set team_id=excluded.team_id, slack_user_id=excluded.slack_user_id`,
		orgID, roleID, ref, teamID, slackUserID)
	return err
}

func (s *Store) RemoveApprovalMember(ctx context.Context, orgID, roleID int64, ref string) error {
	_, err := s.db.ExecContext(ctx, `delete from approval_members where org_id=? and role_id=? and ref=?`, orgID, roleID, ref)
	return err
}

// SetAccessRole records which tier a request was routed to, so execution runs under that tier's
// bundle rather than whatever the approver happens to hold.
func (s *Store) SetAccessRole(ctx context.Context, orgID, id, roleID int64) error {
	_, err := s.db.ExecContext(ctx, `update access_requests set role_id=? where org_id=? and id=?`, roleID, orgID, id)
	return err
}
