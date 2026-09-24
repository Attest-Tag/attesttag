package app

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The console's API reference is written by hand — its own comment says it has to be kept in step
// with api_v1.go by whoever changes that file — and the column a developer reads to find out why a
// key got a 403 is the one that drifted: /v1/artifacts and /v1/jobs ask for artifacts.view and
// jobs.view, and the reference showed them as open to any key. This pins the reference to the
// routes, the way TestManifestAsksForTheScopesTheCodeAsksFor pins the Slack manifest: the same
// endpoints, each naming the permission its route asks for and no other.
func TestTheAPIReferenceNamesThePermissionEachRouteAsksFor(t *testing.T) {
	roles, err := os.ReadFile("console_roles.go")
	if err != nil {
		t.Fatal(err)
	}
	perms := map[string]string{}
	for _, m := range regexp.MustCompile(`(Perm\w+)\s+Permission\s*=\s*"([^"]+)"`).FindAllStringSubmatch(string(roles), -1) {
		perms[m[1]] = m[2]
	}

	src, err := os.ReadFile("api_v1.go")
	if err != nil {
		t.Fatal(err)
	}
	// "GET /v1/jobs" → "jobs.view", or "" for a route any key may call. A path parameter that
	// takes the rest of the path is spelled {path...} in the mux and {path} in the reference.
	routes := map[string]string{}
	for _, m := range regexp.MustCompile(`mux\.HandleFunc\("([A-Z]+ /v1/[^"]*)", (kp?)\((Perm\w+)?`).FindAllStringSubmatch(string(src), -1) {
		route, need := strings.ReplaceAll(m[1], "...}", "}"), ""
		if m[2] == "kp" {
			if need = perms[m[3]]; need == "" {
				t.Fatalf("%s asks for %s, which console_roles.go does not define", route, m[3])
			}
		}
		routes[route] = need
	}
	if len(routes) < 20 {
		t.Fatalf("read %d routes out of api_v1.go; the pattern here no longer matches how they are registered", len(routes))
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", "ui", "src", "components", "developer", "api-reference.tsx"))
	if err != nil {
		t.Fatalf("the console's API reference is missing: %v", err)
	}
	ref := string(raw)
	start := strings.Index(ref, "const ENDPOINTS")
	end := strings.Index(ref[start:], "\n];")
	if start < 0 || end < 0 {
		t.Fatal("the API reference no longer declares its ENDPOINTS list where this test reads it")
	}
	field := func(entry, name string) string {
		m := regexp.MustCompile(`(?m)^    ` + name + `: "([^"]*)"`).FindStringSubmatch(entry)
		if m == nil {
			return ""
		}
		return m[1]
	}
	documented := map[string]string{}
	for _, entry := range strings.Split(ref[start:start+end], "\n  {\n")[1:] {
		documented[field(entry, "method")+" "+field(entry, "path")] = field(entry, "permission")
	}

	for route, need := range routes {
		got, ok := documented[route]
		switch {
		case !ok:
			t.Errorf("%s is served, and the API reference does not list it", route)
		case got != need:
			t.Errorf("the API reference says %s needs %q; the route asks for %q", route, got, need)
		}
	}
	for route := range documented {
		if _, ok := routes[route]; !ok {
			t.Errorf("the API reference lists %s, which nothing serves", route)
		}
	}
}
