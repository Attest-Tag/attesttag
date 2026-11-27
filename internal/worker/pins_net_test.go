package worker

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// Every clone, fetch and push carries gitPins, and the other tests clone over file://, where a pin
// that breaks TLS goes unnoticed: an empty http.sslCAInfo once failed every https remote's
// certificate check, so every fix job failed at clone. This reaches a real https remote with the
// pins on, where the network allows (ATTEST_NET=1; the worker image check sets it).
func TestGitPinsStillReachAnHTTPSRemote(t *testing.T) {
	if os.Getenv("ATTEST_NET") != "1" {
		t.Skip("set ATTEST_NET=1 to reach github.com")
	}
	dir := t.TempDir()
	out, err := runCmd(context.Background(), cmdSpec{Dir: dir, Env: baseEnv(dir, dir), Timeout: time.Minute,
		Argv: append(append([]string{"git"}, gitPins...), "ls-remote", "https://github.com/git/git.git", "HEAD")})
	if err != nil || out.Code != 0 || !strings.Contains(out.Output, "HEAD") {
		t.Fatalf("ls-remote with the pins: code %d, err %v\n%s", out.Code, err, out.Output)
	}
}
