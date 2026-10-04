package app

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
)

// The console assistant answers questions about what an admin is looking at, and stages changes
// they confirm by hand.
//
// It writes nothing. Not "writes carefully" — there is no write in this file or in
// assistant_api.go, and TestAssistantHasNoWriteCapability parses both to keep it that way. A
// staged change is a list of steps, and each step is one call the BROWSER makes to an endpoint
// the console already has, under the same permission, the same CSRF check, the same validation
// and the same audit row as the page that normally makes it. So Confirm hands nobody any
// authority they did not already hold, and "what can the model do?" has the same answer as
// "what can this file do?", which is nothing.
//
// The model never writes a path. Every step is built here from ids that were resolved inside
// the caller's organisation, and checked against stepAllowed before it can be staged — because
// a card that says "add Alice to Approvers" over a step that deletes a connection would be a
// lie told in the console's own voice.
//
// It also has its own turn loop and its own tool type rather than borrowing the Agent's.
// Agent.Run is a Slack turn all the way down: turn() calls c.SL.SetStatus unconditionally,
// runToolRaw reads c.Session.ToolCalls and posts to c.Streamer, the answer path writes a row
// into the per-channel turns table, and chooseModel reads the channel's scope. Every one of
// those is nil or meaningless here. consoleTool.Run takes a *consoleCall, so a Slack tool
// cannot be registered here and a console tool cannot be registered on the Agent — a compile
// error rather than a line in a review checklist.

// assistantChannel is what the assistant's usage and tool calls are filed under. A colon cannot
// appear in a Slack channel id, so this can never be mistaken for one — the same trick the
// playground uses for its thread keys.
const assistantChannel = "console:assistant"

// channelLabel is a channel scope's name as people write it, with one #. Scopes are stored with
// theirs already ("#ops"), and the listings that added another read "##ops" to the model, which
// said it back.
func channelLabel(name string) string { return "#" + strings.TrimPrefix(name, "#") }

// turnChannel says where a turn in the activity happened: a channel by name, a DM as one, and the
// console's own lanes by what they are. RecentTurns carries no names, and every row used to read
// "in —" — so a question about which channels were busy took a call per channel, and the
// assistant's and the playground's own turns looked like a channel that had since gone.
func (b *Bot) turnChannel(ctx context.Context, t TurnRow) string {
	switch {
	case t.ChannelName != "":
		return t.ChannelName
	case t.Channel == assistantChannel:
		return "the console assistant"
	case strings.HasPrefix(t.Channel, playgroundPrefix):
		return "the playground"
	case t.Channel == "":
		return ""
	}
	name := b.channelName(ctx, t.TeamID, t.Channel)
	if name == t.Channel || name == "DM" {
		return name // an id nobody could name, or a direct message
	}
	return channelLabel(name)
}

const (
	assistantRounds   = 6                // tool rounds; a sidebar question is not an investigation
	assistantToolWall = 20 * time.Second // one tool
	assistantWall     = 90 * time.Second // the whole turn; a little longer now files can arrive
	assistantInFlight = 3                // concurrent turns per organisation
	assistantPerHour  = 60               // turns per console account per hour
	assistantHistory  = 8                // turns of history the browser may send back
	assistantResultC  = 4000             // one tool result, as the playground caps it
	assistantMaxFiles = 4
	assistantMaxBytes = 6 << 20  // one file
	assistantMaxTotal = 16 << 20 // all of them
	assistantFileText = 40_000   // characters of one text file put in front of the model
)

// consoleAttachment is one file the person attached to their question, already read.
type consoleAttachment struct {
	Name string
	// ImageURL is a data: URL when this is an image the model can be shown. Text is the
	// extracted contents when it is something that has any. Exactly one is set; a file that is
	// neither is named to the model and nothing more, because its name is the whole truth.
	ImageURL string
	Text     string
	Note     string // why there is no content, when there is none
}

// consoleCall is one console question: who asked, from where, with what, and what it spent.
type consoleCall struct {
	OrgID int64
	// OrgPublic is OrgID by its public id, which every card staged here carries (stage): the session a
	// card is confirmed under is whichever organisation it last switched to, in any tab, and a card
	// must only ever land in the one it was proposed in.
	OrgPublic string
	// Actor is the console account asking, by its opaque public id. Never a Slack id: these
	// rows sit in the same usage table as Slack turns, and a console account borrowing a Slack
	// id would spend that person's allowance and confuse every reader of the table since.
	Actor string
	// Perms is what this session may do. Both the tool list and the set of things read_console
	// will name are built from it, so what a caller may not have is absent rather than present
	// and refusing — defsFor says why on the Slack side and it is the same here: a tool the
	// model can see but cannot call costs a whole round to find that out.
	Perms map[string]bool
	// Path is the console route the question was asked from, as the address bar has it, and
	// Page its title. They shape the prompt and nothing else; no tool reads them.
	Path, Page string
	// Scope is the channel the page had selected, re-resolved here inside OrgID from the id the
	// browser sent. The browser's id is a claim and this is the row — which is the whole reason
	// the id is re-resolved rather than passed through.
	Scope *Scope
	// Focus is what else the page had open — a review type, a level of the review settings —
	// resolved inside OrgID the same way (resolveFocus). Nil when the page had nothing, or nothing
	// this caller may read about.
	Focus *consoleFocus
	Files []consoleAttachment

	model     string
	llm       *LLM // the endpoint it runs on, resolved once (consoleLLM)
	rounds    int
	usage     Usage
	calls     []assistantTool
	proposals []proposal
	// resultCap and resultMore are what the reader read_console just ran allows its result and says
	// to ask for once it is cut (consoleReader.cap and .more), spent by runConsoleTool on that one
	// result. A field rather than a return value because the cut happens a level up, after the
	// redaction; it is race-free because a round's tool calls run one after another.
	resultCap  int
	resultMore string
}

// consoleLLM is the endpoint a console turn runs on: the organisation's, like every other model
// call it causes (model_endpoints.go).
func (b *Bot) consoleLLM(ctx context.Context, c *consoleCall) (*LLM, error) {
	if c.llm != nil {
		return c.llm, nil
	}
	l, err := b.agent.llmFor(ctx, c.OrgID)
	if err != nil {
		return nil, err
	}
	if l == nil {
		return nil, errors.New("no model endpoint is configured")
	}
	c.llm = l
	return l, nil
}

// consoleTool is one thing the assistant may do. Perm is the permission the caller must hold
// for it to be offered at all.
type consoleTool struct {
	Name, Desc string
	Perm       string
	Params     map[string]any
	Run        func(ctx context.Context, c *consoleCall, args json.RawMessage) (string, error)
	// needs is every permission a call's arguments are about, for args: read_console's the resource it
	// names, a propose_ tool's those its Confirm needs. Nil is Perm alone. Activity shows the arguments
	// to a reader holding these as well as what the turn's own words need (consoleArgNeeds).
	needs func(args json.RawMessage) []string
}

func (t consoleTool) def() openai.ChatCompletionToolUnionParam {
	return openai.ChatCompletionFunctionTool(openai.FunctionDefinitionParam{
		Name: t.Name, Description: openai.String(t.Desc), Parameters: openai.FunctionParameters(t.Params),
	})
}

// ---- proposals ----

// proposalChange is one field as a person reads it on the card: a value before and after, unless
// Format says it is something else.
type proposalChange struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	From  string `json:"from"`
	To    string `json:"to"`
	// Format is "" for a value, "text" for prose the card shows whole rather than cut (a review
	// type's purpose), and "list" for a list shown entry by entry in Items (a review type's rules, a
	// level's branch rules). A list's To is its summary — "6 rules (1 added)" — which is what the
	// audit row keeps; the entries are the card's.
	Format string         `json:"format,omitempty"`
	Items  []proposalItem `json:"items,omitempty"`
}

// proposalItem is one entry of a list change, marked with what happens to it: "+" added, "-"
// removed or switched off, "~" changed, "↕" moved, "=" unchanged — one entry standing for every
// one that is, so a forty-rule type does not draw thirty-nine rows to show one.
type proposalItem struct {
	Mark string `json:"mark"`
	Text string `json:"text"`
}

// proposalStep is one call Confirm makes, in the order they are listed. Method and Path are
// built here and never by the model; see stepAllowed.
type proposalStep struct {
	Method string         `json:"method"`
	Path   string         `json:"path"`
	Body   map[string]any `json:"body,omitempty"`
	Label  string         `json:"label"` // what this step does, in the console's words
}

// proposal is a change staged for a human and written nowhere.
type proposal struct {
	// ID is minted here and echoed back in the body of every step that carries one. Those
	// handlers read the keys they know by name, so an unknown one writes nothing — but their
	// audit loops record what they were sent, which is what ties the proposal's audit row to
	// the save's without touching either endpoint.
	ID string `json:"id"`
	// Org is the organisation the card was proposed in, by its public id, which stage also puts in the
	// body of every step it carries. The panel will not confirm a card while the console is signed in to
	// another organisation, and the endpoints refuse a body naming one that is not the session's
	// (refuseOtherOrg) — a type's key, "general", names a type in every organisation, and a create names
	// nothing at all, so the path alone would land in whichever one the session switched to since.
	Org     string           `json:"org,omitempty"`
	Kind    string           `json:"kind"`   // "channel" | "approval" | "review_type" | "review_settings"
	Target  string           `json:"target"` // what it is about, in words: "#ops", "Senior approvers"
	Changes []proposalChange `json:"changes"`
	Steps   []proposalStep   `json:"steps"`
	// Note is what the card says beside the change because the change alone does not show it: a
	// level that stops inheriting its parent's list, a built-in that is copied on its first save.
	// Based is what the change was read from ("Based on v3"), which is what Confirm is checked
	// against: a card is refused, writing nothing, once that has changed.
	Note  string `json:"note,omitempty"`
	Based string `json:"based,omitempty"`
	// Requires is the id of another card in the same answer that has to be confirmed first — a
	// branch rule naming a type only that card creates — so the panel holds this one back until it is.
	Requires string `json:"requires,omitempty"`
	// Open is where the change is seen on its own page. Refresh is the API paths whose readers load
	// again once it is confirmed, so the page behind the panel shows the change without a reload.
	Open    *proposalLink `json:"open,omitempty"`
	Refresh []string      `json:"refresh,omitempty"`

	auditTeam, auditKind, auditID string
	// auditDetail is added to the assistant.proposed row (proposalAudit): what the card's own
	// summary leaves out and a reader of the audit log needs, such as a list before and after where
	// nothing else keeps the before.
	auditDetail map[string]any
	// key is the one thing this card changes, set on a card that cannot stand beside another on the
	// same thing (see stage); empty on one that can.
	key string
}

// stepOrgKey is the body key a staged step names its card's organisation by (proposal.Org).
const stepOrgKey = "org"

// proposalLink is a link the card offers to the page its change is on.
type proposalLink struct {
	Href  string `json:"href"`
	Label string `json:"label"`
}

// The paths the assistant may stage, and nothing else. Every one is an endpoint the console
// already offers on a page, so Confirm is the person making a request they could have made by
// hand — under the same permission, which is the only thing that actually decides whether it
// lands.
//
// Two absences are deliberate. DELETE /api/approval-roles/{id} is not here: deleting a tier
// takes away who can approve every credential it grants, and that belongs to somebody sitting
// on the Approvers page deciding to do it, not to a button that appeared underneath a sentence.
// Nor is anything that attaches a bundle or a connection to a CHANNEL — that is reach, and
// reach should be granted where it can be seen next to everything else the channel already has.
var assistantSteps = []struct {
	method string
	path   *regexp.Regexp
}{
	{"PUT", regexp.MustCompile(`^/api/scopes/\d+$`)},
	{"POST", regexp.MustCompile(`^/api/approval-roles$`)},
	{"PUT", regexp.MustCompile(`^/api/approval-roles/\d+$`)},
	{"POST", regexp.MustCompile(`^/api/approval-roles/\d+/members$`)},
	{"DELETE", regexp.MustCompile(`^/api/approval-roles/\d+/members\?ref=[^&]*$`)},
	{"POST", regexp.MustCompile(`^/api/approval-roles/\d+/(bundles|connections)/\d+$`)},
	{"DELETE", regexp.MustCompile(`^/api/approval-roles/\d+/(bundles|connections)/\d+$`)},
	// A review type is saved by its key and made new by the list's POST — the Types tab's own Save and
	// New type. Its Reset, Revert, Try and the switches are not here: a reset or a revert throws a
	// version's work away, and on and off ride in the PUT, where the version it was read at guards them.
	{"PUT", regexp.MustCompile(`^/api/review-types/[a-z][a-z0-9-]*$`)},
	{"POST", regexp.MustCompile(`^/api/review-types$`)},
	// A level's branch rules are saved by the Settings tab's own PUT: a connection, a group or a
	// repository with settings of its own by its id, and a repository with none yet through its
	// connection, by owner/name escaped as the one parameter — so nothing else can ride along in the
	// query. A level's Move, Remove, Restore and Delete are not here: each changes what is reviewed at
	// all, which is decided on the page.
	{"PUT", regexp.MustCompile(`^/api/review-settings/[0-9a-f]{32}(\?repo=[A-Za-z0-9-]+%2F[A-Za-z0-9_.-]+)?$`)},
}

func stepAllowed(method, path string) bool {
	for _, s := range assistantSteps {
		if s.method == method && s.path.MatchString(path) {
			return true
		}
	}
	return false
}

// stage records a proposal after checking every step it carries. A step that is not on the list
// is a mistake in this file rather than something a person did, so it fails the turn loudly
// instead of quietly dropping the step and drawing a card that does less than it says.
//
// A card with a key replaces the one already staged on the same kind and key, in its place, rather
// than standing beside it. Those are the cards that freeze what they were read from — a review
// type's version, a level's branch rules — and the second of two such cards on one thing could
// never be confirmed: the first one's Confirm changes exactly what the second was checked against,
// so it would be refused as stale every time. The replacement keeps the first card's id
// (proposalIDFor), so a card that Requires it still names it. Cards without a key — a channel's
// fields, a tier's members — each write only what they show, and stand side by side as before.
//
// Every card is bound to the organisation it was proposed in: it carries its public id, and so does
// the body of each of its steps, which the endpoints check against the session's (refuseOtherOrg). A
// step with no body is a DELETE or an attach by ids the endpoint resolves inside the session's
// organisation, which another one's ids name nothing in.
func (c *consoleCall) stage(p proposal) error {
	for _, s := range p.Steps {
		if !stepAllowed(s.Method, s.Path) {
			return fmt.Errorf("internal: %s %s is not a step the assistant may stage", s.Method, s.Path)
		}
	}
	if c.OrgPublic != "" {
		p.Org = c.OrgPublic
		for _, s := range p.Steps {
			if s.Body != nil {
				s.Body[stepOrgKey] = c.OrgPublic
			}
		}
	}
	if p.key != "" {
		for i := range c.proposals {
			if c.proposals[i].Kind != p.Kind || c.proposals[i].key != p.key {
				continue
			}
			if c.proposals[i].ID != p.ID {
				return fmt.Errorf("internal: a %s card on %s replaces %s and must keep its id", p.Kind, p.key, c.proposals[i].ID)
			}
			c.proposals[i] = p
			return nil
		}
	}
	c.proposals = append(c.proposals, p)
	return nil
}

// proposalIDFor is the id a card on kind and key is staged under: the id of the card already staged
// on them in this answer, which the new one replaces (stage), or a new one. replaces says which, so
// the tool can tell the model that the earlier card is gone and this one carries only what it was
// just asked for.
func (c *consoleCall) proposalIDFor(kind, key string) (id string, replaces bool) {
	for _, p := range c.proposals {
		if key != "" && p.Kind == kind && p.key == key {
			return p.ID, true
		}
	}
	return newProposalID(), false
}

func newProposalID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "prop_" + hex.EncodeToString(b)
}

// proposableScopeFields are the settings the assistant may propose on a channel. It is the set
// PUT /api/scopes/{id} applies and nothing else.
var proposableScopeFields = map[string]bool{
	"instructions": true, "default_model": true, "member_edits": true,
	"read_all": true, "email_intake": true, "email_auto_writes": true, "monthly_budget_usd": true,
	"max_tool_rounds": true, "allow_rules": true, "default_repo": true,
}

// ---- what it can read ----

// consoleReader is one thing read_console will fetch. Perm is what the caller must hold; the
// console's own route for the same data is gated on exactly the same permission, so the
// assistant is never a way around a page somebody cannot open.
type consoleReader struct {
	name, perm, desc string
	run              func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error)
	// cap is how much of its result reaches the model, in bytes, when that is not assistantResultC:
	// a reader whose whole answer is one long list, cut short of its end, answers a different
	// question than the one asked. more is what to ask for once it is cut anyway.
	cap  int
	more string
	// on and kinds keep a reader to where it means something: a feature that is on here, and the
	// pages whose focus is one of kinds (none: every page). A reader that is not offered costs
	// nothing; one that is costs its catalogue line and enum entry on every round of every question.
	on    func(*Bot) bool
	kinds []string
}

func (b *Bot) consoleReaders() []consoleReader {
	return append([]consoleReader{
		{name: "channels", desc: "every channel this organisation has configured", run: func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
			scopes, err := b.store.Scopes(ctx, c.OrgID)
			if err != nil {
				return "", err
			}
			var out []string
			for _, s := range scopes {
				if s.Kind != "channel" {
					continue
				}
				if id != "" && !strings.Contains(strings.ToLower(s.Name), strings.ToLower(id)) && s.SlackID != id {
					continue
				}
				kind := "public"
				if s.IsPrivate {
					kind = "private"
				}
				out = append(out, fmt.Sprintf("- %s (%s, %s) id=%s", channelLabel(s.Name), kind, b.teamName(ctx, s.TeamID), s.SlackID))
			}
			return listOr(out, "no channels match"), nil
		}},
		{name: "channel", desc: "one channel in full: its settings, what each field inherits, and what it can reach (id = the channel id)", run: func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
			sc, err := b.consoleScope(ctx, c, id)
			if err != nil {
				return "", err
			}
			out := map[string]any{"channel": sc.Name, "id": sc.SlackID, "scope_id": sc.ID, "workspace": sc.TeamName,
				"settings": scopeSettingsMap(sc), "inherited": b.inheritedSummary(ctx, c.OrgID, sc)}
			// The settings themselves are open — GET /api/scopes/{id} is too — but the names of
			// the credentials behind them are gated on the console's own route.
			if c.Perms[PermConnView] {
				acc, _ := b.resolver.Resolve(ctx, c.OrgID, sc.TeamID, sc.SlackID, -1)
				out["can_reach"] = b.accessSummary(ctx, c.OrgID, sc, acc)
			} else {
				out["can_reach"] = "hidden: you do not hold connections.view"
			}
			return jsonResult(out), nil
		}},
		{name: "approval_tiers", desc: "the tiers that can approve access: rank, members, and what each may grant", run: func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
			roles, err := b.store.ApprovalRoles(ctx, c.OrgID)
			if err != nil {
				return "", err
			}
			out := []map[string]any{}
			for _, r := range roles {
				members := []string{}
				for _, m := range r.Resolved {
					members = append(members, fmt.Sprintf("%s (%s in %s)", m.Ref, m.SlackUserID, m.TeamID))
				}
				out = append(out, map[string]any{"id": r.ID, "name": r.Name, "rank": r.Rank,
					"members": members, "bundle_ids": r.BundleIDs, "connection_ids": r.ConnectionIDs})
			}
			return jsonResult(out), nil
		}},
		{name: "bundles", perm: PermConnView, desc: "the named groups of connections a channel or a tier can be given", run: func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
			bs, err := b.store.Bundles(ctx, c.OrgID)
			if err != nil {
				return "", err
			}
			var out []string
			for _, bd := range bs {
				out = append(out, fmt.Sprintf("- %s (id %d)", bd.Name, bd.ID))
			}
			return listOr(out, "this organisation has no bundles yet"), nil
		}},
		{name: "models", desc: "the models this provider offers, and which one the organisation uses", run: func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
			st := b.settings.Get(ctx, c.OrgID)
			l, err := b.consoleLLM(ctx, c)
			if err != nil {
				return "", err
			}
			models, err := l.ListModels(ctx, false)
			if err != nil {
				return "", fmt.Errorf("the model catalogue is not reachable right now: %w", err)
			}
			out := []string{"organisation default: " + orDash(st.Model), `"heavy" resolves to: ` + orDash(st.HeavyModel)}
			for _, m := range models {
				if m.Kind == "chat" {
					out = append(out, "- "+m.ID)
				}
			}
			return listOr(out, "the catalogue is empty"), nil
		}},
		{name: "access_requests", perm: PermAccessView, desc: "who asked for what, who answered, and what ran", run: func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
			rs, err := b.store.AccessRequests(ctx, c.OrgID, id, limit)
			if err != nil {
				return "", err
			}
			var out []string
			for i := range rs {
				r := &rs[i]
				out = append(out, fmt.Sprintf("- #%d %s — %s, asked by %s in %s, %d step(s)%s",
					r.ID, r.Status, truncate(oneLine(r.What), 120), r.Requester, r.Channel, len(r.Calls), decidedBy(r)))
			}
			return listOr(out, "no access requests match"), nil
		}},
		{name: "activity", perm: PermActivityView, desc: "recent turns the bot has taken", run: func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
			rows, err := b.store.RecentTurns(ctx, c.OrgID, id, limit, "")
			if err != nil {
				return "", err
			}
			var out []string
			for _, t := range rows {
				out = append(out, fmt.Sprintf("- %s in %s (%s): %d in, %d out, $%.4f", t.At, orDash(b.turnChannel(ctx, t)), t.Model, t.In, t.Out, t.Cost))
			}
			return listOr(out, "no turns match"), nil
		}},
		{name: "audit", perm: PermAuditView, desc: "who signed in, what they changed, and what they approved", run: func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
			evs, err := b.store.AuditEvents(ctx, c.OrgID, AuditFilter{Action: id, Limit: limit})
			if err != nil {
				return "", err
			}
			// The target's name quoted, and the actor's on one line: both are whatever somebody typed — a
			// group's name, a channel's, their own — and a line break in one would start a line of its own
			// in what the model reads.
			var out []string
			for _, e := range evs {
				target := orDash(e.TargetName)
				if strings.TrimSpace(e.TargetName) != "" {
					target = strconv.Quote(e.TargetName)
				}
				out = append(out, fmt.Sprintf("- %s %s by %s (%s) on %s %s", e.At, e.Action,
					orDash(oneLine(e.ActorName)), orDash(e.Via), orDash(e.TargetKind), target))
			}
			return listOr(out, "no audit events match"), nil
		}},
		{name: "documents", desc: "the files the bot can search when it answers", run: func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
			docs, err := b.store.Documents(ctx, c.OrgID)
			if err != nil {
				return "", err
			}
			var out []string
			for _, d := range docs {
				out = append(out, "- "+d.Path)
			}
			return listOr(out, "no documents yet"), nil
		}},
		{name: "routines", perm: PermRoutinesManage, desc: "prompts that run on a schedule and post to a channel", run: func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
			rs, err := b.store.Routines(ctx, c.OrgID, id)
			if err != nil {
				return "", err
			}
			var out []string
			for _, r := range rs {
				state := "enabled"
				if !r.Enabled {
					state = "disabled"
				}
				out = append(out, fmt.Sprintf("- #%d (%s) %s in %s: %s", r.ID, state, r.Cron, r.Channel, truncate(oneLine(r.Prompt), 100)))
			}
			return listOr(out, "no routines yet"), nil
		}},
		{name: "jobs", perm: PermJobsView, desc: "fix jobs the bot handed to a worker", run: func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
			js, err := b.store.Jobs(ctx, c.OrgID, JobFilter{Limit: limit})
			if err != nil {
				return "", err
			}
			var out []string
			for _, j := range js {
				out = append(out, fmt.Sprintf("- #%d %s — %s", j.ID, j.Status, truncate(oneLine(j.Title), 120)))
			}
			return listOr(out, "no jobs yet"), nil
		}},
		{name: "artifacts", perm: PermArtifactsView, desc: "files the bot made and posted in Slack", run: func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
			as, err := b.store.Artifacts(ctx, c.OrgID, limit)
			if err != nil {
				return "", err
			}
			var out []string
			for _, a := range as {
				out = append(out, fmt.Sprintf("- %s (%s, %s)", a.Title, a.Kind, a.At))
			}
			return listOr(out, "no artifacts yet"), nil
		}},
		{name: "settings", perm: PermSettingsManage, desc: "the organisation's models, budget and limits", run: func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
			// Named fields only. Settings also holds the shape of a stored web key, and a
			// reader that rendered the struct would put it in a tool result that Activity shows
			// to anybody holding activity.view.
			st := b.settings.Get(ctx, c.OrgID)
			return jsonResult(map[string]any{
				"model": st.Model, "heavy_model": st.HeavyModel,
				"monthly_budget_usd": st.MonthlyBudgetUSD, "effective_budget_usd": st.EffectiveBudget(),
				"max_tool_rounds": st.MaxToolRounds, "user_rate_limit": st.UserRateLimit,
				"allow_self_approve": st.AllowSelfApprove,
			}), nil
		}},
		{name: "overview", desc: "spend, usage and what the bot has to work with", run: func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
			return jsonResult(b.store.OverviewStats(ctx, c.OrgID)), nil
		}},
		{name: "spend", desc: "this month's cost, broken down by workspace and channel", run: func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
			rows, err := b.store.UsageByChannel(ctx, c.OrgID)
			if err != nil {
				return "", err
			}
			total, err := b.store.MonthSpend(ctx, c.OrgID, "", "")
			if err != nil {
				return "", err
			}
			st := b.settings.Get(ctx, c.OrgID)
			out := []string{fmt.Sprintf("this month: $%.4f of the account's $%.2f budget", total, st.EffectiveBudget())}
			for _, r := range rows {
				where := r.Channel
				if where == assistantChannel {
					where = "the console assistant"
				}
				out = append(out, fmt.Sprintf("- %s (%s): %d turns, %d in, %d out, $%.4f",
					orDash(where), orDash(b.teamName(ctx, r.TeamID)), r.Turns, r.In, r.Out, r.Cost))
			}
			return listOr(out, "nothing spent this month"), nil
		}},
		{name: "tool_calls", perm: PermActivityView, desc: "every call the bot has made, with arguments and results — the log to run an analysis over", run: func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
			rows, err := b.store.RecentToolCalls(ctx, c.OrgID, id, limit, false, "")
			if err != nil {
				return "", err
			}
			var out []string
			for _, t := range rows {
				state := "ok"
				if !t.OK {
					state = "FAILED"
				}
				// This assistant's own earlier calls, read as Activity reads them for this caller: their
				// arguments are somebody's question, and only shown to whoever may read it. The result is
				// the one Activity shows too, since a call logged before they were kept private kept
				// what it read whole, and that goes with its arguments.
				args, result := t.Args, t.Result
				if t.Channel == assistantChannel {
					row := t
					if row.unmarkPrivateFor(c.Perms); len(row.Withheld) > 0 {
						args = privateMark + " withheld: reading them needs " + strings.Join(row.Withheld, ", ")
					} else {
						args = privateMark + " " + row.Args
					}
					result = strings.TrimSpace(privateMark + " " + row.Result)
				}
				out = append(out, fmt.Sprintf("- %s %s in %s (%s, %dms) args=%s result=%s",
					t.At, t.Name, orDash(t.Channel), state, t.MS,
					truncate(oneLine(args), 200), truncate(oneLine(result), 300)))
			}
			return listOr(out, "no tool calls match"), nil
		}},
	}, b.reviewReaders()...)
}

func decidedBy(r *AccessRequest) string {
	if r.DecidedBy == "" {
		return ""
	}
	return ", answered by " + r.DecidedBy
}

func listOr(lines []string, empty string) string {
	if len(lines) == 0 {
		return empty
	}
	return strings.Join(lines, "\n")
}

// ---- tools ----

// consoleTools builds the registry for one caller. Per request rather than once at startup,
// because what somebody may be offered depends on what they may do: GET /api/scopes is open to
// any signed-in member, but bundles need connections.view and the audit log needs audit.view,
// and a custom role may hold any combination. Building the list from the caller's permissions
// is what stops the assistant becoming a second read surface that is looser than the first.
func (b *Bot) consoleTools(c *consoleCall) []consoleTool {
	readers := []consoleReader{}
	names := []string{}
	var catalogue strings.Builder
	for _, rd := range b.consoleReaders() {
		if rd.perm != "" && !c.Perms[rd.perm] {
			continue
		}
		if !c.onPage(b, rd.on, rd.kinds) {
			continue
		}
		readers = append(readers, rd)
		names = append(names, rd.name)
		fmt.Fprintf(&catalogue, "\n- %s: %s", rd.name, rd.desc)
	}

	// One tool with a resource list rather than a tool per page. The whole tool set is re-sent
	// on every round of every turn, so thirteen schemas would be paid for thirteen times over
	// on a question that reads one thing.
	tools := []consoleTool{{
		Name: "read_console",
		Desc: "Read something from this console. Resources available to you:" + catalogue.String(),
		Params: schema(map[string]any{
			"resource": map[string]any{"type": "string", "enum": names},
			"id":       str("Optional: the channel id for 'channel', a name filter for 'channels', a status for 'access_requests', an action for 'audit', a channel for 'activity' and 'routines'"),
			"limit":    num("Optional: how many rows, default 20, max 100"),
		}, "resource"),
		Run: func(ctx context.Context, c *consoleCall, args json.RawMessage) (string, error) {
			var p struct {
				Resource, ID string
				Limit        int
			}
			json.Unmarshal(args, &p)
			if p.Limit <= 0 || p.Limit > 100 {
				p.Limit = 20
			}
			for _, rd := range readers {
				if rd.name == p.Resource {
					c.resultCap, c.resultMore = rd.cap, rd.more
					return rd.run(ctx, b, c, strings.TrimSpace(p.ID), p.Limit)
				}
			}
			// Naming what IS available beats "unknown resource": the model asked for a page it
			// cannot see, and the useful answer is which ones it can.
			return "", fmt.Errorf("%q is not something you can read; you have: %s", p.Resource, strings.Join(names, ", "))
		},
		// A read's arguments are about the resource it names — an audit action, a channel's activity —
		// and are no more anybody's to see than the resource is.
		needs: func(args json.RawMessage) []string {
			var p struct{ Resource string }
			json.Unmarshal(args, &p)
			for _, rd := range readers {
				if rd.name == p.Resource && rd.perm != "" {
					return []string{rd.perm}
				}
			}
			return nil
		},
	}}

	// The guide needs no permission: it is this product's public documentation, and the question
	// it answers — "how would I set this up" — is the one somebody without any permissions is
	// most likely to be asking.
	tools = append(tools, consoleTool{
		Name: "search_guide",
		Desc: "Search attest_tag's own documentation: how connections, path prefixes, methods and access grants are configured, " +
			"what an approval tier grants, how confirmed writes work, plans and what they cost, deployment and security, " +
			"and code review: its types and their rules, branch rules, the settings tree, shadow and live, commands. " +
			"Use it for any 'how do I…', 'how does X work' or 'what does X cost' question — the answer is here and not in this organisation's data, " +
			"and none of it is in your training.",
		Params: schema(map[string]any{"query": str("What to look for, in the product's own words")}, "query"),
		Run: func(ctx context.Context, c *consoleCall, args json.RawMessage) (string, error) {
			var p struct{ Query string }
			json.Unmarshal(args, &p)
			if strings.TrimSpace(p.Query) == "" {
				return "", errors.New("say what to look for")
			}
			return searchGuide(p.Query), nil
		},
	})

	for _, p := range b.consoleProposers() {
		if holdsAll(c.Perms, p.perms) && c.onPage(b, p.on, p.kinds) {
			t := p.tool()
			perms := p.perms
			t.needs = func(json.RawMessage) []string { return perms }
			tools = append(tools, t)
		}
	}
	return tools
}

// consoleProposer is one propose_ tool and who it is offered to. perms are every permission its
// Confirm needs: a card whose steps the person could not make is a button that only fails. on is
// whether its feature is on here at all, and kinds the page focus kinds it belongs to — a tool for
// one screen's settings is offered on that screen, and its schema is not paid for on every round of
// every question asked anywhere else.
type consoleProposer struct {
	tool  func() consoleTool
	perms []Permission
	on    func(*Bot) bool
	kinds []string
}

// consoleProposers are the propose_ tools in the order they are offered. A screen the assistant can
// change something on adds its tool here with the focus kinds it belongs to (focusKinds).
func (b *Bot) consoleProposers() []consoleProposer {
	return []consoleProposer{
		{tool: b.proposeChannelTool, perms: []Permission{PermScopesManage}},
		{tool: b.proposeApprovalTool, perms: []Permission{PermApproversManage}},
		// reviews.manage does not imply reviews.view (console_roles.go), and a card about a type or a list
		// of branch rules the person cannot read is one they cannot check before they confirm it.
		{tool: b.proposeReviewTypeTool, perms: []Permission{PermReviewsView, PermReviewsManage}, on: reviewsOn, kinds: reviewKinds},
		{tool: b.proposeBranchRulesTool, perms: []Permission{PermReviewsView, PermReviewsManage}, on: reviewsOn, kinds: reviewKinds},
	}
}

// onPage reports whether a reader or a proposer kept to a feature and to some pages is offered on
// this call: its feature is on (on nil: always), and it belongs to every page (no kinds) or the
// page's focus is one of its kinds. The focus is the resolved one, so a kind the browser merely
// claimed — on another page, or by a caller who may not read what it names — offers nothing.
func (c *consoleCall) onPage(b *Bot, on func(*Bot) bool, kinds []string) bool {
	if on != nil && !on(b) {
		return false
	}
	return len(kinds) == 0 || (c.Focus != nil && slices.Contains(kinds, c.Focus.Kind))
}

// holdsAll reports whether held has every permission in need.
func holdsAll(held map[string]bool, need []Permission) bool {
	for _, p := range need {
		if !held[p] {
			return false
		}
	}
	return true
}

func (b *Bot) proposeChannelTool() consoleTool {
	return consoleTool{
		Name: "propose_channel_settings",
		Perm: PermScopesManage,
		Desc: "Propose a change to one channel's settings. This writes NOTHING: it checks the change and hands the person a card with a Confirm button, and the change happens only if they press it. " +
			"Fields: instructions, default_model, member_edits (inherit|allow|block), read_all (inherit|on|off), email_intake (inherit|on|off), email_auto_writes (inherit|on|off: whether this channel's allow rules apply on a turn a forwarded email started), " +
			"monthly_budget_usd (a number), max_tool_rounds (a whole number; 0 inherits), allow_rules (a JSON array of sentences), default_repo (owner/name). " +
			"Send only the fields that should change. Bundles and connections are attached on the channel's own page, not here.",
		Params: schema(map[string]any{
			"channel": str("The channel's Slack id"),
			"changes": map[string]any{
				"type":                 "object",
				"description":          "The fields to change, as strings, keyed by the names above",
				"additionalProperties": map[string]any{"type": "string"},
			},
		}, "channel", "changes"),
		Run: func(ctx context.Context, c *consoleCall, args json.RawMessage) (string, error) {
			var p struct {
				Channel string
				Changes map[string]string
			}
			json.Unmarshal(args, &p)
			sc, err := b.consoleScope(ctx, c, p.Channel)
			if err != nil {
				return "", err
			}
			if len(p.Changes) == 0 {
				return "", errors.New("name at least one field to change")
			}
			keys := make([]string, 0, len(p.Changes))
			for k := range p.Changes {
				keys = append(keys, k)
			}
			sortStrings(keys)

			current := scopeSettingsMap(sc)
			prop := proposal{ID: newProposalID(), Kind: "channel", Target: channelLabel(sc.Name),
				auditTeam: sc.TeamID, auditKind: "scope", auditID: strconv.FormatInt(sc.ID, 10)}
			body := map[string]any{}
			for _, k := range keys {
				v := p.Changes[k]
				if !proposableScopeFields[k] {
					return "", fmt.Errorf("%q is not a channel setting that can be changed here", k)
				}
				// Checked with the save's own rule, so a card is never offered for something
				// that would fail under the person's hand. The sentence the model gets back is
				// the one the save would have given, which is what it should repeat.
				if err := b.scopeFieldError(ctx, c.OrgID, sc, k, v); err != nil {
					return "", err
				}
				if k == "default_model" && !b.modelOffered(ctx, c.OrgID, v) {
					return "", fmt.Errorf("Default model: %q is not a model this provider offers; read the models resource", v)
				}
				if from := current[k]; from != v {
					prop.Changes = append(prop.Changes, proposalChange{Key: k, Label: scopeFieldLabel(k), From: from, To: v})
					body[k] = v
				}
			}
			if len(prop.Changes) == 0 {
				return "every field you named already has that value; nothing to propose", nil
			}
			body["proposal_id"] = prop.ID
			prop.Steps = []proposalStep{{Method: "PUT", Path: "/api/scopes/" + strconv.FormatInt(sc.ID, 10),
				Body: body, Label: "Save #" + sc.Name + "'s settings"}}
			if err := c.stage(prop); err != nil {
				return "", err
			}
			var out strings.Builder
			fmt.Fprintf(&out, "Staged for #%s. NOTHING HAS CHANGED YET — the person must press Confirm on the card.\n", sc.Name)
			for _, ch := range prop.Changes {
				fmt.Fprintf(&out, "- %s: %s → %s\n", ch.Label, orDash(ch.From), orDash(ch.To))
			}
			return out.String(), nil
		},
	}
}

// approvalActions are the changes to an approval tier the assistant may stage. Deleting a tier
// is not one of them: it takes away who can approve everything that tier grants, and the card
// for it would look like every other card.
var approvalActions = []string{"create_tier", "rename_tier", "add_member", "remove_member",
	"attach_bundle", "detach_bundle", "attach_connection", "detach_connection"}

func (b *Bot) proposeApprovalTool() consoleTool {
	return consoleTool{
		Name: "propose_approval_change",
		Perm: PermApproversManage,
		Desc: "Propose a change to who can approve access, and to what a tier may grant. This writes NOTHING: it hands the person a card with a Confirm button. " +
			"Read the approval_tiers resource first for ids and current members. A member is a Slack user id (U…) or an email address. " +
			"Deleting a tier is not offered here — that is done on the Approvers page.",
		Params: schema(map[string]any{
			"action":        map[string]any{"type": "string", "enum": approvalActions},
			"tier_id":       num("The tier's id, for everything except create_tier"),
			"name":          str("For create_tier and rename_tier"),
			"rank":          num("For create_tier: higher ranks cover everything a lower one can"),
			"member":        str("For add_member and remove_member: a Slack user id or an email address"),
			"bundle_id":     num("For attach_bundle and detach_bundle"),
			"connection_id": num("For attach_connection and detach_connection"),
		}, "action"),
		Run: func(ctx context.Context, c *consoleCall, args json.RawMessage) (string, error) {
			var p struct {
				Action       string `json:"action"`
				Name         string `json:"name"`
				Member       string `json:"member"`
				TierID       int64  `json:"tier_id"`
				Rank         int64  `json:"rank"`
				BundleID     int64  `json:"bundle_id"`
				ConnectionID int64  `json:"connection_id"`
			}
			json.Unmarshal(args, &p)
			p.Name, p.Member = strings.TrimSpace(p.Name), strings.TrimSpace(p.Member)

			prop := proposal{ID: newProposalID(), Kind: "approval", auditKind: "approver"}

			// create_tier is the one action with no tier to resolve yet.
			if p.Action == "create_tier" {
				if p.Name == "" {
					return "", errors.New("a new tier needs a name")
				}
				prop.Target = p.Name
				prop.Changes = []proposalChange{{Key: "tier", Label: "New approval tier", From: "—", To: fmt.Sprintf("%s (rank %d)", p.Name, p.Rank)}}
				prop.Steps = []proposalStep{{Method: "POST", Path: "/api/approval-roles",
					Body:  map[string]any{"name": p.Name, "rank": p.Rank, "proposal_id": prop.ID},
					Label: "Create the tier " + p.Name}}
				if err := c.stage(prop); err != nil {
					return "", err
				}
				return "Staged. NOTHING HAS CHANGED YET — the person must press Confirm.", nil
			}

			role, err := b.consoleTier(ctx, c, p.TierID)
			if err != nil {
				return "", err
			}
			prop.Target = role.Name
			prop.auditID = strconv.FormatInt(role.ID, 10)
			base := "/api/approval-roles/" + strconv.FormatInt(role.ID, 10)

			switch p.Action {
			case "rename_tier":
				if p.Name == "" {
					return "", errors.New("give the new name")
				}
				prop.Changes = []proposalChange{{Key: "name", Label: "Tier name", From: role.Name, To: p.Name}}
				prop.Steps = []proposalStep{{Method: "PUT", Path: base,
					Body: map[string]any{"name": p.Name, "proposal_id": prop.ID}, Label: "Rename the tier"}}

			case "add_member", "remove_member":
				// The same check the console's own field makes, so a proposal that names
				// something which is neither a Slack id nor an address is refused here rather
				// than on the press.
				ids, emails, badOnes := parseApprovers(p.Member)
				if len(badOnes) > 0 || len(ids)+len(emails) == 0 {
					return "", fmt.Errorf("%q is not a Slack user id (U…) or an email address", p.Member)
				}
				if p.Action == "add_member" {
					// An approver is a person in a workspace, and the endpoint resolves the
					// entry in one at the moment it is added. With none connected it refuses,
					// so proposing it here would draw a card that cannot be pressed.
					if !b.hasActiveTeam(ctx, c.OrgID) {
						return "", errors.New("connect a Slack workspace first, so the approver can be looked up in it")
					}
					prop.Changes = []proposalChange{{Key: "member", Label: "Approver", From: "—", To: p.Member}}
					prop.Steps = []proposalStep{{Method: "POST", Path: base + "/members",
						Body:  map[string]any{"ref": p.Member, "proposal_id": prop.ID},
						Label: "Add " + p.Member + " to " + role.Name}}
					break
				}
				if !tierHasMember(role, p.Member) {
					return "", fmt.Errorf("%s is not a member of %s", p.Member, role.Name)
				}
				prop.Changes = []proposalChange{{Key: "member", Label: "Approver", From: p.Member, To: "—"}}
				prop.Steps = []proposalStep{{Method: "DELETE", Path: base + "/members?ref=" + url.QueryEscape(p.Member),
					Label: "Remove " + p.Member + " from " + role.Name}}

			case "attach_bundle", "detach_bundle":
				bd, err := b.consoleBundle(ctx, c, p.BundleID)
				if err != nil {
					return "", err
				}
				on := p.Action == "attach_bundle"
				prop.Changes = []proposalChange{grantChange("Bundle", bd.Name, on)}
				prop.Steps = []proposalStep{{Method: methodFor(on), Path: fmt.Sprintf("%s/bundles/%d", base, bd.ID),
					Label: verbFor(on) + " " + bd.Name + " " + prepFor(on) + " " + role.Name}}

			case "attach_connection", "detach_connection":
				conn, err := b.consoleConnection(ctx, c, p.ConnectionID)
				if err != nil {
					return "", err
				}
				on := p.Action == "attach_connection"
				prop.Changes = []proposalChange{grantChange("Connection", conn.Name, on)}
				prop.Steps = []proposalStep{{Method: methodFor(on), Path: fmt.Sprintf("%s/connections/%d", base, conn.ID),
					Label: verbFor(on) + " " + conn.Name + " " + prepFor(on) + " " + role.Name}}

			default:
				return "", fmt.Errorf("%q is not something that can be proposed; use one of: %s", p.Action, strings.Join(approvalActions, ", "))
			}

			if err := c.stage(prop); err != nil {
				return "", err
			}
			return fmt.Sprintf("Staged for %s. NOTHING HAS CHANGED YET — the person must press Confirm on the card.", role.Name), nil
		},
	}
}

func methodFor(on bool) string {
	if on {
		return "POST"
	}
	return "DELETE"
}

func verbFor(on bool) string {
	if on {
		return "Attach"
	}
	return "Detach"
}

func prepFor(on bool) string {
	if on {
		return "to"
	}
	return "from"
}

func grantChange(label, name string, on bool) proposalChange {
	if on {
		return proposalChange{Key: strings.ToLower(label), Label: label, From: "—", To: name}
	}
	return proposalChange{Key: strings.ToLower(label), Label: label, From: name, To: "—"}
}

func tierHasMember(r *ApprovalRole, ref string) bool {
	for _, m := range r.Members {
		if strings.EqualFold(strings.TrimSpace(m), strings.TrimSpace(ref)) {
			return true
		}
	}
	for _, m := range r.Resolved {
		if strings.EqualFold(m.Ref, ref) || strings.EqualFold(m.SlackUserID, ref) {
			return true
		}
	}
	return false
}

// ---- resolving, always inside the organisation ----

// consoleScope resolves a channel id inside the caller's organisation. Every tool goes through
// here rather than trusting the id it was handed: the model composes that string, sometimes
// from a page the person is looking at and sometimes from its own memory of the conversation,
// and a channel id is only unique within a workspace anyway.
func (b *Bot) consoleScope(ctx context.Context, c *consoleCall, channel string) (*Scope, error) {
	channel = strings.TrimSpace(channel)
	if channel == "" {
		return nil, errors.New("name a channel; read the channels resource if you need its id")
	}
	scopes, err := b.store.Scopes(ctx, c.OrgID)
	if err != nil {
		return nil, err
	}
	for _, s := range scopes {
		if s.Kind == "channel" && s.SlackID == channel {
			// The store leaves TeamName empty — the API layer fills it — and a workspace
			// reported to the model as its raw id is one nobody can talk about by name.
			s.TeamName = b.teamName(ctx, s.TeamID)
			return s, nil
		}
	}
	return nil, fmt.Errorf("no channel %s in this organisation", channel)
}

func (b *Bot) consoleTier(ctx context.Context, c *consoleCall, id int64) (*ApprovalRole, error) {
	if id == 0 {
		return nil, errors.New("give the tier's id; read the approval_tiers resource for it")
	}
	role, err := b.store.ApprovalRole(ctx, c.OrgID, id)
	if err != nil || role == nil {
		return nil, fmt.Errorf("no approval tier %d in this organisation", id)
	}
	return role, nil
}

func (b *Bot) consoleBundle(ctx context.Context, c *consoleCall, id int64) (*Bundle, error) {
	if id == 0 {
		return nil, errors.New("give the bundle's id; read the bundles resource for it")
	}
	bd, err := b.store.Bundle(ctx, c.OrgID, id)
	if err != nil || bd == nil {
		return nil, fmt.Errorf("no bundle %d in this organisation", id)
	}
	return bd, nil
}

func (b *Bot) consoleConnection(ctx context.Context, c *consoleCall, id int64) (*Connection, error) {
	if id == 0 {
		return nil, errors.New("give the connection's id")
	}
	conn, err := b.store.Connection(ctx, c.OrgID, id)
	if err != nil || conn == nil {
		return nil, fmt.Errorf("no connection %d in this organisation", id)
	}
	return conn, nil
}

// scopeSettingsMap is a scope's own fields under the names PUT /api/scopes/{id} takes, so a
// proposal's "from" is spelled the same way as its "to" and the card reads as one thing.
func scopeSettingsMap(sc *Scope) map[string]string {
	budget := ""
	if sc.MonthlyBudgetUSD > 0 {
		budget = strconv.FormatFloat(sc.MonthlyBudgetUSD, 'f', -1, 64)
	}
	rounds := ""
	if sc.MaxToolRounds > 0 {
		rounds = strconv.Itoa(sc.MaxToolRounds)
	}
	rules, _ := json.Marshal(sc.AllowRules)
	return map[string]string{
		"instructions": sc.Instructions, "default_model": sc.DefaultModel,
		"member_edits": orInherit(sc.MemberEdits), "read_all": orInherit(sc.ReadAll),
		"email_intake": orInherit(sc.EmailIntake), "email_auto_writes": orInherit(sc.EmailAutoWrites),
		"monthly_budget_usd": budget, "max_tool_rounds": rounds,
		"allow_rules": string(rules), "default_repo": sc.DefaultRepo,
	}
}

func orInherit(s string) string {
	if strings.TrimSpace(s) == "" {
		return "inherit"
	}
	return s
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func jsonResult(v any) string {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("could not render: %v", err)
	}
	return string(out)
}

// ---- the prompt ----

// The stable half only. The page, the channel and the attached files go in the user message
// instead: they change every turn, and the cache breakpoint is on the system message — moving that
// prefix each turn is how a cache stops being one. So does what holds on one page only
// (focusKind.prompt): it rides with that page's line, and this stays the same bytes on every page.
// runConsoleTurn sets the breakpoint itself (CachedSystemMessage) rather than leave it to withCache,
// which passes over a system message shorter than minCacheChars, as this one is: the entry is worth
// writing for the tool definitions in front of it, which this length says nothing about.
const assistantPrompt = `You are the assistant inside the attest_tag admin console, helping one organisation's admin.

You are talking to a console admin looking at a web page, not to anyone in Slack. Nothing you say reaches a Slack channel.

PAGE CONTEXT
A question may be preceded by a line naming the console page the person is on and, when the page had something selected, what it was showing, such as a channel. Treat that as the one they mean and do not ask which. If several such lines appear in a conversation, the most recent is where they are now. The id in that line is a hint for you; every tool resolves what it is given inside this organisation, so a tool result is the truth and the line is not. A page with tools of its own adds what holds there after that line; follow it on that page.

YOU CANNOT CHANGE ANYTHING
The propose_ tools write nothing. Each checks a change and hands the person a card with a Confirm button; the change happens only if they press it. A staged card is not done: say what the card proposes and why, say plainly that nothing changes until they press Confirm, and stop.
Never open with "Done", never write "I've added", "I've changed" or "I've set", and never say something has been changed, updated, saved or applied — you did not do it, and you have no way to know whether they will.
If a proposal is refused, the reason you are given is the reason the save itself would have given. Repeat it rather than trying a different value.
An earlier reply may end with [proposal "…": confirmed], [… not confirmed] or [… changed before Confirm]: that is the console telling you what the person did.

WHAT YOU CAN SEE
read_console lists exactly what this person may read; it changes with their permissions, so do not assume. If a resource is not in that list they cannot read it here, and neither can you — say so and name the page it lives on rather than guessing.
search_guide is this product's own documentation, and it is where every "how do I set this up", "how does this work" and "what does it cost" answer lives. Search it before answering one. You do not know this product from training, and an answer composed out of how such products usually work will be confident, specific and wrong about exactly the details somebody then goes and tries — which permission a token needs, whether a field is required, whether a prefix matches as a wildcard.
When a question is about setting something up, give the whole shape: the credential, the connection and its path prefixes and methods, the tier, and what then happens when somebody asks. Quote the guide's own words for anything exact.
You cannot see anything in Slack itself: messages, channel membership, who is online. You cannot read the server's log files either — the activity, tool_calls, audit and spend resources are the log, and unlike a file they are narrowed to this organisation.
Some things are read-only for you even when you can see them: creating or deleting documents, routines, jobs, bundles and connections all happen on their own pages. Deleting an approval tier is one of these. Say where, and do not offer.

HOW TO ANSWER
Read before you answer: a number or a setting you did not get from a tool is one you invented.
When a file is attached, use it. It is the person's own file and it is what they are asking about.
Be short. Markdown, no emoji, British spelling.`

// ---- the loop ----

// runConsoleTurn runs one question to an answer. Usage is accumulated onto the call every
// round, before any error return, because the caller logs what was spent from a defer — a turn
// that fails is the expensive one, and the shape where spend is recorded on the way out of a
// successful path is the shape where a budget quietly stops being a budget.
func (b *Bot) runConsoleTurn(ctx context.Context, c *consoleCall, question string, history []assistantMessage) (string, error) {
	l, err := b.consoleLLM(ctx, c)
	if err != nil {
		return "", err
	}
	tools := b.consoleTools(c)
	byName := map[string]consoleTool{}
	defs := make([]openai.ChatCompletionToolUnionParam, 0, len(tools))
	for _, t := range tools {
		byName[t.Name] = t
		defs = append(defs, t.def())
	}

	msgs := []openai.ChatCompletionMessageParamUnion{CachedSystemMessage(assistantPrompt, "")}
	for _, h := range history {
		switch h.Role {
		case "you":
			msgs = append(msgs, openai.UserMessage(h.Text))
		case "assistant":
			msgs = append(msgs, openai.AssistantMessage(h.Text))
		}
	}
	turn, hasImages := c.userMessage(question)
	msgs = append(msgs, turn)
	if hasImages {
		// The same rule a channel turn follows: a model that takes images gets them, and a
		// text-only one gets their file names instead.
		var why string
		why, msgs = imagesFor(ctx, l, c.model, "console", msgs)
		slog.Info("console assistant images", "model", c.model, "why", why)
	}

	for round := 0; round < assistantRounds; round++ {
		c.rounds = round + 1
		send := defs
		if round == assistantRounds-1 {
			// The last round answers with what it has. Ending on "I ran out of rounds" throws
			// away the reads that were already paid for, which is the expensive part.
			send = nil
			msgs = append(msgs, openai.UserMessage(
				"You have no tool calls left. Answer with what you already have, and say what you could not check."))
		}
		resp, us, err := l.Chat(ctx, c.model, msgs, send, "")
		c.usage.add(us)
		if err != nil {
			return "", err
		}
		if len(resp.Choices) == 0 {
			return "", errors.New("the model returned no answer")
		}
		msg := resp.Choices[0].Message
		if len(msg.ToolCalls) == 0 {
			return strings.TrimSpace(msg.Content), nil
		}
		msgs = append(msgs, msg.ToParam())
		for _, tc := range msg.ToolCalls {
			out := b.runConsoleTool(ctx, c, byName, tc.Function.Name, tc.Function.Arguments)
			msgs = append(msgs, openai.ToolMessage(out, tc.ID))
		}
	}
	return "", errors.New("the assistant ran out of tool rounds")
}

// userMessage builds the turn: the page context, the question, and whatever was attached.
// Images ride as their own parts; anything with text is inlined and named, so the model can
// quote it; anything else gets its name and an honest note that its contents were not read.
func (c *consoleCall) userMessage(question string) (openai.ChatCompletionMessageParamUnion, bool) {
	var sb strings.Builder
	sb.WriteString(c.contextLine())
	sb.WriteString(question)

	var parts []openai.ChatCompletionContentPartUnionParam
	hasImage := false
	for _, f := range c.Files {
		switch {
		case f.ImageURL != "":
			hasImage = true
			parts = append(parts, openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{URL: f.ImageURL}))
			fmt.Fprintf(&sb, "\n[attached image: %s]", f.Name)
		case f.Text != "":
			fmt.Fprintf(&sb, "\n\n<file name=%q>\n%s\n</file>", f.Name, redact(f.Text))
		default:
			fmt.Fprintf(&sb, "\n[attached file: %s — %s. Say so rather than guessing what is in it.]", f.Name, f.Note)
		}
	}
	if !hasImage {
		return openai.UserMessage(sb.String()), false
	}
	parts = append([]openai.ChatCompletionContentPartUnionParam{openai.TextContentPart(sb.String())}, parts...)
	return openai.UserMessage(parts), true
}

// contextLine is the prose half of the page context. The scope also rides on the call as a
// resolved row, which is what the tools use — the sentence is so the model knows a channel is
// attached at all. A typed default with nothing said about it produces an assistant that asks
// which channel while looking straight at one; a sentence with no typed default produces one
// that quotes an id back at a tool instead of resolving it. A page focus is said the same way, after
// the channel: its line was written from the row it resolved to (resolveFocus). What holds on that
// page and nowhere else follows, in a bracket of its own, so a question asked anywhere else never
// carries it.
func (c *consoleCall) contextLine() string {
	if c.Path == "" && c.Scope == nil && c.Focus == nil {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[On screen: ")
	switch {
	case c.Page != "" && c.Path != "":
		fmt.Fprintf(&sb, "the %s page (%s)", c.Page, c.Path)
	case c.Path != "":
		sb.WriteString("the console page " + c.Path)
	default:
		sb.WriteString("the console")
	}
	if c.Scope != nil {
		fmt.Fprintf(&sb, `, showing the channel #%s (id %s) in %s — this is the channel they mean; do not ask which one`,
			c.Scope.Name, c.Scope.SlackID, c.Scope.TeamName)
	}
	if c.Focus != nil && c.Focus.Line != "" {
		sb.WriteString(", " + c.Focus.Line)
	}
	sb.WriteString("]\n")
	if c.Focus != nil && c.Focus.Prompt != "" {
		sb.WriteString("[On this page:\n" + c.Focus.Prompt + "]\n")
	}
	sb.WriteString("\n")
	return sb.String()
}

// runConsoleTool runs one call and records it, both for the panel and for Activity. An error
// comes back to the model as text rather than ending the turn: a refused proposal is the most
// common one, and the model's job on that turn is to repeat the reason.
func (b *Bot) runConsoleTool(ctx context.Context, c *consoleCall, byName map[string]consoleTool, name, rawArgs string) string {
	args := json.RawMessage(rawArgs)
	if !json.Valid(args) {
		args = json.RawMessage("{}")
	}
	t, ok := byName[name]
	if !ok {
		// Including a tool this caller was not offered: the model cannot tell a withheld tool
		// from one that does not exist, and neither answer should end the turn.
		return "error: unknown tool"
	}
	start := time.Now()
	tctx, cancel := context.WithTimeout(ctx, assistantToolWall)
	out, err := t.Run(tctx, c, args)
	cancel()
	ms := time.Since(start).Milliseconds()
	if err != nil {
		out = "error: " + err.Error()
	}
	result := consoleCut(truncateToolOutput(redact(out)), cmp.Or(c.resultCap, assistantResultC), c.resultMore)
	c.resultCap, c.resultMore = 0, ""
	logArgs, _ := cutRunes(string(args), 2000)
	// The assistant runs as an admin, and its reads reach what that admin may see — the audit log,
	// the settings, the connections — none of which a plain activity.view holder is allowed to read
	// directly. tool_calls is rendered on /activity to activity.view, so the result is kept out of
	// it: the row records that the assistant ran a tool and how it went, not what came back. The
	// person who ran it still sees the full result in the assistant panel (c.calls, below) and in
	// their own turn history. The mark is what makes unmarkPrivate lay the row out as private.
	//
	// The arguments are kept, with what a reader must hold to be shown them (consoleArgNeeds), and the
	// console's readers of the log withhold them from anybody else (unmarkPrivateFor). They are the
	// model's, written from the question and from what it read before: one round's read of the audit log
	// can be quoted in the next round's arguments as readily as in the reply.
	loggedArgs := markPrivate(consoleLoggedArgs{Needs: consoleArgNeeds(t, args), Args: logArgs})
	loggedResult := markPrivate(struct {
		Bytes int  `json:"bytes"`
		OK    bool `json:"ok"`
	}{Bytes: len(result), OK: err == nil})
	b.store.LogToolCall(ctx, c.OrgID, "", assistantChannel, "", name, loggedArgs, loggedResult, err == nil, ms)
	c.calls = append(c.calls, assistantTool{Name: name, Args: logArgs, Result: result, OK: err == nil, MS: ms})
	return result
}

// consoleArgNeeds is every permission a reader of the log must hold to be shown a call's arguments:
// what the tool's own arguments are about (consoleTool.needs), and audit.view, which is what the
// question and the reply of the same turn are shown for (GET /api/assistant/turns) — the arguments are
// written from both, so they are no more anybody's to read than those are.
func consoleArgNeeds(t consoleTool, args json.RawMessage) []string {
	need := []string{PermAuditView}
	own := []string{t.Perm}
	if t.needs != nil {
		own = t.needs(args)
	}
	for _, p := range own {
		if p != "" && !slices.Contains(need, p) {
			need = append(need, p)
		}
	}
	slices.Sort(need)
	return need
}

// consoleCutMore is what a cut result says to ask for when its reader has nothing more particular
// to say (consoleReader.more).
const consoleCutMore = "ask for fewer rows, a narrower filter, or one by its id"

// consoleCut keeps a tool result within limit bytes and, when it had to cut, says so at its end. The
// cut used to be silent: a list that ran past the cap just stopped, and a model counting what it
// could see answered short by everything it could not, with nothing to tell it so. The mark is
// inside the limit, so the limit is what reaches the model; and it is counted in bytes, as cutRunes
// counts, since a line full of "·" and "—" is longer in bytes than in characters.
func consoleCut(s string, limit int, more string) string {
	if len(s) <= limit {
		return s
	}
	mark := "\n…[cut — " + cmp.Or(more, consoleCutMore) + "]"
	s, _ = cutRunes(s, max(limit-len(mark), 0))
	return s + mark
}

// consoleModel is what a console turn runs on: the organisation's default, with "heavy"
// resolving as it does everywhere else. There is no per-channel override here on purpose — the
// assistant is not in a channel, and reading one channel's model because its page happened to
// be open would make the answer depend on where somebody was standing.
//
// l is the endpoint the turn runs on, whose default stands in for anything it does not serve.
func (b *Bot) consoleModel(ctx context.Context, orgID int64, l *LLM) string {
	st := b.settings.Get(ctx, orgID)
	switch m := strings.TrimSpace(st.Model); m {
	case "":
		return l.Model
	case "heavy":
		// The sentinel resolves through Settings, and falls back to the endpoint's own default
		// rather than being sent to a provider that has never heard of a model called "heavy".
		if st.HeavyModel != "" {
			return l.Servable(ctx, st.HeavyModel)
		}
		return l.Model
	default:
		return l.Servable(ctx, m)
	}
}

func (b *Bot) logConsoleSpend(ctx context.Context, c *consoleCall) {
	if c.usage.In == 0 && c.usage.Out == 0 {
		return
	}
	b.store.LogUsageBy(ctx, c.OrgID, "", assistantChannel, "", c.Actor, c.model, c.usage)
	slog.Info("console assistant", "org", c.OrgID, "model", c.model, "rounds", c.rounds,
		"in", c.usage.In, "out", c.usage.Out, "cost_usd", fmt.Sprintf("%.5f", c.usage.CostUSD),
		"files", len(c.Files), "proposals", len(c.proposals))
}
