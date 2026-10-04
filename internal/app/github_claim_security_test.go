package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// GitHub lists, for a user token, every installation of the app on a repository the person can
// read. An organisation's members and outside collaborators therefore all "see" its installation,
// and seeing it used to be the whole proof: whoever got to /github/connect first with an
// installation nobody had claimed attached it to an attest_tag organisation of their own, and the
// installation's tokens — contents:write on every repository in it — and every pull request code
// review is sent went with it. These tests pin the claim to the account's owner, or to someone
// with admin on everything the installation covers.

// claimGitHub is the slice of GitHub the install flow talks to. repos is what the installation
// covers; admin is which of those the person signed in at GitHub administers; hidden is how many
// of them the person cannot see at all.
type claimGitHub struct {
	userID       int64
	installs     []ghInstallation
	repos        int
	admin        func(i int) bool
	hidden       int
	failUserList bool
}

func (f *claimGitHub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch {
		case r.Method == "POST" && r.URL.Path == "/login/oauth/access_token":
			json.NewEncoder(w).Encode(map[string]string{"access_token": "user-token"})
		case r.Method == "GET" && r.URL.Path == "/user/installations":
			json.NewEncoder(w).Encode(map[string]any{"installations": f.installs})
		case r.Method == "GET" && r.URL.Path == "/user":
			json.NewEncoder(w).Encode(map[string]any{"id": f.userID})
		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			var body struct {
				Permissions map[string]string `json:"permissions"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			tok := "installation-token"
			if len(body.Permissions) == 1 && body.Permissions["metadata"] == "read" {
				tok = "metadata-token"
			}
			json.NewEncoder(w).Encode(map[string]string{"token": tok, "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
		case r.Method == "GET" && r.URL.Path == "/installation/repositories":
			if auth == "Bearer metadata-token" {
				json.NewEncoder(w).Encode(map[string]any{"total_count": f.repos})
				return
			}
			// The repository listing connectInstalledRepos makes after a bind: nothing to file.
			json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "repositories": []any{}})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/user/installations/") && strings.HasSuffix(r.URL.Path, "/repositories"):
			if auth != "Bearer user-token" {
				t.Errorf("the installer's repositories were read with %q, not the installer's own token", auth)
			}
			if f.failUserList {
				w.WriteHeader(502)
				return
			}
			visible := f.repos - f.hidden
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
			var list []map[string]any
			for i := (page - 1) * per; i < page*per && i < visible; i++ {
				list = append(list, map[string]any{"full_name": fmt.Sprintf("acme/r%d", i),
					"permissions": map[string]bool{"admin": f.admin(i), "push": true, "pull": true}})
			}
			json.NewEncoder(w).Encode(map[string]any{"total_count": visible, "repositories": list})
		default:
			t.Errorf("unexpected call to GitHub: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	saved := oauthHTTPClient
	oauthHTTPClient = &http.Client{Transport: rewriteTo(srv.URL)}
	t.Cleanup(func() { oauthHTTPClient = saved })
	return srv
}

func testGitHubApp(t *testing.T) *githubApp {
	t.Helper()
	key, _, _ := testAppKey(t)
	return &githubApp{id: "1", slug: "attesttag-test", key: key, clientID: "cid", clientSecret: "csecret"}
}

func orgInstall(id int64, login string) ghInstallation {
	ins := ghInstallation{ID: id, RepositorySelection: "selected"}
	ins.Account.Login, ins.Account.ID, ins.Account.Type = login, id+1000, "Organization"
	return ins
}

func userInstall(id int64, login string, accountID int64) ghInstallation {
	ins := ghInstallation{ID: id, RepositorySelection: "all"}
	ins.Account.Login, ins.Account.ID, ins.Account.Type = login, accountID, "User"
	return ins
}

func TestMayClaimNeedsControlNotSight(t *testing.T) {
	all := func(int) bool { return true }
	cases := []struct {
		name string
		gh   claimGitHub
		ins  ghInstallation
		want error // nil, errMayNotClaim, errClaimTooLarge, or errAnyOther for "GitHub did not answer"
	}{
		{"the personal account itself", claimGitHub{userID: 77}, userInstall(5, "pat", 77), nil},
		{"a collaborator on somebody's personal repositories", claimGitHub{userID: 78}, userInstall(5, "pat", 77), errMayNotClaim},
		{"an organisation admin of everything it covers", claimGitHub{repos: 3, admin: all}, orgInstall(6, "acme"), nil},
		{"a member who can push but administers one repository fewer", claimGitHub{repos: 3, admin: func(i int) bool { return i != 2 }},
			orgInstall(6, "acme"), errMayNotClaim},
		// Admin on everything they can see is not admin on everything: a member who made their own
		// repository in the organisation administers it and may see nothing else.
		{"a member who administers what they see but cannot see it all", claimGitHub{repos: 3, hidden: 1, admin: all},
			orgInstall(6, "acme"), errMayNotClaim},
		{"an installation with no repositories proves nothing", claimGitHub{repos: 0, admin: all}, orgInstall(6, "acme"), errMayNotClaim},
		{"an owner of a large selection, read page by page", claimGitHub{repos: 250, admin: all}, orgInstall(6, "acme"), nil},
		{"one non-admin repository on a later page", claimGitHub{repos: 250, admin: func(i int) bool { return i != 240 }},
			orgInstall(6, "acme"), errMayNotClaim},
		{"too many repositories to check", claimGitHub{repos: 1001, admin: all}, orgInstall(6, "acme"), errClaimTooLarge},
		{"GitHub not answering is not a yes", claimGitHub{repos: 3, admin: all, failUserList: true}, orgInstall(6, "acme"), errAnyOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.gh.serve(t)
			err := testGitHubApp(t).mayClaim(context.Background(), &ghInstaller{token: "user-token"}, tc.ins)
			switch {
			case tc.want == nil && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want == errAnyOther:
				if err == nil || errors.Is(err, errMayNotClaim) || errors.Is(err, errClaimTooLarge) {
					t.Fatalf("err = %v, want a failure to reach GitHub", err)
				}
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

var errAnyOther = errors.New("any other error")

// The flow end to end: an attest_tag admin whose GitHub user is a plain member of "acme" comes back
// from GitHub with acme's unclaimed installation in view. It is not bound, nothing is minted for
// it later, and the answer says who can connect it. The account's own installation, in the same
// list, is bound.
func TestGitHubSetupBindsOnlyWhatTheInstallerControls(t *testing.T) {
	b, mux, st := installTestBot(t)
	ctx := context.Background()
	orgID, _, tok := seedOrg(t, st, RoleAdmin)

	gh := claimGitHub{userID: 77, repos: 4, admin: func(i int) bool { return i == 0 }, installs: []ghInstallation{
		orgInstall(9001, "acme"),         // the employer's: visible, not controlled
		userInstall(9002, "pat-dev", 77), // the person's own account
	}}
	gh.serve(t)
	g := testGitHubApp(t)
	b.ghApp = g
	b.proxy = NewProxy(b.sealer, st)
	b.proxy.ghApp = g

	setup := func(t *testing.T, installationID string) *httptest.ResponseRecorder {
		t.Helper()
		state, err := st.NewOAuthState(ctx, orgID, "admin@example.com", githubStateTTL)
		if err != nil {
			t.Fatal(err)
		}
		q := url.Values{"state": {state}, "code": {"the-code"}}
		if installationID != "" {
			q.Set("installation_id", installationID)
			q.Set("setup_action", "install")
		}
		r := httptest.NewRequest("GET", "/github/setup?"+q.Encode(), nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		r.AddCookie(&http.Cookie{Name: githubStateCookie, Value: state})
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}

	// The install-time redirect naming acme's installation: refused, and told why.
	w := setup(t, "9001")
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "github_error") || !strings.Contains(loc, "Only+an+owner+of+acme") {
		t.Fatalf("naming acme's installation answered %d %q, want a refusal naming who may connect it", w.Code, loc)
	}
	if g, _ := st.GitHubInstall(ctx, 9001); g != nil {
		t.Fatalf("acme's installation was bound by a member who only sees it: %+v", g)
	}

	// The authorize hop, which binds "everything they can see": only their own account.
	w = setup(t, "")
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "github_install=") {
		t.Fatalf("connect answered %d %q, want the bound redirect", w.Code, loc)
	}
	if g, _ := st.GitHubInstall(ctx, 9001); g != nil {
		t.Fatalf("connect bound acme's installation: %+v", g)
	}
	if g, _ := st.GitHubInstall(ctx, 9002); g == nil || g.OrgID != orgID {
		t.Fatalf("the person's own account was not bound: %+v", g)
	}
	if err := b.proxy.installBelongsTo(ctx, orgID, 9001); err == nil {
		t.Fatal("a token could still be minted for acme's installation")
	}

	// An owner of acme connects it; the member, coming back later, refreshes nothing they lack.
	gh.admin = func(int) bool { return true }
	w = setup(t, "9001")
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "github_install=9001") {
		t.Fatalf("an admin of every repository was refused: %d %q", w.Code, loc)
	}
	if g, _ := st.GitHubInstall(ctx, 9001); g == nil || g.OrgID != orgID {
		t.Fatalf("acme's installation was not bound for its admin: %+v", g)
	}
}

// One this organisation already holds is refreshed on a later visit without the proof: re-reading
// its account and permissions grants nothing new. One disconnected here has to be proved again.
func TestBindInstallationsAsksOnlyForANewClaim(t *testing.T) {
	b, _, st := installTestBot(t)
	ctx := context.Background()
	orgID, _, _ := seedOrg(t, st, RoleAdmin)
	otherOrg, _ := secondOrg(t, st)
	gh := claimGitHub{repos: 1, admin: func(int) bool { return true }}
	gh.serve(t)
	g := testGitHubApp(t)
	b.ghApp, b.proxy = g, NewProxy(b.sealer, st)
	b.proxy.ghApp = g

	seedInstall(t, st, orgID, 100, "ours")
	seedInstall(t, st, otherOrg, 200, "theirs")
	asked := map[int64]int{}
	no := func(_ context.Context, ins ghInstallation) error { asked[ins.ID]++; return errMayNotClaim }
	mine := []ghInstallation{orgInstall(100, "ours"), orgInstall(200, "theirs"), orgInstall(300, "new")}

	res := b.bindInstallations(ctx, orgID, mine, 0, "admin@example.com", no)
	if res.bound != 1 || asked[100] != 0 {
		t.Errorf("an installation this organisation holds: bound %d, asked %d times; want refreshed without asking", res.bound, asked[100])
	}
	if asked[200] != 0 || len(res.elsewhere) != 1 || res.elsewhere[0] != "theirs" {
		t.Errorf("another organisation's installation: asked %d, elsewhere %v; want reported without asking", asked[200], res.elsewhere)
	}
	if asked[300] != 1 || len(res.refused) != 1 || res.refused[0] != "new" {
		t.Errorf("an unclaimed installation: asked %d, refused %v; want asked and refused", asked[300], res.refused)
	}

	if err := st.RevokeGitHubInstall(ctx, orgID, 100, "uninstalled at GitHub"); err != nil {
		t.Fatal(err)
	}
	res = b.bindInstallations(ctx, orgID, mine[:1], 0, "admin@example.com", no)
	if res.bound != 0 || asked[100] != 1 {
		t.Errorf("an installation disconnected here: bound %d, asked %d times; want proved again", res.bound, asked[100])
	}
}
