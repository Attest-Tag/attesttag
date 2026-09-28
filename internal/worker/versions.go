package worker

import (
	"regexp"
	"strconv"
	"strings"
)

// Version ranges from manifests. A package that says requires-python ">=3.9" or engines
// ">=18.0.0" has named the oldest version it supports, which is what the worker checks it on
// when nothing pins a version outright — reading the range as an exact version instead ran
// Node 18.0.0 (the April 2022 release) and read "<3.12" as "install 3.12", the one version it
// excludes.

const (
	// oldestPython is the oldest Python the worker can obtain at all: nothing older has a
	// prebuilt interpreter, uv refuses anything below 3.6 outright, and this image cannot build
	// one. A folder that pins older runs on this one, and says so.
	oldestPython = "3.8"
	// pythonFloorMin is the lowest floor worth installing through mise. mise refuses 3.8's
	// prebuilt builds (they predate the release attestations it verifies), and the image's own
	// Python satisfies a lower bound anyway; an exact minor below this is uv's to fetch, which it
	// does from requires-python by itself.
	pythonFloorMin = "3.9"
)

var pyClauseRe = regexp.MustCompile(`^\s*(~=|===|==|!=|<=|>=|<|>)\s*v?(\d+(?:\.\d+)*)(\.\*)?\s*$`)

// pythonFloor is the oldest Python a requires-python range allows, as major.minor, when it is
// worth installing; "" when the range has no lower bound or its floor is below pythonFloorMin.
func pythonFloor(spec string) string {
	floor := ""
	for _, clause := range strings.Split(spec, ",") {
		m := pyClauseRe.FindStringSubmatch(clause)
		if m == nil {
			continue
		}
		switch m[1] {
		case ">=", ">", "~=", "==", "===":
			if v := majorMinor(m[2]); v != "" && (floor == "" || versionLess(floor, v)) {
				floor = v
			}
		}
	}
	if floor == "" || versionLess(floor, pythonFloorMin) {
		return ""
	}
	return floor
}

var (
	nodeOpSpaceRe = regexp.MustCompile(`(>=|<=|>|<|=|\^|~)\s+`)
	nodeVersionRe = regexp.MustCompile(`^v?(\d+)`)
)

// nodeFloor is the major version an engines.node range starts at — the lowest across its "||"
// alternatives — or "" when some alternative has no lower bound at all ("*", "<20").
func nodeFloor(spec string) string {
	spec = strings.TrimSpace(nodeOpSpaceRe.ReplaceAllString(spec, "$1"))
	if spec == "" {
		return ""
	}
	lowest := -1
	for _, alt := range strings.Split(spec, "||") {
		major := -1
		for _, tok := range strings.Fields(alt) {
			if strings.HasPrefix(tok, "<") || tok == "-" {
				continue
			}
			v := strings.TrimLeft(tok, ">=^~")
			if m := nodeVersionRe.FindStringSubmatch(v); m != nil {
				if n, err := strconv.Atoi(m[1]); err == nil && (major < 0 || n < major) {
					major = n
				}
			}
		}
		if major < 0 {
			return ""
		}
		if lowest < 0 || major < lowest {
			lowest = major
		}
	}
	if lowest <= 0 {
		return ""
	}
	return strconv.Itoa(lowest)
}

// majorMinor is "3.9" out of "3.9", "3.9.18" or "3"; "" when there is no number to read.
func majorMinor(v string) string {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) == 0 || parts[0] == "" {
		return ""
	}
	for _, p := range parts[:min(2, len(parts))] {
		if _, err := strconv.Atoi(p); err != nil {
			return ""
		}
	}
	if len(parts) == 1 {
		return parts[0] + ".0"
	}
	return parts[0] + "." + parts[1]
}

// versionLess compares dotted numeric versions part by part; a missing part is zero.
func versionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(as), len(bs)); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}
