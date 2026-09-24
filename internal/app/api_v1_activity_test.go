package app

import (
	"context"
	"fmt"
	"slices"
	"testing"
)

// /v1/activity pages the way /v1/audit does. It used to hand back at most 500 turns with no id on
// any of them, so a month busier than one page could not be read whole, and a collector had
// nothing to keep its place with.
func TestV1ActivityPagesByID(t *testing.T) {
	b, mux, st := identityBot(t)
	ctx := context.Background()
	_, orgA, sessionA := signedUp(t, b, mux, st, "founder@example.com")
	_, orgB, _ := signedUp(t, b, mux, st, "other@example.com")
	key, _ := mintKey(t, mux, sessionA, "cost export")

	// Five turns of A's between five of B's, so the ids a cursor walks past belong to both.
	for i := range 5 {
		st.LogUsageBy(ctx, orgA, "T1", "C1", "", "U1", "test", Usage{In: 10 * (i + 1), Out: 1, CostUSD: 0.01})
		st.LogUsageBy(ctx, orgB, "T2", "C2", "", "U2", "test", Usage{In: 1, Out: 1, CostUSD: 0.01})
	}

	page := func(query string) (ids []int64, last int64) {
		t.Helper()
		code, body := authReq(t, mux, "GET", "/v1/activity?"+query, nil, key)
		if code != 200 {
			t.Fatalf("GET /v1/activity?%s = %d: %v", query, code, body)
		}
		turns, _ := body["turns"].([]any)
		for _, row := range turns {
			id, ok := row.(map[string]any)["id"].(float64)
			if !ok {
				t.Fatalf("a turn without an id: %v", row)
			}
			ids = append(ids, int64(id))
		}
		lastID, ok := body["last_id"].(float64)
		if !ok {
			t.Fatalf("no last_id in %v", body)
		}
		return ids, int64(lastID)
	}

	// Newest first, and before walks back from the smallest id seen until nothing is left.
	all, last := page("limit=2")
	if len(all) != 2 || last != all[0] {
		t.Fatalf("first page = %v last_id %d, want two ids and the larger as last_id", all, last)
	}
	for guard := 0; ; guard++ {
		if guard > 5 {
			t.Fatalf("before never ran out: %v", all)
		}
		older, _ := page(fmt.Sprintf("limit=2&before=%d", all[len(all)-1]))
		if len(older) == 0 {
			break
		}
		all = append(all, older...)
	}
	if len(all) != 5 {
		t.Fatalf("walking back read %v, want A's five turns and none of B's", all)
	}
	for i := 1; i < len(all); i++ {
		if all[i] >= all[i-1] {
			t.Fatalf("walking back read %v, want distinct ids, newest first", all)
		}
	}

	// after walks forward from a mark, oldest first, and last_id is the next call's mark.
	fwd, last := page(fmt.Sprintf("limit=2&after=%d", all[4]))
	if !slices.Equal(fwd, []int64{all[3], all[2]}) || last != all[2] {
		t.Fatalf("after the oldest = %v last_id %d, want [%d %d] and %d", fwd, last, all[3], all[2], all[2])
	}

	// Past the newest there is nothing to read, and last_id keeps the mark it was sent: a 0
	// there would make the next call ?after=0, which is no cursor at all.
	rest, last := page(fmt.Sprintf("after=%d", all[0]))
	if len(rest) != 0 || last != all[0] {
		t.Fatalf("after the newest = %v last_id %d, want nothing and %d", rest, last, all[0])
	}
}

// The audit log's last_id holds its place on an empty page too, for the same reason.
func TestV1AuditKeepsItsMarkOnAnEmptyPage(t *testing.T) {
	b, mux, st := identityBot(t)
	_, _, session := signedUp(t, b, mux, st, "founder@example.com")
	key, _ := mintKey(t, mux, session, "siem")

	code, body := authReq(t, mux, "GET", "/v1/audit?after=999999", nil, key)
	if code != 200 {
		t.Fatalf("GET /v1/audit = %d: %v", code, body)
	}
	if events, _ := body["events"].([]any); len(events) != 0 {
		t.Fatalf("events past the end = %v", events)
	}
	if body["last_id"] != 999999.0 {
		t.Fatalf("last_id = %v, want the mark it was sent", body["last_id"])
	}
}
