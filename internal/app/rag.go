package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Local-folder RAG: walk DOCS_DIR, split into ~500-token chunks that keep their heading,
// embed via the same OpenAI-compatible endpoint, store vectors in SQLite. Retrieval is
// brute-force cosine over all chunks (fine for thousands of chunks; pgvector later).

var docExts = map[string]bool{".md": true, ".markdown": true, ".txt": true, ".rst": true, ".pdf": true, ".html": true, ".htm": true, ".csv": true, ".json": true}

type Indexer struct {
	// llm is the deployment's own endpoint; endpoints sends an organisation that brought its own
	// key there instead (embedderFor).
	llm       *LLM
	endpoints *modelEndpoints
	store     *Store
	dir       string
	mu        sync.Mutex
	// The search cache is keyed by organisation. A single slice here would be the same leak as
	// an unfiltered query: whichever organisation warmed it would answer everybody's questions.
	cache    map[int64][]DocChunk
	cachedAt map[int64]time.Time
	// docs is where documents are read from, for callers that reach the indexer without one to
	// hand — the !ingest command comes through the agent, which has no DocStore. Set once at
	// boot; Ingest itself always takes the store explicitly.
	docs   DocStore
	scopes func(ctx context.Context, orgID int64) map[string]string // doc rel path → channel scope ('' = everyone)

	// Ingest is serialised per organisation, not process-wide. It walks a folder, makes embedding
	// round-trips and writes to SQLite, and it used to hold the same mutex that guards the search
	// cache for all of it — so one tenant re-indexing a large corpus blocked every other tenant's
	// searches for the duration. mu is now only ever held for the cache itself.
	ingestMu   sync.Mutex
	ingestLock map[int64]*sync.Mutex
	// kicked is the organisations a background re-index is running for (kick), under mu.
	kicked map[int64]bool
}

// docCacheTTL bounds how stale one instance's copy of a corpus may be. A minute, because the
// cost of reloading is one query and the cost of being stale is the bot telling somebody it
// cannot find a document they just uploaded.
const docCacheTTL = time.Minute

func NewIndexer(llm *LLM, st *Store, dir string) *Indexer {
	return &Indexer{llm: llm, store: st, dir: dir, cache: map[int64][]DocChunk{},
		cachedAt: map[int64]time.Time{}, ingestLock: map[int64]*sync.Mutex{}}
}

// ingestGate returns the lock that serialises ingests for one organisation.
func (ix *Indexer) ingestGate(orgID int64) *sync.Mutex {
	ix.ingestMu.Lock()
	defer ix.ingestMu.Unlock()
	if ix.ingestLock == nil {
		ix.ingestLock = map[int64]*sync.Mutex{}
	}
	m, ok := ix.ingestLock[orgID]
	if !ok {
		m = &sync.Mutex{}
		ix.ingestLock[orgID] = m
	}
	return m
}

// orgDir is where one organisation's local documents live, matching DocStore.For.
func (ix *Indexer) orgDir(orgID int64) string { return filepath.Join(ix.dir, orgFolder(orgID)) }

// Forget drops one organisation's cached chunks, so the next search reloads them.
func (ix *Indexer) Forget(orgID int64) {
	ix.mu.Lock()
	delete(ix.cache, orgID)
	delete(ix.cachedAt, orgID)
	ix.mu.Unlock()
}

// readDocBytes turns one document's bytes into the text that gets indexed. It takes the name
// only to know the extension — a document that came out of a bucket has no path on this
// machine, which is the whole reason this is not readDoc(path) any more.
//
// PDFs still go through pdftotext, which reads a file, so the bytes are spilled to a temp file
// and removed. That is the one format where this costs anything, and the alternative is
// piping to its stdin, which its -layout mode does not do reliably.
// maxPDFTextBytes caps the text one PDF may yield. pdftotext can expand a small compressed PDF
// into gigabytes, and .Output() buffers the whole stream, so an attacker-supplied file is a way
// to run the single shared instance out of memory. A real document extracts to a few MB, so this
// ceiling stops the bomb without truncating anything genuine, and the process is killed the moment
// it goes over.
const maxPDFTextBytes = 32 << 20

// pdfToText extracts a PDF's text with a time limit and a hard cap on how much is read back. It is
// the one road from a PDF file to text, so the bound is in one place.
func pdfToText(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pdftotext", "-layout", path, "-")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	out, readErr := io.ReadAll(io.LimitReader(stdout, maxPDFTextBytes+1)) // +1 tells a bomb from a file exactly at the cap
	over := len(out) > maxPDFTextBytes
	if over {
		cancel() // stop pdftotext writing more before we wait on it
	}
	waitErr := cmd.Wait()
	switch {
	case over:
		return "", fmt.Errorf("pdftotext: output over %d bytes", maxPDFTextBytes)
	case waitErr != nil:
		return "", fmt.Errorf("pdftotext: %w", waitErr)
	case readErr != nil:
		return "", fmt.Errorf("pdftotext: %w", readErr)
	}
	return string(out), nil
}

func readDocBytes(ctx context.Context, name string, b []byte) (string, error) {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".pdf":
		f, err := os.CreateTemp("", "attesttag-*.pdf")
		if err != nil {
			return "", err
		}
		defer os.Remove(f.Name())
		if _, err := f.Write(b); err != nil {
			f.Close()
			return "", err
		}
		f.Close()
		return pdfToText(ctx, f.Name())
	case ".html", ".htm":
		return cleanHTML(string(b)), nil
	}
	return string(b), nil
}

// readDocument fetches one document and turns it into text. The whole body is read into memory
// because every format below needs it whole — chunking wants the text, pdftotext wants a file,
// and the HTML cleaner wants the document. Documents here are prose, not archives.
func readDocument(ctx context.Context, docs DocStore, rel string) (string, error) {
	rc, err := docs.Get(ctx, rel)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return "", err
	}
	return readDocBytes(ctx, path.Base(rel), b)
}

// chunk splits text into ~2000-char pieces (≈500 tokens) on paragraph boundaries, tracking the
// nearest Markdown heading so each chunk carries its context.
func chunk(text string) []DocChunk {
	const target, maxLen = 1800, 2600
	var out []DocChunk
	heading := ""
	var cur strings.Builder
	flush := func() {
		t := strings.TrimSpace(cur.String())
		if len(t) > 40 {
			out = append(out, DocChunk{Heading: heading, Text: t})
		}
		cur.Reset()
	}
	for _, para := range strings.Split(text, "\n\n") {
		p := strings.TrimSpace(para)
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, "#") && !strings.Contains(p, "\n") {
			flush()
			heading = strings.TrimSpace(strings.TrimLeft(p, "# "))
			cur.WriteString(p + "\n\n")
			continue
		}
		for len(p) > maxLen { // very long paragraph: hard split
			cut := strings.LastIndex(p[:maxLen], ". ")
			if cut < maxLen/2 {
				cut = maxLen
			} else {
				cut++
			}
			cur.WriteString(p[:cut])
			flush()
			p = strings.TrimSpace(p[cut:])
		}
		if cur.Len()+len(p) > target && cur.Len() > 0 {
			flush()
			if heading != "" {
				cur.WriteString("# " + heading + "\n\n")
			}
		}
		cur.WriteString(p + "\n\n")
	}
	flush()
	return out
}

func hashOf(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:8])
}

type IngestReport struct {
	Docs, Chunks, Embedded, Unchanged, Deleted int
	Errors                                     []string
	Took                                       time.Duration
}

// SetDocs gives the indexer a default document store, for IngestOrg.
func (ix *Indexer) SetDocs(d DocStore) {
	ix.mu.Lock()
	ix.docs = d
	ix.mu.Unlock()
}

// IngestOrg indexes one organisation from the indexer's own store.
func (ix *Indexer) IngestOrg(ctx context.Context, orgID int64) (IngestReport, error) {
	ix.mu.Lock()
	d := ix.docs
	ix.mu.Unlock()
	if d == nil {
		return IngestReport{}, fmt.Errorf("no document store configured")
	}
	return ix.Ingest(ctx, orgID, d.For(orgID))
}

// Ingest indexes one organisation's documents. Docs whose chunks are unchanged are skipped
// (hash per chunk).
//
// It reads through the DocStore rather than walking a directory. That was not a refactor for
// its own sake: walking the filesystem only worked because Cloud Run FUSE-mounts a bucket at
// DOCS_DIR, and there is no equivalent on Fargate, Container Apps or Cloudflare Containers —
// so on every platform without a FUSE mount the indexer found an empty directory and the bot
// answered that it had no documents. The DocStore already knew how to list and fetch them.
func (ix *Indexer) Ingest(ctx context.Context, orgID int64, docs DocStore) (IngestReport, error) {
	gate := ix.ingestGate(orgID)
	gate.Lock()
	defer gate.Unlock()
	start := time.Now()
	var rep IngestReport
	keep := map[string]bool{}
	if docs == nil {
		return rep, fmt.Errorf("no document store configured")
	}
	list, err := docs.List(ctx)
	if err != nil {
		return rep, fmt.Errorf("listing documents: %w", err)
	}
	// The endpoint first, and the identity from it: which chunks count as current is decided by the
	// client that will embed the rest, never by a second read that could see a different endpoint.
	// With nowhere to embed — a key that cannot be used, document search switched off — nothing is
	// touched: the index stays as it was until there is somewhere to send it.
	embedder, err := ix.embedderFor(ctx, orgID)
	if err != nil {
		rep.Errors = append(rep.Errors, "embed: "+err.Error())
		return rep, nil
	}
	embedID := embedIDOf(embedder)
	for _, doc := range list {
		// The path is relative to the organisation's own folder, which is what the DocStore
		// hands back: the doc id and the URL a citation carries are the path the console shows,
		// and the scope map is keyed on it.
		rel := doc.Path
		if !docExts[strings.ToLower(filepath.Ext(rel))] || strings.HasPrefix(path.Base(rel), ".") {
			continue
		}
		docID := "local:" + rel
		keep[docID] = true
		rep.Docs++
		text, err := readDocument(ctx, docs, rel)
		if err != nil {
			rep.Errors = append(rep.Errors, rel+": "+err.Error())
			continue
		}
		chunks := chunk(text)
		title := strings.TrimSuffix(path.Base(rel), filepath.Ext(rel))
		if len(chunks) > 0 && strings.HasPrefix(chunks[0].Text, "# ") {
			title = strings.TrimSpace(strings.TrimPrefix(strings.SplitN(chunks[0].Text, "\n", 2)[0], "# "))
		}
		existing, _ := ix.store.DocHashes(ctx, orgID, docID, embedID)
		var pending []int
		for i := range chunks {
			chunks[i].Source, chunks[i].DocID, chunks[i].Title, chunks[i].URL = "local", docID, title, rel
			chunks[i].EmbedID = embedID
			chunks[i].Hash = hashOf(docID, chunks[i].Heading, chunks[i].Text)
			if !existing[chunks[i].Hash] {
				pending = append(pending, i)
			}
		}
		rep.Chunks += len(chunks)
		if len(pending) == 0 && len(existing) == len(chunks) {
			rep.Unchanged++
			continue
		}
		// Re-embed the whole doc (simple and keeps chunk order); batches of 32.
		failed := false
		for i := 0; i < len(chunks); i += 32 {
			end := min(i+32, len(chunks))
			texts := make([]string, 0, end-i)
			for _, c := range chunks[i:end] {
				texts = append(texts, embedText(c))
			}
			vecs, err := embedder.Embed(ctx, texts)
			if err != nil {
				rep.Errors = append(rep.Errors, rel+": embed: "+err.Error())
				failed = true
				break
			}
			for j, v := range vecs {
				chunks[i+j].Embedding = v
			}
			rep.Embedded += len(vecs)
		}
		if failed {
			continue
		}
		if err := ix.store.ReplaceDoc(ctx, orgID, docID, chunks); err != nil {
			rep.Errors = append(rep.Errors, rel+": store: "+err.Error())
		}
	}
	n, _ := ix.store.DeleteDocsNotIn(ctx, orgID, "local", keep)
	rep.Deleted = int(n)
	// Through Forget, which takes ix.mu: the ingest no longer holds that lock for its duration,
	// so touching the cache map directly here would race with a concurrent Search.
	ix.Forget(orgID)
	rep.Took = time.Since(start)
	slog.Info("ingest", "docs", rep.Docs, "chunks", rep.Chunks, "embedded", rep.Embedded, "unchanged", rep.Unchanged, "deleted", rep.Deleted, "errors", len(rep.Errors), "took", rep.Took)
	return rep, nil
}

func embedText(c DocChunk) string {
	if c.Heading != "" && !strings.HasPrefix(c.Text, "# ") {
		return c.Title + " > " + c.Heading + "\n" + c.Text
	}
	return c.Title + "\n" + c.Text
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return -1
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return -1
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

type hit struct {
	DocChunk
	Score float64
}

func (ix *Indexer) Search(ctx context.Context, orgID int64, query string, k int, channel string) ([]hit, error) {
	ix.mu.Lock()
	// A TTL as well as Forget. Forget is the same-instance fast path and stays: the instance
	// that ingested a document drops its own cache immediately. The TTL is for the others,
	// which never saw the upload and would otherwise answer from a corpus missing it until
	// they restarted — a document somebody added, and the bot saying it cannot find.
	all, ok := ix.cache[orgID]
	if ok && time.Since(ix.cachedAt[orgID]) > docCacheTTL {
		ok = false
	}
	if !ok {
		loaded, err := ix.store.AllChunks(ctx, orgID)
		if err != nil {
			ix.mu.Unlock()
			return nil, err
		}
		ix.cache[orgID], ix.cachedAt[orgID] = loaded, time.Now()
		all = loaded
	}
	ix.mu.Unlock()
	if len(all) == 0 {
		return nil, nil
	}
	embedder, err := ix.embedderFor(ctx, orgID)
	if err != nil {
		return nil, err
	}
	// Only chunks embedded the way the query is: a vector from another model scores as nonsense
	// rather than as a miss. When none are, the corpus was embedded for an endpoint this
	// organisation no longer uses, and one re-index puts that right.
	embedID := embedIDOf(embedder)
	current := all[:0:0]
	for _, c := range all {
		if c.EmbedID == embedID {
			current = append(current, c)
		}
	}
	if len(current) < len(all) {
		ix.kick(orgID)
	}
	if len(current) == 0 {
		return nil, errReindexing
	}
	all = current
	vecs, err := embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	var scopes map[string]string
	if ix.scopes != nil {
		scopes = ix.scopes(ctx, orgID)
	}
	hits := make([]hit, 0, len(all))
	for _, c := range all {
		if sc := scopes[c.URL]; sc != "" && sc != channel {
			continue // channel-restricted document
		}
		hits = append(hits, hit{DocChunk: c, Score: cosine(vecs[0], c.Embedding)})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits, nil
}

func (a *Agent) registerDocTools() {
	a.register(Tool{
		Name:   "search_docs",
		Desc:   "Semantic search over the company's internal documents (policies, processes, runbooks). Returns the most relevant passages with file names. Use before answering 'how do we…' or 'what is our policy on…' questions.",
		Params: schema(map[string]any{"query": str("What to look for, as a full question")}, "query"),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			var p struct{ Query string }
			json.Unmarshal(args, &p)
			hits, err := a.indexer.Search(ctx, c.OrgID, p.Query, 8, c.Channel)
			if err != nil {
				return "", err
			}
			if len(hits) == 0 {
				return "no documents indexed or no matches", nil
			}
			var b strings.Builder
			for i, h := range hits {
				if h.Score < 0.3 {
					continue
				}
				fmt.Fprintf(&b, "### [%d] %s — %s (score %.2f)\n%s\n\n", i+1, h.Title, h.URL, h.Score, truncate(h.Text, 1500))
			}
			if b.Len() == 0 {
				return "no relevant passages", nil
			}
			return b.String(), nil
		},
	})
}

// errReindexing is what a search says while an organisation's documents are being embedded again
// for the endpoint it uses now.
var errReindexing = errors.New("this organisation's documents are being indexed again for its current embedding model; try the search again in a few minutes")

// embedIDOf names how a client embeds: the empty string for the deployment's own endpoint, and
// "<host>|<model>" for an organisation's own — the same string embedIdentity makes of a stored row.
func embedIDOf(l *LLM) string {
	if l == nil || !l.own {
		return ""
	}
	return l.host + "|" + l.EmbedModel
}

// kick starts one re-index of an organisation in the background, unless one this process started
// is still running. A search that found chunks embedded for another endpoint calls it: the save
// that changed the endpoint re-indexes on the instance it happened on, and this is how the others
// catch up without waiting for the six-hourly pass.
func (ix *Indexer) kick(orgID int64) {
	ix.mu.Lock()
	if ix.kicked == nil {
		ix.kicked = map[int64]bool{}
	}
	if ix.kicked[orgID] || ix.docs == nil {
		ix.mu.Unlock()
		return
	}
	ix.kicked[orgID] = true
	ix.mu.Unlock()
	// Stale chunks can mean this instance's settings are the stale thing: read them afresh, so the
	// re-index embeds for the endpoint that is saved rather than for one that was.
	if ix.endpoints != nil {
		ix.endpoints.settings.Invalidate(orgID)
		ix.endpoints.Evict(orgID)
	}
	go func() {
		defer func() {
			ix.mu.Lock()
			delete(ix.kicked, orgID)
			ix.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if _, err := ix.IngestOrg(ctx, orgID); err != nil {
			slog.Warn("re-index for a changed embedding model", "org", orgID, "err", err)
		}
	}()
}
