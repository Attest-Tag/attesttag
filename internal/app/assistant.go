package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
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
	Files []consoleAttachment

	model     string
	llm       *LLM // the endpoint it runs on, resolved once (consoleLLM)
	rounds    int
	usage     Usage
	calls     []assistantTool
	proposals []proposal
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
}

func (t consoleTool) def() openai.ChatCompletionToolUnionParam {
	return openai.ChatCompletionFunctionTool(openai.FunctionDefinitionParam{
		Name: t.Name, Description: openai.String(t.Desc), Parameters: openai.FunctionParameters(t.Params),
	})
}

// ---- proposals ----

// proposalChange is one field as a person reads it on the card.
type proposalChange struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	From  string `json:"from"`
	To    string `json:"to"`
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
	ID      string           `json:"id"`
	Kind    string           `json:"kind"`   // "channel" | "approval"
	Target  string           `json:"target"` // what it is about, in words: "#ops", "Senior approvers"
	Changes []proposalChange `json:"changes"`
	Steps   []proposalStep   `json:"steps"`

	auditTeam, auditKind, auditID string
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
func (c *consoleCall) stage(p proposal) error {
	for _, s := range p.Steps {
		if !stepAllowed(s.Method, s.Path) {
			return fmt.Errorf("internal: %s %s is not a step the assistant may stage", s.Method, s.Path)
		}
	}
	c.proposals = append(c.proposals, p)
	return nil
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
}

func (b *Bot) consoleReaders() []consoleReader {
	return []consoleReader{
		{"channels", "", "every channel this organisation has configured", func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
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
		{"channel", "", "one channel in full: its settings, what each field inherits, and what it can reach (id = the channel id)", func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
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
		{"approval_tiers", "", "the tiers that can approve access: rank, members, and what each may grant", func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
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
		{"bundles", PermConnView, "the named groups of connections a channel or a tier can be given", func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
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
		{"models", "", "the models this provider offers, and which one the organisation uses", func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
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
		{"access_requests", PermAccessView, "who asked for what, who answered, and what ran", func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
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
		{"activity", PermActivityView, "recent turns the bot has taken", func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
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
		{"audit", PermAuditView, "who signed in, what they changed, and what they approved", func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
			evs, err := b.store.AuditEvents(ctx, c.OrgID, AuditFilter{Action: id, Limit: limit})
			if err != nil {
				return "", err
			}
			var out []string
			for _, e := range evs {
				out = append(out, fmt.Sprintf("- %s %s by %s (%s) on %s %s", e.At, e.Action,
					orDash(e.ActorName), orDash(e.Via), orDash(e.TargetKind), orDash(e.TargetName)))
			}
			return listOr(out, "no audit events match"), nil
		}},
		{"documents", "", "the files the bot can search when it answers", func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
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
		{"routines", PermRoutinesManage, "prompts that run on a schedule and post to a channel", func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
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
		{"jobs", PermJobsView, "fix jobs the bot handed to a worker", func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
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
		{"artifacts", PermArtifactsView, "files the bot made and posted in Slack", func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
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
		{"settings", PermSettingsManage, "the organisation's models, budget and limits", func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
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
		{"overview", "", "spend, usage and what the bot has to work with", func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
			return jsonResult(b.store.OverviewStats(ctx, c.OrgID)), nil
		}},
		{"spend", "", "this month's cost, broken down by workspace and channel", func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
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
		{"tool_calls", PermActivityView, "every call the bot has made, with arguments and results — the log to run an analysis over", func(ctx context.Context, b *Bot, c *consoleCall, id string, limit int) (string, error) {
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
				out = append(out, fmt.Sprintf("- %s %s in %s (%s, %dms) args=%s result=%s",
					t.At, t.Name, orDash(t.Channel), state, t.MS,
					truncate(oneLine(t.Args), 200), truncate(oneLine(t.Result), 300)))
			}
			return listOr(out, "no tool calls match"), nil
		}},
	}
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
					return rd.run(ctx, b, c, strings.TrimSpace(p.ID), p.Limit)
				}
			}
			// Naming what IS available beats "unknown resource": the model asked for a page it
			// cannot see, and the useful answer is which ones it can.
			return "", fmt.Errorf("%q is not something you can read; you have: %s", p.Resource, strings.Join(names, ", "))
		},
	}}

	// The guide needs no permission: it is this product's public documentation, and the question
	// it answers — "how would I set this up" — is the one somebody without any permissions is
	// most likely to be asking.
	tools = append(tools, consoleTool{
		Name: "search_guide",
		Desc: "Search attest_tag's own documentation: how connections, path prefixes, methods and access grants are configured, " +
			"what an approval tier grants, how confirmed writes work, plans and what they cost, deployment and security. " +
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

	if c.Perms[PermScopesManage] {
		tools = append(tools, b.proposeChannelTool())
	}
	if c.Perms[PermApproversManage] {
		tools = append(tools, b.proposeApprovalTool())
	}
	return tools
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
// instead: they change every turn, and withCache sets its breakpoint on the system message —
// moving that prefix each turn is how a cache stops being one.
const assistantPrompt = `You are the assistant inside the attest_tag admin console, helping one organisation's admin.

You are talking to a console admin looking at a web page, not to anyone in Slack. Nothing you say reaches a Slack channel.

PAGE CONTEXT
A question may be preceded by a line naming the console page the person is on and, when the page had one selected, the channel it was showing. Treat that channel as the one they mean and do not ask which. If several such lines appear in a conversation, the most recent is where they are now. The id in that line is a hint for you; every tool resolves what it is given inside this organisation, so a tool result is the truth and the line is not.

YOU CANNOT CHANGE ANYTHING
The propose_ tools write nothing. Each checks a change and hands the person a card with a Confirm button; the change happens only if they press it. Say what you are proposing and why, say plainly that nothing has changed yet, and stop.
Never say something has been changed, updated, saved or applied — you have no way to know, and you did not do it.
If a proposal is refused, the reason you are given is the reason the save itself would have given. Repeat it rather than trying a different value.

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

	msgs := []openai.ChatCompletionMessageParamUnion{openai.SystemMessage(assistantPrompt)}
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
// that quotes an id back at a tool instead of resolving it.
func (c *consoleCall) contextLine() string {
	if c.Path == "" && c.Scope == nil {
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
	sb.WriteString("]\n\n")
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
	result, _ := cutRunes(truncateToolOutput(redact(out)), assistantResultC)
	logArgs, _ := cutRunes(string(args), 2000)
	b.store.LogToolCall(ctx, c.OrgID, "", assistantChannel, "", name, logArgs, result, err == nil, ms)
	c.calls = append(c.calls, assistantTool{Name: name, Args: logArgs, Result: result, OK: err == nil, MS: ms})
	return result
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
