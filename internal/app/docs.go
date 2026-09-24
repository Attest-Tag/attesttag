package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

// DocStore is where documents live. Locally that is the docs folder; on Cloud Run the folder
// is a read-only bucket mount, so writes go to the bucket through the API and show up in the
// mount for the indexer.
type DocStore interface {
	List(ctx context.Context) ([]Document, error)
	Get(ctx context.Context, relPath string) (io.ReadCloser, error)
	Put(ctx context.Context, name string, r io.Reader, by, scope string) error
	Delete(ctx context.Context, relPath string) error
	Exists(ctx context.Context, relPath string) (bool, error)
	// Move carries one document to another path, keeping its scope and who uploaded it, and
	// refuses to land on a path that is already taken. Moving a folder is this, once per
	// document under it.
	Move(ctx context.Context, from, to string) error
	// For returns a view holding one organisation's documents. The files themselves live under
	// a per-organisation folder, not just their rows: two organisations that both upload
	// "handbook.md" must not overwrite each other's copy.
	For(orgID int64) DocStore
}

// orgFolder is the path segment one organisation's documents live under. Derived from the id
// rather than the name, so renaming an organisation does not move its files.
func orgFolder(orgID int64) string { return "org-" + strconv.FormatInt(orgID, 10) }

// editableExts are the plain-text document types the console can open in its editor.
var editableExts = map[string]bool{".md": true, ".markdown": true, ".txt": true, ".rst": true, ".html": true, ".htm": true, ".csv": true, ".json": true}

func isEditableDoc(relPath string) bool {
	return editableExts[strings.ToLower(filepath.Ext(relPath))]
}

// docPathError is a path no document can have: one that climbs out of the organisation's folder
// or hides from the indexer. It is the caller's mistake rather than the server's, so fail answers
// it with a 400 — a hidden name used to come back as a 500 and an ERROR line in the log.
type docPathError struct{ msg string }

func (e docPathError) Error() string { return e.msg }

// cleanRel normalises a client-supplied relative path — uploads now carry the folder they
// came from, so a name can be several segments deep — and refuses anything that would climb
// out of the organisation's folder or hide from the indexer.
func cleanRel(p string) (string, error) {
	p = strings.ReplaceAll(p, "\\", "/")
	p = filepath.ToSlash(filepath.Clean("/" + p))[1:]
	if p == "" || strings.HasPrefix(p, "..") || strings.Contains(p, "/../") {
		return "", docPathError{"invalid path"}
	}
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, ".") {
			return "", docPathError{p + ": hidden files and folders are not indexed"}
		}
	}
	return p, nil
}

// parentFolders returns every folder a path sits under, shallowest first:
// "policies/2026/leave.md" -> ["policies", "policies/2026"].
func parentFolders(relPath string) []string {
	segs := strings.Split(relPath, "/")
	var out []string
	for i := 1; i < len(segs); i++ {
		out = append(out, strings.Join(segs[:i], "/"))
	}
	return out
}

// cleanFolder normalises a folder path. The empty string is the root, and means the folder
// people see when they open Documents rather than an error.
func cleanFolder(p string) (string, error) {
	if strings.Trim(p, "/ ") == "" {
		return "", nil
	}
	return cleanRel(p)
}

// documentFolders lists every folder in one organisation: the ones somebody made in the
// console, plus the ones implied by where the documents actually are.
func (b *Bot) documentFolders(ctx context.Context, orgID int64) ([]string, error) {
	made, err := b.store.DocumentFolders(ctx, orgID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, f := range made {
		seen[f] = true
	}
	docs, err := b.docs.For(orgID).List(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range docs {
		for _, f := range parentFolders(d.Path) {
			seen[f] = true
		}
	}
	out := make([]string, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	sort.Strings(out)
	return out, nil
}

// uploadDocuments stores the files of one multipart upload and re-indexes. The console's Upload
// button and POST /v1/documents both send one and both end here, so the two doors cannot disagree
// about what a document may be called or which types the bot reads. by is who the documents are
// recorded against.
//
// Uploading a folder keeps its shape. A part's filename cannot carry the folder — RFC 7578 says
// the directory must not be used, and Go's reader duly strips it — so the path each file keeps
// travels alongside in "paths", one per file in the same order. "folder" is where in the tree the
// whole batch lands.
func (b *Bot) uploadDocuments(w http.ResponseWriter, r *http.Request, by string) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"error": fmt.Sprintf("an upload may be %d MB at most; send the files in more than one request", tooBig.Limit>>20)})
			return
		}
		bad(w, err)
		return
	}
	scope := r.FormValue("scope")
	into, err := cleanFolder(r.FormValue("folder"))
	if err != nil {
		bad(w, err)
		return
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		bad(w, errors.New(`no files: send each one as a part named "files"`))
		return
	}
	// Every name is settled before anything is written, so a batch refused for one file does not
	// leave the files ahead of it stored behind the refusal.
	rel := r.MultipartForm.Value["paths"]
	names := make([]string, len(files))
	for i, fh := range files {
		given := fh.Filename
		if i < len(rel) && rel[i] != "" {
			given = rel[i]
		}
		name, err := cleanRel(path.Join(into, given))
		if err != nil {
			bad(w, err)
			return
		}
		if !docExts[strings.ToLower(filepath.Ext(name))] {
			bad(w, fmt.Errorf("%s: unsupported type; the bot reads %s", path.Base(name), docExtList()))
			return
		}
		names[i] = name
	}
	docs := b.docs.For(orgOf(r))
	for i, fh := range files {
		f, err := fh.Open()
		if err != nil {
			bad(w, err)
			return
		}
		err = docs.Put(r.Context(), names[i], f, by, scope)
		f.Close()
		if err != nil {
			fail(w, err)
			return
		}
		// A folder the upload brought with it is a folder the console should keep, even after
		// the last file in it is deleted again.
		for _, folder := range parentFolders(names[i]) {
			b.store.AddDocumentFolder(r.Context(), orgOf(r), folder)
		}
	}
	go b.reindex(context.WithoutCancel(r.Context()), orgOf(r))
	b.audit(r, "document.uploaded", AuditEvent{TargetKind: "document", TargetName: strings.Join(names, ", "),
		Details: auditDetails(map[string]any{"count": len(names), "folder": into, "scope": scope})})
	writeJSON(w, 201, map[string]any{"ok": true, "saved": names, "indexing": true})
}

// docExtList is the types the indexer reads, for an error that says which ones it would take.
func docExtList() string {
	exts := make([]string, 0, len(docExts))
	for e := range docExts {
		exts = append(exts, e)
	}
	sort.Strings(exts)
	return strings.Join(exts, " ")
}

// ---- local folder ----

type localDocs struct {
	dir   string
	store *Store
	orgID int64
}

func (l *localDocs) For(orgID int64) DocStore {
	return &localDocs{dir: filepath.Join(l.dir, orgFolder(orgID)), store: l.store, orgID: orgID}
}

func (l *localDocs) List(ctx context.Context) ([]Document, error) {
	meta, _ := l.store.Documents(ctx, l.orgID)
	byPath := map[string]Document{}
	for _, d := range meta {
		byPath[d.Path] = d
	}
	var out []Document
	seen := map[string]bool{}
	// A walk error must not be swallowed. What follows this loop prunes metadata for every
	// document the walk did not see, so a directory that could not be read — a permissions
	// change, a bad mount, a bucket briefly unavailable — used to be indistinguishable from
	// "the tenant deleted everything", and their entire document index went with it.
	walkErr := filepath.WalkDir(l.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".") || !docExts[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		rel, _ := filepath.Rel(l.dir, p)
		rel = filepath.ToSlash(rel)
		info, _ := d.Info()
		doc := byPath[rel]
		doc.Path, doc.Name = rel, d.Name()
		if info != nil {
			doc.Size = info.Size()
			if doc.UpdatedAt == "" {
				doc.UpdatedAt = info.ModTime().UTC().Format(time.DateTime)
			}
		}
		if doc.Status == "" {
			doc.Status = "pending"
		}
		seen[rel] = true
		out = append(out, doc)
		return nil
	})
	// A missing directory is simply an organisation with no documents yet, and pruning is right.
	// Any other error means the listing is incomplete, so prune nothing and say so.
	if walkErr != nil && !errors.Is(walkErr, fs.ErrNotExist) {
		return nil, fmt.Errorf("could not read the documents folder: %w", walkErr)
	}
	// metadata rows whose file vanished
	for _, d := range meta {
		if !seen[d.Path] {
			l.store.DeleteDocument(ctx, l.orgID, d.Path)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if out == nil {
		out = []Document{}
	}
	return out, nil
}

func (l *localDocs) Get(ctx context.Context, relPath string) (io.ReadCloser, error) {
	rel, err := cleanRel(relPath)
	if err != nil {
		return nil, err
	}
	return os.Open(filepath.Join(l.dir, filepath.FromSlash(rel)))
}

func (l *localDocs) Put(ctx context.Context, name string, r io.Reader, by, scope string) error {
	rel, err := cleanRel(name)
	if err != nil {
		return err
	}
	dst := filepath.Join(l.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, r)
	f.Close()
	if err != nil {
		return err
	}
	return l.store.UpsertDocument(ctx, l.orgID, Document{Path: rel, Name: filepath.Base(rel), Size: n, UploadedBy: by, Scope: scope, Status: "pending"})
}

func (l *localDocs) Delete(ctx context.Context, relPath string) error {
	rel, err := cleanRel(relPath)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(l.dir, filepath.FromSlash(rel))); err != nil && !os.IsNotExist(err) {
		return err
	}
	return l.store.DeleteDocument(ctx, l.orgID, rel)
}

func (l *localDocs) Exists(ctx context.Context, relPath string) (bool, error) {
	rel, err := cleanRel(relPath)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(filepath.Join(l.dir, filepath.FromSlash(rel)))
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func (l *localDocs) Move(ctx context.Context, from, to string) error {
	src, dst, err := moveEnds(ctx, l, from, to)
	if err != nil {
		return err
	}
	abs := filepath.Join(l.dir, filepath.FromSlash(dst))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(l.dir, filepath.FromSlash(src)), abs); err != nil {
		return err
	}
	return l.store.MoveDocument(ctx, l.orgID, src, dst)
}

// ---- GCS bucket (Cloud Run) ----

type gcsDocs struct {
	bucket, prefix string
	store          *Store
	client         *storage.Client
	orgID          int64
}

func (g *gcsDocs) For(orgID int64) DocStore {
	return &gcsDocs{bucket: g.bucket, prefix: g.prefix + orgFolder(orgID) + "/", store: g.store, client: g.client, orgID: orgID}
}

func newGCSDocs(ctx context.Context, bucket, prefix string, st *Store) (*gcsDocs, error) {
	c, err := storage.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return &gcsDocs{bucket: bucket, prefix: prefix, store: st, client: c}, nil
}

func (g *gcsDocs) List(ctx context.Context) ([]Document, error) {
	meta, _ := g.store.Documents(ctx, g.orgID)
	byPath := map[string]Document{}
	for _, d := range meta {
		byPath[d.Path] = d
	}
	it := g.client.Bucket(g.bucket).Objects(ctx, &storage.Query{Prefix: g.prefix})
	var out []Document
	seen := map[string]bool{}
	for {
		obj, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		rel := strings.TrimPrefix(obj.Name, g.prefix)
		if rel == "" || strings.HasSuffix(rel, "/") || !docExts[strings.ToLower(filepath.Ext(rel))] {
			continue
		}
		doc := byPath[rel]
		doc.Path, doc.Name, doc.Size = rel, filepath.Base(rel), obj.Size
		if doc.UpdatedAt == "" {
			doc.UpdatedAt = obj.Updated.UTC().Format(time.DateTime)
		}
		if doc.Status == "" {
			doc.Status = "pending"
		}
		seen[rel] = true
		out = append(out, doc)
	}
	for _, d := range meta {
		if !seen[d.Path] {
			g.store.DeleteDocument(ctx, g.orgID, d.Path)
		}
	}
	if out == nil {
		out = []Document{}
	}
	return out, nil
}

func (g *gcsDocs) Get(ctx context.Context, relPath string) (io.ReadCloser, error) {
	rel, err := cleanRel(relPath)
	if err != nil {
		return nil, err
	}
	return g.client.Bucket(g.bucket).Object(g.prefix + rel).NewReader(ctx)
}

func (g *gcsDocs) Put(ctx context.Context, name string, r io.Reader, by, scope string) error {
	rel, err := cleanRel(name)
	if err != nil {
		return err
	}
	w := g.client.Bucket(g.bucket).Object(g.prefix + rel).NewWriter(ctx)
	n, err := io.Copy(w, r)
	if err != nil {
		w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return g.store.UpsertDocument(ctx, g.orgID, Document{Path: rel, Name: filepath.Base(rel), Size: n, UploadedBy: by, Scope: scope, Status: "pending"})
}

func (g *gcsDocs) Delete(ctx context.Context, relPath string) error {
	rel, err := cleanRel(relPath)
	if err != nil {
		return err
	}
	if err := g.client.Bucket(g.bucket).Object(g.prefix + rel).Delete(ctx); err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
		return err
	}
	return g.store.DeleteDocument(ctx, g.orgID, rel)
}

func (g *gcsDocs) Exists(ctx context.Context, relPath string) (bool, error) {
	rel, err := cleanRel(relPath)
	if err != nil {
		return false, err
	}
	_, err = g.client.Bucket(g.bucket).Object(g.prefix + rel).Attrs(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return false, nil
	}
	return err == nil, err
}

// A bucket has no rename, so a move is a copy followed by a delete. If the delete fails the
// document is listed twice rather than lost, which is the better half of that trade.
func (g *gcsDocs) Move(ctx context.Context, from, to string) error {
	src, dst, err := moveEnds(ctx, g, from, to)
	if err != nil {
		return err
	}
	b := g.client.Bucket(g.bucket)
	if _, err := b.Object(g.prefix + dst).CopierFrom(b.Object(g.prefix + src)).Run(ctx); err != nil {
		return err
	}
	if err := b.Object(g.prefix + src).Delete(ctx); err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
		return err
	}
	return g.store.MoveDocument(ctx, g.orgID, src, dst)
}

// moveEnds cleans both ends of a move and checks the destination is free.
func moveEnds(ctx context.Context, d DocStore, from, to string) (string, string, error) {
	src, err := cleanRel(from)
	if err != nil {
		return "", "", err
	}
	dst, err := cleanRel(to)
	if err != nil {
		return "", "", err
	}
	if src == dst {
		return "", "", errors.New("that document is already there")
	}
	if !docExts[strings.ToLower(filepath.Ext(dst))] {
		return "", "", fmt.Errorf("%s: unsupported type", filepath.Base(dst))
	}
	taken, err := d.Exists(ctx, dst)
	if err != nil {
		return "", "", err
	}
	if taken {
		return "", "", fmt.Errorf("%s already exists", dst)
	}
	return src, dst, nil
}

// IngestAndRecord runs the indexer and writes per-document status into the documents table.
func (ix *Indexer) IngestAndRecord(ctx context.Context, orgID int64, docs DocStore) (IngestReport, error) {
	rep, err := ix.Ingest(ctx, orgID, docs)
	if err != nil {
		return rep, err
	}
	errByFile := map[string]string{}
	for _, e := range rep.Errors {
		if i := strings.Index(e, ": "); i > 0 {
			errByFile[e[:i]] = e[i+2:]
		}
	}
	counts, _ := ix.store.ChunkCountsByDoc(ctx, orgID)
	list, _ := docs.List(ctx)
	for _, d := range list {
		status, chunks, lastErr := "indexed", counts["local:"+d.Path], ""
		if e, ok := errByFile[d.Path]; ok {
			status, lastErr = "error", e
		} else if chunks == 0 {
			status = "pending"
		}
		ix.store.UpsertDocument(ctx, orgID, Document{Path: d.Path, Name: d.Name, Size: d.Size, Status: status, Chunks: chunks, LastError: lastErr})
	}
	return rep, nil
}

// docStoreReady says whether an ingest pass has anywhere to read from. A bucket always has: it is
// asked through its API, and DOCS_DIR means nothing to it, so waiting on that folder left every
// organisation unindexed on a deployment whose documents are in S3 or GCS and whose DOCS_DIR was
// never made. A local folder that does not exist yet still waits. It is a deployment with no
// documents, or a mount that failed — and a pass over a failed mount would read every
// organisation as empty and prune its index.
func docStoreReady(d DocStore) bool {
	if l, ok := d.(*localDocs); ok {
		_, err := os.Stat(l.dir)
		return err == nil
	}
	return d != nil
}

func describeDocStore(d DocStore) string {
	switch v := d.(type) {
	case *gcsDocs:
		return fmt.Sprintf("gs://%s/%s", v.bucket, v.prefix)
	case *s3Docs:
		return fmt.Sprintf("s3://%s/%s", v.bucket, v.prefix)
	case *localDocs:
		return v.dir
	}
	return "?"
}

// docDisposition is how a document's bytes are handed to a browser. Text is inline for the
// console's editor; anything a browser would render as active content — HTML, SVG, XML — is
// an attachment, so a document somebody uploaded can never run as a page on the console's own
// origin, where the session cookie lives.
func docDisposition(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".html", ".htm", ".svg", ".xml", ".xhtml":
		return "attachment"
	}
	return "inline"
}
