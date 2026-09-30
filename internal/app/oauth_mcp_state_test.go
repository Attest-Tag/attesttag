package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// An MCP connection's sign-in finishes only in the browser that started it. The state row says
// which organisation and connection the tokens are for; the cookie the start sets says which
// browser may bring a code back. A callback carrying the state without the cookie is refused
// before its code is exchanged, and the one that started it still completes.
func TestMCPSignInFinishesOnlyWhereItStarted(t *testing.T) {
	fixedMasterKey(t)
	t.Setenv("ADMIN_BASE_URL", "https://console.example.com")
	st := testStore(t)
	sealer, err := NewSealer()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	orgID, _, token := seedOrg(t, st, RoleAdmin)
	bd, err := st.CreateBundle(ctx, orgID, "tools", "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(&Secret{MCPURL: "https://mcp.example.com/mcp", OAuth: &OAuthState{ClientID: "cid",
		AuthURL: "https://as.example.com/authorize", TokenURL: "https://as.example.com/token"}})
	enc, _ := sealer.Seal(raw)
	connID, err := st.InsertConnection(ctx, orgID, &Connection{BundleID: bd.ID, Name: "Remote MCP", Preset: "mcp",
		CredType: "mcp", AllowedHosts: []string{"mcp.example.com"}, Status: "active"}, enc)
	if err != nil {
		t.Fatal(err)
	}

	var exchanged []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		exchanged = append(exchanged, r.Form.Get("code"))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"at-` + r.Form.Get("code") + `","refresh_token":"rt","expires_in":3600}`))
	}))
	defer provider.Close()
	saved := oauthHTTPClient
	oauthHTTPClient = &http.Client{Transport: rewriteTo(provider.URL)}
	defer func() { oauthHTTPClient = saved }()

	b := &Bot{store: st, sealer: sealer, mail: logMailer{}, settings: newSettingsCache(st, Config{}),
		resolver: NewResolver(st), proxy: NewProxy(sealer, st)}
	b.slacks = NewChatRegistry(st, sealer)
	b.agent = &Agent{mcp: newMCPHub(b.proxy)}
	mux := http.NewServeMux()
	b.routes(mux, nil)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/connections/"+strconv.FormatInt(connID, 10)+"/oauth/start", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+token)
	mux.ServeHTTP(w, r)
	var out struct {
		OK  bool
		URL string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || !out.OK {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	u, _ := url.Parse(out.URL)
	state := u.Query().Get("state")
	var bound *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == mcpOAuthStateCookie {
			bound = c
		}
	}
	if state == "" || bound == nil || bound.Value != state || !bound.HttpOnly || bound.Path != "/api/oauth/" {
		t.Fatalf("the start did not bind its state to this browser: state=%q cookie=%+v", state, bound)
	}
	callback := "/api/oauth/callback?state=" + url.QueryEscape(state) + "&code="

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", callback+"elsewhere", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("a callback without the start's cookie got %d, want 403", w.Code)
	}
	if len(exchanged) != 0 {
		t.Fatalf("a callback without the start's cookie had its code exchanged: %v", exchanged)
	}

	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", callback+"thecode", nil)
	r.AddCookie(bound)
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusFound || !strings.Contains(w.Header().Get("Location"), "connected="+strconv.FormatInt(connID, 10)) {
		t.Fatalf("the starting browser's callback: %d %s %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	conn, _ := st.Connection(ctx, orgID, connID)
	sec, err := b.proxy.secret(conn)
	if err != nil || sec.Token != "at-thecode" {
		t.Fatalf("stored token %q, %v", sec.Token, err)
	}
}
