package app

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The operator's money forms are cookie-authenticated (a 12-hour SameSite=Lax cookie), so a
// browser request that began on another site must be refused before it can move a plan or grant
// credit. A non-browser caller (a Bearer script) sends no Fetch metadata and is unaffected.
func TestOperatorFormsRefuseCrossSitePOST(t *testing.T) {
	const secret = "correct-horse-battery-staple"
	_, mux, _ := planBot(t, Config{FreePlanBudgetUSD: 5, OperatorSecret: secret, SupportEmail: testSupportEmail})
	form := url.Values{"secret": {secret}, "org": {"whatever"}, "amount_usd": {"10"}}.Encode()
	for _, path := range []string{"/operator", "/operator/plan", "/operator/credit", "/operator/size", "/operator/cancel", "/operator/enterprise"} {
		r := httptest.NewRequest("POST", path, strings.NewReader(form))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("cross-site POST %s = %d, want 403", path, w.Code)
		}
	}
}
