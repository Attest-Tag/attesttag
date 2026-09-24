package app

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Settings are the non-secret knobs, stored in the settings table so the console can change
// them live. Env values (Config) are the defaults for keys that have never been saved.
type Settings struct {
	Model, HeavyModel, EmbedModel string
	MonthlyBudgetUSD              float64
	// The ceiling on the budget above, which the setting cannot lift: the free plan's fixed
	// budget, or the operator's per-organisation ceiling for a pro account (budgetCeiling).
	// 0 = none. EffectiveBudgetUSD is what is actually enforced, stamped at load so the console
	// shows the number the bot stops at rather than the number somebody typed.
	PlatformBudgetUSD  float64
	EffectiveBudgetUSD float64
	Plan               string // free | pro (plans.go)
	// What this account has bought (billing.go). Both false on a deployment with no Stripe
	// configured and on every account that has never paid for anything, which is what keeps a
	// self-host's turn exactly as cheap as it was.
	//
	// The credit BALANCE is deliberately not here. These two change when a webhook lands, so a
	// cache that is fifteen seconds stale is fine; the balance changes on every turn, and fifteen
	// seconds of it is fifteen seconds of spending money that is gone. budgetOK reads it live.
	BillingActive  bool // a subscription Stripe would bill, or one an operator comped
	CreditEnforced bool // credit has been bought or granted, so the floor binds
	// AllowanceActive is the other way the floor binds: the plan includes a monthly allowance and
	// the period it belongs to has not passed. The boolean is safe to cache for the same reason
	// the two above are — it changes at a period boundary, not on a turn — while the AMOUNT is
	// not here, for the same reason the balance is not.
	AllowanceActive bool
	// BillingUnknown means the billing record could not be read at all. Carried separately
	// because the alternative is treating "we cannot see it" as "there is nothing to see",
	// which would take every credit floor on the deployment off in silence. budgetOK refuses
	// on it, the same way it refuses when the month's spend cannot be read.
	BillingUnknown bool
	// Where a capped account is told to write for more, carried here so the refusal sentences
	// can be built without a Bot. Empty on a deployment that publishes no support address.
	SupportEmail                string
	Timezone                    string
	HistoryLimit, MaxToolRounds int
	BotName                     string
	UserRateLimit               int    // turns per user per hour, 0 = off
	AlertChannel                string // channel id for operational alerts
	// Answers longer than this are attached as a file, with their lead as the summary. The
	// default was 3,500, which filed almost every report a routine wrote — a brief is meant to
	// be read where it lands, not downloaded. A message has a ceiling of its own
	// (messageAnswerCap) that no setting can raise, so an answer that outgrows a Slack message
	// is filed whatever this says. 0 means never.
	LongAnswerChars int
	// ChannelModels is the short list a channel may pick from on its Configure page, on top of
	// Default and Advanced. Empty means those two only. It exists because the console can set
	// any model by name and a channel cannot: free text on a page anyone in the channel can open
	// is a way to spend money on a model nobody chose, so an admin names the alternatives and
	// the channel picks among them.
	ChannelModels    []string
	ShowCost         bool     // show what a turn cost in the reply footer
	AllowRules       []string // auto mode allow rules: writes these sentences cover run without a human confirm
	AllowSelfApprove bool     // let a requester approve their own access request (testing)
	ConfigVersion    int64    // bumped on every bundle/scope/setting change

	// How members of this organisation are allowed into the console, and whether a password
	// sign-in has to carry a second factor. Both are read on every request, not just at sign-in:
	// a policy that only applied to new sessions would leave the people it was aimed at signed in.
	AuthPolicy       string // any | password | slack
	RequireTwoFactor bool

	// Whose messages the bot answers in this organisation's Slack workspaces. A workspace holds
	// guests, Slack Connect members and contractors, and which of them may reach this tenant's
	// connections is this tenant's call — it used to be one ALLOWED_EMAIL_DOMAINS list applied to
	// every customer at once, so one operator setting decided it for all of them. Empty means
	// anyone in the connected workspaces, which is how a fresh organisation starts.
	AllowedEmailDomains []string
	// Whether guests and Slack Connect members may talk to the bot at all. Off by default: a
	// single-channel guest, or somebody on the other side of a shared channel, is in the room
	// but not in this organisation, and the domain list above cannot tell them from a member
	// when Slack returns no email for them. The list narrows members; this admits non-members.
	AllowExternalUsers bool

	// The digging lane (investigations.go). Enabled by default: it is the seam that keeps a
	// long question out of the reply path, and switching it off only sends those questions
	// back into the path they were taken out of.
	Investigations       bool
	InvestigationRounds  int // tool rounds one investigation may spend
	InvestigationMinutes int // and the clock it spends them in
	InvestigationMaxOpen int // queued plus running, per organisation

	// What one scheduled run is given. A routine is not a reply: nobody is waiting on it, it
	// has no thread to narrate into, and the work it was written for — read a system, cross-
	// check it, write somewhere else — is several times a conversation's. So it carries its own
	// budget rather than the reply budget, the way an investigation does.
	RoutineRounds  int // tool rounds one run may spend
	RoutineMinutes int // and the clock it spends them in

	// How this organisation reaches the public web (web_providers.go). Empty or "builtin" is
	// the scrape-and-fetch path web_search and fetch_url have always used; naming a provider
	// here puts both tools behind Tavily, Firecrawl or Cloudflare instead. The keys are not
	// settings — they are sealed in web_keys — so the list of providers that have one is all the
	// console is told about them.
	WebProvider string
	// The page reader, when it is not the search engine. Brave and Serper search and cannot
	// read a page; Cloudflare reads a page and cannot search. So the two halves are two
	// choices, and an empty one means "whoever is searching, if it can read too".
	WebFetchProvider string
	WebAccountID     string   // Cloudflare account id; unused by the others
	WebKeyProviders  []string // providers this organisation holds a key for

	// OwnKey is the organisation's own model endpoint, as everything but the resolver needs to
	// know it: never the key. Every member of the organisation can read this struct through
	// GET /api/settings, so it is kept out of every JSON encoding of it; the endpoint is shown on
	// its own route, to settings.manage only (model_keys.go).
	OwnKey OwnKey `json:"-"`

	// Fix-job worker knobs (jobs.go); WORKER_MODE itself is deploy-time.
	WorkerEngine             string  // qwen_code | fake (native, claude_code later)
	WorkerModel              string  // '' = heavy model
	WorkerJobBudgetUSD       float64 // per-job model spend cap
	WorkerTimeoutMinutes     int
	WorkerMaxJobs            int    // concurrent jobs across the workspace
	WorkerBranchPrefix       string // the organisation's own convention: a list ("feature/, bugfix/, hotfix/"), or "none"
	WorkerBranchSuffix       string // the tail that marks a branch as the worker's
	WorkerEventRetentionDays int
	WorkerAllowRules         bool // may an allow rule dispatch a job without a human Confirm

	// How many days of turns, usage, tool calls, proxy audit and artifacts this organisation
	// keeps. 0 — the default — keeps everything: a deployment that quietly started deleting its
	// own audit trail on upgrade would be the worst kind of surprise (retention.go).
	DataRetentionDays int
	// The same for the audit log (audit.go), kept as its own number so that shortening what
	// the bot did cannot quietly shorten the record of what the people did. 0 keeps everything.
	AuditRetentionDays int
}

// worker_pr_draft is not among them. It was a console switch, and it never did anything: the worker
// opens every pull request as a draft (internal/worker/run.go), by the owner's decision. A row an
// organisation saved while the switch existed is simply never read.
var settingKeys = []string{"model", "heavy_model", "embed_model", "monthly_budget_usd", "timezone",
	"auth_policy", "require_two_factor", "allowed_email_domains", "allow_external_users",
	"history_limit", "max_tool_rounds", "bot_name", "user_rate_limit", "alert_channel",
	"long_answer_chars", "channel_models", "show_cost", "allow_rules", "access_allow_self_approve", "config_version",
	"investigations", "investigation_rounds", "investigation_minutes", "investigation_max_open",
	"routine_rounds", "routine_minutes",
	"web_provider", "web_fetch_provider", "web_account_id",
	"worker_engine", "worker_model", "worker_job_budget_usd", "worker_timeout_minutes", "worker_max_jobs",
	"worker_branch_prefix", "worker_branch_suffix", "worker_event_retention_days", "worker_allow_rules",
	"data_retention_days", "audit_retention_days"}

var workerEngines = []string{"qwen_code", "fake"}

// ---- sign-in policy ----
//
// Which credentials get somebody into this organisation's console. Held as an ordinary setting
// so it is one row an admin can change and one value every check reads, rather than a flag
// duplicated across the two sign-in paths.
const (
	AuthPolicyAny       = "any"       // a password, Slack, Microsoft or the organisation's IdP, whichever they hold
	AuthPolicyPassword  = "password"  // email and password only; everything else is refused
	AuthPolicySlack     = "slack"     // Slack only; passwords stop working, resets included
	AuthPolicyMicrosoft = "microsoft" // Microsoft only, the way Slack only is for a workspace on Slack
	AuthPolicySSO       = "sso"       // the organisation's identity provider only; it is the roster too
)

var authPolicies = []string{AuthPolicyAny, AuthPolicyPassword, AuthPolicySlack, AuthPolicyMicrosoft, AuthPolicySSO}

// authPolicyOf reads a stored value defensively: anything unrecognised is the permissive
// default, matching what an empty setting means. A typo in the database must not lock a
// workspace out of its own console.
func authPolicyOf(v string) string {
	if slices.Contains(authPolicies, v) {
		return v
	}
	return AuthPolicyAny
}

// The questions every caller actually asks. Each policy names the one credential it
// accepts, so every other one is refused — written out rather than as "not the others", because
// adding a fourth credential to a "not" is how the permissive answer gets given by accident.
func (s Settings) allowsPassword() bool {
	return s.AuthPolicy == AuthPolicyAny || s.AuthPolicy == AuthPolicyPassword
}
func (s Settings) allowsSlack() bool {
	return s.AuthPolicy == AuthPolicyAny || s.AuthPolicy == AuthPolicySlack
}
func (s Settings) allowsMicrosoft() bool {
	return s.AuthPolicy == AuthPolicyAny || s.AuthPolicy == AuthPolicyMicrosoft
}
func (s Settings) allowsSSO() bool {
	return s.AuthPolicy == AuthPolicyAny || s.AuthPolicy == AuthPolicySSO
}

// allowsVia answers for the way a session was created. An empty method is a session from before
// the column existed: it is left alone rather than signed out mid-policy-change.
func (s Settings) allowsVia(via string) bool {
	switch via {
	case "password", "signup":
		return s.allowsPassword()
	case "slack":
		return s.allowsSlack()
	case ProviderMicrosoft:
		return s.allowsMicrosoft()
	case "sso":
		return s.allowsSSO()
	}
	return true
}

// validateSecuritySetting checks a sign-in policy value before it is stored. The lock-yourself-out
// guard is not here: it needs to know who is asking, so it lives with the handler (admin_api.go).
func validateSecuritySetting(k, v string) error {
	// Retention deletes an organisation's own audit trail, so it is validated with the rest of
	// the settings that have consequences rather than with the worker knobs — validateWorkerSetting
	// only ever sees keys beginning with worker_, which this does not.
	if k == "data_retention_days" {
		if v == "" || v == "0" { // the default: keep everything
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < retentionFloor || n > 3650 {
			// The floor is not 1: a number below a week is more likely a typo than a policy,
			// and what a typo costs here is the audit trail.
			return fmt.Errorf("data_retention_days must be 0 (keep everything) or between %d and 3650", retentionFloor)
		}
		return nil
	}
	// The audit log's own policy, kept apart from the activity tables' so that shortening one
	// cannot quietly shorten the other. Its floor is a month, not a week: it is the record of
	// who changed what, and the first person to want it is usually asking about last month.
	if k == "audit_retention_days" {
		if v == "" || v == "0" {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < auditRetentionFloor || n > 3650 {
			return fmt.Errorf("audit_retention_days must be 0 (keep everything) or between %d and 3650", auditRetentionFloor)
		}
		return nil
	}
	switch k {
	case "auth_policy":
		if !slices.Contains(authPolicies, v) {
			return fmt.Errorf("auth_policy must be one of %s", strings.Join(authPolicies, ", "))
		}
	case "require_two_factor", "allow_external_users":
		if v != "0" && v != "1" {
			return fmt.Errorf("%s must be 0 or 1", k)
		}
	case "allowed_email_domains":
		for _, d := range parseEmailDomains(v) {
			if !emailDomainRe.MatchString(d) {
				return fmt.Errorf("%q is not an email domain (something like example.com)", d)
			}
		}
	case "timezone":
		if v != "" && !validTimezone(v) {
			return fmt.Errorf("%q is not a time zone this server knows", v)
		}
	}
	return nil
}

// validateWorkerSetting checks a worker_* value before it is stored.
func validateWorkerSetting(k, v string) error {
	num := func(min, max float64) error {
		f, err := strconv.ParseFloat(v, 64)
		// Written as the range it must be inside, not the two it must not be outside: NaN fails
		// every comparison, so "< min || > max" waves it through, and a NaN budget then mints an
		// uncapped key downstream.
		if err != nil || !(f >= min && f <= max) {
			return fmt.Errorf("%s must be a number between %g and %g", k, min, max)
		}
		return nil
	}
	switch k {
	case "worker_engine":
		for _, e := range workerEngines {
			if v == e {
				return nil
			}
		}
		return fmt.Errorf("worker_engine must be one of %s", strings.Join(workerEngines, ", "))
	case "worker_job_budget_usd":
		return num(0.1, 100)
	case "worker_timeout_minutes":
		return num(5, 60)
	case "worker_max_jobs":
		return num(1, 10)
	case "worker_event_retention_days":
		return num(1, 365)
	case "worker_allow_rules":
		if v != "0" && v != "1" {
			return fmt.Errorf("%s must be 0 or 1", k)
		}
	case "worker_branch_prefix":
		// Empty is an answer: an organisation with no branch convention of its own gets a branch
		// that starts at fix-. A house rule that names a prefix per kind of change ("feature/,
		// bugfix/") is a list, and every entry has to survive git check-ref-format.
		// "none" and empty are both answers — empty takes the default, "none" turns the
		// convention off — so neither reaches the ref-format check below.
		list := branchPrefixes(v)
		if len(list) > maxBranchPrefixes {
			return fmt.Errorf("worker_branch_prefix takes at most %d prefixes", maxBranchPrefixes)
		}
		for _, p := range list {
			if !branchPrefixRe.MatchString(p) {
				return fmt.Errorf("worker_branch_prefix must look like feature/ or bugfix/ — one, or a comma-separated list, or \"none\" for no convention (letters, digits, . _ - and / between segments); %q is not", p)
			}
		}
	case "worker_branch_suffix":
		// Not optional: the suffix is how the worker recognises a branch as its own, and the
		// push guard leans on it.
		if !branchSuffixRe.MatchString(v) {
			return fmt.Errorf("worker_branch_suffix must look like %s (letters, digits, . _ and -, no slash)", defaultBranchSuffix)
		}
	case "worker_model":
		if len(v) > 200 {
			return fmt.Errorf("worker_model is too long")
		}
	}
	return nil
}

// validateModelSetting holds the list of models offered to channels to its cap. It is not part of
// validateWorkerSetting, which only ever sees keys beginning with worker_: the check sat there once
// and never ran, so the console's picker was all that kept the list to twelve.
func validateModelSetting(k, v string) error {
	if k == "channel_models" && len(parseModelList(v)) > maxChannelModels {
		return fmt.Errorf("at most %d models can be offered to channels", maxChannelModels)
	}
	return nil
}

// A branch prefix may carry slashes — hotfix/, feature/bot/ — but no segment may start with a
// dot, which is what git check-ref-format refuses. A branch suffix is appended rather than
// prepended, so it may not carry a slash (that would make it a path segment, not a tail) and may
// not end in a dot, which git refuses at the end of a ref.
// maxBranchPrefixes caps the conventions on offer: a house rule names two or three kinds of
// change, and a longer list is a paste, not a policy.
const maxBranchPrefixes = 8

var (
	branchPrefixRe = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*(/[A-Za-z0-9_-][A-Za-z0-9._-]*)*/?$`)
	branchSuffixRe = regexp.MustCompile(`^[A-Za-z0-9._-]*[A-Za-z0-9_-]$`)
)

// validateWebSetting checks the Web tab's two non-secret values. The key itself goes in through
// its own endpoint and is never one of these.
func validateWebSetting(k, v string) error {
	switch k {
	case "web_provider", "web_fetch_provider":
		if v == "" {
			// web_provider cleared is the built-in path; web_fetch_provider cleared means the
			// reader follows whoever is searching. Both are answers, not omissions.
			return nil
		}
		if _, ok := webProviderByID(v); !ok {
			names := make([]string, 0, len(webProviders))
			for _, p := range webProviders {
				names = append(names, p.ID)
			}
			return fmt.Errorf("%s must be one of %s", k, strings.Join(names, ", "))
		}
	case "web_account_id":
		if len(v) > 64 || strings.ContainsAny(v, "/?#& ") {
			return fmt.Errorf("web_account_id is an account identifier, not a URL")
		}
	}
	return nil
}

// ---- time zone ----

// validTimezone reports whether s names an IANA zone this build can load. The empty string and
// "Local" are refused: neither says where an organisation is, and "Local" would mean the
// container's zone, which is UTC on Cloud Run and the developer's desk elsewhere.
func validTimezone(s string) bool {
	if s == "" || s == "Local" || len(s) > 64 {
		return false
	}
	_, err := time.LoadLocation(s)
	return err == nil
}

// seedTimezone stamps a newly founded organisation with the zone its founder signed up from, so
// "today", the clock in prompts and every routine they create mean what they mean by them
// instead of whatever TZ_NAME happens to say. The first loadable candidate wins.
//
// Only the sign-up that founds an organisation calls this. Somebody accepting an invitation
// later must not move everybody else's clock, and after this the zone is an ordinary setting:
// the console's Timezone field overrides it and nothing seeds it again.
func (b *Bot) seedTimezone(ctx context.Context, orgID int64, candidates ...string) {
	for _, tz := range candidates {
		if !validTimezone(tz) {
			continue
		}
		if err := b.store.PutSetting(ctx, orgID, "timezone", tz); err != nil {
			slog.Warn("could not record the sign-up time zone", "org", orgID, "tz", tz, "err", err)
			return
		}
		b.settings.Invalidate(orgID)
		slog.Info("org time zone taken from sign-up", "org", orgID, "tz", tz)
		return
	}
}

// emailDomainRe is what one entry of allowed_email_domains has to look like once parsed: a
// hostname with a dot in it. parseEmailDomains has already lowercased it and dropped an "@".
var emailDomainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// publicMailDomains are the consumer providers a founder may well sign up from. Their domain
// says nothing about who works together, so it is never seeded as an allowlist: "gmail.com"
// would admit every Gmail user, which is the opposite of what the list is for.
var publicMailDomains = map[string]bool{
	"gmail.com": true, "googlemail.com": true, "outlook.com": true, "hotmail.com": true, "live.com": true,
	"msn.com": true, "yahoo.com": true, "yahoo.co.uk": true, "ymail.com": true, "icloud.com": true,
	"me.com": true, "mac.com": true, "aol.com": true, "proton.me": true, "protonmail.com": true,
	"pm.me": true, "gmx.com": true, "gmx.de": true, "mail.com": true, "zoho.com": true,
	"fastmail.com": true, "hey.com": true, "yandex.com": true, "qq.com": true, "163.com": true,
}

// seedEmailDomain starts a new organisation's bot allowlist at its founder's own email domain.
// The list is theirs to edit under Settings → Security, but an organisation that never opens
// Settings should not be answering everyone a shared channel brings in. A consumer address
// seeds nothing, and a list that is already there is left alone.
func (b *Bot) seedEmailDomain(ctx context.Context, orgID int64, email string) {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return
	}
	domain := strings.ToLower(strings.TrimSpace(email[at+1:]))
	if domain == "" || publicMailDomains[domain] || !emailDomainRe.MatchString(domain) {
		return
	}
	if all, _ := b.store.AllSettings(ctx, orgID); all["allowed_email_domains"] != "" {
		return
	}
	if err := b.store.PutSetting(ctx, orgID, "allowed_email_domains", domain); err != nil {
		slog.Warn("could not seed the bot's email domain", "org", orgID, "domain", domain, "err", err)
		return
	}
	b.settings.Invalidate(orgID)
	slog.Info("bot allowlist seeded from sign-up", "org", orgID, "domain", domain)
}

type settingsCache struct {
	mu    sync.Mutex
	store *Store
	cfg   Config
	// Keyed by organisation: settings are what the bot's behaviour is made of — the model, the
	// budget, the allow rules — so one shared entry would apply one customer's configuration to
	// another's Slack.
	cached map[int64]cachedSettings
}

type cachedSettings struct {
	settings Settings
	fetched  time.Time
}

func newSettingsCache(st *Store, cfg Config) *settingsCache {
	return &settingsCache{store: st, cfg: cfg, cached: map[int64]cachedSettings{}}
}

// Get returns settings, refreshed from the DB at most every 15 s.
func (c *settingsCache) Get(ctx context.Context, orgID int64) Settings {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.cached[orgID]; ok && time.Since(e.fetched) < 15*time.Second {
		return e.settings
	}
	// Not on the caller's cancellation. What is loaded here is cached for everybody, and a request
	// that gave up halfway — a page navigated away from — used to leave its failed reads behind as
	// the organisation's settings for fifteen seconds: the plan read as free, which blocks a key
	// only the enterprise plan allows.
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	loaded := c.load(lctx, orgID)
	// A key whose row could not be read stops the organisation's model calls; caching that would
	// stop them for fifteen seconds over one failed read, so the next call reads again instead.
	if !loaded.OwnKey.Unknown {
		c.cached[orgID] = cachedSettings{settings: loaded, fetched: time.Now()}
	}
	return loaded
}

// Invalidate drops one organisation's cached settings, so a save takes effect on the next turn
// rather than within fifteen seconds.
func (c *settingsCache) Invalidate(orgID int64) {
	c.mu.Lock()
	delete(c.cached, orgID)
	c.mu.Unlock()
}

func (c *settingsCache) load(ctx context.Context, orgID int64) Settings {
	kv, _ := c.store.AllSettings(ctx, orgID)
	get := func(k, def string) string {
		if v, ok := kv[k]; ok && v != "" {
			return v
		}
		return def
	}
	f := func(k string, def float64) float64 {
		if v, err := strconv.ParseFloat(get(k, ""), 64); err == nil {
			return v
		}
		return def
	}
	i := func(k string, def int) int {
		if v, err := strconv.Atoi(get(k, "")); err == nil {
			return v
		}
		return def
	}
	cv, _ := strconv.ParseInt(get("config_version", "0"), 10, 64)
	rules, _ := parseAllowRules(get("allow_rules", ""))
	if rules == nil {
		rules = []string{}
	}
	// The env var remains the default for a single-tenant deployment that already sets it; an
	// organisation that stores its own list overrides it.
	domains := parseEmailDomains(get("allowed_email_domains", ""))
	channelModels := parseModelList(get("channel_models", ""))
	if len(domains) == 0 {
		domains = allowedEmailDomains()
	}
	// The plan is the organisation's, not a setting: nothing a member can save changes it.
	plan, grant, planErr := c.store.orgPlanRead(ctx, orgID)
	plan = planOf(plan)
	// Only asked on a deployment that sells plans. Everywhere else this is the zero value and no
	// query runs at all.
	var bill BillingFacts
	if c.cfg.BillingEnabled() {
		bill = c.store.BillingFacts(ctx, orgID)
	}
	s := Settings{
		Model:             get("model", c.cfg.Model),
		HeavyModel:        get("heavy_model", c.cfg.HeavyModel),
		EmbedModel:        get("embed_model", c.cfg.EmbedModel),
		MonthlyBudgetUSD:  f("monthly_budget_usd", c.cfg.MonthlyBudgetUSD),
		PlatformBudgetUSD: budgetCeiling(plan, grant, c.cfg, bill.Active && bill.CreditEnforced),
		Plan:              plan,
		BillingActive:     bill.Active,
		CreditEnforced:    bill.CreditEnforced,
		AllowanceActive:   bill.AllowanceActive,
		BillingUnknown:    bill.Unknown,
		SupportEmail:      c.cfg.SupportEmail,
		Timezone:          get("timezone", c.cfg.Timezone),
		HistoryLimit:      i("history_limit", c.cfg.HistoryLimit),
		MaxToolRounds:     i("max_tool_rounds", c.cfg.MaxToolRounds),
		BotName:           get("bot_name", c.cfg.BotName),
		UserRateLimit:     i("user_rate_limit", 60),
		AlertChannel:      get("alert_channel", ""),
		LongAnswerChars:   i("long_answer_chars", 16000),
		ShowCost:          get("show_cost", "1") == "1",
		AllowRules:        rules,
		AllowSelfApprove:  get("access_allow_self_approve", os.Getenv("ACCESS_ALLOW_SELF_APPROVE")) == "1",
		ConfigVersion:     cv,

		AuthPolicy:          authPolicyOf(get("auth_policy", AuthPolicyAny)),
		RequireTwoFactor:    get("require_two_factor", "0") == "1",
		AllowedEmailDomains: domains,
		AllowExternalUsers:  get("allow_external_users", "0") == "1",
		ChannelModels:       channelModels,

		WebProvider:      get("web_provider", webProviderBuiltin),
		WebFetchProvider: get("web_fetch_provider", ""),
		WebAccountID:     get("web_account_id", ""),
		WebKeyProviders:  c.store.WebKeyProviders(ctx, orgID),

		Investigations:       get("investigations", "1") == "1",
		InvestigationRounds:  i("investigation_rounds", 40),
		InvestigationMinutes: i("investigation_minutes", 12),
		InvestigationMaxOpen: i("investigation_max_open", 2),

		// cmp.Or, not the config value alone: a Config built in code rather than from the
		// environment leaves these zero, and zero here would clamp every routine to the floor.
		RoutineRounds:  i("routine_rounds", cmp.Or(c.cfg.RoutineRounds, defaultRoutineRounds)),
		RoutineMinutes: i("routine_minutes", cmp.Or(c.cfg.RoutineMinutes, defaultRoutineMinutes)),

		WorkerEngine:             get("worker_engine", "qwen_code"),
		WorkerModel:              get("worker_model", ""),
		WorkerJobBudgetUSD:       f("worker_job_budget_usd", 3),
		WorkerTimeoutMinutes:     i("worker_timeout_minutes", 45),
		WorkerMaxJobs:            i("worker_max_jobs", 2),
		WorkerBranchPrefix:       get("worker_branch_prefix", defaultBranchPrefix),
		WorkerBranchSuffix:       get("worker_branch_suffix", defaultBranchSuffix),
		WorkerEventRetentionDays: i("worker_event_retention_days", 30),
		DataRetentionDays:        i("data_retention_days", 0),
		AuditRetentionDays:       i("audit_retention_days", 0),
		WorkerAllowRules:         get("worker_allow_rules", "0") == "1",
	}
	s.OwnKey = c.ownKey(ctx, orgID, plan, planErr != nil)
	if s.OwnKey.Active() {
		// The deployment's defaults name models on the deployment's endpoint, which the
		// organisation's own may not serve: a choice it never made falls to its endpoint's default,
		// and its documents are embedded with the model it chose there, or not at all.
		if get("model", "") == "" {
			s.Model = s.OwnKey.Ref.DefaultModel
		}
		if get("heavy_model", "") == "" {
			s.HeavyModel = s.OwnKey.Ref.DefaultModel
		}
		s.EmbedModel = s.OwnKey.Ref.EmbedModel
		// Its spend is its own provider's to bill, so none of the ceilings this deployment puts
		// on its own key — the free plan's, the per-organisation one, a deal's — applies to it.
		// Its own monthly budget still does; one it never set is none, rather than the
		// deployment's default, which exists to protect the deployment's key.
		s.PlatformBudgetUSD = 0
		if get("monthly_budget_usd", "") == "" {
			s.MonthlyBudgetUSD = 0
		}
	}
	s.EffectiveBudgetUSD = s.EffectiveBudget()
	return s
}

// ownKey reads the organisation's own model endpoint. A read that fails is Unknown, which stops
// the organisation's model calls for as long as it lasts: the alternative is reading "no key",
// which sends an organisation that brought one to the deployment's provider.
//
// A plan that could not be read is the same: which plan it is decides whether the key may be used,
// and "free" is not an answer to a failed read.
func (c *settingsCache) ownKey(ctx context.Context, orgID int64, plan string, planUnknown bool) OwnKey {
	ref, err := c.store.ModelKeyRef(ctx, orgID)
	if err != nil {
		slog.Error("organisation model key unreadable; its model calls are refused until it is", "org", orgID, "err", err)
		return OwnKey{Unknown: true}
	}
	if ref == nil {
		return OwnKey{}
	}
	if planUnknown {
		return OwnKey{Present: true, Unknown: true, Ref: *ref}
	}
	return OwnKey{Present: true, Allowed: c.cfg.ownKeyAllowed(plan), Ref: *ref}
}

// ---- store side ----

func (s *Store) AllSettings(ctx context.Context, orgID int64) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `select key, value from settings where org_id=?`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

// Setting reads one stored value, "" when this organisation has never set it. AllSettings is
// the right call for a page full of them; this is for the single flag a hot path wants.
func (s *Store) Setting(ctx context.Context, orgID int64, key string) string {
	var v string
	s.db.QueryRowContext(ctx, `select value from settings where org_id=? and key=?`, orgID, key).Scan(&v)
	return v
}

func (s *Store) PutSetting(ctx context.Context, orgID int64, key, value string) error {
	_, err := s.db.ExecContext(ctx, `insert into settings (org_id, key, value, updated_at) values (?, ?, ?, ?)
		on conflict(org_id, key) do update set value=excluded.value, updated_at=excluded.updated_at`, orgID, key, value, now())
	return err
}

// BumpConfigVersion signals running sessions that scopes/bundles/settings changed.
//
// The new value is a timestamp rather than the old value plus one. Two reasons, and the second
// is the one that made the change necessary. It is only ever compared for equality — the
// resolver and the proxy hold it as a cache key and refetch when it differs — so it has to
// change, not to count. And `cast(settings.value as integer)` is a silent 0 in SQLite and an
// error in Postgres when the text is not a number, which is a dialect difference sitting
// underneath a cache that decides which credentials a channel may reach.
//
// It also removes a read-modify-write: two instances bumping at once both write a fresh value
// instead of racing to read the same old one and write the same new one.
func (s *Store) BumpConfigVersion(ctx context.Context, orgID int64) {
	s.db.ExecContext(ctx, `insert into settings (org_id, key, value, updated_at) values (?, 'config_version', ?, ?)
		on conflict(org_id, key) do update set value=excluded.value, updated_at=excluded.updated_at`,
		orgID, strconv.FormatInt(time.Now().UnixNano(), 10), now())
}

// PutSettings commits a fully validated payload as one change.
func (s *Store) PutSettings(ctx context.Context, orgID int64, values map[string]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, value := range values {
		if _, err := tx.ExecContext(ctx, `insert into settings (org_id, key, value, updated_at) values (?, ?, ?, ?) on conflict(org_id,key) do update set value=excluded.value, updated_at=excluded.updated_at`, orgID, key, value, now()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// EffectiveBudget is the monthly model spend this organisation may put on the shared key: its
// own setting, unless the operator's per-organisation ceiling is lower — or the setting is
// zero, which used to mean "no limit" and now means "the ceiling". A tenant's preference cannot
// lift a limit the operator pays for.
func (s Settings) EffectiveBudget() float64 {
	if s.PlatformBudgetUSD > 0 && (s.MonthlyBudgetUSD <= 0 || s.MonthlyBudgetUSD > s.PlatformBudgetUSD) {
		return s.PlatformBudgetUSD
	}
	return s.MonthlyBudgetUSD
}

// maxChannelModels bounds the list. The Configure page draws it as a dropdown a channel member
// reads at a glance, and a list longer than this is a decision nobody makes well.
const maxChannelModels = 12

// parseModelList reads the stored comma- or newline-separated ids, keeping the admin's order
// (it is the order the dropdown draws) and dropping blanks and repeats.
func parseModelList(s string) []string {
	var out []string
	for _, m := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' || r == '\t' }) {
		m = strings.TrimSpace(m)
		if m == "" || len(m) > 200 || slices.Contains(out, m) {
			continue
		}
		out = append(out, m)
	}
	return out
}
