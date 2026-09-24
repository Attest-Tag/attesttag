package app

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/storage"
	"github.com/slack-go/slack"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func bad(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
}
func fail(w http.ResponseWriter, err error) {
	if pathErr := (docPathError{}); errors.As(err, &pathErr) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	slog.Error("admin api", "err", err)
	writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
}

// toolResultPreview is how much of a tool result the activity list carries;
// the rest waits behind GET /api/tool-calls/{id}.
const toolResultPreview = 2000

// cutRunes trims s to at most n bytes without splitting a rune, and reports
// whether anything was cut.
func cutRunes(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

func pathID(r *http.Request, name string) int64 {
	id, _ := strconv.ParseInt(r.PathValue(name), 10, 64)
	return id
}

// pathUserID turns the account id in a path — the opaque users.public_id the console was given,
// never the serial — into the users.id the store joins on. An id that names nobody comes back
// as zero, and every caller then fails its own membership check, because resolving an id is not
// membership: the caller must still prove the person belongs to the organisation asking.
func (b *Bot) pathUserID(r *http.Request, name string) int64 {
	u, _ := b.store.UserByPublicID(r.Context(), r.PathValue(name))
	if u == nil {
		return 0
	}
	return u.ID
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

// artifactJSON shapes one artifact for the console: ids resolved to names, and the body only
// when a single artifact was asked for, so the index stays small.
func artifactJSON(ctx context.Context, b *Bot, a Artifact, withContent bool) map[string]any {
	m := map[string]any{
		"ID": a.ID, "Title": a.Title, "Kind": a.Kind, "Bytes": a.Bytes,
		"TeamID": a.TeamID, "TeamName": b.teamName(ctx, a.TeamID),
		"Channel": a.Channel, "ChannelName": b.channelName(ctx, a.TeamID, a.Channel),
		"ThreadTS": a.ThreadTS, "CreatedBy": a.CreatedBy,
		"CreatedByName": b.userName(ctx, a.TeamID, a.CreatedBy),
		"Permalink":     a.Permalink, "At": a.At,
	}
	if withContent {
		m["Content"] = a.Content
	}
	return m
}

// routineRunJSON shapes one run for the console. The listing previews the output at the same
// length a tool result is previewed at; the whole of it waits behind GET /api/routine-runs/{id}.
func routineRunJSON(r RoutineRun, preview bool) map[string]any {
	out, more := r.Output, false
	if preview {
		out, more = cutRunes(out, toolResultPreview)
	}
	return map[string]any{
		"ID": r.ID, "RoutineID": r.RoutineID, "Status": r.Status, "Reason": r.Reason,
		"Output": out, "More": more, "Error": r.Error, "ThreadTS": r.ThreadTS,
		"TokensIn": r.TokensIn, "TokensOut": r.TokensOut, "CostUSD": r.CostUSD,
		"StartedAt": r.StartedAt, "FinishedAt": r.FinishedAt, "MS": r.MS,
	}
}

// changed marks one organisation's configuration as changed so its running sessions and caches
// pick it up. Scoped, because a save in one organisation must not evict another's caches — with
// enough customers that would be a permanent cache miss for everybody.
func (b *Bot) changed(ctx context.Context, orgID int64) {
	b.store.BumpConfigVersion(ctx, orgID)
	b.settings.Invalidate(orgID)
	b.resolver.Invalidate(orgID)
	if b.agent != nil && b.agent.endpoints != nil {
		b.agent.endpoints.Evict(orgID)
	}
}

// connectionGone clears up after a connection whose row has just been deleted, by whichever
// route deleted it — its own, or its bundle's, which takes every connection in the bundle at
// once. The delete does none of this itself: user_connections and drive_syncs name the
// connection with no foreign key to cascade through, and the proxy and the MCP hub cache by its
// id in memory. So both routes come here rather than each keeping a list of its own; the
// bundle's once had nothing on it, and left every personal grant under its connections live.
func (b *Bot) connectionGone(ctx context.Context, orgID, connID int64) {
	// Personal sign-ins under it go too. A token nobody can see in the console any more is
	// still a live grant at the provider, so it is revoked rather than orphaned.
	b.revokeAllUnder(ctx, orgID, connID)
	// A Drive sync spends this credential and nothing else, so it goes with it rather than
	// failing every six hours against a connection that is not there any more.
	b.store.DeleteDriveSyncsForConnection(ctx, orgID, connID)
	b.proxy.forgetToken(connID)
	b.agent.mcp.forget(connID)
}

// routes registers the admin API and the embedded console.
func (b *Bot) routes(mux *http.ServeMux, uiFS fs.FS) {
	b.slackHTTPRoutes(mux)
	b.msteamsHTTPRoutes(mux)
	b.msteamsConsoleRoutes(mux)
	b.setupLinkRoutes(mux)
	b.configureRoutes(mux)
	b.installRoutes(mux)
	b.githubAppRoutes(mux)
	b.onboardingRoutes(mux)
	b.skillsRoutes(mux)
	b.oauthRoutes(mux)
	b.connectRoutes(mux)
	b.jobsRoutes(mux)
	b.apiKeyRoutes(mux)
	// The audit log's own routes (audit.go); the recording itself happens inside requireAdmin.
	b.auditRoutes(mux)
	// The public developer API. Registered beside the console's own routes and authenticated
	// completely differently: a key, never a session cookie, and no CSRF token because there is
	// no browser and no ambient credential to forge with.
	b.apiV1Routes(mux)
	b.mcpRoutes(mux)
	// The operator's API (operator.go): a bearer secret, never a session, and absent unless
	// OPERATOR_SECRET is set.
	b.operatorRoutes(mux)
	// Self-serve billing (billing.go): checkout, the card portal, and the Stripe webhook —
	// absent entirely unless both Stripe keys are set, which on most deployments they are not.
	b.billingRoutes(mux)
	// auth
	// Signed out. These are the only routes reachable without a session, so each one throttles
	// itself and none of them says whether an address has an account.
	mux.HandleFunc("POST /api/auth/signup", sameSiteOnly(b.handleSignup))
	mux.HandleFunc("POST /api/auth/login", sameSiteOnly(b.handlePasswordLogin))
	mux.HandleFunc("POST /api/auth/verify", sameSiteOnly(b.handleVerify))
	mux.HandleFunc("POST /api/auth/forgot", sameSiteOnly(b.handleForgotPassword))
	mux.HandleFunc("POST /api/auth/reset", sameSiteOnly(b.handleResetPassword))
	mux.HandleFunc("GET /api/auth/login", b.handleLogin)       // sign in with Slack
	mux.HandleFunc("GET /api/auth/callback", b.handleCallback) // ... and back
	// Sign in with Microsoft, and back (auth_microsoft.go).
	mux.HandleFunc("GET /api/auth/microsoft/login", b.handleMicrosoftLogin)
	mux.HandleFunc("GET /api/auth/microsoft/callback", b.handleMicrosoftCallback)
	// Single sign-on (auth_sso.go). Both are unauthenticated by necessity: the first is how
	// somebody with no session finds their organisation's IdP, and the second is where that IdP
	// sends them back, carrying no cookie of ours beyond the one-shot state.
	mux.HandleFunc("POST /api/auth/sso/start", sameSiteOnly(b.handleSSOStart))
	mux.HandleFunc("GET /api/auth/sso/callback/{provider}", b.handleSSOCallback)
	// Logout is a POST only: a state change reachable by a top-level GET is one any page can
	// perform on the visitor, since SameSite=Lax sends the cookie on such navigations.
	mux.HandleFunc("POST /api/auth/logout", sameSiteOnly(b.handleLogout))
	mux.HandleFunc("GET /api/me", b.handleMe)

	// Signed in: the account itself, rather than the organisation's configuration.
	mux.HandleFunc("POST /api/auth/resend-verification", b.requireAdmin(b.handleResendVerification))
	mux.HandleFunc("POST /api/auth/password", b.requireAdmin(b.handleChangePassword))
	mux.HandleFunc("POST /api/auth/two-factor", b.handleTwoFactorLogin) // finishes a sign-in that owed a code
	mux.HandleFunc("GET /api/auth/invite", b.requireAdmin(b.handleInvitePreview))
	mux.HandleFunc("POST /api/auth/accept-invite", b.requireAdmin(b.handleAcceptInvite))
	// Your own account: profile, and the second factor behind your password. Never anybody
	// else's — managing other members is console_users.go, where it is a permission.
	mux.HandleFunc("GET /api/account", b.requireAdmin(b.handleAccount))
	mux.HandleFunc("PUT /api/account", b.requireAdmin(b.handleUpdateAccount))
	mux.HandleFunc("POST /api/account/totp/start", b.requireAdmin(b.handleTOTPStart))
	mux.HandleFunc("POST /api/account/totp/confirm", b.requireAdmin(b.handleTOTPConfirm))
	mux.HandleFunc("POST /api/account/totp/recovery", b.requireAdmin(b.handleTOTPRecovery))
	mux.HandleFunc("POST /api/account/totp/disable", b.requireAdmin(b.handleTOTPDisable))
	mux.HandleFunc("POST /api/auth/switch-org", b.requireAdmin(b.handleSwitchOrg))
	mux.HandleFunc("GET /api/auth/identities", b.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		me := adminFromCtx(r.Context())
		ids, err := b.store.Identities(r.Context(), me.ID)
		if err != nil {
			fail(w, err)
			return
		}
		acct, _ := b.store.User(r.Context(), me.ID)
		writeJSON(w, 200, map[string]any{"identities": ids, "has_password": acct.HasPassword(),
			"email_verified": acct != nil && acct.EmailVerified, "email_configured": b.mail.Configured()})
	}))
	mux.HandleFunc("DELETE /api/auth/identities/{provider}/{subject}", b.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		me := adminFromCtx(r.Context())
		acct, _ := b.store.User(r.Context(), me.ID)
		ids, _ := b.store.Identities(r.Context(), me.ID)
		// Never leave an account with no way back into it.
		if len(ids) <= 1 || (r.PathValue("provider") == ProviderPassword && !acct.HasPassword()) {
			writeJSON(w, 409, map[string]any{"error": "That is the only way you can sign in. Add another first."})
			return
		}
		if err := b.store.RemoveIdentity(r.Context(), me.ID, r.PathValue("provider"), r.PathValue("subject")); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	}))

	// The organisation this session is acting in. Reading what it is called and who made it is
	// every member's; renaming it is a permission; deleting it is neither — see org_delete.go,
	// where the rule is ownership rather than a role.
	mux.HandleFunc("GET /api/org", b.requireAdmin(b.handleOrg))
	mux.HandleFunc("POST /api/org/delete", b.requireAdmin(b.handleDeleteOrg))
	mux.HandleFunc("PUT /api/org", b.requirePerm(PermSettingsManage, func(w http.ResponseWriter, r *http.Request) {
		me := adminFromCtx(r.Context())
		var in struct {
			Name string `json:"name"`
		}
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		if err := b.store.RenameOrg(r.Context(), me.OrgID, in.Name); err != nil {
			writeJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
		b.audit(r, "org.renamed", AuditEvent{TargetKind: "org", TargetID: me.OrgPublic, TargetName: strings.TrimSpace(in.Name),
			Details: auditDetails(map[string]any{"from": me.OrgName})})
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	a := b.requireAdmin

	// overview + settings
	mux.HandleFunc("GET /api/overview", a(func(w http.ResponseWriter, r *http.Request) {
		o := b.store.OverviewStats(r.Context(), orgOf(r))
		names := b.conversationNamer(r.Context(), orgOf(r))
		for i, u := range o.TopChannels {
			o.TopChannels[i].ChannelName = names.usageName(r.Context(), u.TeamID, u.Channel)
		}
		// This organisation's workspaces, not the deployment's. ActiveTeams sweeps every tenant
		// — it exists for the socket loop and the schedulers — so reading teams[0] here put
		// another tenant's workspace name and bot user id on this console's front page.
		if teams, _ := b.store.Teams(r.Context(), orgOf(r)); len(teams) > 0 {
			active := teams[:0]
			for _, t := range teams {
				if t.Status == "active" {
					active = append(active, t)
				}
			}
			if len(active) > 0 {
				o.Bot, o.Team = active[0].BotUserID, active[0].Name
				if len(active) > 1 {
					o.Team = fmt.Sprintf("%d workspaces", len(active))
				}
			}
		}
		st := b.settings.Get(r.Context(), orgOf(r))
		o.Budget, o.Plan = st.EffectiveBudget(), st.Plan
		// 30 days, because that is the window the plan size is judged over: the chart a reader
		// scrolls past is the same month the billing tile is arguing about.
		charts := b.store.OverviewChartData(r.Context(), orgOf(r), 30)
		o.Charts = &charts
		writeJSON(w, 200, o)
	}))
	mux.HandleFunc("GET /api/settings", a(func(w http.ResponseWriter, r *http.Request) {
		kv, _ := b.store.AllSettings(r.Context(), orgOf(r))
		defaultDomains := allowedEmailDomains()
		if defaultDomains == nil {
			defaultDomains = []string{}
		}
		// What every member may know: the engines on offer and how Slack sign-in is set up. An
		// organisation's settings manager also sees the functional shape of the deployment — the
		// LLM endpoint, how the worker is wired, and which secrets are present (as booleans). The
		// raw infrastructure identifiers (database path, docs bucket, GCP project/region/job name)
		// are deliberately not returned: the console never renders them, and an admin session
		// should not carry facts that only aid reconnaissance if the session is ever compromised.
		env := map[string]any{
			"worker":        map[string]any{"mode": b.cfg.WorkerMode, "platform": b.cfg.WorkerDispatcher(), "engines": workerEngines},
			"slack_login":   map[string]any{"configured": slackLoginConfigured(), "redirect_url": b.callbackURL(r)},
			"web_providers": webProviders,
			"support_email": b.cfg.SupportEmail,
			// The console draws a banner from this. Everyone sees it, not only an admin: during
			// a freeze the bot is not answering anybody, and the alternative to saying so is a
			// room full of people who think it is broken.
			"maintenance": b.cfg.Maintenance,
			// Whether this deployment sells plans. Top level rather than inside `secrets`
			// below: that map is only filled in for settings.manage holders, and the Billing
			// tab is gated on billing.manage — put the flag there and the tab disappears for
			// exactly the people who are allowed to use it.
			"billing_enabled": b.cfg.BillingEnabled(),
			// ALLOWED_EMAIL_DOMAINS: the list an organisation that keeps none of its own is held
			// to. Security needs it to say what an empty list means, which is not "everybody"
			// wherever it is set. Nothing a member did not know: with no list stored, it is the
			// effective one above.
			"default_email_domains": defaultDomains,
		}
		if adminFromCtx(r.Context()).Permissions[PermSettingsManage] {
			env["llm_base_url"] = b.cfg.LLMBaseURL
			env["worker"] = map[string]any{"mode": b.cfg.WorkerMode, "platform": b.cfg.WorkerDispatcher(),
				"require_oidc": b.cfg.WorkerRequireOIDC, "engines": workerEngines, "provisioning_key": b.cfg.OpenRouterProvisioningKey != "", "engine_key": b.cfg.WorkerEngineAPIKey != ""}
			env["secrets"] = map[string]bool{"SLACK_SIGNING_SECRET": b.cfg.SlackSigningSecret != "", "OPENROUTER_API_KEY": b.cfg.LLMKey != "",
				"MASTER_KEY": os.Getenv("MASTER_KEY") != "", "RESEND_API_KEY": os.Getenv("RESEND_API_KEY") != "", "SLACK_CLIENT_ID": os.Getenv("SLACK_CLIENT_ID") != "", "SLACK_CLIENT_SECRET": os.Getenv("SLACK_CLIENT_SECRET") != ""}
		}
		writeJSON(w, 200, map[string]any{"effective": b.settings.Get(r.Context(), orgOf(r)), "stored": kv, "env": env})
	}))
	mux.HandleFunc("PUT /api/settings", b.requirePerm(PermSettingsManage, func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		allowed := map[string]bool{}
		for _, k := range settingKeys {
			allowed[k] = true
		}
		for k, v := range in {
			if !allowed[k] || k == "config_version" {
				bad(w, fmt.Errorf("unknown setting %q", k))
				return
			}
			if k == "allow_rules" {
				rules, err := parseAllowRules(v)
				if err != nil {
					bad(w, err)
					return
				}
				raw, _ := json.Marshal(rules)
				v = string(raw)
			}
			if strings.HasPrefix(k, "worker_") {
				if err := validateWorkerSetting(k, v); err != nil {
					bad(w, err)
					return
				}
			}
			if strings.HasPrefix(k, "web_") {
				if err := validateWebSetting(k, v); err != nil {
					bad(w, err)
					return
				}
			}
			if err := validateModelSetting(k, v); err != nil {
				bad(w, err)
				return
			}
			if k == "monthly_budget_usd" {
				if err := b.validateBudget(r.Context(), orgOf(r), v); err != nil {
					bad(w, err)
					return
				}
			}
			if err := validateSecuritySetting(k, v); err != nil {
				bad(w, err)
				return
			}
			// The two settings that can lock their own author out of the console. Refused here
			// rather than warned about: an admin who reads "Slack only" as "for everybody else"
			// finds out at their next sign-in, when there is nobody left who can undo it.
			if k == "auth_policy" || k == "require_two_factor" {
				if err := b.selfLockout(r.Context(), adminFromCtx(r.Context()), k, v); err != nil {
					writeJSON(w, 409, map[string]any{"error": err.Error()})
					return
				}
			}
			in[k] = v
		}
		if err := b.store.PutSettings(r.Context(), orgOf(r), in); err != nil {
			fail(w, err)
			return
		}
		b.changed(r.Context(), orgOf(r))
		// Which keys and what they became. Nothing here is a secret — the web key has a route
		// of its own for exactly that reason — but an allow-rule list can be long, so values are
		// cut rather than copied whole.
		changed := map[string]any{}
		for k, v := range in {
			changed[k], _ = cutRunes(v, 200)
		}
		b.audit(r, "settings.updated", AuditEvent{TargetKind: "settings", TargetName: strings.Join(slices.Sorted(maps.Keys(in)), ", "),
			Details: auditDetails(map[string]any{"changed": changed})})
		writeJSON(w, 200, b.settings.Get(r.Context(), orgOf(r)))
	}))
	// The web provider's key. Not part of PUT /api/settings: that endpoint round-trips every
	// value through the console, and a key that comes back to a browser is a key in a log, a
	// screenshot and a bug report. It goes in and is never read out.
	mux.HandleFunc("PUT /api/settings/web-key", b.requirePerm(PermSettingsManage, b.handleWebKey))
	mux.HandleFunc("GET /api/settings/sso", b.requirePerm(PermSettingsManage, b.handleSSOGet))
	mux.HandleFunc("POST /api/settings/sso", b.requirePerm(PermSettingsManage, b.handleSSORegister))
	mux.HandleFunc("POST /api/settings/sso/verify", b.requirePerm(PermSettingsManage, b.handleSSOVerify))
	mux.HandleFunc("DELETE /api/settings/sso", b.requirePerm(PermSettingsManage, b.handleSSODelete))
	mux.HandleFunc("DELETE /api/settings/web-key", b.requirePerm(PermSettingsManage, b.handleWebKeyDelete))
	mux.HandleFunc("POST /api/settings/web-key/test", b.requirePerm(PermSettingsManage, b.handleWebKeyTest))
	// The organisation's own model key, on routes of its own for the same reason as the web key,
	// with more riding on it: where every conversation is sent (model_keys_api.go).
	b.modelKeyRoutes(mux)
	// The button under a free account's monthly budget: the server writes the support request
	// and sends it, so the operator link that answers it never passes through the account
	// asking. Same permission as the field it is asking to open.
	mux.HandleFunc("POST /api/plan/raise-budget", b.requirePerm(PermSettingsManage, b.handleRaiseBudgetRequest))
	// The help button in the header: a question typed in the console, mailed to support the same
	// way. Any signed-in member, because asking changes nothing (support_request.go).
	mux.HandleFunc("POST /api/support/message", b.requireAdmin(b.handleHelpRequest))

	// Trying a channel spends the account's money and reaches real systems with the channel's
	// own credentials, so it is gated on the permission that decides what those settings are:
	// whoever may change a channel's configuration may find out what it does.
	mux.HandleFunc("POST /api/playground", b.requirePerm(PermScopesManage, b.handlePlayground))
	mux.HandleFunc("POST /api/playground/reset", b.requirePerm(PermScopesManage, b.handlePlaygroundReset))

	// The console assistant. Any signed-in member, because the permission that matters is on
	// each tool rather than on the door: read_console offers exactly the resources this person's
	// own console routes would let them open, and each propose_ tool is withheld unless they
	// hold the permission its endpoint requires. One gate here instead would have to be the
	// widest of those, which would hand a viewer the audit log, or the narrowest, which would
	// take the whole panel away from somebody who only wanted to ask what a page meant.
	//
	// What that does grant everybody is the ability to spend tokens. That is bounded by the
	// per-person hourly limit, the per-organisation in-flight cap and the account's budget,
	// all of which this handler checks before it calls a model.
	mux.HandleFunc("POST /api/assistant", b.requireAdmin(b.handleAssistant))
	// The transcript, on Activity beside the turns and tool calls it produced. activity.view,
	// because that is what the rest of that page needs — and what somebody asked the assistant
	// is no less revealing than the turns it took afterwards.
	mux.HandleFunc("GET /api/assistant/turns", b.requirePerm(PermActivityView, func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		ts, err := b.store.AssistantTurns(r.Context(), orgOf(r), r.URL.Query().Get("q"), limit)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, ts)
	}))

	mux.HandleFunc("GET /api/presets", a(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, Presets) }))
	// The provider's model list for the console's model pickers; ?refresh=1 skips the cache. It
	// is the organisation's own endpoint's list when it brought one, because that is where a model
	// picked from it will be asked for. source is the host, which is what a person knows an
	// endpoint by; own says which of the two it is.
	mux.HandleFunc("GET /api/models", a(func(w http.ResponseWriter, r *http.Request) {
		l, err := b.agent.llmFor(r.Context(), orgOf(r))
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
			return
		}
		refresh := r.URL.Query().Get("refresh") == "1"
		if refresh {
			if ok, _ := modelRefreshes.allow("models-refresh:"+adminFromCtx(r.Context()).OrgPublic, modelRefreshesPerOrg, 10*time.Minute); !ok {
				refresh = false
			}
		}
		models, err := l.ListModels(r.Context(), refresh)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		// complete says a model missing from the list is one the endpoint does not serve, which is
		// when the console may mark a saved pick as not on it.
		writeJSON(w, 200, map[string]any{"models": models, "source": l.host, "own": l.own, "complete": l.catalogueComplete()})
	}))

	// scopes
	mux.HandleFunc("GET /api/scopes", a(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sync") != "0" {
			b.syncScopesOrg(r.Context(), orgOf(r))
		}
		sc, err := b.store.Scopes(r.Context(), orgOf(r))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, b.nameTeams(r.Context(), sc))
	}))
	mux.HandleFunc("GET /api/scopes/{id}", a(func(w http.ResponseWriter, r *http.Request) {
		sc, err := b.store.ScopeByID(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil || sc == nil {
			writeJSON(w, 404, map[string]any{"error": "no such scope"})
			return
		}
		sc.TeamName = b.teamName(r.Context(), sc.TeamID)
		acc, _ := b.resolver.Resolve(r.Context(), orgOf(r), sc.TeamID, sc.SlackID, -1)
		effective := ""
		if acc != nil {
			effective = acc.DefaultRepo
		}
		out := map[string]any{"scope": sc, "default_repo_effective": effective}
		// The connection names, hosts, credential types and write modes this channel reaches are
		// what connections.view gates — the console assistant withholds exactly these without it.
		// A member on a custom role that omits the permission sees the scope, not what it can spend.
		if adminFromCtx(r.Context()).Permissions[PermConnView] {
			out["access"] = b.accessSummary(r.Context(), orgOf(r), sc, acc)
			out["repos"] = b.repoSummary(r.Context(), orgOf(r), sc, acc)
			out["inherited"] = b.inheritedSummary(r.Context(), orgOf(r), sc)
		}
		writeJSON(w, 200, out)
	}))
	// A Configure link, once printed in a channel, is out of the organisation's hands — a
	// screenshot keeps it. Revoking bumps the scope's epoch: every link minted so far stops
	// opening the page, and the next reply's footer carries a fresh one.
	mux.HandleFunc("POST /api/scopes/{id}/configure-links/revoke", b.requirePerm(PermScopesManage, func(w http.ResponseWriter, r *http.Request) {
		sc, err := b.store.ScopeByID(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil || sc == nil {
			writeJSON(w, 404, map[string]any{"error": "no such scope"})
			return
		}
		if err := b.store.BumpLinkEpoch(r.Context(), orgOf(r), sc.ID); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	// Removing a channel is the bot leaving it in Slack. Nothing here is deleted: the channel,
	// its history and everything configured for it stay exactly as they are, and the row is only
	// marked so the rail stops listing a channel the bot cannot reach. Invite the bot back and
	// the next scope sync brings the channel — and its settings — straight back.
	mux.HandleFunc("POST /api/scopes/{id}/leave", b.requirePerm(PermScopesManage, func(w http.ResponseWriter, r *http.Request) {
		sc, err := b.store.ScopeByID(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil || sc == nil || sc.Kind != "channel" {
			// A workspace or the account answers the same as a scope that does not exist: there
			// is no channel to leave either way, and neither is this tenant's business to probe.
			writeJSON(w, 404, map[string]any{"error": "no such channel"})
			return
		}
		sl, err := b.slacks.For(r.Context(), sc.TeamID)
		if err != nil {
			writeJSON(w, 409, map[string]any{"error": "That workspace is not connected, so the bot cannot leave anything in it."})
			return
		}
		api, err := sl.slackAPI()
		if err != nil {
			writeJSON(w, 409, map[string]any{"error": "Only a Slack workspace can be left from here."})
			return
		}
		if _, err := api.LeaveConversationContext(r.Context(), sc.SlackID); err != nil {
			switch {
			case strings.Contains(err.Error(), "missing_scope"):
				writeJSON(w, 409, map[string]any{"error": "This workspace was connected before the bot could leave channels. " +
					"Reconnect it from the workspace above to grant channels:leave, then try again."})
				return
			case strings.Contains(err.Error(), "not_in_channel"),
				strings.Contains(err.Error(), "channel_not_found"),
				strings.Contains(err.Error(), "is_archived"):
				// Already out, or the channel is gone or archived. The console asked for the
				// end state, not for the call, and the end state is what it gets.
			default:
				writeJSON(w, 502, map[string]any{"error": "Slack refused: " + err.Error()})
				return
			}
		}
		if err := b.store.MarkScopeLeft(r.Context(), orgOf(r), sc.ID); err != nil {
			fail(w, err)
			return
		}
		b.deferScopeSync(sc.TeamID)
		b.changed(r.Context(), orgOf(r))
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	mux.HandleFunc("PUT /api/scopes/{id}", b.requirePerm(PermScopesManage, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Instructions, DefaultModel, MemberEdits string `json:",omitempty"`
		}
		var raw map[string]string
		if err := decode(r, &raw); err != nil {
			bad(w, err)
			return
		}
		sc, err := b.store.ScopeByID(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil || sc == nil {
			writeJSON(w, 404, map[string]any{"error": "no such scope"})
			return
		}
		// Every posted field is checked before the first one is written. The setters below run
		// one at a time and several return early on a bad value, so a form carrying a good
		// read_all and a bad max_tool_rounds used to store the first and then 400 — leaving
		// half a change applied and the page showing the other half. Sorted so a request with
		// two bad fields names the same one every time.
		keys := make([]string, 0, len(raw))
		for k := range raw {
			keys = append(keys, k)
		}
		sortStrings(keys)
		for _, k := range keys {
			if err := b.scopeFieldError(r.Context(), orgOf(r), sc, k, raw[k]); err != nil {
				bad(w, err)
				return
			}
		}
		in.Instructions, in.DefaultModel, in.MemberEdits = sc.Instructions, sc.DefaultModel, sc.MemberEdits
		if v, ok := raw["instructions"]; ok {
			in.Instructions = v
		}
		if v, ok := raw["default_model"]; ok {
			in.DefaultModel = v
		}
		if v, ok := raw["member_edits"]; ok {
			in.MemberEdits = v
		}
		if v, ok := raw["read_all"]; ok {
			if err := b.store.SetScopeReadAll(r.Context(), orgOf(r), sc.ID, v); err != nil {
				fail(w, err)
				return
			}
		}
		// Switching email intake on is an admin's decision and only an admin's: the channel's
		// Configure page can turn it off and never on, because a mail forwarded here starts a
		// turn nobody in the workspace asked for. PermScopesManage on this handler is that rule.
		if v, ok := raw["email_intake"]; ok {
			if err := b.store.SetScopeEmailIntake(r.Context(), orgOf(r), sc.ID, v); err != nil {
				fail(w, err)
				return
			}
		}
		// And whether the allow rules apply on that lane, which is the second half of the same
		// decision and deliberately not folded into the first: a channel can take mail and still
		// hold every write, which is what it does until this is switched on.
		if v, ok := raw["email_auto_writes"]; ok {
			if err := b.store.SetScopeEmailAutoWrites(r.Context(), orgOf(r), sc.ID, v); err != nil {
				fail(w, err)
				return
			}
		}
		if v, ok := raw["monthly_budget_usd"]; ok {
			f, _ := strconv.ParseFloat(v, 64)
			b.store.SetScopeBudget(r.Context(), orgOf(r), sc.ID, f)
		}
		if v, ok := raw["max_tool_rounds"]; ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if strings.TrimSpace(v) == "" {
				n, err = 0, nil // an emptied field is "inherit", like every other scope setting
			}
			if err != nil || n < 0 {
				bad(w, errors.New("max_tool_rounds must be a whole number; 0 inherits"))
				return
			}
			if err := b.store.SetScopeMaxToolRounds(r.Context(), orgOf(r), sc.ID, n); err != nil {
				fail(w, err)
				return
			}
		}
		if v, ok := raw["allow_rules"]; ok {
			rules, err := parseAllowRules(v)
			if err != nil {
				bad(w, err)
				return
			}
			if err := b.store.SetScopeAllowRules(r.Context(), orgOf(r), sc.ID, rules); err != nil {
				fail(w, err)
				return
			}
		}
		if v, ok := raw["default_repo"]; ok {
			repo, err := normalizeRepo(v)
			if err != nil {
				bad(w, err)
				return
			}
			if repo != "" {
				acc, _ := b.resolver.Resolve(r.Context(), orgOf(r), sc.TeamID, sc.SlackID, -1)
				if acc == nil || !acc.hasRepo(repo) {
					bad(w, fmt.Errorf("%s is not connected here; connect it first", repo))
					return
				}
			}
			if err := b.store.SetScopeDefaultRepo(r.Context(), orgOf(r), sc.ID, repo); err != nil {
				fail(w, err)
				return
			}
		}
		if err := b.store.UpdateScope(r.Context(), orgOf(r), sc.ID, in.Instructions, in.DefaultModel, in.MemberEdits); err != nil {
			fail(w, err)
			return
		}
		b.changed(r.Context(), orgOf(r))
		// The fields the request carried, with their new values — except the instructions,
		// which are prose and are recorded by length so the row stays a row.
		changed := map[string]any{}
		for k, v := range raw {
			if k == "instructions" {
				changed["instructions_chars"] = len(v)
				continue
			}
			changed[k], _ = cutRunes(v, 200)
		}
		b.audit(r, "scope.updated", AuditEvent{TeamID: sc.TeamID, TargetKind: "scope", TargetID: strconv.FormatInt(sc.ID, 10),
			TargetName: sc.Name, Details: auditDetails(map[string]any{"changed": changed})})
		sc, _ = b.store.ScopeByID(r.Context(), orgOf(r), sc.ID)
		writeJSON(w, 200, sc)
	}))
	mux.HandleFunc("POST /api/scopes/{id}/bundles/{bid}", b.requirePerm(PermScopesManage, func(w http.ResponseWriter, r *http.Request) {
		if err := b.store.AttachBundle(r.Context(), orgOf(r), pathID(r, "id"), pathID(r, "bid")); err != nil {
			fail(w, err)
			return
		}
		b.changed(r.Context(), orgOf(r))
		b.auditGrant(r, "scope.bundle_attached", pathID(r, "id"), "bundle_id", pathID(r, "bid"))
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	mux.HandleFunc("DELETE /api/scopes/{id}/bundles/{bid}", b.requirePerm(PermScopesManage, func(w http.ResponseWriter, r *http.Request) {
		if err := b.store.DetachBundle(r.Context(), orgOf(r), pathID(r, "id"), pathID(r, "bid")); err != nil {
			fail(w, err)
			return
		}
		b.changed(r.Context(), orgOf(r))
		b.auditGrant(r, "scope.bundle_detached", pathID(r, "id"), "bundle_id", pathID(r, "bid"))
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	// one-off grants: a single connection (bundle/name) attached without the rest of its bundle
	// What can this token see? Asked before anything is stored, so the console can offer the
	// repositories rather than have someone type a name it will then fail to find.
	mux.HandleFunc("POST /api/github/repos", b.requirePerm(PermConnManage, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Token          string
			ConnectionID   int64 `json:"connection_id"`
			InstallationID int64 `json:"installation_id"`
		}
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		auth, err := b.repoAuthFor(ctx, orgOf(r), in.Token, in.ConnectionID, in.InstallationID)
		if err != nil {
			bad(w, err)
			return
		}
		// An installation lists exactly what its account's admin ticked; a token lists whatever
		// it happens to reach. Different endpoints, same answer shape to the console.
		var repos []githubRepo
		var truncated bool
		if auth.installationID > 0 {
			repos, truncated, err = b.reposForInstall(ctx, orgOf(r), auth.installationID)
		} else {
			repos, truncated, err = b.reposForToken(ctx, orgOf(r), auth.token)
		}
		if err != nil {
			bad(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"repos": repos, "truncated": truncated})
	}))
	// Connect GitHub repositories with an access token: each checked against GitHub, the token
	// stored sealed under the Repositories bundle, one connection per repository attached to
	// this scope on its own. Takes `repos` (picked from the list above) or a single `repo`; or
	// `connection_ids` of repositories already saved, which are attached here as they are.
	mux.HandleFunc("POST /api/scopes/{id}/repos", b.requirePerm(PermConnManage, func(w http.ResponseWriter, r *http.Request) {
		sc, err := b.store.ScopeByID(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil || sc == nil {
			writeJSON(w, 404, map[string]any{"error": "no such scope"})
			return
		}
		b.handleConnectRepos(w, r, sc)
	}))
	// The same without a scope: the Bundles page saves repositories under Repositories and
	// attaches them nowhere, so a channel is not handed every repository the account has. A
	// channel adds one from its own page.
	mux.HandleFunc("POST /api/repos", b.requirePerm(PermConnManage, func(w http.ResponseWriter, r *http.Request) {
		b.handleConnectRepos(w, r, nil)
	}))
	mux.HandleFunc("POST /api/scopes/{id}/connections/{cid}", b.requirePerm(PermScopesManage, func(w http.ResponseWriter, r *http.Request) {
		sc, err := b.store.ScopeByID(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil || sc == nil {
			writeJSON(w, 404, map[string]any{"error": "no such scope"})
			return
		}
		c, err := b.store.Connection(r.Context(), orgOf(r), pathID(r, "cid"))
		if err != nil || c == nil {
			writeJSON(w, 404, map[string]any{"error": "no such connection"})
			return
		}
		if err := b.store.AttachConnection(r.Context(), orgOf(r), sc.ID, c.ID); err != nil {
			fail(w, err)
			return
		}
		b.changed(r.Context(), orgOf(r))
		b.auditGrant(r, "scope.connection_attached", sc.ID, "connection_id", c.ID)
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	mux.HandleFunc("DELETE /api/scopes/{id}/connections/{cid}", b.requirePerm(PermScopesManage, func(w http.ResponseWriter, r *http.Request) {
		if err := b.store.DetachConnection(r.Context(), orgOf(r), pathID(r, "id"), pathID(r, "cid")); err != nil {
			fail(w, err)
			return
		}
		b.changed(r.Context(), orgOf(r))
		b.auditGrant(r, "scope.connection_detached", pathID(r, "id"), "connection_id", pathID(r, "cid"))
		writeJSON(w, 200, map[string]any{"ok": true})
	}))

	// bundles
	mux.HandleFunc("GET /api/bundles", b.requirePerm(PermConnView, func(w http.ResponseWriter, r *http.Request) {
		bs, err := b.store.Bundles(r.Context(), orgOf(r))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, bs)
	}))
	mux.HandleFunc("POST /api/bundles", b.requirePerm(PermBundlesManage, func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Name string }
		if err := decode(r, &in); err != nil || strings.TrimSpace(in.Name) == "" {
			bad(w, fmt.Errorf("name is required"))
			return
		}
		bd, err := b.store.CreateBundle(r.Context(), orgOf(r), strings.TrimSpace(in.Name), adminFromCtx(r.Context()).UserID)
		if err != nil {
			fail(w, err)
			return
		}
		b.changed(r.Context(), orgOf(r))
		b.audit(r, "bundle.created", AuditEvent{TargetKind: "bundle", TargetID: strconv.FormatInt(bd.ID, 10), TargetName: bd.Name})
		writeJSON(w, 201, bd)
	}))
	mux.HandleFunc("GET /api/bundles/{id}", b.requirePerm(PermConnView, func(w http.ResponseWriter, r *http.Request) {
		bd, err := b.store.Bundle(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil || bd == nil {
			writeJSON(w, 404, map[string]any{"error": "no such bundle"})
			return
		}
		writeJSON(w, 200, bd)
	}))
	mux.HandleFunc("PUT /api/bundles/{id}", b.requirePerm(PermBundlesManage, func(w http.ResponseWriter, r *http.Request) {
		bd, err := b.store.Bundle(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil || bd == nil {
			writeJSON(w, 404, map[string]any{"error": "no such bundle"})
			return
		}
		var in struct {
			Name         *string   `json:"name"`
			Instructions *string   `json:"instructions"`
			ToolPacks    *[]string `json:"tool_packs"`
		}
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		if in.Name != nil {
			bd.Name = *in.Name
		}
		if in.Instructions != nil {
			bd.Instructions = *in.Instructions
		}
		if in.ToolPacks != nil {
			bd.ToolPacks = *in.ToolPacks
		}
		if err := b.store.UpdateBundle(r.Context(), orgOf(r), bd.ID, bd.Name, bd.Instructions, bd.ToolPacks); err != nil {
			fail(w, err)
			return
		}
		b.changed(r.Context(), orgOf(r))
		b.audit(r, "bundle.updated", AuditEvent{TargetKind: "bundle", TargetID: strconv.FormatInt(bd.ID, 10), TargetName: bd.Name,
			Details: auditDetails(map[string]any{"name": in.Name != nil, "instructions": in.Instructions != nil, "tool_packs": in.ToolPacks != nil})})
		bd, _ = b.store.Bundle(r.Context(), orgOf(r), bd.ID)
		writeJSON(w, 200, bd)
	}))
	mux.HandleFunc("DELETE /api/bundles/{id}", b.requirePerm(PermBundlesManage, func(w http.ResponseWriter, r *http.Request) {
		// Named before it goes: the row is the only place the name survives.
		bd, _ := b.store.Bundle(r.Context(), orgOf(r), pathID(r, "id"))
		// Its connections are listed first, for the same reason: the delete takes every one of
		// them, and once their rows are gone nothing records which connections this bundle
		// held. Unlike the name, the list is not optional — a bundle whose connections cannot
		// be read is left alone, since deleting it would orphan the very grants this is for.
		conns, err := b.store.ConnectionIDsForBundle(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil {
			fail(w, err)
			return
		}
		if err := b.store.DeleteBundle(r.Context(), orgOf(r), pathID(r, "id")); err != nil {
			fail(w, err)
			return
		}
		for _, id := range conns {
			b.connectionGone(r.Context(), orgOf(r), id)
		}
		b.changed(r.Context(), orgOf(r))
		name := ""
		if bd != nil {
			name = bd.Name
		}
		b.audit(r, "bundle.deleted", AuditEvent{TargetKind: "bundle", TargetID: r.PathValue("id"), TargetName: name})
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	mux.HandleFunc("POST /api/bundles/{id}/domains", b.requirePerm(PermBundlesManage, func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Host, Ports string }
		if err := decode(r, &in); err != nil || strings.TrimSpace(in.Host) == "" || in.Host == "*" {
			bad(w, fmt.Errorf("a hostname is required (wildcard only as the leftmost label)"))
			return
		}
		id, err := b.store.AddDomain(r.Context(), orgOf(r), pathID(r, "id"), strings.TrimSpace(in.Host), in.Ports)
		if err != nil {
			fail(w, err)
			return
		}
		b.changed(r.Context(), orgOf(r))
		// A domain is where a credential may be sent, which makes adding one a security change.
		b.audit(r, "domain.added", AuditEvent{TargetKind: "domain", TargetID: strconv.FormatInt(id, 10), TargetName: strings.TrimSpace(in.Host),
			Details: auditDetails(map[string]any{"bundle_id": pathID(r, "id"), "ports": in.Ports})})
		writeJSON(w, 201, map[string]any{"id": id})
	}))
	mux.HandleFunc("DELETE /api/domains/{id}", b.requirePerm(PermBundlesManage, func(w http.ResponseWriter, r *http.Request) {
		if err := b.store.DeleteDomain(r.Context(), orgOf(r), pathID(r, "id")); err != nil {
			fail(w, err)
			return
		}
		b.changed(r.Context(), orgOf(r))
		b.audit(r, "domain.removed", AuditEvent{TargetKind: "domain", TargetID: r.PathValue("id")})
		writeJSON(w, 200, map[string]any{"ok": true})
	}))

	// connections
	mux.HandleFunc("POST /api/bundles/{id}/connections", b.requirePerm(PermConnManage, func(w http.ResponseWriter, r *http.Request) {
		var in connectionInput
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		in.BundleID = pathID(r, "id")
		if err := refuseInstallFromBody(&in, nil); err != nil {
			bad(w, err)
			return
		}
		c, sec, err := b.buildConnection(&in, nil)
		if err != nil {
			bad(w, err)
			return
		}
		c.CreatedBy = adminFromCtx(r.Context()).UserID
		enc, err := b.sealSecret(sec)
		if err != nil {
			fail(w, err)
			return
		}
		id, err := b.store.InsertConnection(r.Context(), orgOf(r), c, enc)
		if err != nil {
			fail(w, err)
			return
		}
		b.changed(r.Context(), orgOf(r))
		// The shape of the credential, never the credential: preset, type and the hosts it may
		// reach are what somebody reviewing this later needs to see.
		b.audit(r, "connection.created", AuditEvent{TargetKind: "connection", TargetID: strconv.FormatInt(id, 10), TargetName: c.Name,
			Details: auditDetails(map[string]any{"bundle_id": c.BundleID, "preset": c.Preset, "cred_type": c.CredType, "allowed_hosts": c.AllowedHosts})})
		c, _ = b.store.Connection(r.Context(), orgOf(r), id)
		writeJSON(w, 201, c)
	}))
	mux.HandleFunc("PUT /api/connections/{id}", b.requirePerm(PermConnManage, func(w http.ResponseWriter, r *http.Request) {
		cur, err := b.store.Connection(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil || cur == nil {
			writeJSON(w, 404, map[string]any{"error": "no such connection"})
			return
		}
		var in connectionInput
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		in.BundleID = cur.BundleID
		if err := refuseInstallFromBody(&in, cur); err != nil {
			bad(w, err)
			return
		}
		c, sec, err := b.buildConnection(&in, cur)
		if err != nil {
			bad(w, err)
			return
		}
		var enc []byte
		if sec != nil { // rotate secret only when new secret fields were supplied
			if enc, err = b.sealSecret(sec); err != nil {
				fail(w, err)
				return
			}
			// A repository re-keyed here is no longer the same credential as the ones it was
			// grouped with, and the console groups repositories by this digest.
			if c.Repo != "" {
				c.SecretFP = secretFingerprint(sec.Token)
			}
		}
		if err := b.store.UpdateConnection(r.Context(), orgOf(r), c, enc); err != nil {
			fail(w, err)
			return
		}
		b.audit(r, "connection.updated", AuditEvent{TargetKind: "connection", TargetID: strconv.FormatInt(c.ID, 10), TargetName: c.Name,
			Details: auditDetails(map[string]any{"secret_rotated": sec != nil, "preset": c.Preset, "allowed_hosts": c.AllowedHosts})})
		// The credential may have just been rotated. Both caches keyed on this connection hold
		// something minted from the old one, so drop them here rather than waiting for a TTL.
		b.proxy.forgetToken(c.ID)
		b.agent.mcp.forget(c.ID)
		b.changed(r.Context(), orgOf(r))
		c, _ = b.store.Connection(r.Context(), orgOf(r), c.ID)
		writeJSON(w, 200, c)
	}))
	mux.HandleFunc("DELETE /api/connections/{id}", b.requirePerm(PermConnManage, func(w http.ResponseWriter, r *http.Request) {
		id := pathID(r, "id")
		// Named before it goes, for the same reason a bundle is.
		cur, _ := b.store.Connection(r.Context(), orgOf(r), id)
		if err := b.store.DeleteConnection(r.Context(), orgOf(r), id); err != nil {
			fail(w, err)
			return
		}
		deleted := AuditEvent{TargetKind: "connection", TargetID: strconv.FormatInt(id, 10)}
		if cur != nil {
			deleted.TargetName = cur.Name
			deleted.Details = auditDetails(map[string]any{"preset": cur.Preset, "bundle_id": cur.BundleID})
		}
		b.audit(r, "connection.deleted", deleted)
		b.connectionGone(r.Context(), orgOf(r), id)
		b.changed(r.Context(), orgOf(r))
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	// Who has signed in. Names only, never a token: the sealed secret rides on an unexported
	// field, so what goes out here is the label and the dates.
	mux.HandleFunc("GET /api/connections/{id}/members", b.requirePerm(PermConnView, func(w http.ResponseWriter, r *http.Request) {
		c, err := b.store.Connection(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil || c == nil {
			writeJSON(w, 404, map[string]any{"error": "no such connection"})
			return
		}
		members, err := b.store.UserConnectionsFor(r.Context(), orgOf(r), c.ID)
		if err != nil {
			fail(w, err)
			return
		}
		// What the admin configured, so the edit form can show it rather than the preset's
		// defaults. clientTemplate resolves the connection's own scopes and strips the tokens;
		// a connection whose client is not set up yet simply reports no parts.
		opts := []ConnectionOption{}
		if st, terr := b.clientTemplate(c); terr == nil {
			opts = connectionOptionState(presetByID(c.Preset), c, st.Scopes)
		}
		writeJSON(w, 200, map[string]any{"members": members, "options": opts,
			"redirect_uri": b.connectRedirectURI(r.Context())})
	}))
	mux.HandleFunc("DELETE /api/connections/{id}/members/{member}", b.requirePerm(PermConnManage, func(w http.ResponseWriter, r *http.Request) {
		c, err := b.store.Connection(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil || c == nil {
			writeJSON(w, 404, map[string]any{"error": "no such connection"})
			return
		}
		id, err := strconv.ParseInt(r.PathValue("member"), 10, 64)
		if err != nil {
			bad(w, fmt.Errorf("member id is required"))
			return
		}
		members, _ := b.store.UserConnectionsFor(r.Context(), orgOf(r), c.ID)
		for _, m := range members {
			if m.ID != id {
				continue
			}
			b.disconnectUser(r.Context(), connectClaim{OrgID: orgOf(r), ConnID: c.ID, TeamID: m.TeamID, SlackUserID: m.SlackUserID}, c)
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
		writeJSON(w, 404, map[string]any{"error": "no such member"})
	}))
	mux.HandleFunc("POST /api/connections/{id}/copy", b.requirePerm(PermConnManage, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			BundleID int64 `json:"bundle_id"`
		}
		if err := decode(r, &in); err != nil || in.BundleID == 0 {
			bad(w, fmt.Errorf("bundle_id is required"))
			return
		}
		if bd, _ := b.store.Bundle(r.Context(), orgOf(r), in.BundleID); bd == nil {
			writeJSON(w, 404, map[string]any{"error": "no such bundle"})
			return
		}
		id, err := b.store.CopyConnection(r.Context(), orgOf(r), pathID(r, "id"), in.BundleID, adminFromCtx(r.Context()).UserID)
		if err != nil {
			fail(w, err)
			return
		}
		b.changed(r.Context(), orgOf(r))
		c, _ := b.store.Connection(r.Context(), orgOf(r), id)
		copied := AuditEvent{TargetKind: "connection", TargetID: strconv.FormatInt(id, 10),
			Details: auditDetails(map[string]any{"from_connection_id": pathID(r, "id"), "to_bundle_id": in.BundleID})}
		if c != nil {
			copied.TargetName = c.Name
		}
		b.audit(r, "connection.copied", copied)
		writeJSON(w, 201, c)
	}))
	mux.HandleFunc("POST /api/connections/{id}/test", b.requirePerm(PermConnManage, func(w http.ResponseWriter, r *http.Request) {
		c, err := b.store.Connection(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil || c == nil {
			writeJSON(w, 404, map[string]any{"error": "no such connection"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
		defer cancel()
		writeJSON(w, 200, b.testConnection(ctx, orgOf(r), c))
	}))
	// test a connection before saving it (dialog's Test connection)
	mux.HandleFunc("POST /api/connections/test", b.requirePerm(PermConnManage, func(w http.ResponseWriter, r *http.Request) {
		var in connectionInput
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		if err := refuseInstallFromBody(&in, nil); err != nil {
			bad(w, err)
			return
		}
		c, sec, err := b.buildConnection(&in, nil)
		if err != nil {
			bad(w, err)
			return
		}
		enc, err := b.sealSecret(sec)
		if err != nil {
			fail(w, err)
			return
		}
		c.secretEnc = enc
		ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
		defer cancel()
		writeJSON(w, 200, b.testConnection(ctx, orgOf(r), c))
	}))
	mux.HandleFunc("GET /api/connections/{id}/curl", b.requirePerm(PermConnManage, func(w http.ResponseWriter, r *http.Request) {
		c, err := b.store.Connection(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil || c == nil {
			writeJSON(w, 404, map[string]any{"error": "no such connection"})
			return
		}
		writeJSON(w, 200, map[string]any{"curl": curlExample(c)})
	}))

	// documents
	mux.HandleFunc("GET /api/documents", a(func(w http.ResponseWriter, r *http.Request) {
		docs, err := b.docs.For(orgOf(r)).List(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		// Which of them a Drive sync owns. The table marks those, because editing one by hand
		// is work that the next sync throws away.
		if owned, err := b.store.DriveSyncedPaths(r.Context(), orgOf(r)); err == nil {
			for i := range docs {
				docs[i].DriveSync = owned[docs[i].Path]
			}
		}
		writeJSON(w, 200, docs)
	}))
	// Uploading, folders and all. POST /v1/documents is the same handler behind a key.
	mux.HandleFunc("POST /api/documents", b.requirePerm(PermDocsManage, func(w http.ResponseWriter, r *http.Request) {
		b.uploadDocuments(w, r, adminFromCtx(r.Context()).UserID)
	}))
	// Moving a document, or a whole folder: "to" is the full destination path in both cases.
	mux.HandleFunc("POST /api/documents/move", b.requirePerm(PermDocsManage, func(w http.ResponseWriter, r *http.Request) {
		var in struct{ From, To string }
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		// Cleaned here as well as inside Move, because the folders remembered below have to be
		// the ones the document actually landed in.
		to, err := cleanRel(in.To)
		if err != nil {
			bad(w, err)
			return
		}
		if err := b.docs.For(orgOf(r)).Move(r.Context(), in.From, to); err != nil {
			fail(w, err)
			return
		}
		for _, folder := range parentFolders(to) {
			b.store.AddDocumentFolder(r.Context(), orgOf(r), folder)
		}
		go b.reindex(context.WithoutCancel(r.Context()), orgOf(r))
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	// raw file contents, for the console editor (text types only)
	mux.HandleFunc("GET /api/documents/{path...}", a(func(w http.ResponseWriter, r *http.Request) {
		p := r.PathValue("path")
		if !isEditableDoc(p) {
			writeJSON(w, 415, map[string]any{"error": "not a text document"})
			return
		}
		f, err := b.docs.For(orgOf(r)).Get(r.Context(), p)
		if err != nil {
			if os.IsNotExist(err) || errors.Is(err, storage.ErrObjectNotExist) {
				writeJSON(w, 404, map[string]any{"error": "no such document"})
				return
			}
			fail(w, err)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", docDisposition(p))
		w.Header().Set("Cache-Control", "no-store")
		io.Copy(w, f)
	}))
	// PUT updates the scope, the contents (text types; re-indexed afterwards), or both.
	mux.HandleFunc("PUT /api/documents/{path...}", b.requirePerm(PermDocsManage, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Scope   *string `json:"scope"`
			Content *string `json:"content"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&in); err != nil {
			bad(w, err)
			return
		}
		p := r.PathValue("path")
		if in.Content != nil {
			if !isEditableDoc(p) {
				writeJSON(w, 415, map[string]any{"error": "not a text document"})
				return
			}
			scope := ""
			if in.Scope != nil {
				scope = *in.Scope
			}
			// Put keeps the stored scope when none is given.
			if err := b.docs.For(orgOf(r)).Put(r.Context(), p, strings.NewReader(*in.Content), adminFromCtx(r.Context()).UserID, scope); err != nil {
				fail(w, err)
				return
			}
			go b.reindex(context.WithoutCancel(r.Context()), orgOf(r))
		} else if in.Scope != nil {
			if err := b.store.SetDocumentScope(r.Context(), orgOf(r), p, *in.Scope); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					writeJSON(w, 404, map[string]any{"error": "no such document"})
					return
				}
				fail(w, err)
				return
			}
		}
		changed := map[string]any{"content": in.Content != nil}
		if in.Scope != nil {
			changed["scope"] = *in.Scope
		}
		b.audit(r, "document.updated", AuditEvent{TargetKind: "document", TargetID: p, TargetName: path.Base(p), Details: auditDetails(changed)})
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	mux.HandleFunc("DELETE /api/documents/{path...}", b.requirePerm(PermDocsManage, func(w http.ResponseWriter, r *http.Request) {
		if err := b.docs.For(orgOf(r)).Delete(r.Context(), r.PathValue("path")); err != nil {
			fail(w, err)
			return
		}
		go b.reindex(context.WithoutCancel(r.Context()), orgOf(r))
		b.audit(r, "document.deleted", AuditEvent{TargetKind: "document", TargetID: r.PathValue("path"), TargetName: path.Base(r.PathValue("path"))})
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	// ---- Google Drive sync ----
	// A folder in Drive, copied into Documents on a schedule. Reading the list is part of
	// reading the documents page; everything that changes what is indexed is docs.manage.
	mux.HandleFunc("GET /api/drive/syncs", a(func(w http.ResponseWriter, r *http.Request) {
		syncs, err := b.store.DriveSyncs(r.Context(), orgOf(r))
		if err != nil {
			fail(w, err)
			return
		}
		// The connection and bundle a sync spends are connections.view, the same as everywhere the
		// name of a credential is shown; a member without it sees the sync, not what it runs on.
		if !adminFromCtx(r.Context()).Permissions[PermConnView] {
			for _, s := range syncs {
				s.ConnName, s.BundleName = "", ""
			}
		}
		writeJSON(w, 200, map[string]any{"syncs": syncs, "every_hours": int(driveSyncEvery / time.Hour)})
	}))
	// The credentials a sync could be built on: connections that actually reach Drive. Seeing
	// that a credential exists is connections.view, which is why this is not folded into the
	// list above — the panel renders for any admin, the picker only for those who may look.
	mux.HandleFunc("GET /api/drive/connections", b.requirePerm(PermConnView, func(w http.ResponseWriter, r *http.Request) {
		conns, err := b.store.AllConnections(r.Context(), orgOf(r))
		if err != nil {
			fail(w, err)
			return
		}
		// The bundle each is filed under travels with it: two bundles can each hold a
		// connection called "Drive", and a picker that shows only the name cannot tell them
		// apart — nor say which set of channels the credential is reachable from.
		bundles, _ := b.store.Bundles(r.Context(), orgOf(r))
		bundleName := map[int64]string{}
		for _, bu := range bundles {
			bundleName[bu.ID] = bu.Name
		}
		out := []map[string]any{}
		for _, c := range conns {
			// Not an oauth_user connection, even though Google Workspace reaches the same host:
			// its credential is one token per person, and a sync runs unattended on behalf of
			// nobody in particular. Offering it here would mean a folder that fails every pass.
			if hostAllowed(c, driveHost) && c.HasSecret && c.CredType != "oauth_user" {
				out = append(out, map[string]any{"id": c.ID, "name": c.Name, "preset": c.Preset, "status": c.Status,
					"bundle_id": c.BundleID, "bundle_name": bundleName[c.BundleID]})
			}
		}
		writeJSON(w, 200, out)
	}))
	// Adding one checks the folder before it is saved: a link that turns out to be a file, or a
	// folder the service account was never shared with, is a mistake to catch while somebody is
	// still looking at the dialog rather than six hours later in a run log.
	mux.HandleFunc("POST /api/drive/syncs", b.requirePerm(PermDocsManage, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ConnectionID int64  `json:"connection_id"`
			Folder       string `json:"folder"` // a Drive link, or a bare id
			Dest         string `json:"dest"`
			Scope        string `json:"scope"`
			Recurse      bool   `json:"recurse"`
		}
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		folderID, err := driveFolderID(in.Folder)
		if err != nil {
			bad(w, err)
			return
		}
		dest, err := cleanFolder(in.Dest)
		if err != nil {
			bad(w, err)
			return
		}
		org := orgOf(r)
		conn, err := b.store.Connection(r.Context(), org, in.ConnectionID)
		if err != nil || conn == nil {
			bad(w, errors.New("pick a Drive connection"))
			return
		}
		api, err := newDriveAPI(b.proxy, conn, org)
		if err != nil {
			bad(w, err)
			return
		}
		meta, err := api.meta(r.Context(), folderID)
		if err != nil {
			bad(w, driveShareHint(err))
			return
		}
		if meta.MimeType != driveFolderMime {
			bad(w, fmt.Errorf("%s is a file, not a folder", meta.Name))
			return
		}
		id, err := b.store.AddDriveSync(r.Context(), org, DriveSync{
			ConnectionID: in.ConnectionID, FolderID: folderID, FolderName: meta.Name,
			Dest: dest, Scope: in.Scope, Recurse: in.Recurse, CreatedBy: adminFromCtx(r.Context()).UserID,
		})
		if err != nil {
			fail(w, err)
			return
		}
		if dest != "" {
			for _, folder := range append(parentFolders(dest), dest) {
				b.store.AddDocumentFolder(r.Context(), org, folder)
			}
		}
		writeJSON(w, 201, map[string]any{"id": id, "folder_name": meta.Name})
	}))
	// "Test" in the dialog: walk the folder and say what a pass would bring in, before anything
	// is saved or copied. It reads Drive on the credential exactly as a pass would, which is why
	// it needs the permission that adds a sync rather than the one that reads the page; it
	// writes nothing. Either a folder that is not saved yet, or an existing sync's.
	mux.HandleFunc("POST /api/drive/syncs/preview", b.requirePerm(PermDocsManage, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			SyncID       int64  `json:"sync_id"`       // an existing sync's folder and credential…
			ConnectionID int64  `json:"connection_id"` // …or a folder somebody is still typing in
			Folder       string `json:"folder"`
			Dest         string `json:"dest"`
			Recurse      bool   `json:"recurse"`
		}
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		org := orgOf(r)
		connID, folderID := in.ConnectionID, ""
		if in.SyncID != 0 {
			s, err := b.store.DriveSync(r.Context(), org, in.SyncID)
			if err != nil || s == nil {
				writeJSON(w, 404, map[string]any{"error": "no such sync"})
				return
			}
			connID, folderID = s.ConnectionID, s.FolderID
		} else {
			id, err := driveFolderID(in.Folder)
			if err != nil {
				bad(w, err)
				return
			}
			folderID = id
		}
		dest, err := cleanFolder(in.Dest)
		if err != nil {
			bad(w, err)
			return
		}
		conn, err := b.store.Connection(r.Context(), org, connID)
		if err != nil || conn == nil {
			bad(w, errors.New("pick a Drive connection"))
			return
		}
		api, err := newDriveAPI(b.proxy, conn, org)
		if err != nil {
			bad(w, err)
			return
		}
		pv, err := api.preview(r.Context(), folderID, dest, in.Recurse)
		if err != nil {
			bad(w, err)
			return
		}
		writeJSON(w, 200, pv)
	}))
	mux.HandleFunc("PUT /api/drive/syncs/{id}", b.requirePerm(PermDocsManage, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Dest    string `json:"dest"`
			Scope   string `json:"scope"`
			Recurse bool   `json:"recurse"`
			Enabled bool   `json:"enabled"`
		}
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		dest, err := cleanFolder(in.Dest)
		if err != nil {
			bad(w, err)
			return
		}
		if err := b.store.UpdateDriveSync(r.Context(), orgOf(r), pathID(r, "id"), dest, in.Scope, in.Recurse, in.Enabled); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeJSON(w, 404, map[string]any{"error": "no such sync"})
				return
			}
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	mux.HandleFunc("DELETE /api/drive/syncs/{id}", b.requirePerm(PermDocsManage, func(w http.ResponseWriter, r *http.Request) {
		if err := b.store.DeleteDriveSync(r.Context(), orgOf(r), pathID(r, "id")); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	// Sync now, for one folder. The pass runs while the request is open so the console can show
	// what it did; a big first pass is the slow case, which is why the client waits on it.
	mux.HandleFunc("POST /api/drive/syncs/{id}/run", b.requirePerm(PermDocsManage, func(w http.ResponseWriter, r *http.Request) {
		org := orgOf(r)
		s, err := b.store.DriveSync(r.Context(), org, pathID(r, "id"))
		if err != nil || s == nil {
			writeJSON(w, 404, map[string]any{"error": "no such sync"})
			return
		}
		rep, err := b.runDriveSync(r.Context(), org, s)
		if err != nil {
			writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error(), "report": rep})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "report": rep})
	}))
	// Sync now, for every enabled folder. One report per sync, so a run where one folder failed
	// still says what the other three did.
	mux.HandleFunc("POST /api/drive/syncs/run", b.requirePerm(PermDocsManage, func(w http.ResponseWriter, r *http.Request) {
		org := orgOf(r)
		syncs, err := b.store.DriveSyncs(r.Context(), org)
		if err != nil {
			fail(w, err)
			return
		}
		total := DriveReport{}
		failed := []string{}
		ran := 0
		for _, s := range syncs {
			if !s.Enabled {
				continue
			}
			ran++
			rep, err := b.runDriveSync(r.Context(), org, s)
			total.Added += rep.Added
			total.Updated += rep.Updated
			total.Removed += rep.Removed
			total.Unchanged += rep.Unchanged
			total.Skipped = append(total.Skipped, rep.Skipped...)
			if err != nil {
				name := s.FolderName
				if name == "" {
					name = s.FolderID
				}
				failed = append(failed, name+" — "+err.Error())
			}
		}
		writeJSON(w, 200, map[string]any{"ok": len(failed) == 0, "ran": ran, "report": total, "failed": failed})
	}))

	mux.HandleFunc("POST /api/documents/reindex", b.requirePerm(PermDocsManage, func(w http.ResponseWriter, r *http.Request) {
		rep, err := b.ix.IngestAndRecord(r.Context(), orgOf(r), b.docs.For(orgOf(r)))
		if err != nil {
			writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "report": rep})
	}))

	// document folders. A folder is a prefix of a document path; these routes exist so that
	// an empty one still has somewhere to be.
	mux.HandleFunc("GET /api/document-folders", a(func(w http.ResponseWriter, r *http.Request) {
		folders, err := b.documentFolders(r.Context(), orgOf(r))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, folders)
	}))
	mux.HandleFunc("POST /api/document-folders", b.requirePerm(PermDocsManage, func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Path string }
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		folder, err := cleanFolder(in.Path)
		if err != nil || folder == "" {
			bad(w, fmt.Errorf("a folder name is required"))
			return
		}
		for _, f := range append(parentFolders(folder), folder) {
			if err := b.store.AddDocumentFolder(r.Context(), orgOf(r), f); err != nil {
				fail(w, err)
				return
			}
		}
		writeJSON(w, 201, map[string]any{"ok": true, "path": folder})
	}))
	mux.HandleFunc("POST /api/document-folders/move", b.requirePerm(PermDocsManage, func(w http.ResponseWriter, r *http.Request) {
		var in struct{ From, To string }
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		from, err := cleanFolder(in.From)
		if err != nil || from == "" {
			bad(w, fmt.Errorf("a folder to move is required"))
			return
		}
		to, err := cleanFolder(in.To)
		if err != nil || to == "" {
			bad(w, fmt.Errorf("a destination is required"))
			return
		}
		if to == from || strings.HasPrefix(to, from+"/") {
			bad(w, fmt.Errorf("a folder cannot be moved inside itself"))
			return
		}
		docs := b.docs.For(orgOf(r))
		list, err := docs.List(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		for _, d := range list {
			if !strings.HasPrefix(d.Path, from+"/") {
				continue
			}
			if err := docs.Move(r.Context(), d.Path, to+strings.TrimPrefix(d.Path, from)); err != nil {
				fail(w, err)
				return
			}
		}
		for _, f := range parentFolders(to) {
			b.store.AddDocumentFolder(r.Context(), orgOf(r), f)
		}
		if err := b.store.MoveDocumentFolders(r.Context(), orgOf(r), from, to); err != nil {
			fail(w, err)
			return
		}
		b.store.AddDocumentFolder(r.Context(), orgOf(r), to)
		go b.reindex(context.WithoutCancel(r.Context()), orgOf(r))
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	// One scope for everything in a folder. This is a bulk apply, not an inherited setting:
	// it writes the scope onto the documents that are in the folder now, and a document put
	// on its own scope afterwards keeps it until somebody sets the folder again.
	mux.HandleFunc("PUT /api/document-folders/{path...}", b.requirePerm(PermDocsManage, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Scope *string `json:"scope"`
		}
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		if in.Scope == nil {
			bad(w, fmt.Errorf("a scope is required"))
			return
		}
		folder, err := cleanFolder(r.PathValue("path"))
		if err != nil || folder == "" {
			bad(w, fmt.Errorf("a folder is required"))
			return
		}
		n, err := b.store.SetDocumentScopesUnder(r.Context(), orgOf(r), folder, *in.Scope)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "updated": n})
	}))
	mux.HandleFunc("DELETE /api/document-folders/{path...}", b.requirePerm(PermDocsManage, func(w http.ResponseWriter, r *http.Request) {
		folder, err := cleanFolder(r.PathValue("path"))
		if err != nil || folder == "" {
			bad(w, fmt.Errorf("a folder is required"))
			return
		}
		docs := b.docs.For(orgOf(r))
		list, err := docs.List(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		deleted := 0
		for _, d := range list {
			if !strings.HasPrefix(d.Path, folder+"/") {
				continue
			}
			if err := docs.Delete(r.Context(), d.Path); err != nil {
				fail(w, err)
				return
			}
			deleted++
		}
		if err := b.store.DeleteDocumentFolders(r.Context(), orgOf(r), folder); err != nil {
			fail(w, err)
			return
		}
		if deleted > 0 {
			go b.reindex(context.WithoutCancel(r.Context()), orgOf(r))
		}
		writeJSON(w, 200, map[string]any{"ok": true, "deleted": deleted})
	}))

	// memory
	mux.HandleFunc("GET /api/memories", a(func(w http.ResponseWriter, r *http.Request) {
		ms, err := b.store.AllMemories(r.Context(), orgOf(r))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, ms)
	}))
	mux.HandleFunc("POST /api/memories", b.requirePerm(PermMemoryManage, func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Scope, Text string }
		if err := decode(r, &in); err != nil || in.Scope == "" || strings.TrimSpace(in.Text) == "" {
			bad(w, fmt.Errorf("scope and text are required"))
			return
		}
		// The scope names a workspace; it has to be one of this organisation's. Same rule as
		// the developer API: nobody writes into a workspace by naming it.
		team := teamOfMemoryScope(in.Scope)
		if t, err := b.store.Team(r.Context(), team); team == "" || err != nil || t == nil || t.OrgID != orgOf(r) {
			writeJSON(w, 404, map[string]any{"error": "that scope does not name a workspace this organisation connected"})
			return
		}
		if err := b.store.AddMemory(r.Context(), orgOf(r), team, in.Scope, strings.TrimSpace(in.Text), adminFromCtx(r.Context()).UserID); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 201, map[string]any{"ok": true})
	}))
	mux.HandleFunc("PUT /api/memories/{id}", b.requirePerm(PermMemoryManage, func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Text string }
		if err := decode(r, &in); err != nil || strings.TrimSpace(in.Text) == "" {
			bad(w, fmt.Errorf("text is required"))
			return
		}
		// The id is looked up under this organisation, so somebody else's memory is a 404 and
		// not a rewrite: the same boundary DELETE keeps.
		found, err := b.store.UpdateMemory(r.Context(), orgOf(r), pathID(r, "id"), strings.TrimSpace(in.Text))
		if err != nil {
			fail(w, err)
			return
		}
		if !found {
			writeJSON(w, 404, map[string]any{"error": "no such memory"})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	mux.HandleFunc("DELETE /api/memories/{id}", b.requirePerm(PermMemoryManage, func(w http.ResponseWriter, r *http.Request) {
		if err := b.store.DeleteMemory(r.Context(), orgOf(r), pathID(r, "id")); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	}))

	// a person's own notes; see personal_memory_api.go for why these carry no permission
	b.personalMemoryRoutes(mux, a)

	// artifacts — files the bot made in Slack, kept so the console shows what was produced
	mux.HandleFunc("GET /api/artifacts", b.requirePerm(PermArtifactsView, func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		arts, err := b.store.Artifacts(r.Context(), orgOf(r), limit)
		if err != nil {
			fail(w, err)
			return
		}
		out := make([]map[string]any, 0, len(arts))
		for _, art := range arts {
			out = append(out, artifactJSON(r.Context(), b, art, false))
		}
		writeJSON(w, 200, out)
	}))
	mux.HandleFunc("GET /api/artifacts/{id}", b.requirePerm(PermArtifactsView, func(w http.ResponseWriter, r *http.Request) {
		art, err := b.store.ArtifactByID(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, 200, artifactJSON(r.Context(), b, *art, true))
	}))
	// Raw bytes, always as a download: the content is model-written and this origin holds the
	// admin session, so nothing here is ever rendered in the browser.
	mux.HandleFunc("GET /api/artifacts/{id}/raw", b.requirePerm(PermArtifactsView, func(w http.ResponseWriter, r *http.Request) {
		art, err := b.store.ArtifactByID(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		kind, ok := artifactKinds[art.Kind]
		if !ok {
			kind = artifactKinds["txt"]
		}
		name := slug(art.Title)
		if name == "" {
			name = fmt.Sprintf("artifact-%d", art.ID)
		}
		w.Header().Set("Content-Type", kind.MIME)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name+"."+kind.Ext))
		w.Write([]byte(art.Content))
	}))
	mux.HandleFunc("DELETE /api/artifacts/{id}", b.requirePerm(PermArtifactsManage, func(w http.ResponseWriter, r *http.Request) {
		if err := b.store.DeleteArtifact(r.Context(), orgOf(r), pathID(r, "id")); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	}))

	// routines
	mux.HandleFunc("GET /api/routines", a(func(w http.ResponseWriter, r *http.Request) {
		rs, err := b.store.Routines(r.Context(), orgOf(r), "")
		if err != nil {
			fail(w, err)
			return
		}
		if rs == nil {
			rs = []Routine{}
		}
		writeJSON(w, 200, rs)
	}))
	// A routine set up in the console rather than by asking for one in Slack. It takes the
	// fields the editor writes plus the two only a creation can decide — the channel it posts
	// to, and who it runs as — and everything is checked here, because the store writes a
	// routine as it is given: a schedule that will not parse, a channel the bot is not in, or
	// a model nobody offered would otherwise become a row that fails every morning.
	mux.HandleFunc("POST /api/routines", b.requirePerm(PermRoutinesManage, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Channel string `json:"channel"`
			// Only when the same channel id is shared into more than one connected workspace.
			TeamID     string          `json:"teamId"`
			Cron       string          `json:"cron"`
			TZ         string          `json:"tz"`
			Prompt     string          `json:"prompt"`
			Notify     string          `json:"notify"`
			NotifyWhen string          `json:"notifyWhen"`
			Model      string          `json:"model"`
			Steps      json.RawMessage `json:"steps"`
			Finish     string          `json:"finish"`
			// Absent reads as on: nobody is there to press Confirm when a schedule fires, so a
			// held write would only expire, and the person writing it here holds
			// PermRoutinesManage. A routine asked for in Slack is the opposite — create_routine
			// always starts on Ask first, since what the model was told cannot decide that.
			AutoConfirm *bool `json:"autoConfirm"`
		}
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		id, err := b.addRoutine(r.Context(), orgOf(r), routineDraft{
			Channel: in.Channel, TeamID: in.TeamID, Cron: in.Cron, TZ: in.TZ, Prompt: in.Prompt,
			Notify: in.Notify, NotifyWhen: in.NotifyWhen, Model: in.Model, Finish: in.Finish, Steps: in.Steps,
			AutoConfirm: in.AutoConfirm == nil || *in.AutoConfirm,
			// Whose turn each run is. A console admin who signed in through Slack gets their
			// own id, so the routine reaches what they reach and failures are sent to them;
			// one who signed in with a password has no Slack id to run as, and an empty
			// creator is already what a run treats as the bot itself.
			CreatedBy: adminFromCtx(r.Context()).UserID,
		})
		if err != nil {
			bad(w, err)
			return
		}
		writeJSON(w, 201, map[string]any{"id": id})
	}))
	mux.HandleFunc("PUT /api/routines/{id}", b.requirePerm(PermRoutinesManage, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Enabled *bool   `json:"enabled"`
			Cron    *string `json:"cron"`
			TZ      *string `json:"tz"`
			Prompt  *string `json:"prompt"`
			// Where it posts. A channel the bot is in, by id; teamId only when the same id is
			// shared into more than one connected workspace.
			Channel    *string `json:"channel"`
			TeamID     *string `json:"teamId"`
			Notify     *string `json:"notify"`
			NotifyWhen *string `json:"notifyWhen"`
			// The calls this routine makes before the model is asked anything, either as an
			// array — [{"tool":"http_request","args":{…}}] — or as the JSON text of one, which
			// is what a console textarea sends. parseRoutineSteps is the only thing that
			// decides whether either is valid. Finish is what happens after they run.
			Steps  json.RawMessage `json:"steps"`
			Finish *string         `json:"finish"`
			// Which model its runs answer on: "" for the default, "heavy" for the advanced
			// model, or one of the models Settings offers to channels. Nothing else.
			Model *string `json:"model"`
			// Whether this routine's writes run without a Confirm card. Behind
			// PermRoutinesManage like the rest of this handler: pre-approving every write a
			// schedule will ever make is a different decision from pressing Confirm once.
			AutoConfirm *bool `json:"autoConfirm"`
		}
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		me := adminFromCtx(r.Context())
		wasRunningAs, err := b.editRoutine(r.Context(), orgOf(r), pathID(r, "id"), routineEdit{
			Enabled: in.Enabled, Cron: in.Cron, TZ: in.TZ, Prompt: in.Prompt, Channel: in.Channel, TeamID: in.TeamID,
			Notify: in.Notify, NotifyWhen: in.NotifyWhen, Steps: in.Steps, Finish: in.Finish, Model: in.Model,
			AutoConfirm: in.AutoConfirm,
			// Who is making the change, so the store can decide whether the routine may go on
			// running as the person who wrote it. Empty for a password session or an API key,
			// which is the safe reading rather than a gap: neither one is anybody's Slack
			// account, so neither can hand a run somebody's personal connections.
			EditedBy: me.UserID,
		})
		if err != nil {
			bad(w, err)
			return
		}
		if wasRunningAs != "" {
			// Taken off the person who wrote it: said in the response so the console can show
			// it, and to them by editRoutine, so they do not have to be looking.
			writeJSON(w, 200, map[string]any{"ok": true, "runsAs": me.UserID, "wasRunningAs": wasRunningAs})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	// A routine's history, including the runs that decided to stay out of the channel. Readable
	// by anyone who can see the routine itself, which is what GET /api/routines allows.
	mux.HandleFunc("GET /api/routines/{id}/runs", b.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		runs, err := b.store.RoutineRuns(r.Context(), orgOf(r), pathID(r, "id"), limit)
		if err != nil {
			fail(w, err)
			return
		}
		out := make([]map[string]any, 0, len(runs))
		for _, run := range runs {
			out = append(out, routineRunJSON(run, true))
		}
		writeJSON(w, 200, map[string]any{"runs": out})
	}))
	// One run with its output whole, the way a long tool result waits behind its own route.
	mux.HandleFunc("GET /api/routine-runs/{id}", b.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		run, err := b.store.RoutineRunByID(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil {
			writeJSON(w, 404, map[string]any{"error": "no such run"})
			return
		}
		writeJSON(w, 200, routineRunJSON(*run, false))
	}))
	mux.HandleFunc("DELETE /api/routines/{id}", b.requirePerm(PermRoutinesManage, func(w http.ResponseWriter, r *http.Request) {
		if err := b.store.DeleteRoutine(r.Context(), orgOf(r), pathID(r, "id")); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	mux.HandleFunc("POST /api/routines/{id}/run", b.requirePerm(PermRoutinesManage, func(w http.ResponseWriter, r *http.Request) {
		rs, _ := b.store.Routines(r.Context(), orgOf(r), "")
		for _, rt := range rs {
			if rt.ID == pathID(r, "id") {
				// Recorded like any other run, and the schedule stays where it was: asking for
				// a run now is not a reason to move the next one.
				go b.agent.runRoutineNow(context.Background(), rt, rt.NextRun)
				writeJSON(w, 202, map[string]any{"ok": true})
				return
			}
		}
		writeJSON(w, 404, map[string]any{"error": "no such routine"})
	}))

	// The invite link. Public and token-gated: the token is the authority, and it is exchanged
	// for a membership only once whoever holds it has proved who they are.
	mux.HandleFunc("GET /invite/{token}", b.startInvite)

	// ---- the people in this organisation ----
	//
	// Everything here is scoped to the organisation on the caller's session. A user id in a URL
	// is never enough on its own: the membership is what is read, written and deleted, so a
	// request naming somebody in another organisation finds nothing rather than acting on them.

	mux.HandleFunc("GET /api/console/users", a(func(w http.ResponseWriter, r *http.Request) {
		me := adminFromCtx(r.Context())
		members, err := b.store.MembersOf(r.Context(), me.OrgID)
		if err != nil {
			fail(w, err)
			return
		}
		custom := b.store.CustomRoleMap(r.Context(), me.OrgID)
		out := make([]map[string]any, 0, len(members))
		for _, m := range members {
			out = append(out, map[string]any{
				"id": m.UserPublic, "email": m.Email, "name": m.Name, "role": m.Role,
				"email_verified": m.Verified, "last_seen": m.LastSeen, "created_at": m.CreatedAt,
				"is_you":      m.UserID == me.ID,
				"permissions": permissionKeys(permissionsForRole(m.Role, custom)),
			})
		}
		writeJSON(w, 200, out)
	}))

	mux.HandleFunc("PUT /api/console/users/{id}/role", b.requirePerm(PermUsersManage, func(w http.ResponseWriter, r *http.Request) {
		me := adminFromCtx(r.Context())
		var in struct {
			Role string `json:"role"`
		}
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		target := b.pathUserID(r, "id")
		cur, err := b.store.Membership(r.Context(), target, me.OrgID)
		if err != nil || cur == nil {
			writeJSON(w, 404, map[string]any{"error": "that person is not in this organisation"})
			return
		}
		custom := b.store.CustomRoleMap(r.Context(), me.OrgID)
		if !canAssignRole(me.Permissions, in.Role, custom) || !canAssignRole(me.Permissions, cur.Role, custom) {
			writeJSON(w, 403, map[string]any{"error": "You can only assign a role you hold yourself."})
			return
		}
		// Changing your own role away from admin, or demoting the last one, would leave the
		// organisation with nobody who can undo it.
		if err := b.lastHolderGuard(r.Context(), me.OrgID, target, cur.Role, in.Role); err != nil {
			writeJSON(w, 409, map[string]any{"error": err.Error()})
			return
		}
		if err := b.store.SetMemberRole(r.Context(), target, me.OrgID, in.Role); err != nil {
			fail(w, err)
			return
		}
		// A change of role is the single most-asked-about line in an audit log — "who made
		// them admin, and when" — so it carries both ends of the change.
		b.audit(r, "member.role_changed", AuditEvent{TargetKind: "member", TargetID: r.PathValue("id"), TargetName: b.emailOf(r.Context(), target),
			Details: auditDetails(map[string]any{"from": cur.Role, "to": in.Role})})
		writeJSON(w, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("DELETE /api/console/users/{id}", b.requirePerm(PermUsersManage, func(w http.ResponseWriter, r *http.Request) {
		me := adminFromCtx(r.Context())
		target := b.pathUserID(r, "id")
		cur, err := b.store.Membership(r.Context(), target, me.OrgID)
		if err != nil || cur == nil {
			writeJSON(w, 404, map[string]any{"error": "that person is not in this organisation"})
			return
		}
		if err := b.lastHolderGuard(r.Context(), me.OrgID, target, cur.Role, ""); err != nil {
			writeJSON(w, 409, map[string]any{"error": err.Error()})
			return
		}
		// The membership goes; the account does not. They may belong to other organisations,
		// and deleting a person because one organisation removed them would be somebody else's
		// data disappearing.
		if err := b.store.RemoveMember(r.Context(), target, me.OrgID); err != nil {
			fail(w, err)
			return
		}
		// Removal takes the person's integrations and open consoles with it. A key whose owner
		// has no membership is refused anyway, but a row that still reads as live would start
		// working again the day they are re-invited — at whatever the new role grants.
		if err := b.store.RevokeAPIKeysOf(r.Context(), me.OrgID, target); err != nil {
			fail(w, err)
			return
		}
		b.store.DeleteSessionsForOrg(r.Context(), target, me.OrgID)
		b.audit(r, "member.removed", AuditEvent{TargetKind: "member", TargetID: r.PathValue("id"), TargetName: b.emailOf(r.Context(), target),
			Details: auditDetails(map[string]any{"role": cur.Role})})
		writeJSON(w, 200, map[string]any{"ok": true})
	}))

	// ---- roles this organisation defined ----

	mux.HandleFunc("GET /api/console/roles", a(func(w http.ResponseWriter, r *http.Request) {
		me := adminFromCtx(r.Context())
		custom, err := b.store.ConsoleRoles(r.Context(), me.OrgID)
		if err != nil {
			fail(w, err)
			return
		}
		out := []map[string]any{}
		for _, key := range BuiltinRoleKeys {
			list := permissionKeys(permissionsForRole(key, nil))
			sortStrings(list)
			out = append(out, map[string]any{"key": key, "label": strings.ToUpper(key[:1]) + key[1:], "builtin": true,
				"permissions": list})
		}
		for _, role := range custom {
			out = append(out, map[string]any{"key": role.Key, "label": role.Label, "builtin": false,
				"permissions": role.Permissions})
		}
		// The console reads roles and the full permission catalogue from one response: the Roles
		// tab needs every permission that exists to render the checkboxes, not only the ones some
		// role happens to hold.
		writeJSON(w, 200, map[string]any{"roles": out, "all_permissions": AllPermissions()})
	}))

	mux.HandleFunc("POST /api/console/roles", b.requirePerm(PermRolesManage, func(w http.ResponseWriter, r *http.Request) {
		me := adminFromCtx(r.Context())
		var in ConsoleRole
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		in.Key, in.Label = strings.TrimSpace(in.Key), strings.TrimSpace(in.Label)
		if in.Key == "" || in.Label == "" {
			writeJSON(w, 400, map[string]any{"error": "a role needs a key and a label"})
			return
		}
		if IsBuiltinRole(in.Key) {
			writeJSON(w, 409, map[string]any{"error": "that key is a built-in role"})
			return
		}
		// Nobody may mint a role more powerful than the one they hold.
		for _, p := range in.Permissions {
			if !me.Permissions[Permission(p)] {
				writeJSON(w, 403, map[string]any{"error": "You can only grant permissions you hold yourself."})
				return
			}
		}
		// This is an upsert, so it can land on a key that already exists. Writing over a role
		// more powerful than the caller's is an edit of somebody else's access: it would strip
		// permissions from every member holding that role, which the caller may not do directly.
		// Require them to cover what is already there as well as what they are asking for.
		if cur := b.store.CustomRoleMap(r.Context(), me.OrgID)[in.Key]; cur != nil {
			for _, p := range cur {
				if !me.Permissions[p] {
					writeJSON(w, 403, map[string]any{"error": "That role grants permissions you do not hold, so you cannot change it."})
					return
				}
			}
		}
		in.CreatedBy = me.Email
		if err := b.store.UpsertConsoleRole(r.Context(), me.OrgID, &in); err != nil {
			fail(w, err)
			return
		}
		b.audit(r, "role.saved", AuditEvent{TargetKind: "role", TargetID: in.Key, TargetName: in.Label,
			Details: auditDetails(map[string]any{"permissions": in.Permissions})})
		writeJSON(w, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("DELETE /api/console/roles/{key}", b.requirePerm(PermRolesManage, func(w http.ResponseWriter, r *http.Request) {
		me := adminFromCtx(r.Context())
		key := r.PathValue("key")
		// Same rule as writing over one: deleting a role demotes everybody holding it, so the
		// caller has to cover what it grants.
		if cur := b.store.CustomRoleMap(r.Context(), me.OrgID)[key]; cur != nil {
			for _, p := range cur {
				if !me.Permissions[p] {
					writeJSON(w, 403, map[string]any{"error": "That role grants permissions you do not hold, so you cannot delete it."})
					return
				}
			}
		}
		// And it must not be the last thing standing between this organisation and nobody being
		// able to administer it.
		if err := b.lastHolderGuardForRole(r.Context(), me.OrgID, key); err != nil {
			writeJSON(w, 409, map[string]any{"error": err.Error()})
			return
		}
		if err := b.store.DeleteConsoleRole(r.Context(), me.OrgID, key); err != nil {
			fail(w, err)
			return
		}
		b.audit(r, "role.deleted", AuditEvent{TargetKind: "role", TargetID: key})
		writeJSON(w, 200, map[string]any{"ok": true})
	}))

	// ---- invitations ----

	mux.HandleFunc("GET /api/console/invites", b.requirePerm(PermUsersManage, func(w http.ResponseWriter, r *http.Request) {
		me := adminFromCtx(r.Context())
		pending, err := b.store.PendingInvitesFor(r.Context(), me.OrgID)
		if err != nil {
			fail(w, err)
			return
		}
		out := make([]map[string]any, 0, len(pending))
		for _, i := range pending {
			out = append(out, b.describeInvite(r.Context(), me.OrgID, i))
		}
		writeJSON(w, 200, out)
	}))

	// One endpoint for all three ways in — a mailbox, a Slack account, or a link addressed to
	// nobody. What the request names is what decides; createInvite holds the rules.
	mux.HandleFunc("POST /api/console/invites", b.requirePerm(PermUsersManage, func(w http.ResponseWriter, r *http.Request) {
		me := adminFromCtx(r.Context())
		var in inviteReq
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		out, code, err := b.createInvite(r, me, in)
		if err != nil {
			var rl rateLimited
			if errors.As(err, &rl) {
				tooMany(w, rl.d)
				return
			}
			writeJSON(w, code, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, code, out)
	}))

	// Revoking is keyed on the row rather than the address: a share link has no address, and a
	// Slack invitation to somebody whose workspace withholds email has none either.
	mux.HandleFunc("DELETE /api/console/invites/{id}", b.requirePerm(PermUsersManage, func(w http.ResponseWriter, r *http.Request) {
		me := adminFromCtx(r.Context())
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": "that is not an invitation"})
			return
		}
		if err := b.store.RevokeInviteByID(r.Context(), me.OrgID, id); err != nil {
			writeJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
		slog.Info("invitation revoked", "org", me.OrgID, "by", me.ID, "invite", id)
		b.audit(r, "member.invite_revoked", AuditEvent{TargetKind: "invite", TargetID: strconv.FormatInt(id, 10)})
		writeJSON(w, 200, map[string]any{"ok": true})
	}))

	// approval roles: the tiers that can grant, and who holds them
	mux.HandleFunc("GET /api/approval-roles", a(func(w http.ResponseWriter, r *http.Request) {
		rs, err := b.store.ApprovalRoles(r.Context(), orgOf(r))
		if err != nil {
			fail(w, err)
			return
		}
		out := make([]map[string]any, 0, len(rs))
		for _, role := range rs {
			members := make([]map[string]any, 0, len(role.Resolved))
			for _, am := range role.Resolved {
				m := map[string]any{"ref": am.Ref, "name": "", "team_id": am.TeamID, "slack_user_id": am.SlackUserID, "resolved": am.SlackUserID != ""}
				switch {
				case am.SlackUserID != "":
					m["name"] = b.userName(r.Context(), am.TeamID, am.SlackUserID)
				case userIDRe.MatchString(am.Ref):
					m["name"] = b.userName(r.Context(), "", am.Ref)
				}
				members = append(members, m)
			}
			// Say how many connections the tier can actually grant from, counted the way
			// routing counts them: a tier listing only inactive connections, or an emptied
			// bundle, can approve nothing, and that is invisible otherwise.
			grants := len(grantRules(r.Context(), b.store, orgOf(r), role).Rules)
			out = append(out, map[string]any{"id": role.ID, "name": role.Name, "rank": role.Rank,
				"bundle_ids": role.BundleIDs, "connection_ids": role.ConnectionIDs, "members": members, "grantable": grants})
		}
		writeJSON(w, 200, out)
	}))
	mux.HandleFunc("POST /api/approval-roles", b.requirePerm(PermApproversManage, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name          string  `json:"name"`
			Rank          int     `json:"rank"`
			BundleIDs     []int64 `json:"bundle_ids"`
			ConnectionIDs []int64 `json:"connection_ids"`
		}
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		if strings.TrimSpace(in.Name) == "" {
			bad(w, fmt.Errorf("a name is required"))
			return
		}
		role := &ApprovalRole{Name: strings.TrimSpace(in.Name), Rank: in.Rank, BundleIDs: in.BundleIDs, ConnectionIDs: in.ConnectionIDs}
		if err := b.store.AddApprovalRole(r.Context(), orgOf(r), role); err != nil {
			fail(w, err)
			return
		}
		b.changed(r.Context(), orgOf(r))
		writeJSON(w, 200, map[string]any{"id": role.ID})
	}))
	mux.HandleFunc("PUT /api/approval-roles/{id}", b.requirePerm(PermApproversManage, func(w http.ResponseWriter, r *http.Request) {
		cur, err := b.store.ApprovalRole(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil || cur == nil {
			writeJSON(w, 404, map[string]any{"error": "no such role"})
			return
		}
		var in struct {
			Name *string `json:"name"`
			Rank *int    `json:"rank"`
		}
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		if in.Name != nil {
			cur.Name = strings.TrimSpace(*in.Name)
		}
		if in.Rank != nil {
			cur.Rank = *in.Rank
		}
		if err := b.store.UpdateApprovalRole(r.Context(), orgOf(r), cur); err != nil {
			fail(w, err)
			return
		}
		b.changed(r.Context(), orgOf(r))
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	mux.HandleFunc("DELETE /api/approval-roles/{id}", b.requirePerm(PermApproversManage, func(w http.ResponseWriter, r *http.Request) {
		if err := b.store.DeleteApprovalRole(r.Context(), orgOf(r), pathID(r, "id")); err != nil {
			fail(w, err)
			return
		}
		b.changed(r.Context(), orgOf(r))
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	// What a tier grants from: whole bundles, and connections on their own — the same two shapes
	// a channel's access takes, added and removed one at a time the same way. The store checks
	// that both ids are this organisation's in the one statement that writes the row.
	mux.HandleFunc("POST /api/approval-roles/{id}/bundles/{bid}", b.requirePerm(PermApproversManage, func(w http.ResponseWriter, r *http.Request) {
		if err := b.store.AttachRoleBundle(r.Context(), orgOf(r), pathID(r, "id"), pathID(r, "bid")); err != nil {
			bad(w, err)
			return
		}
		b.changed(r.Context(), orgOf(r))
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	mux.HandleFunc("DELETE /api/approval-roles/{id}/bundles/{bid}", b.requirePerm(PermApproversManage, func(w http.ResponseWriter, r *http.Request) {
		if err := b.store.DetachRoleBundle(r.Context(), orgOf(r), pathID(r, "id"), pathID(r, "bid")); err != nil {
			fail(w, err)
			return
		}
		b.changed(r.Context(), orgOf(r))
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	mux.HandleFunc("POST /api/approval-roles/{id}/connections/{cid}", b.requirePerm(PermApproversManage, func(w http.ResponseWriter, r *http.Request) {
		if err := b.store.AttachRoleConnection(r.Context(), orgOf(r), pathID(r, "id"), pathID(r, "cid")); err != nil {
			bad(w, err)
			return
		}
		b.changed(r.Context(), orgOf(r))
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	mux.HandleFunc("DELETE /api/approval-roles/{id}/connections/{cid}", b.requirePerm(PermApproversManage, func(w http.ResponseWriter, r *http.Request) {
		if err := b.store.DetachRoleConnection(r.Context(), orgOf(r), pathID(r, "id"), pathID(r, "cid")); err != nil {
			fail(w, err)
			return
		}
		b.changed(r.Context(), orgOf(r))
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	mux.HandleFunc("POST /api/approval-roles/{id}/members", b.requirePerm(PermApproversManage, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Ref    string `json:"ref"`
			TeamID string `json:"team_id"` // the workspace to resolve the entry in; optional with one workspace
		}
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		ids, emails, badOnes := parseApprovers(in.Ref)
		if len(badOnes) > 0 || len(ids)+len(emails) == 0 {
			bad(w, fmt.Errorf("give a Slack user id (U…) or an email address"))
			return
		}
		// The role id comes from the path, so it has to be proved to be this organisation's
		// before anything is written against it — the way PUT and DELETE on the role itself do.
		if role, _ := b.store.ApprovalRole(r.Context(), orgOf(r), pathID(r, "id")); role == nil {
			writeJSON(w, 404, map[string]any{"error": "no such approval role"})
			return
		}
		// An approver is a person in a workspace. The entry is resolved now, in a workspace this
		// organisation connected, and stored with it: a press from any other workspace — one a
		// partner or customer administers — will not match, whatever its directory says.
		teams, _ := b.store.Teams(r.Context(), orgOf(r))
		var active []*Team
		for _, t := range teams {
			if t.Status == "active" && (in.TeamID == "" || t.TeamID == in.TeamID) {
				active = append(active, t)
			}
		}
		if len(active) == 0 {
			bad(w, fmt.Errorf("connect a Slack workspace first, so the approver can be looked up in it"))
			return
		}
		if len(emails) > 0 && in.TeamID == "" && len(active) > 1 {
			bad(w, fmt.Errorf("say which workspace to look this address up in (team_id): this organisation has %d connected", len(active)))
			return
		}
		added := 0
		for _, ref := range append(ids, emails...) {
			for _, t := range active {
				sl, err := b.slacks.For(r.Context(), t.TeamID)
				if err != nil {
					continue
				}
				userID := ref
				if strings.Contains(ref, "@") {
					if userID, err = sl.UserByEmail(r.Context(), ref); err != nil {
						continue
					}
				} else if uf, err := sl.UserFacts(r.Context(), ref); err != nil || uf.Deleted || uf.Bot {
					continue // not a person in this workspace
				}
				if err := b.store.AddResolvedApprovalMember(r.Context(), orgOf(r), pathID(r, "id"), ref, t.TeamID, userID); err != nil {
					fail(w, err)
					return
				}
				added++
				break
			}
		}
		if added == 0 {
			bad(w, fmt.Errorf("no account in a connected workspace matches that entry"))
			return
		}
		b.changed(r.Context(), orgOf(r))
		// Who may approve is part of who may spend a credential, so the entry as it was typed
		// — an address or a Slack id — is the target.
		b.audit(r, "approver.added", AuditEvent{TargetKind: "approver", TargetID: strings.TrimSpace(in.Ref),
			Details: auditDetails(map[string]any{"approval_role_id": pathID(r, "id"), "team_id": in.TeamID, "added": added})})
		writeJSON(w, 200, map[string]any{"ok": true, "added": added})
	}))
	mux.HandleFunc("DELETE /api/approval-roles/{id}/members", b.requirePerm(PermApproversManage, func(w http.ResponseWriter, r *http.Request) {
		if role, _ := b.store.ApprovalRole(r.Context(), orgOf(r), pathID(r, "id")); role == nil {
			writeJSON(w, 404, map[string]any{"error": "no such approval role"})
			return
		}
		if err := b.store.RemoveApprovalMember(r.Context(), orgOf(r), pathID(r, "id"), r.URL.Query().Get("ref")); err != nil {
			fail(w, err)
			return
		}
		b.changed(r.Context(), orgOf(r))
		b.audit(r, "approver.removed", AuditEvent{TargetKind: "approver", TargetID: r.URL.Query().Get("ref"),
			Details: auditDetails(map[string]any{"approval_role_id": pathID(r, "id")})})
		writeJSON(w, 200, map[string]any{"ok": true})
	}))

	// access requests. Read-only, plus deny: approving is an act by a named Slack approver with
	// a DM audit trail, checked live against their account in the workspace the request came
	// from — none of which a console session can stand in for.
	//
	// This used to cite the console's lack of CSRF tokens as the second reason. That has not
	// been true since double-submit landed in requireAdmin (console_users.go), which covers
	// every state-changing route including any added later; the approver model is the whole
	// reason now.
	mux.HandleFunc("GET /api/access-requests", b.requirePerm(PermAccessView, func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		rs, err := b.store.AccessRequests(r.Context(), orgOf(r), r.URL.Query().Get("status"), limit)
		if err != nil {
			fail(w, err)
			return
		}
		out := make([]map[string]any, 0, len(rs))
		for i := range rs {
			out = append(out, b.accessJSON(r.Context(), &rs[i], false))
		}
		writeJSON(w, 200, out)
	}))
	mux.HandleFunc("GET /api/access-requests/{id}", b.requirePerm(PermAccessView, func(w http.ResponseWriter, r *http.Request) {
		req, err := b.store.AccessRequest(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil {
			fail(w, err)
			return
		}
		if req == nil {
			writeJSON(w, 404, map[string]any{"error": "no such request"})
			return
		}
		writeJSON(w, 200, b.accessJSON(r.Context(), req, true))
	}))
	mux.HandleFunc("POST /api/access-requests/{id}/deny", b.requirePerm(PermAccessClose, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Reason string `json:"reason"`
		}
		decode(r, &in)
		if strings.TrimSpace(in.Reason) == "" {
			bad(w, fmt.Errorf("a reason is required: the person waiting is owed one"))
			return
		}
		who := "an admin"
		if u := adminFromCtx(r.Context()); u != nil && u.UserID != "" {
			who = u.UserID
		}
		req, err := b.store.DenyAccessRequest(r.Context(), orgOf(r), pathID(r, "id"), who, in.Reason)
		if err != nil {
			fail(w, err)
			return
		}
		if req == nil {
			writeJSON(w, 409, map[string]any{"error": "that request has already been answered"})
			return
		}
		b.audit(r, "access_request.denied", AuditEvent{TeamID: req.TeamID, TargetKind: "access_request", TargetID: r.PathValue("id"), TargetName: req.What,
			Details: auditDetails(map[string]any{"requester": req.Requester, "reason": truncate(oneLine(in.Reason), 300)})})
		// Resolve the workspace before spawning. The goroutine outlives the handler, so a
		// writeJSON from inside it would be writing to a ResponseWriter the server has already
		// finished with — a race at best, and a 409 nobody ever receives at worst.
		sl, slErr := b.slacks.For(r.Context(), req.TeamID)
		if slErr != nil {
			writeJSON(w, 409, map[string]any{"error": "That request's Slack workspace is no longer connected."})
			return
		}
		go func(req *AccessRequest, sl *Chat, reason string) {
			ctx := context.Background()
			b.resolveAllCards(ctx, sl, req, "Closed in the console: "+truncate(oneLine(reason), 200))
			sl.PostText(ctx, req.Channel, req.ThreadTS, fmt.Sprintf(
				"<@%s> this access request was closed in the console: %s", req.Requester, truncate(oneLine(reason), 300)))
		}(req, sl, in.Reason)
		writeJSON(w, 200, map[string]any{"ok": true})
	}))

	// activity
	// The rows behind the "Users, last 30 days" figure on the Billing screen. Behind the same
	// permission as the rest of Activity: it is a list of who used the bot, which is what that
	// page is for, and it says nothing about money.
	mux.HandleFunc("GET /api/active-users", b.requirePerm(PermActivityView, func(w http.ResponseWriter, r *http.Request) {
		org := orgOf(r)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		rows, err := b.store.ActiveUserList(r.Context(), org, activeUserWindow, limit)
		if err != nil {
			fail(w, err)
			return
		}
		b.nameActiveUsers(r.Context(), rows)
		writeJSON(w, 200, map[string]any{"users": rows, "count": b.activeUsers(r.Context(), org, true)})
	}))

	mux.HandleFunc("GET /api/activity", b.requirePerm(PermActivityView, func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 500 {
			limit = 100
		}
		// errors=1 narrows to what went wrong. A turn has no failed state, so that list comes
		// back empty rather than unfiltered: the console hides the tab while the filter is on.
		failedOnly := r.URL.Query().Get("errors") == "1"
		// range= narrows to the window an overview tile counted, so clicking "Turns today" lands
		// on today's turns rather than on the last 200 of all time.
		since := activitySince(r.URL.Query().Get("range"))
		// Every list narrows the same way. Filtering tool calls and proxied requests here rather
		// than in the browser is what makes the tab counts the rows behind them, and stops a busy
		// workspace's last 200 calls from hiding a quiet channel's altogether.
		channel := r.URL.Query().Get("channel")
		// One namer for all three lists, so a conversation reads the same on every tab.
		names := b.conversationNamer(r.Context(), orgOf(r))
		turns := []TurnRow{}
		if !failedOnly {
			var err error
			turns, err = b.store.RecentTurns(r.Context(), orgOf(r), channel, limit, since)
			if err != nil {
				fail(w, err)
				return
			}
			for i := range turns {
				turns[i].ChannelName = names.usageName(r.Context(), turns[i].TeamID, turns[i].Channel)
				turns[i].TeamName = b.teamName(r.Context(), turns[i].TeamID)
			}
		}
		calls, _ := b.store.RecentToolCalls(r.Context(), orgOf(r), channel, limit, failedOnly, since)
		for i := range calls {
			calls[i].unmarkPrivate()
			calls[i].Result, calls[i].More = cutRunes(calls[i].Result, toolResultPreview)
			calls[i].ChannelName = names.name(r.Context(), calls[i].TeamID, calls[i].Channel)
		}
		audit, _ := b.store.ProxyAudits(r.Context(), orgOf(r), channel, limit, failedOnly, since)
		for i := range audit {
			audit[i].ChannelName = names.name(r.Context(), audit[i].TeamID, audit[i].Channel)
		}
		writeJSON(w, 200, map[string]any{"turns": turns, "tool_calls": calls, "proxy": audit})
	}))
	// one tool call with its whole result, for the console's "show full output"
	mux.HandleFunc("GET /api/tool-calls/{id}", b.requirePerm(PermActivityView, func(w http.ResponseWriter, r *http.Request) {
		call, err := b.store.ToolCall(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil {
			writeJSON(w, 404, map[string]any{"error": "no such tool call"})
			return
		}
		call.unmarkPrivate()
		writeJSON(w, 200, call)
	}))
	mux.HandleFunc("GET /api/activity.csv", b.requirePerm(PermActivityView, func(w http.ResponseWriter, r *http.Request) {
		// The file is what the reader is looking at: same channel, same window. An export that
		// ignored the filters would disagree with the table it sits above.
		turns, _ := b.store.RecentTurns(r.Context(), orgOf(r), r.URL.Query().Get("channel"), 5000, activitySince(r.URL.Query().Get("range")))
		// A bulk export is data leaving the system, and the audit log says who took it.
		b.audit(r, "export.activity", AuditEvent{TargetKind: "export", TargetID: "activity.csv",
			Details: auditDetails(map[string]any{"rows": len(turns), "channel": r.URL.Query().Get("channel"), "range": r.URL.Query().Get("range")})})
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", "attachment; filename=attesttag-activity.csv")
		cw := csv.NewWriter(w)
		cw.Write([]string{"time", "channel", "thread_ts", "model", "tokens_in", "tokens_out", "cost_usd"})
		for _, t := range turns {
			cw.Write([]string{t.At, t.Channel, t.ThreadTS, t.Model, fmt.Sprint(t.In), fmt.Sprint(t.Out), fmt.Sprintf("%.6f", t.Cost)})
		}
		cw.Flush()
	}))

	// console: static export served at /admin/, everything else redirects there
	fileServer := http.FileServer(http.FS(uiFS))
	mux.Handle("GET /admin/", http.StripPrefix("/admin/", spaHandler(uiFS, fileServer)))
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/admin/", http.StatusFound) })
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/admin/", http.StatusFound) })
}

// activitySince turns the console's range filter into a lower bound on created_at, in the same
// UTC windows OverviewStats and MonthSpend count in: today is midnight UTC, 7d is the last 168
// hours, month is the first of this month. An unknown or missing range means no bound, so the
// listing keeps its old "last N of everything" behaviour.
func activitySince(rng string) string {
	now := time.Now().UTC()
	switch rng {
	case "today":
		return now.Format("2006-01-02") + " 00:00:00"
	case "7d":
		return now.AddDate(0, 0, -7).Format("2006-01-02 15:04:05")
	case "30d":
		// The window the plan size is judged over, so the billing tile's tile-to-rows link lands
		// on the same set it counted.
		return now.AddDate(0, 0, -30).Format("2006-01-02 15:04:05")
	case "month":
		return now.Format("2006-01") + "-01 00:00:00"
	}
	return ""
}

// spaHandler serves a Next.js static export: exact file, else route/index.html, else index.html.
func spaHandler(fsys fs.FS, fallback http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.Trim(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(fsys, p); err == nil {
			fallback.ServeHTTP(w, r)
			return
		}
		if _, err := fs.Stat(fsys, path.Join(p, "index.html")); err == nil {
			r.URL.Path = "/" + p + "/"
			fallback.ServeHTTP(w, r)
			return
		}
		if _, err := fs.Stat(fsys, p+".html"); err == nil {
			r.URL.Path = "/" + p + ".html"
			fallback.ServeHTTP(w, r)
			return
		}
		r.URL.Path = "/"
		fallback.ServeHTTP(w, r)
	})
}

// ---- connection input handling ----

// accessJSON renders a request for the console, resolving ids to names the way the artifact and
// proxy listings do. The steps are rendered from the stored payload, exactly as the card was.
func (b *Bot) accessJSON(ctx context.Context, r *AccessRequest, full bool) map[string]any {
	m := map[string]any{
		"id": r.ID, "team_id": r.TeamID, "team_name": b.teamName(ctx, r.TeamID),
		"channel": r.Channel, "channel_name": b.channelName(ctx, r.TeamID, r.Channel),
		"thread_ts": r.ThreadTS, "requester": r.Requester, "requester_name": b.userName(ctx, r.TeamID, r.Requester),
		"what": r.What, "why": r.Why, "status": r.Status, "steps": len(r.Calls),
		"approvers": r.Approvers, "decided_by": r.DecidedBy, "decided_at": r.DecidedAt,
		"reason": r.Reason, "created_at": r.CreatedAt, "expires_at": r.ExpiresAt,
	}
	if r.DecidedBy != "" {
		m["decided_by_name"] = b.userName(ctx, r.TeamID, r.DecidedBy)
	}
	if full {
		m["ask"] = r.Ask
		m["plan"] = renderSteps(r.Calls)
		m["result"] = r.Result
	}
	return m
}

type connectionInput struct {
	BundleID     int64              `json:"-"`
	Name         string             `json:"name"`
	Preset       string             `json:"preset"`
	CredType     string             `json:"cred_type"`
	AllowedHosts []string           `json:"allowed_hosts"`
	PathPrefixes []string           `json:"path_prefixes"`
	Methods      []string           `json:"methods"`
	Headers      []Header           `json:"headers"`
	Writes       string             `json:"writes"`
	Notes        string             `json:"notes"`
	Status       string             `json:"status"`
	Options      []ConnectionOption `json:"options"`       // which parts of a multi-service preset were ticked, and where writing is allowed
	AllowGrants  *bool              `json:"allow_grants"`  // pointer: absent means "leave it as it is"
	Secret       *Secret            `json:"secret"`        // write-only
	HeaderValues map[string]string  `json:"header_values"` // write-only, extra header values
	TestCmd      *string            `json:"test_cmd"`      // repositories: the older single-command test override, '' = detect
	Recipe       *Recipe            `json:"recipe"`        // repositories: how to set up, build and test it; nil leaves it alone
	ClearRecipe  bool               `json:"clear_recipe"`  // back to detection
}

func (b *Bot) buildConnection(in *connectionInput, cur *Connection) (*Connection, *Secret, error) {
	pr := presetByID(in.Preset)
	if pr == nil {
		pr = presetByID("custom")
		in.Preset = "custom"
	}
	c := &Connection{BundleID: in.BundleID}
	if cur != nil {
		*c = *cur
	}
	if in.Name != "" {
		c.Name = in.Name
	}
	if c.Name == "" {
		c.Name = pr.Name
	}
	if cur == nil {
		c.Preset = in.Preset
		c.CredType = in.CredType
		if c.CredType == "" || c.CredType == "custom" {
			c.CredType = pr.CredType
		}
		if c.CredType == "custom" {
			c.CredType = "bearer"
		}
	}
	// What the admin ticked wins over both the explicit lists and the preset's defaults: the
	// checkboxes are the whole point, and a dialog that sends "Calendar only" alongside the
	// preset's three hosts would connect all three.
	optHosts, optPrefixes, optScopes := resolveOptions(pr, in.Options)
	// An empty list is not an absent one. A client that sent no options at all gets the whole
	// preset, as every client did before options existed; a console that sent an empty list is
	// an admin who unticked everything, and connecting all of Google in answer to that is the
	// one outcome nobody asked for.
	if len(pr.Options) > 0 && in.Options != nil && optScopes == "" {
		return nil, nil, fmt.Errorf("pick at least one part of %s to connect", pr.Name)
	}
	if optHosts != nil {
		c.AllowedHosts, c.PathPrefixes = optHosts, optPrefixes
	}
	if in.AllowedHosts != nil && optHosts == nil {
		c.AllowedHosts = in.AllowedHosts
	}
	if len(c.AllowedHosts) == 0 {
		c.AllowedHosts = append([]string{}, pr.Hosts...)
	}
	for i, h := range c.AllowedHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
		h = strings.TrimSuffix(h, "/")
		if h == "" || h == "*" || strings.Contains(h, "/") {
			return nil, nil, fmt.Errorf("invalid host %q: use a hostname, wildcard only as the leftmost label", h)
		}
		c.AllowedHosts[i] = h
	}
	if len(c.AllowedHosts) == 0 {
		return nil, nil, fmt.Errorf("at least one allowed host is required")
	}
	if in.PathPrefixes != nil && optHosts == nil {
		c.PathPrefixes = in.PathPrefixes
	}
	if len(c.PathPrefixes) == 0 && cur == nil && optHosts == nil {
		// Some services share a host with another service entirely — Calendar and Drive are
		// both www.googleapis.com — and the prefix is what keeps the two connections apart.
		c.PathPrefixes = append([]string{}, pr.PathPrefixes...)
	}
	// A prefix is compared literally against a URL path, and a URL path always begins with "/".
	// So one that does not — "api/v2", or a stray space in front of it — matches nothing at all:
	// the connection silently allows no path through, or, next to a wider prefix, quietly leans
	// on that one instead. It is refused here rather than repaired, because repairing it means
	// deciding on an admin's behalf that a rule which allows nothing should now allow a whole
	// path tree, and that is a widening of the allowlist nobody asked for.
	for _, pfx := range c.PathPrefixes {
		if strings.TrimSpace(pfx) == "" {
			return nil, nil, fmt.Errorf("a path prefix cannot be blank: remove it, or leave the list empty to allow every path")
		}
		if pfx != strings.TrimSpace(pfx) || !strings.HasPrefix(pfx, "/") {
			return nil, nil, fmt.Errorf("invalid path prefix %q: write it as a path beginning with %q and no surrounding spaces, otherwise it matches nothing the proxy ever sees", pfx, "/")
		}
	}
	if in.Methods != nil {
		c.Methods = in.Methods
	}
	if in.Headers != nil {
		c.Headers = in.Headers
	}
	if in.Writes != "" {
		switch in.Writes {
		case "confirm", "auto", "all":
			c.Writes = in.Writes
		default:
			return nil, nil, fmt.Errorf(`writes must be "confirm", "auto" or "all"`)
		}
	}
	if c.Writes == "" {
		c.Writes = "confirm"
	}
	if in.Notes != "" || cur == nil {
		c.Notes = in.Notes
	}
	if in.Status != "" {
		c.Status = in.Status
	}
	if c.Status == "" {
		c.Status = "active"
	}
	if in.AllowGrants != nil {
		c.AllowGrants = *in.AllowGrants
	}
	if c.AllowGrants {
		// A connection that can hand out access is the sharpest thing in the console, so the two
		// ways of blunting the gate are refused outright rather than left to a reviewer to spot.
		if c.Writes == "auto" {
			return nil, nil, fmt.Errorf(`a connection used for access grants cannot have writes "auto": that would mean grants run with nobody approving them`)
		}
		if len(c.Methods) == 0 || len(c.PathPrefixes) == 0 {
			return nil, nil, fmt.Errorf("a connection used for access grants needs explicit methods and path prefixes, so the allowlist is a list of paths rather than a whole host")
		}
	}
	if in.TestCmd != nil {
		cmd := strings.TrimSpace(*in.TestCmd)
		if len(cmd) > 512 {
			return nil, nil, fmt.Errorf("test_cmd must be a single command line of at most 512 characters")
		}
		// The worker runs this without a shell, so it is checked here the same way it will be
		// split there: a setting the console cannot save is not one the worker will trip over.
		if _, err := SplitCommand(cmd); err != nil {
			return nil, nil, err
		}
		c.TestCmd = cmd
	}
	if in.ClearRecipe {
		c.Recipe = nil
	} else if in.Recipe != nil {
		r, err := validateRecipeInput(in.Recipe)
		if err != nil {
			return nil, nil, err
		}
		c.Recipe = r
	}
	// secret
	var sec *Secret
	if in.Secret != nil || len(in.HeaderValues) > 0 {
		sec = in.Secret
		if sec == nil {
			sec = &Secret{}
		}
		if c.CredType == "header" && sec.HeaderName == "" {
			sec.HeaderName = pr.HeaderName
			if sec.HeaderName == "" {
				sec.HeaderName = "Authorization"
			}
		}
		if c.CredType == "gcp_sa" && sec.Scopes == "" {
			sec.Scopes = pr.Scopes
		}
		if c.CredType == "oauth_user" {
			// The console asks for the client id and secret; where to send people and what to
			// ask them for is the preset's business, not something an admin should retype.
			if sec.OAuth == nil {
				sec.OAuth = &OAuthState{ClientID: sec.ClientID, ClientSecret: sec.ClientSecret}
			}
			if sec.OAuth.AuthURL == "" {
				sec.OAuth.AuthURL = pr.AuthURL
			}
			if sec.OAuth.TokenURL == "" {
				sec.OAuth.TokenURL = pr.TokenURL
			}
			// The ticked boxes are what each person is asked to consent to, and the only place
			// per-service write control is enforced: allowed methods belong to the whole
			// connection, so "Calendar, but read only" is a connection Google refuses to book
			// through rather than one the proxy refuses to POST through.
			if optScopes != "" {
				sec.OAuth.Scopes = optScopes
			}
			if sec.OAuth.Scopes == "" {
				sec.OAuth.Scopes = pr.Scopes
			}
			sec.ClientID, sec.ClientSecret, sec.Token = "", "", "" // one home for the client, not two
		}
		if len(in.HeaderValues) > 0 {
			sec.Headers = in.HeaderValues
		}
		if err := validateSecret(c.CredType, sec); err != nil {
			return nil, nil, err
		}
	} else if cur == nil {
		return nil, nil, fmt.Errorf("a credential is required")
	}

	// The ticked boxes changed but nobody retyped the credential — the ordinary way an admin
	// adds a part to a connection that already works. The hosts and prefixes are plain columns
	// and have just been updated above; the scopes people are asked to consent to live inside
	// the sealed secret, so leaving it alone would let the two halves of one decision disagree:
	// a connection that routes Drive while no one is ever asked for a Drive scope, and a connect
	// page that drops the part because it is not in the ceiling. Re-seal the credential that is
	// already there with the scopes the boxes now imply, changing nothing else about it.
	if sec == nil && cur != nil && c.CredType == "oauth_user" && optScopes != "" {
		if existing, serr := b.proxy.secret(cur); serr == nil && existing != nil {
			if existing.OAuth == nil {
				existing.OAuth = &OAuthState{}
			}
			if existing.OAuth.Scopes != optScopes {
				existing.OAuth.Scopes = optScopes
				sec = existing
			}
		}
	}
	return c, sec, nil
}

func validateSecret(credType string, s *Secret) error {
	switch credType {
	case "bearer", "header", "query", "mcp":
		if credType != "mcp" && s.Token == "" {
			return fmt.Errorf("token is required")
		}
		if credType == "mcp" && s.MCPURL == "" {
			return fmt.Errorf("mcp server url is required")
		}
		if credType == "query" && s.HeaderName == "" {
			return fmt.Errorf("query parameter name is required")
		}
	case "basic":
		if s.User == "" || s.Password == "" {
			return fmt.Errorf("user and password are required")
		}
	case "gcp_sa":
		var sa map[string]any
		if err := json.Unmarshal([]byte(s.SAJSON), &sa); err != nil || sa["private_key"] == nil || sa["client_email"] == nil {
			return fmt.Errorf("paste the full service-account JSON key")
		}
	case "oauth2_cc":
		if s.ClientID == "" || s.ClientSecret == "" || s.TokenURL == "" {
			return fmt.Errorf("client id, client secret and token url are required")
		}
	case "oauth_user":
		// Only the organisation's OAuth client. There is nothing here that reaches an account:
		// the credential that does is granted per person, later, by that person.
		if s.OAuth == nil || s.OAuth.ClientID == "" || s.OAuth.ClientSecret == "" {
			return fmt.Errorf("client id and client secret are required")
		}
		if s.OAuth.AuthURL == "" || s.OAuth.TokenURL == "" {
			return fmt.Errorf("authorization url and token url are required")
		}
		if strings.TrimSpace(s.OAuth.Scopes) == "" {
			return fmt.Errorf("scopes are required")
		}
	case "github_app":
		// There is no credential to check: the installation id is a pointer, and the key it is
		// spent against lives in the environment. What has to be true is that it names one.
		if s.InstallationID == 0 {
			return fmt.Errorf("a GitHub App installation is required")
		}
	case "aws_sigv4":
		if s.AWSKeyID == "" || s.AWSSecret == "" {
			return fmt.Errorf("access key id and secret access key are required")
		}
	default:
		return fmt.Errorf("unsupported credential type %q", credType)
	}
	return nil
}

// refuseInstallFromBody keeps app-backed connections out of the generic connection endpoints.
//
// A github_app connection holds an installation id and no credential, and the token it spends is
// minted from this deployment's own app key. So the id is the whole of the authorisation, and
// unlike every other secret here it is not something only its owner could know — GitHub shows it
// in its settings URLs. A body that names one is therefore a claim about somebody else's account
// as easily as about its own.
//
// These connections are made in one place, connectRepo, from an installation this organisation
// has been shown to hold and scoped to a single repository. That is what is offered instead of
// checking ownership here: an app connection built from free-form fields would carry no
// repository, and a token minted with no repository scope reaches the whole installation.
//
// Editing one that already exists is left alone — the name, the writes setting, the notes are
// ordinary fields — so long as the body does not try to re-point it at a different installation.
func refuseInstallFromBody(in *connectionInput, cur *Connection) error {
	newInstall := in.Secret != nil && in.Secret.InstallationID != 0
	newlyApp := in.CredType == "github_app" && (cur == nil || cur.CredType != "github_app")
	if newInstall || newlyApp {
		return errors.New("a GitHub App installation is connected under Repositories, not from a connection's own fields")
	}
	return nil
}

func (b *Bot) sealSecret(s *Secret) ([]byte, error) {
	if s == nil {
		return nil, nil
	}
	raw, _ := json.Marshal(s)
	return b.sealer.Seal(raw)
}

// curlExample renders what the proxy will send, with the secret masked.
func curlExample(c *Connection) string {
	host := "api.example.com"
	if len(c.AllowedHosts) > 0 {
		host = c.AllowedHosts[0]
	}
	pr := presetByID(c.Preset)
	path := "/"
	if pr != nil {
		// The same call the Test button makes, so the example and the button cannot disagree
		// about which endpoint this connection is checked against.
		path, _ = testPath(pr, c)
	}
	auth := ""
	switch c.CredType {
	case "bearer", "mcp", "gcp_sa", "oauth2_cc", "oauth_user":
		auth = `-H "Authorization: Bearer ••••••••"`
	case "basic":
		auth = `-u "user:••••••••"`
	case "header":
		name, prefix := "Authorization", ""
		if pr != nil && pr.HeaderName != "" {
			name, prefix = pr.HeaderName, pr.HeaderPrefix
		}
		for _, h := range c.Headers {
			if h.Name != "" {
				name, prefix = h.Name, h.Prefix
			}
		}
		auth = fmt.Sprintf(`-H "%s: %s••••••••"`, name, prefix)
	case "query":
		auth = "  # plus ?<param>=•••••••• on the query string"
	case "aws_sigv4":
		auth = `-H "Authorization: AWS4-HMAC-SHA256 Credential=AKIA…/…, Signature=••••••••"`
	}
	return fmt.Sprintf("curl -X %s %s \\\n  https://%s%s", "GET", auth, host, strings.SplitN(path, "{", 2)[0])
}

// accessSummary lists resolved hosts for a scope with where each came from: through a
// bundle or as a one-off connection ("via"), attached here or inherited from the workspace.
func (b *Bot) accessSummary(ctx context.Context, orgID int64, sc *Scope, acc *Access) []map[string]any {
	out := []map[string]any{}
	if acc == nil {
		return out
	}
	bundleName := map[int64]string{}
	bs, _ := b.store.Bundles(ctx, orgID)
	for _, bd := range bs {
		bundleName[bd.ID] = bd.Name
	}
	for _, r := range acc.Rules {
		origin := "inherited from workspace"
		if sc.Kind == "workspace" || r.Rank == 2 {
			origin = "attached here"
		}
		via := "bundle"
		if r.Direct {
			via = "connection"
		}
		out = append(out, map[string]any{"host": strings.Join(r.Conn.AllowedHosts, ", "), "connection": r.Conn.Name,
			"bundle": bundleName[r.Conn.BundleID], "via": via, "origin": origin, "credential": r.Conn.CredType, "writes": r.Conn.Writes})
	}
	for _, d := range acc.Domains {
		out = append(out, map[string]any{"host": d.Host, "connection": "", "bundle": bundleName[d.BundleID], "via": "domain",
			"origin": "domain, no credential", "credential": "none"})
	}
	return out
}

// inheritedSummary lists what a scope carries but cannot edit here: instructions coming down
// from the workspace and from every attached bundle, and allow rules coming from Settings and
// the workspace. The console shows these beside the scope's own fields, because a channel's
// empty instructions box used to read as "the bot has no instructions" when in fact it had a
// page of them from the workspace and its bundles.
//
// email_auto_writes comes back resolved rather than as this row's own word, for the same reason:
// a channel reading "inherit" tells nobody whether its rules are in force for forwarded mail.
func (b *Bot) inheritedSummary(ctx context.Context, orgID int64, sc *Scope) map[string]any {
	instructions, rules := []map[string]any{}, []map[string]any{}
	autoWrites := "off"

	if global := b.settings.Get(ctx, orgID).AllowRules; len(global) > 0 {
		rules = append(rules, map[string]any{"source": "Settings", "kind": "settings", "rules": global})
	}

	// Bundle instructions, in the order Resolve concatenates them: the workspace's bundles
	// first, then the channel's own.
	seen := map[int64]bool{}
	addBundles := func(ids []int64, where string) {
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			bd, err := b.store.Bundle(ctx, orgID, id)
			if err != nil || bd == nil || strings.TrimSpace(bd.Instructions) == "" {
				continue
			}
			instructions = append(instructions, map[string]any{"source": bd.Name, "kind": "bundle",
				"where": where, "text": strings.TrimSpace(bd.Instructions)})
		}
	}

	// What comes down the chain: the account first, then this scope's own workspace. A channel
	// inherits both; a team scope inherits only the account; the account inherits nothing.
	inherit := func(from *Scope, kind string) {
		if from == nil || from.ID == sc.ID {
			return
		}
		// Walked outermost first, so a nearer link overrides a further one and this scope's own
		// word, applied after the walk, is final.
		switch from.EmailAutoWrites {
		case "on":
			autoWrites = "on"
		case "off":
			autoWrites = "off"
		}
		if t := strings.TrimSpace(from.Instructions); t != "" {
			instructions = append(instructions, map[string]any{"source": from.Name, "kind": kind,
				"where": kind, "text": t})
		}
		if len(from.AllowRules) > 0 {
			rules = append(rules, map[string]any{"source": from.Name, "kind": kind, "rules": from.AllowRules})
		}
		addBundles(from.BundleIDs, kind)
	}
	if sc.Kind != "workspace" {
		acct, _ := b.store.AccountScope(ctx, orgID)
		inherit(acct, "workspace")
	}
	if sc.Kind == "channel" {
		team, _ := b.store.TeamScope(ctx, orgID, sc.TeamID)
		inherit(team, "team")
	}
	addBundles(sc.BundleIDs, "here")
	switch sc.EmailAutoWrites {
	case "on":
		autoWrites = "on"
	case "off":
		autoWrites = "off"
	}

	return map[string]any{"instructions": instructions, "allow_rules": rules,
		"email_auto_writes": autoWrites}
}

// syncScopes refreshes the scope list from Slack: the account, then every connected
// workspace and the channels the bot is in there.
func (b *Bot) syncScopes(ctx context.Context) {
	for _, sl := range b.slacks.Active(ctx) {
		b.ensureAccountScope(ctx, sl.OrgID)
		b.syncScopesFor(ctx, sl.OrgID, sl.TeamID)
	}
}

// scopeSyncEvery is how often the scope-sync leader sweeps every workspace: the window
// syncScopesFor throttles each one to anyway.
const scopeSyncEvery = time.Minute

// scopeSyncLoop is scope-sync's work for as long as this instance holds its lease. syncScopes is
// one sweep that returns, and leaderLoop reads a function that returns as work that has finished:
// it gave the lease back and took it again five seconds later, over and over, logging "holding
// leader lease" each time — seventeen thousand lines a day, for what the throttle held to one
// sweep a minute.
func (b *Bot) scopeSyncLoop(ctx context.Context) {
	tick := time.NewTicker(scopeSyncEvery)
	defer tick.Stop()
	for {
		b.syncScopes(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// syncScopesOrg refreshes one organisation's workspaces. The console asks for its own scope list,
// and that must not turn into Slack calls on every other tenant's behalf: a page load in one
// organisation used to sweep every connected workspace on the deployment.
func (b *Bot) syncScopesOrg(ctx context.Context, orgID int64) {
	teams, err := b.store.Teams(ctx, orgID)
	if err != nil {
		return
	}
	b.ensureAccountScope(ctx, orgID)
	for _, t := range teams {
		if t.Status == "active" {
			b.syncScopesFor(ctx, orgID, t.TeamID)
		}
	}
}

// syncScopesFor refreshes one workspace. The throttle is per workspace: one team's sync must
// not silence another's, and GetConversationsForUser is Tier-2, so a sweep that hits the rate
// limit backs off for that team alone rather than aborting the lot.
func (b *Bot) syncScopesFor(ctx context.Context, orgID int64, teamID string) {
	b.syncScopesWithin(ctx, orgID, teamID, 60*time.Second)
}

// deferScopeSync holds this workspace's next sync off for the usual window. It is what keeps a
// channel the console has just left from walking straight back into the rail: conversations.list
// can still name the bot as a member for a moment after it leaves, and a sweep in that moment
// would read that as "invited back" and clear the mark somebody just set.
func (b *Bot) deferScopeSync(teamID string) {
	b.scopeSyncMu.Lock()
	defer b.scopeSyncMu.Unlock()
	if b.scopeSynced == nil {
		b.scopeSynced = map[string]time.Time{}
	}
	b.scopeSynced[teamID] = time.Now()
}

// syncScopesWithin is syncScopesFor with the throttle named by the caller. Somebody watching the
// onboarding walk for the invite they just typed is worth a shorter window than a page load is.
func (b *Bot) syncScopesWithin(ctx context.Context, orgID int64, teamID string, window time.Duration) {
	sl, err := b.slacks.For(ctx, teamID)
	if err != nil {
		slog.Warn("scope sync skipped", "team", teamID, "err", err)
		return
	}
	api, err := sl.slackAPI()
	if err != nil {
		// A Teams tenant's channels are synced as its activities arrive (msteamsSyncTeam): the
		// Bot Framework lists a team's channels only by that team's id, which only an activity
		// from inside the team carries. Nothing is wrong, so nothing is logged.
		if sl.Platform == platformSlack {
			slog.Warn("scope sync skipped", "team", teamID, "err", err)
		}
		return
	}
	b.scopeSyncMu.Lock()
	if b.scopeSynced == nil {
		b.scopeSynced = map[string]time.Time{}
	}
	if time.Since(b.scopeSynced[teamID]) < window {
		b.scopeSyncMu.Unlock()
		return
	}
	b.scopeSynced[teamID] = time.Now()
	b.scopeSyncMu.Unlock()

	name := sl.TeamName
	if team, err := api.GetTeamInfoContext(ctx); err == nil && team.Name != "" {
		name = team.Name
	}
	if name == "" {
		name = teamID
	}
	b.store.UpsertScope(ctx, orgID, "team", teamID, teamID, name)
	cursor := ""
	for {
		chans, next, err := api.GetConversationsForUserContext(ctx, &slack.GetConversationsForUserParameters{
			UserID: sl.BotUserID, Types: []string{"public_channel", "private_channel"}, Limit: 200, Cursor: cursor,
		})
		if err != nil {
			// Back this team off for the full window rather than retrying on the next page load.
			slog.Warn("scope sync", "team", teamID, "err", err)
			return
		}
		for _, ch := range chans {
			b.store.UpsertChannelScope(ctx, orgID, teamID, ch.ID, "#"+ch.Name, ch.IsPrivate)
		}
		if next == "" {
			break
		}
		cursor = next
	}
}

// testConnection backs the console's "Test connection" button. MCP connections get a real
// handshake with their server (initialize + tools/list) and a summary of the tools found;
// everything else makes the preset's check call through the proxy and reports the HTTP status.
func (b *Bot) testConnection(ctx context.Context, orgID int64, c *Connection) map[string]any {
	if c.CredType == "oauth_user" {
		// There is no organisation credential to try. Checking the OAuth client is well formed
		// is all that can be said here, and saying it plainly beats a green tick that means
		// nothing about whether anybody can actually reach their calendar.
		if _, err := b.clientTemplate(c); err != nil {
			return map[string]any{"ok": false, "error": err.Error()}
		}
		return map[string]any{"ok": true, "body": "The OAuth client is complete. Nothing is reachable until someone signs in: " +
			"ask the bot in Slack, or send them the connect link, and they grant their own account."}
	}
	if c.CredType == "mcp" {
		body, err := b.agent.mcp.test(ctx, orgID, c)
		if err != nil {
			return map[string]any{"ok": false, "error": err.Error()}
		}
		return map[string]any{"ok": true, "body": body}
	}
	status, body, err := b.proxy.Test(ctx, orgID, c)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	return map[string]any{"ok": status < 400, "status": status, "body": body}
}

// connectReposRequest is the body of POST /api/repos and POST /api/scopes/{id}/repos.
type connectReposRequest struct {
	Repo, Token, Writes string
	Repos               []string
	InstallationID      int64   `json:"installation_id"` // a GitHub App installation to reach them through
	ConnectionID        int64   `json:"connection_id"`   // whose sealed token to connect with
	ConnectionIDs       []int64 `json:"connection_ids"`  // saved repositories to attach as they are (scope only)
}

// handleConnectRepos connects repositories with a token, attached to sc when there is one and
// filed under Repositories either way; or, given connection_ids and a scope, attaches saved
// repository connections there without any token.
func (b *Bot) handleConnectRepos(w http.ResponseWriter, r *http.Request, sc *Scope) {
	var in connectReposRequest
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	if len(in.ConnectionIDs) > 0 {
		if sc == nil {
			bad(w, errors.New("saved repositories are added to a channel or workspace, not here"))
			return
		}
		done, failed, err := b.attachRepoConnections(r.Context(), orgOf(r), sc, in.ConnectionIDs)
		if err != nil {
			bad(w, err)
			return
		}
		b.changed(r.Context(), orgOf(r))
		writeJSON(w, 200, map[string]any{"repos": done, "repo": done[0], "failed": failed})
		return
	}
	want := in.Repos
	if in.Repo != "" {
		want = append(want, in.Repo)
	}
	repos, seen := []string{}, map[string]bool{}
	for _, raw := range want {
		repo, err := normalizeRepo(raw)
		if err != nil || repo == "" {
			bad(w, errors.New("repository must be owner/name or a github.com url"))
			return
		}
		if !seen[strings.ToLower(repo)] {
			seen[strings.ToLower(repo)] = true
			repos = append(repos, repo)
		}
	}
	if len(repos) == 0 {
		bad(w, errors.New("pick at least one repository"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(10+15*len(repos))*time.Second)
	defer cancel()
	auth, err := b.repoAuthFor(ctx, orgOf(r), in.Token, in.ConnectionID, in.InstallationID)
	if err != nil {
		bad(w, err)
		return
	}
	done, failed, err := b.connectRepos(ctx, orgOf(r), sc, repos, auth, in.Writes, adminFromCtx(r.Context()).Email)
	if err != nil {
		bad(w, err)
		return
	}
	b.changed(r.Context(), orgOf(r))
	writeJSON(w, 201, map[string]any{"repos": done, "repo": done[0], "failed": failed})
}
