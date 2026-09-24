package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func folderDocs(t *testing.T) (*Bot, DocStore, *Store) {
	t.Helper()
	st := testStore(t)
	root := t.TempDir()
	local := &localDocs{dir: root, store: st}
	b := &Bot{store: st, docs: local}
	return b, local.For(orgID), st
}

func put(t *testing.T, d DocStore, path, body string) {
	t.Helper()
	if err := d.Put(context.Background(), path, strings.NewReader(body), "U1", ""); err != nil {
		t.Fatalf("put %s: %v", path, err)
	}
}

func paths(docs []Document) []string {
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.Path)
	}
	return out
}

// A folder is a prefix of a document path, so the only thing the path cleaner has to get
// right is which prefixes are allowed: uploading a folder now sends multi-segment names, and
// one of those must never climb out of the organisation's own folder.
func TestCleanRelAcceptsFoldersAndRefusesEscapes(t *testing.T) {
	ok := map[string]string{
		"handbook.md":                "handbook.md",
		"policies/leave.md":          "policies/leave.md",
		"policies/2026/./leave.md":   "policies/2026/leave.md",
		"/policies//2026/leave.md":   "policies/2026/leave.md",
		"policies\\2026\\leave.md":   "policies/2026/leave.md",
		"policies/drafts/../done.md": "policies/done.md",
	}
	for in, want := range ok {
		got, err := cleanRel(in)
		if err != nil || got != want {
			t.Errorf("cleanRel(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// A path that climbs is clamped to the root of the organisation's folder rather than
	// refused: what matters is that the result never leaves it, and never names another
	// organisation's folder.
	for in, want := range map[string]string{
		"../secrets.md":                    "secrets.md",
		"policies/../../org-2/handbook.md": "org-2/handbook.md",
	} {
		got, err := cleanRel(in)
		if err != nil {
			t.Errorf("cleanRel(%q) = %v", in, err)
		} else if got != want || strings.HasPrefix(got, "..") {
			t.Errorf("cleanRel(%q) = %q, want %q inside the folder", in, got, want)
		}
	}
	for _, in := range []string{"", "..", "/", ".git/config.json", "policies/.hidden/x.md", ".env.json"} {
		if got, err := cleanRel(in); err == nil {
			t.Errorf("cleanRel(%q) = %q, want an error", in, got)
		}
	}
}

func TestParentFolders(t *testing.T) {
	got := parentFolders("policies/2026/leave.md")
	if len(got) != 2 || got[0] != "policies" || got[1] != "policies/2026" {
		t.Errorf("parentFolders = %v", got)
	}
	if got := parentFolders("handbook.md"); len(got) != 0 {
		t.Errorf("a document at the root has no parent folders, got %v", got)
	}
}

// Uploading into a folder keeps the shape, and the listing reports the path rather than just
// the name — that is what the console navigates by.
func TestPutAndListNested(t *testing.T) {
	ctx := context.Background()
	_, docs, _ := folderDocs(t)
	put(t, docs, "policies/2026/leave.md", "# Leave\n\nTake it.")
	put(t, docs, "handbook.md", "# Handbook")

	list, err := docs.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(list); len(got) != 2 || got[0] != "handbook.md" || got[1] != "policies/2026/leave.md" {
		t.Fatalf("List = %v", got)
	}
	for _, d := range list {
		if d.Name != filepath.Base(d.Path) {
			t.Errorf("%s: name = %q, want the base name", d.Path, d.Name)
		}
	}
}

// Moving keeps the file and its metadata; it does not quietly overwrite whatever was already
// at the destination.
func TestMoveDocument(t *testing.T) {
	ctx := context.Background()
	_, docs, st := folderDocs(t)
	put(t, docs, "leave.md", "# Leave")
	if err := st.SetDocumentScope(ctx, orgID, "leave.md", "C123"); err != nil {
		t.Fatal(err)
	}

	if err := docs.Move(ctx, "leave.md", "policies/leave.md"); err != nil {
		t.Fatalf("move: %v", err)
	}
	list, _ := docs.List(ctx)
	if got := paths(list); len(got) != 1 || got[0] != "policies/leave.md" {
		t.Fatalf("after the move List = %v", got)
	}
	if list[0].Scope != "C123" {
		t.Errorf("scope = %q, want the one the document already had", list[0].Scope)
	}
	body, err := docs.Get(ctx, "policies/leave.md")
	if err != nil {
		t.Fatalf("the moved file is not readable: %v", err)
	}
	body.Close()

	put(t, docs, "archive/leave.md", "# Older leave")
	if err := docs.Move(ctx, "archive/leave.md", "policies/leave.md"); err == nil {
		t.Error("a move onto an existing document was allowed and would have overwritten it")
	}
	if _, err := docs.Get(ctx, "archive/leave.md"); err != nil {
		t.Errorf("the refused move still took the source away: %v", err)
	}
	if err := docs.Move(ctx, "policies/leave.md", "policies/leave.exe"); err == nil {
		t.Error("a move to an unsupported type was allowed")
	}
	// A destination that climbs lands at the root of this organisation's folder, not outside it.
	if err := docs.Move(ctx, "policies/leave.md", "../../escaped.md"); err != nil {
		t.Fatalf("move: %v", err)
	}
	list, _ = docs.List(ctx)
	if got := paths(list); len(got) != 2 || got[1] != "escaped.md" {
		t.Fatalf("after a climbing move List = %v, want it clamped to escaped.md", got)
	}
}

// One organisation's move cannot reach another's file, because each view is rooted in its own
// folder before any path is cleaned.
func TestMoveStaysInsideTheOrganisation(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	root := t.TempDir()
	local := &localDocs{dir: root, store: st}
	acme, beta := local.For(1), local.For(2)
	put(t, acme, "handbook.md", "# Acme")
	put(t, beta, "handbook.md", "# Beta")

	if err := acme.Move(ctx, "handbook.md", "moved.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, orgFolder(2), "handbook.md")); err != nil {
		t.Errorf("the other organisation's file moved too: %v", err)
	}
	list, _ := beta.List(ctx)
	if got := paths(list); len(got) != 1 || got[0] != "handbook.md" {
		t.Errorf("Beta's documents = %v", got)
	}
}

// Folders come from two places — the ones somebody made in the console and the ones the
// documents imply — and the console needs the union of both, empty ones included.
func TestDocumentFoldersUnion(t *testing.T) {
	ctx := context.Background()
	b, docs, st := folderDocs(t)
	put(t, docs, "policies/2026/leave.md", "# Leave")
	if err := st.AddDocumentFolder(ctx, orgID, "runbooks"); err != nil {
		t.Fatal(err)
	}

	got, err := b.documentFolders(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"policies", "policies/2026", "runbooks"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("folders = %v, want %v", got, want)
	}

	// Deleting a folder takes the folders under it with it, and leaves the rest alone.
	if err := st.DeleteDocumentFolders(ctx, orgID, "policies"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddDocumentFolder(ctx, orgID, "runbooks/oncall"); err != nil {
		t.Fatal(err)
	}
	if err := st.MoveDocumentFolders(ctx, orgID, "runbooks", "ops"); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.DocumentFolders(ctx, orgID)
	if strings.Join(rows, ",") != "ops,ops/oncall" {
		t.Fatalf("after renaming runbooks the rows are %v", rows)
	}
}

// Folders are per organisation like everything else.
func TestDocumentFoldersAreIsolated(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	if err := st.AddDocumentFolder(ctx, 1, "acme-only"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddDocumentFolder(ctx, 2, "beta-only"); err != nil {
		t.Fatal(err)
	}
	got, _ := st.DocumentFolders(ctx, 1)
	if len(got) != 1 || got[0] != "acme-only" {
		t.Fatalf("Acme sees %v", got)
	}
	if err := st.DeleteDocumentFolders(ctx, 1, "acme-only"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.DocumentFolders(ctx, 2); len(got) != 1 {
		t.Fatalf("Beta's folders changed when Acme deleted one: %v", got)
	}
}

// Moving a folder onto one that already exists. SQLite's `update or replace` used to resolve
// this by deleting the row in the way; that clause exists in no other dialect, so the delete is
// explicit now, and this is what pins the behaviour it replaced. The unique index on
// (org_id, path) means the alternative is not a wrong answer but a failed move.
func TestMovingAFolderOntoAnExistingOneReplacesIt(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	const orgID = int64(1)
	for _, p := range []string{"runbooks", "runbooks/oncall", "ops", "ops/oncall", "untouched"} {
		if err := st.AddDocumentFolder(ctx, orgID, p); err != nil {
			t.Fatal(err)
		}
	}
	// runbooks -> ops, where every destination path is already taken.
	if err := st.MoveDocumentFolders(ctx, orgID, "runbooks", "ops"); err != nil {
		t.Fatalf("a move onto existing folders failed: %v", err)
	}
	rows, err := st.DocumentFolders(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rows, ","); got != "ops,ops/oncall,untouched" {
		t.Errorf("folders = %q, want %q", got, "ops,ops/oncall,untouched")
	}

	// And a move whose destination sits inside its own source must not delete what it is
	// moving: the rows being moved are excluded from the delete for exactly this case.
	if err := st.MoveDocumentFolders(ctx, orgID, "ops", "ops/archive"); err != nil {
		t.Fatalf("moving a folder beneath itself failed: %v", err)
	}
	rows, _ = st.DocumentFolders(ctx, orgID)
	if len(rows) != 3 {
		t.Errorf("a self-nested move lost rows: %v", rows)
	}
}
