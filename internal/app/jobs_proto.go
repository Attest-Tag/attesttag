package app

import (
	"encoding/json"
	"strings"
)

// The bot ⇄ worker protocol. The worker binary (internal/worker) imports these types, so the two
// sides cannot drift apart. The endpoints live in jobs_api.go; the plan's Protocol section is the
// human-readable contract.

// What the dispatcher puts in the worker container's environment. Nothing else reaches it: the
// spec and every secret come back over the claim call.
const (
	EnvJobID    = "ATTEST_JOB_ID"
	EnvBotURL   = "ATTEST_BOT_URL"
	EnvJobToken = "ATTEST_JOB_TOKEN"
)

// Event kinds.
const (
	JobKindPhase     = "phase"
	JobKindLog       = "log"
	JobKindTests     = "tests"
	JobKindUsage     = "usage"
	JobKindHeartbeat = "heartbeat"
	JobKindWarn      = "warn"
)

// Phases a worker reports, in order. Phase events carry a status: started | ok | failed | skipped.
// setup is the dependency install, build the compile/typecheck gate: on a repository with no test
// suite the build is the only thing that says the change is not nonsense, so it is its own phase
// rather than a line inside another one.
var JobPhases = []string{"clone", "setup", "build_before", "test_before", "engine", "build_after", "test_after", "commit", "push", "pr"}

// Terminal statuses, used both for the job row and for the worker's result.
const (
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
	JobCancelled = "cancelled"
	JobTimeout   = "timeout"
)

// Size caps, enforced by the bot and respected by the worker.
const (
	JobEventMaxBytes   = 4 << 10
	JobEventsBatchMax  = 50
	JobSummaryMaxBytes = 16 << 10
	JobLogTailMaxBytes = 64 << 10
	JobDiffMaxBytes    = 256 << 10
	JobResultMaxBytes  = 128 << 10
	JobHeartbeatSecs   = 60
)

// JobSpec is the brief the bot composes and the worker executes.
type JobSpec struct {
	V            int    `json:"v"`
	Repo         string `json:"repo"` // owner/name
	ConnectionID int64  `json:"connection_id"`
	BaseBranch   string `json:"base_branch"`
	Branch       string `json:"branch"` // the branch the worker may push, set at dispatch
	Title        string `json:"title"`
	// Kind is what sort of change this is — jobKindBugfix or jobKindFeature. It is the job's
	// own word for itself, and it chooses among the branch prefixes the organisation allows.
	Kind          string         `json:"kind,omitempty"`
	Requirement   string         `json:"requirement"`
	Evidence      string         `json:"evidence,omitempty"`
	Acceptance    []string       `json:"acceptance"`
	Ticket        string         `json:"ticket,omitempty"`
	FilesHint     []string       `json:"files_hint,omitempty"`
	ThreadText    string         `json:"thread_text,omitempty"`
	ThreadLink    string         `json:"thread_link,omitempty"`
	ToolEvidence  []ToolEvidence `json:"tool_evidence,omitempty"`
	Requester     string         `json:"requester"`
	RequesterName string         `json:"requester_name,omitempty"`
	Channel       string         `json:"channel"`
	ThreadTS      string         `json:"thread_ts"`
	Constraints   JobConstraints `json:"constraints"`
}

// ToolEvidence is a tool result the bot gathered earlier in the thread (logs, Sentry, GitHub).
type ToolEvidence struct {
	Tool   string `json:"tool"`
	Args   string `json:"args"`
	Result string `json:"result"`
}

// JobConstraints are the settings snapshotted at dispatch, so a change in the console mid-job
// does not move the goalposts.
type JobConstraints struct {
	Engine       string  `json:"engine"`
	Model        string  `json:"model"`
	BudgetUSD    float64 `json:"budget_usd"`
	TimeoutS     int     `json:"timeout_s"`
	DraftPR      bool    `json:"draft_pr"`
	TestCmd      string  `json:"test_cmd,omitempty"` // legacy single-command override; folded into Recipe at dispatch
	BranchPrefix string  `json:"branch_prefix,omitempty"`
	BranchSuffix string  `json:"branch_suffix"`
	MaxRounds    int     `json:"max_rounds"`
	// Recipe is what the console knows about how to build and test this repository, or nil to
	// let the worker read the repository's own .attest/recipe.yaml and then fall back to
	// detection. It is snapshotted at dispatch like everything else here.
	Recipe *Recipe `json:"recipe,omitempty"`
}

// ---- recipes ----

// RecipeSourceRepo and friends say where the recipe the worker used came from, which is the
// difference between "nobody has told us how to test this repository" and "what we were told
// does not work". It reaches the pull request, the Slack report and the console.
const (
	RecipeSourceRepoFile   = "repo_file"  // .attest/recipe.yaml, committed in the repository
	RecipeSourceConnection = "connection" // set by an admin on the repository connection
	RecipeSourceDetected   = "detected"   // worked out from the marker files in the clone
	RecipeSourceNone       = "none"       // nothing matched
)

// RecipeStep is one command the worker runs inside the clone: a program and its arguments,
// never a shell line. Everything a shell would interpret is refused when a human writes it
// (SplitCommand) and never introduced when the worker detects it, so the repository connection
// is not a shell in the worker container and neither is a file committed by a contributor.
type RecipeStep struct {
	Name string `json:"name,omitempty"`
	// Run is the write-only convenience form: one line, which the console and the API split
	// into Argv on save and which is never stored. Splitting happens on this side because it is
	// where the refusal lives — a line with a pipe or a semicolon in it is rejected, not
	// silently truncated into something that looks like it worked.
	Run      string   `json:"run,omitempty"`
	Argv     []string `json:"argv"`
	Dir      string   `json:"dir,omitempty"`       // relative to the repository root; empty means the recipe's workdir
	TimeoutS int      `json:"timeout_s,omitempty"` // 0 = the phase's share of what is left of the job
	Optional bool     `json:"optional,omitempty"`  // a non-zero exit is reported, not fatal
}

func (s *RecipeStep) String() string {
	if s == nil || len(s.Argv) == 0 {
		return ""
	}
	return strings.Join(s.Argv, " ")
}

// Recipe is everything the worker needs to know about one repository: which toolchains, where
// the package lives, and the commands that install, build, lint and test it. A repository can
// commit one; an admin can set one on the connection; the worker detects one when neither did.
type Recipe struct {
	Source    string            `json:"source"`
	Ecosystem string            `json:"ecosystem,omitempty"` // go | node | python | java | dotnet | ruby | ...
	Workdir   string            `json:"workdir,omitempty"`   // package root inside the repository, for monorepos
	Tools     map[string]string `json:"tools,omitempty"`     // toolchain → version, provisioned with mise
	Setup     []RecipeStep      `json:"setup,omitempty"`
	Build     *RecipeStep       `json:"build,omitempty"`
	Lint      *RecipeStep       `json:"lint,omitempty"`
	Test      *RecipeStep       `json:"test,omitempty"`
	Services  []string          `json:"services,omitempty"` // postgres:16, redis:7 — what the suite needs running
	Why       string            `json:"why,omitempty"`      // when Test is nil: why nothing runs
}

// Runnable reports whether there is anything worth running at all.
func (r *Recipe) Runnable() bool {
	return r != nil && (r.Test != nil || r.Build != nil || r.Lint != nil)
}

// Describe is the one-line form people read in the confirm card and the console.
func (r *Recipe) Describe() string {
	if r == nil {
		return "not resolved yet"
	}
	var parts []string
	if r.Workdir != "" && r.Workdir != "." {
		parts = append(parts, "in "+r.Workdir)
	}
	for _, s := range []struct {
		label string
		step  *RecipeStep
	}{{"build", r.Build}, {"lint", r.Lint}, {"test", r.Test}} {
		if s.step != nil {
			parts = append(parts, s.label+": "+s.step.String())
		}
	}
	if len(parts) == 0 {
		if r.Why != "" {
			return "nothing to run (" + r.Why + ")"
		}
		return "nothing to run"
	}
	return strings.Join(parts, " · ")
}

// ---- claim ----

type JobClaimRequest struct {
	Worker JobWorkerInfo `json:"worker"`
}

type JobWorkerInfo struct {
	Mode      string `json:"mode"`
	Execution string `json:"execution,omitempty"`
	Hostname  string `json:"hostname,omitempty"`
	Version   string `json:"version,omitempty"`
}

// JobClaim is the one response that carries secrets. The bot never logs it.
type JobClaim struct {
	Job     JobClaimJob `json:"job"`
	Secrets JobSecrets  `json:"secrets"`
	Limits  JobLimits   `json:"limits"`
	Cache   JobCache    `json:"cache,omitempty"`
}

// JobCache is where a worker restores and saves this repository's dependency cache: two
// short-lived signed URLs onto the bot's own bucket, so the bytes go straight to storage and
// neither a credential nor the traffic passes through the bot. Empty URLs mean no cache this
// run, which costs a cold install and nothing else.
//
// The key is per repository and base branch, not per lockfile: everything cached here — the npm,
// uv, Go module, Cargo registry and Gradle stores — is content-addressed, so a stale entry is
// dead weight rather than a wrong answer, and a coarse key is what makes the cache warm often.
type JobCache struct {
	Key      string `json:"key,omitempty"`
	GetURL   string `json:"get_url,omitempty"`
	PutURL   string `json:"put_url,omitempty"`
	MaxBytes int64  `json:"max_bytes,omitempty"`
}

// JobCacheMaxBytes caps what a job may save, so one repository with a pathological dependency
// tree cannot fill the bucket.
const JobCacheMaxBytes = 2 << 30

type JobClaimJob struct {
	ID   int64   `json:"id"`
	Spec JobSpec `json:"spec"`
}

type JobSecrets struct {
	GitHubToken  string       `json:"github_token"`
	LLM          JobLLMSecret `json:"llm"`
	EngineAPIKey string       `json:"engine_api_key,omitempty"`
}

type JobLLMSecret struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Model   string `json:"model"`
	// PerJobKey says the key was minted for this job alone, so what it has spent is what the job
	// spent. Only then is it worth metering (internal/worker/spend.go): a shared key's running
	// total moves with everything else that key is doing while the job runs.
	PerJobKey bool `json:"per_job_key,omitempty"`
}

type JobLimits struct {
	BudgetUSD        float64 `json:"budget_usd"`
	Deadline         string  `json:"deadline"` // RFC3339, UTC
	HeartbeatSeconds int     `json:"heartbeat_seconds"`
	EventMaxBytes    int     `json:"event_max_bytes"`
	SummaryMaxBytes  int     `json:"summary_max_bytes"`
	LogTailMaxBytes  int     `json:"log_tail_max_bytes"`
	DiffMaxBytes     int     `json:"diff_max_bytes"`
}

// ---- events ----

type JobUsage struct {
	In      int     `json:"in"`
	Out     int     `json:"out"`
	CostUSD float64 `json:"cost_usd"`
}

// usage is what a worker's report looks like as a turn's usage. The cache and reasoning counts
// are zero because the worker never sends them: it runs someone else's agent in another
// container and reports three numbers over this wire. Zero is the truthful value here, not a
// missing one — see the note on the columns in migrations/sqlite/0010_usage_cache_tokens.sql.
func (u JobUsage) usage() Usage {
	return Usage{In: u.In, Out: u.Out, CostUSD: u.CostUSD}
}

// usageOn is usage as one job spent it: on the organisation's own key it is recorded as the
// organisation's, which LogUsageBy charges to nothing.
func (u JobUsage) usageOn(j *Job) Usage {
	us := u.usage()
	if j.KeyOwner == keyOwnerOrg {
		us.KeyOwner = keyOwnerOrg
	}
	return us
}

// JobEvent is one progress line. seq is the worker's monotonic counter and the idempotency key.
// A usage event carries a delta; the result carries the total.
type JobEvent struct {
	ID        int64           `json:"id,omitempty"`
	Seq       int64           `json:"seq"`
	At        string          `json:"at,omitempty"`
	Kind      string          `json:"kind"`
	Phase     string          `json:"phase,omitempty"`
	Status    string          `json:"status,omitempty"`
	Message   string          `json:"message,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Usage     *JobUsage       `json:"usage,omitempty"`
	CreatedAt string          `json:"created_at,omitempty"`
}

type JobEventsRequest struct {
	Events []JobEvent `json:"events"`
}

type JobEventsResponse struct {
	OK       bool   `json:"ok"`
	Accepted int    `json:"accepted"`
	Cancel   bool   `json:"cancel"`
	Reason   string `json:"reason,omitempty"`
}

// ---- result ----

type JobResult struct {
	Seq           int64       `json:"seq"`
	Status        string      `json:"status"` // succeeded | failed | cancelled | timeout
	Summary       string      `json:"summary"`
	Note          string      `json:"note,omitempty"` // a caveat on a job that still succeeded: the engine stopped at its turn or spend cap
	PR            *JobPR      `json:"pr,omitempty"`
	Branch        string      `json:"branch,omitempty"`
	HeadSHA       string      `json:"head_sha,omitempty"`
	Tests         JobTests    `json:"tests"`
	Build         JobCheck    `json:"build"`           // the compile/typecheck gate, before and after
	Lint          JobCheck    `json:"lint,omitempty"`  // when the recipe has one
	Setup         JobStepRun  `json:"setup,omitempty"` // the dependency install
	Recipe        *Recipe     `json:"recipe,omitempty"`
	DiffStat      JobDiffStat `json:"diff_stat"`
	FilesChanged  []string    `json:"files_changed,omitempty"`
	Usage         JobUsage    `json:"usage"` // total for the job
	Model         string      `json:"model,omitempty"`
	Engine        string      `json:"engine,omitempty"`
	LogTail       string      `json:"log_tail,omitempty"`
	Error         JobError    `json:"error"`
	TicketComment string      `json:"ticket_comment,omitempty"`
	DurationS     int         `json:"duration_s,omitempty"`
}

type JobPR struct {
	URL     string `json:"url"`
	Number  int    `json:"number"`
	Branch  string `json:"branch"`
	Base    string `json:"base"`
	HeadSHA string `json:"head_sha,omitempty"`
	Draft   bool   `json:"draft"`
}

// JobTests is the suite, which is one JobCheck among several now that the build is its own
// gate. It stays a distinct name because the console, the store and every row already written
// use it.
type JobTests = JobCheck

// JobCheck is a gate run twice, before the change and after it: the build, the linter. It is
// the same shape as JobTests, which stays as it is so the console keeps reading old rows.
type JobCheck struct {
	Command string     `json:"command,omitempty"`
	Skipped string     `json:"skipped,omitempty"`
	Before  JobTestRun `json:"before"`
	After   JobTestRun `json:"after"`
}

// JobStepRun is a step that happens once: the dependency install.
type JobStepRun struct {
	Command string  `json:"command,omitempty"`
	Ran     bool    `json:"ran"`
	OK      bool    `json:"ok"`
	Seconds float64 `json:"seconds,omitempty"`
	Output  string  `json:"output,omitempty"` // tail
}

// JobTestRun: Ran=false means no test suite was found or it never got to run.
type JobTestRun struct {
	Ran     bool    `json:"ran"`
	OK      bool    `json:"ok"`
	Passed  int     `json:"passed,omitempty"`
	Failed  int     `json:"failed,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
	Output  string  `json:"output,omitempty"` // tail
}

type JobDiffStat struct {
	Files      int `json:"files"`
	Insertions int `json:"insertions"`
	Deletions  int `json:"deletions"`
}

// JobError codes: empty_diff | tests_failed | budget_exceeded | clone_failed | base_branch_missing |
// pr_create_failed | push_failed | engine_error | timeout | cancelled | dispatch_failed | stale |
// setup_failed | toolchain_missing | services_unavailable | no_recipe.
type JobError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}
