package review

import (
	"strings"
	"testing"
)

var testMarkerKey = []byte("test-marker-key-0123456789abcdef")

const testFindingID = "0123456789abcdef0123456789abcdef"

// scope is a marker scope on the test pull request, acme/web#7, for an organisation.
func scope(org int64) MarkerScope { return MarkerScope{OrgID: org, Repo: "acme/web", PR: 7} }

func TestMarkerRoundTrips(t *testing.T) {
	for _, kind := range []string{MarkerFinding, MarkerReview, MarkerRun} {
		m := Marker(testMarkerKey, scope(42), kind, testFindingID)
		if !strings.HasPrefix(m, "<!-- attest_tag:"+kind+"="+testFindingID+".") || !strings.HasSuffix(m, " -->") {
			t.Fatalf("%s marker %q is not in the documented shape", kind, m)
		}
		body := "Some comment text.\n\n" + m
		got := ParseMarkers(body)
		if len(got) != 1 || got[0].Kind != kind || got[0].PublicID != testFindingID || len(got[0].MAC) != 16 {
			t.Fatalf("ParseMarkers(%q) = %+v", body, got)
		}
		if !Verify(testMarkerKey, scope(42), got[0]) {
			t.Errorf("%s marker does not verify under the key and org it was made with", kind)
		}
		if id, ok := VerifiedMarker(testMarkerKey, scope(42), body, kind); !ok || id != testFindingID {
			t.Errorf("VerifiedMarker = %q, %v", id, ok)
		}
	}
}

// A marker proves which organisation's comment it is, on which pull request, and what kind of
// thing it names, so a real marker copied from somewhere else must not verify anywhere else.
func TestMarkerDoesNotVerifyElsewhere(t *testing.T) {
	m := ParseMarkers(Marker(testMarkerKey, scope(42), MarkerFinding, testFindingID))[0]
	cases := map[string]struct {
		key []byte
		sc  MarkerScope
		m   ParsedMarker
	}{
		"another organisation": {testMarkerKey, scope(43), m},
		// A private repository's finding pasted into a public one of the same organisation.
		"another repository":   {testMarkerKey, MarkerScope{OrgID: 42, Repo: "acme/api", PR: 7}, m},
		"another pull request": {testMarkerKey, MarkerScope{OrgID: 42, Repo: "acme/web", PR: 8}, m},
		"no pull request":      {testMarkerKey, MarkerScope{OrgID: 42, Repo: "acme/web"}, m},
		"another key":          {[]byte("some-other-key"), scope(42), m},
		"no key":               {nil, scope(42), m},
		"replayed as the summary": {testMarkerKey, scope(42),
			ParsedMarker{Kind: MarkerReview, PublicID: m.PublicID, MAC: m.MAC}},
		"another finding": {testMarkerKey, scope(42),
			ParsedMarker{Kind: m.Kind, PublicID: "ffffffffffffffffffffffffffffffff", MAC: m.MAC}},
		"a tampered mac": {testMarkerKey, scope(42),
			ParsedMarker{Kind: m.Kind, PublicID: m.PublicID, MAC: flipHex(m.MAC)}},
		"a short mac": {testMarkerKey, scope(42),
			ParsedMarker{Kind: m.Kind, PublicID: m.PublicID, MAC: m.MAC[:15]}},
		"an unknown kind": {testMarkerKey, scope(42),
			ParsedMarker{Kind: "state", PublicID: m.PublicID, MAC: m.MAC}},
	}
	for name, c := range cases {
		if Verify(c.key, c.sc, c.m) {
			t.Errorf("%s: verified", name)
		}
	}
	// GitHub's names are case-insensitive, so the scope is too.
	if !Verify(testMarkerKey, MarkerScope{OrgID: 42, Repo: "ACME/Web", PR: 7}, m) {
		t.Error("the same repository in another case did not verify")
	}
}

func flipHex(s string) string {
	b := []byte(s)
	if b[0] == '0' {
		b[0] = '1'
	} else {
		b[0] = '0'
	}
	return string(b)
}

// An empty key computes a MAC like any other, so the same call made by anybody would
// produce the same marker; Verify refusing it is what makes a lost key fail closed.
func TestVerifyRefusesAnEmptyKeyEvenForItsOwnMarker(t *testing.T) {
	m := ParseMarkers(Marker(nil, scope(42), MarkerRun, testFindingID))
	if len(m) != 1 {
		t.Fatalf("a marker made with no key still renders: got %v", m)
	}
	if Verify(nil, scope(42), m[0]) || Verify([]byte{}, scope(42), m[0]) {
		t.Error("a marker verified under an empty key")
	}
}

func TestMarkerRefusesWhatCannotBeReadBack(t *testing.T) {
	for _, c := range []struct{ kind, id string }{
		{"state", testFindingID},
		{"", testFindingID},
		{MarkerFinding, ""},
		{MarkerFinding, "has space"},
		{MarkerFinding, "dot.ted"},
		{MarkerFinding, "x -->"},
		{MarkerFinding, strings.Repeat("a", 65)},
	} {
		if m := Marker(testMarkerKey, scope(1), c.kind, c.id); m != "" {
			t.Errorf("Marker(%q, %q) = %q, want none", c.kind, c.id, m)
		}
	}
	for _, sc := range []MarkerScope{{OrgID: 1, PR: 7}, {OrgID: 1, Repo: "acme", PR: 7}, {OrgID: 1, Repo: "acme/web"}} {
		if m := Marker(testMarkerKey, sc, MarkerFinding, testFindingID); m != "" {
			t.Errorf("Marker with scope %+v = %q, want none", sc, m)
		}
	}
}

// ParseMarkers reads exactly what Marker writes: our own output, so a near miss is somebody
// else's text, and the summary's unsigned state line is not a marker at all.
func TestParseMarkersIsExact(t *testing.T) {
	good := Marker(testMarkerKey, scope(7), MarkerFinding, "abc")
	other := Marker(testMarkerKey, scope(7), MarkerRun, "run1")
	body := strings.Join([]string{
		"<!-- attest_tag:state sha=c03ddb1 score=3 open=2 -->",
		strings.ToUpper(good),
		strings.Replace(good, "<!-- ", "<!--", 1),
		"<!-- attest_tag:finding=abc.0123456789ABCDEF -->",
		"<!-- attest_tag:finding=abc.0123 -->",
		"<!-- attest_tag:note=abc.0123456789abcdef -->",
		good,
		"text " + other + " more",
	}, "\n")
	got := ParseMarkers(body)
	if len(got) != 2 || got[0].PublicID != "abc" || got[1].Kind != MarkerRun || got[1].PublicID != "run1" {
		t.Fatalf("ParseMarkers = %+v, want only the two well-formed markers, in order", got)
	}
	if ParseMarkers("no markers here") != nil {
		t.Error("a body with no marker parsed to something")
	}
}

func TestVerifiedMarkerSkipsForgeriesAndOtherKinds(t *testing.T) {
	forged := "<!-- attest_tag:finding=" + testFindingID + ".0123456789abcdef -->"
	run := Marker(testMarkerKey, scope(9), MarkerRun, "run1")
	real := Marker(testMarkerKey, scope(9), MarkerFinding, testFindingID)
	body := forged + "\n" + run + "\n" + real
	if id, ok := VerifiedMarker(testMarkerKey, scope(9), body, MarkerFinding); !ok || id != testFindingID {
		t.Errorf("VerifiedMarker = %q, %v; want the real finding marker after the forged one", id, ok)
	}
	if _, ok := VerifiedMarker(testMarkerKey, scope(9), forged+"\n"+run, MarkerFinding); ok {
		t.Error("a forged finding marker verified")
	}
	if _, ok := VerifiedMarker(testMarkerKey, scope(9), body, MarkerReview); ok {
		t.Error("found a review marker in a body that has none")
	}
}
