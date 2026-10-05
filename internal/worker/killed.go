package worker

import (
	"os"
	"strconv"
	"strings"
)

// A check the sandbox stopped before it finished is not a check that failed. A large repository's
// suite or build can run the worker out of memory, and the kernel then kills whatever it picks — the
// runner, or one of its workers, which the runner may report as an ordinary failure — the same way
// before the change as after it. Reported as a failure, that put "tests still fail after this
// change" at the top of a pull request whose change no check had looked at, and told the coding
// agent the suite "FAILED before your change — that may be the bug". It is reported as what it is:
// a check that did not finish, and why.

// cgroupOOMCounters are where the kernel counts the OOM kills in the worker's own cgroup — the
// container's memory limit, which every process of a job shares: cgroup v2's memory.events, then
// v1's memory.oom_control, which only newer kernels give the count in. A variable so a test can
// point it at a file of its own.
var cgroupOOMCounters = []string{"/sys/fs/cgroup/memory.events", "/sys/fs/cgroup/memory/memory.oom_control"}

// oomKills is the cgroup's count of OOM kills so far, -1 when it cannot be read: not Linux, no
// cgroup mounted, a kernel that does not count. Two reads around a check tell whether anything of
// it was killed for want of memory while it ran, whichever process the kernel chose.
func oomKills() int64 {
	for _, path := range cgroupOOMCounters {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), " "); ok && k == "oom_kill" {
				if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && n >= 0 {
					return n
				}
			}
		}
	}
	return -1
}

// sigKill is SIGKILL's number, on every system the worker's sandbox runs on.
const sigKill = 9

// killedBy says why a check that did not pass was stopped from outside before it could finish, or
// "" when it got to its own end. The cgroup's own count says it for certain when it can be read.
// Without it, the marks a kill leaves are read instead: the command itself ended by SIGKILL; a
// shell or a package manager passing on 137 (128 + SIGKILL) or printing "Killed" for a child it
// lost; yarn saying a command failed with SIGKILL; Node's heap running out, which aborts it. In the
// sandbox nothing but the kernel sends a SIGKILL — the worker's own timeout and cancel are told
// apart before this is asked — so one is taken for memory, and said to be likely, not certain.
func killedBy(out procOut, oomBefore, oomAfter int64) string {
	switch {
	case oomBefore >= 0 && oomAfter > oomBefore:
		return "out of memory"
	case strings.Contains(out.Output, "JavaScript heap out of memory"):
		return "out of memory in the JavaScript heap"
	case out.Signal == sigKill, out.Code == 128+sigKill, lastLine(out.Output) == "Killed",
		strings.Contains(out.Output, `failed with signal "SIGKILL"`):
		return "SIGKILL, most likely out of memory"
	}
	return ""
}

// lastLine is the last line of output with anything on it.
func lastLine(s string) string {
	s = strings.TrimRight(s, " \t\r\n")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}
