package app

import "testing"

// Which endpoint the console's Test button calls. The button used to make the preset's check
// call unconditionally, so a custom API allowed only under /api/v2 was tested at the site root:
// a call the connection's own prefixes refuse, reported as a failure that says nothing about
// the credential. A preset whose check call already lives under the prefix keeps it — the
// curated endpoint answers something, where the bare prefix usually 404s.
func TestTestPathPrefersTheConnectionsOwnPath(t *testing.T) {
	custom := &Preset{Test: TestCall{Method: "GET", Path: "/"}}
	stripe := &Preset{Test: TestCall{Method: "GET", Path: "/v1/balance"}}
	linear := &Preset{Test: TestCall{Method: "POST", Path: "/graphql", Body: `{"query":"{ viewer { id } }"}`}}

	for _, tc := range []struct {
		name       string
		pr         *Preset
		prefixes   []string
		path, body string
	}{
		{"no prefixes: the preset's check call, untouched", custom, nil, "/", ""},
		{"narrowed to one path: that path", custom, []string{"/api/v2"}, "/api/v2", ""},
		{"the first prefix, not the widest", custom, []string{"/api/v1/billing/", "/"}, "/api/v1/billing/", ""},
		{"a prefix typed without its slash still names a path", custom, []string{"api/v1/billing/"}, "/api/v1/billing/", ""},
		{"the preset's endpoint already lives there: keep it", stripe, []string{"/v1"}, "/v1/balance", ""},
		// A POST check call is never moved: its body was written for that one endpoint, and a
		// write pointed somewhere merely because it is allowed is not a test worth making.
		{"a write-shaped check call keeps its endpoint", linear, []string{"/graphql"}, "/graphql", `{"query":"{ viewer { id } }"}`},
		{"and keeps it even when the prefixes exclude it", linear, []string{"/issues/"}, "/graphql", `{"query":"{ viewer { id } }"}`},
	} {
		path, body := testPath(tc.pr, &Connection{PathPrefixes: tc.prefixes})
		if path != tc.path || body != tc.body {
			t.Errorf("%s: testPath = (%q, %q), want (%q, %q)", tc.name, path, body, tc.path, tc.body)
		}
	}
}
