package review

import "testing"

func TestScore(t *testing.T) {
	f := func(s Severity) Finding { return Finding{Severity: s} }
	pre := func(s Severity) Finding { return Finding{Severity: s, PreExisting: true} }
	cases := []struct {
		name      string
		open      []Finding
		full      bool
		injection bool
		want      int
	}{
		{"nothing open", nil, true, false, 5},
		{"only P2s", []Finding{f(P2), f(P2), f(P2)}, true, false, 4},
		{"one P1", []Finding{f(P1), f(P2)}, true, false, 3},
		{"two P1s", []Finding{f(P1), f(P1)}, true, false, 2},
		{"one P0 outweighs any number of P1s", []Finding{f(P0), f(P1), f(P1), f(P1)}, true, false, 1},
		{"two P0s", []Finding{f(P0), f(P0)}, true, false, 0},
		{"three P0s", []Finding{f(P0), f(P0), f(P0)}, true, false, 0},

		{"pre-existing findings never count", []Finding{pre(P0), pre(P1), pre(P2)}, true, false, 5},
		{"pre-existing beside a real P1", []Finding{pre(P0), f(P1)}, true, false, 3},
		{"an unknown severity is still an open finding", []Finding{f("P9")}, true, false, 4},

		{"partial coverage cannot claim nothing found", nil, false, false, 4},
		{"injection text cannot buy a clean score", nil, true, true, 4},
		{"the cap never raises a score", []Finding{f(P0)}, false, true, 1},
		{"capped at 4 with only P2s is still 4", []Finding{f(P2)}, false, false, 4},
	}
	for _, c := range cases {
		if got := Score(c.open, c.full, c.injection); got != c.want {
			t.Errorf("%s: Score = %d, want %d", c.name, got, c.want)
		}
	}
}
