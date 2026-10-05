package worker

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"attesttag/internal/app"
)

// The fix-job eval: each task under evals/fixjobs is run as a real job — the worker, the model
// proxy, a real engine on a real model — against a local copy of its repository, and graded by
// tests the engine never sees. It is how an engine, a model or a provider choice is compared on
// the same work: whether the change is right, and what it took to get there.
//
//	FIXEVAL_API_KEY=sk-or-... go test ./internal/worker -run TestFixEval -fixeval -v -timeout 2h
//
// FIXEVAL_ENGINES (qwen_code,pi), FIXEVAL_MODELS (z-ai/glm-5.3), FIXEVAL_TASKS (all), FIXEVAL_PROVIDERS,
// FIXEVAL_MAX_ROUNDS (80), FIXEVAL_BUDGET_USD (1), FIXEVAL_BASE_URL (OpenRouter) and FIXEVAL_OUT (a
// JSON file for the rows) shape the run; WORKER_QWEN_BIN and WORKER_PI_BIN find the engines.
// It spends real money and is never part of an ordinary test run.
var fixEval = flag.Bool("fixeval", false, "run the fix-job eval against a real model (costs money)")

type fixEvalTask struct {
	Title       string   `json:"title"`
	Requirement string   `json:"requirement"`
	Acceptance  []string `json:"acceptance"`
}

type fixEvalRow struct {
	Task, Engine, Model, Status string
	Correct                     bool
	Turns, In, Cached, Out      int
	CostUSD, Seconds            float64
	Note                        string
}

func TestFixEval(t *testing.T) {
	if !*fixEval {
		t.Skip("the fix-job eval runs only with -fixeval")
	}
	key := cmpEnv("FIXEVAL_API_KEY", os.Getenv("OPENROUTER_API_KEY"))
	if key == "" {
		t.Fatal("FIXEVAL_API_KEY (or OPENROUTER_API_KEY) is not set")
	}
	root := filepath.Join("..", "..", "evals", "fixjobs")
	tasks := splitList(os.Getenv("FIXEVAL_TASKS"))
	if len(tasks) == 0 {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.IsDir() {
				tasks = append(tasks, e.Name())
			}
		}
	}
	engines := splitList(cmpEnv("FIXEVAL_ENGINES", "qwen_code,pi"))
	models := splitList(cmpEnv("FIXEVAL_MODELS", "z-ai/glm-5.3"))
	var rows []fixEvalRow
	for _, task := range tasks {
		for _, engine := range engines {
			for _, model := range models {
				row := runFixEval(t, filepath.Join(root, task), task, engine, model, key)
				t.Logf("%s · %s · %s: %s, correct=%v, %d turns, %d in (%d cached) / %d out, $%.4f, %.0fs %s",
					row.Task, row.Engine, row.Model, row.Status, row.Correct, row.Turns, row.In, row.Cached, row.Out, row.CostUSD, row.Seconds, row.Note)
				rows = append(rows, row)
			}
		}
	}
	t.Log("\n" + fixEvalTable(rows))
	if out := os.Getenv("FIXEVAL_OUT"); out != "" {
		raw, _ := json.MarshalIndent(rows, "", "  ")
		if err := os.WriteFile(out, raw, 0o644); err != nil {
			t.Error(err)
		}
	}
}

func runFixEval(t *testing.T, dir, name, engine, model, key string) fixEvalRow {
	row := fixEvalRow{Task: name, Engine: engine, Model: model}
	raw, err := os.ReadFile(filepath.Join(dir, "task.json"))
	if err != nil {
		t.Fatal(err)
	}
	var task fixEvalTask
	if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatal(err)
	}
	origin := seedFrom(t, filepath.Join(dir, "repo"))
	rounds, _ := strconv.Atoi(cmpEnv("FIXEVAL_MAX_ROUNDS", "80"))
	budget, _ := strconv.ParseFloat(cmpEnv("FIXEVAL_BUDGET_USD", "1"), 64)
	branch := "fix-1-eval-attest_tag"
	claim := app.JobClaim{
		Job: app.JobClaimJob{ID: 1, Spec: app.JobSpec{V: 1, Repo: "local/eval", ConnectionID: 1, BaseBranch: "main", Branch: branch,
			Title: task.Title, Requirement: task.Requirement, Acceptance: task.Acceptance, Requester: "eval", Channel: "eval", ThreadTS: "1.1",
			Constraints: app.JobConstraints{Engine: engine, Model: model, BudgetUSD: budget, TimeoutS: 1200, DraftPR: true, BranchSuffix: "attest_tag",
				MaxRounds: rounds, Providers: splitList(os.Getenv("FIXEVAL_PROVIDERS"))}}},
		Secrets: app.JobSecrets{GitHubToken: testPAT, LLM: app.JobLLMSecret{BaseURL: cmpEnv("FIXEVAL_BASE_URL", "https://openrouter.ai/api/v1"), APIKey: key, Model: model}},
		Limits:  app.JobLimits{BudgetUSD: budget, Deadline: time.Now().Add(20 * time.Minute).UTC().Format(time.RFC3339), HeartbeatSeconds: 60, DiffMaxBytes: app.JobDiffMaxBytes},
	}
	fb := &fakeBot{claim: claim}
	bot := httptest.NewServer(fb)
	defer bot.Close()
	ghs := httptest.NewServer(&fakeGitHub{})
	defer ghs.Close()
	opts := Options{JobID: 1, BotURL: bot.URL, Token: testToken, Mode: "local", WorkDir: t.TempDir(), MaxWall: 20 * time.Minute, Version: "eval",
		QwenBin: cmpEnv("WORKER_QWEN_BIN", "qwen"), PiBin: cmpEnv("WORKER_PI_BIN", "pi"), MiseBin: "mise-not-used"}
	r := &Runner{opts: opts, client: newClient(opts), scrub: newScrubber(opts.Token, key), cloneURL: "file://" + origin,
		github: &GitHub{base: ghs.URL, token: "t", client: http.DefaultClient}}
	start := time.Now()
	r.Run(context.Background())
	row.Seconds = time.Since(start).Seconds()
	res := fb.result
	if res == nil {
		row.Status, row.Note = "no result", ""
		return row
	}
	row.Status, row.Turns = res.Status, res.Turns
	row.In, row.Cached, row.Out, row.CostUSD = res.Usage.In, res.Usage.Cached, res.Usage.Out, res.Usage.CostUSD
	row.Note = strings.TrimSpace(res.Note + " " + res.Error.Message)
	if res.Branch == "" {
		return row
	}
	row.Correct = gradeFixEval(t, origin, branch, filepath.Join(dir, "hidden"))
	return row
}

// seedFrom makes a bare origin whose main is the folder's files in one commit.
func seedFrom(t *testing.T, src string) string {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	gitCmd(t, root, "init", "-q", "--bare", "--initial-branch=main", origin)
	work := filepath.Join(root, "work")
	if err := os.CopyFS(work, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, work, "init", "-q", "--initial-branch=main")
	gitCmd(t, work, "add", "-A")
	gitCmd(t, work, "commit", "-q", "-m", "init")
	gitCmd(t, work, "push", "-q", origin, "main")
	return origin
}

// gradeFixEval checks the pushed branch out with the hidden tests added and runs the suite.
func gradeFixEval(t *testing.T, origin, branch, hidden string) bool {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "grade")
	gitCmd(t, filepath.Dir(dir), "clone", "-q", "--branch", branch, "file://"+origin, dir)
	if err := os.CopyFS(dir, os.DirFS(hidden)); err != nil {
		// CopyFS refuses to overwrite; a hidden test the engine happened to write by the same
		// name is replaced file by file.
		filepath.WalkDir(hidden, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(hidden, p)
			b, _ := os.ReadFile(p)
			os.MkdirAll(filepath.Join(dir, filepath.Dir(rel)), 0o755)
			return os.WriteFile(filepath.Join(dir, rel), b, 0o644)
		})
	}
	cmd := exec.Command("make", "-s", "test")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("hidden tests failed on %s:\n%s", branch, lastLines(string(out), 2000))
	}
	return err == nil
}

func fixEvalTable(rows []fixEvalRow) string {
	var b strings.Builder
	b.WriteString("| task | engine | model | status | correct | turns | input | cached | output | cost | time |\n|---|---|---|---|---|---:|---:|---:|---:|---:|---:|\n")
	for _, r := range rows {
		cached := "—"
		if r.In > 0 {
			cached = fmt.Sprintf("%d%%", r.Cached*100/r.In)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %v | %d | %s | %s | %s | $%.4f | %.0fs |\n",
			r.Task, r.Engine, r.Model, r.Status, r.Correct, r.Turns, groupDigits(r.In), cached, groupDigits(r.Out), r.CostUSD, r.Seconds)
	}
	return b.String()
}

func cmpEnv(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
