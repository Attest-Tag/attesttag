package app

import (
	"archive/zip"
	"bytes"
	"embed"
	"encoding/json"
	"net/url"
	"strings"
)

// The Teams app package: a zip of manifest.json and two icons, which a tenant admin uploads in the
// Teams admin centre. It is generated per deployment rather than checked in, because the manifest
// names this deployment's own bot and its own host — a self-hosted attest_tag has its own app
// registration, and a package pointing at somebody else's would install a bot that answers there.

//go:embed msteams_icons/color.png msteams_icons/outline.png
var msteamsIcons embed.FS

// msteamsAppVersion is the package's version. Teams installs an uploaded package over an older one
// only when this goes up, so it moves whenever the manifest does.
const msteamsAppVersion = "1.0.1"

// msteamsAppName is what the app is called in Teams, and so what people type after the @. It is
// the name the bot goes by in Slack, so a team on both mentions it the same way.
const msteamsAppName = "attestTag"

// msteamsRSC are the resource-specific permissions the app asks for, granted by the team owner or
// the chat's members when they install it — not by a tenant admin. The first lets the bot receive
// every message in a team's channels rather than only the ones that mention it, which is what
// makes its conversation log whole; the second is the same for a group chat.
//
// TestTeamsManifestAsksForThePermissionsTheCodeUses pins this list to the manifest.
var msteamsRSC = []string{"ChannelMessage.Read.Group", "ChatMessage.Read.Chat"}

// msteamsManifest is the manifest for this deployment's bot, served from origin.
func (c *msteamsClient) msteamsManifest(origin string) map[string]any {
	host := ""
	if u, err := url.Parse(origin); err == nil {
		host = u.Hostname()
	}
	rsc := make([]map[string]string, 0, len(msteamsRSC))
	for _, name := range msteamsRSC {
		rsc = append(rsc, map[string]string{"name": name, "type": "Application"})
	}
	scopes := []string{"personal", "team", "groupChat"}
	// The developer/privacy/terms links point at this deployment's own site (SITE_URL), or its
	// console origin when none is set — never a hard-coded attesttag.com in a self-host's package.
	site := c.siteURL
	if site == "" {
		site = strings.TrimRight(origin, "/")
	}
	m := map[string]any{
		"$schema":         "https://developer.microsoft.com/json-schemas/teams/v1.25/MicrosoftTeams.schema.json",
		"manifestVersion": "1.25",
		"version":         msteamsAppVersion,
		// The Teams app and the Entra app are one to one — Microsoft refuses an install that shares
		// an Entra app between two Teams apps — so the app registration's id is the app's id too.
		"id": c.appID,
		"developer": map[string]string{
			"name":          "attest_tag",
			"websiteUrl":    site,
			"privacyUrl":    site + "/privacy",
			"termsOfUseUrl": site + "/terms",
		},
		"name": map[string]string{"short": msteamsAppName, "full": msteamsAppName + ", an AI teammate"},
		"description": map[string]string{
			"short": "An AI teammate that answers in your channels and chats.",
			"full": "attest_tag answers questions in your channels and chats from your documents and the tools your " +
				"organisation connects it to. Anything that changes something waits for a person to confirm it, and " +
				"access it has not been given is asked for from a named approver.",
		},
		"icons":       map[string]string{"color": "color.png", "outline": "outline.png"},
		"accentColor": "#5A50C8",
		"bots": []map[string]any{{
			"botId":              c.appID,
			"scopes":             scopes,
			"supportsFiles":      true, // or Teams offers no way to send the bot a file (msteams_files.go)
			"isNotificationOnly": false,
			"commandLists": []map[string]any{{
				"scopes": scopes,
				"commands": []map[string]string{
					{"title": "link", "description": "Connect this organisation to attest_tag with a code from the console"},
				},
			}},
		}},
		"permissions": []string{"identity", "messageTeamMembers"},
		// Required for resource-specific consent. resource does nothing for RSC, in Microsoft's own
		// words, but an install without one fails.
		"webApplicationInfo": map[string]string{"id": c.appID, "resource": "https://RscBasedStoreApp"},
		"authorization":      map[string]any{"permissions": map[string]any{"resourceSpecific": rsc}},
		// Teams refuses a manifest of 1.25 or later with the team scope and without this: it says
		// the bot is ready for private and shared channels as well as standard ones. It is, for
		// the reasons it is ready for any channel — every channel reads as private here, who is in
		// one is asked of the roster, and a person from another tenant is refused (mayUseBot).
		"supportsChannelFeatures": "tier1",
	}
	if host != "" {
		m["validDomains"] = []string{host}
	}
	return m
}

// msteamsPackage zips the manifest and the icons, all at the root, which is where Teams looks.
func (c *msteamsClient) msteamsPackage(origin string) ([]byte, error) {
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	manifest, err := json.MarshalIndent(c.msteamsManifest(origin), "", "  ")
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{"manifest.json": manifest}
	for _, name := range []string{"color.png", "outline.png"} {
		b, err := msteamsIcons.ReadFile("msteams_icons/" + name)
		if err != nil {
			return nil, err
		}
		files[name] = b
	}
	for _, name := range []string{"manifest.json", "color.png", "outline.png"} {
		w, err := z.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(files[name]); err != nil {
			return nil, err
		}
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
