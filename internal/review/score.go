package review

// Score is the review's confidence that the pull request is safe to merge, 0 to 5, computed
// from the open findings on the current head and nothing else:
//
//	no open findings  5
//	only P2s          4
//	one P1            3
//	two or more P1s   2
//	one P0            1
//	two or more P0s   0
//
// It is computed here in Go, never asked of the model, so that it moves the moment a finding
// does — withdrawn after a reply, fixed by a push, acknowledged as a known risk — and is never a
// stale number in a summary nobody re-rendered. Pre-existing findings never count: a pull request
// is not marked down for what it inherited. Which findings are open is the caller's to say
// (Standing); an acknowledged one is not, since the team has accepted the risk it names.
//
// A review that did not see every reviewable changed line (fullCoverage false) cannot say
// "nothing found", and neither can one whose diff carried text written to steer the reviewer
// (injectionDetected): the easiest attack on a model reviewer is to talk it out of reporting
// anything, which would otherwise earn a 5. Both cap the score at 4. The score is advisory
// either way, and the summary says so.
//
// A finding whose severity is not one of the three still counts, as a P2: it is an open
// finding, and the one number a score of 5 must never mean is "there are open findings".
func Score(open []Finding, fullCoverage, injectionDetected bool) int {
	var p0, p1, p2 int
	for _, f := range open {
		if f.PreExisting {
			continue
		}
		switch f.Severity {
		case P0:
			p0++
		case P1:
			p1++
		default:
			p2++
		}
	}
	score := 5
	switch {
	case p0 >= 2:
		score = 0
	case p0 == 1:
		score = 1
	case p1 >= 2:
		score = 2
	case p1 == 1:
		score = 3
	case p2 > 0:
		score = 4
	}
	if !fullCoverage || injectionDetected {
		score = min(score, 4)
	}
	return score
}

// Standing reports whether a finding in status s still stands on the pull request — open, or
// disputed, which the bot still stands by — and so whether the score counts it. Withdrawn, fixed,
// outdated, resolved by a person and acknowledged as a known risk do not.
func Standing(s FindingStatus) bool {
	return s == "" || s == FindingOpen || s == FindingDisputed
}
