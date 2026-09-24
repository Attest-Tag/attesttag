package app

import (
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Config is everything read from the environment (the dotenv file named by EnvFile
// is loaded if present).
type Config struct {
	SlackBotToken, SlackSigningSecret string
	LLMBaseURL, LLMKey                string
	Model, HeavyModel, EmbedModel     string
	// Microsoft Teams (msteams_auth.go): the bot's Entra app registration. All empty means this
	// deployment has no Teams — /msteams/messages refuses every delivery and the console does not
	// offer to connect a tenant. MSTeamsTenantID is the directory the registration lives in, which a
	// single-tenant bot takes its tokens from.
	MSTeamsAppID, MSTeamsAppPassword string
	MSTeamsTenantID, MSTeamsAppType  string
	// Who may Sign in with Microsoft (auth_microsoft.go), through the same registration:
	// "organizations" for any work or school account, or one directory's tenant id. Empty offers
	// no such button, since it also needs a redirect URI registered on the app.
	MSTeamsSignIn string
	// Where the database is. DatabaseURL wins when both are set, which makes a cutover — and
	// its rollback — one environment variable rather than a different build. Empty means
	// SQLite at DBPath, which is the default so that one container and one volume works with
	// nothing configured.
	DatabaseURL string
	// RequirePostgres refuses to start on anything but Postgres. A deployment that has been
	// cut over has exactly one database, and the fallback below — empty DatabaseURL means
	// SQLite at DBPath — is a good default for a first `docker run` and a trap for that
	// deployment: a DATABASE_URL that fails to arrive (an unreadable secret, a deploy that
	// drops the variable) is not an error, it is a container quietly opening an empty SQLite
	// file on a disk that does not survive the revision, serving what looks like a brand new
	// install. Set on any deployment whose rows are in Postgres; deploy/gcp/cloudrun.sh sets
	// it whenever it configures one.
	RequirePostgres  bool
	DBPath, DocsDir  string
	Timezone         string
	MonthlyBudgetUSD float64
	DenyTraining     bool   // OpenRouter: only route to providers that don't train on data
	ProviderSort     string // OpenRouter: price | throughput | latency; "" or "none" leaves load balancing on
	ProviderOrder    string // OpenRouter: comma-separated provider names to try first
	Reasoning        string // none | minimal | low | medium | high (see reasoningOpt)
	SelfTest         bool   // answer our own messages tagged [selftest] (curl-driven e2e tests)
	HealthAddr       string
	MaxToolRounds    int
	TurnMaxMinutes   int     // ceiling on one turn's wall clock, whatever its rounds allow
	TurnMaxUSD       float64 // and on what it may spend, whatever its rounds allow
	RoutineRounds    int     // tool rounds one scheduled run may spend (org setting's default)
	RoutineMinutes   int     // and the clock it spends them in
	HistoryLimit     int     // max thread messages fed to the model
	BotName          string
	LLMKeyName       string // which variable LLMKey came from: LLM_API_KEY or OPENROUTER_API_KEY
	LLMKeySource     string // where that value came from: the dotenv file, the shell environment, or both (see keyProvenance)

	// The operator's ceilings, which no organisation's own settings can lift: the most model
	// spend one organisation may put on the shared key in a month (0 = no ceiling), and how many
	// turns one organisation may have running at once on this instance.
	PlatformMonthlyBudgetUSDPerOrg float64
	PlatformMaxInFlightPerOrg      int
	// What a free-plan organisation may spend in a month (plans.go). Every signup starts free
	// and only the operator moves an account to pro. 0 leaves free accounts under the ceiling
	// above, like everybody else — which is the right default for a deployment spending its
	// own model key, so a self-host is not capped at a figure it never chose. A service
	// handing strangers a shared key sets this; deploy/gcp/cloudrun.sh does.
	FreePlanBudgetUSD float64
	// Where a capped account is told to write for a bigger budget (plans.go). Empty means this
	// deployment publishes no support address: the refusals then say the budget is fixed
	// without naming anyone, and the console's Upgrade button — which sends mail — is not
	// offered. A self-host that left this unset must not point its users at somebody else's
	// inbox.
	SupportEmail string
	// This deployment's public website, if it has one — the origin the console's Get started
	// page fetches its walkthroughs from. This one setting is both halves of that: it is named
	// in the console's CSP and handed to the page on /api/me (headers.go, handleMe), so the
	// origin the page asks is the origin the browser permits. Empty, and the console talks to
	// its own origin and nothing else.
	SiteURL string
	// The cutover freeze (pgimport.go, and the runbook in deploy/docs/postgres-cutover.md).
	// While it is on the deployment stops writing: Slack and Teams deliveries are acknowledged
	// and dropped, the schedulers do not start, and the console says so. It is not a read-only
	// mode and not a graceful degradation — it is the ten minutes between taking a snapshot
	// and pointing the service at the database that snapshot became.
	Maintenance bool
	// Who may create an account: open, first-run or closed (auth_password.go). Defaults to
	// first-run — the first signup founds the deployment, everybody after it is invited —
	// because a deployment whose owner has not thought about this is a self-host on a public
	// address, and open registration is not what they meant.
	SignupMode string
	// Which organisations may bring their own model key (model_keys.go): all, enterprise or off.
	// Defaults to all, so a deployment running its own can rotate the key from the console; the
	// hosted service names enterprise in deploy/gcp/cloudrun.sh.
	OrgModelKeys string
	// The bearer secret behind /api/operator/ (operator.go). Empty means those routes do not
	// exist on this deployment.
	OperatorSecret string

	// Self-serve billing (billing.go). All of it is for a service selling a plan on a key it
	// pays for. A deployment spending its own model key wants none of it and gets none: with
	// these unset the billing routes are never registered, the console has no Billing tab, and
	// no account has a credit floor.
	StripeSecretKey     string
	StripeWebhookSecret string
	// The plan sizes a Pro fee is set by, in order. One variable rather than ten, parsed
	// from size=price_id:minor_units — the price rides along so drawing the console's size
	// picker needs no call to Stripe. A size with no entry here is not offered.
	StripeSizes []Size
	// Prepaid credit: the currency it is held in, what one top-up may be, and the balance at
	// which an account is warned. The two limits are enforced server-side and an amount from a
	// browser is never trusted; a checkout is a public-facing write on a service with open
	// signup, which is a card-testing target.
	BillingCurrency string
	TopUpMinUSD     float64
	TopUpMaxUSD     float64
	CreditLowUSD    float64
	// How many investigations this container may run at once, across every organisation on it
	// (investigations.go). 0 switches the lane off: the tool is still offered, so questions are
	// queued and answered by whichever container does run one.
	InvestigationWorkers int

	// Replication and the single-writer lease (lease.go, replication.go). Replica nil means
	// neither runs: the database is local, and backing it up is the operator's business —
	// which is what local development, one box with a disk, and the launchd deployment want.
	Replica        *replicaTarget // resolved by resolveReplica: a GCS or an S3 bucket, or nothing
	ReplicaBucket  string         // LITESTREAM_BUCKET, kept because the worker cache defaults to it
	ReplicaPath    string         // key of the replica within the bucket; the lease object is its sibling
	LitestreamBin  string
	LitestreamConf string // an operator's own config file; empty means the one rendered at startup

	// Fix-job worker (jobs.go). There are two answers an operator has to give — should jobs run
	// at all, and where — and only the first is usually theirs to make: a deployment already
	// knows which platform it is on, because the platform tells every container it starts.
	//
	//	WorkerMode      off | workers | local, as written. `workers` runs each job in a
	//	                container of its own and works out where from the environment.
	//	WorkerPlatform  what that resolved to, and the dispatcher key: "" when off, else
	//	                local | cloudrun | ecs | aca | k8s | docker.
	//
	// Naming a platform directly still works and pins it, which is both the escape hatch for a
	// deployment detection would guess wrong about and the reason no existing .env file had to
	// change when `workers` arrived.
	WorkerMode         string
	WorkerPlatform     string
	WorkerJobName      string // the execution the platform starts: a Cloud Run job, an ECS task definition, an ACA job; on k8s and docker the image
	WorkerProject      string // GCP project of the job; metadata server when empty
	WorkerRegion       string // GCP region, or the AWS region of the ECS cluster
	WorkerSAEmail      string // the job's service account, checked on the worker's identity token
	WorkerRequireOIDC  bool   // workers must present a Google identity token (default on in cloudrun mode)
	WorkerLLMBaseURL   string // model endpoint handed to the worker with the shared key (default: the bot's)
	WorkerLLMKey       string // the shared key a worker gets when no provisioning key exists (default: the bot's key)
	WorkerEngineAPIKey string // extra engine credential (an Anthropic key for claude_code), if any

	// Where a worker container gets its CPU and memory, for the modes that state it per
	// execution rather than on a job object created ahead of time (k8s, docker). Cloud Run,
	// ECS and ACA carry it on the job or task definition their deploy script created.
	WorkerCPU    string // "2" — cores
	WorkerMemory string // "4Gi"

	// ecs (jobs_ecs.go). The cluster and the network a Fargate task lands in; a task cannot
	// be started without them, because Fargate has no default VPC placement.
	WorkerECSCluster        string
	WorkerECSSubnets        []string
	WorkerECSSecurityGroups []string
	WorkerECSPublicIP       bool // ENABLED unless the subnets route out through a NAT

	// aca (jobs_aca.go). The ARM path of the Container Apps job the bot starts.
	WorkerAzureSubscription  string
	WorkerAzureResourceGroup string

	// k8s (jobs_k8s.go). The bot creates one batch/v1 Job per fix job in this namespace;
	// empty means the namespace the bot's own service account token names.
	WorkerK8sNamespace      string
	WorkerK8sServiceAccount string
	WorkerK8sPullSecret     string
	// The node architecture a worker pod is pinned to, as kubernetes.io/arch; empty pins
	// nothing (workerK8sArch).
	WorkerK8sArch string

	// docker (jobs_docker.go). The Engine API socket, and the network the worker joins so it
	// can reach the bot by its compose service name.
	WorkerDockerHost    string
	WorkerDockerNetwork string
	// The dependency cache (jobs_cache.go). Defaults to the replica bucket, which every
	// deployment that keeps a database already has; WORKER_CACHE=off turns it off.
	WorkerCacheBucket string
	WorkerCacheOff    bool
	// Worker images by ecosystem: WORKER_JOB_NAMES="java-gradle=attesttag-worker-jvm,dotnet=attesttag-worker-jvm".
	// A repository whose ecosystem is not named here runs on WORKER_JOB_NAME.
	WorkerJobNames            map[string]string
	OpenRouterProvisioningKey string // mints the per-job capped OpenRouter key (jobs_openrouter.go); the safe choice
}

// WorkerDispatcher is the dispatcher key this configuration resolves to, and "" when the fix
// worker is off.
//
// It exists so that WorkerPlatform is not a second field every Config has to remember to set.
// LoadConfig fills WorkerPlatform in; anything that builds a Config by hand — a test, a tool —
// sets WorkerMode alone and gets the same answer, because a mode that names something other
// than `off` or `workers` *is* the dispatcher. `workers` without a resolved platform is off:
// detection only runs in LoadConfig, and guessing here would be worse than declining.
func (c Config) WorkerDispatcher() string {
	if c.WorkerPlatform != "" {
		return c.WorkerPlatform
	}
	switch c.WorkerMode {
	case "", "off", "workers":
		return ""
	}
	return c.WorkerMode
}

// LLMKeyFingerprint identifies the model key in logs without revealing it.
func (c Config) LLMKeyFingerprint() string { return fingerprint(c.LLMKey) }

// fingerprint is the tail of a secret, enough to tell two keys apart and no more.
func fingerprint(v string) string {
	switch {
	case v == "":
		return "unset"
	case len(v) <= 8:
		return fmt.Sprintf("(%d chars)", len(v))
	}
	return "…" + v[len(v)-4:]
}

// Shadow is a variable whose dotenv value is masked by one already in the process environment.
type Shadow struct{ Key, Env, DotEnv string } // Env and DotEnv are fingerprints

// envShadows compares what a dotenv file defines with the values the process already had
// (godotenv.Load never overrides those) and returns the keys where both exist and differ,
// sorted by name. lookup is the pre-load environment.
func envShadows(dotenv map[string]string, lookup func(string) (string, bool)) []Shadow {
	var out []Shadow
	for k, dv := range dotenv {
		if ev, ok := lookup(k); ok && ev != dv {
			out = append(out, Shadow{Key: k, Env: fingerprint(ev), DotEnv: fingerprint(dv)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// keyProvenance says where the effective value of name comes from, for the startup log and
// !whoami. file is the dotenv file dotenv was read from, so the message names the real file.
func keyProvenance(name, file string, dotenv map[string]string, lookup func(string) (string, bool)) string {
	sv, inShell := lookup(name)
	dv, inDot := dotenv[name]
	switch {
	case inShell && inDot && sv != dv:
		return "the shell environment (differs from " + file + ")"
	case inShell && inDot:
		return "the shell environment (same as " + file + ")"
	case inShell:
		return "the environment"
	case inDot:
		return file
	}
	return "nowhere"
}

// isSecretName picks the variables whose shadowing deserves a warning rather than a note.
func isSecretName(k string) bool {
	k = strings.ToUpper(k)
	for _, m := range []string{"TOKEN", "KEY", "SECRET", "PASSWORD"} {
		if strings.Contains(k, m) {
			return true
		}
	}
	return false
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

// Size is one plan size a Pro subscription may be bought at: the key the console and the
// webhook name it by, the Stripe Price the fee is charged through, and that price in minor units
// so a page can be drawn without asking Stripe. Label is for people.
//
// IncludedMinor is the model credit the fee carries with it, granted when the first invoice is
// paid and again on every renewal (billing.go). It is an allowance rather than a refund of the
// fee: the two are separate figures on purpose, so a size can be repriced without silently
// repricing what it includes, and a size that includes nothing is simply left at zero.
type Size struct {
	Key           string
	PriceID       string
	AmountMinor   int64
	IncludedMinor int64
	// UserLimit is how many people the size is sold for, 0 for one sold without a ceiling. It
	// is what the console compares the 30-day active count against; see active_users.go.
	UserLimit int
	// JobLimit is how many fix jobs a month the size is sold with, 0 for one sold without a
	// figure. Printed beside the size and counted on Billing, never enforced; see sizeJobLimits.
	JobLimit int
	Label    string
}

// sizeLabels is what each size is called on screen. Keys rather than free text so the console,
// the operator page and the website cannot drift into describing the same size differently.
//
// The ladder is counted in users — people who may sign in and use the bot — and runs 10, 25, 50,
// 100 and 250. Past that a size is a conversation: over_250 is listed in STRIPE_SIZES with no
// Price behind it, so the console and the site print "Talk to us" where a fee would be and the
// checkout route refuses it. The keys below the ladder were sold before it: rungs of the earlier
// user ladders of 2026-09-21, and the employee-headcount ladder before those. All are kept because
// they are written into the `size` column of accounts bought under them: dropping them would
// leave those rows rendering a raw key where a name belongs. Nothing offers them, because
// offering a size takes an entry in STRIPE_SIZES and there is no longer a price behind these.
//
// A key names a ceiling, never a price. upto_25 sold for $199 before it sold for $99, and that is
// fine — the fee lives in Stripe and in STRIPE_SIZES. What a key must never do is change the
// number in its name: editing upto_25 into "Up to 50" would quietly hand every account bought at
// 25 a ceiling nobody agreed to, so a rung whose ceiling moves is a new key.
var sizeLabels = map[string]string{
	"upto_10":  "Up to 10 users",
	"upto_25":  "Up to 25 users",
	"upto_50":  "Up to 50 users",
	"upto_100": "Up to 100 users",
	"upto_250": "Up to 250 users",
	"over_250": "More than 250 users",

	"upto_200":  "Up to 200 users",
	"upto_500":  "Up to 500 users",
	"unlimited": "Unlimited users",

	"under_25":  "Under 25 employees",
	"25_100":    "25–100 employees",
	"100_500":   "100–500 employees",
	"500_2000":  "500–2,000 employees",
	"over_2000": "Over 2,000 employees",

	// Not a rung. The size an enterprise account carries, whose figures are its own deal
	// (enterprise_terms, store_enterprise.go) rather than anything configured here — which is
	// why it is absent from the two maps below.
	sizeEnterprise: "Enterprise",
}

// sizeUserLimits is how many people each size is sold for. A map beside sizeLabels rather than a
// fourth field in STRIPE_SIZES, for the reason the labels are one: the console, the operator page
// and the website must not drift into describing the same size differently, and a limit typed
// into a deployment's environment is a limit that can disagree with the name printed next to it.
//
// A size absent from here — over_250, "unlimited" — has no ceiling. 0 means the count is shown
// and nothing is ever "over", which is also what a free account gets, because it has no size.
//
// The employee ladder is deliberately absent: those sizes were sold on a declared headcount that
// nothing counted, so there is no figure to hold an account bought under one against.
var sizeUserLimits = map[string]int{
	"upto_10":  10,
	"upto_25":  25,
	"upto_50":  50,
	"upto_100": 100,
	"upto_250": 250,

	// Retired rungs. An account still on one is held to the ceiling it bought.
	"upto_200": 200,
	"upto_500": 500,
}

// sizeJobLimits is how many fix jobs a month each size is sold with, and freePlanJobsPerMonth is
// the free plan's figure. A fix job is a container run — clone, tests, change, pull request — and
// that compute is the one cost credit does not meter, so it is the one thing no plan is sold
// without a number for; "unlimited" came off the pricing page on 2026-09-21 for exactly that.
//
// Printed, not enforced. The figure is on the pricing page, beside each size in the console's
// picker, and on Billing as this calendar month's count against it. Nothing refuses the job past
// it — the same policy as the user ceiling, for the same reason: stopping work somebody asked
// for, over a number only an admin can move, is the wrong end of the product to put a commercial
// limit on. A size absent from here has no figure, and the console prints the count alone rather
// than the word "unlimited"; over_250 is absent because its number is written into the deal.
var sizeJobLimits = map[string]int{
	"upto_10":  25,
	"upto_25":  50,
	"upto_50":  100,
	"upto_100": 200,
	"upto_250": 500,
}

// freePlanJobsPerMonth is the free plan's fix jobs a month, printed under the same rule.
const freePlanJobsPerMonth = 5

// STRIPE_BANDS is the old spelling of STRIPE_SIZES and is still read when the new one is unset.
// The rename is ours and the deployment's environment is not: a deployment that updates the binary
// without updating its secrets would otherwise come up selling no plans at all, which is a silent
// failure — the console simply shows nothing to buy. Remove the fallback once every deployment has
// been moved, not before.
//
// parseSizes reads STRIPE_SIZES: size=price_id:minor_units[:included_minor_units], comma
// separated, in the order they should be offered. The third field is the credit the size includes
// each month and may be left off, which is what every entry written before it existed means: no
// allowance, buy credit separately. A malformed entry is dropped with a warning rather than
// failing the boot — the rest of the deployment is not billing and should not be held hostage by
// a typo in it — and a size nobody configured is simply not offered. An entry with no Price id,
// `over_250=:0`, is offered and not sold: listed under its label with "Talk to us" where the fee
// would be, and refused by the checkout route.
func parseSizes(raw string) []Size {
	var out []Size
	for _, part := range splitList(raw) {
		key, rest, ok := strings.Cut(part, "=")
		if !ok {
			slog.Warn("STRIPE_SIZES entry has no '=', ignoring", "entry", part)
			continue
		}
		price, amount, ok := strings.Cut(rest, ":")
		if !ok {
			slog.Warn("STRIPE_SIZES entry has no ':<minor units>', ignoring", "entry", part)
			continue
		}
		// The included allowance is the optional tail. Cut before parsing so an entry in the
		// two-field spelling reads exactly as it did before this field existed.
		amount, included, hasIncluded := strings.Cut(amount, ":")
		minor, err := strconv.ParseInt(strings.TrimSpace(amount), 10, 64)
		if err != nil || minor < 0 {
			slog.Warn("STRIPE_SIZES amount is not a whole number of minor units, ignoring", "entry", part)
			continue
		}
		var includedMinor int64
		if hasIncluded {
			includedMinor, err = strconv.ParseInt(strings.TrimSpace(included), 10, 64)
			if err != nil || includedMinor < 0 {
				// The fee parsed and only the allowance did not. Dropping the size over that
				// would take a plan off sale because of a typo in what it throws in; selling it
				// with no allowance is the smaller wrong, and the warning says so.
				slog.Warn("STRIPE_SIZES included credit is not a whole number of minor units; selling the size with none",
					"entry", part)
				includedMinor = 0
			}
		}
		key = strings.TrimSpace(key)
		label := sizeLabels[key]
		if label == "" {
			label = key
		}
		out = append(out, Size{Key: key, PriceID: strings.TrimSpace(price), AmountMinor: minor,
			IncludedMinor: includedMinor, UserLimit: sizeUserLimits[key], JobLimit: sizeJobLimits[key], Label: label})
	}
	return out
}

// SizeByKey finds one configured size. The second return is false for a size this deployment does
// not sell, which the console renders as "contact sales" and the checkout route refuses.
func (c Config) SizeByKey(key string) (Size, bool) {
	for _, b := range c.StripeSizes {
		if b.Key == key {
			return b, true
		}
	}
	return Size{}, false
}

// SizeByPrice is the same map read backwards, for a subscription event that names a Price and not
// a size. A price this deployment does not sell answers false, and the webhook keeps the record it
// already has rather than believing an unknown one.
func (c Config) SizeByPrice(priceID string) (Size, bool) {
	for _, b := range c.StripeSizes {
		if b.PriceID != "" && b.PriceID == priceID {
			return b, true
		}
	}
	return Size{}, false
}

// BillingEnabled is the whole of the off-by-default switch, and it wants both keys. See the
// warning in LoadConfig for why half a configuration is no configuration.
func (c Config) BillingEnabled() bool {
	return c.StripeSecretKey != "" && c.StripeWebhookSecret != ""
}

// splitList reads a comma- or space-separated setting — a list of subnet ids, a list of
// security groups — into its non-empty members. Both separators are accepted because both
// spellings turn up in the wild and neither is worth a support round trip.
func splitList(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// workerK8sArch reads WORKER_K8S_ARCH: the kubernetes.io/arch a worker pod is pinned to, or ""
// for no pin. amd64 when unset, because the worker images this project publishes are built for
// nothing else (.github/workflows/release.yml) — unpinned, a Job landed on whichever node had
// room, and on an arm64 one the worker died with an exec format error that reads like a broken
// build. `any` is for a worker image built for every architecture the cluster runs.
func workerK8sArch(raw string) string {
	switch arch := strings.ToLower(strings.TrimSpace(raw)); arch {
	case "":
		return "amd64"
	case "any":
		return ""
	default:
		return arch
	}
}

// EnvFile is the dotenv file LoadConfig reads: ENV_FILE when set, otherwise .env.testing —
// local runs (start_dev.sh, live tests) use a throwaway copy so experimenting, and the
// MASTER_KEY the bot appends when none is set, never edits the deployed secrets. Those live
// in .env.prod, which only deploy/gcp/cloudrun.sh reads; Cloud Run has no dotenv file at all and
// takes everything from Secret Manager. .env is the fallback for a checkout predating the split.
func EnvFile() string {
	if v := os.Getenv("ENV_FILE"); v != "" {
		return v
	}
	if _, err := os.Stat(".env.testing"); err == nil {
		return ".env.testing"
	}
	return ".env"
}

func envFloat(k string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil {
		return v
	}
	return def
}

func LoadConfig() Config {
	// Snapshot what the shell already exported for the keys .env defines: Load never
	// overrides those, and a stale export (a forgotten .zshrc line) is otherwise invisible.
	file := EnvFile()
	dotenv, dotenvErr := godotenv.Read(file) // err = no dotenv file, e.g. on Cloud Run
	shell := map[string]string{}
	for _, k := range append(keysOf(dotenv), "LLM_API_KEY", "OPENROUTER_API_KEY") {
		if v, ok := os.LookupEnv(k); ok {
			shell[k] = v
		}
	}
	lookup := func(k string) (string, bool) { v, ok := shell[k]; return v, ok }
	_ = godotenv.Load(file)
	c := Config{
		SlackBotToken:      os.Getenv("SLACK_BOT_TOKEN"),
		SlackSigningSecret: os.Getenv("SLACK_SIGNING_SECRET"),
		MSTeamsAppID:       os.Getenv("MSTEAMS_APP_ID"),
		MSTeamsAppPassword: os.Getenv("MSTEAMS_APP_PASSWORD"),
		MSTeamsTenantID:    os.Getenv("MSTEAMS_TENANT_ID"),
		MSTeamsAppType:     env("MSTEAMS_APP_TYPE", msteamsSingleTenant),
		MSTeamsSignIn:      strings.ToLower(strings.TrimSpace(os.Getenv("MSTEAMS_SIGNIN"))),
		LLMBaseURL:         env("LLM_BASE_URL", "https://openrouter.ai/api/v1"),
		LLMKey:             env("LLM_API_KEY", os.Getenv("OPENROUTER_API_KEY")),
		Model:              env("LLM_MODEL", "z-ai/glm-5.3-flash"),
		HeavyModel:         env("HEAVY_MODEL", "z-ai/glm-5.3"),
		EmbedModel:         env("EMBED_MODEL", "qwen/qwen3-embedding-8b"),
		DatabaseURL:        env("DATABASE_URL", ""),
		RequirePostgres:    env("REQUIRE_POSTGRES", "") == "1",
		DBPath:             env("DB_PATH", "attesttag.db"),
		DocsDir:            env("DOCS_DIR", "docs"),
		Timezone:           env("TZ_NAME", "America/New_York"),
		MonthlyBudgetUSD:   envFloat("MONTHLY_BUDGET_USD", 20),
		DenyTraining:       env("DENY_TRAINING", "1") == "1",
		ProviderSort:       strings.ToLower(env("PROVIDER_SORT", "price")),
		ProviderOrder:      env("PROVIDER_ORDER", ""),
		Reasoning:          strings.ToLower(env("REASONING", "low")),
		SelfTest:           os.Getenv("SELF_TEST") == "1",
		HealthAddr:         env("HEALTH_ADDR", ":"+env("PORT", "8080")), // Cloud Run sets PORT
		MaxToolRounds:      envInt("MAX_TOOL_ROUNDS", 50),
		TurnMaxMinutes:     envInt("TURN_MAX_MINUTES", 12),
		TurnMaxUSD:         envFloat("TURN_MAX_USD", 0.50),
		RoutineRounds:      envInt("ROUTINE_ROUNDS", defaultRoutineRounds),
		RoutineMinutes:     envInt("ROUTINE_MINUTES", defaultRoutineMinutes),
		HistoryLimit:       envInt("HISTORY_LIMIT", 40),
		BotName:            env("BOT_NAME", "attest_tag"),
		LLMKeyName:         "OPENROUTER_API_KEY",

		PlatformMonthlyBudgetUSDPerOrg: envFloat("PLATFORM_MONTHLY_BUDGET_USD_PER_ORG", 0),
		PlatformMaxInFlightPerOrg:      envInt("PLATFORM_MAX_INFLIGHT_PER_ORG", 8),
		FreePlanBudgetUSD:              envFloat("FREE_PLAN_BUDGET_USD", 0),
		SupportEmail:                   env("SUPPORT_EMAIL", ""),
		Maintenance:                    os.Getenv("MAINTENANCE") == "1",
		SignupMode:                     signupModeOf(os.Getenv("SIGNUP_MODE")),
		OrgModelKeys:                   orgModelKeysOf(os.Getenv("ORG_MODEL_KEYS")),
		SiteURL:                        env("SITE_URL", ""),
		OperatorSecret:                 os.Getenv("OPERATOR_SECRET"),
		StripeSecretKey:                os.Getenv("STRIPE_SECRET_KEY"),
		StripeWebhookSecret:            os.Getenv("STRIPE_WEBHOOK_SECRET"),
		StripeSizes:                    parseSizes(env("STRIPE_SIZES", os.Getenv("STRIPE_BANDS"))),
		BillingCurrency:                strings.ToLower(env("BILLING_CURRENCY", "usd")),
		TopUpMinUSD:                    envFloat("BILLING_TOPUP_MIN_USD", 25),
		TopUpMaxUSD:                    envFloat("BILLING_TOPUP_MAX_USD", 10000),
		CreditLowUSD:                   envFloat("BILLING_LOW_BALANCE_USD", 10),
		InvestigationWorkers:           envInt("INVESTIGATION_WORKERS", 2),
	}
	if os.Getenv("LLM_API_KEY") != "" {
		c.LLMKeyName = "LLM_API_KEY"
	}
	c.LLMKeySource = keyProvenance(c.LLMKeyName, file, dotenv, lookup)
	c.WorkerMode = strings.ToLower(strings.TrimSpace(env("WORKER_MODE", "off")))
	switch c.WorkerMode {
	case "", "off":
		c.WorkerMode, c.WorkerPlatform = "off", ""
	case "local":
		c.WorkerPlatform = "local"
	case "workers":
		c.WorkerPlatform = detectWorkerPlatform()
		if c.WorkerPlatform == "" {
			slog.Error("WORKER_MODE=workers, but nothing here says which platform this is: " +
				"expected one of KUBERNETES_SERVICE_HOST, AWS_CONTAINER_CREDENTIALS_RELATIVE_URI, " +
				"CONTAINER_APP_NAME, K_SERVICE, or a Docker socket. Name the platform instead — " +
				"cloudrun, ecs, aca, k8s or docker. Treating as off")
			c.WorkerMode = "off"
		} else {
			slog.Info("fix-job worker", "mode", "workers", "platform", c.WorkerPlatform)
		}
	case "cloudrun", "ecs", "aca", "k8s", "docker":
		c.WorkerPlatform = c.WorkerMode
	default:
		slog.Warn("WORKER_MODE must be off, workers or local — or a platform named directly "+
			"(cloudrun, ecs, aca, k8s, docker). Treating as off", "value", c.WorkerMode)
		c.WorkerMode, c.WorkerPlatform = "off", ""
	}
	if c.DatabaseURL != "" && c.DBPath != "" && c.DBPath != "attesttag.db" {
		slog.Warn("both DATABASE_URL and DB_PATH are set; DATABASE_URL wins and DB_PATH is ignored")
	}
	// A typo in SIGNUP_MODE closes the door rather than opening it (signupModeOf), which is the
	// safe way round but silent — so say so, and say which mode is in force either way. Who may
	// create an account on a deployment reachable from the internet is worth one line in the log.
	if v := os.Getenv("SIGNUP_MODE"); v != "" && !strings.EqualFold(strings.TrimSpace(v), c.SignupMode) {
		slog.Warn("SIGNUP_MODE must be open, first-run or closed; treating as first-run", "value", v)
	}
	// The same for ORG_MODEL_KEYS: a typo narrows it to enterprise (orgModelKeysOf), and an
	// organisation outside that whose key is stored then has its model calls refused.
	if v := os.Getenv("ORG_MODEL_KEYS"); v != "" && !strings.EqualFold(strings.TrimSpace(v), c.OrgModelKeys) {
		slog.Warn("ORG_MODEL_KEYS must be all, enterprise or off; treating as enterprise", "value", v)
	}
	// The per-organisation caps and signup throttles, after the dotenv file has been read —
	// package-level initialisation would run before it and see none of them (limits.go).
	applyLimitOverrides()
	if c.Maintenance {
		slog.Warn("MAINTENANCE=1 — Slack and Teams deliveries are acknowledged and DROPPED, and the schedulers are not running. " +
			"This is the cutover freeze; unset it to resume.")
	}
	if c.SignupMode == SignupOpen {
		slog.Warn("SIGNUP_MODE=open — anyone who can reach the console can create an account and their own organisation")
	} else {
		slog.Info("signup mode", "mode", c.SignupMode)
	}
	// A short secret behind a write API is a guessable one. Refusing it here, at startup, is
	// louder than serving 401s to the operator and 200s to whoever guesses first.
	if n := len(c.OperatorSecret); n > 0 && n < minOperatorSecret {
		slog.Warn("OPERATOR_SECRET is too short; the operator routes are off until it is longer", "min_chars", minOperatorSecret)
		c.OperatorSecret = ""
	}
	// Billing needs both keys or neither. A deployment that can charge a card and cannot hear
	// the result would take money and never credit it, which is worse than not taking it — so
	// half a configuration is switched off rather than half-honoured, loudly.
	switch {
	case c.StripeSecretKey != "" && c.StripeWebhookSecret == "":
		slog.Warn("STRIPE_SECRET_KEY is set but STRIPE_WEBHOOK_SECRET is not; billing is off — a service that can charge a card and cannot hear the result must not charge one")
		c.StripeSecretKey = ""
	case c.StripeSecretKey == "" && c.StripeWebhookSecret != "":
		slog.Warn("STRIPE_WEBHOOK_SECRET is set but STRIPE_SECRET_KEY is not; billing is off")
		c.StripeWebhookSecret = ""
	}
	if c.BillingEnabled() {
		if len(c.StripeSizes) == 0 {
			slog.Warn("billing is on with no STRIPE_SIZES: credit top-ups work and no plan can be subscribed to")
		}
		for _, b := range c.StripeSizes {
			// Printed so that a price that has drifted from the one in the Stripe dashboard is
			// visible in the deploy log rather than in a customer's invoice.
			slog.Info("billing size", "size", b.Key, "price", b.PriceID, "usd", float64(b.AmountMinor)/100)
		}
	}
	// deploy/entrypoint.sh used to apply these defaults before exec'ing the binary; it no longer
	// exists, so they live here and are passed explicitly to the Litestream child.
	c.ReplicaBucket = os.Getenv("LITESTREAM_BUCKET")
	c.ReplicaPath = env("LITESTREAM_PATH", "litestream/attesttag.db")
	c.LitestreamBin = env("LITESTREAM_BIN", "litestream")
	c.LitestreamConf = os.Getenv("LITESTREAM_CONFIG")
	// Which bucket the replica goes to, if any. A malformed URL is fatal rather than a warning:
	// the alternative is a deployment that believes it is replicating and is not.
	replica, err := resolveReplica(c.ReplicaPath)
	if err != nil {
		slog.Error("replication", "err", err)
		os.Exit(2)
	}
	c.Replica = replica
	if c.Replica != nil && c.DatabaseURL != "" {
		// Nothing to replicate: the rows are in Postgres, and the SQLite file this would stream
		// is not the database this process reads.
		slog.Warn("DATABASE_URL is set, so the SQLite replica is not used", "replica", c.Replica.String())
		c.Replica = nil
	}

	// On k8s and docker the "job name" is the image to run, because there is no job object
	// created ahead of time to name — the bot writes the whole pod spec per execution. Both
	// spell it WORKER_IMAGE, which is what an operator expects to be setting there, and
	// WORKER_JOB_NAME stays the one field the ecosystem routing below overrides.
	switch c.WorkerPlatform {
	case "k8s", "docker":
		c.WorkerJobName = env("WORKER_JOB_NAME", os.Getenv("WORKER_IMAGE"))
	default:
		c.WorkerJobName = env("WORKER_JOB_NAME", "attesttag-worker")
	}
	c.WorkerProject = os.Getenv("WORKER_PROJECT")
	// us-central1 is a Cloud Run region and means nothing to ECS, so only default it where it
	// is one: an AWS deployment inherits AWS_REGION, which every task already has.
	if c.WorkerPlatform == "ecs" {
		c.WorkerRegion = env("WORKER_REGION", env("AWS_REGION", os.Getenv("AWS_DEFAULT_REGION")))
	} else {
		c.WorkerRegion = env("WORKER_REGION", "us-central1")
	}
	c.WorkerSAEmail = os.Getenv("WORKER_SA_EMAIL")
	c.WorkerCPU = env("WORKER_CPU", "2")
	c.WorkerMemory = env("WORKER_MEMORY", "4Gi")
	c.WorkerECSCluster = env("WORKER_ECS_CLUSTER", "attesttag")
	c.WorkerECSSubnets = splitList(os.Getenv("WORKER_ECS_SUBNETS"))
	c.WorkerECSSecurityGroups = splitList(os.Getenv("WORKER_ECS_SECURITY_GROUPS"))
	c.WorkerECSPublicIP = env("WORKER_ECS_ASSIGN_PUBLIC_IP", "1") == "1"
	c.WorkerAzureSubscription = os.Getenv("WORKER_AZURE_SUBSCRIPTION")
	c.WorkerAzureResourceGroup = os.Getenv("WORKER_AZURE_RESOURCE_GROUP")
	c.WorkerK8sNamespace = os.Getenv("WORKER_K8S_NAMESPACE")
	c.WorkerK8sServiceAccount = os.Getenv("WORKER_K8S_SERVICE_ACCOUNT")
	c.WorkerK8sPullSecret = os.Getenv("WORKER_K8S_PULL_SECRET")
	c.WorkerK8sArch = workerK8sArch(os.Getenv("WORKER_K8S_ARCH"))
	c.WorkerDockerHost = env("WORKER_DOCKER_HOST", "unix:///var/run/docker.sock")
	c.WorkerDockerNetwork = os.Getenv("WORKER_DOCKER_NETWORK")
	warnWorkerConfig(c)
	oidcDefault := "0"
	if c.WorkerPlatform == "cloudrun" {
		oidcDefault = "1"
	}
	c.WorkerRequireOIDC = env("WORKER_REQUIRE_OIDC", oidcDefault) == "1"
	c.WorkerLLMBaseURL = env("WORKER_LLM_BASE_URL", c.LLMBaseURL)
	c.WorkerLLMKey = env("WORKER_LLM_API_KEY", c.LLMKey)
	c.WorkerEngineAPIKey = os.Getenv("WORKER_ENGINE_API_KEY")
	c.WorkerCacheBucket = env("WORKER_CACHE_BUCKET", c.ReplicaBucket)
	c.WorkerCacheOff = strings.EqualFold(os.Getenv("WORKER_CACHE"), "off")
	c.WorkerJobNames = map[string]string{}
	for _, pair := range strings.Split(os.Getenv("WORKER_JOB_NAMES"), ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if ok && k != "" && v != "" {
			c.WorkerJobNames[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	c.OpenRouterProvisioningKey = os.Getenv("OPENROUTER_PROVISIONING_KEY")
	if dotenvErr == nil {
		for _, s := range envShadows(dotenv, lookup) {
			msg := s.Key + ": using the value from the shell environment, which differs from " + file
			if isSecretName(s.Key) {
				slog.Warn(msg, "env", s.Env, "dotenv", s.DotEnv)
			} else {
				slog.Info(msg, "env", s.Env, "dotenv", s.DotEnv)
			}
		}
	}
	// SLACK_BOT_TOKEN is deliberately not here any more: bot tokens arrive from installs and
	// live sealed in the teams table, one per connected workspace. The signing secret
	// authenticates HTTP event and interaction payloads for the Slack app.
	//
	// The client pair used to be a warning here rather than a refusal, and that was the wrong
	// call: an install *is* an OAuth round trip, so without it there is no way to attach a
	// workspace at all. The deployment came up, looked healthy, and failed three screens into
	// the console with "Connecting a workspace needs SLACK_CLIENT_ID and SLACK_CLIENT_SECRET on
	// the server" — after deploying, which is the expensive place to learn it. A startup log
	// nobody reads on purpose is not where that belongs.
	//
	// A slice rather than a map so the first missing variable is reported deterministically;
	// ranging a map named a different one each run.
	for _, req := range []struct{ name, value, why string }{
		{"SLACK_SIGNING_SECRET", c.SlackSigningSecret,
			"it authenticates every event and interaction payload Slack sends; Basic Information → App Credentials"},
		{"OPENROUTER_API_KEY (or LLM_API_KEY)", c.LLMKey,
			"there is nothing to answer with; any OpenAI-compatible endpoint works via LLM_BASE_URL"},
		{"SLACK_CLIENT_ID", os.Getenv("SLACK_CLIENT_ID"),
			"connecting a workspace is an OAuth install and cannot start without it; Basic Information → App Credentials, and register <base>/slack/oauth/callback as a redirect URL on the app"},
		{"SLACK_CLIENT_SECRET", os.Getenv("SLACK_CLIENT_SECRET"),
			"the other half of that install; same page as the client id"},
	} {
		if req.value == "" {
			slog.Error("missing required env var", "name", req.name, "why", req.why)
			os.Exit(2)
		}
	}
	if c.SlackBotToken != "" {
		slog.Warn("SLACK_BOT_TOKEN is set but no longer used: workspaces are connected from /admin/workspaces and their tokens are stored sealed. Remove it")
	}
	if why := c.msteamsMisconfigured(); why != "" {
		slog.Error("Microsoft Teams is half configured", "why", why)
		os.Exit(2)
	}
	return c
}

// msteamsMisconfigured says what is missing from a Teams configuration that has been started, or
// "" when it is whole or absent. Teams is optional, so none of it is required — but half of it is a
// refusal rather than a warning, for the reason the Slack client pair is: the deployment would come
// up looking healthy and fail on the first message a tenant sent it, which is the expensive place
// to find out.
func (c Config) msteamsMisconfigured() string {
	if c.MSTeamsSignIn != "" && c.MSTeamsSignIn != msSignInAnyOrganisation && !isEntraObjectID(c.MSTeamsSignIn) {
		return "MSTEAMS_SIGNIN is " + c.MSTeamsSignIn + "; it is organizations, for any work or school account, or a tenant id"
	}
	if c.MSTeamsAppID == "" {
		if c.MSTeamsAppPassword != "" {
			return "MSTEAMS_APP_PASSWORD is set without MSTEAMS_APP_ID, the app registration's client id"
		}
		if c.MSTeamsSignIn != "" {
			return "MSTEAMS_SIGNIN is set without MSTEAMS_APP_ID: Sign in with Microsoft goes through the bot's app registration"
		}
		return ""
	}
	if c.MSTeamsAppPassword == "" {
		return "MSTEAMS_APP_PASSWORD is missing: the app registration's client secret, without which the bot cannot get a token to answer with"
	}
	switch c.MSTeamsAppType {
	case msteamsSingleTenant:
		if c.MSTeamsTenantID == "" {
			return "MSTEAMS_TENANT_ID is missing: a single-tenant bot takes its tokens from the directory its app registration lives in"
		}
	case msteamsMultiTenant:
	default:
		return "MSTEAMS_APP_TYPE is " + c.MSTeamsAppType + "; it is SingleTenant or MultiTenant, the Azure Bot's own type"
	}
	return ""
}

// msteamsConfigured is whether this deployment talks to Teams at all.
func (c Config) msteamsConfigured() bool {
	return c.MSTeamsAppID != "" && c.msteamsMisconfigured() == ""
}

// turnCeiling is the longest any one turn may run, however many rounds its channel allows. It
// is a wall the operator owns rather than a tenant setting: a turn holds a Slack delivery open
// while it runs, and a container that is asked to stop gives what is running only seconds.
func (c Config) turnCeiling() time.Duration {
	if c.TurnMaxMinutes <= 0 {
		return 0
	}
	return time.Duration(c.TurnMaxMinutes) * time.Minute
}

// turnSpendCeiling is the same wall in money. Rounds and the clock bound how long a turn digs;
// neither bounds what the digging costs, because the transcript is re-sent every round and so
// cost grows with the square of the rounds, not with them. Measured over eight days of
// production: the median turn cost $0.002 and the mean $0.024, and three turns — one routine,
// three days running — reached 200 rounds at $1.12, $1.41 and $1.80, together 78% of all spend.
// Fifty cents is some seven times the largest turn that ever did its job, and a twentieth of
// what that routine had already spent by the time anything noticed. Zero turns it off.
func (c Config) turnSpendCeiling() float64 {
	if c.TurnMaxUSD <= 0 {
		return 0
	}
	return c.TurnMaxUSD
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// warnWorkerConfig says at startup what each mode cannot work without.
//
// The dispatcher constructors refuse these too, but they only run when somebody asks for a fix
// — so without this the deployment comes up, looks healthy, offers the tool in Slack, and fails
// on the first request in front of whoever asked. A log line at boot is cheap and is read by the
// person who just changed the setting.
//
// It warns rather than exits: unlike the Slack credentials, a worker that cannot start leaves
// every other part of the bot working, and taking the whole deployment down over it would be the
// larger failure.
func warnWorkerConfig(c Config) {
	missing := func(names ...string) {
		for _, n := range names {
			slog.Warn("the fix-job worker cannot start without this", "platform", c.WorkerPlatform, "name", n)
		}
	}
	switch c.WorkerPlatform {
	case "ecs":
		if c.WorkerRegion == "" {
			missing("WORKER_REGION (or AWS_REGION)")
		}
		if len(c.WorkerECSSubnets) == 0 {
			missing("WORKER_ECS_SUBNETS")
		}
		if len(c.WorkerECSSecurityGroups) == 0 {
			// Not fatal — the VPC default group is used — but that group often has no egress
			// rule, and a task that cannot reach GitHub looks like a worker that hangs.
			slog.Warn("WORKER_ECS_SECURITY_GROUPS is unset, so worker tasks use the VPC default group; if it has no egress rule the worker will not reach GitHub or the model endpoint")
		}
	case "aca":
		if c.WorkerAzureSubscription == "" {
			missing("WORKER_AZURE_SUBSCRIPTION")
		}
		if c.WorkerAzureResourceGroup == "" {
			missing("WORKER_AZURE_RESOURCE_GROUP")
		}
	case "k8s":
		if c.WorkerJobName == "" {
			missing("WORKER_IMAGE")
		}
		if os.Getenv("KUBERNETES_SERVICE_HOST") == "" {
			slog.Warn("the worker platform is k8s but this process is not running in a cluster: there is no service account token to create Jobs with")
		}
	case "docker":
		if c.WorkerJobName == "" {
			missing("WORKER_IMAGE")
		}
		// Worth saying out loud once per boot, because it is the one mode whose cost is not
		// visible in its own configuration.
		slog.Warn("the worker platform is docker, which gives the bot the Docker socket — root on the host; see deploy/local/worker.yml")
	}
}

// detectWorkerPlatform works out which platform this container is running on, so that
// WORKER_MODE=workers is all an operator has to write.
//
// Every signal here is one the platform sets on its own containers and one of the dispatchers
// already depends on, so nothing is being guessed from a coincidence: the Kubernetes API service,
// the ECS task-role endpoint, the Container Apps revision, the Cloud Run service name. Only the
// Docker socket is a filesystem check rather than an environment variable, which is why it is
// last — it is the one that could plausibly be present somewhere it does not mean anything.
//
// It returns "" rather than a default. A deployment told to run workers and unable to say where
// should say so at boot, not start a job on the wrong thing an hour later.
func detectWorkerPlatform() string {
	for _, p := range []struct{ name, env string }{
		{"k8s", "KUBERNETES_SERVICE_HOST"},
		{"ecs", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"},
		{"ecs", "AWS_CONTAINER_CREDENTIALS_FULL_URI"},
		{"aca", "CONTAINER_APP_NAME"},
		{"cloudrun", "K_SERVICE"},
	} {
		if os.Getenv(p.env) != "" {
			return p.name
		}
	}
	sock := strings.TrimPrefix(env("WORKER_DOCKER_HOST", "unix:///var/run/docker.sock"), "unix://")
	if st, err := os.Stat(sock); err == nil && st.Mode()&os.ModeSocket != 0 {
		return "docker"
	}
	return ""
}
