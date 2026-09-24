package app

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Every audit row has to say which workspace the call belonged to. Slack guarantees channel ids
// unique within a team and no further, so a row carrying only a channel cannot be traced back
// once an account has two workspaces connected. The identity used to be typed out by hand at
// each call site, and the paths the model itself drives — http_request and MCP tools — were the
// ones that left it out: a call was attributable only when a human had pressed Confirm.
func TestProxiedCallAuditNamesTheWorkspace(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	p := NewProxy(nil, st)
	call := &Call{TeamID: "T1", OrgID: orgID, Channel: "C1", ThreadTS: "111.1", UserID: "U1"}

	// A host nothing grants: refused before any network, and recorded — which is exactly the
	// row an admin goes looking for, so it has to name the workspace too.
	if _, err := p.Do(ctx, orgID, &Access{}, ProxyRequest{Method: "GET", URL: "https://example.com/x"}, callAudit(call), false); err == nil {
		t.Fatal("a host with no grant was allowed through")
	}
	rows, err := st.ProxyAudits(ctx, orgID, "", 10, false, "")
	if err != nil {
		t.Fatalf("audits: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 audit row, got %d", len(rows))
	}
	got := rows[0]
	if got.TeamID != "T1" || got.Channel != "C1" || got.ThreadTS != "111.1" || got.Requester != "U1" {
		t.Errorf("audit row lost the caller: team=%q channel=%q thread=%q requester=%q",
			got.TeamID, got.Channel, got.ThreadTS, got.Requester)
	}
	if got.BlockedJ == "" {
		t.Error("a refused call was recorded without saying why")
	}
}

// The MCP path had it worst: the audit row was built inside the hub from the connection alone,
// so a tool the model called recorded no workspace, no channel, no thread and no requester.
func TestMCPCallAuditNamesTheWorkspace(t *testing.T) {
	ctx := context.Background()
	ts := fakeMCPServer(t, "list_documents")
	hub, st, seal := mcpTestHub(t)
	a := &Agent{store: st, tools: map[string]Tool{}, mcp: hub, settings: newSettingsCache(st, Config{}),
		slacks: testRegistry(&Chat{}), loc: time.UTC}
	conn := &Connection{ID: 7, Name: "Acme MCP", CredType: "mcp", Writes: "auto", AllowedHosts: []string{"example.com"},
		secretEnc: seal(Secret{MCPURL: ts.URL + "/mcp", Token: "tok"})}
	c := &Call{TeamID: "T1", OrgID: orgID, Channel: "C1", ThreadTS: "111.1", UserID: "U1", Kind: "channel",
		Text: "list the documents", Session: &Session{}, Streamer: &Streamer{failed: true},
		Access: &Access{Rules: []Rule{{Conn: conn}}, ToolPacks: map[string]bool{}}}

	if out := a.runTool(ctx, c, "use_connection", `{"connection":"acme mcp"}`); !strings.Contains(out, "Loaded") {
		t.Fatalf("use_connection: %s", out)
	}
	if out := a.runTool(ctx, c, "acme_mcp_list_documents", `{}`); strings.Contains(out, "error") {
		t.Fatalf("tool call: %s", out)
	}

	rows, err := st.ProxyAudits(ctx, orgID, "", 10, false, "")
	if err != nil {
		t.Fatalf("audits: %v", err)
	}
	var mcpRow *ProxyAudit
	for i := range rows {
		if rows[i].Method == "MCP" && rows[i].Path == "list_documents" {
			mcpRow = &rows[i]
		}
	}
	if mcpRow == nil {
		t.Fatalf("no audit row for the MCP tool call: %+v", rows)
	}
	if mcpRow.TeamID != "T1" || mcpRow.Channel != "C1" || mcpRow.ThreadTS != "111.1" || mcpRow.Requester != "U1" {
		t.Errorf("MCP audit row lost the caller: team=%q channel=%q thread=%q requester=%q",
			mcpRow.TeamID, mcpRow.Channel, mcpRow.ThreadTS, mcpRow.Requester)
	}
}
