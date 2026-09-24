package app

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Connections people sign into for themselves.
//
// Every other connection is one credential an admin pastes and a whole channel spends. That is
// the wrong shape for a mailbox or a calendar: the answer to "what is on my calendar" depends on
// who asked, and one person must never read another's mail through a shared token. So a
// connection of cred_type oauth_user holds only the organisation's OAuth client — a client id and
// secret, which reach nothing on their own — and the credential that actually spends is one row
// per person in user_connections, sealed against their Slack identity.
//
// The link the bot sends is a bearer capability, minted for one person and one connection under a
// key derived for this purpose alone, in the same shape as a Configure link (configure.go). It is
// never shown to the model: a tool result travels to whichever provider serves the model, and a
// link that grants a Google sign-in has no business going there.

const connectLinkTTL = 30 * time.Minute

// connectMAC covers the workspace, the person and the connection together. A token minted for
// one person must not open a sign-in that writes tokens against another, and a Slack user id is
// only unique inside its workspace.
func connectMAC(orgID, connID int64, teamID, slackUserID, body string) string {
	key := derivedKey("connect/v1")
	if key == nil {
		return "" // no usable master key: refuse, never fall back to an empty one
	}
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "connect:%d:%d:%s:%s:%s", orgID, connID, teamID, slackUserID, body)
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// Token shape: v1.<org>.<conn>.<team>.<user>.<expiry unix>.<nonce>.<mac>. The identity travels in
// the token rather than in the URL path so that the page has nothing to trust but the MAC.
func mintConnectToken(orgID, connID int64, teamID, slackUserID string, now time.Time) string {
	nonce := make([]byte, 8)
	rand.Read(nonce)
	body := fmt.Sprintf("v1.%d.%d.%s.%s.%d.%s", orgID, connID, teamID, slackUserID,
		now.Add(connectLinkTTL).Unix(), hex.EncodeToString(nonce))
	mac := connectMAC(orgID, connID, teamID, slackUserID, body)
	if mac == "" {
		return ""
	}
	return body + "." + mac
}

type connectClaim struct {
	OrgID       int64
	ConnID      int64
	TeamID      string
	SlackUserID string
}

func verifyConnectToken(tok string, now time.Time) (connectClaim, bool) {
	var cl connectClaim
	i := strings.LastIndex(tok, ".")
	if i < 0 {
		return cl, false
	}
	body, sig := tok[:i], tok[i+1:]
	parts := strings.Split(body, ".")
	if len(parts) != 7 || parts[0] != "v1" {
		return cl, false
	}
	orgID, err1 := strconv.ParseInt(parts[1], 10, 64)
	connID, err2 := strconv.ParseInt(parts[2], 10, 64)
	exp, err3 := strconv.ParseInt(parts[5], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return cl, false
	}
	cl = connectClaim{OrgID: orgID, ConnID: connID, TeamID: parts[3], SlackUserID: parts[4]}
	want := connectMAC(orgID, connID, cl.TeamID, cl.SlackUserID, body)
	if want == "" || !hmac.Equal([]byte(sig), []byte(want)) {
		return connectClaim{}, false
	}
	if now.Unix() > exp {
		return connectClaim{}, false
	}
	return cl, true
}

// connectLinkURL is the link the bot sends someone so they can grant their own account. Empty
// when the deployment has no public origin to come back to, which is the honest answer: an
// OAuth round trip needs a URL the provider can redirect a browser to.
func (b *Bot) connectLinkURL(ctx context.Context, orgID, connID int64, teamID, slackUserID string) string {
	base := publicBaseURL(ctx, b.store, b.cfg)
	if base == "" || slackUserID == "" {
		return ""
	}
	tok := mintConnectToken(orgID, connID, teamID, slackUserID, time.Now())
	if tok == "" {
		return ""
	}
	return base + "/connect/" + tok
}

// connectRedirectURI is what the admin registers with the provider, so it has to be the same
// string every time: derived from the deployment's public origin, never from the request that
// happens to be in hand. A provider matches it character for character.
func (b *Bot) connectRedirectURI(ctx context.Context) string {
	base := publicBaseURL(ctx, b.store, b.cfg)
	if base == "" {
		return ""
	}
	return base + "/connect/callback"
}

// ---- the credential itself ----

// userToken returns a valid access token for the person named in the audit, refreshing and
// re-sealing when it is within two minutes of expiry. It is the oauth_user twin of
// freshMCPToken, and the one difference is the one that matters: the row it reads is keyed by
// the requester, so the answer is different for each person in the channel.
func (p *Proxy) userToken(ctx context.Context, orgID int64, conn *Connection, teamID, slackUserID string) (string, error) {
	if slackUserID == "" {
		// No requester means nobody, not everybody: a path that lost track of who it acts for
		// must not reach into the first token it can find.
		return "", ErrNeedsUserAuth
	}
	uc, err := p.store.UserConnection(ctx, orgID, conn.ID, teamID, slackUserID)
	if err != nil {
		return "", err
	}
	if uc == nil || len(uc.secretEnc) == 0 {
		return "", ErrNeedsUserAuth
	}
	st, err := p.openUserSecret(uc)
	if err != nil {
		return "", err
	}
	if st.AccessToken != "" && (st.ExpiresAt == 0 || time.Until(time.Unix(st.ExpiresAt, 0)) > 2*time.Minute) {
		p.store.TouchUserConnection(ctx, orgID, uc.ID)
		return st.AccessToken, nil
	}
	if st.RefreshToken == "" {
		// Google drops refresh tokens for an app still in Testing after seven days. Nothing to
		// do but ask again, so say so as an unconnected account rather than as an error.
		return "", ErrNeedsUserAuth
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {st.RefreshToken}, "client_id": {st.ClientID}}
	if err := tokenRequest(ctx, st, form); err != nil {
		return "", err
	}
	if enc, err := p.sealUserSecret(st); err == nil {
		p.store.UpdateUserSecret(ctx, orgID, uc.ID, enc)
	}
	p.store.TouchUserConnection(ctx, orgID, uc.ID)
	return st.AccessToken, nil
}

func (p *Proxy) openUserSecret(uc *UserConnection) (*OAuthState, error) {
	plain, err := p.sealer.Open(uc.secretEnc)
	if err != nil {
		return nil, err
	}
	var st OAuthState
	if err := json.Unmarshal(plain, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (p *Proxy) sealUserSecret(st *OAuthState) ([]byte, error) {
	raw, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	return p.sealer.Seal(raw)
}

// clientTemplate reads the organisation's OAuth client off the connection. What comes back has
// no tokens in it and is not a credential: it is the shape a person's sign-in is made from.
func (b *Bot) clientTemplate(conn *Connection) (*OAuthState, error) {
	sec, err := b.proxy.secret(conn)
	if err != nil {
		return nil, err
	}
	st := &OAuthState{}
	if sec.OAuth != nil {
		*st = *sec.OAuth
	}
	if st.ClientID == "" {
		st.ClientID, st.ClientSecret = sec.ClientID, sec.ClientSecret
	}
	if st.TokenURL == "" {
		st.TokenURL = sec.TokenURL
	}
	if st.Scopes == "" {
		st.Scopes = sec.Scopes
	}
	if pr := presetByID(conn.Preset); pr != nil {
		if st.AuthURL == "" {
			st.AuthURL = pr.AuthURL
		}
		if st.TokenURL == "" {
			st.TokenURL = pr.TokenURL
		}
		if st.Scopes == "" {
			st.Scopes = pr.Scopes
		}
	}
	st.AccessToken, st.RefreshToken, st.ExpiresAt = "", "", 0
	if st.ClientID == "" || st.AuthURL == "" || st.TokenURL == "" {
		return nil, errors.New("this connection has no OAuth client yet; an admin sets it in the console")
	}
	for _, raw := range []string{st.AuthURL, st.TokenURL} {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, err
		}
		if err := publicURL(u); err != nil {
			return nil, err
		}
	}
	return st, nil
}

// ---- asking somebody to connect ----

// actConnectOpen labels the link button so a press lands somewhere on purpose. Slack sends a
// block_actions payload even for a button that only opens a URL, and an action id nobody knows
// falls through the interaction switch and is logged as unhandled.
const actConnectOpen = "attest_connect_open"

// askToConnect sends the person a link and tells the model what happened. The link is a bearer
// capability: whoever holds it can attach a Google account to this person's name, so it goes to
// them in Slack and never into the tool result. A tool result is prompt text, and prompt text
// travels to whoever serves the model.
func (a *Agent) askToConnect(ctx context.Context, c *Call, conn *Connection) string {
	if conn == nil {
		return "That service is connected per person and this one has not been connected. Say so; do not retry."
	}
	name := conn.Name
	if c.SL == nil || c.UserID == "" {
		return fmt.Sprintf("%s works per person and this request has nobody to attribute it to, so there is no account to use. Say that plainly.", name)
	}
	link := a.connectURL(ctx, c.OrgID, conn.ID, c.TeamID, c.UserID)
	if link == "" {
		return fmt.Sprintf("%s needs each person to connect their own account, but this deployment has no public address for the sign-in page yet. "+
			"Tell them an admin has to open the console over its public URL once, then they can connect.", name)
	}
	card := Card{
		Parts: []CardPart{{Markdown: fmt.Sprintf("*Connect %s*\nI need your own account before I can answer that — nobody else's will do. "+
			"The link is yours alone and lasts 30 minutes.%s", escapeMrkdwn(name), examplesBlock(conn))}},
		Buttons: []Button{{ActionID: actConnectOpen, Value: "connect", Label: "Connect", Primary: true, URL: link}},
		Footer:  "You can disconnect from the same page whenever you like.",
	}
	if _, _, err := c.SL.PostCard(ctx, c.UserID, "", card, "Connect your account to carry on."); err != nil {
		return fmt.Sprintf("%s needs their own account connected, and I could not send them the link (%s). "+
			"Ask them to run `!connect` in the channel. Do not invent a link.", name, truncate(err.Error(), 120))
	}
	return fmt.Sprintf("Nothing was sent to %s. That connection uses each person's own account and this one has not connected theirs yet, "+
		"so I have just sent them a private message with a Connect button. Tell them to check their direct messages from me and to ask again "+
		"once they are connected. Do not offer a link of your own: you do not have one.", name)
}

// connectionParts is which of a preset's parts an admin actually connected, in words.
// "Google Workspace" is three different things depending on what was ticked, and somebody
// deciding whether to sign in should not have to guess which of them they are being asked for.
func connectionParts(conn *Connection) string {
	opts := connectionOptions(presetByID(conn.Preset), conn)
	if len(opts) == 0 {
		return ""
	}
	labels := make([]string, 0, len(opts))
	for _, o := range opts {
		labels = append(labels, o.Label)
	}
	return " (" + strings.Join(labels, ", ") + ")"
}

// personalSetupRe matches an ask that only somebody's own account can answer. An organisation
// that has never set one of these up pays for the setup steps on the turn that needs them and
// on no other: "what's in my inbox" is the question they answer, and it is not every question.
var personalSetupRe = regexp.MustCompile(`(?i)\b(e-?mails?|inbox|mailbox|gmail|calendars?|contacts?|meetings?)\b`)

// wantsPersonalSetup is an organisation with nothing of the kind anywhere, asked for one of the
// things only a personal account can answer. There is nothing to connect and nothing to attach:
// the only true answer is what an admin would have to do first, so it is the one the turn gets.
func (a *Agent) wantsPersonalSetup(ctx context.Context, c *Call) bool {
	if a.store == nil || c.Access == nil || c.Access.HasPersonal() || len(a.personalElsewhere(ctx, c)) > 0 {
		return false
	}
	return personalSetupRe.MatchString(c.Text)
}

// setupSteps is what an admin does once, in order, with the pages it happens on. It is written
// here rather than left to the model for the reason every other link in this file is: a console
// URL invented from memory sends somebody to a page that does not exist, and a person who has
// been told the wrong first step does not take the second.
func (a *Agent) setupSteps(ctx context.Context) string {
	base := publicBaseURL(ctx, a.store, a.cfg)
	page := func(path, label string) string {
		if base == "" {
			return "*" + label + "*"
		}
		return "<" + base + path + "|" + label + ">"
	}
	callback := "your console's /connect/callback"
	if base != "" {
		callback = base + "/connect/callback"
	}
	return "*Mail, calendar, Drive files and contacts run on each person's own account, and this organisation has not set that up yet.* " +
		"An admin does it once, and then everyone here connects themselves:\n" +
		"1. In Google Cloud → APIs and Services → Credentials, create an *OAuth client ID* of type Web application with the redirect URI `" + callback + "`. " +
		"Enable the Gmail, Calendar, Drive and People APIs on that project, and set the consent screen to *Internal* — an External app using Gmail scopes needs Google's verification first.\n" +
		"2. In the console, open " + page("/bundles", "Access bundles") + ", add a connection and choose the *Google Workspace* preset.\n" +
		"3. Paste the client id and secret into it, tick the parts to allow — Gmail, Calendar, Drive, Contacts, with reading and writing as separate boxes; Drive starts unticked, because it reaches every file the person can open — and save.\n" +
		"4. Open " + page("/workspaces", "Workspaces") + " and attach that bundle to the workspace, or to this channel alone if it should not reach everywhere.\n" +
		"5. Back here, everyone says `!connect` and signs in with their own Google account. Each person reaches only their own mail and calendar, and nobody — including an admin — reaches anyone else's."
}

// postSetupCard puts those steps in the thread rather than in a direct message. There is no
// capability in them, and whoever asked is not usually the person who can act on them: a card
// they can point an admin at is worth more than one only they can read.
func (a *Agent) postSetupCard(ctx context.Context, c *Call, steps string) bool {
	if c.SL == nil || c.offline() {
		return false
	}
	card := Card{Parts: []CardPart{{Markdown: steps}}}
	_, _, err := c.SL.PostCard(ctx, c.Channel, c.ThreadTS, card, "Setting up Google Workspace")
	return err == nil
}

// connectAccountTool is `!connect` reached by language. "Connect my Gmail" is a request to
// connect, not a question about connecting, and the answer to it is the card the command sends
// rather than a paragraph about what somebody else would have to do first. Withheld where there
// is nobody to send a direct message to, and where nothing here runs on a personal account.
func (a *Agent) connectAccountTool() Tool {
	return Tool{
		Name: "connect_account",
		Desc: "Send this person a private link to connect, switch or disconnect their own account for a service that runs per person (mail, calendar, contacts). " +
			"Call it when they ask to connect one, and when answering needs an account they have not connected yet. The link goes to them in Slack; you never see one. " +
			"Where the organisation has no such service set up at all, it posts the steps an admin follows instead.",
		Params: schema(map[string]any{
			"connection": str("Which connection, by name. Only needed when several here run on people's own accounts; omit for all of them"),
		}),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			var p struct{ Connection string }
			json.Unmarshal(args, &p)
			return a.connectReply(ctx, c, p.Connection, true), nil
		},
	}
}

// connectCommand answers `!connect`: which services in this channel run on your own account,
// and where you stand with each. The links go to the person by DM, never into the channel — a
// connect link is a capability, and a channel is not a private place.
func (a *Agent) connectCommand(ctx context.Context, c *Call) string {
	return a.connectReply(ctx, c, "", false)
}

// connectReply is that answer, written once for the two ways of asking for it. only names a
// single connection or is empty for all of them; forModel swaps the lines addressed to the
// person for lines addressed to the model, which is holding a tool result and not a reply.
func (a *Agent) connectReply(ctx context.Context, c *Call, only string, forModel bool) string {
	// A turn resolves the channel's access before any tool runs, but `!connect` is a bang command
	// and never becomes a turn, so without this it saw no connections at all and answered every
	// person with the refusal below. The tool path arrives with Access already set; this is a no-op
	// there.
	a.ensureAccess(ctx, c)
	if c.Access == nil {
		return "I can't see this channel's connections right now."
	}
	only = strings.TrimSpace(only)
	var personal, byOtherName []*Connection
	for _, r := range c.Access.Rules {
		if r.Conn == nil || r.Conn.CredType != "oauth_user" {
			continue
		}
		if only != "" && !strings.EqualFold(only, r.Conn.Name) {
			byOtherName = append(byOtherName, r.Conn)
			continue
		}
		personal = append(personal, r.Conn)
	}
	// Asked in a room the connection has not been added to. The link is still theirs to press —
	// it connects their own account, not this channel — so it goes, and the closing line carries
	// the half that is still missing rather than the whole thing becoming a refusal.
	offChannel := false
	if len(personal) == 0 {
		for _, conn := range a.personalElsewhere(ctx, c) {
			if only == "" || strings.EqualFold(only, conn.Name) {
				personal = append(personal, conn)
			}
		}
		offChannel = len(personal) > 0
	}
	if len(personal) == 0 {
		if len(byOtherName) > 0 {
			names := make([]string, 0, len(byOtherName))
			for _, conn := range byOtherName {
				names = append(names, conn.Name)
			}
			msg := "Nothing here called " + only + " runs on your own account. These do: " + strings.Join(names, ", ") + "."
			if forModel {
				msg += " Nothing was sent. Ask which they meant; do not write a link yourself."
			}
			return msg
		}
		steps := a.setupSteps(ctx)
		if !forModel {
			return "Nothing here runs on your own account yet.\n\n" + steps
		}
		if a.postSetupCard(ctx, c, steps) {
			return "Nothing was sent to them, because there is nothing yet to connect: this organisation has no connection that runs on people's own accounts. " +
				"I have posted the steps an admin follows, in this thread. Tell them in a line that reading their own mail or calendar needs an admin to set Google Workspace up once, " +
				"that the steps are in the thread, and that they can pass them on. Do not repeat the steps and do not invent a console link."
		}
		return "There is nothing for them to connect: this organisation has no connection that runs on people's own accounts, and an admin sets one up in the console first. " +
			"Say that much; do not invent steps or links."
	}
	var b strings.Builder
	if !forModel {
		b.WriteString("These run on *your own* account, not a shared one — I only ever act as whoever asked, and nobody here can read your mail or calendar through me.\n\n")
	}
	sent := 0
	for _, conn := range personal {
		uc, _ := a.store.UserConnection(ctx, c.OrgID, conn.ID, c.TeamID, c.UserID)
		parts := escapeMrkdwn(connectionParts(conn))
		switch {
		case uc.Connected() && uc.Account != "":
			fmt.Fprintf(&b, "• *%s*%s — connected as %s\n", escapeMrkdwn(conn.Name), parts, escapeMrkdwn(uc.Account))
		case uc.Connected():
			fmt.Fprintf(&b, "• *%s*%s — connected\n", escapeMrkdwn(conn.Name), parts)
		default:
			fmt.Fprintf(&b, "• *%s*%s — not connected yet\n", escapeMrkdwn(conn.Name), parts)
		}
		// What it is for, under what it is called. Somebody reading `!connect` for the first
		// time is deciding whether to hand over their mailbox, and the name of a connection has
		// never been an argument either way.
		if ex := connectExamples(conn); len(ex) > 0 {
			for _, e := range ex {
				fmt.Fprintf(&b, "    ◦ _%s_\n", escapeMrkdwn(e))
			}
		}
		if link := a.connectURL(ctx, c.OrgID, conn.ID, c.TeamID, c.UserID); link != "" && c.SL != nil {
			card := Card{
				Parts:   []CardPart{{Markdown: "*" + escapeMrkdwn(conn.Name) + "*\nConnect or disconnect your own account. The link lasts 30 minutes." + examplesBlock(conn)}},
				Buttons: []Button{{ActionID: actConnectOpen, Value: "connect", Label: "Open", Primary: true, URL: link}},
			}
			if _, _, err := c.SL.PostCard(ctx, c.UserID, "", card, "Connect your account."); err == nil {
				sent++
			}
		}
	}
	switch {
	case sent > 0 && forModel:
		fmt.Fprintf(&b, "\nA Connect button has gone to <@%s> privately, one for each service above. Tell them to check their direct "+
			"messages from you and to ask again once they are connected. Do not offer a link of your own: you do not have one.", c.UserID)
	case sent > 0:
		b.WriteString("\nI've sent you the links in a direct message — they're yours alone, so they don't go in the channel.")
	case forModel:
		b.WriteString("\nThe direct message did not go through, so they have no link. Say so; do not write one yourself.")
	default:
		b.WriteString("\nI couldn't send you the links by direct message. Check that you can receive DMs from me.")
	}
	if sent > 0 && offChannel {
		if forModel {
			b.WriteString(" This channel has not been given that connection, so connecting does not by itself make it usable here — an admin adds it to this channel as well. Say both halves, and promise nothing here until it is added.")
		} else {
			b.WriteString(" This channel hasn't been given that connection yet, so once you're connected an admin still has to add it here before I can use it in this channel.")
		}
	}
	return strings.TrimSpace(b.String())
}

// examplesBlock is the "once this is connected you can ask me" tail of a Connect card, or
// nothing at all for a connection whose preset does not say what it is for. Capped at three:
// the card is a decision, not documentation, and `!connect` carries the full list.
func examplesBlock(conn *Connection) string {
	ex := connectExamples(conn)
	if len(ex) == 0 {
		return ""
	}
	if len(ex) > 3 {
		ex = ex[:3]
	}
	var b strings.Builder
	b.WriteString("\n\nOnce it is, you can ask me things like:")
	for _, e := range ex {
		b.WriteString("\n• _" + escapeMrkdwn(e) + "_")
	}
	return b.String()
}

// connectURL is connectLinkURL reached from the agent, which holds the store and config the
// same way the Bot does.
func (a *Agent) connectURL(ctx context.Context, orgID, connID int64, teamID, slackUserID string) string {
	base := publicBaseURL(ctx, a.store, a.cfg)
	if base == "" || slackUserID == "" {
		return ""
	}
	tok := mintConnectToken(orgID, connID, teamID, slackUserID, time.Now())
	if tok == "" {
		return ""
	}
	return base + "/connect/" + tok
}

// ---- the pages ----

const connectCSRFCookie = "attest_connect_csrf"

// connectGrant is one box: reading a part, or writing to it. They are separate scopes at Google
// and they are separate decisions here — "read my mail and draft replies, but only look at my
// calendar" is an ordinary thing to want, and a single per-part switch cannot say it.
type connectGrant struct {
	Key     string // "<part>:read" or "<part>:write", the form value
	Label   string
	Lines   []string // what it grants, in words
	Checked bool
	scopes  []string
}

// connectPart is one thing a person may hand over, with its boxes.
type connectPart struct {
	ID     string
	Label  string
	Hint   string
	Grants []connectGrant
}

// connectParts is what the sign-in page offers, worked out by intersecting the parts an admin
// connected with the scopes this connection actually asks for. A part the admin left out reaches
// no scopes and is not offered, and neither is writing the admin did not allow — nobody is
// invited to grant something that would be refused anyway. `chosen` is nil on the first render,
// when everything the admin allowed starts ticked.
func connectParts(conn *Connection, asks string, chosen map[string]bool) []connectPart {
	ceiling := map[string]bool{}
	for _, s := range strings.Fields(asks) {
		ceiling[s] = true
	}
	within := func(raw string) []string {
		var out []string
		for _, s := range strings.Fields(raw) {
			if ceiling[s] {
				out = append(out, s)
			}
		}
		return out
	}
	var out []connectPart
	for _, o := range connectionOptions(presetByID(conn.Preset), conn) {
		part := connectPart{ID: o.ID, Label: o.Label, Hint: o.Hint}
		for _, g := range []struct{ kind, label, raw string }{
			{"read", "Read", o.ReadScopes},
			{"write", writeBoxLabel(o), o.WriteScopes},
		} {
			scopes := within(g.raw)
			if len(scopes) == 0 {
				continue
			}
			key, lines := o.ID+":"+g.kind, scopeLines(strings.Join(scopes, " "))
			// A box whose one scope says exactly what the box is called does not need to say it
			// twice — "Write drafts and send mail as you", under "Write drafts and send mail as
			// you", reads as a page that was assembled rather than written.
			if len(lines) == 1 && strings.EqualFold(lines[0], g.label) {
				lines = nil
			}
			part.Grants = append(part.Grants, connectGrant{Key: key, Label: g.label,
				Lines: lines, Checked: chosen == nil || chosen[key], scopes: scopes})
		}
		if len(part.Grants) > 0 {
			out = append(out, part)
		}
	}
	return out
}

// writeBoxLabel turns the preset's phrasing for writing into a box label, since "Write" alone
// does not say whether it means booking a meeting or sending mail as you.
func writeBoxLabel(o PresetOption) string {
	if o.WriteLabel == "" {
		return "Write"
	}
	return strings.ToUpper(o.WriteLabel[:1]) + o.WriteLabel[1:]
}

// scopesForParts is the same intersection read the other way: the scope string to ask the
// provider for, given what this person ticked. Empty when they ticked nothing, which the caller
// treats as a question to put back to them rather than as a reason to ask for everything.
func scopesForParts(conn *Connection, asks string, chosen map[string]bool) string {
	// Anything the parts do not claim stays in — openid and email name the account, which is how
	// a second sign-in is recognised as the same person.
	claimed := map[string]bool{}
	for _, o := range connectionOptions(presetByID(conn.Preset), conn) {
		for _, s := range strings.Fields(o.ReadScopes + " " + o.WriteScopes) {
			claimed[s] = true
		}
	}
	var out []string
	for _, s := range strings.Fields(asks) {
		if !claimed[s] {
			out = appendNew(out, s)
		}
	}
	picked := false
	for _, p := range connectParts(conn, asks, chosen) {
		for _, g := range p.Grants {
			if !g.Checked {
				continue
			}
			picked = true
			out = appendNew(out, g.scopes...)
		}
	}
	if !picked {
		return ""
	}
	return strings.Join(out, " ")
}

// instructionsHint is the placeholder in the notes box: an example drawn from what this
// connection actually covers, since "write your instructions here" tells nobody what to write.
func instructionsHint(conn *Connection) string {
	has := map[string]bool{}
	for _, o := range connectionOptions(presetByID(conn.Preset), conn) {
		has[o.ID] = true
	}
	// Two kinds, deliberately mixed: how the bot should sound as you, and how it should go about
	// the work. The second is the one people do not think to write down, and it is the one that
	// saves a wrong answer — "my latest email" means something different if sent mail counts.
	var lines []string
	if has["gmail"] {
		lines = append(lines, "Only ever look at my inbox — never sent mail, archives or spam.",
			"“My latest email” means the newest one I received, not the newest in the account.",
			"Sign my drafts off with my first name only, never “Best regards”.")
	}
	if has["calendar"] {
		lines = append(lines, "Never book me before 9am or on Fridays.",
			"My work calendar is the only one that counts — ignore the others.",
			"Put a 15 minute gap either side of anything external.")
	}
	if has["contacts"] {
		lines = append(lines, "When two people share a name, ask me rather than guessing.")
	}
	if len(lines) == 0 {
		return "How the bot should go about things when it acts as you, and how it should sound."
	}
	return strings.Join(lines, "\n")
}

// setConnectCSP replaces the blanket page policy from secureHeaders with one that lets this page
// hand the browser on to the provider. Everything else about the policy stays as strict as the
// default: no scripts at all, nothing framed, and the only two places a form may go are back
// here and to the provider named in the connection.
func setConnectCSP(w http.ResponseWriter, authURL string) {
	u, err := url.Parse(authURL)
	if err != nil || u.Host == "" {
		return // leave the strict default in place rather than widening it to something unparsed
	}
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; img-src 'self' data:; "+
		"frame-ancestors 'none'; object-src 'none'; base-uri 'self'; form-action 'self' "+u.Scheme+"://"+u.Host)
}

func (b *Bot) connectRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /connect/callback", b.handleConnectCallback)
	mux.HandleFunc("GET /connect/{token}", b.handleConnectPage)
	mux.HandleFunc("POST /connect/{token}", b.handleConnectPage)
}

// handleConnectPage shows the person what they are about to grant, and starts the round trip
// when they press the button. No session: the link is the authority, exactly as it is for the
// Configure page, so the POST needs a CSRF cookie of its own — with only the URL, any site the
// reader visited could submit the form for them.
func (b *Bot) handleConnectPage(w http.ResponseWriter, r *http.Request) {
	cl, ok := verifyConnectToken(r.PathValue("token"), time.Now())
	if !ok {
		http.Error(w, "This link has expired or is not valid. Ask the bot again and it will send a fresh one.", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	conn, err := b.store.Connection(ctx, cl.OrgID, cl.ConnID)
	if err != nil || conn == nil || conn.CredType != "oauth_user" {
		http.Error(w, "That connection no longer exists.", http.StatusNotFound)
		return
	}
	st, terr := b.clientTemplate(conn)
	if terr != nil {
		http.Error(w, terr.Error(), http.StatusBadRequest)
		return
	}
	// Both the page and the POST it makes need a policy that names the provider. form-action is
	// enforced against the *navigation*, redirects included, and against the policy of the
	// document the form lives in — so a page served under the default `form-action 'self'` has
	// its Continue button submit fine and then silently swallows the 302 to Google. Nothing is
	// shown, nothing is logged server-side, and the page just sits there. Setting it on the POST
	// response alone, as this did, is setting it on the wrong response.
	setConnectCSP(w, st.AuthURL)
	csrf := ""
	if c, cerr := r.Cookie(connectCSRFCookie); cerr == nil {
		csrf = c.Value
	}
	if r.Method != http.MethodPost {
		if csrf == "" {
			csrf = randomToken()
			http.SetCookie(w, &http.Cookie{Name: connectCSRFCookie, Value: csrf, Path: "/connect/", HttpOnly: true,
				MaxAge: 3600, SameSite: http.SameSiteLaxMode, Secure: b.secureCookies(r)})
		}
		existing, _ := b.store.UserConnection(ctx, cl.OrgID, cl.ConnID, cl.TeamID, cl.SlackUserID)
		account := ""
		if existing != nil {
			account = existing.Account
		}
		b.renderConnect(w, map[string]any{
			"Name": conn.Name, "CSRF": csrf, "Scopes": scopeLines(st.Scopes), "Account": account,
			"Parts": connectParts(conn, st.Scopes, nil), "Adding": existing.Connected(),
		})
		return
	}
	if csrf == "" || !hmac.Equal([]byte(r.FormValue("csrf")), []byte(csrf)) {
		http.Error(w, "This form has expired. Reload the page and try again.", http.StatusForbidden)
		return
	}
	if r.FormValue("disconnect") != "" {
		b.disconnectUser(ctx, cl, conn)
		b.renderConnect(w, map[string]any{"Name": conn.Name, "Done": "Disconnected. The bot can no longer reach your account."})
		return
	}
	// What this person chose to hand over. The admin's boxes are the ceiling and the intersection
	// happens in scopesForParts, so a tampered form can only ever ask for less.
	if parts := connectParts(conn, st.Scopes, nil); len(parts) > 0 {
		r.ParseForm()
		chosen := map[string]bool{}
		for _, key := range r.Form["grant"] {
			chosen[key] = true
		}
		want := scopesForParts(conn, st.Scopes, chosen)
		if want == "" {
			existing, _ := b.store.UserConnection(ctx, cl.OrgID, cl.ConnID, cl.TeamID, cl.SlackUserID)
			account := ""
			if existing != nil {
				account = existing.Account
			}
			b.renderConnect(w, map[string]any{
				"Name": conn.Name, "CSRF": csrf, "Scopes": scopeLines(st.Scopes), "Account": account,
				"Parts": connectParts(conn, st.Scopes, chosen), "Adding": account != "",
				"Error": "Pick at least one thing to connect, or close this page to connect nothing.",
			})
			return
		}
		st.Scopes = want
	}
	redirect := b.connectRedirectURI(ctx)
	if redirect == "" {
		http.Error(w, "This deployment has no public address to come back to yet; an admin has to open the console over its public URL once.", http.StatusInternalServerError)
		return
	}
	verifier, challenge := pkcePair()
	state := randomToken()
	if err := b.store.SaveConnectState(ctx, state, connectState{OrgID: cl.OrgID, ConnID: cl.ConnID,
		TeamID: cl.TeamID, SlackUserID: cl.SlackUserID, Verifier: verifier, RedirectURI: redirect}); err != nil {
		http.Error(w, "Could not start the sign-in. Try again.", http.StatusInternalServerError)
		return
	}
	q := url.Values{
		"response_type": {"code"}, "client_id": {st.ClientID}, "redirect_uri": {redirect}, "state": {state},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"},
		// offline plus consent is what makes the provider hand over a refresh token; without
		// both, a second sign-in returns an access token that dies in an hour and nothing to
		// renew it with.
		//
		// include_granted_scopes is deliberately absent. It is incremental authorisation: Google
		// adds every scope this person has ever granted the client to the request, so somebody
		// who once connected all of Google Workspace was shown all of it again however few boxes
		// they ticked — which made the choice on our page look like a lie. Asking for exactly
		// what was ticked is the whole point, and it costs nothing: connecting a second part
		// later is another sign-in with both parts ticked, which is the same one click.
		"access_type": {"offline"}, "prompt": {"consent"},
	}
	if st.Scopes != "" {
		q.Set("scope", st.Scopes)
	}
	sep := "?"
	if strings.Contains(st.AuthURL, "?") {
		sep = "&"
	}
	http.Redirect(w, r, st.AuthURL+sep+q.Encode(), http.StatusFound)
}

// handleConnectCallback is where the provider sends the browser back. Registered bare, with no
// session and no CSRF token, for the reason the MCP callback is: the provider sends neither. The
// single-use state row is what ties this request to the person who started it.
func (b *Bot) handleConnectCallback(w http.ResponseWriter, r *http.Request) {
	if e := r.URL.Query().Get("error"); e != "" {
		b.renderConnect(w, map[string]any{"Done": "Nothing was connected: " + e + ". You can close this page."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	cs, err := b.store.TakeConnectState(ctx, r.URL.Query().Get("state"))
	if err != nil || cs == nil {
		http.Error(w, "This sign-in has expired or was already finished. Ask the bot again for a fresh link.", http.StatusForbidden)
		return
	}
	conn, err := b.store.Connection(ctx, cs.OrgID, cs.ConnID)
	if err != nil || conn == nil || conn.CredType != "oauth_user" {
		http.Error(w, "That connection no longer exists.", http.StatusNotFound)
		return
	}
	st, err := b.clientTemplate(conn)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {r.URL.Query().Get("code")},
		"redirect_uri": {cs.RedirectURI}, "client_id": {st.ClientID}, "code_verifier": {cs.Verifier}}
	if err := tokenRequest(ctx, st, form); err != nil {
		http.Error(w, "The provider refused the sign-in: "+truncate(err.Error(), 300), http.StatusBadGateway)
		return
	}
	enc, err := b.proxy.sealUserSecret(st)
	if err != nil {
		http.Error(w, "Could not store the credential.", http.StatusInternalServerError)
		return
	}
	account := accountFromIDToken(st.IDToken)
	if err := b.store.SaveUserConnection(ctx, cs.OrgID, cs.ConnID, cs.TeamID, cs.SlackUserID, account, enc); err != nil {
		http.Error(w, "Could not store the credential.", http.StatusInternalServerError)
		return
	}
	done := "You're connected. Go back to Slack and ask again."
	if account != "" {
		done = "Connected as " + account + ". Go back to Slack and ask again."
	}
	b.renderConnect(w, map[string]any{"Name": conn.Name, "Done": done})
}

// disconnectUser gives the grant back to the provider before forgetting it here. Dropping the
// row alone would leave a standing grant nobody can see or withdraw.
func (b *Bot) disconnectUser(ctx context.Context, cl connectClaim, conn *Connection) {
	if uc, _ := b.store.UserConnection(ctx, cl.OrgID, cl.ConnID, cl.TeamID, cl.SlackUserID); uc != nil {
		if st, err := b.proxy.openUserSecret(uc); err == nil {
			revokeToken(ctx, st)
		}
	}
	b.store.DeleteUserConnection(ctx, cl.OrgID, cl.ConnID, cl.TeamID, cl.SlackUserID)
}

// revokeAllUnder hands back every personal grant made through a connection, for when the
// connection itself goes away.
func (b *Bot) revokeAllUnder(ctx context.Context, orgID, connID int64) {
	members, err := b.store.UserConnectionsFor(ctx, orgID, connID)
	if err != nil {
		return
	}
	for _, m := range members {
		if st, err := b.proxy.openUserSecret(m); err == nil {
			revokeToken(ctx, st)
		}
	}
	b.store.DeleteUserConnectionsFor(ctx, orgID, connID)
}

// revokeToken tells the provider the grant is over. Best effort: a provider that does not
// publish a revocation endpoint, or is simply down, must not stop us forgetting the token.
func revokeToken(ctx context.Context, st *OAuthState) {
	tok := st.RefreshToken
	if tok == "" {
		tok = st.AccessToken
	}
	if tok == "" || st.TokenURL == "" {
		return
	}
	u, err := url.Parse(st.TokenURL)
	if err != nil {
		return
	}
	u.Path = strings.TrimSuffix(u.Path, "/token") + "/revoke"
	req, err := http.NewRequestWithContext(ctx, "POST", u.String(), strings.NewReader(url.Values{"token": {tok}}.Encode()))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if resp, err := oauthHTTPClient.Do(req); err == nil {
		resp.Body.Close()
	}
}

// accountFromIDToken reads the address out of the id_token so the console and the connect page
// can say which account is linked. The token came straight from the provider's token endpoint
// over TLS a moment ago, so the payload is read without verifying the signature — and it is used
// only as a label. Nothing is ever looked up by it.
func accountFromIDToken(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	return claims.Email
}

// scopeLines turns the scope string into what it lets the bot do, in words. A consent screen
// that only lists URLs tells somebody nothing about what they are agreeing to.
func scopeLines(scopes string) []string {
	known := []struct{ suffix, says string }{
		{"calendar.readonly", "Read your calendars and when you are busy"},
		{"calendar.events", "Create and change events on your calendars"},
		{"calendar", "Read and change your calendars"},
		{"gmail.readonly", "Read your mail"},
		{"gmail.compose", "Write drafts and send mail as you"},
		{"gmail.modify", "Read your mail and change labels on it"},
		{"drive.readonly", "Read your Drive files"},
		{"drive", "Read and change your Drive files"},
		{"contacts.readonly", "Read your contacts"},
		{"contacts.other.readonly", "Read the addresses of people you have mailed"},
		{"directory.readonly", "Look people up in your organisation's directory"},
	}
	out := []string{}
	seen := map[string]bool{}
	for _, s := range strings.Fields(scopes) {
		if s == "openid" || s == "email" || s == "profile" {
			continue
		}
		says := s
		for _, k := range known {
			if strings.HasSuffix(s, "/"+k.suffix) {
				says = k.says
				break
			}
		}
		if !seen[says] {
			seen[says] = true
			out = append(out, says)
		}
	}
	return out
}

func (b *Bot) renderConnect(w http.ResponseWriter, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	connectPage.Execute(w, data)
}

// The console's own tokens, transcribed from ui/src/app/globals.css. This page cannot import
// them: it is server-rendered Go under a policy that forbids scripts, while the console is a
// static Next export. Transcribing the handful it needs is the cheap way to have the two look
// like one product — the palette, the near-square 0.25rem radius, and Geist falling back to the
// system stack, since a page in the middle of an OAuth round trip should not wait on a webfont.
var connectPage = template.Must(template.New("connect").Parse(`<!doctype html><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>Connect your account</title>
<style>
:root{
  color-scheme:light dark;
  --background:#fbfaf8; --foreground:#191c2b; --card:#ffffff; --border:#e8e6e1;
  --muted:#f2f1ed; --muted-foreground:#5d6170; --primary:#5a50c8; --primary-foreground:#ffffff;
  --accent:#eeedf7; --accent-foreground:#33307a; --destructive:#bf3b2b; --radius:0.25rem;
}
@media(prefers-color-scheme:dark){:root{
  --background:#101019; --foreground:#eceaf3; --card:#17161f; --border:#292834;
  --muted:#1d1c27; --muted-foreground:#9a97a8; --primary:#8a80e8; --primary-foreground:#101019;
  --accent:#232138; --accent-foreground:#b3aef0; --destructive:#e0604f;
}}
*{box-sizing:border-box}
body{margin:0;padding:3rem 1.25rem;background:var(--background);color:var(--foreground);
 font-family:Geist,-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;
 font-size:.9375rem;line-height:1.55;display:flex;justify-content:center;
 -webkit-font-smoothing:antialiased}
.card{max-width:29rem;width:100%;background:var(--card);border:1px solid var(--border);
 border-radius:calc(var(--radius)*1.8);padding:1.75rem}
h1{margin:0;font-size:1.125rem;font-weight:650;letter-spacing:-.01em}
.sub{margin:.25rem 0 0;color:var(--muted-foreground);font-size:.875rem}
.lede{margin:1.5rem 0 .75rem;font-size:.875rem;color:var(--muted-foreground)}
.grants{list-style:none;margin:0;padding:0;border:1px solid var(--border);
 border-radius:var(--radius);overflow:hidden}
.grants li{display:flex;gap:.625rem;align-items:flex-start;padding:.625rem .75rem;
 border-top:1px solid var(--border)}
.grants li:first-child{border-top:0}
.tick{flex:none;width:1rem;height:1rem;margin-top:.15rem;color:var(--primary)}
.who{display:flex;align-items:center;gap:.5rem;margin-top:1rem;padding:.5rem .75rem;
 background:var(--accent);color:var(--accent-foreground);border-radius:var(--radius);font-size:.8125rem}
.actions{display:flex;align-items:center;gap:1rem;margin-top:1.5rem}
button{font:inherit;font-weight:600;border:0;cursor:pointer;border-radius:calc(var(--radius)*0.8)}
.go{background:var(--primary);color:var(--primary-foreground);padding:.5rem 1rem;font-size:.875rem}
.go:hover{filter:brightness(1.08)}
.go:focus-visible,.off:focus-visible{outline:2px solid var(--primary);outline-offset:2px}
.off{background:transparent;color:var(--destructive);padding:.5rem 0;font-size:.8125rem;font-weight:500}
.off:hover{text-decoration:underline}
.note{margin:1.25rem 0 0;padding-top:1.25rem;border-top:1px solid var(--border);
 color:var(--muted-foreground);font-size:.8125rem}
.done{display:flex;gap:.625rem;align-items:flex-start}
/* More specific than the .grants li flex row used for the plain scope list. A part stacks its
   name, its hint and its scopes instead, and losing this puts the three side by side. */
.grants li.part{display:block;padding:.75rem}
.partname{margin:0;font-weight:600}
.hint{margin:.125rem 0 0;color:var(--muted-foreground);font-size:.8125rem}
.box{display:flex;gap:.5rem;align-items:flex-start;cursor:pointer;margin-top:.5rem;
 padding:.4375rem .5rem;border:1px solid var(--border);border-radius:var(--radius)}
.box:hover{background:var(--muted)}
.box input{accent-color:var(--primary);width:.9375rem;height:.9375rem;margin:.1875rem 0 0;flex:none}
.box:has(input:checked){border-color:var(--primary);background:var(--accent)}
.box:has(input:focus-visible){outline:2px solid var(--primary);outline-offset:1px}
.boxname{display:block;font-size:.875rem;font-weight:550}
.sub-grants{display:block;margin-top:.125rem;color:var(--muted-foreground);font-size:.8125rem}
.sub-grants span{display:block}
.err{margin:1rem 0 0;padding:.5rem .75rem;border-radius:var(--radius);font-size:.8125rem;
 color:var(--destructive);border:1px solid var(--destructive)}
</style>
<div class="card">
{{if .Done}}
  <div class="done">
    <svg class="tick" viewBox="0 0 16 16" fill="none" aria-hidden="true"><path d="M13.5 4.5 6.5 11.5 2.5 7.5" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round"/></svg>
    <h1>{{.Done}}</h1>
  </div>
{{else}}
  <h1>Connect your account</h1>
  <p class="sub">{{.Name}}</p>
  {{if .Account}}<p class="who">Currently connected as {{.Account}}.</p>{{end}}
  {{if .Error}}<p class="err">{{.Error}}</p>{{end}}
  {{if .Parts}}
    <p class="lede">Choose what the bot may reach. It acts only when you ask it to, and only as you.</p>
    <form method="post" id="go"><input type="hidden" name="csrf" value="{{.CSRF}}">
      <ul class="grants">{{range .Parts}}<li class="part">
        <p class="partname">{{.Label}}</p>
        {{if .Hint}}<p class="hint">{{.Hint}}</p>{{end}}
        {{range .Grants}}<label class="box">
          <input type="checkbox" name="grant" value="{{.Key}}"{{if .Checked}} checked{{end}}>
          <span>
            <span class="boxname">{{.Label}}</span>
            <span class="sub-grants">{{range .Lines}}<span>{{.}}</span>{{end}}</span>
          </span>
        </label>{{end}}
      </li>{{end}}</ul>
    </form>
  {{else}}
    <p class="lede">You are about to let the bot, acting only when you ask it to:</p>
    <ul class="grants">{{range .Scopes}}<li>
      <svg class="tick" viewBox="0 0 16 16" fill="none" aria-hidden="true"><path d="M13.5 4.5 6.5 11.5 2.5 7.5" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round"/></svg>
      <span>{{.}}</span></li>{{end}}</ul>
    <form method="post" id="go"><input type="hidden" name="csrf" value="{{.CSRF}}"></form>
  {{end}}
  <div class="actions">
    <button class="go" type="submit" form="go">Continue</button>
    {{if .Account}}<form method="post"><input type="hidden" name="csrf" value="{{.CSRF}}">
      <input type="hidden" name="disconnect" value="1">
      <button class="off" type="submit">Disconnect this account</button></form>{{end}}
  </div>

  <p class="note">Only you can use what you grant here. Anything the bot writes — an event, a
   draft — is shown to you in Slack for a yes before it happens.{{if .Adding}} You are already connected, so
   signing in again replaces what I hold with exactly what is ticked above — untick something and
   I lose it. To withdraw the app from your Google account altogether, use <em>Disconnect</em>.{{end}}</p>
{{end}}
</div>
`))

// personalInstructionsCommand answers `!personal_instructions`: read them, set them, clear them,
// without leaving Slack. The same field the Personal tab edits — this is the fast path, that one
// is the place to sit and write a paragraph.
//
// It replies in the channel where it was asked, which is safe because the text is the person's
// own and they just typed it there. The link to the settings page is not: that one is DMed.
func (a *Agent) personalInstructionsCommand(ctx context.Context, c *Call, arg string) string {
	a.ensureAccess(ctx, c) // a bang command is not a turn, so nothing has resolved it yet (connectReply)
	if c.Access == nil {
		return "I can't see this channel's connections right now."
	}
	var personal []*Connection
	for _, r := range c.Access.Rules {
		if r.Conn != nil && r.Conn.CredType == "oauth_user" {
			personal = append(personal, r.Conn)
		}
	}
	if len(personal) == 0 {
		return "Nothing in this channel runs on your own account, so there is nothing here to instruct. " +
			"Every service in this channel uses a credential an admin set up, and the channel's own instructions live on the Configure page."
	}
	arg = strings.TrimSpace(arg)

	// More than one is rare and the text is almost always meant for one of them, so rather than
	// guess, hand over the page that can address them separately.
	if arg != "" && len(personal) > 1 {
		names := make([]string, 0, len(personal))
		for _, conn := range personal {
			names = append(names, "*"+escapeMrkdwn(conn.Name)+"*")
		}
		msg := "You have more than one account connected here — " + strings.Join(names, ", ") +
			" — so I won't guess which this is for."
		if link := a.personalConfigureURL(ctx, c.OrgID, c.TeamID, c.Channel, c.UserID); link != "" && c.SL != nil {
			if a.dmLink(ctx, c, "Your accounts", "Set instructions for each account you have connected here.", link) {
				return msg + " I've sent you a link in a direct message where you can set each one."
			}
		}
		return msg + " Set them from the Configure link under any of my replies, on the *Your accounts* tab."
	}

	if arg != "" {
		text := arg
		if strings.EqualFold(arg, "clear") || strings.EqualFold(arg, "none") || strings.EqualFold(arg, "off") {
			text = ""
		}
		conn := personal[0]
		if err := a.store.SaveUserInstructions(ctx, c.OrgID, conn.ID, c.TeamID, c.UserID, text); err != nil {
			return "I couldn't save that: " + truncate(err.Error(), 200)
		}
		if text == "" {
			return fmt.Sprintf("Cleared. I'll use your *%s* account with no instructions of your own.", escapeMrkdwn(conn.Name))
		}
		return fmt.Sprintf("Saved for *%s*. I'll follow this whenever I act as you, and nobody else here is affected by it:\n>%s",
			escapeMrkdwn(conn.Name), escapeMrkdwn(oneLine(text)))
	}

	// No argument: say what is set, and how to change it.
	var b strings.Builder
	b.WriteString("*Your own instructions* — these reach me only on your turns, and nobody else in this channel sees them.\n")
	for _, conn := range personal {
		uc, _ := a.store.UserConnection(ctx, c.OrgID, conn.ID, c.TeamID, c.UserID)
		switch {
		case uc != nil && strings.TrimSpace(uc.Instructions) != "":
			fmt.Fprintf(&b, "• *%s*:\n>%s\n", escapeMrkdwn(conn.Name), escapeMrkdwn(oneLine(uc.Instructions)))
		default:
			fmt.Fprintf(&b, "• *%s*: nothing set.\n", escapeMrkdwn(conn.Name))
		}
	}
	if len(personal) == 1 {
		b.WriteString("\nSet them with `!personal_instructions <what you want>` — how I should go about the work as much as how I " +
			"should sound. For example `!personal_instructions only ever look at my inbox, never sent mail or archives` or " +
			"`!personal_instructions sign my drafts off with my first name only`. `!personal_instructions clear` removes them.")
	}
	if link := a.personalConfigureURL(ctx, c.OrgID, c.TeamID, c.Channel, c.UserID); link != "" && c.SL != nil {
		if a.dmLink(ctx, c, "Your accounts", "Write instructions for the accounts you have connected here.", link) {
			b.WriteString("\nI've also sent you a private link where you can write them out properly.")
		}
	}
	return strings.TrimSpace(b.String())
}

// dmLink sends one person a card with a link button. Used for anything that carries the
// authority to read or change their own settings, which must never be posted in a channel.
func (a *Agent) dmLink(ctx context.Context, c *Call, title, says, link string) bool {
	if c.SL == nil || c.UserID == "" {
		return false
	}
	_, _, err := c.SL.PostCard(ctx, c.UserID, "", linkCard(title, says, link), title)
	return err == nil
}

// linkCard is a titled line and one Open button: a personal link, wherever it is posted.
func linkCard(title, says, link string) Card {
	return Card{
		Parts:   []CardPart{{Markdown: "*" + escapeMrkdwn(title) + "*\n" + escapeMrkdwn(says) + " The link is yours alone."}},
		Buttons: []Button{{ActionID: actConnectOpen, Value: "open", Label: "Open", Primary: true, URL: link}},
	}
}
