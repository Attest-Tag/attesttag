package app

import "testing"

func TestVersionStringNamesTheCommitItCameFrom(t *testing.T) {
	defer func(v, c string) { Version, Commit = v, c }(Version, Commit)

	for _, tc := range []struct {
		version, commit, env, want string
	}{
		{"dev", "", "", "dev"},
		// make dist stamps the full hash; it is shortened to what a person can quote.
		{"0.1.0", "4f2a9c1e0b7d55aa1b2c3d4e5f60718293a4b5c6", "", "0.1.0 (4f2a9c1e0b7d)"},
		// The image carries the commit as GIT_COMMIT, full from the release workflow...
		{"0.1.0", "", "4f2a9c1e0b7d55aa1b2c3d4e5f60718293a4b5c6", "0.1.0 (4f2a9c1e0b7d)"},
		// ...and short from cloudrun.sh, whose -dirty must survive.
		{"dev", "", "4f2a9c1e0b7d-dirty", "dev (4f2a9c1e0b7d-dirty)"},
		// A stamped commit wins over the environment, which may belong to something else.
		{"0.1.0", "4f2a9c1e0b7d55aa1b2c3d4e5f60718293a4b5c6", "0123456789ab", "0.1.0 (4f2a9c1e0b7d)"},
	} {
		Version, Commit = tc.version, tc.commit
		t.Setenv("GIT_COMMIT", tc.env)
		if got := VersionString(); got != tc.want {
			t.Errorf("Version=%q Commit=%q GIT_COMMIT=%q: got %q, want %q", tc.version, tc.commit, tc.env, got, tc.want)
		}
	}
}
