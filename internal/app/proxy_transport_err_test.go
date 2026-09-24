package app

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

// A transport failure — a refused redirect, a timeout, a dial error — comes back from net/http as a
// *url.Error carrying the whole request URL, and a connection whose credential travels as a query
// parameter has that credential in the URL. What the proxy returns goes to the model, the thread
// and the activity log, so the URL must not survive the rewrite; the host and the cause must.
func TestSafeTransportErrDropsTheURL(t *testing.T) {
	const secret = "QK9f3bSECRETtoken"
	ue := &url.Error{Op: "Get", URL: "https://api.example.com/data?appid=" + secret, Err: errors.New("net/http: timeout awaiting response headers")}
	got := safeTransportErr("GET", "api.example.com", ue).Error()
	if strings.Contains(got, secret) {
		t.Errorf("the credential survived in the error: %q", got)
	}
	if !strings.Contains(got, "api.example.com") || !strings.Contains(got, "timeout") {
		t.Errorf("the host or cause was lost: %q", got)
	}
	// A plain error with no URL passes through unchanged.
	if got := safeTransportErr("GET", "h", errors.New("boom")).Error(); got != "boom" {
		t.Errorf("a non-url error was rewritten: %q", got)
	}
}
