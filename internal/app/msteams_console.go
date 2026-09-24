package app

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The console's half of connecting Microsoft Teams: a code that joins a tenant to this
// organisation, and the app package a tenant admin uploads. Both are the connections permission —
// connecting a workspace is what lets the bot spend this organisation's credentials there, which is
// why the Slack install is gated on it too.

func (b *Bot) msteamsConsoleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/workspaces/msteams/code", b.requirePerm(PermConnManage, b.handleMSTeamsCode))
	mux.HandleFunc("GET /api/workspaces/msteams/package", b.requirePerm(PermConnManage, b.handleMSTeamsPackage))
}

// msteamsNotConfigured is what the console says on a deployment with no Teams app registration.
const msteamsNotConfigured = "This deployment has no Microsoft Teams app registration: set MSTEAMS_APP_ID, " +
	"MSTEAMS_APP_PASSWORD and MSTEAMS_TENANT_ID. guide/msteams.md walks through creating one."

// handleMSTeamsCode mints a link code. It is shown once, to the person who asked, and only its hash
// is kept; whoever sends it to the bot joins the tenant they send it from to this organisation.
func (b *Bot) handleMSTeamsCode(w http.ResponseWriter, r *http.Request) {
	if b.slacks.msteams == nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": msteamsNotConfigured})
		return
	}
	u := adminFromCtx(r.Context())
	code, expires, err := b.store.NewLinkCode(r.Context(), u.OrgID, platformMSTeams, u.ID)
	if err != nil {
		slog.Error("mint a Teams link code", "org", u.OrgID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not make a link code"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code": code, "command": "link " + code, "expires_at": expires.UTC().Format(time.RFC3339),
		// What the app is called in Teams, for the line the dialog shows for a channel, where the
		// command needs the bot mentioned in front of it.
		"bot_name": msteamsAppName,
		// A chat with the bot with the command already typed, so the person has only to press Send.
		"chat_url": msteamsChatLink(b.slacks.msteams.appID, "link "+code),
	})
}

// msteamsChatLink is a Teams deep link to a one-to-one chat with the bot, with text waiting in the
// compose box. Teams never sends it by itself; the person presses Send. It opens in whichever Teams
// they use, the web client or the app. The bot is addressed by its "28:" id, the app registration's,
// which unlike the id a tenant's catalogue gives the app is the same in every organisation.
func msteamsChatLink(appID, text string) string {
	link := "https://teams.microsoft.com/l/chat/0/0?users=28:" + url.QueryEscape(appID)
	if text != "" {
		// %20 rather than +: the message is read as a URL component, not a form field.
		link += "&message=" + strings.ReplaceAll(url.QueryEscape(text), "+", "%20")
	}
	return link
}

// handleMSTeamsPackage serves the app package for this deployment's bot.
func (b *Bot) handleMSTeamsPackage(w http.ResponseWriter, r *http.Request) {
	if b.slacks.msteams == nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": msteamsNotConfigured})
		return
	}
	zip, err := b.slacks.msteams.msteamsPackage(b.baseURL(r))
	if err != nil {
		slog.Error("build the Teams app package", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not build the Teams app package"})
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="attest_tag-teams.zip"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(zip)
}
