package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

// Identity: organisations, the people in them, and how those people prove who they are.
//
// The rule this file exists to enforce is that an email address is a delivery address and a
// label, never a join key and never authority. An account is not adopted because a sign-in
// asserted a matching address — Slack's OIDC email is set by the workspace's own admin (and by
// its SAML IdP), so treating it as proof of control would let any workspace admin mint an
// assertion for any address. A second sign-in method is linked from inside an already
// authenticated session, never at the door. See LinkIdentity.

// ---- organisations ----

type Org struct {
	// ID is the join key and stays inside the process: it is a serial, and a serial handed out
	// tells its holder how many accounts came before it and what its neighbours are called.
	ID int64 `json:"-"`
	// PublicID is the organisation's name everywhere outside — responses, the operator's plan
	// link, deploy/plan.sh. It is what "the org id" means to anyone who is not this package.
	PublicID string `json:"id"`
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	Status   string `json:"status"`
	Plan     string `json:"plan"` // free | pro | enterprise (plans.go)
	// The monthly budget the operator granted with a pro or enterprise plan; 0 = the deployment's
	// ceiling on pro, and no ceiling on enterprise.
	PlanBudgetUSD float64 `json:"plan_budget_usd"`
	CreatedBy     int64   `json:"created_by"`
	CreatedAt     string  `json:"created_at"`
}

const orgCols = `id, coalesce(public_id,''), name, slug, coalesce(status,'active'), coalesce(plan,'free'), coalesce(plan_budget_usd,0), coalesce(created_by,0), created_at`

func scanOrg(row interface{ Scan(...any) error }) (*Org, error) {
	var o Org
	if err := row.Scan(&o.ID, &o.PublicID, &o.Name, &o.Slug, &o.Status, &o.Plan, &o.PlanBudgetUSD, &o.CreatedBy, &o.CreatedAt); err != nil {
		return nil, err
	}
	return &o, nil
}

// newPublicID is the identifier an organisation or a person is known by outside this process:
// 128 random bits in hex, the shape of a uuid with the hyphens left off. Random rather than
// sequential is the whole point — it carries no count, no order and no neighbours.
func newPublicID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on any platform this runs on, and a guessable id would be
		// worse than no row at all, so this is not papered over with a timestamp.
		panic("public id: " + err.Error())
	}
	return hex.EncodeToString(b)
}

var slugStrip = regexp.MustCompile(`[^a-z0-9]+`)

// slugify turns an organisation name into something that can sit in a URL. It is a convenience,
// not an identifier: the id is the key, and a collision just gets a numeric suffix.
func slugify(name string) string {
	s := slugStrip.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
	s = strings.Trim(s, "-")
	if len(s) > 48 {
		s = strings.Trim(s[:48], "-")
	}
	if s == "" {
		s = "org"
	}
	return s
}

// CreateOrg makes an organisation and its founder's membership in one go: an org with nobody in
// it is unreachable, and a half-finished signup that left one behind would be a permanent orphan.
func (s *Store) CreateOrg(ctx context.Context, name string, founder int64) (*Org, error) {
	name, err := cleanName(name, 80)
	if err != nil {
		return nil, errors.New("an organisation needs a name of up to 80 ordinary characters")
	}
	base := slugify(name)
	slug := base
	for i := 2; ; i++ {
		var n int
		s.db.QueryRowContext(ctx, `select count(*) from orgs where slug=?`, slug).Scan(&n)
		if n == 0 {
			break
		}
		slug = fmt.Sprintf("%s-%d", base, i)
		if i > 500 {
			return nil, errors.New("could not find a free name for that organisation")
		}
	}
	public := newPublicID()
	var id int64
	err = s.db.QueryRowContext(ctx, `insert into orgs (public_id, name, slug, created_by) values (?, ?, ?, ?) returning id`,
		public, name, slug, founder).Scan(&id)
	if err != nil {
		return nil, err
	}
	if founder != 0 {
		if err := s.AddMembership(ctx, founder, id, RoleAdmin, 0); err != nil {
			return nil, err
		}
	}
	return &Org{ID: id, PublicID: public, Name: name, Slug: slug, Status: "active", Plan: PlanFree, CreatedBy: founder}, nil
}

func (s *Store) Org(ctx context.Context, id int64) (*Org, error) {
	o, err := scanOrg(s.db.QueryRowContext(ctx, `select `+orgCols+` from orgs where id=?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return o, err
}

// OrgByPublicID is the lookup every route that takes an organisation from outside uses: the
// operator's plan routes, and anything else handed an id by a caller. An id that does not exist
// reads as nil, never as "the first one".
func (s *Store) OrgByPublicID(ctx context.Context, public string) (*Org, error) {
	public = strings.ToLower(strings.TrimSpace(public))
	if public == "" {
		return nil, nil
	}
	o, err := scanOrg(s.db.QueryRowContext(ctx, `select `+orgCols+` from orgs where public_id=?`, public))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return o, err
}

func (s *Store) OrgBySlug(ctx context.Context, slug string) (*Org, error) {
	o, err := scanOrg(s.db.QueryRowContext(ctx, `select `+orgCols+` from orgs where slug=?`, slug))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return o, err
}

// OrgCount is what the first-run signup gate reads: while no organisation exists, the first
// signup is a bootstrap and is allowed. A read error returns a non-zero count on purpose — the
// gate opens only on a confirmed-empty database, so a database it cannot query reads as "not
// empty" and the door stays shut rather than swinging open for a stranger during a blip.
func (s *Store) OrgCount(ctx context.Context) int {
	var n int
	if err := s.db.QueryRowContext(ctx, `select count(*) from orgs`).Scan(&n); err != nil {
		slog.Warn("could not count organisations; treating the deployment as not empty", "err", err)
		return 1
	}
	return n
}

func (s *Store) RenameOrg(ctx context.Context, id int64, name string) error {
	name, err := cleanName(name, 80)
	if err != nil {
		return errors.New("an organisation needs a name of up to 80 ordinary characters")
	}
	_, err = s.db.ExecContext(ctx, `update orgs set name=? where id=?`, name, id)
	return err
}

// OrgPlan is what the settings loader reads on every refresh — the plan and the budget granted
// with it — so it is its own query rather than a whole Org. An organisation that does not exist
// reads as free, which is what planOf makes of the empty string.
func (s *Store) OrgPlan(ctx context.Context, id int64) (plan string, grant float64) {
	plan, grant, _ = s.orgPlanRead(ctx, id)
	return plan, grant
}

// orgPlanRead is OrgPlan with the error, for the one reader that has to tell "free" from "could not
// read": whether an organisation's own model key may be used depends on the plan (model_keys.go).
// No such organisation is not an error.
func (s *Store) orgPlanRead(ctx context.Context, id int64) (plan string, grant float64, err error) {
	err = s.db.QueryRowContext(ctx, `select coalesce(plan,'free'), coalesce(plan_budget_usd,0) from orgs where id=?`, id).Scan(&plan, &grant)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return plan, grant, err
}

// SetOrgPlan moves an account between plans, with the monthly budget that goes with a pro or an
// enterprise plan (0 for free; on pro 0 means the deployment's ceiling, on enterprise no ceiling).
// Only the operator API calls it: a member of the organisation, whatever their role, has no route
// to it.
func (s *Store) SetOrgPlan(ctx context.Context, id int64, plan string, grant float64) error {
	if planOf(plan) != plan {
		return fmt.Errorf("plan must be one of %s", strings.Join(plans, ", "))
	}
	if plan == PlanFree {
		grant = 0
	}
	res, err := s.db.ExecContext(ctx, `update orgs set plan=?, plan_budget_usd=? where id=?`, plan, grant, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("no such organisation")
	}
	return nil
}

// Orgs lists every organisation on the deployment, oldest first. The operator's listing; no
// tenant-facing route returns it.
func (s *Store) Orgs(ctx context.Context) ([]Org, error) {
	rows, err := s.db.QueryContext(ctx, `select `+orgCols+` from orgs order by id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Org{}
	for rows.Next() {
		o, err := scanOrg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

// ---- users ----

type User struct {
	// ID is the join key, and like an organisation's it stays inside the process.
	ID int64 `json:"-"`
	// PublicID is what a person is called outside: in the console's own responses, in the users
	// list, and in the path of the routes that change somebody's role or remove them.
	PublicID      string `json:"id"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	CreatedAt     string `json:"created_at"`
	LastSeen      string `json:"last_seen"`

	passwordHash string
	// How the address came to be confirmed (one of the emailBy constants, or "" for one confirmed
	// before that was recorded) and, for an invitation, whose. Only a domain-limited link asks;
	// everything else reads EmailVerified alone (confirmedForDomainLink).
	emailVerifiedBy  string
	emailVerifiedOrg int64
}

// HasPassword says whether this account can be signed into with a password at all, which is what
// the console shows instead of ever revealing anything about the hash.
func (u *User) HasPassword() bool { return u != nil && u.passwordHash != "" }

const userCols = `id, coalesce(public_id,''), email, email_verified, coalesce(name,''), coalesce(status,'active'),
	created_at, coalesce(last_seen,''), coalesce(password_hash,''), coalesce(email_verified_by,''), coalesce(email_verified_org,0)`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	if err := row.Scan(&u.ID, &u.PublicID, &u.Email, &u.EmailVerified, &u.Name, &u.Status,
		&u.CreatedAt, &u.LastSeen, &u.passwordHash, &u.emailVerifiedBy, &u.emailVerifiedOrg); err != nil {
		return nil, err
	}
	return &u, nil
}

// normalEmail is how an address is stored and compared. Lowercased and trimmed, and nothing
// cleverer: stripping dots or +tags is a provider-specific guess that would merge two addresses
// their owner considers different.
func normalEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

var errEmailTaken = errors.New("that email address already has an account — sign in instead")

func (s *Store) CreateUser(ctx context.Context, email, name, passwordHash string) (*User, error) {
	email = normalEmail(email)
	if email == "" {
		return nil, errors.New("an account needs an email address")
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `insert into users (public_id, email, name, password_hash) values (?, ?, ?, ?) returning id`,
		newPublicID(), email, strings.TrimSpace(name), passwordHash).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, errEmailTaken
		}
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	return s.User(ctx, id)
}

func (s *Store) User(ctx context.Context, id int64) (*User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, `select `+userCols+` from users where id=?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return u, err
}

// UserByPublicID resolves the id a console route was handed. Callers must still check that the
// person belongs to the organisation making the request: knowing an id is not membership.
func (s *Store) UserByPublicID(ctx context.Context, public string) (*User, error) {
	public = strings.ToLower(strings.TrimSpace(public))
	if public == "" {
		return nil, nil
	}
	u, err := scanUser(s.db.QueryRowContext(ctx, `select `+userCols+` from users where public_id=?`, public))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return u, err
}

func (s *Store) UserByEmail(ctx context.Context, email string) (*User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, `select `+userCols+` from users where email=?`, normalEmail(email)))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return u, err
}

func (s *Store) SetPassword(ctx context.Context, userID int64, hash string) error {
	_, err := s.db.ExecContext(ctx, `update users set password_hash=? where id=?`, hash, userID)
	return err
}

// What confirmed an address. Recorded because the answers are not equally hard to arrange, and
// one question — may this account join through a domain-limited link — has to tell them apart.
const (
	emailByMail   = "mail"   // a link we mailed to it: verification, or a password reset
	emailBySSO    = "sso"    // an identity provider, for an address at the domain it proved by DNS
	emailByInvite = "invite" // an invitation addressed to it; its link also went to whoever sent it
	emailBySlack  = "slack"  // Slack's email_verified claim, which the workspace's own admin controls
)

// SetEmailVerified marks an address confirmed and records what confirmed it; org is the
// organisation whose invitation did, for emailByInvite. A confirmation by our own mail is never
// replaced by a weaker one: every other caller only confirms an address that is not yet
// confirmed, and this keeps that true if a new one forgets to ask.
func (s *Store) SetEmailVerified(ctx context.Context, userID int64, by string, org int64) error {
	_, err := s.db.ExecContext(ctx, `update users set email_verified=1,
		email_verified_org = case when email_verified=1 and email_verified_by=? then email_verified_org else ? end,
		email_verified_by = case when email_verified=1 and email_verified_by=? then email_verified_by else ? end
		where id=?`, emailByMail, org, emailByMail, by, userID)
	return err
}

func (s *Store) SetUserName(ctx context.Context, userID int64, name string) error {
	_, err := s.db.ExecContext(ctx, `update users set name=? where id=?`, strings.TrimSpace(name), userID)
	return err
}

func (s *Store) TouchUser(ctx context.Context, userID int64) {
	s.db.ExecContext(ctx, `update users set last_seen=? where id=?`, now(), userID)
}

// ---- how a user signs in ----

const (
	ProviderPassword = "password"
	ProviderSlack    = "slack"
	// An account in an organisation's own identity provider. The subject is scoped by the
	// provider it came from (ssoSubject), because `sub` is only unique within its issuer.
	ProviderSSO = "sso"
)

// slackSubject is the identity a Slack sign-in proves: a user id inside one workspace. Slack
// user ids are unique to their workspace, so the workspace has to be part of the key.
func slackSubject(teamID, slackUserID string) string { return teamID + ":" + slackUserID }

// chatAccount is the chat account a sign-in identity proves, keyed the way the bot keys people: a
// Slack user in a workspace, or a Teams member — an Entra object id — in the workspace that is
// their tenant. A password or an organisation's own identity provider proves no chat account.
func chatAccount(i Identity) (teamID, userID string, ok bool) {
	switch i.Provider {
	case ProviderSlack:
		team, user, _ := strings.Cut(i.Subject, ":")
		return team, user, team != "" && user != ""
	case ProviderMicrosoft:
		tenant, oid, _ := strings.Cut(i.Subject, ":")
		return msteamsTeamPrefix + tenant, oid, tenant != "" && oid != ""
	}
	return "", "", false
}

// AddIdentity records a way of signing in. It fails rather than moving an identity that already
// belongs to somebody else — quietly reassigning one is exactly the takeover this design refuses.
func (s *Store) AddIdentity(ctx context.Context, userID int64, provider, subject string) error {
	_, err := s.db.ExecContext(ctx, `insert into user_identities (user_id, provider, subject) values (?, ?, ?)`,
		userID, provider, subject)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return errors.New("that sign-in is already connected to another account")
	}
	return err
}

// UserByIdentity is the only lookup a sign-in may use. Note what it is not: a lookup by email.
func (s *Store) UserByIdentity(ctx context.Context, provider, subject string) (*User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, `select `+userCols+` from users
		where id = (select user_id from user_identities where provider=? and subject=?)`, provider, subject))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return u, err
}

type Identity struct {
	Provider  string `json:"provider"`
	Subject   string `json:"subject"`
	CreatedAt string `json:"created_at"`
}

func (s *Store) Identities(ctx context.Context, userID int64) ([]Identity, error) {
	rows, err := s.db.QueryContext(ctx, `select provider, subject, created_at from user_identities where user_id=? order by id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Identity{}
	for rows.Next() {
		var i Identity
		if err := rows.Scan(&i.Provider, &i.Subject, &i.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (s *Store) RemoveIdentity(ctx context.Context, userID int64, provider, subject string) error {
	_, err := s.db.ExecContext(ctx, `delete from user_identities where user_id=? and provider=? and subject=?`,
		userID, provider, subject)
	return err
}

// ---- memberships: the only source of authority ----

type Membership struct {
	// The join keys, kept inside the process. UserPublic and OrgPublic are what a response
	// carries under the names "user_id" and "org_id": the console's organisation switcher and
	// its users list both send one of these back, and neither should be a counter.
	UserID     int64  `json:"-"`
	OrgID      int64  `json:"-"`
	UserPublic string `json:"user_id"`
	OrgPublic  string `json:"org_id"`
	OrgName    string `json:"org_name"`
	OrgSlug    string `json:"org_slug"`
	Role       string `json:"role"`
	CreatedAt  string `json:"created_at"`

	// Filled when listing an org's members.
	Email    string `json:"email,omitempty"`
	Name     string `json:"name,omitempty"`
	LastSeen string `json:"last_seen,omitempty"`
	Verified bool   `json:"email_verified,omitempty"`
}

func (s *Store) AddMembership(ctx context.Context, userID, orgID int64, role string, by int64) error {
	if by > 0 {
		if err := s.inviterAuthorized(ctx, orgID, by, role); err != nil {
			return err
		}
	}
	_, err := s.db.ExecContext(ctx, `insert into memberships (user_id, org_id, role, created_by) values (?, ?, ?, ?)
		on conflict(user_id, org_id) do nothing`, userID, orgID, role, by)
	return err
}

// Membership is the authorisation check every request makes: does this person belong to this
// organisation, and as what. A nil answer means no, and callers must treat it as a refusal
// rather than a missing row to fill in.
func (s *Store) Membership(ctx context.Context, userID, orgID int64) (*Membership, error) {
	var m Membership
	err := s.db.QueryRowContext(ctx, `select m.user_id, m.org_id, m.role, m.created_at, o.name, o.slug,
		coalesce(u.public_id,''), coalesce(o.public_id,'')
		from memberships m join orgs o on o.id = m.org_id join users u on u.id = m.user_id
		where m.user_id=? and m.org_id=?`, userID, orgID).
		Scan(&m.UserID, &m.OrgID, &m.Role, &m.CreatedAt, &m.OrgName, &m.OrgSlug, &m.UserPublic, &m.OrgPublic)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &m, err
}

// MembershipsFor is what the console's organisation switcher lists.
func (s *Store) MembershipsFor(ctx context.Context, userID int64) ([]Membership, error) {
	rows, err := s.db.QueryContext(ctx, `select m.user_id, m.org_id, m.role, m.created_at, o.name, o.slug,
		coalesce(u.public_id,''), coalesce(o.public_id,'')
		from memberships m join orgs o on o.id = m.org_id join users u on u.id = m.user_id
		where m.user_id=? and coalesce(o.status,'active')='active' order by o.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Membership{}
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.UserID, &m.OrgID, &m.Role, &m.CreatedAt, &m.OrgName, &m.OrgSlug,
			&m.UserPublic, &m.OrgPublic); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MembersOf is the Users page: everyone in one organisation, with the account facts beside the role.
func (s *Store) MembersOf(ctx context.Context, orgID int64) ([]Membership, error) {
	rows, err := s.db.QueryContext(ctx, `select m.user_id, m.org_id, m.role, m.created_at,
		u.email, coalesce(u.name,''), coalesce(u.last_seen,''), u.email_verified, coalesce(u.public_id,'')
		from memberships m join users u on u.id = m.user_id
		where m.org_id=? order by u.email`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Membership{}
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.UserID, &m.OrgID, &m.Role, &m.CreatedAt,
			&m.Email, &m.Name, &m.LastSeen, &m.Verified, &m.UserPublic); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) SetMemberRole(ctx context.Context, userID, orgID int64, role string) error {
	return s.changeMember(ctx, userID, orgID, role, false)
}
func (s *Store) RemoveMember(ctx context.Context, userID, orgID int64) error {
	return s.changeMember(ctx, userID, orgID, "", true)
}
func (s *Store) changeMember(ctx context.Context, userID, orgID int64, role string, remove bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `update email_tokens set used_at=? where org_id=? and kind='invite' and used_at is null and (email=(select email from users where id=?) or created_by=?)`, now(), orgID, userID, userID); err != nil {
		return err
	}
	if remove {
		_, err = tx.ExecContext(ctx, `delete from memberships where user_id=? and org_id=?`, userID, orgID)
	} else {
		_, err = tx.ExecContext(ctx, `update memberships set role=? where user_id=? and org_id=?`, role, userID, orgID)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) inviterAuthorized(ctx context.Context, orgID, by int64, role string) error {
	m, err := s.Membership(ctx, by, orgID)
	if err != nil || m == nil {
		return errors.New("inviter no longer belongs to this organisation")
	}
	custom := s.CustomRoleMap(ctx, orgID)
	have, grant := permissionsForRole(m.Role, custom), permissionsForRole(role, custom)
	if !have[PermUsersManage] || len(grant) == 0 {
		return errors.New("invitation authority is no longer valid")
	}
	for p := range grant {
		if !have[p] {
			return errors.New("invitation exceeds inviter authority")
		}
	}
	return nil
}

// CountMembersHolding counts the people in an org whose role still grants a permission, ignoring
// one user — what the "you cannot remove the last admin" guard asks.
func (s *Store) CountMembersHolding(ctx context.Context, orgID int64, perm Permission, except int64) (int, error) {
	rows, err := s.db.QueryContext(ctx, `select user_id, role from memberships where org_id=? and user_id<>?`, orgID, except)
	if err != nil {
		return 0, err
	}
	roles := []string{}
	for rows.Next() {
		var uid int64
		var role string
		if err := rows.Scan(&uid, &role); err != nil {
			rows.Close()
			return 0, err
		}
		roles = append(roles, role)
	}
	err = rows.Err()
	// One SQLite connection: the cursor has to be closed before CustomRoleMap runs its own
	// query, or the two wait on each other and the request never returns.
	rows.Close()
	if err != nil {
		return 0, err
	}
	custom := s.CustomRoleMap(ctx, orgID)
	n := 0
	for _, role := range roles {
		if permissionsForRole(role, custom)[perm] {
			n++
		}
	}
	return n, nil
}

// ---- one-time email links ----

const (
	TokenVerify = "verify"
	TokenReset  = "reset"
	TokenInvite = "invite"
)

// EmailToken is what a link carries. Only the hash is stored, so a copy of the database cannot
// be replayed into somebody's account — the same reasoning as console invites already used.
type EmailToken struct {
	// ID is the row, not the secret: the console lists and revokes invitations by it, so that a
	// link addressed to nobody in particular is as revocable as one addressed to a mailbox.
	ID     int64
	Kind   string
	UserID int64
	OrgID  int64
	// Who the invitation names. An invitation names at most one of these, and a share link
	// names neither — that is the whole difference between the two.
	Email       string
	SlackUserID string
	Role        string
	// Share links only: what to call it in the list, and an email domain it will accept. The
	// domain is the one guard a link handed around in public still has, so it is checked
	// wherever a link is redeemed rather than only where it is made.
	Label  string
	Domain string
	// MaxUses is how many memberships this link may still produce: 1 for an invitation to one
	// person, UsesUnlimited for a share link with no cap. The zero value is normalised to 1 on
	// the way in, so a caller that never thinks about this field gets the safe answer rather
	// than an invitation anybody can redeem forever.
	MaxUses   int
	Uses      int
	CreatedBy int64
	CreatedAt string
	ExpiresAt string
	// InviteHash is on a verification link only: the domain-limited share link a password sign-up
	// is waiting to join through, by the hash its row is stored under. The join is made when the
	// mail is answered, so that is when the link is read and spent (handleVerify).
	InviteHash string
}

// Shareable reports whether this is a link meant to be passed around rather than an invitation
// addressed to one person. The two are the same row and the same redemption path; what separates
// them is that nobody's identity is written on this one.
func (t EmailToken) Shareable() bool {
	return t.Kind == TokenInvite && t.Email == "" && t.SlackUserID == ""
}

// Addressee is who the invitation was sent to, for logs and for the console list.
func (t EmailToken) Addressee() string {
	switch {
	case t.Email != "":
		return t.Email
	case t.SlackUserID != "":
		return t.SlackUserID
	}
	return "anyone with the link"
}

// AcceptsEmail reports whether an address may redeem this link. An invitation addressed to a
// mailbox accepts that mailbox and nothing else; a share link accepts any address unless its
// maker narrowed it to one domain. Both are asked here so that the password sign-up, the Slack
// sign-in and the accept-invitation button cannot drift into three different answers.
func (t EmailToken) AcceptsEmail(email string) bool {
	email = normalEmail(email)
	if t.Email != "" {
		return strings.EqualFold(normalEmail(t.Email), email)
	}
	if t.Domain == "" {
		return true
	}
	at := strings.LastIndex(email, "@")
	return at >= 0 && strings.EqualFold(email[at+1:], t.Domain)
}

// domainLink reports whether this is a share link narrowed to a domain but addressed to no
// mailbox. Holding such a link is not proof of an address in that domain — the redeemer types the
// address themselves — so a domain link may join an account only once that address has been proved
// by our own verification mail, unlike an addressed invitation (proof by delivery) or an open
// share link (anyone, by design).
func (t EmailToken) domainLink() bool {
	return t.Email == "" && t.SlackUserID == "" && t.Domain != ""
}

// emailTokenCols is the one column list every read of this table shares, so a field added to
// EmailToken cannot arrive filled in from one query and zeroed from another. The first column
// is the surrogate id, whose name is the one thing here that differs by dialect
// (emailTokenIDCol).
func (s *Store) emailTokenCols() string {
	return emailTokenIDCol(s.db.postgres()) + `, kind, user_id, org_id, coalesce(email,''), coalesce(slack_user_id,''),
	coalesce(role,''), coalesce(label,''), coalesce(domain,''), coalesce(max_uses,1), coalesce(uses,0),
	coalesce(created_by,0), coalesce(created_at,''), expires_at, coalesce(invite_hash,'')`
}

func scanEmailToken(row interface{ Scan(...any) error }) (*EmailToken, error) {
	var t EmailToken
	err := row.Scan(&t.ID, &t.Kind, &t.UserID, &t.OrgID, &t.Email, &t.SlackUserID, &t.Role,
		&t.Label, &t.Domain, &t.MaxUses, &t.Uses, &t.CreatedBy, &t.CreatedAt, &t.ExpiresAt, &t.InviteHash)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// UsesUnlimited is a link with no cap on how many people it lets in. Only its expiry, or
// somebody revoking it, ends it.
const UsesUnlimited = -1

// spent reports whether a token has been redeemed as many times as it is allowed to be.
func (t EmailToken) spent() bool { return t.MaxUses >= 0 && t.Uses >= t.MaxUses }

func hashEmailToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// NewEmailToken mints a link and returns the raw token, which exists only in what we send.
func (s *Store) NewEmailToken(ctx context.Context, t EmailToken, ttl time.Duration) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	// Inviting the same person twice replaces the first link rather than leaving two live: the
	// second invitation is what the sender meant, and two working links to one mailbox is one
	// more than anybody intended to hand out.
	//
	// Only an invitation that names somebody supersedes anything. A share link names nobody, and
	// superseding on an empty addressee would mean every new share link silently killed the last
	// one — including the invitations to Slack accounts, which carry no email at all.
	if t.Kind == TokenInvite {
		switch {
		case normalEmail(t.Email) != "":
			_, err = tx.ExecContext(ctx, `update email_tokens set used_at=? where org_id=? and kind='invite'
				and email=? and used_at is null`, now(), t.OrgID, normalEmail(t.Email))
		case t.SlackUserID != "":
			_, err = tx.ExecContext(ctx, `update email_tokens set used_at=? where org_id=? and kind='invite'
				and slack_user_id=? and used_at is null`, now(), t.OrgID, t.SlackUserID)
		}
		if err != nil {
			return "", err
		}
	}
	switch {
	case t.Kind != TokenInvite, t.MaxUses == 0:
		t.MaxUses = 1 // verification and reset links are one person's, once, always
	case t.MaxUses < 0:
		t.MaxUses = UsesUnlimited
	}
	raw := randomToken()
	_, err = tx.ExecContext(ctx, `insert into email_tokens
		(token_hash, kind, user_id, org_id, email, slack_user_id, role, label, domain, max_uses, created_by, expires_at, invite_hash)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		hashEmailToken(raw), t.Kind, t.UserID, t.OrgID, normalEmail(t.Email), t.SlackUserID, t.Role,
		t.Label, strings.ToLower(strings.TrimSpace(t.Domain)), t.MaxUses, t.CreatedBy,
		time.Now().UTC().Add(ttl).Format(time.DateTime), t.InviteHash)
	if err != nil {
		return "", err
	}
	return raw, tx.Commit()
}

var errBadEmailToken = errors.New("that link has expired or was already used — ask for a new one")

// TakeEmailToken consumes a link exactly once. The single conditional update is what makes a
// double click harmless: the second one matches no rows.
func (s *Store) TakeEmailToken(ctx context.Context, kind, raw string) (*EmailToken, error) {
	if raw == "" {
		return nil, errBadEmailToken
	}
	return s.TakeEmailTokenHash(ctx, kind, hashEmailToken(raw))
}

// TakeEmailTokenHash is TakeEmailToken for a link known by the hash its row is stored under rather
// than by the token itself: the share link a verification link names (InviteHash), spent when the
// mail is answered, long after anybody last had its token in hand. Every check the token path
// makes is made here, because it is the same path.
func (s *Store) TakeEmailTokenHash(ctx context.Context, kind, h string) (*EmailToken, error) {
	if h == "" {
		return nil, errBadEmailToken
	}
	t, err := scanEmailToken(s.db.QueryRowContext(ctx, `select `+s.emailTokenCols()+` from email_tokens
		where token_hash=? and kind=? and used_at is null and expires_at > ?`, h, kind, now()))
	if err != nil {
		return nil, errBadEmailToken
	}
	if t.Kind == TokenInvite {
		if err := s.inviterAuthorized(ctx, t.OrgID, t.CreatedBy, t.Role); err != nil {
			return nil, err
		}
	}
	// One statement does the counting and the closing, because that is what makes a double click
	// — or two people opening the same share link in the same instant — harmless. `uses < max_uses`
	// inside the WHERE is the guard: a second writer reads the row it has already changed, finds
	// the count no longer under the cap, and matches nothing. A link with no cap (max_uses 0)
	// never closes on a use; only its expiry or a revocation ends it.
	res, err := s.db.ExecContext(ctx, `update email_tokens
		set uses = uses + 1,
		    used_at = case when max_uses >= 0 and uses + 1 >= max_uses then ? else used_at end
		where token_hash=? and used_at is null and (max_uses < 0 or uses < max_uses)`, now(), h)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, errBadEmailToken // somebody else used it between the read and the write
	}
	t.Uses++
	return t, nil
}

// PeekEmailToken reads a link without consuming it, so an invite page can say who it is for
// before anyone fills the form in.
func (s *Store) PeekEmailToken(ctx context.Context, kind, raw string) (*EmailToken, error) {
	if raw == "" {
		return nil, errBadEmailToken
	}
	return s.PeekEmailTokenHash(ctx, kind, hashEmailToken(raw))
}

// PeekEmailTokenHash reads a link by the hash its row is stored under, without consuming it.
func (s *Store) PeekEmailTokenHash(ctx context.Context, kind, h string) (*EmailToken, error) {
	if h == "" {
		return nil, errBadEmailToken
	}
	t, err := scanEmailToken(s.db.QueryRowContext(ctx, `select `+s.emailTokenCols()+` from email_tokens
		where token_hash=? and kind=? and used_at is null and expires_at > ?`, h, kind, now()))
	if err != nil || t.spent() {
		return nil, errBadEmailToken
	}
	return t, nil
}

// InvalidateTokens burns every outstanding link of a kind for one user: a completed password
// reset must not leave a second reset email live in somebody's inbox.
func (s *Store) InvalidateTokens(ctx context.Context, userID int64, kind string) {
	s.db.ExecContext(ctx, `update email_tokens set used_at=? where user_id=? and kind=? and used_at is null`, now(), userID, kind)
}

// PendingInvitesFor lists an org's unaccepted invitations for the Users page.
func (s *Store) PendingInvitesFor(ctx context.Context, orgID int64) ([]EmailToken, error) {
	rows, err := s.db.QueryContext(ctx, `select `+s.emailTokenCols()+` from email_tokens
		where org_id=? and kind='invite' and used_at is null and expires_at > ? order by created_at desc`, orgID, now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EmailToken{}
	for rows.Next() {
		t, err := scanEmailToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// RevokeInviteByID withdraws one outstanding invitation or share link. Keyed on the row rather
// than the address, because a share link has no address and two invitations to the same person
// cannot both be live anyway. The organisation is in the WHERE so that one tenant cannot cancel
// another's by guessing a number.
func (s *Store) RevokeInviteByID(ctx context.Context, orgID, id int64) error {
	res, err := s.db.ExecContext(ctx, `update email_tokens set used_at=?
		where `+emailTokenIDCol(s.db.postgres())+`=? and org_id=? and kind='invite' and used_at is null`, now(), id, orgID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("that invitation is already gone")
	}
	return nil
}

func (s *Store) sweepEmailTokens(ctx context.Context) {
	s.db.ExecContext(ctx, `delete from email_tokens where expires_at < ?`, now())
}

// OrgIDs lists every organisation, for the sweeps that run once over the whole database.
func (s *Store) OrgIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `select id from orgs order by id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// CountMembersHoldingExcludingRole counts members who would still hold perm if one custom role
// were deleted. Members on that role are skipped rather than one named user, which is the shape
// deleting a role has: it demotes everybody holding it at once.
func (s *Store) CountMembersHoldingExcludingRole(ctx context.Context, orgID int64, perm Permission, roleKey string) (int, error) {
	rows, err := s.db.QueryContext(ctx, `select role from memberships where org_id=? and role<>?`, orgID, roleKey)
	if err != nil {
		return 0, err
	}
	roles := []string{}
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			rows.Close()
			return 0, err
		}
		roles = append(roles, role)
	}
	err = rows.Err()
	// Same reason as CountMembersHolding: one SQLite connection, so the cursor closes before
	// CustomRoleMap opens its own query.
	rows.Close()
	if err != nil {
		return 0, err
	}
	custom := s.CustomRoleMap(ctx, orgID)
	n := 0
	for _, role := range roles {
		if permissionsForRole(role, custom)[perm] {
			n++
		}
	}
	return n, nil
}
