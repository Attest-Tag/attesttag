package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Job statuses before a terminal one (JobSucceeded, JobFailed, JobCancelled, JobTimeout).
const (
	jobQueued     = "queued"     // row exists, dispatcher not called yet
	jobStarting   = "starting"   // execution started, not claimed
	jobRunning    = "running"    // claimed, events flowing
	jobStale      = "stale"      // no event for a while; the reconciler is checking
	jobCancelling = "cancelling" // cancel asked for, waiting for the worker's result
)

var jobActiveStatuses = []string{jobQueued, jobStarting, jobRunning, jobStale, jobCancelling}

func jobTerminal(s string) bool {
	switch s {
	case JobSucceeded, JobFailed, JobCancelled, JobTimeout:
		return true
	}
	return false
}

// Job is one dispatched fix job. Spec and Result are JSON bodies kept off the list responses;
// the token and key columns never leave the process.
type Job struct {
	ID              int64   `json:"id"`
	Status          string  `json:"status"`
	OrgID           int64   `json:"-"` // the owning account; a join key, never part of a response
	TeamID          string  `json:"team_id"`
	Channel         string  `json:"channel"`
	ThreadTS        string  `json:"thread_ts"`
	Requester       string  `json:"requester"`
	ApprovedBy      string  `json:"approved_by"`
	Approval        string  `json:"approval"` // confirm | rule:<text>
	ConnectionID    int64   `json:"connection_id"`
	Repo            string  `json:"repo"`
	BaseBranch      string  `json:"base_branch"`
	Branch          string  `json:"branch"`
	Title           string  `json:"title"`
	Spec            string  `json:"-"`
	Engine          string  `json:"engine"`
	Model           string  `json:"model"`
	BudgetUSD       float64 `json:"budget_usd"`
	TimeoutS        int     `json:"timeout_s"`
	DraftPR         bool    `json:"draft_pr"`
	Dispatcher      string  `json:"dispatcher"`
	ExecutionRef    string  `json:"execution_ref"`
	TokenHash       string  `json:"-"`
	TokenExpires    string  `json:"-"`
	ClaimedAt       string  `json:"claimed_at"`
	ClaimCount      int     `json:"claim_count"`
	WorkerInfo      string  `json:"worker_info"`
	StatusTS        string  `json:"status_ts"`
	Phase           string  `json:"phase"`
	LastSeq         int64   `json:"last_seq"`
	LastEventAt     string  `json:"last_event_at"`
	CancelRequested bool    `json:"cancel_requested"`
	CancelBy        string  `json:"cancel_by"`
	CancelReason    string  `json:"cancel_reason"`
	CancelAt        string  `json:"cancel_requested_at"`
	Result          string  `json:"-"`
	PRURL           string  `json:"pr_url"`
	Error           string  `json:"error"`
	CostUSD         float64 `json:"cost_usd"`
	TokensIn        int     `json:"tokens_in"`
	TokensOut       int     `json:"tokens_out"`
	LLMKeyHash      string  `json:"-"`
	llmKeyEnc       []byte
	// KeyOwner is whose model key the job runs on: keyOwnerPlatform, or keyOwnerOrg when the
	// organisation brought its own (model_keys.go). Decided at dispatch and kept, so a job
	// confirmed on one key is never quietly moved to the other.
	KeyOwner string `json:"key_owner"`
	CreatedAt       string `json:"created_at"`
	StartedAt       string `json:"started_at"`
	FinishedAt      string `json:"finished_at"`
}

func jobCols(full bool) string {
	spec, result := "''", "''"
	if full {
		spec, result = "spec", "coalesce(result,'')"
	}
	return `id, status, coalesce(org_id,0), coalesce(team_id,''), channel, thread_ts, coalesce(requester,''), coalesce(approved_by,''), coalesce(approval,''),
		connection_id, repo, coalesce(base_branch,''), coalesce(branch,''), coalesce(title,''), ` + spec + `,
		coalesce(engine,''), coalesce(model,''), budget_usd, timeout_s, draft_pr, coalesce(dispatcher,''), coalesce(execution_ref,''),
		coalesce(token_hash,''), coalesce(token_expires,''), coalesce(claimed_at,''), claim_count, coalesce(worker_info,''),
		coalesce(status_ts,''), coalesce(phase,''), last_seq, coalesce(last_event_at,''),
		cancel_requested, coalesce(cancel_by,''), coalesce(cancel_reason,''), coalesce(cancel_requested_at,''),
		` + result + `, coalesce(pr_url,''), coalesce(error,''), cost_usd, tokens_in, tokens_out, coalesce(llm_key_hash,''), llm_key_enc,
		created_at, coalesce(started_at,''), coalesce(finished_at,''), key_owner`
}

func scanJob(row interface{ Scan(...any) error }) (*Job, error) {
	var j Job
	var draft, cancel int
	if err := row.Scan(&j.ID, &j.Status, &j.OrgID, &j.TeamID, &j.Channel, &j.ThreadTS, &j.Requester, &j.ApprovedBy, &j.Approval,
		&j.ConnectionID, &j.Repo, &j.BaseBranch, &j.Branch, &j.Title, &j.Spec,
		&j.Engine, &j.Model, &j.BudgetUSD, &j.TimeoutS, &draft, &j.Dispatcher, &j.ExecutionRef,
		&j.TokenHash, &j.TokenExpires, &j.ClaimedAt, &j.ClaimCount, &j.WorkerInfo,
		&j.StatusTS, &j.Phase, &j.LastSeq, &j.LastEventAt,
		&cancel, &j.CancelBy, &j.CancelReason, &j.CancelAt,
		&j.Result, &j.PRURL, &j.Error, &j.CostUSD, &j.TokensIn, &j.TokensOut, &j.LLMKeyHash, &j.llmKeyEnc,
		&j.CreatedAt, &j.StartedAt, &j.FinishedAt, &j.KeyOwner); err != nil {
		return nil, err
	}
	j.DraftPR, j.CancelRequested = draft == 1, cancel == 1
	return &j, nil
}

func (s *Store) InsertJob(ctx context.Context, j *Job) (int64, error) {
	draft := 0
	if j.DraftPR {
		draft = 1
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `insert into jobs
		(status, org_id, team_id, channel, thread_ts, requester, approved_by, approval, connection_id, repo, base_branch, branch, title, spec,
		 engine, model, budget_usd, timeout_s, draft_pr, dispatcher, key_owner)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) returning id`,
		nonEmpty(j.Status, jobQueued), j.OrgID, j.TeamID, j.Channel, j.ThreadTS, j.Requester, j.ApprovedBy, j.Approval, j.ConnectionID, j.Repo,
		j.BaseBranch, j.Branch, j.Title, j.Spec, j.Engine, j.Model, j.BudgetUSD, j.TimeoutS, draft, j.Dispatcher,
		nonEmpty(j.KeyOwner, keyOwnerPlatform)).Scan(&id)
	return id, err
}

// Job loads one job with its spec and result bodies, for a caller acting as an organisation.
func (s *Store) Job(ctx context.Context, orgID, id int64) (*Job, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx, `select `+jobCols(true)+` from jobs where org_id=? and id=?`, orgID, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return j, err
}

// jobByID is the one read that is not scoped to an organisation, because its caller does not yet
// know one: the worker container presents a token minted for a single job, and the row is what
// says which organisation that job belongs to. Every call after the token check takes j.OrgID, so
// this is where a request stops being anonymous and starts being one tenant's. Keep it unexported
// and keep requireJobToken its only caller.
func (s *Store) jobByID(ctx context.Context, id int64) (*Job, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx, `select `+jobCols(true)+` from jobs where id=?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return j, err
}

type JobFilter struct {
	Status, Channel string
	Limit           int
}

func (s *Store) queryJobs(ctx context.Context, orgID int64, where string, args []any, limit int) ([]Job, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args = append([]any{orgID}, args...)
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `select `+jobCols(false)+` from jobs where org_id=? `+where+` order by id desc limit ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// Jobs lists newest first without the spec and result bodies.
func (s *Store) Jobs(ctx context.Context, orgID int64, f JobFilter) ([]Job, error) {
	var conds []string
	var args []any
	if f.Status != "" {
		if f.Status == "active" {
			conds = append(conds, `status in (`+placeholders(len(jobActiveStatuses))+`)`)
			for _, st := range jobActiveStatuses {
				args = append(args, st)
			}
		} else {
			conds = append(conds, `status=?`)
			args = append(args, f.Status)
		}
	}
	if f.Channel != "" {
		conds = append(conds, `channel=?`)
		args = append(args, f.Channel)
	}
	where := ""
	if len(conds) > 0 {
		where = `and ` + strings.Join(conds, " and ")
	}
	return s.queryJobs(ctx, orgID, where, args, f.Limit)
}

// ActiveJobs is deliberately every organisation's: the reconciler is one sweep for the whole
// deployment, and each row carries the organisation it belongs to, so what it does next is scoped.
func (s *Store) ActiveJobs(ctx context.Context) ([]Job, error) {
	args := []any{}
	for _, st := range jobActiveStatuses {
		args = append(args, st)
	}
	args = append(args, 500)
	rows, err := s.db.QueryContext(ctx, `select `+jobCols(false)+` from jobs where status in (`+placeholders(len(jobActiveStatuses))+`) order by id desc limit ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

func (s *Store) ActiveJobsInThread(ctx context.Context, orgID int64, teamID, channel, threadTS string) ([]Job, error) {
	args := []any{teamID, channel, threadTS}
	for _, st := range jobActiveStatuses {
		args = append(args, st)
	}
	return s.queryJobs(ctx, orgID, `and team_id=? and channel=? and thread_ts=? and status in (`+placeholders(len(jobActiveStatuses))+`)`, args, 50)
}

// ActiveJobsFor is everything one organisation still has in flight, whatever thread it came
// from. Used when the account is being deleted, which is the one moment the question is asked
// about the whole account rather than about one conversation in it.
func (s *Store) ActiveJobsFor(ctx context.Context, orgID int64) ([]Job, error) {
	args := []any{}
	for _, st := range jobActiveStatuses {
		args = append(args, st)
	}
	return s.queryJobs(ctx, orgID, `and status in (`+placeholders(len(jobActiveStatuses))+`)`, args, 200)
}

// CountActiveJobsFor counts one organisation's in-flight jobs. WorkerMaxJobs is a per-
// organisation setting — the console presents it as this tenant's limit — so comparing it
// against a deployment-wide total meant one busy tenant could hold every other tenant at their
// own limit without either of them having done anything wrong.
func (s *Store) CountActiveJobsFor(ctx context.Context, orgID int64) int {
	var n int
	args := []any{orgID}
	for _, st := range jobActiveStatuses {
		args = append(args, st)
	}
	s.db.QueryRowContext(ctx, `select count(*) from jobs where org_id=? and status in (`+placeholders(len(jobActiveStatuses))+`)`, args...).Scan(&n)
	return n
}

// JobsThisMonth counts one organisation's fix jobs created this calendar month, UTC, on the same
// boundary as MonthSpend. Every job counts — one that failed or was cancelled still ran a
// container — and the figure is shown against sizeJobLimits, never enforced.
func (s *Store) JobsThisMonth(ctx context.Context, orgID int64) int {
	start := time.Now().UTC().Format("2006-01") + "-01 00:00:00"
	var n int
	s.db.QueryRowContext(ctx, `select count(*) from jobs where org_id=? and created_at >= ?`, orgID, start).Scan(&n)
	return n
}

// CountActiveJobs counts the whole deployment's in-flight jobs. It guards the worker pool, which
// really is shared, and is deliberately not the check behind WorkerMaxJobs.
func (s *Store) CountActiveJobs(ctx context.Context) int {
	var n int
	args := []any{}
	for _, st := range jobActiveStatuses {
		args = append(args, st)
	}
	s.db.QueryRowContext(ctx, `select count(*) from jobs where status in (`+placeholders(len(jobActiveStatuses))+`)`, args...).Scan(&n)
	return n
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// jobFields are the columns SetJobFields and SetJobStatus may write.
var jobFields = map[string]bool{"status_ts": true, "execution_ref": true, "dispatcher": true, "phase": true, "llm_key_hash": true,
	"llm_key_enc": true, "error": true, "branch": true, "spec": true, "token_hash": true, "token_expires": true, "finished_at": true,
	"started_at": true, "worker_info": true, "last_event_at": true, "cost_usd": true}

func (s *Store) SetJobFields(ctx context.Context, orgID, id int64, set map[string]any) error {
	if len(set) == 0 {
		return nil
	}
	var cols []string
	var args []any
	for k, v := range set {
		if !jobFields[k] {
			return fmt.Errorf("bad job field %q", k)
		}
		cols = append(cols, k+"=?")
		args = append(args, v)
	}
	args = append(args, orgID, id)
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`update jobs set %s where org_id=? and id=?`, strings.Join(cols, ", ")), args...)
	return err
}

// SetJobStatus moves a job to a new status only if it is in one of from (compare-and-set), and
// reports whether it did.
func (s *Store) SetJobStatus(ctx context.Context, orgID, id int64, from []string, to string, set map[string]any) (bool, error) {
	cols := []string{"status=?"}
	args := []any{to}
	for k, v := range set {
		if !jobFields[k] {
			return false, fmt.Errorf("bad job field %q", k)
		}
		cols = append(cols, k+"=?")
		args = append(args, v)
	}
	q := fmt.Sprintf(`update jobs set %s where org_id=? and id=?`, strings.Join(cols, ", "))
	args = append(args, orgID, id)
	if len(from) > 0 {
		q += ` and status in (` + placeholders(len(from)) + `)`
		for _, f := range from {
			args = append(args, f)
		}
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ClaimJob hands a dispatched job to the worker that presents its token. A lost response may
// make an honest worker claim twice, so up to three claims are allowed; anything else is refused
// with a reason the API turns into 409/410.
func (s *Store) ClaimJob(ctx context.Context, orgID, id int64, workerInfo string) (*Job, string, error) {
	j, err := s.Job(ctx, orgID, id)
	if err != nil {
		return nil, "", err
	}
	switch {
	case j == nil:
		return nil, "no such job", nil
	case jobTerminal(j.Status):
		return nil, "finished", nil
	case j.CancelRequested || j.Status == jobCancelling:
		return nil, "cancelled", nil
	case j.Status != jobStarting && j.Status != jobRunning && j.Status != jobStale:
		return nil, "not dispatched", nil
	case j.ClaimCount >= 3:
		return nil, "already claimed", nil
	}
	n := now()
	res, err := s.db.ExecContext(ctx, `update jobs set status='running', claimed_at=coalesce(nullif(claimed_at,''), ?),
		started_at=coalesce(nullif(started_at,''), ?), claim_count=claim_count+1, last_event_at=?, worker_info=?
		where org_id=? and id=? and status in ('starting','running','stale') and claim_count<3 and cancel_requested=0`, n, n, n, workerInfo, orgID, id)
	if err != nil {
		return nil, "", err
	}
	if k, _ := res.RowsAffected(); k == 0 {
		return nil, "already claimed", nil
	}
	j, err = s.Job(ctx, orgID, id)
	return j, "", err
}

// RequestJobCancel flags a job for cancellation. A queued job is finished by the caller; anything
// dispatched moves to cancelling and the worker learns on its next event.
func (s *Store) RequestJobCancel(ctx context.Context, orgID, id int64, by, reason string) (bool, error) {
	args := []any{by, reason, now(), orgID, id}
	for _, st := range jobActiveStatuses {
		args = append(args, st)
	}
	res, err := s.db.ExecContext(ctx, `update jobs set cancel_requested=1, cancel_by=?, cancel_reason=?, cancel_requested_at=?,
		status=case when status in ('starting','running','stale') then 'cancelling' else status end
		where org_id=? and id=? and status in (`+placeholders(len(jobActiveStatuses))+`) and cancel_requested=0`, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// AddJobEvents stores a batch, ignoring any seq already seen, and reports how many were new and
// the usage those new ones carried. A stale job that speaks again is running again.
func (s *Store) AddJobEvents(ctx context.Context, orgID, id int64, evs []JobEvent) (int, JobUsage, error) {
	accepted := 0
	var used JobUsage
	var maxSeq int64
	phase := ""
	for _, e := range evs {
		data := string(e.Data)
		if strings.TrimSpace(data) == "" {
			data = "{}"
		}
		var in, out int
		var cost float64
		if e.Usage != nil {
			in, out, cost = e.Usage.In, e.Usage.Out, e.Usage.CostUSD
		}
		res, err := s.db.ExecContext(ctx, `insert into job_events (org_id, job_id, seq, kind, phase, status, message, data, at, tokens_in, tokens_out, cost_usd)
			values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) on conflict do nothing`, orgID, id, e.Seq, e.Kind, e.Phase, e.Status, e.Message, data, e.At, in, out, cost)
		if err != nil {
			return accepted, used, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		accepted++
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
		if e.Kind == JobKindPhase && e.Phase != "" {
			phase = e.Phase
		}
		if in > 0 || out > 0 || cost > 0 {
			used.In, used.Out, used.CostUSD = used.In+in, used.Out+out, used.CostUSD+cost
		}
	}
	if accepted == 0 {
		return 0, used, nil
	}
	setPhase := ""
	// `case when` rather than max(last_seq, ?): SQLite's max takes two scalars, Postgres's
	// takes a column and calls the two-argument form greatest, and neither knows the other's
	// spelling. The sequence number is therefore passed twice.
	args := []any{maxSeq, maxSeq, now(), used.CostUSD, used.In, used.Out}
	if phase != "" {
		setPhase = ", phase=?"
		args = append(args, phase)
	}
	args = append(args, orgID, id)
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`update jobs set
		last_seq=case when last_seq > ? then last_seq else ? end, last_event_at=?,
		status=case when status='stale' then 'running' else status end,
		cost_usd=cost_usd+?, tokens_in=tokens_in+?, tokens_out=tokens_out+?%s where org_id=? and id=?`, setPhase), args...)
	return accepted, used, err
}

// FinishJob moves a job to a terminal status once. The result's usage is the job total; what
// comes back is the part not yet counted from events, so the caller logs it exactly once.
func (s *Store) FinishJob(ctx context.Context, orgID, id int64, status string, res *JobResult) (JobUsage, bool, error) {
	var cur string
	var cost float64
	var in, out int
	err := s.db.QueryRowContext(ctx, `select status, cost_usd, tokens_in, tokens_out from jobs where org_id=? and id=?`, orgID, id).Scan(&cur, &cost, &in, &out)
	if err == sql.ErrNoRows {
		return JobUsage{}, false, nil
	}
	if err != nil {
		return JobUsage{}, false, err
	}
	if jobTerminal(cur) {
		return JobUsage{}, false, nil
	}
	var delta JobUsage
	raw, prURL, branch, errText := "", "", "", ""
	if res != nil {
		if res.Usage.CostUSD > cost {
			delta.CostUSD, cost = res.Usage.CostUSD-cost, res.Usage.CostUSD
		}
		if res.Usage.In > in {
			delta.In, in = res.Usage.In-in, res.Usage.In
		}
		if res.Usage.Out > out {
			delta.Out, out = res.Usage.Out-out, res.Usage.Out
		}
		b, _ := json.Marshal(res)
		raw = string(b)
		if res.PR != nil {
			prURL = res.PR.URL
		}
		branch = res.Branch
		errText = strings.TrimSpace(strings.TrimSpace(res.Error.Code + ": " + res.Error.Message))
		errText = strings.TrimPrefix(errText, ": ")
		errText = strings.TrimSuffix(errText, ":")
		errText = truncate(errText, 500)
	}
	r, err := s.db.ExecContext(ctx, `update jobs set status=?, result=?, pr_url=case when ?<>'' then ? else pr_url end,
		branch=case when ?<>'' then ? else branch end, error=?, cost_usd=?, tokens_in=?, tokens_out=?, finished_at=?
		where org_id=? and id=? and status not in ('succeeded','failed','cancelled','timeout')`,
		status, raw, prURL, prURL, branch, branch, errText, cost, in, out, now(), orgID, id)
	if err != nil {
		return delta, false, err
	}
	n, _ := r.RowsAffected()
	return delta, n == 1, nil
}

func (s *Store) JobEvents(ctx context.Context, orgID, id int64, limit int) ([]JobEvent, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `select id, seq, coalesce(at,''), kind, coalesce(phase,''), coalesce(status,''), coalesce(message,''),
		coalesce(data,'{}'), tokens_in, tokens_out, cost_usd, created_at from job_events where org_id=? and job_id=? order by seq limit ?`, orgID, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []JobEvent{}
	for rows.Next() {
		var e JobEvent
		var data string
		var in, out2 int
		var cost float64
		if err := rows.Scan(&e.ID, &e.Seq, &e.At, &e.Kind, &e.Phase, &e.Status, &e.Message, &data, &in, &out2, &cost, &e.CreatedAt); err != nil {
			return nil, err
		}
		if data != "" && data != "{}" {
			e.Data = json.RawMessage(data)
		}
		if in > 0 || out2 > 0 || cost > 0 {
			e.Usage = &JobUsage{In: in, Out: out2, CostUSD: cost}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- job files (diff, log) ----

func (s *Store) PutJobFile(ctx context.Context, orgID, id int64, kind, content string) error {
	if _, err := s.db.ExecContext(ctx, `delete from job_files where org_id=? and job_id=? and kind=?`, orgID, id, kind); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `insert into job_files (org_id, job_id, kind, bytes, content) values (?, ?, ?, ?, ?)`, orgID, id, kind, len(content), content)
	return err
}

func (s *Store) JobFile(ctx context.Context, orgID, id int64, kind string) (content, fileID, permalink string, err error) {
	err = s.db.QueryRowContext(ctx, `select content, coalesce(file_id,''), coalesce(permalink,'') from job_files where org_id=? and job_id=? and kind=?`, orgID, id, kind).
		Scan(&content, &fileID, &permalink)
	if err == sql.ErrNoRows {
		return "", "", "", nil
	}
	return
}

func (s *Store) SetJobFileLink(ctx context.Context, orgID, id int64, kind, fileID, permalink string) {
	s.db.ExecContext(ctx, `update job_files set file_id=?, permalink=? where org_id=? and job_id=? and kind=?`, fileID, permalink, orgID, id, kind)
}

// PurgeJobData applies only this organization's retention policy, atomically.
func (s *Store) PurgeJobData(ctx context.Context, orgID int64, keep time.Duration) (int64, error) {
	cutoff := time.Now().Add(-keep).UTC().Format(time.DateTime)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `delete from job_events where org_id=? and job_id in (select id from jobs where org_id=? and finished_at<>'' and finished_at < ?)`, orgID, orgID, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `delete from job_files where org_id=? and job_id in (select id from jobs where org_id=? and finished_at<>'' and finished_at < ?)`, orgID, orgID, cutoff); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// RevokeJobTokens forgets the tokens of jobs that finished more than grace ago, so a worker that
// keeps talking after its result gets 401s rather than silently accepted events. Deployment-wide:
// an expired credential must expire for everyone.
func (s *Store) RevokeJobTokens(ctx context.Context, grace time.Duration) {
	cutoff := time.Now().Add(-grace).UTC().Format(time.DateTime)
	s.db.ExecContext(ctx, `update jobs set token_hash='', llm_key_enc=null where token_hash<>'' and status in ('succeeded','failed','cancelled','timeout') and finished_at<>'' and finished_at < ?`, cutoff)
}

// ThreadToolResults returns recent successful tool calls in a thread whose name starts with one
// of prefixes, newest first: the evidence the bot gathered before someone asked for a fix.
func (s *Store) ThreadToolResults(ctx context.Context, teamID, channel, threadTS string, prefixes []string, limit int) ([]ToolCallRow, error) {
	rows, err := s.db.QueryContext(ctx, toolCallCols+` where team_id=? and channel=? and thread_ts=? and ok=1 order by id desc limit 50`, teamID, channel, threadTS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ToolCallRow
	for rows.Next() {
		t, err := scanToolCall(rows)
		if err != nil {
			return nil, err
		}
		for _, p := range prefixes {
			if strings.HasPrefix(t.Name, p) && strings.TrimSpace(t.Result) != "" {
				out = append(out, t)
				break
			}
		}
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}
