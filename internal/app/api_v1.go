package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"cloud.google.com/go/storage"
)

// The public developer API: /v1, authenticated with a key rather than a session.
//
// It is deliberately a narrow surface over what the console already does — the knowledge the bot
// answers from, what it produced, and what it spent — rather than a second way to configure the
// product. Credentials are the line: a connection's secret cannot be read or written here at any
// permission, because the whole point of holding them is that they leave through the proxy and
// nowhere else.
//
// Every handler reaches its data through the same org-scoped store calls the console uses, and
// the permission each one names is the same constant the console checks. There is one definition
// of "may manage documents" in this codebase, and both doors ask it.

// v1Limit reads ?limit, with a default and a ceiling. A public API cannot let the caller choose
// how much of the database to read in one request.
func v1Limit(r *http.Request, def, max int) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// v1UploadLimit caps one upload request. Cloud Run refuses an HTTP/1 request over 32 MiB before
// it reaches the bot, so on the hosted service this only makes that refusal readable; on a
// deployment of somebody's own it is what stops one request from being read without end.
const v1UploadLimit = 32 << 20

// v1Missing is the single not-found answer. It says the same thing for a row that does not exist
// and one that belongs to somebody else, because a key must not be able to tell those apart.
func v1Missing(w http.ResponseWriter, what string) {
	writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such " + what})
}

// apiV1Routes registers the public API.
func (b *Bot) apiV1Routes(mux *http.ServeMux) {
	// Named so each route reads as "key, then permission": k for anything a member of the
	// organisation may read, kp where the console demands a permission for the same thing.
	k, kp := b.requireKey, b.requireKeyPerm

	// Who this key is, resolved fresh. The first call anybody makes, and the one that answers
	// "why am I getting a 403" without a support ticket.
	mux.HandleFunc("GET /v1/whoami", k(func(w http.ResponseWriter, r *http.Request) {
		u := adminFromCtx(r.Context())
		writeJSON(w, 200, map[string]any{
			"organisation": map[string]any{"id": u.OrgPublic, "name": u.OrgName, "slug": u.OrgSlug},
			"account":      map[string]any{"id": u.PublicID, "email": u.Email, "name": u.Name},
			"role":         u.Role,
			"permissions":  permissionKeys(u.Permissions),
			"rate_limit":   map[string]any{"requests_per_minute": apiKeyRateLimit},
		})
	}))

	// ---- workspaces and channels ----

	mux.HandleFunc("GET /v1/workspaces", k(func(w http.ResponseWriter, r *http.Request) {
		teams, err := b.store.Teams(r.Context(), orgOf(r))
		if err != nil {
			fail(w, err)
			return
		}
		out := make([]map[string]any, 0, len(teams))
		for _, t := range teams {
			out = append(out, map[string]any{"team_id": t.TeamID, "name": t.Name, "domain": t.Domain,
				"status": t.Status, "installed_at": t.InstalledAt})
		}
		writeJSON(w, 200, map[string]any{"workspaces": out})
	}))

	// Channels and workspaces the bot has settings for. Read-only here on purpose: changing what
	// a channel may reach is a decision with a person behind it, and the console is where the
	// consequences are shown.
	mux.HandleFunc("GET /v1/scopes", k(func(w http.ResponseWriter, r *http.Request) {
		sc, err := b.store.Scopes(r.Context(), orgOf(r))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"scopes": b.nameTeams(r.Context(), sc)})
	}))

	// ---- documents: what the bot can search when it answers ----

	mux.HandleFunc("GET /v1/documents", k(func(w http.ResponseWriter, r *http.Request) {
		docs, err := b.docs.For(orgOf(r)).List(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"documents": docs})
	}))

	// Upload files, a PDF as readily as a text file, one or many to a request. The multipart form
	// the console's Upload button sends — "files", with "paths" and "folder" beside them — handled
	// by the same code, so what a key may store is exactly what a person may.
	mux.HandleFunc("POST /v1/documents", kp(PermDocsManage, func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, v1UploadLimit)
		b.uploadDocuments(w, r, v1Actor(r))
	}))

	mux.HandleFunc("GET /v1/documents/{path...}", k(func(w http.ResponseWriter, r *http.Request) {
		p := r.PathValue("path")
		if !isEditableDoc(p) {
			writeJSON(w, 415, map[string]any{"error": "that document is not a text type, so it has no contents to return"})
			return
		}
		f, err := b.docs.For(orgOf(r)).Get(r.Context(), p)
		if err != nil {
			if os.IsNotExist(err) || errors.Is(err, storage.ErrObjectNotExist) {
				v1Missing(w, "document")
				return
			}
			fail(w, err)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", docDisposition(p))
		w.Header().Set("Cache-Control", "no-store")
		io.Copy(w, f)
	}))

	// Write a document and re-index it. This is the endpoint that earns the API: a repository
	// hook or a nightly export can keep what the bot knows in step with a source of truth
	// somewhere else, without anybody dragging files into a browser.
	mux.HandleFunc("PUT /v1/documents/{path...}", kp(PermDocsManage, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Content *string `json:"content"`
			Scope   *string `json:"scope"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&in); err != nil {
			bad(w, err)
			return
		}
		p := r.PathValue("path")
		org := orgOf(r)
		if in.Content != nil {
			if !isEditableDoc(p) {
				writeJSON(w, 415, map[string]any{"error": "only text documents can be written through the API"})
				return
			}
			scope := ""
			if in.Scope != nil {
				scope = *in.Scope
			}
			if err := b.docs.For(org).Put(r.Context(), p, strings.NewReader(*in.Content), v1Actor(r), scope); err != nil {
				fail(w, err)
				return
			}
		} else if in.Scope != nil {
			if err := b.store.SetDocumentScope(r.Context(), org, p, *in.Scope); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					v1Missing(w, "document")
					return
				}
				fail(w, err)
				return
			}
		} else {
			bad(w, fmt.Errorf("send content, scope, or both"))
			return
		}
		// Indexing is the slow half and the caller does not need to wait for it. The document is
		// stored either way; until the index catches up the bot answers from the old text.
		go b.reindex(context.WithoutCancel(r.Context()), org)
		writeJSON(w, 200, map[string]any{"ok": true, "path": p, "indexing": true})
	}))

	mux.HandleFunc("DELETE /v1/documents/{path...}", kp(PermDocsManage, func(w http.ResponseWriter, r *http.Request) {
		org := orgOf(r)
		if err := b.docs.For(org).Delete(r.Context(), r.PathValue("path")); err != nil {
			if os.IsNotExist(err) || errors.Is(err, storage.ErrObjectNotExist) {
				v1Missing(w, "document")
				return
			}
			fail(w, err)
			return
		}
		go b.reindex(context.WithoutCancel(r.Context()), org)
		writeJSON(w, 200, map[string]any{"ok": true})
	}))

	// ---- memory: what the bot remembers per workspace and channel ----

	mux.HandleFunc("GET /v1/memories", k(func(w http.ResponseWriter, r *http.Request) {
		ms, err := b.store.AllMemories(r.Context(), orgOf(r))
		if err != nil {
			fail(w, err)
			return
		}
		out := make([]map[string]any, 0, len(ms))
		for _, m := range ms {
			out = append(out, map[string]any{"id": m.ID, "team_id": m.TeamID, "scope": m.Scope,
				"text": m.Text, "created_by": m.CreatedBy, "created_at": m.At})
		}
		writeJSON(w, 200, map[string]any{"memories": out})
	}))

	mux.HandleFunc("POST /v1/memories", kp(PermMemoryManage, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Scope string `json:"scope"`
			Text  string `json:"text"`
		}
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		in.Scope, in.Text = strings.TrimSpace(in.Scope), strings.TrimSpace(in.Text)
		if in.Scope == "" || in.Text == "" {
			bad(w, fmt.Errorf("scope and text are both required"))
			return
		}
		// A memory is keyed by the Slack workspace in its scope, not by the organisation, so the
		// organisation has to be established here: a key must not be able to write into a
		// workspace that belongs to somebody else by naming it.
		team := teamOfMemoryScope(in.Scope)
		if team == "" {
			bad(w, fmt.Errorf(`scope must name a workspace or channel: "team:T0123" or "channel:T0123/C0456"`))
			return
		}
		t, err := b.store.Team(r.Context(), team)
		if err != nil {
			fail(w, err)
			return
		}
		if t == nil || t.OrgID != orgOf(r) {
			v1Missing(w, "workspace")
			return
		}
		if err := b.store.AddMemory(r.Context(), orgOf(r), team, in.Scope, in.Text, v1Actor(r)); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 201, map[string]any{"ok": true})
	}))

	mux.HandleFunc("PUT /v1/memories/{id}", kp(PermMemoryManage, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Text string `json:"text"`
		}
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		in.Text = strings.TrimSpace(in.Text)
		if in.Text == "" {
			bad(w, fmt.Errorf("text is required"))
			return
		}
		// Looked up under the key's organisation: a memory that is not yours is "no such
		// memory", the same answer a missing one gets.
		found, err := b.store.UpdateMemory(r.Context(), orgOf(r), pathID(r, "id"), in.Text)
		if err != nil {
			fail(w, err)
			return
		}
		if !found {
			v1Missing(w, "memory")
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	}))

	mux.HandleFunc("DELETE /v1/memories/{id}", kp(PermMemoryManage, func(w http.ResponseWriter, r *http.Request) {
		if err := b.store.DeleteMemory(r.Context(), orgOf(r), pathID(r, "id")); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	}))

	// ---- artifacts: files a turn produced ----

	mux.HandleFunc("GET /v1/artifacts", kp(PermArtifactsView, func(w http.ResponseWriter, r *http.Request) {
		arts, err := b.store.Artifacts(r.Context(), orgOf(r), v1Limit(r, 50, 200))
		if err != nil {
			fail(w, err)
			return
		}
		out := make([]map[string]any, 0, len(arts))
		for _, a := range arts {
			out = append(out, b.v1ArtifactJSON(r.Context(), a, false))
		}
		writeJSON(w, 200, map[string]any{"artifacts": out})
	}))

	// One artifact, contents included — the list omits them so it stays small.
	mux.HandleFunc("GET /v1/artifacts/{id}", kp(PermArtifactsView, func(w http.ResponseWriter, r *http.Request) {
		a, err := b.store.ArtifactByID(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil || a == nil {
			v1Missing(w, "artifact")
			return
		}
		writeJSON(w, 200, b.v1ArtifactJSON(r.Context(), *a, true))
	}))

	// ---- routines: prompts on a schedule ----

	mux.HandleFunc("GET /v1/routines", k(func(w http.ResponseWriter, r *http.Request) {
		rs, err := b.orgRoutines(r.Context(), orgOf(r))
		if err != nil {
			fail(w, err)
			return
		}
		names := b.conversationNamer(r.Context(), orgOf(r))
		out := make([]map[string]any, 0, len(rs))
		for _, rt := range rs {
			out = append(out, v1RoutineNamed(r.Context(), rt, names))
		}
		writeJSON(w, 200, map[string]any{"routines": out})
	}))

	// Make a routine: a prompt on a schedule, posting in a channel the bot is in. Which channel
	// is the caller's to say — its id, or its name — and what each run may reach is that
	// channel's. Two things differ from the console's editor, both on purpose. It runs as the
	// bot, never as anybody: no key and no MCP token is a Slack account, so none can lend a
	// routine somebody's personal connections. And its writes start on Ask first, the default a
	// routine the bot makes from a Slack message gets: an MCP client writing a routine is a model
	// deciding what a schedule may change, and letting those writes run unasked is a decision
	// somebody makes in the console, looking at the routine.
	mux.HandleFunc("POST /v1/routines", kp(PermRoutinesManage, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Channel     string          `json:"channel"`
			TeamID      string          `json:"team_id"`
			Cron        string          `json:"cron"`
			Timezone    string          `json:"timezone"`
			Prompt      string          `json:"prompt"`
			Notify      string          `json:"notify"`
			NotifyWhen  string          `json:"notify_when"`
			Model       string          `json:"model"`
			Steps       json.RawMessage `json:"steps"`
			Finish      string          `json:"finish"`
			AutoConfirm *bool           `json:"auto_confirm"`
		}
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		if in.AutoConfirm != nil && *in.AutoConfirm {
			bad(w, errV1AutoConfirm)
			return
		}
		ctx, org := r.Context(), orgOf(r)
		channel, team := "", ""
		if strings.TrimSpace(in.Channel) != "" {
			var err error
			if channel, team, err = b.findRoutineChannel(ctx, org, in.Channel, in.TeamID); err != nil {
				bad(w, err)
				return
			}
		}
		id, err := b.addRoutine(ctx, org, routineDraft{Channel: channel, TeamID: team, Cron: in.Cron, TZ: in.Timezone,
			Prompt: in.Prompt, Notify: in.Notify, NotifyWhen: in.NotifyWhen, Model: in.Model, Steps: in.Steps,
			Finish: in.Finish})
		if err != nil {
			bad(w, err)
			return
		}
		rt := b.routineByID(ctx, org, id)
		if rt == nil {
			fail(w, errors.New("the routine was saved but could not be read back"))
			return
		}
		writeJSON(w, 201, v1RoutineNamed(ctx, *rt, b.conversationNamer(ctx, org)))
	}))

	// Change a routine. Only what is sent changes; enabled pauses and resumes it. The same rule
	// as the console's editor decides whose it is afterwards: rewriting what a routine does
	// takes it off the person it was running as — it runs as the bot from then on, and they are
	// told — while pausing it or widening its schedule is housekeeping and leaves it theirs.
	mux.HandleFunc("PUT /v1/routines/{id}", kp(PermRoutinesManage, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Enabled     *bool           `json:"enabled"`
			Cron        *string         `json:"cron"`
			Timezone    *string         `json:"timezone"`
			Prompt      *string         `json:"prompt"`
			Channel     *string         `json:"channel"`
			TeamID      *string         `json:"team_id"`
			Notify      *string         `json:"notify"`
			NotifyWhen  *string         `json:"notify_when"`
			Model       *string         `json:"model"`
			Steps       json.RawMessage `json:"steps"`
			Finish      *string         `json:"finish"`
			AutoConfirm *bool           `json:"auto_confirm"`
		}
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		if in.AutoConfirm != nil && *in.AutoConfirm {
			bad(w, errV1AutoConfirm)
			return
		}
		ctx, org, id := r.Context(), orgOf(r), pathID(r, "id")
		if b.routineByID(ctx, org, id) == nil {
			v1Missing(w, "routine")
			return
		}
		e := routineEdit{Enabled: in.Enabled, Cron: in.Cron, TZ: in.Timezone, Prompt: in.Prompt, Notify: in.Notify,
			NotifyWhen: in.NotifyWhen, Model: in.Model, Steps: in.Steps, Finish: in.Finish, AutoConfirm: in.AutoConfirm}
		if in.Channel != nil {
			teamID := ""
			if in.TeamID != nil {
				teamID = *in.TeamID
			}
			channel, team, err := b.findRoutineChannel(ctx, org, *in.Channel, teamID)
			if err != nil {
				bad(w, err)
				return
			}
			e.Channel, e.TeamID = &channel, &team
		}
		wasRunningAs, err := b.editRoutine(ctx, org, id, e)
		if err != nil {
			bad(w, err)
			return
		}
		out := map[string]any{}
		if rt := b.routineByID(ctx, org, id); rt != nil {
			out = v1RoutineNamed(ctx, *rt, b.conversationNamer(ctx, org))
		}
		if wasRunningAs != "" {
			out["was_running_as"] = wasRunningAs
		}
		writeJSON(w, 200, out)
	}))

	mux.HandleFunc("DELETE /v1/routines/{id}", kp(PermRoutinesManage, func(w http.ResponseWriter, r *http.Request) {
		ctx, org, id := r.Context(), orgOf(r), pathID(r, "id")
		if b.routineByID(ctx, org, id) == nil {
			v1Missing(w, "routine")
			return
		}
		if err := b.store.DeleteRoutine(ctx, org, id); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	}))

	// Run one now, out of band. Accepted rather than done: the turn happens in Slack, in its own
	// time, and its result lands in the channel the routine posts to.
	mux.HandleFunc("POST /v1/routines/{id}/run", kp(PermRoutinesManage, func(w http.ResponseWriter, r *http.Request) {
		rs, err := b.orgRoutines(r.Context(), orgOf(r))
		if err != nil {
			fail(w, err)
			return
		}
		id := pathID(r, "id")
		for _, rt := range rs {
			if rt.ID != id {
				continue
			}
			// Recorded like any other run, and the schedule stays where it was.
			go b.agent.runRoutineNow(context.Background(), rt, rt.NextRun)
			writeJSON(w, 202, map[string]any{"ok": true, "routine_id": id})
			return
		}
		v1Missing(w, "routine")
	}))

	// One routine's history. A quiet routine posts nothing to Slack, so for anything built on
	// this API these rows are the only record that a run happened at all.
	mux.HandleFunc("GET /v1/routines/{id}/runs", k(func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		runs, err := b.store.RoutineRuns(r.Context(), orgOf(r), pathID(r, "id"), limit)
		if err != nil {
			fail(w, err)
			return
		}
		out := make([]map[string]any, 0, len(runs))
		for _, run := range runs {
			out = append(out, v1RoutineRunJSON(run))
		}
		writeJSON(w, 200, map[string]any{"runs": out})
	}))

	// ---- jobs: the fixes the bot handed to a worker ----

	mux.HandleFunc("GET /v1/jobs", kp(PermJobsView, func(w http.ResponseWriter, r *http.Request) {
		jobs, err := b.store.Jobs(r.Context(), orgOf(r), JobFilter{
			Status: r.URL.Query().Get("status"), Limit: v1Limit(r, 50, 200)})
		if err != nil {
			fail(w, err)
			return
		}
		out := make([]map[string]any, 0, len(jobs))
		for _, j := range jobs {
			out = append(out, v1JobJSON(j))
		}
		writeJSON(w, 200, map[string]any{"jobs": out})
	}))

	mux.HandleFunc("GET /v1/jobs/{id}", kp(PermJobsView, func(w http.ResponseWriter, r *http.Request) {
		j, err := b.store.Job(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil || j == nil {
			v1Missing(w, "job")
			return
		}
		writeJSON(w, 200, v1JobJSON(*j))
	}))

	// ---- access requests ----

	mux.HandleFunc("GET /v1/access-requests", kp(PermAccessView, func(w http.ResponseWriter, r *http.Request) {
		rs, err := b.store.AccessRequests(r.Context(), orgOf(r), r.URL.Query().Get("status"), v1Limit(r, 50, 200))
		if err != nil {
			fail(w, err)
			return
		}
		out := make([]map[string]any, 0, len(rs))
		for i := range rs {
			out = append(out, b.accessJSON(r.Context(), &rs[i], false))
		}
		writeJSON(w, 200, map[string]any{"access_requests": out})
	}))

	// ---- what it did and what it cost ----

	mux.HandleFunc("GET /v1/activity", kp(PermActivityView, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		// range=today|7d|month are the windows /v1/usage counts in, so a caller reading
		// turns_today can ask for the very turns behind that number. before and after page it
		// the way they page /v1/audit, so a month with more turns than one page holds can be read
		// whole: before=<id> walks back newest first, after=<id> walks forward oldest first.
		before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
		after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
		turns, err := b.store.Turns(r.Context(), orgOf(r), TurnFilter{Channel: q.Get("channel"),
			Since: activitySince(q.Get("range")), Limit: v1Limit(r, 100, 500), Before: before, After: after})
		if err != nil {
			fail(w, err)
			return
		}
		// last_id is the high-water mark, as on /v1/audit. It starts at the mark the caller sent
		// (see there for why).
		last := after
		out := make([]map[string]any, 0, len(turns))
		for _, t := range turns {
			last = max(last, t.ID)
			out = append(out, map[string]any{"id": t.ID, "at": t.At, "team_id": t.TeamID,
				"team_name": b.teamName(r.Context(), t.TeamID), "channel": t.Channel,
				"channel_name": b.channelName(r.Context(), t.TeamID, t.Channel),
				"thread_ts":    t.ThreadTS, "model": t.Model,
				"tokens_in": t.In, "tokens_out": t.Out, "cost_usd": t.Cost})
		}
		writeJSON(w, 200, map[string]any{"turns": out, "last_id": last})
	}))

	// The audit log, for whoever keeps one elsewhere. ?after=<id> walks forward from the last
	// row a collector saw and comes back oldest first, which is the shape a SIEM poll wants;
	// without it the newest rows come first, the way the console shows them. last_id is the
	// high-water mark either way, so the next call is ?after=<last_id>.
	mux.HandleFunc("GET /v1/audit", kp(PermAuditView, func(w http.ResponseWriter, r *http.Request) {
		f := auditFilterFrom(r, 100, 500)
		events, err := b.store.AuditEvents(r.Context(), orgOf(r), f)
		if err != nil {
			fail(w, err)
			return
		}
		// A page with nothing new hands back the mark it was sent. It used to say 0, and the
		// next call, ?after=0, is no cursor at all: it came back with the newest page, so
		// anything older than that page which the collector had not yet read was skipped.
		last := f.After
		for _, e := range events {
			last = max(last, e.ID)
		}
		writeJSON(w, 200, map[string]any{"events": events, "last_id": last})
	}))

	mux.HandleFunc("GET /v1/usage", k(func(w http.ResponseWriter, r *http.Request) {
		org := orgOf(r)
		o := b.store.OverviewStats(r.Context(), org)
		st := b.settings.Get(r.Context(), org)
		// paused_by is the field worth polling: it is empty until one of the two limits binds
		// and then names which. A script watching it is warned before the bot goes quiet rather
		// than told afterwards by somebody in the channel.
		// Read unconditionally now: an account can be metered by a plan allowance without ever
		// having bought credit, and the old `if st.CreditEnforced` guard would have answered
		// "nothing is stopping you" right up to the moment the allowance ran out.
		spendable, metered, _ := b.store.SpendableCredit(r.Context(), org)
		bal := int64(0)
		if metered {
			bal = spendable
		}
		names := b.conversationNamer(r.Context(), org)
		channels := make([]map[string]any, 0, len(o.TopChannels))
		for _, u := range o.TopChannels {
			channels = append(channels, v1ChannelUsageJSON(u, names.usageName(r.Context(), u.TeamID, u.Channel)))
		}
		writeJSON(w, 200, map[string]any{
			"month_spend_usd":    o.MonthSpend,
			"monthly_budget_usd": st.EffectiveBudget(),
			"plan":               st.Plan,
			"credit_enabled":     metered,
			"credit_balance_usd": microsToUSD(bal),
			"paused_by":          pausedBy(st, metered, spendable, v1BudgetSpend(r, b, org, st)),
			"turns_today":        o.TurnsToday,
			"turns_7d":           o.Turns7d,
			"documents":          o.Docs,
			"chunks":             o.Chunks,
			"workspaces":         o.Teams,
			"channels":           o.Scopes,
			"routines":           o.Routines,
			"memories":           o.Memories,
			"top_channels":       channels,
			// The people behind the turns, over the window the plan size is judged on. No
			// per-workspace breakdown here: a script asking for the operational picture wants
			// the number, and the console is where somebody asks why it is what it is.
			"active_users": b.activeUsers(r.Context(), org, false),
		})
	}))

	// Billing, for a script that wants the commercial picture rather than the operational one:
	// what the plan costs, when it renews, and how much credit is left. Any key, like /v1/usage
	// and for the same reason — the balance answers "why has the bot gone quiet", which is not a
	// privileged question.
	//
	// It carries no Stripe identifiers at any permission: those name this account to Stripe's own
	// support, and an API key has no business holding them. There is deliberately no checkout or
	// portal route here either — both mint URLs bound to a browser session, and a key has no
	// browser to hand them to.
	//
	// Only on a deployment that sells plans, like the rest of billing (billingRoutes): elsewhere it
	// answers 404 as any path the deployment does not serve does, rather than describing a plan and
	// a balance on an account nothing was ever sold to.
	if b.cfg.BillingEnabled() {
		mux.HandleFunc("GET /v1/billing", k(func(w http.ResponseWriter, r *http.Request) {
			org := orgOf(r)
			acct, err := b.store.BillingAccountOf(r.Context(), org)
			if err != nil {
				fail(w, err)
				return
			}
			st := b.settings.Get(r.Context(), org)
			spend, _ := budgetSpend(r.Context(), b.store, org, st)
			out := map[string]any{
				"plan": st.Plan, "currency": b.cfg.BillingCurrency,
				"month_spend_usd": spend, "monthly_budget_usd": st.EffectiveBudget(),
				"paused_by": pausedBy(st, acct.Metered(), acct.SpendableMicros(), spend),
				"credit": map[string]any{
					// balance_usd is prepaid credit, which never expires. The allowance is separate
					// and does: it belongs to the period it was granted for.
					"balance_usd":         microsToUSD(acct.CreditBalanceMicros),
					"enabled":             acct.Metered(),
					"overdraft_usd":       microsToUSD(maxOverdraftMicros),
					"allowance_usd":       microsToUSD(acct.SpendableAllowanceMicros()),
					"allowance_month_usd": microsToUSD(acct.AllowanceGrantedMicros),
					"allowance_expires":   acct.AllowancePeriodEnd,
					"spendable_usd":       microsToUSD(acct.SpendableMicros()),
				},
				"subscription": nil,
				"users":        b.activeUsers(r.Context(), org, false),
			}
			if acct.Exists && acct.Status != "" {
				out["subscription"] = map[string]any{
					"status": acct.Status, "size": acct.Size, "size_label": sizeLabels[acct.Size],
					"amount_usd": microsToUSD(acct.AmountMicros()),
					"period_end": acct.PeriodEnd, "cancel_at_period_end": acct.CancelAtPeriodEnd,
				}
			}
			writeJSON(w, 200, out)
		}))
	}
}

// v1ArtifactJSON is an artifact in the API's own vocabulary. Not artifactJSON: that one answers
// the console, in the field names the console's TypeScript expects, and a public contract should
// not be one refactor of an internal screen away from breaking. Same rows, snake_case, and the
// body only when a single artifact was asked for.
func (b *Bot) v1ArtifactJSON(ctx context.Context, a Artifact, withContent bool) map[string]any {
	m := map[string]any{
		"id": a.ID, "title": a.Title, "kind": a.Kind, "bytes": a.Bytes,
		"team_id": a.TeamID, "team_name": b.teamName(ctx, a.TeamID),
		"channel": a.Channel, "channel_name": b.channelName(ctx, a.TeamID, a.Channel),
		"thread_ts": a.ThreadTS, "created_by": a.CreatedBy,
		"created_by_name": b.userName(ctx, a.TeamID, a.CreatedBy),
		"permalink":       a.Permalink, "created_at": a.At,
	}
	if withContent {
		m["content"] = a.Content
	}
	return m
}

// v1Actor is who a write is attributed to. A key has no Slack identity, so the account's email is
// what appears against a document or a memory — the person, not the script, which is the honest
// answer to "who put this here".
func v1Actor(r *http.Request) string {
	if u := adminFromCtx(r.Context()); u != nil && u.Email != "" {
		return u.Email
	}
	return "api"
}

// orgRoutines narrows the routine list to one organisation. The narrowing is now the store's
// where clause rather than a filter applied afterwards, so an id from another organisation is
// never read in the first place — but the helper stays, because callers want the empty slice
// rather than a nil one.
func (b *Bot) orgRoutines(ctx context.Context, orgID int64) ([]Routine, error) {
	rs, err := b.store.Routines(ctx, orgID, "")
	if err != nil {
		return nil, err
	}
	if rs == nil {
		return []Routine{}, nil
	}
	return rs, nil
}

func v1RoutineJSON(rt Routine) map[string]any {
	// The steps go out as the array they are, not as a string holding one: a caller reading
	// this should not have to parse JSON a second time to see what the routine calls.
	steps := json.RawMessage(rt.Steps)
	if !json.Valid(steps) {
		steps = json.RawMessage("[]")
	}
	return map[string]any{"id": rt.ID, "team_id": rt.TeamID, "channel": rt.Channel,
		"cron": rt.Cron, "timezone": rt.TZ, "prompt": rt.Prompt, "enabled": rt.Enabled,
		"created_by": rt.CreatedBy, "next_run": rt.NextRun, "last_run": rt.LastRun,
		"last_error": rt.LastError, "notify": notifyMode(rt.Notify), "notify_when": rt.NotifyWhen,
		"last_status": rt.LastStatus, "steps": steps, "finish": routineFinish(rt.Finish), "model": rt.Model,
		"auto_confirm": rt.AutoConfirm}
}

// v1RoutineNamed is v1RoutineJSON with the channel named the way the console names it, from the
// scopes table rather than a round trip to Slack per row.
func v1RoutineNamed(ctx context.Context, rt Routine, names *conversationNamer) map[string]any {
	m := v1RoutineJSON(rt)
	m["channel_name"] = names.name(ctx, rt.TeamID, rt.Channel)
	return m
}

// errV1AutoConfirm is the one routine setting the API will not turn on.
var errV1AutoConfirm = errors.New("a routine made or changed through the API asks before it writes: " +
	"letting its writes run without asking is switched on in the console, under Routines")

// v1RoutineRunJSON is one run, output and all. Unlike the console's listing this does not
// preview: an API caller asked for the record, not a page to scroll.
func v1RoutineRunJSON(r RoutineRun) map[string]any {
	return map[string]any{"id": r.ID, "routine_id": r.RoutineID, "status": r.Status,
		"reason": r.Reason, "output": r.Output, "error": r.Error, "thread_ts": r.ThreadTS,
		"tokens_in": r.TokensIn, "tokens_out": r.TokensOut, "cost_usd": r.CostUSD,
		"started_at": r.StartedAt, "finished_at": r.FinishedAt, "ms": r.MS}
}

// v1ChannelUsageJSON is one conversation's month, in the words /v1/activity uses for a turn, and
// named the way the console names it. /v1/usage used to hand out the console's UsageRow as it is,
// PascalCase, while the API reference documented snake_case, so a caller coding to either one
// found the other.
func v1ChannelUsageJSON(u UsageRow, name string) map[string]any {
	return map[string]any{"team_id": u.TeamID, "channel": u.Channel, "channel_name": name,
		"turns": u.Turns, "tokens_in": u.In, "tokens_out": u.Out, "cost_usd": u.Cost}
}

// v1JobJSON is a job as an integration wants it: what it is doing, where the pull request went,
// and what it cost. The worker's own bookkeeping — its token, its execution reference, the spec
// it was handed — stays inside.
func v1JobJSON(j Job) map[string]any {
	return map[string]any{
		"id": j.ID, "status": j.Status, "phase": j.Phase,
		"repo": j.Repo, "base_branch": j.BaseBranch, "branch": j.Branch, "title": j.Title,
		"team_id": j.TeamID, "channel": j.Channel, "thread_ts": j.ThreadTS,
		"requester": j.Requester, "approved_by": j.ApprovedBy,
		"engine": j.Engine, "model": j.Model, "budget_usd": j.BudgetUSD,
		"created_at": j.CreatedAt, "finished_at": j.FinishedAt,
		"pr_url": j.PRURL, "cost_usd": j.CostUSD, "error": j.Error,
	}
}

// v1BudgetSpend is budgetSpend for a /v1 handler, where the month's total is already in hand for
// the figure it reports and the limit is measured against the key in use.
func v1BudgetSpend(r *http.Request, b *Bot, orgID int64, st Settings) float64 {
	spent, _ := budgetSpend(r.Context(), b.store, orgID, st)
	return spent
}
