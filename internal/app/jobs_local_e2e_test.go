package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The whole thing, locally: the real bot serving the real worker endpoints, the real local
// dispatcher starting the real `attesttag worker` binary as a subprocess, a real git clone of a
// real repository, a real recipe worked out from it, a real build and a real suite — and at the
// end a pull request, a report, and the recipe written back onto the connection so the next job
// starts from it.
//
// Only two things are not real: the model (the fake engine makes one deterministic edit) and
// github.com, which is a stub here because nothing in a test should push a branch to a
// repository somebody owns. The worker refuses both redirections outside local mode.

// stubGitHub answers the one call the worker makes there: opening the pull request.
type stubGitHub struct {
	mu      sync.Mutex
	created []map[string]any
}

func (s *stubGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls") {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.created = append(s.created, body)
		n := len(s.created)
		s.mu.Unlock()
		head, _ := body["head"].(string)
		base, _ := body["base"].(string)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"number":%d,"html_url":"https://github.com/acme/app/pull/%d","draft":true,`+
			`"head":{"ref":%q,"sha":"deadbeef"},"base":{"ref":%q}}`, n, n, head, base)
		return
	}
	fmt.Fprint(w, `[]`)
}

func (s *stubGitHub) prs() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.created...)
}

// buildWorkerBinary compiles the real binary once per run.
func buildWorkerBinary(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	bin := filepath.Join(t.TempDir(), "attesttag")
	out, err := exec.Command("go", "build", "-o", bin, "attesttag/cmd/attesttag").CombinedOutput()
	if err != nil {
		t.Fatalf("building the worker binary: %v\n%s", err, out)
	}
	return bin
}

// seedLocalRepo makes a bare repository the worker can clone by owner/name.
func seedLocalRepo(t *testing.T, base, repo string, files map[string]string) {
	t.Helper()
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	origin := filepath.Join(base, repo+".git")
	os.MkdirAll(filepath.Dir(origin), 0o755)
	git(base, "init", "-q", "--bare", "--initial-branch=main", origin)
	scratch := filepath.Join(base, "scratch-"+strings.ReplaceAll(repo, "/", "-"))
	os.MkdirAll(scratch, 0o755)
	git(base, "init", "-q", "--initial-branch=main", scratch)
	for name, body := range files {
		p := filepath.Join(scratch, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(scratch, "add", "-A")
	git(scratch, "commit", "-q", "-m", "init")
	git(scratch, "push", "-q", origin, "main")
}

func TestLocalDeploymentRunsARealJob(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary and runs a real job")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	bin := buildWorkerBinary(t)

	gh := &stubGitHub{}
	ghs := httptest.NewServer(gh)
	t.Cleanup(ghs.Close)

	base := t.TempDir()
	seedLocalRepo(t, base, "acme/app", map[string]string{
		"go.mod":      "module example.com/demo\n\ngo 1.21\n",
		"add.go":      "package demo\n\nfunc Add(a, b int) int { return a + b }\n",
		"add_test.go": "package demo\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 2) != 4 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
		"README.md":   "# demo\n",
	})

	h := newJobHarness(t, nil)
	ctx := context.Background()

	// The real local dispatcher, starting the real binary, told where to clone from and which
	// API to open the pull request against.
	t.Setenv("WORKER_GIT_BASE", base)
	t.Setenv("WORKER_GITHUB_API", ghs.URL)
	t.Setenv("WORKER_DIR", filepath.Join(t.TempDir(), "work"))
	t.Setenv("ADMIN_BASE_URL", h.api.URL) // where the worker calls back
	h.b.cfg.WorkerMode = "local"
	h.b.jobs.cfg = h.b.cfg
	h.b.jobs.dispatchers["local"] = newLocalDispatcher(bin)

	// The fake engine, so this test costs nothing and is deterministic.
	if err := h.b.store.PutSettings(ctx, orgID, map[string]string{"worker_engine": "fake"}); err != nil {
		t.Fatal(err)
	}
	h.b.settings.Invalidate(orgID)

	spec := testSpec(h.connID)
	spec.FilesHint = []string{"README.md"}
	j, err := h.b.jobs.Dispatch(ctx, DispatchRequest{OrgID: orgID, TeamID: "T1", Spec: spec, ApprovedBy: "U2", Approval: "confirm"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if j.Dispatcher != "local" || !strings.HasPrefix(j.ExecutionRef, "pid:") {
		t.Fatalf("not started by the real dispatcher: %+v", j)
	}

	// Wait for the worker to finish the job the way the reconciler would.
	deadline := time.Now().Add(3 * time.Minute)
	var done *Job
	for time.Now().Before(deadline) {
		got, err := h.b.store.Job(ctx, orgID, j.ID)
		if err == nil && got != nil && jobTerminal(got.Status) {
			done = got
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if done == nil {
		logs, _ := os.ReadFile(filepath.Join(os.TempDir(), "attesttag-jobs", fmt.Sprintf("job-%d.log", j.ID)))
		t.Fatalf("the job never finished\nworker log:\n%s", lastN(string(logs), 4000))
	}
	logs, _ := os.ReadFile(filepath.Join(os.TempDir(), "attesttag-jobs", fmt.Sprintf("job-%d.log", j.ID)))
	if done.Status != JobSucceeded {
		t.Fatalf("status %s: %s\nworker log:\n%s", done.Status, done.Error, lastN(string(logs), 6000))
	}

	var res JobResult
	json.Unmarshal([]byte(done.Result), &res)
	if res.Recipe == nil || res.Recipe.Ecosystem != "go" || res.Recipe.Source != RecipeSourceDetected {
		t.Errorf("recipe: %+v", res.Recipe)
	}
	if res.Tests.Command != "go test ./..." || !res.Tests.Before.OK || !res.Tests.After.OK {
		t.Errorf("tests: %+v", res.Tests)
	}
	if res.Build.Command != "go build ./..." || !res.Build.After.OK {
		t.Errorf("build: %+v", res.Build)
	}
	if len(res.FilesChanged) != 1 || res.FilesChanged[0] != "README.md" {
		t.Errorf("committed %v", res.FilesChanged)
	}
	if prs := gh.prs(); len(prs) != 1 || prs[0]["draft"] != true {
		t.Fatalf("pull requests: %v", prs)
	}
	body, _ := gh.prs()[0]["body"].(string)
	for _, want := range []string{"| test after | `go test ./...` | pass", "| build after | `go build ./...` | pass", "Recipe:"} {
		if !strings.Contains(body, want) {
			t.Errorf("pr body lacks %q\n%s", want, body)
		}
	}

	// The branch really exists on the origin, with the change on it and main untouched.
	origin := filepath.Join(base, "acme/app.git")
	if out, err := exec.Command("git", "-C", origin, "show", j.Branch+":README.md").CombinedOutput(); err != nil ||
		!strings.Contains(string(out), "attest_tag fix job") {
		t.Errorf("branch %s: %v %s", j.Branch, err, out)
	}

	// And the connection now remembers how this repository is built, so the next job on it
	// starts from what this one learned instead of working it out again.
	conn, err := h.b.store.Connection(ctx, orgID, h.connID)
	if err != nil || conn == nil {
		t.Fatal(err)
	}
	if conn.Recipe == nil || conn.Recipe.Ecosystem != "go" || conn.Recipe.Test.String() != "go test ./..." {
		t.Fatalf("the connection did not remember the recipe: %+v", conn.Recipe)
	}
	t.Logf("job #%d: %s · %s · %s", j.ID, done.Status, res.Recipe.Describe(), res.PR.URL)
}

func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
