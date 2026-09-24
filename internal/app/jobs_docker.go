package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// dockerDispatcher runs each fix job as a container on the Docker daemon the bot can reach,
// which is what makes the feature available to a `docker compose` deployment — the one shape
// with no job-running control plane of its own.
//
// Read this before turning it on: the bot needs the Docker socket, and access to the Docker
// socket is root on the host. That is a real widening of what a compromise of the bot is worth,
// which is why deploy/docker-compose.yml puts it behind a profile rather than mounting it by
// default, and why the socket is not mounted at all in the modes above — Cloud Run, ECS,
// Container Apps and Kubernetes each start the worker through an API that can be scoped to
// "start this one job and nothing else". Docker's API cannot. On a single machine that you own
// and that runs nothing else, that is a reasonable trade; on a shared host it is not.
const dockerAPIVersion = "v1.43"

type dockerDispatcher struct {
	image    string
	network  string
	nanoCPUs int64
	memory   int64
	client   *http.Client
}

func newDockerDispatcher(cfg Config) (*dockerDispatcher, error) {
	if cfg.WorkerJobName == "" {
		return nil, errors.New("WORKER_IMAGE is required in docker mode: it is the worker image to run")
	}
	host := cfg.WorkerDockerHost
	tr := &http.Transport{}
	switch {
	case strings.HasPrefix(host, "unix://"):
		path := strings.TrimPrefix(host, "unix://")
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}
	case strings.HasPrefix(host, "tcp://"), strings.HasPrefix(host, "http://"):
		// A plain TCP daemon is unauthenticated and unencrypted, so it is accepted but not
		// encouraged: anyone who can reach the port can run anything on that host as root.
		addr := host[strings.Index(host, "://")+3:]
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		}
	default:
		return nil, fmt.Errorf("WORKER_DOCKER_HOST must be unix:// or tcp://, not %q", host)
	}
	mem, err := parseQuantityBytes(cfg.WorkerMemory)
	if err != nil {
		return nil, fmt.Errorf("WORKER_MEMORY: %w", err)
	}
	cpus, err := strconv.ParseFloat(strings.TrimSpace(cfg.WorkerCPU), 64)
	if err != nil {
		return nil, fmt.Errorf("WORKER_CPU: %q is not a number of cores", cfg.WorkerCPU)
	}
	return &dockerDispatcher{
		image:    cfg.WorkerJobName,
		network:  cfg.WorkerDockerNetwork,
		nanoCPUs: int64(cpus * 1e9),
		memory:   mem,
		client:   &http.Client{Timeout: 60 * time.Second, Transport: tr},
	}, nil
}

func (d *dockerDispatcher) Name() string { return "docker" }

// parseQuantityBytes reads the Kubernetes spelling of a memory quantity — 4Gi, 512Mi, 2G — into
// bytes, because WORKER_MEMORY is one setting across every mode and Docker is the one that wants
// a number. A bare number is already bytes.
func parseQuantityBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	for _, suf := range []struct {
		s string
		m int64
	}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
		{"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}} {
		if strings.HasSuffix(s, suf.s) {
			mult, s = suf.m, strings.TrimSuffix(s, suf.s)
			break
		}
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a quantity like 4Gi or 512Mi", s)
	}
	return int64(n * float64(mult)), nil
}

// do speaks the Engine API. The host in the URL is ignored — the transport above dials the
// socket — but it has to be something, and "docker" is what the daemon logs.
func (d *dockerDispatcher) do(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker/"+dockerAPIVersion+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
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
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

const dockerJobLabel = "com.attesttag.job"

func (d *dockerDispatcher) Start(ctx context.Context, j *Job, l JobLaunch) (string, error) {
	image := d.image
	if t := strings.TrimSpace(l.Target); t != "" && safeImageRef(t) {
		image = t
	}
	var env []string
	env = append(env, l.env("docker")...)

	host := map[string]any{
		"NanoCpus": d.nanoCPUs,
		"Memory":   d.memory,
		// A worker clones a repository and installs its dependencies into the container's own
		// writable layer, so it needs no mount from the host and is given none.
		"AutoRemove": false, // the exit code has to outlive the process for Status to read it
	}
	if d.network != "" {
		host["NetworkMode"] = d.network
	}
	spec := map[string]any{
		"Image":      image,
		"Env":        env,
		"Labels":     map[string]string{dockerJobLabel: fmt.Sprint(j.ID)},
		"HostConfig": host,
	}
	name := fmt.Sprintf("attesttag-job-%d-%d", j.ID, time.Now().Unix())
	var created struct {
		ID      string   `json:"Id"`
		Warning []string `json:"Warnings"`
	}
	create := func() error {
		return d.do(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(name), spec, &created)
	}
	if err := create(); err != nil {
		// The Engine API does not pull on create the way `docker run` does: a missing image is a
		// 404 here, and without this the first fix job on a fresh machine fails with "No such
		// image" and nothing that suggests the fix is a pull.
		if !strings.Contains(err.Error(), "404") {
			return "", fmt.Errorf("create container from %s: %w", image, err)
		}
		if err := d.pull(ctx, image); err != nil {
			return "", fmt.Errorf("pull %s: %w", image, err)
		}
		if err := create(); err != nil {
			return "", fmt.Errorf("create container from %s: %w", image, err)
		}
	}
	if err := d.do(ctx, http.MethodPost, "/containers/"+created.ID+"/start", nil, nil); err != nil {
		// A container that was created but will not start would otherwise sit there forever.
		_ = d.do(ctx, http.MethodDelete, "/containers/"+created.ID+"?force=1", nil, nil)
		return "", fmt.Errorf("start container: %w", err)
	}
	// Sweep finished workers on the way past, so a compose deployment does not accumulate
	// exited containers. Best effort: a failure here must not fail the job that just started.
	go d.reap(context.WithoutCancel(ctx))
	return created.ID, nil
}

// splitImageRef separates a reference into what the Engine API wants as fromImage and tag. The
// last colon is only a tag separator when it comes after the last slash: in
// registry.example.com:5000/attesttag/worker it is a port, and splitting there would ask the
// daemon to pull a registry rather than an image. A digest splits on @ instead.
func splitImageRef(image string) (ref, tag string) {
	if i := strings.LastIndex(image, "@"); i > 0 {
		return image[:i], image[i+1:]
	}
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[:i], image[i+1:]
	}
	return image, "latest"
}

// pull fetches a worker image the daemon does not have. The response is a stream of progress
// objects that ends when the pull does, so reading it to the end is how we wait; a failed pull
// still answers 200 and says so in the stream, which is why the body is checked rather than
// only the status.
//
// No registry credential is sent. A private registry is a `docker login` on the host, whose
// credentials the daemon already holds — passing one through here would mean holding it.
func (d *dockerDispatcher) pull(ctx context.Context, image string) error {
	ref, tag := splitImageRef(image)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	q := url.Values{"fromImage": {ref}, "tag": {tag}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker/"+dockerAPIVersion+"/images/create?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	// Its own client, because the worker image is gigabytes and the one-minute timeout the rest
	// of these calls want would cut the pull off partway and report it as a failure to pull.
	slow := &http.Client{Timeout: 20 * time.Minute, Transport: d.client.Transport}
	resp, err := slow.Do(req)
	if err != nil {
		return err
	}
	raw, err := readAllLimit(resp)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return apiError("pull", resp, raw)
	}
	if i := strings.LastIndex(string(raw), `"error"`); i >= 0 {
		return errors.New(strings.TrimSpace(string(raw)[i:]))
	}
	return nil
}

// reap removes worker containers that exited more than an hour ago — long enough that the
// reconciler, which runs every thirty seconds, has certainly read the outcome.
func (d *dockerDispatcher) reap(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	filters := url.QueryEscape(fmt.Sprintf(`{"label":["%s"],"status":["exited","dead","created"]}`, dockerJobLabel))
	var list []struct {
		ID      string `json:"Id"`
		Created int64  `json:"Created"`
		State   string `json:"State"`
	}
	if err := d.do(ctx, http.MethodGet, "/containers/json?all=1&filters="+filters, nil, &list); err != nil {
		return
	}
	cutoff := time.Now().Add(-time.Hour).Unix()
	for _, c := range list {
		if c.Created < cutoff {
			_ = d.do(ctx, http.MethodDelete, "/containers/"+c.ID+"?force=1", nil, nil)
		}
	}
}

func (d *dockerDispatcher) Cancel(ctx context.Context, ref string) error {
	// SIGTERM, then SIGKILL ten seconds later, which is what the worker's own shutdown path
	// expects: it has that long to post its last progress event before it goes.
	return d.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(ref)+"/stop?t=10", nil, nil)
}

func (d *dockerDispatcher) Status(ctx context.Context, ref string) (DispatchStatus, error) {
	var out struct {
		State struct {
			Status     string `json:"Status"`
			Running    bool   `json:"Running"`
			ExitCode   int    `json:"ExitCode"`
			OOMKilled  bool   `json:"OOMKilled"`
			Error      string `json:"Error"`
			FinishedAt string `json:"FinishedAt"`
		} `json:"State"`
	}
	if err := d.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(ref)+"/json", nil, &out); err != nil {
		if strings.Contains(err.Error(), "404") {
			return DispatchStatus{State: "unknown", Message: "the container is gone"}, nil
		}
		return DispatchStatus{State: "unknown", Message: err.Error()}, err
	}
	s := out.State
	switch {
	case s.Running:
		return DispatchStatus{State: "running"}, nil
	case s.Status == "created":
		return DispatchStatus{State: "pending"}, nil
	case s.OOMKilled:
		// Worth its own message: an out-of-memory kill looks like an ordinary non-zero exit,
		// and the fix for it is WORKER_MEMORY rather than anything in the repository.
		return DispatchStatus{State: "failed", Message: "the worker was killed for running out of memory; raise WORKER_MEMORY"}, nil
	case s.ExitCode == 0 && s.Status == "exited":
		return DispatchStatus{State: "succeeded"}, nil
	case s.Status == "exited" || s.Status == "dead":
		return DispatchStatus{State: "failed", Message: nonEmpty(strings.TrimSpace(s.Error), fmt.Sprintf("exit status %d", s.ExitCode))}, nil
	}
	return DispatchStatus{State: "pending"}, nil
}
