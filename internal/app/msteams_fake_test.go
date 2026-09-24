package app

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMicrosoft stands in for every Microsoft endpoint the Teams code talks to: the Bot Framework's
// OpenID metadata and signing keys, the Entra token endpoint, and a Bot Framework service URL that
// records what the bot posts and answers roster questions. Tokens it signs are real RS256 JWTs, over
// a key made for the test, so what is under test is the verification itself.
type fakeMicrosoft struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey
	kid string

	mu            sync.Mutex
	posts         []fakePost
	members       map[string]msMember       // by 29: id and by object id
	teams         map[string]fakeTeam       // by the Teams team id, which is its General channel's
	files         map[string]string         // what /files/{name} and, as "img:"+id, /v3/attachments serve
	downloadAuth  []string                  // every Authorization header a /files/ link was sent with
	codes         map[string]map[string]any // sign-in codes, each redeemable once for an id_token of these claims
	tokenRequests int
	keyFetches    int
	opened        int // one-to-one conversations the bot asked for
	next          int
	// refuse, when set, answers a post in place of the fake, the way Teams refuses one: a non-zero
	// status is sent with body and the post is not recorded.
	refuse func(msOutgoing) (int, string)
}

type fakePost struct {
	Method, Conversation, ID string
	Activity                 msOutgoing
}

// fakeTeam is a Teams team as the Bot Framework describes it to a bot installed in it: a name, and
// its channels by id and name, the General channel's name left out as Microsoft leaves it out.
type fakeTeam struct {
	Name     string
	Channels map[string]string
}

const fakeAppID = "11111111-2222-3333-4444-555555555555"

func newFakeMicrosoft(t *testing.T) *fakeMicrosoft {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeMicrosoft{t: t, key: key, kid: "test-key", members: map[string]msMember{}, teams: map[string]fakeTeam{},
		files: map[string]string{}, codes: map[string]map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /openid", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": botFrameworkIssuer, "jwks_uri": f.srv.URL + "/keys"})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.keyFetches++
		f.mu.Unlock()
		e := big.NewInt(int64(key.E)).Bytes()
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "kid": f.kid, "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(e),
			"endorsements": []string{"msteams", "webchat"},
		}}})
	})
	mux.HandleFunc("POST /{tenant}/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		f.tokenRequests++
		f.mu.Unlock()
		if r.Form.Get("client_secret") != "secret" {
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client", "error_description": "AADSTS7000215: Invalid client secret."})
			return
		}
		if r.Form.Get("grant_type") == "authorization_code" { // Sign in with Microsoft redeeming its code
			f.mu.Lock()
			claims, ok := f.codes[r.Form.Get("code")]
			delete(f.codes, r.Form.Get("code"))
			f.mu.Unlock()
			if !ok || r.Form.Get("code_verifier") == "" {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "AADSTS70008: The code has expired."})
				return
			}
			head, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
			body, _ := json.Marshal(claims)
			idToken := base64.RawURLEncoding.EncodeToString(head) + "." + base64.RawURLEncoding.EncodeToString(body) + ".c2ln"
			json.NewEncoder(w).Encode(map[string]any{"id_token": idToken, "access_token": "user-token", "token_type": "Bearer"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "bot-token", "expires_in": 3600, "token_type": "Bearer"})
	})
	mux.HandleFunc("POST /v3/conversations/{conv}/activities", func(w http.ResponseWriter, r *http.Request) {
		f.record(w, r, "POST", r.PathValue("conv"), "")
	})
	mux.HandleFunc("PUT /v3/conversations/{conv}/activities/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.record(w, r, "PUT", r.PathValue("conv"), r.PathValue("id"))
	})
	mux.HandleFunc("GET /v3/conversations/{conv}/members/{member}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		m, ok := f.members[r.PathValue("member")]
		f.mu.Unlock()
		if !ok {
			http.Error(w, `{"error":{"code":"MemberNotFoundInConversation"}}`, 404)
			return
		}
		json.NewEncoder(w).Encode(m)
	})
	mux.HandleFunc("GET /v3/teams/{team}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		team, ok := f.teams[r.PathValue("team")]
		f.mu.Unlock()
		if !ok {
			http.Error(w, `{"error":{"code":"NotFound"}}`, 404)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"id": r.PathValue("team"), "name": team.Name})
	})
	mux.HandleFunc("GET /v3/teams/{team}/conversations", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		team, ok := f.teams[r.PathValue("team")]
		f.mu.Unlock()
		if !ok {
			http.Error(w, `{"error":{"code":"NotFound"}}`, 404)
			return
		}
		list := []map[string]string{}
		for id, name := range team.Channels {
			list = append(list, map[string]string{"id": id, "name": name})
		}
		json.NewEncoder(w).Encode(map[string]any{"conversations": list})
	})
	// A OneDrive download link: pre-authorised, so it takes no credential and should be sent none.
	mux.HandleFunc("GET /files/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		body, ok := f.files[r.PathValue("name")]
		if a := r.Header.Get("Authorization"); a != "" {
			f.downloadAuth = append(f.downloadAuth, a)
		}
		f.mu.Unlock()
		if !ok {
			http.Error(w, "gone", 403)
			return
		}
		io.WriteString(w, body)
	})
	// The Bot Framework's attachment service, which serves a pasted image to the bot's token only.
	mux.HandleFunc("GET /v3/attachments/{id}/views/original", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer bot-token" {
			http.Error(w, "unauthorized", 401)
			return
		}
		f.mu.Lock()
		body, ok := f.files["img:"+r.PathValue("id")]
		f.mu.Unlock()
		if !ok {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		io.WriteString(w, body)
	})
	mux.HandleFunc("POST /v3/conversations", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.opened++
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]string{"id": "a:direct-chat"})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)

	oldOpenID, oldLogin, oldOK := botFrameworkOpenID, msLoginBase, msServiceURLOK
	oldDownload, oldInline := msDownloadHostOK, msInlineHostOK
	botFrameworkOpenID, msLoginBase = f.srv.URL+"/openid", f.srv.URL
	msServiceURLOK = func(u string) bool { return strings.HasPrefix(u, f.srv.URL) }
	msDownloadHostOK = func(u *url.URL) bool { return strings.HasPrefix(u.String(), f.srv.URL+"/files/") }
	msInlineHostOK = func(u *url.URL) bool { return strings.HasPrefix(u.String(), f.srv.URL+"/v3/attachments/") }
	t.Cleanup(func() {
		botFrameworkOpenID, msLoginBase, msServiceURLOK = oldOpenID, oldLogin, oldOK
		msDownloadHostOK, msInlineHostOK = oldDownload, oldInline
	})
	return f
}

func (f *fakeMicrosoft) record(w http.ResponseWriter, r *http.Request, method, conv, id string) {
	if r.Header.Get("Authorization") != "Bearer bot-token" {
		http.Error(w, "unauthorized", 401)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var act msOutgoing
	json.Unmarshal(raw, &act)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refuse != nil {
		if status, body := f.refuse(act); status != 0 {
			http.Error(w, body, status)
			return
		}
	}
	if id == "" {
		f.next++
		id = strconv.Itoa(1700000000000 + f.next)
	}
	f.posts = append(f.posts, fakePost{Method: method, Conversation: conv, ID: id, Activity: act})
	json.NewEncoder(w).Encode(map[string]string{"id": id})
}

// messages is every message the bot has sent so far, typing indicators left out.
func (f *fakeMicrosoft) messages() []fakePost {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []fakePost{}
	for _, p := range f.posts {
		if p.Activity.Type != "typing" {
			out = append(out, p)
		}
	}
	return out
}

// waitForMessages waits for the bot to have sent at least n messages; turns run on goroutines.
func (f *fakeMicrosoft) waitForMessages(n int) []fakePost {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got := f.messages(); len(got) >= n {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.t.Fatalf("the bot sent %d messages, want at least %d: %+v", len(f.messages()), n, f.messages())
	return nil
}

func (f *fakeMicrosoft) addMember(m msMember) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members[m.ID] = m
	f.members[m.AADObjectID] = m
}

// sign makes a Bot Framework-shaped token with claims over the defaults, which a nil value
// removes.
func (f *fakeMicrosoft) sign(over map[string]any) string {
	now := time.Now()
	claims := map[string]any{
		"iss": botFrameworkIssuer, "aud": fakeAppID, "serviceurl": f.srv.URL,
		"exp": now.Add(5 * time.Minute).Unix(), "nbf": now.Add(-time.Minute).Unix(),
	}
	for k, v := range over {
		if v == nil {
			delete(claims, k)
			continue
		}
		claims[k] = v
	}
	head, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": f.kid, "typ": "JWT"})
	body, _ := json.Marshal(claims)
	signing := base64.RawURLEncoding.EncodeToString(head) + "." + base64.RawURLEncoding.EncodeToString(body)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		f.t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// client is a Teams client for fakeAppID that talks to the fake, not to Microsoft.
func (f *fakeMicrosoft) client(st *Store) *msteamsClient {
	hc := f.srv.Client()
	return &msteamsClient{
		appID: fakeAppID, secret: "secret", homeTenant: "home-tenant", appType: msteamsSingleTenant,
		http: hc, store: st,
		verifier: &bfVerifier{appID: fakeAppID, http: hc},
		tokens:   &msTokens{appID: fakeAppID, secret: "secret", http: hc},
	}
}
