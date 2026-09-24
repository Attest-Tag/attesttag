package app

import (
	"context"
	"testing"
)

// A memory can be corrected in place, from the console and from a developer key, and the row
// keeps its identity. The edit is looked up under the caller's organisation, so somebody
// else's memory answers "no such memory" rather than being rewritten.
func TestAMemoryCanBeEditedInPlaceButOnlyByItsOwnOrganisation(t *testing.T) {
	b, mux, st := identityBot(t)
	ctx := context.Background()
	_, orgA, sessionA := signedUp(t, b, mux, st, "a@example.com")
	_, orgB, _ := signedUp(t, b, mux, st, "b@example.com")

	if err := st.AddMemory(ctx, orgA, "TA", teamMemoryScope("TA"), "Deploys go out on Tuesdays.", "U1"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMemory(ctx, orgB, "TB", teamMemoryScope("TB"), "B's private fact", "U2"); err != nil {
		t.Fatal(err)
	}
	msA, _ := st.AllMemories(ctx, orgA)
	msB, _ := st.AllMemories(ctx, orgB)
	if len(msA) != 1 || len(msB) != 1 {
		t.Fatalf("setup: A has %d memories, B has %d", len(msA), len(msB))
	}
	idA, idB := msA[0].ID, msB[0].ID

	// The console rewrites A's own memory; the id survives and the text changes.
	code, body := authReq(t, mux, "PUT", "/api/memories/"+itoa(idA),
		map[string]string{"text": "  Deploys go out on Thursdays.  "}, sessionA)
	if code != 200 {
		t.Fatalf("edit own memory = %d: %v", code, body)
	}
	msA, _ = st.AllMemories(ctx, orgA)
	if len(msA) != 1 || msA[0].ID != idA || msA[0].Text != "Deploys go out on Thursdays." {
		t.Fatalf("after edit: %+v", msA)
	}

	// Blank text is not an edit.
	if code, _ := authReq(t, mux, "PUT", "/api/memories/"+itoa(idA), map[string]string{"text": "   "}, sessionA); code != 400 {
		t.Errorf("blank edit = %d, want 400", code)
	}

	// B's memory, by id, from A's session: not found, and untouched.
	if code, _ := authReq(t, mux, "PUT", "/api/memories/"+itoa(idB), map[string]string{"text": "planted"}, sessionA); code != 404 {
		t.Errorf("editing B's memory from A = %d, want 404", code)
	}
	msB, _ = st.AllMemories(ctx, orgB)
	if msB[0].Text != "B's private fact" {
		t.Fatalf("B's memory was rewritten: %+v", msB)
	}

	// The developer API follows the same rule.
	keyA, _ := mintKey(t, mux, sessionA, "a's key")
	if code, body := authReq(t, mux, "PUT", "/v1/memories/"+itoa(idA), map[string]string{"text": "Deploys go out on Fridays."}, keyA); code != 200 {
		t.Fatalf("v1 edit own memory = %d: %v", code, body)
	}
	if code, _ := authReq(t, mux, "PUT", "/v1/memories/"+itoa(idB), map[string]string{"text": "planted"}, keyA); code != 404 {
		t.Errorf("v1 editing B's memory from A = %d, want 404", code)
	}
	msA, _ = st.AllMemories(ctx, orgA)
	msB, _ = st.AllMemories(ctx, orgB)
	if msA[0].Text != "Deploys go out on Fridays." || msB[0].Text != "B's private fact" {
		t.Fatalf("after v1 edits: A=%+v B=%+v", msA, msB)
	}
}
