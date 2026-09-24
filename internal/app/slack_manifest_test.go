package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The scope list has lived in three places kept in step by hand: botScopes here, the manifest a
// new deployment pastes into Slack, and the consent table on the marketing site. Getting it
// wrong is expensive in a particular way — Slack refuses the whole install with "Invalid
// permissions requested" and nothing connects, or worse, the install succeeds without a scope
// and the feature that needed it fails later, in a workspace, in front of somebody.
//
// This pins the manifest to the code. The site's copy is prose for humans and stays a manual
// job; two places, one of them checked, is the improvement.
func TestManifestAsksForTheScopesTheCodeAsksFor(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "slack", "manifest.json"))
	if err != nil {
		t.Fatalf("the manifest a new deployment pastes into Slack is missing: %v", err)
	}
	var m struct {
		OAuth struct {
			RedirectURLs []string `json:"redirect_urls"`
			Scopes       struct {
				Bot []string `json:"bot"`
			} `json:"scopes"`
		} `json:"oauth_config"`
		Settings struct {
			Events struct {
				RequestURL string `json:"request_url"`
			} `json:"event_subscriptions"`
			Interactivity struct {
				RequestURL string `json:"request_url"`
			} `json:"interactivity"`
			SocketMode bool `json:"socket_mode_enabled"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("the manifest is not valid JSON, so nobody can paste it: %v", err)
	}

	want := map[string]bool{}
	for _, s := range botScopes {
		want[s] = true
	}
	got := map[string]bool{}
	for _, s := range m.OAuth.Scopes.Bot {
		got[s] = true
	}
	for s := range want {
		if !got[s] {
			t.Errorf("the code asks Slack for %q and the manifest does not: an install will be missing it", s)
		}
	}
	for s := range got {
		if !want[s] {
			t.Errorf("the manifest asks for %q and the code does not: an unused permission nobody can justify at review", s)
		}
	}

	// Both redirect URLs, or half the product is broken in a way whose error message says
	// nothing useful: one is the workspace install, the other is Sign in with Slack.
	joined := strings.Join(m.OAuth.RedirectURLs, " ")
	for _, path := range []string{"/slack/oauth/callback", "/api/auth/callback"} {
		if !strings.Contains(joined, path) {
			t.Errorf("the manifest registers no redirect URL for %s: %v", path, m.OAuth.RedirectURLs)
		}
	}

	// The transport. Socket Mode swallows events silently — the app looks connected and answers
	// nothing — and there is no Socket Mode code left in this codebase.
	if m.Settings.SocketMode {
		t.Error("the manifest turns Socket Mode on; this bot is delivered over HTTP and would receive nothing")
	}
	if !strings.HasSuffix(m.Settings.Events.RequestURL, "/slack/events") {
		t.Errorf("events request URL = %q, want it to end in /slack/events", m.Settings.Events.RequestURL)
	}
	if !strings.HasSuffix(m.Settings.Interactivity.RequestURL, "/slack/interactions") {
		t.Errorf("interactivity request URL = %q, want it to end in /slack/interactions", m.Settings.Interactivity.RequestURL)
	}
}
