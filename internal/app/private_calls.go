package app

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// privateMark opens the logged arguments and result of a call that belongs to one person: their
// personal notes, or an http_request that spent their own connection — their Gmail, Calendar or
// Drive. tool_calls is rendered on /activity to everyone holding activity.view, a viewer among
// them, so neither is ever written as it was. After the mark comes what the log does keep, as
// JSON: whose account it was, the connection, the method and the endpoint without its query
// string, the status and the size of what came back. That is enough to see what the bot did on
// somebody's behalf and whether it worked, and none of it is what they asked or what they got.
//
// Rows logged before the mark carried anything are the bare word. The console takes the mark off
// and sets ToolCallRow.Private; every other reader gets the row as written, which says what it is.
const privateMark = "(private)"

// privateCall says whether a call's arguments and result are one person's. conn is the connection
// an http_request spent (requestConn), nil for every other tool.
func privateCall(name string, conn *Connection) bool {
	return personalMemoryTool(name) || (conn != nil && conn.CredType == "oauth_user")
}

// requestConn is the connection an http_request call will spend and the request it makes. Every
// other tool has neither.
func (a *Agent) requestConn(c *Call, name string, args json.RawMessage) (*Connection, ProxyRequest) {
	var p ProxyRequest
	if name != "http_request" {
		return nil, p
	}
	json.Unmarshal(args, &p)
	return a.matchedConn(c, p), p
}

// privateArgs is what the log keeps of a private call's arguments. The owner is the person whose
// turn it is, because that is whose credential the proxy spends (callAudit) and whose notes the
// memory tools open.
func privateArgs(ctx context.Context, c *Call, conn *Connection, req ProxyRequest) string {
	kept := struct {
		Connection string `json:"connection,omitempty"`
		Owner      string `json:"owner,omitempty"`
		Method     string `json:"method,omitempty"`
		Endpoint   string `json:"endpoint,omitempty"`
	}{Owner: ownerName(ctx, c)}
	if conn != nil {
		kept.Connection = conn.Name
		kept.Method = strings.ToUpper(nonEmpty(strings.TrimSpace(req.Method), "GET"))
		kept.Endpoint = endpointOf(req.URL)
	}
	return markPrivate(kept)
}

// privateResult is what the log keeps of what came back: the size of what the model was handed,
// the status when the request reached the service, and otherwise the first sentence of why it did
// not — held for a Confirm, waiting on an approver, not connected yet. A memory call keeps only its
// size, because its errors can quote the note, and so does a run_js script: what it printed, and
// any error it threw, is whatever it made of the mail or calendar it fetched. So do the Drive
// tools: their first sentence is a count of somebody's files, and their errors name the file.
func privateResult(name string, req ProxyRequest, out string, size int, err error) string {
	kept := struct {
		Status  int    `json:"status,omitempty"`
		Bytes   int    `json:"bytes"`
		Error   string `json:"error,omitempty"`
		Outcome string `json:"outcome,omitempty"`
	}{Bytes: size}
	switch {
	case personalMemoryTool(name), name == "run_js", isDriveTool(name):
	case err != nil:
		kept.Error = privateLine(err.Error(), req.URL)
	default:
		if code, ok := httpStatus(out); ok {
			kept.Status = code
		} else {
			kept.Outcome = privateLine(out, req.URL)
		}
	}
	return markPrivate(kept)
}

func markPrivate(kept any) string {
	raw, _ := json.Marshal(kept)
	return privateMark + " " + string(raw)
}

// ownerName is the person a private call belongs to, by name, falling back to their id.
func ownerName(ctx context.Context, c *Call) string {
	if c.UserID == "" || c.SL == nil {
		return c.UserID
	}
	return c.SL.DisplayName(ctx, c.UserID)
}

// endpointOf is a URL without its query string or fragment. The query is where the content is —
// a Gmail search's q=, a Calendar window, a Drive fullText — and the path says what was called.
func endpointOf(raw string) string {
	raw = strings.TrimSpace(raw)
	endpoint := raw
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		endpoint = u.Scheme + "://" + u.Host + u.EscapedPath()
	} else if i := strings.IndexAny(raw, "?#"); i >= 0 {
		endpoint = raw[:i]
	}
	if cut, more := cutRunes(endpoint, 300); more {
		return cut + "…"
	}
	return endpoint
}

// httpStatus reads the status proxied() leads every response with: "HTTP 200", with or without
// what follows it.
func httpStatus(out string) (int, bool) {
	rest, ok := strings.CutPrefix(out, "HTTP ")
	if !ok || len(rest) < 3 {
		return 0, false
	}
	code, err := strconv.Atoi(rest[:3])
	return code, err == nil
}

var urlQueryRe = regexp.MustCompile(`(https?://[^\s?#"'<>]+)[?#][^\s"'<>]*`)

// privateLine is the first sentence of what a private call said instead of a response. Those
// sentences name the request, so the request's own URL is swapped for its endpoint first — as the
// model wrote it, which may hold a space an URL pattern would stop at — and then any other URL is
// cut at its query. Only the first line is read: a held write's card puts the body on the next.
func privateLine(s, rawURL string) string {
	if rawURL = strings.TrimSpace(rawURL); rawURL != "" {
		s = strings.ReplaceAll(s, rawURL, endpointOf(rawURL))
	}
	s = urlQueryRe.ReplaceAllString(s, "$1")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, ". "); i >= 0 {
		s = s[:i+1]
	}
	s = strings.TrimSpace(s)
	if cut, more := cutRunes(s, 200); more {
		return cut + "…"
	}
	return s
}

// unmarkPrivate is how the console reads a row: the flag set, and the mark taken off what the log
// kept so the page can lay it out. The arguments decide, because a tool's own arguments are JSON
// and cannot begin with the mark, where a result might quote it. A repeated call the guard
// refused keeps its result, which is the refusal and nobody's content.
func (t *ToolCallRow) unmarkPrivate() {
	rest, ok := strings.CutPrefix(t.Args, privateMark)
	if !ok {
		return
	}
	t.Private = true
	t.Args = strings.TrimSpace(rest)
	if rest, ok := strings.CutPrefix(t.Result, privateMark); ok {
		t.Result = strings.TrimSpace(rest)
	}
}
