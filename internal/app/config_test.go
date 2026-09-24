package app

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigUsesSigningSecret(t *testing.T) {
	t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "absent.env"))
	t.Setenv("SLACK_SIGNING_SECRET", "test-signing-secret")
	t.Setenv("SLACK_BOT_TOKEN", "")
	t.Setenv("LLM_API_KEY", "test-model-key")
	// Required since the client pair stopped being a warning: LoadConfig exits without them.
	t.Setenv("SLACK_CLIENT_ID", "test-client-id")
	t.Setenv("SLACK_CLIENT_SECRET", "test-client-secret")
	c := LoadConfig()
	if c.SlackSigningSecret != "test-signing-secret" {
		t.Fatal("HTTP signing secret was not loaded")
	}
}

func TestFingerprint(t *testing.T) {
	cases := map[string]string{
		"":                        "unset",
		"short":                   "(5 chars)",
		"sk-or-v1-abcdefghijklmn": "…klmn",
	}
	for in, want := range cases {
		if got := fingerprint(in); got != want {
			t.Errorf("fingerprint(%q)=%q want %q", in, got, want)
		}
	}
}

func TestEnvShadows(t *testing.T) {
	dotenv := map[string]string{
		"OPENROUTER_API_KEY": "sk-or-v1-from-dotenv-1111",
		"SLACK_BOT_TOKEN":    "xoxb-same-in-both-2222",
		"MASTER_KEY":         "only-in-dotenv-3333",
		"LOG_LEVEL":          "info",
	}
	shell := map[string]string{
		"OPENROUTER_API_KEY": "sk-or-v1-stale-zshrc-9999", // differs: shadowed
		"SLACK_BOT_TOKEN":    "xoxb-same-in-both-2222",    // identical: fine
		"LOG_LEVEL":          "debug",                     // differs, not a secret
		"HEALTH_ADDR":        ":8090",                     // not in .env: irrelevant
	}
	lookup := func(k string) (string, bool) { v, ok := shell[k]; return v, ok }
	got := envShadows(dotenv, lookup)
	if len(got) != 2 || got[0].Key != "LOG_LEVEL" || got[1].Key != "OPENROUTER_API_KEY" {
		t.Fatalf("envShadows = %+v, want LOG_LEVEL and OPENROUTER_API_KEY", got)
	}
	if got[1].Env != "…9999" || got[1].DotEnv != "…1111" {
		t.Errorf("fingerprints = %+v", got[1])
	}
	for _, s := range got {
		for _, full := range []string{"stale-zshrc", "from-dotenv"} {
			if s.Env == full || s.DotEnv == full {
				t.Errorf("full secret leaked into %+v", s)
			}
		}
	}
	if !isSecretName("OPENROUTER_API_KEY") || !isSecretName("slack_bot_token") || isSecretName("LOG_LEVEL") {
		t.Error("isSecretName misclassifies")
	}
	if len(envShadows(map[string]string{}, lookup)) != 0 {
		t.Error("no .env means no shadows")
	}
}

func TestKeyProvenance(t *testing.T) {
	dotenv := map[string]string{"OPENROUTER_API_KEY": "dotenv-value-1234"}
	mk := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	cases := []struct {
		shell map[string]string
		want  string
	}{
		{map[string]string{"OPENROUTER_API_KEY": "zshrc-value-9999"}, "the shell environment (differs from .env.testing)"},
		{map[string]string{"OPENROUTER_API_KEY": "dotenv-value-1234"}, "the shell environment (same as .env.testing)"},
		{map[string]string{}, ".env.testing"},
	}
	for _, c := range cases {
		if got := keyProvenance("OPENROUTER_API_KEY", ".env.testing", dotenv, mk(c.shell)); got != c.want {
			t.Errorf("shell=%v: got %q want %q", c.shell, got, c.want)
		}
	}
	if got := keyProvenance("OPENROUTER_API_KEY", ".env", map[string]string{}, mk(map[string]string{"OPENROUTER_API_KEY": "x"})); got != "the environment" {
		t.Errorf("no dotenv file, shell set: got %q", got)
	}
	if got := keyProvenance("OPENROUTER_API_KEY", ".env", map[string]string{}, mk(map[string]string{})); got != "nowhere" {
		t.Errorf("unset everywhere: got %q", got)
	}
}

// WORKER_MODE answers two questions and the operator usually only has to answer one: `workers`
// runs each job in a container of its own and works out where from the environment. Naming a
// platform still works and pins it, which is what every existing .env file does.
func TestWorkerModes(t *testing.T) {
	base := func(t *testing.T) {
		t.Helper()
		t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "absent.env"))
		t.Setenv("SLACK_SIGNING_SECRET", "s")
		t.Setenv("LLM_API_KEY", "k")
		t.Setenv("SLACK_CLIENT_ID", "cid")
		t.Setenv("SLACK_CLIENT_SECRET", "csec")
		t.Setenv("WORKER_IMAGE", "ghcr.io/attest-tag/attesttag-worker:latest")
		// Detection reads these, and a developer machine may have any of them set.
		for _, k := range []string{"KUBERNETES_SERVICE_HOST", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
			"AWS_CONTAINER_CREDENTIALS_FULL_URI", "CONTAINER_APP_NAME", "K_SERVICE"} {
			t.Setenv(k, "")
		}
		// …including a Docker socket, which is the one signal that is not an environment variable.
		t.Setenv("WORKER_DOCKER_HOST", "unix://"+filepath.Join(t.TempDir(), "absent.sock"))
	}

	// Naming a platform pins it, and is still what .env.prod says.
	for _, p := range []string{"cloudrun", "ecs", "aca", "k8s", "docker"} {
		t.Run("pinned "+p, func(t *testing.T) {
			base(t)
			t.Setenv("WORKER_MODE", p)
			c := LoadConfig()
			if c.WorkerMode != p || c.WorkerPlatform != p {
				t.Errorf("WORKER_MODE=%s gave mode %q platform %q", p, c.WorkerMode, c.WorkerPlatform)
			}
		})
	}

	t.Run("off and local", func(t *testing.T) {
		for mode, want := range map[string]string{"off": "", "local": "local"} {
			base(t)
			t.Setenv("WORKER_MODE", mode)
			if got := LoadConfig().WorkerPlatform; got != want {
				t.Errorf("WORKER_MODE=%s gave platform %q, want %q", mode, got, want)
			}
		}
	})

	// One signal per platform, each set by the platform on its own containers.
	t.Run("workers detects the platform", func(t *testing.T) {
		for _, c := range []struct{ env, val, want string }{
			{"KUBERNETES_SERVICE_HOST", "10.0.0.1", "k8s"},
			{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "/v2/credentials/abc", "ecs"},
			{"AWS_CONTAINER_CREDENTIALS_FULL_URI", "http://169.254.170.2/v2/x", "ecs"},
			{"CONTAINER_APP_NAME", "attesttag", "aca"},
			{"K_SERVICE", "attesttag", "cloudrun"},
		} {
			t.Run(c.want+" via "+c.env, func(t *testing.T) {
				base(t)
				t.Setenv("WORKER_MODE", "workers")
				t.Setenv(c.env, c.val)
				cfg := LoadConfig()
				if cfg.WorkerPlatform != c.want {
					t.Errorf("%s=%s gave platform %q, want %q", c.env, c.val, cfg.WorkerPlatform, c.want)
				}
				// The mode stays what the operator wrote, so the console can say both.
				if cfg.WorkerMode != "workers" {
					t.Errorf("mode = %q, want workers", cfg.WorkerMode)
				}
			})
		}
	})

	// A real socket, since a path that is merely present is not one.
	t.Run("workers falls back to the docker socket", func(t *testing.T) {
		base(t)
		// os.MkdirTemp rather than t.TempDir: the latter puts the test's name in the path, and a
		// unix socket path has 104 bytes on macOS — this subtest's name alone overruns it, and
		// the failure is an unhelpful "invalid argument" that reads like a broken test.
		dir, err := os.MkdirTemp("", "s")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(dir)
		sock := filepath.Join(dir, "d.sock")
		l, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatalf("could not make a unix socket at %s (%d bytes): %v", sock, len(sock), err)
		}
		defer l.Close()
		t.Setenv("WORKER_MODE", "workers")
		t.Setenv("WORKER_DOCKER_HOST", "unix://"+sock)
		if got := LoadConfig().WorkerPlatform; got != "docker" {
			t.Errorf("platform = %q, want docker", got)
		}
	})

	// Told to run workers and unable to say where is off, not a guess. A job started on the
	// wrong thing an hour later is much worse than a refusal at boot.
	t.Run("workers with no signal is off", func(t *testing.T) {
		base(t)
		t.Setenv("WORKER_MODE", "workers")
		c := LoadConfig()
		if c.WorkerPlatform != "" || c.WorkerMode != "off" {
			t.Errorf("mode %q platform %q, want off and empty", c.WorkerMode, c.WorkerPlatform)
		}
	})

	t.Run("an unknown mode is off", func(t *testing.T) {
		base(t)
		t.Setenv("WORKER_MODE", "nomad")
		if c := LoadConfig(); c.WorkerPlatform != "" || c.WorkerMode != "off" {
			t.Errorf("mode %q platform %q, want off and empty", c.WorkerMode, c.WorkerPlatform)
		}
	})

	// On k8s and docker the "job name" is the image, because there is no job object created
	// ahead of time to name; elsewhere WORKER_IMAGE means nothing and the job name stands.
	t.Run("WORKER_IMAGE is the target on k8s only", func(t *testing.T) {
		base(t)
		t.Setenv("WORKER_MODE", "k8s")
		t.Setenv("WORKER_IMAGE", "reg.example.com/worker:1")
		if got := LoadConfig().WorkerJobName; got != "reg.example.com/worker:1" {
			t.Errorf("WorkerJobName = %q, want the image", got)
		}
		base(t)
		t.Setenv("WORKER_MODE", "cloudrun")
		t.Setenv("WORKER_IMAGE", "reg.example.com/worker:1")
		if got := LoadConfig().WorkerJobName; got != "attesttag-worker" {
			t.Errorf("WorkerJobName = %q, want attesttag-worker", got)
		}
	})

	// The OIDC check is a Google identity token, so it can only default on where there is one —
	// and that is a property of the platform, not of what the operator typed.
	t.Run("OIDC defaults on only for cloudrun", func(t *testing.T) {
		for _, p := range []string{"cloudrun", "ecs", "aca", "k8s", "docker"} {
			base(t)
			t.Setenv("WORKER_MODE", p)
			if got, want := LoadConfig().WorkerRequireOIDC, p == "cloudrun"; got != want {
				t.Errorf("%s: WorkerRequireOIDC = %v, want %v", p, got, want)
			}
		}
		// Reached through detection rather than pinned, it must still be on.
		base(t)
		t.Setenv("WORKER_MODE", "workers")
		t.Setenv("K_SERVICE", "attesttag")
		if !LoadConfig().WorkerRequireOIDC {
			t.Error("detected cloudrun did not default WorkerRequireOIDC on")
		}
	})
}

// The guard that keeps a cut-over deployment on Postgres is only as good as parseDSN's answer
// about the DSN production actually uses, which is a Cloud SQL Unix socket rather than a host
// and port. Getting this wrong would not be a missed guard — it would be a deployment that
// refuses to start.
func TestRequirePostgresRecognisesTheDSNsProductionUses(t *testing.T) {
	for _, tc := range []struct {
		dsn string
		pg  bool
	}{
		{"postgres://u:p@/attesttag?host=/cloudsql/proj:us-central1:my-instance", true},
		{"postgresql://u:p@/attesttag?host=/cloudsql/proj:us-central1:my-instance", true},
		{"postgres://u:p@10.0.0.1:5432/attesttag?sslmode=disable", true},
		{"", false},
		{"attesttag.db", false},
		{"/data/attesttag.db", false},
		{"sqlite:///data/attesttag.db", false},
	} {
		if _, _, pg := parseDSN(tc.dsn); pg != tc.pg {
			t.Errorf("parseDSN(%q) postgres=%v, want %v", tc.dsn, pg, tc.pg)
		}
	}
}

// The startup line is where "which database is this process on" gets answered, and a DSN that
// answers it carries a password. Neither half is allowed to go wrong.
func TestDescribeDSNNamesTheEngineAndKeepsThePassword(t *testing.T) {
	prod := "postgres://attesttag:hunter2@/attesttag?host=/cloudsql/my-project:us-central1:my-instance"
	got := describeDSN(prod)
	if strings.Contains(got, "hunter2") {
		t.Fatalf("describeDSN leaked the password: %q", got)
	}
	for _, want := range []string{"postgres", "attesttag", "/cloudsql/my-project:us-central1:my-instance"} {
		if !strings.Contains(got, want) {
			t.Errorf("describeDSN(prod) = %q, missing %q", got, want)
		}
	}
	if got := describeDSN("postgres://u:pw@10.0.0.1:5432/attesttag?sslmode=disable"); strings.Contains(got, "pw@") {
		t.Errorf("describeDSN leaked credentials over TCP: %q", got)
	}
	// SQLite is a path and says so, which is what it always said.
	if got := describeDSN("/data/attesttag.db"); got != "/data/attesttag.db" {
		t.Errorf("describeDSN(sqlite) = %q, want the path", got)
	}
}
