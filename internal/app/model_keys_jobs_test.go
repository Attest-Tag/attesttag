package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// An Azure key: thirty-two hex characters that match none of redact's shapes, which is why the
// scrub masks the organisation's key by value.
const azureLikeKey = "0123456789abcdef0123456789abcdef"

// ownKeyJobs puts the harness's organisation on its own key, with an agent whose resolver prices
// from a catalogue and cannot reach anything else.
func ownKeyJobs(t *testing.T, h *jobHarness, fixJobs bool) {
	t.Helper()
	ctx := context.Background()
	if err := h.b.store.PutModelKey(ctx, orgID, ModelKeyRef{Preset: "openai", BaseURL: "https://acme.openai.azure.com/openai/v1",
		DefaultModel: "gpt-5-mini", FixJobs: fixJobs}, azureLikeKey, "a@x", h.b.sealer); err != nil {
		t.Fatal(err)
	}
	h.b.settings.Invalidate(orgID)
	eps := newModelEndpoints(h.b.cfg, catalogueLLM(t, pricedCatalogue), h.b.store, h.b.sealer, h.b.settings)
	eps.transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })
	h.b.jobs.agent = &Agent{endpoints: eps, settings: h.b.settings, store: h.b.store}
}

// A job for an organisation on its own key runs on that key, on its endpoint: no per-job key is
// minted on the deployment's provisioning account, its spend is recorded as the organisation's
// and priced from the catalogue, and the key never survives into anything the worker reported.
func TestAFixJobRunsOnTheOrganisationsOwnKey(t *testing.T) {
	h := newJobHarness(t, nil)
	ctx := context.Background()
	ownKeyJobs(t, h, true)

	j, err := h.b.jobs.Dispatch(ctx, DispatchRequest{OrgID: orgID, TeamID: "T1", Spec: testSpec(h.connID), ApprovedBy: "U2", Approval: "confirm"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if j.KeyOwner != keyOwnerOrg || j.Model != "gpt-5-mini" {
		t.Errorf("job on %q with model %q; want the organisation's key and its default model", j.KeyOwner, j.Model)
	}
	token := h.fd.launches[0].Token
	code, body := h.call(t, "POST", "/api/worker/jobs/1/claim", token, JobClaimRequest{Worker: JobWorkerInfo{Mode: "fake"}}, "")
	secrets, _ := body["secrets"].(map[string]any)
	llm, _ := secrets["llm"].(map[string]any)
	if code != 200 || llm["api_key"] != azureLikeKey || llm["base_url"] != "https://acme.openai.azure.com/openai/v1" || llm["model"] != "gpt-5-mini" {
		t.Fatalf("claim: %d %v", code, body)
	}
	if llm["per_job_key"] == true {
		t.Error("the organisation's own key was marked as minted for this job")
	}
	if n := len(h.or.minted()); n != 0 {
		t.Errorf("minted %d keys on the deployment's provisioning account for a job on the organisation's key", n)
	}

	code, body = h.call(t, "POST", "/api/worker/jobs/1/events", token, JobEventsRequest{Events: []JobEvent{
		{Seq: 1, Kind: JobKindLog, Message: "engine started with OPENAI_API_KEY=" + azureLikeKey},
		{Seq: 2, Kind: JobKindUsage, Usage: &JobUsage{In: 1000, Out: 100}},
	}}, "")
	if code != 200 {
		t.Fatalf("events: %d %v", code, body)
	}
	evs, _ := h.b.store.JobEvents(ctx, orgID, j.ID, 0)
	for _, e := range evs {
		if strings.Contains(e.Message, azureLikeKey) {
			t.Errorf("a worker event kept the organisation's key: %q", e.Message)
		}
	}
	got, _ := h.b.store.Job(ctx, orgID, j.ID)
	if want := 1000*0.25e-6 + 100*2e-6; !near(got.CostUSD, want) {
		t.Errorf("job cost = %v; the worker reported tokens only, so it should be priced at %v", got.CostUSD, want)
	}
	var owner string
	if err := h.b.store.db.QueryRowContext(ctx, `select key_owner from usage where org_id=? order by id desc limit 1`, orgID).Scan(&owner); err != nil || owner != keyOwnerOrg {
		t.Errorf("the job's usage row is %q (%v); want the organisation's", owner, err)
	}
}

// A job is refused before anybody confirms it when it could only run on a key it may not use.
func TestAFixJobIsRefusedWhenTheOwnKeyCannotRunIt(t *testing.T) {
	h := newJobHarness(t, nil)
	ctx := context.Background()
	ownKeyJobs(t, h, false)
	if err := h.b.jobs.precheck(ctx, orgID, "T1", "C1", "9.9"); !errors.Is(err, errFixJobsOffOwnKey) {
		t.Errorf("precheck with fix jobs off for the key: %v", err)
	}
	// A key the plan does not allow blocks jobs as it blocks everything else.
	h.b.jobs.settings = newSettingsCache(h.b.store, Config{OrgModelKeys: OrgModelKeysEnterprise})
	if err := h.b.jobs.precheck(ctx, orgID, "T1", "C1", "9.9"); err == nil || !strings.Contains(err.Error(), "not part of its plan") {
		t.Errorf("precheck with a key the plan does not include: %v", err)
	}
	if n := len(h.fd.launches); n != 0 {
		t.Errorf("launched %d jobs", n)
	}
}

// A job keeps the key it was dispatched on. Removing the key, or bringing one, between dispatch
// and claim fails the job with the reason rather than running it on a key nobody confirmed.
func TestAFixJobKeepsTheKeyItWasDispatchedOn(t *testing.T) {
	h := newJobHarness(t, nil)
	ctx := context.Background()
	ownKeyJobs(t, h, true)
	if _, err := h.b.jobs.Dispatch(ctx, DispatchRequest{OrgID: orgID, TeamID: "T1", Spec: testSpec(h.connID), ApprovedBy: "U2", Approval: "confirm"}); err != nil {
		t.Fatal(err)
	}
	h.b.store.DeleteModelKey(ctx, orgID)
	h.b.settings.Invalidate(orgID)
	if code, _ := h.call(t, "POST", "/api/worker/jobs/1/claim", h.fd.launches[0].Token, JobClaimRequest{Worker: JobWorkerInfo{Mode: "fake"}}, ""); code == 200 {
		t.Error("a job dispatched on the organisation's key was handed a credential after the key was removed")
	}
	if j, _ := h.b.store.Job(ctx, orgID, 1); j == nil || !strings.Contains(j.Error, "since been removed") {
		t.Errorf("job after its key was removed: %+v", j)
	}

	// And the other way: dispatched on the deployment's key, claimed after the organisation
	// brought its own.
	spec := testSpec(h.connID)
	spec.ThreadTS = "2.2"
	if _, err := h.b.jobs.Dispatch(ctx, DispatchRequest{OrgID: orgID, TeamID: "T1", Spec: spec, ApprovedBy: "U2", Approval: "confirm"}); err != nil {
		t.Fatal(err)
	}
	ownKeyJobs(t, h, true)
	if code, _ := h.call(t, "POST", "/api/worker/jobs/2/claim", h.fd.launches[1].Token, JobClaimRequest{Worker: JobWorkerInfo{Mode: "fake"}}, ""); code == 200 {
		t.Error("a job dispatched on the deployment's key ran after the organisation brought its own")
	}
}

// A job's minted key is still that job's alone when a second claim reopens it, so it is still
// metered: without the flag the running cost stays $0 and the budget cancel cannot fire.
func TestAReclaimedJobKeepsMeteringItsOwnKey(t *testing.T) {
	h := newJobHarness(t, nil)
	ctx := context.Background()
	if _, err := h.b.jobs.Dispatch(ctx, DispatchRequest{OrgID: orgID, TeamID: "T1", Spec: testSpec(h.connID), ApprovedBy: "U2", Approval: "confirm"}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		code, body := h.call(t, "POST", "/api/worker/jobs/1/claim", h.fd.launches[0].Token, JobClaimRequest{Worker: JobWorkerInfo{Mode: "fake"}}, "")
		secrets, _ := body["secrets"].(map[string]any)
		llm, _ := secrets["llm"].(map[string]any)
		if code != 200 || llm["per_job_key"] != true {
			t.Errorf("claim %d: %d, per_job_key = %v", i, code, llm["per_job_key"])
		}
	}
	if n := len(h.or.minted()); n != 1 {
		t.Errorf("minted %d keys for one job", n)
	}
}
