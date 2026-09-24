package app

// The console's side of an organisation's own model key (model_keys.go): Settings → Models, for
// whoever holds settings.manage. Four routes, and the key goes in through one of them and never
// comes out of any: what is read back is the host, the last four characters and a fingerprint.
//
// Three rules shape the handlers:
//
//   - A key is checked against its endpoint before it is stored. Every model call the organisation
//     makes goes there the moment it is saved, so a typo in the address or the model is an outage;
//     a real completion (and an embedding, when there is an embedding model) has to come back first.
//   - A stored key is only ever sent to the address it was saved for. Moving to another address, or
//     testing one, needs the key pasted again — otherwise "test this URL" with the key field left
//     empty would hand the stored key to whatever server the URL names.
//   - A new key or a new address decides where every conversation in the organisation is sent, and
//     on whose account. That is what somebody holding a stolen session would change, so it asks for
//     the same proof as turning two-factor off (proveIdentity), is audited, and is mailed to every
//     member who could have made it.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
)

// modelKeyTests bounds the Test button: each press is a request from this server to an address
// the member typed.
var modelKeyTests = newRateLimiter()

const modelKeyTestsPerHour = 30

// Test hooks: the address check and the transport a candidate endpoint is reached on. Tests swap in
// ones that reach a server on loopback, which the real ones refuse by design.
var (
	checkModelBaseURL   = validModelBaseURL
	modelProbeTransport = orgTransport
)

// modelKeyPresets are the forms the console offers. The request shape is decided by the host
// (dialectFor), not by which of these was picked; the preset is only which form to draw.
var modelKeyPresets = []modelKeyPreset{
	{ID: "openai", Name: "OpenAI", BaseURL: "https://api.openai.com/v1",
		Hint: "A key from platform.openai.com/api-keys. A project key keeps this organisation's usage separate from anything else on the account."},
	{ID: "openrouter", Name: "OpenRouter", BaseURL: "https://openrouter.ai/api/v1",
		Hint: "A key from openrouter.ai/settings/keys, on your own OpenRouter account."},
	{ID: "compatible", Name: "Another OpenAI-compatible endpoint",
		Hint: "Any https endpoint that speaks the Chat Completions API — Azure OpenAI's v1 endpoint is https://<resource>.openai.azure.com/openai/v1, and its models are your deployment names."},
}

type modelKeyPreset struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	Hint    string `json:"hint"`
}

func presetOf(id string) string {
	for _, p := range modelKeyPresets {
		if p.ID == id {
			return id
		}
	}
	return "compatible"
}

func (b *Bot) modelKeyRoutes(mux *http.ServeMux) {
	modelKeyTests.shareAcross(b.store)
	mux.HandleFunc("GET /api/settings/model-key", b.requirePerm(PermSettingsManage, b.handleModelKeyGet))
	mux.HandleFunc("PUT /api/settings/model-key", b.requirePerm(PermSettingsManage, b.handleModelKeyPut))
	mux.HandleFunc("DELETE /api/settings/model-key", b.requirePerm(PermSettingsManage, b.handleModelKeyDelete))
	mux.HandleFunc("POST /api/settings/model-key/test", b.requirePerm(PermSettingsManage, b.handleModelKeyTest))
}

// modelKeyView is everything the console is told about the key.
type modelKeyView struct {
	// Allowed: this organisation may bring a key (ORG_MODEL_KEYS and its plan). Policy and Plan
	// are why not, so the page can say so rather than draw a form that would refuse.
	Allowed bool   `json:"allowed"`
	Policy  string `json:"policy"`
	Plan    string `json:"plan"`
	// Key is the stored endpoint, never the key itself; nil when there is none.
	Key     *ModelKeyRef `json:"key"`
	Active  bool         `json:"active"`
	Failing bool         `json:"failing"`
	// Refusal is why a stored key is not in use, in the words a turn would be refused with.
	Refusal string `json:"refusal,omitempty"`
	// Proof is what saving a new key or address will ask this member for: password, code or
	// recent (a session under ten minutes old) — the three answers proveIdentity accepts.
	Proof string `json:"proof"`
	// MonthlyBudgetUSD is the organisation's own budget, the one limit left on its own key;
	// Priced says whether calls there carry a figure it can be measured in.
	MonthlyBudgetUSD float64          `json:"monthly_budget_usd"`
	Priced           bool             `json:"priced"`
	Presets          []modelKeyPreset `json:"presets"`
}

func (b *Bot) modelKeyView(ctx context.Context, orgID int64, me *AdminUser) modelKeyView {
	st := b.settings.Get(ctx, orgID)
	v := modelKeyView{Allowed: b.cfg.ownKeyAllowed(st.Plan), Policy: orgModelKeysOf(b.cfg.OrgModelKeys), Plan: st.Plan,
		Active: st.OwnKey.Active(), MonthlyBudgetUSD: st.MonthlyBudgetUSD, Presets: modelKeyPresets}
	if me != nil {
		v.Proof = b.proofKind(ctx, me)
	}
	if st.OwnKey.Present {
		ref := st.OwnKey.Ref
		v.Key, v.Failing = &ref, ref.Failing()
		// Priced when the provider reports a charge (OpenRouter) or the catalogue lists the model.
		if dialectFor(ref.BaseURL) == dialectOpenRouter {
			v.Priced = true
		} else if b.agent != nil && b.agent.endpoints != nil {
			_, v.Priced = b.agent.endpoints.platform.priceOf(ctx, ref.DefaultModel)
		}
	}
	if err := st.OwnKey.refusal(); err != nil {
		v.Refusal = err.Error()
	}
	return v
}

// ownKeyNotOffered says why this organisation cannot bring a key, for the save and test routes.
func (b *Bot) ownKeyNotOffered() string {
	switch orgModelKeysOf(b.cfg.OrgModelKeys) {
	case OrgModelKeysEnterprise:
		msg := "bringing your own model key is part of the Enterprise plan"
		if b.cfg.SupportEmail != "" {
			msg += "; write to " + b.cfg.SupportEmail + " to talk about it"
		}
		return msg
	}
	return "this deployment does not let organisations bring their own model key"
}

func (b *Bot) handleModelKeyGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, b.modelKeyView(r.Context(), orgOf(r), adminFromCtx(r.Context())))
}

type modelKeyInput struct {
	Preset       string `json:"preset"`
	BaseURL      string `json:"base_url"`
	Key          string `json:"key"` // empty keeps the stored key, at the stored address
	DefaultModel string `json:"default_model"`
	EmbedModel   string `json:"embed_model"`
	FixJobs      *bool  `json:"fix_jobs"` // nil keeps what is stored; a new key starts with them off
	Password     string `json:"password"`
	Code         string `json:"code"`
}

func (in *modelKeyInput) clean() {
	in.BaseURL = strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
	in.Key = strings.TrimSpace(in.Key)
	in.DefaultModel = strings.TrimSpace(in.DefaultModel)
	in.EmbedModel = strings.TrimSpace(in.EmbedModel)
}

// storedKey reads the organisation's endpoint and its key together (ModelKeyRow), for a handler
// that may send the stored key somewhere: what it compares an address with and what it sends are
// then the same row. An unreadable key is reported as no key, with the row, so a save can replace it.
func (b *Bot) storedKey(ctx context.Context, orgID int64) (*ModelKeyRef, string, error) {
	cur, key, err := b.store.ModelKeyRow(ctx, orgID, b.sealer)
	if errors.Is(err, errModelKeyUnreadable) {
		return cur, "", nil
	}
	return cur, key, err
}

// keyFor is the key a request may use against in.BaseURL: the one it carries, or the stored one
// when the address is the one it was stored for — and never the stored one anywhere else.
func keyFor(in modelKeyInput, cur *ModelKeyRef, stored string) (string, error) {
	if in.Key != "" {
		if len(in.Key) > 1000 {
			return "", errors.New("that is too long to be an API key")
		}
		return in.Key, nil
	}
	if cur == nil {
		return "", errors.New("paste the key")
	}
	if cur.BaseURL != in.BaseURL {
		return "", errors.New("paste the key again to use it at a new address: a stored key is only ever sent where it was saved for")
	}
	if stored == "" {
		return "", errors.New("the stored key could not be read; paste it again")
	}
	return stored, nil
}

func (b *Bot) handleModelKeyPut(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID, me := orgOf(r), adminFromCtx(r.Context())
	var in modelKeyInput
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	in.clean()
	st := b.settings.Get(ctx, orgID)
	if !b.cfg.ownKeyAllowed(st.Plan) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": b.ownKeyNotOffered()})
		return
	}
	if err := checkModelBaseURL(in.BaseURL); err != nil {
		bad(w, err)
		return
	}
	if in.DefaultModel == "" {
		bad(w, errors.New("choose the model the bot answers with"))
		return
	}
	cur, stored, err := b.storedKey(ctx, orgID)
	if err != nil {
		fail(w, err)
		return
	}
	key, err := keyFor(in, cur, stored)
	if err != nil {
		bad(w, err)
		return
	}
	// A new key starts with fix jobs OFF: a job runs the repository's own code with the key in its
	// environment, so exposing the key that way is an opt-in an admin makes deliberately, not a
	// default they inherit by saving a key for chat.
	fixJobs := false
	if cur != nil {
		fixJobs = cur.FixJobs
	}
	if in.FixJobs != nil {
		fixJobs = *in.FixJobs
	}
	moved := cur == nil || in.Key != "" || cur.BaseURL != in.BaseURL
	// Turning fix jobs on widens where the key can be read, so it is held to the same identity
	// proof as changing the key or its address — a stolen session must not be able to do it alone.
	enablingJobs := fixJobs && (cur == nil || !cur.FixJobs)
	if moved || enablingJobs {
		if ok, why := b.proveIdentity(ctx, me, in.Password, in.Code); !ok {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": why, "proof": b.proofKind(ctx, me)})
			return
		}
	}
	if err := b.probeModelEndpoint(ctx, in.BaseURL, key, in.DefaultModel, in.EmbedModel); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "nothing was saved: " + err.Error()})
		return
	}
	ref := ModelKeyRef{Preset: presetOf(in.Preset), BaseURL: in.BaseURL, DefaultModel: in.DefaultModel,
		EmbedModel: in.EmbedModel, FixJobs: fixJobs}
	if in.Key == "" {
		ref.KeyFP = cur.KeyFP // the row this save was made against; another save since refuses it
	}
	if err := b.store.PutModelKey(ctx, orgID, ref, in.Key, me.Email, b.sealer); err != nil {
		if errors.Is(err, errModelKeyChanged) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
			return
		}
		fail(w, err)
		return
	}
	b.changed(ctx, orgID)
	saved, _ := b.store.ModelKeyRef(ctx, orgID)
	if saved == nil {
		fail(w, errors.New("the key was saved and could not be read back"))
		return
	}
	if embedIdentity(cur) != embedIdentity(saved) {
		b.reindexLater(orgID)
	}
	b.audit(r, "model_key.saved", AuditEvent{TargetKind: "model_key", TargetName: saved.Host(), Details: auditDetails(map[string]any{
		"preset": saved.Preset, "host": saved.Host(), "key_hint": saved.KeyHint, "key_fp": saved.KeyFP,
		"new_key": in.Key != "", "moved": moved, "default_model": saved.DefaultModel, "embed_model": saved.EmbedModel,
		"fix_jobs": saved.FixJobs})})
	if moved {
		b.mailModelKeyChange(ctx, orgID, me, saved)
	}
	writeJSON(w, 200, b.modelKeyView(ctx, orgID, me))
}

// handleModelKeyDelete removes the key. It asks for the same proof a save does: removing the key
// moves every conversation too — to this deployment's provider, which for an organisation that
// brought a key to keep its data in one place is exactly the change it brought it to prevent.
func (b *Bot) handleModelKeyDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID, me := orgOf(r), adminFromCtx(r.Context())
	var in struct{ Password, Code string }
	if r.ContentLength != 0 {
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
	}
	cur, _ := b.store.ModelKeyRef(ctx, orgID)
	if cur != nil {
		if ok, why := b.proveIdentity(ctx, me, in.Password, in.Code); !ok {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": why, "proof": b.proofKind(ctx, me)})
			return
		}
	}
	removed, err := b.store.DeleteModelKey(ctx, orgID)
	if err != nil {
		fail(w, err)
		return
	}
	if removed {
		b.changed(ctx, orgID)
		if embedIdentity(cur) != "" {
			b.reindexLater(orgID)
		}
		details := map[string]any{}
		if cur != nil {
			details = map[string]any{"host": cur.Host(), "key_hint": cur.KeyHint, "key_fp": cur.KeyFP}
		}
		b.audit(r, "model_key.removed", AuditEvent{TargetKind: "model_key", Details: auditDetails(details)})
		b.mailModelKeyChange(ctx, orgID, me, nil)
	}
	writeJSON(w, 200, b.modelKeyView(ctx, orgID, me))
}

// handleModelKeyTest lists what an endpoint serves on a key, for the model pickers — the key
// typed, or the stored one at its own address. It stores nothing, and it never records a status:
// a key being tried is not the key the organisation is using.
func (b *Bot) handleModelKeyTest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := orgOf(r)
	var in modelKeyInput
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	in.clean()
	if !b.cfg.ownKeyAllowed(b.settings.Get(ctx, orgID).Plan) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": b.ownKeyNotOffered()})
		return
	}
	if ok, wait := modelKeyTests.allow("model-key-test:"+strconv.FormatInt(orgID, 10), modelKeyTestsPerHour, time.Hour); !ok {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": fmt.Sprintf("that is a lot of tests for one hour; try again in %d minutes", int(wait.Minutes())+1)})
		return
	}
	if err := checkModelBaseURL(in.BaseURL); err != nil {
		bad(w, err)
		return
	}
	cur, stored, err := b.storedKey(ctx, orgID)
	if err != nil {
		fail(w, err)
		return
	}
	key, err := keyFor(in, cur, stored)
	if err != nil {
		bad(w, err)
		return
	}
	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	l := b.candidateLLM(in.BaseURL, key, "", "")
	models, err := l.ListModels(tctx, true)
	if err == nil && l.dialect == dialectOpenRouter {
		// OpenRouter lists its models to anybody, so the list proves nothing about the key; its
		// key route does.
		var info map[string]any
		err = l.client.Get(tctx, "key", nil, &info)
	}
	if err != nil {
		if me := classifyModelError(l.host, "", err); me != nil {
			err = me
		}
		writeJSON(w, 200, map[string]any{"ok": false, "detail": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "detail": fmt.Sprintf("%s answered with %d models", l.host, len(models)), "models": models})
}

// candidateLLM is a client for an endpoint that is not stored yet: the one being tested or
// saved. Its errors are explained like an organisation's own, and nothing it does is recorded.
func (b *Bot) candidateLLM(baseURL, key, model, embedModel string) *LLM {
	l := newLLM(b.cfg, endpoint{BaseURL: baseURL, Key: key, Dialect: dialectFor(baseURL), Model: model, EmbedModel: embedModel,
		HTTPClient: &http.Client{Transport: modelProbeTransport(), CheckRedirect: rejectRedirect}})
	l.own = true
	return l
}

// probeModelEndpoint is the check before a save: one real completion on the chosen model, and one
// embedding when there is an embedding model. A few tokens on the organisation's own key, spent so
// that a wrong address, key or model is a refused save rather than a silent organisation.
func (b *Bot) probeModelEndpoint(ctx context.Context, baseURL, key, model, embedModel string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	l := b.candidateLLM(baseURL, key, model, embedModel)
	if _, _, err := l.Chat(ctx, "", []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage("You are being checked for availability. Reply with the single word OK."),
		openai.UserMessage("ping"),
	}, nil, ""); err != nil {
		return err
	}
	if embedModel != "" {
		if _, err := l.Embed(ctx, []string{"ping"}); err != nil {
			return fmt.Errorf("the embedding model: %w", err)
		}
	}
	return nil
}

// embedIdentity is how documents are embedded under a stored endpoint — Indexer.embedID's answer
// for it — so a save can tell whether the organisation's documents need embedding again.
func embedIdentity(ref *ModelKeyRef) string {
	if ref == nil {
		return ""
	}
	return ref.Host() + "|" + ref.EmbedModel
}

// reindexLater embeds an organisation's documents again for the endpoint it now uses, off the
// request: a corpus takes minutes, and a search in the meantime says so (errReindexing).
func (b *Bot) reindexLater(orgID int64) {
	if b.ix == nil || b.docs == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		b.reindex(ctx, orgID)
	}()
}

// mailModelKeyChange tells every member who could have made the change that it was made. A new
// key or address sends every conversation in the organisation somewhere else, and the person who
// did it may not be the person the members think; the audit log records it, and this is how
// anybody finds out without reading one. saved is nil for a removal.
func (b *Bot) mailModelKeyChange(ctx context.Context, orgID int64, by *AdminUser, saved *ModelKeyRef) {
	if b.mail == nil {
		return
	}
	org, _ := b.store.Org(ctx, orgID)
	name := "your organisation"
	if org != nil && org.Name != "" {
		name = org.Name
	}
	who := "Somebody"
	if by != nil {
		who = orDefault(by.Name, by.Email)
	}
	var subject, body string
	if saved != nil {
		subject = "The model key for " + name + " was changed"
		body = fmt.Sprintf("%s saved a model key for %s: %s, key ending %s.\n\nEvery conversation the bot has in %s is now sent to %s, on that key.",
			who, name, saved.Host(), strings.TrimPrefix(saved.KeyHint, "…"), name, saved.Host())
	} else {
		subject = "The model key for " + name + " was removed"
		body = fmt.Sprintf("%s removed %s's own model key. The bot now answers on the models this service includes.", who, name)
	}
	if base := publicBaseURL(ctx, b.store, b.cfg); base != "" {
		body += "\n\nIf you did not expect this, look at Settings → Models: " + base + "/admin/settings/?tab=models"
	}
	members, err := b.store.MembersOf(ctx, orgID)
	if err != nil {
		slog.Warn("model key change not mailed", "org", orgID, "err", err)
		return
	}
	custom := b.store.CustomRoleMap(ctx, orgID)
	for _, m := range members {
		if m.Email == "" || !permissionsForRole(m.Role, custom)[PermSettingsManage] {
			continue
		}
		if err := b.mail.Send(ctx, Mail{To: m.Email, Subject: subject, Body: body}); err != nil {
			slog.Warn("model key change not mailed", "org", orgID, "err", err)
		}
	}
}
