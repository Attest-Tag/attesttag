package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testWall is the wall clock these tests run under. The production default is 10 seconds, which
// is right for a script but is not a sane budget for the test binary: under -race the QuickJS
// WASM runtime takes 5–7 seconds just to start on an idle machine, and longer on a loaded one,
// so inheriting the production default made every test here fail intermittently in exactly the
// build you would want to trust. Tests that are about the limit itself set their own.
const testWall = 120 * time.Second

func run(t *testing.T, code, input string, lim Limits) (string, error) {
	t.Helper()
	if lim.Wall == 0 {
		lim.Wall = testWall
	}
	return Run(context.Background(), code, input, lim)
}

func TestComputes(t *testing.T) {
	out, err := run(t, `const xs=[1,2,3,4]; xs.reduce((a,b)=>a+b,0)`, "", Limits{})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if !strings.Contains(out, "10") {
		t.Fatalf("got %q", out)
	}
}

func TestConsoleAndInput(t *testing.T) {
	out, err := run(t, `console.log("n =", input.rows.length); input.rows.filter(r=>r.ok).length`,
		`{"rows":[{"ok":true},{"ok":false},{"ok":true}]}`, Limits{})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if !strings.Contains(out, "n = 3") || !strings.Contains(out, "=> 2") {
		t.Fatalf("got %q", out)
	}
}

func TestSyntaxErrorIsReadable(t *testing.T) {
	_, err := run(t, `const x = ;`, "", Limits{})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "syntax") {
		t.Fatalf("want a syntax error, got %v", err)
	}
}

func TestInfiniteLoopTimesOut(t *testing.T) {
	start := time.Now()
	_, err := run(t, `while(true){}`, "", Limits{Wall: 2 * time.Second})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("took %s to give up", d)
	}
}

func TestMemoryLimitStopsTheScriptNotTheHost(t *testing.T) {
	_, err := run(t, `new Array(50000000).fill(7).length`, "",
		Limits{Memory: 8 << 20, Wall: 20 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "out of memory") {
		t.Fatalf("want an out-of-memory error, got %v", err)
	}
	// The host must still be able to run the next script.
	if out, err := run(t, `1+1`, "", Limits{}); err != nil || !strings.Contains(out, "2") {
		t.Fatalf("host unusable after OOM: %q %v", out, err)
	}
}

func TestNoFilesystem(t *testing.T) {
	// A file in the process working directory must not be reachable through QuickJS's
	// std/os modules, which the wasm build registers.
	secret := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(secret, []byte("token-abc123"), 0o600)
	wd, _ := os.Getwd()
	os.WriteFile(filepath.Join(wd, "leak-probe.txt"), []byte("token-abc123"), 0o600)
	defer os.Remove(filepath.Join(wd, "leak-probe.txt"))

	for _, code := range []string{
		`import * as std from "qjs:std"; std.open("/leak-probe.txt","r").readAsString()`,
		`import * as os from "qjs:os"; JSON.stringify(os.readdir("/"))`,
		`std.open("/leak-probe.txt","r").readAsString()`,
		`require("fs").readFileSync("/leak-probe.txt","utf8")`,
	} {
		out, err := run(t, code, "", Limits{})
		if err == nil && strings.Contains(out, "token-abc123") {
			t.Fatalf("script read a host file: %q via %s", out, code)
		}
		if err == nil && strings.Contains(out, "leak-probe") {
			t.Fatalf("script listed the host working directory via %s: %q", code, out)
		}
	}
}

func TestNoNetwork(t *testing.T) {
	for _, code := range []string{`typeof fetch`, `typeof XMLHttpRequest`, `typeof WebSocket`} {
		out, err := run(t, code, "", Limits{})
		if err != nil {
			t.Fatalf("%s: %v", code, err)
		}
		if !strings.Contains(out, "undefined") {
			t.Fatalf("%s is defined in the sandbox: %q", code, out)
		}
	}
}

func TestOutputIsCapped(t *testing.T) {
	out, err := run(t, `for(let i=0;i<2000;i++) console.log("x".repeat(100)); "done"`, "",
		Limits{MaxOutput: 500, Wall: 20 * time.Second})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if len(out) > 700 {
		t.Fatalf("output not capped: %d bytes", len(out))
	}
}

// --- network ---

func fakeFetch(t *testing.T, pages map[string]string, calls *int) Fetch {
	t.Helper()
	return func(_ context.Context, url string, _ map[string]string) (int, string, bool, error) {
		*calls++
		body, ok := pages[url]
		if !ok {
			return 404, `{"error":"not found"}`, false, nil
		}
		return 200, body, false, nil
	}
}

func TestFetchIsAbsentUnlessGranted(t *testing.T) {
	out, err := run(t, `typeof fetch`, "", Limits{})
	if err != nil || !strings.Contains(out, "undefined") {
		t.Fatalf("fetch leaked into a sandbox with no Fetch: %q %v", out, err)
	}
}

func TestFetchPagesAndAggregates(t *testing.T) {
	calls := 0
	pages := map[string]string{
		"https://api.example.com/tasks?page=1": `{"items":[{"pts":3},{"pts":5}],"next":"https://api.example.com/tasks?page=2"}`,
		"https://api.example.com/tasks?page=2": `{"items":[{"pts":8}],"next":null}`,
	}
	out, err := Run(context.Background(), `
		let url = "https://api.example.com/tasks?page=1", total = 0, n = 0;
		while (url) {
			const page = fetch(url).json();
			for (const it of page.items) { total += it.pts; n++; }
			url = page.next;
		}
		({ total, n })
	`, "", Limits{Fetch: fakeFetch(t, pages, &calls)})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if !strings.Contains(out, `"total":16`) || !strings.Contains(out, `"n":3`) {
		t.Fatalf("got %q", out)
	}
	if calls != 2 {
		t.Fatalf("want 2 requests, got %d", calls)
	}
}

func TestFetchWorksUnderAwait(t *testing.T) {
	calls := 0
	pages := map[string]string{"https://api.example.com/x": `{"v":42}`}
	out, err := Run(context.Background(),
		`const r = await fetch("https://api.example.com/x"); return r.json().v;`,
		"", Limits{Fetch: fakeFetch(t, pages, &calls)})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if !strings.Contains(out, "42") {
		t.Fatalf("got %q", out)
	}
}

func TestFetchRefusesWrites(t *testing.T) {
	calls := 0
	_, err := Run(context.Background(),
		`fetch("https://api.example.com/x", {method:"POST", body:"{}"})`,
		"", Limits{Fetch: fakeFetch(t, map[string]string{}, &calls)})
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "http_request") {
		t.Fatalf("the refusal should point at http_request, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("a write reached the host: %d calls", calls)
	}
}

// Running out of requests ends the loop and keeps everything the loop had gathered. It used to
// throw, which killed the script and lost all of it — the failure this whole path exists to
// stop, and the one a production run hit three times in a row.
func TestFetchRequestBudget(t *testing.T) {
	calls := 0
	out, err := Run(context.Background(), `
		let ok = 0, spent = 0;
		for (let i = 0; i < 50; i++) {
			const r = fetch("https://api.example.com/x");
			if (r.exhausted) { spent++; break; }
			ok++;
		}
		({ ok, spent })
	`, "", Limits{Fetch: fakeFetch(t, map[string]string{"https://api.example.com/x": "{}"}, &calls),
		MaxRequests: 5})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if calls > 5 {
		t.Fatalf("budget exceeded: %d host calls", calls)
	}
	if !strings.Contains(out, `"ok":5`) || !strings.Contains(out, `"spent":1`) {
		t.Fatalf("the script should have kept its five pages and stopped: %q", out)
	}
}

// The same budget, read past rather than checked: a script that ignores the flag and asks for
// the body is still stopped, and still told which budget it was.
func TestFetchBudgetStillStopsAScriptThatIgnoresIt(t *testing.T) {
	calls := 0
	_, err := Run(context.Background(), `
		let n = 0;
		for (let i = 0; i < 50; i++) { fetch("https://api.example.com/x").json(); n++; }
		n
	`, "", Limits{Fetch: fakeFetch(t, map[string]string{"https://api.example.com/x": "{}"}, &calls),
		MaxRequests: 5})
	if err == nil || !strings.Contains(err.Error(), "requests") {
		t.Fatalf("want a refusal naming the budget, got %v", err)
	}
	if calls > 5 {
		t.Fatalf("budget exceeded: %d host calls", calls)
	}
}

// The byte budget stops a paging loop the same way the request budget does, and for the same
// reason: it is the one that actually binds on a real list API, where a page is most of a MB.
func TestFetchByteBudget(t *testing.T) {
	calls := 0
	page := strings.Repeat("x", 4096)
	out, err := Run(context.Background(), `
		let pages = 0;
		while (true) {
			const r = fetch("https://api.example.com/x");
			if (r.exhausted) break;
			pages++;
		}
		pages
	`, "", Limits{Fetch: fakeFetch(t, map[string]string{"https://api.example.com/x": page}, &calls),
		MaxRequests: 100, MaxFetchBytes: 10 * 4096})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if !strings.Contains(out, "=> 10") {
		t.Fatalf("ten pages fit in ten pages of budget, got %q", out)
	}
}

func TestFetchErrorReachesTheScript(t *testing.T) {
	calls := 0
	fetch := func(_ context.Context, _ string, _ map[string]string) (int, string, bool, error) {
		calls++
		return 0, "", false, errors.New("this call needs a person to confirm it; make it with the http_request tool instead")
	}
	out, err := Run(context.Background(),
		`try { fetch("https://api.example.com/x"); "no error" } catch (e) { e.message }`,
		"", Limits{Fetch: fetch})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if !strings.Contains(out, "http_request") {
		t.Fatalf("the script could not see why it was refused: %q", out)
	}
}

// A model that ends a script with `return` has assumed it is inside a function. Nothing ran --
// that is a parse error -- so it is given the function and the value comes back.
func TestTopLevelReturnIsGivenAFunction(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, code, want string }{
		{"plain return", `const rows = [1,2,3]; return rows.length;`, "3"},
		{"return after log", `console.log("counting"); return 41 + 1;`, "42"},
		{"nested return still yields the last expression", `function f(){ return 7 } f() * 2`, "14"},
		{"return inside a loop body", `for (const n of [5]) { if (n) { return n * 4 } }`, "20"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Run(ctx, tc.code, "", Limits{})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("got %q, want it to contain %q", out, tc.want)
			}
		})
	}
	// A real syntax error is still a real syntax error.
	if _, err := Run(ctx, `const = ;`, "", Limits{}); err == nil {
		t.Error("a broken script should still fail")
	}
}

// Numbers came back with their last digit replaced by a space -- console.log(12345) printed
// "1234 " -- and [12] came back as 12. Nothing about it was random: whether a value was damaged
// depended on where its text sat in the wasm heap, and that moves with the script's length, its
// input and whether fetch is bound. So two hundred runs of one script prove no more than one run
// does, and each run here gets a fresh runtime with a different heap: the script is shifted by
// a comment of a different length, and the runs cycle through a bare sandbox, one with fetch
// bound as run_js has it in production, and one with input. Against the old display every case
// failed in some of its runs -- each digit case in at least nineteen of the two hundred -- but
// console.log('abc'), String(...) and {"n":20100}, which never did and stay as the controls.
func TestValuesPrintExactly(t *testing.T) {
	const runs = 200
	for _, tc := range []struct{ code, want string }{
		{`console.log(12345)`, "12345\n"},
		{`console.log(20100)`, "20100\n"},
		{`console.log(123456)`, "123456\n"},
		{`console.log('abc')`, "abc\n"},
		{`const v = 20100; v`, "=> 20100"},
		{`[4200,15000,900].reduce((a,b)=>a+b,0)`, "=> 20100"},
		{`[4200, 15000, 900].reduce((a, b) => a + b, 0)`, "=> 20100"},
		{`let s=0; for (const x of [4200,15000,900]) s+=x; s`, "=> 20100"},
		{`4200+15000+900`, "=> 20100"},
		{`20100 + 0`, "=> 20100"},
		{`Math.max(20100, 1)`, "=> 20100"},
		{`[20100][0]`, "=> 20100"},
		{`Number('20100')`, "=> 20100"},
		{`String(4200+15000+900)`, "=> 20100"},
		{`({n: 4200+15000+900})`, `=> {"n":20100}`},
		{`false`, "=> false"},
		{`[12]`, "=> [12]"},
		// JSON writes null for these. The old display printed null or the real value depending
		// on the heap -- the real value only when the damage sent it down the fallback -- so an
		// empty average could read NaN in one call and null in the next.
		{`const xs = []; xs.reduce((a,b)=>a+b,0)/xs.length`, "=> NaN"},
		{`console.log(20100/0, -1/0)`, "Infinity -Infinity\n"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			t.Parallel()
			wrong, first := 0, ""
			for i := range runs {
				pad := strings.Repeat("x", i/3)
				lim, input, how := Limits{}, "", "bare"
				switch i % 3 {
				case 1:
					lim.Fetch, how = fakeFetch(t, nil, new(int)), "with fetch"
				case 2:
					input, how = `{"rows":[1,2,3]}`, "with input"
				}
				out, err := run(t, "/*"+pad+"*/"+tc.code, input, lim)
				if err != nil {
					out = "error: " + err.Error()
				}
				if out != tc.want {
					if wrong == 0 {
						first = fmt.Sprintf("run %d (%d-byte pad, %s) gave %q", i, len(pad), how, out)
					}
					wrong++
				}
			}
			if wrong > 0 {
				t.Errorf("%d of %d runs printed something other than %q; the first, %s", wrong, runs, tc.want, first)
			}
		})
	}
}
