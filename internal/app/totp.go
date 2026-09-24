package app

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Time-based one-time passwords (RFC 6238): the second factor on a console sign-in.
//
// Written on the standard library rather than pulled from a module. The algorithm is an
// HMAC-SHA1 over a counter and it fits on a screen, its test vectors are published in the RFC
// and pinned in totp_test.go, and it will not change again — so a dependency here would buy
// nothing and widen the supply chain of the thing that guards every account.
//
// SHA-1 is not a choice: it is what every authenticator app implements. HMAC-SHA1 is unaffected
// by the collision attacks that retired SHA-1 for signatures, and the secret never leaves here.

const (
	totpDigits = 6
	totpPeriod = 30 * time.Second
	// One step either side of now. Phones drift and people finish typing late; two steps of
	// tolerance is the usual reading of the RFC's "at most one" and costs 90 seconds of replay
	// window, which the used-step check below closes anyway.
	totpSkew = 1
)

// Authenticator apps read unpadded base32, and several refuse the '=' padding outright.
var totpEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// newTOTPSecret returns a fresh 160-bit secret, base32 as the apps expect it.
func newTOTPSecret() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return totpEncoding.EncodeToString(b), nil
}

// totpStep is the counter a code is derived from: which 30-second window t falls in. It is
// returned alongside a match so an accepted code can be remembered and refused a second time.
func totpStep(t time.Time) int64 { return t.Unix() / int64(totpPeriod/time.Second) }

func totpCodeAt(secret string, step int64) (string, error) {
	key, err := totpEncoding.DecodeString(normalTOTPSecret(secret))
	if err != nil {
		return "", fmt.Errorf("bad secret: %w", err)
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	// Dynamic truncation, RFC 4226 §5.3: the low nibble of the last byte picks the offset.
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, v%1_000_000), nil
}

// checkTOTP tests one typed code against the window around now. The step it matched comes back
// with it: the caller stores that and refuses anything at or below it next time, so a code read
// over somebody's shoulder — or off a phishing page — is worth one use and not ninety seconds of
// them. `after` is the last step this account already spent; pass 0 when there is none.
func checkTOTP(secret, code string, now time.Time, after int64) (int64, bool) {
	code = strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' {
			return -1
		}
		return r
	}, strings.TrimSpace(code))
	if len(code) != totpDigits {
		return 0, false
	}
	center := totpStep(now)
	for step := center - totpSkew; step <= center+totpSkew; step++ {
		if step <= after {
			continue
		}
		want, err := totpCodeAt(secret, step)
		if err != nil {
			return 0, false
		}
		// Constant time: the comparison is against a secret-derived value, and a timing signal
		// here would let somebody learn a code digit by digit.
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

func normalTOTPSecret(s string) string {
	return strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "=", "").Replace(s))
}

// otpauthURI is the string the QR code carries. The label is "issuer:account" and the issuer is
// repeated as a parameter, which is what the apps actually read; without it the entry shows up
// as a bare email address among everything else the person has enrolled.
func otpauthURI(issuer, account, secret string) string {
	q := url.Values{
		"secret": {normalTOTPSecret(secret)}, "issuer": {issuer},
		"algorithm": {"SHA1"}, "digits": {fmt.Sprint(totpDigits)},
		"period": {fmt.Sprint(int(totpPeriod / time.Second))},
	}
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + q.Encode()
}

// ---- recovery codes ----

// What gets you back in when the phone is gone. Ten of them, single use, shown once at
// enrolment and never again — regenerating replaces the lot.
const recoveryCodeCount = 10

// Crockford's base32 without the letters that read as digits, so a code copied off a screen by
// hand does not fail on I/1 or O/0.
const recoveryAlphabet = "23456789ABCDEFGHJKMNPQRSTVWXYZ"

// newRecoveryCodes returns codes to show the person, and the hashes to keep.
func newRecoveryCodes() (codes []string, hashes []string, err error) {
	for range recoveryCodeCount {
		raw := make([]byte, 10)
		if _, err := rand.Read(raw); err != nil {
			return nil, nil, err
		}
		out := make([]byte, 10)
		for i, b := range raw {
			out[i] = recoveryAlphabet[int(b)%len(recoveryAlphabet)]
		}
		code := string(out[:5]) + "-" + string(out[5:])
		codes = append(codes, code)
		hashes = append(hashes, hashRecoveryCode(code))
	}
	return codes, hashes, nil
}

// hashRecoveryCode is a plain SHA-256, deliberately, where a password would get bcrypt: these
// codes are 50 bits of machine-chosen randomness rather than something a person invented, so
// there is no dictionary to run and nothing for a slow hash to buy. What it does buy is a
// database dump that does not hand over ten working second factors.
func hashRecoveryCode(code string) string {
	sum := sha256.Sum256([]byte(normalRecoveryCode(code)))
	return hex.EncodeToString(sum[:])
}

func normalRecoveryCode(s string) string {
	return strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(s)))
}
