package app

import (
	"encoding/json"
	"strings"
	"testing"
)

// Models drift from submit_review's schema in a handful of recurring ways. Each shape here was
// decoded into an empty finding before, and a whole pass's findings were dropped as "invalid"
// with nothing in the drop to say what the model had sent.
func TestDecodeFindingAcceptsTheShapesModelsSend(t *testing.T) {
	cases := []struct {
		name, raw string
	}{
		{"schema", `{"path":"a.go","line":12,"start_line":10,"severity":"P1","title":"Race on map","scenario":"two writers"}`},
		{"aliases", `{"file":"a.go","line_start":10,"line_end":12,"priority":"high","name":"Race on map","description":"two writers"}`},
		{"wrapped", `{"finding":{"path":"a.go","line":12,"start_line":10,"severity":"P1","title":"Race on map","scenario":"two writers"}}`},
		{"location", `{"location":{"path":"a.go","start_line":10,"line":12},"severity":"P1","title":"Race on map","scenario":"two writers"}`},
		{"lines string", `{"path":"a.go","lines":"L10-L12","severity":"major","title":"Race on map","issue":"two writers"}`},
		{"lines array", `{"filename":"a.go","lines":[10,12],"severity":"P1","title":"Race on map","explanation":"two writers"}`},
		{"stringified", `"{\"path\":\"a.go\",\"line\":12,\"start_line\":10,\"severity\":\"P1\",\"title\":\"Race on map\",\"scenario\":\"two writers\"}"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := decodeFinding(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if f.Path != "a.go" || f.Line != 12 || f.StartLine != 10 || string(f.Severity) != "P1" ||
				f.Title != "Race on map" || f.Scenario != "two writers" {
				t.Fatalf("decoded %+v", f)
			}
		})
	}
}

// A finding in a shape nobody anticipated is refused with the names it did carry, so the drop
// shown in the console says what the model sent instead of listing five missing fields.
func TestDecodeFindingNamesTheFieldsOfAnUnknownShape(t *testing.T) {
	_, err := decodeFinding(json.RawMessage(`{"where":"a.go:12","what":"race","how_bad":"bad"}`))
	if err == nil || !strings.Contains(err.Error(), "how_bad, what, where") {
		t.Fatalf("err = %v, want the fields named", err)
	}
	sub, err := parseSubmission(`{"summary":"s","findings":[{"where":"a.go:12"},{"path":"a.go","line":3,"severity":"P2","title":"Typo here","scenario":"x"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(sub.Findings) != 1 || len(sub.Invalid) != 1 || !strings.Contains(sub.Invalid[0], "where") {
		t.Fatalf("findings=%d invalid=%v", len(sub.Findings), sub.Invalid)
	}
}
