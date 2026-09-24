// Package sandbox runs untrusted JavaScript with hard resource limits.
//
// The code it runs is written by the model, and the model's context is full of things other
// people wrote — channel messages, fetched pages, documents. So the code is treated as hostile:
// the interpreter is QuickJS compiled to WebAssembly and run under wazero, which gives it no
// syscalls, no network and no filesystem beyond what the host hands over, and a memory ceiling
// the guest cannot grow past.
package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/fastschema/qjs"
)

// Fetch performs one outbound read on the script's behalf. It is supplied by the caller — the
// sandbox has no network of its own — so whatever allowlist, credentials and audit trail the
// host applies to an ordinary request apply here unchanged.
type Fetch func(ctx context.Context, url string, headers map[string]string) (status int, body string, truncated bool, err error)

// Limits are the resources one call may use. Zero fields take the defaults below.
type Limits struct {
	Memory    int           // bytes of QuickJS heap
	Stack     int           // bytes of interpreter stack
	Wall      time.Duration // wall clock for the whole call
	MaxOutput int           // bytes of console output plus result

	// Fetch, when set, gives the script a synchronous fetch(). Nil means no network at all.
	Fetch Fetch
	// MaxRequests and MaxFetchBytes bound what one script may pull; zero takes the defaults.
	MaxRequests   int
	MaxFetchBytes int
}

func (l Limits) withDefaults() Limits {
	if l.Memory == 0 {
		l.Memory = 64 << 20
	}
	if l.Stack == 0 {
		l.Stack = 1 << 20
	}
	if l.Wall == 0 {
		l.Wall = 10 * time.Second
	}
	if l.MaxOutput == 0 {
		l.MaxOutput = 8000
	}
	if l.MaxRequests == 0 {
		l.MaxRequests = 100
	}
	// What one script may read in total. This bounds bandwidth and the time spent on it, not
	// the prompt: a fetched body is parsed and dropped inside the sandbox and only the answer
	// comes back, so the old 8 MiB was protecting the model's context from bytes that were
	// never going to reach it. It was also, measured against a real list API, the binding
	// limit: a ClickUp page of 100 tasks is ~800 KB, so ten pages exhausted the budget, and a
	// production run on 2026-09-21 spent sixteen run_js calls discovering that ten was the
	// number — each call re-paging from a higher offset and returning a count, because nothing
	// carries between calls. Sixty-four is the whole of a list that size in one script.
	//
	// It is deliberately not the heap (Memory, above): bodies are transient, so a script may
	// read far more than it can hold. What it keeps is its own business and its own ceiling.
	if l.MaxFetchBytes == 0 {
		l.MaxFetchBytes = 64 << 20
	}
	return l
}

// ErrTimeout is returned when the script outran its wall clock.
var ErrTimeout = errors.New("script timed out")

// Run evaluates code and returns whatever it logged plus the value of its last expression.
// input, when non-empty, must be JSON; it is parsed and bound to the global `input`.
func Run(ctx context.Context, code, input string, lim Limits) (out string, err error) {
	lim = lim.withDefaults()
	ctx, cancel := context.WithTimeout(ctx, lim.Wall)
	defer cancel()

	// qjs mounts Option.CWD at / inside the guest and defaults it to the process working
	// directory, which would hand the script the container's filesystem through QuickJS's
	// std/os modules. Mount an empty directory instead: the guest keeps a root, and there is
	// nothing in it.
	jail, err := os.MkdirTemp("", "qjs-jail-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(jail)

	// When wazero tears the module down mid-call — which is how a deadline is enforced — the
	// binding panics rather than returning, and panics again while closing. Both are contained
	// here so that a runaway script is an error the model can read, not a downed bot.
	defer func() {
		if r := recover(); r != nil {
			if ctx.Err() != nil {
				out, err = "", ErrTimeout
				return
			}
			out, err = "", fmt.Errorf("sandbox aborted: %s", truncateMsg(fmt.Sprint(r)))
		}
	}()

	rt, err := qjs.New(qjs.Option{
		CWD:     jail,
		Context: ctx,
		// The engine's own MaxExecutionTime is not wired up in the wasm build, so the
		// deadline rests on this: wazero closes the module when ctx is done.
		CloseOnContextDone: true,
		MemoryLimit:        lim.Memory,
		MaxStackSize:       lim.Stack,
		Stdout:             io.Discard,
		Stderr:             io.Discard,
	})
	if err != nil {
		return "", fmt.Errorf("sandbox start: %w", err)
	}
	defer safeClose(rt)

	var buf strings.Builder
	jc := rt.Context()
	jc.SetFunc("__log", func(t *qjs.This) (*qjs.Value, error) {
		// A line past the cap is dropped, so it is not rendered either. Rendering a value is some
		// twenty calls into the runtime and keeps its text in the heap until the run ends (see
		// jsonStringify): a loop logging 300,000 objects took ten seconds of wall clock with
		// every line rendered and three without, and held all the lines it could not print.
		if buf.Len() >= lim.MaxOutput {
			return t.Context().NewUndefined(), nil
		}
		parts := make([]string, 0, len(t.Args()))
		for _, a := range t.Args() {
			parts = append(parts, display(a))
		}
		buf.WriteString(strings.Join(parts, " "))
		buf.WriteByte('\n')
		return t.Context().NewUndefined(), nil
	})
	if input != "" {
		jc.SetFunc("__input", func(t *qjs.This) (*qjs.Value, error) {
			return t.Context().NewString(input), nil
		})
	}
	if lim.Fetch != nil {
		bindFetch(jc, ctx, lim)
	}

	// The prelude is evaluated separately so that line numbers in errors refer to the
	// script the model actually wrote.
	prelude := "globalThis.console = { log: __log, info: __log, warn: __log, error: __log, debug: __log };"
	if input != "" {
		prelude += " globalThis.input = JSON.parse(__input());"
	}
	if lim.Fetch != nil {
		prelude += fetchPrelude
	}
	if v, err := jc.Eval("prelude.js", qjs.Code(prelude)); err != nil {
		return "", fmt.Errorf("sandbox start: %w", err)
	} else {
		v.Free()
	}

	// Top-level await is not valid in a plain script, and models write it by habit. When the
	// code uses it, run the whole thing inside an async function and await the result, so the
	// model's instinct works instead of costing it a round trip on a syntax error.
	wrapped := awaitRe.MatchString(code)
	if wrapped {
		code = "(async () => {\n" + code + "\n})()"
	}
	res, err := jc.Eval("script.js", qjs.Code(code))
	// And the same habit one step further on: a model that finishes a script with `return rows`
	// is saying what it wants handed back, having assumed it was inside a function. It is not, so
	// the script does not run -- `return` outside a function is a parse error, which means
	// nothing has executed and there is nothing to undo. Give it the function it assumed, and
	// the value it returns becomes the result the same way a last expression would. Retried on
	// the error rather than sniffed for with a regex: `return` inside a nested function is both
	// legal and ordinary, and wrapping a script that did not need it would swallow the value of
	// its last expression.
	if err != nil && !wrapped && returnOutsideFunction(err) {
		wrapped = true
		res, err = jc.Eval("script.js", qjs.Code("(() => {\n"+code+"\n})()"))
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return buf.String(), ErrTimeout
		}
		return buf.String(), errors.New(cleanErr(err))
	}
	defer res.Free()

	if wrapped && res.IsPromise() {
		settled, aerr := res.Await()
		if aerr != nil {
			return buf.String(), errors.New(cleanErr(aerr))
		}
		defer settled.Free()
		res = settled
	}

	if !res.IsUndefined() {
		if buf.Len() > 0 {
			buf.WriteString("\n")
		}
		buf.WriteString("=> " + display(res))
	}
	s := buf.String()
	if len(s) > lim.MaxOutput {
		s = s[:lim.MaxOutput] + "\n… output truncated"
	}
	if strings.TrimSpace(s) == "" {
		return "(the script produced no output; log a value or end with an expression)", nil
	}
	return s, nil
}

// safeClose swallows the secondary panic qjs raises when it frees a runtime whose wasm module
// wazero has already closed.
func safeClose(rt *qjs.Runtime) {
	defer func() { _ = recover() }()
	rt.Close()
}

func truncateMsg(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// fetchPrelude gives the script a fetch() that reads like the browser one but blocks, so a model
// can write a paging loop without an event loop to reason about.
const fetchPrelude = `
globalThis.fetch = function (url, opts) {
  opts = opts || {};
  const method = String(opts.method || "GET").toUpperCase();
  if (method !== "GET" && method !== "HEAD") {
    throw new Error("the sandbox can only read; make the " + method + " with the http_request tool so a person can see it");
  }
  const r = JSON.parse(__fetch(String(url), JSON.stringify(opts.headers || {})));
  if (r.error) throw new Error(r.error);
  const spent = function () { throw new Error(r.message); };
  return {
    status: r.status || 0,
    ok: !r.exhausted && r.status >= 200 && r.status < 300,
    // exhausted says the budget ran out before this request was made, so there is no body and
    // no status: break your loop and return what you already have. A script that ignores it
    // and reads the body anyway gets the same message as an exception instead.
    exhausted: !!r.exhausted,
    body: r.body || "",
    truncated: !!r.truncated,
    text: r.exhausted ? spent : function () { return r.body; },
    json: r.exhausted ? spent : function () { return JSON.parse(r.body); },
  };
};
`

var awaitRe = regexp.MustCompile(`\bawait\b`)

// returnOutsideFunction recognises the one parse error worth retrying: QuickJS says
// "return not in a function", and every engine words it about that way.
func returnOutsideFunction(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "return not in a function") ||
		strings.Contains(msg, "'return' outside of function") ||
		strings.Contains(msg, "illegal return statement")
}

// bindFetch installs the host half of fetch(), with the per-script budgets that stop a loop from
// turning one tool call into a thousand requests.
func bindFetch(jc *qjs.Context, ctx context.Context, lim Limits) {
	var calls, bytes int
	jc.SetFunc("__fetch", func(t *qjs.This) (*qjs.Value, error) {
		fail := func(msg string) (*qjs.Value, error) {
			b, _ := json.Marshal(map[string]string{"error": msg})
			return t.Context().NewString(string(b)), nil
		}
		// Running out of budget is a boundary, not a fault: the script asked a reasonable
		// question and the host is declining to answer any more of them. Returning it as an
		// error made fetch() throw, which killed the script and took everything it had already
		// gathered with it — a paging loop that had read a thousand rows lost all thousand and
		// came back with nothing to show for half a minute of work. So it comes back as a
		// response the loop can test, and only a script that ignores the flag and reads the
		// body anyway is stopped by an exception. See fetchPrelude.
		exhausted := func(msg string) (*qjs.Value, error) {
			b, _ := json.Marshal(map[string]any{"exhausted": true, "message": msg})
			return t.Context().NewString(string(b)), nil
		}
		args := t.Args()
		if len(args) == 0 {
			return fail("fetch needs a url")
		}
		if calls >= lim.MaxRequests {
			return exhausted(fmt.Sprintf("this script has used all %d of its requests; "+
				"return what you have gathered so far and where you stopped", lim.MaxRequests))
		}
		if bytes >= lim.MaxFetchBytes {
			return exhausted(fmt.Sprintf("this script has read its %d MB; "+
				"return what you have gathered so far and where you stopped", lim.MaxFetchBytes>>20))
		}
		calls++

		headers := map[string]string{}
		if len(args) > 1 {
			json.Unmarshal([]byte(args[1].String()), &headers)
		}
		status, body, truncated, err := lim.Fetch(ctx, args[0].String(), headers)
		if err != nil {
			return fail(truncateMsg(err.Error()))
		}
		bytes += len(body)
		b, err := json.Marshal(map[string]any{"status": status, "body": body, "truncated": truncated})
		if err != nil {
			return fail("response could not be encoded")
		}
		return t.Context().NewString(string(b)), nil
	})
}

// display renders a value the way a person reading a transcript would want it: strings bare,
// everything else as JSON, and anything JSON cannot hold as its toString.
func display(v *qjs.Value) string {
	if v.IsString() {
		return v.String()
	}
	// NaN and Infinity are among what JSON cannot hold, but it writes null for them instead of
	// failing, and null reads as a missing value where the truth is a sum that went wrong -- the
	// average of an empty list, a division by zero.
	if v.IsNumber() {
		if f := v.Float64(); math.IsNaN(f) || math.IsInf(f, 0) {
			return v.String()
		}
	}
	if s, err := jsonStringify(v); err == nil && s != "" {
		return s
	}
	return v.String()
}

// jsonStringify is JSON.stringify called as JavaScript, its result read back like any other
// string. It is not the binding's v.JSONStringify(), whose C half in qjs v0.0.6 frees the JSON
// text before handing Go its address (fastschema/qjs#44, open and unfixed): Go then reads memory
// the allocator has already written its own bookkeeping into, which for a result of five to
// seven characters puts a space over the fifth and ends the string there. So 20100 printed as
// "2010 " and 123456 as "1234 ", and [12], whose terminator went instead, fell through to
// toString and printed as 12. Whether a value was hit depended on where its text sat in the
// heap, so the same sum could print right in one call and wrong in the next. String() never
// frees early; the text lives until the runtime closes.
func jsonStringify(v *qjs.Value) (string, error) {
	jsJSON := v.Context().Global().GetPropertyStr("JSON")
	defer jsJSON.Free()
	s, err := jsJSON.InvokeJS("stringify", v)
	if err != nil {
		return "", err
	}
	defer s.Free()
	return s.String(), nil
}

// cleanErr flattens an exception onto one line, keeping the "at script.js:1:20" location because
// that is what lets the model fix its own code on the next attempt.
func cleanErr(err error) string {
	fields := strings.Fields(err.Error())
	return truncateMsg(strings.Join(fields, " "))
}
