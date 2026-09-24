package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Access requests: somebody asks for access, somebody else says yes.
//
// Three things can raise one, and they all end up as a single row with an ordered list of calls:
//
//   - the proxy, when a write lands on a connection an admin marked allow_grants. This is the
//     enforcement floor. It cannot be routed around, because it fires wherever the request is
//     composed — request_access, plain http_request, or a tool pack.
//   - the request_access tool, which is the front door: it carries what and why, and lets one
//     approval cover an ordered plan rather than one call.
//   - escalatePending, for the case in between: an ordinary write held for confirmation on a turn
//     where an approver was tagged is not the requester's own to press.
//
// What the approver reads is rendered from the stored calls, never from what the model wrote about
// them. The prose and the payload sit in different blocks on the card for the same reason.

const (
	actAccessApprove = "attest_access_approve"
	actAccessDeny    = "attest_access_deny"
	maxGrantCalls    = 5 // a plan an approver cannot read on a phone is a plan approved unread
	maxOpenPerPerson = 3 // an approver's DMs are a shared resource
)

// grantStep is the readable half of a recorded call. The payload itself stays in the same JSON
// shape pending_writes has always used, so runStep replays either kind without knowing about
// access requests at all; conn and label ride along as fields ProxyRequest ignores.
type grantStep struct {
	Conn   int64  `json:"conn,omitempty"`  // the connection this was matched to when recorded
	Label  string `json:"label,omitempty"` // one line a person reads
	Method string `json:"method,omitempty"`
	URL    string `json:"url,omitempty"`
	Body   string `json:"body,omitempty"`
	// AttachFiles rides along so the card can say what the write carries. It is read back out of
	// the same stored bytes that will run, never re-derived, for the reason the whole card is.
	AttachFiles []string       `json:"attach_files,omitempty"`
	MCP         int64          `json:"mcp,omitempty"`
	Tool        string         `json:"tool,omitempty"`
	Args        map[string]any `json:"args,omitempty"`
	// A held fix job, in the shape start_fix_job stores it. It is the one payload that is not a
	// call to anything: approving it dispatches a worker, which opens a draft pull request.
	// Without this field a job escalated to an approver parsed as an empty HTTP request — no
	// method, no URL — which no tier could cover and no replay could run.
	Job *JobSpec `json:"job,omitempty"`
}

// holdForGrant records a call this turn may not run on its own authority. They accumulate: a turn
// that holds three writes produces one request with three steps, and one card.
func (c *Call) holdForGrant(raw []byte) {
	if len(c.pendingGrants) < maxGrantCalls {
		c.pendingGrants = append(c.pendingGrants, json.RawMessage(raw))
	}
}

// soloGrant is one write that needs an approval to itself, rather than a step in the ordered plan
// pendingGrants carries. Filing a ticket and starting a coding job are not one decision: an
// approver may well want the first and not the second, and a single Approve covering both takes
// that choice away from them.
type soloGrant struct {
	what  string
	calls []json.RawMessage
}

// holdSolo records one such write.
func (c *Call) holdSolo(what string, raw []byte) {
	if len(c.soloGrants) < maxHeldPerTurn {
		c.soloGrants = append(c.soloGrants, soloGrant{what: what, calls: []json.RawMessage{json.RawMessage(raw)}})
	}
}

// holdHeadline is the one line an approver reads first. Built from the stored bytes, like
// everything else on the card, so the prose cannot describe something other than what will run.
func holdHeadline(raw []byte) string {
	var s grantStep
	if json.Unmarshal(raw, &s) != nil {
		return "run one write"
	}
	if s.Job != nil {
		return "start a fix job on " + s.Job.Repo + ", opening a draft pull request"
	}
	if _, ok := clickupTaskCreate(ProxyRequest{Method: s.Method, URL: s.URL}); ok {
		return "file a ticket in ClickUp"
	}
	if u, err := url.Parse(s.URL); err == nil && u.Host != "" {
		return strings.ToUpper(s.Method) + " " + u.Host + u.Path
	}
	return "run one write"
}

// taggedApprovers is which of the people tagged this turn hold any approval role. It is a hint
// for routing and for the prompt — never an authorization decision, because it is intersected
// with configuration before it is used.
func (c *Call) taggedApprovers() []string {
	out := []string{}
	for _, id := range c.Tagged {
		if slices.Contains(c.approvers, id) && (c.allowSelfApprove || id != c.UserID) {
			out = append(out, id)
		}
	}
	return out
}

func parseGrantURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	if err := checkURL(u); err != nil {
		return nil, err
	}
	return u, nil
}

// stepsOf reads the recorded payloads back into their readable form.
func stepsOf(calls []json.RawMessage) []grantStep {
	out := make([]grantStep, 0, len(calls))
	for _, raw := range calls {
		var s grantStep
		json.Unmarshal(raw, &s)
		out = append(out, s)
	}
	return out
}

// ---- the tool ----

func (a *Agent) registerAccessTools() {
	a.register(Tool{
		Name: "request_access",
		Desc: "Ask a named person to approve access the requester cannot grant themselves — an " +
			"account, a permission, an invite, a licence, a seat. List the exact calls you would " +
			"run, in order; nothing runs until an approver presses Approve, and then exactly this " +
			"list runs and nothing else. Never tell the requester the access has been granted.",
		Params: schema(map[string]any{
			"what": str("One line: the access being asked for"),
			"why":  str("The reason the requester gave, in their words"),
			"calls": map[string]any{
				"type": "array", "description": "The calls to run on approval, in order",
				"items": schema(map[string]any{
					"label":  str("One line a person reads: what this step does"),
					"method": map[string]any{"type": "string", "enum": []string{"GET", "POST", "PUT", "PATCH", "DELETE"}},
					"url":    str("Absolute https URL"),
					"body":   str("Request body (a JSON string)"),
				}, "label", "method", "url"),
			},
		}, "what", "calls"),
		Run: a.requestAccess,
	})
}

func (a *Agent) requestAccess(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
	var p struct {
		What  string      `json:"what"`
		Why   string      `json:"why"`
		Calls []grantStep `json:"calls"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	if !a.anyApprover(ctx, c.OrgID) {
		return "Nobody is set up to approve access requests. Tell the requester an admin needs to add an " +
			"approval role, with members and a bundle, on the Approvers page in the console.", nil
	}
	if len(p.Calls) == 0 || len(p.Calls) > maxGrantCalls {
		return fmt.Sprintf("Give between 1 and %d calls. If it needs more steps than that, ask for the "+
			"first part now and the rest once it is done.", maxGrantCalls), nil
	}
	if n, _ := a.store.OpenAccessRequestCount(ctx, c.OrgID, c.UserID); n >= maxOpenPerPerson {
		return fmt.Sprintf("%s already has %d access requests waiting for an answer. Ask them to chase "+
			"those before raising another.", c.SL.UserName(ctx, c.UserID), n), nil
	}
	for i := range p.Calls {
		if _, err := parseGrantURL(p.Calls[i].URL); err != nil {
			return fmt.Sprintf("Step %d: %s", i+1, err.Error()), nil
		}
	}
	// Route before recording. Validating against the tier that will actually run it is the point:
	// otherwise an approver signs off on something that turns out not to match.
	t, targets := a.routeTo(ctx, c, p.Calls)
	if t == nil {
		return "No approval tier can grant all of that — either the connection is not set up for access " +
			"grants, or the hosts are outside every approver's bundle. Say so plainly and suggest they ask " +
			"an admin, rather than trying a different URL.", nil
	}
	for i := range p.Calls {
		step := &p.Calls[i]
		u, _ := parseGrantURL(step.URL)
		conn, _ := a.proxy.Match(t.Access, strings.ToUpper(step.Method), u)
		step.Conn = conn.ID
		raw, _ := json.Marshal(step)
		c.holdForGrant(raw)
	}
	c.grantWhat, c.grantWhy = strings.TrimSpace(p.What), strings.TrimSpace(p.Why)
	c.grantRole, c.grantTargets = t.Role.ID, targets
	return fmt.Sprintf("Recorded, and it goes to %s. Tell the requester what you have asked for and who has "+
		"to approve it, in one or two sentences. Do not say it is done — nothing has run.", t.Role.Name), nil
}

// ---- raising the request ----

// escalatePending converts a write held for in-thread confirmation into an access request, for the
// case where the requester tagged an approver. Without it there is a hole: the model does the
// ordinary write, a Confirm card appears in the thread, and the requester presses it themselves.
func (a *Agent) escalatePending(ctx context.Context, c *Call) {
	if c.pendingID == 0 {
		return
	}
	// Two ways a held write turns out not to be the requester's own to press. An approver was
	// tagged — or nobody asked for it at all: an email turn's requester is synthetic, so the
	// in-thread card would be a button belonging to nobody, five minutes long, on a mail that
	// arrived at three in the morning. The approval card waits a week and names who may answer.
	if len(c.taggedApprovers()) == 0 && !c.emailTurn() {
		return
	}
	if slices.Contains(c.approvers, c.UserID) {
		return // an approver asking for something can still confirm it in the thread
	}
	// Every write this turn held, not only the last. Each becomes a request of its own, because
	// each is a decision of its own — a ticket filed and a coding job started are not one thing
	// to say yes to, and an approver who wants the first and not the second has to be able to
	// say so. Before this the earlier hold got no card at all.
	for _, h := range c.held {
		raw, err := a.store.PendingWriteRequest(ctx, c.OrgID, h.id)
		if err != nil || raw == "" {
			continue
		}
		a.store.DiscardPendingWrite(ctx, c.OrgID, c.TeamID, c.Channel, h.id)
		c.holdSolo(holdHeadline([]byte(raw)), []byte(raw))
	}
	c.pendingID, c.pendingSummary, c.held = 0, "", nil
}

// postAccessRequest writes the row and sends the card, once the turn's answer is already in the
// thread — same ordering as the confirm card, so people read what is proposed before the buttons.
func (a *Agent) postAccessRequest(ctx context.Context, c *Call) {
	if c.SL == nil {
		return
	}
	// The ordered plan request_access built, if there is one, and then one request per write that
	// needs an answer of its own. Separate cards, separately answered.
	if len(c.pendingGrants) > 0 {
		a.raiseAccessRequest(ctx, c, c.pendingGrants, c.grantWhat, c.grantWhy)
	}
	for _, g := range c.soloGrants {
		a.raiseAccessRequest(ctx, c, g.calls, g.what, "")
	}
}

func (a *Agent) raiseAccessRequest(ctx context.Context, c *Call, calls []json.RawMessage, what, why string) {
	if len(calls) == 0 {
		return
	}
	steps := stepsOf(calls)
	t, targets := a.routeTo(ctx, c, steps)
	if t == nil || len(targets) == 0 {
		c.SL.PostText(ctx, c.Channel, c.ThreadTS,
			"I've held that for an approver, but no approval tier can grant it, so I can't ask anyone. An admin can set that up on the Approvers page.")
		return
	}
	if c.grantRole != 0 && c.grantRole != t.Role.ID {
		// request_access already routed this turn; keep its answer rather than routing twice.
		if picked, _ := a.store.ApprovalRole(ctx, c.OrgID, c.grantRole); picked != nil {
			targets = c.grantTargets
		}
	}
	r := &AccessRequest{
		OrgID: c.OrgID, TeamID: c.TeamID, Channel: c.Channel, ThreadTS: c.ThreadTS, Requester: c.UserID,
		Approver: targets[0], Approvers: targets, RoleID: t.Role.ID,
		What: what, Why: why, Ask: truncate(c.Text, 1500),
		Calls: calls,
	}
	if r.What == "" && c.emailTurn() {
		// What the mail is about says far more to an approver than which host the steps happen
		// to reach, and on this lane there is no request_access call to have supplied a headline.
		r.What = truncate(oneLine(c.Text), 300)
	}
	if r.What == "" {
		r.What = "access to " + strings.Join(grantHosts(r.Calls), ", ")
	}
	if err := a.store.AddAccessRequest(ctx, r); err != nil {
		slog.Error("record access request", "err", err)
		return
	}
	a.deliverAccessCards(ctx, c.SL, r)
}

func (a *Agent) deliverAccessCards(ctx context.Context, sl *Chat, r *AccessRequest) {
	requester := sl.UserName(ctx, r.Requester)
	where := "<#" + r.Channel + ">"
	if isDirectConversation(r.Channel) {
		where = "a direct message"
	}
	private := sl.IsPrivateConversation(ctx, r.Channel)
	card := accessCard(r, requester, where, private)
	sent := 0
	for _, ap := range r.Approvers {
		// chat.postMessage takes a user id as the channel and resolves the IM. It returns the
		// D… id it actually landed in, which is what updating the card later needs.
		ch, ts, err := sl.PostCard(ctx, ap, "", card, "An access request is waiting for you.")
		if err != nil {
			slog.Warn("could not DM an approver", "approver", ap, "err", err)
			continue
		}
		a.store.AddAccessCard(ctx, r.OrgID, r.ID, ap, ch, ts)
		sent++
	}
	if sent == 0 {
		// Never leave the requester believing somebody was asked.
		a.store.FinishAccessRequest(ctx, r.OrgID, r.ID, "failed", "", "the approver could not be reached")
		sl.PostText(ctx, r.Channel, r.ThreadTS,
			"I couldn't reach an approver by DM, so this hasn't been asked. An admin may need to add the `im:write` scope to the Slack app.")
		if a.settings.Get(ctx, sl.OrgID).AlertChannel != "" {
			a.alert(ctx, sl.OrgID, fmt.Sprintf("access-dm:%d", r.ID),
				fmt.Sprintf("Could not DM any approver about access request #%d.", r.ID))
		}
		return
	}
	_, ts, _ := sl.PostCard(ctx, r.Channel, r.ThreadTS, accessThreadCard(r), "Waiting for approval.")
	if ts != "" {
		a.store.SetAccessOriginTS(ctx, r.OrgID, r.ID, ts)
	}
}

// accessThreadCard is the copy that goes back in the thread the request came from. In a channel
// it carries the same two buttons as the approvers' DMs, because that is where the people waiting
// on it are already looking: an approver who is in the channel should not have to go and find a
// DM to release something the whole thread is stuck behind. It matters most on a forwarded email,
// where the thread is the only place anybody is watching.
//
// The steps are shown here for the same reason they are shown on the DM: a button pressed against
// a one-line headline is a button pressed unread. Nothing is leaked by it — these are the calls
// this channel's own turn proposed, in the channel it proposed them in.
//
// Who may press is decided on the press (mayApprove), never by who can see the card, so a card
// everybody can see is still not a card everybody can act on. A press by somebody without an
// approval role is answered on the card and changes nothing.
func accessThreadCard(r *AccessRequest) Card {
	card := Card{
		Parts: []CardPart{{Markdown: fmt.Sprintf("*Waiting on %s* — access request #%d: %s",
			mentionList(r.Approvers), r.ID, escapeMrkdwn(r.What))}},
		Footer: "Nothing runs until it is approved. Expires " + humanExpiry(r.ExpiresAt) + ".",
	}
	// A DM origin is the requester's own conversation with the bot: no approver can see it, so
	// buttons there would be an Approve nobody present is allowed to press.
	if isDirectConversation(r.Channel) {
		return card
	}
	card.Parts = append(card.Parts, CardPart{Markdown: "*What I'll run if you approve*\n" + renderSteps(r.Calls)})
	card.Buttons = accessButtons(r)
	return card
}

// accessButtons are the two answers to a request, on every copy of its card.
func accessButtons(r *AccessRequest) []Button {
	v := confirmValue(r.ID, "")
	return []Button{
		{ActionID: actAccessApprove, Value: v, Label: "Approve", Primary: true},
		{ActionID: actAccessDeny, Value: v, Label: "Deny"},
	}
}

// ---- the card ----

// accessCard is what an approver reads. The step list is built from the stored calls, so the
// prose on the card cannot misdescribe what will run; the requester's own words are kept, but in
// their own paragraph, escaped, and labelled as theirs.
func accessCard(r *AccessRequest, requesterName, where string, private bool) Card {
	// Who asked. A person is mentioned, so the card pings them; a turn nobody started is named
	// instead, because "<@email:C0123>" is not a mention — it renders as that literal string —
	// and because the distinction is the first thing an approver needs to read here. Nobody in
	// the workspace stands behind this one.
	who := "<@" + r.Requester + ">"
	if isEmailRequester(r.Requester) {
		who = requesterName
	}
	head := fmt.Sprintf("*%s is asking for access*\n%s", who, escapeMrkdwn(r.What))
	ctxLine := fmt.Sprintf("Asked by %s in %s · request #%d", who, where, r.ID)
	if private {
		ctxLine += " · private channel, contents not shown"
	}
	parts := []CardPart{
		{Markdown: head},
		{Markdown: ctxLine, Small: true},
	}
	if words := strings.TrimSpace(r.Why + "\n" + r.Ask); strings.TrimSpace(words) != "" {
		parts = append(parts, CardPart{Markdown: "*Their words* (unverified)\n>" +
			strings.ReplaceAll(escapeMrkdwn(truncate(redact(words), 600)), "\n", "\n>")})
	}
	parts = append(parts, CardPart{Markdown: "*What I'll run if you approve*\n" + renderSteps(r.Calls)})
	return Card{Parts: parts, Buttons: accessButtons(r), Footer: accessFooter(r, who)}
}

// accessFooter is the line under the buttons. For a person it is the self-approval rule; for a
// turn nobody started there is no such person, so it says where the request came from instead —
// which is what decides how hard the steps above deserve to be read.
func accessFooter(r *AccessRequest, who string) string {
	if isEmailRequester(r.Requester) {
		return fmt.Sprintf("Expires %s. Nobody in this workspace asked for this: it came from a "+
			"forwarded email, and the words above are the sender's.", humanExpiry(r.ExpiresAt))
	}
	return fmt.Sprintf("Expires %s. %s cannot approve their own request.", humanExpiry(r.ExpiresAt), who)
}

// renderSteps turns the stored payloads into the numbered list on the card. This is the only
// description of the calls anyone sees, and it is built from the bytes that will be replayed.
func renderSteps(calls []json.RawMessage) string {
	if len(calls) == 0 {
		return "_nothing — this request has no steps_"
	}
	var b strings.Builder
	for i, raw := range calls {
		var s grantStep
		json.Unmarshal(raw, &s)
		switch {
		case s.Job != nil:
			fmt.Fprintf(&b, "%d. start a fix job on `%s` — branch from `%s`, open a *draft* pull request: %s",
				i+1, escapeMrkdwn(s.Job.Repo), escapeMrkdwn(nonEmpty(s.Job.BaseBranch, "the default branch")),
				escapeMrkdwn(truncate(oneLine(s.Job.Title), 160)))
		case s.MCP != 0:
			fmt.Fprintf(&b, "%d. run `%s`", i+1, escapeMrkdwn(s.Tool))
		default:
			u, err := url.Parse(s.URL)
			shown := s.URL
			if err == nil {
				shown = u.Host + u.Path
				if u.RawQuery != "" {
					// Parameter names only: a token in a query string must not be printed on a
					// card that then lives in a DM forever.
					keys := []string{}
					for k := range u.Query() {
						keys = append(keys, k)
					}
					slices.Sort(keys)
					shown += "?" + strings.Join(keys, "=…&") + "=…"
				}
			}
			fmt.Fprintf(&b, "%d. `%s %s`", i+1, strings.ToUpper(s.Method), escapeMrkdwn(truncate(shown, 200)))
			if n := len(s.AttachFiles); n > 0 {
				fmt.Fprintf(&b, " (and upload %d file(s) from the thread to it)", n)
			}
		}
		if s.Label != "" {
			fmt.Fprintf(&b, " — %s", escapeMrkdwn(truncate(oneLine(s.Label), 120)))
		}
		b.WriteString("\n")
		if body := strings.TrimSpace(s.Body); body != "" {
			fmt.Fprintf(&b, "```%s```\n", escapeCode(truncate(redact(body), 500)))
		}
	}
	return b.String()
}

// escapeMrkdwn neutralises text somebody else wrote. Slack display names are user-set, so without
// this a requester called "Alice (approved by Security)" forges card structure, and a link written
// as <https://evil.example|https://api.github.com/…> shows the safe URL and goes somewhere else.
func escapeMrkdwn(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

func mentionList(ids []string) string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, "<@"+id+">")
	}
	return strings.Join(out, " or ")
}

func humanExpiry(at string) string {
	t, err := time.Parse(time.DateTime, at)
	if err != nil {
		return "in 7 days"
	}
	return fmt.Sprintf("<!date^%d^{date_short_pretty}|on %s>", t.Unix(), t.Format("2 Jan"))
}

func grantHosts(calls []json.RawMessage) []string {
	out := []string{}
	for _, raw := range calls {
		var s grantStep
		json.Unmarshal(raw, &s)
		if u, err := url.Parse(s.URL); err == nil && u.Host != "" {
			out = appendOnce(out, u.Hostname())
		}
	}
	if len(out) == 0 {
		return []string{"a connected service"}
	}
	return out
}

// ---- pressing a button ----

// mayApprove reports whether this person can answer this request. Both the snapshot taken when the
// card went out and the live configuration have to allow it: a later config change cannot widen an
// open request, and removing somebody takes effect immediately.
func (b *Bot) mayApprove(ctx context.Context, sl *Chat, user string, r *AccessRequest) (bool, string) {
	if user == "" || user == sl.BotUserID {
		return false, "Only a person can approve an access request."
	}
	// The request names the workspace it came from, and the card is answered there. A press
	// arriving through any other connected workspace is somebody else's directory vouching for
	// somebody else's account, and it does not count.
	if r.TeamID != "" && sl.TeamID != "" && r.TeamID != sl.TeamID {
		return false, "This request belongs to a different workspace, so it can't be answered from here."
	}
	if user == r.Requester && !b.settings.Get(ctx, r.OrgID).AllowSelfApprove {
		return false, "You can't approve your own access request."
	}
	// A tier covers everything below it, so a super admin can answer a request routed to a plain
	// approver. Both the snapshot taken when the card went out and the live configuration have to
	// allow it: a later change cannot widen an open request, and removing somebody takes effect now.
	rank := b.agent.rankOf(ctx, r.OrgID, sl, user)
	if rank < 0 {
		return false, "You don't hold an approval role, so I can't act on this."
	}
	if role, _ := b.store.ApprovalRole(ctx, r.OrgID, r.RoleID); role != nil && rank < role.Rank {
		return false, fmt.Sprintf("This one needs %s or above.", role.Name)
	}
	if len(r.Approvers) > 0 && !slices.Contains(r.Approvers, user) && rank == 0 {
		return false, "You're not one of the approvers this request was sent to."
	}
	// Fresh, not cached: an approval is worth one API call to be sure the account still stands.
	uf, err := sl.UserFacts(ctx, user)
	if err != nil {
		return false, "I couldn't check your account with Slack just now, so I've stopped here. Try again in a moment."
	}
	switch {
	case uf.Deleted:
		return false, "That account is deactivated."
	case uf.Bot:
		return false, "Only a person can approve an access request."
	case uf.Restricted:
		return false, "Guest accounts can't approve access requests."
	case uf.TeamID != "" && sl.TeamID != "" && uf.TeamID != sl.TeamID:
		return false, "Only members of this workspace can approve access requests."
	}
	return true, ""
}

// accessPressed handles Approve and Deny.
func (b *Bot) accessPressed(ctx context.Context, sl *Chat, user, channel, card string, id int64, approve bool) {
	// The press arrives on the socket loop's context, which has no deadline and dies with a
	// SIGTERM from a revision swap. A grant must not be cancelled halfway through.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	defer cancel()

	r, err := b.store.AccessRequest(ctx, sl.OrgID, id)
	if err != nil || r == nil {
		sl.ResolveCard(ctx, channel, card, Card{}, "I can't find that request any more.")
		return
	}
	if ok, why := b.mayApprove(ctx, sl, user, r); !ok {
		// Say so on the card. Logging and returning, as the confirm path used to, leaves the
		// approver watching the spinner clear with nothing to show for it.
		sl.ResolveCard(ctx, channel, card, Card{}, why)
		return
	}
	if !approve {
		denied, err := b.store.DenyAccessRequest(ctx, r.OrgID, id, user, "")
		if err != nil || denied == nil {
			b.resolveAllCards(ctx, sl, r, b.alreadyAnswered(ctx, r.OrgID, id))
			return
		}
		b.resolveAllCards(ctx, sl, r, fmt.Sprintf("Denied by <@%s>. Reply here with a reason and I'll pass it on.", user))
		sl.PostText(ctx, r.Channel, r.ThreadTS,
			fmt.Sprintf("<@%s> <@%s> declined this one. Nothing was run.", r.Requester, user))
		b.auditSlack(ctx, sl, user, "access_request.denied", AuditEvent{TargetKind: "access_request", TargetID: strconv.FormatInt(id, 10),
			TargetName: r.What, Details: auditDetails(map[string]any{"requester": r.Requester, "channel": r.Channel})})
		return
	}
	selfOK := b.settings.Get(ctx, r.OrgID).AllowSelfApprove
	taken, err := b.store.TakeAccessRequest(ctx, r.OrgID, id, user, selfOK && user == r.Requester)
	if err != nil || taken == nil {
		b.resolveAllCards(ctx, sl, r, b.alreadyAnswered(ctx, r.OrgID, id))
		return
	}
	note := fmt.Sprintf("Approved by <@%s> — running it now.", user)
	if taken.SelfApproved {
		// Never let this pass quietly. One person playing both parts is a testing convenience,
		// not an approval, and everyone who can see the thread should be able to tell.
		slog.Warn("access request self-approved", "id", taken.ID, "user", user)
		note = fmt.Sprintf("*Self-approved* by <@%s> — running it now. (Self-approval is on for this workspace.)", user)
		sl.PostText(ctx, taken.Channel, taken.ThreadTS,
			fmt.Sprintf("<@%s> approved their own access request #%d — self-approval is switched on here.", user, taken.ID))
	}
	b.resolveAllCards(ctx, sl, taken, note)
	// The grant is the one act in Slack that spends a credential on somebody else's say-so, so
	// it is the one most worth a row: who approved, for whom, and whether they were the same
	// person.
	b.auditSlack(ctx, sl, user, "access_request.approved", AuditEvent{TargetKind: "access_request", TargetID: strconv.FormatInt(id, 10),
		TargetName: taken.What, Details: auditDetails(map[string]any{"requester": taken.Requester, "channel": taken.Channel,
			"steps": len(taken.Calls), "self_approved": taken.SelfApproved})})
	b.runGrant(ctx, sl, taken, user)
}

// alreadyAnswered reads back what actually happened, so a lost race says something true.
func (b *Bot) alreadyAnswered(ctx context.Context, orgID, id int64) string {
	r, err := b.store.AccessRequest(ctx, orgID, id)
	if err != nil || r == nil {
		return "That request is gone."
	}
	switch r.Status {
	case "expired":
		return "This expired before anyone answered it."
	case "denied":
		return fmt.Sprintf("Already denied by <@%s>.", r.DecidedBy)
	case "cancelled":
		return "The person who asked withdrew this."
	case "pending":
		return "I couldn't take this one — you may be the requester."
	}
	return fmt.Sprintf("Already answered by <@%s>.", r.DecidedBy)
}

// resolveAllCards rewrites every copy of the card. A request goes to everyone who can answer it,
// so settling it has to clear all of them — otherwise the others keep live buttons.
func (b *Bot) resolveAllCards(ctx context.Context, sl *Chat, r *AccessRequest, outcome string) {
	keep := accessCard(r, "", "", false) // ResolveCard keeps what it said and drops the rest
	cards, _ := b.store.AccessCards(ctx, r.OrgID, r.ID)
	for _, c := range cards {
		sl.ResolveCard(ctx, c.Channel, c.TS, keep, outcome)
	}
	if r.OriginTS != "" {
		sl.ResolveCard(ctx, r.Channel, r.OriginTS, Card{},
			fmt.Sprintf("Access request #%d: %s", r.ID, outcome))
	}
}

// ---- running an approved request ----

// replay runs the recorded calls in order, stopping at the first failure. It touches no Slack API,
// so it can be tested end to end against an httptest server.
func (b *Bot) replay(ctx context.Context, sl *Chat, acc *Access, r *AccessRequest, by string) ([]string, error) {
	notes := []string{}
	for i, raw := range r.Calls {
		var s grantStep
		json.Unmarshal(raw, &s)
		// Pin the connection. Over a week a credential can be rotated, a bundle detached, or a
		// different connection start matching the same host — and then this would quietly spend
		// the wrong one.
		if s.Conn != 0 && s.MCP == 0 {
			u, err := url.Parse(s.URL)
			if err != nil {
				return notes, fmt.Errorf("step %d has an unreadable URL", i+1)
			}
			conn, why := b.proxy.Match(acc, strings.ToUpper(s.Method), u)
			if why != "" {
				return notes, fmt.Errorf("step %d can no longer run: %s", i+1, why)
			}
			if conn == nil || conn.ID != s.Conn {
				return notes, fmt.Errorf("step %d would now use a different connection than the one approved, so I've stopped", i+1)
			}
		}
		audit := ProxyAudit{TeamID: r.TeamID, Channel: r.Channel, ThreadTS: r.ThreadTS, Requester: r.Requester, AccessRequestID: r.ID}
		note, status, err := b.runStep(ctx, sl, r.OrgID, acc, audit, by, string(raw))
		if err != nil {
			return notes, fmt.Errorf("step %d of %d failed: %s", i+1, len(r.Calls), err.Error())
		}
		// A non-2xx is a failure here even though the proxy returns it without an error: the step
		// did not do what was approved, so the rest of the plan must not be applied on top of it.
		if status != 0 && (status < 200 || status > 299) {
			// With the service's own words, not just the number. A 400 from ClickUp names the
			// field it would not take; reporting "HTTP 400" alone leaves whoever approved this
			// with nothing to act on and no way to tell a wrong id from an outage.
			return notes, fmt.Errorf("step %d of %d failed. %s", i+1, len(r.Calls), truncate(oneLine(note), 400))
		}
		notes = append(notes, note)
	}
	return notes, nil
}

// runGrant replays an approved request and reports back in the thread the ask came from.
func (b *Bot) runGrant(ctx context.Context, sl *Chat, r *AccessRequest, approver string) {
	fail := func(reason string) {
		b.store.FinishAccessRequest(ctx, r.OrgID, r.ID, "failed", "", reason)
		b.resolveAllCards(ctx, sl, r, "Couldn't run it: "+reason)
		sl.PostText(ctx, r.Channel, r.ThreadTS, fmt.Sprintf("<@%s> %s", r.Requester, reason))
	}
	// Nothing is trusted from a week-old record: not the requester's standing, not the config.
	if ok, why := b.mayUseBot(ctx, sl, r.Requester); !ok {
		fail("I can't complete this: " + why)
		return
	}
	role, err := b.store.ApprovalRole(ctx, r.OrgID, r.RoleID)
	if err != nil || role == nil {
		fail("the approval tier this was routed to no longer exists, so I've not run it")
		return
	}
	// Execution runs under what the tier grants from, not the approver's own reach: what ran
	// must be what the card described, even when a super admin answered a request meant for a
	// plain approver.
	acc := b.agent.roleGrants(ctx, r.OrgID, *role)
	if len(acc.Rules) == 0 {
		fail("the " + role.Name + " tier no longer grants from any active connection")
		return
	}
	notes, err := b.replay(ctx, sl, acc, r, approver)
	result := strings.Join(notes, "\n")
	if err != nil {
		done := "Nothing ran."
		if len(notes) > 0 {
			done = fmt.Sprintf("Steps 1–%d succeeded and were not undone.", len(notes))
		}
		b.store.FinishAccessRequest(ctx, r.OrgID, r.ID, "failed", result, err.Error())
		b.resolveAllCards(ctx, sl, r, fmt.Sprintf("%s %s", err.Error(), done))
		sl.PostText(ctx, r.Channel, r.ThreadTS,
			fmt.Sprintf("<@%s> <@%s> approved this, but %s %s", r.Requester, approver, err.Error(), done))
		return
	}
	b.store.FinishAccessRequest(ctx, r.OrgID, r.ID, "executed", result, "")
	link := sl.Permalink(ctx, r.Channel, r.ThreadTS)
	outcome := fmt.Sprintf("Ran %d of %d steps. I've told <@%s>.", len(notes), len(r.Calls), r.Requester)
	if link != "" {
		outcome = fmt.Sprintf("Ran %d of %d steps. <%s|I've told <@%s>>.", len(notes), len(r.Calls), link, r.Requester)
	}
	// The approver is owed the thing their approval made, not just a count of steps: the card is
	// where they pressed the button and is where they come back to. The URL is a service's own
	// answer rather than the requester's words, but it is escaped all the same — a card must not
	// be able to render a masked link, whoever wrote it.
	if made := viewedLink(result); made != "" {
		outcome += "\nWhat it made: " + escapeMrkdwn(made)
	}
	b.resolveAllCards(ctx, sl, r, outcome)

	// Hand the result to the model to summarise, the same way a confirmed write is reported. The
	// note is what the model sees; the response bodies inside it are already redacted by runStep.
	note := fmt.Sprintf("<@%s> approved <@%s>'s access request #%d and it has now run. %s",
		approver, r.Requester, r.ID, truncate(result, 2000))
	b.store.AddTurn(ctx, r.TeamID, r.Channel, r.ThreadTS, "note", approver, note, "", 0, 0)
	sess, kind := b.threadSession(ctx, sl, r.Channel, r.ThreadTS)
	c := &Call{TeamID: r.TeamID, OrgID: sl.OrgID, SL: sl, Channel: r.Channel, ThreadTS: r.ThreadTS, UserID: r.Requester, Kind: kind, Session: sess, NoTools: true,
		Text: fmt.Sprintf("The access request was approved by <@%s> and has run. Report the result briefly, "+
			"addressing <@%s> directly. If the result carries a \"view:\" link, that is the page for what was just "+
			"made — put it in your reply as a link they can click, not only an id. Do not repeat any credential or "+
			"token from the result.", approver, r.Requester),
		Streamer: sl.NewStreamer(r.Channel, r.ThreadTS, r.Requester)}
	if err := b.agent.Run(ctx, c); err != nil {
		sl.PostText(ctx, r.Channel, r.ThreadTS,
			fmt.Sprintf("<@%s> that's approved and done — %d of %d steps ran.", r.Requester, len(notes), len(r.Calls)))
	}
}

// ---- withdrawal, deny reasons, expiry ----

// withdrawAccessRequests is the requester taking their own ask back. Only their own: one person
// saying "cancel" must not drop somebody else's request that shares the thread.
func (b *Bot) withdrawAccessRequests(ctx context.Context, sl *Chat, channel, threadTS, user string) int {
	gone, err := b.store.CancelAccessRequests(ctx, sl.TeamID, channel, threadTS, user)
	if err != nil {
		return 0
	}
	for i := range gone {
		b.resolveAllCards(ctx, sl, &gone[i], fmt.Sprintf("Withdrawn by <@%s>. Nothing ran.", user))
	}
	return len(gone)
}

// denyReason relays a sentence typed after pressing Deny. Bounded on purpose: only the person who
// denied, only while the reason is still empty, only for a few minutes afterwards.
func (b *Bot) denyReason(ctx context.Context, sl *Chat, user, text string) bool {
	id, err := b.store.AwaitingDenyReason(ctx, sl.OrgID, user)
	if err != nil || id == 0 {
		return false
	}
	r, err := b.store.AccessRequest(ctx, sl.OrgID, id)
	if err != nil || r == nil {
		return false
	}
	b.store.SetAccessReason(ctx, sl.OrgID, id, truncate(text, 500))
	sl.PostText(ctx, r.Channel, r.ThreadTS,
		fmt.Sprintf("<@%s> <@%s> added: %s", r.Requester, user, truncate(oneLine(text), 500)))
	return true
}

// sweepAccessRequests closes out what nobody answered and tells the people who were waiting.
// Silent expiry is the failure mode worth avoiding: the requester is owed an answer either way.
func (b *Bot) sweepAccessRequests(ctx context.Context) {
	gone, err := b.store.ExpireAccessRequests(ctx)
	if err != nil || len(gone) == 0 {
		return
	}
	for i := range gone {
		r := &gone[i]
		// The sweep runs for every workspace at once, so each request names its own.
		sl, err := b.slacks.For(ctx, r.TeamID)
		if err != nil {
			slog.Warn("could not close an expired access request", "id", r.ID, "team", r.TeamID, "err", err)
			continue
		}
		b.resolveAllCards(ctx, sl, r, "Expired without an answer.")
		sl.PostText(ctx, r.Channel, r.ThreadTS, fmt.Sprintf(
			"<@%s> nobody answered this in time, so I've closed access request #%d. Ask again if you still need it — I still have the exact calls on file.",
			r.Requester, r.ID))
	}
}

// accessCommand is `!access`: what the caller has waiting, and a way to take it back. Withdrawal
// has to be something a person can do deliberately — the alternative is a request sitting in
// somebody's DMs for a week after it stopped being wanted.
func (a *Agent) accessCommand(ctx context.Context, c *Call, arg string) string {
	if strings.EqualFold(strings.TrimSpace(arg), "cancel") {
		gone, err := a.store.CancelAccessRequests(ctx, c.TeamID, c.Channel, c.ThreadTS, c.UserID)
		if err != nil {
			return "Couldn't withdraw that: " + err.Error()
		}
		if len(gone) == 0 {
			return "You have nothing waiting in this thread."
		}
		if a.cancelled != nil {
			a.cancelled(ctx, gone, c.UserID)
		}
		return fmt.Sprintf("Withdrawn %d request(s). I've told the approver.", len(gone))
	}
	rs, err := a.store.AccessRequests(ctx, c.OrgID, "pending", 20)
	if err != nil {
		return "Couldn't read the queue: " + err.Error()
	}
	var b strings.Builder
	for _, r := range rs {
		if r.Requester != c.UserID {
			continue
		}
		fmt.Fprintf(&b, "• #%d %s — waiting on %s, expires %s\n", r.ID, r.What, mentionList(r.Approvers), humanExpiry(r.ExpiresAt))
	}
	if b.Len() == 0 {
		return "You have no access requests waiting."
	}
	return "*Your access requests*\n" + b.String() + "\nReply `!access cancel` in the thread one was raised in to withdraw it."
}

// accessWithdrawn clears the approver's cards for requests a person has just taken back.
func (b *Bot) accessWithdrawn(ctx context.Context, rs []AccessRequest, by string) {
	for i := range rs {
		sl, err := b.slacks.For(ctx, rs[i].TeamID)
		if err != nil {
			slog.Warn("could not clear a withdrawn request's cards", "id", rs[i].ID, "err", err)
			continue
		}
		b.resolveAllCards(ctx, sl, &rs[i], fmt.Sprintf("Withdrawn by <@%s>. Nothing ran.", by))
	}
}
