package app

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 is enough of the S3 object API for the six requests this backend makes: list, get,
// put, delete, head, and put-with-copy-source. It checks every request is signed, because an
// unsigned request working against a permissive fake is exactly the bug that would then fail
// against a real bucket.
// It also keeps an ETag per object and enforces the two conditional headers, because the write
// lease (lease_s3.go) is built out of them and a fake that ignored them would prove nothing.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	etags   map[string]string
	seq     int
	seen    []string // method + path, for asserting what was actually called

	// A store that accepts `If-None-Match: *` and writes anyway. Conditional writes came late
	// to S3 and some compatible stores still do not have them; this is the behaviour that
	// would otherwise hand one lease to two writers.
	ignorePreconditions bool
	// And the half-implemented case: create-if-absent enforced, replace-if-unchanged ignored,
	// which would let a stale holder overwrite a live one on its next renewal.
	ignoreIfMatch bool
	// AWS ignores If-Match on DELETE; R2 enforces it. Off by default, so the default fake is
	// the weaker of the two real behaviours.
	enforceDeleteCondition bool
	// Whether the bucket itself exists, which only `attesttag bucket-init` asks about: HEAD on
	// the bucket answers 404 until something has created it.
	bucketExists bool
}

func newFakeS3(t *testing.T) (*fakeS3, string) {
	t.Helper()
	f := &fakeS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func (f *fakeS3) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.objects))
	for k := range f.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// store records a new version of a key and returns its ETag. Callers hold the lock.
func (f *fakeS3) store(key string, body []byte) string {
	f.seq++
	if f.etags == nil {
		f.etags = map[string]string{}
	}
	etag := fmt.Sprintf("%q", fmt.Sprintf("v%d", f.seq))
	f.objects[key], f.etags[key] = body, etag
	return etag
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		http.Error(w, "unsigned request", http.StatusForbidden)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, r.Method+" "+r.URL.Path)
	// /bucket/key…
	rest := strings.TrimPrefix(r.URL.Path, "/")
	_, key, hasKey := strings.Cut(rest, "/")

	// …and /bucket on its own, which is a request about the bucket rather than about a key.
	if !hasKey && r.URL.RawQuery == "" {
		switch r.Method {
		case "HEAD", "GET":
			if !f.bucketExists {
				w.WriteHeader(404)
				return
			}
			w.WriteHeader(200)
		case "PUT":
			if f.bucketExists {
				http.Error(w, "BucketAlreadyOwnedByYou", http.StatusConflict)
				return
			}
			f.bucketExists = true
			w.WriteHeader(200)
		default:
			http.Error(w, "unexpected "+r.Method+" on the bucket", 400)
		}
		return
	}

	if r.Method == "GET" && r.URL.Query().Get("list-type") == "2" {
		prefix := r.URL.Query().Get("prefix")
		type obj struct {
			Key          string `xml:"Key"`
			Size         int64  `xml:"Size"`
			LastModified string `xml:"LastModified"`
		}
		var out struct {
			XMLName   xml.Name `xml:"ListBucketResult"`
			Contents  []obj    `xml:"Contents"`
			Truncated bool     `xml:"IsTruncated"`
		}
		var keys []string
		for k := range f.objects {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			out.Contents = append(out.Contents, obj{Key: k, Size: int64(len(f.objects[k])), LastModified: "2026-09-14T10:00:00Z"})
		}
		w.Header().Set("Content-Type", "application/xml")
		xml.NewEncoder(w).Encode(out)
		return
	}
	switch r.Method {
	case "GET":
		b, ok := f.objects[key]
		if !ok {
			http.Error(w, "NoSuchKey", 404)
			return
		}
		w.Header().Set("ETag", f.etags[key])
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.Write(b)
	case "HEAD":
		if _, ok := f.objects[key]; !ok {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("ETag", f.etags[key])
		w.WriteHeader(200)
	case "PUT":
		if !f.ignorePreconditions {
			_, exists := f.objects[key]
			if r.Header.Get("If-None-Match") == "*" && exists {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			if m := r.Header.Get("If-Match"); m != "" && !f.ignoreIfMatch && (!exists || m != f.etags[key]) {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
		}
		if src := r.Header.Get("x-amz-copy-source"); src != "" {
			_, from, _ := strings.Cut(strings.TrimPrefix(src, "/"), "/")
			b, ok := f.objects[from]
			if !ok {
				http.Error(w, "NoSuchKey", 404)
				return
			}
			f.store(key, b)
			fmt.Fprint(w, `<CopyObjectResult/>`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("ETag", f.store(key, b))
		w.WriteHeader(200)
	case "DELETE":
		if m := r.Header.Get("If-Match"); m != "" && f.enforceDeleteCondition && m != f.etags[key] {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		delete(f.objects, key)
		delete(f.etags, key)
		w.WriteHeader(204)
	default:
		http.Error(w, "unexpected "+r.Method, 400)
	}
}

func s3Fixture(t *testing.T) (*s3Docs, *fakeS3) {
	t.Helper()
	f, url := newFakeS3(t)
	st := testStore(t)
	sd, err := newS3Docs("s3://my-bucket/docs?endpoint="+url+"&region=auto", "AKIATEST", "secret", st)
	if err != nil {
		t.Fatal(err)
	}
	return sd.For(1).(*s3Docs), f
}

// The round trip every other backend already passes: put, list, read back, move, delete.
func TestS3DocsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, f := s3Fixture(t)

	if err := s.Put(ctx, "handbook.md", strings.NewReader("# Handbook\nhello"), "someone", ""); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Filed under the organisation's own prefix — two tenants uploading "handbook.md" must not
	// overwrite each other, which is what For(orgID) is for.
	if _, ok := f.objects["docs/org-1/handbook.md"]; !ok {
		t.Fatalf("stored under the wrong key: %v", s3Keys(f))
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Path != "handbook.md" || list[0].Name != "handbook.md" {
		t.Fatalf("list = %+v", list)
	}
	if list[0].Size != 16 || list[0].Status != "pending" {
		t.Errorf("size/status = %d/%q", list[0].Size, list[0].Status)
	}

	rc, err := s.Get(ctx, "handbook.md")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "# Handbook\nhello" {
		t.Errorf("read back %q", b)
	}

	if ok, _ := s.Exists(ctx, "handbook.md"); !ok {
		t.Error("Exists said no about a document that is there")
	}
	if ok, _ := s.Exists(ctx, "nothing.md"); ok {
		t.Error("Exists said yes about a document that is not")
	}

	// Move is a copy and a delete, and must refuse to land on an occupied path.
	if err := s.Put(ctx, "taken.md", strings.NewReader("x"), "someone", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Move(ctx, "handbook.md", "taken.md"); err == nil {
		t.Error("a move onto an existing document was allowed")
	}
	if err := s.Move(ctx, "handbook.md", "policies/handbook.md"); err != nil {
		t.Fatalf("move: %v", err)
	}
	if _, ok := f.objects["docs/org-1/policies/handbook.md"]; !ok {
		t.Errorf("the moved document is not at its new key: %v", s3Keys(f))
	}
	if _, ok := f.objects["docs/org-1/handbook.md"]; ok {
		t.Error("the old key survived the move")
	}

	if err := s.Delete(ctx, "policies/handbook.md"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := f.objects["docs/org-1/policies/handbook.md"]; ok {
		t.Error("delete left the object behind")
	}
}

// A path from a request must not reach another organisation's documents. An object store has
// no directory to confine it, so cleanRel is the only thing between a crafted path and another
// tenant's files — and what it does is neutralise rather than refuse: "../org-2/x" cleans to
// "org-2/x", which lands harmlessly inside this organisation's own prefix. Confinement is
// therefore the property worth asserting, not rejection.
func TestS3DocsConfinesAPathToItsOrganisation(t *testing.T) {
	ctx := context.Background()
	s, f := s3Fixture(t)
	// Something to try and reach, filed under another organisation.
	f.objects["docs/org-2/secrets.md"] = []byte("another tenant's")
	before := map[string]bool{}
	for _, k := range s3Keys(f) {
		before[k] = true
	}

	for _, crafted := range []string{"../org-2/secrets.md", "a/../../org-2/secrets.md", "../../../../etc/passwd"} {
		// Reading is confined: whatever it resolves to is under this organisation's prefix, so
		// the other tenant's document is not what comes back.
		if rc, err := s.Get(ctx, crafted); err == nil {
			b, _ := io.ReadAll(rc)
			rc.Close()
			t.Errorf("Get(%q) returned %q", crafted, b)
		}
		// Writing is confined too, which is the half that would be destructive.
		if err := s.Put(ctx, crafted, strings.NewReader("overwritten"), "attacker", ""); err == nil {
			for _, k := range s3Keys(f) {
				if !before[k] && !strings.HasPrefix(k, "docs/org-1/") {
					t.Errorf("Put(%q) created %q, outside this organisation", crafted, k)
				}
			}
		}
		if err := s.Delete(ctx, crafted); err != nil {
			continue // refused outright is also fine
		}
	}
	// The other organisation's document is untouched, however it was asked for.
	if string(f.objects["docs/org-2/secrets.md"]) != "another tenant's" {
		t.Error("another organisation's document was reached")
	}
	// And a hidden file is refused outright, because it is not a document.
	if _, err := s.Get(ctx, ".env"); err == nil {
		t.Error("a dot-file was served as a document")
	}
}

// Only documents. A bucket holds whatever somebody put in it, and a directory marker is not a
// file at all.
func TestS3DocsListsOnlyDocuments(t *testing.T) {
	ctx := context.Background()
	s, f := s3Fixture(t)
	f.objects["docs/org-1/"] = nil                  // directory marker
	f.objects["docs/org-1/notes.md"] = []byte("a")  // yes
	f.objects["docs/org-1/photo.png"] = []byte("b") // not a document type
	f.objects["docs/org-2/other.md"] = []byte("c")  // another organisation
	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Path != "notes.md" {
		t.Fatalf("list = %+v", list)
	}
}

// The configuration is one URL and two secrets, and a mistake in it should say so at boot
// rather than at the first upload.
func TestS3DocsConfig(t *testing.T) {
	st := testStore(t)
	if _, err := newS3Docs("s3://b/p", "", "secret", st); err == nil {
		t.Error("missing key id was accepted")
	}
	if _, err := newS3Docs("https://not-s3/b", "k", "s", st); err == nil {
		t.Error("a non-s3 URL was accepted")
	}
	// AWS needs no endpoint; it comes from the region.
	sd, err := newS3Docs("s3://b/p?region=eu-west-2", "k", "s", st)
	if err != nil {
		t.Fatal(err)
	}
	if sd.endpoint != "https://s3.eu-west-2.amazonaws.com" || sd.bucket != "b" || sd.prefix != "p" {
		t.Errorf("endpoint=%q bucket=%q prefix=%q", sd.endpoint, sd.bucket, sd.prefix)
	}
	// A provider's own endpoint is kept, and a bare host is assumed to be https.
	sd, _ = newS3Docs("s3://b?endpoint=acct.r2.cloudflarestorage.com&region=auto", "k", "s", st)
	if sd.endpoint != "https://acct.r2.cloudflarestorage.com" || sd.cred.AWSRegion != "auto" {
		t.Errorf("endpoint=%q region=%q", sd.endpoint, sd.cred.AWSRegion)
	}
}

// The six-hourly pass asks the document store, not the machine's DOCS_DIR. It used to skip every
// organisation while that folder was missing, and where the documents are in a bucket nothing ever
// creates it. A local folder that does not exist still holds the pass back, because there it means
// no documents or a failed mount, and the second would be read as every organisation having
// deleted everything.
func TestTheIngestLoopReadsABucketWhateverDocsDirSays(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f, url := newFakeS3(t)
	st := testStore(t)
	org := testOrg(t, st, "Bucket Ltd")
	if err := st.SaveTeam(ctx, &Team{TeamID: "T_BUCKET", OrgID: org.ID, Name: "Bucket"}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	sd, err := newS3Docs("s3://my-bucket/docs?endpoint="+url+"&region=auto", "AKIATEST", "secret", st)
	if err != nil {
		t.Fatal(err)
	}
	// Straight into the bucket, as a sync or another instance would put it: no row yet, so the only
	// way one appears is a pass listing it.
	f.mu.Lock()
	f.store(fmt.Sprintf("docs/org-%d/handbook.md", org.ID), []byte("# Handbook\nhello"))
	f.mu.Unlock()

	missing := filepath.Join(t.TempDir(), "never-made")
	b := &Bot{cfg: Config{DocsDir: missing}, store: st, docs: sd, ix: NewIndexer(nil, st, missing)}
	done := make(chan struct{})
	go func() { defer close(done); b.ingestLoop(ctx) }()
	defer func() { cancel(); <-done }() // the pass must be over before the store closes

	deadline := time.Now().Add(10 * time.Second)
	for {
		if docs, _ := st.Documents(ctx, org.ID); len(docs) == 1 && docs[0].Path == "handbook.md" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ingest loop never listed the bucket while DOCS_DIR (%s) did not exist", missing)
		}
		time.Sleep(20 * time.Millisecond)
	}

	local := &localDocs{dir: missing, store: st}
	if docStoreReady(local) {
		t.Error("a local folder that does not exist was read as ready to ingest")
	}
	if err := os.MkdirAll(missing, 0o755); err != nil {
		t.Fatal(err)
	}
	if !docStoreReady(local) {
		t.Error("a local folder that exists was held back")
	}
}

func s3Keys(f *fakeS3) []string {
	var k []string
	for n := range f.objects {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}
