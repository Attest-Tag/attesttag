package worker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"attesttag/internal/app"
)

func TestC4_ReadRepoFileRefusesSymlinkEscape(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("TOP_SECRET_ENV_TOKEN"), 0o600); err != nil {
		t.Fatal(err)
	}
	repoDir := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(repoDir, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, ".attest"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repoDir, ".attest", "recipe.yaml")); err != nil {
		t.Fatal(err)
	}

	if got := conventions(repoDir, 4000); strings.Contains(got, "TOP_SECRET_ENV_TOKEN") {
		t.Fatalf("conventions followed symlink outside repo: %q", got)
	}
	if r, err := readRepoRecipe(repoDir); r != nil || err != nil {
		t.Fatalf("readRepoRecipe should skip/refuse symlink outside repo, got r=%+v err=%v", r, err)
	}
	if got := readText(repoDir, "AGENTS.md"); got != "" {
		t.Fatalf("readText followed symlink outside repo: %q", got)
	}
}

func TestC4_OptionsFromEnvUnsetsJobToken(t *testing.T) {
	t.Setenv(app.EnvJobID, "7")
	t.Setenv(app.EnvBotURL, "http://localhost:8090")
	t.Setenv(app.EnvJobToken, testToken)
	t.Setenv("WORKER_MODE", "local")
	t.Setenv("WORKER_DIR", t.TempDir())
	o, err := optionsFromEnv()
	if err != nil || o.Token != testToken {
		t.Fatalf("optionsFromEnv: %+v %v", o, err)
	}
	if got := os.Getenv(app.EnvJobToken); got != "" {
		t.Fatalf("ATTEST_JOB_TOKEN remained set in environment after optionsFromEnv")
	}
}

func TestC5_H9_GitAuthScopeAndStageRejectsPreStagedOrCommittedExcludedFiles(t *testing.T) {
	env := gitAuthEnv("ghs_secret_token_123456")
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "http.https://github.com/.extraheader") {
		t.Fatalf("gitAuthEnv not scoped to github.com: %v", env)
	}
	for _, pin := range gitPins {
		if strings.HasPrefix(pin, "http.sslCA") || strings.HasPrefix(pin, "http.proxy") {
			t.Fatalf("git pin %q breaks https remotes or the egress proxy", pin)
		}
	}
	s := newScrubber("ghs_secret_token_123456", gitAuthB64("ghs_secret_token_123456"))
	if out := s.Clean("Authorization: Basic " + gitAuthB64("ghs_secret_token_123456")); strings.Contains(out, gitAuthB64("ghs_secret_token_123456")) {
		t.Fatalf("scrubber failed to redact base64 basic auth token: %q", out)
	}

	origin := seedRepo(t, "")
	work := t.TempDir()
	ws := &Workspace{JobDir: work, RepoDir: filepath.Join(work, "repo"), Env: baseEnv(work, work), Scrub: s}
	os.MkdirAll(filepath.Join(work, "home"), 0o700)
	os.MkdirAll(filepath.Join(work, "tmp"), 0o700)
	spec := app.JobSpec{Repo: "acme/app", BaseBranch: "main", Branch: "fix-1-x-attest_tag"}
	repo, err := cloneRepo(context.Background(), ws, spec, "tok", "file://"+origin)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.checkoutBranch(context.Background(), spec.Branch); err != nil {
		t.Fatal(err)
	}
	baseSHA := repo.headSHA(context.Background())
	pushDir := filepath.Join(t.TempDir(), "push.git")
	if err := repo.snapshotPushRepo(context.Background(), pushDir, baseSHA); err != nil {
		t.Fatal(err)
	}

	// Simulate hostile repo code that pre-stages AND pre-commits .github/workflows/pwn.yml.
	os.MkdirAll(filepath.Join(ws.RepoDir, ".github", "workflows"), 0o755)
	os.WriteFile(filepath.Join(ws.RepoDir, ".github", "workflows", "pwn.yml"), []byte("on: push\n"), 0o644)
	os.WriteFile(filepath.Join(ws.RepoDir, "README.md"), []byte("ok\n"), 0o644)
	gitCmd(t, ws.RepoDir, "add", ".github/workflows/pwn.yml", "README.md")
	gitCmd(t, ws.RepoDir, "commit", "-q", "-m", "sneaky")

	files, skipped, err := repo.stage(context.Background())
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	for _, f := range files {
		if strings.HasPrefix(f, ".github/") {
			t.Fatalf("stage failed to strip pre-committed excluded file: %v", files)
		}
	}
	if len(skipped) == 0 {
		t.Fatalf("expected .github/workflows/pwn.yml to be reported as skipped")
	}
}

func TestM10_CacheTarSkipsSymlinks(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "host-secret")
	os.WriteFile(outside, []byte("HOST_SECRET_DATA"), 0o600)
	cacheDir := t.TempDir()
	os.MkdirAll(filepath.Join(cacheDir, "npm"), 0o755)
	os.Symlink(outside, filepath.Join(cacheDir, "npm", "leak"))
	os.WriteFile(filepath.Join(cacheDir, "npm", "ok.txt"), []byte("hello"), 0o644)

	var buf bytes.Buffer
	if _, err := tarFrom(&buf, cacheDir, 1<<20); err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	if _, err := untarInto(&buf, outDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "npm", "leak")); !os.IsNotExist(err) {
		t.Fatal("symlink file leaked into cache archive")
	}
	if raw, err := os.ReadFile(filepath.Join(outDir, "npm", "ok.txt")); err != nil || string(raw) != "hello" {
		t.Fatalf("normal cache file missing: %q %v", raw, err)
	}
}
