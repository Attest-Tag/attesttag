package app

import (
	"context"
	"slices"
	"strings"
	"sync"
)

// A tool call that spent somebody's own connection is logged without what it asked or what came
// back (privateCall, private_calls.go). For http_request that is known before the call: the
// request the model wrote names the connection it will spend. run_js cannot be judged that way.
// Its script decides what to fetch while it runs, and a fetch() through the asker's own Gmail
// brings their mail into the script's output, which was then logged like any other result — onto
// /activity, where everyone holding activity.view reads it.
//
// So the fetch reports what it spent, on a note carried in the tool call's context, and the log
// reads the note once the call is over. One note per call: a script that read somebody's mail
// makes that call private, and the next call starts clean.
type spendNote struct {
	mu    sync.Mutex // the fetch runs on the sandbox's goroutine; the log reads after it returns
	conns []string   // the personal connections spent, by name, in the order first spent
}

type spendNoteKey struct{}

// withSpendNote gives one tool call a fresh note.
func withSpendNote(ctx context.Context) (context.Context, *spendNote) {
	n := &spendNote{}
	return context.WithValue(ctx, spendNoteKey{}, n), n
}

// spendNoteFrom is the note of the tool call ctx belongs to, or nil outside one. Every method is
// safe on nil, so code that spends a connection can report it without asking first.
func spendNoteFrom(ctx context.Context) *spendNote {
	n, _ := ctx.Value(spendNoteKey{}).(*spendNote)
	return n
}

// notePersonal records that the call is about to spend somebody's own connection. Recorded before
// the request rather than after a success: a refusal can quote what was asked, and a note that is
// only ever wrong towards privacy costs a log line its detail and nothing else.
func (n *spendNote) notePersonal(conn string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if !slices.Contains(n.conns, conn) {
		n.conns = append(n.conns, conn)
	}
}

// spentPersonal says whether anything in the call spent somebody's own connection.
func (n *spendNote) spentPersonal() bool {
	if n == nil {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.conns) > 0
}

// personalConns names the personal connections the call spent.
func (n *spendNote) personalConns() []string {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.conns)
}

// privateScriptArgs is what the log keeps of a script that spent somebody's own connection: whose
// account it was and which connection. Never the script: it is where the request is written —
// the URL with its search, the fields picked out — so it says what was asked as plainly as an
// http_request's query does, and there is no endpoint to keep instead, because one run may fetch
// a dozen.
func privateScriptArgs(ctx context.Context, c *Call, conns []string) string {
	kept := struct {
		Connection string `json:"connection,omitempty"`
		Owner      string `json:"owner,omitempty"`
	}{Connection: strings.Join(conns, ", "), Owner: ownerName(ctx, c)}
	return markPrivate(kept)
}

// personalReach names the personal connections a script in this call could fetch through. The
// repeat guard needs it: a refused run_js repeats the script of runs that already happened, and
// with no note from those runs, a channel that reaches somebody's own account is reason enough to
// keep the script out of the log.
func (a *Agent) personalReach(c *Call) []string {
	if a.sandboxFetch(c, nil) == nil {
		return nil
	}
	var names []string
	for _, r := range c.Access.Rules {
		if r.Conn != nil && r.Conn.CredType == "oauth_user" && !slices.Contains(names, r.Conn.Name) {
			names = append(names, r.Conn.Name)
		}
	}
	return names
}
