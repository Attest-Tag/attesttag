package app

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// http_request is the generic door to every connected service. Its description is rebuilt per
// call so the model knows which hosts this channel may reach and how to use them.
func (a *Agent) httpTool(c *Call) Tool {
	var b strings.Builder
	b.WriteString("Call an HTTP API of a connected service. Credentials are added by a proxy; never include them yourself. Reachable hosts in this channel: ")
	hosts := c.Access.ReachableHosts()
	if len(hosts) == 0 {
		b.WriteString("none (ask an admin to add a connection in the console).")
	} else {
		b.WriteString(strings.Join(hosts, "; ") + ".")
	}
	b.WriteString(" Reads (GET) run straight away. Write requests (POST/PUT/PATCH/DELETE) may wait for a person in the thread to press Confirm.")
	// Only said when there is actually a choice: a channel with one credential per host should
	// not be paying prompt for a parameter it can never need.
	if shared := c.Access.SharedHosts(); len(shared) > 0 {
		b.WriteString(" More than one connection covers " + strings.Join(shared, "; ") +
			" — pass connection with the name to pick one. If it matters which and the request does not say, ask before calling.")
	}
	return Tool{
		Name: "http_request", Desc: b.String(),
		Params: schema(map[string]any{
			"method":     map[string]any{"type": "string", "enum": []string{"GET", "POST", "PUT", "PATCH", "DELETE"}},
			"url":        str("Absolute https URL on a reachable host, including query string"),
			"body":       str("Request body (JSON string) for POST/PUT/PATCH"),
			"headers":    map[string]any{"type": "object", "description": "Extra non-auth headers", "additionalProperties": map[string]any{"type": "string"}},
			"connection": str("Which connection's credential to use, by name. Only needed when several cover the same host; omit otherwise"),
		}, "method", "url"),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			p, err := httpToolRequest(args)
			if err != nil {
				return "", err
			}
			return a.proxied(ctx, c, p)
		},
	}
}

// httpToolRequest reads the model's arguments for http_request into a request.
//
// Which files ride along is not the model's to say. attach_files is on the struct for the pack
// tools, which fill it from attachableFiles — the files this turn was actually handed — and it is
// deliberately absent from http_request's schema; but a key the schema never mentions still decodes
// into a tagged field, and nothing downstream checks those ids against the turn. They are fetched
// at confirm time with the bot token, which reads private channels and DMs the asker cannot, so an
// id the model composed is a way to put a file somebody was never shown onto a ticket they can
// read — and the card says "from this thread" either way.
//
// Dropped here rather than in proxied(), which is also the pack tools' road and is where theirs has
// to survive; and a function of its own so the rule has one home and a test can hold it there.
func httpToolRequest(args json.RawMessage) (ProxyRequest, error) {
	var p ProxyRequest
	if err := json.Unmarshal(args, &p); err != nil {
		return ProxyRequest{}, err
	}
	p.AttachFiles = nil
	return p, nil
}

// proxied runs a request through the proxy for this call, handling the confirmation gate.
func (a *Agent) proxied(ctx context.Context, c *Call, p ProxyRequest) (string, error) {
	p.MaxBytes = modelMaxBody // whatever the model asked for, a tool result stays prompt-sized
	// A preview sends no write, and a connection whose writes are automatic would otherwise send
	// one without the proxy ever saying it was a write. So the proxy holds them all for a preview,
	// and the branch below stops them before anything that could still let one through.
	p.HoldWrites = c.Preview
	audit := callAudit(c)
	resp, err := a.proxy.Do(ctx, c.OrgID, c.Access, p, audit, false)
	if errors.Is(err, ErrNeedsApproval) {
		// Deliberately no allowedByRule here. An allow rule is a model reading an admin's prose
		// about text the model itself composed; that is fine for "post the standup summary" and
		// is not an authorization boundary for handing out access.
		if c.Preview {
			return c.previewHold(strings.ToUpper(p.Method)+" "+p.URL, "it would need a named approver's OK"), nil
		}
		if c.Silent {
			return c.silentRefusal(strings.ToUpper(p.Method) + " " + p.URL), nil
		}
		raw, _ := json.Marshal(p)
		c.holdForGrant(raw)
		return fmt.Sprintf("Held for approval: %s %s needs a named approver's OK, and it is not %s's to confirm. "+
			"Say plainly what you have asked for and who has to approve it. Do not say the access has been granted.",
			strings.ToUpper(p.Method), p.URL, "the requester"), nil
	}
	if errors.Is(err, ErrNeedsUserAuth) {
		if c.Preview {
			// askToConnect DMs the person a consent card. A preview is a page someone is
			// looking at, so the answer belongs on the page, not in their DMs.
			name := "That service"
			if conn := a.matchedConn(c, p); conn != nil {
				name = conn.Name
			}
			return name + " is connected per person and whoever this preview is running as has not connected theirs. " +
				"Say so plainly; in the channel they would be sent a Connect link. Do not retry.", nil
		}
		if c.Silent {
			// A routine speaks to nobody, so there is nobody to hand a consent screen to. Say
			// why it stopped rather than posting into a channel that asked for quiet.
			return c.silentRefusal(strings.ToUpper(p.Method) + " " + p.URL), nil
		}
		return a.askToConnect(ctx, c, a.matchedConn(c, p)), nil
	}
	if errors.Is(err, ErrNeedsConfirmation) {
		// A preview stops here, ahead of the two things below that exist to let a write run with
		// nobody pressing anything. Either one would send it: the page is somebody trying the
		// channel's settings, not the channel.
		if c.Preview {
			conn := a.matchedConn(c, p)
			return c.previewHold(httpConfirmSummary(conn, p), previewInChannel(conn)), nil
		}
		// A routine set up to act was told this once, when it was written, and asking again on
		// every run asks nobody: the card goes out at 6am and expires unpressed. Checked before
		// the allow rules because it is the cheaper answer to the same question — a rule check
		// is a model call, and this one is a flag somebody set deliberately.
		if c.autoConfirm {
			resp, err = a.proxy.Do(ctx, c.OrgID, c.Access, p, audit, true)
			if err != nil {
				return "", err
			}
			c.writesRun++
			return fmt.Sprintf("HTTP %d (ran without asking: this routine is set to confirm its own writes)%s\n%s%s",
				resp.Status, viewLine(resp.Body), resp.Body, bodyNote(resp)), nil
		}
		// An allow rule may cover it; otherwise hold it for a human. The second description is
		// the one a turn a forwarded email started is judged on — the same request with the body
		// left out, because on that lane the body is text somebody outside the company steered.
		conn := a.matchedConn(c, p)
		if rule, ok := a.allowedByRule(ctx, c, describeHTTPWrite(conn, p), describeHTTPWriteDestination(conn, p)); ok {
			resp, err = a.proxy.Do(ctx, c.OrgID, c.Access, p, audit, true)
			if err != nil {
				return "", err
			}
			// Counted like the autoConfirm branch above: both are writes that happened with
			// nobody pressing anything, which is what the count is for.
			c.writesRun++
			return fmt.Sprintf("HTTP %d (ran without asking: pre-approved by the allow rule %q)%s\n%s%s",
				resp.Status, rule, viewLine(resp.Body), resp.Body, bodyNote(resp)), nil
		}
		if c.Silent {
			return c.silentRefusal(strings.ToUpper(p.Method) + " " + p.URL), nil
		}
		raw, _ := json.Marshal(p)
		id, aerr := a.store.AddPendingWrite(ctx, c.OrgID, c.TeamID, c.Channel, c.ThreadTS, c.UserID, string(raw))
		if aerr != nil {
			return "", aerr
		}
		c.holdForConfirm(id, httpConfirmSummary(a.matchedConn(c, p), p))
		return fmt.Sprintf("This is a write request (%s %s) and needs a human OK. Tell the user exactly what will be sent. %s",
			strings.ToUpper(p.Method), p.URL, heldNote(c)), nil
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("HTTP %d%s\n%s%s", resp.Status, viewLine(resp.Body), resp.Body, bodyNote(resp)), nil
}

// bodyNote says that a response was cut short, and what to do about it. The proxy has always
// truncated at modelMaxBody and never mentioned it, so a model handed the first half of a list
// counted what it could see and answered with a number that was simply wrong, confidently. A
// truncation the model knows about is a narrower query or a run_js loop; one it does not know
// about is a made-up total.
func bodyNote(resp *ProxyResponse) string {
	if resp == nil || !resp.Truncated {
		return ""
	}
	return fmt.Sprintf("\n\n[cut off: this is the first %d KB of a longer response, so anything you count "+
		"or total from it is incomplete. Narrow it with the API's own filters or paging, or do the whole job "+
		"in run_js with fetch(), which reads far more than a tool result can carry and returns just the answer.]",
		modelMaxBody>>10)
}

// Keys services use for the page a person can open, best first. An API self-link (api.github.com,
// api.clickup.com) is not one of those, so hosts starting with "api." are skipped.
var linkKeys = []string{"html_url", "permalink", "web_url", "app_url", "short_url", "browse_url", "url", "link"}

// viewLine pulls the human-viewable link out of a response — the ClickUp task, the GitHub issue,
// the Slack message that was just created — and states it plainly, so the reply can carry a link
// people can click instead of an id they cannot use.
func viewLine(body string) string {
	if link := viewableLink(body); link != "" {
		return " view: " + link
	}
	return ""
}

func viewableLink(body string) string {
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return ""
	}
	found := map[string]string{}
	var walk func(any, int)
	walk = func(n any, depth int) {
		if depth > 3 {
			return
		}
		switch t := n.(type) {
		case map[string]any:
			for _, k := range linkKeys {
				s, _ := t[k].(string)
				if found[k] == "" && isViewable(s) {
					found[k] = s
				}
			}
			for _, child := range t {
				walk(child, depth+1)
			}
		case []any:
			for _, child := range t {
				walk(child, depth+1)
			}
		}
	}
	walk(v, 0)
	for _, k := range linkKeys {
		if found[k] != "" {
			return found[k]
		}
	}
	return ""
}

// viewedLink reads the link back out of the notes a replayed write left behind, for the places
// that have the note and not the response any more — the card an approver pressed, above all.
//
// It looks only at the head of each note, before the response body the note also carries: a
// service that answers a write with the literal text "view: https://…" in its body must not be
// able to choose what a card links to.
func viewedLink(notes string) string {
	for _, line := range strings.Split(notes, "\n") {
		head, _, _ := strings.Cut(line, " Response: ")
		head, _, _ = strings.Cut(head, " Result: ")
		_, rest, ok := strings.Cut(head, " view: ")
		if !ok {
			continue
		}
		if link, _, _ := strings.Cut(rest, " "); isViewable(link) {
			return link
		}
	}
	return ""
}

func isViewable(s string) bool {
	if !strings.HasPrefix(s, "https://") {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && u.Host != "" && !strings.HasPrefix(u.Host, "api.")
}

// matchedConn is the connection the proxy would use for a request, for describing it.
func (a *Agent) matchedConn(c *Call, p ProxyRequest) *Connection {
	u, err := url.Parse(strings.TrimSpace(p.URL))
	if err != nil || c.Access == nil {
		return nil
	}
	method := strings.ToUpper(p.Method)
	if method == "" {
		method = "GET"
	}
	conn, _ := a.proxy.MatchNamed(c.Access, method, u, p.Connection)
	return conn
}

// ---- ClickUp list lookup ----

// A task needs a list id, and ClickUp only gives one up at the end of workspace → space →
// folder → list. Walked from the model that is four or five http_request rounds before the
// write even starts; walked here it is one tool call, and the answer is cached for the next one.
type clickupList struct{ ID, Name, Path string }

type cachedLists struct {
	at    time.Time
	lists []clickupList
}

const clickupListTTL = 10 * time.Minute

// getJSON runs a read through the proxy and decodes it, for tools that need several calls.
func (a *Agent) getJSON(ctx context.Context, c *Call, url string, out any) error {
	resp, err := a.proxy.Do(ctx, c.OrgID, c.Access, ProxyRequest{Method: "GET", URL: url, MaxBytes: proxyMaxRead},
		callAudit(c), true)
	if err != nil {
		return err
	}
	if resp.Status >= 400 {
		return fmt.Errorf("%s returned %d", url, resp.Status)
	}
	return json.Unmarshal([]byte(resp.Body), out)
}

func (a *Agent) clickupLists(ctx context.Context, c *Call, base string) ([]clickupList, error) {
	// Keyed by organisation as well as channel: a Slack Connect channel carries the same channel
	// id in both workspaces that share it, so channel-plus-base alone would serve one tenant's
	// ClickUp list tree to the other. The schema guards the same hazard on scopes.
	key := strconv.FormatInt(c.OrgID, 10) + "|" + c.TeamID + "|" + c.Channel + "|" + base
	a.listsMu.Lock()
	if hit, ok := a.lists[key]; ok && time.Since(hit.at) < clickupListTTL {
		a.listsMu.Unlock()
		return hit.lists, nil
	}
	a.listsMu.Unlock()

	var teams struct {
		Teams []struct{ ID, Name string } `json:"teams"`
	}
	if err := a.getJSON(ctx, c, base+"/api/v2/team", &teams); err != nil {
		return nil, err
	}
	type listRow struct {
		ID, Name string
	}
	out := []clickupList{}
	for _, team := range teams.Teams {
		var spaces struct {
			Spaces []struct{ ID, Name string } `json:"spaces"`
		}
		if err := a.getJSON(ctx, c, base+"/api/v2/team/"+team.ID+"/space?archived=false", &spaces); err != nil {
			continue
		}
		for _, space := range spaces.Spaces {
			var folderless struct {
				Lists []listRow `json:"lists"`
			}
			if a.getJSON(ctx, c, base+"/api/v2/space/"+space.ID+"/list?archived=false", &folderless) == nil {
				for _, l := range folderless.Lists {
					out = append(out, clickupList{ID: l.ID, Name: l.Name, Path: team.Name + " › " + space.Name + " › " + l.Name})
				}
			}
			var folders struct {
				Folders []struct {
					ID, Name string
					Lists    []listRow `json:"lists"`
				} `json:"folders"`
			}
			if a.getJSON(ctx, c, base+"/api/v2/space/"+space.ID+"/folder?archived=false", &folders) == nil {
				for _, f := range folders.Folders {
					for _, l := range f.Lists {
						out = append(out, clickupList{ID: l.ID, Name: l.Name,
							Path: team.Name + " › " + space.Name + " › " + f.Name + " › " + l.Name})
					}
				}
			}
		}
	}
	a.listsMu.Lock()
	if a.lists == nil {
		a.lists = map[string]cachedLists{}
	}
	a.lists[key] = cachedLists{at: time.Now(), lists: out}
	a.listsMu.Unlock()
	return out, nil
}

// clickupFindLists renders the lists whose name, folder or space matches, id first so the model
// can pass it straight to clickup_create_task.
func (a *Agent) clickupFindLists(ctx context.Context, c *Call, base, query string) (string, error) {
	lists, err := a.clickupLists(ctx, c, base)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	n := 0
	for _, l := range lists {
		if query != "" && !strings.Contains(strings.ToLower(l.Path), strings.ToLower(query)) {
			continue
		}
		fmt.Fprintf(&b, "%s\t%s\n", l.ID, l.Path)
		if n++; n >= 60 {
			break
		}
	}
	if n == 0 {
		if len(lists) == 0 {
			return "No lists came back: the token may not see any workspace.", nil
		}
		return fmt.Sprintf("No list matches %q. Call clickup_lists without a query to see all %d.", query, len(lists)), nil
	}
	return fmt.Sprintf("list_id\tpath (%d shown)\n%s", n, b.String()), nil
}

// ---- tool packs: thin named wrappers that make an open-weight model reliable ----

func packTool(a *Agent, name, desc string, params map[string]any, required []string, build func(args map[string]any) (ProxyRequest, error)) Tool {
	return Tool{Name: name, Desc: desc, Params: schema(params, required...),
		Run: func(ctx context.Context, c *Call, raw json.RawMessage) (string, error) {
			req, err := build(toolArgs(raw))
			if err != nil {
				return "", err
			}
			return a.proxied(ctx, c, req)
		}}
}

// packToolReq is packTool for a tool that has to see the turn before it can build its request —
// on GitHub, to resolve a repository to the connection holding its credential. packTool is the
// same thing for the majority that do not.
func packToolReq(a *Agent, name, desc string, params map[string]any, required []string, build func(c *Call, args map[string]any) (ProxyRequest, error)) Tool {
	return Tool{Name: name, Desc: desc, Params: schema(params, required...),
		Run: func(ctx context.Context, c *Call, raw json.RawMessage) (string, error) {
			args := toolArgs(raw)
			req, err := build(c, args)
			if err != nil {
				return "", err
			}
			return a.proxied(ctx, c, req)
		}}
}

// packToolFor is for a tool that is not one request: it fans out across repositories, or folds
// several calls into one answer, and returns the text itself.
func packToolFor(a *Agent, name, desc string, params map[string]any, required []string, run func(ctx context.Context, c *Call, args map[string]any) (string, error)) Tool {
	return Tool{Name: name, Desc: desc, Params: schema(params, required...),
		Run: func(ctx context.Context, c *Call, raw json.RawMessage) (string, error) {
			return run(ctx, c, toolArgs(raw))
		}}
}

// toolArgs decodes whatever the model sent, never returning nil: a tool whose arguments are all
// optional is called with no arguments at all often enough to be worth not writing twice.
func toolArgs(raw json.RawMessage) map[string]any {
	var args map[string]any
	json.Unmarshal(raw, &args)
	if args == nil {
		args = map[string]any{}
	}
	return args
}

func argS(m map[string]any, k string) string {
	if v, ok := m[k]; ok {
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprint(v)
	}
	return ""
}

func argI(m map[string]any, k string, def int) int {
	if v, ok := m[k].(float64); ok && v > 0 {
		return int(v)
	}
	return def
}

func jsonBody(v any) string { b, _ := json.Marshal(v); return string(b) }

// packs returns the tool pack for a preset, given the first allowed host of its connection.
// gcpMetricsRequest builds the Cloud Monitoring read. Named rather than inline because the
// aggregation half is the part that decides whether the answer fits in a tool result at all.
func gcpMetricsRequest(m map[string]any) (ProxyRequest, error) {
	end := time.Now().UTC()
	start := end.Add(-time.Duration(argI(m, "minutes", 60)) * time.Minute)
	q := url.Values{"filter": {argS(m, "filter")}, "interval.startTime": {start.Format(time.RFC3339)},
		"interval.endTime": {end.Format(time.RFC3339)}, "view": {"FULL"}}
	if al := argS(m, "aligner"); al != "" {
		q.Set("aggregation.perSeriesAligner", al)
		// An aligner with no period is ignored by the API, which is the silent version of the
		// problem this is here to fix. A reducer without an aligner is rejected outright, so it
		// only ever goes out alongside one.
		q.Set("aggregation.alignmentPeriod", cmp.Or(argS(m, "alignment_period"), "300s"))
		if rd := argS(m, "reducer"); rd != "" {
			q.Set("aggregation.crossSeriesReducer", rd)
		}
	}
	return ProxyRequest{Method: "GET",
		URL: "https://monitoring.googleapis.com/v3/projects/" + argS(m, "project_id") + "/timeSeries?" + q.Encode()}, nil
}

// clickupListID is what a ClickUp v2 list id looks like: digits, nothing else.
var clickupListID = regexp.MustCompile(`^[0-9]+$`)

// checkClickUpListID refuses an id that cannot be a list, and says what to do instead. The
// message is written to the model, because the model is who reads it and who can fix it.
func checkClickUpListID(id string) error {
	id = strings.TrimSpace(id)
	if clickupListID.MatchString(id) {
		return nil
	}
	if id == "" {
		return errors.New("list_id is required. Call clickup_lists to find the list and its id.")
	}
	return fmt.Errorf("%q is not a ClickUp list id — those are digits, like 900000000001. "+
		"An id with letters or a hyphen is a task's id, not a list's. Call clickup_lists to get "+
		"the right one, and do not guess it from a task URL.", truncate(id, 60))
}

func (a *Agent) packs(preset, host string) []Tool {
	base := "https://" + host
	switch preset {
	case "clickup":
		return []Tool{
			{Name: "clickup_lists", Desc: "Find ClickUp lists and their ids by name, searching every workspace, space and folder in one call. Use this to get a list_id — do not walk /team, /space and /folder yourself.",
				Params: schema(map[string]any{"query": str("Match a list, folder or space name (optional; omit for all)")}),
				Run: func(ctx context.Context, c *Call, raw json.RawMessage) (string, error) {
					var args map[string]any
					json.Unmarshal(raw, &args)
					return a.clickupFindLists(ctx, c, base, argS(args, "query"))
				}},
			packTool(a, "clickup_workspaces", "List ClickUp workspaces (teams) with ids.", map[string]any{}, nil,
				func(m map[string]any) (ProxyRequest, error) {
					return ProxyRequest{Method: "GET", URL: base + "/api/v2/team"}, nil
				}),
			packTool(a, "clickup_search_tasks", "Search tasks in a ClickUp workspace. Filter by status names, assignee ids, or free text.",
				map[string]any{"team_id": str("Workspace id from clickup_workspaces"), "statuses": str("Comma-separated status names (optional)"), "assignee": str("User id (optional)"), "include_closed": map[string]any{"type": "boolean"}, "page": num("Page number (0-based)")}, []string{"team_id"},
				func(m map[string]any) (ProxyRequest, error) {
					q := url.Values{"page": {fmt.Sprint(argI(m, "page", 0))}, "subtasks": {"true"}}
					for _, s := range strings.Split(argS(m, "statuses"), ",") {
						if s = strings.TrimSpace(s); s != "" {
							q.Add("statuses[]", s)
						}
					}
					if v := argS(m, "assignee"); v != "" {
						q.Add("assignees[]", v)
					}
					if v, _ := m["include_closed"].(bool); v {
						q.Set("include_closed", "true")
					}
					return ProxyRequest{Method: "GET", URL: base + "/api/v2/team/" + argS(m, "team_id") + "/task?" + q.Encode()}, nil
				}),
			packTool(a, "clickup_get_task", "Get one ClickUp task by id, including description, status, assignees and comments count.",
				map[string]any{"task_id": str("Task id like 86abc123")}, []string{"task_id"},
				func(m map[string]any) (ProxyRequest, error) {
					return ProxyRequest{Method: "GET", URL: base + "/api/v2/task/" + argS(m, "task_id")}, nil
				}),
			packToolReq(a, "clickup_create_task", "Create a ClickUp task in a list (write; needs human confirmation). Get the list id from clickup_lists. Files attached in this thread — a customer's screenshot, a log — are uploaded to the task once it is created, so do not ask for that and do not describe them as missing.",
				map[string]any{"list_id": str("List id from clickup_lists"), "name": str("Task title"), "description": str("Markdown description"), "priority": num("1 urgent … 4 low")}, []string{"list_id", "name"},
				func(c *Call, m map[string]any) (ProxyRequest, error) {
					// Checked here, where it costs a tool round to fix, rather than at the far end
					// of an approval. A task id put where a list id goes is answered by ClickUp
					// with a bare 400 — after somebody has read the card and pressed Approve, and
					// with nothing filed. The two are easy to confuse: a list id is digits, and
					// the id in a task's own URL (2kydrjut-334) is not.
					if err := checkClickUpListID(argS(m, "list_id")); err != nil {
						return ProxyRequest{}, err
					}
					body := map[string]any{"name": argS(m, "name"), "markdown_description": argS(m, "description")}
					if p := argI(m, "priority", 0); p > 0 {
						body["priority"] = p
					}
					// The evidence goes with the report. A ticket written from a mail whose whole
					// content is a screenshot is worth very little without it, and the approval
					// that files the ticket is the same approval that carries the picture.
					return ProxyRequest{Method: "POST", URL: base + "/api/v2/list/" + argS(m, "list_id") + "/task",
						Body: jsonBody(body), AttachFiles: attachableFiles(c)}, nil
				}),
		}
	case "github":
		// Every one of these resolves the repository to its own connection before calling, so
		// a question about acme/web is answered by acme/web's credential. The three that take
		// an explicit repo used to go out under whichever repository connection ranked first
		// for api.github.com, which is the right answer only when every repository shares a
		// token.
		return []Tool{
			packToolFor(a, "github_find_code", "Search the code itself across the repositories this channel can reach, e.g. 'parseTimeout' or 'func handleLogin'. Use this to find which repository something lives in. Only the default branch is searched, and only files under 384 KB.",
				map[string]any{"query": str("Words or an identifier to find in the code"), "repo": str("owner/name to search just one (optional; omit to search all of them)"), "limit": num("Max results (default 10)")}, []string{"query"},
				func(ctx context.Context, c *Call, m map[string]any) (string, error) {
					return a.githubFindCode(ctx, c, base, argS(m, "query"), argS(m, "repo"), argI(m, "limit", 10))
				}),
			packToolFor(a, "github_find_file", "Find files by name or path fragment across the repositories this channel can reach, e.g. 'auth/session' or 'Dockerfile'. Works where code search does not, and is the quickest way to tell which repository owns a feature.",
				map[string]any{"name": str("Part of a file name or path"), "repo": str("owner/name to look in just one (optional)"), "limit": num("Max results (default 20)")}, []string{"name"},
				func(ctx context.Context, c *Call, m map[string]any) (string, error) {
					return a.githubFindFile(ctx, c, base, argS(m, "name"), argS(m, "repo"), argI(m, "limit", 20))
				}),
			packToolFor(a, "github_read_file", "Read a file from a repository. Get the path from github_find_file or github_find_code first.",
				map[string]any{"path": str("Path within the repository, e.g. internal/auth/session.go"), "repo": str("owner/name (optional when the channel has one repository)"), "ref": str("Branch, tag or commit (optional; default branch otherwise)")}, []string{"path"},
				func(ctx context.Context, c *Call, m map[string]any) (string, error) {
					return a.githubReadFile(ctx, c, base, argS(m, "repo"), argS(m, "path"), argS(m, "ref"))
				}),
			packToolFor(a, "github_search", "Search issues and pull requests in the repositories this channel can reach, e.g. 'is:pr is:open label:bug'. The repositories are chosen by what this channel is granted; do not write repo: or org: in the query.",
				map[string]any{"query": str("Search terms and GitHub filters such as is:pr, is:open, label:bug"), "repo": str("owner/name to search just one (optional)"), "limit": num("Max results (default 10)")}, []string{"query"},
				func(ctx context.Context, c *Call, m map[string]any) (string, error) {
					return a.githubSearchIssues(ctx, c, base, argS(m, "query"), argS(m, "repo"), argI(m, "limit", 10))
				}),
			packToolReq(a, "github_get_pr", "Get a pull request: title, body, state, reviewers, changed files summary.",
				map[string]any{"repo": str("owner/name"), "number": num("PR number")}, []string{"repo", "number"},
				func(c *Call, m map[string]any) (ProxyRequest, error) {
					conn, err := repoConnection(c, argS(m, "repo"))
					if err != nil {
						return ProxyRequest{}, err
					}
					return ProxyRequest{Method: "GET", Connection: conn.Name,
						URL: fmt.Sprintf("%s/repos/%s/pulls/%d", base, conn.Repo, argI(m, "number", 0))}, nil
				}),
			packToolReq(a, "github_recent_commits", "List recent commits on a repo branch.",
				map[string]any{"repo": str("owner/name"), "branch": str("Branch (default the repository's own)"), "limit": num("Max (default 10)")}, []string{"repo"},
				func(c *Call, m map[string]any) (ProxyRequest, error) {
					conn, err := repoConnection(c, argS(m, "repo"))
					if err != nil {
						return ProxyRequest{}, err
					}
					u := fmt.Sprintf("%s/repos/%s/commits?per_page=%d", base, conn.Repo, argI(m, "limit", 10))
					if br := argS(m, "branch"); br != "" {
						u += "&sha=" + url.QueryEscape(br)
					}
					return ProxyRequest{Method: "GET", URL: u, Connection: conn.Name}, nil
				}),
			packToolReq(a, "github_comment", "Comment on an issue or pull request (write; needs human confirmation).",
				map[string]any{"repo": str("owner/name"), "number": num("Issue/PR number"), "body": str("Markdown comment")}, []string{"repo", "number", "body"},
				func(c *Call, m map[string]any) (ProxyRequest, error) {
					conn, err := repoConnection(c, argS(m, "repo"))
					if err != nil {
						return ProxyRequest{}, err
					}
					return ProxyRequest{Method: "POST", Connection: conn.Name,
						URL:  fmt.Sprintf("%s/repos/%s/issues/%d/comments", base, conn.Repo, argI(m, "number", 0)),
						Body: jsonBody(map[string]string{"body": argS(m, "body")})}, nil
				}),
		}
	case "gcp_logs":
		return []Tool{
			packTool(a, "gcp_query_logs", "Query Google Cloud Logging entries with a Logging filter, newest first.",
				map[string]any{"project_id": str("GCP project id"), "filter": str("Logging filter, e.g. resource.type=\"k8s_container\" AND severity>=ERROR"), "hours": num("Look back this many hours (default 1)"), "limit": num("Max entries (default 20, max 100)")}, []string{"project_id", "filter"},
				func(m map[string]any) (ProxyRequest, error) {
					hours := argI(m, "hours", 1)
					since := time.Now().Add(-time.Duration(hours) * time.Hour).UTC().Format(time.RFC3339)
					filter := argS(m, "filter") + fmt.Sprintf(` AND timestamp>="%s"`, since)
					return ProxyRequest{Method: "POST", URL: "https://logging.googleapis.com/v2/entries:list", Body: jsonBody(map[string]any{
						"resourceNames": []string{"projects/" + argS(m, "project_id")}, "filter": filter, "orderBy": "timestamp desc", "pageSize": min(argI(m, "limit", 20), 100)})}, nil
				}),
			// Unaggregated, this returns every raw point of every series: one revision times one
			// response class times a point a minute, and a plain hour of run.googleapis.com/
			// request_count came back at a quarter of a million characters -- of which the model
			// saw the first twelve thousand. It was truncated on every call in production, so the
			// aggregation arguments are the point of the tool rather than an advanced option:
			// asking Monitoring to align and reduce is the difference between 250k characters and
			// a dozen numbers, and it is the only way to ask for less that this endpoint has.
			packTool(a, "gcp_query_metrics", "Read a Cloud Monitoring time series (last N minutes). Aggregate unless you truly need raw points: pass aligner (ALIGN_RATE for a counter like request_count, ALIGN_MEAN for a gauge like memory or latency) with alignment_period, and reducer REDUCE_SUM or REDUCE_MEAN to collapse the per-revision series into one line. Without them a busy hour returns far more than can be read.",
				map[string]any{"project_id": str("GCP project id"), "filter": str("Monitoring filter, e.g. metric.type=\"run.googleapis.com/request_count\""), "minutes": num("Window in minutes (default 60)"),
					"alignment_period": str("Bucket size like 300s; fewer, wider buckets is fewer points to read"),
					"aligner":          str("How points inside a bucket combine: ALIGN_RATE, ALIGN_SUM, ALIGN_MEAN, ALIGN_MAX, ALIGN_PERCENTILE_99"),
					"reducer":          str("How series combine: REDUCE_SUM, REDUCE_MEAN, REDUCE_MAX. Needs aligner")}, []string{"project_id", "filter"},
				gcpMetricsRequest),
		}
	case "sentry":
		return []Tool{
			packTool(a, "sentry_issues", "List unresolved Sentry issues for an organization, optionally filtered by project or query.",
				map[string]any{"org": str("Organization slug"), "project": str("Project slug (optional)"), "query": str("Search, default is:unresolved"), "period": str("statsPeriod like 24h or 7d")}, []string{"org"},
				func(m map[string]any) (ProxyRequest, error) {
					q := url.Values{"query": {"is:unresolved"}, "statsPeriod": {"24h"}, "limit": {"25"}}
					if v := argS(m, "query"); v != "" {
						q.Set("query", v)
					}
					if v := argS(m, "period"); v != "" {
						q.Set("statsPeriod", v)
					}
					if v := argS(m, "project"); v != "" {
						q.Set("project", v)
					}
					return ProxyRequest{Method: "GET", URL: base + "/api/0/organizations/" + argS(m, "org") + "/issues/?" + q.Encode()}, nil
				}),
			packTool(a, "sentry_issue", "Get one Sentry issue and its latest event (stack trace summary).",
				map[string]any{"issue_id": str("Issue id")}, []string{"issue_id"},
				func(m map[string]any) (ProxyRequest, error) {
					return ProxyRequest{Method: "GET", URL: base + "/api/0/issues/" + argS(m, "issue_id") + "/events/latest/"}, nil
				}),
		}
	case "hubspot":
		return []Tool{
			packTool(a, "hubspot_search", "Search HubSpot CRM objects (contacts, companies or deals) by free text. It takes no filters and no sort: for property values or dates, call the search endpoint through http_request.",
				map[string]any{"object": map[string]any{"type": "string", "enum": []string{"contacts", "companies", "deals"}}, "query": str("Free-text search"), "limit": num("Max results (default 10)")}, []string{"object", "query"},
				func(m map[string]any) (ProxyRequest, error) {
					props := map[string][]string{"contacts": {"firstname", "lastname", "email", "company", "lifecyclestage"}, "companies": {"name", "domain", "industry", "numberofemployees"}, "deals": {"dealname", "amount", "dealstage", "closedate", "pipeline"}}
					obj := argS(m, "object")
					return ProxyRequest{Method: "POST", URL: base + "/crm/v3/objects/" + obj + "/search", Body: jsonBody(map[string]any{"query": argS(m, "query"), "limit": argI(m, "limit", 10), "properties": props[obj]})}, nil
				}),
		}
	}
	return nil
}

// mcpPreloadWindow is how far back a channel's tool calls count as evidence of what it is going
// to call next. Long enough that a weekly routine still looks like a habit, short enough that a
// server used once and abandoned stops being paid for on every turn.
const mcpPreloadWindow = 14 * 24 * time.Hour

// toolsFor assembles the per-call tool set: native tools, http_request, enabled packs, and
// use_connection standing in for MCP servers. MCP tool definitions are big (ClickUp's server
// lists 60-odd tools, about 20k tokens) so they are not sent up front: the model sees the
// connection names and loads one connection's tools when a request needs them. A message that
// names the connection, or a thread that already called its tools, loads them before the first
// model call and saves the extra round.
func (a *Agent) toolsFor(ctx context.Context, c *Call) map[string]Tool {
	out := map[string]Tool{}
	for k, v := range a.tools {
		out[k] = v
	}
	c.tools = out
	// Ahead of the Access check on purpose: what a quiet run may hold does not depend on the
	// channel's configuration, and a channel with none resolved must not be the one that keeps
	// the tools that post. create_artifact uploads a file into a thread that does not exist and
	// request_access DMs approvers a card pointing at the same place; in their place the run
	// gets the two tools that carry its whole decision.
	if c.offline() {
		delete(out, "create_artifact")
		delete(out, "request_access")
	}
	// And the same rule for the organisation that has nobody to ask: see approverIn.
	if !c.offline() && !a.approverIn(ctx, c.OrgID, c.SL) {
		delete(out, "request_access")
	}
	if c.Silent {
		for _, t := range a.quietDecisionTools() {
			out[t.Name] = t
		}
	}
	// A preview must not leave anything behind in the channel it is previewing. Reads are the
	// whole point of trying a channel's settings; writes to the shared memory are a side effect
	// nobody asked a test for, and the console says these two are off rather than letting the
	// run look like one that chose not to remember anything.
	//
	// Scheduling goes with them, and for a second reason as well as the first: a routine created
	// from the playground is real recurring work in the real channel, and the page is opened by
	// whoever may change a channel's settings — which is not the same permission as managing its
	// routines. Listing them stays; reading what a channel has is what a preview is for.
	if c.Preview {
		delete(out, "remember")
		delete(out, "forget")
		delete(out, "create_routine")
		delete(out, "delete_routine")
		// And no reactions: the playground is a page somebody is looking at, and a tick it put
		// on a real message in a real channel would outlive the page and be seen by the channel.
		delete(out, "react")
	}
	// A Teams workspace has no Slack to search, pin in, react on or list channels of. The tools
	// are withheld rather than left to fail: a tool that always errors costs a round to find that
	// out, every time, and teaches the model nothing. Reading a channel or a thread stays — those
	// read the conversation log the Teams transport keeps.
	if c.SL != nil && c.SL.Platform == platformMSTeams {
		for _, name := range slackOnlyTools {
			delete(out, name)
		}
	}
	// Nor artifacts where the platform takes no file from the bot: an artifact is a file uploaded
	// into the thread, and in Teams every call failed after the model had written the whole of it.
	if c.SL != nil && !c.SL.filesInThread() {
		delete(out, "create_artifact")
	}
	// A turn a forwarded email started. Everything in front of the model on one of these was
	// written by somebody outside the company, so the tools it does not get are the ones that
	// would let that text outlive the turn or reach past the thread: standing work, a line in
	// the system prompt of every later turn in this channel, a DM to somebody who never asked
	// to hear from it. What is left is reading the code, answering in this thread, and
	// proposing work a person still has to approve.
	//
	// remember is the sharp one: a shared memory is read into the prompt on every turn here,
	// which turns one mail into an instruction that keeps arriving.
	//
	// fetch_url and web_search go too: the mail's body is attacker-written, so leaving open web
	// egress lets an injected instruction read what the channel's credentials can reach and post
	// it out in a URL — the mail lane's job is to read the mail and the connected repositories,
	// not to reach a fresh host of the sender's choosing.
	if c.emailTurn() {
		delete(out, "remember")
		delete(out, "forget")
		delete(out, "create_routine")
		delete(out, "delete_routine")
		delete(out, "request_access")
		delete(out, "connect_account")
		delete(out, "send_dm")
		delete(out, "fetch_url")
		delete(out, "web_search")
	}
	// A scheduled run does not manage schedules. The routine tools exist for a person asking
	// in the channel, and a routine that can create or delete routines is a footgun on top of
	// being dead weight: create_routine alone is one of the largest definitions in the set, and
	// the whole list is re-sent on every round of every run, several times an hour, forever.
	if c.Kind == "routine" {
		delete(out, "create_routine")
		delete(out, "list_routines")
		delete(out, "delete_routine")
		// Nobody is in the room to ask how the bot works, so the manual is dead weight on every
		// round of every run.
		delete(out, "about_me")
		// And two it gets that nothing else does: a run whose findings are per-item can put each
		// one where it belongs — with the person it concerns, or in the thread the whole channel
		// can read — instead of finishing with one post that covers everybody.
		t := a.routineDMTool()
		out[t.Name] = t
		// The second only where there is something to reply under. A quiet run posts no header
		// until it has decided to speak and a preview posts none at all, so for them this is
		// create_artifact's problem above: a message addressed to a thread that does not exist.
		if !c.offline() {
			pt := a.routineThreadTool()
			out[pt.Name] = pt
		}
	}
	// A person's own notes belong to whoever is typing, so the tools for them exist only on a turn
	// that has one. personalKey is the single place that decides, and it says no to a routine, a
	// preview, a quiet run and anything attributed to the bot -- so unlike the block above there is
	// nothing to delete here, and a lane added later is excluded by not being a person rather than
	// by being remembered. Ahead of the Access check because none of this depends on what the
	// channel can reach: your own notes are yours in a channel with no connections at all.
	if k, ok := c.personalKey(); ok {
		// One indexed count, to decide whether this person is carrying one definition or three.
		// Somebody who has never saved a note has nothing to recall and nothing to forget.
		n, err := a.store.CountPersonalMemories(ctx, k)
		for _, t := range a.personalMemoryTools(k, err == nil && n > 0) {
			out[t.Name] = t
		}
	}
	// The digging lane is offered to the turns that might hand work over — an ask that reads
	// like a question about a cause — and not to every "thanks, that worked". Its definition is
	// paid for on every round of the turn that carries it, and a tool nobody in a conversation
	// would call is exactly the weight the first call is kept clear of. A run that is itself the
	// lane never gets it: an investigation that can queue an investigation queues them forever.
	if a.wantsDigging(ctx, c) {
		it := a.investigationTool(c)
		out[it.Name] = it
	}
	if c.Access == nil {
		return out
	}
	t := a.httpTool(c)
	out[t.Name] = t
	// run_js gains its fetch() paragraph here, where there is something for it to fetch from.
	// Only ever an upgrade of the one already registered: a deployment that did not register the
	// sandbox must not be handed it by a channel having connections.
	if _, ok := out["run_js"]; ok && a.sandboxFetch(c, nil) != nil {
		rt := a.reachingSandboxTool()
		out[rt.Name] = rt
	}
	for _, r := range c.Access.Rules {
		if r.Conn.CredType == "mcp" || !c.Access.ToolPacks[r.Conn.Preset] || len(r.Conn.AllowedHosts) == 0 {
			continue
		}
		for _, pt := range a.packs(r.Conn.Preset, r.Conn.AllowedHosts[0]) {
			if _, dup := out[pt.Name]; !dup {
				out[pt.Name] = pt
			}
		}
	}
	// Connecting an account is the person's own to do, so the tool that hands them the link is
	// offered wherever one of these is in reach — and in a channel that has not been given one,
	// wherever the organisation has one to connect at all. It sends a direct message, which is
	// why a quiet run and a preview do not get it: they have nobody to send one to.
	if c.canConnectAccount() && (c.Access.HasPersonal() || len(a.personalElsewhere(ctx, c)) > 0 || a.wantsPersonalSetup(ctx, c)) {
		ct := a.connectAccountTool()
		out[ct.Name] = ct
	}
	// The fix-job tool is offered only where a repository is connected, so channels without one
	// pay nothing for it on the first model call. A quiet run does not get it either: a job
	// posts its own checklist into the thread and reports there when it finishes.
	if a.jobs != nil && a.jobs.Enabled() && !c.offline() && len(c.Access.Repos()) > 0 {
		t := a.fixJobTool(c)
		out[t.Name] = t
	}
	conns := c.mcpConns()
	if len(conns) == 0 || c.NoTools {
		return out
	}
	u := a.useConnectionTool(conns)
	out[u.Name] = u
	var used []string
	if a.store != nil {
		used = a.store.ThreadToolNames(ctx, c.TeamID, c.Channel, c.ThreadTS)
		if len(used) == 0 {
			// A thread nobody has called a tool in yet, which every thread is once. The channel's
			// own recent history is the next best evidence of what this turn will reach for, and
			// acting on it here is worth more than the definitions cost: a connection loaded
			// later, by use_connection or by the model naming one of its tools, changes the tool
			// array mid-turn, and the array is in front of the system block in the prefix a
			// provider caches — so the round after the load re-reads the whole conversation at
			// full price, and the later in the turn it happens the more there is to re-read.
			//
			// Only when the thread has nothing. A thread that has been calling tools has already
			// said which ones it wants, and that answer beats the room's average.
			used = a.store.ChannelToolNames(ctx, c.TeamID, c.Channel, mcpPreloadWindow)
		}
	}
	for _, conn := range conns {
		if mentionsConn(c.Text, conn) || usedConn(used, conn) {
			if _, err := a.loadMCP(ctx, c, conn); err != nil {
				slog.Warn("mcp preload", "connection", conn.Name, "err", err)
			}
		}
	}
	return out
}

// ensureTools builds the call's tool set once per turn.
func (a *Agent) ensureTools(ctx context.Context, c *Call) map[string]Tool {
	if c.tools == nil {
		a.toolsFor(ctx, c)
	}
	return c.tools
}
