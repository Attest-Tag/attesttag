package worker

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"attesttag/internal/app"
)

// mise is what makes "any language" true for the languages the image does not bake in, what makes
// a pinned version real rather than polite, and — through its shims — what lets two folders of one
// repository pin different versions of the same thing. All of it is checked here against live
// fetches, which need mise, uv, the network and a few minutes — so it runs only when asked, and in
// the worker image, where it is the thing being tested.
//
//	ATTEST_TOOLCHAIN_LIVE=1 go test ./internal/worker -run TestToolchain
func TestToolchainProvisioning(t *testing.T) {
	if os.Getenv("ATTEST_TOOLCHAIN_LIVE") != "1" {
		t.Skip("set ATTEST_TOOLCHAIN_LIVE=1 (needs mise, uv and the network)")
	}
	if _, err := exec.LookPath("mise"); err != nil {
		t.Skip("mise is not installed here")
	}
	ctx := context.Background()
	work := t.TempDir()
	newWS := func(name string, files map[string]string) *Workspace {
		job := filepath.Join(work, name)
		repo := filepath.Join(job, "repo")
		for _, d := range []string{"home", "tmp"} {
			os.MkdirAll(filepath.Join(job, d), 0o755)
		}
		for n, b := range files {
			os.MkdirAll(filepath.Dir(filepath.Join(repo, n)), 0o755)
			os.WriteFile(filepath.Join(repo, n), []byte(b), 0o644)
		}
		os.MkdirAll(repo, 0o755)
		return &Workspace{JobDir: job, RepoDir: repo, Env: baseEnv(job, work),
			Deadline: time.Now().Add(25 * time.Minute), Reporter: newReporter(newClient(Options{JobID: 1, BotURL: "http://127.0.0.1:1", Token: "t"}), newScrubber(""), func(error) {}), Scrub: newScrubber("")}
	}
	opts := Options{WorkDir: work, MiseBin: "mise"}
	run := func(ws *Workspace, dir string, env []string, argv ...string) string {
		t.Helper()
		out, err := runCmd(ctx, cmdSpec{Dir: filepath.Join(ws.RepoDir, dir), Argv: argv, Env: env, Timeout: 2 * time.Minute})
		if err != nil || out.Code != 0 {
			t.Fatalf("%s in %s: %v %s", strings.Join(argv, " "), dir, err, out.Output)
		}
		return strings.TrimSpace(out.Output)
	}

	// 1. A toolchain the base image does not carry at all, asked for by the recipe.
	ws := newWS("java", nil)
	tc := newToolchains(ws, opts)
	primary := &pkgRun{Dir: ".", Primary: true, Recipe: &app.Recipe{Tools: map[string]string{"java": "21"}}}
	tc.provision(ctx, ws, primary, time.Now().Add(10*time.Minute))
	tc.activate(ws)
	if tool := stepMissing(&app.RecipeStep{Argv: []string{"java"}}, ws.RepoDir, ".", envValue(primary.workspace(ws).Env, "PATH")); tool != "" {
		t.Errorf("java is still missing after provisioning; PATH=%s notes=%v", envValue(ws.Env, "PATH"), primary.Notes)
	}

	// 2. One repository, folders pinning different versions of the same tools, and one pinning
	//    nothing: every folder runs what it pins, from the checks and from a shell that cds there.
	ws2 := newWS("folders", map[string]string{
		"a/.nvmrc": "20.19.0\n", "b/.nvmrc": "22.12.0\n", "c/README.md": "no pin\n",
		"p39/.python-version": "3.9\n", "p312/.python-version": "3.12\n",
		"legacy/.python-version": "3.5.10\n", "legacy/requirements.txt": "pytest\n",
		"n0/.nvmrc": "0.0.1\n",
	})
	tc2 := newToolchains(ws2, opts)
	pkgs := map[string]*pkgRun{}
	for i, dir := range []string{"a", "b", "c", "p39", "p312", "legacy", "n0"} {
		p := &pkgRun{Dir: dir, Primary: i == 0, Recipe: &app.Recipe{}}
		start := time.Now()
		tc2.provision(ctx, ws2, p, time.Now().Add(5*time.Minute))
		tc2.activate(ws2)
		pkgs[dir] = p
		t.Logf("%s: %s tools=%v notes=%v", dir, time.Since(start).Round(time.Second), p.Tools, p.Notes)
		if dir == "legacy" && time.Since(start) > 90*time.Second {
			t.Errorf("an unobtainable Python took %s to give up on", time.Since(start))
		}
	}
	imageNode := run(ws2, "c", withEnv(ws2.Env, "PATH="+tc2.basePath), "node", "--version")
	for dir, want := range map[string]string{"a": "v20.19.0", "b": "v22.12.0", "c": imageNode} {
		if got := run(ws2, dir, pkgs[dir].workspace(ws2).Env, "node", "--version"); got != want {
			t.Errorf("node in %s/ = %s, want %s", dir, got, want)
		}
	}
	if got := run(ws2, ".", ws2.Env, "sh", "-c", "cd a && node --version; cd ../b && node --version"); got != "v20.19.0\nv22.12.0" {
		t.Errorf("a shell that cds into each folder got %q", got)
	}
	for dir, want := range map[string]string{"p39": "Python 3.9", "p312": "Python 3.12"} {
		if got := run(ws2, dir, pkgs[dir].workspace(ws2).Env, "python3", "--version"); !strings.HasPrefix(got, want) {
			t.Errorf("python3 in %s/ = %s, want %s", dir, got, want)
		}
	}

	// 3. A Python older than anything obtainable runs on the oldest that is, says so, and uv —
	//    which refuses a 3.5 request outright — builds its environment on that one too.
	legacy := pkgs["legacy"]
	lenv := legacy.workspace(ws2).Env
	if got := run(ws2, "legacy", lenv, "python3", "--version"); !strings.HasPrefix(got, "Python "+oldestPython) {
		t.Errorf("legacy/ runs %s", got)
	}
	if !strings.HasPrefix(legacy.OldPython, oldestPython) || !strings.Contains(joinNotes(legacy.Notes), "3.5.10") {
		t.Errorf("the substitution is not said: old=%q notes=%v", legacy.OldPython, legacy.Notes)
	}
	run(ws2, "legacy", lenv, "uv", "venv", ".venv")
	if got := run(ws2, "legacy", lenv, ".venv/bin/python", "--version"); !strings.HasPrefix(got, "Python "+oldestPython) {
		t.Errorf("legacy/'s environment runs %s", got)
	}

	// 4. A pin nothing can serve is said, and the folder runs the image's.
	if n := joinNotes(pkgs["n0"].Notes); !strings.Contains(n, "0.0.1") || !strings.Contains(n, "could not be installed") {
		t.Errorf("an unobtainable node pin was not reported: %q", n)
	}
	if got := run(ws2, "n0", pkgs["n0"].workspace(ws2).Env, "node", "--version"); got != imageNode {
		t.Errorf("n0/ runs node %s, want the image's %s", got, imageNode)
	}

	// 5. The engine starts on the image's node whatever the repository pins at its root.
	if _, err := exec.LookPath("qwen"); err == nil {
		argv := qwenArgv("qwen")
		if len(argv) != 2 || !strings.HasSuffix(argv[0], "/node") {
			t.Errorf("qwen is not started on a named node: %v", argv)
		}
	}

	// 6. What the toolchains are made of survives a trip through the cache: the links in them
	//    are what makes npx and python3 the pinned ones.
	cacheRoot := filepath.Join(work, ".cache")
	pr, pw := io.Pipe()
	go func() {
		_, err := tarFrom(pw, cacheRoot, app.JobCacheMaxBytes)
		pw.CloseWithError(err)
	}()
	restored := t.TempDir()
	if _, err := untarInto(pr, restored); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for _, rel := range []string{"mise/installs/node/20.19.0/bin/npx", "mise/installs/python/3.9"} {
		if st, err := os.Lstat(filepath.Join(restored, rel)); err != nil || st.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s did not come back as a link: %v", rel, err)
		}
	}
}
