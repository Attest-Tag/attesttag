package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// acaDispatcher starts one Azure Container Apps job execution per fix job. The job itself — its
// image, CPU, memory, replica timeout — is created by deploy/azure/worker.sh; this overrides
// only the container environment for one execution, so the job definition anyone can read in the
// portal never holds a repository token or a model key.
//
// The bot's managed identity needs Container Apps Contributor (or a role with
// Microsoft.App/jobs/start/action, /read and /executions/stop/action) on the worker job. The
// job's own identity is granted nothing.
//
// Like the ECS dispatcher this is plain HTTP against the ARM REST API rather than the Azure SDK:
// one token endpoint and three resource calls do not earn a dependency tree.
const acaAPIVersion = "2024-03-01"

type acaDispatcher struct {
	subscription, group, job string
	container                string
	tok                      *azureToken
	client                   *http.Client
}

func newACADispatcher(cfg Config) (*acaDispatcher, error) {
	if cfg.WorkerAzureSubscription == "" || cfg.WorkerAzureResourceGroup == "" {
		return nil, errors.New("WORKER_AZURE_SUBSCRIPTION and WORKER_AZURE_RESOURCE_GROUP are required in aca mode")
	}
	return &acaDispatcher{
		subscription: cfg.WorkerAzureSubscription,
		group:        cfg.WorkerAzureResourceGroup,
		job:          nonEmpty(cfg.WorkerJobName, "attesttag-worker"),
		container:    "worker",
		tok:          newAzureToken(),
		client:       &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func (d *acaDispatcher) Name() string { return "aca" }

func (d *acaDispatcher) jobID(target string) string {
	name := d.job
	if t := strings.TrimSpace(target); t != "" && safeJobName(t) {
		name = t
	}
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.App/jobs/%s", d.subscription, d.group, name)
}

// arm sends one ARM request. path is a resource id plus any action suffix; the api-version is
// added here so no caller can forget it, which ARM answers with a message that names nothing.
func (d *acaDispatcher) arm(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	u := "https://management.azure.com" + path + "?api-version=" + acaAPIVersion
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	tok, err := d.tok.get(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	raw, err := readAllLimit(resp)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return apiError(method+" "+shortRef(path), resp, raw)
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// acaTemplate is the slice of a job's definition an execution may override. Only the container
// env is changed, but the whole container has to be sent back: the start API replaces the
// template for that execution rather than merging into it, so an image left out of the request
// is an image left out of the execution.
type acaTemplate struct {
	Containers []acaContainer `json:"containers"`
}

type acaContainer struct {
	Name      string          `json:"name"`
	Image     string          `json:"image"`
	Command   []string        `json:"command,omitempty"`
	Args      []string        `json:"args,omitempty"`
	Env       []acaEnv        `json:"env,omitempty"`
	Resources json.RawMessage `json:"resources,omitempty"`
}

type acaEnv struct {
	Name      string `json:"name"`
	Value     string `json:"value,omitempty"`
	SecretRef string `json:"secretRef,omitempty"`
}

func (d *acaDispatcher) Start(ctx context.Context, j *Job, l JobLaunch) (string, error) {
	id := d.jobID(l.Target)
	// Read the job's own template first so the execution keeps the image and resources the
	// deploy script set. The alternative — naming the image in the bot's configuration — means
	// two places to change it and a silent mismatch when only one of them is updated.
	var job struct {
		Properties struct {
			Template acaTemplate `json:"template"`
		} `json:"properties"`
	}
	if err := d.arm(ctx, http.MethodGet, id, nil, &job); err != nil {
		return "", fmt.Errorf("read job %s: %w", shortRef(id), err)
	}
	tmpl := job.Properties.Template
	if len(tmpl.Containers) == 0 {
		return "", fmt.Errorf("job %s has no container to run", shortRef(id))
	}
	target := 0
	for i, c := range tmpl.Containers {
		if c.Name == d.container {
			target = i
			break
		}
	}
	// The launch variables win over anything of the same name already on the job, so a stale
	// WORKER_MODE baked into the definition cannot send the worker down the wrong path.
	set := map[string]bool{}
	var env []acaEnv
	for _, kv := range envPairs(l.env("aca")) {
		env = append(env, acaEnv{Name: kv[0], Value: kv[1]})
		set[kv[0]] = true
	}
	for _, e := range tmpl.Containers[target].Env {
		if !set[e.Name] {
			env = append(env, e)
		}
	}
	tmpl.Containers[target].Env = env

	var out struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := d.arm(ctx, http.MethodPost, id+"/start", map[string]any{"template": tmpl}, &out); err != nil {
		return "", fmt.Errorf("start job %s: %w", shortRef(id), err)
	}
	switch {
	case out.ID != "":
		return out.ID, nil
	case out.Name != "":
		return id + "/executions/" + out.Name, nil
	}
	return "", errors.New("Azure did not return an execution id")
}

func (d *acaDispatcher) Cancel(ctx context.Context, ref string) error {
	if !strings.Contains(ref, "/executions/") {
		return fmt.Errorf("not a Container Apps execution id: %q", ref)
	}
	return d.arm(ctx, http.MethodPost, ref+"/stop", nil, nil)
}

func (d *acaDispatcher) Status(ctx context.Context, ref string) (DispatchStatus, error) {
	if !strings.Contains(ref, "/executions/") {
		return DispatchStatus{State: "unknown", Message: "not a Container Apps execution id"}, nil
	}
	var out struct {
		Properties struct {
			Status  string `json:"status"`
			EndTime string `json:"endTime"`
		} `json:"properties"`
	}
	if err := d.arm(ctx, http.MethodGet, ref, nil, &out); err != nil {
		return DispatchStatus{State: "unknown", Message: err.Error()}, err
	}
	switch strings.ToLower(out.Properties.Status) {
	case "succeeded":
		return DispatchStatus{State: "succeeded"}, nil
	case "failed", "degraded":
		return DispatchStatus{State: "failed", Message: out.Properties.Status}, nil
	case "stopped":
		return DispatchStatus{State: "cancelled"}, nil
	case "running", "processing":
		return DispatchStatus{State: "running"}, nil
	}
	if out.Properties.EndTime != "" {
		return DispatchStatus{State: "failed", Message: "execution ended as " + out.Properties.Status}, nil
	}
	return DispatchStatus{State: "pending"}, nil
}

// acaConsoleURL links the job's execution history rather than the execution, which the portal
// only resolves from inside its own navigation — the same reason cloudrun links the job.
func acaConsoleURL(ref string) string {
	i := strings.Index(ref, "/executions/")
	if i < 0 || !strings.Contains(ref, "/providers/Microsoft.App/jobs/") {
		return ""
	}
	return "https://portal.azure.com/#@/resource" + ref[:i] + "/executionHistory"
}

// ---- the ARM token ----

// azureToken is a management-plane bearer token from whichever of the three sources answers:
//
//  1. a Container Apps / App Service managed identity (IDENTITY_ENDPOINT + IDENTITY_HEADER),
//     which is what deploy/azure/containerapps.sh gives the app
//  2. an IMDS managed identity, for the same thing on a VM or AKS node
//  3. a service principal (AZURE_TENANT_ID / AZURE_CLIENT_ID / AZURE_CLIENT_SECRET), which is
//     the escape hatch for running the bot outside Azure against an Azure worker
type azureToken struct {
	mu      sync.Mutex
	tok     string
	expires time.Time
	client  *http.Client
}

func newAzureToken() *azureToken {
	return &azureToken{client: &http.Client{Timeout: 15 * time.Second}}
}

const azureARMResource = "https://management.azure.com/"

func (a *azureToken) get(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tok != "" && time.Now().Add(2*time.Minute).Before(a.expires) {
		return a.tok, nil
	}
	tok, exp, err := a.fetch(ctx)
	if err != nil {
		return "", err
	}
	a.tok, a.expires = tok, exp
	return tok, nil
}

func (a *azureToken) fetch(ctx context.Context) (string, time.Time, error) {
	if ep, hdr := os.Getenv("IDENTITY_ENDPOINT"), os.Getenv("IDENTITY_HEADER"); ep != "" && hdr != "" {
		u := ep + "?api-version=2019-08-01&resource=" + url.QueryEscape(azureARMResource)
		if id := os.Getenv("AZURE_CLIENT_ID"); id != "" {
			u += "&client_id=" + url.QueryEscape(id)
		}
		return a.token(ctx, http.MethodGet, u, map[string]string{"X-IDENTITY-HEADER": hdr}, nil)
	}
	if tenant, id, sec := os.Getenv("AZURE_TENANT_ID"), os.Getenv("AZURE_CLIENT_ID"), os.Getenv("AZURE_CLIENT_SECRET"); tenant != "" && id != "" && sec != "" {
		form := url.Values{"grant_type": {"client_credentials"}, "client_id": {id},
			"client_secret": {sec}, "scope": {azureARMResource + ".default"}}
		return a.token(ctx, http.MethodPost, "https://login.microsoftonline.com/"+tenant+"/oauth2/v2.0/token",
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, strings.NewReader(form.Encode()))
	}
	u := "http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01&resource=" + url.QueryEscape(azureARMResource)
	return a.token(ctx, http.MethodGet, u, map[string]string{"Metadata": "true"}, nil)
}

func (a *azureToken) token(ctx context.Context, method, u string, hdr map[string]string, body *strings.Reader) (string, time.Time, error) {
	var req *http.Request
	var err error
	if body != nil {
		req, err = http.NewRequestWithContext(ctx, method, u, body)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, u, nil)
	}
	if err != nil {
		return "", time.Time{}, err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("azure token: %w", err)
	}
	raw, err := readAllLimit(resp)
	if err != nil {
		return "", time.Time{}, err
	}
	if resp.StatusCode/100 != 2 {
		return "", time.Time{}, apiError("azure token", resp, raw)
	}
	// expires_in is seconds everywhere; the IMDS and App Service endpoints send it as a string
	// and the login endpoint as a number, so it is read loosely and defaulted when absent.
	var doc struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   any    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", time.Time{}, err
	}
	if doc.AccessToken == "" {
		return "", time.Time{}, errors.New("azure token: the response carried no access_token")
	}
	secs := 3600.0
	switch v := doc.ExpiresIn.(type) {
	case float64:
		secs = v
	case string:
		fmt.Sscanf(v, "%f", &secs)
	}
	return doc.AccessToken, time.Now().Add(time.Duration(secs) * time.Second), nil
}
