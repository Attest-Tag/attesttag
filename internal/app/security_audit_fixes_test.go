package app

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestC1_RequestAccessStripsInternalFields(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	role(t, st, "ops", 1, "93.184.216.34", "U0OPS00001")
	a := NewAgent(Config{Timezone: "UTC"}, nil, nil, st, nil, nil, NewProxy(nil, st), newSettingsCache(st, Config{}))
	c := &Call{TeamID: "T1", OrgID: orgID, Channel: "C1", ThreadTS: "1.1", UserID: "U_ASK", SL: &Chat{TeamID: "T1", OrgID: orgID}}

	// Attempt to smuggle mcp, tool, args, attach_files, job, conn through request_access.
	rawArgs := `{"what":"grant","why":"please","calls":[{"label":"safe","method":"PUT","url":"https://93.184.216.34/ok","mcp":99,"tool":"evil","args":{"x":1},"attach_files":["F1"],"job":{"repo":"org/repo"},"conn":99}]}`
	out, err := a.requestAccess(ctx, c, json.RawMessage(rawArgs))
	if err != nil {
		t.Fatalf("requestAccess: %v", err)
	}
	if !strings.Contains(out, "Recorded") {
		t.Fatalf("unexpected response: %s", out)
	}
	if len(c.pendingGrants) != 1 {
		t.Fatalf("expected 1 pending grant, got %d", len(c.pendingGrants))
	}
	var step grantStep
	if err := json.Unmarshal(c.pendingGrants[0], &step); err != nil {
		t.Fatal(err)
	}
	if step.MCP != 0 || step.Tool != "" || len(step.Args) != 0 || len(step.AttachFiles) != 0 || step.Job != nil {
		t.Fatalf("smuggled internal fields survived request_access: %+v", step)
	}
}

func TestC1_RunStepRefusesUnauthorizedMCPConnection(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	enc, _ := testSealer(t).Seal([]byte(`{"mcp_url":"https://mcp.example.com"}`))
	id, err := st.InsertConnection(ctx, orgID, &Connection{
		Name: "secret-mcp", CredType: "mcp", AllowedHosts: []string{"mcp.example.com"}, Status: "active",
	}, enc)
	if err != nil {
		t.Fatal(err)
	}
	b := &Bot{store: st}
	raw, _ := json.Marshal(map[string]any{"mcp": id, "tool": "admin_op"})
	// Empty Access must refuse the MCP connection.
	if _, _, err := b.runStep(ctx, nil, orgID, &Access{}, ProxyAudit{}, "U_APP", string(raw)); err == nil || !strings.Contains(err.Error(), "no longer allowed") {
		t.Fatalf("expected refusal for unauthorized MCP connection, got %v", err)
	}
}

func TestC2_CanReadBlocksBotFallbackAndPreviewPrivateChannel(t *testing.T) {
	a := &Agent{}
	privSL := &Chat{BotUserID: "UBOT", TeamID: "T1", OrgID: 1, t: &fakePrivTransport{}}

	// BotUserID as UserID must never pass private channel check.
	cBot := &Call{Channel: "C_PUB", UserID: "UBOT", SL: privSL}
	if err := a.canRead(context.Background(), cBot, "C_PRIV"); err == nil {
		t.Fatal("canRead allowed BotUserID to read private channel")
	}
	// Preview (Playground) must check membership even when channel == c.Channel.
	cPrev := &Call{Channel: "C_PRIV", UserID: "", Preview: true, SL: privSL}
	if err := a.canRead(context.Background(), cPrev, "C_PRIV"); err == nil {
		t.Fatal("canRead allowed unlinked Preview caller to read private channel")
	}
	// Preview caller who is NOT a member must be refused even on c.Channel.
	cPrevNonMember := &Call{Channel: "C_PRIV", UserID: "U_OUTSIDER", Preview: true, SL: privSL}
	if err := a.canRead(context.Background(), cPrevNonMember, "C_PRIV"); err == nil {
		t.Fatal("canRead allowed non-member Preview caller to read private channel")
	}
	// Preview caller who IS a member is allowed.
	cPrevMember := &Call{Channel: "C_PRIV", UserID: "U_MEMBER", Preview: true, SL: privSL}
	if err := a.canRead(context.Background(), cPrevMember, "C_PRIV"); err != nil {
		t.Fatalf("canRead refused member Preview caller: %v", err)
	}
}

type fakePrivTransport struct{ transport }

func (fakePrivTransport) conversation(context.Context, string) (string, convInfo, error) {
	return "secret", convInfo{IsPrivate: true}, nil
}

func (fakePrivTransport) members(context.Context, string) (map[string]bool, error) {
	return map[string]bool{"UBOT": true, "U_MEMBER": true}, nil
}

func TestC3_ConnectionHostValidationAndWideningGuard(t *testing.T) {
	for _, bad := range []string{"", "*", "*.", "*.com", "*example.com", "a.*.example.com", "evil.com/x", "a..b.com", "evil.com:443"} {
		if validAllowedHost(bad) {
			t.Errorf("validAllowedHost(%q) = true, want false", bad)
		}
	}
	for _, good := range []string{"api.example.com", "*.example.com", "*.sub.example.com", "127.0.0.1"} {
		if !validAllowedHost(good) {
			t.Errorf("validAllowedHost(%q) = false, want true", good)
		}
	}

	b := &Bot{}
	cur := &Connection{ID: 1, Name: "Stripe", Preset: "custom", CredType: "bearer", AllowedHosts: []string{"api.stripe.com"}, Status: "active"}
	// Widening hosts without re-entering secret must fail.
	_, _, err := b.buildConnection(&connectionInput{AllowedHosts: []string{"api.stripe.com", "evil.example.com"}}, cur)
	if err == nil || !strings.Contains(err.Error(), "re-entering") {
		t.Fatalf("expected secret re-entry error on host widening, got %v", err)
	}
	// Keeping the same hosts without re-entering secret succeeds.
	if _, _, err := b.buildConnection(&connectionInput{Name: "Stripe Renamed", AllowedHosts: []string{"api.stripe.com"}}, cur); err != nil {
		t.Fatalf("unchanged hosts update failed: %v", err)
	}
}

func TestH1_MCPOAuthCallbackRequiresStateCookie(t *testing.T) {
	st := testStore(t)
	b := &Bot{store: st, sealer: testSealer(t), proxy: NewProxy(testSealer(t), st)}
	mux := http.NewServeMux()
	b.oauthRoutes(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/oauth/callback?code=abc&state=xyz", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("GET /api/oauth/callback without state cookie returned %d, want 403", w.Code)
	}
}

func TestH2_SSOSessionOrgBinding(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	s := Settings{AuthPolicy: AuthPolicySSO}
	if !s.allowsViaForOrg("sso", 10, 10) {
		t.Fatal("SSO session for org 10 should be allowed in org 10")
	}
	if s.allowsViaForOrg("sso", 10, 20) {
		t.Fatal("SSO session for org 10 must be refused in org 20")
	}
	tok, err := st.NewLoginChallengeForOrg(ctx, 42, "sso", 10)
	if err != nil {
		t.Fatal(err)
	}
	via, ssoOrg, err := st.loginChallengeInfo(ctx, tok)
	if err != nil || via != "sso" || ssoOrg != 10 {
		t.Fatalf("loginChallengeInfo = (%q, %d, %v), want (sso, 10, nil)", via, ssoOrg, err)
	}
}

func TestH3_V1RoutineEditClearsAutoConfirmOnBehaviorChange(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	st.UpsertChannelScope(ctx, orgID, "T1", "C1", "#eng", false)
	b := &Bot{store: st, settings: newSettingsCache(st, Config{Timezone: "UTC"})}
	id, err := st.AddRoutine(ctx, Routine{
		OrgID: orgID, TeamID: "T1", Channel: "C1", Cron: "0 9 * * *", TZ: "UTC", Prompt: "original",
		Enabled: true, AutoConfirm: true, NextRun: "2099-01-01 09:00:00",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Pausing only preserves AutoConfirm.
	off := false
	if _, err := b.editRoutine(ctx, orgID, id, routineEdit{Enabled: &off, ClearAutoConfirmOnChange: true}); err != nil {
		t.Fatal(err)
	}
	if rt := b.routineByID(ctx, orgID, id); rt == nil || !rt.AutoConfirm {
		t.Fatalf("pausing routine cleared AutoConfirm unexpectedly: %+v", rt)
	}
	// Rewriting the prompt via /v1 must clear AutoConfirm.
	newPrompt := "delete everything"
	if _, err := b.editRoutine(ctx, orgID, id, routineEdit{Prompt: &newPrompt, ClearAutoConfirmOnChange: true}); err != nil {
		t.Fatal(err)
	}
	if rt := b.routineByID(ctx, orgID, id); rt == nil || rt.AutoConfirm {
		t.Fatalf("rewriting prompt via /v1 kept AutoConfirm=true: %+v", rt)
	}
}

func TestH4_NoToolsBlocksRunToolRaw(t *testing.T) {
	a := &Agent{tools: map[string]Tool{"echo": {Name: "echo", Run: func(context.Context, *Call, json.RawMessage) (string, error) {
		return "ran", nil
	}}}}
	c := &Call{NoTools: true}
	if out, ok := a.runToolRaw(context.Background(), c, "echo", `{}`); ok || !strings.Contains(out, "disabled") {
		t.Fatalf("runToolRaw allowed tool with NoTools=true: ok=%v out=%q", ok, out)
	}
}

func TestH5_AWSQueryProtocolMutatingGETNeedsConfirmAndApproval(t *testing.T) {
	awsConn := &Connection{ID: 1, Name: "AWS", CredType: "aws_sigv4", Writes: "confirm"}
	awsGrant := &Connection{ID: 2, Name: "AWS Grant", CredType: "aws_sigv4", Writes: "confirm", AllowGrants: true}

	for _, raw := range []string{
		"https://iam.amazonaws.com/?Action=CreateAccessKey&UserName=alice",
		"https://iam.amazonaws.com/?action=AttachUserPolicy&UserName=alice",
		"https://sts.amazonaws.com/?Action=GetSessionToken",
		"https://sts.amazonaws.com/?Action=AssumeRole&RoleArn=arn:aws:iam::123:role/admin",
		// A read first, then the write: the second Action must not ride through on the first.
		"https://ec2.us-east-1.amazonaws.com/?Action=DescribeInstances&Action=TerminateInstances",
	} {
		u, _ := url.Parse(raw)
		if !needsConfirm(awsConn, "GET", u) {
			t.Errorf("expected needsConfirm=true for %s", raw)
		}
		if !needsApproval(awsGrant, "GET", u) {
			t.Errorf("expected needsApproval=true for %s", raw)
		}
		if !needsConfirm(awsConn, "HEAD", u) {
			t.Errorf("expected needsConfirm=true for HEAD %s", raw)
		}
	}
	readURL, _ := url.Parse("https://ec2.us-east-1.amazonaws.com/?Action=DescribeInstances")
	if needsConfirm(awsConn, "GET", readURL) {
		t.Error("DescribeInstances should remain read-only")
	}
}

func TestH6_MCPLooksReadOnlyHardening(t *testing.T) {
	for _, readName := range []string{
		"get_issue", "get_message", "retrieve_block", "get_charge", "search_messages", "list_issues", "me", "look_up_user",
	} {
		if !mcpLooksReadOnly(readName) {
			t.Errorf("mcpLooksReadOnly(%q) = false, want true", readName)
		}
	}
	for _, writeName := range []string{"message_user", "issue_token", "block_user", "transfer_funds", "sign_document"} {
		if mcpLooksReadOnly(writeName) {
			t.Errorf("mcpLooksReadOnly(%q) = true, want false", writeName)
		}
	}
	for _, writeName := range []string{
		"direct_message", "kick_member", "ban_member", "listAndDelete",
		"getOrCreateUser", "resolve_incident", "query_database", "sendMessage",
	} {
		if mcpLooksReadOnly(writeName) {
			t.Errorf("mcpLooksReadOnly(%q) = true, want false", writeName)
		}
	}
	for _, readName := range []string{
		"me", "whoami", "get_user", "listIssues", "search_docs", "look_up_record",
	} {
		if !mcpLooksReadOnly(readName) {
			t.Errorf("mcpLooksReadOnly(%q) = false, want true", readName)
		}
	}
}

func TestH8_MaxHeldPerTurnAndM5AtomicTOTPAndM3MCPCodes(t *testing.T) {
	c := &Call{}
	for i := int64(1); i <= maxHeldPerTurn; i++ {
		if !c.canHoldMore() {
			t.Fatalf("expected room for hold %d", i)
		}
		c.holdForConfirm(i, "w")
	}
	if c.canHoldMore() {
		t.Fatal("canHoldMore() must be false after maxHeldPerTurn holds")
	}

	ctx := context.Background()
	st := testStore(t)
	if err := st.StartTOTP(ctx, 1, []byte("enc")); err != nil {
		t.Fatal(err)
	}
	if err := st.ConfirmTOTP(ctx, 1, 100); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.SpendTOTPStep(ctx, 1, 101); err != nil || !ok {
		t.Fatalf("first SpendTOTPStep failed: ok=%v err=%v", ok, err)
	}
	if ok, err := st.SpendTOTPStep(ctx, 1, 101); err != nil || ok {
		t.Fatalf("replayed SpendTOTPStep must return false, got ok=%v err=%v", ok, err)
	}

	// M3: RevokeAPIKeysOf spends every unexchanged mcp_code, so none can still be traded for a grant.
	_, err := st.db.ExecContext(ctx, `insert into mcp_codes (code_hash, org_id, user_id, client_id, redirect_uri, code_challenge, expires_at)
		values ('h1', ?, 7, 'c1', 'https://example.com/cb', 'ch', '2099-01-01 00:00:00')`, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeAPIKeysOf(ctx, orgID, 7); err != nil {
		t.Fatal(err)
	}
	var count int
	st.db.QueryRowContext(ctx, `select count(*) from mcp_codes where org_id=? and user_id=7 and used_at=''`, orgID).Scan(&count)
	if count != 0 {
		t.Fatalf("RevokeAPIKeysOf left %d pending mcp_codes", count)
	}
}

func TestM6_M7_M8_M9_M11_ProxyAndSSRFHardening(t *testing.T) {
	// M6: publicHTTPSURL rejects http://
	httpU, _ := url.Parse("http://example.com/oauth/token")
	if err := publicHTTPSURL(httpU); err == nil {
		t.Error("publicHTTPSURL must reject plain http://")
	}
	// M11: special IPv6 ranges blocked
	for _, addr := range []string{"::192.0.2.1", "64:ff9b:1::1", "fec0::1", "2002:c000:0201::1", "2001::1", "100::1"} {
		if publicIP(net.ParseIP(addr)) {
			t.Errorf("publicIP(%q) = true, want false", addr)
		}
	}
	// M11: hasDotSegment rejects \ and ; and pathMatchesPrefix enforces segment boundary
	for _, raw := range []string{"https://a.example/v1\\..\\admin", "https://a.example/v1;../admin", "https://a.example/v1%3badmin"} {
		u, _ := url.Parse(raw)
		if !hasDotSegment(u) {
			t.Errorf("hasDotSegment(%q) = false, want true", raw)
		}
	}
	if pathMatchesPrefix("/v1/items-admin", "/v1/items") {
		t.Error("pathMatchesPrefix matched /v1/items-admin against /v1/items")
	}
	if !pathMatchesPrefix("/v1/items/42", "/v1/items") || !pathMatchesPrefix("/v1/items:list", "/v1/items") {
		t.Error("pathMatchesPrefix rejected valid child or colon-verb path")
	}
	// M7: methodOverride HPP, case-insensitivity, and JSON unicode escape
	uHPP, _ := url.Parse("https://api.example.com/x?_METHOD=GET&_method=DELETE")
	if v, _ := methodOverride(ProxyRequest{Method: "GET"}, uHPP); v != "DELETE" {
		t.Errorf("methodOverride missed HPP query override: got %q", v)
	}
	if v, _ := methodOverride(ProxyRequest{Method: "POST", Body: `{"\u005fmethod":"DELETE"}`}, nil); v != "DELETE" {
		t.Errorf("methodOverride missed JSON unicode escape: got %q", v)
	}
	// M7: stripRepoScope removes parenthesised repo: and OR
	if got := stripRepoScope(`(repo:secret/private) foo OR bar`); strings.Contains(got, "repo:") || strings.Contains(got, "OR") {
		t.Errorf("stripRepoScope left bypass tokens: %q", got)
	}
	// M8: redact new token prefixes
	for _, tok := range []string{
		"ato1.abcdefghijklmnopqrstuvwxyz1234",
		"atr1.abcdefghijklmnopqrstuvwxyz1234",
		"atc1.abcdefghijklmnopqrstuvwxyz1234",
		"mcps_abcdefghijklmnopqrstuvwxyz1234",
		"ya29.a0AfH6SMBxabcdefghijklmnopqrstuvwxyz",
	} {
		if got := redact("token=" + tok); strings.Contains(got, tok) {
			t.Errorf("redact failed to mask %s: %q", tok, got)
		}
	}
	// M9: describeHTTPWriteDestination strips query & fragment
	dest := describeHTTPWriteDestination(nil, ProxyRequest{Method: "POST", URL: "https://api.example.com/tasks?note=APPROVED_BY_ADMIN#frag"})
	if strings.Contains(dest, "APPROVED_BY_ADMIN") || strings.Contains(dest, "frag") {
		t.Errorf("describeHTTPWriteDestination leaked query/fragment: %q", dest)
	}
	_ = time.Now()
}
