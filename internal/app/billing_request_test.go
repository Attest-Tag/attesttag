package app

import (
	"context"
	"strings"
	"testing"
)

// Talk to us is a button: the size with no Price mails support with the account's figures and
// the asker as Reply-To, the organisation remembers it asked, a size that is for sale is bought
// rather than asked for, and three a day is the ceiling because each one lands in an inbox.
func TestTalkToUsMailsSupportWithTheAccountsFigures(t *testing.T) {
	sizeRequests = newRateLimiter()
	cfg := billingCfg()
	cfg.SupportEmail = testSupportEmail
	cfg.StripeSizes = parseSizes("upto_10=price_a:4900:500,over_250=:0")
	b, mux, st := planBot(t, cfg)
	ctx := context.Background()
	orgID, _, tok := seedOrg(t, st, RoleAdmin)
	box := &mailbox{}
	b.mail = box
	ask := func(body string) (int, map[string]any) {
		return call(t, mux, "POST", "/api/billing/size-request", body, "Bearer "+tok)
	}

	code, out := ask(`{"size":"over_250","note":"about 400 people across three workspaces"}`)
	if code != 200 || out["ok"] != true || out["delivered"] != true {
		t.Fatalf("asking about a larger plan: %d %v", code, out)
	}
	if len(box.sent) != 1 {
		t.Fatalf("%d messages sent, want 1", len(box.sent))
	}
	m := box.sent[0]
	if m.To != testSupportEmail || m.ReplyTo != "admin@example.com" {
		t.Errorf("to=%q reply-to=%q, want %q and the requester", m.To, m.ReplyTo, testSupportEmail)
	}
	if m.Subject != "Upgrade request from Test Org" {
		t.Errorf("subject = %q", m.Subject)
	}
	for _, want := range []string{"More than 250 users", "Test Org", "admin@example.com", "400 people", "used the bot in the last 30 days", "fix jobs"} {
		if !strings.Contains(m.Body, want) {
			t.Errorf("the message support reads lacks %q:\n%s", want, m.Body)
		}
	}
	if st.Setting(ctx, orgID, sizeRequestKey) == "" || st.Setting(ctx, orgID, sizeRequestSizeKey) != "over_250" {
		t.Error("the request was not recorded against the organisation")
	}
	code, view := call(t, mux, "GET", "/api/billing", "", "Bearer "+tok)
	req, _ := view["size_request"].(map[string]any)
	if code != 200 || req == nil || req["size_label"] != "More than 250 users" || req["at"] == "" {
		t.Errorf("the billing screen does not say it asked: %d %v", code, view["size_request"])
	}

	// A size with a price is bought on the screen, not asked for by mail.
	if code, _ := ask(`{"size":"upto_10"}`); code != 400 {
		t.Errorf("asking for a size that is for sale answered %d, want 400", code)
	}
	if code, _ := ask(`{"size":"nothing"}`); code != 400 {
		t.Errorf("asking for an unknown size answered %d, want 400", code)
	}
	if len(box.sent) != 1 {
		t.Errorf("%d messages sent after refused requests, want still 1", len(box.sent))
	}

	// Three a day.
	for i := 0; i < 2; i++ {
		if code, _ := ask(`{"size":"over_250"}`); code != 200 {
			t.Fatalf("request %d answered %d", i+2, code)
		}
	}
	if code, _ := ask(`{"size":"over_250"}`); code != 429 {
		t.Errorf("a fourth request in a day answered %d, want 429", code)
	}
}

// With no support address there is nobody to mail, so the route refuses and nothing is sent.
func TestTalkToUsNeedsASupportAddress(t *testing.T) {
	sizeRequests = newRateLimiter()
	cfg := billingCfg()
	cfg.StripeSizes = parseSizes("over_250=:0")
	b, mux, st := planBot(t, cfg)
	_, _, tok := seedOrg(t, st, RoleAdmin)
	box := &mailbox{}
	b.mail = box
	if code, _ := call(t, mux, "POST", "/api/billing/size-request", `{"size":"over_250"}`, "Bearer "+tok); code != 400 {
		t.Errorf("answered %d with no support address, want 400", code)
	}
	if len(box.sent) != 0 {
		t.Errorf("%d messages sent to nobody", len(box.sent))
	}
}
