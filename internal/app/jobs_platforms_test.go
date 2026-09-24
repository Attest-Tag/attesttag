package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A task ARN is the whole execution reference, so the reconciler can ask after a task started
// before a restart — or before the cluster setting changed — without consulting the config.
func TestECSRefParts(t *testing.T) {
	region, cluster, id, ok := ecsRefParts("arn:aws:ecs:eu-west-2:123456789012:task/attesttag/9e1c0a2b4d5f")
	if !ok || region != "eu-west-2" || cluster != "attesttag" || id != "9e1c0a2b4d5f" {
		t.Fatalf("got %q %q %q ok=%v", region, cluster, id, ok)
	}
	for _, bad := range []string{"", "pid:10800", "arn:aws:ecs:eu-west-2:123:task/only-two",
		"arn:aws:lambda:eu-west-2:123:task/c/i", "projects/p/locations/r/jobs/j/executions/e"} {
		if _, _, _, ok := ecsRefParts(bad); ok {
			t.Errorf("ecsRefParts(%q) accepted a reference it should not have", bad)
		}
	}
}

// Every console link is chosen by the shape of the reference, so one job's link does not depend
// on what WORKER_MODE happens to say today.
func TestExecutionConsoleURLPerPlatform(t *testing.T) {
	for _, c := range []struct{ ref, want string }{
		{"arn:aws:ecs:eu-west-2:123456789012:task/attesttag/9e1c",
			"https://eu-west-2.console.aws.amazon.com/ecs/v2/clusters/attesttag/tasks/9e1c?region=eu-west-2"},
		{"/subscriptions/0000-1111/resourceGroups/rg/providers/Microsoft.App/jobs/attesttag-worker/executions/attesttag-worker-abc123",
			"https://portal.azure.com/#@/resource/subscriptions/0000-1111/resourceGroups/rg/providers/Microsoft.App/jobs/attesttag-worker/executionHistory"},
		// k8s and docker have no console page, and the Jobs page already copes with that.
		{"attesttag/attesttag-job-42-9f3c", ""},
		{"7b3c9e1f0a2d4b6c8e0f1a2b3c4d5e6f", ""},
	} {
		if got := executionConsoleURL(c.ref); got != c.want {
			t.Errorf("executionConsoleURL(%q)\n got %q\nwant %q", c.ref, got, c.want)
		}
	}
}

// A routed target comes from configuration, so each dispatcher admits only the shape its own
// platform names and nothing that could become a path, a flag or a second argument.
func TestRoutedTargetShapes(t *testing.T) {
	for _, ok := range []string{"attesttag-worker-jvm", "attesttag-worker:12", "attesttag_worker"} {
		if !safeECSName(ok) {
			t.Errorf("safeECSName(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "arn:aws:ecs:r:a:task-definition/x", "a b", "x/y"} {
		if safeECSName(bad) {
			t.Errorf("safeECSName(%q) = true, want false", bad)
		}
	}
	for _, ok := range []string{"attesttag-worker-jvm:abc123",
		"europe-west2-docker.pkg.dev/p/attesttag/worker-jvm:0cb92aa",
		"ghcr.io/attest-tag/attesttag-worker@sha256:" + "0123456789abcdef"} {
		if !safeImageRef(ok) {
			t.Errorf("safeImageRef(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "/etc/passwd", "worker --privileged", "a//b", `w"x`, "worker\nrun"} {
		if safeImageRef(bad) {
			t.Errorf("safeImageRef(%q) = true, want false", bad)
		}
	}
}

// WORKER_MEMORY is one setting across every mode and Docker is the one that wants bytes.
func TestParseQuantityBytes(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int64
	}{{"4Gi", 4 << 30}, {"512Mi", 512 << 20}, {"2G", 2e9}, {"1024", 1024}, {"", 0}, {" 8Gi ", 8 << 30}} {
		got, err := parseQuantityBytes(c.in)
		if err != nil || got != c.want {
			t.Errorf("parseQuantityBytes(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
	}
	if _, err := parseQuantityBytes("plenty"); err == nil {
		t.Error("parseQuantityBytes(\"plenty\") should have refused")
	}
}

func TestSplitList(t *testing.T) {
	want := []string{"subnet-a", "subnet-b", "subnet-c"}
	for _, in := range []string{"subnet-a,subnet-b,subnet-c", "subnet-a subnet-b subnet-c", " subnet-a , subnet-b,subnet-c "} {
		if got := splitList(in); !reflect.DeepEqual(got, want) {
			t.Errorf("splitList(%q) = %v, want %v", in, got, want)
		}
	}
	if got := splitList(""); got != nil {
		t.Errorf("splitList(\"\") = %v, want nil", got)
	}
}

// RunTask answers 200 with an empty task list when placement fails. Losing the reason there
// would leave the job stuck on "dispatched but never claimed" a quarter of an hour later.
func TestECSStartReportsPlacementFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"tasks": []any{},
			"failures": []any{map[string]string{"reason": "RESOURCE:MEMORY", "detail": "no capacity"}}})
	}))
	defer srv.Close()
	d := ecsTestDispatcher(t, srv)
	_, err := d.Start(context.Background(), &Job{ID: 7, TimeoutS: 600}, JobLaunch{JobID: 7, BotURL: "https://bot.example.com", Token: "atj1.7.x"})
	if err == nil {
		t.Fatal("Start should have failed when ECS started nothing")
	}
	if got := err.Error(); !strings.Contains(got, "RESOURCE:MEMORY") || !strings.Contains(got, "no capacity") {
		t.Errorf("error %q should name the placement failure", got)
	}
}

// The four launch variables reach the container as an override, and nothing else does: the task
// definition anyone can read in the console never holds a token or a key.
func TestECSStartSendsOnlyTheLaunchEnv(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(map[string]any{"tasks": []any{
			map[string]string{"taskArn": "arn:aws:ecs:eu-west-2:123456789012:task/attesttag/abc"}}})
	}))
	defer srv.Close()
	d := ecsTestDispatcher(t, srv)
	ref, err := d.Start(context.Background(), &Job{ID: 7, TimeoutS: 600},
		JobLaunch{JobID: 7, BotURL: "https://bot.example.com", Token: "atj1.7.secret"})
	if err != nil {
		t.Fatal(err)
	}
	if ref != "arn:aws:ecs:eu-west-2:123456789012:task/attesttag/abc" {
		t.Errorf("ref = %q", ref)
	}
	over := body["overrides"].(map[string]any)["containerOverrides"].([]any)[0].(map[string]any)
	if over["name"] != "worker" {
		t.Errorf("container override names %v, not the worker container", over["name"])
	}
	got := map[string]string{}
	for _, e := range over["environment"].([]any) {
		kv := e.(map[string]any)
		got[kv["name"].(string)] = kv["value"].(string)
	}
	want := map[string]string{EnvJobID: "7", EnvBotURL: "https://bot.example.com", EnvJobToken: "atj1.7.secret", "WORKER_MODE": "ecs"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("container environment\n got %v\nwant %v", got, want)
	}
}

// A stopped task ECS has already forgotten is "cannot tell", not "failed": the reconciler's
// silence rule is the right judge of a worker that stopped reporting.
func TestECSStatusMapping(t *testing.T) {
	cases := []struct {
		task  map[string]any
		state string
	}{
		{map[string]any{"lastStatus": "STOPPED", "containers": []any{map[string]any{"exitCode": 0}}}, "succeeded"},
		{map[string]any{"lastStatus": "STOPPED", "containers": []any{map[string]any{"exitCode": 2}}}, "failed"},
		{map[string]any{"lastStatus": "STOPPED", "stopCode": "UserInitiated", "containers": []any{map[string]any{"exitCode": 137}}}, "cancelled"},
		{map[string]any{"lastStatus": "STOPPED", "stoppedReason": "CannotPullContainerError", "containers": []any{map[string]any{}}}, "failed"},
		{map[string]any{"lastStatus": "RUNNING"}, "running"},
		{map[string]any{"lastStatus": "PROVISIONING"}, "pending"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"tasks": []any{c.task}})
		}))
		d := ecsTestDispatcher(t, srv)
		st, err := d.Status(context.Background(), "arn:aws:ecs:eu-west-2:123456789012:task/attesttag/abc")
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if st.State != c.state {
			t.Errorf("task %v\n got %q\nwant %q", c.task, st.State, c.state)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"tasks": []any{}})
	}))
	defer srv.Close()
	d := ecsTestDispatcher(t, srv)
	st, err := d.Status(context.Background(), "arn:aws:ecs:eu-west-2:123456789012:task/attesttag/abc")
	if err != nil || st.State != "unknown" {
		t.Errorf("a forgotten task = %q (%v), want unknown", st.State, err)
	}
}

// The execution keeps the image and the resources the deploy script put on the job, and the
// launch variables win over anything of the same name already there.
func TestACAStartOverlaysEnvOnTheJobTemplate(t *testing.T) {
	var started map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]any{"properties": map[string]any{"template": map[string]any{
				"containers": []any{map[string]any{
					"name": "worker", "image": "reg.azurecr.io/attesttag/worker:0cb92aa",
					"resources": map[string]any{"cpu": 2, "memory": "4Gi"},
					"env":       []any{map[string]any{"name": "WORKER_MODE", "value": "stale"}, map[string]any{"name": "LOG_LEVEL", "value": "info"}},
				}}}}})
			return
		}
		json.NewDecoder(r.Body).Decode(&started)
		json.NewEncoder(w).Encode(map[string]any{"name": "attesttag-worker-abc123"})
	}))
	defer srv.Close()
	d := acaTestDispatcher(t, srv)
	ref, err := d.Start(context.Background(), &Job{ID: 7, TimeoutS: 600},
		JobLaunch{JobID: 7, BotURL: "https://bot.example.com", Token: "atj1.7.secret"})
	if err != nil {
		t.Fatal(err)
	}
	if want := d.jobID("") + "/executions/attesttag-worker-abc123"; ref != want {
		t.Errorf("ref = %q, want %q", ref, want)
	}
	c := started["template"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	if c["image"] != "reg.azurecr.io/attesttag/worker:0cb92aa" {
		t.Errorf("the execution lost the job's image: %v", c["image"])
	}
	if c["resources"] == nil {
		t.Error("the execution lost the job's resources")
	}
	env := map[string]string{}
	for _, e := range c["env"].([]any) {
		kv := e.(map[string]any)
		env[kv["name"].(string)] = kv["value"].(string)
	}
	if env["WORKER_MODE"] != "aca" {
		t.Errorf("WORKER_MODE = %q; the launch value must win over the one on the job", env["WORKER_MODE"])
	}
	if env["LOG_LEVEL"] != "info" {
		t.Error("an unrelated variable on the job was dropped")
	}
	if env[EnvJobToken] != "atj1.7.secret" {
		t.Errorf("the job token did not reach the container: %q", env[EnvJobToken])
	}
}

func TestACAStatusMapping(t *testing.T) {
	for _, c := range []struct{ status, state string }{
		{"Succeeded", "succeeded"}, {"Failed", "failed"}, {"Degraded", "failed"},
		{"Stopped", "cancelled"}, {"Running", "running"}, {"Processing", "running"}, {"Unknown", "pending"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"properties": map[string]any{"status": c.status}})
		}))
		d := acaTestDispatcher(t, srv)
		st, err := d.Status(context.Background(), d.jobID("")+"/executions/e1")
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if st.State != c.state {
			t.Errorf("status %q = %q, want %q", c.status, st.State, c.state)
		}
	}
}

// The worker images are published for linux/amd64 only, so the pod a Job runs is pinned to amd64
// nodes: unpinned, it landed wherever there was room, and on an arm64 node the worker died with
// an exec format error that reads like a broken build. WORKER_K8S_ARCH=any takes the pin off, for
// a worker image built for every architecture the cluster runs.
func TestK8sJobPinsTheWorkerArchitecture(t *testing.T) {
	for _, c := range []struct{ env, want string }{
		{"", "amd64"}, {"arm64", "arm64"}, {" Any ", ""},
	} {
		var job map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewDecoder(r.Body).Decode(&job)
			w.Write([]byte("{}"))
		}))
		d := &k8sDispatcher{base: srv.URL, namespace: "attest-tag", image: "ghcr.io/attest-tag/attesttag-worker:1.0",
			arch: workerK8sArch(c.env), cpu: "2", mem: "4Gi", client: srv.Client(),
			token: "test-token", tokenAge: time.Now()}
		_, err := d.Start(context.Background(), &Job{ID: 7, TimeoutS: 600},
			JobLaunch{JobID: 7, BotURL: "https://bot.example.com", Token: "atj1.7.secret"})
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		pod := job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		sel, _ := pod["nodeSelector"].(map[string]any)
		if got, _ := sel["kubernetes.io/arch"].(string); got != c.want {
			t.Errorf("WORKER_K8S_ARCH=%q pins the pod to %q, want %q", c.env, got, c.want)
		}
		if c.want == "" && pod["nodeSelector"] != nil {
			t.Errorf("WORKER_K8S_ARCH=%q still wrote a nodeSelector: %v", c.env, pod["nodeSelector"])
		}
	}
}

// ---- test dispatchers pointed at an httptest server ----

// ecsTestDispatcher signs against static credentials and talks to srv rather than AWS. The
// signature is still computed, so a change that breaks signing fails here too.
func ecsTestDispatcher(t *testing.T, srv *httptest.Server) *ecsDispatcher {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	d, err := newECSDispatcher(Config{WorkerRegion: "eu-west-2", WorkerECSCluster: "attesttag",
		WorkerECSSubnets: []string{"subnet-a"}, WorkerJobName: "attesttag-worker"})
	if err != nil {
		t.Fatal(err)
	}
	d.client = srv.Client()
	d.client.Transport = rewriteHost{srv.URL, srv.Client().Transport}
	return d
}

func acaTestDispatcher(t *testing.T, srv *httptest.Server) *acaDispatcher {
	t.Helper()
	d, err := newACADispatcher(Config{WorkerAzureSubscription: "0000-1111",
		WorkerAzureResourceGroup: "rg", WorkerJobName: "attesttag-worker"})
	if err != nil {
		t.Fatal(err)
	}
	d.client = srv.Client()
	d.client.Transport = rewriteHost{srv.URL, srv.Client().Transport}
	d.tok.tok, d.tok.expires = "test-token", time.Now().Add(time.Hour)
	return d
}

// rewriteHost sends a request built for a real control plane to the test server instead, leaving
// the path, the body and every signed header alone.
type rewriteHost struct {
	base string
	next http.RoundTripper
}

func (h rewriteHost) RoundTrip(r *http.Request) (*http.Response, error) {
	c := r.Clone(r.Context())
	u := *r.URL
	base, _ := http.NewRequest(http.MethodGet, h.base, nil)
	u.Scheme, u.Host = base.URL.Scheme, base.URL.Host
	c.URL, c.Host = &u, ""
	next := h.next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(c)
}

// The Engine API wants the image and the tag apart, and the last colon is not always the tag:
// in a registry host with a port it is the port, and splitting there asks the daemon to pull a
// registry rather than an image.
func TestSplitImageRef(t *testing.T) {
	for _, c := range []struct{ in, ref, tag string }{
		{"ghcr.io/attest-tag/attesttag-worker:0cb92aa", "ghcr.io/attest-tag/attesttag-worker", "0cb92aa"},
		{"attesttag-worker", "attesttag-worker", "latest"},
		{"registry.example.com:5000/attesttag/worker", "registry.example.com:5000/attesttag/worker", "latest"},
		{"registry.example.com:5000/attesttag/worker:v2", "registry.example.com:5000/attesttag/worker", "v2"},
		{"ghcr.io/a/w@sha256:abc123", "ghcr.io/a/w", "sha256:abc123"},
	} {
		ref, tag := splitImageRef(c.in)
		if ref != c.ref || tag != c.tag {
			t.Errorf("splitImageRef(%q) = %q, %q; want %q, %q", c.in, ref, tag, c.ref, c.tag)
		}
	}
}
