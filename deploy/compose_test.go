package deploy

// The compose files, read rather than run. One property, and it is the one that was wrong: a
// password variable must never carry a default.
//
// `${VAR:-default}` substitutes on an *empty* value as well as an unset one, and
// deploy/env/selfhost.env.example ships these keys empty — so somebody who copied the template
// instead of running bootstrap.sh got `attesttag` and `attesttag-minio`, both readable here, on
// a MinIO holding the documents and the database replica. `${VAR:?message}` refuses to start
// and names the script that generates one.
//
// Checked by reading the files because there is no compose on most machines that would run this,
// and because the mistake is a literal two characters.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A ${VAR:-default} substitution where VAR names a credential and the default is not empty.
// `${VAR:-}` is fine and means "optional": it substitutes nothing, which is not a password.
var defaultedSecret = regexp.MustCompile(`\$\{([A-Z0-9_]*(?:PASSWORD|SECRET|KEY|TOKEN)[A-Z0-9_]*):-[^}]`)

func TestNoComposeFileDefaultsACredential(t *testing.T) {
	files := []string{"../docker-compose.yml"}
	local, err := filepath.Glob("local/*.yml")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, local...)
	if len(files) < 3 {
		t.Fatalf("expected the compose files to be found, got %v — has the layout moved?", files)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			for _, m := range defaultedSecret.FindAllStringSubmatch(line, -1) {
				// DOCS_S3_KEY_ID is a key *id*, which is a username; it is named in the same
				// breath as the secret and is not one.
				if strings.HasSuffix(m[1], "_KEY_ID") {
					continue
				}
				t.Errorf("%s:%d gives %s a default, so an empty value in .env becomes a password "+
					"that is published in this repository:\n\t%s", f, i+1, m[1], strings.TrimSpace(line))
			}
		}
	}
}
