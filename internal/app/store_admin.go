package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

// ---- scopes ----

// A scope is one link in the Workspace -> Team -> Channel chain. Kind says which:
// 'workspace' is the account default (no Slack id of its own), 'team' is one connected
// Slack workspace, 'channel' is a channel inside one. SlackID is unique only within a
// team, so TeamID is always part of the key.
type Scope struct {
	ID                                      int64    `json:"id"`
	TeamID                                  string   `json:"team_id"`
	TeamName                                string   `json:"team_name"` // filled by the API layer, not stored here
	Kind, SlackID, Name                     string   `json:"-"`
	KindJ                                   string   `json:"kind"`
	SlackIDJ                                string   `json:"slack_id"`
	NameJ                                   string   `json:"name"`
	Instructions, DefaultModel, MemberEdits string   `json:"-"`
	InstructionsJ                           string   `json:"instructions"`
	DefaultModelJ                           string   `json:"default_model"`
	MemberEditsJ                            string   `json:"member_edits"`
	ReadAll                                 string   `json:"read_all"`     // inherit | on | off: read every message here and decide whether to answer
	EmailIntake                             string   `json:"email_intake"` // inherit | on | off: a mail forwarded to this channel's Slack address becomes a turn
	// inherit | on | off: whether the allow rules below are consulted at all on a turn a
	// forwarded email started. Off everywhere by default, and separate from EmailIntake
	// because taking mail and acting on it unattended are two decisions, not one.
	EmailAutoWrites string `json:"email_auto_writes"`
	DefaultRepo                             string   `json:"default_repo"` // owner/name; "" = inherit (channel) or none (workspace)
	MonthlyBudgetUSD                        float64  `json:"monthly_budget_usd"`
	IsPrivate                               bool     `json:"is_private"` // a private channel: shown with a lock, never a hash
	BundleIDs                               []int64  `json:"bundle_ids"`
	ConnectionIDs                           []int64  `json:"connection_ids"` // attached on their own, outside a bundle
	AllowRules                              []string `json:"allow_rules"`    // channel-level auto mode allow rules, on top of Settings
	LinkEpoch                               int64    `json:"link_epoch"`     // bumped to void every Configure link printed so far
	// How many tool rounds a turn here may spend before it has to answer; 0 = inherit. A
	// channel where the bot reads logs to answer needs more of them than a chat thread does.
	MaxToolRounds int `json:"max_tool_rounds"`
}

func (sc *Scope) sync() {
	sc.KindJ, sc.SlackIDJ, sc.NameJ = sc.Kind, sc.SlackID, sc.Name
	sc.InstructionsJ, sc.DefaultModelJ, sc.MemberEditsJ = sc.Instructions, sc.DefaultModel, sc.MemberEdits
	if sc.BundleIDs == nil {
		sc.BundleIDs = []int64{}
	}
	if sc.ConnectionIDs == nil {
		sc.ConnectionIDs = []int64{}
	}
	if sc.AllowRules == nil {
		sc.AllowRules = []string{}
	}
}

// UpsertScope creates or renames one scope. kind='workspace' takes an empty teamID and
// slackID; 'team' takes T… for both; 'channel' takes its team plus the channel id.
func (s *Store) UpsertScope(ctx context.Context, orgID int64, kind, teamID, slackID, name string) (*Scope, error) {
	_, err := s.db.ExecContext(ctx, `insert into scopes (org_id, kind, team_id, slack_id, name) values (?, ?, ?, ?, ?)
		on conflict(org_id, kind, team_id, slack_id) do update set name=excluded.name`, orgID, kind, teamID, slackID, name)
	if err != nil {
		return nil, err
	}
	return s.ScopeFor(ctx, orgID, kind, teamID, slackID)
}

// UpsertChannelScope is UpsertScope for a channel, which also carries whether it is private.
// Slack stopped encoding that in the id years ago — a private channel is a C…, not a G… —
// so it has to come from whoever asked Slack, and is refreshed on every scope sync.
func (s *Store) UpsertChannelScope(ctx context.Context, orgID int64, teamID, slackID, name string, private bool) (*Scope, error) {
	// Seeing the channel at all means the bot is in it, so this is also where a channel that
	// was removed comes back — with the instructions and bundles it had before, since the row
	// was only marked, never dropped.
	_, err := s.db.ExecContext(ctx, `insert into scopes (org_id, kind, team_id, slack_id, name, is_private) values (?, 'channel', ?, ?, ?, ?)
		on conflict(org_id, kind, team_id, slack_id) do update set name=excluded.name, is_private=excluded.is_private, left_at=''`,
		orgID, teamID, slackID, name, private)
	if err != nil {
		return nil, err
	}
	return s.ScopeFor(ctx, orgID, "channel", teamID, slackID)
}

func (s *Store) scanScope(row interface{ Scan(...any) error }) (*Scope, error) {
	var sc Scope
	var name, instr, model, me, ra, ei, eaw sql.NullString
	var budget sql.NullFloat64
	var rules string
	if err := row.Scan(&sc.ID, &sc.Kind, &sc.TeamID, &sc.SlackID, &name, &instr, &model, &me, &ra, &ei, &eaw, &budget, &rules, &sc.DefaultRepo, &sc.IsPrivate, &sc.LinkEpoch, &sc.MaxToolRounds); err != nil {
		return nil, err
	}
	sc.MonthlyBudgetUSD = budget.Float64
	json.Unmarshal([]byte(rules), &sc.AllowRules)
	sc.Name, sc.Instructions, sc.DefaultModel, sc.MemberEdits = name.String, instr.String, model.String, me.String
	sc.ReadAll = ra.String
	if sc.ReadAll == "" {
		sc.ReadAll = "inherit"
	}
	sc.EmailIntake = ei.String
	if sc.EmailIntake == "" {
		sc.EmailIntake = "inherit"
	}
	sc.EmailAutoWrites = eaw.String
	if sc.EmailAutoWrites == "" {
		sc.EmailAutoWrites = "inherit"
	}
	return &sc, nil
}

const scopeCols = `id, kind, team_id, slack_id, name, instructions, default_model, member_edits, read_all, coalesce(email_intake,'inherit'), coalesce(email_auto_writes,'inherit'), monthly_budget_usd, coalesce(allow_rules,'[]'), coalesce(default_repo,''), coalesce(is_private,0), coalesce(link_epoch,0), coalesce(max_tool_rounds,0)`

// ScopeFor looks one scope up by its real key. There is deliberately no by-slack-id-alone
// lookup: two teams can hold the same channel id (Slack Connect shares one C-id across
// workspaces), so a bare slack_id would silently pick whichever row was written first.
func (s *Store) ScopeFor(ctx context.Context, orgID int64, kind, teamID, slackID string) (*Scope, error) {
	sc, err := s.scanScope(s.db.QueryRowContext(ctx,
		`select `+scopeCols+` from scopes where org_id=? and kind=? and team_id=? and slack_id=?`, orgID, kind, teamID, slackID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sc.BundleIDs, _ = s.scopeBundleIDs(ctx, orgID, sc.ID)
	sc.ConnectionIDs, _ = s.scopeConnectionIDs(ctx, orgID, sc.ID)
	sc.sync()
	return sc, nil
}

// AccountScope is the top link: the organisation itself, above every connected workspace.
func (s *Store) AccountScope(ctx context.Context, orgID int64) (*Scope, error) {
	return s.ScopeFor(ctx, orgID, "workspace", "", "")
}

// TeamScope is the middle link: one connected Slack workspace.
func (s *Store) TeamScope(ctx context.Context, orgID int64, teamID string) (*Scope, error) {
	return s.ScopeFor(ctx, orgID, "team", teamID, teamID)
}

// ChannelScope is the narrowest link.
func (s *Store) ChannelScope(ctx context.Context, orgID int64, teamID, channel string) (*Scope, error) {
	return s.ScopeFor(ctx, orgID, "channel", teamID, channel)
}

// InheritedDefaultModel is the default model a channel answers on when its thread names none: the
// narrowest level that set one — the channel ("channel"), its workspace ("team") or the whole
// organisation ("workspace") — and which level that was. Empty when none did. One query rather
// than three scope reads, because it runs on every turn.
func (s *Store) InheritedDefaultModel(ctx context.Context, orgID int64, teamID, channel string) (model, level string) {
	rows, err := s.db.QueryContext(ctx, `select kind, default_model from scopes
		where org_id=? and coalesce(default_model,'') <> '' and (
			(kind='channel' and team_id=? and slack_id=?) or
			(kind='team' and team_id=? and slack_id=?) or
			(kind='workspace' and team_id='' and slack_id=''))`, orgID, teamID, channel, teamID, teamID)
	if err != nil {
		return "", ""
	}
	defer rows.Close()
	rank := map[string]int{"channel": 3, "team": 2, "workspace": 1}
	best := 0
	for rows.Next() {
		var kind, m string
		if rows.Scan(&kind, &m) == nil && rank[kind] > best {
			best, model, level = rank[kind], m, kind
		}
	}
	return model, level
}

// ScopeByID takes the organisation as well as the id: the id arrives from a URL, and without the
// predicate a request naming another organisation's scope would read and write it.
func (s *Store) ScopeByID(ctx context.Context, orgID, id int64) (*Scope, error) {
	sc, err := s.scanScope(s.db.QueryRowContext(ctx, `select `+scopeCols+` from scopes where org_id=? and id=?`, orgID, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sc.BundleIDs, _ = s.scopeBundleIDs(ctx, orgID, sc.ID)
	sc.ConnectionIDs, _ = s.scopeConnectionIDs(ctx, orgID, sc.ID)
	sc.sync()
	return sc, nil
}

func (s *Store) Scopes(ctx context.Context, orgID int64) ([]*Scope, error) {
	rows, err := s.db.QueryContext(ctx, `select `+scopeCols+` from scopes where org_id=? and left_at=''
		order by case kind when 'workspace' then 0 else 1 end, team_id,
		         case kind when 'team' then 0 else 1 end, name`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Scope
	for rows.Next() {
		sc, err := s.scanScope(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	for _, sc := range out {
		sc.BundleIDs, _ = s.scopeBundleIDs(ctx, orgID, sc.ID)
		sc.ConnectionIDs, _ = s.scopeConnectionIDs(ctx, orgID, sc.ID)
		sc.sync()
	}
	if out == nil {
		out = []*Scope{}
	}
	return out, rows.Err()
}

func (s *Store) UpdateScope(ctx context.Context, orgID, id int64, instructions, defaultModel, memberEdits string) error {
	_, err := s.db.ExecContext(ctx, `update scopes set instructions=?, default_model=?, member_edits=? where org_id=? and id=?`,
		instructions, defaultModel, memberEdits, orgID, id)
	return err
}

// SetScopeReadAll stores the "read every message" mode for a scope: inherit | on | off. It is
// its own setter rather than another string on UpdateScope, which carries the three settings a
// form posts together and would take a bare "inherit" at every other call site.
func (s *Store) SetScopeReadAll(ctx context.Context, orgID, id int64, mode string) error {
	if mode != "on" && mode != "off" {
		mode = "inherit"
	}
	_, err := s.db.ExecContext(ctx, `update scopes set read_all=? where org_id=? and id=?`, mode, orgID, id)
	return err
}

// SetScopeEmailIntake stores whether a mail forwarded to this channel's Slack address becomes a
// turn: inherit | on | off. Its own setter for the same reason SetScopeReadAll is one, and the
// same narrow set of values — a turn nobody in the workspace started is not a thing to enable by
// posting a form that happened to leave the field out.
func (s *Store) SetScopeEmailIntake(ctx context.Context, orgID, id int64, mode string) error {
	if mode != "on" && mode != "off" {
		mode = "inherit"
	}
	_, err := s.db.ExecContext(ctx, `update scopes set email_intake=? where org_id=? and id=?`, mode, orgID, id)
	return err
}

// SetScopeEmailAutoWrites stores whether this channel's allow rules are consulted at all on a
// turn a forwarded email started: inherit | on | off. Its own setter, and the same narrow set of
// values, for the reason the two above are: this one decides whether a write can happen on that
// lane with nobody in the room, and a form that left the field out must not be able to say yes.
func (s *Store) SetScopeEmailAutoWrites(ctx context.Context, orgID, id int64, mode string) error {
	if mode != "on" && mode != "off" {
		mode = "inherit"
	}
	_, err := s.db.ExecContext(ctx, `update scopes set email_auto_writes=? where org_id=? and id=?`, mode, orgID, id)
	return err
}

// CountChannelScopes is how many channels of one workspace the bot is in — the number the rail
// draws under it. Its own query rather than len(Scopes()): the caller wants a count, and Scopes
// resolves every row's bundles and connections one query at a time to hand it back.
func (s *Store) CountChannelScopes(ctx context.Context, orgID int64, teamID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`select count(*) from scopes where org_id=? and kind='channel' and team_id=? and left_at=''`,
		orgID, teamID).Scan(&n)
	return n, err
}

// MarkScopeLeft records that the bot is no longer in this channel. The row stays: everything
// configured here — instructions, budget, the bundles and connections attached — is kept for
// the day somebody invites the bot back, and until then the channel is simply not listed.
// Channels only: the account and a workspace are not places the bot can be removed from.
func (s *Store) MarkScopeLeft(ctx context.Context, orgID, id int64) error {
	_, err := s.db.ExecContext(ctx, `update scopes set left_at=? where org_id=? and id=? and kind='channel'`,
		time.Now().UTC().Format(time.DateTime), orgID, id)
	return err
}

// BumpLinkEpoch voids every Configure link minted for a scope so far: links carry the epoch
// they were minted under, and the page refuses any that no longer match.
func (s *Store) BumpLinkEpoch(ctx context.Context, orgID, id int64) error {
	_, err := s.db.ExecContext(ctx, `update scopes set link_epoch=link_epoch+1 where org_id=? and id=?`, orgID, id)
	return err
}

func (s *Store) SetScopeAllowRules(ctx context.Context, orgID, id int64, rules []string) error {
	raw, _ := json.Marshal(rules)
	_, err := s.db.ExecContext(ctx, `update scopes set allow_rules=? where org_id=? and id=?`, string(raw), orgID, id)
	return err
}

// SetScopeDefaultRepo stores the owner/name the model should assume in this scope; "" clears it.
func (s *Store) SetScopeDefaultRepo(ctx context.Context, orgID, id int64, repo string) error {
	_, err := s.db.ExecContext(ctx, `update scopes set default_repo=? where org_id=? and id=?`, repo, orgID, id)
	return err
}

// SetScopeMaxToolRounds stores how hard the bot may dig in a scope; 0 restores inheritance.
// The ceiling is the same one the turn applies, checked here so the console cannot store a
// number that would only be silently clamped later.
func (s *Store) SetScopeMaxToolRounds(ctx context.Context, orgID, id int64, rounds int) error {
	if rounds < 0 {
		rounds = 0
	}
	if rounds > maxRoundsCeiling {
		rounds = maxRoundsCeiling
	}
	_, err := s.db.ExecContext(ctx, `update scopes set max_tool_rounds=? where org_id=? and id=?`, rounds, orgID, id)
	return err
}

func (s *Store) SetScopeBudget(ctx context.Context, orgID, id int64, usd float64) error {
	_, err := s.db.ExecContext(ctx, `update scopes set monthly_budget_usd=? where org_id=? and id=?`, usd, orgID, id)
	return err
}

func (s *Store) scopeBundleIDs(ctx context.Context, orgID, scopeID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `select bundle_id from scope_bundles where org_id=? and scope_id=? order by bundle_id`, orgID, scopeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids, nil
}

// AttachBundle grants a bundle at a scope. It is one statement with an existence check on both
// sides rather than a read-then-write, so there is no gap in which either id could belong to
// somebody else — and no ownership check a later caller could forget to make.
func (s *Store) AttachBundle(ctx context.Context, orgID, scopeID, bundleID int64) error {
	res, err := s.db.ExecContext(ctx, `insert into scope_bundles (org_id, scope_id, bundle_id)
		select ?, ?, ? where exists(select 1 from scopes where id=? and org_id=?)
		            and exists(select 1 from bundles where id=? and org_id=?) on conflict do nothing`,
		orgID, scopeID, bundleID, scopeID, orgID, bundleID, orgID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Either it was already attached, or one of the two is not ours. Re-read to tell them
		// apart rather than reporting a duplicate as a failure.
		var ok int
		s.db.QueryRowContext(ctx, `select count(*) from scope_bundles where org_id=? and scope_id=? and bundle_id=?`,
			orgID, scopeID, bundleID).Scan(&ok)
		if ok == 0 {
			return errors.New("no such scope or bundle")
		}
	}
	return nil
}

func (s *Store) DetachBundle(ctx context.Context, orgID, scopeID, bundleID int64) error {
	_, err := s.db.ExecContext(ctx, `delete from scope_bundles where org_id=? and scope_id=? and bundle_id=?`, orgID, scopeID, bundleID)
	return err
}

// One-off grants: a connection attached to a scope on its own. The scope gets that
// connection (and its preset's tool pack, when its bundle enables it) and nothing else
// from the bundle.

func (s *Store) scopeConnectionIDs(ctx context.Context, orgID, scopeID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `select connection_id from scope_connections where org_id=? and scope_id=? order by connection_id`, orgID, scopeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids, nil
}

func (s *Store) AttachConnection(ctx context.Context, orgID, scopeID, connID int64) error {
	res, err := s.db.ExecContext(ctx, `insert into scope_connections (org_id, scope_id, connection_id)
		select ?, ?, ? where exists(select 1 from scopes where id=? and org_id=?)
		            and exists(select 1 from connections where id=? and org_id=?) on conflict do nothing`,
		orgID, scopeID, connID, scopeID, orgID, connID, orgID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var ok int
		s.db.QueryRowContext(ctx, `select count(*) from scope_connections where org_id=? and scope_id=? and connection_id=?`,
			orgID, scopeID, connID).Scan(&ok)
		if ok == 0 {
			return errors.New("no such scope or connection")
		}
	}
	return nil
}

func (s *Store) DetachConnection(ctx context.Context, orgID, scopeID, connID int64) error {
	_, err := s.db.ExecContext(ctx, `delete from scope_connections where org_id=? and scope_id=? and connection_id=?`, orgID, scopeID, connID)
	return err
}

// connectionScopeIDs lists the scopes a connection is attached to on its own.
func (s *Store) connectionScopeIDs(ctx context.Context, orgID, connID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `select scope_id from scope_connections where org_id=? and connection_id=? order by scope_id`, orgID, connID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids, nil
}

// ---- bundles ----

type Bundle struct {
	ID           int64         `json:"id"`
	Name         string        `json:"name"`
	Instructions string        `json:"instructions"`
	ToolPacks    []string      `json:"tool_packs"`
	CreatedBy    string        `json:"created_by"`
	CreatedAt    string        `json:"created_at"`
	Connections  []*Connection `json:"connections"`
	Domains      []Domain      `json:"domains"`
	Skills       []Skill       `json:"skills"`
	ScopeIDs     []int64       `json:"scope_ids"`
	UsedIn       int           `json:"used_in"`
}

type Domain struct {
	ID       int64  `json:"id"`
	BundleID int64  `json:"bundle_id"`
	Host     string `json:"host"`
	Ports    string `json:"ports"`
}

func (s *Store) CreateBundle(ctx context.Context, orgID int64, name, by string) (*Bundle, error) {
	if s.countRows(ctx, "bundles", orgID) >= maxBundlesPerOrg {
		return nil, errOrgCap
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `insert into bundles (org_id, name, created_by) values (?, ?, ?) returning id`, orgID, name, by).Scan(&id)
	if err != nil {
		return nil, err
	}
	return s.Bundle(ctx, orgID, id)
}

func (s *Store) UpdateBundle(ctx context.Context, orgID, id int64, name, instructions string, packs []string) error {
	pj, _ := json.Marshal(packs)
	_, err := s.db.ExecContext(ctx, `update bundles set name=?, instructions=?, tool_packs=?, updated_at=? where org_id=? and id=?`,
		name, instructions, string(pj), now(), orgID, id)
	return err
}

// DeleteBundle removes a bundle and everything hanging off it. Every statement carries the
// organisation, including the ones that look like they are already narrowed by bundle_id: an id
// arrives from a URL, and "it must belong to us because we looked it up" is the assumption that
// stops being true the first time somebody forgets the lookup.
func (s *Store) DeleteBundle(ctx context.Context, orgID, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`delete from scope_connections where org_id=? and connection_id in (select id from connections where bundle_id=?)`,
		`delete from connections where org_id=? and bundle_id=?`,
		`delete from domains where org_id=? and bundle_id=?`,
		`delete from scope_bundles where org_id=? and bundle_id=?`,
		`delete from bundles where org_id=? and id=?`} {
		if _, err := tx.ExecContext(ctx, q, orgID, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Bundle(ctx context.Context, orgID, id int64) (*Bundle, error) {
	bs, err := s.bundles(ctx, orgID, id)
	if err != nil || len(bs) == 0 {
		return nil, err
	}
	return bs[0], nil
}

func (s *Store) Bundles(ctx context.Context, orgID int64) ([]*Bundle, error) {
	return s.bundles(ctx, orgID, 0)
}

func (s *Store) bundles(ctx context.Context, orgID, only int64) ([]*Bundle, error) {
	q, args := `select id, name, instructions, tool_packs, coalesce(created_by,''), created_at from bundles where org_id=?`, []any{orgID}
	if only != 0 {
		q += ` and id=?`
		args = append(args, only)
	}
	rows, err := s.db.QueryContext(ctx, q+` order by name`, args...)
	if err != nil {
		return nil, err
	}
	var out []*Bundle
	for rows.Next() {
		var b Bundle
		var packs string
		if err := rows.Scan(&b.ID, &b.Name, &b.Instructions, &packs, &b.CreatedBy, &b.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		json.Unmarshal([]byte(packs), &b.ToolPacks)
		if b.ToolPacks == nil {
			b.ToolPacks = []string{}
		}
		out = append(out, &b)
	}
	rows.Close()
	// Every one of these is reported rather than discarded. They used to be assigned through
	// `_`, on the reading that a bundle is still worth drawing when one of its lists cannot be
	// fetched. That is true of a bundle with nothing in it and false of one whose credentials
	// are unreadable, and the two are indistinguishable once the error is dropped: a missing
	// column on the connections table rendered every bundle in the console as holding no
	// credentials, for a day, without one line in the log. An empty list means empty; a
	// failure to read one is a 500 that says what broke.
	for _, b := range out {
		var err error
		if b.Connections, err = s.ConnectionsForBundle(ctx, orgID, b.ID); err != nil {
			return nil, fmt.Errorf("bundle %d connections: %w", b.ID, err)
		}
		if b.Domains, err = s.DomainsForBundle(ctx, orgID, b.ID); err != nil {
			return nil, fmt.Errorf("bundle %d domains: %w", b.ID, err)
		}
		if b.Skills, err = s.SkillsForBundle(ctx, orgID, b.ID); err != nil {
			return nil, fmt.Errorf("bundle %d skills: %w", b.ID, err)
		}
		b.ScopeIDs = []int64{}
		r2, err := s.db.QueryContext(ctx, `select scope_id from scope_bundles where org_id=? and bundle_id=?`, orgID, b.ID)
		if err != nil {
			return nil, fmt.Errorf("bundle %d scopes: %w", b.ID, err)
		}
		for r2.Next() {
			var id int64
			if err := r2.Scan(&id); err != nil {
				r2.Close()
				return nil, err
			}
			b.ScopeIDs = append(b.ScopeIDs, id)
		}
		err = r2.Err()
		r2.Close()
		if err != nil {
			return nil, err
		}
		b.UsedIn = len(b.ScopeIDs)
	}
	if out == nil {
		out = []*Bundle{}
	}
	return out, nil
}

func (s *Store) AddDomain(ctx context.Context, orgID, bundleID int64, host, ports string) (int64, error) {
	if ports == "" {
		ports = "443"
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `insert into domains (org_id, bundle_id, host, ports) values (?, ?, ?, ?) returning id`,
		orgID, bundleID, strings.ToLower(host), ports).Scan(&id)
	return id, err
}

func (s *Store) DeleteDomain(ctx context.Context, orgID, id int64) error {
	_, err := s.db.ExecContext(ctx, `delete from domains where org_id=? and id=?`, orgID, id)
	return err
}

func (s *Store) DomainsForBundle(ctx context.Context, orgID, bundleID int64) ([]Domain, error) {
	rows, err := s.db.QueryContext(ctx, `select id, bundle_id, host, ports from domains where org_id=? and bundle_id=? order by host`, orgID, bundleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Domain{}
	for rows.Next() {
		var d Domain
		if err := rows.Scan(&d.ID, &d.BundleID, &d.Host, &d.Ports); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---- connections ----

type Connection struct {
	ID           int64    `json:"id"`
	BundleID     int64    `json:"bundle_id"`
	Name         string   `json:"name"`
	Preset       string   `json:"preset"`
	CredType     string   `json:"cred_type"`
	AllowedHosts []string `json:"allowed_hosts"`
	PathPrefixes []string `json:"path_prefixes"`
	Methods      []string `json:"methods"`
	Headers      []Header `json:"headers"`
	Writes       string   `json:"writes"`
	Notes        string   `json:"notes"`
	Repo         string   `json:"repo"` // owner/name when this connection is a repository (github preset)
	// GitHubInstallationID is set when this repository is reached through a GitHub App install
	// rather than a pasted token: the secret then holds that id and no token at all.
	GitHubInstallationID int64 `json:"github_installation_id"`
	// SecretFP is the first bytes of a SHA-256 of the token this repository was connected
	// with, hex. Not a credential: it identifies the credential without carrying it.
	SecretFP    string `json:"secret_fp"`
	AllowGrants bool   `json:"allow_grants"` // may an approved access request spend this credential
	TestCmd     string `json:"test_cmd"`     // repositories: the older single-command test override; folded into Recipe
	// Recipe is how this repository is set up, built and tested, when someone told us. Nil
	// means the worker works it out from the clone, and writes back what it found (jobs.go),
	// so the second job on a repository starts from what the first one learned.
	Recipe    *Recipe `json:"recipe,omitempty"`
	Status    string  `json:"status"`
	LastUsed  string  `json:"last_used"`
	CreatedBy string  `json:"created_by"`
	CreatedAt string  `json:"created_at"`
	HasSecret bool    `json:"has_secret"`
	ScopeIDs  []int64 `json:"scope_ids"` // scopes it is attached to on its own, outside its bundle
	secretEnc []byte
}

// Header is an extra header the proxy adds; the value is stored sealed inside the secret.
type Header struct {
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
}

// Secret is the sealed part of a connection. Only the fields relevant to cred_type are set.
type Secret struct {
	Token        string `json:"token,omitempty"`         // bearer / header / query
	User         string `json:"user,omitempty"`          // basic
	Password     string `json:"password,omitempty"`      // basic
	HeaderName   string `json:"header_name,omitempty"`   // header / query param name
	ClientID     string `json:"client_id,omitempty"`     // oauth2_cc
	ClientSecret string `json:"client_secret,omitempty"` // oauth2_cc
	TokenURL     string `json:"token_url,omitempty"`     // oauth2_cc
	Scopes       string `json:"scopes,omitempty"`        // oauth2_cc / gcp_sa
	SAJSON       string `json:"sa_json,omitempty"`       // gcp_sa
	MCPURL       string `json:"mcp_url,omitempty"`       // mcp
	// github_app: not a credential at all, a pointer. The token is minted per call from the
	// app's own key (github_token.go) and scoped to this connection's repository.
	InstallationID int64 `json:"installation_id,omitempty"`

	// aws_sigv4. There is no token to inject: the proxy signs each request with
	// these, and only a hash goes over the wire. Region and service are read off
	// the hostname when it names them; these are the override.
	AWSKeyID        string `json:"aws_key_id,omitempty"`
	AWSSecret       string `json:"aws_secret,omitempty"`
	AWSRegion       string `json:"aws_region,omitempty"`
	AWSService      string `json:"aws_service,omitempty"`
	AWSSessionToken string `json:"aws_session_token,omitempty"`

	Headers map[string]string `json:"headers,omitempty"` // extra header values by name
	OAuth   *OAuthState       `json:"oauth,omitempty"`   // mcp with OAuth 2.0 authorization-code
}

const connCols = `id, bundle_id, name, preset, cred_type, secret_enc, allowed_hosts, path_prefixes, methods, headers, writes, notes, status, coalesce(last_used,''), coalesce(created_by,''), created_at, coalesce(repo,''), coalesce(allow_grants,0), coalesce(test_cmd,''), coalesce(recipe,''), coalesce(github_installation_id,0), coalesce(secret_fp,'')`

func scanConn(row interface{ Scan(...any) error }) (*Connection, error) {
	var c Connection
	var hosts, prefixes, methods, headers, recipe string
	if err := row.Scan(&c.ID, &c.BundleID, &c.Name, &c.Preset, &c.CredType, &c.secretEnc, &hosts, &prefixes, &methods, &headers,
		&c.Writes, &c.Notes, &c.Status, &c.LastUsed, &c.CreatedBy, &c.CreatedAt, &c.Repo, &c.AllowGrants, &c.TestCmd, &recipe, &c.GitHubInstallationID, &c.SecretFP); err != nil {
		return nil, err
	}
	if strings.TrimSpace(recipe) != "" {
		var r Recipe
		if json.Unmarshal([]byte(recipe), &r) == nil {
			c.Recipe = &r
		}
	}
	json.Unmarshal([]byte(hosts), &c.AllowedHosts)
	json.Unmarshal([]byte(prefixes), &c.PathPrefixes)
	json.Unmarshal([]byte(methods), &c.Methods)
	json.Unmarshal([]byte(headers), &c.Headers)
	for _, p := range []*[]string{&c.AllowedHosts, &c.PathPrefixes, &c.Methods} {
		if *p == nil {
			*p = []string{}
		}
	}
	if c.Headers == nil {
		c.Headers = []Header{}
	}
	c.HasSecret = len(c.secretEnc) > 0
	c.ScopeIDs = []int64{}
	return &c, nil
}

func (s *Store) ConnectionsForBundle(ctx context.Context, orgID, bundleID int64) ([]*Connection, error) {
	rows, err := s.db.QueryContext(ctx, `select `+connCols+` from connections where org_id=? and bundle_id=? order by name`, orgID, bundleID)
	if err != nil {
		return nil, err
	}
	out := []*Connection{}
	for rows.Next() {
		c, err := scanConn(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, c)
	}
	err = rows.Err()
	rows.Close() // single sqlite connection: release it before the per-row lookups
	for _, c := range out {
		c.ScopeIDs, _ = s.connectionScopeIDs(ctx, orgID, c.ID)
	}
	return out, err
}

// ConnectionIDsForBundle is which connections a bundle holds, for reading before DeleteBundle
// takes them: what hangs off a connection by id outlives the row, and once the row has gone
// these ids are the only record of which connections the bundle had.
func (s *Store) ConnectionIDsForBundle(ctx context.Context, orgID, bundleID int64) ([]int64, error) {
	return s.idList(ctx, `select id from connections where org_id=? and bundle_id=? order by id`, orgID, bundleID)
}

// AllConnections is every connection in one organisation, whichever bundle it sits in. For
// the places that care what a credential can reach rather than where it is filed — picking the
// one a Drive sync spends, say. The sealed secret rides on an unexported field either way, so
// what a caller can read is the shape of the connection and never the credential.
func (s *Store) AllConnections(ctx context.Context, orgID int64) ([]*Connection, error) {
	rows, err := s.db.QueryContext(ctx, `select `+connCols+` from connections where org_id=? order by name`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Connection{}
	for rows.Next() {
		c, err := scanConn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Connection takes the organisation as well as the id. The id comes from a URL and the row
// holds a sealed credential, so a missing predicate here hands one customer another's secret.
func (s *Store) Connection(ctx context.Context, orgID, id int64) (*Connection, error) {
	c, err := scanConn(s.db.QueryRowContext(ctx, `select `+connCols+` from connections where org_id=? and id=?`, orgID, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.ScopeIDs, _ = s.connectionScopeIDs(ctx, orgID, c.ID)
	return c, nil
}

func (s *Store) InsertConnection(ctx context.Context, orgID int64, c *Connection, secretEnc []byte) (int64, error) {
	if s.countRows(ctx, "connections", orgID) >= maxConnectionsPerOrg {
		return 0, errOrgCap
	}
	hosts, _ := json.Marshal(c.AllowedHosts)
	prefixes, _ := json.Marshal(c.PathPrefixes)
	methods, _ := json.Marshal(c.Methods)
	headers, _ := json.Marshal(c.Headers)
	var id int64
	err := s.db.QueryRowContext(ctx, `insert into connections
		(org_id, bundle_id, name, preset, cred_type, secret_enc, allowed_hosts, path_prefixes, methods, headers, writes, notes, status, created_by, repo, allow_grants, test_cmd, recipe, github_installation_id, secret_fp)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) returning id`,
		orgID, c.BundleID, c.Name, c.Preset, c.CredType, secretEnc, string(hosts), string(prefixes), string(methods), string(headers),
		c.Writes, c.Notes, c.Status, c.CreatedBy, c.Repo, c.AllowGrants, c.TestCmd, jsonOrEmpty(c.Recipe), c.GitHubInstallationID, c.SecretFP).Scan(&id)
	return id, err
}

func (s *Store) UpdateConnection(ctx context.Context, orgID int64, c *Connection, secretEnc []byte) error {
	hosts, _ := json.Marshal(c.AllowedHosts)
	prefixes, _ := json.Marshal(c.PathPrefixes)
	methods, _ := json.Marshal(c.Methods)
	headers, _ := json.Marshal(c.Headers)
	setSecret := ""
	args := []any{c.Name, string(hosts), string(prefixes), string(methods), string(headers), c.Writes, c.Notes, c.Status, c.Repo, c.AllowGrants, c.TestCmd, jsonOrEmpty(c.Recipe), c.GitHubInstallationID, c.SecretFP, now()}
	if secretEnc != nil {
		setSecret = ", secret_enc=?"
		args = append(args, secretEnc)
	}
	args = append(args, orgID, c.ID)
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`update connections set name=?, allowed_hosts=?, path_prefixes=?, methods=?,
		headers=?, writes=?, notes=?, status=?, repo=?, allow_grants=?, test_cmd=?, recipe=?, github_installation_id=?, secret_fp=?, updated_at=?%s
		where org_id=? and id=?`, setSecret), args...)
	return err
}

// CopyConnection duplicates a connection (including its sealed secret) into another bundle.
func (s *Store) CopyConnection(ctx context.Context, orgID, id, toBundle int64, by string) (int64, error) {
	var newID int64
	err := s.db.QueryRowContext(ctx, `insert into connections
		(org_id, bundle_id, name, preset, cred_type, secret_enc, allowed_hosts, path_prefixes, methods, headers, writes, notes, status, created_by, repo, allow_grants, test_cmd, recipe, github_installation_id, secret_fp)
		-- The installation id and the token's digest come along: the copy spends the same
		-- credential (its sealed secret carries the id too), so a copy that dropped them would
		-- be filed under the wrong source in the console while working perfectly.
		select org_id, ?, name, preset, cred_type, secret_enc, allowed_hosts, path_prefixes, methods, headers, writes, notes, status, ?, coalesce(repo,''), coalesce(allow_grants,0), coalesce(test_cmd,''), coalesce(recipe,''), coalesce(github_installation_id,0), coalesce(secret_fp,'')
		from connections where org_id=? and id=? and exists(select 1 from bundles where id=? and org_id=?)
		returning id`,
		toBundle, by, orgID, id, toBundle, orgID).Scan(&newID)
	return newID, err
}

func (s *Store) DeleteConnection(ctx context.Context, orgID, id int64) error {
	if _, err := s.db.ExecContext(ctx, `delete from scope_connections where org_id=? and connection_id=?`, orgID, id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `delete from connections where org_id=? and id=?`, orgID, id)
	return err
}

func (s *Store) TouchConnection(ctx context.Context, orgID, id int64) {
	s.db.ExecContext(ctx, `update connections set last_used=? where org_id=? and id=?`, now(), orgID, id)
}

// ---- proxy audit ----

type ProxyAudit struct {
	ID                              int64  `json:"id"`
	TeamID                          string `json:"team_id"`
	Channel, ThreadTS, Requester    string `json:"-"`
	ChannelJ                        string `json:"channel"`
	ChannelName                     string `json:"channel_name,omitempty"` // what the console calls it; only the Activity list fills it in
	ThreadTSJ                       string `json:"thread_ts"`
	RequesterJ                      string `json:"requester"`
	ConnectionID                    int64  `json:"connection_id"`
	Method, Host, Path, Blocked, At string `json:"-"`
	MethodJ                         string `json:"method"`
	HostJ                           string `json:"host"`
	PathJ                           string `json:"path"`
	Status                          int    `json:"status"`
	MS                              int64  `json:"ms"`
	BlockedJ                        string `json:"blocked"`
	AtJ                             string `json:"created_at"`
	AccessRequestID                 int64  `json:"access_request_id"` // set when this ran off an approved access request
}

func (s *Store) LogProxy(ctx context.Context, orgID int64, a ProxyAudit) {
	s.db.ExecContext(ctx, `insert into proxy_audit (org_id, team_id, channel, thread_ts, requester, connection_id, method, host, path, status, ms, blocked, access_request_id)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, orgID, a.TeamID, a.Channel, a.ThreadTS, a.Requester, a.ConnectionID, a.Method, a.Host, a.Path, a.Status, a.MS, a.Blocked, a.AccessRequestID)
}

// ProxyAudits returns the newest requests first. failedOnly keeps the ones that did not
// go through: blocked by a rule, or answered with an error status.
func (s *Store) ProxyAudits(ctx context.Context, orgID int64, channel string, limit int, failedOnly bool, since string) ([]ProxyAudit, error) {
	q := `select id, coalesce(team_id,''), coalesce(channel,''), coalesce(thread_ts,''), coalesce(requester,''), coalesce(connection_id,0),
		coalesce(method,''), coalesce(host,''), coalesce(path,''), coalesce(status,0), coalesce(ms,0), coalesce(blocked,''), created_at,
		coalesce(access_request_id,0)
		from proxy_audit where org_id=?`
	args := []any{orgID}
	if channel != "" {
		q += ` and channel=?`
		args = append(args, channel)
	}
	if failedOnly {
		q += ` and (coalesce(blocked,'') <> '' or coalesce(status,0) >= 400)`
	}
	if since != "" {
		q += ` and created_at >= ?`
		args = append(args, since)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q+` order by id desc limit ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProxyAudit{}
	for rows.Next() {
		var a ProxyAudit
		if err := rows.Scan(&a.ID, &a.TeamID, &a.Channel, &a.ThreadTS, &a.Requester, &a.ConnectionID, &a.Method, &a.Host, &a.Path, &a.Status, &a.MS, &a.Blocked, &a.At, &a.AccessRequestID); err != nil {
			return nil, err
		}
		a.ChannelJ, a.ThreadTSJ, a.RequesterJ, a.MethodJ, a.HostJ, a.PathJ, a.BlockedJ, a.AtJ = a.Channel, a.ThreadTS, a.Requester, a.Method, a.Host, a.Path, a.Blocked, a.At
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---- admin sessions ----

func randomToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// hashSessionToken is what admin_sessions stores. The cookie is the credential; the row is a
// lookup key, and a copy of the database (a replica, a backup, a support dump) must not be a
// set of working sign-ins. Same rule as email tokens and developer keys.
func hashSessionToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// rehashSessionTokens is the one-off migration for rows written before tokens were hashed: a
// raw token is 48 hex characters, a hash 64, so the rows that still hold a credential are the
// ones to rewrite. Idempotent, and it keeps everyone signed in.
func rehashSessionTokens(db *database) error {
	rows, err := db.Query(`select token from admin_sessions where length(token)=48`)
	if err != nil {
		return err
	}
	var raw []string
	for rows.Next() {
		var tok string
		if err := rows.Scan(&tok); err != nil {
			rows.Close()
			return err
		}
		raw = append(raw, tok)
	}
	rows.Close()
	for _, tok := range raw {
		if _, err := db.Exec(`update admin_sessions set token=? where token=?`, hashSessionToken(tok), tok); err != nil {
			return err
		}
	}
	return nil
}

// AdminUser is who is making a request, and which organisation they are making it in. Both
// halves matter: authority comes from the membership joining them, never from the user alone.
type AdminUser struct {
	ID     int64  `json:"-"`       // users.id — the join key, and it stays in the process
	UserID string `json:"user_id"` // the Slack user id when they signed in that way; display only
	// PublicID is users.public_id: what "the account id" means to the console, the developer
	// API and anything else outside. See Org.PublicID for why the serial does not go out.
	PublicID string `json:"id"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	// OrgID is the organisation this session is acting in. A user who belongs to several picks
	// one; every read and write on the request is scoped to it. OrgPublic is the same
	// organisation as the outside names it, and is what responses carry as "org_id".
	OrgID     int64  `json:"-"`
	OrgPublic string `json:"org_id"`
	OrgName   string `json:"org_name"`
	OrgSlug   string `json:"org_slug"`
	// Role and Permissions are resolved per request rather than stored on the session, so a
	// change to somebody's tier takes effect on their next click instead of their next sign-in.
	Role        string          `json:"role"`
	Permissions map[string]bool `json:"permissions"`
	// Via is how this session was signed in: 'password', 'slack', 'microsoft', 'sso' or 'signup'.
	// The organisation's sign-in policy and its two-factor requirement are both read against it,
	// so it has to survive on the session rather than being asked again per request.
	Via string `json:"via"`
	// APIKeyID is the key a /v1 request authenticated with, and zero for a session. The audit
	// log records it, so what a key did can be tied to the key after it is rotated or revoked.
	APIKeyID int64 `json:"-"`
	// MCPGrantID is the connected MCP client a request came through, when it came that way
	// (mcp_oauth.go); zero otherwise. Recorded for the same reason as APIKeyID.
	MCPGrantID  int64 `json:"-"`
	MFAVerified bool  `json:"-"`
	// CreatedAt is when the session was minted, in the store's UTC text form. A change that
	// would let a stolen cookie keep the account can ask for a session younger than a few
	// minutes when there is no password or authenticator to ask for instead.
	CreatedAt string `json:"-"`
}

// sessionAge is how long ago the session was minted, or -1 when that is not known.
func (u *AdminUser) sessionAge() time.Duration {
	t, err := time.ParseInLocation(time.DateTime, u.CreatedAt, time.UTC)
	if err != nil {
		return -1
	}
	return time.Since(t)
}

// CreateAdminSession opens a session for one user acting in one organisation. The org is on the
// session rather than resolved per request so that switching organisation is an explicit act
// with its own audit line, not a query parameter anyone can vary.
func (s *Store) CreateAdminSession(ctx context.Context, u AdminUser, ttl time.Duration) (string, error) {
	tok := randomToken()
	_, err := s.db.ExecContext(ctx, `insert into admin_sessions (token, id, user_id, org_id, name, email, via, expires_at, mfa_verified)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		hashSessionToken(tok), u.ID, u.UserID, u.OrgID, u.Name, u.Email, u.Via, time.Now().Add(ttl).UTC().Format(time.DateTime), u.MFAVerified)
	return tok, err
}

// SetSessionOrg moves a session to another organisation the same user belongs to. The caller
// checks the membership; this only writes it.
func (s *Store) SetSessionOrg(ctx context.Context, token string, orgID int64) error {
	_, err := s.db.ExecContext(ctx, `update admin_sessions set org_id=? where token=?`, orgID, hashSessionToken(token))
	return err
}

// MarkSessionMFA records that this session presented the second factor, so the check that
// refuses an enrolled account's session without one lets it through from here on.
func (s *Store) MarkSessionMFA(ctx context.Context, token string, userID int64) error {
	_, err := s.db.ExecContext(ctx, `update admin_sessions set mfa_verified=1 where token=? and id=?`, hashSessionToken(token), userID)
	return err
}

func (s *Store) AdminSession(ctx context.Context, tok string) (*AdminUser, error) {
	var u AdminUser
	err := s.db.QueryRowContext(ctx, `select coalesce(a.id,0), coalesce(a.org_id,0), coalesce(a.user_id,''),
		coalesce(a.name,''), coalesce(a.email,''), coalesce(o.name,''), coalesce(o.slug,''), coalesce(a.via,''), a.mfa_verified, coalesce(a.created_at,''),
		coalesce(usr.public_id,''), coalesce(o.public_id,'')
		from admin_sessions a left join orgs o on o.id = a.org_id left join users usr on usr.id = a.id
		where a.token=? and a.expires_at > ?`, hashSessionToken(tok), now()).
		Scan(&u.ID, &u.OrgID, &u.UserID, &u.Name, &u.Email, &u.OrgName, &u.OrgSlug, &u.Via, &u.MFAVerified, &u.CreatedAt,
			&u.PublicID, &u.OrgPublic)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &u, err
}

// DeleteSessionsFor ends every session belonging to one account. A password reset is what
// somebody does when they think the account is compromised, so leaving the other side signed in
// would defeat the point of it.
func (s *Store) DeleteSessionsFor(ctx context.Context, userID int64) {
	s.db.ExecContext(ctx, `delete from admin_sessions where id=?`, userID)
}

// DeleteOtherSessionsFor ends every session of an account except the one making the change.
// Changing a password from inside the account is the same act as a reset — "I think somebody
// else has this" — and it has to close the other consoles too, just not the one in hand.
func (s *Store) DeleteOtherSessionsFor(ctx context.Context, userID int64, keep string) {
	s.db.ExecContext(ctx, `delete from admin_sessions where id=? and token<>?`, userID, hashSessionToken(keep))
}

// DeleteSessionsForOrg ends an account's sessions that are acting in one organisation. Removing
// a member closes the consoles they had open there; their sessions in other organisations are
// not this organisation's to end.
func (s *Store) DeleteSessionsForOrg(ctx context.Context, userID, orgID int64) {
	s.db.ExecContext(ctx, `delete from admin_sessions where id=? and org_id=?`, userID, orgID)
}

func (s *Store) DeleteAdminSession(ctx context.Context, tok string) {
	s.db.ExecContext(ctx, `delete from admin_sessions where token=?`, hashSessionToken(tok))
}

// ---- documents metadata ----

type Document struct {
	ID         int64  `json:"id"`
	Path       string `json:"path"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	UploadedBy string `json:"uploaded_by"`
	Scope      string `json:"scope"`
	Status     string `json:"status"`
	Chunks     int    `json:"chunks"`
	LastError  string `json:"last_error"`
	UpdatedAt  string `json:"updated_at"`
	// DriveSync is the sync that owns this document, when one does. Not a column: it is joined
	// on for the listing, so the table can say which rows a person must not edit by hand —
	// anything typed into one is overwritten the next time the folder is synced.
	DriveSync int64 `json:"drive_sync_id,omitempty"`
}

func (s *Store) UpsertDocument(ctx context.Context, orgID int64, d Document) error {
	if s.countRows(ctx, "documents", orgID) >= maxDocumentsPerOrg {
		var existing int
		s.db.QueryRowContext(ctx, `select count(*) from documents where org_id=? and path=?`, orgID, d.Path).Scan(&existing)
		if existing == 0 {
			return errOrgCap
		}
	}
	_, err := s.db.ExecContext(ctx, `insert into documents (org_id, path, name, size, uploaded_by, scope, status, chunks, last_error, updated_at)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		on conflict(org_id, path) do update set name=excluded.name, size=excluded.size, status=excluded.status, chunks=excluded.chunks,
		  last_error=excluded.last_error, updated_at=excluded.updated_at,
		  uploaded_by=coalesce(nullif(excluded.uploaded_by,''), documents.uploaded_by),
		  scope=case when excluded.scope<>'' then excluded.scope else documents.scope end`,
		orgID, d.Path, d.Name, d.Size, d.UploadedBy, d.Scope, d.Status, d.Chunks, d.LastError, now())
	return err
}

// SetDocumentScope restricts a document to one channel. The path is normalised the way the
// store wrote it, and a path that matches nothing is an error rather than a silent success:
// an admin who restricted a document must not be told it is restricted when it is not.
func (s *Store) SetDocumentScope(ctx context.Context, orgID int64, p, scope string) error {
	if clean, err := cleanRel(p); err == nil {
		p = clean
	}
	res, err := s.db.ExecContext(ctx, `update documents set scope=?, updated_at=? where org_id=? and path=?`, scope, now(), orgID, p)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetDocumentScopesUnder puts every document inside a folder on one scope, in one statement
// so half a folder cannot end up on each side. The prefix is matched with substr rather than
// like: a folder called "q1_notes" would otherwise also match "q1-notes", and over-matching
// here widens who can read a document. Returns how many rows it touched.
func (s *Store) SetDocumentScopesUnder(ctx context.Context, orgID int64, folder, scope string) (int64, error) {
	clean, err := cleanFolder(folder)
	if err != nil {
		return 0, err
	}
	if clean == "" {
		return 0, fmt.Errorf("a folder is required")
	}
	prefix := clean + "/"
	res, err := s.db.ExecContext(ctx, `update documents set scope=?, updated_at=? where org_id=? and substr(path, 1, ?)=?`,
		scope, now(), orgID, utf8.RuneCountInString(prefix), prefix)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (s *Store) DeleteDocument(ctx context.Context, orgID int64, path string) error {
	_, err := s.db.ExecContext(ctx, `delete from documents where org_id=? and path=?`, orgID, path)
	return err
}

func (s *Store) Documents(ctx context.Context, orgID int64) ([]Document, error) {
	rows, err := s.db.QueryContext(ctx, `select id, path, name, size, coalesce(uploaded_by,''), coalesce(scope,''), status, chunks, coalesce(last_error,''), updated_at
		from documents where org_id=? order by path`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Document{}
	for rows.Next() {
		var d Document
		if err := rows.Scan(&d.ID, &d.Path, &d.Name, &d.Size, &d.UploadedBy, &d.Scope, &d.Status, &d.Chunks, &d.LastError, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) DocumentScopes(ctx context.Context, orgID int64) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `select path, coalesce(scope,'') from documents where org_id=?`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var p, sc string
		rows.Scan(&p, &sc)
		m[p] = sc
	}
	return m, nil
}

// MoveDocument points a document row at its new path. The doc_id the index keys on is the
// path, so the chunks under the old one are stale: the row goes back to pending and the
// re-index that follows a move re-embeds it and prunes the rest.
func (s *Store) MoveDocument(ctx context.Context, orgID int64, from, to string) error {
	_, err := s.db.ExecContext(ctx, `update documents set path=?, name=?, status='pending', chunks=0, last_error='', updated_at=?
		where org_id=? and path=?`, to, path.Base(to), now(), orgID, from)
	return err
}

// ---- document folders ----

func (s *Store) DocumentFolders(ctx context.Context, orgID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `select path from document_folders where org_id=? order by path`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) AddDocumentFolder(ctx context.Context, orgID int64, folder string) error {
	_, err := s.db.ExecContext(ctx, `insert into document_folders (org_id, path) values (?, ?) on conflict(org_id, path) do nothing`, orgID, folder)
	return err
}

// DeleteDocumentFolders removes a folder and every folder under it.
func (s *Store) DeleteDocumentFolders(ctx context.Context, orgID int64, folder string) error {
	// The "under it" clause is a LIKE, so % or _ in a folder name would be wildcards and widen the
	// delete beyond the subtree named. org_id keeps it inside one tenant either way, but a folder
	// called "a_b" should not also match "axb". Escape the literal part and name the escape char.
	_, err := s.db.ExecContext(ctx, `delete from document_folders where org_id=? and (path=? or path like ? escape '\')`, orgID, folder, escapeLike(folder)+"/%")
	return err
}

// MoveDocumentFolders renames a folder and every folder under it.
//
// Two statements rather than SQLite's `update or replace`, which no other dialect has. The
// clause was doing real work: document_folders_path is unique on (org_id, path), so moving a
// folder onto one that already exists is a conflict, and `or replace` resolved it by deleting
// the row that was in the way. That is what the delete below does — explicitly, and only to
// rows that are not themselves being moved, so a move whose destination lies inside its own
// source cannot delete the thing it is moving.
func (s *Store) MoveDocumentFolders(ctx context.Context, orgID int64, from, to string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// under is a LIKE pattern, so the literal part of `from` is escaped (% and _ in a folder name
	// are not wildcards) and every LIKE names its escape character.
	cut, under := len(from)+1, escapeLike(from)+"/%"
	if _, err := tx.ExecContext(ctx, `delete from document_folders
		where org_id=? and not (path=? or path like ? escape '\')
		  and path in (select ? || substr(path, ?) from document_folders
		               where org_id=? and (path=? or path like ? escape '\'))`,
		orgID, from, under, to, cut, orgID, from, under); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `update document_folders set path = ? || substr(path, ?) where org_id=? and (path=? or path like ? escape '\')`,
		to, cut, orgID, from, under); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- pending writes (human confirmation) ----

func (s *Store) AddPendingWrite(ctx context.Context, orgID int64, teamID, channel, threadTS, requester, request string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `insert into pending_writes (org_id, team_id, channel, thread_ts, requester, request) values (?, ?, ?, ?, ?, ?) returning id`, orgID, teamID, channel, threadTS, requester, request).Scan(&id)
	return id, err
}

// pendingWriteWindow is how long a confirmation card stays answerable. Past it the card is a
// button on an old message, and the write it describes may no longer be the write the person
// reading it expects — so the row is still there and the press does nothing.
const pendingWriteWindow = 5 * time.Minute

func (s *Store) TakePendingWrite(ctx context.Context, orgID int64, teamID, channel, threadTS string) (int64, string, error) {
	var id int64
	var req string
	err := s.db.QueryRowContext(ctx, `select id, request from pending_writes where team_id=? and channel=? and thread_ts=? and status='pending'
		and created_at > ? order by id desc limit 1`, teamID, channel, threadTS, nowMinus(pendingWriteWindow)).Scan(&id, &req)
	if err == sql.ErrNoRows {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", err
	}
	_, err = s.db.ExecContext(ctx, `update pending_writes set status='approved' where org_id=? and id=?`, orgID, id)
	return id, req, err
}

// TakePendingWriteByID approves one specific held write — what a Confirm button carries. A
// request that expired, was cancelled or was already run comes back empty. The workspace and
// channel the press arrived from have to be the ones the write was held in: what runs is
// resolved against the pressing channel's connections, so a press from anywhere else would
// spend a different credential from the one the card described.
func (s *Store) TakePendingWriteByID(ctx context.Context, orgID int64, teamID, channel string, id int64) (string, error) {
	var req string
	err := s.db.QueryRowContext(ctx, `select request from pending_writes where org_id=? and team_id=? and channel=? and id=? and status='pending'
		and created_at > ?`, orgID, teamID, channel, id, nowMinus(pendingWriteWindow)).Scan(&req)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	res, err := s.db.ExecContext(ctx, `update pending_writes set status='approved' where org_id=? and team_id=? and channel=? and id=? and status='pending'`,
		orgID, teamID, channel, id)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", nil // someone else answered it first
	}
	return req, nil
}

// PendingWriteRequester says who asked for a held write, so a press can be limited to them and
// to the people who may approve on their behalf. Empty when there is no such live request here.
func (s *Store) PendingWriteRequester(ctx context.Context, orgID int64, teamID, channel string, id int64) (string, error) {
	var who string
	err := s.db.QueryRowContext(ctx, `select coalesce(requester,'') from pending_writes where org_id=? and team_id=? and channel=? and id=? and status='pending'
		and created_at > ?`, orgID, teamID, channel, id, nowMinus(pendingWriteWindow)).Scan(&who)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return who, err
}

// LatestPendingWrite is the newest live held write in a thread, for somebody typing "confirm"
// rather than pressing the button: its id and who asked for it.
func (s *Store) LatestPendingWrite(ctx context.Context, orgID int64, teamID, channel, threadTS string) (int64, string, error) {
	var id int64
	var who string
	err := s.db.QueryRowContext(ctx, `select id, coalesce(requester,'') from pending_writes where org_id=? and team_id=? and channel=? and thread_ts=? and status='pending'
		and created_at > ? order by id desc limit 1`, orgID, teamID, channel, threadTS, nowMinus(pendingWriteWindow)).Scan(&id, &who)
	if err == sql.ErrNoRows {
		return 0, "", nil
	}
	return id, who, err
}

// HeldWrite is one live held write in a thread: its row, and who asked for it.
type HeldWrite struct {
	ID        int64
	Requester string
}

// PendingWritesInThread lists a thread's live held writes, oldest first, each with who asked for
// it — so a cancel can drop the ones that are its author's to drop and leave the rest standing.
func (s *Store) PendingWritesInThread(ctx context.Context, orgID int64, teamID, channel, threadTS string) ([]HeldWrite, error) {
	rows, err := s.db.QueryContext(ctx, `select id, coalesce(requester,'') from pending_writes where org_id=? and team_id=? and channel=? and thread_ts=? and status='pending'
		and created_at > ? order by id`, orgID, teamID, channel, threadTS, nowMinus(pendingWriteWindow))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HeldWrite
	for rows.Next() {
		var h HeldWrite
		if err := rows.Scan(&h.ID, &h.Requester); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// PendingWriteRequest reads a held write without claiming it, so it can be moved to a table where
// a different person answers for it.
func (s *Store) PendingWriteRequest(ctx context.Context, orgID, id int64) (string, error) {
	var req string
	err := s.db.QueryRowContext(ctx, `select request from pending_writes where org_id=? and id=? and status='pending'`, orgID, id).Scan(&req)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return req, err
}

func (s *Store) DiscardPendingWrite(ctx context.Context, orgID int64, teamID, channel string, id int64) {
	s.db.ExecContext(ctx, `update pending_writes set status='discarded' where org_id=? and team_id=? and channel=? and id=? and status='pending'`,
		orgID, teamID, channel, id)
}

func (s *Store) DiscardPendingWrites(ctx context.Context, teamID, channel, threadTS string) {
	s.db.ExecContext(ctx, `update pending_writes set status='discarded' where team_id=? and channel=? and thread_ts=? and status='pending'`, teamID, channel, threadTS)
}

// ---- overview stats ----

type Overview struct {
	Bot          string        `json:"bot"`
	Team         string        `json:"team"`
	MonthSpend   float64       `json:"month_spend_usd"`
	Budget       float64       `json:"budget_usd"` // the effective budget (Settings.EffectiveBudget), not the setting
	Plan         string        `json:"plan"`       // free | pro (plans.go)
	TurnsToday   int           `json:"turns_today"`
	Turns7d      int           `json:"turns_7d"`
	Docs, Chunks int           `json:"-"`
	DocsJ        int           `json:"docs"`
	ChunksJ      int           `json:"chunks"`
	Bundles      int           `json:"bundles"`
	Connections  int           `json:"connections"`
	Scopes       int           `json:"scopes"` // channels the bot is in, across every workspace
	Teams        int           `json:"teams"`  // connected Slack workspaces
	Routines     int           `json:"routines"`
	Memories     int           `json:"memories"`
	RecentErrors []RecentError `json:"recent_errors"`
	TopChannels  []UsageRow    `json:"top_channels"`
	// The chart series, which only the console is given: OverviewStats leaves this nil and the
	// console's own handler fills it, so an API key polling /v1/usage does not pay for three
	// grouped scans nothing reads. See OverviewChartData.
	Charts *OverviewCharts `json:"charts,omitempty"`
}

// RecentError is one failed tool call, cut down to what the overview card shows. It carries
// the call's id so the card can open the very same row on Activity rather than leaving the
// reader to hunt for it.
type RecentError struct {
	ID   int64  `json:"id"`
	At   string `json:"at"`
	Name string `json:"name"`
	Args string `json:"args"`
}

// ChannelScopes is the channels the bot is in, named, and nothing else about them. The
// onboarding walk runs on every console page load, and Scopes() resolves each row's bundles and
// connections one query at a time — a tree the walk has no use for.
func (s *Store) ChannelScopes(ctx context.Context, orgID int64, limit int) []*Scope {
	rows, err := s.db.QueryContext(ctx,
		`select team_id, slack_id, coalesce(name,'') from scopes where org_id=? and kind='channel' and left_at='' order by name limit ?`,
		orgID, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*Scope
	for rows.Next() {
		sc := &Scope{Kind: "channel"}
		if err := rows.Scan(&sc.TeamID, &sc.SlackID, &sc.Name); err != nil {
			return out
		}
		out = append(out, sc)
	}
	return out
}

// ChannelNames is what each channel scope is called, keyed "team|channel". It includes channels
// the bot has since left: their usage outlives the membership, and the name a channel had is a
// better label than its id.
func (s *Store) ChannelNames(ctx context.Context, orgID int64) map[string]string {
	names := map[string]string{}
	rows, err := s.db.QueryContext(ctx,
		`select team_id, slack_id, coalesce(name,'') from scopes where org_id=? and kind='channel'`, orgID)
	if err != nil {
		return names
	}
	defer rows.Close()
	for rows.Next() {
		var team, id, name string
		if rows.Scan(&team, &id, &name) == nil && name != "" {
			names[team+"|"+id] = name
		}
	}
	return names
}

// TurnCount is every turn this organisation has ever been billed for. The onboarding walk asks
// it to find out whether anybody has spoken to the bot yet, so it counts from the beginning
// rather than over a window: a workspace set up last month is finished, not back at step three.
func (s *Store) TurnCount(ctx context.Context, orgID int64) int {
	var n int
	s.db.QueryRowContext(ctx, `select count(*) from usage where org_id=?`, orgID).Scan(&n)
	return n
}

func (s *Store) OverviewStats(ctx context.Context, orgID int64) Overview {
	var o Overview
	s.db.QueryRowContext(ctx, `select count(*) from usage where org_id=? and created_at >= ?`, orgID, today()).Scan(&o.TurnsToday)
	s.db.QueryRowContext(ctx, `select count(*) from usage where org_id=? and created_at >= ?`, orgID, nowMinus(7*24*time.Hour)).Scan(&o.Turns7d)
	o.Docs, o.Chunks, _ = s.DocStats(ctx, orgID)
	o.DocsJ, o.ChunksJ = o.Docs, o.Chunks
	s.db.QueryRowContext(ctx, `select count(*) from bundles where org_id=?`, orgID).Scan(&o.Bundles)
	s.db.QueryRowContext(ctx, `select count(*) from connections where org_id=?`, orgID).Scan(&o.Connections)
	s.db.QueryRowContext(ctx, `select count(*) from scopes where org_id=? and kind='channel'`, orgID).Scan(&o.Scopes)
	s.db.QueryRowContext(ctx, `select count(*) from teams where org_id=? and status='active'`, orgID).Scan(&o.Teams)
	s.db.QueryRowContext(ctx, `select count(*) from routines where org_id=? and enabled=1`, orgID).Scan(&o.Routines)
	s.db.QueryRowContext(ctx, `select count(*) from memories where org_id=?`, orgID).Scan(&o.Memories)
	o.MonthSpend, _ = s.MonthSpend(ctx, orgID, "", "")
	o.TopChannels, _ = s.UsageByChannel(ctx, orgID)
	if o.TopChannels == nil {
		o.TopChannels = []UsageRow{}
	}
	o.RecentErrors = []RecentError{}
	rows, err := s.db.QueryContext(ctx, `select id, name, coalesce(args,''), created_at from tool_calls where org_id=? and ok=0 order by id desc limit 5`, orgID)
	if err == nil {
		for rows.Next() {
			var e RecentError
			rows.Scan(&e.ID, &e.Name, &e.Args, &e.At)
			e.Args = truncate(e.Args, 80)
			o.RecentErrors = append(o.RecentErrors, e)
		}
		rows.Close()
	}
	return o
}

// jsonOrEmpty stores a recipe as its JSON, and a nil one as the empty string rather than the
// four bytes "null", so "nobody has said" and "somebody said nothing" read the same in SQL.
func jsonOrEmpty(r *Recipe) string {
	if r == nil {
		return ""
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	return string(raw)
}

// SetConnectionRecipe stores what a job worked out about a repository. It touches nothing else
// on the row, so it is safe to call while somebody is editing the connection in the console.
func (s *Store) SetConnectionRecipe(ctx context.Context, orgID, id int64, r *Recipe) error {
	_, err := s.db.ExecContext(ctx, `update connections set recipe=?, updated_at=? where id=? and org_id=?`,
		jsonOrEmpty(r), now(), id, orgID)
	return err
}
