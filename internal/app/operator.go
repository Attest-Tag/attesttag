package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The operator's API: the handful of things the person running the deployment does to an
// account that no member of it may do to itself. Today that is one thing — moving an account
// between plans (plans.go) — reachable two ways: JSON routes for curl, and a page the support
// email links to, so the answer to "please raise our budget" is one click on the link and one
// on a button.
//
// Both are authenticated by OPERATOR_SECRET, never by a console session: a session belongs to
// a member of one organisation, and a member must not be able to move their own account to a
// plan. The JSON routes take the secret as a bearer. The page takes it typed into a form once
// and then from a cookie derived from it, so the requester who wrote the email — and can read
// the link in it — gets nothing from clicking it themselves. Unset, none of this exists: the
// routes answer 404 like any path the deployment does not serve.

const (
	minOperatorSecret       = 16
	operatorAttemptsPerHour = 100 // per address; every attempt counts, right or wrong
	operatorCookie          = "attesttag_operator"
)

// operatorCookieTTL is how long a browser may remember the proof. It was thirty days, which for
// a credential that could not be revoked without rotating OPERATOR_SECRET was thirty days of
// somebody else's operator access if the cookie ever leaked.
const operatorCookieTTL = 12 * time.Hour

var errNoSuchOrg = errors.New("no such organisation")

func (b *Bot) operatorRoutes(mux *http.ServeMux) {
	// Counted in the database, not in this process. The ceiling below is the number somebody
	// reading it will believe, and N instances counting separately quietly give a guesser N
	// times it. Shared here rather than in the list at bot.go, because that list is skipped in
	// maintenance mode and these routes are not.
	b.operatorAttempts = newRateLimiter()
	b.operatorAttempts.shareAcross(b.store)
	mux.HandleFunc("GET /api/operator/orgs", b.requireOperator(b.handleOperatorOrgs))
	mux.HandleFunc("PUT /api/operator/orgs/{id}/plan", b.requireOperator(b.handleOperatorSetPlan))
	// The page behind the link in a support email. GET only shows: mail clients prefetch links,
	// so nothing may change until the button is pressed.
	mux.HandleFunc("GET /operator/plan", b.handleOperatorPlanPage)
	mux.HandleFunc("POST /operator/plan", b.handleOperatorPlanSubmit)
	// The dashboard the link's page grew into: the list of accounts, and the money actions on
	// one of them. Everything here is behind the same secret and never behind a console session
	// — a member must not be able to move their own plan or grant themselves credit, and the
	// only way to be sure of that is for these to be unreachable from a signed-in browser.
	mux.HandleFunc("GET /operator", b.handleOperatorIndex)
	// The index's own unlock. It posts here rather than at /operator/plan, which expects a plan
	// to move and answers 400 without one — so the first thing a new operator saw was an error
	// message under a form that had in fact just worked.
	mux.HandleFunc("POST /operator", b.handleOperatorUnlock)
	mux.HandleFunc("POST /operator/credit", b.handleOperatorCreditForm)
	mux.HandleFunc("POST /operator/size", b.handleOperatorSizeForm)
	mux.HandleFunc("POST /operator/cancel", b.handleOperatorCancelForm)
	// The enterprise deal (enterprise.go). Not behind billing like the three above: a deal's plan,
	// budget and figures mean something on a deployment with no Stripe keys too, and only paying
	// by subscription needs them — setEnterprise says so when it is asked for that.
	mux.HandleFunc("POST /operator/enterprise", b.handleOperatorEnterpriseForm)
	mux.HandleFunc("POST /api/operator/orgs/{id}/credit", b.requireOperator(b.handleOperatorCredit))
	mux.HandleFunc("POST /api/operator/orgs/{id}/size", b.requireOperator(b.handleOperatorSize))
}

// operatorCredential finds what the request offers as proof. The two are checked differently —
// a bearer or a typed form field is the secret itself, a cookie is a dated MAC — so they are
// returned apart rather than as one string and the value it should equal. Both empty means
// nothing was offered, which is not an attempt.
func (b *Bot) operatorCredential(r *http.Request) (typed, cookie string) {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer "), ""
	}
	if r.Method == http.MethodPost {
		if v := r.PostFormValue("secret"); v != "" {
			return v, ""
		}
	}
	if c, err := r.Cookie(operatorCookie); err == nil && c.Value != "" {
		return "", c.Value
	}
	return "", ""
}

// operatorOK reports whether the request carries the operator secret, and if not, why: "off"
// (no secret configured), "none" (nothing offered), "throttled", or "unauthorized". Every
// attempt counts against the caller's address before the comparison, so guessing is slow
// whether or not a guess lands.
func (b *Bot) operatorOK(r *http.Request) (bool, string) {
	if b.cfg.OperatorSecret == "" {
		return false, "off"
	}
	typed, cookie := b.operatorCredential(r)
	if typed == "" && cookie == "" {
		return false, "none"
	}
	if ok, _ := b.operatorAttempts.allow("ip:"+clientIP(r), operatorAttemptsPerHour, time.Hour); !ok {
		return false, "throttled"
	}
	ok := false
	switch {
	case typed != "":
		ok = subtle.ConstantTimeCompare([]byte(typed), []byte(b.cfg.OperatorSecret)) == 1
	case operatorCookieOK(cookie, b.cfg.OperatorSecret, time.Now()):
		ok = true
	}
	if ok {
		// The operator's address stays here, in the server log, and never in the tenant-facing
		// ledger or audit rows — those record the actor as "operator" (operatorActor).
		slog.Info("operator request authorised", "ip", clientIP(r), "path", r.URL.Path)
		return true, ""
	}
	slog.Warn("operator secret refused", "ip", clientIP(r), "path", r.URL.Path)
	return false, "unauthorized"
}

// operatorActor is the actor recorded on anything an operator action writes into a tenant's own
// records (the credit ledger, the audit log). It is a role, not the operator's IP address, which
// a tenant should never see; the address is kept in the server log by operatorOK.
const operatorActor = "operator"

// operatorCookieValue is what the page's cookie holds: an expiry, and a MAC over it. A browser
// remembers a proof rather than the secret itself, as before — but a proof that runs out on its
// own rather than one that is a permanent function of the secret, which is what the v1 cookie
// was. Anybody who read that one held the operator API until OPERATOR_SECRET was rotated, and
// nothing else could take it back.
//
// The MAC key is derived from MASTER_KEY under its own label rather than being the operator
// secret, so this shares nothing with the credentials at rest, and the secret goes into the
// message — which means rotating either key ends every cookie already handed out. Returns ""
// when there is no usable master key; the caller must then not set a cookie at all.
func operatorCookieValue(secret string, exp time.Time) string {
	unix := strconv.FormatInt(exp.Unix(), 10)
	mac := operatorCookieMAC(secret, unix)
	if mac == "" {
		return ""
	}
	return unix + "." + mac
}

func operatorCookieMAC(secret, unix string) string {
	key := derivedKey("operator/cookie")
	if key == nil {
		return "" // no usable MASTER_KEY: refuse, never MAC under an empty key
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte("attesttag operator cookie v2|" + secret + "|" + unix))
	return hex.EncodeToString(m.Sum(nil))
}

// operatorCookieOK checks one cookie: still in date, and the MAC over that date still computes.
// An expiry a browser edited fails the MAC; one that has simply passed fails before the MAC is
// worth computing. A v1 cookie has no "." in it and fails at the first step.
func operatorCookieOK(raw, secret string, now time.Time) bool {
	unix, mac, found := strings.Cut(raw, ".")
	if !found {
		return false
	}
	sec, err := strconv.ParseInt(unix, 10, 64)
	if err != nil || now.After(time.Unix(sec, 0)) {
		return false
	}
	want := operatorCookieMAC(secret, unix)
	return want != "" && hmac.Equal([]byte(mac), []byte(want))
}

func (b *Bot) requireOperator(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ok, why := b.operatorOK(r)
		switch {
		case ok:
			next(w, r)
		case why == "off":
			http.NotFound(w, r)
		case why == "throttled":
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many attempts from this address; try again in an hour"})
		default:
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "operator secret required"})
		}
	}
}

// maxPlanBudgetUSD bounds what one conversion may grant: a typo with three extra zeros is
// a month of somebody else's spending on the shared key.
const maxPlanBudgetUSD = 100000

// setPlan is the one write. A pro plan carries the monthly budget the operator is granting —
// the figure sent, or defaultProBudgetUSD when none was — and that figure becomes both the
// account's ceiling (orgs.plan_budget_usd) and its current setting, so what was granted is
// what the bot stops at from the next turn. A tenant may lower it in the console afterwards
// and raise it back up to the grant, never past it. Moving to free clears the grant. The
// config version bump and the cache drop are what make it immediate.
// The organisation is named by its public id throughout — the path segment, the page's query
// string, the hidden form field and deploy/plan.sh all carry that and nothing else. The serial
// would have told whoever held a support email roughly how many accounts exist and what the
// next one along is called, which is exactly the guess an operator link should not fund.
func (b *Bot) setPlan(ctx context.Context, orgPublic, plan string, budget float64, by string) (*Org, error) {
	plan = strings.ToLower(strings.TrimSpace(plan))
	if planOf(plan) != plan {
		return nil, fmt.Errorf("plan must be one of %s", strings.Join(plans, ", "))
	}
	// Enterprise is a deal as well as a plan, and the deal is written by setEnterprise. Moving the
	// plan alone would leave an enterprise account with no figures at all.
	if plan == PlanEnterprise {
		return nil, errors.New("an enterprise plan comes with its deal: send the terms with it (PUT …/plan with \"plan\":\"enterprise\"), or use the Enterprise form")
	}
	if plan == PlanPro {
		if budget == 0 {
			budget = defaultProBudgetUSD
		}
		if math.IsNaN(budget) || budget < 1 || budget > maxPlanBudgetUSD {
			return nil, fmt.Errorf("budget_usd must be between 1 and %d dollars a month", maxPlanBudgetUSD)
		}
	} else {
		budget = 0
	}
	org, err := b.store.OrgByPublicID(ctx, orgPublic)
	if err != nil {
		return nil, err
	}
	if org == nil {
		return nil, errNoSuchOrg
	}
	return org, b.applyPlan(ctx, org, planChange{Plan: plan, BudgetUSD: budget, SetBudget: true, By: by})
}

// planChange is what moving an organisation between plans consists of, whoever asked for it.
type planChange struct {
	Plan      string
	BudgetUSD float64 // the operator's grant; 0 = none
	// SetBudget says whether to overwrite the organisation's own monthly_budget_usd with the
	// grant. True for the operator, who is deciding that number by hand. FALSE for a payment,
	// and this is the whole reason this struct exists: setPlan defaults a zero budget to
	// defaultProBudgetUSD, so a webhook reusing it unchanged would cap a customer who has just
	// paid for $500 of credit at $25 a month AND overwrite a guard rail they chose themselves.
	SetBudget bool
	By        string // an operator's address, or "stripe:<event id>"
	// System is set by anything that is not a person at a keyboard, so the audit line says
	// via=system rather than attributing a renewal to whoever last typed the operator secret.
	System bool
}

// applyPlan is the one write. Everything that must happen when a plan moves happens here and
// nowhere else — the column, the setting, the outstanding request, the config version, the cache
// and the audit row — because there are now two callers: an operator's hand, and a payment.
//
// A pro plan carries the monthly budget the operator is granting, and that figure becomes both the
// account's ceiling (orgs.plan_budget_usd) and its current setting, so what was granted is what
// the bot stops at from the next turn. A tenant may lower it in the console afterwards and raise
// it back up to the grant, never past it. Moving to free clears the grant. The config version bump
// and the cache drop are what make it immediate.
func (b *Bot) applyPlan(ctx context.Context, org *Org, ch planChange) error {
	// Leaving enterprise takes its deal with it, and is refused while the deal is still being
	// charged at Stripe — so it goes first, before anything below has been written.
	if org.Plan == PlanEnterprise && ch.Plan != PlanEnterprise {
		if err := b.leaveEnterprise(ctx, org); err != nil {
			return err
		}
	}
	if err := b.store.SetOrgPlan(ctx, org.ID, ch.Plan, ch.BudgetUSD); err != nil {
		return err
	}
	// Enterprise with no figure is a deal that sets no ceiling, and the account's own budget is
	// left as it was rather than overwritten with a zero that would read as "unlimited".
	if ch.Plan != PlanFree && ch.SetBudget && ch.BudgetUSD > 0 {
		if err := b.store.PutSetting(ctx, org.ID, "monthly_budget_usd", strconv.FormatFloat(ch.BudgetUSD, 'f', -1, 64)); err != nil {
			return err
		}
	}
	// Whatever was asked for has now been answered, either way: the console stops saying a
	// request is outstanding, and an account moved back to free may ask again.
	b.store.PutSetting(ctx, org.ID, planRequestKey, "")
	b.store.BumpConfigVersion(ctx, org.ID)
	b.settings.Invalidate(org.ID)
	slog.Info("org plan changed", "org", org.PublicID, "slug", org.Slug, "from", org.Plan, "to", ch.Plan, "budget_usd", ch.BudgetUSD, "by", ch.By)
	// In the organisation's own log too: the plan decides what it may spend, and the person
	// reading that log is entitled to see when and by whom that changed, operator or payment.
	ev := AuditEvent{TargetKind: "org", TargetID: org.PublicID, TargetName: org.Name,
		Details: auditDetails(map[string]any{"from": org.Plan, "to": ch.Plan, "budget_usd": ch.BudgetUSD, "by": ch.By})}
	if ch.System {
		b.auditSystem(ctx, org.ID, "plan.changed", ev)
	} else {
		b.auditOperator(ctx, org.ID, ch.By, "plan.changed", ev)
	}
	org.Plan, org.PlanBudgetUSD = ch.Plan, ch.BudgetUSD
	return nil
}

// operatorOrgJSON is one organisation as the operator sees it: the plan, what it has spent
// against what it may, and the link that moves it.
func (b *Bot) operatorOrgJSON(ctx context.Context, r *http.Request, o Org) map[string]any {
	st := b.settings.Get(ctx, o.ID)
	spend, _ := b.store.MonthSpend(ctx, o.ID, "", "")
	members, _ := b.store.MembersOf(ctx, o.ID)
	out := map[string]any{
		"id": o.PublicID, "name": o.Name, "slug": o.Slug, "status": o.Status, "plan": o.Plan, "plan_budget_usd": o.PlanBudgetUSD, "created_at": o.CreatedAt,
		"members": len(members), "month_spend_usd": spend, "budget_usd": st.EffectiveBudget(),
		// members is console seats; active_users_30d is the figure the size is actually sold on.
		// Both, because the gap between them is the interesting part of a support conversation.
		"active_users_30d": b.activeUsers(ctx, o.ID, false).Users,
		"plan_url":         b.baseURL(r) + "/operator/plan?org=" + o.PublicID,
	}
	// The deal, note included: this is the operator's own view, the one place the note is read.
	if t, err := b.store.EnterpriseTerms(ctx, o.ID); err == nil && t.Exists {
		out["enterprise"] = map[string]any{
			"users": t.UserLimit, "jobs": t.JobLimit, "fee_usd": float64(t.FeeMinor) / 100,
			"interval": t.FeeInterval, "included_usd": float64(t.IncludedMinor) / 100,
			"paid_by": t.paidBy(), "price_id": t.PriceID, "pay_url": t.PayURL, "note": t.Note,
			"updated_at": t.UpdatedAt,
		}
	}
	// The organisation's own model key as the operator may see it: where it points, which key, and
	// whether the bot can answer on it. Never the key. A support conversation about a silent bot
	// starts here.
	if k := st.OwnKey; k.Present {
		mk := map[string]any{"host": k.Ref.Host(), "key_hint": k.Ref.KeyHint, "active": k.Active(),
			"failing": k.Ref.Failing(), "last_error": k.Ref.LastError, "updated_at": k.Ref.UpdatedAt}
		if err := k.refusal(); err != nil {
			mk["refusal"] = err.Error()
		}
		out["model_key"] = mk
	}
	return out
}

// ownKeyStranded is the warning a plan change carries when it leaves an organisation holding a
// key its new plan does not include. Nothing falls back, so from that moment its model calls
// refuse — which is right, and which the operator should hear about before the customer does.
func (b *Bot) ownKeyStranded(ctx context.Context, orgID int64) string {
	b.settings.Invalidate(orgID)
	if k := b.settings.Get(ctx, orgID).OwnKey; k.Present && !k.Allowed {
		return "this organisation holds its own model key (" + k.Ref.Host() + "), which its new plan does not include: " +
			"its model calls now refuse until an admin removes the key under Settings → Models, or the plan moves back"
	}
	return ""
}

func (b *Bot) handleOperatorOrgs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgs, err := b.store.Orgs(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	// ?email= narrows to the organisations that address belongs to. The support mailbox knows
	// who wrote, not always which organisation they meant.
	if email := strings.TrimSpace(r.URL.Query().Get("email")); email != "" {
		keep := []Org{}
		if u, _ := b.store.UserByEmail(ctx, email); u != nil {
			ms, _ := b.store.MembershipsFor(ctx, u.ID)
			in := map[int64]bool{}
			for _, m := range ms {
				in[m.OrgID] = true
			}
			for _, o := range orgs {
				if in[o.ID] {
					keep = append(keep, o)
				}
			}
		}
		orgs = keep
	}
	out := make([]map[string]any, 0, len(orgs))
	for _, o := range orgs {
		out = append(out, b.operatorOrgJSON(ctx, r, o))
	}
	writeJSON(w, 200, map[string]any{"orgs": out})
}

func (b *Bot) handleOperatorSetPlan(w http.ResponseWriter, r *http.Request) {
	// budget_usd, with "pro", is the monthly budget granted; absent = defaultProBudgetUSD. With
	// "enterprise" it and every other field of the deal are optional, and absent means unchanged:
	// see enterpriseChange.
	var in struct {
		Plan string `json:"plan"`
		enterpriseChange
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	if strings.EqualFold(strings.TrimSpace(in.Plan), PlanEnterprise) {
		org, err := b.store.OrgByPublicID(r.Context(), r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		if org == nil {
			writeJSON(w, 404, map[string]any{"error": errNoSuchOrg.Error()})
			return
		}
		if _, err := b.setEnterprise(r.Context(), org, in.enterpriseChange, operatorActor); err != nil {
			bad(w, err)
			return
		}
		writeJSON(w, 200, b.operatorOrgJSON(r.Context(), r, *org))
		return
	}
	var budget float64
	if in.BudgetUSD != nil {
		budget = *in.BudgetUSD
	}
	org, err := b.setPlan(r.Context(), r.PathValue("id"), in.Plan, budget, operatorActor)
	switch {
	case errors.Is(err, errNoSuchOrg):
		writeJSON(w, 404, map[string]any{"error": err.Error()})
		return
	case err != nil:
		bad(w, err)
		return
	}
	out := b.operatorOrgJSON(r.Context(), r, *org)
	if warn := b.ownKeyStranded(r.Context(), org.ID); warn != "" {
		out["warning"] = warn
	}
	writeJSON(w, 200, out)
}

// ---- the page behind the email link ----

// operatorPlanView is what the page shows. Org is nil until the secret has been given: the
// link names an organisation by id, and a page that showed its name and spend to whoever
// held the link would tell any tenant about any other.
type operatorPlanView struct {
	// OrgID is the organisation's public id: what the link carries and the form posts back.
	OrgID      string
	Org        *Org
	Spend      float64
	Budget     float64
	Members    int
	Workspaces int
	Target     string  // the plan the button moves to
	Action     string  // what the button says
	Offer      float64 // what the budget field starts with: the current grant, or the default
	Remembered bool    // this browser holds a valid cookie, so no secret field
	Hours      int     // how long the cookie lasts, for the hint under the field
	Error      string
	Notice     string

	// Billing, when this deployment sells plans. Absent everywhere else, so a self-host's
	// operator page is exactly the page it was.
	Billing      bool
	CreditUSD    float64
	CreditOn     bool
	Overdraft    float64
	Subscription string // the Stripe status, or "comped" for one an operator granted by hand
	Size         string
	SizeLabel    string
	FeeUSD       float64
	RenewsAt     string
	HasStripeSub bool
	Sizes        []Size
	Ledger       []CreditEntry

	// The enterprise deal, when the account is on one. A value rather than a pointer so the form
	// can read its fields whether or not there is a deal yet — an empty form is the offer to write
	// one.
	IsEnterprise bool
	Enterprise   EnterpriseTerms

	// Orgs is the index: every account on the deployment, newest first, or the ones matching a
	// search. Only filled on /operator.
	Index bool
	Query string
	Orgs  []operatorOrgRow
}

// operatorOrgRow is one line of the index. Deliberately small: the list is for finding an
// account, and everything about it is one click away on its own page.
type operatorOrgRow struct {
	ID, Name, Slug, Plan string
	SpendUSD, BudgetUSD  float64
	CreditUSD            float64
	CreditOn             bool
	Members              int
	// ActiveUsers is the 30-day figure the size is sold on; Members is console seats. An account
	// whose two numbers are far apart is the one worth looking at, in either direction.
	ActiveUsers  int
	OverLimit    bool
	Subscription string
}

func (b *Bot) handleOperatorPlanPage(w http.ResponseWriter, r *http.Request) {
	if b.cfg.OperatorSecret == "" {
		http.NotFound(w, r)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("org"))
	ok, why := b.operatorOK(r)
	v := operatorPlanView{OrgID: id, Remembered: ok}
	if why == "unauthorized" {
		// A cookie from before the secret was rotated. Drop it and ask again.
		b.forgetOperator(w, r)
		v.Error = "This browser's operator cookie no longer matches the secret. Enter it again."
	}
	if ok {
		b.fillOperatorOrg(r.Context(), &v)
	}
	b.renderOperatorPlan(w, http.StatusOK, v)
}

func (b *Bot) handleOperatorPlanSubmit(w http.ResponseWriter, r *http.Request) {
	if b.cfg.OperatorSecret == "" {
		http.NotFound(w, r)
		return
	}
	id := strings.TrimSpace(r.PostFormValue("org"))
	v := operatorPlanView{OrgID: id}
	ok, why := b.operatorOK(r)
	if !ok {
		status := http.StatusUnauthorized
		switch why {
		case "throttled":
			status, v.Error = http.StatusTooManyRequests, "Too many attempts from this address. Try again in an hour."
		case "none":
			v.Error = "The operator secret is needed."
		default:
			b.forgetOperator(w, r)
			v.Error = "That is not the operator secret."
		}
		b.renderOperatorPlan(w, status, v)
		return
	}
	// A secret typed into the form is remembered by this browser, so the next email's link is
	// a click and a button. What is stored is derived from the secret, not the secret.
	// SameSite stays Lax rather than becoming Strict: the whole point of this cookie is the link
	// in a support email, and a top-level navigation from a mail client is cross-site. Strict
	// would drop it there and ask for the secret again every time. Lax still withholds it from a
	// cross-site POST, and a cross-site GET here only renders the page — the button is a POST.
	if v := operatorCookieValue(b.cfg.OperatorSecret, time.Now().Add(operatorCookieTTL)); v != "" && r.PostFormValue("secret") != "" {
		http.SetCookie(w, &http.Cookie{Name: operatorCookie, Value: v, Path: "/operator",
			HttpOnly: true, MaxAge: int(operatorCookieTTL / time.Second), SameSite: http.SameSiteLaxMode, Secure: b.secureCookies(r)})
	}
	v.Remembered = true
	var budget float64
	if raw := strings.TrimSpace(r.PostFormValue("budget_usd")); raw != "" {
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			v.Error = "The monthly budget has to be a number of dollars."
			b.fillOperatorOrg(r.Context(), &v)
			b.renderOperatorPlan(w, http.StatusBadRequest, v)
			return
		}
		budget = f
	}
	org, err := b.setPlan(r.Context(), id, r.PostFormValue("plan"), budget, operatorActor)
	if err != nil {
		v.Error = err.Error()
		b.fillOperatorOrg(r.Context(), &v)
		b.renderOperatorPlan(w, http.StatusBadRequest, v)
		return
	}
	if org.Plan == PlanPro {
		v.Notice = fmt.Sprintf("%s is now on the pro plan with a $%.2f monthly budget.", org.Name, org.PlanBudgetUSD)
	} else {
		v.Notice = fmt.Sprintf("%s is now on the free plan.", org.Name)
	}
	b.fillOperatorOrg(r.Context(), &v)
	b.renderOperatorPlan(w, http.StatusOK, v)
}

func (b *Bot) forgetOperator(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: operatorCookie, Value: "", Path: "/operator", HttpOnly: true, MaxAge: -1,
		SameSite: http.SameSiteLaxMode, Secure: b.secureCookies(r)})
}

// fillOperatorOrg loads what the page may show once the secret is in hand, and decides which
// way the button moves the account.
func (b *Bot) fillOperatorOrg(ctx context.Context, v *operatorPlanView) {
	v.Target, v.Action, v.Offer = PlanPro, "Move to Pro", defaultProBudgetUSD
	if v.OrgID == "" {
		return
	}
	org, err := b.store.OrgByPublicID(ctx, v.OrgID)
	if err != nil || org == nil {
		if v.Error == "" {
			v.Error = "There is no organisation with that id."
		}
		return
	}
	v.Org = org
	v.Spend, _ = b.store.MonthSpend(ctx, org.ID, "", "")
	v.Budget = b.settings.Get(ctx, org.ID).EffectiveBudget()
	members, _ := b.store.MembersOf(ctx, org.ID)
	v.Members = len(members)
	teams, _ := b.store.Teams(ctx, org.ID)
	for _, t := range teams {
		if t.Status == "active" {
			v.Workspaces++
		}
	}
	switch org.Plan {
	case PlanPro:
		v.Target, v.Action = PlanFree, "Move back to Free"
	case PlanEnterprise:
		// Off enterprise is down a step, to pro with a budget; free from there is one more press.
		v.Target, v.Action = PlanPro, "Move to Pro"
	}
	if org.PlanBudgetUSD > 0 {
		v.Offer = org.PlanBudgetUSD
	}
	if t, err := b.store.EnterpriseTerms(ctx, org.ID); err == nil && t.Exists {
		v.IsEnterprise, v.Enterprise = true, t
	}
	b.fillOperatorBilling(ctx, v, org)
}

// fillOperatorBilling adds the money half of the page, and only on a deployment that has any.
// Split out so the plan page a self-host sees is unchanged rather than full of empty rows.
func (b *Bot) fillOperatorBilling(ctx context.Context, v *operatorPlanView, org *Org) {
	if !b.cfg.BillingEnabled() {
		return
	}
	v.Billing, v.Sizes, v.Overdraft = true, b.cfg.StripeSizes, microsToUSD(maxOverdraftMicros)
	acct, err := b.store.BillingAccountOf(ctx, org.ID)
	if err != nil {
		return
	}
	v.CreditUSD, v.CreditOn = microsToUSD(acct.CreditBalanceMicros), acct.CreditEnforced
	v.Subscription, v.Size, v.RenewsAt = acct.Status, acct.Size, acct.PeriodEnd
	v.SizeLabel, v.FeeUSD = sizeLabels[acct.Size], microsToUSD(acct.AmountMicros())
	// Whether there is a real subscription at Stripe, as opposed to one comped here. It decides
	// whether the page offers Cancel, and it is the thing that stops somebody hand-moving an
	// account that is being billed every month without noticing.
	v.HasStripeSub = acct.SubscriptionID != ""
	v.Ledger, _ = b.store.CreditLedger(ctx, org.ID, 12)
}

func (b *Bot) renderOperatorPlan(w http.ResponseWriter, status int, v operatorPlanView) {
	if v.Target == "" {
		v.Target, v.Action = PlanPro, "Move to Pro"
	}
	if v.Offer == 0 {
		v.Offer = defaultProBudgetUSD
	}
	v.Hours = int(operatorCookieTTL / time.Hour)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Robots-Tag", "noindex")
	w.WriteHeader(status)
	operatorPlanPage.Execute(w, v)
}

// operatorFuncs is the one thing the page cannot do for itself: minor units are what Stripe
// counts in and dollars are what a person reads, and html/template has no arithmetic.
var operatorFuncs = template.FuncMap{
	"divf": func(a int64, b float64) float64 { return float64(a) / b },
	// money puts the sign outside the symbol: "-$60.00", not "$-60.00". A credit balance goes
	// below zero while the turns already in flight finish, so this page shows negatives often
	// enough for the difference to matter.
	"money": func(usd float64) string {
		if usd < 0 {
			return fmt.Sprintf("-$%.2f", -usd)
		}
		return fmt.Sprintf("$%.2f", usd)
	},
	// signed is the same for a statement line, where the direction is the point.
	"signed": func(usd float64) string {
		if usd > 0 {
			return fmt.Sprintf("+$%.2f", usd)
		}
		return fmt.Sprintf("-$%.2f", -usd)
	},
}

var operatorPlanPage = template.Must(template.New("operator-plan").Funcs(operatorFuncs).Parse(`<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex">
<title>Operator · plan{{if .Org}} · {{.Org.Name}}{{end}}</title>
<style>
:root{color-scheme:light dark;--bg:#fbfaf8;--fg:#191c2b;--card:#fff;--muted-fg:#5d6170;--border:#e8e6e1;--input:#d4d2cb;--primary:#5a50c8;--ok:#2e7d4f;--bad:#bf3b2b}
@media (prefers-color-scheme:dark){:root{--bg:#101019;--fg:#eceaf3;--card:#17161f;--muted-fg:#9a97a8;--border:#292834;--input:#363443;--primary:#8a80e8;--ok:#4caf7d;--bad:#e0604f}}
body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.5 Geist,ui-sans-serif,system-ui,sans-serif}
main{max-width:520px;margin:48px auto;padding:0 20px}main.wide{max-width:860px}
table{width:100%;border-collapse:collapse;font-size:13px}th,td{text-align:left;padding:7px 8px;border-bottom:1px solid var(--border)}
th{color:var(--muted-fg);font-weight:600}td.num,th.num{text-align:right;font-variant-numeric:tabular-nums}
a{color:var(--primary)}.searchrow{display:flex;gap:8px;margin-bottom:12px}.searchrow input{margin:0}
input[type=text],input[type=search]{box-sizing:border-box;width:100%;border:1px solid var(--input);border-radius:3px;background:var(--card);color:var(--fg);padding:8px 10px;font:inherit;margin-bottom:12px}
select{box-sizing:border-box;width:100%;border:1px solid var(--input);border-radius:3px;background:var(--card);color:var(--fg);padding:8px 10px;font:inherit;margin-bottom:12px}
.bal{font-size:22px;font-weight:600;font-variant-numeric:tabular-nums}.bal.low{color:var(--bad)}
h2{font-size:14px;margin:0 0 10px}h1{font-size:20px;margin:0 0 4px}.sub{color:var(--muted-fg);margin:0 0 16px}
.card{background:var(--card);border:1px solid var(--border);border-radius:6px;box-shadow:0 1px 2px rgba(16,24,40,.04);padding:16px;margin-bottom:12px}
.row{display:flex;justify-content:space-between;gap:16px;padding:8px 0;border-bottom:1px solid var(--border)}.row:last-child{border-bottom:0}.row span{color:var(--muted-fg)}
.plan{display:inline-block;padding:1px 8px;border-radius:999px;border:1px solid var(--border);font-size:12px;font-weight:600;text-transform:capitalize}
.plan.pro{border-color:var(--primary);color:var(--primary)}.plan.enterprise{border-color:var(--ok);color:var(--ok)}
label{display:block;font-weight:600;margin:0 0 6px}
input[type=password],input[type=number]{box-sizing:border-box;width:100%;border:1px solid var(--input);border-radius:3px;background:var(--card);color:var(--fg);padding:8px 10px;font:inherit;margin-bottom:12px}
input[type=password]:focus-visible,input[type=number]:focus-visible{outline:2px solid var(--primary);outline-offset:1px;border-color:var(--primary)}
button{background:var(--primary);color:#fff;border:0;border-radius:3px;padding:8px 14px;font:inherit;font-weight:500;cursor:pointer}
button.ghost{background:transparent;color:var(--fg);border:1px solid var(--border)}
.msg{padding:10px 12px;border-radius:4px;margin-bottom:12px;border:1px solid}
.bad{color:var(--bad);border-color:var(--bad)}.ok{color:var(--ok);border-color:var(--ok)}
.hint{font-size:12px;color:var(--muted-fg);margin:-6px 0 12px}
</style>
<main{{if .Index}} class="wide"{{end}}>
{{if .Index}}
<h1>Operator</h1>
<p class="sub">Every account on this deployment. Only the operator secret opens this.</p>
{{if .Error}}<div class="msg bad">{{.Error}}</div>{{end}}
{{if .Remembered}}
<form method="get" action="/operator" class="searchrow">
<input type="search" name="q" value="{{.Query}}" placeholder="Name, slug, id, or a member&#39;s email" autofocus>
<button>Search</button>
</form>
<div class="card">
<table>
<tr><th>Account</th><th>Plan</th><th class="num">Spend this month</th>{{if .Billing}}<th class="num">Credit</th><th>Subscription</th>{{end}}<th class="num">Users, 30d</th><th class="num">Seats</th></tr>
{{range .Orgs}}<tr>
<td><a href="/operator/plan?org={{.ID}}">{{.Name}}</a><br><span class="hint">{{.Slug}}</span></td>
<td><span class="plan {{.Plan}}">{{.Plan}}</span></td>
<td class="num">{{money .SpendUSD}}{{if gt .BudgetUSD 0.0}} <span class="hint">of {{money .BudgetUSD}}</span>{{end}}</td>
{{if $.Billing}}<td class="num">{{if .CreditOn}}{{money .CreditUSD}}{{else}}<span class="hint">—</span>{{end}}</td>
<td>{{if .Subscription}}{{.Subscription}}{{else}}<span class="hint">—</span>{{end}}</td>{{end}}
<td class="num">{{.ActiveUsers}}{{if .OverLimit}} <span class="hint">over size</span>{{end}}</td>
<td class="num">{{.Members}}</td>
</tr>{{else}}<tr><td colspan="7"><span class="hint">Nothing matches.</span></td></tr>{{end}}
</table>
</div>
{{else}}
<form method="post" action="/operator" class="card">
<label for="secret">Operator secret</label>
<input id="secret" type="password" name="secret" autocomplete="current-password" required autofocus>
<p class="hint">Typed once; this browser remembers it for {{.Hours}} hours. Then reload this page.</p>
<button>Unlock</button>
</form>
{{end}}
</main>
{{else}}
<h1>{{if .Org}}{{.Org.Name}}{{else if .OrgID}}An organisation{{else}}Operator · plan{{end}}</h1>
<p class="sub">{{if .Org}}<span class="plan {{.Org.Plan}}">{{.Org.Plan}} plan</span>{{else}}Move an account between the free, pro and enterprise plans. Only the operator secret opens this.{{end}}</p>
{{if .Error}}<div class="msg bad">{{.Error}}</div>{{end}}
{{if .Notice}}<div class="msg ok">{{.Notice}}</div>{{end}}
{{if .Org}}<div class="card">
<div class="row"><span>Spend this month</span><b>{{money .Spend}} of {{money .Budget}}</b></div>
<div class="row"><span>Members · workspaces</span><b>{{.Members}} · {{.Workspaces}}</b></div>
<div class="row"><span>Slug · since</span><b>{{.Org.Slug}} · {{.Org.CreatedAt}}</b></div>
<div class="row"><span>Id</span><b><code>{{.Org.PublicID}}</code></b></div>
{{if .IsEnterprise}}<div class="row"><span>Deal</span><b>{{if .Enterprise.UserLimit}}{{.Enterprise.UserLimit}} users{{else}}no user ceiling{{end}}{{if .Enterprise.JobLimit}} · {{.Enterprise.JobLimit}} fix jobs a month{{end}}{{if .Enterprise.IncludedMinor}} · {{money (divf .Enterprise.IncludedMinor 100)}} credit a month{{end}}</b></div>
<div class="row"><span>Paid by</span><b>{{if .Enterprise.PriceID}}subscription to <code>{{.Enterprise.PriceID}}</code>{{else if .Enterprise.PayURL}}a link{{else}}invoice, by hand{{end}}{{if .Enterprise.FeeMinor}} · {{money (divf .Enterprise.FeeMinor 100)}} a {{.Enterprise.FeeInterval}}{{end}}</b></div>{{end}}
</div>{{end}}
{{if .OrgID}}<form method="post" action="/operator/plan" class="card">
<input type="hidden" name="org" value="{{.OrgID}}">
<input type="hidden" name="plan" value="{{.Target}}">
{{if eq .Target "pro"}}<label for="budget">Monthly budget, US dollars</label>
<input id="budget" type="number" name="budget_usd" min="1" max="100000" step="1" value="{{printf "%g" .Offer}}">
<p class="hint">What the account may spend a month on the shared key. Left alone, it is $25.</p>{{end}}
{{if not .Remembered}}<label for="secret">Operator secret</label>
<input id="secret" type="password" name="secret" autocomplete="current-password" required autofocus>
<p class="hint">Typed once; this browser remembers it for {{.Hours}} hours.</p>{{end}}
<button{{if eq .Target "free"}} class="ghost"{{end}}>{{.Action}}</button>
</form>

{{if and .Org .Remembered}}<form method="post" action="/operator/enterprise" class="card">
<input type="hidden" name="org" value="{{.OrgID}}">
<h2>Enterprise</h2>
<p class="hint">{{if .IsEnterprise}}The deal this account is on. Change any figure and save; the account sees the new one on its Billing screen straight away.{{else}}A deal written by hand: the account&#39;s own users, fix jobs, included credit and fee, and how it pays. Saving moves it onto the enterprise plan.{{end}}</p>
<label for="e_users">Users</label>
<input id="e_users" type="number" name="users" min="0" step="1" value="{{.Enterprise.UserLimit}}">
<p class="hint">How many people the deal is sold for; 0 for no ceiling. Shown to the account and counted, never enforced.</p>
<label for="e_jobs">Fix jobs a month</label>
<input id="e_jobs" type="number" name="jobs" min="0" step="1" value="{{.Enterprise.JobLimit}}">
<label for="e_included">Included credit a month, US dollars</label>
<input id="e_included" type="number" name="included_usd" min="0" step="0.01" value="{{printf "%g" (divf .Enterprise.IncludedMinor 100)}}">
<p class="hint">Spent before any credit bought or granted, and gone at the end of the month, like a plan size&#39;s. Grant credit that does not expire in the Credit card below.</p>
<label for="e_budget">Monthly budget, US dollars</label>
<input id="e_budget" type="number" name="budget_usd" min="0" max="100000" step="1" value="{{if .IsEnterprise}}{{printf "%g" .Org.PlanBudgetUSD}}{{else}}0{{end}}">
<p class="hint">The most it may spend on models a month. 0 sets no ceiling: the account&#39;s own budget and its credit are what stop it.</p>
<label for="e_price">Stripe Price, to subscribe to</label>
<input id="e_price" type="text" name="price_id" maxlength="255" placeholder="price_…" value="{{.Enterprise.PriceID}}">
<p class="hint">A recurring price made for this account. Its admin subscribes to it from Settings → Billing, and the fee is read from it.</p>
<label for="e_link">Or a link to pay at</label>
<input id="e_link" type="text" name="pay_url" maxlength="2000" placeholder="https://buy.stripe.com/…" value="{{.Enterprise.PayURL}}">
<p class="hint">Billing shows a Pay button that opens it. Payments made there are not seen here; reconcile them by hand. Leave both empty to invoice by hand.</p>
<label for="e_fee">Fee, US dollars</label>
<input id="e_fee" type="number" name="fee_usd" min="0" step="0.01" value="{{printf "%g" (divf .Enterprise.FeeMinor 100)}}">
<select name="interval" aria-label="Fee interval"><option value="month">a month</option><option value="year"{{if eq .Enterprise.FeeInterval "year"}} selected{{end}}>a year</option></select>
<p class="hint">What the account is shown it pays. Read from the price when there is one; 0 shows no figure.</p>
<label for="e_note">Note</label>
<input id="e_note" type="text" name="note" maxlength="500" placeholder="Order form, contact, renewal date…" value="{{.Enterprise.Note}}">
<p class="hint">Yours. The account never sees it.</p>
<button>{{if .IsEnterprise}}Save the deal{{else}}Move to Enterprise{{end}}</button>
</form>{{end}}

{{if and .Billing .Org .Remembered}}
<div class="card">
<h2>Credit</h2>
<p class="bal{{if and .CreditOn (le .CreditUSD 0.0)}} low{{end}}">{{money .CreditUSD}}</p>
<p class="hint">{{if .CreditOn}}The bot stops when this reaches -{{money .Overdraft}} — work already running when the money runs out is still charged.{{else}}This account has never bought credit, so nothing here stops it. Granting any amount turns the floor on.{{end}}</p>
</div>

<form method="post" action="/operator/credit" class="card">
<input type="hidden" name="org" value="{{.OrgID}}">
<h2>Grant credit</h2>
<label for="amount">US dollars</label>
<input id="amount" type="number" name="amount_usd" step="0.01" min="-100000" max="100000" placeholder="100" required>
<label for="note">Note</label>
<input id="note" type="text" name="note" maxlength="200" placeholder="Beta partner, agreed with…">
<p class="hint">Goes on the statement as a grant, with no payment behind it, and switches the credit floor on. A negative amount takes credit back and is recorded as an adjustment.</p>
<button>Grant credit</button>
</form>

{{if not .IsEnterprise}}<form method="post" action="/operator/size" class="card">
<input type="hidden" name="org" value="{{.OrgID}}">
<h2>Size</h2>
{{if .Subscription}}<div class="row"><span>Now</span><b>{{if .SizeLabel}}{{.SizeLabel}}{{else}}—{{end}} · {{money .FeeUSD}} a month · {{.Subscription}}</b></div>{{end}}
{{if .HasStripeSub}}<p class="hint">This account has a live subscription at Stripe. Change the size there — doing it here would leave the two disagreeing about what is being charged.</p>
{{else}}<label for="size">Record a size, comped</label>
<select id="size" name="size">{{range .Sizes}}<option value="{{.Key}}">{{.Label}} · {{money (divf .AmountMinor 100)}} a month</option>{{end}}</select>
<p class="hint">Nothing is charged for a comped size: it records what the account would be paying, so every other page reads it like a paid one.</p>
<button>Set size</button>{{end}}
</form>{{end}}

{{if .HasStripeSub}}<form method="post" action="/operator/cancel" class="card">
<input type="hidden" name="org" value="{{.OrgID}}">
<h2>Subscription</h2>
<div class="row"><span>Renews</span><b>{{if .RenewsAt}}{{.RenewsAt}}{{else}}—{{end}}</b></div>
<p class="hint">Cancels at Stripe. The plan moves when their confirmation arrives, so there is one path into a plan change and this page cannot disagree with it. Credit already bought is not touched.</p>
<button class="ghost">Cancel the subscription</button>
</form>{{end}}

{{if .Ledger}}<div class="card">
<h2>Statement</h2>
<table>
<tr><th>When</th><th>What</th><th class="num">Amount</th><th class="num">Balance</th></tr>
{{range .Ledger}}<tr><td>{{.At}}</td><td>{{.Kind}}{{if .Note}} <span class="hint">{{.Note}}</span>{{end}}</td>
<td class="num">{{signed .AmountUSD}}</td><td class="num">{{money .BalanceUSD}}</td></tr>{{end}}
</table>
</div>{{end}}
{{end}}

{{else}}<p class="sub">Open this page from the link in a support email, add <code>?org=&lt;id&gt;</code> to the address, or start at <a href="/operator">the account list</a>.</p>{{end}}
{{if and .Org .Remembered}}<p class="sub"><a href="/operator">← every account</a></p>{{end}}
</main>
{{end}}
`))
