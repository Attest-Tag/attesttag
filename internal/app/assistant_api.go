package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// POST /api/assistant — one console question, answered.
//
// Stateless per turn on purpose: the browser holds the conversation and sends the last few
// turns back, so the model's context is rebuilt from the page rather than looked up. What that
// costs is rehydration — close the tab and the conversation is gone from the panel. That is the
// right trade for a sidebar whose whole job is answering about the page in front of you.
//
// The transcript is still recorded, in assistant_turns, but as history rather than as state:
// nothing reads it back into a turn, and Activity is its only reader. So a question is
// answerable without it and reviewable because of it.
//
// Two shapes arrive here. JSON is the ordinary turn; multipart/form-data is the same fields
// plus files, which are read into the turn and never stored — an attachment is context for one
// question, not a document the organisation now owns.

// assistantMessage is one earlier turn as the browser holds it. Any role but these two is
// dropped rather than trusted: the history arrives from the page, and a "system" line in it
// would be the one part of the prompt the person asking got to write.
type assistantMessage struct {
	Role string `json:"role"` // "you" | "assistant"
	Text string `json:"text"`
}

type assistantRequest struct {
	Question string             `json:"question"`
	History  []assistantMessage `json:"history"`
	// Path is the console route the question was asked from and Page its title; ScopeID the
	// channel the page had selected. All three are claims by the browser: the first two only
	// ever reach the prompt as text, and the third is re-read inside the organisation before
	// anything is done with it.
	Path    string `json:"path"`
	Page    string `json:"page"`
	ScopeID int64  `json:"scope_id"`
	// Conversation groups a panel's questions together in Activity. The browser mints it and
	// nothing is looked up by it: every row carries its own org_id, so the worst a forged one
	// can do is group somebody's own questions oddly in their own organisation's page.
	Conversation string `json:"conversation"`
	// Model answers this one turn on something other than the organisation's default. It is a
	// choice about this conversation and is not stored: changing what every channel answers on
	// is a settings change, made on the Settings page, by somebody holding settings.manage.
	Model string `json:"model"`
}

// assistantTool is field-identical to playgroundTool so the console renders both with the row
// it already has.
type assistantTool struct {
	Name   string `json:"name"`
	Args   string `json:"args"`
	Result string `json:"result"`
	OK     bool   `json:"ok"`
	MS     int64  `json:"ms"`
}

// assistantFile is one attachment, as the panel shows it back. No contents: the panel has the
// file already, and a data URL round-tripping through the answer would double the response.
type assistantFile struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // "image" | "text" | "skipped"
	Note string `json:"note"`
}

type assistantReply struct {
	Reply  string          `json:"reply"`
	Model  string          `json:"model"`
	Rounds int             `json:"rounds"`
	Tools  []assistantTool `json:"tools"`
	Files  []assistantFile `json:"files"`
	// Proposals are changes staged for a Confirm button. Nothing has been written.
	Proposals []proposal `json:"proposals"`
	TokensIn  int        `json:"tokens_in"`
	TokensOut int        `json:"tokens_out"`
	CostUSD   float64    `json:"cost_usd"`
	// Error is a turn that failed rather than a request that was refused: the tool calls it
	// made are still worth showing, so it rides along with a 200 instead of replacing it.
	Error string `json:"error"`
}

const assistantHistoryChars = 8000

// enterAssistant takes a slot for one organisation, or reports that they are all busy. The
// Agent's own in-flight cap counts runs registered by beginRun, which a console turn never
// calls, so this is its own counter — and it is the limit with the widest blast radius, because
// it is the one protecting every other tenant on a shared instance from one person holding a
// handful of minute-long turns open.
func (b *Bot) enterAssistant(orgID int64) (func(), bool) {
	b.assistantMu.Lock()
	defer b.assistantMu.Unlock()
	if b.assistantRuns == nil {
		b.assistantRuns = map[int64]int{}
	}
	if b.assistantRuns[orgID] >= assistantInFlight {
		return func() {}, false
	}
	b.assistantRuns[orgID]++
	return func() {
		b.assistantMu.Lock()
		defer b.assistantMu.Unlock()
		if b.assistantRuns[orgID] <= 1 {
			delete(b.assistantRuns, orgID)
			return
		}
		b.assistantRuns[orgID]--
	}, true
}

// trimHistory keeps the tail of a conversation within a bound the server sets. The browser is
// the only thing that decides what to send, so the cap is here rather than there.
func trimHistory(in []assistantMessage) []assistantMessage {
	if len(in) > assistantHistory {
		in = in[len(in)-assistantHistory:]
	}
	out := make([]assistantMessage, 0, len(in))
	budget := assistantHistoryChars
	for i := len(in) - 1; i >= 0; i-- {
		m := in[i]
		if m.Role != "you" && m.Role != "assistant" {
			continue
		}
		m.Text = strings.TrimSpace(m.Text)
		if m.Text == "" {
			continue
		}
		if len(m.Text) > budget {
			break
		}
		budget -= len(m.Text)
		out = append([]assistantMessage{m}, out...)
	}
	return out
}

// imageTypes are the ones a model can be shown. Anything else with text in it is inlined as
// text; anything else at all is named and not read.
var imageTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp",
}

// readAttachment reads one uploaded file into the turn. It never fails the request: a file that
// cannot be used comes back as a named skip, because "I could not read that" is an answer and a
// 400 in the middle of a conversation is not.
func readAttachment(fh *multipart.FileHeader) consoleAttachment {
	at := consoleAttachment{Name: filepath.Base(fh.Filename)}
	if fh.Size > assistantMaxBytes {
		at.Note = "too large to read here"
		return at
	}
	f, err := fh.Open()
	if err != nil {
		at.Note = "could not be opened"
		return at
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, assistantMaxBytes))
	if err != nil {
		at.Note = "could not be read"
		return at
	}
	if ct, ok := imageTypes[strings.ToLower(filepath.Ext(at.Name))]; ok {
		at.ImageURL = "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(raw)
		return at
	}
	// Text by inspection rather than by extension: a .log, a .conf and a file with no extension
	// at all are the ones somebody actually drags into a console, and valid UTF-8 with no NULs
	// is what "the model can read this" means in practice.
	if utf8.Valid(raw) && !strings.ContainsRune(string(raw), 0) {
		text := string(raw)
		if len([]rune(text)) > assistantFileText {
			text, _ = cutRunes(text, assistantFileText)
			text += "\n[…truncated]"
		}
		if strings.TrimSpace(text) != "" {
			at.Text = text
			return at
		}
	}
	at.Note = "not a file this assistant can read (attach an image, or a text file)"
	return at
}

// decodeAssistant reads either shape of request. Files only arrive on the multipart one.
func decodeAssistant(r *http.Request) (assistantRequest, []consoleAttachment, error) {
	var req assistantRequest
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return req, nil, decode(r, &req)
	}
	if err := r.ParseMultipartForm(assistantMaxTotal); err != nil {
		return req, nil, err
	}
	req.Question = r.FormValue("question")
	req.Path = r.FormValue("path")
	req.Page = r.FormValue("page")
	req.ScopeID, _ = strconv.ParseInt(r.FormValue("scope_id"), 10, 64)
	req.Conversation = r.FormValue("conversation")
	req.Model = r.FormValue("model")
	if h := strings.TrimSpace(r.FormValue("history")); h != "" {
		json.Unmarshal([]byte(h), &req.History)
	}
	var files []consoleAttachment
	total := int64(0)
	for _, fh := range r.MultipartForm.File["file"] {
		if len(files) >= assistantMaxFiles {
			break
		}
		total += fh.Size
		if total > assistantMaxTotal {
			files = append(files, consoleAttachment{Name: filepath.Base(fh.Filename), Note: "over the total upload limit for one question"})
			break
		}
		files = append(files, readAttachment(fh))
	}
	return req, files, nil
}

func (b *Bot) handleAssistant(w http.ResponseWriter, r *http.Request) {
	req, files, err := decodeAssistant(r)
	if err != nil {
		bad(w, err)
		return
	}
	req.Question = strings.TrimSpace(req.Question)
	if req.Question == "" && len(files) == 0 {
		bad(w, errors.New("nothing to ask"))
		return
	}
	ctx := r.Context()
	orgID := orgOf(r)
	u := adminFromCtx(ctx)
	if u == nil || orgID == 0 {
		writeJSON(w, 401, map[string]any{"error": "sign in required"})
		return
	}

	// What is left after authentication is cost, in the order that spends least to refuse: a
	// count, a counter, then a sum. Nothing here is a permission check — the tools carry those,
	// each matching the permission on the console's own route for the same data.
	if n := b.store.ConsoleTurnsSince(ctx, orgID, u.PublicID, time.Hour); n >= assistantPerHour {
		writeJSON(w, 429, map[string]any{"error": "you've asked the assistant " + strconv.Itoa(n) + " times this hour; try again a bit later"})
		return
	}
	release, ok := b.enterAssistant(orgID)
	if !ok {
		writeJSON(w, 429, map[string]any{"error": "this organisation already has several assistant questions running; try again in a moment"})
		return
	}
	defer release()
	if ok, why := b.agent.budgetOK(ctx, orgID, ""); !ok {
		writeJSON(w, 429, map[string]any{"error": why})
		return
	}
	l, err := b.agent.llmFor(ctx, orgID)
	if err != nil || l == nil {
		msg := "no model endpoint is configured for this deployment"
		if err != nil {
			msg = err.Error()
		}
		writeJSON(w, 503, map[string]any{"error": msg})
		return
	}
	model := b.consoleModel(ctx, orgID, l)
	if picked := strings.TrimSpace(req.Model); picked != "" {
		// On the deployment's own key the choice is bounded to the models an admin has offered the
		// org — the same list a routine picks from — so that a member cannot point the shared key
		// at the dearest model in the catalogue. An org spending its own key may pick anything its
		// endpoint offers, checked against the catalogue the way the picker populated itself.
		st := b.settings.Get(ctx, orgID)
		allowed := routineModelAllowed(st, picked)
		if l.own {
			allowed = b.modelOffered(ctx, orgID, picked)
		}
		if !allowed {
			bad(w, errRoutineModel(picked))
			return
		}
		if picked == "heavy" {
			picked = st.HeavyModel
		}
		model = picked
	}
	if model == "" {
		writeJSON(w, 503, map[string]any{"error": "no model is configured for this deployment"})
		return
	}

	c := &consoleCall{OrgID: orgID, Actor: u.PublicID, Perms: u.Permissions, model: model, llm: l, Files: files}
	c.Path, _ = cutRunes(strings.TrimSpace(req.Path), 120)
	c.Page, _ = cutRunes(strings.TrimSpace(req.Page), 60)
	// The page's channel, re-read inside this organisation. A scope id from another tenant is
	// simply not a channel this request can name, and one naming a workspace rather than a
	// channel is dropped too — the assistant only knows how to talk about channels.
	if req.ScopeID > 0 {
		if sc, err := b.store.ScopeByID(ctx, orgID, req.ScopeID); err == nil && sc != nil && sc.Kind == "channel" {
			sc.TeamName = b.teamName(ctx, sc.TeamID) // the store leaves it empty; see nameTeams
			c.Scope = sc
		}
	}
	// Spend is recorded on the way out of every path, including the ones that failed. A turn
	// that dies on its fourth round has already been charged for three, and a log that only
	// runs after a successful answer is a budget with a hole in exactly the expensive shape.
	// WithoutCancel because a person who closes the tab mid-turn still spent the tokens.
	defer b.logConsoleSpend(context.WithoutCancel(ctx), c)

	runCtx, cancel := context.WithTimeout(ctx, assistantWall)
	defer cancel()
	reply, runErr := b.runConsoleTurn(runCtx, c, req.Question, trimHistory(req.History))

	out := assistantReply{
		Reply: reply, Model: c.model, Rounds: c.rounds,
		Tools: c.calls, Proposals: c.proposals, Files: describeFiles(c.Files),
		TokensIn: c.usage.In, TokensOut: c.usage.Out, CostUSD: c.usage.CostUSD,
	}
	if out.Tools == nil {
		out.Tools = []assistantTool{}
	}
	if out.Proposals == nil {
		out.Proposals = []proposal{}
	}
	if runErr != nil {
		out.Error = runErr.Error()
	}
	// The transcript, so Activity shows what was asked and not only that something was. Written
	// after the turn rather than before it, and best-effort: a question whose record fails to
	// save is still a question that was answered, and failing the answer over the log would be
	// the wrong way round. A failed turn is recorded too — those are the ones worth reading.
	b.store.AddAssistantTurn(context.WithoutCancel(ctx), orgID, AssistantTurn{
		Actor: u.PublicID, ActorName: u.Name, Conversation: conversationOf(req.Conversation),
		Question: req.Question, Reply: reply, Model: c.model, Page: c.Page,
		TokensIn: c.usage.In, TokensOut: c.usage.Out, CostUSD: c.usage.CostUSD,
		ToolCalls: len(c.calls), Proposals: len(c.proposals), Error: out.Error,
	})
	// One row per staged proposal, carrying the diff. The save the person confirms writes its
	// own row carrying the same proposal_id, so the pair reads as what it was: a model
	// suggested this, and then a named person applied it.
	for _, p := range c.proposals {
		changed := map[string]any{}
		for _, ch := range p.Changes {
			if ch.Key == "instructions" {
				changed["instructions_chars"] = len(ch.To)
				continue
			}
			changed[ch.Key], _ = cutRunes(ch.To, 200)
		}
		b.audit(r, "assistant.proposed", AuditEvent{TeamID: p.auditTeam, TargetKind: p.auditKind,
			TargetID: p.auditID, TargetName: p.Target,
			Details: auditDetails(map[string]any{"proposal_id": p.ID, "kind": p.Kind, "changed": changed, "steps": len(p.Steps)})})
	}
	writeJSON(w, 200, out)
}

// conversationOf bounds what the browser may put in the column. It is a grouping key and
// nothing reads it back as an id, so the only thing worth enforcing is that it stays small.
func conversationOf(s string) string {
	out, _ := cutRunes(strings.TrimSpace(s), 64)
	return out
}

func describeFiles(in []consoleAttachment) []assistantFile {
	out := make([]assistantFile, 0, len(in))
	for _, f := range in {
		switch {
		case f.ImageURL != "":
			out = append(out, assistantFile{Name: f.Name, Kind: "image"})
		case f.Text != "":
			out = append(out, assistantFile{Name: f.Name, Kind: "text"})
		default:
			out = append(out, assistantFile{Name: f.Name, Kind: "skipped", Note: f.Note})
		}
	}
	return out
}
