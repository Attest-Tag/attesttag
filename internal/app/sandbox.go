package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"attesttag/internal/sandbox"
)

// runJSDesc is the whole of the wiring that decides whether this tool ever gets used, so it says
// what the tool is *for* rather than what it is: the model reaches for a calculator when it
// recognises the shape of arithmetic, not when it is told a calculator exists.
const runJSDesc = `Run JavaScript to compute something, and get back what it logs and the value of its last expression. Use it whenever an answer depends on arithmetic or on picking apart data rather than on recalling facts: totals, counts, averages, percentages and percentiles, grouping or deduplicating rows, sorting and taking a top-N, diffing two lists, date and duration maths, parsing CSV or JSON, or checking a regex against real strings. Prefer it to working a calculation out in your head whenever the data is more than a couple of numbers, or the steps are more than one.

Do not use it for a single sum, product or comparison you can state exactly — "what's 17*23" is an answer, not a tool call. The cost of a call is a second of latency and a card in the channel, so it has to buy something.

Pass data through the "input" argument as JSON, not by pasting it into the code; it arrives as the global "input". A previous tool result is a good source for it. If the script fails, read the error and try again — it carries the line number.`

// runJSSealed closes the description in a channel with nothing connected, where the sandbox
// really has no way out at all.
const runJSSealed = `The sandbox is sealed: no filesystem, no network, no require/import, no credentials. Ten seconds and 64MB per call.`

// runJSReach closes it in a channel that has something to reach, and it is attached only in a channel that has
// something to reach: sandboxFetch hands the script a fetch() built from the channel's own
// connections, and returns nil where there are none. Describing fetch() where it does not exist
// is the plain "advertised but not runnable" mistake -- a round spent finding out, with the whole
// prompt re-sent to find out -- and it is ~180 tokens on every round of every turn that pays it.
const runJSReach = `fetch(url) reads from the services connected to this channel — the same hosts http_request can reach, listed above — and blocks rather than returning a promise, so a paging loop needs no await. It returns {status, ok, exhausted, body, truncated, text(), json()}. Use it when the answer needs more data than a tool result can carry: page through a list and count, or pull several endpoints and join them, then return just the number. It can only read; a POST, PATCH or DELETE has to go through http_request so the person in the channel sees it.

One script may make 100 requests, read 64MB in total, and run for two minutes — a whole list of a few thousand rows. So page the whole thing in ONE call rather than a call per batch: nothing carries between calls, and a script that returns "total: 1000" has thrown those thousand rows away. What you hold on to has a separate limit, 64MB of memory, so push the two or three fields you are actually counting rather than the whole row.

If a budget does run out, fetch() returns {exhausted: true} instead of a response, with no body. That is not an error and nothing you gathered is lost: break the loop and return your running totals so far together with where you stopped, e.g. {done: 4200, byStatus: {…}, nextPage: 42}. Pass that straight back as "input" next call and carry on from there — small summaries, never the raw rows.

Otherwise the sandbox is sealed: no filesystem, no require/import, no credentials — the fetch above is performed by the host, which attaches authentication your code never sees.`

// How long one script may run. Ten seconds is a great deal of arithmetic, and it is the wrong
// budget for a script whose time is not arithmetic: one with fetch() spends nearly all of its
// wall clock waiting on somebody else's API, and twenty requests at a few hundred milliseconds
// each is most of the old limit before any work has been done. Cutting a paging loop off there
// sends the turn back to paging by hand, which is the thing this tool exists to replace -- one
// production turn did it in 198 http_request rounds and 9.1M prompt tokens, for an answer a
// single script could have returned.
//
// Thirty seconds was then the same mistake one size smaller. Measured against a real list API,
// a page took ~2.2 seconds, so the clock and the byte budget both ran out at ten pages and a
// run that needed seventy could not be written at all — the model spent sixteen calls finding
// that out. Two minutes is a list of that size read through. It is a long time to hold one of
// four sandbox slots, which is the cost: a script that means to read a whole API blocks one
// slot for as long as it takes, and the alternative was that it could not be written.
const (
	sandboxWall      = 10 * time.Second
	sandboxFetchWall = 2 * time.Minute
)

// sandboxFetch gives a script the channel's own reach and nothing more: the same allowlist,
// credentials and audit rows an http_request would get, minus every method that changes
// something. It grants no authority the model did not already have — which is why it needs no
// separate switch — and it holds a far larger body than a tool result can, because that is the
// point: the page stays in the sandbox and only the answer comes back.
//
// note is the run's spendNote (private_spend.go), nil when nobody will log the call. A fetch that
// goes through somebody's own connection says so on it before it is made, so the call's arguments
// and result are logged as private, the way an http_request through that connection is.
func (a *Agent) sandboxFetch(c *Call, note *spendNote) sandbox.Fetch {
	if c == nil || c.Access == nil || len(c.Access.Rules) == 0 {
		return nil
	}
	return func(ctx context.Context, url string, headers map[string]string) (int, string, bool, error) {
		req := ProxyRequest{Method: "GET", URL: url, Headers: headers, MaxBytes: proxyMaxRead}
		if conn := a.matchedConn(c, req); conn != nil && conn.CredType == "oauth_user" {
			note.notePersonal(conn.Name)
		}
		audit := callAudit(c)
		resp, err := a.proxy.Do(ctx, c.OrgID, c.Access, req, audit, false)
		switch {
		case errors.Is(err, ErrNeedsApproval), errors.Is(err, ErrNeedsConfirmation):
			// A confirmation is a card a person presses in the thread. Nothing can press it
			// from inside a loop, and a loop must never become the way past it.
			return 0, "", false, errors.New("this call needs a person to confirm it; make it with the http_request tool instead")
		case err != nil:
			return 0, "", false, err
		}
		return resp.Status, resp.Body, resp.Truncated, nil
	}
}

func (a *Agent) registerSandboxTools() {
	a.register(a.sandboxTool(runJSDesc + "\n\n" + runJSSealed))
}

// reachingSandboxTool is run_js as a channel with connections sees it: the same tool, with the
// paragraph about fetch() that only makes sense there.
func (a *Agent) reachingSandboxTool() Tool {
	return a.sandboxTool(runJSDesc + "\n\n" + runJSReach)
}

func (a *Agent) sandboxTool(desc string) Tool {
	return Tool{
		Name: "run_js",
		Desc: desc,
		Params: schema(map[string]any{
			"code":  str("JavaScript (ES2023). End with an expression, or console.log what you want back."),
			"input": str("Optional JSON bound to the global `input`, e.g. the rows returned by an earlier tool call"),
		}, "code"),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			var p struct{ Code, Input string }
			json.Unmarshal(args, &p)
			if strings.TrimSpace(p.Code) == "" {
				return "", errors.New("code is required")
			}
			if p.Input != "" && !json.Valid([]byte(p.Input)) {
				return "", errors.New("input must be JSON")
			}
			// Each run is its own JavaScript runtime with a 64 MiB heap. A handful at once is
			// the instance's memory, so they queue for a slot rather than all starting.
			select {
			case sandboxSlots <- struct{}{}:
				defer func() { <-sandboxSlots }()
			case <-ctx.Done():
				return "", ctx.Err()
			}
			lim := sandbox.Limits{Fetch: a.sandboxFetch(c, spendNoteFrom(ctx)), Wall: sandboxWall}
			if lim.Fetch != nil {
				lim.Wall = sandboxFetchWall
			}
			// The elapsed time is measured rather than reported as lim.Wall, because the wall
			// is not always what stopped it: sandbox.Run sits under the turn's own deadline
			// too, and a round started near the end of a turn gets whatever is left. Telling a
			// model its script ran for two minutes when the turn cut it off after forty
			// seconds sends it away to optimise something that was never the problem.
			started := time.Now()
			out, err := sandbox.Run(ctx, p.Code, p.Input, lim)
			if errors.Is(err, sandbox.ErrTimeout) {
				return "", fmt.Errorf("the script ran for %s without finishing; make it cheaper — less data, "+
					"or fewer requests per run — and return only what you need",
					time.Since(started).Round(time.Second))
			}
			return out, err
		},
	}
}

// sandboxSlots bounds how many run_js runtimes exist at once across every organisation.
var sandboxSlots = make(chan struct{}, 4)
