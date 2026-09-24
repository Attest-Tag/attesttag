package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
)

// testStore is the database every test in this package gets. SQLite in a temp directory by
// default, so a fresh clone passes `go test ./...` with nothing installed; Postgres when
// TEST_DATABASE_URL names one, which is how the two dialects are kept honest:
//
//	createdb attesttag_test
//	TEST_DATABASE_URL="postgres://$(whoami)@localhost:5432/attesttag_test?sslmode=disable" go test ./internal/app
//
// On Postgres each test gets its own schema rather than its own database — a schema is
// milliseconds where a database is most of a second, and there are hundreds of these. The
// schema is dropped afterwards, so a failed run leaves at most one behind per failure.
func testStore(t *testing.T) *Store {
	t.Helper()
	// Every test database starts its ids at 1 and every test signs up from the same address,
	// so the process-wide limiters (limits.go) are reset with the store.
	resetLimiters()
	if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
		return testStorePostgres(t, dsn)
	}
	st, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.db.Close() })
	return st
}

var testSchemaSeq atomic.Int64

func testStorePostgres(t *testing.T, dsn string) *Store {
	t.Helper()
	schema := fmt.Sprintf("t%d_%d", os.Getpid(), testSchemaSeq.Add(1))

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("connecting to TEST_DATABASE_URL: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec(`create schema ` + schema); err != nil {
		t.Fatalf("creating test schema: %v", err)
	}

	// search_path in the connection string is what puts the migrations and every query in this
	// test's own schema, and what makes current_schema() in db_dialect.go resolve to it.
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	st, err := OpenStore(dsn + sep + "search_path=" + schema)
	if err != nil {
		admin.Exec(`drop schema ` + schema + ` cascade`)
		t.Fatalf("open store on Postgres: %v", err)
	}
	t.Cleanup(func() {
		st.db.Close()
		cleanup, err := sql.Open("pgx", dsn)
		if err == nil {
			cleanup.Exec(`drop schema ` + schema + ` cascade`)
			cleanup.Close()
		}
	})
	return st
}

// The record is what makes artifacts auditable, so a round trip must keep every field, and
// the list must stay newest-first and body-free.
func TestArtifactRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	first := &Artifact{
		TeamID: "T1", Channel: "C1", ThreadTS: "1788358509.639309", CreatedBy: "U1",
		Title: "Access summary", Kind: "md", Content: "# Access\n\n0 hosts", Bytes: 17,
		FileID: "F1", Permalink: "https://x.slack.com/files/F1",
	}
	if err := st.AddArtifact(ctx, orgID, first); err != nil {
		t.Fatalf("add: %v", err)
	}
	if first.ID == 0 {
		t.Fatal("AddArtifact did not set ID")
	}
	second := &Artifact{Channel: "C1", Title: "Rows", Kind: "csv", Content: "a,b\n1,2", Bytes: 7}
	if err := st.AddArtifact(ctx, orgID, second); err != nil {
		t.Fatalf("add second: %v", err)
	}

	list, err := st.Artifacts(ctx, orgID, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 artifacts, got %d", len(list))
	}
	if list[0].Title != "Rows" {
		t.Errorf("list is not newest-first: got %q first", list[0].Title)
	}
	if list[0].Content != "" {
		t.Errorf("list carried a body: %q", list[0].Content)
	}

	got, err := st.ArtifactByID(ctx, orgID, first.ID)
	if err != nil {
		t.Fatalf("by id: %v", err)
	}
	if got.Content != first.Content || got.Permalink != first.Permalink || got.Kind != "md" {
		t.Errorf("round trip lost fields: %+v", got)
	}
	if got.ThreadTS != first.ThreadTS || got.CreatedBy != "U1" {
		t.Errorf("round trip lost provenance: %+v", got)
	}

	if err := st.DeleteArtifact(ctx, orgID, first.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.ArtifactByID(ctx, orgID, first.ID); err == nil {
		t.Error("deleted artifact still readable")
	}
}

// Every format the tool advertises must have a kind, or the model can pick one the raw
// endpoint cannot serve.
func TestArtifactFormatsAllHaveKinds(t *testing.T) {
	for _, f := range artifactFormats() {
		k, ok := artifactKinds[f]
		if !ok {
			t.Errorf("format %q has no kind", f)
			continue
		}
		if k.Ext == "" || k.MIME == "" {
			t.Errorf("format %q has an incomplete kind: %+v", f, k)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int]string{0: "0 bytes", 512: "512 bytes", 2048: "2.0 KB", 1 << 20: "1.0 MB"}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d)=%q want %q", in, got, want)
		}
	}
}

// The task card is what the thread shows while the file is being written, so it must name the
// file rather than say "create artifact".
func TestArtifactTaskCardNamesTheFile(t *testing.T) {
	args := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	got := humanTitle("create_artifact", args(map[string]any{"title": "Access summary"}), nil)
	if !strings.Contains(got, "Access summary") {
		t.Errorf("task card %q does not name the file", got)
	}
}

// The console reads artifacts over the admin API, so the routes have to shape them the way the
// page expects: names resolved, no bodies in the index, and the raw endpoint handing back a
// download rather than something the browser will render on the admin origin.
func TestArtifactAPI(t *testing.T) {
	st := testStore(t)
	token := seedAdmin(t, st)
	sl := &Chat{TeamID: "T1"}
	sl.chans.Store("C1", "product-updates")
	sl.names.Store("U1", "Alex Kim")
	b := &Bot{store: st, settings: newSettingsCache(st, Config{}), slacks: testRegistry(sl)}

	mux := http.NewServeMux()
	b.routes(mux, fstest.MapFS{})

	art := &Artifact{
		TeamID: "T1", Channel: "C1", ThreadTS: "1788358509.639309", CreatedBy: "U1",
		Title: "Access summary", Kind: "html", Content: "<h1>hi</h1>", Bytes: 11,
		Permalink: "https://x.slack.com/files/F1",
	}
	if err := st.AddArtifact(context.Background(), orgID, art); err != nil {
		t.Fatalf("add: %v", err)
	}

	do := func(method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}

	// Unauthenticated callers get nothing.
	anon := httptest.NewRecorder()
	mux.ServeHTTP(anon, httptest.NewRequest("GET", "/api/artifacts", nil))
	if anon.Code != http.StatusUnauthorized {
		t.Errorf("anonymous list = %d, want 401", anon.Code)
	}

	list := do("GET", "/api/artifacts")
	if list.Code != 200 {
		t.Fatalf("list = %d: %s", list.Code, list.Body)
	}
	var rows []map[string]any
	if err := json.Unmarshal(list.Body.Bytes(), &rows); err != nil {
		t.Fatalf("list json: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0]["ChannelName"] != "product-updates" || rows[0]["CreatedByName"] != "Alex Kim" {
		t.Errorf("names not resolved: %v", rows[0])
	}
	if _, ok := rows[0]["Content"]; ok {
		t.Error("index carried a body")
	}

	one := do("GET", fmt.Sprintf("/api/artifacts/%d", art.ID))
	var full map[string]any
	json.Unmarshal(one.Body.Bytes(), &full)
	if full["Content"] != "<h1>hi</h1>" {
		t.Errorf("detail body = %v", full["Content"])
	}

	raw := do("GET", fmt.Sprintf("/api/artifacts/%d/raw", art.ID))
	if raw.Body.String() != "<h1>hi</h1>" {
		t.Errorf("raw body = %q", raw.Body.String())
	}
	if cd := raw.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Errorf("raw must be a download, got Content-Disposition %q", cd)
	}
	if raw.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("raw is missing nosniff")
	}
	if !strings.Contains(raw.Header().Get("Content-Disposition"), "access_summary.html") {
		t.Errorf("raw filename = %q", raw.Header().Get("Content-Disposition"))
	}

	if missing := do("GET", "/api/artifacts/9999"); missing.Code != http.StatusNotFound {
		t.Errorf("unknown artifact = %d, want 404", missing.Code)
	}
	if del := do("DELETE", fmt.Sprintf("/api/artifacts/%d", art.ID)); del.Code != 200 {
		t.Errorf("delete = %d: %s", del.Code, del.Body)
	}
	after := do("GET", "/api/artifacts")
	rows = nil
	json.Unmarshal(after.Body.Bytes(), &rows)
	if len(rows) != 0 {
		t.Errorf("artifact still listed after delete: %v", rows)
	}
}
