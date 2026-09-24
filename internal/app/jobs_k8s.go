package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// k8sDispatcher creates one batch/v1 Job per fix job in the cluster the bot is running in.
//
// This is the one platform where the bot writes the whole pod spec rather than starting a job
// object somebody else created, because Kubernetes has no "run this again with different
// environment" call — a Job is created, runs once and is collected. WORKER_IMAGE is therefore
// configuration here in a way it is not on Cloud Run, ECS or Container Apps, and the Helm chart
// renders it from the same values as the bot's own image.
//
// The bot's service account needs create, get, list and delete on batch/jobs in the worker
// namespace and nothing else; deploy/helm/attest-tag/templates/worker-rbac.yaml is that Role.
// The worker's own service account is granted nothing, and automountServiceAccountToken is off
// so a compromised worker has no cluster credential at all.
const (
	k8sTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	k8sCAPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	k8sNSPath    = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	// A finished Job is collected an hour after it ends. Long enough that the reconciler, which
	// runs every thirty seconds, has always seen the outcome; short enough that a busy cluster
	// is not left holding a week of completed Jobs.
	k8sTTLSeconds = 3600
)

type k8sDispatcher struct {
	base      string // https://host:port
	namespace string
	image     string
	sa        string
	pull      string
	arch      string // kubernetes.io/arch the pod is pinned to; "" pins nothing
	cpu, mem  string
	client    *http.Client

	mu       sync.Mutex
	token    string
	tokenAge time.Time
}

func newK8sDispatcher(cfg Config) (*k8sDispatcher, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), env("KUBERNETES_SERVICE_PORT", "443")
	if host == "" {
		return nil, errors.New("KUBERNETES_SERVICE_HOST is not set: k8s mode runs the bot inside the cluster, with a service account token mounted")
	}
	ns := cfg.WorkerK8sNamespace
	if ns == "" {
		b, err := os.ReadFile(k8sNSPath)
		if err != nil {
			return nil, fmt.Errorf("WORKER_K8S_NAMESPACE is not set and the service account namespace could not be read: %w", err)
		}
		ns = strings.TrimSpace(string(b))
	}
	if cfg.WorkerJobName == "" {
		return nil, errors.New("WORKER_IMAGE is required in k8s mode: it is the worker image the Job runs")
	}
	pool := x509.NewCertPool()
	ca, err := os.ReadFile(k8sCAPath)
	if err != nil {
		return nil, fmt.Errorf("cluster CA: %w", err)
	}
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("cluster CA: not a PEM certificate")
	}
	d := &k8sDispatcher{
		base:      fmt.Sprintf("https://%s", netJoin(host, port)),
		namespace: ns,
		image:     cfg.WorkerJobName,
		sa:        cfg.WorkerK8sServiceAccount,
		pull:      cfg.WorkerK8sPullSecret,
		arch:      cfg.WorkerK8sArch,
		cpu:       cfg.WorkerCPU,
		mem:       cfg.WorkerMemory,
		client: &http.Client{Timeout: 30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}},
	}
	if _, err := d.bearer(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *k8sDispatcher) Name() string { return "k8s" }

// netJoin brackets an IPv6 literal, which a cluster running on one will hand us in
// KUBERNETES_SERVICE_HOST without them.
func netJoin(host, port string) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

// bearer re-reads the projected service account token, which the kubelet rotates roughly hourly
// and which stops being accepted the moment it does. Caching it for a minute keeps the common
// path off the filesystem without ever holding one long enough to go stale.
func (d *k8sDispatcher) bearer() (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.token != "" && time.Since(d.tokenAge) < time.Minute {
		return d.token, nil
	}
	b, err := os.ReadFile(k8sTokenPath)
	if err != nil {
		return "", fmt.Errorf("service account token: %w", err)
	}
	d.token, d.tokenAge = strings.TrimSpace(string(b)), time.Now()
	return d.token, nil
}

func (d *k8sDispatcher) do(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, d.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	tok, err := d.bearer()
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
		return apiError(method+" "+path, resp, raw)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// safeImageRef admits a container image reference and nothing else, so a routed target from
// configuration cannot become a path or a flag. Registry, repository, tag and digest only.
func safeImageRef(s string) bool {
	if s == "" || len(s) > 255 || strings.ContainsAny(s, " \t\n\"'\\") {
		return false
	}
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '.' || r == '-' || r == '_' || r == '/' || r == ':' || r == '@'
		if !ok {
			return false
		}
	}
	return !strings.HasPrefix(s, "/") && !strings.Contains(s, "//")
}

func (d *k8sDispatcher) Start(ctx context.Context, j *Job, l JobLaunch) (string, error) {
	image := d.image
	if t := strings.TrimSpace(l.Target); t != "" && safeImageRef(t) {
		image = t
	}
	var env []map[string]string
	for _, kv := range envPairs(l.env("k8s")) {
		env = append(env, map[string]string{"name": kv[0], "value": kv[1]})
	}
	suffix := make([]byte, 4)
	rand.Read(suffix)
	name := fmt.Sprintf("attesttag-job-%d-%s", j.ID, hex.EncodeToString(suffix))

	pod := map[string]any{
		"restartPolicy": "Never",
		// The worker has no business talking to the API server, and a mounted token is the
		// usual way a compromised workload finds it can.
		"automountServiceAccountToken": false,
		"containers": []map[string]any{{
			"name":  "worker",
			"image": image,
			"env":   env,
			"resources": map[string]any{
				"requests": map[string]string{"cpu": d.cpu, "memory": d.mem},
				"limits":   map[string]string{"memory": d.mem},
			},
		}},
	}
	if d.sa != "" {
		pod["serviceAccountName"] = d.sa
	}
	if d.pull != "" {
		pod["imagePullSecrets"] = []map[string]string{{"name": d.pull}}
	}
	// The published worker images are linux/amd64 only, and a pod on a node of another
	// architecture fails with an exec format error that reads like a broken build rather than
	// a wrong machine. WORKER_K8S_ARCH=any leaves the choice to the scheduler (workerK8sArch).
	if d.arch != "" {
		pod["nodeSelector"] = map[string]string{"kubernetes.io/arch": d.arch}
	}
	manifest := map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{
			"name":      name,
			"namespace": d.namespace,
			"labels": map[string]string{
				"app.kubernetes.io/name":      "attest-tag",
				"app.kubernetes.io/component": "fix-worker",
				"attesttag.dev/job":           fmt.Sprint(j.ID),
			},
		},
		"spec": map[string]any{
			// One attempt. A retried pod would find its job already claimed and report a
			// confusing failure over the top of the real one.
			"backoffLimit":            0,
			"activeDeadlineSeconds":   j.TimeoutS + 120,
			"ttlSecondsAfterFinished": k8sTTLSeconds,
			"template":                map[string]any{"spec": pod},
		},
	}
	if err := d.do(ctx, http.MethodPost, "/apis/batch/v1/namespaces/"+d.namespace+"/jobs", manifest, nil); err != nil {
		return "", fmt.Errorf("create job %s: %w", name, err)
	}
	return d.namespace + "/" + name, nil
}

func k8sRefParts(ref string) (ns, name string, ok bool) {
	ns, name, ok = strings.Cut(ref, "/")
	return ns, name, ok && ns != "" && name != ""
}

func (d *k8sDispatcher) Cancel(ctx context.Context, ref string) error {
	ns, name, ok := k8sRefParts(ref)
	if !ok {
		return fmt.Errorf("not a namespace/name reference: %q", ref)
	}
	// Background propagation so the pod goes with the Job; deleting the Job alone leaves the
	// pod running, which is the opposite of cancelling.
	return d.do(ctx, http.MethodDelete, "/apis/batch/v1/namespaces/"+ns+"/jobs/"+name+"?propagationPolicy=Background", nil, nil)
}

func (d *k8sDispatcher) Status(ctx context.Context, ref string) (DispatchStatus, error) {
	ns, name, ok := k8sRefParts(ref)
	if !ok {
		return DispatchStatus{State: "unknown", Message: "not a namespace/name reference"}, nil
	}
	var out struct {
		Status struct {
			Active     int `json:"active"`
			Succeeded  int `json:"succeeded"`
			Failed     int `json:"failed"`
			Conditions []struct {
				Type, Status, Reason, Message string
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := d.do(ctx, http.MethodGet, "/apis/batch/v1/namespaces/"+ns+"/jobs/"+name, nil, &out); err != nil {
		// A Job the TTL collected, or one a cancel deleted, is gone rather than failed: the
		// reconciler's silence rule is the right judge of what that means for the fix job.
		if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "Not Found") {
			return DispatchStatus{State: "unknown", Message: "the Job is no longer in the cluster"}, nil
		}
		return DispatchStatus{State: "unknown", Message: err.Error()}, err
	}
	var reason, msg string
	for _, c := range out.Status.Conditions {
		if c.Status == "True" && (c.Type == "Failed" || c.Type == "Complete") {
			reason, msg = c.Reason, c.Message
		}
	}
	switch {
	case out.Status.Succeeded > 0:
		return DispatchStatus{State: "succeeded"}, nil
	case out.Status.Failed > 0 || reason != "":
		// DeadlineExceeded is the activeDeadlineSeconds above, which is the bot's own timeout
		// plus two minutes — worth naming, because it reads as an unexplained failure.
		return DispatchStatus{State: "failed", Message: strings.TrimSpace(nonEmpty(msg, reason))}, nil
	case out.Status.Active > 0:
		return DispatchStatus{State: "running"}, nil
	}
	return DispatchStatus{State: "pending"}, nil
}
