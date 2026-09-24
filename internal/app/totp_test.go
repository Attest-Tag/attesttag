package app

import (
	"encoding/base32"
	"testing"
	"time"
)

// RFC 6238's own SHA-1 vectors, truncated to the six digits we ask for. If a phone and this
// package ever disagree, it is this table that says which one is wrong.
func TestTOTPMatchesRFC6238(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	for _, c := range []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	} {
		got, err := totpCodeAt(secret, totpStep(time.Unix(c.unix, 0)))
		if err != nil || got != c.want {
			t.Errorf("code at %d = %q (%v), want %q", c.unix, got, err, c.want)
		}
	}
}

func TestTOTPAcceptsDriftAndRefusesReuse(t *testing.T) {
	secret, err := newTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	code, _ := totpCodeAt(secret, totpStep(now))

	step, ok := checkTOTP(secret, code, now, 0)
	if !ok || step != totpStep(now) {
		t.Fatalf("the current code was refused: step=%d ok=%v", step, ok)
	}
	// Spaces and dashes are how people copy a code off a screen.
	if _, ok := checkTOTP(secret, "  "+code[:3]+" "+code[3:], now, 0); !ok {
		t.Error("a code typed with a space in it was refused")
	}
	// A clock a step behind ours still works; two steps does not.
	if _, ok := checkTOTP(secret, code, now.Add(totpPeriod), 0); !ok {
		t.Error("a code one step old was refused: no tolerance for drift")
	}
	if _, ok := checkTOTP(secret, code, now.Add(3*totpPeriod), 0); ok {
		t.Error("a code three steps old was accepted")
	}
	// The point of remembering the step: the same code cannot be spent twice.
	if _, ok := checkTOTP(secret, code, now, step); ok {
		t.Error("a code was accepted a second time within its window")
	}
	if _, ok := checkTOTP(secret, "000000", now, 0); ok {
		t.Error("a made-up code was accepted")
	}
}

func TestRecoveryCodesAreDistinctAndForgiving(t *testing.T) {
	codes, hashes, err := newRecoveryCodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != recoveryCodeCount || len(hashes) != recoveryCodeCount {
		t.Fatalf("got %d codes and %d hashes", len(codes), len(hashes))
	}
	seen := map[string]bool{}
	for i, c := range codes {
		if seen[c] {
			t.Fatalf("recovery code %q was issued twice", c)
		}
		seen[c] = true
		// Typed back in lower case, with the dash left out: still the same code.
		if got := hashRecoveryCode(normalRecoveryCode("  " + c + "  ")); got != hashes[i] {
			t.Errorf("a code copied with whitespace hashed differently")
		}
		if hashRecoveryCode(c) == c {
			t.Error("the stored hash is the code itself")
		}
	}
}

func TestOtpauthURICarriesTheIssuer(t *testing.T) {
	uri := otpauthURI("attest_tag", "founder@example.com", "JBSWY3DPEHPK3PXP")
	for _, want := range []string{"otpauth://totp/attest_tag:founder@example.com", "issuer=attest_tag", "digits=6", "period=30"} {
		if !contains(uri, want) {
			t.Errorf("uri %q is missing %q", uri, want)
		}
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
