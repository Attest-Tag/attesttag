package app

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

// The confirmation step reads the method, and the method is not the only place a request can say
// what it wants done. `X-HTTP-Method-Override: DELETE` on a GET is a delete on every upstream
// that honours the convention, and needsConfirm saw a read — so the card nobody had to press was
// the card that never appeared. The headers are the model's to choose (tools_http.go offers them
// in the tool schema), which puts this within reach of anything that can steer a turn: a
// document, a fetched page, a message.
func TestMethodOverrideIsFound(t *testing.T) {
	cases := []struct {
		name string
		req  ProxyRequest
		url  string
		want string
	}{
		{"nothing to find", ProxyRequest{}, "https://api.example.com/v2/thing/1", ""},
		{"the usual header", ProxyRequest{Headers: map[string]string{"X-HTTP-Method-Override": "DELETE"}}, "https://api.example.com/v2/thing/1", "DELETE"},
		{"the header, shouted", ProxyRequest{Headers: map[string]string{"x-http-method-override": "delete"}}, "https://api.example.com/v2/thing/1", "DELETE"},
		{"the shorter spelling", ProxyRequest{Headers: map[string]string{"X-Method-Override": "PUT"}}, "https://api.example.com/v2/thing/1", "PUT"},
		{"the third spelling", ProxyRequest{Headers: map[string]string{"X-HTTP-Method": "PATCH"}}, "https://api.example.com/v2/thing/1", "PATCH"},
		{"in the query string", ProxyRequest{}, "https://api.example.com/v2/thing/1?_method=delete", "DELETE"},
		{"in the query string, other name", ProxyRequest{}, "https://api.example.com/v2/thing/1?_httpmethod=PUT", "PUT"},
		{"in a form body", ProxyRequest{Body: "name=x&_method=delete"}, "https://api.example.com/v2/thing/1", "DELETE"},
		{"in a JSON body", ProxyRequest{Body: `{"_method": "DELETE", "id": 1}`}, "https://api.example.com/v2/thing/1", "DELETE"},

		// Not an override, and must not read as one — a proxy that refuses ordinary traffic gets
		// turned off, which protects nothing.
		{"an override that reads", ProxyRequest{Headers: map[string]string{"X-HTTP-Method-Override": "GET"}}, "https://api.example.com/v2/thing/1", ""},
		{"an empty override", ProxyRequest{Headers: map[string]string{"X-HTTP-Method-Override": ""}}, "https://api.example.com/v2/thing/1", ""},
		{"an ordinary header", ProxyRequest{Headers: map[string]string{"X-Request-Id": "delete"}}, "https://api.example.com/v2/thing/1", ""},
		{"the words in prose", ProxyRequest{Body: `{"note": "the _method field is documented as delete"}`}, "https://api.example.com/v2/thing/1", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, err := url.Parse(c.url)
			if err != nil {
				t.Fatal(err)
			}
			got, where := methodOverride(c.req, u)
			if got != c.want {
				t.Errorf("methodOverride = %q (%s), want %q", got, where, c.want)
			}
			if got != "" && where == "" {
				t.Error("an override was found but not named, so the refusal cannot say where it was")
			}
		})
	}
}

// And it is refused before anything else looks at the request, so the answer cannot be confused
// with "that host is not granted" — and so it holds for a host that *is* granted.
func TestMethodOverrideIsRefusedBeforeTheHostIsMatched(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	p := NewProxy(nil, st)
	call := &Call{TeamID: "T1", OrgID: orgID, Channel: "C1", UserID: "U1"}

	req := ProxyRequest{Method: "GET", URL: "https://api.example.com/v2/thing/1",
		Headers: map[string]string{"X-HTTP-Method-Override": "DELETE"}}
	_, err := p.Do(ctx, orgID, &Access{}, req, callAudit(call), false)
	if err == nil {
		t.Fatal("a method override was allowed through")
	}
	if !strings.Contains(err.Error(), "X-HTTP-Method-Override") || !strings.Contains(err.Error(), "method field") {
		t.Errorf("the refusal does not say what was wrong or what to do instead: %v", err)
	}

	// Recorded, like every other block, so an operator can see it was tried.
	rows, err := st.ProxyAudits(ctx, orgID, "", 10, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !strings.Contains(rows[0].BlockedJ, "X-HTTP-Method-Override") {
		t.Errorf("the attempt was not recorded as a block: %+v", rows)
	}
}
