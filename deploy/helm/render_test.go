package helm

// What the chart renders, checked by rendering it. The two mistakes this is here to catch are
// the expensive ones: a second replica against a ReadWriteOnce volume, which corrupts the
// database rather than scaling it, and a release that quietly runs two of something because two
// ways of configuring storage were both set.
//
// Skipped where helm is absent, which is most machines that are not doing this work.

import (
	"os/exec"
	"strings"
	"testing"
)

const chart = "./attest-tag"

func render(t *testing.T, args ...string) (string, error) {
	t.Helper()
	base := []string{"template", "t", chart,
		"--set", "ingress.host=bot.example.com",
		"--set", "secrets.MASTER_KEY=key",
		"--set", "secrets.SLACK_SIGNING_SECRET=sss",
		"--set", "secrets.OPENROUTER_API_KEY=orr",
		// Required since connecting a workspace stopped being optional: the chart refuses to
		// render without the OAuth pair, the same way the binary refuses to start.
		"--set", "secrets.SLACK_CLIENT_ID=cid",
		"--set", "secrets.SLACK_CLIENT_SECRET=csec",
	}
	out, err := exec.Command("helm", append(base, args...)...).CombinedOutput()
	return string(out), err
}

func TestChartShapes(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("no helm on PATH")
	}
	for _, tc := range []struct {
		name string
		set  []string
		want []string // substrings the rendering must contain
		not  []string // and must not
	}{
		{
			name: "nothing configured is one pod with two volumes",
			want: []string{"kind: StatefulSet", "name: DB_PATH", "name: DOCS_DIR", "name: data", "name: docs"},
			not:  []string{"name: DATABASE_URL", "name: DOCS_S3_URL", "kind: Deployment", "name: REQUIRE_POSTGRES"},
		},
		{
			name: "a MinIO of its own, with the database still local and now replicated into it",
			set:  []string{"--set", "minio.enabled=true"},
			want: []string{"kind: StatefulSet", "component: minio", "name: DOCS_S3_URL", "name: bucket-init", "name: DB_PATH"},
			not:  []string{"name: DOCS_DIR", "kind: Deployment", "name: REQUIRE_POSTGRES"},
		},
		{
			name: "a Postgres of its own, with the documents still on a volume",
			set:  []string{"--set", "postgres.enabled=true"},
			want: []string{"component: postgres", "name: DATABASE_URL", "$(POSTGRES_PASSWORD)", "name: DOCS_DIR", "name: REQUIRE_POSTGRES"},
			not:  []string{"name: DB_PATH", "kind: Deployment"},
		},
		{
			name: "both of its own: no local state, so a Deployment that may be scaled",
			set:  []string{"--set", "postgres.enabled=true", "--set", "minio.enabled=true", "--set", "replicaCount=3"},
			want: []string{"kind: Deployment", "replicas: 3", "component: postgres", "component: minio", "name: bucket-init", "name: REQUIRE_POSTGRES"},
			not:  []string{"name: DB_PATH", "name: DOCS_DIR", "volumeClaimTemplates:\n    - metadata:\n        name: data\n      spec:\n        accessModes: [ReadWriteOnce]\n        resources:\n          requests:\n            storage: 8Gi"},
		},
		{
			name: "storage brought from outside renders no storage of its own",
			set: []string{
				"--set", "database.url=postgres://u:p@db:5432/attesttag",
				"--set", "documents.s3.url=s3://bucket/docs?region=auto",
				"--set", "documents.s3.keyId=k", "--set", "documents.s3.secret=s",
				"--set", "replicaCount=2",
			},
			want: []string{"kind: Deployment", "replicas: 2", "name: REQUIRE_POSTGRES"},
			not:  []string{"component: postgres", "component: minio", "name: bucket-init", "name: DB_PATH"},
		},
		{
			// The shape comes from values even when the storage does not: database.url is a
			// placeholder and the real DSN is in a Secret the chart never reads. If that Secret
			// lacks it, every pod must refuse to start rather than open an empty SQLite file of
			// its own on a disk that dies with it.
			name: "an existingSecret holding the DSN still says Postgres to the binary",
			set: []string{
				"--set", "existingSecret=attest-tag-secrets",
				"--set", "database.url=in-secret", "--set", "documents.s3.url=in-secret",
				"--set", "replicaCount=2",
			},
			want: []string{"kind: Deployment", "replicas: 2", "name: REQUIRE_POSTGRES", "name: attest-tag-secrets"},
			not:  []string{"kind: Secret", "name: DB_PATH", "name: DOCS_DIR"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := render(t, tc.set...)
			if err != nil {
				t.Fatalf("helm template: %v\n%s", err, out)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("rendering does not contain %q", want)
				}
			}
			for _, not := range tc.not {
				if strings.Contains(out, not) {
					t.Errorf("rendering contains %q and should not", not)
				}
			}
		})
	}
}

// The failures worth having: each refuses to render, and says which two values disagree.
func TestChartRefusesWhatWouldCorruptSomething(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("no helm on PATH")
	}
	for _, tc := range []struct {
		name, want string
		set        []string
	}{
		{
			name: "a second replica against a local volume",
			set:  []string{"--set", "replicaCount=2"},
			want: "replicaCount is 2",
		},
		{
			name: "a second replica with only the documents moved off",
			set:  []string{"--set", "minio.enabled=true", "--set", "replicaCount=2"},
			want: "SQLite database",
		},
		{
			name: "two Postgres answers at once",
			set:  []string{"--set", "postgres.enabled=true", "--set", "database.url=postgres://u:p@db:5432/x"},
			want: "postgres.enabled and database.url are both set",
		},
		{
			name: "two bucket answers at once",
			set: []string{"--set", "minio.enabled=true",
				"--set", "documents.s3.url=s3://bucket/docs?region=auto"},
			want: "minio.enabled and documents.s3.url are both set",
		},
		{
			name: "no ingress host, which Slack cannot deliver to",
			set:  []string{"--set", "ingress.host="},
			want: "ingress.host is required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := render(t, tc.set...)
			if err == nil {
				t.Fatalf("this rendered, and should have been refused:\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("the refusal does not say %q:\n%s", tc.want, out)
			}
		})
	}
}

// MASTER_KEY is required like the other four: the image sets REQUIRE_MASTER_KEY and exits without
// it, and a release that renders and then crash-loops teaches what `helm install` could have said.
func TestChartRequiresMasterKey(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("no helm on PATH")
	}
	out, err := render(t, "--set", "secrets.MASTER_KEY=")
	if err == nil {
		t.Fatalf("this rendered with no MASTER_KEY, and the pod would never start:\n%s", out)
	}
	for _, want := range []string{"secrets.MASTER_KEY is required", "openssl rand -base64 32"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, out)
		}
	}
	// With an existingSecret the key is in a Secret the chart does not read, so it cannot check.
	if out, err := render(t, "--set", "secrets.MASTER_KEY=", "--set", "existingSecret=attest-tag-secrets"); err != nil {
		t.Errorf("an existingSecret was refused for a value the chart cannot see: %v\n%s", err, out)
	}
}

// The fix-job worker is off unless it is asked for, and asking for it adds exactly one Role,
// bound to this release's own service account, in this release's own namespace.
func TestWorkerRBAC(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("no helm on PATH")
	}
	off, err := render(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"WORKER_MODE", "kind: Role", "kind: ClusterRole"} {
		if strings.Contains(off, s) {
			t.Errorf("worker.enabled is false but the rendering still has %q", s)
		}
	}

	on, err := render(t, "--set", "worker.enabled=true")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"name: WORKER_MODE",
		// `workers`, not `k8s`: the bot reads KUBERNETES_SERVICE_HOST rather than being told
		// which platform a chart could only ever be running on.
		"value: workers",
		`value: "ghcr.io/attest-tag/attesttag-worker:`,
		"kind: Role",
		"kind: RoleBinding",
		`resources: ["jobs"]`,
		// The worker's own account, which is granted nothing and carries no token.
		"name: t-attest-tag-worker",
		"automountServiceAccountToken: false",
		// The published worker images are amd64 only, so the bot pins the pod to amd64 nodes.
		"name: WORKER_K8S_ARCH\n              value: \"amd64\"",
	} {
		if !strings.Contains(on, s) {
			t.Errorf("the rendering with the worker on is missing %q", s)
		}
	}
	anyArch, err := render(t, "--set", "worker.enabled=true", "--set", "worker.arch=")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(anyArch, "name: WORKER_K8S_ARCH\n              value: \"any\"") {
		t.Errorf("an empty worker.arch does not take the pin off:\n%s", grepLines(anyArch, "WORKER_K8S_ARCH"))
	}
	// A bot that can create Jobs anywhere in the cluster is a different risk from one that can
	// create them beside itself, and this chart only ever grants the second.
	if strings.Contains(on, "kind: ClusterRole") {
		t.Error("the worker RBAC is cluster-scoped, and must not be")
	}
}

// Naming a namespace the release is not in would render a Role that lands in the wrong place, or
// nowhere. It is refused rather than applied, and the refusal offers only ways out that work: it
// used to suggest creating the Role there by hand, which this same check then refused.
func TestWorkerNamespaceMustBeTheReleaseNamespace(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("no helm on PATH")
	}
	out, err := render(t, "--set", "worker.enabled=true", "--set", "worker.namespace=elsewhere")
	if err == nil {
		t.Fatalf("this rendered, and should have been refused:\n%s", out)
	}
	for _, want := range []string{"worker.namespace is", "Leave worker.namespace empty", `install the release into "elsewhere"`} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "yourself") {
		t.Errorf("the refusal still offers a way out that it refuses:\n%s", out)
	}
	// The release's own namespace named explicitly is the same as leaving it empty.
	if out, err := render(t, "--set", "worker.enabled=true", "--set", "worker.namespace=default"); err != nil {
		t.Errorf("naming the release's own namespace was refused: %v\n%s", err, out)
	}
}

// The image runs as uid 10001 and needs no capabilities, and the pod needs no Kubernetes API at
// all unless it is dispatching worker Jobs. None of that is enforced by saying it in the
// Dockerfile: a securityContext that leaves runAsNonRoot out is a pod whose only protection is
// the image, so a rebuilt or substituted one takes it away silently. These are the lines that
// make the cluster refuse instead.
func TestChartAsksTheClusterToEnforceWhatTheImageAssumes(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("no helm on PATH")
	}
	out, err := render(t)
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"runAsNonRoot: true",
		"runAsUser: 10001",
		"allowPrivilegeEscalation: false",
		"capabilities:\n              drop:\n              - ALL",
		"type: RuntimeDefault",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the rendered pod does not carry %q", want)
		}
	}
	// No worker, so no Job to create, so no token to carry.
	if strings.Count(out, "automountServiceAccountToken: false") != 2 {
		t.Errorf("want the API token withheld on both the ServiceAccount and the pod spec, got:\n%s",
			grepLines(out, "automountServiceAccountToken"))
	}

	// With the worker on it is a different answer, because then the bot really does create Jobs
	// — and this is the half that would otherwise be "turned off and never turned back on".
	withWorker, err := render(t, "--set", "worker.enabled=true")
	if err != nil {
		t.Fatalf("render with worker failed: %v\n%s", err, withWorker)
	}
	if !strings.Contains(withWorker, "automountServiceAccountToken: true") {
		t.Error("with the worker enabled the bot cannot reach the API it was just granted a Role on")
	}
}

// The bundled stores are containers too, and they were the ones with no container context at all.
func TestBundledStoresAreConstrained(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("no helm on PATH")
	}
	for _, tc := range []struct{ name, set string }{
		{"postgres", "postgres.enabled=true"},
		{"minio", "minio.enabled=true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := render(t, "--set", tc.set)
			if err != nil {
				t.Fatalf("render failed: %v\n%s", err, out)
			}
			// Both get at least these two. Postgres deliberately keeps SETUID/SETGID and starts
			// as root — its entrypoint drops privileges itself — so `drop: [ALL]` is not asserted
			// for it here; see the comment on postgres.securityContext in values.yaml.
			block := after(out, "- name: "+tc.name)
			for _, want := range []string{"allowPrivilegeEscalation: false", "type: RuntimeDefault"} {
				if !strings.Contains(block, want) {
					t.Errorf("the %s container does not carry %q", tc.name, want)
				}
			}
		})
	}
}

// grepLines is for a failure message: the lines that matched, so the diff is readable.
func grepLines(out, needle string) string {
	var keep []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, needle) {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "\n")
}

// after is everything from the first occurrence of marker onwards, which is enough to read one
// container's own fields without parsing YAML.
func after(out, marker string) string {
	if i := strings.Index(out, marker); i >= 0 {
		return out[i:]
	}
	return ""
}
