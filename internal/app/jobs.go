package app

import (
	"cloud.google.com/go/storage"

	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// "Fix it and raise a PR". The bot never runs code itself: start_fix_job composes a brief from
// the thread, a human confirms it, and the JobRunner hands it to a separate worker container
// (whatever WORKER_MODE resolved to — a Cloud Run Job, a Fargate task, a Container Apps job, a
// Kubernetes Job, a container on the host daemon, or a subprocess of this binary in
// development) that clones the repository, changes it, runs the tests, pushes a branch and
// opens a draft pull request. The
// worker calls back over /api/worker/jobs/{id}/* (jobs_api.go); the runner keeps one checklist
// message in the thread current and posts the report at the end (jobs_slack.go). Nothing secret
// reaches the container's environment: the execution carries only the job id, the bot's URL and
// a single-use token; the spec, the repository token and the model key travel back over the
// claim call.

type JobRunner struct {
	cfg      Config
	store    *Store
	slacks   *ChatRegistry // a job posts back into its own workspace's thread
	proxy    *Proxy
	settings *settingsCache
	agent    *Agent // alerts and the allow-rule checker; set by Run after NewAgent

	mu          sync.Mutex
	dispatchers map[string]Dispatcher
	updaters    map[int64]*time.Timer // pending checklist edits, one per job (jobs_slack.go)
	keys        *openRouterKeys
	gcs         *storage.Client // signs the dependency-cache URLs (jobs_cache.go)
}

func NewJobRunner(cfg Config, st *Store, slacks *ChatRegistry, px *Proxy, sc *settingsCache) *JobRunner {
	r := &JobRunner{cfg: cfg, store: st, slacks: slacks, proxy: px, settings: sc,
		dispatchers: map[string]Dispatcher{}, updaters: map[int64]*time.Timer{}, keys: newOpenRouterKeys(cfg.OpenRouterProvisioningKey)}
	if cfg.WorkerDispatcher() == "local" {
		bin, _ := os.Executable()
		r.dispatchers["local"] = newLocalDispatcher(bin)
	}
	return r
}

// Enabled reports whether jobs can be dispatched at all: WORKER_MODE named somewhere to run
// them, and — for `workers` — the environment said which platform that is.
func (r *JobRunner) Enabled() bool {
	return r != nil && r.cfg.WorkerDispatcher() != ""
}

func (r *JobRunner) dispatcher(ctx context.Context) (Dispatcher, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := r.cfg.WorkerDispatcher()
	if d, ok := r.dispatchers[key]; ok {
		return d, nil
	}
	d, err := newDispatcherFor(ctx, r.cfg, key)
	if err != nil {
		return nil, err
	}
	r.dispatchers[key] = d
	return d, nil
}

// botURL is where the worker calls back: the public console URL (ADMIN_BASE_URL or the
// learned origin, public_origin.go), else this process.
func (r *JobRunner) botURL() string { return publicBaseURL(context.Background(), r.store, r.cfg) }

// ---- tokens and names ----

// A job token is atj1.<job id>.<32 random bytes>: the id lets the API find the row without a
// scan, the prefix lets redact catch it, and only its sha256 is stored.
func mintJobToken(id int64) string {
	b := make([]byte, 32)
	rand.Read(b)
	return fmt.Sprintf("atj1.%d.%s", id, base64.RawURLEncoding.EncodeToString(b))
}

func hashJobToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func parseJobToken(tok string) (int64, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 || parts[0] != "atj1" || len(parts[2]) < 40 {
		return 0, false
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	return id, err == nil && id > 0
}

var branchSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

func branchSlug(s string, max int) string {
	s = strings.Trim(branchSlugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > max {
		s = strings.TrimRight(s[:max], "-")
	}
	return s
}

// defaultBranchSuffix is what a branch ends with when the console has not been told otherwise.
const defaultBranchSuffix = "attest_tag"

// The kinds of change a job can be. They are the vocabulary the branch convention is written
// in — a house rule that says a branch must start with feature/ or bugfix/ is naming these.
const (
	jobKindBugfix  = "bugfix"
	jobKindFeature = "feature"
	jobKindHotfix  = "hotfix"
)

// defaultBranchPrefix is the convention most teams already keep, so the worker's branches land
// where their branch protection, their filters and their eyes already look. An admin who wants
// no prefix at all sets the setting to "none" — empty means "unset", which is the default.
const defaultBranchPrefix = "feature/, bugfix/, hotfix/"

// normalizeJobKind keeps the model's word inside the vocabulary; anything unrecognised is a bug
// fix, which is what the worker is mostly for and the safest of the three to guess. hotfix is
// tested first because it also contains "fix".
func normalizeJobKind(s string) string {
	switch k := strings.ToLower(strings.TrimSpace(s)); {
	case strings.Contains(k, "hotfix"), strings.Contains(k, "hot-fix"), strings.Contains(k, "hot fix"):
		return jobKindHotfix
	case strings.Contains(k, "feat"):
		return jobKindFeature
	default:
		return jobKindBugfix
	}
}

// branchPrefixes splits the worker_branch_prefix setting into the conventions on offer. A list —
// "feature/, bugfix/, hotfix/" — is a house rule that names a prefix per kind of change and says
// that one of them must be present. The word "none" is how an organisation says it has no such
// rule, since an empty setting means "unset" and takes the default.
func branchPrefixes(s string) []string {
	if t := strings.TrimSpace(s); t == "" || strings.EqualFold(t, "none") {
		return nil
	}
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// pickBranchPrefix chooses the convention that fits this change. With one on offer it is that
// one. With several, it is the one whose leading segment names the kind — feature/ for a
// feature, bugfix/ for a bug, hotfix/ for a hotfix — and failing that the first, because a rule
// that lists prefixes means one of them is required, not that none applies.
func pickBranchPrefix(setting, kind string) string {
	list := branchPrefixes(setting)
	if len(list) == 0 {
		return ""
	}
	// Normalising here and not only at the tool means a spec that never said what kind of change
	// it is — an older row, an API caller — is treated as the bug fix it probably is, rather than
	// taking whichever prefix happens to be listed first.
	kind = normalizeJobKind(kind)
	for _, p := range list {
		seg := strings.ToLower(strings.Trim(strings.SplitN(p, "/", 2)[0], "-_."))
		if seg != "" && (strings.Contains(seg, kind) || strings.Contains(kind, seg)) {
			return p
		}
	}
	return list[0]
}

// jobBranch is the only ref the worker may push: <prefix>fix-<id>-<slug><suffix>. The prefix is
// the organisation's own convention (hotfix/, feature/) and may be empty; the suffix is the mark
// that says the branch is the bot's, and is never empty.
func jobBranch(prefix, suffix string, id int64, title string) string {
	return jobBranchID(prefix, suffix, strconv.FormatInt(id, 10), title)
}

// jobBranchID is jobBranch with the id already written out, so the card people read before
// pressing Confirm can show the shape of the branch while the job still has no id.
func jobBranchID(prefix, suffix, id, title string) string {
	// Neither separator is assumed: a prefix that does not end in one gets a slash, a suffix
	// that does not start with one gets a dash, so the two never run into the slug.
	if prefix != "" && !strings.HasSuffix(prefix, "/") && !strings.HasSuffix(prefix, "-") && !strings.HasSuffix(prefix, "_") {
		prefix += "/"
	}
	if suffix == "" {
		suffix = defaultBranchSuffix
	}
	if !strings.HasPrefix(suffix, "-") && !strings.HasPrefix(suffix, "_") && !strings.HasPrefix(suffix, ".") {
		suffix = "-" + suffix
	}
	sl := branchSlug(title, 40)
	if sl == "" {
		sl = "change"
	}
	return fmt.Sprintf("%sfix-%s-%s%s", prefix, id, sl, suffix)
}

// ---- dispatch ----

type DispatchRequest struct {
	OrgID      int64  // the organisation whose budget and connections it spends
	TeamID     string // the workspace whose thread asked for it, and where the result goes back
	Spec       JobSpec
	ApprovedBy string // Slack user who confirmed; empty when a rule did
	Approval   string // "confirm" | "rule:<text>"
}

// engineMaxRounds is the engine's turn cap per job. 60 was too few for a real change on a
// repository with a large system prompt: job #2 hit it and shipped a partial pull request.
const engineMaxRounds = 500

// constraints snapshots the worker settings into the spec at dispatch. kind is the job's own
// kind of change, which chooses among the branch prefixes the organisation allows.
func (r *JobRunner) constraints(st Settings, conn *Connection, kind string) JobConstraints {
	model := st.WorkerModel
	if model == "" {
		model = st.HeavyModel
	}
	if model == "" {
		model = st.Model
	}
	// DraftPR is not a choice: the worker opens every pull request as a draft whatever the spec
	// says (internal/worker/run.go). It is recorded so the job's row says what was opened.
	c := JobConstraints{Engine: st.WorkerEngine, Model: model, BudgetUSD: st.WorkerJobBudgetUSD, TimeoutS: st.WorkerTimeoutMinutes * 60,
		DraftPR: true, BranchPrefix: pickBranchPrefix(st.WorkerBranchPrefix, kind), BranchSuffix: st.WorkerBranchSuffix,
		MaxRounds: engineMaxRounds}
	if conn != nil {
		c.TestCmd = conn.TestCmd
		// Only a recipe somebody set steers the worker. One an earlier job worked out and
		// rememberRecipe kept is about whatever package that job was about; handed on, it
		// overrode detection and pinned every later job in a monorepo to the first one's folder.
		if conn.Recipe.SetByAdmin() {
			c.Recipe = conn.Recipe
		}
		if clamped := clampJobTimeout(c.TimeoutS, conn); clamped != c.TimeoutS {
			slog.Info("job timeout clamped to the life of a GitHub App token",
				"repo", conn.Repo, "setting_s", c.TimeoutS, "timeout_s", clamped)
			c.TimeoutS = clamped
		}
	}
	return c
}

// jobTimeoutCeiling is the longest an app-backed job may run. A GitHub App installation token is
// good for an hour; this leaves ten minutes of that for the push and the pull request, which
// happen at the very end. A job that outlived its token would do all the work and then fail to
// deliver it, which is the one failure worth spending a setting to avoid.
const jobTimeoutCeiling = 50 * 60

// clampJobTimeout holds an app-backed job inside the life of the token it will be handed. A
// repository connected with a pasted token has no such ceiling: a PAT outlives any job.
func clampJobTimeout(timeoutS int, conn *Connection) int {
	if conn == nil || (conn.CredType != "github_app" && conn.GitHubInstallationID == 0) {
		return timeoutS
	}
	if timeoutS > jobTimeoutCeiling {
		return jobTimeoutCeiling
	}
	return timeoutS
}

// budgetRoom refuses a job the monthly budgets could not absorb.
func (r *JobRunner) budgetRoom(ctx context.Context, orgID int64, teamID, channel string, jobBudget float64) error {
	st := r.settings.Get(ctx, orgID)
	// A fix job is the one workload big enough to matter here: it runs in another container for
	// up to worker_timeout_minutes and is only cut off above its own budget, so it is the thing
	// most able to spend credit that is not there. Reserved against the balance the same way it
	// is reserved against the month.
	// Not on the organisation's own key, which this job will run on: nothing is drawn from credit.
	ownKey := st.OwnKey.Active()
	if st.BillingUnknown && !ownKey {
		return errors.New("credit accounting is unavailable, so this job has not been started")
	}
	if (st.CreditEnforced || st.AllowanceActive) && !ownKey {
		bal, metered, err := r.store.SpendableCredit(ctx, orgID)
		if err != nil {
			return errors.New("credit accounting is unavailable, so this job has not been started")
		}
		// The whole of what is spendable, the plan's monthly allowance included: a job reserved
		// only against prepaid credit would be refused on an account whose plan covers it.
		if metered && microsToUSD(bal) < jobBudget {
			return fmt.Errorf("this account has %s of credit left, less than the job budget of $%.2f; top up under Settings → Billing",
				creditAmount(bal), jobBudget)
		}
	}
	// The effective budget, not the setting: a free account's setting is not what it may spend.
	if budget := st.EffectiveBudget(); budget > 0 {
		spent, _ := budgetSpend(ctx, r.store, orgID, st)
		if spent+jobBudget > budget {
			err := fmt.Errorf("the account has $%.2f of its $%.2f monthly budget left, less than the job budget of $%.2f", budget-spent, budget, jobBudget)
			if hint := st.raiseBudgetHint(); hint != "" {
				err = fmt.Errorf("%w. %s", err, hint)
			}
			return err
		}
	}
	if sc, _ := r.store.ChannelScope(ctx, orgID, teamID, channel); sc != nil && sc.MonthlyBudgetUSD > 0 {
		spent, _ := r.store.MonthSpend(ctx, orgID, teamID, channel)
		if spent+jobBudget > sc.MonthlyBudgetUSD {
			return fmt.Errorf("this channel has $%.2f of its $%.2f monthly budget left, less than the job budget of $%.2f", sc.MonthlyBudgetUSD-spent, sc.MonthlyBudgetUSD, jobBudget)
		}
	}
	return nil
}

// precheck is everything that can refuse a job before anyone is asked to confirm it.
func (r *JobRunner) precheck(ctx context.Context, orgID int64, teamID, channel, threadTS string) error {
	if !r.Enabled() {
		if r.cfg.WorkerMode == "workers" {
			return errors.New("the fix worker is on, but the server could not tell which platform it is running on; an admin can name it with WORKER_MODE (cloudrun, ecs, aca, k8s or docker)")
		}
		return errors.New("the fix worker is switched off (WORKER_MODE=off)")
	}
	st := r.settings.Get(ctx, orgID)
	if active, _ := r.store.ActiveJobsInThread(ctx, orgID, teamID, channel, threadTS); len(active) > 0 {
		return fmt.Errorf("fix job #%d is still running in this thread; say stop first", active[0].ID)
	}
	if n := r.store.CountActiveJobsFor(ctx, orgID); n >= st.WorkerMaxJobs {
		return fmt.Errorf("%d of your fix jobs are already running (the limit is %d); wait for one to finish", n, st.WorkerMaxJobs)
	}
	// The job token travels back in a header, so the callback has to be https wherever it
	// crosses a real network. Named platforms rather than "anything but local": `docker` runs
	// the worker on this same host and reaches the bot over a private bridge, and a mode this
	// list has never heard of is not something to make a security claim about.
	switch r.cfg.WorkerDispatcher() {
	case "cloudrun", "ecs", "aca", "k8s":
		if !strings.HasPrefix(r.botURL(), "https://") {
			return errors.New("the worker needs an https console URL to call back: open the console over https once so the bot learns it, or set ADMIN_BASE_URL")
		}
	}
	if err := r.credentialPolicy(ctx, orgID); err != nil {
		return err
	}
	return r.budgetRoom(ctx, orgID, teamID, channel, st.WorkerJobBudgetUSD)
}

// Dispatch creates the job row, posts the checklist and starts the worker. Called after a human
// confirmed (runPending) or an allow rule approved (the tool).
func (r *JobRunner) Dispatch(ctx context.Context, req DispatchRequest) (*Job, error) {
	spec := req.Spec
	// Which key the budget checks below were made for. The job is recorded on the key read after
	// them, and if the two disagree the checks were for the wrong key — a key removed in between
	// would send a job that skipped the credit floor to the deployment's.
	checkedOwn := r.settings.Get(ctx, req.OrgID).OwnKey.Active()
	if err := r.precheck(ctx, req.OrgID, req.TeamID, spec.Channel, spec.ThreadTS); err != nil {
		return nil, err
	}
	conn, err := r.store.Connection(ctx, req.OrgID, spec.ConnectionID)
	if err != nil || conn == nil || conn.Preset != "github" || conn.Repo == "" {
		return nil, errors.New("the repository connection no longer exists")
	}
	if conn.Status != "active" {
		return nil, fmt.Errorf("the connection for %s is %s", conn.Repo, conn.Status)
	}
	if !strings.EqualFold(conn.Repo, spec.Repo) {
		return nil, fmt.Errorf("the connection is for %s, not %s", conn.Repo, spec.Repo)
	}
	st := r.settings.Get(ctx, req.OrgID)
	spec.V = 1
	spec.Repo = conn.Repo
	spec.Constraints = r.constraints(st, conn, spec.Kind)
	if st.OwnKey.Active() != checkedOwn {
		return nil, errors.New("this organisation's model key changed while the job was being started; start it again")
	}
	keyOwner := keyOwnerPlatform
	if st.OwnKey.Active() {
		// The job runs where the organisation's other model calls go, on a model that endpoint
		// serves: a worker model chosen for the deployment's endpoint would fail there.
		keyOwner = keyOwnerOrg
		if r.agent != nil {
			if l, err := r.agent.llmFor(ctx, req.OrgID); err == nil && l != nil {
				spec.Constraints.Model = l.Servable(ctx, spec.Constraints.Model)
			}
		}
	}
	if spec.BaseBranch == "" {
		spec.BaseBranch = "main"
	}
	raw, _ := json.Marshal(spec)
	j := &Job{Status: jobQueued, OrgID: req.OrgID, TeamID: req.TeamID, Channel: spec.Channel, ThreadTS: spec.ThreadTS, Requester: spec.Requester,
		ApprovedBy: req.ApprovedBy, Approval: req.Approval, ConnectionID: conn.ID, Repo: conn.Repo, BaseBranch: spec.BaseBranch,
		Title: spec.Title, Spec: string(raw), Engine: spec.Constraints.Engine, Model: spec.Constraints.Model,
		BudgetUSD: spec.Constraints.BudgetUSD, TimeoutS: spec.Constraints.TimeoutS, DraftPR: spec.Constraints.DraftPR, Dispatcher: r.cfg.WorkerDispatcher(),
		KeyOwner: keyOwner}
	id, err := r.store.InsertJob(ctx, j)
	if err != nil {
		return nil, err
	}
	j.ID = id
	spec.Branch = jobBranch(spec.Constraints.BranchPrefix, spec.Constraints.BranchSuffix, id, spec.Title)
	j.Branch = spec.Branch
	raw, _ = json.Marshal(spec)
	tok := mintJobToken(id)
	expires := time.Now().Add(time.Duration(j.TimeoutS)*time.Second + 15*time.Minute).UTC().Format(time.DateTime)
	if err := r.store.SetJobFields(ctx, j.OrgID, id, map[string]any{"branch": spec.Branch, "spec": string(raw), "token_hash": hashJobToken(tok), "token_expires": expires}); err != nil {
		return nil, err
	}
	j.Spec = string(raw)
	sl, slErr := r.slacks.For(ctx, j.TeamID)
	if slErr != nil {
		return nil, fmt.Errorf("this workspace is no longer connected: %w", slErr)
	}
	if ts, err := sl.PostMarkdown(ctx, j.Channel, j.ThreadTS, jobChecklist(j, nil), jobFooter(j)); err == nil {
		j.StatusTS = ts
		r.store.SetJobFields(ctx, j.OrgID, id, map[string]any{"status_ts": ts})
	} else {
		slog.Warn("job checklist post failed", "job", id, "err", err)
	}
	d, err := r.dispatcher(ctx)
	if err == nil {
		var ref string
		ref, err = d.Start(ctx, j, JobLaunch{JobID: id, BotURL: r.botURL(), Token: tok, Target: r.workerFor(conn)})
		if err == nil {
			r.store.SetJobStatus(ctx, j.OrgID, id, []string{jobQueued}, jobStarting, map[string]any{"execution_ref": ref, "dispatcher": d.Name()})
			j.Status, j.ExecutionRef, j.Dispatcher = jobStarting, ref, d.Name()
		}
	}
	if err != nil {
		r.finish(ctx, j.OrgID, id, JobFailed, &JobResult{Status: JobFailed, Error: JobError{Code: "dispatch_failed", Message: err.Error()}})
		return j, fmt.Errorf("could not start the worker: %w", err)
	}
	by := req.ApprovedBy
	if by == "" {
		by = "an allow rule"
	} else {
		by = "<@" + by + ">"
	}
	r.store.AddTurn(ctx, j.TeamID, j.Channel, j.ThreadTS, "note", spec.Requester,
		fmt.Sprintf("Fix job #%d dispatched to the worker for %s (branch %s), approved by %s. Its checklist message in this thread shows progress; the result is posted when it finishes.", id, j.Repo, j.Branch, by), "", 0, 0)
	slog.Info("job dispatched", "job", id, "repo", j.Repo, "branch", j.Branch, "dispatcher", j.Dispatcher, "execution", j.ExecutionRef, "channel", j.Channel)
	r.refresh(j.OrgID, id)
	return j, nil
}

// ---- cancel ----

// CancelInThread cancels the active jobs of a thread (a bare "stop", !stop, or a Cancel button).
func (r *JobRunner) CancelInThread(ctx context.Context, orgID int64, teamID, channel, threadTS, by string) int {
	if r == nil {
		return 0
	}
	jobs, _ := r.store.ActiveJobsInThread(ctx, orgID, teamID, channel, threadTS)
	n := 0
	for _, j := range jobs {
		if r.cancel(ctx, &j, by, "user") {
			n++
		}
	}
	return n
}

// CancelForOrg stops everything one organisation has in flight, for the account that is being
// deleted. The reason is not "user", so no thread is told: the workspace it would be told in is
// being disconnected in the same breath.
func (r *JobRunner) CancelForOrg(ctx context.Context, orgID int64, why string) int {
	if r == nil {
		return 0
	}
	jobs, _ := r.store.ActiveJobsFor(ctx, orgID)
	n := 0
	for _, j := range jobs {
		if r.cancel(ctx, &j, "", why) {
			n++
		}
	}
	return n
}

// cancel asks a job to stop. Nothing dispatched yet ends now; a running worker is told on its
// next event and the reconciler kills the execution if it does not answer within 90 s.
func (r *JobRunner) cancel(ctx context.Context, j *Job, by, reason string) bool {
	if jobTerminal(j.Status) {
		return false
	}
	if j.Status == jobQueued {
		r.finish(ctx, j.OrgID, j.ID, JobCancelled, &JobResult{Status: JobCancelled, Error: JobError{Code: "cancelled", Message: "cancelled before the worker started"}})
		return true
	}
	ok, err := r.store.RequestJobCancel(ctx, j.OrgID, j.ID, by, reason)
	if err != nil || !ok {
		return false
	}
	slog.Info("job cancel requested", "job", j.ID, "by", by, "reason", reason)
	if j.Status == jobStarting { // never claimed: nothing to wait for
		if d, err := r.dispatcher(ctx); err == nil && j.ExecutionRef != "" {
			d.Cancel(ctx, j.ExecutionRef)
		}
		r.finish(ctx, j.OrgID, j.ID, JobCancelled, &JobResult{Status: JobCancelled, Error: JobError{Code: "cancelled", Message: "cancelled before the worker claimed the job"}})
		return true
	}
	r.refresh(j.OrgID, j.ID)
	if by != "" && reason == "user" {
		if sl, err := r.slacks.For(ctx, j.TeamID); err == nil {
			sl.PostText(ctx, j.Channel, j.ThreadTS, fmt.Sprintf("Cancelling fix job #%d… the worker stops at its next step and nothing more is pushed.", j.ID))
		}
	}
	return true
}

// ---- finish ----

// finish is the single exit for every terminal transition: the worker's result, a cancel, a
// timeout, a reconciler verdict. It records the outcome once, settles cost, updates the
// checklist and posts the report.
func (r *JobRunner) finish(ctx context.Context, orgID, id int64, status string, res *JobResult) {
	if res == nil {
		res = &JobResult{}
	}
	res.Status = status
	delta, ok, err := r.store.FinishJob(ctx, orgID, id, status, res)
	if err != nil {
		slog.Error("finish job", "job", id, "err", err)
		return
	}
	if !ok {
		return // already terminal
	}
	r.cancelUpdater(id)
	j, err := r.store.Job(ctx, orgID, id)
	if err != nil || j == nil {
		return
	}
	if extra := r.settleKey(ctx, j); extra > 0 {
		delta.CostUSD += extra
		j.CostUSD += extra
	}
	if delta.In > 0 || delta.Out > 0 || delta.CostUSD > 0 {
		r.store.LogUsageBy(ctx, j.OrgID, j.TeamID, j.Channel, j.ThreadTS, j.Requester, "worker:"+j.Engine+"/"+j.Model, delta.usageOn(j))
	}
	r.rememberRecipe(ctx, j, res)
	r.renderChecklist(ctx, j)
	note := r.postReport(ctx, j, res)
	r.store.AddTurn(ctx, j.TeamID, j.Channel, j.ThreadTS, "note", j.Requester, truncate(oneLine(note), 1500), "", 0, 0)
	if status != JobSucceeded && r.agent != nil {
		r.agent.alert(ctx, j.OrgID, fmt.Sprintf("job:%d:%s", id, status), fmt.Sprintf(":warning: Fix job #%d on %s in <#%s> %s: %s", id, j.Repo, j.Channel, status, truncate(j.Error, 200)))
	}
	slog.Info("job finished", "job", id, "status", status, "repo", j.Repo, "pr", j.PRURL, "cost_usd", fmt.Sprintf("%.4f", j.CostUSD),
		"in", j.TokensIn, "out", j.TokensOut, "error", j.Error)
}

// errNoWorkerKey is why no fix job can start when the bot has no model credential to hand a
// worker at all. The asker reads it in Slack and the operator in the console, so it names both
// knobs and which one is the safe choice.
var errNoWorkerKey = errors.New("no model credential for the fix worker: set OPENROUTER_PROVISIONING_KEY (a key minted per job and capped at its budget, the safe choice) or OPENROUTER_API_KEY / WORKER_LLM_API_KEY (one shared key, uncapped)")

// errFixJobsOffOwnKey is why an organisation on its own model key cannot run a fix job: the key
// would sit in a container running the repository's own code, which is its owner's call to make.
var errFixJobsOffOwnKey = errors.New("fix jobs are off for this organisation's own model key: a job runs the repository's own code with the key in reach, so an admin turns them on under Settings → Models")

// errSharedKeyOpenSignup refuses a fix job that would run on the shared, uncapped platform key
// when anyone can sign up. The key ends up in the engine's environment, and the engine runs the
// job's own repository code, so a stranger who signs up and points a job at a repository they
// control can read it out. A per-job key (OPENROUTER_PROVISIONING_KEY) is capped and disposable;
// an organisation's own key never touches the shared credential. Either closes the hole; a
// trusted single-tenant deployment that is not open to signup keeps the shared-key fallback.
var errSharedKeyOpenSignup = errors.New("this deployment is open to signup, so a fix job may not run on the shared, uncapped model key (a stranger's repository code could read it): set OPENROUTER_PROVISIONING_KEY (a key minted per job and capped at its budget) or have the organisation bring its own model key")

// credentialPolicy is the part of llmSecret that needs no job: whether the bot can hand a worker
// a credential at all. precheck applies it before anyone is asked to confirm, so a missing key
// refuses the tool instead of failing the job at claim time. A provisioning key mints a key per
// job, capped at the job budget; without one the worker gets the shared key, uncapped, which
// the owner chose over refusing jobs (2026-09-08).
//
// An organisation on its own key runs its jobs on that key or not at all: the deployment's key is
// not a fallback for work the organisation chose to pay its own provider for.
func (r *JobRunner) credentialPolicy(ctx context.Context, orgID int64) error {
	if r.settings != nil {
		k := r.settings.Get(ctx, orgID).OwnKey
		if err := k.refusal(); err != nil {
			return err
		}
		if k.Active() {
			if !k.Ref.FixJobs {
				return errFixJobsOffOwnKey
			}
			return nil
		}
	}
	if r.keys.enabled() {
		return nil // a key minted for this job and capped at its budget
	}
	if r.cfg.WorkerLLMKey != "" {
		// The shared key is uncapped and reaches the engine's environment, which the job's own
		// repository code runs in. Safe only where whoever can start a job is trusted; with open
		// signup a stranger is not, so refuse there and make the operator set a per-job key.
		if r.cfg.SignupMode == SignupOpen {
			return errSharedKeyOpenSignup
		}
		return nil
	}
	return errNoWorkerKey
}

// llmSecret is the model credential the worker gets: an OpenRouter key minted for this job and
// capped at its budget, sealed on the row so a retried claim gets the same one; or, with no
// provisioning key, the shared key. A worker runs the repository's own code, so the shared key
// is the weaker choice, and the log and the console say so.
func (r *JobRunner) llmSecret(ctx context.Context, j *Job) (JobLLMSecret, error) {
	if j.KeyOwner == keyOwnerOrg {
		return r.ownKeySecret(ctx, j)
	}
	sec := JobLLMSecret{BaseURL: openRouterBaseURL, Model: j.Model}
	if st := r.settings.Get(ctx, j.OrgID); st.OwnKey.Active() {
		// Dispatched on the deployment's key and claimed after the organisation brought its own.
		// The job was confirmed to run here; it is not moved without anybody saying so, and it is
		// not run here either, now that the organisation has chosen where its calls go.
		return sec, errors.New("this job was dispatched on the deployment's model key, and the organisation has since brought its own; dispatch it again")
	}
	if err := r.credentialPolicy(ctx, j.OrgID); err != nil {
		return sec, err
	}
	if !r.keys.enabled() {
		slog.Warn("fix job on the shared model key: no OPENROUTER_PROVISIONING_KEY, so no per-job cap", "job", j.ID)
		sec.APIKey = r.cfg.WorkerLLMKey
		if r.cfg.WorkerLLMBaseURL != "" {
			sec.BaseURL = r.cfg.WorkerLLMBaseURL
		}
		return sec, nil
	}
	if len(j.llmKeyEnc) > 0 {
		plain, err := r.proxy.sealer.Open(j.llmKeyEnc)
		if err != nil {
			return sec, err
		}
		// The key this job's first claim minted: still this job's alone, so still worth metering.
		sec.APIKey, sec.PerJobKey = string(plain), true
		return sec, nil
	}
	// The name is what the key is called in the operator's own OpenRouter dashboard, so it says
	// which job the charge belongs to and nothing about the product that made it.
	key, hash, err := r.keys.mint(ctx, fmt.Sprintf("job-%d", j.ID), j.BudgetUSD)
	if err != nil {
		return sec, fmt.Errorf("could not provision a restricted worker credential: %w", err)
	}
	enc, err := r.proxy.sealer.Seal([]byte(key))
	if err == nil {
		err = r.store.SetJobFields(ctx, j.OrgID, j.ID, map[string]any{"llm_key_hash": hash, "llm_key_enc": enc})
	}
	if err != nil {
		r.keys.delete(ctx, hash)
		return sec, err
	}
	sec.APIKey = key
	sec.PerJobKey = true
	return sec, nil
}

// ownKeySecret is the credential a job dispatched on the organisation's own key gets: that key,
// on its endpoint. It cannot be capped per job — nothing here can mint a key on somebody else's
// account — so the job is held to its budget by the estimate the server makes from what the worker
// reports (priceJobUsage), and by its turn cap and its clock.
func (r *JobRunner) ownKeySecret(ctx context.Context, j *Job) (JobLLMSecret, error) {
	sec := JobLLMSecret{Model: j.Model}
	if err := r.settings.Get(ctx, j.OrgID).OwnKey.refusal(); err != nil {
		return sec, err
	}
	// The row, not the cached settings: the key and the address it goes to come from one read,
	// and so does the switch that says jobs may have it.
	row, key, err := r.store.ModelKeyRow(ctx, j.OrgID, r.proxy.sealer)
	switch {
	case err != nil:
		return sec, errors.New("the organisation's model key could not be read")
	case row == nil:
		return sec, errors.New("this job was dispatched on the organisation's own model key, which has since been removed; dispatch it again to run it on the deployment's key")
	case !row.FixJobs:
		return sec, errFixJobsOffOwnKey
	}
	sec.BaseURL, sec.APIKey = row.BaseURL, key
	return sec, nil
}

// priceJobUsage gives a job's reported tokens a cost when nothing measured one. A per-job key's
// spend is read back exactly at the end (settleKey); on the shared key or an organisation's own,
// the worker reports tokens and nothing else, and a job recorded at $0.00 would never meet its
// budget or anybody's. The estimate is the model's list price from the deployment's catalogue.
func (r *JobRunner) priceJobUsage(ctx context.Context, j *Job, u *JobUsage) {
	if u == nil || u.CostUSD > 0 || (u.In == 0 && u.Out == 0) || j.LLMKeyHash != "" {
		return
	}
	if r.agent == nil || r.agent.endpoints == nil {
		return
	}
	if p, ok := r.agent.endpoints.platform.priceOf(ctx, j.Model); ok {
		u.CostUSD = p.cost(Usage{In: u.In, Out: u.Out})
	}
}

// settleKey reads the exact spend off the per-job key, deletes the key, and returns any cost the
// worker's own accounting missed.
func (r *JobRunner) settleKey(ctx context.Context, j *Job) float64 {
	if j.LLMKeyHash == "" || !r.keys.enabled() {
		return 0
	}
	extra := 0.0
	if usage, err := r.keys.usage(ctx, j.LLMKeyHash); err == nil && usage > j.CostUSD {
		extra = usage - j.CostUSD
		r.store.SetJobFields(ctx, j.OrgID, j.ID, map[string]any{"cost_usd": usage})
	} else if err != nil {
		slog.Warn("per-job OpenRouter key usage", "job", j.ID, "err", err)
	}
	if err := r.keys.delete(ctx, j.LLMKeyHash); err != nil {
		slog.Warn("per-job OpenRouter key delete", "job", j.ID, "err", err)
	}
	r.store.SetJobFields(ctx, j.OrgID, j.ID, map[string]any{"llm_key_enc": nil})
	return extra
}

// ---- the tool ----

// evidencePrefixes are the tools whose earlier results in the thread are worth handing the worker.
var evidencePrefixes = []string{"gcp_", "sentry_", "github_", "clickup_", "http_request", "fetch_url", "read_thread", "datadog_"}

// fixJobTool is start_fix_job, registered per call only when the channel has a repository.
func (a *Agent) fixJobTool(c *Call) Tool {
	return Tool{
		Name: "start_fix_job",
		Desc: "Hand a code change to the fix worker: it clones the connected GitHub repository in a separate container, changes the code, runs the tests, pushes a NEW branch and opens a draft pull request. Use it when someone asks to fix, patch or implement something in a repository and open a PR, and reach for it straight away: the worker reads the code, so you do not have to go and find the file first. Do not write code yourself. Give a precise brief: what is wrong, the evidence you already have (log lines, stack traces, ticket), and what done looks like. A human must press Confirm before it starts.",
		Params: schema(map[string]any{
			"title":       str("Short title for the change (becomes the PR title), at most 80 characters"),
			"kind":        str("What sort of change this is: \"bugfix\" for a defect, \"feature\" for new behaviour, \"hotfix\" for an urgent production fix. It picks the branch prefix when the organisation's convention names one per kind. Defaults to bugfix"),
			"requirement": str("What to change and why, in full sentences: the bug or feature, where it shows, what the fix should do"),
			"evidence":    str("Log excerpts, stack traces, error messages, ticket text you have seen (verbatim, trimmed)"),
			"acceptance":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "What must be true when the job is done, one criterion per item"},
			"repo":        str("Repository as owner/name; omit to use the channel's default repository"),
			"base_branch": str("Branch to start from and target with the PR; omit for the repository's default branch"),
			"ticket":      str("Ticket id or URL this fixes, if any"),
			"files_hint":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Files or areas likely involved, if known"},
		}, "title", "requirement"),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			var p struct {
				Title, Kind, Requirement, Evidence, Repo, BaseBranch, Ticket string
				Acceptance, FilesHint                                        []string
			}
			var raw struct {
				Title       string   `json:"title"`
				Kind        string   `json:"kind"`
				Requirement string   `json:"requirement"`
				Evidence    string   `json:"evidence"`
				Acceptance  []string `json:"acceptance"`
				Repo        string   `json:"repo"`
				BaseBranch  string   `json:"base_branch"`
				Ticket      string   `json:"ticket"`
				FilesHint   []string `json:"files_hint"`
			}
			json.Unmarshal(args, &raw)
			p.Title, p.Requirement, p.Evidence, p.Repo, p.BaseBranch, p.Ticket = strings.TrimSpace(raw.Title), strings.TrimSpace(raw.Requirement), strings.TrimSpace(raw.Evidence), strings.TrimSpace(raw.Repo), strings.TrimSpace(raw.BaseBranch), strings.TrimSpace(raw.Ticket)
			p.Acceptance, p.FilesHint = raw.Acceptance, raw.FilesHint
			p.Kind = normalizeJobKind(raw.Kind)
			if p.Title == "" || p.Requirement == "" {
				return "", errors.New("title and requirement are required")
			}
			if len([]rune(p.Title)) > 80 {
				p.Title = string([]rune(p.Title)[:80])
			}
			if a.jobs == nil || !a.jobs.Enabled() {
				return "", errors.New("the fix worker is switched off; an admin sets WORKER_MODE to enable it")
			}
			if c.Access == nil {
				return "", errors.New("no repository is connected in this channel")
			}
			repos := c.Access.Repos()
			repo := p.Repo
			if repo == "" {
				repo = c.Access.DefaultRepo
			}
			if repo == "" && len(repos) == 1 {
				repo = repos[0].Repo
			}
			if repo == "" {
				names := make([]string, 0, len(repos))
				for _, rc := range repos {
					names = append(names, rc.Repo)
				}
				return "", fmt.Errorf("name the repository: %s", strings.Join(names, ", "))
			}
			if n, err := normalizeRepo(repo); err == nil {
				repo = n
			}
			var conn *Connection
			for _, rc := range repos {
				if strings.EqualFold(rc.Repo, repo) {
					conn = rc
				}
			}
			if conn == nil {
				return "", fmt.Errorf("%s is not connected in this channel; an admin can add it under Repositories", repo)
			}
			if err := a.jobs.precheck(ctx, c.OrgID, c.TeamID, c.Channel, c.ThreadTS); err != nil {
				return "", err
			}
			base := p.BaseBranch
			if base == "" {
				base = a.defaultBranch(ctx, c, conn.Repo)
			}
			spec := JobSpec{Repo: conn.Repo, ConnectionID: conn.ID, BaseBranch: base, Title: p.Title, Kind: p.Kind,
				Requirement: truncate(redact(p.Requirement), 8000), Evidence: truncate(redact(p.Evidence), 8000),
				Acceptance: cleanList(p.Acceptance, 10, 300), Ticket: truncate(p.Ticket, 300), FilesHint: cleanList(p.FilesHint, 20, 200),
				Requester: c.UserID, RequesterName: c.SL.UserName(ctx, c.UserID), Channel: c.Channel, ThreadTS: c.ThreadTS}
			if len(spec.Acceptance) == 0 {
				spec.Acceptance = []string{"the repository's test command passes", "a test covering the change is added when practical", "no changes outside the scope of the fix"}
			}
			if msgs, err := c.SL.Thread(ctx, c.Channel, c.ThreadTS, 40); err == nil && len(msgs) > 0 {
				spec.ThreadText = truncate(redact(formatMsgs(ctx, c.SL, msgs)), 8000)
			}
			spec.ThreadLink = c.SL.Permalink(ctx, c.Channel, c.ThreadTS)
			if rows, err := a.store.ThreadToolResults(ctx, c.TeamID, c.Channel, c.ThreadTS, evidencePrefixes, 3); err == nil {
				for _, t := range rows {
					spec.ToolEvidence = append(spec.ToolEvidence, ToolEvidence{Tool: t.Name, Args: truncate(t.Args, 500), Result: truncate(redact(t.Result), 4000)})
				}
			}
			// The branch the card promises has to be the branch the worker pushes, so the two
			// settings that shape it are read here as well; dispatch snapshots them again, which
			// is what makes a mid-flight settings change land on the next job and not this one.
			st := a.settings.Get(ctx, c.OrgID)
			spec.Constraints.BranchPrefix = pickBranchPrefix(st.WorkerBranchPrefix, spec.Kind)
			spec.Constraints.BranchSuffix = st.WorkerBranchSuffix
			// A preview is not offered this tool (toolsFor), and holds it here as well, ahead of
			// the allow rules that could dispatch it: a job pushes a branch and opens a pull
			// request against a real repository, which is what a preview promises not to do.
			if c.Preview {
				inChannel := "it would wait for somebody there to press Confirm"
				if st.WorkerAllowRules {
					inChannel += ", unless an allow rule covers it"
				}
				return c.previewHold(jobConfirmSummary(spec), inChannel), nil
			}
			if st.WorkerAllowRules {
				// No email destination, which is how a fix job says it is never eligible on the
				// lane a forwarded email starts — whatever that channel's rules or its
				// email_auto_writes setting say. A ticket filed from a stranger's mail is a row
				// somebody can delete; a branch pushed and a pull request opened from one is not,
				// and this is the write on that lane that always waits for a named approver.
				if rule, ok := a.allowedByRule(ctx, c, describeJobDispatch(spec), ""); ok {
					job, err := a.jobs.Dispatch(ctx, DispatchRequest{OrgID: c.OrgID, TeamID: c.TeamID, Spec: spec, Approval: "rule:" + rule})
					if err != nil {
						return "", err
					}
					return fmt.Sprintf("Fix job #%d started on %s (pre-approved by the allow rule %q). A checklist message in this thread tracks it and the result is posted when it finishes. Tell the user it is running and stop; do not wait for it or call this tool again.", job.ID, job.Repo, rule), nil
				}
			}
			held, _ := json.Marshal(map[string]any{"job": spec})
			id, err := a.store.AddPendingWrite(ctx, c.OrgID, c.TeamID, c.Channel, c.ThreadTS, c.UserID, string(held))
			if err != nil {
				return "", err
			}
			c.holdForConfirm(id, jobConfirmSummary(spec))
			return fmt.Sprintf("This starts a coding job in a separate worker on %s and needs a human OK. In your reply, tell the user in a few lines: the repository and base branch (%s), what the worker will change, and the acceptance criteria. %s Do not call this tool again in this turn.", conn.Repo, base, heldNote(c)), nil
		},
	}
}

// defaultBranch asks GitHub for the repository's default branch through the proxy (a read).
func (a *Agent) defaultBranch(ctx context.Context, c *Call, repo string) string {
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := a.getJSON(rctx, c, "https://api.github.com/repos/"+repo, &out); err == nil && out.DefaultBranch != "" {
		return out.DefaultBranch
	}
	return "main"
}

func cleanList(in []string, max, each int) []string {
	out := []string{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		out = append(out, truncate(redact(s), each))
		if len(out) >= max {
			break
		}
	}
	return out
}

// jobConfirmSummary is the card people read before pressing Confirm.
func jobConfirmSummary(spec JobSpec) string {
	var b strings.Builder
	// Title, requirement, acceptance and ticket are written by the model, on a turn whose input
	// may be a stranger's mail. What the person reads on the card has to be what will run, not a
	// link dressed up as one — the same reason httpConfirmSummary escapes its body.
	fmt.Fprintf(&b, "*Waiting for your OK* to start a fix job on `%s`\n", spec.Repo)
	fmt.Fprintf(&b, "*Title:* %s\n", escapeMrkdwn(truncate(oneLine(spec.Title), 120)))
	fmt.Fprintf(&b, "*Base:* `%s` → a new branch `%s`\n", spec.BaseBranch, jobBranchID(spec.Constraints.BranchPrefix, spec.Constraints.BranchSuffix, "<n>", spec.Title))
	fmt.Fprintf(&b, "*Change:* %s\n", escapeMrkdwn(truncate(oneLine(spec.Requirement), 600)))
	if len(spec.Acceptance) > 0 {
		b.WriteString("*Done when:*")
		for _, a := range spec.Acceptance {
			b.WriteString("\n• " + escapeMrkdwn(truncate(oneLine(a), 200)))
		}
		b.WriteString("\n")
	}
	if spec.Ticket != "" {
		fmt.Fprintf(&b, "*Ticket:* %s\n", escapeMrkdwn(truncate(oneLine(spec.Ticket), 200)))
	}
	// What the worker will run inside the repository is a decision somebody made — in the
	// console, or in the repository's own .attest/recipe.yaml — and the person pressing Confirm
	// should see it before they do. When nobody has said, the worker works it out from the
	// clone, and this says that rather than implying a suite exists.
	fmt.Fprintf(&b, "*Checks:* %s\n", escapeMrkdwn(truncate(describeChecks(spec), 300)))
	b.WriteString("The worker may only push a new branch and open a draft pull request. It never merges and never pushes to the base branch. " +
		"What this thread observed goes to the worker privately; the pull request carries the change, the test results and the links only.")
	return b.String()
}

// describeJobDispatch is what the allow-rule checker sees.
func describeJobDispatch(spec JobSpec) string {
	return fmt.Sprintf("Start a coding job on GitHub repository %s: clone it, run its tests, change code to: %s; then push a NEW branch and open a draft pull request against %s. It never merges and never pushes to the base branch. Ticket: %s",
		spec.Repo, truncate(oneLine(spec.Requirement), 600), spec.BaseBranch, nonEmpty(spec.Ticket, "none"))
}

// describeChecks is what the confirm card says the worker will run. A recipe set on the
// connection is quoted; otherwise the honest answer is that it will be worked out from the
// clone, which is a different promise from naming a command.
func describeChecks(spec JobSpec) string {
	if r := spec.Constraints.Recipe; r.SetByAdmin() && r.Runnable() {
		return r.Describe() + " (set in the console)"
	}
	if cmd := strings.TrimSpace(spec.Constraints.TestCmd); cmd != "" {
		return "test: " + cmd + " (set in the console)"
	}
	return "worked out from the repository — its .attest/recipe.yaml if it has one, else its build files. The result says what ran."
}

// rememberRecipe writes back what the first job on a repository worked out, so the dispatcher
// can pick a worker image for its ecosystem (workerFor). It never steers a later job:
// constraints hands the worker only a recipe somebody set, and every job detects its own
// packages afresh. Only a recipe that was worked out from the clone is stored — one an admin or
// the repository already authored is theirs, and overwriting it with our guess would be rude
// and wrong.
func (r *JobRunner) rememberRecipe(ctx context.Context, j *Job, res *JobResult) {
	if res == nil || res.Recipe == nil || res.Recipe.Source != RecipeSourceDetected || j.ConnectionID == 0 {
		return
	}
	conn, err := r.store.Connection(ctx, j.OrgID, j.ConnectionID)
	if err != nil || conn == nil || conn.Recipe != nil || strings.TrimSpace(conn.TestCmd) != "" {
		return
	}
	stored := *res.Recipe
	stored.Source = RecipeSourceDetected
	if err := r.store.SetConnectionRecipe(ctx, j.OrgID, j.ConnectionID, &stored); err != nil {
		slog.Warn("remember recipe", "job", j.ID, "connection", j.ConnectionID, "err", err)
		return
	}
	slog.Info("recipe remembered", "connection", j.ConnectionID, "repo", j.Repo, "ecosystem", stored.Ecosystem, "recipe", stored.Describe())
}

// workerFor is which worker image this repository needs. It reads the ecosystem off whatever
// the connection remembers — an admin's recipe, or the one a previous job worked out — so the
// first job on a repository runs on the base worker and every one after it can be routed. That
// is enough: the ecosystem of a repository is not a thing that changes.
func (r *JobRunner) workerFor(conn *Connection) string {
	if conn == nil || conn.Recipe == nil || len(r.cfg.WorkerJobNames) == 0 {
		return ""
	}
	eco := strings.ToLower(strings.TrimSpace(conn.Recipe.Ecosystem))
	if name := r.cfg.WorkerJobNames[eco]; name != "" {
		return name
	}
	// java-gradle and java-maven both answer to "java".
	if family, _, ok := strings.Cut(eco, "-"); ok {
		return r.cfg.WorkerJobNames[family]
	}
	return ""
}
