package app

import (
	"context"
	"strings"
	"testing"
)

func testSealer(t *testing.T) *Sealer {
	t.Helper()
	fixedMasterKey(t)
	s, err := NewSealer()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOrgModelKeysOf(t *testing.T) {
	for in, want := range map[string]string{
		"":             OrgModelKeysAll,
		"all":          OrgModelKeysAll,
		"ALL":          OrgModelKeysAll,
		"off":          OrgModelKeysOff,
		"enterprise":   OrgModelKeysEnterprise,
		" Enterprise ": OrgModelKeysEnterprise,
		"yes":          OrgModelKeysEnterprise, // a typo narrows it, and LoadConfig says so
	} {
		if got := orgModelKeysOf(in); got != want {
			t.Errorf("orgModelKeysOf(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestOwnKeyAllowedFollowsThePlan(t *testing.T) {
	for _, c := range []struct {
		mode, plan string
		want       bool
	}{
		{OrgModelKeysAll, PlanFree, true},
		{OrgModelKeysAll, PlanEnterprise, true},
		{OrgModelKeysEnterprise, PlanEnterprise, true},
		{OrgModelKeysEnterprise, PlanPro, false},
		{OrgModelKeysEnterprise, PlanFree, false},
		{OrgModelKeysOff, PlanEnterprise, false},
	} {
		if got := (Config{OrgModelKeys: c.mode}).ownKeyAllowed(c.plan); got != c.want {
			t.Errorf("ORG_MODEL_KEYS=%s, plan %s: allowed = %v, want %v", c.mode, c.plan, got, c.want)
		}
	}
}

func TestModelKeyRoundTrip(t *testing.T) {
	st := testStore(t)
	sealer := testSealer(t)
	ctx := context.Background()

	if ref, err := st.ModelKeyRef(ctx, 1); ref != nil || err != nil {
		t.Fatalf("a fresh org has a key: %+v, %v", ref, err)
	}
	if _, ok, err := st.ModelKeySecret(ctx, 1, sealer); ok || err != nil {
		t.Fatalf("a fresh org's secret = ok %v, %v", ok, err)
	}
	// A save with no key and nothing stored has nothing to keep.
	if err := st.PutModelKey(ctx, 1, ModelKeyRef{BaseURL: "https://api.openai.com/v1"}, "", "a@x", sealer); err == nil {
		t.Error("a keyless save with no stored key was accepted")
	}

	ref := ModelKeyRef{Preset: "openai", BaseURL: "https://api.openai.com/v1", DefaultModel: "gpt-5-mini",
		EmbedModel: "text-embedding-3-small", FixJobs: true}
	if err := st.PutModelKey(ctx, 1, ref, "sk-proj-first-secret-9876", "a@x", sealer); err != nil {
		t.Fatal(err)
	}
	got, err := st.ModelKeyRef(ctx, 1)
	if err != nil || got == nil {
		t.Fatalf("ref = %+v, %v", got, err)
	}
	if got.KeyHint != "…9876" || got.KeyFP != secretFingerprint("sk-proj-first-secret-9876") || got.Host() != "api.openai.com" ||
		got.DefaultModel != "gpt-5-mini" || !got.FixJobs || got.UpdatedBy != "a@x" || got.LastOKAt == "" || got.Failing() {
		t.Errorf("ref after the first save = %+v", got)
	}
	if key, ok, err := st.ModelKeySecret(ctx, 1, sealer); key != "sk-proj-first-secret-9876" || !ok || err != nil {
		t.Errorf("secret = %q %v %v", key, ok, err)
	}

	// Changing a model keeps the key; the key is never re-typed to move a dropdown. The save names
	// the key it was made against, and one made against a key since replaced is refused.
	stale := ref
	stale.KeyFP = "0000000000000000"
	if err := st.PutModelKey(ctx, 1, stale, "", "b@x", sealer); err != errModelKeyChanged {
		t.Errorf("a save against a replaced key = %v, want errModelKeyChanged", err)
	}
	ref.DefaultModel, ref.FixJobs, ref.KeyFP = "gpt-4.1-mini", false, got.KeyFP
	if err := st.PutModelKey(ctx, 1, ref, "", "b@x", sealer); err != nil {
		t.Fatal(err)
	}
	if key, _, _ := st.ModelKeySecret(ctx, 1, sealer); key != "sk-proj-first-secret-9876" {
		t.Errorf("a model change replaced the key with %q", key)
	}
	if got, _ := st.ModelKeyRef(ctx, 1); got.DefaultModel != "gpt-4.1-mini" || got.FixJobs || got.UpdatedBy != "b@x" {
		t.Errorf("ref after a model change = %+v", got)
	}

	// Another organisation sees none of it.
	if other, _ := st.ModelKeyRef(ctx, 2); other != nil {
		t.Errorf("org 2 reads org 1's endpoint: %+v", other)
	}
	if key, ok, _ := st.ModelKeySecret(ctx, 2, sealer); key != "" || ok {
		t.Errorf("org 2 reads org 1's key: %q", key)
	}

	// The status is what the console's banner and the operator read.
	if err := st.MarkModelKeyStatus(ctx, 1, "401: Incorrect API key provided"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.ModelKeyRef(ctx, 1); !got.Failing() || !strings.Contains(got.LastError, "401") {
		t.Errorf("after an error: %+v", got)
	}
	// A new key starts over: it has just been checked, so it is not failing.
	if err := st.PutModelKey(ctx, 1, ref, "sk-proj-second-secret-1234", "a@x", sealer); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.ModelKeyRef(ctx, 1); got.Failing() || got.LastError != "" || got.KeyHint != "…1234" {
		t.Errorf("after a new key: %+v", got)
	}

	if removed, err := st.DeleteModelKey(ctx, 1); !removed || err != nil {
		t.Errorf("delete = %v, %v", removed, err)
	}
	if removed, _ := st.DeleteModelKey(ctx, 1); removed {
		t.Error("deleted twice")
	}
	if ref, _ := st.ModelKeyRef(ctx, 1); ref != nil {
		t.Errorf("after delete: %+v", ref)
	}
}

// Settings go to every member of the organisation, so nothing about the key may ride on them:
// not the key, not its tail, not where the organisation's conversations are sent.
func TestTheModelKeyIsNotInSettings(t *testing.T) {
	b, mux, st := installTestBot(t)
	org, _, token := seedOrg(t, st, RoleAdmin)
	ctx := context.Background()
	if err := st.PutModelKey(ctx, org, ModelKeyRef{Preset: "compatible", BaseURL: "https://llm.secret-host.example/v1",
		DefaultModel: "house-model-7b"}, "sk-verysecretkey-abcdefgh-5555", "a@x", b.sealer); err != nil {
		t.Fatal(err)
	}
	b.settings.Invalidate(org)
	if !b.settings.Get(ctx, org).OwnKey.Present {
		t.Fatal("the settings cache does not see the stored key")
	}
	w := do(t, mux, "GET", "/api/settings", token)
	if w.Code != 200 {
		t.Fatalf("GET /api/settings = %d: %s", w.Code, w.Body)
	}
	for _, leak := range []string{"sk-verysecretkey", "5555", "secret-host", secretFingerprint("sk-verysecretkey-abcdefgh-5555"), "OwnKey"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("GET /api/settings carries %q", leak)
		}
	}
}

func TestTheSettingsCacheSaysWhetherTheKeyMayBeUsed(t *testing.T) {
	st := testStore(t)
	sealer := testSealer(t)
	ctx := context.Background()
	if err := st.PutModelKey(ctx, 1, ModelKeyRef{BaseURL: "https://api.openai.com/v1", DefaultModel: "gpt-5-mini"},
		"sk-proj-abcdefghijkl", "a@x", sealer); err != nil {
		t.Fatal(err)
	}
	// Org 1 is on the free plan: under enterprise-only its key is stored and may not be used,
	// which blocks it rather than sending it to the deployment's key.
	k := newSettingsCache(st, Config{OrgModelKeys: OrgModelKeysEnterprise}).Get(ctx, 1).OwnKey
	if !k.Present || k.Allowed || k.Active() || !k.Blocked() {
		t.Errorf("free org under enterprise-only: %+v active=%v blocked=%v", k, k.Active(), k.Blocked())
	}
	k = newSettingsCache(st, Config{OrgModelKeys: OrgModelKeysAll}).Get(ctx, 1).OwnKey
	if !k.Active() || k.Blocked() {
		t.Errorf("under all: active=%v blocked=%v", k.Active(), k.Blocked())
	}
	// No key at all is neither.
	if k := newSettingsCache(st, Config{OrgModelKeys: OrgModelKeysAll}).Get(ctx, 2).OwnKey; k.Active() || k.Blocked() {
		t.Errorf("an org with no key: %+v", k)
	}
}

// A caller that gave up while the settings were loading is not a failed read: reading it as one
// would stop the organisation's model calls, and keep them stopped for as long as it was cached.
func TestACancelledCallerDoesNotBlockTheKey(t *testing.T) {
	st := testStore(t)
	sealer := testSealer(t)
	if err := st.PutModelKey(context.Background(), 1, ModelKeyRef{BaseURL: "https://api.openai.com/v1", DefaultModel: "gpt-5-mini"},
		"sk-proj-abcdefghijkl", "a@x", sealer); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if k := newSettingsCache(st, Config{}).Get(ctx, 1).OwnKey; k.Unknown || !k.Active() {
		t.Errorf("a cancelled load read the key as %+v", k)
	}
}

// The plan decides whether an enterprise key may be used, so a load whose caller gave up must not
// read the plan as free — and cache that — any more than it may read the key as missing.
func TestACancelledLoadDoesNotBlockAnEnterpriseKey(t *testing.T) {
	st := testStore(t)
	sealer := testSealer(t)
	ctx := context.Background()
	org := testOrg(t, st, "Enterprise Ltd")
	if err := st.SetOrgPlan(ctx, org.ID, PlanEnterprise, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.PutModelKey(ctx, org.ID, ModelKeyRef{BaseURL: "https://api.openai.com/v1", DefaultModel: "gpt-5-mini"},
		"sk-proj-enterprise-key-1234", "a@x", sealer); err != nil {
		t.Fatal(err)
	}
	sc := newSettingsCache(st, Config{OrgModelKeys: OrgModelKeysEnterprise})
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if k := sc.Get(cancelled, org.ID).OwnKey; !k.Active() {
		t.Errorf("a cancelled load read the enterprise key as %+v", k)
	}
	if k := sc.Get(ctx, org.ID).OwnKey; !k.Active() {
		t.Errorf("the next load, from the cache, = %+v", k)
	}
}
