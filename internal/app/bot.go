// Package app is attest_tag: a Claude-Tag-style AI teammate for Slack on open-weight models.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"attesttag/ui"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// mentionRe matches both forms Slack sends a mention in: `<@U123>` and the older
// `<@U123|handle>`, which the id-only pattern used to miss and leave in the text verbatim.
// Rewriting them is (*Chat).NameMentions's job; nothing else should erase them.
var mentionRe = regexp.MustCompile(`<@([A-Za-z0-9-]+)(?:\|[^>]*)?>`)

type Bot struct {
	cfg    Config
	slacks *ChatRegistry
	store  *Store
	agent  *Agent
	ix     *Indexer
	sealer *Sealer
	// ghApp is the GitHub App this deployment installs as (github_app.go). Nil when none is
	// configured, which is what installConfigured-style checks test before offering the flow.
	ghApp *githubApp
	mail  Mailer
	// pay is the payment provider (billing_stripe.go). Always non-nil: offPayments when this
	// deployment sells nothing, which is most of them.
	pay      Payments
	settings *settingsCache
	resolver *Resolver
	proxy    *Proxy
	docs     DocStore
	jobs     *JobRunner

	scopeSyncMu sync.Mutex
	scopeSynced map[string]time.Time // per team: one workspace's sync must not throttle another's

	askedMu sync.Mutex
	asked   map[string]time.Time // "user:team": an invitation already asked for, once a day

	// Console assistant turns in flight, per organisation. Its own counter rather than the
	// Agent's: a.inFlight counts runs registered by beginRun, and a console turn never is one.
	assistantMu   sync.Mutex
	assistantRuns map[int64]int

	operatorAttempts *rateLimiter // per-address throttle on the operator API (operator.go)

	// Turns started from the Slack inbox and still running. Shutdown waits on this, so a turn
	// that can finish inside the grace period closes its delivery instead of leaving it to be
	// redelivered.
	turns sync.WaitGroup
}

// Run starts the bot and blocks until the process is signalled.
//
// The order here is the single-writer contract, and each step is load-bearing:
// listen first so the revision can go Ready, take the write lease before anything reads or writes
// the database, and only then open the store and start the loops that use it.
func Run() {
	lvl := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		lvl = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})))

	// Before anything that can block. This process is PID 1 in the container, and the kernel
	// discards SIGTERM at PID 1 unless a handler is installed — Go installs one only once
	// signal.Notify has been called. Registering late means an early SIGTERM is dropped and the
	// container hangs until Cloud Run SIGKILLs it, which is exactly when the final sync is lost.
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	// The subsystems watch ctx rather than the signal context, so losing the write lease can stand
	// them down just as a signal would.
	ctx, cancel := context.WithCancel(sigCtx)
	defer cancel()

	cfg := LoadConfig()

	// The listener binds before any slow startup work. A bind failure must fail startup rather
	// than leaving an apparently healthy process with no way to receive Slack events — and
	// answering /health from this moment on is what lets Cloud Run mark the revision Ready, shift
	// traffic, and finally tell the previous container to stop. A container that waited for the
	// lease before listening would deadlock against the very container it is waiting for.
	g := newGate()
	ln, err := net.Listen("tcp", cfg.HealthAddr)
	if err != nil {
		slog.Error("listen", "addr", cfg.HealthAddr, "err", err)
		os.Exit(1)
	}
	// Every response goes out through secureHeaders (headers.go), including the 503s served before
	// the database is open. ReadTimeout bounds how long a client may take to send a body —
	// generous enough for a document upload on a slow link, short enough that twenty stalled
	// uploads cannot hold the instance's connections forever.
	srv := &http.Server{Handler: secureHeaders(g, inlineScriptHashes(ui.FS()), cfg.SiteURL),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 2 * time.Minute, IdleTimeout: 60 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	slog.Info("listening", "addr", cfg.HealthAddr)

	// One writer, enforced. Cloud Run's --max-instances 1 is not a writer lock: during a deploy
	// the new container is running while the old one still owns the database. Until this lease is
	// held, nothing here may touch the file — restoring it least of all.
	var lease *Lease
	var rep *replicator
	switch {
	case cfg.DatabaseURL != "":
		// Postgres holds the rows and its own locks. There is no local file to stream and no
		// writer to fence from out here, which is the whole reason more than one instance is
		// allowed on it.
		g.setWriter("postgres")

	case cfg.Replica != nil:
		t := cfg.Replica
		slog.Info("replicating the database", "to", t.String(), "named_by", t.provenance())
		lease, err = newReplicaLease(sigCtx, cfg, t)
		switch {
		case errors.Is(err, errNoConditionalWrites):
			// The bucket can hold the replica but cannot fence a writer, so say precisely that
			// rather than imply a protection that is not there. One instance is still correct
			// and still durable; a second one against this database would corrupt it.
			lease = nil
			g.setWriter("unfenced")
			slog.Warn("this object store does not honour conditional writes, so there is no write lease: "+
				"replication is on, and this deployment must run exactly one instance",
				"store", t.String())
		case err != nil:
			slog.Error("write lease", "err", err)
			os.Exit(1)
		default:
			lease.onState = g.setWriter
			if err := lease.Acquire(sigCtx); err != nil {
				// Exiting is right: the database has an owner, and Cloud Run restarting us is a
				// visible retry rather than a container quietly serving 503 forever.
				slog.Error("could not take the write lease", "err", err)
				os.Exit(1)
			}
			go lease.Run(ctx)
		}

		if rep, err = newReplicator(cfg); err != nil {
			slog.Error("replicate", "err", err)
			os.Exit(1)
		}
		// Restoring is safe here because the lease above is held — or, on a store that cannot
		// fence, because the operator has been told this deployment is single-instance.
		if err := rep.Restore(sigCtx); err != nil {
			slog.Error("restore", "err", err)
			os.Exit(1)
		}
		if err := rep.Start(); err != nil {
			slog.Error("replicate", "err", err)
			os.Exit(1)
		}
		go rep.Run(ctx)

	default:
		g.setWriter("unreplicated")
		slog.Warn("no bucket is configured (DOCS_S3_URL, LITESTREAM_S3_URL or LITESTREAM_BUCKET): " +
			"the database is a local file with no replication and no write lease, so backing up " +
			"the volume it lives on is yours to do — and on a platform with no persistent disk it is lost on restart")
	}

	// DATABASE_URL first, DB_PATH second: see the comment on Config.DatabaseURL. One
	// expression, so the two cannot disagree about which database this process is using.
	dsn := cfg.DatabaseURL
	if dsn == "" {
		dsn = cfg.DBPath
	}
	// Said before the store opens, because the failure this prevents is a successful one: with
	// no DATABASE_URL the line above falls back to a path, and opening a path that is not there
	// creates it. A deployment whose rows are in Postgres would come up empty and serving.
	if _, _, pg := parseDSN(dsn); cfg.RequirePostgres && !pg {
		slog.Error("REQUIRE_POSTGRES is set and this is not a Postgres DSN; refusing to start on SQLite. "+
			"Check that DATABASE_URL reached the container", "dsn_is_empty", cfg.DatabaseURL == "")
		os.Exit(1)
	}
	store, err := OpenStore(dsn)
	if err != nil {
		slog.Error("open store", "err", err, "postgres", cfg.DatabaseURL != "")
		os.Exit(1)
	}

	llm := NewLLM(cfg)
	ix := NewIndexer(llm, store, cfg.DocsDir)
	sealer, err := NewSealer()
	if err != nil {
		slog.Error("sealer", "err", err)
		os.Exit(1)
	}
	// Mail never stops startup: NewMailer warns and degrades to logging when no key is set.
	mail := NewMailer()
	// Billing is off unless both Stripe keys are set; NewPayments returns a refusing stub
	// otherwise and billingRoutes registers nothing at all.
	pay := NewPayments(cfg)
	// A half-configured app is a startup error rather than a first-call one: the console would
	// otherwise offer an Install button that leads nowhere. Nothing configured is fine and just
	// means this deployment connects repositories with pasted tokens.
	ghApp, err := newGitHubApp()
	if err != nil {
		slog.Error("github app", "err", err)
		os.Exit(1)
	}
	if ghApp.configured() {
		slog.Info("github app", "slug", ghApp.slug, "app_id", ghApp.id, "can_install", ghApp.canInstall())
		if !ghApp.canInstall() {
			slog.Warn("github app: installations already connected keep working, but nobody can install it from " +
				"the console until GITHUB_APP_CLIENT_ID and GITHUB_APP_CLIENT_SECRET are set")
		}
	}
	// Bot tokens come from installs, not the environment. The registry is built from the
	// database and never calls Slack at boot, so one revoked workspace cannot stop the process.
	slacks := NewChatRegistry(store, sealer)
	// Microsoft Teams, when this deployment has an app registration for it. Nil otherwise, and
	// then a Teams row in the registry is an error rather than a client.
	slacks.msteams = newMSTeamsClient(cfg, store)
	if slacks.msteams != nil {
		slog.Info("microsoft teams", "app_id", cfg.MSTeamsAppID, "type", cfg.MSTeamsAppType, "endpoint", "/msteams/messages")
	}
	settings := newSettingsCache(store, cfg)
	// Where each organisation's model calls go: this deployment's endpoint, or the one it brought
	// (model_endpoints.go). The indexer and the agent both ask it, never the endpoint directly.
	endpoints := newModelEndpoints(cfg, llm, store, sealer, settings)
	ix.endpoints = endpoints
	resolver := NewResolver(store)
	proxy := NewProxy(sealer, store)

	var docs DocStore = &localDocs{dir: cfg.DocsDir, store: store}
	// Where documents live. A local folder unless one of the two object stores is configured;
	// S3 wins if both are, because naming it is the more deliberate act. The S3 one is what
	// makes this run on a platform with no persistent disk and no FUSE mount — which is every
	// one of them except Cloud Run and a VM.
	if u := os.Getenv("DOCS_S3_URL"); u != "" {
		sd, err := newS3Docs(u, os.Getenv("DOCS_S3_KEY_ID"), os.Getenv("DOCS_S3_SECRET"), store)
		if err != nil {
			slog.Error("s3 docs", "err", err)
			os.Exit(1)
		}
		slog.Info("documents", "store", "s3", "bucket", sd.bucket, "endpoint", sd.endpoint)
		docs = sd
	} else if bucket := os.Getenv("DOCS_BUCKET"); bucket != "" {
		gd, err := newGCSDocs(ctx, bucket, env("DOCS_PREFIX", "docs"), store)
		if err != nil {
			slog.Error("gcs docs", "err", err)
			os.Exit(1)
		}
		docs = gd
	}
	b := &Bot{cfg: cfg, slacks: slacks, store: store, ix: ix, sealer: sealer, ghApp: ghApp, mail: mail, pay: pay, settings: settings, resolver: resolver, proxy: proxy, docs: docs,
		scopeSynced: map[string]time.Time{}}
	proxy.ghApp = ghApp // installation tokens are minted from it (github_token.go)
	ix.SetDocs(b.docs)  // the !ingest command reaches the indexer through the agent, which has no store
	b.agent = NewAgent(cfg, llm, slacks, store, ix, resolver, proxy, settings)
	b.agent.endpoints = endpoints
	endpoints.alert = b.agent.alert
	b.agent.mcp.token = b.freshMCPToken
	b.agent.sweepAccess = b.sweepAccessRequests
	b.agent.cancelled = b.accessWithdrawn
	b.jobs = NewJobRunner(cfg, store, slacks, proxy, settings)
	b.jobs.agent = b.agent
	b.agent.jobs = b.jobs
	ix.scopes = func(ctx context.Context, orgID int64) map[string]string {
		m, _ := store.DocumentScopes(ctx, orgID)
		return m
	}
	teams, _ := store.ActiveTeams(ctx)
	// The startup line describes the deployment, not any one organisation, so the model and
	// approver summary come from the environment defaults rather than a customer's settings.
	slog.Info("started", "workspaces", len(teams), "model", cfg.Model, "db", describeDSN(dsn), "docs", describeDocStore(docs), "selftest", cfg.SelfTest,
		"llm_key", cfg.LLMKeyFingerprint()+" ("+cfg.LLMKeyName+" from "+cfg.LLMKeySource+")", "usable_by", describeBotAccess(), "writer", g.writerState())
	if len(teams) == 0 {
		what := "a Slack workspace"
		if slacks.msteams != nil {
			what = "Slack or Microsoft Teams"
		}
		slog.Warn("nothing is connected yet — open /admin/ and the console will walk you through connecting " + what)
	}
	for _, t := range teams {
		slog.Info("workspace", "team", t.TeamID, "name", t.Name, "bot", t.BotUserID)
		if !t.EmailScope {
			slog.Warn("workspace is missing users:read.email, so nobody can be checked against ALLOWED_EMAIL_DOMAINS — reinstall it", "team", t.TeamID)
		}
		if !t.DMScope {
			slog.Warn("workspace is missing im:write, so the bot cannot DM approvers — reinstall it", "team", t.TeamID)
		}
	}
	if len(allowedEmailDomains()) == 0 {
		slog.Warn("ALLOWED_EMAIL_DOMAINS is not set: an organisation with no email-domain list of its own lets every member of its workspace use the bot. " +
			"Guests and people from other organisations (Slack Connect, other Microsoft 365 tenants) are refused either way, unless the organisation allows them under Settings → Security")
	}
	// Under the cutover freeze nothing that writes is started. The listener still comes up, so
	// /health answers, the console can be opened to read the banner, and Slack's Request URL
	// can still be verified — but no routine runs, no queued delivery is dispatched, and no
	// document is ingested, because every one of those would write to a database that is about
	// to be replaced by a snapshot taken before they did.
	deliveriesDone := make(chan struct{})
	if cfg.Maintenance {
		close(deliveriesDone)
	} else {
		// The counters that are security controls rather than conveniences: signup, invitations,
		// the plan request, the help button. Counted per process they give N instances N times
		// the budget the number claims, with nothing saying so (limits.go).
		for _, l := range []*rateLimiter{signups, invites, planRequests, helpRequests, emailTurns} {
			l.shareAcross(store)
		}
		// The login lockout (auth_slack.go, loginFails) is still per instance. It counts runs
		// of consecutive failures with a growing backoff rather than events in a window, so it
		// does not fit this shape and wants a table of its own. Until then N instances mean a
		// password guesser gets N times the attempts — bounded, but not by the number the code
		// says.

		// Behind a leader lease: the loops that must run once across the deployment, whatever
		// the instance count. On one instance each takes its lease on the first poll and runs
		// exactly as it did before (singleton.go).
		go b.leaderLoop(ctx, "scope-sync", b.scopeSyncLoop)
		go b.leaderLoop(ctx, "ingest", b.ingestLoop)
		go b.leaderLoop(ctx, "drive-sync", b.RunDriveSyncs)
		go b.leaderLoop(ctx, "job-reconciler", b.jobs.RunReconciler)
		go b.leaderLoop(ctx, "retention", b.runRetention)
		// Billing's own housekeeping: writing spend up into the statement, warning accounts that
		// are running low, and dropping webhook dedup keys Stripe has stopped retrying. Behind
		// the lease because all three are once-per-deployment, and inside this block because a
		// maintenance freeze is exactly when nothing should be writing.
		if b.cfg.BillingEnabled() {
			go b.leaderLoop(ctx, "billing", b.runBilling)
		}

		// Not behind one, deliberately. The inbox dispatcher and the investigation lane claim
		// their own rows under a lease already (store_slack_deliveries.go, claimInvestigation),
		// so several instances sharing them is the point rather than a hazard — work spreads
		// instead of queueing behind one. The routine scheduler is the same after this commit:
		// it claims each routine on the row.
		go func() {
			defer close(deliveriesDone)
			b.runSlackDeliveries(ctx)
		}()
		go b.agent.RunScheduler(ctx)
		go b.agent.RunInvestigations(ctx)
	}

	// Open for business: the 503s stop here.
	mux := http.NewServeMux()
	b.routes(mux, ui.FS())
	g.live(mux, b.healthDetails)
	slog.Info("console + api listening", "addr", cfg.HealthAddr)

	code := 0
	select {
	case <-ctx.Done(): // signalled
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			slog.Error("http server stopped", "err", err)
			code = 1
		}
	case <-leaseLost(lease):
		// Another container owns the database now. Everything this process does from here on
		// would be thrown away, so it stops and lets Cloud Run start a replacement.
		code = 1
	case <-replicationFailed(rep):
		// Writing to Cloud Run's ephemeral disk with nothing streaming it off is silent data
		// loss, which is worse than being down.
		code = 1
	}

	shutdown(shutdownArgs{code: code, gate: g, srv: srv, cancel: cancel, stopSignals: stopSignals,
		deliveries: deliveriesDone, turns: &b.turns, store: store, rep: rep, lease: lease})
	os.Exit(code)
}

// A nil channel blocks forever in a select, which is exactly what "there is no lease here" means.
func leaseLost(l *Lease) <-chan struct{} {
	if l == nil {
		return nil
	}
	return l.Lost()
}

func replicationFailed(r *replicator) <-chan struct{} {
	if r == nil {
		return nil
	}
	return r.Fatal()
}

type shutdownArgs struct {
	code        int
	gate        *gate
	srv         *http.Server
	cancel      context.CancelFunc
	stopSignals func()
	deliveries  <-chan struct{}
	turns       *sync.WaitGroup
	store       *Store
	rep         *replicator
	lease       *Lease
}

// shutdown unwinds in the one order that keeps the replica complete: stop taking work, let the
// database finish what it has, close it, and only then let Litestream make its final sync.
//
// Everything is measured against one deadline just under Cloud Run's ~10s SIGTERM grace, with the
// last few seconds reserved: flushing the database and the replica must not be squeezed out by a
// slow turn upstream of them, because a lost turn is redelivered and a lost final sync is not.
func shutdown(a shutdownArgs) {
	hard := time.Now().Add(leaseShutdown)
	flushReserve := 3 * time.Second
	// left returns what is left before d, capped at max and never negative.
	left := func(d time.Time, max time.Duration) time.Duration {
		if r := time.Until(d); r < max {
			return max0(r)
		}
		return max
	}
	work := hard.Add(-flushReserve)

	a.gate.drain()
	a.stopSignals() // a second signal now kills us outright, as an operator would expect
	a.cancel()

	sctx, done := context.WithTimeout(context.Background(), left(work, 2*time.Second))
	a.srv.Shutdown(sctx) // lets in-flight Slack posts finish, unlike a bare Close
	done()
	a.srv.Close()

	select {
	case <-a.deliveries: // finish inbox DB operations before closing the store
	case <-time.After(left(work, 2*time.Second)):
		slog.Warn("the Slack inbox did not finish draining")
	}

	// Turns already running get what remains of the working budget. One that finishes closes its
	// own delivery; one that does not leaves the row open, so its lease expires and the next
	// container redelivers it. That is the whole reason this wait is allowed to be short.
	if a.turns != nil {
		if !waitFor(a.turns, left(work, flushReserve)) {
			slog.Warn("turns still running at shutdown; their Slack deliveries stay open and will be redelivered")
		}
	}

	if a.store != nil {
		a.store.Close()
	}
	if a.rep != nil {
		if err := a.rep.Stop(left(hard, 2*time.Second)); err != nil {
			slog.Error("litestream shutdown", "err", err)
		}
	}
	// Only on a clean exit. Once the lease is lost the object belongs to another container, and
	// releasing it then would evict a healthy writer.
	if a.lease != nil && a.code == 0 {
		if err := a.lease.Release(context.Background()); err != nil {
			slog.Warn("could not release the write lease; a successor waits for it to expire", "err", err)
		}
	}
}

func max0(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}

// waitFor reports whether wg finished within d.
func waitFor(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// healthDetails is the operator's view behind HEALTH_SECRET: how many workspaces the deployment
// holds and what has been spent. The gate serves it (gate.go) so that /health keeps answering
// before this Bot exists.
func (b *Bot) healthDetails(w io.Writer, r *http.Request) {
	d, c, _ := b.store.DocStats(r.Context(), 0)
	spend, _ := b.store.MonthSpend(r.Context(), 0, "", "")
	teams, _ := b.store.ActiveTeams(r.Context())
	fmt.Fprintf(w, "workspaces=%d docs=%d chunks=%d month_spend_usd=%.4f\n", len(teams), d, c, spend)
}

func (b *Bot) ingestLoop(ctx context.Context) {
	if !docStoreReady(b.docs) {
		slog.Info("no docs dir; RAG idle until it exists", "dir", b.cfg.DocsDir)
	}
	for {
		if docStoreReady(b.docs) {
			// One pass per organisation: each has its own folder, its own chunks and its own
			// cache, so an ingest for one must not touch another's.
			for _, o := range b.orgIDs(ctx) {
				if _, err := b.ix.IngestAndRecord(ctx, o, b.docs.For(o)); err != nil {
					slog.Warn("ingest", "org", o, "err", err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(6 * time.Hour):
		}
	}
}

// teamOf says which installed workspace an envelope belongs to.
//
// Slack's own SDKs read authorizations[0].team_id in preference to the top-level team_id,
// because for a Slack Connect channel the top-level value can be the *other* workspace — and
// for an org-wide install it is the enterprise. slack-go does not parse authorizations at all,
// so it is read here straight off the raw payload, with the parsed team_id as the fallback.
//
// An empty answer means the turn is dropped. There is deliberately no default team: replying
// to workspace B on workspace A's token is the failure this whole design exists to prevent.
func teamOf(raw json.RawMessage, ev slackevents.EventsAPIEvent) string {
	if len(raw) > 0 {
		var p struct {
			Authorizations []struct {
				TeamID string `json:"team_id"`
			} `json:"authorizations"`
			IsExtSharedChannel bool `json:"is_ext_shared_channel"`
		}
		if json.Unmarshal(raw, &p) == nil {
			if len(p.Authorizations) > 0 && p.Authorizations[0].TeamID != "" {
				return p.Authorizations[0].TeamID
			}
			if p.IsExtSharedChannel {
				// The one case where the top-level id is most likely to be the wrong workspace.
				slog.Warn("shared-channel event carried no workspace authorization; refusing to guess", "team", ev.TeamID)
				return ""
			}
		}
	}
	return ev.TeamID
}

func (b *Bot) route(ctx context.Context, teamID string, ev slackevents.EventsAPIEvent) {
	// Uninstall and revoke arrive like any other event and are the only ones handled without
	// a client — by then the token is already dead.
	switch ev.InnerEvent.Data.(type) {
	case *slackevents.AppUninstalledEvent, *slackevents.TokensRevokedEvent:
		if teamID != "" {
			slog.Info("workspace disconnected by Slack", "team", teamID)
			b.slacks.Evict(teamID)
			b.store.RevokeTeam(ctx, teamID, "the app was uninstalled or its tokens were revoked in Slack")
			if org, err := b.store.OrgOfTeam(ctx, teamID); err == nil {
				b.changed(ctx, org)
			}
		}
		return
	}
	sl, err := b.slacks.For(ctx, teamID)
	if err != nil {
		slog.Warn("event dropped", "team", teamID, "err", err)
		return
	}
	switch e := ev.InnerEvent.Data.(type) {
	case *slackevents.AppMentionEvent:
		b.incoming(ctx, sl, "channel", e.Channel, e.User, e.BotID, e.ThreadTimeStamp, e.TimeStamp, e.Text, eventActionToken(e.ActionToken, e.AssistantThread), true)
	case *slackevents.MessageEvent:
		b.message(ctx, sl, e)
	case *slackevents.AssistantThreadStartedEvent:
		t := e.AssistantThread
		sl.SuggestPrompts(ctx, t.ChannelID, t.ThreadTimeStamp, [][2]string{
			{"Summarize a channel", "Summarize what happened in #general this week."},
			{"Find a decision", "What did we decide about pricing recently, and who owns it?"},
			{"Search our docs", "How do we handle customer refunds?"},
		})
	}
}

func (b *Bot) message(ctx context.Context, sl *Chat, e *slackevents.MessageEvent) {
	// A mail forwarded to this channel's Slack address, answered before the subtype switch below
	// can drop it. What identifies one is who posted it and what is attached — Slackbot, with the
	// body in a file — rather than the label it carries, which has varied (file_share now,
	// bot_message and a bare "email" in older workspaces). email_intake.go decides the rest; a
	// channel that has not asked for the lane gets silence.
	if emailedMessage(e) {
		b.emailIntake(ctx, sl, e)
		return
	}
	switch e.SubType {
	case "message_changed":
		// Slack sends message_changed for things that are not edits at all: a thread parent gets
		// one every time a reply lands under it, and an unfurl rewrites the message in place.
		// Recording those wrote «alex edited a message: "X" → "X"» into the transcript once per
		// reply — 24 of one test thread's 35 notes said nothing had changed.
		//
		// The bot's own messages are skipped for the same reason, self test or not: an answer is
		// streamed by editing the message already posted, so every turn left a note holding the
		// finished reply, footer and step lines and signed Configure link included. The model read
		// that back and copied the shape into later answers, inventing footers with invented token
		// counts and once claiming to have saved a memory it never saved. The answer itself is
		// already stored as an assistant turn.
		if e.Message == nil || e.PreviousMessage == nil ||
			e.Message.Text == e.PreviousMessage.Text || e.Message.User == sl.BotUserID {
			return
		}
		threadTS := e.Message.ThreadTimestamp
		if threadTS == "" {
			threadTS = e.Message.Timestamp // root message edited
		}
		if s, _ := b.store.GetSession(ctx, sl.TeamID, e.Channel, threadTS); s != nil && s.Status == "active" {
			note := fmt.Sprintf("%s edited a message: \"%s\" → \"%s\"", sl.UserName(ctx, e.Message.User),
				truncate(oneLine(e.PreviousMessage.Text), 200), truncate(oneLine(e.Message.Text), 200))
			b.store.AddTurn(ctx, sl.TeamID, e.Channel, threadTS, "note", e.Message.User, note, e.Message.Timestamp, 0, 0)
		}
		return
	case "message_deleted":
		if e.PreviousMessage != nil && e.PreviousMessage.ThreadTimestamp == "" || (e.PreviousMessage != nil && e.PreviousMessage.ThreadTimestamp == e.DeletedTimeStamp) {
			// root deleted → close the session
			b.store.ArchiveSession(ctx, sl.TeamID, e.Channel, e.DeletedTimeStamp)
		} else if e.PreviousMessage != nil && e.PreviousMessage.ThreadTimestamp != "" {
			note := fmt.Sprintf("%s deleted a message: \"%s\"", sl.UserName(ctx, e.PreviousMessage.User), truncate(oneLine(e.PreviousMessage.Text), 200))
			b.store.AddTurn(ctx, sl.TeamID, e.Channel, e.PreviousMessage.ThreadTimestamp, "note", e.PreviousMessage.User, note, "", 0, 0)
		}
		return
	case "", "file_share":
	default:
		return // joins, bot_message and friends are ignored
	}
	if b.cfg.SelfTest && e.User == sl.BotUserID && strings.Contains(e.Text, "[selftest]") {
		b.incoming(ctx, sl, kindOf(e.ChannelType), e.Channel, "selftest", "", e.ThreadTimeStamp, e.TimeStamp, e.Text, eventActionToken(e.ActionToken, e.AssistantThread), true)
		return
	}
	if e.ChannelType == "im" {
		b.incoming(ctx, sl, "dm", e.Channel, e.User, e.BotID, e.ThreadTimeStamp, e.TimeStamp, e.Text, eventActionToken(e.ActionToken, e.AssistantThread), true)
		return
	}
	// Channel/group message without a mention: continue threads we're already active in, or,
	// when the channel has "read every message" on, let the classifier decide. Whether it may
	// run is settled in incoming(), after the rate limit and budget: it is a model call, and
	// one may not run for a workspace that is over.
	if !strings.Contains(e.Text, "<@"+sl.BotUserID+">") { // sl is the event's own workspace: bot user ids differ per install
		if e.ThreadTimeStamp != "" || b.readAll(ctx, sl.OrgID, sl.TeamID, e.Channel) {
			b.incoming(ctx, sl, kindOf(e.ChannelType), e.Channel, e.User, e.BotID, e.ThreadTimeStamp, e.TimeStamp, e.Text, eventActionToken(e.ActionToken, e.AssistantThread), false)
		}
	}
}

// readAll resolves "read every message": whether the bot reads each message in a channel and
// decides for itself whether to answer, react, or say nothing. A channel inherits from its own
// workspace and the workspace from the account; unset anywhere it is off, which is the point —
// a channel where every message costs a classifier call has to be asked for.
func (b *Bot) readAll(ctx context.Context, orgID int64, teamID, channel string) bool {
	// Narrowest link first, and each is read only if the one before it inherited: this runs on
	// every unmentioned message in every channel, whether or not the mode is in use anywhere.
	links := []func() *Scope{
		func() *Scope { sc, _ := b.store.ChannelScope(ctx, orgID, teamID, channel); return sc },
		func() *Scope { sc, _ := b.store.TeamScope(ctx, orgID, teamID); return sc },
		func() *Scope { sc, _ := b.store.AccountScope(ctx, orgID); return sc },
	}
	for _, link := range links {
		sc := link()
		if sc == nil {
			continue
		}
		switch sc.ReadAll {
		case "on":
			return true
		case "off":
			return false
		}
	}
	return false
}

// watch is the "read every message" decision for one message. It reports whether the turn
// should carry on into a full answer; a reaction is placed here and reported as false, because
// an emoji is the whole response. The channel's instructions are what the verdict is judged
// against — "thumbsup anything that mentions a deploy" only means something if the model can
// read it — so they come from the same resolver that builds the system prompt.
func (b *Bot) watch(ctx context.Context, sl *Chat, channel, ts, text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	var instructions string
	if acc, err := b.resolver.Resolve(ctx, sl.OrgID, sl.TeamID, channel, b.settings.Get(ctx, sl.OrgID).ConfigVersion); err == nil && acc != nil {
		instructions = acc.Instructions
	}
	action, emoji := b.agent.watchVerdict(ctx, sl.OrgID, sl.TeamID, instructions, text)
	switch action {
	case "reply":
		return true
	case "react":
		// No transcript note: there is no session here to write one to, and a reaction is
		// already visible in Slack. The same reasoning keeps message_changed out, above.
		if err := sl.AddReaction(ctx, channel, ts, emoji); err != nil {
			slog.Warn("reaction not added", "channel", channel, "emoji", emoji, "err", err)
		}
	}
	return false
}

func kindOf(channelType string) string {
	if channelType == "im" {
		return "dm"
	}
	return "channel"
}

// eventActionToken is the search token Slack mints for whatever it has just delivered, and the
// only thing that lets a bot token search the workspace. Newer payloads carry it at the top
// level and the ones that shipped with the AI-app APIs carry it under assistant_thread, so
// both are read: a workspace on either shape can still search.
func eventActionToken(top string, at *slackevents.AssistantThreadActionToken) string {
	if top != "" {
		return top
	}
	if at != nil {
		return at.ActionToken
	}
	return ""
}

// incoming is the single entry point for a human turn. explicit=false means "a reply in a
// thread with no mention", which only counts if we already have an active session there.
func (b *Bot) incoming(ctx context.Context, sl *Chat, kind, channel, user, botID, threadTS, ts, text, actionToken string, explicit bool) {
	if (botID != "" || user == sl.BotUserID) && user != "selftest" {
		return // never talk to ourselves or other bots
	}
	if user == "" {
		return
	}
	if !b.store.SeenEvent(ctx, sl.TeamID+":"+channel+":"+ts, deliveryOwner(ctx)) {
		return // redelivery, or mention+DM double fire
	}
	if threadTS == "" {
		threadTS = ts
	}
	// From here a turn definitely starts, so it takes over its delivery: the inbox row stays open
	// until this goroutine ends. b.turns is what lets shutdown wait for it.
	finish := adoptDelivery(ctx)
	b.turns.Add(1)
	go func() {
		defer b.turns.Done()
		defer finish(nil)
		// The outer ceiling. The turn sets its own, shorter deadline from the rounds its channel
		// allows (turnWall); the extra minute here is what is left to post an answer, or to say
		// what went wrong, once that one has run out.
		ctx, cancel := context.WithTimeout(context.Background(), b.cfg.turnCeiling()+time.Minute)
		defer cancel()
		// Who is allowed to use the bot at all (ALLOWED_EMAIL_DOMAINS). Checked before any
		// session, tool or model call. Only an explicit mention or DM gets told why —
		// answering a message that merely looked addressed to us would be noise.
		if ok, why := b.mayUseBot(ctx, sl, user); !ok {
			if explicit {
				sl.PostText(ctx, channel, threadTS, why)
			}
			return
		}
		// Rate limit, budget and the in-flight cap come before anything that costs: the
		// classifier below is a model call, and attachments are fetched and parsed further on.
		// A workspace over its budget used to keep spending on both until the check was reached.
		if ok, why := b.agent.allowed(ctx, &Call{TeamID: sl.TeamID, OrgID: sl.OrgID, SL: sl, Channel: channel, UserID: user}); !ok {
			if explicit {
				sl.PostText(ctx, channel, threadTS, "I can't take this one right now: "+why+".")
			}
			return
		}
		sess, err := b.store.GetSession(ctx, sl.TeamID, channel, threadTS)
		if err != nil {
			slog.Error("session", "err", err)
			return
		}
		if !explicit && (sess == nil || sess.Status != "active") {
			// A message in a channel we're not already talking in. Off by default; when
			// "read every message" is on, a cheap classifier judges it against the channel's
			// instructions and answers with a reply, a reaction, or nothing at all.
			if !b.readAll(ctx, sl.OrgID, sl.TeamID, channel) {
				return
			}
			if !b.watch(ctx, sl, channel, ts, text) {
				return // reacted, or nothing worth saying
			}
		}
		if sess == nil || sess.Status != "active" {
			sess, err = b.store.EnsureSession(ctx, sl.TeamID, channel, threadTS, kind, "")
			if err != nil {
				slog.Error("session", "err", err)
				return
			}
		} else {
			b.store.EnsureSession(ctx, sl.TeamID, channel, threadTS, kind, "") // bump last_active
		}
		// Whether a person is behind this turn. An email turn is the case the distinction was
		// written for: a requester that looks real with nobody at all behind it.
		human := !isEmailRequester(user)
		// Read the mentions off the raw text: who was tagged is deterministic input, never
		// something the model decides. It is only ever a hint — approvers.go intersects it with
		// config, so a message that nominates its own approver still gains nothing by it.
		tagged := mentionedUsers(text)
		if !human {
			// Who was tagged is meant to be a hint from the person asking, and there is no such
			// person. routeTo prefers a tier the requester named, so a mention in a mail body
			// would otherwise let whoever sent it choose which approver hears about it.
			tagged = nil
		}
		clean := strings.TrimSpace(strings.ReplaceAll(sl.NameMentions(ctx, text), "[selftest]", ""))
		if user == "selftest" {
			user = sl.BotUserID
		}
		// Everything from here to the turn itself is somebody answering the bot: a command, the
		// word "confirm", "stop", the reason for a Deny. None of it is available to a turn nobody
		// typed — the text is a stranger's mail, and a body that says "confirm" would otherwise
		// press whatever was waiting in the thread.
		if human {
			if handled, reply := b.agent.command(ctx, &Call{TeamID: sl.TeamID, OrgID: sl.OrgID, SL: sl, Channel: channel, ThreadTS: threadTS, MessageTS: ts, UserID: user, Kind: kind, Session: sess, HumanTurn: true}, clean); handled {
				if reply != "" { // `!stop` says nothing here: the run it stops reports that itself
					sl.PostText(ctx, channel, threadTS, reply)
				}
				return
			}
			if b.stopRequested(sl.OrgID, sl.TeamID, channel, threadTS, user, clean) {
				return // a bare "stop" while I'm working means stop, not a new question
			}
			if handled := b.confirmFlow(ctx, sl, kind, channel, threadTS, user, clean, sess); handled {
				return
			}
			// A sentence typed straight after pressing Deny is the reason for it, and goes to the
			// person who was refused rather than to the model.
			if kind == "dm" && b.denyReason(ctx, sl, user, clean) {
				return
			}
		}
		// Muting is outside that: `!mute` silences the channel, and a mail is not an exception.
		if sess.Muted {
			return
		}
		b.store.AddTurn(ctx, sl.TeamID, channel, threadTS, "user", user, clean, ts, 0, 0)
		// A person used the product, and the size they are billed under is counted in people.
		// Here rather than earlier in this function because everything above is a gate — access,
		// rate limit, budget, the in-flight cap, mute, commands, the confirm flow — and somebody
		// who was refused did not use anything. `human` is false for a forwarded mail, which is
		// keyed per channel and not per sender and is therefore nobody; the bot's own id is what
		// a self-test turn was rewritten to a few lines above.
		if countsAsUser(user, sl.BotUserID) {
			b.markActive(ctx, sl, user)
		}
		// HumanTurn: somebody typed this, just now, and is waiting for the answer. It is the one
		// claim that unlocks their own private notes; see (*Call).personalKey.
		c := &Call{TeamID: sl.TeamID, OrgID: sl.OrgID, SL: sl, Channel: channel, ThreadTS: threadTS, UserID: user, Text: clean, Kind: kind, Session: sess,
			Tagged: tagged, HumanTurn: human, ActionToken: actionToken, Streamer: sl.NewStreamer(channel, threadTS, user)}
		if !human {
			// What this run is, said where the model reads it: there is no question in the
			// conversation and nobody to ask one of.
			c.Brief = emailTurnBrief
		}
		if err := b.agent.Run(ctx, c); err != nil {
			slog.Error("turn failed", "channel", channel, "thread", threadTS, "err", err)
			if !c.Streamer.Started() {
				sl.PostText(ctx, channel, threadTS, "Sorry, that failed: "+truncate(err.Error(), 300))
			}
		}
		if kind == "dm" && sess.Title == "" {
			b.titleThread(ctx, sl, channel, threadTS, clean)
		}
	}()
}

// titleThread names an assistant (DM) thread from its first message.
func (b *Bot) titleThread(ctx context.Context, sl *Chat, channel, threadTS, first string) {
	title := truncate(oneLine(first), 60)
	sl.SetTitle(ctx, channel, threadTS, title)
	b.store.SetSessionField(ctx, sl.TeamID, channel, threadTS, "title", title)
}

// confirmFlow handles `confirm` / `cancel` replies for pending write requests in a thread. The
// card posted with the reply carries the same three answers as buttons.
func (b *Bot) confirmFlow(ctx context.Context, sl *Chat, kind, channel, threadTS, user, text string, sess *Session) bool {
	switch strings.ToLower(strings.TrimSpace(strings.Trim(text, "`.! "))) {
	case "cancel", "no":
		// The typed form of the Cancel button, so it asks the button's question: a held write is
		// its requester's to drop or an approver's, and nobody else's. It used to drop everything
		// held in the thread for whoever typed the word, which let anybody reading a thread cancel
		// the writes somebody else was about to confirm.
		dropped, left := b.dropHeldWrites(ctx, sl, channel, threadTS, user)
		for _, id := range dropped {
			// Recorded as the button's press is (confirm.go).
			b.auditSlack(ctx, sl, user, "write.cancelled", AuditEvent{TargetKind: "pending_write", TargetID: strconv.FormatInt(id, 10),
				Details: auditDetails(map[string]any{"channel": channel, "thread_ts": threadTS, "typed": true})})
		}
		if n := b.withdrawAccessRequests(ctx, sl, channel, threadTS, user); n > 0 {
			sl.PostText(ctx, channel, threadTS, "Withdrawn — I've told the approver.")
			return true
		}
		// Refused outright rather than passed to the model, which would read "cancel" and say it
		// had been.
		if len(dropped) == 0 && left != "" {
			sl.PostText(ctx, channel, threadTS, fmt.Sprintf("<@%s>, only <@%s> or an approver can cancel this one.", user, left))
			return true
		}
		return false // let the model answer normally too
	case "confirm", "yes, confirm", "approve", "approved":
	default:
		return false
	}
	id, requester, err := b.store.LatestPendingWrite(ctx, sl.OrgID, sl.TeamID, channel, threadTS)
	if err != nil || id == 0 {
		sl.PostText(ctx, channel, threadTS, "There is nothing waiting for confirmation in this thread (requests expire after 5 minutes).")
		return true
	}
	if !b.mayConfirm(ctx, sl, user, requester) {
		sl.PostText(ctx, channel, threadTS, fmt.Sprintf("<@%s>, only <@%s> or an approver can confirm this one.", user, requester))
		return true
	}
	raw, err := b.store.TakePendingWriteByID(ctx, sl.OrgID, sl.TeamID, channel, id)
	if err != nil || raw == "" {
		sl.PostText(ctx, channel, threadTS, "That request expired or was already answered.")
		return true
	}
	// The typed form of the Confirm button, recorded the same way (confirm.go).
	b.auditSlack(ctx, sl, user, "write.confirmed", AuditEvent{TargetKind: "pending_write", TargetID: strconv.FormatInt(id, 10),
		Details: auditDetails(map[string]any{"requester": requester, "channel": channel, "thread_ts": threadTS, "typed": true})})
	b.runPending(ctx, sl, kind, channel, threadTS, requester, user, raw, sess)
	return true
}

// runStep executes one recorded call — an MCP tool or a proxied HTTP request — and returns the
// note describing what happened. The payload is exactly what was stored when a human was asked,
// so what runs cannot drift from what they read.
//
// The errors it returns are the sentences posted to the thread, which is why they read like
// sentences: there is one caller per path and each of them posts this verbatim.
// The int is the HTTP status, or 0 for an MCP call. runPending ignores it — the model reports
// whatever happened — but a grant replay must stop on a 4xx/5xx: a step that did not do what was
// approved means the steps after it would be applied on top of a state nobody agreed to.
// How much of a confirmed call's response the model gets to report from. It used to be 1,500
// characters, which the prompt then cut to 400 again on the way in: asked for a hundred log
// entries, the model was handed the opening brace of the JSON and nothing else.
const confirmedResultChars = 12000

func (b *Bot) runStep(ctx context.Context, sl *Chat, orgID int64, acc *Access, audit ProxyAudit, by, raw string) (string, int, error) {
	var pend struct {
		MCP  int64          `json:"mcp"`
		Tool string         `json:"tool"`
		Args map[string]any `json:"args"`
	}
	json.Unmarshal([]byte(raw), &pend)
	who := by
	if sl != nil { // replay is deliberately usable without Slack, so it can be tested end to end
		who = sl.UserName(ctx, by)
	}
	// A held fix job. runPending has understood this shape since fix jobs shipped, because a press
	// on the in-thread card lands there; an approved access request replays through here instead,
	// and until it knew the shape too, approving one parsed it as an HTTP request with no method
	// and no URL. Approving the job is the dispatch — there is nothing else to run.
	var held struct {
		Job *JobSpec `json:"job"`
	}
	if json.Unmarshal([]byte(raw), &held) == nil && held.Job != nil {
		if b.jobs == nil {
			return "", 0, errors.New("The worker is not enabled here, so I could not start the job.")
		}
		approval := "approval_request"
		if audit.AccessRequestID != 0 {
			approval = fmt.Sprintf("approval_request:%d", audit.AccessRequestID)
		}
		job, err := b.jobs.Dispatch(ctx, DispatchRequest{OrgID: orgID, TeamID: audit.TeamID,
			Spec: *held.Job, ApprovedBy: by, Approval: approval})
		if err != nil {
			return "", 0, errors.New("I couldn't start the fix job: " + truncate(err.Error(), 300))
		}
		return fmt.Sprintf("%s approved the fix job on %s; it is running now as job #%d, and a checklist message in this thread tracks it.",
			who, held.Job.Repo, job.ID), 0, nil
	}
	if pend.MCP != 0 { // an MCP tool call
		conn, _ := b.store.Connection(ctx, orgID, pend.MCP)
		if conn == nil {
			return "", 0, errors.New("That connection no longer exists.")
		}
		out, err := b.agent.mcp.call(ctx, orgID, conn, pend.Tool, pend.Args, audit)
		if err != nil {
			return "", 0, errors.New("The tool failed: " + truncate(err.Error(), 300))
		}
		// redact here because this path does not go through runTool, which is where tool output
		// is normally scrubbed — and the note below lands in a turn that Store.Notes feeds back
		// into the system prompt. An approved write is exactly the call whose response may carry
		// a freshly minted credential.
		return fmt.Sprintf("%s confirmed running %s on %s.%s Result: %s",
			who, pend.Tool, conn.Name, viewLine(out), truncate(redact(oneLine(out)), confirmedResultChars)), 0, nil
	}
	var req ProxyRequest
	json.Unmarshal([]byte(raw), &req)
	if acc == nil {
		// Resolve failed. Match would dereference this, and a half-restored SQLite file on a
		// fresh instance is a plausible way to get here, so say so rather than panicking.
		return "", 0, errors.New("I couldn't read this channel's connections just now, so I've not run it. Try again in a moment.")
	}
	resp, err := b.proxy.Do(ctx, orgID, acc, req, audit, true)
	if err != nil {
		return "", 0, errors.New("The request failed: " + truncate(err.Error(), 300))
	}
	// The link to the thing that was just made goes ahead of the body, not after it: the body is
	// truncated here and truncated again by the callers, and a ticket's URL that falls off the end
	// leaves the reply quoting an id nobody can click. It is the one part of a write's result that
	// has to survive to the thread.
	note := fmt.Sprintf("%s confirmed the write %s %s; the service returned HTTP %d.%s Response: %s",
		who, req.Method, req.URL, resp.Status, viewLine(resp.Body), truncate(redact(oneLine(resp.Body)), confirmedResultChars))
	return note + b.uploadEvidence(ctx, sl, orgID, acc, req, resp, audit), resp.Status, nil
}

// uploadEvidence puts the files a write was carrying onto whatever it just created.
//
// It runs on the same approval as the write, deliberately: somebody who approved filing a ticket
// about a customer's screenshot approved the screenshot going on it, and a second card for the
// second half of one action is how attachments come to be skipped. Nothing here can fail the
// write — the ticket exists either way, and what is reported is what actually landed.
func (b *Bot) uploadEvidence(ctx context.Context, sl *Chat, orgID int64, acc *Access, req ProxyRequest, resp *ProxyResponse, audit ProxyAudit) string {
	if len(req.AttachFiles) == 0 || resp == nil || resp.Status >= 300 {
		return ""
	}
	base, ok := clickupTaskCreate(req)
	if !ok {
		return "" // only ClickUp knows how to be given a file so far
	}
	taskID := createdTaskID(resp.Body)
	if taskID == "" {
		return ""
	}
	var files []slack.File
	for _, id := range req.AttachFiles {
		if f, ok := fileByID(ctx, sl, id); ok {
			files = append(files, f)
		}
	}
	done := attachToClickUp(ctx, b.proxy, b.slacks, orgID, acc, audit.TeamID, base, taskID, files, audit)
	switch {
	case len(done) == len(req.AttachFiles):
		return fmt.Sprintf(" The files from this thread are on the task: %s.", strings.Join(done, ", "))
	case len(done) > 0:
		return fmt.Sprintf(" %d of %d files from this thread are on the task: %s — say that the rest could not be uploaded.",
			len(done), len(req.AttachFiles), strings.Join(done, ", "))
	}
	return " The files from this thread could not be uploaded to the task; say so rather than implying they are there."
}

// runPending executes a write a human has just approved — by button or by replying confirm —
// and hands the result back to the model to report in the thread.
// runPending executes a write a human has just released. requester is whose it is — whose
// credential it spends and whose name the audit carries; approver is who pressed, which matters
// only for the line in the thread. They are the same person unless an approver confirmed.
func (b *Bot) runPending(ctx context.Context, sl *Chat, kind, channel, threadTS, requester, approver, raw string, sess *Session) {
	user := requester
	if user == "" {
		user = approver
	}
	// A held fix job: the confirmation is the dispatch; its checklist message is the reply.
	var held struct {
		Job *JobSpec `json:"job"`
	}
	if json.Unmarshal([]byte(raw), &held) == nil && held.Job != nil {
		if _, err := b.jobs.Dispatch(ctx, DispatchRequest{OrgID: sl.OrgID, TeamID: sl.TeamID, Spec: *held.Job, ApprovedBy: approver, Approval: "confirm"}); err != nil {
			sl.PostText(ctx, channel, threadTS, "I couldn't start the fix job: "+truncate(err.Error(), 300))
		}
		return
	}
	acc, err := b.resolver.Resolve(ctx, sl.OrgID, sl.TeamID, channel, b.settings.Get(ctx, sl.OrgID).ConfigVersion)
	if err != nil {
		slog.Error("resolve for a confirmed write", "channel", channel, "err", err)
	}
	note, _, err := b.runStep(ctx, sl, sl.OrgID, acc, ProxyAudit{TeamID: sl.TeamID, Channel: channel, ThreadTS: threadTS, Requester: user}, user, raw)
	if err != nil {
		sl.PostText(ctx, channel, threadTS, err.Error())
		return
	}
	// Feed the result back through the agent so it can summarise it for the thread.
	b.store.AddTurn(ctx, sl.TeamID, channel, threadTS, "note", user, note, "", 0, 0)
	c := &Call{TeamID: sl.TeamID, OrgID: sl.OrgID, SL: sl, Channel: channel, ThreadTS: threadTS, UserID: user, Text: "The write was confirmed and executed; report the result briefly.", Kind: kind, Session: sess,
		Streamer: sl.NewStreamer(channel, threadTS, user), NoTools: true,
		Result: "The call above was confirmed and has already run. This is what the service returned. " +
			"Report it to the thread now — answer the original question from this data. Do not say you are about to run " +
			"anything, and do not write out a tool call: there are no tools on this turn. If the result carries a " +
			"\"view:\" link, that is the page for what you just made — put it in your reply as a link people can " +
			"click, alongside whatever else you report, rather than only an id.\n\n" + note}
	if err := b.agent.Run(ctx, c); err != nil {
		sl.PostText(ctx, channel, threadTS, "Done. "+truncate(note, 1500))
	}
}
