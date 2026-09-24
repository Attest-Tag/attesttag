package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A delivery is believed only when its token is Microsoft's, for this bot, for the service URL the
// activity names, and signed with a key Microsoft endorses for Teams. Each case below is one way a
// forged or misdirected delivery would otherwise get in.
func TestABotFrameworkTokenIsVerifiedBeforeAnythingIsBelieved(t *testing.T) {
	f := newFakeMicrosoft(t)
	v := f.client(nil).verifier
	ctx := context.Background()

	if _, err := v.verify(ctx, "Bearer "+f.sign(nil), msteamsChannelID, f.srv.URL); err != nil {
		t.Fatalf("a genuine token was refused: %v", err)
	}
	// A trailing slash is the same service URL; Microsoft sends both forms.
	if _, err := v.verify(ctx, "Bearer "+f.sign(nil), msteamsChannelID, f.srv.URL+"/"); err != nil {
		t.Fatalf("the same service URL with a slash was refused: %v", err)
	}

	cases := []struct {
		name, header, channel, serviceURL string
	}{
		{"no header", "", msteamsChannelID, f.srv.URL},
		{"not a bearer token", "Basic abc", msteamsChannelID, f.srv.URL},
		{"another bot's token", "Bearer " + f.sign(map[string]any{"aud": "someone-else"}), msteamsChannelID, f.srv.URL},
		{"another issuer", "Bearer " + f.sign(map[string]any{"iss": "https://sts.windows.net/x/"}), msteamsChannelID, f.srv.URL},
		{"expired", "Bearer " + f.sign(map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}), msteamsChannelID, f.srv.URL},
		{"no expiry", "Bearer " + f.sign(map[string]any{"exp": nil}), msteamsChannelID, f.srv.URL},
		{"not valid yet", "Bearer " + f.sign(map[string]any{"nbf": time.Now().Add(time.Hour).Unix()}), msteamsChannelID, f.srv.URL},
		// A genuine token carrying an activity that points replies at a host of the sender's choosing.
		{"service url swapped", "Bearer " + f.sign(nil), msteamsChannelID, "https://attacker.example"},
		// The fake's key is endorsed for msteams and webchat only.
		{"channel the key is not endorsed for", "Bearer " + f.sign(nil), "slack", f.srv.URL},
	}
	for _, c := range cases {
		if _, err := v.verify(ctx, c.header, c.channel, c.serviceURL); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}

	// Tampering with the claims after signing breaks the signature.
	parts := strings.Split(f.sign(nil), ".")
	var claims map[string]any
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	json.Unmarshal(raw, &claims)
	claims["aud"] = fakeAppID
	claims["exp"] = time.Now().Add(24 * time.Hour).Unix()
	forged, _ := json.Marshal(claims)
	parts[1] = base64.RawURLEncoding.EncodeToString(forged)
	if _, err := v.verify(ctx, "Bearer "+strings.Join(parts, "."), msteamsChannelID, f.srv.URL); err == nil {
		t.Error("a token whose claims were edited after signing was accepted")
	}

	// alg "none" with no signature: the classic way to make a token vouch for itself.
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","kid":"test-key"}`))
	if _, err := v.verify(ctx, "Bearer "+head+"."+parts[1]+".", msteamsChannelID, f.srv.URL); err == nil {
		t.Error(`a token with alg "none" was accepted`)
	}
}

// A token naming a key the bot has never seen reloads Microsoft's keys, so a key Microsoft rotated
// is picked up at once — including in the process's first minutes, which the first version of
// this got wrong by counting the initial load as a miss. But a key that is still unknown after that
// does not reload them again on every request, or anyone could make the bot fetch Microsoft's keys
// once per message they sent.
func TestARotatedSigningKeyIsPickedUpButAnUnknownOneIsNotRefetchedEveryTime(t *testing.T) {
	f := newFakeMicrosoft(t)
	v := f.client(nil).verifier
	ctx := context.Background()
	if _, err := v.verify(ctx, "Bearer "+f.sign(nil), msteamsChannelID, f.srv.URL); err != nil {
		t.Fatal(err)
	}
	f.kid = "rotated"
	if _, err := v.verify(ctx, "Bearer "+f.sign(nil), msteamsChannelID, f.srv.URL); err != nil {
		t.Fatalf("a token signed with the key Microsoft rotated to was refused: %v", err)
	}
	fetches := f.keyFetches

	// A token naming a key Microsoft does not publish: refused, and after the one reload it earns,
	// the next ones are refused without asking Microsoft again.
	signed := f.sign(nil)
	f.kid = "rotated" // the fake keeps publishing "rotated"; the token names another
	forged := strings.Replace(signed, strings.Split(signed, ".")[0],
		base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"nobody-has-this","typ":"JWT"}`)), 1)
	for i := 0; i < 3; i++ {
		if _, err := v.verify(ctx, "Bearer "+forged, msteamsChannelID, f.srv.URL); err == nil {
			t.Fatal("a token naming a key that is not published was accepted")
		}
	}
	if got := f.keyFetches - fetches; got > 1 {
		t.Errorf("three tokens naming an unknown key fetched Microsoft's keys %d times, want at most once", got)
	}
}

// The bot's own token is fetched once and reused until shortly before it expires, and a refusal
// says why in Microsoft's words, which is what somebody debugging an expired secret needs.
func TestTheBotTokenIsCachedAndARefusalSaysWhy(t *testing.T) {
	f := newFakeMicrosoft(t)
	c := f.client(nil)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		tok, err := c.botToken(ctx)
		if err != nil || tok != "bot-token" {
			t.Fatalf("token %q, err %v", tok, err)
		}
	}
	if f.tokenRequests != 1 {
		t.Errorf("asked Microsoft for a token %d times for three uses, want once", f.tokenRequests)
	}

	bad := f.client(nil)
	bad.tokens.secret = "wrong"
	_, err := bad.botToken(ctx)
	if err == nil || !strings.Contains(err.Error(), "AADSTS7000215") {
		t.Errorf("a refused token should carry Microsoft's reason, got %v", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Error("the error repeated the client secret")
	}
}

// Replies go only to the Bot Framework's own hosts, whatever a delivery says.
func TestOnlyBotFrameworkHostsAreServiceURLs(t *testing.T) {
	for u, want := range map[string]bool{
		"https://smba.trafficmanager.net/amer/":      true,
		"https://smba.trafficmanager.net/teams/":     true,
		"https://europe.webchat.botframework.com/":   true,
		"http://smba.trafficmanager.net/amer/":       false, // not https
		"https://smba.trafficmanager.net.evil.com/":  false,
		"https://evil.com/smba.trafficmanager.net/":  false,
		"https://user@smba.trafficmanager.net/amer/": false,
		"https://169.254.169.254/":                   false,
	} {
		if got := msteamsServiceHost(u); got != want {
			t.Errorf("msteamsServiceHost(%q) = %v, want %v", u, got, want)
		}
	}
}
