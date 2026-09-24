package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const apiTestKey = "sk-proj-console-test-key-24680"

// modelKeyAPI is the console on a bot whose candidate endpoints may be a server on loopback,
// which the real address check and transport refuse by design.
func modelKeyAPI(t *testing.T) (*Bot, *http.ServeMux, *Store, *modelServer, int64, string, *stubMailer) {
	t.Helper()
	b, mux, st := installTestBot(t)
	prevCheck, prevTransport := checkModelBaseURL, modelProbeTransport
	checkModelBaseURL = func(raw string) error {
		if strings.HasPrefix(raw, "http://127.0.0.1:") {
			return nil
		}
		return validModelBaseURL(raw)
	}
	modelProbeTransport = func() http.RoundTripper { return http.DefaultTransport }
	modelKeyTests = newRateLimiter()
	t.Cleanup(func() { checkModelBaseURL, modelProbeTransport = prevCheck, prevTransport })
	mail := &stubMailer{}
	b.mail = mail
	org, _, token := seedOrg(t, st, RoleAdmin)
	return b, mux, st, newModelServer(t), org, token, mail
}

func saveBody(url, key string) map[string]any {
	return map[string]any{"preset": "compatible", "base_url": url, "key": key, "default_model": "org-model", "embed_model": "org-embed"}
}

// A key is tried against its endpoint before it is kept, and nothing the console, the audit log
// or anybody's inbox is given afterwards carries it.
func TestSavingAKeyChecksItFirstAndNeverHandsItBack(t *testing.T) {
	b, mux, st, own, org, token, mail := modelKeyAPI(t)
	ctx := context.Background()
	code, body := authReq(t, mux, "PUT", "/api/settings/model-key", saveBody(own.URL, apiTestKey), token)
	if code != 200 {
		t.Fatalf("save = %d %v", code, body)
	}
	if own.hits["POST /chat/completions"] == 0 || own.hits["POST /embeddings"] == 0 || !own.sawKey(apiTestKey) {
		t.Errorf("the key was saved without being tried: %v", own.hits)
	}
	if key, _ := body["key"].(map[string]any); key["key_hint"] != "…4680" || body["active"] != true {
		t.Errorf("view after saving = %v", body)
	}
	for _, where := range []string{"/api/settings/model-key", "/api/settings", "/api/me"} {
		w := do(t, mux, "GET", where, token)
		if strings.Contains(w.Body.String(), apiTestKey) {
			t.Errorf("GET %s carries the key", where)
		}
	}
	events, _ := st.AuditEvents(ctx, org, AuditFilter{Action: "model_key."})
	if len(events) != 1 || events[0].Action != "model_key.saved" {
		t.Fatalf("audit = %+v", events)
	}
	if raw, _ := json.Marshal(events); strings.Contains(string(raw), apiTestKey) {
		t.Error("the audit log carries the key")
	}
	if len(mail.sent) != 1 || !strings.Contains(mail.sent[0].Subject, "model key") || strings.Contains(mail.sent[0].Body, apiTestKey) {
		t.Errorf("mail = %+v", mail.sent)
	}
	// And the console's model pickers now read the organisation's endpoint.
	b.agent = &Agent{settings: b.settings, endpoints: newModelEndpoints(b.cfg, NewLLM(Config{LLMBaseURL: "http://127.0.0.1:1"}), st, b.sealer, b.settings)}
	b.agent.endpoints.transport = http.DefaultTransport
	w := do(t, mux, "GET", "/api/models", token)
	var models struct {
		Source string `json:"source"`
		Own    bool   `json:"own"`
	}
	json.Unmarshal(w.Body.Bytes(), &models)
	if w.Code != 200 || !models.Own || !strings.HasPrefix(models.Source, "127.0.0.1:") {
		t.Errorf("GET /api/models = %d %s", w.Code, w.Body)
	}
}

// A key the endpoint refuses is not kept: a save is the moment every conversation starts going
// there, so a typo would be an outage.
func TestAKeyTheEndpointRefusesIsNotSaved(t *testing.T) {
	_, mux, st, own, org, token, _ := modelKeyAPI(t)
	own.status, own.errBody = 401, `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`
	code, body := authReq(t, mux, "PUT", "/api/settings/model-key", saveBody(own.URL, apiTestKey), token)
	if code != 422 || !strings.Contains(body["error"].(string), "nothing was saved") || !strings.Contains(body["error"].(string), "refused") {
		t.Errorf("save with a refused key = %d %v", code, body)
	}
	if ref, _ := st.ModelKeyRef(context.Background(), org); ref != nil {
		t.Errorf("a refused key was stored: %+v", ref)
	}
}

// The stored key goes only to the address it was saved for. With the key field left empty, a new
// address — to save or to test — is refused before anything is sent anywhere.
func TestAStoredKeyOnlyGoesWhereItWasSaved(t *testing.T) {
	_, mux, _, own, _, token, _ := modelKeyAPI(t)
	if code, body := authReq(t, mux, "PUT", "/api/settings/model-key", saveBody(own.URL, apiTestKey), token); code != 200 {
		t.Fatalf("save = %d %v", code, body)
	}
	elsewhere := newModelServer(t)
	for _, c := range []struct{ method, path string }{{"PUT", "/api/settings/model-key"}, {"POST", "/api/settings/model-key/test"}} {
		code, body := authReq(t, mux, c.method, c.path, saveBody(elsewhere.URL, ""), token)
		if code != 400 || !strings.Contains(body["error"].(string), "paste the key again") {
			t.Errorf("%s %s to a new address with no key = %d %v", c.method, c.path, code, body)
		}
	}
	if n := elsewhere.total(); n != 0 {
		t.Errorf("another address was sent %d requests: the stored key could have gone with them", n)
	}
	// At its own address it may: changing a model does not ask for the key again.
	next := saveBody(own.URL, "")
	next["default_model"] = "org-model"
	next["embed_model"] = ""
	if code, body := authReq(t, mux, "PUT", "/api/settings/model-key", next, token); code != 200 {
		t.Errorf("a model change at the same address = %d %v", code, body)
	}
	if code, body := authReq(t, mux, "POST", "/api/settings/model-key/test", map[string]any{"base_url": own.URL}, token); code != 200 || body["ok"] != true {
		t.Errorf("test of the stored key at its own address = %d %v", code, body)
	}
}

// A new key or address is where every conversation goes next, so it takes the proof two-factor
// takes; moving a model does not.
func TestANewKeyNeedsProofOfWhoIsAsking(t *testing.T) {
	_, mux, st, own, org, token, _ := modelKeyAPI(t)
	ctx := context.Background()
	members, _ := st.MembersOf(ctx, org)
	hash, _ := hashPassword(signupPassword)
	if err := st.SetPassword(ctx, members[0].UserID, hash); err != nil {
		t.Fatal(err)
	}
	// The refusal below counts towards this account's lockout, which is process-wide and keyed by
	// a user id the next test's fresh database will hand out again.
	logins.reset(proofKey(members[0].UserID))
	defer logins.reset(proofKey(members[0].UserID))
	code, body := authReq(t, mux, "PUT", "/api/settings/model-key", saveBody(own.URL, apiTestKey), token)
	if code != 403 || body["proof"] != "password" {
		t.Errorf("save with no password = %d %v", code, body)
	}
	if own.total() != 0 {
		t.Error("the endpoint was tried before the proof was checked")
	}
	with := saveBody(own.URL, apiTestKey)
	with["password"] = signupPassword
	if code, body := authReq(t, mux, "PUT", "/api/settings/model-key", with, token); code != 200 {
		t.Fatalf("save with the password = %d %v", code, body)
	}
	fixJobs := false
	change := saveBody(own.URL, "")
	change["fix_jobs"] = fixJobs
	if code, body := authReq(t, mux, "PUT", "/api/settings/model-key", change, token); code != 200 {
		t.Errorf("a change with no new key or address asked for proof: %d %v", code, body)
	}
	if ref, _ := st.ModelKeyRef(ctx, org); ref == nil || ref.FixJobs {
		t.Errorf("fix jobs switch = %+v", ref)
	}
}

// Under ORG_MODEL_KEYS=enterprise an organisation on another plan is told why, and cannot use the
// test route as a way to make this server fetch addresses.
func TestAKeyOutsideThePlanIsRefused(t *testing.T) {
	b, mux, _, own, _, token, _ := modelKeyAPI(t)
	b.cfg.OrgModelKeys = OrgModelKeysEnterprise
	b.cfg.SupportEmail = "support@example.com"
	for _, c := range []struct{ method, path string }{{"PUT", "/api/settings/model-key"}, {"POST", "/api/settings/model-key/test"}} {
		code, body := authReq(t, mux, c.method, c.path, saveBody(own.URL, apiTestKey), token)
		if code != 403 || !strings.Contains(body["error"].(string), "Enterprise plan") {
			t.Errorf("%s %s = %d %v", c.method, c.path, code, body)
		}
	}
	if own.total() != 0 {
		t.Errorf("the endpoint was called %d times for an organisation that may not bring a key", own.total())
	}
	w := do(t, mux, "GET", "/api/settings/model-key", token)
	var v modelKeyView
	json.Unmarshal(w.Body.Bytes(), &v)
	if v.Allowed || v.Policy != OrgModelKeysEnterprise {
		t.Errorf("view = %+v", v)
	}
}

// Removing the key is recorded and said to the admins, like saving one.
func TestRemovingTheKeyIsRecordedAndMailed(t *testing.T) {
	_, mux, st, own, org, token, mail := modelKeyAPI(t)
	ctx := context.Background()
	if code, body := authReq(t, mux, "PUT", "/api/settings/model-key", saveBody(own.URL, apiTestKey), token); code != 200 {
		t.Fatalf("save = %d %v", code, body)
	}
	if code, body := authReq(t, mux, "DELETE", "/api/settings/model-key", nil, token); code != 200 || body["key"] != nil {
		t.Errorf("remove = %d %v", code, body)
	}
	if ref, _ := st.ModelKeyRef(ctx, org); ref != nil {
		t.Errorf("still stored: %+v", ref)
	}
	events, _ := st.AuditEvents(ctx, org, AuditFilter{Action: "model_key.removed"})
	if len(events) != 1 {
		t.Errorf("audit = %+v", events)
	}
	if len(mail.sent) != 2 || !strings.Contains(mail.sent[1].Subject, "removed") {
		t.Errorf("mail = %+v", mail.sent)
	}
}

// Enterprise is what lets the hosted service's customers bring a key, so moving one off it with a
// key stored leaves its model calls refusing. The operator hears that in the answer to the move,
// and the organisation's view in the operator API shows where its calls were going.
func TestMovingAnOrgWithAKeyOffEnterpriseWarnsTheOperator(t *testing.T) {
	b, mux, st, _ := enterpriseBot(t, Config{OrgModelKeys: OrgModelKeysEnterprise})
	ctx := context.Background()
	orgID, _, _ := seedOrg(t, st, RoleAdmin)
	org := reloadOrg(t, st, orgID)
	if code, out := putDeal(t, mux, org, `{"plan":"enterprise","users":50}`); code != 200 {
		t.Fatalf("to enterprise = %d %v", code, out)
	}
	if err := st.PutModelKey(ctx, orgID, ModelKeyRef{BaseURL: "https://api.openai.com/v1", DefaultModel: "gpt-5-mini"},
		apiTestKey, "a@x", b.sealer); err != nil {
		t.Fatal(err)
	}
	b.settings.Invalidate(orgID)
	if !b.settings.Get(ctx, orgID).OwnKey.Active() {
		t.Fatal("an enterprise organisation's key is not in use")
	}
	code, out := putDeal(t, mux, org, `{"plan":"pro"}`)
	if code != 200 {
		t.Fatalf("to pro = %d %v", code, out)
	}
	if w, _ := out["warning"].(string); !strings.Contains(w, "api.openai.com") || !strings.Contains(w, "refuse") {
		t.Errorf("moving an organisation with a key off enterprise said nothing: %v", out)
	}
	mk, _ := out["model_key"].(map[string]any)
	if mk["host"] != "api.openai.com" || mk["active"] != false || mk["refusal"] == nil {
		t.Errorf("operator view of the key = %v", mk)
	}
	if raw, _ := json.Marshal(out); strings.Contains(string(raw), apiTestKey) {
		t.Error("the operator's view carries the key")
	}
}

// Removing the key moves every conversation too — to the deployment's provider — so it asks for
// the same proof a new key does.
func TestRemovingAKeyNeedsProofToo(t *testing.T) {
	_, mux, st, own, org, token, _ := modelKeyAPI(t)
	ctx := context.Background()
	if code, body := authReq(t, mux, "PUT", "/api/settings/model-key", saveBody(own.URL, apiTestKey), token); code != 200 {
		t.Fatalf("save = %d %v", code, body)
	}
	members, _ := st.MembersOf(ctx, org)
	hash, _ := hashPassword(signupPassword)
	if err := st.SetPassword(ctx, members[0].UserID, hash); err != nil {
		t.Fatal(err)
	}
	logins.reset(proofKey(members[0].UserID))
	defer logins.reset(proofKey(members[0].UserID))
	if code, body := authReq(t, mux, "DELETE", "/api/settings/model-key", nil, token); code != 403 || body["proof"] != "password" {
		t.Errorf("remove with no password = %d %v", code, body)
	}
	if ref, _ := st.ModelKeyRef(ctx, org); ref == nil {
		t.Fatal("removed without proof")
	}
	if code, body := authReq(t, mux, "DELETE", "/api/settings/model-key", map[string]any{"password": signupPassword}, token); code != 200 {
		t.Errorf("remove with the password = %d %v", code, body)
	}
	if ref, _ := st.ModelKeyRef(ctx, org); ref != nil {
		t.Errorf("still stored: %+v", ref)
	}
}
