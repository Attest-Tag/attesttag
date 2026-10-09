package app

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"attesttag/internal/review"
)

// Code review's console API: Reviews › History, Settings and Types, and Start review. The
// storage is store_review*.go, the decisions are internal/review's, and the review itself is the
// lane's (review_lane.go) — what is here is who may do what, and the shape the console reads.
//
// Two permissions gate the routes: reviews.view to read anything, reviews.manage to change it or
// to start a review. Inside a write, some fields reach further than review's noise and need
// connections.manage as well — the line between tuning a review and deciding what it may do,
// which the editor deliberately does not cross (console_roles.go):
//
//   - posting live, and reviewing on every push — what the bot writes on a pull request without
//     anybody pressing anything, and how often it is paid to;
//   - forks — a stranger's code, on the organisation's money;
//   - context repositories — which of the organisation's other repositories a review may read, and
//     so quote on this one;
//   - the model and max_usd — what one review spends;
//   - the channel reviews are announced in (notify) — it carries private repositories' names, titles
//     and findings into a chat channel, which anyone in it reads;
//   - a branch rule that posts live, reviews every push, names a model or announces in a channel of
//     its own, for the same reasons.
//
// Which events that channel hears of (notify_on) is not among them: it sends nothing anywhere the
// channel was not already being sent it, and how much a team hears is the noise an editor tunes.
//
// Each is judged on what the write changes, never on what a form sends back unchanged: a level's
// own fields by review.ChangedFields, and what it makes effective by comparing the resolved settings
// before and after (reviewTierNeeds). The second is what stops an editor reaching the first by the
// side door — resetting a repository's "shadow" so it inherits its connection's "live", deleting the
// group that held a repository to shadow, or moving it out of one.
//
// Everything is addressed by public id, never by a serial, and every read and write names the
// organisation, so another tenant's id answers exactly like one that does not exist.

const (
	reviewRunsPage     = 50
	reviewPullsPages   = 3
	reviewReachDenial  = "Posting live, reviewing every push, forks, context repositories, the model, what a review may spend and the channel it is announced in need the connections permission as well."
	reviewReposDenial  = "Adding repositories to code review saves them as connections of the organisation, which needs the connections permission as well."
	reviewMoneyDenial  = "Code review's budgets need the connections permission as well: they decide what reviews may spend."
	reviewTypeUnsaved  = "draft"
	reviewEstimateBase = 9_000 // the stable half of a finder prompt, in tokens: rubric, instructions, repo map
)

func (b *Bot) reviewRoutes(mux *http.ServeMux) {
	// Every route is gone where CODE_REVIEW=off (reviewOn); where the organisation's plan has no code
	// review they still answer, so the console can show what was set and reviewed before, and the
	// starts refuse with why (reviewPlanRefused).
	view := func(h http.HandlerFunc) http.HandlerFunc { return b.reviewOn(b.requirePerm(PermReviewsView, h)) }
	manage := func(h http.HandlerFunc) http.HandlerFunc { return b.reviewOn(b.requirePerm(PermReviewsManage, h)) }

	mux.HandleFunc("GET /api/review-settings", view(b.handleReviewSettingsTree))
	mux.HandleFunc("POST /api/review-settings", manage(b.handleReviewSettingsAdd))
	mux.HandleFunc("GET /api/review-settings/{id}", view(b.handleReviewSettingGet))
	mux.HandleFunc("PUT /api/review-settings/{id}", manage(b.handleReviewSettingPut))
	mux.HandleFunc("DELETE /api/review-settings/{id}", manage(b.handleReviewSettingDelete))
	mux.HandleFunc("POST /api/review-settings/{id}/move", manage(b.handleReviewSettingMove))
	mux.HandleFunc("POST /api/review-settings/{id}/restore", manage(b.handleReviewSettingRestore))
	mux.HandleFunc("POST /api/review-settings/{id}/remove", manage(b.handleReviewSettingRemove))
	mux.HandleFunc("GET /api/review-settings/{id}/available", manage(b.handleReviewSettingAvailable))
	mux.HandleFunc("POST /api/review-settings/{id}/repos", manage(b.handleReviewSettingRepos))

	mux.HandleFunc("GET /api/review-types", view(b.handleReviewTypes))
	mux.HandleFunc("POST /api/review-types", manage(b.handleReviewTypeCreate))
	mux.HandleFunc("POST /api/review-types/try", manage(b.handleReviewTypeTry))
	mux.HandleFunc("POST /api/review-types/skill-check", manage(b.handleReviewSkillCheck))
	mux.HandleFunc("GET /api/review-types/{id}", view(b.handleReviewTypeGet))
	mux.HandleFunc("PUT /api/review-types/{id}", manage(b.handleReviewTypePut))
	mux.HandleFunc("DELETE /api/review-types/{id}", manage(b.handleReviewTypeDelete))
	mux.HandleFunc("POST /api/review-types/{id}/reset", manage(b.handleReviewTypeReset))
	mux.HandleFunc("GET /api/review-types/{id}/versions", view(b.handleReviewTypeVersions))
	mux.HandleFunc("GET /api/review-types/{id}/versions/{version}", view(b.handleReviewTypeVersion))
	mux.HandleFunc("POST /api/review-types/{id}/revert", manage(b.handleReviewTypeRevert))
	mux.HandleFunc("POST /api/review-types/{id}/enable", manage(b.handleReviewTypeSwitch(true)))
	mux.HandleFunc("POST /api/review-types/{id}/disable", manage(b.handleReviewTypeSwitch(false)))

	mux.HandleFunc("GET /api/reviews", view(b.handleReviewRuns))
	mux.HandleFunc("GET /api/reviews/estimate", view(b.handleReviewEstimate))
	mux.HandleFunc("GET /api/reviews/{id}", view(b.handleReviewRun))
	mux.HandleFunc("POST /api/reviews", manage(b.handleReviewStart))
	mux.HandleFunc("POST /api/reviews/{id}/rerun", manage(b.handleReviewRerun))
	mux.HandleFunc("GET /api/review-pulls", view(b.handleReviewPulls))
	mux.HandleFunc("POST /api/review-pulls/resume", manage(b.handleReviewPullResume))
}

// reviewApp is the GitHub App code review mints its tokens from: the deployment's, which the proxy
// holds too.
func (b *Bot) reviewApp() *githubApp {
	if b.ghApp != nil {
		return b.ghApp
	}
	if b.proxy != nil {
		return b.proxy.ghApp
	}
	return nil
}

// reviewMissing is why pull requests cannot be reviewed as they open on this deployment, as the
// settings that would fix it: no App to mint tokens from, or no secret to verify GitHub's
// deliveries with — without one, GitHub's deliveries are refused and nothing is ever heard about.
// Empty when nothing is missing. Starting a review in the console needs only the App.
//
// Under open signup the webhook secret is left out. Whether the operator has set it is the
// operator's fact, which handleGitHubInstallations already keeps from hosted tenants
// (webhook_secret_set), and every tenant asking would otherwise be told it through this list — and
// through /api/me's github_review, which is this list being empty. A tenant cannot set it either way.
func (b *Bot) reviewMissing() []string {
	out := []string{}
	if !b.reviewApp().configured() {
		out = append(out, "GITHUB_APP_ID", "GITHUB_APP_SLUG", "GITHUB_APP_PRIVATE_KEY_B64")
	}
	if !b.ghHook.configured() && b.cfg.SignupMode != SignupOpen {
		out = append(out, "GITHUB_APP_WEBHOOK_SECRET")
	}
	return out
}

// reviewAPIError answers a store error with the status that says what it was. A node, type, run or
// installation that is not this organisation's is a 404 like one that does not exist.
func reviewAPIError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrReviewSettingNotFound), errors.Is(err, ErrReviewTypeNotFound), errors.Is(err, ErrReviewRunNotFound),
		errors.Is(err, ErrReviewPRNotFound), errors.Is(err, ErrReviewInstallNotLinked):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
	case errors.Is(err, ErrReviewSettingExists), errors.Is(err, ErrReviewGroupNameTaken), errors.Is(err, ErrReviewTypeKeyTaken),
		errors.Is(err, ErrReviewTypeStale), errors.Is(err, ErrReviewSettingsStale):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
	case errors.Is(err, ErrReviewMoveAcrossConnections), errors.Is(err, ErrReviewSettingsInvalid), errors.Is(err, ErrReviewName),
		errors.Is(err, ErrReviewTypeInvalid), errors.Is(err, ErrReviewRunInvalid):
		bad(w, err)
	default:
		fail(w, err)
	}
}

// reviewActor is who a write is recorded as being made by: the email the console shows for them.
func reviewActor(r *http.Request) string {
	if u := adminFromCtx(r.Context()); u != nil {
		return cmp.Or(u.Email, u.Name, u.UserID)
	}
	return ""
}

// reviewProposalDetail adds to a write's audit details the console assistant's card it confirms,
// when it confirms one, so the row pairs with the assistant.proposed row the card left: a model
// suggested this, and then a named person applied it. The id is the browser's to send, so it is
// bounded like any other string the browser puts in a column.
func reviewProposalDetail(details map[string]any, proposalID string) map[string]any {
	if id, _ := cutRunes(strings.TrimSpace(proposalID), 64); id != "" {
		details["proposal_id"] = id
	}
	return details
}

// reviewMayReach reports whether this person may make the changes that reach further than review's
// noise (see the top of the file).
func reviewMayReach(r *http.Request) bool {
	u := adminFromCtx(r.Context())
	return u != nil && u.Permissions[PermConnManage]
}

// ---- the settings tree ----

// reviewTreeIndex is an organisation's whole review tree in memory, read once per request, with
// what the console lays out beside it: the repositories each installation reaches through the
// organisation's saved App connections. The tree is small — a few connections, a few groups, a row
// per repository somebody set something on — and a request that asks about one node usually needs
// its whole chain, so one read beats a query per level.
type reviewTreeIndex struct {
	nodes   []*ReviewSetting
	byID    map[int64]*ReviewSetting
	byPub   map[string]*ReviewSetting
	repoRow map[string]*ReviewSetting // owner/name, lower-cased → its own row
	// appRepos is each installation's repositories among the organisation's App connections,
	// lower-cased and sorted; appInstalls the reverse.
	appRepos    map[int64][]string
	appInstalls map[string][]int64
	// typeKeys is every review type key that exists here, built-in or the organisation's own,
	// switched on or not: what a branch rule may name.
	typeKeys map[string]bool
}

func (b *Bot) reviewTreeIndex(ctx context.Context, orgID int64) (*reviewTreeIndex, error) {
	nodes, err := b.store.ReviewSettingsTree(ctx, orgID)
	if err != nil {
		return nil, err
	}
	t := &reviewTreeIndex{nodes: nodes, byID: map[int64]*ReviewSetting{}, byPub: map[string]*ReviewSetting{},
		repoRow: map[string]*ReviewSetting{}, appRepos: map[int64][]string{}, appInstalls: map[string][]int64{},
		typeKeys: map[string]bool{}}
	for _, n := range nodes {
		t.byID[n.ID], t.byPub[n.PublicID] = n, n
		if n.Kind == reviewKindRepo {
			t.repoRow[n.Repo] = n
		}
	}
	conns, err := b.store.AllConnections(ctx, orgID)
	if err != nil {
		return nil, err
	}
	for _, c := range conns {
		if c.CredType != "github_app" || c.GitHubInstallationID <= 0 || !validGitHubRepo(c.Repo) {
			continue
		}
		repo := strings.ToLower(c.Repo)
		if !slices.Contains(t.appRepos[c.GitHubInstallationID], repo) {
			t.appRepos[c.GitHubInstallationID] = append(t.appRepos[c.GitHubInstallationID], repo)
		}
		if !slices.Contains(t.appInstalls[repo], c.GitHubInstallationID) {
			t.appInstalls[repo] = append(t.appInstalls[repo], c.GitHubInstallationID)
		}
	}
	for id := range t.appRepos {
		slices.Sort(t.appRepos[id])
	}
	for _, bt := range review.BuiltinTypes() {
		t.typeKeys[bt.Key] = true
	}
	rows, err := b.store.ReviewTypes(ctx, orgID)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		t.typeKeys[row.Key] = true
	}
	return t, nil
}

// connectionOf is the connection a node hangs from: itself, its parent, or its parent's parent.
func (t *reviewTreeIndex) connectionOf(n *ReviewSetting) *ReviewSetting {
	for i := 0; n != nil && i < 3; i++ {
		if n.Kind == reviewKindConnection {
			return n
		}
		n = t.byID[n.ParentID]
	}
	return nil
}

// chain is what a node's effective settings are resolved from, broadest first, the node last — a
// repository with no row of its own (virtualRepo) included, since it names its connection as parent.
func (t *reviewTreeIndex) chain(n *ReviewSetting) []*ReviewSetting {
	var out []*ReviewSetting
	for i := 0; n != nil && i < 3; i++ {
		out = append([]*ReviewSetting{n}, out...)
		if n.Kind == reviewKindConnection {
			break
		}
		n = t.byID[n.ParentID]
	}
	return out
}

// virtualRepo is a repository of a connection's installation that has no row of its own: it
// inherits everything, and gets a row (EnsureReviewRepo) the moment something is set on it.
func virtualRepo(conn *ReviewSetting, repo string) *ReviewSetting {
	return &ReviewSetting{Kind: reviewKindRepo, ParentID: conn.ID, ParentPublicID: conn.PublicID, Repo: repo,
		Settings: json.RawMessage("{}")}
}

// resolveChain is the effective settings of a chain, with the last level's own settings replaced by
// last when it is not nil: what a change would make of them before it is saved.
func resolveChain(chain []*ReviewSetting, last *review.Settings) (review.Effective, error) {
	levels, err := reviewLevels(chain)
	if err != nil {
		return review.Effective{}, err
	}
	if last != nil && len(levels) > 0 {
		levels[len(levels)-1].Settings = *last
	}
	return review.Resolve(levels), nil
}

// reviewTarget is the node a settings route names, in a tree read for the request: see reviewTargetIn.
func (b *Bot) reviewTarget(r *http.Request) (*reviewTreeIndex, *ReviewSetting, error) {
	t, err := b.reviewTreeIndex(r.Context(), orgOf(r))
	if err != nil {
		return nil, nil, err
	}
	n, err := reviewTargetIn(t, r.PathValue("id"), r.URL.Query().Get("repo"))
	if err != nil {
		return nil, nil, err
	}
	return t, n, nil
}

// reviewTargetIn is the node an id and a repo name address in t: a node by its public id or, with a
// repo owner/name on a connection's id, one repository of that connection — which may have no row
// yet. The repository has to be the connection's: its own row is under that connection, or the
// organisation has saved it as an App connection of the same installation. Anything else is not
// found. It reads nothing but t, so whatever addresses a node without a request — the console
// assistant, which only proposes — finds exactly the node the route would write.
func reviewTargetIn(t *reviewTreeIndex, id, repo string) (*ReviewSetting, error) {
	n := t.byPub[id]
	if n == nil {
		return nil, ErrReviewSettingNotFound
	}
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return n, nil
	}
	if n.Kind != reviewKindConnection {
		return nil, ErrReviewSettingNotFound
	}
	repo, err := reviewRepoName(repo)
	if err != nil {
		return nil, err
	}
	if row := t.repoRow[repo]; row != nil {
		if c := t.connectionOf(row); c == nil || c.ID != n.ID {
			return nil, ErrReviewSettingNotFound
		}
		return row, nil
	}
	if !slices.Contains(t.appRepos[n.InstallationID], repo) {
		return nil, ErrReviewSettingNotFound
	}
	return virtualRepo(n, repo), nil
}

// reviewNodeByName is a repository named owner/name, and the connection it is reviewed through, as
// the console's Reviews › Settings finds one from a link that only knows the name (resolveSelection
// in review-format.ts): the first connection in the tree's order that reaches it, or the one connPub
// names when two installations both do. A repository with a row of its own is only ever its
// connection's, so the name alone is enough for it; one with no row is any connection's whose
// installation the organisation saved it under. A repository taken out of code review is found too,
// where the console lists it apart: writing its settings is allowed, and saying it reviews nothing
// is the caller's.
func reviewNodeByName(t *reviewTreeIndex, name, connPub string) (*ReviewSetting, *ReviewSetting, error) {
	repo, err := reviewRepoName(name)
	if err != nil {
		return nil, nil, err
	}
	for _, c := range t.nodes {
		if c.Kind != reviewKindConnection || (connPub != "" && c.PublicID != connPub) {
			continue
		}
		if n, err := reviewTargetIn(t, c.PublicID, repo); err == nil {
			return n, c, nil
		}
	}
	return nil, nil, ErrReviewSettingNotFound
}

// reviewNodeJSON is one node as the console lists it. A repository with no row of its own has no id
// and says it inherits.
func reviewNodeJSON(t *reviewTreeIndex, n *ReviewSetting) map[string]any {
	out := map[string]any{"id": n.PublicID, "kind": n.Kind, "parent_id": n.ParentPublicID, "settings": n.Settings,
		"updated_by": n.UpdatedBy, "updated_at": n.UpdatedAt, "inherits": n.PublicID == ""}
	switch n.Kind {
	case reviewKindConnection:
		out["installation_id"], out["removed_at"] = n.InstallationID, n.RemovedAt
	case reviewKindGroup:
		out["name"] = n.Name
	case reviewKindRepo:
		out["repo"], out["removed_at"] = n.Repo, n.RemovedAt
	}
	if eff, err := resolveChain(t.chain(n), nil); err == nil {
		out["mode"] = eff.Mode
	}
	if n.Kind == reviewKindRepo && n.RemovedAt != "" {
		out["mode"] = review.ModeOff // removed from code review, whatever its settings say
	}
	return out
}

// handleReviewSettingsTree is Reviews › Settings: the connections at the top, each with how its
// installation stands, its groups and its repositories — every repository its installation reaches
// through the organisation's App connections, set on or not — and the installations the
// organisation holds that are not reviewed yet, for Add connection.
func (b *Bot) handleReviewSettingsTree(w http.ResponseWriter, r *http.Request) {
	ctx, orgID := r.Context(), orgOf(r)
	t, err := b.reviewTreeIndex(ctx, orgID)
	if err != nil {
		fail(w, err)
		return
	}
	installs, err := b.store.GitHubInstalls(ctx, orgID)
	if err != nil {
		fail(w, err)
		return
	}
	held := map[int64]*GitHubInstall{}
	for _, g := range installs {
		held[g.ID] = g
	}
	conns := []map[string]any{}
	inTree := map[int64]bool{}
	for _, c := range t.nodes {
		if c.Kind != reviewKindConnection {
			continue
		}
		inTree[c.InstallationID] = true
		node := reviewNodeJSON(t, c)
		// How the installation stands at GitHub, as the Workspaces install card says it of Slack.
		// Not held any more — uninstalled, or bound elsewhere since — is "uninstalled": reviews
		// stopped there by themselves (reviewInstallLinked), and nothing was deleted.
		status, missing, known := "uninstalled", []string{}, false
		node["installed_by"], node["installed_at"], node["suspended_at"] = "", "", ""
		if g := held[c.InstallationID]; g != nil {
			status = cmp.Or(g.Status, "active")
			if g.SuspendedAt != "" {
				status = "suspended"
			}
			node["account_login"], node["account_type"], node["repo_selection"] = g.AccountLogin, g.AccountType, g.RepoSelection
			node["installed_by"], node["installed_at"], node["suspended_at"] = g.InstalledBy, g.InstalledAt, g.SuspendedAt
			missing, known = reviewMissingPermissions(g.Permissions)
		}
		node["status"], node["missing_permissions"], node["permissions_known"] = status, missing, known
		node["last_delivery_at"] = ""
		if at, err := b.store.LastGitHubDeliveryAt(ctx, orgID, c.InstallationID); err == nil && !at.IsZero() {
			node["last_delivery_at"] = at.Format(time.RFC3339)
		}
		// When GitHub last refused to resolve a finding's thread for this installation's token, for the
		// card to say that resolving threads needs a permission it lacks; "" once one resolves again
		// (review_thread_resolve.go).
		node["threads_refused_at"] = ""
		if at, ok := b.store.AlertSentAt(ctx, orgID, reviewThreadsRefusedLastKey(c.InstallationID)); ok {
			node["threads_refused_at"] = at.UTC().Format(time.RFC3339)
		}
		groups := []map[string]any{}
		repos := []map[string]any{}
		// Repositories taken out of code review leave the tree for a list of their own, which is
		// where they are restored from.
		removed := []map[string]any{}
		byGroup := map[int64][]map[string]any{}
		for _, n := range t.nodes {
			if t.connectionOf(n) != c {
				continue
			}
			switch {
			case n.Kind == reviewKindRepo && n.RemovedAt != "":
				removed = append(removed, reviewNodeJSON(t, n))
			case n.Kind == reviewKindRepo && n.ParentID == c.ID:
				repos = append(repos, reviewNodeJSON(t, n))
			case n.Kind == reviewKindRepo:
				byGroup[n.ParentID] = append(byGroup[n.ParentID], reviewNodeJSON(t, n))
			}
		}
		for _, repo := range t.appRepos[c.InstallationID] {
			if t.repoRow[repo] == nil {
				repos = append(repos, reviewNodeJSON(t, virtualRepo(c, repo)))
			}
		}
		slices.SortFunc(repos, func(a, b map[string]any) int { return strings.Compare(a["repo"].(string), b["repo"].(string)) })
		for _, g := range t.nodes {
			if g.Kind == reviewKindGroup && g.ParentID == c.ID {
				gj := reviewNodeJSON(t, g)
				gj["repos"] = nonNil(byGroup[g.ID])
				groups = append(groups, gj)
			}
		}
		node["groups"], node["repos"], node["removed"] = groups, repos, removed
		conns = append(conns, node)
	}
	available := []map[string]any{}
	for _, g := range installs {
		if !inTree[g.ID] {
			missing, known := reviewMissingPermissions(g.Permissions)
			available = append(available, map[string]any{"installation_id": g.ID, "account_login": g.AccountLogin,
				"account_type": g.AccountType, "status": cmp.Or(g.Status, "active"), "repo_selection": g.RepoSelection,
				"repos": nonNil(t.appRepos[g.ID]), "missing_permissions": missing, "permissions_known": known})
		}
	}
	// The channels a notify may name (checkReviewSettings): the console's channel pickers offer these
	// and no others — a channel of a workspace disconnected since is still listed under Workspaces,
	// and saving it would only be refused.
	channels, err := b.store.ReviewChannels(ctx, orgID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"connections": conns, "available": available, "channels": channels})
}

// reviewSettingsInput is what a settings write carries: the level's own settings, whole, and for a
// group, its name.
type reviewSettingsInput struct {
	Kind           string          `json:"kind"` // connection | group, on an add
	InstallationID int64           `json:"installation_id"`
	CopyFrom       string          `json:"copy_from"`     // a connection's public id, on an add
	ConnectionID   string          `json:"connection_id"` // a group's connection, on an add
	Name           *string         `json:"name"`
	Settings       json.RawMessage `json:"settings"`
	// Fields, on a write, are the settings it changes: only those are taken from Settings — one left
	// out of Settings is reset — and every other one stays as it is stored. Without it Settings is
	// the level's whole object, as it always was. A form that saves one field sends it, so a page
	// open since before somebody else's change cannot put that change back by sending what it showed.
	Fields []string `json:"fields"`
	// Expect is what the writer read a field it changes as, by reviewFieldDigest — only branch_rules
	// keeps one. Fields stops a save putting back a field it did not mean to change; this stops it
	// replacing one it did, whole, over a list somebody changed since it was read. A digest that no
	// longer matches is ErrReviewSettingsStale, and nothing is written.
	Expect map[string]string `json:"expect"`
	// ProposalID is the console assistant's card this write confirms, recorded on the audit row so
	// it pairs with the assistant.proposed row the card left.
	ProposalID string `json:"proposal_id"`
	// Org is the organisation such a card was proposed in, by its public id: a write naming one that
	// is not the session's is refused (refuseOtherOrg). The page's own saves name none.
	Org    string `json:"org"`
	Parent string `json:"parent"` // a move's destination
}

// decodeReviewSettings reads a level's settings strictly: a key review does not know is refused
// rather than stored to be ignored, which is how a typo — "strictnes" — would otherwise save, look
// saved, and change nothing. A JSON null reads as unset, which is how a field is reset.
func decodeReviewSettings(raw json.RawMessage) (review.Settings, error) {
	var s review.Settings
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" {
		return s, nil
	}
	if len(t) > reviewSettingsMaxBytes || !strings.HasPrefix(t, "{") {
		return s, ErrReviewSettingsInvalid
	}
	dec := json.NewDecoder(strings.NewReader(t))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) || errors.Is(err, io.ErrUnexpectedEOF) {
			return s, fmt.Errorf("%w: %v", ErrReviewSettingsInvalid, err) // not an object after all
		}
		// An object with a key review does not know, or a value of the wrong kind: said as that.
		return s, invalidReviewSettings(fmt.Errorf("settings: %s", strings.TrimPrefix(err.Error(), "json: ")))
	}
	return s, nil
}

// reviewSettingsError is a refusal of a level's settings that reads as its own sentence — "branch_rules:
// rule 2 names review type "nope", which does not exist here" — and still is ErrReviewSettingsInvalid to
// errors.Is, so it answers 400 like it. Wrapped with %w, the refusal came after that error's own words,
// which are about a body that is not a JSON object at all: every rule a form or an assistant's card got
// wrong read as if the whole body had been.
type reviewSettingsError struct{ err error }

func (e reviewSettingsError) Error() string        { return e.err.Error() }
func (e reviewSettingsError) Unwrap() error        { return e.err }
func (e reviewSettingsError) Is(target error) bool { return target == ErrReviewSettingsInvalid }

// invalidReviewSettings is err as a refusal of a level's settings (reviewSettingsError).
func invalidReviewSettings(err error) error { return reviewSettingsError{err} }

// mergeReviewFields is a write that names the fields it changes (reviewSettingsInput.Fields): what is
// stored, with those fields as sent — or reset, where sent leaves one out — and every other field as
// it was. A name that is not a setting is refused, as an unknown key in the settings is.
func mergeReviewFields(stored, sent review.Settings, fields []string) (review.Settings, error) {
	for _, f := range fields {
		if !slices.Contains(review.SettingFields(), f) {
			return review.Settings{}, invalidReviewSettings(fmt.Errorf("fields: %q is not a setting", f))
		}
	}
	asMap := func(s review.Settings) (map[string]json.RawMessage, error) {
		raw, err := json.Marshal(s)
		if err != nil {
			return nil, err
		}
		m := map[string]json.RawMessage{}
		return m, json.Unmarshal(raw, &m)
	}
	out, err := asMap(stored)
	if err != nil {
		return review.Settings{}, err
	}
	in, err := asMap(sent)
	if err != nil {
		return review.Settings{}, err
	}
	for _, f := range fields {
		if v, ok := in[f]; ok {
			out[f] = v
		} else {
			delete(out, f)
		}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return review.Settings{}, err
	}
	var merged review.Settings
	return merged, json.Unmarshal(raw, &merged)
}

// fillReviewNotifyTeams names the workspace of each channel s gives by its id alone, where only one of
// the organisation's workspaces has it — the console's channel picker knows both, an API caller may
// know only the channel. What is stored then always says which workspace to post with. A channel in
// none of them, or in several, is left as it was, for checkReviewSettings to refuse.
func (b *Bot) fillReviewNotifyTeams(ctx context.Context, orgID int64, s *review.Settings) error {
	bare := func(n *review.NotifyChannel) bool { return n != nil && n.Channel != "" && n.Team == "" }
	if !bare(s.Notify) && !slices.ContainsFunc(s.BranchRules, func(r review.BranchRule) bool { return bare(r.Notify) }) {
		return nil
	}
	channels, err := b.store.ReviewChannels(ctx, orgID)
	if err != nil {
		return err
	}
	fill := func(n *review.NotifyChannel) {
		if !bare(n) {
			return
		}
		var teams []string
		for _, c := range channels {
			if c.Channel == n.Channel {
				teams = append(teams, c.Team)
			}
		}
		if len(teams) == 1 {
			n.Team = teams[0]
		}
	}
	fill(s.Notify)
	for i := range s.BranchRules {
		fill(s.BranchRules[i].Notify)
	}
	return nil
}

// storedReviewSettings reads what a level has saved. Leniently, unlike a request: what is stored
// was checked on its way in, and a key this build does not know — written by a newer one, say —
// is one the resolution ignores too (reviewLevels), not a reason the level cannot be shown or
// changed.
func storedReviewSettings(raw json.RawMessage) (review.Settings, error) {
	var s review.Settings
	if t := strings.TrimSpace(string(raw)); t == "" || t == "null" {
		return s, nil
	}
	err := json.Unmarshal(raw, &s)
	return s, err
}

// checkReviewSettings holds one level's settings to what review.Validate says, and then, for the
// fields this write changes, to what only this organisation can say: the models it offers, the
// repositories it connected through the App, and the review types it has. Only the changed ones:
// those facts move — a connection is deleted, a model taken off the list — and a value that was
// right when it was saved must not stop somebody saving a field next to it, least of all one the
// stale value is not theirs to change.
func (b *Bot) checkReviewSettings(ctx context.Context, orgID int64, t *reviewTreeIndex, s review.Settings, changed []string) error {
	errs := []error{}
	if err := s.Validate(); err != nil {
		errs = append(errs, err)
	}
	st := b.settings.Get(ctx, orgID)
	modelOK := func(m string) bool {
		return m == "" || m == "heavy" || m == reviewDefaultModel || routineModelAllowed(st, m)
	}
	if s.Model != nil && slices.Contains(changed, "model") && !modelOK(*s.Model) {
		errs = append(errs, fmt.Errorf("model %q: pick the default, Advanced, or one of the models offered to channels under Settings", *s.Model))
	}
	if slices.Contains(changed, "context_repos") {
		for _, repo := range s.ContextRepos {
			if len(t.appInstalls[strings.ToLower(strings.TrimSpace(repo))]) == 0 {
				errs = append(errs, fmt.Errorf("context_repos: %s is not one of the organisation's repositories connected through the GitHub App", repo))
			}
		}
	}
	if !slices.Contains(changed, "branch_rules") {
		s.BranchRules = nil
	}
	var channels []review.NotifyChannel
	channelOK := func(n *review.NotifyChannel) error {
		if n == nil || !n.Set() {
			return nil
		}
		if channels == nil {
			var err error
			if channels, err = b.store.ReviewChannels(ctx, orgID); err != nil {
				return err
			}
		}
		if slices.Contains(channels, *n) {
			if b.reviewChannelShared(ctx, *n) {
				return fmt.Errorf("%s is shared with another organisation (Slack Connect): a repository's findings are not "+
					"announced where another company reads them", n.Channel)
			}
			return nil
		}
		// A channel given by its id alone is named by its workspace already when only one has it
		// (fillReviewNotifyTeams); one left without is in none of them, or in several.
		same := slices.DeleteFunc(slices.Clone(channels), func(c review.NotifyChannel) bool { return c.Channel != n.Channel })
		if n.Team == "" && len(same) > 1 {
			return fmt.Errorf("%s is shared into more than one of the organisation's workspaces: say which with team", n.Channel)
		}
		return fmt.Errorf("%s is not a channel of this organisation's connected workspaces that the bot is in", n.Channel)
	}
	if slices.Contains(changed, "notify") {
		if err := channelOK(s.Notify); err != nil {
			errs = append(errs, fmt.Errorf("notify: %w", err))
		}
	}
	for i, rule := range s.BranchRules {
		for _, k := range rule.Types {
			if !t.typeKeys[strings.ToLower(k)] {
				errs = append(errs, fmt.Errorf("branch_rules: rule %d names review type %q, which does not exist here", i+1, k))
			}
		}
		if rule.Model != "" && !modelOK(rule.Model) {
			errs = append(errs, fmt.Errorf("branch_rules: rule %d's model %q is not one code review may use", i+1, rule.Model))
		}
		if err := channelOK(rule.Notify); err != nil {
			errs = append(errs, fmt.Errorf("branch_rules: rule %d's notify: %w", i+1, err))
		}
	}
	if len(errs) > 0 {
		return invalidReviewSettings(errors.Join(errs...))
	}
	return nil
}

// reviewTierNeeds is what a change does that needs connections.manage on top of reviews.manage, as
// the fields that say so; empty when reviews.manage is enough. changed is what the level's own
// settings change (review.ChangedFields); before and after are what is effective at a level either
// side of the change, which is what catches a change made by inheritance rather than by setting a
// field.
//
// Posting live, every push, a rule's model and the channel are judged branch by branch, as a pull
// request meets them: what each branch rule, in order, makes of the settings it is applied to
// (reviewReachOf). A level's own mode is only half of where a review goes — a rule may post live
// under a shadow mode, or record in shadow under a live one — so deleting a shadow exception above a
// live fallback turns posting on for the branches it held as surely as setting mode live does. And a
// level that was off reviewed nothing, whatever it held: switching it on is judged like restoring a
// stopped connection, against nothing at all, since a live rule kept dormant under off is switched on
// by whoever switches the level on.
func reviewTierNeeds(changed []string, before, after review.Effective) []string {
	return reviewTierReach(changed, before, after, true)
}

// reviewTierReach is reviewTierNeeds, which names what a changed rule list moves by the field it
// changed, "branch_rules", as the refusal names it — or, byField false, by what it moves there: "mode"
// for a branch that starts posting live, "trigger" for one reviewed on every push, "model" and
// "notify". The second is what the console assistant's card says Confirm will use connections.manage
// for; the one function says both, so the card cannot say other than the check does.
func reviewTierReach(changed []string, before, after review.Effective, byField bool) []string {
	need := reviewFieldNeeds(changed)
	add := func(f string) {
		if !slices.Contains(need, f) {
			need = append(need, f)
		}
	}
	if before.Forks != after.Forks {
		add("forks")
	}
	if !slices.EqualFunc(before.ContextRepos, after.ContextRepos, strings.EqualFold) {
		add("context_repos")
	}
	// The automatic choice reads repositories nobody named, so turning it on reaches further, as
	// naming one does. Turning it off asks nothing more.
	if after.ContextReposAuto && !before.ContextReposAuto {
		add("context_repos_auto")
	}
	if before.Model != after.Model {
		add("model")
	}
	if before.MaxUSD != after.MaxUSD {
		add("max_usd")
	}
	// Which field to name is about what the change touched — the list, or what it is applied under —
	// so it is asked before a level that was off is taken for one with no rules at all.
	rulesChanged := !reflect.DeepEqual(before.BranchRules, after.BranchRules)
	if before.Mode == review.ModeOff || before.Mode == "" {
		before = reviewStopped(before)
	}
	off := after.Mode == review.ModeOff || after.Mode == ""
	if !off && after.Mode == review.ModeLive && before.Mode != review.ModeLive {
		add("mode")
	}
	// A channel told about reviews, set, moved or turned off — by the field, or by what the level now
	// inherits. A level that was off told nobody, so switching one on that inherits a channel starts
	// telling it.
	if !off && after.Notify != before.Notify {
		add("notify")
	}
	if !off && after.Trigger == review.TriggerPush && before.Trigger != review.TriggerPush {
		add("trigger")
	}
	// Fixes on lets the App push commits to the pull request's own branch when somebody with write
	// access asks: a write to the repository's code, which is the connection's to allow, as posting
	// live is. Turning them off asks nothing more.
	if !off && after.Fixes && !before.Fixes {
		add("fixes")
	}
	// Branch by branch. The comparison is of the whole ordered list, since the first match wins: the
	// same rules in another order, or one fewer above a live one, send pull requests elsewhere. It
	// errs towards asking — a rule list that still posts live somewhere, changed in any way that
	// moves which branch meets which rule, needs the permission even when the change only narrowed
	// it — because the one thing it must never do is let a branch start posting unasked. Except
	// where every branch already did: a list ends in the fallback (review.ValidateRules), so one
	// whose every rule posts live posted every pull request live, and nothing can start to.
	was, now := reviewReachOf(before), reviewReachOf(after)
	field := func(level string) string {
		if rulesChanged && byField {
			return "branch_rules"
		}
		return level
	}
	for _, d := range []struct {
		field    string
		on       func(reviewRuleReach) bool
		same     func(a, b reviewRuleReach) bool
		coverAll bool
	}{
		{"mode", func(x reviewRuleReach) bool { return x.Live }, func(a, b reviewRuleReach) bool { return a.Live == b.Live }, true},
		{"trigger", func(x reviewRuleReach) bool { return x.Push }, func(a, b reviewRuleReach) bool { return a.Push == b.Push }, true},
		{"model", func(x reviewRuleReach) bool { return x.Model != "" }, func(a, b reviewRuleReach) bool { return a.Model == b.Model }, false},
	} {
		if !slices.ContainsFunc(now, d.on) {
			continue
		}
		if d.coverAll && len(was) > 0 && !slices.ContainsFunc(was, func(x reviewRuleReach) bool { return !d.on(x) }) {
			continue
		}
		if !slices.EqualFunc(was, now, func(a, b reviewRuleReach) bool { return a.Base == b.Base && a.Head == b.Head && d.same(a, b) }) {
			add(field(d.field))
		}
	}
	// The channel, branch by branch, on the one each branch's pull requests are announced in: a
	// rule's own, an empty one silencing them, or the settings' where the rule names none or is
	// farther from the repository than the level that set the channel (Effective.WithRule). Unlike
	// the model, which a rule only ever adds, any change of where a branch is announced counts,
	// silencing and un-silencing alike — so a list made farther than the channel, by a move or by
	// resetting a repository's own copy of it, is judged as surely as one rewritten. Only lists that
	// name a channel somewhere are compared, so tuning one that never does stays the editor's: the
	// settings' own channel is the check above. A level that was off told nobody, so switching one
	// on counts only a branch it starts announcing.
	if !off && (reviewRulesNotify(before.BranchRules) || reviewRulesNotify(after.BranchRules)) {
		moved := !slices.EqualFunc(was, now, func(a, b reviewRuleReach) bool {
			return a.Base == b.Base && a.Head == b.Head && a.Notify == b.Notify
		})
		if len(was) == 0 {
			moved = slices.ContainsFunc(now, func(x reviewRuleReach) bool { return x.Notify.Set() })
		}
		if moved {
			add(field("notify"))
		}
	}
	slices.Sort(need)
	return need
}

// reviewRulesNotify reports whether any rule of a list names a channel of its own, an empty one
// included.
func reviewRulesNotify(rules []review.BranchRule) bool {
	return slices.ContainsFunc(rules, func(r review.BranchRule) bool { return r.Notify != nil })
}

// reviewFieldNeeds is the fields a level's own change sets that need connections.manage whatever they
// resolve to: forks, context repositories, the model, max_usd and the channel, set or cleared.
func reviewFieldNeeds(changed []string) []string {
	need := []string{}
	for _, f := range changed {
		switch f {
		case "forks", "context_repos", "model", "max_usd", "notify":
			if !slices.Contains(need, f) {
				need = append(need, f)
			}
		}
	}
	return need
}

// reviewStopped is e as a level that reviews nothing leaves it: off, asked for by nobody, telling no
// channel, and with no branch rule to send a pull request anywhere. It is the "before" of switching
// reviews on — a restore, a level set from off to shadow, a group that held its repositories off
// deleted.
func reviewStopped(e review.Effective) review.Effective {
	e.Mode, e.Trigger, e.BranchRules, e.Notify = review.ModeOff, review.TriggerCommand, nil, review.NotifyChannel{}
	return e
}

// reviewRuleReach is what one branch rule makes of the settings it is applied to, in the terms the
// connections permission is about: whether a pull request it matches is posted live, reviewed on
// every push, on a model the rule chose (empty when the settings' own model runs), and the channel it
// is announced in — the rule's, or the settings' when the rule does not override them.
type reviewRuleReach struct {
	Base, Head string
	Live, Push bool
	Model      string
	Notify     review.NotifyChannel
}

// reviewReachOf is e's branch rules, in order, as reviewRuleReach: where each goes, through
// Effective.WithRule as planReview applies it. Settings that are off reach nothing at all. Label
// rules are left out: MatchRule passes over them, so they send no pull request anywhere, and one
// counted here would stand in for the branch rule it replaced — turning "main posts in shadow"
// into a label rule on main reads as the same list while main's pull requests fall through to
// whatever rule is below it.
func reviewReachOf(e review.Effective) []reviewRuleReach {
	if e.Mode == review.ModeOff || e.Mode == "" {
		return nil
	}
	out := make([]reviewRuleReach, 0, len(e.BranchRules))
	for _, r := range e.BranchRules {
		if r.LabelRule() {
			continue
		}
		w := e.WithRule(r)
		x := reviewRuleReach{Base: r.Base, Head: r.Head, Live: w.Mode == review.ModeLive, Push: w.Trigger == review.TriggerPush}
		if w.Source["model"] == review.LevelRule {
			x.Model = w.Model
		}
		x.Notify = w.Notify
		out = append(out, x)
	}
	return out
}

// reviewTreeNeeds is reviewTierNeeds over everything a change to one level reaches: the level itself
// and every level under it, each resolved through its own chain as it stands and as the change leaves
// it. A level below can hold settings of its own that the level above keeps dormant — a repository's
// own live rule under a group that is off — and the change is what wakes them, so judging only the
// level that was edited would let an editor turn on what an admin set and nobody switched on. after
// rewrites one chain the way the change does: a level's settings swapped, a group taken out. A chain
// that cannot be read counts as live: not knowing is no reason to let it through.
func reviewTreeNeeds(t *reviewTreeIndex, n *ReviewSetting, changed []string, after func(chain []*ReviewSetting) ([]review.LevelSettings, error)) []string {
	return reviewTreeReach(t, n, changed, after, true)
}

// reviewTreeReach is reviewTreeNeeds, saying what a changed rule list moves as reviewTierReach does.
func reviewTreeReach(t *reviewTreeIndex, n *ReviewSetting, changed []string, after func(chain []*ReviewSetting) ([]review.LevelSettings, error), byField bool) []string {
	need := reviewFieldNeeds(changed)
	reached := []*ReviewSetting{n} // a repository with no row of its own is reached only as itself
	if n.PublicID != "" {
		for _, m := range t.nodes {
			// A repository removed from code review is reviewed through nothing, before or after.
			if m.ID != n.ID && m.RemovedAt == "" && slices.ContainsFunc(t.chain(m), func(c *ReviewSetting) bool { return c.ID == n.ID }) {
				reached = append(reached, m)
			}
		}
	}
	for _, m := range reached {
		chain := t.chain(m)
		before, err := resolveChain(chain, nil)
		if err != nil {
			return []string{"mode"}
		}
		levels, err := after(chain)
		if err != nil {
			return []string{"mode"}
		}
		if levels == nil {
			continue // the level is gone, and nothing under it is reviewed through it
		}
		for _, f := range reviewTierReach(nil, before, review.Resolve(levels), byField) {
			if !slices.Contains(need, f) {
				need = append(need, f)
			}
		}
	}
	slices.Sort(need)
	return need
}

// reviewChainWith is a chain's levels with the level of node id given settings s: what a write to
// that level makes of the chain.
func reviewChainWith(chain []*ReviewSetting, id int64, s review.Settings) ([]review.LevelSettings, error) {
	levels, err := reviewLevels(chain)
	if err != nil {
		return nil, err
	}
	for i, c := range chain {
		if c.ID == id {
			levels[i].Settings = s
		}
	}
	return levels, nil
}

// reviewChainWithout is a chain's levels with node id taken out — a deleted group's repositories,
// which move up to the connection — or nil when id is the chain's own node, which is gone.
func reviewChainWithout(chain []*ReviewSetting, id int64) ([]review.LevelSettings, error) {
	if len(chain) > 0 && chain[len(chain)-1].ID == id {
		return nil, nil
	}
	kept := slices.DeleteFunc(slices.Clone(chain), func(c *ReviewSetting) bool { return c.ID == id })
	return reviewLevels(kept)
}

// reviewRestoreNeeds is what starting a stopped connection's reviews again turns back on that
// needs connections.manage: posting live, reviewing every push, or telling a channel, anywhere in its
// tree. While it was stopped nothing there was reviewed at all, so whatever its settings say is
// switched on by the restore, as much as by the save that first set it. A level whose settings
// cannot be read counts as live: not knowing is no reason to let it through.
func reviewRestoreNeeds(t *reviewTreeIndex, conn *ReviewSetting) []string {
	var need []string
	for _, n := range t.nodes {
		if t.connectionOf(n) != conn || n.Kind == reviewKindRepo && n.RemovedAt != "" {
			continue // a repository removed from code review stays out of it
		}
		eff, err := resolveChain(t.chain(n), nil)
		if err != nil {
			return []string{"mode"}
		}
		for _, f := range reviewTierNeeds(nil, reviewStopped(eff), eff) {
			if !slices.Contains(need, f) {
				need = append(need, f)
			}
		}
	}
	slices.Sort(need)
	return need
}

// reviewTierRefused answers a change reviewTierNeeds says this person may not make, and reports
// whether it did.
func reviewTierRefused(w http.ResponseWriter, r *http.Request, need []string) bool {
	if len(need) == 0 || reviewMayReach(r) {
		return false
	}
	writeJSON(w, http.StatusForbidden, map[string]any{"error": reviewReachDenial, "fields": need})
	return true
}

// reviewFieldDigest is what a level's field reads as, for a write to say it read it so
// (reviewSettingsInput.Expect): a hash of the field's EFFECTIVE value, not of what the level sets.
// One hash then catches both ways a whole-list save goes stale — the level's own list changed, or,
// where it inherits, the list above it — since either changes what the person was shown and is
// about to replace. It works for a repository with no row of its own, whose chain runs through its
// connection.
//
// Only branch_rules keeps one so far: an ordered list that every save replaces whole, so two people
// each changing one rule of it is where one save most easily drops the other's — and the field the
// console assistant proposes changes to, on a card that may be confirmed minutes after it was read.
func reviewFieldDigest(t *reviewTreeIndex, n *ReviewSetting, field string) (string, error) {
	if field != "branch_rules" {
		return "", fmt.Errorf("no digest is kept of %q", field)
	}
	eff, err := resolveChain(t.chain(n), nil)
	if err != nil {
		// Never a hash of the nothing a failed resolution leaves, which a write could be sent to
		// match: the write is refused instead.
		return "", err
	}
	return reviewRulesDigest(eff.BranchRules), nil
}

// reviewRulesDigest is the hash reviewFieldDigest keeps of a branch rule list.
func reviewRulesDigest(rules []review.BranchRule) string {
	raw, _ := json.Marshal(rules)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:16])
}

// reviewExpectHolds reports whether every field a write said it read (reviewSettingsInput.Expect)
// still reads that way at n.
func reviewExpectHolds(t *reviewTreeIndex, n *ReviewSetting, expect map[string]string) (bool, error) {
	for field, want := range expect {
		got, err := reviewFieldDigest(t, n, field)
		if err != nil {
			return false, err
		}
		if got != want {
			return false, nil
		}
	}
	return true, nil
}

// reviewNodeDetail is one node as its settings panel reads it: its own settings, what is effective
// and where each value comes from, what it would inherit with nothing set here — the "Inherit: High
// · from Frontend" line, and what Reset goes back to — and the chain the sources name. With a base
// and a head branch it also says which branch rule a pull request between them would fall under,
// and with labels what label rules would add to it. Its digests are what a save of the branch rules
// sends back as expect; a write answers with this detail, so the next save carries a fresh one.
func reviewNodeDetail(t *reviewTreeIndex, n *ReviewSetting, base, head string, labels []string) (map[string]any, error) {
	chain := t.chain(n)
	eff, err := resolveChain(chain, nil)
	if err != nil {
		return nil, err
	}
	inherited, err := resolveChain(chain[:len(chain)-1], nil)
	if err != nil {
		return nil, err
	}
	links := []map[string]any{}
	for _, c := range chain {
		links = append(links, map[string]any{"id": c.PublicID, "kind": c.Kind, "name": cmp.Or(c.Name, c.Repo)})
	}
	out := map[string]any{"node": reviewNodeJSON(t, n), "own": n.Settings, "effective": eff, "inherited": inherited, "chain": links,
		"digests": map[string]string{"branch_rules": reviewRulesDigest(eff.BranchRules)}}
	if base != "" || head != "" || len(labels) > 0 {
		if i, rule, ok := review.MatchRule(eff.BranchRules, base, head); ok {
			add, matched := review.LabelTypes(eff.BranchRules, base, head, labels, rule.Types)
			label := rule.String()
			for _, l := range matched {
				label += " +label:" + l
			}
			out["rule"] = map[string]any{"index": i, "label": label, "types": append(slices.Clone(rule.Types), add...),
				"added_types": nonNil(add), "labels": nonNil(matched), "effective": eff.WithRule(rule)}
		}
	}
	return out, nil
}

func (b *Bot) handleReviewSettingGet(w http.ResponseWriter, r *http.Request) {
	t, n, err := b.reviewTarget(r)
	if err != nil {
		reviewAPIError(w, err)
		return
	}
	var labels []string
	for _, l := range strings.Split(r.URL.Query().Get("labels"), ",") {
		if l = strings.TrimSpace(l); l != "" {
			labels = append(labels, l)
		}
	}
	out, err := reviewNodeDetail(t, n, r.URL.Query().Get("base"), r.URL.Query().Get("head"), labels)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, out)
}

// handleReviewSettingsAdd is Add connection — one of the organisation's installations into the
// tree, in shadow, from the built-in defaults or a copy of another connection's settings — and New
// group under a connection.
func (b *Bot) handleReviewSettingsAdd(w http.ResponseWriter, r *http.Request) {
	ctx, orgID := r.Context(), orgOf(r)
	var in reviewSettingsInput
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	t, err := b.reviewTreeIndex(ctx, orgID)
	if err != nil {
		fail(w, err)
		return
	}
	switch in.Kind {
	case reviewKindGroup:
		conn := t.byPub[in.ConnectionID]
		if conn == nil || conn.Kind != reviewKindConnection {
			reviewAPIError(w, ErrReviewSettingNotFound)
			return
		}
		name := ""
		if in.Name != nil {
			name = *in.Name
		}
		g, err := b.store.AddReviewGroup(ctx, orgID, conn.ID, name, reviewActor(r))
		if err != nil {
			reviewAPIError(w, err)
			return
		}
		b.audit(r, "review.group_added", AuditEvent{TargetKind: "review_settings", TargetID: g.PublicID, TargetName: g.Name,
			Details: auditDetails(map[string]any{"connection": conn.PublicID})})
		t, _ = b.reviewTreeIndex(ctx, orgID)
		writeJSON(w, 200, map[string]any{"node": reviewNodeJSON(t, g)})
	case reviewKindConnection:
		settings := review.Settings{}
		if in.CopyFrom != "" {
			src := t.byPub[in.CopyFrom]
			if src == nil || src.Kind != reviewKindConnection {
				reviewAPIError(w, ErrReviewSettingNotFound)
				return
			}
			if settings, err = storedReviewSettings(src.Settings); err != nil {
				fail(w, err)
				return
			}
		}
		// A connection is added in shadow whatever it is copied from: going live is a decision about
		// this installation's repositories, made on purpose once it is in the tree, not a side effect
		// of copying one that was. A branch rule's post is where a review goes as much as the mode is
		// (Effective.WithRule), so a rule that posts live is copied posting wherever the connection's
		// mode says — shadow, until somebody makes it live — and one that records in shadow stays so.
		if settings.Mode != nil && *settings.Mode == review.ModeLive {
			shadow := review.ModeShadow
			settings.Mode = &shadow
		}
		for i := range settings.BranchRules {
			if settings.BranchRules[i].Post == review.ModeLive {
				settings.BranchRules[i].Post = ""
			}
		}
		before := review.Resolve(nil)
		after := review.Resolve([]review.LevelSettings{{Level: review.LevelConnection, Settings: settings}})
		need := reviewTierNeeds(review.ChangedFields(review.Settings{}, settings), before, after)
		for _, n := range t.nodes {
			if n.Kind == reviewKindConnection && n.InstallationID == in.InstallationID && n.RemovedAt != "" {
				// Adding a stopped connection is restoring it, with its own settings, not these.
				need = reviewRestoreNeeds(t, n)
			}
		}
		if reviewTierRefused(w, r, need) {
			return
		}
		raw, _ := json.Marshal(settings)
		c, restored, err := b.store.AddReviewConnection(ctx, orgID, in.InstallationID, raw, reviewActor(r))
		if err != nil {
			reviewAPIError(w, err)
			return
		}
		action := "review.connection_added"
		if restored {
			action = "review.connection_restored"
		}
		b.audit(r, action, AuditEvent{TargetKind: "review_settings", TargetID: c.PublicID,
			TargetName: "installation " + strconv.FormatInt(c.InstallationID, 10),
			Details:    auditDetails(map[string]any{"installation_id": c.InstallationID, "copy_from": in.CopyFrom, "restored": restored})})
		t, _ = b.reviewTreeIndex(ctx, orgID)
		writeJSON(w, 200, map[string]any{"node": reviewNodeJSON(t, c), "restored": restored})
	default:
		bad(w, errors.New(`kind is "connection" or "group"`))
	}
}

// handleReviewSettingPut replaces what one level sets, whole, and renames a group. A repository with
// no row of its own gets one here, under its connection, once everything else has passed.
func (b *Bot) handleReviewSettingPut(w http.ResponseWriter, r *http.Request) {
	ctx, orgID := r.Context(), orgOf(r)
	t, n, err := b.reviewTarget(r)
	if err != nil {
		reviewAPIError(w, err)
		return
	}
	var in reviewSettingsInput
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	// A card names its level by an id only the organisation it was proposed in has, so in another one
	// the level is already not found above; its body's organisation is held to the session's all the
	// same, as the type routes hold it, rather than leave the binding to how a level happens to be named.
	if refuseOtherOrg(w, r, in.Org) {
		return
	}
	if in.Name != nil && n.Kind != reviewKindGroup {
		bad(w, errors.New("only a group has a name to change"))
		return
	}
	// What the writer read is checked before anything else is worked out from what is stored. Only a
	// field this write changes may be named: a digest of one it leaves alone would refuse a save over
	// a change that save could not undo anyway.
	for k := range in.Expect {
		if k != "branch_rules" {
			bad(w, fmt.Errorf("expect: no digest is kept of %q, only of branch_rules", k))
			return
		}
		if !slices.Contains(in.Fields, k) {
			bad(w, fmt.Errorf("expect names %s, a field this write does not change: name it in fields too", k))
			return
		}
	}
	if ok, err := reviewExpectHolds(t, n, in.Expect); err != nil {
		fail(w, err)
		return
	} else if !ok {
		reviewAPIError(w, ErrReviewSettingsStale)
		return
	}
	var before, after review.Settings
	if before, err = storedReviewSettings(n.Settings); err != nil {
		fail(w, err)
		return
	}
	after = before
	if in.Settings != nil || in.Fields != nil { // a rename alone leaves the settings as they are
		sent, err := decodeReviewSettings(in.Settings)
		if err != nil {
			bad(w, err)
			return
		}
		after = sent
		if in.Fields != nil {
			if after, err = mergeReviewFields(before, sent, in.Fields); err != nil {
				bad(w, err)
				return
			}
		}
	}
	if err := b.fillReviewNotifyTeams(ctx, orgID, &after); err != nil {
		fail(w, err)
		return
	}
	changed := review.ChangedFields(before, after)
	if err := b.checkReviewSettings(ctx, orgID, t, after, changed); err != nil {
		bad(w, err)
		return
	}
	// Judged at the level and at every level under it: see reviewTreeNeeds.
	need := reviewTreeNeeds(t, n, changed, func(chain []*ReviewSetting) ([]review.LevelSettings, error) {
		return reviewChainWith(chain, n.ID, after)
	})
	if reviewTierRefused(w, r, need) {
		return
	}
	// A rename alone leaves the settings as they are, so it writes none of them back: a save made
	// meanwhile must not be put back by a request that never meant to touch them.
	writes := in.Name == nil || in.Settings != nil || in.Fields != nil
	if writes {
		// And again from a fresh read, just before anything is written: the checks above are not the
		// write's own. checkReviewSettings asks Slack about every channel a rule names, so what lies
		// between them can be seconds of somebody else's save, not milliseconds. after was made from
		// what this level set when the request began, so a save at this level in that time — of any
		// field, not only one expect names — would be put back whole, and by somebody the tier check
		// never judged for it: "mode" goes back to live under an editor who could not have set it.
		// That is refused, and so is a repository that had no row of its own and has one now. A save
		// above or below undoes nothing here, but can change what this write reaches, so the tiers
		// are judged again on the tree as it now stands. The last gap, between this read and the
		// update, is closed by the update itself, which lands only on the settings it was made from.
		fresh, err := b.reviewTreeIndex(ctx, orgID)
		if err != nil {
			fail(w, err)
			return
		}
		again, err := reviewTargetIn(fresh, r.PathValue("id"), r.URL.Query().Get("repo"))
		if err != nil {
			reviewAPIError(w, err)
			return
		}
		if again.PublicID != n.PublicID || string(again.Settings) != string(n.Settings) {
			reviewAPIError(w, ErrReviewSettingsStale)
			return
		}
		if ok, err := reviewExpectHolds(fresh, again, in.Expect); err != nil {
			fail(w, err)
			return
		} else if !ok {
			reviewAPIError(w, ErrReviewSettingsStale)
			return
		}
		if reviewTierRefused(w, r, reviewTreeNeeds(fresh, again, changed, func(chain []*ReviewSetting) ([]review.LevelSettings, error) {
			return reviewChainWith(chain, again.ID, after)
		})) {
			return
		}
	}
	by := reviewActor(r)
	if writes {
		was := string(n.Settings) // "{}" for a repository with no row yet, as its new row starts
		if n.PublicID == "" {
			if n, err = b.store.EnsureReviewRepo(ctx, orgID, n.ParentID, n.Repo, by); err != nil {
				reviewAPIError(w, err)
				return
			}
		}
		raw, _ := json.Marshal(after)
		if err := b.store.UpdateReviewSettingsFrom(ctx, orgID, n.ID, was, raw, by); err != nil {
			reviewAPIError(w, err)
			return
		}
	}
	if in.Name != nil {
		if err := b.store.RenameReviewGroup(ctx, orgID, n.ID, *in.Name, by); err != nil {
			reviewAPIError(w, err)
			return
		}
	}
	b.audit(r, "review.settings_updated", AuditEvent{TargetKind: "review_settings", TargetID: n.PublicID,
		TargetName: cmp.Or(n.Name, n.Repo, "installation "+strconv.FormatInt(n.InstallationID, 10)),
		Details: auditDetails(reviewProposalDetail(map[string]any{"kind": n.Kind, "changed": nonNil(changed),
			"renamed": in.Name != nil}, in.ProposalID))})
	t, err = b.reviewTreeIndex(ctx, orgID)
	if err != nil {
		fail(w, err)
		return
	}
	out, err := reviewNodeDetail(t, t.byID[n.ID], "", "", nil)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, out)
}

// handleReviewSettingMove puts a repository in a group of its connection, or back directly under the
// connection. What it then inherits changes, so the move is held to the same tiers as a write.
func (b *Bot) handleReviewSettingMove(w http.ResponseWriter, r *http.Request) {
	ctx, orgID := r.Context(), orgOf(r)
	t, n, err := b.reviewTarget(r)
	if err != nil {
		reviewAPIError(w, err)
		return
	}
	if n.Kind != reviewKindRepo {
		bad(w, errors.New("only a repository moves: a group belongs to its connection"))
		return
	}
	var in reviewSettingsInput
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	to := t.byPub[in.Parent]
	if to == nil || to.Kind == reviewKindRepo {
		reviewAPIError(w, ErrReviewSettingNotFound)
		return
	}
	conn := t.connectionOf(t.byID[n.ParentID])
	if c := t.connectionOf(to); c == nil || conn == nil || c.ID != conn.ID {
		reviewAPIError(w, ErrReviewMoveAcrossConnections)
		return
	}
	effBefore, err := resolveChain(t.chain(n), nil)
	if err != nil {
		fail(w, err)
		return
	}
	effAfter, err := resolveChain(append(t.chain(to), n), nil)
	if err != nil {
		fail(w, err)
		return
	}
	if reviewTierRefused(w, r, reviewTierNeeds(nil, effBefore, effAfter)) {
		return
	}
	by := reviewActor(r)
	if n.PublicID == "" {
		// Made where it is going: a repository with no row is under its connection already, and
		// moving it is making its row in the group.
		n, err = b.store.EnsureReviewRepo(ctx, orgID, to.ID, n.Repo, by)
	} else {
		err = b.store.MoveReviewRepo(ctx, orgID, n.ID, to.ID, by)
	}
	if err != nil {
		reviewAPIError(w, err)
		return
	}
	b.audit(r, "review.settings_moved", AuditEvent{TargetKind: "review_settings", TargetID: n.PublicID, TargetName: n.Repo,
		Details: auditDetails(map[string]any{"to": to.PublicID, "to_kind": to.Kind})})
	t, _ = b.reviewTreeIndex(ctx, orgID)
	out, err := reviewNodeDetail(t, t.byID[n.ID], "", "", nil)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, out)
}

// handleReviewSettingDelete removes a level: a group, whose repositories move up to its connection;
// a repository's own row, which goes back to inheriting everything; or a connection, which is
// stopping its reviews — nothing is deleted, and restore brings it back as it was.
func (b *Bot) handleReviewSettingDelete(w http.ResponseWriter, r *http.Request) {
	ctx, orgID := r.Context(), orgOf(r)
	t, n, err := b.reviewTarget(r)
	if err != nil {
		reviewAPIError(w, err)
		return
	}
	by := reviewActor(r)
	action := "review.settings_removed"
	switch {
	case n.Kind == reviewKindConnection:
		// Stopping reviews takes nothing anywhere that needs the connections permission.
		err = b.store.RemoveReviewConnection(ctx, orgID, n.ID, by)
		action = "review.connection_stopped"
	case n.PublicID == "":
		err = ErrReviewSettingNotFound // a repository with no row has nothing to remove
	case n.RemovedAt != "":
		// Its row is what keeps it out of code review: deleting it would put the repository back in,
		// under its connection's settings, as a side effect of clearing its own.
		bad(w, errors.New("this repository is removed from code review: restore it before resetting its settings"))
		return
	default:
		own, derr := storedReviewSettings(n.Settings)
		if derr != nil {
			fail(w, derr)
			return
		}
		// What is reviewed once it is gone: a group's repositories move up to the connection with
		// their own settings, which the group may have held dormant; a repository's own row goes, and
		// the repository inherits its connection's settings and nothing else.
		after := func(chain []*ReviewSetting) ([]review.LevelSettings, error) { return reviewChainWithout(chain, n.ID) }
		if n.Kind == reviewKindRepo {
			after = func([]*ReviewSetting) ([]review.LevelSettings, error) {
				return reviewLevels([]*ReviewSetting{t.connectionOf(n)})
			}
		}
		if reviewTierRefused(w, r, reviewTreeNeeds(t, n, review.ChangedFields(own, review.Settings{}), after)) {
			return
		}
		if n.Kind == reviewKindGroup {
			err = b.store.DeleteReviewGroup(ctx, orgID, n.ID, by)
		} else {
			err = b.store.DeleteReviewRepo(ctx, orgID, n.ID, by)
		}
	}
	if err != nil {
		reviewAPIError(w, err)
		return
	}
	b.audit(r, action, AuditEvent{TargetKind: "review_settings", TargetID: n.PublicID,
		TargetName: cmp.Or(n.Name, n.Repo, "installation "+strconv.FormatInt(n.InstallationID, 10)),
		Details:    auditDetails(map[string]any{"kind": n.Kind})})
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleReviewSettingRestore starts a stopped connection's reviews again, with its settings, groups
// and repositories as they were — the organisation must still hold the installation.
func (b *Bot) handleReviewSettingRestore(w http.ResponseWriter, r *http.Request) {
	ctx, orgID := r.Context(), orgOf(r)
	t, n, err := b.reviewTarget(r)
	if err != nil {
		reviewAPIError(w, err)
		return
	}
	if n.Kind == reviewKindRepo {
		b.restoreReviewRepo(w, r, t, n)
		return
	}
	if n.Kind != reviewKindConnection {
		bad(w, errors.New("only a connection is stopped and restored, and a repository removed from code review"))
		return
	}
	if n.RemovedAt != "" && reviewTierRefused(w, r, reviewRestoreNeeds(t, n)) {
		return
	}
	if err := b.store.RestoreReviewConnection(ctx, orgID, n.ID, reviewActor(r)); err != nil {
		reviewAPIError(w, err)
		return
	}
	b.audit(r, "review.connection_restored", AuditEvent{TargetKind: "review_settings", TargetID: n.PublicID,
		TargetName: "installation " + strconv.FormatInt(n.InstallationID, 10)})
	t, _ = b.reviewTreeIndex(ctx, orgID)
	writeJSON(w, 200, map[string]any{"node": reviewNodeJSON(t, t.byID[n.ID])})
}

// restoreReviewRepo puts a repository removed from code review back in it, with its settings and its
// group as they were — judged like restoring a stopped connection, since while it was out nothing on
// it was reviewed: what its settings would post live, review on every push or tell a channel is
// switched on by the restore, and needs connections.manage. One that was never removed is answered as
// it stands.
func (b *Bot) restoreReviewRepo(w http.ResponseWriter, r *http.Request, t *reviewTreeIndex, n *ReviewSetting) {
	ctx, orgID := r.Context(), orgOf(r)
	if n.PublicID == "" || n.RemovedAt == "" {
		writeJSON(w, 200, map[string]any{"node": reviewNodeJSON(t, n)})
		return
	}
	eff, err := resolveChain(t.chain(n), nil)
	if err != nil {
		fail(w, err)
		return
	}
	if reviewTierRefused(w, r, reviewTierNeeds(nil, reviewStopped(eff), eff)) {
		return
	}
	if err := b.store.RestoreReviewRepo(ctx, orgID, n.ID, reviewActor(r)); err != nil {
		reviewAPIError(w, err)
		return
	}
	b.audit(r, "review.repo_restored", AuditEvent{TargetKind: "review_settings", TargetID: n.PublicID, TargetName: n.Repo})
	t, _ = b.reviewTreeIndex(ctx, orgID)
	writeJSON(w, 200, map[string]any{"node": reviewNodeJSON(t, t.byID[n.ID])})
}

// handleReviewSettingRemove is "Remove from reviews" on a repository: it leaves the tree for its
// connection's Removed list, and nothing on it is reviewed — not automatically, not when asked, and
// its pull requests' deliveries are not even kept — until it is restored. Its settings stay, and so
// does the organisation's connection for it, which the bot's GitHub tools and fix jobs go on using:
// this is taking a repository out of code review, not disconnecting it. Stopping reviews reaches
// nothing further, so reviews.manage is enough, as it is to stop a whole connection.
func (b *Bot) handleReviewSettingRemove(w http.ResponseWriter, r *http.Request) {
	ctx, orgID := r.Context(), orgOf(r)
	t, n, err := b.reviewTarget(r)
	if err != nil {
		reviewAPIError(w, err)
		return
	}
	if n.Kind != reviewKindRepo {
		bad(w, errors.New("only a repository is removed from code review: a connection is stopped, and a group deleted"))
		return
	}
	by := reviewActor(r)
	if n.PublicID == "" {
		if n, err = b.store.EnsureReviewRepo(ctx, orgID, n.ParentID, n.Repo, by); err != nil {
			reviewAPIError(w, err)
			return
		}
	}
	if err := b.store.RemoveReviewRepo(ctx, orgID, n.ID, by); err != nil {
		reviewAPIError(w, err)
		return
	}
	b.audit(r, "review.repo_removed", AuditEvent{TargetKind: "review_settings", TargetID: n.PublicID, TargetName: n.Repo})
	if t, err = b.reviewTreeIndex(ctx, orgID); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"node": reviewNodeJSON(t, t.byID[n.ID])})
}

// reviewInstallRepos is what a connection's installation reaches at GitHub, read live: the list Add
// repositories offers, and what a request to add one is checked against. Only an installation the
// organisation still holds is asked — the tree keeps a connection whose installation is gone, and a
// listing must never be a way to read somebody else's repositories.
func (b *Bot) reviewInstallRepos(ctx context.Context, orgID int64, conn *ReviewSetting) ([]githubRepo, bool, error) {
	if err := b.ownsInstall(ctx, orgID, conn.InstallationID); err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrReviewInstallNotLinked, err)
	}
	if b.proxy == nil {
		return nil, false, errors.New("the GitHub App is not configured on this deployment")
	}
	return b.reposForInstall(ctx, orgID, conn.InstallationID)
}

// reviewConnectionTarget is the connection a connection-only route names; a repository addressed
// through it (?repo=) or another node is not one.
func (b *Bot) reviewConnectionTarget(w http.ResponseWriter, r *http.Request) (*reviewTreeIndex, *ReviewSetting, bool) {
	if r.URL.Query().Get("repo") != "" {
		reviewAPIError(w, ErrReviewSettingNotFound)
		return nil, nil, false
	}
	t, n, err := b.reviewTarget(r)
	if err != nil {
		reviewAPIError(w, err)
		return nil, nil, false
	}
	if n.Kind != reviewKindConnection {
		bad(w, errors.New("repositories are added to a connection"))
		return nil, nil, false
	}
	return t, n, true
}

// handleReviewSettingAvailable is Add repositories' list: the repositories a connection's installation
// reaches at GitHub that are not in its tree — never saved as the organisation's App connections for
// that installation, or removed from code review — each saying whether it is private. Adding one
// saves it as a connection of the organisation, so even reading the list is for whoever may do that.
func (b *Bot) handleReviewSettingAvailable(w http.ResponseWriter, r *http.Request) {
	if !reviewMayReach(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": reviewReposDenial})
		return
	}
	ctx, orgID := r.Context(), orgOf(r)
	t, conn, ok := b.reviewConnectionTarget(w, r)
	if !ok {
		return
	}
	listed, truncated, err := b.reviewInstallRepos(ctx, orgID, conn)
	if err != nil {
		reviewReposError(w, err)
		return
	}
	saved := t.appRepos[conn.InstallationID]
	out := []map[string]any{}
	for _, g := range listed {
		name := strings.ToLower(g.Repo)
		row := t.repoRow[name]
		removed := row != nil && row.RemovedAt != "" && t.connectionOf(row) == conn
		if slices.Contains(saved, name) && !removed {
			continue // in the tree already
		}
		visibility := "public"
		if g.Private {
			visibility = "private"
		}
		out = append(out, map[string]any{"repo": g.Repo, "private": g.Private, "visibility": visibility,
			"removed": removed, "saved": slices.Contains(saved, name), "pushed_at": g.Pushed})
	}
	writeJSON(w, 200, map[string]any{"repos": out, "truncated": truncated})
}

// reviewReposInput is Add repositories' request: owner/name of each, as the list gave them.
type reviewReposInput struct {
	Repos []string `json:"repos"`
}

// handleReviewSettingRepos is Add repositories: each repository named, checked against what the
// connection's installation reaches at GitHub now, is saved as one of the organisation's App
// connections under Repositories — the way installing the App files the ones picked at GitHub, and
// attached to no channel — and one that was removed from code review is put back. Either way it is
// then in the tree, under the connection's settings. Saving connections is connections.manage's.
// GitHub's listing is read only so far (reposForInstall); past it, a repository is asked about by
// name instead — a token minted for it is refused unless the installation reaches it — so an
// installation of more repositories than one listing reads can still have any of them added.
func (b *Bot) handleReviewSettingRepos(w http.ResponseWriter, r *http.Request) {
	if !reviewMayReach(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": reviewReposDenial})
		return
	}
	ctx, orgID := r.Context(), orgOf(r)
	t, conn, ok := b.reviewConnectionTarget(w, r)
	if !ok {
		return
	}
	var in reviewReposInput
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	if len(in.Repos) == 0 || len(in.Repos) > autoConnectMax {
		bad(w, fmt.Errorf("name between 1 and %d repositories", autoConnectMax))
		return
	}
	work, cancel := context.WithTimeout(ctx, time.Duration(10+5*len(in.Repos))*time.Second)
	defer cancel()
	listed, truncated, err := b.reviewInstallRepos(work, orgID, conn)
	if err != nil {
		reviewReposError(w, err)
		return
	}
	reach := map[string]string{} // lower-cased → as GitHub spells it
	for _, g := range listed {
		reach[strings.ToLower(g.Repo)] = g.Repo
	}
	saved := t.appRepos[conn.InstallationID]
	var connect, unlisted, restore, already []string
	failed := []map[string]string{}
	seen := map[string]bool{}
	for _, raw := range in.Repos {
		repo, err := normalizeRepo(raw)
		name := strings.ToLower(repo)
		if err != nil || repo == "" {
			failed = append(failed, map[string]string{"repo": raw, "error": "a repository is written owner/name"})
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		if reach[name] == "" && !truncated {
			failed = append(failed, map[string]string{"repo": repo,
				"error": "not one of the repositories this installation of the GitHub App can reach"})
			continue
		}
		row := t.repoRow[name]
		removed := row != nil && row.RemovedAt != "" && t.connectionOf(row) == conn
		if removed {
			restore = append(restore, name)
		}
		switch {
		case slices.Contains(saved, name):
			// Saved for this installation already: reach enough, listed or not.
			if !removed {
				already = append(already, name)
			}
		case reach[name] != "":
			connect = append(connect, reach[name])
		default:
			unlisted = append(unlisted, repo)
		}
	}
	by := reviewActor(r)
	added := []string{}
	for _, c := range []struct {
		repos []string
		// verified: GitHub listed these a moment ago, so asking again per repository buys nothing;
		// one past the listing is asked about by name, which is what refuses one it does not reach.
		verified bool
	}{{connect, true}, {unlisted, false}} {
		if len(c.repos) == 0 {
			continue
		}
		done, notDone, _ := b.connectRepos(work, orgID, nil, c.repos, repoAuth{installationID: conn.InstallationID, verified: c.verified}, "", by)
		added, failed = append(added, done...), append(failed, notDone...)
		if len(done) > 0 {
			b.changed(ctx, orgID)
		}
	}
	restored := []string{}
	for _, name := range restore {
		if slices.ContainsFunc(failed, func(f map[string]string) bool { return strings.EqualFold(f["repo"], name) }) {
			continue // its connection could not be saved, so it is not reviewed through this installation either
		}
		if err := b.store.RestoreReviewRepo(ctx, orgID, t.repoRow[name].ID, by); err != nil {
			failed = append(failed, map[string]string{"repo": name, "error": err.Error()})
			continue
		}
		restored = append(restored, name)
	}
	// A removed repository whose connection had gone too was saved again and then restored: it is
	// one repository put back, and said once, as restored.
	added = slices.DeleteFunc(added, func(a string) bool { return slices.Contains(restored, strings.ToLower(a)) })
	if len(added) > 0 || len(restored) > 0 {
		b.audit(r, "review.repos_added", AuditEvent{TargetKind: "review_settings", TargetID: conn.PublicID,
			TargetName: "installation " + strconv.FormatInt(conn.InstallationID, 10),
			Details:    auditDetails(map[string]any{"added": added, "restored": restored, "failed": len(failed)})})
	}
	status := http.StatusOK
	if len(added) == 0 && len(restored) == 0 && len(already) == 0 {
		status = http.StatusBadRequest
	}
	out := map[string]any{"added": added, "restored": restored, "already": nonNil(already), "failed": failed}
	if status != http.StatusOK {
		out["error"] = "none of these repositories could be added"
		if len(failed) == 1 {
			out["error"] = failed[0]["error"]
		}
	}
	writeJSON(w, status, out)
}

// reviewReposError answers a failure to read what an installation reaches: not this organisation's
// installation is a 404 like any other id that is not, GitHub not answering is a 502.
func reviewReposError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrReviewInstallNotLinked) {
		reviewAPIError(w, ErrReviewInstallNotLinked)
		return
	}
	writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
}

// ---- review types ----

// reviewTypeView is a type as Reviews › Types shows it: the organisation's row, or the built-in as it
// ships when it has none, with what kind of type it is and how many branch rules name it.
type reviewTypeView struct {
	*ReviewType
	Builtin bool `json:"builtin"` // its key ships with attest_tag
	Custom  bool `json:"custom"`  // the organisation's own
	// Edited is an organisation's copy of a built-in that says something the built-in does not.
	Edited bool `json:"edited"`
	UsedBy int  `json:"used_by"`
	// NewBuiltinRules are rules the built-in has now that the organisation's copy does not — added in
	// a release since the copy was made. They run, as shipped (reviewNewBuiltinRules), and the console
	// shows them as new so a team can switch one off, which saves it into the copy, off.
	NewBuiltinRules []ReviewTypeRule `json:"new_builtin_rules,omitempty"`
}

// reviewTypeOfBuiltin is a built-in as the console shows one nobody here has edited: version 0,
// which no saved version is.
func reviewTypeOfBuiltin(bt review.Type) *ReviewType {
	t := reviewTypeRowOfBuiltin(bt)
	if t.PathGlobs == nil {
		t.PathGlobs = []string{}
	}
	if t.Skills == nil {
		t.Skills = []review.SkillLink{}
	}
	if t.Rules == nil {
		t.Rules = []ReviewTypeRule{}
	}
	for i := range t.Rules {
		t.Rules[i].Position = i
		if t.Rules[i].PathGlobs == nil {
			t.Rules[i].PathGlobs = []string{}
		}
	}
	return t
}

// reviewTypeContent is what a type says, for comparing two: everything but who saved it, when, which
// version it is and whether it is switched on — that last being its own switch, not an edit.
func reviewTypeContent(t *ReviewType) string {
	type rule struct {
		Text, Cap, Bad, Good, Status string
		Globs                        []string
		On                           bool
	}
	c := struct {
		Name, Purpose, Strictness, Model, Inline string
		Globs                                    []string
		MaxUSD                                   float64
		Rules                                    []rule
		Skills                                   []review.SkillLink
	}{t.Name, t.Purpose, t.Strictness, t.Model, t.InlineMinSeverity, append([]string{}, t.PathGlobs...), t.MaxUSD, nil,
		append([]review.SkillLink{}, t.Skills...)}
	for _, r := range t.Rules {
		c.Rules = append(c.Rules, rule{r.Text, r.SeverityCap, r.ExampleBad, r.ExampleGood, cmp.Or(r.Status, "active"),
			append([]string{}, r.PathGlobs...), r.Enabled})
	}
	b, _ := json.Marshal(c)
	return string(b)
}

// reviewTypeUsage counts the branch rules naming each type key across the organisation's tree. A
// rule naming no type runs the default one, and counts for it.
func reviewTypeUsage(t *reviewTreeIndex) map[string]int {
	out := map[string]int{}
	for _, n := range t.nodes {
		s, err := storedReviewSettings(n.Settings)
		if err != nil {
			continue
		}
		for _, rule := range s.BranchRules {
			keys := rule.Types
			if len(keys) == 0 {
				keys = []string{review.DefaultType}
			}
			for _, k := range keys {
				out[strings.ToLower(k)]++
			}
		}
	}
	return out
}

func (b *Bot) reviewTypeViews(ctx context.Context, orgID int64) ([]reviewTypeView, error) {
	rows, err := b.store.ReviewTypes(ctx, orgID)
	if err != nil {
		return nil, err
	}
	t, err := b.reviewTreeIndex(ctx, orgID)
	if err != nil {
		return nil, err
	}
	used := reviewTypeUsage(t)
	byKey := map[string]*ReviewType{}
	for _, row := range rows {
		byKey[row.Key] = row
	}
	out := []reviewTypeView{}
	for _, bt := range review.BuiltinTypes() {
		out = append(out, reviewTypeViewOf(byKey[bt.Key], &bt, used))
		delete(byKey, bt.Key)
	}
	for _, row := range rows {
		if byKey[row.Key] != nil {
			out = append(out, reviewTypeViewOf(row, nil, used))
		}
	}
	return out, nil
}

func reviewTypeViewOf(row *ReviewType, bt *review.Type, used map[string]int) reviewTypeView {
	if bt == nil && row != nil && row.BuiltinKey != "" {
		if t, ok := review.BuiltinType(row.BuiltinKey); ok {
			bt = &t
		}
	}
	v := reviewTypeView{ReviewType: row, Builtin: bt != nil, Custom: bt == nil}
	if row == nil {
		v.ReviewType = reviewTypeOfBuiltin(*bt)
	} else if bt != nil {
		shipped := reviewTypeOfBuiltin(*bt)
		v.Edited = reviewTypeContent(row) != reviewTypeContent(shipped)
		for _, sr := range shipped.Rules {
			if !slices.ContainsFunc(row.Rules, func(r ReviewTypeRule) bool { return r.Text == sr.Text }) {
				v.NewBuiltinRules = append(v.NewBuiltinRules, sr)
			}
		}
	}
	v.UsedBy = used[v.Key]
	return v
}

// reviewTypeTarget is the type a types route names, by the organisation's row's public id or by its
// key: the organisation's row for it when there is one, and the built-in of that key when it is one.
// Not found is neither.
func (b *Bot) reviewTypeTarget(ctx context.Context, orgID int64, id string) (*ReviewType, *review.Type, error) {
	row, err := b.store.ReviewTypeByPublicID(ctx, orgID, id)
	if err != nil {
		return nil, nil, err
	}
	key := strings.ToLower(strings.TrimSpace(id))
	if row != nil {
		key = row.Key
	} else if row, err = b.store.ReviewTypeByKey(ctx, orgID, key); err != nil {
		return nil, nil, err
	}
	var bt *review.Type
	if t, ok := review.BuiltinType(key); ok && (row == nil || row.BuiltinKey == key) {
		bt = &t
	}
	if row == nil && bt == nil {
		return nil, nil, ErrReviewTypeNotFound
	}
	return row, bt, nil
}

func (b *Bot) handleReviewTypes(w http.ResponseWriter, r *http.Request) {
	views, err := b.reviewTypeViews(r.Context(), orgOf(r))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"types": views})
}

func (b *Bot) handleReviewTypeGet(w http.ResponseWriter, r *http.Request) {
	ctx, orgID := r.Context(), orgOf(r)
	row, bt, err := b.reviewTypeTarget(ctx, orgID, r.PathValue("id"))
	if err != nil {
		reviewAPIError(w, err)
		return
	}
	t, err := b.reviewTreeIndex(ctx, orgID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"type": reviewTypeViewOf(row, bt, reviewTypeUsage(t))})
}

// reviewTypeInput is a type as the console sends it. Enabled is a pointer so a save that leaves it
// out keeps the type's switch where it was instead of turning the type off; the same goes for each
// rule's switch, and a new rule with none is on.
type reviewTypeInput struct {
	Key      string `json:"key"`
	CopyFrom string `json:"copy_from"` // a new type's starting point: a key or a public id
	// CopyFromVersion is the version of copy_from the create was made from, 0 for a built-in nobody
	// has edited. Sent, a create over a copy saved since is refused (409): its body carries what it
	// read of that type — the whole rule list, each rule's switch — and creating from that would bring
	// back whatever the save in between changed. Left out, as the Types tab's own dialog leaves it, the
	// copy is of the type as it reads when the create lands.
	CopyFromVersion   *int             `json:"copy_from_version"`
	Name              *string          `json:"name"`
	Purpose           *string          `json:"purpose"`
	PathGlobs         []string         `json:"path_globs"`
	Strictness        *string          `json:"strictness"`
	Model             *string          `json:"model"`
	MaxUSD            *float64         `json:"max_usd"`
	InlineMinSeverity *string          `json:"inline_min_severity"`
	Enabled           *bool            `json:"enabled"`
	Version           int              `json:"version"`
	Rules             []reviewRuleEdit `json:"rules"`
	// Skills is the whole list when sent, an empty one unlinking every skill; left out keeps them.
	Skills *[]review.SkillLink `json:"skills"`
	// ProposalID is the console assistant's card a save or a create confirms, for its audit row, as
	// on a settings write (reviewSettingsInput.ProposalID), and Org the organisation it was proposed
	// in (reviewSettingsInput.Org).
	ProposalID string `json:"proposal_id"`
	Org        string `json:"org"`
}

type reviewRuleEdit struct {
	ID          string   `json:"id"`
	Text        string   `json:"text"`
	SeverityCap string   `json:"severity_cap"`
	PathGlobs   []string `json:"path_globs"`
	ExampleBad  string   `json:"example_bad"`
	ExampleGood string   `json:"example_good"`
	Enabled     *bool    `json:"enabled"`
	Status      string   `json:"status"` // active, proposed or rejected: approving a learned rule is setting it active
}

// apply lays an edit over base, which is the type as it stands — the organisation's row, the
// built-in, or a type being copied — and returns the result. Fields left out keep base's. Rules are
// the whole list: the console sends every rule, in order, and one it leaves out is deleted — save a
// built-in's own rule in its copy, which the save keeps switched off instead (keepBuiltinRules). A
// rule is matched to base's by its public id; its source and where it was learned from are base's
// and not the editor's to change, and a rule with no known id is new and the team's.
func (in reviewTypeInput) apply(base *ReviewType) *ReviewType {
	t := *base
	t.PathGlobs, t.Rules, t.Skills = slices.Clone(base.PathGlobs), slices.Clone(base.Rules), slices.Clone(base.Skills)
	if in.Skills != nil {
		t.Skills = make([]review.SkillLink, 0, len(*in.Skills))
		for _, l := range *in.Skills {
			t.Skills = append(t.Skills, review.NormalizeSkillLink(l))
		}
	}
	if in.Name != nil {
		t.Name = *in.Name
	}
	if in.Purpose != nil {
		t.Purpose = *in.Purpose
	}
	if in.PathGlobs != nil {
		t.PathGlobs = in.PathGlobs
	}
	if in.Strictness != nil {
		t.Strictness = *in.Strictness
	}
	if in.Model != nil {
		t.Model = strings.TrimSpace(*in.Model)
	}
	if in.MaxUSD != nil {
		t.MaxUSD = *in.MaxUSD
	}
	if in.InlineMinSeverity != nil {
		t.InlineMinSeverity = *in.InlineMinSeverity
	}
	if in.Enabled != nil {
		t.Enabled = *in.Enabled
	}
	if in.Rules == nil {
		return &t
	}
	known := map[string]ReviewTypeRule{}
	for _, r := range base.Rules {
		if r.PublicID != "" {
			known[r.PublicID] = r
		}
	}
	// A rule sent with no id may still be one of base's: a built-in nobody has edited has no rule
	// ids to send, and its copy is made on this save. The same text is the same rule, so it keeps
	// its source — a built-in rule left as it was is still a built-in rule after a neighbour changed.
	taken := map[int]bool{}
	byText := func(text string) (ReviewTypeRule, bool) {
		for i, r := range base.Rules {
			if !taken[i] && r.Text == strings.TrimSpace(text) && (r.PublicID == "" || !slices.ContainsFunc(in.Rules,
				func(e reviewRuleEdit) bool { return e.ID == r.PublicID })) {
				taken[i] = true
				return r, true
			}
		}
		return ReviewTypeRule{}, false
	}
	t.Rules = make([]ReviewTypeRule, 0, len(in.Rules))
	for _, e := range in.Rules {
		r, ok := known[e.ID]
		if !ok && e.ID == "" {
			r, ok = byText(e.Text)
		}
		if !ok {
			r = ReviewTypeRule{Source: review.RuleTeam, Enabled: true}
		}
		r.Text, r.SeverityCap, r.PathGlobs, r.ExampleBad, r.ExampleGood = e.Text, e.SeverityCap, e.PathGlobs, e.ExampleBad, e.ExampleGood
		if r.PathGlobs == nil {
			r.PathGlobs = []string{}
		}
		if e.Enabled != nil {
			r.Enabled = *e.Enabled
		}
		if e.Status != "" {
			r.Status = e.Status
		}
		t.Rules = append(t.Rules, r)
	}
	return &t
}

// checkReviewTypeMoney holds what a type may spend to what the API allows on top of ValidateType —
// a model this organisation offers, max_usd inside review's bounds or 0 to inherit — and says
// whether it changes either against was, which needs connections.manage. Only a value this save
// changes is checked, as the settings check only changed fields (checkReviewSettings): a model taken
// off the organisation's list since the type was saved must not stop somebody turning the type off,
// or fixing a rule beside it.
func (b *Bot) checkReviewTypeMoney(ctx context.Context, orgID int64, t, was *ReviewType) (reach bool, err error) {
	var wasModel string
	var wasMax float64
	if was != nil {
		wasModel, wasMax = was.Model, was.MaxUSD
	}
	st := b.settings.Get(ctx, orgID)
	if t.Model != wasModel && t.Model != "" && t.Model != "heavy" && t.Model != reviewDefaultModel && !routineModelAllowed(st, t.Model) {
		return false, fmt.Errorf("%w: model %q: pick the default, Advanced, or one of the models offered to channels under Settings",
			ErrReviewTypeInvalid, t.Model)
	}
	if t.MaxUSD != wasMax && t.MaxUSD != 0 && !(t.MaxUSD >= review.MinMaxUSD && t.MaxUSD <= review.MaxMaxUSD) {
		return false, fmt.Errorf("%w: max_usd is 0 (the settings' own) or between $%.2f and $%.2f", ErrReviewTypeInvalid,
			review.MinMaxUSD, review.MaxMaxUSD)
	}
	return t.Model != wasModel || t.MaxUSD != wasMax, nil
}

// handleReviewTypeCreate makes a new type of the organisation's own, blank or from a copy of any
// type. A built-in's key is refused: the built-in is edited, not shadowed by a type of the same name.
func (b *Bot) handleReviewTypeCreate(w http.ResponseWriter, r *http.Request) {
	ctx, orgID := r.Context(), orgOf(r)
	var in reviewTypeInput
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	if refuseOtherOrg(w, r, in.Org) {
		return
	}
	key := strings.ToLower(strings.TrimSpace(in.Key))
	if _, ok := review.BuiltinType(key); ok {
		reviewAPIError(w, fmt.Errorf("%w: %q is a built-in type's key; edit the built-in, or choose another key", ErrReviewTypeKeyTaken, key))
		return
	}
	base, err := b.reviewTypeCreateBase(ctx, orgID, in.CopyFrom)
	if err != nil {
		reviewAPIError(w, err)
		return
	}
	if in.CopyFrom != "" && in.CopyFromVersion != nil && base.Version != *in.CopyFromVersion {
		reviewAPIError(w, fmt.Errorf("%w: %s was saved since this copy of it was made (v%d now, v%d copied); nothing was created",
			ErrReviewTypeStale, in.CopyFrom, base.Version, *in.CopyFromVersion))
		return
	}
	t := reviewTypeFromCreate(in, base, key)
	reach, err := b.checkReviewTypeMoney(ctx, orgID, t, nil)
	if err != nil {
		bad(w, err)
		return
	}
	if reach && !reviewMayReach(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": reviewReachDenial, "fields": []string{"model", "max_usd"}})
		return
	}
	saved, err := b.store.CreateReviewType(ctx, orgID, t, reviewActor(r))
	if err != nil {
		reviewAPIError(w, err)
		return
	}
	b.audit(r, "review.type_created", AuditEvent{TargetKind: "review_type", TargetID: saved.PublicID, TargetName: saved.Key,
		Details: auditDetails(reviewProposalDetail(map[string]any{"copy_from": in.CopyFrom, "rules": len(saved.Rules)}, in.ProposalID))})
	writeJSON(w, 200, map[string]any{"type": reviewTypeViewOf(saved, nil, nil)})
}

// reviewTypeCreateBase is what a new type starts from: nothing, or a copy of the type copyFrom names
// by key or public id — the organisation's own, or a built-in as it ships — named "Copy of …" and
// switched on, its rules new rows of the new type. It only reads, so the console assistant builds a
// create it proposes from the same start the create will.
func (b *Bot) reviewTypeCreateBase(ctx context.Context, orgID int64, copyFrom string) (*ReviewType, error) {
	if copyFrom == "" {
		return &ReviewType{Enabled: true, PathGlobs: []string{}}, nil
	}
	row, bt, err := b.reviewTypeTarget(ctx, orgID, copyFrom)
	if err != nil {
		return nil, err
	}
	src := row
	if src == nil {
		src = reviewTypeOfBuiltin(*bt)
	}
	c := *src
	c.Rules = make([]ReviewTypeRule, 0, len(src.Rules))
	for _, rule := range src.Rules {
		rule.PublicID, rule.ID = "", 0 // a copy's rules are new rows, of the new type
		c.Rules = append(c.Rules, rule)
	}
	c.Name, c.Enabled = "Copy of "+src.Name, true
	return &c, nil
}

// reviewTypeFromCreate is the type a create makes of in over base (reviewTypeCreateBase): a row of
// the organisation's own under key, whatever base was a copy of. Pure, for the same reason as the
// base: what the handler saves and what a card proposes are built by the one function, and cannot
// drift apart.
func reviewTypeFromCreate(in reviewTypeInput, base *ReviewType, key string) *ReviewType {
	t := in.apply(base)
	t.Key, t.BuiltinKey, t.ID, t.PublicID = key, "", 0, ""
	if in.Rules != nil {
		// The new rules came with no ids worth matching: every one is the new type's.
		for i := range t.Rules {
			t.Rules[i].PublicID = ""
		}
	}
	return t
}

// reviewTypeForEdit is the organisation's row for a type, made the first time one of its built-ins
// is edited (copy-on-write).
func (b *Bot) reviewTypeForEdit(ctx context.Context, orgID int64, row *ReviewType, bt *review.Type, by string) (*ReviewType, error) {
	if row != nil {
		return row, nil
	}
	return b.store.CopyBuiltinReviewType(ctx, orgID, reviewTypeRowOfBuiltin(*bt), by)
}

// saveReviewTypeEdit saves edit — the type as it should now read — as the type's next version,
// against the version the person was looking at (0 for a built-in nobody had edited: its copy is
// version 1), holding model and money changes to connections.manage. keep is an edit of the rule
// list by hand, whose omissions are deletions: a built-in's rule among them is kept, switched off
// (keepBuiltinRules). A revert is not that — the version it goes back to says what it held — and a
// reset holds every rule the built-in ships.
func (b *Bot) saveReviewTypeEdit(w http.ResponseWriter, r *http.Request, row *ReviewType, bt *review.Type,
	edit func(cur *ReviewType) *ReviewType, against int, keep bool, action string, details map[string]any) {
	ctx, orgID, by := r.Context(), orgOf(r), reviewActor(r)
	was := row
	if was == nil {
		was = reviewTypeOfBuiltin(*bt)
	}
	next := edit(was)
	reach, err := b.checkReviewTypeMoney(ctx, orgID, next, was)
	if err != nil {
		bad(w, err)
		return
	}
	if reach && !reviewMayReach(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": reviewReachDenial, "fields": []string{"model", "max_usd"}})
		return
	}
	if err := checkReviewType(next, was.Key); err != nil {
		bad(w, err)
		return
	}
	cur, err := b.reviewTypeForEdit(ctx, orgID, row, bt, by)
	if err != nil {
		reviewAPIError(w, err)
		return
	}
	if row == nil && against == 0 {
		against = 1
	}
	next = edit(cur)
	next.ID, next.Version = cur.ID, against
	if bt != nil && keep {
		next.Rules = keepBuiltinRules(*bt, cur, next.Rules)
	}
	saved, err := b.store.SaveReviewType(ctx, orgID, next, by)
	if err != nil {
		reviewAPIError(w, err)
		return
	}
	if details == nil {
		details = map[string]any{}
	}
	details["version"] = saved.Version
	b.audit(r, action, AuditEvent{TargetKind: "review_type", TargetID: saved.PublicID, TargetName: saved.Key, Details: auditDetails(details)})
	t, _ := b.reviewTreeIndex(ctx, orgID)
	used := map[string]int{}
	if t != nil {
		used = reviewTypeUsage(t)
	}
	writeJSON(w, 200, map[string]any{"type": reviewTypeViewOf(saved, bt, used)})
}

// keepBuiltinRules is rules — the rule list an edit of a built-in's copy saves — with every rule the
// built-in ships that cur held and the edit left out put back at the end, switched off: deleted by
// hand, or reworded, which leaves the shipped text out of the list. Without it the shipped rule
// would come straight back, on, as one the copy lacks (reviewNewBuiltinRules); switched off, it is
// the team's decision about it, kept where the console shows it and can turn it on again. Only text
// the built-in ships now counts: a rule a team reworded is the team's, and a rule a later release
// dropped is gone.
func keepBuiltinRules(bt review.Type, cur *ReviewType, rules []ReviewTypeRule) []ReviewTypeRule {
	out := slices.Clone(rules)
	for _, c := range cur.Rules {
		shipped := slices.ContainsFunc(bt.Rules, func(sr review.TypeRule) bool { return sr.Text == c.Text })
		if !shipped || slices.ContainsFunc(out, func(r ReviewTypeRule) bool { return r.Text == c.Text }) {
			continue
		}
		c.Enabled = false
		if slices.ContainsFunc(out, func(r ReviewTypeRule) bool { return r.PublicID == c.PublicID }) {
			c.PublicID = "" // its row now holds the reworded text; the shipped one is a row of its own
		}
		out = append(out, c)
	}
	return out
}

// handleReviewTypePut saves a type as edited — its fields and its whole rule list, which is how a
// rule is added, edited, turned off, approved or rejected — as its next version. Editing a built-in
// copies it first.
func (b *Bot) handleReviewTypePut(w http.ResponseWriter, r *http.Request) {
	ctx, orgID := r.Context(), orgOf(r)
	row, bt, err := b.reviewTypeTarget(ctx, orgID, r.PathValue("id"))
	if err != nil {
		reviewAPIError(w, err)
		return
	}
	var in reviewTypeInput
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	if refuseOtherOrg(w, r, in.Org) {
		return
	}
	details := map[string]any{}
	if in.Rules != nil {
		// Only when the list was sent: a save that leaves the rules as they are — a card that only
		// switches the type or sets its strictness — would otherwise read as one that saved none.
		details["rules"] = len(in.Rules)
	}
	if in.Enabled != nil {
		// A save can switch the type as well as edit it — it is how a card turns one off, guarded by the
		// version it was read at — and the switch is what takes it out of every branch rule naming it.
		// The /enable and /disable routes have rows of their own for that; this one says it here.
		details["enabled"] = *in.Enabled
	}
	if in.Skills != nil {
		// Where the reviewer is sent to read is worth the audit's line: it is what the next review is told.
		links := make([]string, 0, len(*in.Skills))
		for _, l := range *in.Skills {
			links = append(links, review.NormalizeSkillLink(l).String())
		}
		details["skills"] = links
	}
	b.saveReviewTypeEdit(w, r, row, bt, in.apply, in.Version, in.Rules != nil, "review.type_saved",
		reviewProposalDetail(details, in.ProposalID))
}

// handleReviewTypeReset puts a built-in back as it ships. Nothing is deleted — a run names the
// {key, version} it ran with, and a version that meant two things would explain neither — so the
// built-in's text is saved as the copy's newest version, its switch left where it is.
func (b *Bot) handleReviewTypeReset(w http.ResponseWriter, r *http.Request) {
	ctx, orgID := r.Context(), orgOf(r)
	row, bt, err := b.reviewTypeTarget(ctx, orgID, r.PathValue("id"))
	if err != nil {
		reviewAPIError(w, err)
		return
	}
	if bt == nil {
		bad(w, errors.New("only a built-in type is reset; a type of your own is reverted to one of its versions"))
		return
	}
	if row == nil {
		writeJSON(w, 200, map[string]any{"type": reviewTypeViewOf(nil, bt, nil)}) // nothing to reset
		return
	}
	b.saveReviewTypeEdit(w, r, row, bt, func(cur *ReviewType) *ReviewType {
		shipped := reviewTypeOfBuiltin(*bt)
		shipped.Enabled = cur.Enabled
		// The shipped rules replace the copy's whole list, learned ones included: none of them has an
		// id the copy knows, so the copy's are deleted and these are written new.
		return shipped
	}, row.Version, false, "review.type_reset", nil)
}

// handleReviewTypeDelete deletes a type of the organisation's own, against ?version=, the version
// the person was looking at. A built-in, or the organisation's copy of one, is turned off or reset
// instead. A type a branch rule still names is refused: deleting it would leave the rule naming
// nothing, which a pull request's summary could only call "no such review type".
func (b *Bot) handleReviewTypeDelete(w http.ResponseWriter, r *http.Request) {
	ctx, orgID := r.Context(), orgOf(r)
	row, bt, err := b.reviewTypeTarget(ctx, orgID, r.PathValue("id"))
	if err != nil {
		reviewAPIError(w, err)
		return
	}
	if bt != nil {
		bad(w, errors.New("a built-in type is not deleted; turn it off, or reset it to the built-in"))
		return
	}
	version, err := strconv.Atoi(r.URL.Query().Get("version"))
	if err != nil || version < 1 {
		bad(w, errors.New("send ?version=, the version of the type being deleted"))
		return
	}
	t, err := b.reviewTreeIndex(ctx, orgID)
	if err != nil {
		fail(w, err)
		return
	}
	if n := reviewTypeUsage(t)[row.Key]; n > 0 {
		rules := "branch rules name"
		if n == 1 {
			rules = "branch rule names"
		}
		writeJSON(w, http.StatusConflict, map[string]any{"error": fmt.Sprintf(
			"%d %s %s; take it out of them first, or turn the type off instead", n, rules, row.Key)})
		return
	}
	if err := b.store.DeleteReviewType(ctx, orgID, row.ID, version, reviewActor(r)); err != nil {
		reviewAPIError(w, err)
		return
	}
	b.audit(r, "review.type_deleted", AuditEvent{TargetKind: "review_type", TargetID: row.PublicID, TargetName: row.Key,
		Details: auditDetails(map[string]any{"version": row.Version})})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (b *Bot) handleReviewTypeVersions(w http.ResponseWriter, r *http.Request) {
	ctx, orgID := r.Context(), orgOf(r)
	row, _, err := b.reviewTypeTarget(ctx, orgID, r.PathValue("id"))
	if err != nil {
		reviewAPIError(w, err)
		return
	}
	if row == nil {
		writeJSON(w, 200, map[string]any{"versions": []ReviewTypeVersion{}}) // a built-in nobody has edited
		return
	}
	vs, err := b.store.ReviewTypeVersions(ctx, orgID, row.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"versions": vs})
}

// handleReviewTypeVersion is a type exactly as one version of it read: what an old run ran with.
func (b *Bot) handleReviewTypeVersion(w http.ResponseWriter, r *http.Request) {
	ctx, orgID := r.Context(), orgOf(r)
	row, _, err := b.reviewTypeTarget(ctx, orgID, r.PathValue("id"))
	if err != nil {
		reviewAPIError(w, err)
		return
	}
	v, _ := strconv.Atoi(r.PathValue("version"))
	var snap *ReviewType
	if row != nil && v > 0 {
		if snap, err = b.store.ReviewTypeAtVersion(ctx, orgID, row.ID, v); err != nil {
			fail(w, err)
			return
		}
	}
	if snap == nil {
		writeJSON(w, 404, map[string]any{"error": "no such version of this review type"})
		return
	}
	writeJSON(w, 200, map[string]any{"type": snap})
}

// handleReviewTypeRevert saves an old version's text as the newest version: the history only ever
// grows, so the run that used the version being undone still names what it ran with.
func (b *Bot) handleReviewTypeRevert(w http.ResponseWriter, r *http.Request) {
	ctx, orgID := r.Context(), orgOf(r)
	row, bt, err := b.reviewTypeTarget(ctx, orgID, r.PathValue("id"))
	if err != nil {
		reviewAPIError(w, err)
		return
	}
	var in struct {
		Version int `json:"version"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	var snap *ReviewType
	if row != nil && in.Version > 0 {
		if snap, err = b.store.ReviewTypeAtVersion(ctx, orgID, row.ID, in.Version); err != nil {
			fail(w, err)
			return
		}
	}
	if snap == nil {
		writeJSON(w, 404, map[string]any{"error": "no such version of this review type"})
		return
	}
	b.saveReviewTypeEdit(w, r, row, bt, func(cur *ReviewType) *ReviewType {
		t := *snap
		t.Rules = slices.Clone(snap.Rules)
		// The text goes back, and the switch stays where it is, as Reset leaves it: turning a type on
		// or off is its own switch, not an edit (reviewTypeContent), and reverting a rule must not take
		// the type out of every branch rule because it happened to be off when that version was saved.
		t.Enabled = cur.Enabled
		return &t
	}, row.Version, false, "review.type_reverted", map[string]any{"to": in.Version})
}

// handleReviewTypeSwitch turns a type on or off. Off takes it out of every branch rule and the Start
// review dialog — the rules that name it say so in the console — and keeps it, so it can come back.
func (b *Bot) handleReviewTypeSwitch(on bool) http.HandlerFunc {
	action := "review.type_disabled"
	if on {
		action = "review.type_enabled"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, orgID := r.Context(), orgOf(r)
		row, bt, err := b.reviewTypeTarget(ctx, orgID, r.PathValue("id"))
		if err != nil {
			reviewAPIError(w, err)
			return
		}
		version := 0
		if row != nil {
			version = row.Version
		}
		b.saveReviewTypeEdit(w, r, row, bt, func(cur *ReviewType) *ReviewType {
			t := *cur
			t.Enabled = on
			return &t
		}, version, false, action, nil)
	}
}

// handleReviewTypeTry runs a type as it is being edited, before it is saved, on a pull request
// somebody picked: in shadow, always, so its findings show in the console and nothing is posted.
// It is how a new rule is checked before it can say anything on a real pull request.
func (b *Bot) handleReviewTypeTry(w http.ResponseWriter, r *http.Request) {
	ctx, orgID := r.Context(), orgOf(r)
	var in struct {
		Type reviewTypeInput `json:"type"`
		Repo string          `json:"repo"`
		PR   int             `json:"pr"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	named := strings.ToLower(strings.TrimSpace(in.Type.Key))
	key := cmp.Or(named, reviewTypeUnsaved)
	// What it is tried against, for the money check and the rules' ids: the type of that key as it
	// stands here, if there is one. A new type, which has no key yet, is tried from nothing — never
	// from a type of the organisation's that happens to be called "draft", whose model, budget and
	// rules it would otherwise quietly inherit.
	var was *ReviewType
	base := &ReviewType{Enabled: true, PathGlobs: []string{}}
	if named != "" {
		switch row, bt, err := b.reviewTypeTarget(ctx, orgID, named); {
		case err == nil:
			if was = row; was == nil {
				was = reviewTypeOfBuiltin(*bt)
			}
			base = was
		case !errors.Is(err, ErrReviewTypeNotFound):
			fail(w, err)
			return
		}
	}
	t := in.Type.apply(base)
	t.Key, t.Enabled = key, true // a try runs the type whatever its switch says: trying it is the point
	if err := checkReviewType(t, key); err != nil {
		bad(w, err)
		return
	}
	reach, err := b.checkReviewTypeMoney(ctx, orgID, t, was)
	if err != nil {
		bad(w, err)
		return
	}
	if reach && !reviewMayReach(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": reviewReachDenial, "fields": []string{"model", "max_usd"}})
		return
	}
	b.startReview(w, r, reviewStart{Repo: in.Repo, PR: in.PR, Post: string(review.ModeShadow)}, t)
}

// handleReviewSkillCheck reads a skill link the way a review would, for the type editor to show what
// it points at before it is saved: the commit, the files, and anything worth a warning. It takes the
// link's fields, or a github.com address pasted whole, which it answers with as fields. A link to
// the repository under review is read from against, one of the organisation's repositories, at its
// default branch, since a review reads it at each pull request's base.
func (b *Bot) handleReviewSkillCheck(w http.ResponseWriter, r *http.Request) {
	ctx, orgID := r.Context(), orgOf(r)
	var in struct {
		URL     string `json:"url"`
		Repo    string `json:"repo"`
		Path    string `json:"path"`
		Ref     string `json:"ref"`
		Against string `json:"against"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	l := review.NormalizeSkillLink(review.SkillLink{Repo: in.Repo, Path: in.Path, Ref: in.Ref})
	if strings.TrimSpace(in.URL) != "" {
		parsed, err := review.ParseSkillURL(in.URL)
		if err != nil {
			bad(w, errors.New("paste a folder or file on github.com, such as https://github.com/owner/repo/tree/main/skills/review"))
			return
		}
		l = parsed
	}
	if err := review.ValidateSkillLinks([]review.SkillLink{l}); err != nil {
		bad(w, err)
		return
	}
	if b.review == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "code review is not running on this deployment"})
		return
	}
	// A check reads GitHub as a review would, and spends the reads without credentials that every
	// organisation here shares: like starting a review, it is for a plan that has code review.
	if b.reviewPlanRefused(w, r) {
		return
	}
	if ok, wait := skillChecks.allow("skill-check:"+adminFromCtx(ctx).OrgPublic, skillChecksPerOrgAnHour, time.Hour); !ok {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": fmt.Sprintf(
			"%d checks an hour is the most one organisation may run; try again in %s", skillChecksPerOrgAnHour, wait.Round(time.Minute))})
		return
	}
	conns, err := b.store.AllConnections(ctx, orgID)
	if err != nil {
		fail(w, err)
		return
	}
	installs := map[string]int64{}
	for _, c := range conns {
		if c.CredType == "github_app" && c.GitHubInstallationID > 0 && c.Repo != "" {
			if _, ok := installs[strings.ToLower(c.Repo)]; !ok {
				installs[strings.ToLower(c.Repo)] = c.GitHubInstallationID
			}
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	writeJSON(w, 200, b.review.checkSkill(cctx, orgID, installs, l, strings.TrimSpace(in.Against)))
}

// ---- runs ----

// reviewRunPost is where a run's result went, or was to go: live or shadow, or "" for a run that
// ended before it was decided and was asked for nothing in particular — the repository's mode then.
func reviewRunPost(r *ReviewRun) string {
	switch {
	case reviewTry(r), r.Status == "shadow":
		return string(review.ModeShadow)
	case r.Status == "posted" || r.GitHubReviewID > 0:
		return string(review.ModeLive)
	}
	return string(reviewOptionsOf(r).Post)
}

func reviewScore(n int) any {
	if n < 0 {
		return nil
	}
	return n
}

// reviewRunJSON is one run as Reviews › History lists it.
func reviewRunJSON(r *ReviewRun) map[string]any {
	types := r.Types
	if types == nil {
		types = []ReviewRunType{}
	}
	return map[string]any{"id": r.PublicID, "repo": r.Repo, "pr": r.PRNumber, "kind": r.Kind, "trigger": r.Trigger,
		"requested_by": r.RequestedBy, "status": r.Status, "score": reviewScore(r.Score), "head_sha": r.HeadSHA,
		"base_sha": r.BaseSHA, "rule": r.RuleLabel, "types": types, "post": reviewRunPost(r), "findings": r.Kept,
		"candidates": r.Candidates, "dropped": r.Dropped, "files_reviewed": r.FilesReviewed, "cost_usd": r.CostUSD,
		"error": r.Error, "created_at": r.CreatedAt, "started_at": r.StartedAt, "finished_at": r.FinishedAt}
}

func (b *Bot) handleReviewRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pr, _ := strconv.Atoi(q.Get("pr"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = reviewRunsPage
	}
	var kinds []string
	if k := strings.TrimSpace(q.Get("kind")); k != "" {
		for _, s := range strings.Split(k, ",") {
			if s = strings.TrimSpace(s); slices.Contains(reviewRunKinds, s) {
				kinds = append(kinds, s)
			}
		}
		if len(kinds) == 0 {
			bad(w, fmt.Errorf("kind is one or more of %s", strings.Join(reviewRunKinds, ", ")))
			return
		}
	}
	runs, err := b.store.ReviewRunsPage(r.Context(), orgOf(r), ReviewRunFilter{Repo: q.Get("repo"), PR: pr,
		Status: strings.TrimSpace(q.Get("status")), Kinds: kinds, Before: strings.TrimSpace(q.Get("cursor")), Limit: limit})
	if err != nil {
		fail(w, err)
		return
	}
	// Beside what each run kept, how much of it is still open: the list's "2 open of 3", whose rows
	// are the detail's Open findings.
	ids := make([]int64, 0, len(runs))
	for _, run := range runs {
		ids = append(ids, run.ID)
	}
	open, err := b.store.reviewRunsOpenFindings(r.Context(), orgOf(r), ids)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]map[string]any, 0, len(runs))
	for _, run := range runs {
		row := reviewRunJSON(run)
		row["open"] = open[run.ID]
		out = append(out, row)
	}
	// The next page's cursor only when this one was full, so an empty answer is the end.
	next := ""
	if len(runs) == limit {
		next = runs[len(runs)-1].PublicID
	}
	writeJSON(w, 200, map[string]any{"runs": out, "next_cursor": next})
}

// reviewFindingJSON is one finding as a run's detail shows it, with how it got where it stands.
func reviewFindingJSON(pr *ReviewPR, f *ReviewFinding, history []map[string]any) map[string]any {
	out := map[string]any{"id": f.PublicID, "path": f.Path, "side": f.Side, "start_line": f.StartLine, "line": f.Line,
		"severity": f.Severity, "category": f.Category, "title": f.Title, "body": f.Scenario, "suggestion": f.Suggestion,
		"evidence": nonNil(f.Evidence), "rule_ids": nonNil(f.RuleIDs), "types": f.TypeKeys(),
		"pre_existing": f.PreExisting, "kind": f.Kind, "placement": f.Placement, "place": f.Place,
		"possibly_outdated": f.PossiblyOutdated, "status": f.Status, "status_reason": f.StatusReason, "status_by": f.StatusBy,
		"claimed_fixed_sha": f.ClaimedFixedSHA, "claimed_by": f.ClaimedBy, "verifier_confidence": f.VerifierConfidence,
		"comment_url": "", "created_at": f.CreatedAt, "updated_at": f.UpdatedAt, "history": history}
	if f.GitHubCommentID > 0 {
		out["comment_url"] = fmt.Sprintf("https://github.com/%s/pull/%d#discussion_r%d", pr.Repo, pr.Number, f.GitHubCommentID)
	}
	return out
}

// reviewFindingHistory is how each of a pull request's findings came to stand where it does: raised,
// then each reply in its thread that got as far as a verdict, then where it stands now if a reply
// does not explain it — a push that fixed it, a person resolving its thread.
func reviewFindingHistory(f *ReviewFinding, replies []*ReviewRun) []map[string]any {
	out := []map[string]any{{"at": f.CreatedAt, "status": string(review.FindingOpen), "what": "raised", "by": "code review"}}
	for i := len(replies) - 1; i >= 0; i-- { // replies come newest first
		run := replies[i]
		var req reviewReplyRequest
		json.Unmarshal([]byte(run.RequestJSON), &req) // written only by queueReviewReply
		ck, ok := replyCheckpointFrom(run)
		if req.Finding != f.PublicID || !ok {
			continue
		}
		out = append(out, map[string]any{"at": cmp.Or(run.FinishedAt, run.StartedAt, run.CreatedAt), "what": "reply",
			"by": "github:" + cmp.Or(ck.Login, req.Login), "class": ck.Class, "verdict": ck.Verdict, "outcome": ck.Outcome,
			"run": run.PublicID})
	}
	if f.Status != review.FindingOpen {
		out = append(out, map[string]any{"at": f.UpdatedAt, "status": string(f.Status), "what": "now",
			"by": f.StatusBy, "reason": f.StatusReason})
	}
	return out
}

// handleReviewRun is one run in full: what was asked for, under which rule and types, what it came
// to, every finding it raised and how each stands now, what it dropped and why, what it did not read,
// what it cost, how long it took, and where it is on GitHub.
func (b *Bot) handleReviewRun(w http.ResponseWriter, r *http.Request) {
	ctx, orgID := r.Context(), orgOf(r)
	run, err := b.store.ReviewRunByPublicID(ctx, orgID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	if run == nil {
		reviewAPIError(w, ErrReviewRunNotFound)
		return
	}
	pr, err := b.store.ReviewPR(ctx, orgID, run.ReviewPRID)
	if err != nil || pr == nil {
		reviewAPIError(w, cmp.Or(err, ErrReviewPRNotFound))
		return
	}
	opts := reviewOptionsOf(run)
	request := map[string]any{"trigger": run.Trigger, "trigger_ref": run.TriggerRef, "requested_by": run.RequestedBy,
		"post": string(opts.Post), "allow_live": opts.AllowLive, "scope": opts.Scope, "types": nonNil(opts.Types),
		"full": opts.Full}
	if opts.Inline != nil {
		request["inline"] = opts.Inline
	}
	out := map[string]any{"run": reviewRunJSON(run), "request": request, "summary": run.Summary, "risk": run.Risk}
	prJSON := map[string]any{"repo": pr.Repo, "number": pr.Number, "state": pr.State, "author": pr.AuthorLogin,
		"head_sha": pr.HeadSHA, "last_reviewed_sha": pr.LastReviewedSHA, "score": reviewScore(pr.Score), "reviews": pr.ReviewsCount,
		"skip_reason": pr.SkipReason, "is_fork": pr.IsFork, "is_private": pr.IsPrivate,
		"url": fmt.Sprintf("https://github.com/%s/pull/%d", pr.Repo, pr.Number)}
	maps.Copy(prJSON, reviewPausedJSON(pr))
	out["pr"] = prJSON
	ck, _ := checkpointFrom(run)
	if ck == nil {
		ck = &reviewCheckpoint{}
	}
	types := []map[string]any{}
	for _, t := range run.Types {
		entry := map[string]any{"key": t.Key, "version": t.Version}
		for _, ct := range ck.Types {
			if ct.Key == t.Key {
				entry["summary"], entry["skipped"], entry["auto"] = ct.Summary, ct.Skipped, ct.Auto
			}
		}
		types = append(types, entry)
	}
	for _, ct := range ck.Types { // asked for and not run: turned off, or no such type
		if !slices.ContainsFunc(run.Types, func(t ReviewRunType) bool { return t.Key == ct.Key }) {
			types = append(types, map[string]any{"key": ct.Key, "summary": ct.Summary, "skipped": ct.Skipped})
		}
	}
	out["types"] = types
	findings, err := b.store.reviewRunFindings(ctx, orgID, run.ID)
	if err != nil {
		fail(w, err)
		return
	}
	replies, err := b.store.reviewReplyRuns(ctx, orgID, pr.ID, 500)
	if err != nil {
		fail(w, err)
		return
	}
	fs := make([]map[string]any, 0, len(findings))
	for _, f := range findings {
		fs = append(fs, reviewFindingJSON(pr, f, reviewFindingHistory(f, replies)))
	}
	out["findings"] = fs
	out["dropped"] = nonNil(ck.Drops)
	out["resolved"] = nonNil(ck.Resolved) // what it decided about earlier findings: moved, fixed, gone, a claim refuted
	notReviewed := []reviewNotReviewed{}
	for _, f := range run.NotReviewed {
		notReviewed = append(notReviewed, reviewNotReviewed{Path: f.Path, Reason: f.Reason})
	}
	if len(notReviewed) == 0 && len(ck.NotReviewed) > 0 {
		notReviewed = ck.NotReviewed // still running, or ended before it recorded its own
	}
	out["not_reviewed"] = notReviewed
	out["context_repos"], out["context_notes"] = nonNil(ck.ContextRepos), nonNil(ck.ContextNotes)
	out["skills"] = nonNil(ck.Skills)
	out["full_coverage"], out["injection"] = ck.FullCoverage, ck.Injection
	out["usage"] = map[string]any{"model": run.Model, "tokens_in": run.TokensIn, "tokens_out": run.TokensOut,
		"tokens_cached": run.TokensCached, "cost_usd": run.CostUSD, "reserved_usd": run.ReservedUSD}
	timings := map[string]any{"created_at": run.CreatedAt, "started_at": run.StartedAt, "finished_at": run.FinishedAt, "duration_ms": nil}
	if s, err1 := time.Parse(time.DateTime, run.StartedAt); err1 == nil {
		if f, err2 := time.Parse(time.DateTime, run.FinishedAt); err2 == nil && !f.Before(s) {
			timings["duration_ms"] = f.Sub(s).Milliseconds()
		}
	}
	out["timings"] = timings
	links := map[string]any{"pull_request": fmt.Sprintf("https://github.com/%s/pull/%d", pr.Repo, pr.Number), "review": "", "summary": ""}
	if run.GitHubReviewID > 0 {
		links["review"] = fmt.Sprintf("https://github.com/%s/pull/%d#pullrequestreview-%d", pr.Repo, pr.Number, run.GitHubReviewID)
	}
	if pr.SummaryCommentID > 0 && run.Status == "posted" {
		links["summary"] = fmt.Sprintf("https://github.com/%s/pull/%d#issuecomment-%d", pr.Repo, pr.Number, pr.SummaryCommentID)
	}
	out["links"] = links
	writeJSON(w, 200, out)
}

// reviewPausedJSON is where a pull request stands with its automatic reviews, for the console: paused
// or not, how many have run towards the pause, the pause's ceiling, and — when paused — whether the
// ceiling or a person paused them (by: "auto" or "member"), which is what the footer says too. The
// ceiling is a setting (auto_pause_after); one that paused them is the count it paused them at, as the
// footer says, and otherwise the built-in one.
func reviewPausedJSON(pr *ReviewPR) map[string]any {
	out := map[string]any{"paused": pr.Paused, "auto_reviews": pr.AutoReviews, "auto_pause_after": reviewAutoPauseAfter,
		"paused_by": ""}
	if pr.Paused {
		out["paused_by"] = "member"
		if pr.PausedAuto {
			out["paused_by"], out["auto_pause_after"] = "auto", pr.AutoReviews
		}
	}
	return out
}

// installationFor is the installation a repository is reviewed through here: the one its own
// settings row hangs from, or else the one its App connection names — preferring one whose reviews
// are on when the organisation has connected it through two. 0 when it has neither, which is a
// repository this organisation does not reach through the App: the only way code review reaches
// anything.
func (t *reviewTreeIndex) installationFor(repo string) int64 {
	repo = strings.ToLower(strings.TrimSpace(repo))
	if row := t.repoRow[repo]; row != nil {
		if c := t.connectionOf(row); c != nil {
			return c.InstallationID
		}
	}
	ids := t.appInstalls[repo]
	for _, id := range ids {
		for _, n := range t.nodes {
			if n.Kind == reviewKindConnection && n.InstallationID == id && n.RemovedAt == "" {
				return id
			}
		}
	}
	if len(ids) > 0 {
		return ids[0]
	}
	return 0
}

// reviewStart is the Start review dialog's request: a pull request, the types (none is its branch
// rule's), where the result goes ("" is the repository's own mode) and how much of it to read.
// Force is "Run anyway": a review of a head already reviewed with the same types is otherwise
// answered from that review, at no cost.
type reviewStart struct {
	Repo  string   `json:"repo"`
	PR    int      `json:"pr"`
	Types []string `json:"types"`
	Post  string   `json:"post"`
	Scope string   `json:"scope"`
	Force bool     `json:"force"`
	// leftOut is Run again's: the past run's types turned off since, run without and said so.
	leftOut []string
}

func (b *Bot) handleReviewStart(w http.ResponseWriter, r *http.Request) {
	var in reviewStart
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	b.startReview(w, r, in, nil)
}

// handleReviewRerun is Run again on a past review — the same types unless others are named, and the
// same destination — on the pull request's head as it is now. A type the run had that is turned off
// since is left out, and the answer says so (left_out), rather than the whole run refused for it:
// the run again is of what still runs. With every one of them off there is nothing to run again.
func (b *Bot) handleReviewRerun(w http.ResponseWriter, r *http.Request) {
	run, err := b.store.ReviewRunByPublicID(r.Context(), orgOf(r), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	if run == nil {
		reviewAPIError(w, ErrReviewRunNotFound)
		return
	}
	if run.Kind != "review" {
		bad(w, errors.New("only a review is run again; a try is tried again from its type"))
		return
	}
	var in reviewStart
	if r.ContentLength != 0 {
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
	}
	in.Repo, in.PR = run.Repo, run.PRNumber
	if len(in.Types) == 0 && len(run.Types) > 0 {
		on, err := b.reviewTypeKeys(r.Context(), orgOf(r))
		if err != nil {
			fail(w, err)
			return
		}
		for _, t := range run.Types {
			if slices.Contains(on, t.Key) {
				in.Types = append(in.Types, t.Key)
			} else {
				in.leftOut = append(in.leftOut, t.Key)
			}
		}
		if len(in.Types) == 0 {
			bad(w, fmt.Errorf("every review type this review ran is turned off now (%s): run it with other types", strings.Join(in.leftOut, ", ")))
			return
		}
	}
	if in.Post == "" {
		in.Post = reviewRunPost(run)
	}
	b.startReview(w, r, in, nil)
}

// startReview is a review somebody asked for in the console: Start review, Run again, and a type's
// Try on a PR (inline). It goes through the lane's gate like every other request (enqueueReview),
// past the filters that are for reviews nobody asked for — the "when" setting, drafts, the author
// list — and not past the connection having been added, fork policy, the money or the throttles.
// Posting live on a repository that only records needs connections.manage. A review of the head the
// pull request's last review read, with the same types under the same settings and to the same
// place, is answered from that review unless force says to run it anyway.
func (b *Bot) startReview(w http.ResponseWriter, r *http.Request, in reviewStart, inline *ReviewType) {
	ctx, orgID := r.Context(), orgOf(r)
	repo, err := reviewRepoName(in.Repo)
	switch {
	case err != nil:
		bad(w, err)
		return
	case in.PR <= 0:
		bad(w, errors.New("pr is the pull request's number"))
		return
	case in.Post != "" && in.Post != string(review.ModeLive) && in.Post != string(review.ModeShadow):
		bad(w, errors.New(`post is "live" or "shadow"`))
		return
	case in.Scope != "" && in.Scope != reviewScopeWhole && in.Scope != reviewScopeSinceLast:
		bad(w, fmt.Errorf("scope is %q or %q", reviewScopeWhole, reviewScopeSinceLast))
		return
	case b.reviewUnavailable(w), b.reviewPlanRefused(w, r):
		return
	}
	t, err := b.reviewTreeIndex(ctx, orgID)
	if err != nil {
		fail(w, err)
		return
	}
	installation := t.installationFor(repo)
	if installation == 0 {
		writeJSON(w, 404, map[string]any{"error": repo + " is not one of the organisation's repositories connected through the GitHub App"})
		return
	}
	var types []string
	for _, k := range in.Types {
		k = strings.ToLower(strings.TrimSpace(k))
		if k != "" && !slices.Contains(types, k) {
			types = append(types, k)
		}
	}
	if inline == nil && len(types) > 0 {
		keys, err := b.reviewTypeKeys(ctx, orgID)
		if err != nil {
			fail(w, err)
			return
		}
		var unknown []string
		for _, k := range types {
			if !slices.Contains(keys, k) {
				unknown = append(unknown, k)
			}
		}
		if len(unknown) > 0 {
			bad(w, fmt.Errorf("no review type %s here, or it is turned off", strings.Join(unknown, ", ")))
			return
		}
	}
	gh, err := b.reviewClient(orgID, installation, repo, in.PR)
	if err != nil {
		fail(w, err)
		return
	}
	pull, err := gh.Pull(ctx)
	if err != nil {
		reviewGitHubAnswer(w, err)
		return
	}
	if pull.Number == 0 {
		pull.Number = in.PR
	}
	post, allowLive := review.Mode(in.Post), reviewMayReach(r)
	answer := func(run *ReviewRun, same bool) {
		out := map[string]any{"run": reviewRunJSON(run), "answered_from_state": same}
		if len(in.leftOut) > 0 {
			out["left_out"] = in.leftOut
		}
		writeJSON(w, 200, out)
	}
	if inline == nil && !in.Force {
		if last, ok := b.reviewSameState(ctx, orgID, installation, repo, pull, types, post, allowLive); ok {
			answer(last, true)
			return
		}
	}
	u := adminFromCtx(ctx)
	run, err := b.enqueueReview(ctx, orgID, repo, in.PR, reviewRequest{InstallationID: installation, Trigger: "console",
		TriggerRef: "console:" + newPublicID(), RequestedBy: "console:" + cmp.Or(u.Email, u.Name), Types: types,
		Post: post, AllowLive: allowLive, Scope: in.Scope, BypassFilters: true, Full: in.Force && inline == nil,
		Pull: pull, Inline: inline})
	var skip *reviewSkip
	switch {
	case errors.As(err, &skip) && skip.Reason == "plan":
		// The plan changed between the check above and the gate: answered as the check would have.
		writeJSON(w, http.StatusPaymentRequired, map[string]any{"error": skip.Detail, "reason": skip.Reason})
		return
	case errors.As(err, &skip):
		writeJSON(w, http.StatusConflict, map[string]any{"error": skip.Detail, "reason": skip.Reason})
		return
	case errors.Is(err, errReviewLiveRefused):
		writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error(), "fields": []string{"mode"}})
		return
	case err != nil:
		reviewGitHubAnswer(w, err)
		return
	}
	details := map[string]any{"run": run.PublicID, "head": pull.Head.SHA, "types": types, "post": in.Post,
		"scope": in.Scope, "force": in.Force}
	if inline != nil {
		details["try"] = inline.Key
	}
	b.audit(r, "review.started", AuditEvent{TargetKind: "pull_request", TargetID: fmt.Sprintf("%s#%d", repo, in.PR),
		TargetName: fmt.Sprintf("%s#%d", repo, in.PR), Details: auditDetails(details)})
	answer(run, false)
}

// reviewUnavailable answers a request that has to reach GitHub on a deployment with no App to reach
// it with, and reports whether it did.
func (b *Bot) reviewUnavailable(w http.ResponseWriter) bool {
	if b.review != nil && b.proxy != nil && b.reviewApp().configured() {
		return false
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "code review is not set up on this deployment: it needs the GitHub App",
		"review_missing": b.reviewMissing()})
	return true
}

// reviewSameState finds the pull request's last finished review if a review asked for now would be
// the same one: the same head, the same types at the same versions, the same settings, posted to the
// same place. The lane would answer it from that review anyway, at claim, for nothing; saying so
// here lets the dialog say so before it is queued, and offer Run anyway.
func (b *Bot) reviewSameState(ctx context.Context, orgID, installation int64, repo string, pull *githubPull, types []string,
	post review.Mode, allowLive bool) (*ReviewRun, bool) {
	row, err := b.store.ReviewPRByNumber(ctx, orgID, repo, pull.Number)
	if err != nil || row == nil {
		return nil, false
	}
	last, err := b.store.latestReviewOutcome(ctx, orgID, row.ID)
	if err != nil || last == nil || last.HeadSHA != pull.Head.SHA {
		return nil, false
	}
	eff, s, err := b.reviewEffective(ctx, orgID, installation, repo)
	if err != nil || s != nil {
		return nil, false
	}
	plan, s, err := planReview(eff, pull, types, post, allowLive)
	if err != nil || s != nil || plan.eff.Hash() != last.ConfigHash || string(plan.post) != reviewRunPost(last) {
		return nil, false
	}
	specs, _, err := resolveReviewTypes(ctx, b.store, orgID, plan.keys)
	if err != nil || !slices.Equal(reviewRunTypes(specs), reviewChosenTypes(last)) {
		return nil, false
	}
	return last, true
}

// reviewGitHubAnswer answers an error from reading GitHub for the console: a pull request GitHub
// says is not there is a 404, GitHub asking for a wait is a 503 that says how long, and anything
// else is the server's.
func reviewGitHubAnswer(w http.ResponseWriter, err error) {
	var api *githubAPIError
	var wait *githubRetryError
	switch {
	case errors.As(err, &api) && api.Status == http.StatusNotFound:
		writeJSON(w, 404, map[string]any{"error": "GitHub has no such pull request, or the App cannot see it"})
	case errors.As(err, &api) && api.Status < 500:
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
	case errors.As(err, &wait):
		w.Header().Set("Retry-After", strconv.Itoa(int(max(wait.Wait, time.Second).Seconds())))
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
	default:
		fail(w, err)
	}
}

// ---- open pull requests and the estimate ----

// githubPullItem is one entry of GET /repos/{owner}/{repo}/pulls.
type githubPullItem struct {
	Number    int           `json:"number"`
	State     string        `json:"state"`
	Title     string        `json:"title"`
	Draft     bool          `json:"draft"`
	CreatedAt string        `json:"created_at"`
	UpdatedAt string        `json:"updated_at"`
	HTMLURL   string        `json:"html_url"`
	User      githubUser    `json:"user"`
	Head      githubPullRef `json:"head"`
	Base      githubPullRef `json:"base"`
	Labels    []githubLabel `json:"labels"`
}

// pull is the entry as the gate reads a pull request: everything the gate and the stored facts need
// is in the listing — the branches, the labels, the author, whether it is a draft or a fork — so a
// pull request found in one is not read again before it is queued. The body is not, and the run
// reads the pull request whole when it is claimed.
func (p githubPullItem) pull() *githubPull {
	return &githubPull{Number: p.Number, State: cmp.Or(p.State, "open"), Draft: p.Draft, Title: p.Title, User: p.User,
		Head: p.Head, Base: p.Base, Labels: p.Labels}
}

// reviewOpenPulls lists a repository's open pull requests, most recently updated first, up to pages
// pages of a hundred: what the Start review dialog picks from (reviewPullsPages), and what the
// catch-up compares with what it knows (review_catchup.go). It is read with review_read like
// everything else code review reads, through the installation the repository is reviewed through,
// and its URL is made here from a repository name already checked — no part of it comes from a
// request — which is the allowlist a pull request's client keeps for its own routes.
func (b *Bot) reviewOpenPulls(ctx context.Context, orgID, installation int64, repo string, pages int) ([]githubPullItem, bool, error) {
	conn, err := b.proxy.reviewConnection(installation, repo)
	if err != nil {
		return nil, false, err
	}
	acc := &Access{Rules: []Rule{{Conn: conn}}}
	base := reviewGitHubBase
	if b.review != nil {
		base = b.review.base
	}
	out := []githubPullItem{}
	for page := 1; page <= pages; page++ {
		q := url.Values{"state": {"open"}, "sort": {"updated"}, "direction": {"desc"}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
		resp, err := reviewSend(ctx, b.proxy, orgID, acc, conn, "GET", base+"/repos/"+conn.Repo+"/pulls?"+q.Encode(),
			"GET "+conn.Repo+"/pulls", "", "", proxyMaxRead)
		if err != nil {
			return out, false, err
		}
		if resp.Truncated {
			return out, true, nil
		}
		var batch []githubPullItem
		if err := json.Unmarshal([]byte(resp.Body), &batch); err != nil {
			return out, false, err
		}
		out = append(out, batch...)
		if len(batch) < 100 {
			return out, false, nil
		}
	}
	return out, true, nil
}

// handleReviewPulls is a repository's open pull requests, each with how its last review went: the
// Start review dialog's list, and a repository panel's "not reviewed yet".
func (b *Bot) handleReviewPulls(w http.ResponseWriter, r *http.Request) {
	ctx, orgID := r.Context(), orgOf(r)
	repo, err := reviewRepoName(r.URL.Query().Get("repo"))
	if err != nil {
		bad(w, err)
		return
	}
	t, err := b.reviewTreeIndex(ctx, orgID)
	if err != nil {
		fail(w, err)
		return
	}
	installation := t.installationFor(repo)
	if installation == 0 {
		writeJSON(w, 404, map[string]any{"error": repo + " is not one of the organisation's repositories connected through the GitHub App"})
		return
	}
	if b.reviewUnavailable(w) {
		return
	}
	pulls, more, err := b.reviewOpenPulls(ctx, orgID, installation, repo, reviewPullsPages)
	if err != nil {
		reviewGitHubAnswer(w, err)
		return
	}
	prs, err := b.store.ReviewPRsOfRepo(ctx, orgID, repo)
	if err != nil {
		fail(w, err)
		return
	}
	last, err := b.store.latestReviewRunsOfRepo(ctx, orgID, repo)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]map[string]any, 0, len(pulls))
	for _, p := range pulls {
		item := map[string]any{"number": p.Number, "title": p.Title, "author": p.User.Login, "base": p.Base.Ref,
			"head": p.Head.Ref, "head_sha": p.Head.SHA, "draft": p.Draft, "updated_at": p.UpdatedAt, "url": p.HTMLURL,
			"labels": githubLabelNames(p.Labels), "review": nil}
		if row := prs[p.Number]; row != nil {
			rv := map[string]any{"score": reviewScore(row.Score), "last_reviewed_sha": row.LastReviewedSHA,
				"reviewed_head": row.LastReviewedSHA != "" && row.LastReviewedSHA == p.Head.SHA, "skip_reason": row.SkipReason}
			maps.Copy(rv, reviewPausedJSON(row))
			if run := last[row.ID]; run != nil {
				rv["run"], rv["status"] = run.PublicID, run.Status
			}
			item["review"] = rv
		}
		out = append(out, item)
	}
	writeJSON(w, 200, map[string]any{"pulls": out, "more": more})
}

// reviewResumeIn is POST /api/review-pulls/resume: the pull request whose automatic reviews start again.
type reviewResumeIn struct {
	Repo string `json:"repo"`
	PR   int    `json:"pr"`
}

// handleReviewPullResume is the console's Resume on a pull request whose automatic reviews are paused
// — by themselves after reviewAutoPauseAfter, or by a member's `@… pause` — and does what `@… resume`
// does from the pull request: they start again with the count towards the next pause back at nothing,
// it is audited (review.resumed, as the person who pressed it), and the summary's footer stops saying
// paused, rendered again for nothing. reviews.manage, as a start is: resuming spends nothing by itself,
// and what the next push spends is held to the plan, the money and the throttles at the gate like any
// review nobody asked for. One that is not paused is answered so and left alone, rather than having its
// count reset by a page that was open while somebody resumed it from GitHub.
func (b *Bot) handleReviewPullResume(w http.ResponseWriter, r *http.Request) {
	ctx, orgID := r.Context(), orgOf(r)
	var in reviewResumeIn
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	repo, err := reviewRepoName(in.Repo)
	switch {
	case err != nil:
		bad(w, err)
		return
	case in.PR <= 0:
		bad(w, errors.New("pr is the pull request's number"))
		return
	}
	row, err := b.store.ReviewPRByNumber(ctx, orgID, repo, in.PR)
	if err != nil {
		fail(w, err)
		return
	}
	if row == nil {
		reviewAPIError(w, ErrReviewPRNotFound)
		return
	}
	if !row.Paused {
		writeJSON(w, 200, map[string]any{"resumed": false, "pr": reviewPausedJSON(row)})
		return
	}
	was := reviewPausedJSON(row)
	if err := b.store.resumeReviewPR(ctx, orgID, row.ID); err != nil {
		reviewAPIError(w, err)
		return
	}
	b.audit(r, "review.resumed", reviewAuditEvent(row, "", map[string]any{"paused_by": was["paused_by"], "auto_reviews": row.AutoReviews}))
	// The footer follows through the installation the repository is reviewed through now; a repository
	// with none (its connection stopped since) keeps a footer nobody reads until it is reviewed again.
	if t, err := b.reviewTreeIndex(ctx, orgID); err != nil {
		slog.Warn("code review: the summary was not queued to follow a resume", "org", orgID, "repo", repo, "pr", in.PR, "err", err)
	} else if installation := t.installationFor(repo); installation != 0 {
		if err := b.queueReviewResync(ctx, orgID, installation, row, "console", "resume:"+newPublicID()); err != nil {
			slog.Warn("code review: the summary was not queued to follow a resume", "org", orgID, "repo", repo, "pr", in.PR, "err", err)
		}
	}
	row.Paused, row.PausedAuto, row.AutoReviews = false, false, 0
	writeJSON(w, 200, map[string]any{"resumed": true, "pr": reviewPausedJSON(row)})
}

// reviewListPrice is a model's list price as the engine reads one to estimate a verification
// (reviewRun.verifyEstimate): the endpoint's own pricer when it has one, else its catalogue.
func reviewListPrice(ctx context.Context, l *LLM, model string) (modelPrice, bool) {
	if l == nil {
		return modelPrice{}, false
	}
	if l.pricer != nil {
		return l.pricer(ctx, model)
	}
	return l.priceOf(ctx, model)
}

// reviewEstimateTokens is the estimate's arithmetic: a low and a high guess at the tokens a review
// of this size reads and writes, by the shape the engine runs (review_engine.go). It is a range to
// show before somebody spends money, not a quote — the run is held to max_usd whatever it says.
//
//	diff     = (additions + deletions) × 12 + files × 150 tokens
//	           (a numbered diff line runs to about 40 characters, at 3.5 to a token; each file adds
//	           its header, its hunk headers and the head excerpt around them)
//	finder   = per type: low 4 rounds × (9k + diff) in and 2k out;
//	           high 8 rounds × (9k + 2 × diff) in and 8k out
//	           (9k is the stable half of the prompt: the rubric, the instructions, the repository map;
//	           over a pass the prompt grows by about another diff of tool results)
//	verifier = shared: low 3 candidates × (12k in, 1.5k out);
//	           high min(10, 2 + files) candidates × 2 rounds × (12k in, 1.5k out)
//
// Each bound is then priced at the model's list price and capped at max_usd, where a run stops.
func reviewEstimateTokens(files, additions, deletions, types int) (finderLow, finderHigh, verifyLow, verifyHigh Usage) {
	diff := (additions+deletions)*12 + files*150
	types = max(types, 1)
	finderLow = Usage{In: types * 4 * (reviewEstimateBase + diff), Out: types * 2_000}
	finderHigh = Usage{In: types * 8 * (reviewEstimateBase + 2*diff), Out: types * 8_000}
	verifyLow = Usage{In: 3 * 12_000, Out: 3 * 1_500}
	n := min(10, 2+files)
	verifyHigh = Usage{In: n * 2 * 12_000, Out: n * 2 * 1_500}
	return
}

// handleReviewEstimate is the Start review dialog's "about $x–$y": the pull request's size from
// GitHub, the types it would run, and the models' list prices (reviewEstimateTokens says how).
func (b *Bot) handleReviewEstimate(w http.ResponseWriter, r *http.Request) {
	ctx, orgID := r.Context(), orgOf(r)
	q := r.URL.Query()
	repo, err := reviewRepoName(q.Get("repo"))
	if err != nil {
		bad(w, err)
		return
	}
	n, _ := strconv.Atoi(q.Get("pr"))
	if n <= 0 {
		bad(w, errors.New("pr is the pull request's number"))
		return
	}
	t, err := b.reviewTreeIndex(ctx, orgID)
	if err != nil {
		fail(w, err)
		return
	}
	installation := t.installationFor(repo)
	if installation == 0 {
		writeJSON(w, 404, map[string]any{"error": repo + " is not one of the organisation's repositories connected through the GitHub App"})
		return
	}
	if b.reviewUnavailable(w) {
		return
	}
	gh, err := b.reviewClient(orgID, installation, repo, n)
	if err != nil {
		fail(w, err)
		return
	}
	pull, err := gh.Pull(ctx)
	if err != nil {
		reviewGitHubAnswer(w, err)
		return
	}
	var named []string
	for _, k := range strings.Split(q.Get("types"), ",") {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" && !slices.Contains(named, k) {
			named = append(named, k)
		}
	}
	eff, s, err := b.reviewEffective(ctx, orgID, installation, repo)
	if err != nil {
		fail(w, err)
		return
	}
	if s != nil {
		// Not reviewed under these settings: the built-in defaults still give a size to go on.
		eff = review.Resolve(nil)
	}
	keys, rule := named, ""
	settingsModel := eff.Model
	if _, matched, ok := review.MatchRule(eff.BranchRules, pull.Base.Ref, pull.Head.Ref); ok {
		applied := eff.WithRule(matched)
		settingsModel, eff.MaxUSD = applied.Model, applied.MaxUSD
		if len(keys) == 0 {
			// With what the pull request's labels add, as planReview will run it.
			add, labels := review.LabelTypes(eff.BranchRules, pull.Base.Ref, pull.Head.Ref, pull.LabelNames(), matched.Types)
			keys, rule = append(slices.Clone(matched.Types), add...), matched.String()
			for _, l := range labels {
				rule += " +label:" + oneLine(l)
			}
		}
	}
	specs, _, err := resolveReviewTypes(ctx, b.store, orgID, keys)
	if err != nil {
		fail(w, err)
		return
	}
	st := b.settings.Get(ctx, orgID)
	var l *LLM
	if b.agent != nil {
		l, _ = b.agent.llmFor(ctx, orgID)
	}
	modelOf := func(m string) string {
		// "default" is the deployment's default model by name, which the engine runs as the empty
		// model (reviewRun.resolveModel); passed through as an id it would be priced as a model called
		// "default", which no list has.
		if m = strings.TrimSpace(m); m == reviewDefaultModel {
			m = ""
		}
		m = resolveHeavy(m, st)
		if l != nil {
			return l.Servable(ctx, m)
		}
		return m
	}
	verifier := modelOf(settingsModel)
	fLow, fHigh, vLow, vHigh := reviewEstimateTokens(pull.ChangedFiles, pull.Additions, pull.Deletions, 1)
	priced := true
	var low, high float64
	price := func(model string, u Usage) float64 {
		p, ok := reviewListPrice(ctx, l, model)
		if !ok {
			priced = false
			return 0
		}
		return p.cost(u)
	}
	typeKeys := []string{}
	tokLow, tokHigh := Usage{}, Usage{}
	for _, spec := range specs {
		m := verifier
		if spec.Model != "" {
			m = modelOf(spec.Model)
		}
		low += price(m, fLow)
		high += price(m, fHigh)
		tokLow, tokHigh = addUsage(tokLow, fLow), addUsage(tokHigh, fHigh)
		typeKeys = append(typeKeys, spec.Key)
	}
	low += price(verifier, vLow)
	high += price(verifier, vHigh)
	tokLow, tokHigh = addUsage(tokLow, vLow), addUsage(tokHigh, vHigh)
	out := map[string]any{"files": pull.ChangedFiles, "additions": pull.Additions, "deletions": pull.Deletions,
		"types": typeKeys, "rule": rule, "model": verifier, "max_usd": eff.MaxUSD, "priced": priced,
		"tokens": map[string]any{"low": map[string]int{"in": tokLow.In, "out": tokLow.Out},
			"high": map[string]int{"in": tokHigh.In, "out": tokHigh.Out}},
		"usd": nil, "capped": false}
	if priced {
		out["usd"] = map[string]float64{"low": min(low, eff.MaxUSD), "high": min(high, eff.MaxUSD)}
		out["capped"] = high > eff.MaxUSD
	}
	writeJSON(w, 200, out)
}

func addUsage(a, b Usage) Usage { return Usage{In: a.In + b.In, Out: a.Out + b.Out} }
