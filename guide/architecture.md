# Architecture

How the process is put together, what each file in it is for, and what the
database holds.

```
Slack (signed webhooks)      Microsoft Teams (Bot Framework)      Console, API and links (HTTP, :8080)
  │ /slack/events              │ /msteams/messages                  │ /admin/ /api/ /v1/ /configure/ /setup/
  │ /slack/interactions        │                                    │ /connect/ /invite/ /mcp /oauth/ /operator /health
  ▼                            ▼                                    ▼
┌───────────────────────────────────── attesttag (one Go process) ─────────────────────────────────────┐
│  bot.go: event router, sessions, confirm flow      chat.go: one Chat per workspace, either platform  │
│  agent.go: system prompt, thread → messages, tool loop, streaming, footer                            │
│  tools: slack · web · docs (RAG) · memory · routines · run_js · http_request + packs · MCP client    │
│  proxy.go: resolve access → match → inject credential → confirm writes → scrub → audit               │
│  store.go + db.go: SQLite or Postgres (sessions, turns, memories, routines, chunks, usage, …)        │
│  jobs_*.go: start a fix-job worker container on whichever platform this is (internal/worker)         │
└────────────┬───────────────────────────────────────────────────────────────────┬─────────────────────┘
             │ chat / stream / embeddings                                        │ credential injected
             ▼                                                                   ▼
   OpenAI-compatible endpoint (OpenRouter by default)                 GitHub, Google, ClickUp, MCP servers, …
```

## Slack, Teams and model calls

- **Slack I/O** uses signed HTTP POSTs to `/slack/events` and `/slack/interactions`.
  Requests are authenticated with `SLACK_SIGNING_SECRET` and a five-minute timestamp window.
  Accepted deliveries are sealed into a bounded inbox in the database before a 200 response —
  4,096 pending for the deployment, 64 per organisation and 32 per workspace, 1 MiB each — and
  workers dispatch them asynchronously. Retries are deduplicated by event/interaction ID. The
  inbox survives a restart before dispatch; existing handlers retain their own message/write
  deduplication. This does not guarantee exactly-once external side effects or recover an
  interrupted model turn.
- **Microsoft Teams** delivers to `/msteams/messages`, each activity carrying a Bot Framework
  token checked against Microsoft's published keys. It goes down the same turn path as a Slack
  message, through a transport of its own; Teams gives an app no way to read a thread back, so
  the bot keeps the conversation log itself ([msteams.md](msteams.md)).
- **Model calls** use the OpenAI Go SDK against any compatible base URL for chat, streaming,
  and embeddings, in each endpoint's own dialect (`llm.go`). The default, advanced and embedding
  models are settings, and an organisation may bring its own endpoint and key
  ([plans.md](plans.md#its-own-model-key)); every call resolves which one through
  `model_endpoints.go`.

## Documents, storage and fix jobs

- **Documents** are chunked (about 500 tokens, keeping their nearest heading), embedded, and
  stored as float32 blobs. Retrieval is brute-force cosine in memory, fine for thousands of
  chunks. Indexing runs on start, every six hours, on `!ingest`, and on console reindex. The
  files themselves live in a local folder, a GCS bucket or any S3-compatible bucket, one folder
  per organisation.
- **Storage** is SQLite by default: one file, replicated continuously by Litestream to a GCS or
  S3-compatible bucket when one is configured, behind a write lease so that only one instance
  ever writes. Set `DATABASE_URL` and it is Postgres instead, which is what running more than one
  instance needs. Every statement is written once, with `?` placeholders, in a form both accept
  (`db.go`); the schema is versioned migrations, one set per dialect, under
  `internal/app/migrations/`.
- **Fix jobs** run in a container of their own that the bot starts on whichever platform it is
  on — a Cloud Run Job, a Fargate task, a Container Apps job, a Kubernetes Job, or a container on
  the host's Docker daemon — and the worker inside it is `internal/worker`
  ([fix-jobs.md](fix-jobs.md)).
- **The marketing site** is not in this process and not in this repository. The one setting
  between them is `SITE_URL` ([configuration](configuration.md#http-server-and-public-origin)),
  which a self-host leaves empty.

## Repository layout

`internal/app` is one package. The files group like this.

### The process and a turn

| path | what |
|---|---|
| `cmd/attesttag/main.go` | entrypoint; also the `worker`, `bucket-init` and `pg-import` subcommands |
| `internal/app/bot.go` | wiring, the workspace event router, sessions, the confirm flow, the HTTP listener |
| `internal/app/gate.go`, `singleton.go` | the handler that answers `/health` before the database is open; the loops that run once across the deployment, each behind a leader lease |
| `internal/app/config.go`, `settings.go`, `limits.go` | environment loading and defaults; the console settings cache over them; the per-organisation and per-address ceilings |
| `internal/app/slack_http.go`, `store_slack_deliveries.go`, `inbox_handoff.go` | signed Events/Interactivity endpoints; the sealed delivery inbox, its deduplication, limits and leases; who finishes a delivery |
| `internal/app/chat.go`, `chat_registry.go`, `slack_transport.go` | one `Chat` per connected workspace over its platform's transport: names, thread and history, the neutral card, streaming, assistant status — and the registry that builds each from its row |
| `internal/app/msteams_*.go`, `store_msteams.go` | Microsoft Teams: the Bot Framework endpoint and its token checks, the transport and the conversation log it reads threads from, streaming, Adaptive Cards, files, the link code and the generated app package ([`msteams.md`](msteams.md)) |
| `internal/app/agent.go` | system prompt, thread to messages, the tool-calling loop, streaming, the footer |
| `internal/app/routing.go`, `repeat_guard.go`, `runs.go` | which model a turn runs on, its rounds, clock and spend, rate limits and alerts; landing a turn that keeps repeating a call; `stop` and `!stop` |
| `internal/app/summary.go`, `longanswer.go`, `files.go` | thread windowing, long answers as files, attachments |
| `internal/app/llm.go` | OpenAI-compatible chat and embeddings in each endpoint's dialect; secret redaction |
| `internal/app/model_keys.go`, `model_endpoints.go`, `model_keys_api.go`, `store_model_keys.go` | an organisation's own model key: who may bring one, the resolver every model call goes through, the console routes, the sealed row |
| `internal/app/models.go` | `GET /api/models`: the model lists behind the console's dropdowns |
| `internal/app/commands.go`, `help.go`, `whoami.go` | bang commands; the bot's own manual (`about_me`); the `!whoami` report |
| `internal/app/investigations.go`, `investigations_store.go` | the digging lane: the `start_investigation` tool, the worker pool that answers in the thread, and the queue that survives a deploy |
| `internal/app/playground.go` | a real turn against a channel's real configuration, answered over HTTP to the console, with its writes held |
| `internal/app/assistant.go`, `assistant_api.go`, `assistant_guide.go`, `store_assistant.go` | the console assistant: reads the console, searches this guide (embedded by `guide/embed.go`), and stages changes a person confirms |

### Tools

| path | what |
|---|---|
| `internal/app/tools_slack.go`, `tools_web.go`, `tools_memory.go`, `rag.go`, `routines.go`, `routine_steps.go` | native tools: Slack reads and reactions, web search and fetch, memory, document search, routines and the steps a routine can run before the model is asked anything |
| `internal/app/routine_writes.go` | creating and changing a routine, the one set of checks the console's editor and the `/v1` routes share |
| `internal/app/web_providers.go` | paid web search and page readers (Tavily, Firecrawl, Cloudflare and others) in place of the built-in scrape |
| `internal/app/sandbox.go`, `internal/sandbox/` | `run_js`: model-written JavaScript in QuickJS compiled to WebAssembly under wazero — no syscalls, no filesystem, a host-provided fetch |
| `internal/app/personal_memory.go`, `personal_memory_store.go`, `personal_memory_api.go` | one person's own notes: who a turn may read for, the tools, the store, the console routes |
| `internal/app/artifacts.go` | `create_artifact`: files the bot writes into a thread |
| `internal/app/tools_http.go`, `presets.go` | `http_request` and the per-preset tool packs; the connection presets |
| `internal/app/tools_github.go`, `repos.go` | code search across the repositories a channel can reach; connected repositories and the default repository per scope |
| `internal/app/mcp.go`, `oauth_mcp.go` | MCP client and OAuth for remote servers |
| `internal/app/email_intake.go` | a mail forwarded to a channel's Slack address, turned into a turn on a lane of its own |

### Connections and approval

| path | what |
|---|---|
| `internal/app/proxy.go`, `outbound.go` | the access resolver and the credential-injecting proxy; the public-address check made again at dial time |
| `internal/app/confirm.go`, `allow_rules.go` | held writes — the approval card, buttons and "Something else" dialog — and the plain-sentence allow rules with the model check that applies them |
| `internal/app/access_requests.go`, `approvers.go`, `store_access.go` | access requests: the tool, the approver's DM card, the press and the replay; approval tiers and who holds them |
| `internal/app/oauth_user.go`, `store_user_conns.go`, `store_oauth_pending.go` | connections people sign into for themselves — each person's own Google account |
| `internal/app/github_app.go`, `github_token.go`, `store_github_installs.go` | the GitHub App: installing it, and minting an installation token per request |
| `internal/app/aws_sigv4.go`, `aws_creds.go` | AWS Signature Version 4 for connections, and the task-role credential the ECS dispatcher signs with |
| `internal/app/drive_sync.go`, `store_drive.go` | a Google Drive folder mirrored into Documents |
| `internal/app/skills.go`, `setup_links.go`, `configure.go`, `scope_fields.go` | skills; setup links; the member Configure page; the one check every channel setting goes through |
| `internal/app/crypto.go` | AES-GCM sealing under `MASTER_KEY` |

### The console, accounts and access

| path | what |
|---|---|
| `internal/app/admin_api.go` | the `/api/*` routes and the embedded console at `/admin/` |
| `internal/app/auth_password.go`, `auth_slack.go`, `auth_microsoft.go`, `auth_sso.go`, `sso_api.go`, `store_sso.go` | signing in: email and password, Sign in with Slack, Sign in with Microsoft, an organisation's own OpenID Connect provider |
| `internal/app/account.go`, `totp.go`, `password_strength.go`, `store_security.go`, `store_identity.go` | one's own account, the second factor and recovery codes, what makes a password unacceptable, sign-in identities |
| `internal/app/console_roles.go`, `console_users.go`, `store_console.go`, `invites.go`, `invite_join.go` | console permissions, the built-in roles and the subset rule; members, invitations and share links; joining another organisation |
| `internal/app/installs.go`, `store_teams.go`, `team_delete.go`, `store_team_delete.go`, `onboarding.go`, `access.go` | connecting a Slack workspace; removing one; the first-run walk; who the bot answers, in Slack and in Teams |
| `internal/app/api_keys.go`, `store_api_keys.go`, `api_v1.go` | developer API keys, the middleware that resolves one to its owner, and the `/v1` surface |
| `internal/app/mcp_server.go`, `mcp_oauth.go`, `store_mcp_oauth.go` | the MCP server at `/mcp`, where each tool runs its `/v1` route in-process, and its own OAuth: registration, the consent page's API, tokens and revocation |
| `internal/app/audit.go`, `retention.go` | the audit log and the write floor every authenticated route passes; the per-organisation retention sweeps |
| `internal/app/headers.go`, `public_origin.go` | the response headers every route sends; the public origin links and sign-in redirects are built from |
| `internal/app/org_delete.go`, `store_org_delete.go` | deleting an account |
| `internal/app/email.go`, `support_request.go` | transactional mail; the console's Help button |
| `internal/app/store_charts.go` | the series behind the overview's charts |

### Plans and billing

| path | what |
|---|---|
| `internal/app/plans.go`, `operator.go`, `enterprise.go`, `store_enterprise.go` | the free, pro and enterprise plans, the ceiling each sets, the operator's API and pages, and the enterprise deal ([`plans.md`](plans.md)) |
| `internal/app/billing.go`, `billing_policy.go`, `billing_stripe.go`, `store_billing.go`, `operator_billing.go`, `billing_request.go` | selling a plan size and prepaid credit: checkout, the Stripe webhook, the ledger and the floor it enforces, the operator's money actions, and Talk to us. Off unless both Stripe keys are set ([`billing.md`](billing.md)) |
| `internal/app/active_users.go`, `store_active_users.go` | who counts towards a plan size, recorded once a day |

### Fix jobs

| path | what |
|---|---|
| `internal/app/jobs.go`, `jobs_store.go`, `jobs_api.go` | the `start_fix_job` tool and runner, the jobs tables, the worker-facing and console routes |
| `internal/app/jobs_dispatch.go`, `jobs_cloudrun.go`, `jobs_ecs.go`, `jobs_aca.go`, `jobs_k8s.go`, `jobs_docker.go` | how a job's worker is started: a local subprocess, a Cloud Run Job execution, a Fargate task, a Container Apps job, a Kubernetes Job, or a container on the host's Docker daemon |
| `internal/app/jobs_slack.go`, `jobs_reconcile.go`, `jobs_openrouter.go`, `jobs_cache.go` | the checklist and report in the thread, the stale/timeout/cancel reconciler, per-job OpenRouter keys, the dependency cache's signed URLs |
| `internal/app/recipe_input.go`, `shellwords.go` | a recipe an admin typed, checked before it is stored; a command line split into argv without a shell |
| `internal/app/jobs_proto.go` | the bot ⇄ worker protocol types, shared with the worker |
| `internal/worker/` | `attesttag worker`: claim, clone, recipe, tests, engine (Qwen Code or fake), commit, push, draft PR, result — with the repository's code run as a sandbox user |

### Storage

| path | what |
|---|---|
| `internal/app/store.go`, `store_admin.go` | the queries |
| `internal/app/db.go`, `db_dialect.go`, `migrations.go`, `migrations/` | the seam between the code and SQLite or Postgres, the one file allowed SQL that differs by dialect, and the versioned migrations with their checksums |
| `internal/app/lease.go`, `lease_gcs.go`, `lease_s3.go` | the single-writer lease for SQLite, on GCS or any S3-compatible bucket |
| `internal/app/replication.go`, `replica_target.go`, `bucket_init.go` | Litestream as a child process and where its replica goes; `attesttag bucket-init` |
| `internal/app/pgimport.go` | `attesttag pg-import`: copying a SQLite database into an empty Postgres one at a cutover |
| `internal/app/docs.go`, `docs_s3.go` | the document store: a local folder, a GCS bucket, or any S3-compatible bucket |

### Outside `internal/app`

| path | what |
|---|---|
| `internal/app/*_test.go` | unit tests, the guard tests and the live eval harness |
| `ui/` | admin console (Next.js static export), embedded via `ui/embed.go` |
| `guide/` | the documentation, which lives here rather than in `docs/` — and is embedded into the binary for the console assistant |
| `docs/` | the document store's default folder — the bot's corpus, not documentation. Each organisation's documents live under `docs/org-<id>/` and are chunked, embedded and answered from; the four samples at the top belong to no organisation until copied into one |
| `Dockerfile`, `Dockerfile.worker`, `Dockerfile.worker.jvm` | the bot image; the fix-job worker image and its heavier JVM/.NET variant |
| `docker-compose.yml` | the single-machine deployment, with profiles for a tunnel, Caddy, Postgres and MinIO |
| `deploy/` | one folder per platform — `local/` (compose overlays and launchd), `gcp/` (Cloud Run: the bot and the worker job), `aws/` (ECS Fargate), `azure/` (Container Apps), `helm/` (Kubernetes) — over shared `docs/`, `env/` and `slack/`, `plan.sh`, and `test/`, which runs the AWS and Azure scripts against fake CLIs |
| `evals/` | eval cases and how to run them |
| `Makefile` | `make ui`, `make build`, `make test`, and the Cloud Run deploy targets |
| `LICENSE` | MIT |

Ignored by git: `.env*` except [`.env.example`](../.env.example), `*.db*`, `bot.log`, the root
`attesttag` binary, `ui/out` (all but the checked-in `ui/out/index.html`, which `//go:embed`
needs to find), `ui/node_modules` and `ui/.next`, organisations' uploaded folders under
`docs/org-*/`, key and credential files (`*.pem`, `*.key`, `*.p12`, `*.pfx`,
`*credentials*.json`, `*service-account*.json`), and the recorder's outputs.
[`.dockerignore`](../.dockerignore) is a separate, stricter list — an allowlist naming only what
the build stages read, so that a build context is a few megabytes rather than the whole working
tree.

## Data model

One schema, written once with `?` placeholders and created by the migrations in
`internal/app/migrations/sqlite` and `internal/app/migrations/postgres` — the same numbered
files in both, checksummed in `schema_migrations`, never edited once applied. Tables:

- Accounts: `orgs`, `users`, `memberships`, `user_identities` (a Slack, Microsoft or single
  sign-on identity joined to an account), `user_totp`, `user_recovery_codes`,
  `login_challenges`, `email_tokens` (verification, reset and invitation links, stored hashed),
  `sso_providers`, `admin_sessions`.
- Workspaces: `teams` (a connected Slack workspace or Teams organisation, with its sealed bot
  token), `slack_deliveries` (the inbox), `oauth_states`, `oauth_pendings`, `connect_states`.
- Conversation: `sessions` (one per thread, with status, muted, model override, running
  summary), `turns`, `seen_events` (dedupe), `file_texts` (extracted attachment cache).
- Knowledge: `memories`, `personal_memories` (one person's own notes, keyed by workspace and
  Slack user rather than by scope, so no organisation-wide read can return one), `documents`,
  `document_folders`, `doc_chunks` (text plus float32 embedding blob, and the embedding it was
  made with), `drive_syncs`, `drive_sync_files`.

### Automation, access and approval tables

- Automation: `routines`, `routine_runs` (one row per run, the quiet ones included; trimmed to
  the last 100 per routine), `pending_writes`, `alerts_sent`, `investigations`, `jobs` (fix
  jobs: spec, status, token hash, cost, result), `job_events` (unique per job and sequence
  number), `job_files` (diff and log).
- Approval: `access_requests` (the ask, the recorded calls, and what was decided), plus
  `access_request_cards` (where each approver's copy of the card landed, so a decision can clear
  all of them). Deliberately separate from `pending_writes`, whose five-minute window and
  by-thread discards belong to the in-thread Confirm card.
- Output: `artifacts` (files the bot wrote, with their content, Slack file id and permalink).
- Access: `scopes`, `bundles`, `scope_bundles`, `scope_connections` (one-off grants),
  `connections` (sealed secret), `domains`, `skills`, `setup_links`, `user_connections` (each
  person's own sealed tokens), `github_installs`, `web_keys`.
- Approval tiers: `approval_roles` (name, rank), `approval_role_bundles` and
  `approval_role_connections` (what each may grant from — whole bundles and one-off connections,
  recorded the way a channel's access is), and `approval_members` (who holds each tier, by Slack
  id or email).
- Console access: `console_users` (who may sign in and the role they hold), `console_roles`
  (roles this install defined; the built-ins stay in code) and `console_invites` (hashed
  single-use tokens).

### Teams, billing, operations and audit tables

- Microsoft Teams: `teams.platform` says which platform a connected workspace is on, and a Teams
  row keeps the Bot Framework `service_url` its replies go to rather than a token.
  `msteams_messages` is the conversation log a Teams thread is read back from, since Microsoft
  gives an app no way to read one; `msteams_users` maps an Entra object id to the Teams roster id
  and name; `link_codes` holds the hashed, single-use codes that join a tenant to an organisation.
- Models: `org_model_keys` (an organisation's own sealed key and endpoint); `usage.key_owner`
  and `jobs.key_owner` say whose key a call spent.
- Billing: `billing_accounts` (the Stripe customer and subscription, the plan size, the credit
  floor and the month's included allowance), `credit_ledger` (append-only money in and out),
  `billing_events` (Stripe events already acted on), `active_users` (who used the bot, one row
  per person per day), `enterprise_terms` (an enterprise deal's figures).
- Operations: `tool_calls`, `usage` (tokens, cached and reasoning tokens, and cost per completion),
  `proxy_audit`, `settings`, `api_keys` (developer keys: the hash, whose authority the key carries,
  and when it lapses), `mcp_clients`, `mcp_codes` and `mcp_grants` (the MCP server's OAuth: clients
  that registered themselves, single-use authorization codes, and the connections people approved,
  with their tokens as hashes), `assistant_turns`, `leader_leases` (which instance runs each
  singleton loop), `throttle_events` (limits counted in the database rather than per instance).
- Audit: `audit_log` (who did what, from where, and what came of it: one append-only row per
  sign-in, refused sign-in, console or API write, Slack approval, export and retention sweep;
  the actor copied in at the time so a removed account stays legible). Swept only by its own
  `audit_retention_days`, never by `data_retention_days`.

### Vectors and pgvector

Vectors are float32 blobs searched brute-force in memory, on either database. That is fine for
thousands of chunks and is not the limit on a large corpus; pgvector is. Adopting it means
changing `doc_chunks` in both migration sets and `Indexer.Search` in `rag.go`, and nothing else —
it has not been built because it has not been needed, and untested code should not ship.
