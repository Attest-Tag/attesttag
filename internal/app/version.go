package app

import "os"

// Version is the release this binary was built as. The Dockerfile and `make dist` stamp it with
// -ldflags "-X attesttag/internal/app.Version=0.1.0", from the tag the release workflow was run
// for; any other build is "dev". It is the first line of a useful bug report, which is why
// `attesttag version` prints it and the startup line records it.
var Version = "dev"

// Commit is the git commit the binary was built from, stamped by `make dist` the same way. The
// image does not stamp it: -trimpath and a build context without .git leave nothing to read it
// from, so the commit reaches a container as the GIT_COMMIT variable instead (see the Dockerfile).
var Commit = ""

// VersionString is the version with the commit it came from, when that is known:
// "0.1.0 (4f2a9c1e0b7d)", or "dev (4f2a9c1e0b7d-dirty)" for a build no release made. Only a full
// hash is shortened; deploy/gcp/cloudrun.sh already passes a short one, sometimes with -dirty on
// the end, and cutting that off would hide the one thing it is there to say.
func VersionString() string {
	c := Commit
	if c == "" {
		c = os.Getenv("GIT_COMMIT")
	}
	if len(c) == 40 {
		c = c[:12]
	}
	if c == "" {
		return Version
	}
	return Version + " (" + c + ")"
}
