package app

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/api/idtoken"
)

// jobsRoutes registers the worker-facing endpoints (job-token auth; reachable at the public edge
// like /setup) and the console's job endpoints (admin auth).
func (b *Bot) jobsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/worker/jobs/{id}/claim", b.requireJobToken(b.handleJobClaim))
	mux.HandleFunc("POST /api/worker/jobs/{id}/events", b.requireJobToken(b.handleJobEvents))
	mux.HandleFunc("PUT /api/worker/jobs/{id}/diff", b.requireJobToken(b.handleJobDiff))
	mux.HandleFunc("POST /api/worker/jobs/{id}/result", b.requireJobToken(b.handleJobResult))
	mux.HandleFunc("GET /api/worker/jobs/{id}", b.requireJobToken(b.handleJobPoll))
	mux.HandleFunc("GET /api/jobs", b.requirePerm(PermJobsView, b.handleJobsList))
	mux.HandleFunc("GET /api/jobs/{id}", b.requirePerm(PermJobsView, b.handleJobDetail))
	mux.HandleFunc("POST /api/jobs/{id}/cancel", b.requirePerm(PermJobsManage, b.handleJobCancel))
	mux.HandleFunc("GET /api/jobs/{id}/diff", b.requirePerm(PermJobsView, b.handleJobFile("diff")))
	mux.HandleFunc("GET /api/jobs/{id}/log", b.requirePerm(PermJobsView, b.handleJobFile("log")))
}

type jobHandler func(w http.ResponseWriter, r *http.Request, j *Job)

func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[len("Bearer "):])
	}
	return ""
}

// requireJobToken admits a worker that presents the token minted for that exact job. Unknown id
// and wrong token look the same (401), failures are counted per address like console logins, and
// in cloudrun mode the request must also carry a Google identity token for the worker's account.
func (b *Bot) requireJobToken(next jobHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := "job:" + clientIP(r)
		if d, yes := logins.locked(key); yes {
			w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many failed requests"})
			return
		}
		id := pathID(r, "id")
		tok := bearerToken(r)
		var j *Job
		if tid, ok := parseJobToken(tok); ok && tid == id && id != 0 {
			j, _ = b.store.jobByID(r.Context(), id)
		}
		refuse := func(why string) {
			logins.fail(key)
			slog.Warn("worker request refused", "job", id, "ip", clientIP(r), "why", why)
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		}
		if j == nil || j.TokenHash == "" || subtle.ConstantTimeCompare([]byte(hashJobToken(tok)), []byte(j.TokenHash)) != 1 {
			refuse("bad token")
			return
		}
		if j.TokenExpires != "" && j.TokenExpires < now() {
			refuse("token expired")
			return
		}
		if b.cfg.WorkerRequireOIDC && !b.verifyWorkerOIDC(r) {
			refuse("identity token")
			return
		}
		logins.reset(key)
		next(w, r, j)
	}
}

// verifyWorkerOIDC checks the Google ID token a Cloud Run worker sends (audience = our URL,
// issued to the worker's service account), so a job token alone is useless from outside GCP.
func (b *Bot) verifyWorkerOIDC(r *http.Request) bool {
	tok := r.Header.Get("X-Worker-OIDC")
	if tok == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	p, err := idtoken.Validate(ctx, tok, b.jobs.botURL())
	if err != nil {
		slog.Warn("worker identity token", "err", err)
		return false
	}
	if want := b.cfg.WorkerSAEmail; want != "" {
		email, _ := p.Claims["email"].(string)
		if !strings.EqualFold(email, want) {
			slog.Warn("worker identity token from another account", "email", email)
			return false
		}
	}
	return true
}

// scrub masks anything secret-shaped and the job's own repository token in worker-supplied text.
//
// And the organisation's own model key, by value, when the job ran on it. redact knows the shapes
// OpenAI and OpenRouter keys take; an Azure key is thirty-two hex characters that look like
// nothing in particular, and a worker log is exactly where one would be printed.
func (r *JobRunner) scrub(ctx context.Context, j *Job, s string) string {
	s = redact(s)
	if conn, err := r.store.Connection(ctx, j.OrgID, j.ConnectionID); err == nil && conn != nil {
		s = r.proxy.scrubSecrets(conn, s)
	}
	if j.KeyOwner == keyOwnerOrg && r.proxy != nil && r.proxy.sealer != nil {
		if key, ok, err := r.store.ModelKeySecret(ctx, j.OrgID, r.proxy.sealer); err == nil && ok && len(key) >= 8 {
			s = strings.ReplaceAll(s, key, "[redacted-secret]")
		}
	}
	return s
}

func tooBig(w http.ResponseWriter, err error) bool {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "body too large"})
		return true
	}
	return false
}

// ---- worker side ----

// repoJobToken is the credential the worker clones and pushes with. A repository connected by
// pasting a token spends that token. One connected through the GitHub App has no token stored at
// all — only an installation id — and mints one here, scoped to this repository alone
// (github_token.go). Both kinds reach the worker in the same field: it authenticates as
// x-access-token either way (worker/git.go), which is the form both a PAT and an installation
// token take.
//
// A minted token lives about an hour, so a job allowed to run longer than that would die at the
// push having already paid for the change. worker_timeout_minutes defaults to 45; clampJobTimeout
// is what keeps a raised setting from crossing the line.
func (b *Bot) repoJobToken(ctx context.Context, orgID int64, conn *Connection, repo string) (string, error) {
	sec, err := b.proxy.secret(conn)
	if err != nil {
		return "", fmt.Errorf("could not open what is stored for %s: %w", conn.Name, err)
	}
	if conn.CredType != "github_app" && conn.GitHubInstallationID == 0 {
		if sec.Token == "" {
			return "", fmt.Errorf("%s has no stored access token; connect the repository again", conn.Name)
		}
		return sec.Token, nil
	}
	// installationToken scopes by conn.Repo. An app-backed connection always carries it, but a
	// job knows its own repository too, and a token scoped to the whole installation is wider
	// than this job was approved for — so borrow the spec's rather than fall back to that.
	scoped := *conn
	if scoped.Repo == "" {
		scoped.Repo = repo
	}
	tok, err := b.proxy.installationToken(ctx, orgID, &scoped, sec)
	if err != nil {
		return "", fmt.Errorf("could not mint a GitHub App token for %s: %w", scoped.Repo, err)
	}
	return tok, nil
}

// handleJobClaim hands the worker its spec and secrets. This is the one response with secrets in
// it, and it is never logged.
func (b *Bot) handleJobClaim(w http.ResponseWriter, r *http.Request, j *Job) {
	ctx := r.Context()
	var in JobClaimRequest
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in)
	info, _ := json.Marshal(in.Worker)
	j2, why, err := b.store.ClaimJob(ctx, j.OrgID, j.ID, string(info))
	if err != nil {
		fail(w, err)
		return
	}
	if j2 == nil {
		code := http.StatusGone
		if why == "already claimed" {
			code = http.StatusConflict
		}
		writeJSON(w, code, map[string]any{"error": why})
		return
	}
	var spec JobSpec
	json.Unmarshal([]byte(j2.Spec), &spec)
	conn, err := b.store.Connection(ctx, j2.OrgID, j2.ConnectionID)
	if err != nil || conn == nil {
		b.jobs.finish(ctx, j2.OrgID, j2.ID, JobFailed, &JobResult{Error: JobError{Code: "dispatch_failed", Message: "the repository connection is gone"}})
		writeJSON(w, http.StatusGone, map[string]any{"error": "connection gone"})
		return
	}
	ghToken, err := b.repoJobToken(ctx, j2.OrgID, conn, spec.Repo)
	if err != nil {
		// The reason is for the operator: it goes on the job and into the log, never to the
		// worker, which is told only that there is no token for it.
		slog.Error("worker repository token", "job", j2.ID, "repo", spec.Repo, "cred", conn.CredType, "err", err)
		b.jobs.finish(ctx, j2.OrgID, j2.ID, JobFailed, &JobResult{Error: JobError{Code: "dispatch_failed", Message: err.Error()}})
		writeJSON(w, http.StatusGone, map[string]any{"error": "repository token unavailable"})
		return
	}
	deadline := time.Now().Add(time.Duration(j2.TimeoutS) * time.Second)
	if t, ok := parseStoreTime(j2.CreatedAt); ok {
		deadline = t.Add(time.Duration(j2.TimeoutS) * time.Second)
	}
	llm, err := b.jobs.llmSecret(ctx, j2)
	if err != nil {
		// The reason is for the operator: it goes on the job, never to the worker.
		slog.Error("worker credential", "job", j2.ID, "err", err)
		b.jobs.finish(ctx, j2.OrgID, j2.ID, JobFailed, &JobResult{Error: JobError{Code: "credential_unavailable", Message: err.Error()}})
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "restricted model credential unavailable"})
		return
	}
	claim := JobClaim{
		Job:     JobClaimJob{ID: j2.ID, Spec: spec},
		Secrets: JobSecrets{GitHubToken: ghToken, LLM: llm, EngineAPIKey: ""},
		Limits: JobLimits{BudgetUSD: j2.BudgetUSD, Deadline: deadline.UTC().Format(time.RFC3339), HeartbeatSeconds: JobHeartbeatSecs,
			EventMaxBytes: JobEventMaxBytes, SummaryMaxBytes: JobSummaryMaxBytes, LogTailMaxBytes: JobLogTailMaxBytes, DiffMaxBytes: JobDiffMaxBytes},
		Cache: b.jobs.cacheURLs(ctx, j2),
	}
	slog.Info("job claimed", "job", j2.ID, "worker", string(info), "claims", j2.ClaimCount)
	b.jobs.refresh(j2.OrgID, j2.ID)
	writeJSON(w, 200, claim)
}

func (b *Bot) handleJobEvents(w http.ResponseWriter, r *http.Request, j *Job) {
	ctx := r.Context()
	if jobTerminal(j.Status) {
		writeJSON(w, http.StatusGone, map[string]any{"error": "finished"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 512<<10)
	var in JobEventsRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		if !tooBig(w, err) {
			bad(w, err)
		}
		return
	}
	if len(in.Events) > JobEventsBatchMax {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": fmt.Sprintf("at most %d events per call", JobEventsBatchMax)})
		return
	}
	for i := range in.Events {
		e := &in.Events[i]
		if len(e.Message) > JobEventMaxBytes {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": fmt.Sprintf("event %d text is over %d bytes", e.Seq, JobEventMaxBytes)})
			return
		}
		e.Message = b.jobs.scrub(ctx, j, e.Message)
		if len(e.Data) > 32<<10 {
			e.Data = nil
		}
		switch e.Kind {
		case JobKindPhase, JobKindLog, JobKindTests, JobKindUsage, JobKindHeartbeat, JobKindWarn:
		default:
			e.Kind = JobKindLog
		}
		// Priced before it is stored, so the job's running cost — which the budget cancel below
		// reads — includes what nothing on the worker's side could measure.
		b.jobs.priceJobUsage(ctx, j, e.Usage)
	}
	accepted, used, err := b.store.AddJobEvents(ctx, j.OrgID, j.ID, in.Events)
	if err != nil {
		fail(w, err)
		return
	}
	if used.In > 0 || used.Out > 0 || used.CostUSD > 0 {
		b.store.LogUsageBy(ctx, j.OrgID, j.TeamID, j.Channel, j.ThreadTS, j.Requester, "worker:"+j.Engine+"/"+j.Model, used.usageOn(j))
	}
	j2, _ := b.store.Job(ctx, j.OrgID, j.ID)
	if j2 == nil {
		j2 = j
	}
	if j2.BudgetUSD > 0 && j2.CostUSD > j2.BudgetUSD*1.25 && !j2.CancelRequested {
		if ok, _ := b.store.RequestJobCancel(ctx, j.OrgID, j.ID, "", "budget"); ok {
			j2.CancelRequested, j2.CancelReason = true, "budget"
			slog.Warn("job over budget; cancelling", "job", j.ID, "cost_usd", j2.CostUSD, "budget_usd", j2.BudgetUSD)
		}
	}
	if accepted > 0 {
		b.jobs.refresh(j.OrgID, j.ID)
	}
	writeJSON(w, 200, JobEventsResponse{OK: true, Accepted: accepted, Cancel: j2.CancelRequested, Reason: j2.CancelReason})
}

func (b *Bot) handleJobDiff(w http.ResponseWriter, r *http.Request, j *Job) {
	if jobTerminal(j.Status) {
		writeJSON(w, http.StatusGone, map[string]any{"error": "finished"})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, JobDiffMaxBytes+1))
	if err != nil {
		bad(w, err)
		return
	}
	if len(raw) > JobDiffMaxBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": fmt.Sprintf("diff over %d bytes; send the stat only", JobDiffMaxBytes)})
		return
	}
	if err := b.store.PutJobFile(r.Context(), j.OrgID, j.ID, "diff", b.jobs.scrub(r.Context(), j, string(raw))); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "bytes": len(raw)})
}

func (b *Bot) handleJobResult(w http.ResponseWriter, r *http.Request, j *Job) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, JobResultMaxBytes)
	var res JobResult
	if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
		if !tooBig(w, err) {
			bad(w, err)
		}
		return
	}
	if jobTerminal(j.Status) {
		var prev JobResult
		json.Unmarshal([]byte(j.Result), &prev)
		if res.Seq != 0 && prev.Seq == res.Seq {
			writeJSON(w, 200, map[string]any{"ok": true, "duplicate": true})
			return
		}
		writeJSON(w, http.StatusGone, map[string]any{"error": "finished"})
		return
	}
	switch res.Status {
	case JobSucceeded, JobFailed, JobCancelled, JobTimeout:
	default:
		bad(w, fmt.Errorf("status must be succeeded, failed, cancelled or timeout"))
		return
	}
	scrub := func(s string, n int) string { return truncate(b.jobs.scrub(ctx, j, s), n) }
	res.Summary = scrub(res.Summary, JobSummaryMaxBytes)
	res.LogTail = scrub(res.LogTail, JobLogTailMaxBytes)
	res.Error.Message = scrub(res.Error.Message, 2000)
	res.TicketComment = scrub(res.TicketComment, 8000)
	res.Tests.Before.Output = scrub(res.Tests.Before.Output, 4000)
	res.Tests.After.Output = scrub(res.Tests.After.Output, 4000)
	if len(res.FilesChanged) > 200 {
		res.FilesChanged = res.FilesChanged[:200]
	}
	if res.PR != nil && !strings.HasPrefix(res.PR.URL, "https://") {
		res.PR.URL = ""
	}
	b.jobs.priceJobUsage(ctx, j, &res.Usage)
	b.jobs.finish(ctx, j.OrgID, j.ID, res.Status, &res)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (b *Bot) handleJobPoll(w http.ResponseWriter, r *http.Request, j *Job) {
	writeJSON(w, 200, map[string]any{"status": j.Status, "cancel": j.CancelRequested, "reason": j.CancelReason})
}

// ---- console side ----

// jobJSON shapes a job for the console: ids resolved to names, links added.
func (b *Bot) jobJSON(ctx context.Context, j Job) map[string]any {
	raw, _ := json.Marshal(j)
	var m map[string]any
	json.Unmarshal(raw, &m)
	// A job belongs to one workspace; naming its channel and people needs that workspace's
	// client. When it is gone the ids stand in, which is better than another team's names.
	sl, _ := b.slacks.For(ctx, j.TeamID)
	if sl != nil {
		m["channel_name"] = sl.ChannelName(ctx, j.Channel)
		m["requester_name"] = sl.UserName(ctx, j.Requester)
		if j.ApprovedBy != "" {
			m["approved_by_name"] = sl.UserName(ctx, j.ApprovedBy)
		}
	}
	if t, _ := b.store.Team(ctx, j.TeamID); t != nil {
		m["team_name"] = t.Name
	}
	m["duration_s"] = int(jobDuration(&j).Seconds())
	m["console_url"] = executionConsoleURL(j.ExecutionRef)
	m["thread_link"] = ""
	if j.ThreadTS != "" && sl != nil {
		m["thread_link"] = sl.Permalink(ctx, j.Channel, j.ThreadTS)
	}
	return m
}

func (b *Bot) handleJobsList(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	jobs, err := b.store.Jobs(r.Context(), orgOf(r), JobFilter{Status: r.URL.Query().Get("status"), Channel: r.URL.Query().Get("channel"), Limit: limit})
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]map[string]any, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, b.jobJSON(r.Context(), j))
	}
	writeJSON(w, 200, out)
}

func (b *Bot) handleJobDetail(w http.ResponseWriter, r *http.Request) {
	j, err := b.store.Job(r.Context(), orgOf(r), pathID(r, "id"))
	if err != nil || j == nil {
		writeJSON(w, 404, map[string]any{"error": "no such job"})
		return
	}
	events, _ := b.store.JobEvents(r.Context(), j.OrgID, j.ID, 1000)
	var spec, result any
	json.Unmarshal([]byte(j.Spec), &spec)
	if j.Result != "" {
		json.Unmarshal([]byte(j.Result), &result)
	}
	diff, _, diffLink, _ := b.store.JobFile(r.Context(), j.OrgID, j.ID, "diff")
	writeJSON(w, 200, map[string]any{"job": b.jobJSON(r.Context(), *j), "spec": spec, "result": result, "events": events,
		"diff_bytes": len(diff), "diff_link": diffLink})
}

func (b *Bot) handleJobCancel(w http.ResponseWriter, r *http.Request) {
	j, err := b.store.Job(r.Context(), orgOf(r), pathID(r, "id"))
	if err != nil || j == nil {
		writeJSON(w, 404, map[string]any{"error": "no such job"})
		return
	}
	by := ""
	if u := adminFromCtx(r.Context()); u != nil {
		by = u.UserID
	}
	if !b.jobs.cancel(r.Context(), j, by, "admin") {
		writeJSON(w, 409, map[string]any{"error": "the job is not running"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (b *Bot) handleJobFile(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		j, err := b.store.Job(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil || j == nil {
			writeJSON(w, 404, map[string]any{"error": "no such job"})
			return
		}
		content, _, _, _ := b.store.JobFile(r.Context(), j.OrgID, j.ID, kind)
		if content == "" && kind == "log" && j.Result != "" {
			var res JobResult
			json.Unmarshal([]byte(j.Result), &res)
			content = res.LogTail
		}
		if content == "" {
			writeJSON(w, 404, map[string]any{"error": "nothing stored"})
			return
		}
		name := fmt.Sprintf("fix-job-%d.%s", j.ID, map[string]string{"diff": "diff", "log": "log"}[kind])
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		io.WriteString(w, content)
	}
}
