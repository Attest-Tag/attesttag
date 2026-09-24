package app

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// A file object arrives inside a Slack-signed envelope, but its address does not have to be
// Slack's: a remote file carries whatever url_private the app that registered it chose. The
// workspace bot token must not follow it there.

func TestSlackFileHostPinning(t *testing.T) {
	allowed := []string{
		"https://files.slack.com/files-pri/T1-F1/download/notes.pdf",
		"https://slack.com/files-pri/T1-F1/notes.pdf",
		"https://files-origin.slack.com/x",
		"https://FILES.SLACK.COM/x",  // case
		"https://files.slack.com./x", // trailing root dot
	}
	for _, raw := range allowed {
		u, err := url.Parse(raw)
		if err != nil || !slackFileHost(u) {
			t.Errorf("slackFileHost(%q) = false, want true", raw)
		}
	}
	refused := []string{
		"https://evil.com/steal",
		"https://files.slack.com.evil.com/steal", // suffix confusion
		"https://notslack.com/x",
		"https://slack.com.evil.io/x",
		"http://files.slack.com/x", // plaintext: the token would go out in the clear
		"https://169.254.169.254/computeMetadata/v1/",
		"file:///etc/passwd",
		"",
	}
	for _, raw := range refused {
		u, err := url.Parse(raw)
		if err == nil && slackFileHost(u) {
			t.Errorf("slackFileHost(%q) = true, want false", raw)
		}
	}
}

// The token is attached by the transport, per hop. A redirect that leaves Slack keeps being
// followed — Slack signs its own storage URLs — but arrives without the credential.
func TestSlackFileTokenStaysOnSlack(t *testing.T) {
	var offSlackAuth string
	var sawOffSlack bool
	offSlack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawOffSlack = true
		offSlackAuth = r.Header.Get("Authorization")
		w.Write([]byte("bytes"))
	}))
	defer offSlack.Close()

	var onSlackAuth string
	onSlack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		onSlackAuth = r.Header.Get("Authorization")
		http.Redirect(w, r, offSlack.URL+"/signed-object", http.StatusFound)
	}))
	defer onSlack.Close()

	// Pretend the first hop is Slack, and let the guarded dialler be replaced so loopback
	// test servers are reachable at all.
	prevBase := slackFileBaseTransport
	slackFileBaseTransport = func() http.RoundTripper { return http.DefaultTransport }
	defer func() { slackFileBaseTransport = prevBase }()

	prevHost := slackFileHostFn
	slackFileHostFn = func(u *url.URL) bool { return u != nil && u.Host == strings.TrimPrefix(onSlack.URL, "http://") }
	defer func() { slackFileHostFn = prevHost }()

	req, _ := http.NewRequest("GET", onSlack.URL+"/files-pri/T1-F1/download/notes.pdf", nil)
	resp, err := slackFileClient("xoxb-secret-token").Do(req)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	resp.Body.Close()

	if onSlackAuth != "Bearer xoxb-secret-token" {
		t.Errorf("Slack hop got %q, want the bot token", onSlackAuth)
	}
	if !sawOffSlack {
		t.Fatal("the redirect off Slack was not followed; a signed storage URL would fail to download")
	}
	if offSlackAuth != "" {
		t.Errorf("the bot token leaked off Slack: %q", offSlackAuth)
	}
}

// The real transport refuses a private address even when the host check passes, so a Slack
// hostname that resolved somewhere internal still could not be dialled.
func TestSlackFileTransportKeepsTheNetworkGuard(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the guarded transport dialled a loopback address")
	}))
	defer local.Close()

	prevHost := slackFileHostFn
	slackFileHostFn = func(u *url.URL) bool { return true }
	defer func() { slackFileHostFn = prevHost }()

	req, _ := http.NewRequest("GET", local.URL+"/x", nil)
	if _, err := slackFileClient("xoxb-secret-token").Do(req); err == nil {
		t.Fatal("the guarded transport connected to loopback")
	} else {
		t.Logf("refused: %v", err)
	}
}
