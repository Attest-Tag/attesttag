package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// POST /v1/documents: files through a key, a PDF as readily as text. PUT only ever took a text
// body in JSON, so a PDF could reach the bot through the console and nowhere else.

// uploadPart is one file in an upload: the filename its part carries, and the path it should
// keep when that is not the filename.
type uploadPart struct{ filename, path, body string }

// v1Upload posts a multipart body the way curl -F sends one, with a key.
func v1Upload(t *testing.T, mux *http.ServeMux, key, folder string, parts ...uploadPart) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if folder != "" {
		mw.WriteField("folder", folder)
	}
	for _, p := range parts {
		fw, err := mw.CreateFormFile("files", p.filename)
		if err != nil {
			t.Fatal(err)
		}
		fw.Write([]byte(p.body))
		mw.WriteField("paths", p.path)
	}
	mw.Close()
	r := httptest.NewRequest("POST", "/v1/documents", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// docsBot is identityBot with somewhere to keep documents. The indexer points at an empty folder,
// so the re-index each upload starts returns at once instead of reaching for an embedder.
func docsBot(t *testing.T) (*Bot, *http.ServeMux, *Store) {
	t.Helper()
	b, mux, st := identityBot(t)
	b.docs = &localDocs{dir: t.TempDir(), store: st}
	b.ix = NewIndexer(nil, st, t.TempDir())
	return b, mux, st
}

func storedDoc(t *testing.T, b *Bot, orgID int64, p string) (string, bool) {
	t.Helper()
	f, err := b.docs.For(orgID).Get(context.Background(), p)
	if err != nil {
		return "", false
	}
	defer f.Close()
	raw, _ := io.ReadAll(f)
	return string(raw), true
}

func TestV1UploadStoresAPDFInItsOwnOrganisation(t *testing.T) {
	b, mux, st := docsBot(t)
	_, orgA, sessionA := signedUp(t, b, mux, st, "founder@example.com")
	_, orgB, sessionB := signedUp(t, b, mux, st, "other@example.com")
	keyA, _ := mintKey(t, mux, sessionA, "handbook sync")
	keyB, _ := mintKey(t, mux, sessionB, "b's key")

	pdf := "%PDF-1.4\n\x00\x01\x02\xff binary, not text\n%%EOF\n"
	code, body := v1Upload(t, mux, keyA, "policies",
		uploadPart{filename: "handbook.pdf", body: pdf},
		uploadPart{filename: "leave.md", path: "2026/leave.md", body: "# Leave"})
	if code != 201 {
		t.Fatalf("upload = %d: %v", code, body)
	}
	saved, _ := body["saved"].([]any)
	if len(saved) != 2 || saved[0] != "policies/handbook.pdf" || saved[1] != "policies/2026/leave.md" || body["indexing"] != true {
		t.Fatalf("upload answered %v", body)
	}
	if got, ok := storedDoc(t, b, orgA, "policies/handbook.pdf"); !ok || got != pdf {
		t.Fatalf("stored PDF = %q (found %v), want the bytes sent", got, ok)
	}

	// Recorded against the person the key belongs to, in the list and in the audit log.
	_, body = authReq(t, mux, "GET", "/v1/documents", nil, keyA)
	docs, _ := body["documents"].([]any)
	var by string
	for _, d := range docs {
		if d.(map[string]any)["path"] == "policies/handbook.pdf" {
			by, _ = d.(map[string]any)["uploaded_by"].(string)
		}
	}
	if by != "founder@example.com" {
		t.Errorf("uploaded_by = %q, want the key owner's email", by)
	}
	_, body = authReq(t, mux, "GET", "/v1/audit?action=document.uploaded", nil, keyA)
	if events, _ := body["events"].([]any); len(events) != 1 {
		t.Errorf("document.uploaded events = %v, want one", body["events"])
	}

	// Another organisation has none of it, by listing or by path.
	if _, ok := storedDoc(t, b, orgB, "policies/handbook.pdf"); ok {
		t.Fatal("the upload landed in the other organisation's folder")
	}
	_, body = authReq(t, mux, "GET", "/v1/documents", nil, keyB)
	if docs, _ := body["documents"].([]any); len(docs) != 0 {
		t.Fatalf("B's key lists %v", docs)
	}
	if code, _ := authReq(t, mux, "GET", "/v1/documents/policies/2026/leave.md", nil, keyB); code != 404 {
		t.Errorf("B's key reading A's document = %d, want 404", code)
	}
}

func TestV1UploadRefusesBeforeItWrites(t *testing.T) {
	b, mux, st := docsBot(t)
	ctx := context.Background()
	_, orgID, session := signedUp(t, b, mux, st, "founder@example.com")
	key, _ := mintKey(t, mux, session, "sync")

	if code, body := v1Upload(t, mux, key, ""); code != 400 || !strings.Contains(body["error"].(string), `"files"`) {
		t.Errorf("an upload with no files = %d %v, want 400 naming the field", code, body)
	}

	// One file the bot cannot read refuses the batch, and the readable file ahead of it is not
	// left stored behind the refusal.
	code, body := v1Upload(t, mux, key, "",
		uploadPart{filename: "fine.md", body: "# Fine"},
		uploadPart{filename: "payroll.xlsx", body: "PK\x03\x04"})
	if code != 400 || !strings.Contains(body["error"].(string), "payroll.xlsx") || !strings.Contains(body["error"].(string), ".pdf") {
		t.Errorf("an unreadable type = %d %v, want 400 naming the file and the types it takes", code, body)
	}
	if _, ok := storedDoc(t, b, orgID, "fine.md"); ok {
		t.Error("the batch was refused, but the file ahead of the bad one was stored")
	}

	if code, _ := v1Upload(t, mux, key, "", uploadPart{filename: "x.md", path: "notes/.draft.md", body: "#"}); code != 400 {
		t.Errorf("a hidden path = %d, want 400", code)
	}

	// Past the cap, a readable refusal rather than a request read without end.
	big := strings.Repeat("a", v1UploadLimit+1)
	if code, body := v1Upload(t, mux, key, "", uploadPart{filename: "big.txt", body: big}); code != http.StatusRequestEntityTooLarge {
		t.Errorf("an upload over the cap = %d %v, want 413", code, body)
	}
	if _, ok := storedDoc(t, b, orgID, "big.txt"); ok {
		t.Error("the upload over the cap was stored")
	}

	// And a key is its owner: a role without documents.manage uploads nothing.
	if err := st.UpsertConsoleRole(ctx, orgID, &ConsoleRole{Key: "integrator", Label: "Integrator",
		Permissions: []string{PermAPIKeysManage}}); err != nil {
		t.Fatal(err)
	}
	mate, err := st.CreateUser(ctx, "mate@example.com", "Mate", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddMembership(ctx, mate.ID, orgID, "integrator", 0); err != nil {
		t.Fatal(err)
	}
	theirs, err := st.CreateAdminSession(ctx, AdminUser{ID: mate.ID, Email: mate.Email, OrgID: orgID, Via: "password"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	narrow, _ := mintKey(t, mux, theirs, "narrow")
	if code, _ := v1Upload(t, mux, narrow, "", uploadPart{filename: "planted.md", body: "# Planted"}); code != 403 {
		t.Errorf("a key without documents.manage uploaded: %d", code)
	}
	if _, ok := storedDoc(t, b, orgID, "planted.md"); ok {
		t.Error("the refused upload was stored")
	}
}
