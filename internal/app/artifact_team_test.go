package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

// fakeSlackUploads answers the three calls files.upload.v2 makes, plus the files.info lookup
// that turns the id into a permalink. It cares about nothing but shape.
func fakeSlackUploads(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/files.getUploadURLExternal"):
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "file_id": "F1",
				"upload_url": "http://" + r.Host + "/put"})
		case strings.HasSuffix(r.URL.Path, "/files.completeUploadExternal"):
			json.NewEncoder(w).Encode(map[string]any{"ok": true,
				"files": []map[string]any{{"id": "F1", "title": "Smoke test results"}}})
		case strings.HasSuffix(r.URL.Path, "/files.info"):
			json.NewEncoder(w).Encode(map[string]any{"ok": true,
				"file": map[string]any{"id": "F1", "permalink": "https://x.slack.com/files/F1"}})
		default: // the upload PUT itself
			w.Write([]byte("ok"))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The artifact record is what the console shows, and it resolves the channel and the person
// through the workspace the row names. create_artifact left TeamID out — the store round-trip
// test set it by hand, so nothing noticed — and the Artifacts page showed raw D…/U… ids for
// every file the bot wrote.
func TestCreateArtifactRecordsTheWorkspace(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	srv := fakeSlackUploads(t)
	sl := &Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/api/"))}, TeamID: "T1", BotUserID: "UBOT"}
	a := &Agent{store: st, tools: map[string]Tool{}, settings: newSettingsCache(st, Config{}), slacks: testRegistry(sl)}
	a.registerArtifactTools()

	c := &Call{TeamID: "T1", OrgID: orgID, SL: sl, Channel: "C1", ThreadTS: "111.1", UserID: "U1",
		Session: &Session{}, Streamer: &Streamer{failed: true}}
	out := a.runTool(ctx, c, "create_artifact", `{"title":"Smoke test results","format":"csv","content":"id,result\n1,passed"}`)
	if !strings.Contains(out, "posted") {
		t.Fatalf("create_artifact: %s", out)
	}

	arts, err := st.Artifacts(ctx, orgID, 0)
	if err != nil {
		t.Fatalf("artifacts: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("want 1 artifact, got %d", len(arts))
	}
	if arts[0].TeamID != "T1" {
		t.Errorf("artifact recorded team_id %q, want T1 — the console cannot name the channel without it", arts[0].TeamID)
	}
}
