package review

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// The kinds of thing a hidden marker can name. A finding's marker ends each inline comment, a
// review's ends the sticky summary comment, and a run's is the whole body of the GitHub review
// object one run posts, which is how a run that died after posting can find its own review and
// adopt it instead of posting a second one.
const (
	MarkerFinding = "finding"
	MarkerReview  = "review"
	MarkerRun     = "run"
)

// markerMACLen is how much of the HMAC a marker carries, in hex: 64 bits, which nobody guesses
// in the handful of comments a pull request holds, in a line short enough to sit unseen at the
// end of a comment.
const markerMACLen = 16

// markerID is what a marker may carry as a public id: the 32-hex ids this codebase hands out,
// with room for any other opaque token of letters, digits, '-' and '_'. Nothing that could end
// the comment or the marker early, which is what keeps Marker's output parseable whatever it is
// given.
var markerID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// markerPattern reads exactly what Marker writes and nothing looser: a marker is our own
// output, so a near miss — another case, another spacing — is somebody else's text.
var markerPattern = regexp.MustCompile(`<!-- attest_tag:(finding|review|run)=([A-Za-z0-9_-]{1,64})\.([0-9a-f]{16}) -->`)

// MarkerScope is what a marker is bound to: the organisation that posted it and the pull
// request it was posted on. Repo is owner/name in any case; PR is the pull request's number.
type MarkerScope struct {
	OrgID int64
	Repo  string
	PR    int
}

func (sc MarkerScope) valid() bool { return validRepoName(sc.Repo) && sc.PR > 0 }

// Marker renders the hidden HTML comment that identifies one of our own comments on GitHub:
//
//	<!-- attest_tag:<kind>=<publicID>.<mac> -->
//
// where mac is the first 16 hex characters of
// HMAC-SHA256(key, "<orgID>|<owner/name, lower-cased>|<pr>|<kind>|<publicID>").
//
// Markers are how the bot recognises what it wrote when a stored comment id is missing — a
// post whose response was lost, a summary somebody's edit moved — and a pull request is a place
// where anybody can write anything, the model included. The MAC is what makes one unforgeable:
// a comment that carries a marker without the key behind it, or carries a real marker copied
// from somewhere else, does not verify. The organisation is in the MAC so another
// organisation's marker never verifies here. The repository and the pull request are in it so
// one of ours does not travel either: anybody with write access to a public repository can edit
// the bot's comments there, and a finding's marker copied in from a private repository of the
// same organisation would otherwise verify, and have a reply on the public pull request answered
// about — and quoting — the private finding. The kind is in it so a finding's marker can never be
// replayed as the summary's.
//
// The key is the caller's (the app derives it from MASTER_KEY under its own label); this
// package never reads one from the environment. An unknown kind, an id that is not an opaque
// token, or a scope with no repository or pull request renders as "", so a programming error
// leaves a comment unmarked rather than marked with something that cannot be verified back.
func Marker(key []byte, sc MarkerScope, kind, publicID string) string {
	if !validMarkerKind(kind) || !markerID.MatchString(publicID) || !sc.valid() {
		return ""
	}
	return "<!-- attest_tag:" + kind + "=" + publicID + "." + markerMAC(key, sc, kind, publicID) + " -->"
}

// ParsedMarker is one marker found in a comment body. Nothing in it is trusted until Verify
// says so.
type ParsedMarker struct {
	Kind     string
	PublicID string
	MAC      string
}

// ParseMarkers returns every well-formed marker in body, in order, whether or not it verifies.
// The informational state line the summary carries ("<!-- attest_tag:state …") is not a marker
// and is not returned.
func ParseMarkers(body string) []ParsedMarker {
	var out []ParsedMarker
	for _, m := range markerPattern.FindAllStringSubmatch(body, -1) {
		out = append(out, ParsedMarker{Kind: m[1], PublicID: m[2], MAC: m[3]})
	}
	return out
}

// Verify reports whether m was made by Marker with this key for this organisation and this
// pull request. It compares in constant time. With no key it refuses everything: a MAC under an
// empty key is one anybody can compute, so it proves nothing, and a deployment that somehow
// lost its key must not start trusting comments because of it.
func Verify(key []byte, sc MarkerScope, m ParsedMarker) bool {
	if len(key) == 0 || !sc.valid() || !validMarkerKind(m.Kind) || !markerID.MatchString(m.PublicID) || len(m.MAC) != markerMACLen {
		return false
	}
	want := markerMAC(key, sc, m.Kind, m.PublicID)
	return hmac.Equal([]byte(want), []byte(m.MAC))
}

// VerifiedMarker returns the public id in the first marker of kind in body that verifies for
// the pull request the comment is on — the delivery's repository and number, never anything the
// comment itself says. Whoever calls it must also check who wrote the comment — a verified
// marker on a comment the bot did not author is a copy, quoted or pasted by a person — since the
// MAC proves where a marker came from, not where it is now.
func VerifiedMarker(key []byte, sc MarkerScope, body, kind string) (string, bool) {
	for _, m := range ParseMarkers(body) {
		if m.Kind == kind && Verify(key, sc, m) {
			return m.PublicID, true
		}
	}
	return "", false
}

// markerMAC signs the fields joined by '|', which none of them can hold — the org and the number
// are digits, the kind is one of three words, and the repository and the id are checked against
// patterns without one — so no two different scopes join to the same text.
func markerMAC(key []byte, sc MarkerScope, kind, publicID string) string {
	h := hmac.New(sha256.New, key)
	fmt.Fprintf(h, "%d|%s|%d|%s|%s", sc.OrgID, strings.ToLower(sc.Repo), sc.PR, kind, publicID)
	return hex.EncodeToString(h.Sum(nil))[:markerMACLen]
}

func validMarkerKind(kind string) bool {
	return kind == MarkerFinding || kind == MarkerReview || kind == MarkerRun
}
