package app

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The audit log: who did what, from where, and what came of it.
//
// The Activity page records what the bot did. This records what the people did — signed in,
// changed a credential, gave somebody admin, approved a write in Slack, exported a table — which
// is the half an auditor, a security review or a departing employee's manager actually asks
// about. It was slog lines before this, which live in whatever the platform keeps of stdout and
// are gone from anything the organisation can open for itself.
//
// Two ways a row gets written, and the second exists because of the first. A handler that knows
// what it did records a named event: "connection.updated", with the connection's name and
// whether the secret was rotated. That is the useful kind, and it is also the kind that has to
// be remembered at the end of every handler, which is the shape of thing that eventually is not.
// So requireAdmin also wraps every authenticated write in auditedWrites, which records a plain
// "console.request" for any POST, PUT or DELETE that finished without a named event. The named
// row wins when there is one; the plain row is the floor. Nothing that changes state goes
// unrecorded because somebody forgot.
//
// Rows are never edited and only two things delete them: the organisation's own
// audit_retention_days, and the deletion of the organisation. data_retention_days deliberately
// does not reach this table — a policy set by an admin deleting the record of the admin setting
// it is exactly what the table exists to prevent.

// What a row says about how it happened.
const (
	auditOK     = "ok"
	auditDenied = "denied" // the request was made and refused: a 403, a wrong password
	auditFailed = "failed"

	viaConsole  = "console"
	viaAPIKey   = "api_key"
	viaMCP      = "mcp"
	viaSlack    = "slack"
	viaOperator = "operator"
	viaSystem   = "system"
)

// auditRetentionFloor is the shortest history an organisation may keep of its own audit log.
// Higher than the activity table's week: an audit trail that vanishes before anybody has had a
// reason to read it is not one.
const auditRetentionFloor = 30

// AuditEvent is one row. The actor is copied in full at the time rather than joined at read
// time, so a person who has since been removed — or whose account is gone — is still legible in
// the record of what they did. Details is JSON the console shows as-is; it never holds a secret,
// a token or a password, and every writer is responsible for that.
type AuditEvent struct {
	ID    int64  `json:"id"`
	OrgID int64  `json:"-"`
	At    string `json:"at"`
	// Action is a dotted verb — "auth.sign_in", "connection.deleted" — and the vocabulary is the
	// set of strings written by the callers in this package. The console lists the ones that
	// exist rather than a fixed enumeration, so a new event needs no registration.
	Action  string `json:"action"`
	Outcome string `json:"outcome"`
	Via     string `json:"via"`
	// ActorID is users.id and stays in the process; ActorPublic is what the console names the
	// person by and the value the ?actor= filter takes. Both are empty for a Slack-side actor
	// (ActorSlack carries them), the operator and the system.
	ActorID     int64           `json:"-"`
	ActorPublic string          `json:"actor_id"`
	ActorEmail  string          `json:"actor_email"`
	ActorName   string          `json:"actor_name"`
	ActorSlack  string          `json:"actor_slack"`
	TeamID      string          `json:"team_id"`
	TargetKind  string          `json:"target_kind"`
	TargetID    string          `json:"target_id"`
	TargetName  string          `json:"target_name"`
	IP          string          `json:"ip"`
	UserAgent   string          `json:"user_agent"`
	Details     json.RawMessage `json:"details"`
}

// AuditFilter is everything the listing, the export and the API narrow by.
type AuditFilter struct {
	Limit   int
	Since   string // created_at lower bound in the stored format; "" for none
	Actor   string // an actor's public id, email or Slack id, exactly
	Action  string // an action exactly, or a family when it ends with a dot: "auth."
	Outcome string
	Query   string // a substring of the actor, the action, the target, the details or the address
	Before  int64  // rows older than this id, newest first: the console's "load older"
	After   int64  // rows newer than this id, oldest first: a SIEM's incremental pull
}

// ---- the store ----

func (s *Store) LogAudit(ctx context.Context, e AuditEvent) (int64, error) {
	if e.Outcome == "" {
		e.Outcome = auditOK
	}
	if len(e.Details) == 0 {
		e.Details = json.RawMessage("{}")
	}
	// `returning id` rather than LastInsertId, which is the rule everywhere else in this
	// package: the pgx driver does not implement LastInsertId at all, so on Postgres it
	// returned an error this line discarded and an id of 0, for a row the database had in fact
	// written under a perfectly good id. Nothing in production read the value, which is why it
	// went unnoticed — the only caller that did was a test, and it failed for a day looking
	// like a broken retention sweep.
	var id int64
	err := s.db.QueryRowContext(ctx, `insert into audit_log (org_id, created_at, action, outcome, via, actor_id, actor_public, actor_email, actor_name, actor_slack,
		team_id, target_kind, target_id, target_name, ip, user_agent, details)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) returning id`,
		e.OrgID, now(), e.Action, e.Outcome, e.Via, e.ActorID, e.ActorPublic, e.ActorEmail, e.ActorName, e.ActorSlack,
		e.TeamID, e.TargetKind, e.TargetID, e.TargetName, e.IP, e.UserAgent, string(e.Details)).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// AuditEvents lists one organisation's rows, newest first unless the caller is walking forward
// from an id it has already seen.
func (s *Store) AuditEvents(ctx context.Context, orgID int64, f AuditFilter) ([]AuditEvent, error) {
	q := `select id, created_at, action, outcome, via, actor_id, actor_public, actor_email, actor_name, actor_slack,
		team_id, target_kind, target_id, target_name, ip, user_agent, details
		from audit_log where org_id=?`
	args := []any{orgID}
	if f.Since != "" {
		q += ` and created_at >= ?`
		args = append(args, f.Since)
	}
	if f.Actor != "" {
		q += ` and (actor_public=? or actor_email=? or actor_slack=?)`
		args = append(args, f.Actor, f.Actor, f.Actor)
	}
	if f.Action != "" {
		if strings.HasSuffix(f.Action, ".") {
			q += ` and action like ?`
			args = append(args, f.Action+"%")
		} else {
			q += ` and action=?`
			args = append(args, f.Action)
		}
	}
	if f.Outcome != "" {
		q += ` and outcome=?`
		args = append(args, f.Outcome)
	}
	if f.Query != "" {
		// lower() on both sides rather than ilike: Postgres has ilike and SQLite does not, and
		// the guard test refuses a statement written in one dialect's words.
		like := "%" + strings.ToLower(f.Query) + "%"
		q += ` and (lower(actor_email) like ? or lower(actor_name) like ? or lower(action) like ?
			or lower(target_name) like ? or lower(target_id) like ? or lower(details) like ? or ip like ?)`
		args = append(args, like, like, like, like, like, like, like)
	}
	if f.Before > 0 {
		q += ` and id < ?`
		args = append(args, f.Before)
	}
	if f.After > 0 {
		q += ` and id > ?`
		args = append(args, f.After)
	}
	order := ` order by id desc limit ?`
	if f.After > 0 {
		order = ` order by id asc limit ?`
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q+order, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEvent{}
	for rows.Next() {
		var e AuditEvent
		var details string
		if err := rows.Scan(&e.ID, &e.At, &e.Action, &e.Outcome, &e.Via, &e.ActorID, &e.ActorPublic, &e.ActorEmail, &e.ActorName, &e.ActorSlack,
			&e.TeamID, &e.TargetKind, &e.TargetID, &e.TargetName, &e.IP, &e.UserAgent, &details); err != nil {
			return nil, err
		}
		e.OrgID = orgID
		e.Details = json.RawMessage(details)
		if !json.Valid(e.Details) {
			e.Details = json.RawMessage("{}")
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AuditActions is the vocabulary this organisation's log actually uses, for the filter menu.
func (s *Store) AuditActions(ctx context.Context, orgID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `select distinct action from audit_log where org_id=? order by action`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// PurgeAuditLog deletes one organisation's rows older than its own audit policy. keep <= 0
// deletes nothing, the same rule PurgeOrgData follows and for the same reason.
func (s *Store) PurgeAuditLog(ctx context.Context, orgID int64, keep time.Duration) (int64, error) {
	if keep <= 0 || orgID == 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-keep).UTC().Format(time.DateTime)
	res, err := s.db.ExecContext(ctx, `delete from audit_log where org_id=? and created_at < ?`, orgID, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- recording ----

// auditMarkKey carries the flag auditedWrites reads after a handler returns: set by any named
// event recorded during the request, so the plain row is written only when nothing better was.
const auditMarkKey ctxKey = 2

type auditMark struct{ done bool }

// auditDetails builds the details column. A map rather than a struct per event, because the
// point of the column is that each event says what it has to say and nothing has to agree on a
// shape. Never put a secret in one.
func auditDetails(kv map[string]any) json.RawMessage {
	raw, err := json.Marshal(kv)
	if err != nil {
		return json.RawMessage("{}")
	}
	return raw
}

// audit records a named event for the request in hand. The actor and the organisation come from
// the session unless the caller already filled them in — a sign-in has no session yet, so it
// says who — and the address and browser come from the request.
func (b *Bot) audit(r *http.Request, action string, e AuditEvent) {
	ctx := r.Context()
	if m, _ := ctx.Value(auditMarkKey).(*auditMark); m != nil {
		m.done = true
	}
	if u := adminFromCtx(ctx); u != nil {
		if e.OrgID == 0 {
			e.OrgID = u.OrgID
		}
		if e.ActorID == 0 && e.ActorEmail == "" {
			e.ActorID, e.ActorPublic, e.ActorEmail, e.ActorName, e.ActorSlack = u.ID, u.PublicID, u.Email, u.Name, u.UserID
		}
		if e.Via == "" && u.Via == "api_key" {
			e.Via = viaAPIKey
			// Which key, so a rotated or revoked one can be tied to what it did.
			e.Details = withDetail(e.Details, "api_key_id", u.APIKeyID)
		}
		// Through the MCP server, with a key or with a connected client's token: the same
		// tying-back, for whichever it was.
		if e.Via == "" && u.Via == viaMCP {
			e.Via = viaMCP
			if u.APIKeyID != 0 {
				e.Details = withDetail(e.Details, "api_key_id", u.APIKeyID)
			}
			if u.MCPGrantID != 0 {
				e.Details = withDetail(e.Details, "mcp_grant_id", u.MCPGrantID)
			}
		}
	}
	if e.OrgID == 0 {
		return // nobody's organisation to record this against
	}
	if e.Via == "" {
		e.Via = viaConsole
	}
	e.Action = action
	e.IP = clientIP(r)
	e.UserAgent, _ = cutRunes(r.UserAgent(), 200)
	b.record(ctx, e)
}

// auditAs records an event by an account that is not on the request: a sign-in, where the
// session is only now being minted, or a password reset, where there is no session at all.
func (b *Bot) auditAs(r *http.Request, u *User, orgID int64, action string, e AuditEvent) {
	if u == nil || orgID == 0 {
		return
	}
	e.OrgID = orgID
	e.ActorID, e.ActorPublic, e.ActorEmail, e.ActorName = u.ID, u.PublicID, u.Email, u.Name
	b.audit(r, action, e)
}

// auditEverywhere records the same event in every organisation the account belongs to. A
// failed sign-in or a password reset is about the person, and each organisation they are a
// member of has a legitimate interest in it; an address that names nobody is recorded nowhere,
// which is the same answer the sign-in form gives.
func (b *Bot) auditEverywhere(r *http.Request, u *User, action string, e AuditEvent) {
	if u == nil {
		return
	}
	ms, err := b.store.MembershipsFor(r.Context(), u.ID)
	if err != nil {
		return
	}
	for _, m := range ms {
		b.auditAs(r, u, m.OrgID, action, e)
	}
}

// auditSlack records something a person did in Slack: pressed Approve, typed confirm. The actor
// is a Slack user id, resolved to a name now because the workspace may not be connected when
// somebody reads this.
func (b *Bot) auditSlack(ctx context.Context, sl *Chat, user, action string, e AuditEvent) {
	if sl == nil {
		return
	}
	e.OrgID, e.TeamID, e.Via, e.ActorSlack, e.Action = sl.OrgID, sl.TeamID, viaSlack, user, action
	if e.ActorName == "" && user != "" {
		e.ActorName = sl.UserName(ctx, user)
	}
	b.record(ctx, e)
}

// auditOperator records an act of the deployment's operator (operator.go): not a member of the
// organisation, so the row names the role and the address it came from.
func (b *Bot) auditOperator(ctx context.Context, orgID int64, ip, action string, e AuditEvent) {
	e.OrgID, e.Via, e.ActorName, e.IP, e.Action = orgID, viaOperator, "operator", ip, action
	b.record(ctx, e)
}

// auditSystem records something the process did on its own schedule — a retention sweep — so
// that "rows were deleted on this date by policy" is in the record beside the policy that did it.
func (b *Bot) auditSystem(ctx context.Context, orgID int64, action string, e AuditEvent) {
	e.OrgID, e.Via, e.ActorName, e.Action = orgID, viaSystem, "attest_tag", action
	b.record(ctx, e)
}

// auditAutoWrite records a write that ran on a turn a forwarded email started, with no human
// anywhere in its history: no Confirm pressed, no approver, nobody who typed the request. The
// row names the rule that allowed it and the destination it reached, which together are the
// whole of the decision — everything else about that turn was written outside the company.
//
// On Agent rather than Bot, and therefore reaching the store directly: the gate that calls this
// is in the tool path, which has no Bot. The failure handling is Bot.record's, for its reason —
// refusing somebody's action because the audit table was briefly unwritable would be an outage
// in the name of a record nobody could then read either.
func (a *Agent) auditAutoWrite(ctx context.Context, c *Call, rule, action string) {
	if a.store == nil || c == nil || c.OrgID == 0 {
		return
	}
	e := AuditEvent{
		OrgID: c.OrgID, TeamID: c.TeamID, Via: viaSystem, Action: "write.auto_ran",
		ActorName: "a forwarded email", TargetKind: "channel", TargetID: c.Channel,
		TargetName: truncate(oneLine(action), 300),
		Details:    auditDetails(map[string]any{"rule": rule, "thread_ts": c.ThreadTS}),
	}
	if _, err := a.store.LogAudit(context.WithoutCancel(ctx), e); err != nil {
		slog.Error("audit log write failed", "org", e.OrgID, "action", e.Action, "err", err)
	}
}

// record is the one write. A failed insert is logged loudly and does not fail the request that
// caused it: refusing somebody's action because the audit table was briefly unwritable would be
// an outage in the name of a record nobody could then read either.
func (b *Bot) record(ctx context.Context, e AuditEvent) {
	if b.store == nil || e.OrgID == 0 {
		return
	}
	// The request may be cancelled — the client gone — by the time the handler gets here, and
	// the row must be written anyway: the act happened.
	if _, err := b.store.LogAudit(context.WithoutCancel(ctx), e); err != nil {
		slog.Error("audit log write failed", "org", e.OrgID, "action", e.Action, "err", err)
	}
}

// auditGrant records a change to what a channel may reach: a bundle or a one-off connection
// attached to or detached from a scope. The scope is named so the row reads as "#platform"
// rather than as a number.
func (b *Bot) auditGrant(r *http.Request, action string, scopeID int64, key string, id int64) {
	e := AuditEvent{TargetKind: "scope", TargetID: strconv.FormatInt(scopeID, 10),
		Details: auditDetails(map[string]any{key: id})}
	if sc, _ := b.store.ScopeByID(r.Context(), orgOf(r), scopeID); sc != nil {
		e.TargetName, e.TeamID = sc.Name, sc.TeamID
	}
	b.audit(r, action, e)
}

// emailOf names an account for a row about it. The id is what the route carries; the address
// is what the reader recognises, and it stays legible after the account is gone.
func (b *Bot) emailOf(ctx context.Context, userID int64) string {
	if u, _ := b.store.User(ctx, userID); u != nil {
		return u.Email
	}
	return ""
}

// withDetail adds one key to a details document, building it if there is none yet.
func withDetail(raw json.RawMessage, key string, value any) json.RawMessage {
	m := map[string]any{}
	if len(raw) > 0 {
		json.Unmarshal(raw, &m)
	}
	m[key] = value
	return auditDetails(m)
}

// ---- the floor under every write ----

// auditedWrites is the safety net under every authenticated route. A POST, PUT or DELETE that
// finished with a success or a refusal, and recorded nothing better on the way, is recorded as
// a plain "console.request" naming the route and the status. Reads are not: a GET is not an
// act, and a log of every page load is a log nobody reads.
//
// A person's own private notes (/api/personal-memories) are the one exception: the organisation
// does not get to read them, and it does not get a line saying when they were written either.
func (b *Bot) auditedWrites(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next(w, r)
			return
		}
		mark := &auditMark{}
		r = r.WithContext(context.WithValue(r.Context(), auditMarkKey, mark))
		sw := &statusWriter{ResponseWriter: w}
		next(sw, r)
		if mark.done || strings.HasPrefix(r.URL.Path, "/api/personal-memories") {
			return
		}
		outcome := ""
		switch code := sw.code(); {
		case code >= 200 && code < 300:
			outcome = auditOK
		case code == http.StatusForbidden:
			outcome = auditDenied
		default:
			return // a validation error, a conflict, a 404: nothing happened
		}
		// The path as it was hit, except where a segment is a token: a setup link being revoked
		// is still a credential until this request finishes, and the record does not need it.
		target := r.URL.Path
		if strings.Contains(r.Pattern, "{token") {
			target = patternPath(r.Pattern)
		}
		b.audit(r, "console.request", AuditEvent{
			Outcome: outcome, TargetKind: "route", TargetID: r.Method + " " + target,
			Details: auditDetails(map[string]any{"method": r.Method, "pattern": patternPath(r.Pattern), "status": sw.code()}),
		})
	}
}

// patternPath is the registered pattern without its method: "PUT /api/scopes/{id}" → "/api/scopes/{id}".
func patternPath(pattern string) string {
	if _, p, ok := strings.Cut(pattern, " "); ok {
		return p
	}
	return pattern
}

// statusWriter remembers the status a handler wrote, which net/http does not expose afterwards.
// Flush and Unwrap are passed through so a handler that streams, or uses http.ResponseController,
// sees the writer it was written against.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(p)
}

func (s *statusWriter) code() int {
	if s.status == 0 {
		return http.StatusOK
	}
	return s.status
}

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// ---- the console ----

// auditFilterFrom reads the query string every reader of the log shares: the console listing,
// its export and /v1/audit take the same parameters, so a filter built on the page can be
// pasted into a script.
func auditFilterFrom(r *http.Request, def, max int) AuditFilter {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = def
	}
	if limit > max {
		limit = max
	}
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	since := activitySince(q.Get("range"))
	if since == "" {
		// An explicit lower bound, in the stored format ("2026-09-15 08:00:00"): what a script
		// that keeps its own high-water mark by time rather than by id sends.
		since = strings.TrimSpace(q.Get("since"))
	}
	return AuditFilter{
		Limit: limit, Since: since, Before: before, After: after,
		Actor:   strings.TrimSpace(q.Get("actor")),
		Action:  strings.TrimSpace(q.Get("action")),
		Outcome: strings.TrimSpace(q.Get("outcome")),
		Query:   strings.TrimSpace(q.Get("q")),
	}
}

// auditFilterDetails is the filter as the export event records it, so a row saying "the log was
// exported" also says which part of it.
func auditFilterDetails(f AuditFilter, rows int) map[string]any {
	d := map[string]any{"rows": rows}
	if f.Since != "" {
		d["since"] = f.Since
	}
	if f.Actor != "" {
		d["actor"] = f.Actor
	}
	if f.Action != "" {
		d["action"] = f.Action
	}
	if f.Outcome != "" {
		d["outcome"] = f.Outcome
	}
	if f.Query != "" {
		d["q"] = f.Query
	}
	return d
}

// auditCSVCap is how many rows one export carries. The same ceiling as the activity export; a
// script wanting everything walks /v1/audit with ?after= instead.
const auditCSVCap = 5000

func (b *Bot) auditRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/audit", b.requirePerm(PermAuditView, func(w http.ResponseWriter, r *http.Request) {
		f := auditFilterFrom(r, 100, 500)
		events, err := b.store.AuditEvents(r.Context(), orgOf(r), f)
		if err != nil {
			fail(w, err)
			return
		}
		actions, _ := b.store.AuditActions(r.Context(), orgOf(r))
		// The cursor for the next page: only when this one was full, so an empty answer means
		// the end rather than a page that happened to fit.
		var next int64
		if len(events) == f.Limit && f.After == 0 {
			next = events[len(events)-1].ID
		}
		writeJSON(w, 200, map[string]any{"events": events, "actions": actions, "next_before": next})
	}))
	mux.HandleFunc("GET /api/audit.csv", b.requirePerm(PermAuditView, func(w http.ResponseWriter, r *http.Request) {
		f := auditFilterFrom(r, auditCSVCap, auditCSVCap)
		events, err := b.store.AuditEvents(r.Context(), orgOf(r), f)
		if err != nil {
			fail(w, err)
			return
		}
		// The export is itself an event — a copy of the record has left the system — and it is
		// written before the file goes out, so a download that breaks off still left a line.
		b.audit(r, "export.audit", AuditEvent{TargetKind: "export", TargetID: "audit.csv",
			Details: auditDetails(auditFilterDetails(f, len(events)))})
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", "attachment; filename=attesttag-audit.csv")
		cw := csv.NewWriter(w)
		cw.Write([]string{"id", "time", "action", "outcome", "via", "actor_id", "actor_email", "actor_name", "actor_slack",
			"team_id", "target_kind", "target_id", "target_name", "ip", "user_agent", "details"})
		for _, e := range events {
			cw.Write([]string{strconv.FormatInt(e.ID, 10), e.At, e.Action, e.Outcome, e.Via, e.ActorPublic, e.ActorEmail, e.ActorName, e.ActorSlack,
				e.TeamID, e.TargetKind, e.TargetID, e.TargetName, e.IP, e.UserAgent, string(e.Details)})
		}
		cw.Flush()
	}))
}
