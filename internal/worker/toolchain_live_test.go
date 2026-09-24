package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"attesttag/internal/app"
)

// mise is what makes "any language" true for the languages the image does not bake in, and what
// makes a pinned version real rather than polite. Both claims are checked here against a live
// fetch, which needs mise, the network and a few minutes — so it runs only when asked, and in
// the worker image, where it is the thing being tested.
//
//	ATTEST_TOOLCHAIN_LIVE=1 go test ./internal/worker -run TestToolchain
func TestToolchainProvisioning(t *testing.T) {
	if os.Getenv("ATTEST_TOOLCHAIN_LIVE") != "1" {
		t.Skip("set ATTEST_TOOLCHAIN_LIVE=1 (needs mise and the network)")
	}
	if _, err := exec.LookPath("mise"); err != nil {
		t.Skip("mise is not installed here")
	}
	work := t.TempDir()
	newWS := func(files map[string]string) *Workspace {
		job := filepath.Join(work, "job")
		repo := filepath.Join(job, "repo")
		for _, d := range []string{"home", "tmp"} {
			os.MkdirAll(filepath.Join(job, d), 0o755)
		}
		os.MkdirAll(repo, 0o755)
		for n, b := range files {
			os.WriteFile(filepath.Join(repo, n), []byte(b), 0o644)
		}
		return &Workspace{JobDir: job, RepoDir: repo, Env: baseEnv(job, work),
			Deadline: time.Now().Add(25 * time.Minute), Reporter: newReporter(newClient(Options{JobID: 1, BotURL: "http://127.0.0.1:1", Token: "t"}), newScrubber(""), func(error) {}), Scrub: newScrubber("")}
	}
	opts := Options{WorkDir: work, MiseBin: "mise"}

	// 1. A toolchain the base image does not carry at all, asked for by the recipe.
	ws := newWS(nil)
	if err := provisionTools(context.Background(), ws, &app.Recipe{Tools: map[string]string{"java": "21"}}, opts); err != nil {
		t.Fatalf("provisioning java: %v", err)
	}
	if tool := stepMissing(&app.RecipeStep{Argv: []string{"java"}}, ws.RepoDir, ".", envValue(ws.Env, "PATH")); tool != "" {
		t.Errorf("java is still missing after provisioning; PATH=%s", envValue(ws.Env, "PATH"))
	}

	// 2. A version the repository pins in a file it already keeps. The image has Node 22; this
	//    asks for 20, and the point is that the steps get 20.
	ws2 := newWS(map[string]string{".nvmrc": "20.19.0\n"})
	if err := provisionTools(context.Background(), ws2, &app.Recipe{Ecosystem: "node", Tools: map[string]string{"node": "20.19.0"}}, opts); err != nil {
		t.Fatalf("provisioning node: %v", err)
	}
	out, err := runCmd(context.Background(), cmdSpec{Dir: ws2.RepoDir, Argv: []string{"node", "--version"}, Env: ws2.Env, Timeout: time.Minute})
	if err != nil || out.Code != 0 {
		t.Fatalf("running the pinned node: %v %s", err, out.Output)
	}
	if got := out.Output; got == "" || got[:3] != "v20" {
		t.Errorf("the repository pinned node 20 and got %q — the pin is not being honoured", got)
	}
	t.Logf("pinned node → %s", out.Output)
}
