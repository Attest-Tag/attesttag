package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Signup is open, so the free plan's budget is what stands between a stranger with a script
// and the shared model key. Nothing a member can do lifts it; only the operator's secret does.

// testSupportEmail stands in for the deployment's published address; the real one is
// configuration now (SUPPORT_EMAIL), not a constant these tests can reach for.
const testSupportEmail = "support@example.com"

func planBot(t *testing.T, cfg Config) (*Bot, *http.ServeMux, *Store) {
	t.Helper()
	fixedMasterKey(t)
	st := testStore(t)
	sealer, err := NewSealer()
	if err != nil {
		t.Fatal(err)
	}
	b := &Bot{cfg: cfg, store: st, sealer: sealer, mail: logMailer{}, settings: newSettingsCache(st, cfg), resolver: NewResolver(st)}
	b.slacks = NewChatRegistry(st, sealer)
	mux := http.NewServeMux()
	b.routes(mux, nil)
	return b, mux, st
}

func call(t *testing.T, mux *http.ServeMux, method, path, body, auth string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// orgPublic is the organisation's id as everything outside the process says it: the plan
// routes, the operator's link and deploy/plan.sh all take this and never the serial.
func orgPublic(t *testing.T, st *Store, orgID int64) string {
	t.Helper()
	o, err := st.Org(context.Background(), orgID)
	if err != nil || o == nil {
		t.Fatalf("org %d: %v", orgID, err)
	}
	if len(o.PublicID) != 32 {
		t.Fatalf("org %d has public id %q, want 32 hex characters", orgID, o.PublicID)
	}
	return o.PublicID
}

func TestFreePlanCapsBudgetAndProDoesNot(t *testing.T) {
	cfg := Config{FreePlanBudgetUSD: 5, PlatformMonthlyBudgetUSDPerOrg: 25, MonthlyBudgetUSD: 20, SupportEmail: testSupportEmail}
	b, _, st := planBot(t, cfg)
	ctx := context.Background()
	orgID, _, _ := seedOrg(t, st, RoleAdmin)

	// A fresh organisation is free, and the environment's $20 default is not what it may spend.
	s := b.settings.Get(ctx, orgID)
	if s.Plan != PlanFree || s.EffectiveBudget() != 5 || s.EffectiveBudgetUSD != 5 || s.PlatformBudgetUSD != 5 {
		t.Fatalf("fresh org: plan=%q effective=%v stamped=%v ceiling=%v", s.Plan, s.EffectiveBudget(), s.EffectiveBudgetUSD, s.PlatformBudgetUSD)
	}
	// The setting cannot lift it...
	st.PutSettings(ctx, orgID, map[string]string{"monthly_budget_usd": "500"})
	b.settings.Invalidate(orgID)
	if got := b.settings.Get(ctx, orgID).EffectiveBudget(); got != 5 {
		t.Errorf("a free account raised its own budget to %v", got)
	}
	// ...but may lower it: the plan is a ceiling, not a value.
	st.PutSettings(ctx, orgID, map[string]string{"monthly_budget_usd": "2"})
	b.settings.Invalidate(orgID)
	if got := b.settings.Get(ctx, orgID).EffectiveBudget(); got != 2 {
		t.Errorf("a free account lowering its budget got %v, want 2", got)
	}

	// Pro: its own setting, under the operator's ceiling like before.
	if _, err := b.setPlan(ctx, orgPublic(t, st, orgID), PlanPro, 0, "test"); err != nil {
		t.Fatal(err)
	}
	st.PutSettings(ctx, orgID, map[string]string{"monthly_budget_usd": "500"})
	b.settings.Invalidate(orgID)
	if s := b.settings.Get(ctx, orgID); s.Plan != PlanPro || s.EffectiveBudget() != 25 {
		t.Errorf("pro over the ceiling: plan=%q effective=%v, want pro/25", s.Plan, s.EffectiveBudget())
	}
	st.PutSettings(ctx, orgID, map[string]string{"monthly_budget_usd": "12"})
	b.settings.Invalidate(orgID)
	if got := b.settings.Get(ctx, orgID).EffectiveBudget(); got != 12 {
		t.Errorf("pro under the ceiling = %v, want 12", got)
	}
	// A grant named with the plan is this account's own ceiling, above the deployment's 25:
	// the operator decided it, so the deployment-wide figure does not hold it down.
	if _, err := b.setPlan(ctx, orgPublic(t, st, orgID), PlanPro, 60, "test"); err != nil {
		t.Fatal(err)
	}
	if s := b.settings.Get(ctx, orgID); s.EffectiveBudget() != 60 || s.PlatformBudgetUSD != 60 || s.MonthlyBudgetUSD != 60 {
		t.Errorf("granted 60: effective=%v ceiling=%v setting=%v", s.EffectiveBudget(), s.PlatformBudgetUSD, s.MonthlyBudgetUSD)
	}
	st.PutSettings(ctx, orgID, map[string]string{"monthly_budget_usd": "500"})
	b.settings.Invalidate(orgID)
	if got := b.settings.Get(ctx, orgID).EffectiveBudget(); got != 60 {
		t.Errorf("a pro account raised its own budget past the grant: %v", got)
	}
	// Back to free: the grant is gone and the ceiling returns at once, not after the cache's
	// fifteen seconds.
	if _, err := b.setPlan(ctx, orgPublic(t, st, orgID), PlanFree, 0, "test"); err != nil {
		t.Fatal(err)
	}
	if s := b.settings.Get(ctx, orgID); s.EffectiveBudget() != 5 || s.PlatformBudgetUSD != 5 {
		t.Errorf("after moving back to free: effective=%v ceiling=%v, want 5/5", s.EffectiveBudget(), s.PlatformBudgetUSD)
	}
	if o, _ := st.Org(ctx, orgID); o.PlanBudgetUSD != 0 {
		t.Errorf("the grant survived the move to free: %v", o.PlanBudgetUSD)
	}
	for _, bad := range []float64{-1, 0.5, 1e9} {
		if _, err := b.setPlan(ctx, orgPublic(t, st, orgID), PlanPro, bad, "test"); err == nil {
			t.Errorf("a grant of %v was accepted", bad)
		}
	}
	if _, err := b.setPlan(ctx, orgPublic(t, st, orgID), "enterprise", 0, "test"); err == nil {
		t.Error("an unknown plan was accepted")
	}
	if _, err := b.setPlan(ctx, "ffffffffffffffffffffffffffffffff", PlanPro, 0, "test"); err == nil {
		t.Error("a missing organisation was moved to a plan")
	}
	// A deployment with no free budget configured behaves as before: the operator's ceiling only.
	b2, _, st2 := planBot(t, Config{PlatformMonthlyBudgetUSDPerOrg: 25, MonthlyBudgetUSD: 20, SupportEmail: testSupportEmail})
	org2, _, _ := seedOrg(t, st2, RoleAdmin)
	if got := b2.settings.Get(ctx, org2).EffectiveBudget(); got != 20 {
		t.Errorf("without FREE_PLAN_BUDGET_USD = %v, want the $20 default", got)
	}
}

func TestFreeAccountRefusalNamesSupport(t *testing.T) {
	cfg := Config{FreePlanBudgetUSD: 5, MonthlyBudgetUSD: 20, SupportEmail: testSupportEmail}
	b, _, st := planBot(t, cfg)
	ctx := context.Background()
	orgID, _, _ := seedOrg(t, st, RoleAdmin)
	a := &Agent{store: st, settings: b.settings, cfg: cfg}
	if _, err := st.db.ExecContext(ctx, `insert into usage (org_id, team_id, channel, cost_usd) values (?, '', '', 6)`, orgID); err != nil {
		t.Fatal(err)
	}
	ok, why := a.budgetOK(ctx, orgID, "")
	if ok {
		t.Fatal("a free account spent past $5 and was not stopped")
	}
	if !strings.Contains(why, "$5.00") || !strings.Contains(why, testSupportEmail) {
		t.Errorf("the refusal should name the free budget and where to ask: %q", why)
	}
	// Pro: the same spend under the $20 setting is fine, and no support address in sight.
	if _, err := b.setPlan(ctx, orgPublic(t, st, orgID), PlanPro, 0, "test"); err != nil {
		t.Fatal(err)
	}
	if ok, why := a.budgetOK(ctx, orgID, ""); !ok {
		t.Errorf("a pro account under its budget was stopped: %q", why)
	}
	// And a pro account that runs out is told about its budget, not about support.
	if _, err := st.db.ExecContext(ctx, `insert into usage (org_id, team_id, channel, cost_usd) values (?, '', '', 20)`, orgID); err != nil {
		t.Fatal(err)
	}
	if ok, why := a.budgetOK(ctx, orgID, ""); ok || strings.Contains(why, testSupportEmail) {
		t.Errorf("pro refusal: ok=%v %q", ok, why)
	}
}

func TestConsoleCannotRaiseFreeBudget(t *testing.T) {
	cfg := Config{FreePlanBudgetUSD: 5, PlatformMonthlyBudgetUSDPerOrg: 25, MonthlyBudgetUSD: 20, SupportEmail: testSupportEmail}
	b, mux, st := planBot(t, cfg)
	ctx := context.Background()
	orgID, _, tok := seedOrg(t, st, RoleAdmin)
	put := func(v string) (int, string) {
		code, out := call(t, mux, "PUT", "/api/settings", `{"monthly_budget_usd":"`+v+`"}`, "Bearer "+tok)
		return code, fmt.Sprint(out["error"])
	}
	if code, msg := put("50"); code != 400 || !strings.Contains(msg, testSupportEmail) {
		t.Errorf("free account raising to 50: %d %q, want 400 naming support", code, msg)
	}
	if code, msg := put("3"); code != 200 {
		t.Errorf("free account lowering to 3: %d %q", code, msg)
	}
	if code, _ := put("abc"); code != 400 {
		t.Errorf("a non-number was stored: %d", code)
	}
	if code, _ := put("-1"); code != 400 {
		t.Errorf("a negative budget was stored: %d", code)
	}
	if _, err := b.setPlan(ctx, orgPublic(t, st, orgID), PlanPro, 0, "test"); err != nil {
		t.Fatal(err)
	}
	if code, msg := put("20"); code != 200 {
		t.Errorf("pro account setting 20: %d %q", code, msg)
	}
	if code, msg := put("50"); code != 400 || strings.Contains(msg, testSupportEmail) {
		t.Errorf("pro account over the operator's ceiling: %d %q, want 400 about the ceiling", code, msg)
	}
	// What the console and the developer API show is the enforced number, not the setting.
	code, out := call(t, mux, "GET", "/api/overview", "", "Bearer "+tok)
	if code != 200 || out["budget_usd"] != 20.0 || out["plan"] != PlanPro {
		t.Errorf("overview: %d budget=%v plan=%v", code, out["budget_usd"], out["plan"])
	}
}

func TestOperatorRoutes(t *testing.T) {
	ctx := context.Background()
	// Off: no secret configured, so the routes do not exist, whatever is sent.
	_, mux, st := planBot(t, Config{FreePlanBudgetUSD: 5, SupportEmail: testSupportEmail})
	orgID, _, tok := seedOrg(t, st, RoleAdmin)
	if code, _ := call(t, mux, "PUT", fmt.Sprintf("/api/operator/orgs/%s/plan", orgPublic(t, st, orgID)), `{"plan":"pro"}`, "Bearer anything"); code != 404 {
		t.Errorf("operator route without OPERATOR_SECRET = %d, want 404", code)
	}

	const secret = "correct-horse-battery-staple"
	b, mux, st := planBot(t, Config{FreePlanBudgetUSD: 5, OperatorSecret: secret, SupportEmail: testSupportEmail})
	orgID, _, tok = seedOrg(t, st, RoleAdmin)
	path := fmt.Sprintf("/api/operator/orgs/%s/plan", orgPublic(t, st, orgID))
	for _, auth := range []string{"", "Bearer wrong", "Bearer " + tok} {
		if code, _ := call(t, mux, "PUT", path, `{"plan":"pro"}`, auth); code != 401 {
			t.Errorf("auth %q = %d, want 401", auth, code)
		}
	}
	if b.settings.Get(ctx, orgID).Plan != PlanFree {
		t.Fatal("a refused request changed the plan")
	}
	code, out := call(t, mux, "PUT", path, `{"plan":"pro"}`, "Bearer "+secret)
	if code != 200 || out["plan"] != PlanPro {
		t.Fatalf("set plan = %d %v", code, out)
	}
	// No figure sent: the default grant, as the ceiling and as the setting.
	if s := b.settings.Get(ctx, orgID); s.Plan != PlanPro || s.PlatformBudgetUSD != defaultProBudgetUSD || s.EffectiveBudget() != defaultProBudgetUSD {
		t.Errorf("after the move: plan=%q ceiling=%v effective=%v, want pro at the default %v", s.Plan, s.PlatformBudgetUSD, s.EffectiveBudget(), defaultProBudgetUSD)
	}
	// A figure sent: that, exactly.
	code, out = call(t, mux, "PUT", path, `{"plan":"pro","budget_usd":50}`, "Bearer "+secret)
	if code != 200 || out["plan_budget_usd"] != 50.0 || out["budget_usd"] != 50.0 {
		t.Fatalf("set plan with budget = %d %v", code, out)
	}
	if got := b.settings.Get(ctx, orgID).EffectiveBudget(); got != 50 {
		t.Errorf("granted 50, effective = %v", got)
	}
	if code, _ := call(t, mux, "PUT", path, `{"plan":"pro","budget_usd":-3}`, "Bearer "+secret); code != 400 {
		t.Errorf("negative grant = %d, want 400", code)
	}
	if code, _ := call(t, mux, "PUT", path, `{"plan":"gold"}`, "Bearer "+secret); code != 400 {
		t.Errorf("unknown plan = %d, want 400", code)
	}
	if code, _ := call(t, mux, "PUT", "/api/operator/orgs/ffffffffffffffffffffffffffffffff/plan", `{"plan":"pro"}`, "Bearer "+secret); code != 404 {
		t.Errorf("missing org = %d, want 404", code)
	}
	// The listing, narrowed to the organisations an address belongs to.
	code, out = call(t, mux, "GET", "/api/operator/orgs?email=admin@example.com", "", "Bearer "+secret)
	orgs, _ := out["orgs"].([]any)
	if code != 200 || len(orgs) != 1 {
		t.Fatalf("list by email = %d %v", code, out)
	}
	row := orgs[0].(map[string]any)
	if row["plan"] != PlanPro || row["name"] != "Test Org" || row["id"] != orgPublic(t, st, orgID) ||
		!strings.HasSuffix(fmt.Sprint(row["plan_url"]), "/operator/plan?org="+orgPublic(t, st, orgID)) {
		t.Errorf("row = %v", row)
	}
	code, out = call(t, mux, "GET", "/api/operator/orgs?email=nobody@example.com", "", "Bearer "+secret)
	if orgs, _ := out["orgs"].([]any); code != 200 || len(orgs) != 0 {
		t.Errorf("list for a stranger = %d %v", code, out)
	}
	// Guessing is throttled: every attempt from an address counts, right or wrong.
	for i := 0; i < operatorAttemptsPerHour; i++ {
		call(t, mux, "PUT", path, `{"plan":"pro"}`, "Bearer wrong")
	}
	if code, _ := call(t, mux, "PUT", path, `{"plan":"pro"}`, "Bearer "+secret); code != 429 {
		t.Errorf("after %d attempts = %d, want 429 even for the right secret", operatorAttemptsPerHour, code)
	}
}

func TestOperatorPageNeedsSecretThenRemembers(t *testing.T) {
	ctx := context.Background()
	const secret = "correct-horse-battery-staple"
	b, mux, st := planBot(t, Config{FreePlanBudgetUSD: 5, OperatorSecret: secret, SupportEmail: testSupportEmail})
	orgID, _, _ := seedOrg(t, st, RoleAdmin)
	page := "/operator/plan?org=" + orgPublic(t, st, orgID)
	get := func(cookie string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", page, nil)
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: operatorCookie, Value: cookie})
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	post := func(form string, cookie string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/operator/plan", strings.NewReader(form))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: operatorCookie, Value: cookie})
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	// The link in the email, opened by whoever holds it: the id, a secret field, and nothing
	// about the organisation itself.
	w := get("")
	// Note what the anonymous page does NOT say: not the name, not the spend, and not a row
	// number either — the heading is deliberately anonymous, so the link tells its holder
	// nothing about the account or about how many accounts there are.
	if body := w.Body.String(); w.Code != 200 || !strings.Contains(body, "An organisation") || strings.Contains(body, "Test Org") || !strings.Contains(body, `name="secret"`) || !strings.Contains(body, `name="budget_usd"`) {
		t.Fatalf("anonymous page = %d:\n%s", w.Code, body)
	}
	// A wrong secret is refused and nothing changes.
	if w := post("org="+orgPublic(t, st, orgID)+"&plan=pro&secret=wrong", ""); w.Code != 401 || b.settings.Get(ctx, orgID).Plan != PlanFree {
		t.Fatalf("wrong secret = %d, plan=%q", w.Code, b.settings.Get(ctx, orgID).Plan)
	}
	// The right one moves the account, says so, and leaves a cookie derived from the secret.
	w = post(fmt.Sprintf("org=%s&plan=pro&budget_usd=40&secret=%s", orgPublic(t, st, orgID), secret), "")
	if body := w.Body.String(); w.Code != 200 || !strings.Contains(body, "Test Org is now on the pro plan with a $40.00 monthly budget.") {
		t.Fatalf("right secret = %d:\n%s", w.Code, body)
	}
	if s := b.settings.Get(ctx, orgID); s.Plan != PlanPro || s.EffectiveBudget() != 40 {
		t.Fatalf("the button did not move the plan with its budget: %q %v", s.Plan, s.EffectiveBudget())
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == operatorCookie {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value == secret || !cookie.HttpOnly || cookie.Path != "/operator" {
		t.Fatalf("cookie = %+v", cookie)
	}
	// It is a proof of the secret rather than the secret, and it runs out on its own: the v1
	// cookie was a fixed function of OPERATOR_SECRET, so anybody who read one held the operator
	// API until the secret was rotated, and nothing short of that took it back.
	if !operatorCookieOK(cookie.Value, secret, time.Now()) {
		t.Fatalf("the cookie it set does not check out: %q", cookie.Value)
	}
	if cookie.MaxAge <= 0 || time.Duration(cookie.MaxAge)*time.Second > operatorCookieTTL {
		t.Errorf("cookie MaxAge = %d seconds, want a positive value no longer than %s", cookie.MaxAge, operatorCookieTTL)
	}
	// With the cookie the page shows the organisation, the way back, and no secret field.
	w = get(cookie.Value)
	if body := w.Body.String(); w.Code != 200 || !strings.Contains(body, "Test Org") || !strings.Contains(body, "Move back to Free") || strings.Contains(body, `name="secret"`) {
		t.Fatalf("remembered page = %d:\n%s", w.Code, body)
	}
	// Mail clients prefetch links: a GET never changes the plan.
	if b.settings.Get(ctx, orgID).Plan != PlanPro {
		t.Fatal("a GET changed the plan")
	}
	// The cookie alone moves it back — that is the one-click the email is for.
	if w := post("org="+orgPublic(t, st, orgID)+"&plan=free", cookie.Value); w.Code != 200 || b.settings.Get(ctx, orgID).Plan != PlanFree {
		t.Fatalf("cookie-only move = %d, plan=%q", w.Code, b.settings.Get(ctx, orgID).Plan)
	}
	// Rotating the secret ends every cookie: the field comes back and the stale cookie is dropped.
	b.cfg.OperatorSecret = "rotated-secret-1234567"
	w = get(cookie.Value)
	if body := w.Body.String(); !strings.Contains(body, `name="secret"`) || strings.Contains(body, "Test Org") {
		t.Fatalf("stale cookie page:\n%s", body)
	}
	dropped := false
	for _, c := range w.Result().Cookies() {
		if c.Name == operatorCookie && c.MaxAge < 0 {
			dropped = true
		}
	}
	if !dropped {
		t.Error("a stale operator cookie was not cleared")
	}
	// A page with no organisation named is only a signpost.
	r := httptest.NewRequest("GET", "/operator/plan", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), "<form") {
		t.Errorf("bare page = %d:\n%s", w.Code, w.Body.String())
	}
}

// mailbox stands in for Resend: it keeps what would have left, so a test can read the message
// rather than the intention to send one.
type mailbox struct{ sent []Mail }

func (m *mailbox) Configured() bool { return true }

func (m *mailbox) Send(_ context.Context, msg Mail) error {
	m.sent = append(m.sent, msg)
	return nil
}

// The support request under a free account's monthly budget is written and sent by the server,
// and the operator link that answers it goes into that message and nowhere else. The console
// used to build a mailto: carrying the link, which handed the URL that moves an account between
// plans to every account that wanted moving.
func TestBudgetRequestMailsSupportAndKeepsTheOperatorLinkOffTheScreen(t *testing.T) {
	planRequests = newRateLimiter()
	cfg := Config{FreePlanBudgetUSD: 5, PlatformMonthlyBudgetUSDPerOrg: 25, SupportEmail: testSupportEmail}
	b, mux, st := planBot(t, cfg)
	ctx := context.Background()
	orgID, _, tok := seedOrg(t, st, RoleAdmin)
	box := &mailbox{}
	b.mail = box
	ask := func() (int, map[string]any) {
		return call(t, mux, "POST", "/api/plan/raise-budget", "", "Bearer "+tok)
	}

	code, out := ask()
	if code != 200 || out["ok"] != true || out["delivered"] != true {
		t.Fatalf("asking for pro: %d %v", code, out)
	}
	if len(box.sent) != 1 {
		t.Fatalf("%d messages sent, want 1", len(box.sent))
	}
	m := box.sent[0]
	link := "/operator/plan?org=" + orgPublic(t, st, orgID)
	if m.To != testSupportEmail || m.ReplyTo != "admin@example.com" {
		t.Errorf("to=%q reply-to=%q, want %q and the requester", m.To, m.ReplyTo, testSupportEmail)
	}
	if !strings.Contains(m.Body, link) || !strings.Contains(m.Body, "Test Org") || !strings.Contains(m.Subject, "Test Org") {
		t.Errorf("the message support reads: %q / %q", m.Subject, m.Body)
	}
	// Nothing of the operator's comes back down the wire to the account that asked.
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "operator") {
		t.Errorf("the answer carries the operator link: %s", raw)
	}
	// The organisation remembers it asked, so the banner on the next page says so rather than
	// offering the button again.
	if st.Setting(ctx, orgID, planRequestKey) == "" {
		t.Error("the request was not recorded against the organisation")
	}
	code, me := call(t, mux, "GET", "/api/me", "", "Bearer "+tok)
	plan, _ := me["plan"].(map[string]any)
	if code != 200 || plan == nil || plan["plan"] != PlanFree || plan["budget_usd"] != 5.0 || plan["requested_at"] == "" {
		t.Errorf("/api/me plan block: %d %v", code, me["plan"])
	}

	// Three a day, because each one lands in a person's inbox.
	for i := 0; i < 2; i++ {
		if code, out := ask(); code != 200 {
			t.Fatalf("ask %d: %d %v", i+2, code, out)
		}
	}
	if code, _ := ask(); code != http.StatusTooManyRequests {
		t.Errorf("a fourth request the same day: %d, want 429", code)
	}
	if len(box.sent) != 3 {
		t.Errorf("%d messages sent, want 3", len(box.sent))
	}

	// A viewer cannot ask: the answer opens a field they may not set either.
	planRequests = newRateLimiter()
	_, _, viewer := seedOrgAs(t, st, "viewer@example.com", RoleViewer)
	if code, _ := call(t, mux, "POST", "/api/plan/raise-budget", "", "Bearer "+viewer); code != http.StatusForbidden {
		t.Errorf("a viewer asked: %d, want 403", code)
	}

	// And a pro account has nobody to ask: its budget is its own.
	if _, err := b.setPlan(ctx, orgPublic(t, st, orgID), PlanPro, 0, "test"); err != nil {
		t.Fatal(err)
	}
	if code, _ := ask(); code != http.StatusBadRequest {
		t.Errorf("a pro account asked for pro: %d, want 400", code)
	}
	// The move answered the request, so nothing outstanding is left to show.
	if got := st.Setting(ctx, orgID, planRequestKey); got != "" {
		t.Errorf("the plan moved and the request is still outstanding: %q", got)
	}
	if len(box.sent) != 3 {
		t.Errorf("%d messages sent after the refusals, want 3", len(box.sent))
	}
}

// A deployment that publishes no support address must not name one. Every self-host starts this
// way — SUPPORT_EMAIL is empty until somebody sets it — and the failure this guards against is
// the quiet one: a stranger's bot telling their colleagues to write to an inbox belonging to
// whoever built the binary. The budget still holds; only the address goes away, and the button
// that would have sent the mail refuses rather than sending it somewhere.
func TestNoSupportAddressNamesNobody(t *testing.T) {
	planRequests = newRateLimiter()
	cfg := Config{FreePlanBudgetUSD: 5, MonthlyBudgetUSD: 20} // SupportEmail deliberately unset
	b, mux, st := planBot(t, cfg)
	ctx := context.Background()
	orgID, _, tok := seedOrg(t, st, RoleAdmin)
	a := &Agent{store: st, settings: b.settings, cfg: cfg}
	if _, err := st.db.ExecContext(ctx, `insert into usage (org_id, team_id, channel, cost_usd) values (?, '', '', 6)`, orgID); err != nil {
		t.Fatal(err)
	}
	// The cap is still enforced, and the refusal still says what the budget is.
	ok, why := a.budgetOK(ctx, orgID, "")
	if ok {
		t.Fatal("a free account spent past $5 and was not stopped")
	}
	if !strings.Contains(why, "$5.00") {
		t.Errorf("the refusal should still name the budget: %q", why)
	}
	if strings.Contains(why, "@") {
		t.Errorf("a deployment with no support address named one anyway: %q", why)
	}
	// And the Upgrade button refuses instead of mailing a stranger.
	box := &mailbox{}
	b.mail = box
	code, _ := call(t, mux, "POST", "/api/plan/raise-budget", "", "Bearer "+tok)
	if code == 200 {
		t.Error("the raise-budget button worked with no address to send to")
	}
	if len(box.sent) != 0 {
		t.Errorf("%d messages sent with no support address configured: %+v", len(box.sent), box.sent)
	}
}
