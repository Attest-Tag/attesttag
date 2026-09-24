package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"strings"
	"testing"
	"time"
)

// attest_tag writes Slack's dialect everywhere and Teams is taught it once, on the way out. A
// mention becomes a real Teams mention only for somebody the bot knows how to address; a link,
// a channel and emphasis come out as Teams draws them; and nothing inside code is touched.
func TestSlackDialectIsRenderedForTeams(t *testing.T) {
	r := msRenderer{
		who: func(id string) (string, string) {
			if id == "8f3b1c2d-0000-4000-8000-000000000001" {
				return "Ana <Ops>", "29:ana"
			}
			return "", ""
		},
		channel: func(id string) string { return map[string]string{"19:x@thread.tacv2": "Eng › general"}[id] },
	}
	in := "*Waiting on* <@8f3b1c2d-0000-4000-8000-000000000001> and <@8f3b1c2d-0000-4000-8000-000000000001> in <#19:x@thread.tacv2>. " +
		"See <https://example.com/doc|the doc>, <https://example.com/raw>, ~gone~ and `*not bold* <https://code|x>`. " +
		"Unknown <@8f3b1c2d-0000-4000-8000-000000000009>."
	md, mentions := r.markdown(in, true)

	for _, want := range []string{
		"**Waiting on**", "<at>Ana &lt;Ops&gt;</at>", "in Eng › general.", "[the doc](https://example.com/doc)",
		"https://example.com/raw", "~~gone~~", "`*not bold* <https://code|x>`", "Unknown someone.",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("rendered text lacks %q:\n%s", want, md)
		}
	}
	if len(mentions) != 1 || mentions[0].Mentioned.ID != "29:ana" || mentions[0].Text != "<at>Ana &lt;Ops&gt;</at>" {
		t.Errorf("mentions = %+v, want one entity for Ana however often she is named", mentions)
	}

	// An answer the model wrote in Markdown keeps its own emphasis: single asterisks are its italics.
	answer, _ := r.markdown("this is *emphasis*", false)
	if answer != "this is *emphasis*" {
		t.Errorf("a Markdown answer was rewritten as mrkdwn: %q", answer)
	}
}

// A message from Teams arrives in the internal dialect: the bot's own mention is dropped, a person
// becomes <@object-id>, Teams' HTML goes, and what somebody typed can never become markup — typing
// "<@U123>" in Teams mentions nobody, exactly as it does in Slack.
func TestATeamsMessageArrivesInTheInternalDialect(t *testing.T) {
	text := "<at>attest_tag</at>&nbsp;ask <at>Ana</at> about <b>this</b>:<br>it's &lt;@U123&gt; and a < b > c\x00"
	got := fromTeams(text, map[string]bool{"attest_tag": true}, map[string]string{"Ana": "8f3b1c2d-0000-4000-8000-000000000001"})
	want := "ask <@8f3b1c2d-0000-4000-8000-000000000001> about this:\nit's &lt;@U123&gt; and a &lt; b &gt; c"
	if got != want {
		t.Errorf("fromTeams:\n got %q\nwant %q", got, want)
	}
	// The raw < from the message is escaped, so nothing downstream can read a mention into it.
	if n := len(mentionIDRe.FindAllString(got, -1)); n != 1 {
		t.Errorf("%d mentions readable in %q, want only Ana's", n, got)
	}
	// Somebody tagged whom the bot cannot place keeps their name and gains no id.
	if got := fromTeams("<at>Bo</at> hi", nil, nil); got != "Bo hi" {
		t.Errorf("an unknown mention became %q", got)
	}
}

// A card comes out in the order a Slack card has — words, buttons, small print — with each press
// carrying back the action, the value and what the card said.
func TestACardIsDrawnAsAnAdaptiveCardInSlackOrder(t *testing.T) {
	card := accessCard(&AccessRequest{ID: 7, Requester: "8f3b1c2d-0000-4000-8000-000000000001", What: "a seat",
		Channel: "19:x@thread.tacv2", ExpiresAt: "2026-09-23 12:00:00"}, "Ana", "Eng › general", false)
	card.Buttons = append(card.Buttons, Button{ActionID: actConnectOpen, Value: "open", Label: "Open", URL: "https://example.com"})
	ac := msRenderer{}.adaptiveCard(card)
	raw, _ := json.Marshal(ac)
	var got struct {
		Type, Version string
		Body          []struct {
			Type, Text, Size string
			Actions          []struct {
				Type, Title, Style, URL string
				Data                    map[string]string
			}
		}
	}
	json.Unmarshal(raw, &got)
	if got.Type != "AdaptiveCard" || got.Version == "" {
		t.Fatalf("not an adaptive card: %s", raw)
	}
	var kinds []string
	for _, b := range got.Body {
		kinds = append(kinds, b.Type)
	}
	if len(kinds) < 3 || kinds[len(kinds)-2] != "ActionSet" || kinds[len(kinds)-1] != "TextBlock" {
		t.Fatalf("body order %v, want the text, then the buttons, then the footer", kinds)
	}
	if got.Body[len(got.Body)-1].Size != "Small" {
		t.Error("the footer is not small print")
	}
	acts := got.Body[len(got.Body)-2].Actions
	approve := acts[0]
	if approve.Type != "Action.Submit" || approve.Style != "positive" || approve.Data[msCardAction] != actAccessApprove ||
		approve.Data[msCardValue] != confirmValue(7, "") || approve.Data[msCardSummary] == "" {
		t.Errorf("Approve is %+v", approve)
	}
	if last := acts[len(acts)-1]; last.Type != "Action.OpenUrl" || last.URL != "https://example.com" {
		t.Errorf("a link button is %+v, want it to open its URL", last)
	}
}

// The log is read back in the order things happened, whatever the ids are, and a chat — which is one
// thread for as long as it exists — gives its newest messages, with a channel thread's root kept
// even when there are more than the cap.
func TestTheTeamsLogReadsBackInOrder(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	team := "msteams:tenant-1"
	base := time.Date(2026, 9, 22, 18, 0, 0, 0, time.UTC)
	// Written out of order, with ids that do not sort, and with times whose RFC3339 forms would
	// mis-sort as text (.1 after .123).
	for _, m := range []msMessage{
		{Channel: "19:c@thread.tacv2", Thread: "b", ID: "z-reply", Text: "second", At: msTime(base.Add(123 * time.Millisecond))},
		{Channel: "19:c@thread.tacv2", Thread: "b", ID: "b", Text: "root", At: msTime(base)},
		{Channel: "19:c@thread.tacv2", Thread: "b", ID: "a-reply", Text: "first", At: msTime(base.Add(100 * time.Millisecond))},
		{Channel: "19:c@thread.tacv2", Thread: "q", ID: "q", Text: "another post", At: msTime(base.Add(time.Second))},
	} {
		if err := st.LogTeamsMessage(ctx, team, m); err != nil {
			t.Fatal(err)
		}
	}
	thread, err := st.TeamsThread(ctx, team, "19:c@thread.tacv2", "b")
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, m := range thread {
		texts = append(texts, m.Text)
	}
	if strings.Join(texts, ",") != "root,first,second" {
		t.Errorf("thread read back as %v", texts)
	}
	posts, err := st.TeamsHistory(ctx, team, "19:c@thread.tacv2", base.Add(-time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 2 || posts[0].ID != "b" || posts[0].Replies != 2 || posts[1].Replies != 0 {
		t.Errorf("history = %+v, want two posts, the first with two replies", posts)
	}
	// Another tenant's log is another tenant's.
	if other, _ := st.TeamsThread(ctx, "msteams:tenant-2", "19:c@thread.tacv2", "b"); len(other) != 0 {
		t.Errorf("tenant 2 read %d of tenant 1's messages", len(other))
	}
}

// A link code joins a tenant once, to the organisation that minted it, within its lifetime and for
// its platform only.
func TestALinkCodeIsSingleUseShortLivedAndBoundToItsOrganisation(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	code, expires, err := st.NewLinkCode(ctx, 42, platformMSTeams, 7)
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(expires) > linkCodeTTL || time.Until(expires) < linkCodeTTL-time.Minute {
		t.Errorf("expires in %v, want about %v", time.Until(expires), linkCodeTTL)
	}
	if _, err := st.SpendLinkCode(ctx, code, platformSlack, "T1"); err == nil {
		t.Error("a Teams code linked a Slack workspace")
	}
	// Case and the dash are how people retype it; neither matters.
	org, err := st.SpendLinkCode(ctx, strings.ToLower(strings.ReplaceAll(code, "-", "")), platformMSTeams, "msteams:t1")
	if err != nil || org != 42 {
		t.Fatalf("spend = %d, %v", org, err)
	}
	if _, err := st.SpendLinkCode(ctx, code, platformMSTeams, "msteams:t2"); err == nil {
		t.Error("a code linked a second tenant")
	}
	stale, _, _ := st.NewLinkCode(ctx, 42, platformMSTeams, 7)
	if _, err := st.db.ExecContext(ctx, `update link_codes set expires_at=? where code_hash=?`,
		msTime(time.Now().Add(-time.Minute)), linkCodeHash(stale)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SpendLinkCode(ctx, stale, platformMSTeams, "msteams:t3"); err == nil {
		t.Error("an expired code linked a tenant")
	}
}

// The package a tenant admin uploads asks for exactly the resource-specific permissions the code
// declares, names this deployment's bot, and carries both icons at the root, at the sizes Teams
// requires — the Teams twin of TestManifestAsksForTheScopesTheCodeAsksFor.
func TestTeamsManifestAsksForThePermissionsTheCodeUses(t *testing.T) {
	c := &msteamsClient{appID: fakeAppID}
	m := c.msteamsManifest("https://bot.example.com")
	raw, _ := json.Marshal(m)
	var got struct {
		ID                 string `json:"id"`
		WebApplicationInfo struct {
			ID, Resource string
		} `json:"webApplicationInfo"`
		Authorization struct {
			Permissions struct {
				ResourceSpecific []struct{ Name, Type string } `json:"resourceSpecific"`
			} `json:"permissions"`
		} `json:"authorization"`
		Bots []struct {
			BotID  string   `json:"botId"`
			Scopes []string `json:"scopes"`
		} `json:"bots"`
		ValidDomains            []string `json:"validDomains"`
		ManifestVersion         string   `json:"manifestVersion"`
		SupportsChannelFeatures string   `json:"supportsChannelFeatures"`
	}
	json.Unmarshal(raw, &got)
	if got.ID != fakeAppID || got.WebApplicationInfo.ID != fakeAppID || got.WebApplicationInfo.Resource == "" {
		t.Errorf("app ids: %+v", got)
	}
	// Found by uploading the package to a real tenant: from manifest 1.25 Teams rejects an app with
	// the team scope that does not declare this, with a validation error and nothing installed.
	if got.ManifestVersion >= "1.25" && got.SupportsChannelFeatures != "tier1" {
		t.Errorf("a %s manifest with the team scope needs supportsChannelFeatures tier1, has %q", got.ManifestVersion, got.SupportsChannelFeatures)
	}
	if len(got.Bots) != 1 || got.Bots[0].BotID != fakeAppID || len(got.Bots[0].Scopes) != 3 {
		t.Errorf("bots: %+v", got.Bots)
	}
	var names []string
	for _, p := range got.Authorization.Permissions.ResourceSpecific {
		if p.Type != "Application" {
			t.Errorf("%s is %s, want an application permission", p.Name, p.Type)
		}
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != strings.Join(msteamsRSC, ",") {
		t.Errorf("manifest asks for %v, the code for %v", names, msteamsRSC)
	}
	if len(got.ValidDomains) != 1 || got.ValidDomains[0] != "bot.example.com" {
		t.Errorf("validDomains = %v", got.ValidDomains)
	}

	zipped, err := c.msteamsPackage("https://bot.example.com")
	if err != nil {
		t.Fatal(err)
	}
	for name, size := range map[string][2]int{"color.png": {192, 192}, "outline.png": {32, 32}} {
		img := pngInZip(t, zipped, name)
		if b := img.Bounds(); b.Dx() != size[0] || b.Dy() != size[1] {
			t.Errorf("%s is %dx%d, want %dx%d", name, b.Dx(), b.Dy(), size[0], size[1])
		}
	}
	// Teams draws the outline icon white on its own background, so it has to be white on nothing.
	// The first one made for this was flattened onto white by the tool that drew it: a solid square.
	outline := pngInZip(t, zipped, "outline.png")
	clear, white := 0, 0
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			r, g, b, a := outline.At(x, y).RGBA()
			switch {
			case a == 0:
				clear++
			case a > 0xc000 && (r < 0xf000 || g < 0xf000 || b < 0xf000):
				t.Fatalf("outline pixel %d,%d is not white", x, y)
			default:
				white++
			}
		}
	}
	if clear == 0 || white == 0 {
		t.Errorf("outline icon has %d transparent and %d white pixels; it needs both", clear, white)
	}
}

func pngInZip(t *testing.T, zipped []byte, name string) image.Image {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer rc.Close()
			img, err := png.Decode(rc)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			return img
		}
	}
	t.Fatalf("%s is not at the root of the package", name)
	return nil
}
