package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

const testSlackSigningSecret = "test-signing-secret-not-a-real-credential"

func signedSlackRequest(path, body string, at time.Time) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	timestamp := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(testSlackSigningSecret))
	fmt.Fprintf(mac, "v0:%s:%s", timestamp, body)
	r.Header.Set("X-Slack-Request-Timestamp", timestamp)
	r.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	r.Header.Set("Content-Type", "application/json")
	if path == "/slack/interactions" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return r
}

func slackHTTPTestBot(t *testing.T) (*Bot, *http.ServeMux) {
	t.Helper()
	b, mux, st := identityBot(t)
	b.cfg.SlackSigningSecret = testSlackSigningSecret
	for i, team := range []string{"T_A", "T_B"} {
		if err := st.SaveTeam(context.Background(), &Team{TeamID: team, OrgID: int64(i + 1), Name: team, Status: "active"}, []byte("unused")); err != nil {
			t.Fatal(err)
		}
	}
	return b, mux
}

func eventBody(id, team, kind string) string {
	return fmt.Sprintf(`{"type":"event_callback","event_id":%q,"team_id":%q,"event":{"type":%q}}`, id, team, kind)
}

func TestSlackHTTPChallengeAndSignature(t *testing.T) {
	_, mux := slackHTTPTestBot(t)
	body := `{"type":"url_verification","challenge":"verified"}`
	for _, tc := range []struct {
		name   string
		modify func(*http.Request)
		want   int
	}{
		{"valid", func(r *http.Request) {}, 200},
		{"unsigned", func(r *http.Request) { r.Header.Del("X-Slack-Signature") }, 401},
		{"forged", func(r *http.Request) { r.Header.Set("X-Slack-Signature", "v0="+strings.Repeat("0", 64)) }, 401},
		{"wrong version", func(r *http.Request) {
			r.Header.Set("X-Slack-Signature", strings.Replace(r.Header.Get("X-Slack-Signature"), "v0=", "v1=", 1))
		}, 401},
		{"invalid timestamp", func(r *http.Request) { r.Header.Set("X-Slack-Request-Timestamp", "not-a-time") }, 401},
		{"body tampering", func(r *http.Request) { r.Body = httptest.NewRequest("POST", "/", strings.NewReader(body+" ")).Body }, 401},
		{"cookie is not authentication", func(r *http.Request) {
			r.Header.Del("X-Slack-Signature")
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "anything"})
		}, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := signedSlackRequest("/slack/events", body, time.Now())
			tc.modify(r)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("%d: %s", w.Code, w.Body.String())
			}
			if tc.want == 200 && !strings.Contains(w.Body.String(), `"challenge":"verified"`) {
				t.Fatal(w.Body.String())
			}
		})
	}
	for _, at := range []time.Time{time.Now().Add(-6 * time.Minute), time.Now().Add(6 * time.Minute)} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, signedSlackRequest("/slack/events", body, at))
		if w.Code != 401 {
			t.Fatalf("expired/future signature accepted: %d", w.Code)
		}
	}
}

func TestSlackHTTPRejectsMalformedAndOversizedPayloads(t *testing.T) {
	_, mux := slackHTTPTestBot(t)
	for _, body := range []string{`{`, `{"type":"url_verification"}`, `{"type":"event_callback","event_id":"E1","team_id":"T_A","event":null}`, eventBody("", "T_A", "message"), eventBody("E1", "", "message")} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, signedSlackRequest("/slack/events", body, time.Now()))
		if w.Code != 400 {
			t.Errorf("%s: %d", body, w.Code)
		}
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, signedSlackRequest("/slack/events", strings.Repeat("x", slackBodyLimit+1), time.Now()))
	if w.Code != 413 {
		t.Fatalf("oversize: %d", w.Code)
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/slack/events", nil))
	if w.Code != 405 {
		t.Fatalf("method: %d", w.Code)
	}
}

func TestSlackHTTPDurableReceiptRetryAndTenantRouting(t *testing.T) {
	b, mux := slackHTTPTestBot(t)
	body := `{"type":"event_callback","event_id":"E1","team_id":"T_B","is_ext_shared_channel":true,"authorizations":[{"team_id":"T_A"}],"event":{"type":"app_uninstalled"}}`
	for range 2 {
		r := signedSlackRequest("/slack/events", body, time.Now())
		r.Header.Set("X-Slack-Retry-Num", "1")
		ctx, cancel := context.WithCancel(r.Context())
		r = r.WithContext(ctx)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		cancel()
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	var count int
	var enc []byte
	if err := b.store.db.QueryRow(`select count(*) from slack_deliveries`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := b.store.db.QueryRow(`select payload_enc from slack_deliveries limit 1`).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	if count != 1 || strings.Contains(string(enc), "app_uninstalled") {
		t.Fatal("retry not deduplicated or plaintext persisted")
	}
	// No dispatch happened on the HTTP handler; cancellation after ACK must not
	// cancel work retrieved by the background dispatcher.
	a, _ := b.store.Team(context.Background(), "T_A")
	if a.Status != "active" {
		t.Fatal("processed before dispatch")
	}
	if !b.dispatchNextSlackDelivery(context.Background()) {
		t.Fatal("not dispatched")
	}
	a, _ = b.store.Team(context.Background(), "T_A")
	z, _ := b.store.Team(context.Background(), "T_B")
	if a.Status != "revoked" || z.Status != "active" {
		t.Fatalf("wrong tenant: A=%s B=%s", a.Status, z.Status)
	}
	if b.dispatchNextSlackDelivery(context.Background()) {
		t.Fatal("duplicate dispatch")
	}
	if err := b.store.db.QueryRow(`select payload_enc from slack_deliveries`).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	if len(enc) != 0 {
		t.Fatal("completed payload retained")
	}
}

func TestSlackHTTPUnknownAndAmbiguousTeams(t *testing.T) {
	b, mux := slackHTTPTestBot(t)
	for _, tc := range []struct {
		body string
		want int
	}{
		{eventBody("E1", "T_UNKNOWN", "message"), 200},
		{`{"type":"event_callback","event_id":"E2","team_id":"T_B","is_ext_shared_channel":true,"event":{"type":"message"}}`, 400},
		{eventBody("E3", "T_A", "future_event"), 200},
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, signedSlackRequest("/slack/events", tc.body, time.Now()))
		if w.Code != tc.want {
			t.Fatalf("%d: %s", w.Code, w.Body.String())
		}
	}
	var n int
	b.store.db.QueryRow(`select count(*) from slack_deliveries`).Scan(&n)
	if n != 0 {
		t.Fatalf("queued %d unknown/ambiguous events", n)
	}
	if got := teamOf(json.RawMessage(`{"is_ext_shared_channel":true}`), slackevents.EventsAPIEvent{TeamID: "T_B"}); got != "" {
		t.Fatal(got)
	}
}

func TestSlackHTTPInteractionFormAndReplay(t *testing.T) {
	b, mux := slackHTTPTestBot(t)
	payload := `{"type":"block_actions","team":{"id":"T_A"},"user":{"id":"U_A"},"trigger_id":"press-1","actions":[{"type":"button","block_id":"confirm-actions","action_id":"attest_confirm","value":"12|1.2"}]}`
	body := url.Values{"payload": {payload}}.Encode()
	for range 2 {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, signedSlackRequest("/slack/interactions", body, time.Now()))
		if w.Code != 200 || w.Body.Len() != 0 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	d, err := b.store.claimSlackDelivery(context.Background())
	if err != nil || d == nil {
		t.Fatalf("%v %v", d, err)
	}
	plain, err := b.sealer.Open(d.Payload)
	if err != nil || string(plain) != payload || d.Team != "T_A" || d.Kind != "interaction" {
		t.Fatalf("bad payload: %v", err)
	}
	if next, _ := b.store.claimSlackDelivery(context.Background()); next != nil {
		t.Fatal("replayed interaction")
	}
	for _, invalid := range []string{"payload=%", "payload={", "payload=null", url.Values{"payload": {payload, payload}}.Encode(), url.Values{"payload": {`{"type":"block_actions","team":null,"user":{"id":"U_A"}}`}}.Encode()} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, signedSlackRequest("/slack/interactions", invalid, time.Now()))
		if w.Code != 400 {
			t.Fatalf("malformed form accepted: %q %d", invalid, w.Code)
		}
	}
	r := signedSlackRequest("/slack/interactions", body, time.Now())
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 415 {
		t.Fatal(w.Code)
	}
}

func TestSlackHTTPBackpressureAndRetryAfterRecovery(t *testing.T) {
	b, mux := slackHTTPTestBot(t)
	for i := range slackTeamPendingLimit {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, signedSlackRequest("/slack/events", eventBody(fmt.Sprint(i), "T_A", "app_uninstalled"), time.Now()))
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	for _, tc := range []struct {
		id, team string
		want     int
	}{{"0", "T_A", 200}, {"overflow", "T_A", 503}, {"other-tenant", "T_B", 200}} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, signedSlackRequest("/slack/events", eventBody(tc.id, tc.team, "app_uninstalled"), time.Now()))
		if w.Code != tc.want {
			t.Fatalf("%s: %d", tc.id, w.Code)
		}
	}
	d, err := b.store.claimSlackDelivery(context.Background())
	if err != nil || d == nil {
		t.Fatal(err)
	}
	if err := b.store.finishSlackDelivery(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, signedSlackRequest("/slack/events", eventBody("overflow", "T_A", "app_uninstalled"), time.Now()))
	if w.Code != 200 {
		t.Fatal("rejected request left a dedup tombstone", w.Code)
	}
}

func TestSlackHTTPMissingSecretAndDatabaseFailure(t *testing.T) {
	b, mux := slackHTTPTestBot(t)
	b.cfg.SlackSigningSecret = ""
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, signedSlackRequest("/slack/events", `{"type":"url_verification","challenge":"x"}`, time.Now()))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	b.cfg.SlackSigningSecret = testSlackSigningSecret
	b.store.Close()
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, signedSlackRequest("/slack/events", eventBody("E1", "T_A", "app_uninstalled"), time.Now()))
	if w.Code != 503 {
		t.Fatalf("acknowledged failed persistence: %d", w.Code)
	}
}

func TestSlackDeliveryRestartLeaseAndConcurrentClaim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.db")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.enqueueSlackDelivery(ctx, "event:T_A:E1", 1, "T_A", "event", []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var wg sync.WaitGroup
	claims := make(chan *slackDelivery, 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := st.claimSlackDelivery(ctx)
			if err != nil {
				errs <- err
			}
			if d != nil {
				claims <- d
			}
		}()
	}
	wg.Wait()
	close(claims)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if len(claims) != 1 {
		t.Fatalf("%d concurrent claims", len(claims))
	}
	old := <-claims
	st.db.Exec(`update slack_deliveries set lease_until=0`)
	d, err := st.claimSlackDelivery(ctx)
	if err != nil || d == nil {
		t.Fatal("expired lease not recoverable", err)
	}
	if err := st.finishSlackDelivery(ctx, old); err != nil {
		t.Fatal(err)
	}
	var done int64
	st.db.QueryRow(`select done_at from slack_deliveries`).Scan(&done)
	if done != 0 {
		t.Fatal("stale claimant finished new lease")
	}
	if err := st.finishSlackDelivery(ctx, d); err != nil {
		t.Fatal(err)
	}
	if next, _ := st.claimSlackDelivery(ctx); next != nil {
		t.Fatal("completed delivery reclaimed")
	}
}

func TestSlackActionOutlivesDispatch(t *testing.T) {
	b, mux := slackHTTPTestBot(t)
	t.Setenv("ALLOWED_EMAIL_DOMAINS", "")
	// Exercise a real Cancel action, delaying the outbound Slack response until
	// both the request and delivery contexts have ended.
	started, release, complete := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "users.info") {
			// The access gate asks who is pressing before anything else (mayUseBot); answer at
			// once so the delayed call below is the outbound response this test is about.
			fmt.Fprint(w, `{"ok":true,"user":{"id":"U_A","team_id":"T_A","profile":{"email":"a@example.com"}}}`)
			return
		}
		close(started)
		<-release
		fmt.Fprint(w, `{"ok":true,"channel":"C_A","ts":"1.2"}`)
		close(complete)
	}))
	defer func() { unblock(); srv.Close() }()
	b.slacks.Put("T_A", &Chat{t: &slackTransport{api: slack.New("fake", slack.OptionAPIURL(srv.URL+"/"))}, TeamID: "T_A", OrgID: 1})
	payload := `{"type":"block_actions","team":{"id":"T_A"},"user":{"id":"U_A"},"trigger_id":"cancel-press","channel":{"id":"C_A"},"container":{"message_ts":"1.2"},"actions":[{"type":"button","block_id":"confirm-actions","action_id":"attest_cancel","value":"12|1.2"}]}`
	r := signedSlackRequest("/slack/interactions", url.Values{"payload": {payload}}.Encode(), time.Now())
	ctx, cancel := context.WithCancel(r.Context())
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	cancel()
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	if !b.dispatchNextSlackDelivery(context.Background()) {
		t.Fatal("not dispatched")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("action canceled with delivery context")
	}
	unblock()
	select {
	case <-complete:
	case <-time.After(2 * time.Second):
		t.Fatal("action did not finish")
	}
}

func TestSlackDeliveryGlobalLimitAndReceiptCleanup(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	for i := range slackPendingLimit {
		if err := st.enqueueSlackDelivery(ctx, fmt.Sprint(i), int64(i/slackTeamPendingLimit)+1, fmt.Sprintf("T%d", i/slackTeamPendingLimit), "event", []byte("sealed")); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.enqueueSlackDelivery(ctx, "overflow", int64(slackPendingLimit/slackTeamPendingLimit)+1, "T_NEW", "event", []byte("sealed")); err == nil {
		t.Fatal("global limit bypassed")
	}
	d, err := st.claimSlackDelivery(ctx)
	if err != nil || d == nil {
		t.Fatal(err)
	}
	if err := st.finishSlackDelivery(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := st.purgeSlackDeliveries(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	st.db.QueryRow(`select count(*) from slack_deliveries where delivery_key=?`, d.Key).Scan(&n)
	if n != 1 {
		t.Fatal("fresh retry receipt deleted")
	}
	st.db.Exec(`update slack_deliveries set done_at=? where delivery_key=?`, time.Now().Add(-25*time.Hour).UnixNano(), d.Key)
	if err := st.purgeSlackDeliveries(ctx); err != nil {
		t.Fatal(err)
	}
	st.db.QueryRow(`select count(*) from slack_deliveries`).Scan(&n)
	if n != slackPendingLimit-1 {
		t.Fatal("cleanup deleted pending work or kept expired receipt", n)
	}
}
