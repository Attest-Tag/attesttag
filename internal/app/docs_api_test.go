package app

import (
	"bytes"
	"encoding/json"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"strings"
	"testing"
)

// The console's side of folders: uploading one, making an empty one, moving things between
// them and deleting one whole. The indexer is pointed at a folder that does not exist, so the
// re-index each write kicks off returns straight away instead of reaching for an embedder.
type docsAPI struct {
	t    *testing.T
	mux  *http.ServeMux
	sess string
	docs DocStore
	st   *Store
}

func newDocsAPI(t *testing.T) *docsAPI {
	t.Helper()
	fixedMasterKey(t)
	st := testStore(t)
	sealer, err := NewSealer()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	local := &localDocs{dir: root, store: st}
	b := &Bot{store: st, sealer: sealer, mail: logMailer{}, settings: newSettingsCache(st, Config{}),
		resolver: NewResolver(st), docs: local, ix: NewIndexer(nil, st, t.TempDir())}
	b.slacks = NewChatRegistry(st, sealer)
	mux := http.NewServeMux()
	b.routes(mux, nil)
	return &docsAPI{t: t, mux: mux, sess: seedAdmin(t, st), docs: local.For(orgID), st: st}
}

func (d *docsAPI) do(method, path string, body any) (int, map[string]any) {
	d.t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		raw, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
	}
	return d.send(r)
}

func (d *docsAPI) send(r *http.Request) (int, map[string]any) {
	d.t.Helper()
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: d.sess})
	r.AddCookie(&http.Cookie{Name: csrfCookie, Value: "tok"})
	r.Header.Set(csrfHeader, "tok")
	w := httptest.NewRecorder()
	d.mux.ServeHTTP(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// upload posts a multipart body shaped the way the console sends one: the files, and beside
// them the path each should keep, because a part's filename cannot carry a folder.
func (d *docsAPI) upload(into string, files map[string]string) (int, map[string]any) {
	d.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if into != "" {
		mw.WriteField("folder", into)
	}
	names := slices.Sorted(maps.Keys(files))
	for _, name := range names {
		w, err := mw.CreateFormFile("files", path.Base(name))
		if err != nil {
			d.t.Fatal(err)
		}
		w.Write([]byte(files[name]))
		mw.WriteField("paths", name)
	}
	mw.Close()
	r := httptest.NewRequest("POST", "/api/documents", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return d.send(r)
}

func (d *docsAPI) folders() []string {
	d.t.Helper()
	r := httptest.NewRequest("GET", "/api/document-folders", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: d.sess})
	w := httptest.NewRecorder()
	d.mux.ServeHTTP(w, r)
	var out []string
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		d.t.Fatalf("GET /api/document-folders = %d %s", w.Code, w.Body.String())
	}
	return out
}

func (d *docsAPI) paths() []string {
	d.t.Helper()
	list, err := d.docs.List(d.t.Context())
	if err != nil {
		d.t.Fatal(err)
	}
	return paths(list)
}

func TestUploadKeepsFolderShape(t *testing.T) {
	api := newDocsAPI(t)
	code, body := api.upload("", map[string]string{
		"policies/leave.md":    "# Leave",
		"policies/2026/pay.md": "# Pay",
		"handbook.md":          "# Handbook",
	})
	if code != 201 {
		t.Fatalf("upload = %d: %v", code, body)
	}
	if got := api.paths(); strings.Join(got, ",") != "handbook.md,policies/2026/pay.md,policies/leave.md" {
		t.Fatalf("documents = %v", got)
	}
	// The folders the upload brought with it are folders the console now knows.
	if got := api.folders(); strings.Join(got, ",") != "policies,policies/2026" {
		t.Fatalf("folders = %v", got)
	}

	// A second upload lands inside the folder that is open.
	if code, body := api.upload("policies/2026", map[string]string{"holidays/dec.md": "# December"}); code != 201 {
		t.Fatalf("upload into a folder = %d: %v", code, body)
	}
	if got := api.paths(); !strings.Contains(strings.Join(got, ","), "policies/2026/holidays/dec.md") {
		t.Fatalf("documents = %v", got)
	}

	// An unsupported type is refused by name, and a climbing path cannot reach out of the
	// organisation's folder.
	if code, _ := api.upload("", map[string]string{"payroll.xlsx": "binary"}); code != 400 {
		t.Errorf("uploading .xlsx = %d, want 400", code)
	}
	if code, body := api.upload("", map[string]string{"../../escaped.md": "# Nope"}); code != 201 {
		t.Fatalf("upload = %d: %v", code, body)
	}
	for _, p := range api.paths() {
		if strings.Contains(p, "..") {
			t.Errorf("a document escaped its folder: %q", p)
		}
	}
}

func TestFolderRoutes(t *testing.T) {
	api := newDocsAPI(t)

	// An empty folder is remembered even though nothing is in it.
	if code, body := api.do("POST", "/api/document-folders", map[string]string{"path": "runbooks/oncall"}); code != 201 {
		t.Fatalf("create = %d: %v", code, body)
	}
	if got := api.folders(); strings.Join(got, ",") != "runbooks,runbooks/oncall" {
		t.Fatalf("folders = %v, want the folder and its parent", got)
	}

	api.upload("", map[string]string{"handbook.md": "# Handbook"})

	// Moving a document into it.
	if code, body := api.do("POST", "/api/documents/move", map[string]string{
		"from": "handbook.md", "to": "runbooks/oncall/handbook.md",
	}); code != 200 {
		t.Fatalf("move = %d: %v", code, body)
	}
	if got := api.paths(); strings.Join(got, ",") != "runbooks/oncall/handbook.md" {
		t.Fatalf("after the move documents = %v", got)
	}

	// Moving the folder carries the document with it.
	if code, body := api.do("POST", "/api/document-folders/move", map[string]string{
		"from": "runbooks", "to": "ops",
	}); code != 200 {
		t.Fatalf("move folder = %d: %v", code, body)
	}
	if got := api.paths(); strings.Join(got, ",") != "ops/oncall/handbook.md" {
		t.Fatalf("after the folder move documents = %v", got)
	}
	if got := api.folders(); strings.Join(got, ",") != "ops,ops/oncall" {
		t.Fatalf("after the folder move folders = %v", got)
	}
	// A folder cannot swallow itself.
	if code, _ := api.do("POST", "/api/document-folders/move", map[string]string{"from": "ops", "to": "ops/inner"}); code != 400 {
		t.Errorf("moving a folder into itself = %d, want 400", code)
	}

	// A destination that climbs is clamped, and the folders remembered afterwards are the ones
	// the document landed in — never the raw path somebody sent.
	if code, body := api.do("POST", "/api/documents/move", map[string]string{
		"from": "ops/oncall/handbook.md", "to": "../../elsewhere/handbook.md",
	}); code != 200 {
		t.Fatalf("move = %d: %v", code, body)
	}
	if got := api.paths(); strings.Join(got, ",") != "elsewhere/handbook.md" {
		t.Fatalf("after a climbing move documents = %v", got)
	}
	for _, f := range api.folders() {
		if strings.Contains(f, "..") {
			t.Errorf("a junk folder was remembered: %q", f)
		}
	}
	if code, body := api.do("POST", "/api/document-folders/move", map[string]string{
		"from": "elsewhere", "to": "ops",
	}); code != 200 {
		t.Fatalf("move folder back = %d: %v", code, body)
	}

	// Deleting takes the documents under it, and says how many.
	code, body := api.do("DELETE", "/api/document-folders/ops", nil)
	if code != 200 || body["deleted"] != float64(1) {
		t.Fatalf("delete = %d: %v", code, body)
	}
	if got := api.paths(); len(got) != 0 {
		t.Fatalf("documents survived the folder delete: %v", got)
	}
	if got := api.folders(); len(got) != 0 {
		t.Fatalf("folders survived the delete: %v", got)
	}
}

// Documents.manage is the permission that gates every one of these, so a reader who can list
// documents still cannot rearrange them.
func TestFolderWritesNeedDocsManage(t *testing.T) {
	fixedMasterKey(t)
	st := testStore(t)
	sealer, err := NewSealer()
	if err != nil {
		t.Fatal(err)
	}
	local := &localDocs{dir: t.TempDir(), store: st}
	b := &Bot{store: st, sealer: sealer, mail: logMailer{}, settings: newSettingsCache(st, Config{}),
		resolver: NewResolver(st), docs: local, ix: NewIndexer(nil, st, t.TempDir())}
	b.slacks = NewChatRegistry(st, sealer)
	mux := http.NewServeMux()
	b.routes(mux, nil)
	_, _, tok := seedOrg(t, st, RoleViewer)
	d := &docsAPI{t: t, mux: mux, sess: tok, docs: local.For(orgID), st: st}

	for _, call := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/document-folders", map[string]string{"path": "secrets"}},
		{"POST", "/api/document-folders/move", map[string]string{"from": "a", "to": "b"}},
		{"DELETE", "/api/document-folders/a", nil},
		{"PUT", "/api/document-folders/a", map[string]string{"scope": "C123"}},
		{"POST", "/api/documents/move", map[string]string{"from": "a.md", "to": "b.md"}},
	} {
		if code, _ := d.do(call.method, call.path, call.body); code != 403 {
			t.Errorf("%s %s as a viewer = %d, want 403", call.method, call.path, code)
		}
	}
	if got, _ := st.DocumentFolders(t.Context(), orgID); len(got) != 0 {
		t.Errorf("a viewer created folders: %v", got)
	}
}

// scopeByPath reads back where each document applies, so a bulk change can be checked one
// document at a time rather than on the count the handler reports.
func (d *docsAPI) scopeByPath() map[string]string {
	d.t.Helper()
	list, err := d.st.Documents(d.t.Context(), orgID)
	if err != nil {
		d.t.Fatal(err)
	}
	out := map[string]string{}
	for _, doc := range list {
		out[doc.Path] = doc.Scope
	}
	return out
}

// Scoping a folder reaches everything under it, however deep, and nothing outside it. The
// sibling here is named so that a `like 'q1_notes/%'` match would swallow it: `_` is a
// single-character wildcard, and quietly widening who can read a document is the failure
// this guards.
func TestFolderScopeReachesTheFolderAndNothingElse(t *testing.T) {
	api := newDocsAPI(t)
	if code, body := api.upload("", map[string]string{
		"q1_notes/plan.md":        "# Plan",
		"q1_notes/deep/detail.md": "# Detail",
		"q1-notes/other.md":       "# Other",
		"top.md":                  "# Top",
	}); code != 201 {
		t.Fatalf("upload = %d %v", code, body)
	}

	code, body := api.do("PUT", "/api/document-folders/q1_notes", map[string]string{"scope": "C123"})
	if code != 200 {
		t.Fatalf("PUT folder scope = %d %v", code, body)
	}
	if n, _ := body["updated"].(float64); n != 2 {
		t.Errorf("updated = %v, want 2", body["updated"])
	}

	want := map[string]string{
		"q1_notes/plan.md":        "C123",
		"q1_notes/deep/detail.md": "C123",
		"q1-notes/other.md":       "",
		"top.md":                  "",
	}
	got := api.scopeByPath()
	for p, w := range want {
		if got[p] != w {
			t.Errorf("scope of %s = %q, want %q", p, got[p], w)
		}
	}
}

// The folder's scope is a bulk apply, not something the folder keeps: a document put on its
// own scope afterwards holds it, which is what makes "set the folder, then fix the one
// exception inside" work.
func TestFolderScopeIsABulkApplyNotAnInheritance(t *testing.T) {
	api := newDocsAPI(t)
	if code, body := api.upload("", map[string]string{
		"policies/leave.md": "# Leave",
		"policies/pay.md":   "# Pay",
	}); code != 201 {
		t.Fatalf("upload = %d %v", code, body)
	}
	if code, body := api.do("PUT", "/api/document-folders/policies", map[string]string{"scope": "C123"}); code != 200 {
		t.Fatalf("PUT folder scope = %d %v", code, body)
	}
	if code, body := api.do("PUT", "/api/documents/policies/pay.md", map[string]string{"scope": "C999"}); code != 200 {
		t.Fatalf("PUT document scope = %d %v", code, body)
	}

	got := api.scopeByPath()
	if got["policies/leave.md"] != "C123" {
		t.Errorf("leave.md = %q, want C123", got["policies/leave.md"])
	}
	if got["policies/pay.md"] != "C999" {
		t.Errorf("pay.md lost its own scope: %q, want C999", got["policies/pay.md"])
	}

	// Setting the folder again is what takes the exception back, and only then.
	if code, body := api.do("PUT", "/api/document-folders/policies", map[string]string{"scope": ""}); code != 200 {
		t.Fatalf("PUT folder scope = %d %v", code, body)
	}
	for p, sc := range api.scopeByPath() {
		if sc != "" {
			t.Errorf("scope of %s = %q, want workspace-wide", p, sc)
		}
	}
}

// The root is not a folder anybody can scope in one go: an empty path would otherwise mean
// every document in the organisation.
func TestFolderScopeRefusesTheRoot(t *testing.T) {
	api := newDocsAPI(t)
	if code, body := api.upload("", map[string]string{"top.md": "# Top"}); code != 201 {
		t.Fatalf("upload = %d %v", code, body)
	}
	if code, _ := api.do("PUT", "/api/document-folders/", map[string]string{"scope": "C123"}); code != 400 && code != 404 {
		t.Errorf("PUT root scope = %d, want 400 or 404", code)
	}
	if sc := api.scopeByPath()["top.md"]; sc != "" {
		t.Errorf("a root scope reached top.md: %q", sc)
	}
}

// A path no document can have is the caller's mistake: a hidden name, or one that climbs out of
// the organisation's folder. Asking for one came back as a 500 and an ERROR in the log; it is a
// 400 now, from the console and the public API alike (both answer through fail), and a document
// that is simply not there is still a 404.
func TestAPathNoDocumentCanHaveIsABadRequest(t *testing.T) {
	d := newDocsAPI(t)
	for _, p := range []string{"/api/documents/.hidden.md", "/api/documents/notes/.draft.md"} {
		if code, body := d.do("GET", p, nil); code != http.StatusBadRequest || !strings.Contains(body["error"].(string), "hidden") {
			t.Errorf("GET %s = %d %v, want 400 naming it hidden", p, code, body)
		}
	}
	if code, _ := d.do("GET", "/api/documents/missing.md", nil); code != http.StatusNotFound {
		t.Errorf("a document that is not there = %d, want 404", code)
	}
	w := httptest.NewRecorder()
	fail(w, docPathError{"invalid path"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("fail(docPathError) = %d, want 400", w.Code)
	}
}
