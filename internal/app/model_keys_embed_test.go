package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeDoc(t *testing.T, st *Store, orgID int64) DocStore {
	t.Helper()
	docs := (&localDocs{dir: t.TempDir(), store: st}).For(orgID).(*localDocs)
	if err := os.MkdirAll(docs.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docs.dir, "policy.md"),
		[]byte("# Deploys\n\nDeploys happen on Tuesdays after the release review, never on a Friday afternoon."), 0o644); err != nil {
		t.Fatal(err)
	}
	return docs
}

// Vectors from two models are not comparable, so a corpus embedded for one endpoint has to be
// embedded again when the organisation moves to another — even though not a word of it changed —
// and a search in between must not score its query against the old vectors.
func TestAChangeOfEmbeddingModelEmbedsTheCorpusAgain(t *testing.T) {
	r := newOwnKeyRig(t, OrgModelKeysAll)
	ctx := context.Background()
	docs := writeDoc(t, r.st, 1)

	rep, err := r.ix.Ingest(ctx, 1, docs)
	if err != nil || rep.Embedded == 0 {
		t.Fatalf("first ingest: %+v %v", rep, err)
	}
	chunks, _ := r.st.AllChunks(ctx, 1)
	embedder, _ := r.ix.embedderFor(ctx, 1)
	if len(chunks) == 0 || chunks[0].EmbedID != embedIDOf(embedder) || chunks[0].EmbedID == "" {
		t.Fatalf("chunks were not marked with the organisation's endpoint: %+v", chunks)
	}
	// Nothing changed: nothing is embedded again.
	if rep, _ := r.ix.Ingest(ctx, 1, docs); rep.Embedded != 0 || rep.Unchanged != 1 {
		t.Errorf("an unchanged corpus was embedded again: %+v", rep)
	}

	// The organisation removes its key. Its chunks were embedded by a model the deployment's
	// endpoint does not run, so a search refuses rather than scoring against them.
	if _, err := r.st.DeleteModelKey(ctx, 1); err != nil {
		t.Fatal(err)
	}
	r.settings.Invalidate(1)
	r.endpoints.Evict(1)
	r.ix.Forget(1)
	if _, err := r.ix.Search(ctx, 1, "when do deploys happen", 3, "C1"); !errors.Is(err, errReindexing) {
		t.Errorf("search over another endpoint's vectors = %v; want the re-indexing refusal", err)
	}
	platformEmbeds := r.platform.hits["POST /embeddings"]
	rep, err = r.ix.Ingest(ctx, 1, docs)
	if err != nil || rep.Embedded == 0 || rep.Unchanged != 0 {
		t.Fatalf("after the switch the corpus was not embedded again: %+v %v", rep, err)
	}
	if r.platform.hits["POST /embeddings"] == platformEmbeds {
		t.Error("the re-index did not use the endpoint now in use")
	}
	r.ix.Forget(1)
	if hits, err := r.ix.Search(ctx, 1, "when do deploys happen", 3, "C1"); err != nil || len(hits) == 0 {
		t.Errorf("search after the re-index: %v %v", hits, err)
	}
	if chunks, _ := r.st.AllChunks(ctx, 1); len(chunks) == 0 || chunks[0].EmbedID != "" {
		t.Errorf("chunks after moving back to the deployment's endpoint: %+v", chunks)
	}
}

// An organisation's own endpoint with no embedding model has document search off. It is not a
// reason to embed its documents somewhere it did not choose.
func TestNoEmbeddingModelMeansDocumentSearchIsOff(t *testing.T) {
	r := newOwnKeyRig(t, OrgModelKeysAll)
	ctx := context.Background()
	ref, _ := r.st.ModelKeyRef(ctx, 1)
	ref.EmbedModel = ""
	if err := r.st.PutModelKey(ctx, 1, *ref, "", "a@x", r.endpoints.sealer); err != nil {
		t.Fatal(err)
	}
	r.settings.Invalidate(1)
	r.endpoints.Evict(1)
	rep, _ := r.ix.Ingest(ctx, 1, writeDoc(t, r.st, 1))
	if rep.Embedded != 0 || len(rep.Errors) == 0 {
		t.Errorf("ingest with no embedding model: %+v", rep)
	}
	if _, err := r.ix.embedderFor(ctx, 1); !errors.Is(err, errDocSearchOff) {
		t.Errorf("embedder = %v; want document search off", err)
	}
	if n := r.platform.sent() + r.own.hits["POST /embeddings"]; n != 0 {
		t.Errorf("documents were embedded somewhere %d times", n)
	}
}
