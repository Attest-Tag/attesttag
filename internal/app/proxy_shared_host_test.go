package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The incident this file is about: a Drive service account granted to a channel, with no path
// prefixes, outranked the workspace's Google connection. Every Calendar call on
// www.googleapis.com went out under the service account's drive-only token, Google answered
// "insufficient authentication scopes", and the person reconnected their own account three
// times for a grant that was never the one in use.
func TestAPathBeatsAConnectionThatClaimsTheWholeHost(t *testing.T) {
	sa := &Connection{ID: 1, Name: "Google Drive", CredType: "gcp_sa", AllowedHosts: []string{"www.googleapis.com"}}
	google := &Connection{ID: 2, Name: "Google Workspace", CredType: "oauth_user", AllowedHosts: []string{"www.googleapis.com"},
		PathPrefixes: []string{"/calendar/v3", "/drive/v3"}}
	// The service account is granted closer in, so it ranks first — the arrangement that broke.
	acc := &Access{Rules: []Rule{{Conn: sa, Rank: 9}, {Conn: google, Rank: 1}}}
	p := NewProxy(nil, nil)

	for _, tc := range []struct {
		path, want string
		id         int64
	}{
		{"/calendar/v3/calendars/primary/events", "", 2}, // the calendar is the person's
		{"/drive/v3/files", "", 2},                       // and so is "my Drive"
		{"/customsearch/v1", "", 1},                      // a path only the host-wide one covers is still its
		{"/drive/v3/files", "Google Drive", 1},           // and a name still chooses
	} {
		u, _ := url.Parse("https://www.googleapis.com" + tc.path)
		conn, why := p.MatchNamed(acc, "GET", u, tc.want)
		if why != "" || conn == nil || conn.ID != tc.id {
			t.Errorf("%s (connection=%q) went to %+v (%s), want id %d", tc.path, tc.want, conn, why, tc.id)
		}
	}
}

// Where a shared credential and a person's own both claim the same path, "search my Drive" is
// the asker's — rank only says where each was granted.
func TestTheAskersOwnAccountBeatsASharedOneOnTheSamePath(t *testing.T) {
	sa := &Connection{ID: 1, Name: "Google Drive", CredType: "gcp_sa", AllowedHosts: []string{"www.googleapis.com"}, PathPrefixes: []string{"/drive/v3"}}
	google := &Connection{ID: 2, Name: "Google Workspace", CredType: "oauth_user", AllowedHosts: []string{"www.googleapis.com"}, PathPrefixes: []string{"/drive/v3"}}
	acc := &Access{Rules: []Rule{{Conn: sa, Rank: 9}, {Conn: google, Rank: 1}}}
	u, _ := url.Parse("https://www.googleapis.com/drive/v3/files?q=soc2")
	if conn, _ := NewProxy(nil, nil).MatchNamed(acc, "GET", u, ""); conn == nil || conn.ID != 2 {
		t.Errorf("picked %+v, want the personal connection", conn)
	}
}

// sharedHostFixture is the channel from the incident: the person's Google connection on
// Calendar and Drive, plus shared credentials beside it — one for Drive, one for the whole host.
// The upstream answers with whose token it was handed — the name, not the token, which the proxy
// would scrub from a shared connection's response.
func sharedHostFixture(t *testing.T) (*Proxy, *Access, *Store) {
	t.Helper()
	st, p, google := userConnFixture(t)
	ctx := context.Background()
	google.PathPrefixes = []string{"/calendar/v3", "/drive/v3"}
	shared := func(name, token string, prefixes []string) *Connection {
		enc, err := p.sealer.Seal(mustJSON(t, &Secret{Token: token}))
		if err != nil {
			t.Fatal(err)
		}
		id, err := st.InsertConnection(ctx, orgID, &Connection{BundleID: google.BundleID, Name: name, CredType: "bearer",
			AllowedHosts: []string{"www.googleapis.com"}, PathPrefixes: prefixes, Writes: "auto", Status: "active"}, enc)
		if err != nil {
			t.Fatal(err)
		}
		return mustConn(t, st, id)
	}
	drive := shared("Drive SA", "drive-sa-token", []string{"/drive/v3"})
	wide := shared("Everything SA", "wide-sa-token", nil)
	grant(t, st, p, google, "T1", "USAM", "sam-token", time.Now().Add(time.Hour))
	p.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		body := `{"who":"` + strings.TrimSuffix(tok, "-token") + `"}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})
	return p, &Access{Rules: []Rule{{Conn: wide, Rank: 9}, {Conn: drive, Rank: 9}, {Conn: google, Rank: 1}}}, st
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestProxyUsesTheAskersAccountAndSaysSo(t *testing.T) {
	p, acc, _ := sharedHostFixture(t)
	ctx := context.Background()
	for _, path := range []string{"/calendar/v3/calendars/primary/events", "/drive/v3/files"} {
		resp, err := p.Do(ctx, orgID, acc, ProxyRequest{Method: "GET", URL: "https://www.googleapis.com" + path},
			ProxyAudit{TeamID: "T1", Requester: "USAM"}, false)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if !strings.Contains(resp.Body, `"who":"sam"`) {
			t.Errorf("%s went out as %s, want the asker's own token", path, resp.Body)
		}
		if resp.Account != "USAM@example.com" || resp.Skipped != "" {
			t.Errorf("%s: account=%q skipped=%q", path, resp.Account, resp.Skipped)
		}
		if via := viaLine(resp); !strings.Contains(via, `"Google" as USAM@example.com`) {
			t.Errorf("%s: via line %q does not say whose account answered", path, via)
		}
	}
}

// Somebody who has not connected their own account can still be answered by a shared credential
// meant for the same path — labelled as that, so nobody reports a service account's shared
// folder as "your Drive". Not by one that merely covers the whole host, whose 403 would bury the
// Connect link, and never for a write, which done as somebody else is a different act.
func TestAnUnconnectedAskerFallsBackOnlyToAnEqualSharedRead(t *testing.T) {
	p, acc, _ := sharedHostFixture(t)
	ctx := context.Background()
	audit := ProxyAudit{TeamID: "T1", Requester: "UNEW"}

	resp, err := p.Do(ctx, orgID, acc, ProxyRequest{Method: "GET", URL: "https://www.googleapis.com/drive/v3/files"}, audit, false)
	if err != nil {
		t.Fatalf("drive read: %v", err)
	}
	if !strings.Contains(resp.Body, `"who":"drive-sa"`) || resp.Skipped != "Google" {
		t.Errorf("drive read went out as %s (skipped %q), want the Drive service account standing in", resp.Body, resp.Skipped)
	}
	if note := connNote(resp); !strings.Contains(note, "not their own account") || !strings.Contains(note, "Connect link") {
		t.Errorf("the stand-in was not labelled: %q", note)
	}

	if _, err := p.Do(ctx, orgID, acc, ProxyRequest{Method: "GET", URL: "https://www.googleapis.com/calendar/v3/calendars/primary/events"},
		audit, false); !errors.Is(err, ErrNeedsUserAuth) {
		t.Errorf("calendar read with only a host-wide stand-in: err=%v, want to be asked to connect", err)
	}
	if _, err := p.Do(ctx, orgID, acc, ProxyRequest{Method: "POST", URL: "https://www.googleapis.com/drive/v3/files", Body: "{}"},
		audit, true); !errors.Is(err, ErrNeedsUserAuth) {
		t.Errorf("drive write: err=%v, want to be asked to connect rather than write as the service account", err)
	}
	// Naming the personal connection is a choice, and a choice is not second-guessed.
	if _, err := p.Do(ctx, orgID, acc, ProxyRequest{Method: "GET", URL: "https://www.googleapis.com/drive/v3/files", Connection: "Google"},
		audit, false); !errors.Is(err, ErrNeedsUserAuth) {
		t.Errorf("named personal connection: err=%v, want to be asked to connect", err)
	}
}

// A scope refusal says whose credential it was, because only a personal grant is fixed by
// reconnecting — telling somebody to reconnect for a service account's missing scope is the
// loop this started with.
func TestScopeRefusalSaysWhoseGrantItIs(t *testing.T) {
	body := `{"error":{"code":403,"message":"Request had insufficient authentication scopes.","status":"PERMISSION_DENIED"}}`
	shared := &ProxyResponse{Status: 403, Body: body, Others: []string{"Google Workspace"},
		Conn: &Connection{Name: "Google Drive", CredType: "gcp_sa"}}
	if note := connNote(shared); !strings.Contains(note, "not the requester's grant") || !strings.Contains(note, `"Google Workspace"`) {
		t.Errorf("shared refusal note: %q", note)
	}
	own := &ProxyResponse{Status: 403, Body: body, Conn: &Connection{Name: "Google Workspace", CredType: "oauth_user"}}
	if note := connNote(own); !strings.Contains(note, "fresh Connect link") {
		t.Errorf("personal refusal note: %q", note)
	}
	plain := &ProxyResponse{Status: 403, Body: `{"error":"forbidden"}`, Conn: &Connection{Name: "ClickUp"}}
	if note := connNote(plain); note != "" {
		t.Errorf("a 403 that is not about scopes got a note: %q", note)
	}
}
