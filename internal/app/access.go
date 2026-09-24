package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
)

// Who the bot answers, in Slack and in Teams.
//
// The console has no domain gate: anyone may sign up and found an organisation, and what they
// may then do is bounded by verification, throttles and caps (limits.go). This is the chat
// half: ALLOWED_EMAIL_DOMAINS says whose messages the bot acts on at all. A workspace holds more
// than employees — single- and multi-channel guests, Slack Connect members from other
// companies, shared channels — and every one of them could otherwise mention the bot and
// reach whatever the channel's connections reach. Empty means any member of the workspace. The
// guests and the other companies are a separate gate, allow_external_users below, and are
// refused whether or not there is a list.

func allowedEmailDomains() []string { return parseEmailDomains(os.Getenv("ALLOWED_EMAIL_DOMAINS")) }

// mayUseBot reports whether a Slack user may talk to the bot, and what to tell them when not.
// It is strict on purpose: an account Slack will not describe is refused, because "we could
// not check" must not resolve to "allowed".
//
// Two gates, in order. Membership first: a guest, or somebody on the other side of a Slack
// Connect channel, is in the room but not in this organisation, and is refused unless the
// organisation has said otherwise (allow_external_users). Then the organisation's own
// email-domain list, if it keeps one. Both are read from that organisation's settings, not
// the deployment's: the workspace belongs to one tenant and who may reach its connections is
// that tenant's decision; ALLOWED_EMAIL_DOMAINS survives only as the default for a deployment
// that already sets it.
func (b *Bot) mayUseBot(ctx context.Context, sl *Chat, userID string) (bool, string) {
	if userID == "" {
		return false, "I can't tell which account this is from, so I've stopped here."
	}
	// Our own SELF_TEST messages: the bot user itself, or the synthetic "selftest" requester the
	// event handler hands them in as (a real Slack id never looks like that). Nobody else can
	// reach this path — it is only taken for the bot's own messages when SELF_TEST=1.
	if userID == sl.BotUserID || (b.cfg.SelfTest && userID == "selftest") {
		return true, ""
	}
	// A mail forwarded to a channel's Slack address (email_intake.go). The gates below are about
	// a Slack account and there is none here to read: Slackbot posted it, and the person who
	// wrote it is outside the workspace by definition. What stands in their place is the
	// channel's own opt-in, which has already been checked, and a turn that carries none of a
	// member's privileges — see emailRequester.
	if isEmailRequester(userID) {
		return true, ""
	}
	st := b.settings.Get(ctx, orgOfChat(sl))
	// One users.info per person per userInfoTTL, shared with everything else that asks about
	// them. Its team id and guest flags are what tell a member from a visitor; its email is what
	// the domain list reads.
	uf, err := sl.user(ctx, userID, false)
	if err != nil {
		slog.Warn("could not read the asker's account", "user", userID, "err", err)
		return false, "I couldn't check your account with " + platformName(sl) + " just now, so I've stopped here. Try again in a moment."
	}
	if uf.Deleted {
		return false, "That account is deactivated."
	}
	if !st.AllowExternalUsers {
		const who = " Someone who manages this organisation's console can change that under Settings → Security."
		switch {
		case uf.TeamID != "" && sl.TeamID != "" && uf.TeamID != sl.TeamID:
			slog.Info("bot use refused: another workspace", "user", userID, "team", uf.TeamID, "installed", sl.TeamID)
			if sl.Platform == platformMSTeams {
				return false, "I only answer members of this organisation, not guests from other Microsoft 365 organisations." + who
			}
			return false, "I only answer members of this Slack workspace, not accounts joining through Slack Connect." + who
		case uf.Restricted:
			slog.Info("bot use refused: guest account", "user", userID)
			return false, "I don't answer guest accounts here." + who
		}
	}
	domains := st.AllowedEmailDomains
	if len(domains) == 0 {
		return true, ""
	}
	only := fmt.Sprintf("I only work with %s accounts.", strings.Join(domains, " and "))
	at := strings.LastIndex(uf.Email, "@")
	if at < 0 && sl.Platform == platformMSTeams {
		// The roster gives an address for every member it describes, so this is an account with
		// none — and the domain list cannot pass somebody it cannot read.
		return false, only + " I can't see an email address on your Microsoft account."
	}
	if at < 0 {
		// Almost always a missing scope rather than an account without an address: users.info
		// omits profile.email entirely unless the bot token holds users:read.email. Say so, or
		// this reads as "you have no email" to someone who plainly does.
		slog.Error("cannot read emails from Slack: add the users:read.email bot scope to the Slack app and reinstall it, "+
			"or clear the organisation's allowed email domains. Until then nobody can use the bot", "user", userID)
		return false, only + " I can't see the email on your Slack account though, which usually means the Slack app is missing the `users:read.email` permission — an admin needs to add that scope and reinstall it."
	}
	if !slices.Contains(domains, strings.ToLower(uf.Email[at+1:])) {
		slog.Info("bot use refused: email domain", "user", userID, "domain", uf.Email[at+1:])
		return false, only + " Someone who manages this organisation's console can add a domain under Settings → Security."
	}
	return true, ""
}

// describeBotAccess is the startup line: the deployment-wide default, which each organisation
// may now override in its own settings.
func describeBotAccess() string {
	if d := allowedEmailDomains(); len(d) > 0 {
		return strings.Join(d, ",") + " by default (each organisation may set its own)"
	}
	return "any member of a connected workspace, and guests or other organisations' accounts only where an organisation allows them (ALLOWED_EMAIL_DOMAINS unset; each organisation may set its own)"
}

// checkEmailScope reports installs that cannot satisfy ALLOWED_EMAIL_DOMAINS. The gate reads
// profile.email from users.info, and that field is simply absent unless the bot token holds
// users:read.email — in which case every single person in that workspace is refused. Granted
// scopes are per install and can differ between workspaces and between reinstalls, so this is
// a loop over what each one actually granted rather than one process-wide answer.
func (b *Bot) checkEmailScope(ctx context.Context) {
	teams, _ := b.store.ActiveTeams(ctx)
	for _, t := range teams {
		if t.EmailScope {
			continue
		}
		// Per organisation, because the list is now per organisation: a workspace missing the
		// scope only matters where somebody is actually gating on email.
		if len(b.settings.Get(ctx, t.OrgID).AllowedEmailDomains) == 0 {
			continue
		}
		slog.Error("this organisation gates the bot on email domains but the workspace's install is missing the users:read.email bot scope, "+
			"so nobody there can use the bot: every account looks like it has no email address. "+
			"Reconnect the workspace from /admin/workspaces to grant it", "team", t.TeamID, "name", t.Name)
	}
}
