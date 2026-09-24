package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// A Drive stood up locally: a folder tree, and a count of what was actually fetched, so a test
// can say not only what ended up in Documents but what it cost to get there.
type fakeDrive struct {
	mu      sync.Mutex
	files   map[string]fakeDriveFile // id -> file
	fetched []string                 // ids whose bytes were downloaded or exported
	srv     *httptest.Server
}

type fakeDriveFile struct {
	name, mime, parent, body, modified, md5 string
}

func newFakeDrive(t *testing.T, files map[string]fakeDriveFile) *fakeDrive {
	t.Helper()
	d := &fakeDrive{files: files}
	mux := http.NewServeMux()
	// Listing one folder.
	mux.HandleFunc("GET /drive/v3/files", func(w http.ResponseWriter, r *http.Request) {
		parent := parentFromQuery(r.URL.Query().Get("q"))
		d.mu.Lock()
		defer d.mu.Unlock()
		out := []map[string]any{}
		for id, f := range d.files {
			if f.parent != parent {
				continue
			}
			out = append(out, map[string]any{"id": id, "name": f.name, "mimeType": f.mime,
				"modifiedTime": f.modified, "md5Checksum": f.md5, "size": fmt.Sprint(len(f.body))})
		}
		json.NewEncoder(w).Encode(map[string]any{"files": out})
	})
	mux.HandleFunc("GET /drive/v3/files/{id}/export", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		f, ok := d.files[r.PathValue("id")]
		if ok {
			d.fetched = append(d.fetched, r.PathValue("id"))
		}
		d.mu.Unlock()
		if !ok {
			http.Error(w, `{"error":{"message":"File not found"}}`, 404)
			return
		}
		fmt.Fprint(w, f.body)
	})
	// Metadata, and bytes when alt=media asks for them.
	mux.HandleFunc("GET /drive/v3/files/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		d.mu.Lock()
		f, ok := d.files[id]
		media := r.URL.Query().Get("alt") == "media"
		if ok && media {
			d.fetched = append(d.fetched, id)
		}
		d.mu.Unlock()
		if !ok {
			http.Error(w, `{"error":{"message":"File not found"}}`, 404)
			return
		}
		if media {
			fmt.Fprint(w, f.body)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": id, "name": f.name, "mimeType": f.mime,
			"modifiedTime": f.modified, "md5Checksum": f.md5, "size": fmt.Sprint(len(f.body))})
	})
	d.srv = httptest.NewServer(mux)
	t.Cleanup(d.srv.Close)
	return d
}

// parentFromQuery pulls the folder id out of the `"<id>" in parents ...` clause the sync sends.
func parentFromQuery(q string) string {
	i := strings.Index(q, `"`)
	j := strings.Index(q[i+1:], `"`)
	if i < 0 || j < 0 {
		return ""
	}
	return q[i+1 : i+1+j]
}

func (d *fakeDrive) remove(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.files, id)
}

func (d *fakeDrive) edit(id string, body, modified string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f := d.files[id]
	f.body, f.modified, f.md5 = body, modified, modified
	d.files[id] = f
}

func (d *fakeDrive) fetchCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.fetched)
}

// driveTestBot wires the docs harness to a fake Drive, and returns the sync that points at the
// tree's root folder. The credential is a bearer token rather than a service account: what is
// under test is the walk and the mirror, and a real key exchange would only add a network call.
func driveTestBot(t *testing.T, files map[string]fakeDriveFile) (*Bot, *fakeDrive, *DriveSync) {
	t.Helper()
	fixedMasterKey(t)
	st := testStore(t)
	sealer, err := NewSealer()
	if err != nil {
		t.Fatal(err)
	}
	local := &localDocs{dir: t.TempDir(), store: st}
	b := &Bot{store: st, sealer: sealer, mail: logMailer{}, settings: newSettingsCache(st, Config{}),
		resolver: NewResolver(st), docs: local, ix: NewIndexer(nil, st, t.TempDir()), proxy: NewProxy(sealer, st)}
	b.slacks = NewChatRegistry(st, sealer)

	drive := newFakeDrive(t, files)
	host := mustHost(t, drive.srv.URL)
	ctx := context.Background()
	enc, err := b.sealSecret(&Secret{Token: "t0ken"})
	if err != nil {
		t.Fatal(err)
	}
	connID, err := st.InsertConnection(ctx, orgID, &Connection{
		Name: "Drive", Preset: "gdrive", CredType: "bearer", AllowedHosts: []string{host}, Status: "active",
	}, enc)
	if err != nil {
		t.Fatal(err)
	}
	syncID, err := st.AddDriveSync(ctx, orgID, DriveSync{
		ConnectionID: connID, FolderID: "root", FolderName: "Handbook", Dest: "handbook", Recurse: true, CreatedBy: "U1",
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.DriveSync(ctx, orgID, syncID)
	if err != nil {
		t.Fatal(err)
	}
	// Point the sync's Drive at the fake one, client included: the proxy's own client refuses
	// to dial loopback, which is exactly what it is there for.
	driveTest = &driveOverride{scheme: "http", host: host, client: drive.srv.Client()}
	t.Cleanup(func() { driveTest = nil })
	return b, drive, s
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

// docPaths is what is in the document store now, for comparing against what Drive holds.
func docPaths(t *testing.T, b *Bot) map[string]string {
	t.Helper()
	list, err := b.docs.For(orgID).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, d := range list {
		rc, err := b.docs.For(orgID).Get(context.Background(), d.Path)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 4096)
		n, _ := rc.Read(buf)
		rc.Close()
		out[d.Path] = string(buf[:n])
	}
	return out
}

var driveTree = map[string]fakeDriveFile{
	"root":  {name: "Handbook", mime: driveFolderMime},
	"leave": {name: "Leave policy", mime: "application/vnd.google-apps.document", parent: "root", body: "# Leave\nTwenty-five days.", modified: "v1"},
	"notes": {name: "notes.md", mime: "text/markdown", parent: "root", body: "raw markdown", modified: "v1", md5: "abc"},
	"logo":  {name: "logo.png", mime: "image/png", parent: "root", body: "PNG", modified: "v1", md5: "def"},
	"sub":   {name: "2026", mime: driveFolderMime, parent: "root"},
	"pay":   {name: "Pay bands", mime: "application/vnd.google-apps.document", parent: "sub", body: "bands", modified: "v1"},
}

// The first pass: native Docs are exported, real files are downloaded, subfolders keep their
// shape, and a type the index cannot read is skipped with a reason rather than half-stored.
func TestDriveSyncFirstPass(t *testing.T) {
	b, _, s := driveTestBot(t, copyTree(driveTree))
	rep, err := b.syncDrive(context.Background(), orgID, s)
	if err != nil {
		t.Fatal(err)
	}
	got := docPaths(t, b)
	want := map[string]string{
		"handbook/Leave policy.md":   "# Leave\nTwenty-five days.",
		"handbook/notes.md":          "raw markdown",
		"handbook/2026/Pay bands.md": "bands",
	}
	for path, body := range want {
		if got[path] != body {
			t.Errorf("document %q = %q, want %q", path, got[path], body)
		}
	}
	if len(got) != len(want) {
		t.Errorf("stored %d documents, want %d: %v", len(got), len(want), docKeys(got))
	}
	if rep.Added != 3 {
		t.Errorf("added = %d, want 3", rep.Added)
	}
	if len(rep.Skipped) != 1 || !strings.Contains(rep.Skipped[0], "logo.png") {
		t.Errorf("skipped = %v, want the png with a reason", rep.Skipped)
	}
}

// A second pass over an unchanged folder must not download anything: the version Drive reports
// is what decides, and re-exporting every Doc every six hours is the cost this avoids.
func TestDriveSyncSkipsUnchanged(t *testing.T) {
	b, drive, s := driveTestBot(t, copyTree(driveTree))
	ctx := context.Background()
	if _, err := b.syncDrive(ctx, orgID, s); err != nil {
		t.Fatal(err)
	}
	first := drive.fetchCount()
	rep, err := b.syncDrive(ctx, orgID, s)
	if err != nil {
		t.Fatal(err)
	}
	if drive.fetchCount() != first {
		t.Errorf("second pass fetched %d more files, want 0", drive.fetchCount()-first)
	}
	if rep.Unchanged != 3 || rep.Added != 0 || rep.Updated != 0 {
		t.Errorf("second pass: %+v, want 3 unchanged and nothing else", rep)
	}
}

// An edit in Drive is a new version, so the copy is taken again.
func TestDriveSyncTakesEdits(t *testing.T) {
	b, drive, s := driveTestBot(t, copyTree(driveTree))
	ctx := context.Background()
	if _, err := b.syncDrive(ctx, orgID, s); err != nil {
		t.Fatal(err)
	}
	drive.edit("leave", "# Leave\nThirty days.", "v2")
	rep, err := b.syncDrive(ctx, orgID, s)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Updated != 1 {
		t.Errorf("updated = %d, want 1", rep.Updated)
	}
	if got := docPaths(t, b)["handbook/Leave policy.md"]; got != "# Leave\nThirty days." {
		t.Errorf("document = %q, want the edited text", got)
	}
}

// The mirror: a file deleted in Drive is deleted here, and a document a person uploaded into
// the same folder is left alone. The second half is the one that matters — a sync that swept
// the folder rather than its own rows would delete somebody's upload.
func TestDriveSyncMirrorsDeletesButSparesUploads(t *testing.T) {
	b, drive, s := driveTestBot(t, copyTree(driveTree))
	ctx := context.Background()
	if _, err := b.syncDrive(ctx, orgID, s); err != nil {
		t.Fatal(err)
	}
	if err := b.docs.For(orgID).Put(ctx, "handbook/by-hand.md", strings.NewReader("typed here"), "U1", ""); err != nil {
		t.Fatal(err)
	}
	drive.remove("notes")
	rep, err := b.syncDrive(ctx, orgID, s)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Removed != 1 {
		t.Errorf("removed = %d, want 1", rep.Removed)
	}
	got := docPaths(t, b)
	if _, ok := got["handbook/notes.md"]; ok {
		t.Error("notes.md is still here; a file deleted in Drive must not go on being answered from")
	}
	if _, ok := got["handbook/by-hand.md"]; !ok {
		t.Error("the uploaded document was deleted; the mirror must only take back what it put there")
	}
}

// Renaming in Drive moves the document rather than leaving the old name behind as a duplicate
// that the bot would go on answering from.
func TestDriveSyncFollowsRenames(t *testing.T) {
	b, drive, s := driveTestBot(t, copyTree(driveTree))
	ctx := context.Background()
	if _, err := b.syncDrive(ctx, orgID, s); err != nil {
		t.Fatal(err)
	}
	drive.mu.Lock()
	f := drive.files["leave"]
	f.name, f.modified = "Time off policy", "v2"
	drive.files["leave"] = f
	drive.mu.Unlock()

	if _, err := b.syncDrive(ctx, orgID, s); err != nil {
		t.Fatal(err)
	}
	got := docPaths(t, b)
	if _, ok := got["handbook/Leave policy.md"]; ok {
		t.Error("the old name is still a document; a rename must not leave a stale copy")
	}
	if _, ok := got["handbook/Time off policy.md"]; !ok {
		t.Errorf("the renamed document is missing: %v", docKeys(got))
	}
}

// A folder that has been deleted in Drive is not "every file is gone": failing the whole pass
// is what keeps a mistyped or unshared folder from emptying the index.
func TestDriveSyncRefusesMissingFolder(t *testing.T) {
	b, drive, s := driveTestBot(t, copyTree(driveTree))
	ctx := context.Background()
	if _, err := b.syncDrive(ctx, orgID, s); err != nil {
		t.Fatal(err)
	}
	drive.remove("root")
	if _, err := b.syncDrive(ctx, orgID, s); err == nil {
		t.Fatal("a missing folder must fail the pass, not mirror it as a mass deletion")
	}
	if n := len(docPaths(t, b)); n != 3 {
		t.Errorf("%d documents left, want the 3 from the first pass", n)
	}
}

// A title with a slash in it is one document, not a folder somebody did not ask for.
func TestDriveSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"Q3 / Q4 plan": "Q3 - Q4 plan",
		" .hidden":     "hidden",
		"normal":       "normal",
		"  ":           "untitled",
	} {
		if got := driveSafeName(in); got != want {
			t.Errorf("driveSafeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// Whatever somebody pastes, the id comes out of it.
func TestDriveFolderID(t *testing.T) {
	ok := map[string]string{
		"https://drive.google.com/drive/folders/1AbC_dEfGhIjK?usp=sharing": "1AbC_dEfGhIjK",
		"https://drive.google.com/drive/u/0/folders/1AbC_dEfGhIjK":         "1AbC_dEfGhIjK",
		"https://drive.google.com/open?id=1AbC_dEfGhIjK":                   "1AbC_dEfGhIjK",
		"1AbC_dEfGhIjK": "1AbC_dEfGhIjK",
	}
	for in, want := range ok {
		got, err := driveFolderID(in)
		if err != nil || got != want {
			t.Errorf("driveFolderID(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "not a link", "https://example.com/nope"} {
		if _, err := driveFolderID(in); err == nil {
			t.Errorf("driveFolderID(%q) should not have found an id", in)
		}
	}
}

// A credential that cannot reach Drive is refused before it is spent, so a Slack token is never
// sent to Google because somebody picked the wrong connection.
func TestDriveAPIRefusesWrongHost(t *testing.T) {
	conn := &Connection{Name: "Slack", CredType: "bearer", AllowedHosts: []string{"slack.com"}}
	if _, err := newDriveAPI(NewProxy(nil, nil), conn, orgID); err == nil {
		t.Fatal("a connection that does not reach Drive must not be usable as a Drive credential")
	}
	if _, err := newDriveAPI(NewProxy(nil, nil), nil, orgID); err == nil {
		t.Fatal("a deleted connection must be an error, not a nil dereference")
	}
}

func docKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// "Test" in the dialog: the preview is worked out from the listing alone, and it must agree
// with what a pass then does — the same files in, the same files left out for the same reasons,
// and not one byte fetched to find out.
func TestDrivePreviewMatchesPass(t *testing.T) {
	b, drive, s := driveTestBot(t, copyTree(driveTree))
	ctx := context.Background()
	conn, err := b.store.Connection(ctx, orgID, s.ConnectionID)
	if err != nil {
		t.Fatal(err)
	}
	api, err := newDriveAPI(b.proxy, conn, orgID)
	if err != nil {
		t.Fatal(err)
	}
	pv, err := api.preview(ctx, "root", "handbook", true)
	if err != nil {
		t.Fatal(err)
	}
	if drive.fetchCount() != 0 {
		t.Errorf("preview fetched %d files, want 0", drive.fetchCount())
	}
	if pv.FolderName != "Handbook" || pv.Files != 3 || pv.Skipped != 1 || pv.Folders != 1 || pv.Capped || pv.More != 0 {
		t.Errorf("preview = %+v, want Handbook with 3 files, 1 skipped, 1 folder", *pv)
	}
	// Folders first, then names in order — the way Drive shows the folder.
	if got := treeLine(pv.Tree); got != "Handbook/[2026/[Pay bands→Pay bands.md] Leave policy→Leave policy.md logo.png(not a type the index reads) notes.md→notes.md]" {
		t.Errorf("tree = %s", got)
	}
	// The fake reports a size for everything; real Drive has none for a native Doc, and the
	// total is of whatever Drive said, never of the png that is not coming in.
	if want := int64(len(driveTree["leave"].body) + len(driveTree["notes"].body) + len(driveTree["pay"].body)); pv.Bytes != want {
		t.Errorf("bytes = %d, want %d — the three files coming in, not the skipped png", pv.Bytes, want)
	}

	rep, err := b.syncDrive(ctx, orgID, s)
	if err != nil {
		t.Fatal(err)
	}
	got := docPaths(t, b)
	promised := promisedDocs(pv.Tree, "handbook")
	for _, p := range promised {
		if _, ok := got[p]; !ok {
			t.Errorf("preview promised %q, the pass did not store it: %v", p, docKeys(got))
		}
	}
	if len(got) != len(promised) {
		t.Errorf("pass stored %d documents, preview promised %d", len(got), len(promised))
	}
	if len(rep.Skipped) != pv.Skipped {
		t.Errorf("pass skipped %v, preview said %d", rep.Skipped, pv.Skipped)
	}
}

// With subfolders off, the preview still shows them — greyed, with the reason — so somebody
// can see what the setting leaves behind, and counts only what would come in.
func TestDrivePreviewWithoutSubfolders(t *testing.T) {
	b, _, s := driveTestBot(t, copyTree(driveTree))
	ctx := context.Background()
	conn, _ := b.store.Connection(ctx, orgID, s.ConnectionID)
	api, err := newDriveAPI(b.proxy, conn, orgID)
	if err != nil {
		t.Fatal(err)
	}
	pv, err := api.preview(ctx, "root", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if pv.Files != 2 || pv.Folders != 0 || pv.Skipped != 1 {
		t.Errorf("preview = %+v, want 2 files, no folders walked", *pv)
	}
	if got := treeLine(pv.Tree); got != "Handbook/[2026/(subfolders are off) Leave policy→Leave policy.md logo.png(not a type the index reads) notes.md→notes.md]" {
		t.Errorf("tree = %s", got)
	}
}

// The route behind the button: a link is enough, an existing sync can be previewed by id, and
// a link to something other than a shared folder is turned away with the reason.
func TestDrivePreviewAPI(t *testing.T) {
	const folder, file = "1AbCdEfGhIjKlMnOpQrSt", "1FiLeIdIsAlSoLoNgEnOuGh"
	tree := copyTree(driveTree)
	tree[folder] = fakeDriveFile{name: "Policies", mime: driveFolderMime}
	tree[file] = fakeDriveFile{name: "Travel", mime: "application/vnd.google-apps.document", parent: folder, body: "trains", modified: "v1"}
	b, drive, s := driveTestBot(t, tree)
	mux := http.NewServeMux()
	b.routes(mux, nil)
	oid, _, tok := seedOrg(t, b.store, RoleAdmin)
	if oid != orgID {
		t.Fatalf("seeded org %d, the connection is in %d", oid, orgID)
	}

	code, body := authReq(t, mux, "POST", "/api/drive/syncs/preview", map[string]any{
		"connection_id": s.ConnectionID, "folder": "https://drive.google.com/drive/u/0/folders/" + folder + "?usp=sharing",
		"dest": "policies", "recurse": true,
	}, tok)
	if code != 200 || body["folder_name"] != "Policies" || body["files"] != float64(1) {
		t.Fatalf("preview = %d %v", code, body)
	}
	root, _ := body["tree"].(map[string]any)
	kids, _ := root["children"].([]any)
	if len(kids) != 1 || kids[0].(map[string]any)["doc"] != "Travel.md" {
		t.Errorf("tree = %v, want the one Doc as Travel.md", root)
	}
	if drive.fetchCount() != 0 {
		t.Errorf("preview fetched %d files, want 0", drive.fetchCount())
	}

	code, body = authReq(t, mux, "POST", "/api/drive/syncs/preview", map[string]any{"sync_id": s.ID, "recurse": true}, tok)
	if code != 200 || body["folder_name"] != "Handbook" || body["files"] != float64(3) {
		t.Errorf("preview by sync = %d %v", code, body)
	}

	code, body = authReq(t, mux, "POST", "/api/drive/syncs/preview", map[string]any{
		"connection_id": s.ConnectionID, "folder": file,
	}, tok)
	if code != 400 || !strings.Contains(fmt.Sprint(body["error"]), "is a file, not a folder") {
		t.Errorf("file as folder = %d %v", code, body)
	}
	code, body = authReq(t, mux, "POST", "/api/drive/syncs/preview", map[string]any{
		"connection_id": s.ConnectionID, "folder": "1NoSuchFolderAnywhere",
	}, tok)
	if code != 400 || !strings.Contains(fmt.Sprint(body["error"]), "shared with this connection") {
		t.Errorf("unshared folder = %d %v", code, body)
	}
	code, body = authReq(t, mux, "POST", "/api/drive/syncs/preview", map[string]any{
		"connection_id": s.ConnectionID, "folder": "not a link",
	}, tok)
	if code != 400 {
		t.Errorf("junk = %d %v", code, body)
	}
}

// The sharing hint is for a folder Drive would not hand over, not for a credential that never
// got as far as Drive — sharing the folder would not fix that, so the hint would mislead.
func TestDriveShareHint(t *testing.T) {
	if got := driveShareHint(errors.New("drive 404 Not Found: File not found")).Error(); !strings.HasSuffix(got, "is the folder shared with this connection?") {
		t.Errorf("unshared folder: %q, want the hint", got)
	}
	cred := fmt.Errorf("%w: %w", errCredential, errors.New("token exchange failed: account not found"))
	if got := driveShareHint(cred).Error(); strings.Contains(got, "shared") {
		t.Errorf("credential failure: %q, want no sharing hint", got)
	}
}

// treeLine flattens a preview tree to one line: folders as name/[children], files as
// name→doc or name(reason).
func treeLine(n *DriveNode) string {
	if !n.Folder {
		if n.Skip != "" {
			return n.Name + "(" + n.Skip + ")"
		}
		return n.Name + "→" + n.Doc
	}
	s := n.Name + "/"
	if n.Skip != "" {
		return s + "(" + n.Skip + ")"
	}
	parts := []string{}
	for _, c := range n.Children {
		parts = append(parts, treeLine(c))
	}
	return s + "[" + strings.Join(parts, " ") + "]"
}

// promisedDocs is every document path a preview says a pass will store.
func promisedDocs(n *DriveNode, dir string) []string {
	var out []string
	for _, c := range n.Children {
		switch {
		case c.Folder && c.Skip == "":
			out = append(out, promisedDocs(c, dir+"/"+driveSafeName(c.Name))...)
		case !c.Folder && c.Doc != "":
			out = append(out, strings.TrimPrefix(dir+"/"+c.Doc, "/"))
		}
	}
	return out
}

// copyTree copies the shared tree so one test's edits cannot reach another's.
func copyTree(in map[string]fakeDriveFile) map[string]fakeDriveFile {
	out := make(map[string]fakeDriveFile, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
