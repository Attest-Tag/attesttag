package app

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// The page's cookie is a proof of OPERATOR_SECRET that a browser keeps, so that the link in a
// support email is one click and a button rather than a paste. The v1 cookie was HMAC of a fixed
// label under the secret — the same value for the life of that secret, for thirty days at a time,
// bound to no browser and revocable only by rotating the secret itself. Anyone who ever read one
// held the operator API, which lists every organisation on the deployment.
func TestOperatorCookieExpiresAndIsNotTheSecretsShadow(t *testing.T) {
	fixedMasterKey(t)
	const secret = "an-operator-secret-long-enough"
	now := time.Now()
	v := operatorCookieValue(secret, now.Add(operatorCookieTTL))

	if v == "" {
		t.Fatal("no cookie minted")
	}
	if strings.Contains(v, secret) {
		t.Error("the cookie carries the secret it is supposed to stand in for")
	}
	if !operatorCookieOK(v, secret, now) {
		t.Fatal("a cookie just minted does not check out")
	}

	// It runs out on its own. That is the whole point of the change: a leaked cookie stops
	// working without anybody having to notice it leaked.
	if operatorCookieOK(v, secret, now.Add(operatorCookieTTL+time.Minute)) {
		t.Error("an expired cookie still checks out")
	}

	// The expiry is signed, so pushing it out by hand does not work.
	_, mac, _ := strings.Cut(v, ".")
	far := time.Now().Add(365 * 24 * time.Hour).Unix()
	if operatorCookieOK(strconv.FormatInt(far, 10)+"."+mac, secret, now) {
		t.Error("a cookie whose expiry was edited still checks out")
	}

	// Rotating the secret ends every cookie already handed out, which the v1 cookie also did and
	// was the only thing that ever took one back.
	if operatorCookieOK(v, "a-different-operator-secret", now) {
		t.Error("a cookie minted under another secret checks out")
	}

	// A v1 cookie — a bare hex MAC, no expiry — is simply not a cookie any more.
	if operatorCookieOK(strings.Repeat("ab", 32), secret, now) {
		t.Error("the old format still checks out")
	}
}

// Every counter that is a security control rather than a convenience is counted in the database,
// because N instances counting separately give a guesser N times the number the code says. The
// operator's was the one left out: it is built in operatorRoutes rather than in the list at
// bot.go, and that list is where sharing was wired up.
func TestOperatorAttemptsAreCountedAcrossInstances(t *testing.T) {
	b, _, _ := installTestBot(t)
	if b.operatorAttempts == nil {
		t.Fatal("no operator limiter after the routes were registered")
	}
	b.operatorAttempts.mu.Lock()
	shared := b.operatorAttempts.store != nil
	b.operatorAttempts.mu.Unlock()
	if !shared {
		t.Errorf("the operator limiter counts in this process only, so %d attempts an hour is %d per instance",
			operatorAttemptsPerHour, operatorAttemptsPerHour)
	}
}
