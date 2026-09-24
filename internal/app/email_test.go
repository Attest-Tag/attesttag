package app

// Mail is optional everywhere, including on a managed deployment. It used to be fatal there — a
// Cloud Run service with no RESEND_API_KEY refused to start — which meant a deployment could be
// held up by a mail provider nobody had signed up for yet. These tests hold the line that a
// missing or half-finished mail configuration degrades to logging and never stops the process,
// and that what degrades reports itself as unconfigured rather than quietly swallowing mail.

import "testing"

func TestMailerIsOptionalOnAManagedDeployment(t *testing.T) {
	t.Setenv("K_SERVICE", "attesttag")
	t.Setenv("RESEND_API_KEY", "")
	t.Setenv("MAIL_FROM", "")

	m := NewMailer()
	if m == nil {
		t.Fatal("a managed deployment with no RESEND_API_KEY got no mailer: startup would have nothing to send with")
	}
	if m.Configured() {
		t.Fatal("the log mailer reported itself as configured: the console would promise an inbox nothing reaches")
	}
}

// A key with no MAIL_FROM cannot send — Resend rejects the call — so it degrades the same way
// rather than taking the deployment down with it.
func TestMailerWithoutFromAddressDegrades(t *testing.T) {
	t.Setenv("K_SERVICE", "attesttag")
	t.Setenv("RESEND_API_KEY", "re_test")
	t.Setenv("MAIL_FROM", "")

	if m := NewMailer(); m == nil || m.Configured() {
		t.Fatal("RESEND_API_KEY without MAIL_FROM should degrade to the log mailer")
	}
}

func TestMailerSendsWhenBothAreSet(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "re_test")
	t.Setenv("MAIL_FROM", "attest_tag <no-reply@mail.example.com>")

	m := NewMailer()
	if !m.Configured() {
		t.Fatal("a key and a from address should give a mailer that sends")
	}
}
