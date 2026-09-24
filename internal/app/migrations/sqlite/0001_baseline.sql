-- attest_tag baseline schema, SQLite.
--
-- Generated from what OpenStore produced at the end of the migrate() era: the ddl constant,
-- slackDeliveryDDL, and every ADD COLUMN the old additive migrator applied. Derived rather than
-- merged by hand, because merging eighty ADD COLUMNs into fifty-nine create-table statements is
-- a job with no way to check the answer.
--
-- Its Postgres twin is ../postgres/0001_baseline.sql. The two are compared column by column by
-- TestSchemasMatch; the composite primary keys here are the ones that used to exist only on a
-- freshly created database, which is why an old database could not be upgraded in place.

CREATE TABLE access_request_cards (
  org_id     integer not null,
  request_id integer not null,
  approver   text not null,
  channel    text not null,               -- the DM channel the card landed in
  ts         text not null,               -- the card's ts, for chat.update
  primary key (request_id, approver)
);
CREATE TABLE access_requests (
  id         integer primary key autoincrement,
  org_id     integer not null,
  team_id    text not null default '',
  channel    text not null, thread_ts text not null, -- origin: where the answer goes back
  requester  text not null,
  approver   text not null default '',      -- who the card was addressed to
  approvers  text not null default '[]',    -- json array: who could answer when it went out
  what       text default '',               -- one line: the access asked for
  why        text default '',               -- the reason given
  ask        text default '',               -- the requester's own words, verbatim
  calls      text not null default '[]',    -- json array of recorded payloads, run in order
  status     text not null default 'pending', -- pending|approved|executed|failed|denied|expired|cancelled
  origin_ts  text default '',               -- the "waiting on…" card in the origin thread
  decided_by text default '', decided_at text,
  reason     text default '',               -- deny reason, or why a run failed
  result     text default '',               -- what the run produced, for the console
  created_at text default (datetime('now')),
  expires_at text not null
, self_approved integer default 0, role_id integer default 0);
CREATE TABLE admin_sessions (
  token text primary key,
  id integer not null default 0,          -- users.id
  org_id integer not null default 0,      -- the organisation this session is acting in
  user_id text, name text, email text,
  created_at text default (datetime('now')), expires_at text not null
, mfa_verified integer not null default 0, via text not null default '');
CREATE TABLE alerts_sent (
  org_id integer not null,
  key text not null, created_at text default (datetime('now')),
  primary key (org_id, key)
);
CREATE TABLE api_keys (
  id           integer primary key autoincrement,
  org_id       integer not null,
  user_id      integer not null,              -- users.id: the account this key acts as
  name         text not null,                 -- what it is for, in the maker's words
  key_hash     text not null,
  key_prefix   text not null default '',      -- display only: enough to recognise, never to use
  last_used_at text,
  expires_at   text,                          -- null = never; checked at every request, not swept
  revoked_at   text,
  created_by   text default '',
  created_at   text default (datetime('now'))
);
CREATE TABLE approval_members (
  id         integer primary key autoincrement,
  org_id     integer not null,
  role_id    integer not null,
  ref        text not null,                -- Slack user id or email, as entered
  created_at text default (datetime('now'))
, team_id text not null default '', slack_user_id text not null default '');
CREATE TABLE approval_role_bundles (
  org_id  integer not null,             -- denormalised so the join can be filtered
  role_id integer not null, bundle_id integer not null,
  primary key (role_id, bundle_id)
);
CREATE TABLE approval_role_connections (   -- one-off: a connection without the rest of its bundle
  org_id  integer not null,
  role_id integer not null, connection_id integer not null,
  primary key (role_id, connection_id)
);
CREATE TABLE approval_roles (
  id         integer primary key autoincrement,
  org_id     integer not null,
  name       text not null,                -- "Approver", "Super admin"
  rank       integer not null default 1,   -- higher grants more; a tier covers everything below it
  bundle_id  integer not null default 0,   -- the bundle its members may grant from
  created_at text default (datetime('now'))
);
CREATE TABLE artifacts (
  id         integer primary key autoincrement,
  org_id     integer not null,
  team_id    text not null default '',
  channel    text, thread_ts text, created_by text,
  title      text not null,
  kind       text not null default 'md',   -- md | csv | json | html | txt | yaml
  bytes      integer default 0,
  content    text not null default '',     -- kept so the console can show what was made
  file_id    text default '',              -- Slack file id
  permalink  text default '',              -- Slack permalink, the link the reply carries
  created_at text default (datetime('now'))
);
CREATE TABLE bundles (
  id           integer primary key autoincrement,
  org_id integer not null,
  name         text not null,
  instructions text default '',
  tool_packs   text default '[]',          -- json array of preset ids whose packs are enabled
  created_by   text,
  created_at   text default (datetime('now')),
  updated_at   text default (datetime('now'))
);
CREATE TABLE connect_states (
  state         text primary key,
  org_id        integer not null,
  conn_id       integer not null,
  team_id       text not null default '',
  slack_user_id text not null,
  verifier      text not null,
  redirect_uri  text not null default '',
  created_at    text default (datetime('now')),
  expires_at    text not null
);
CREATE TABLE connections (
  id            integer primary key autoincrement,
  org_id  integer not null,
  bundle_id     integer not null,
  name          text not null,
  preset        text default 'custom',
  cred_type     text not null,             -- bearer | basic | header | query | oauth2_cc | gcp_sa | aws_sigv4 | mcp
  secret_enc    blob,                      -- sealed json {token|user,pass|value|client_id,client_secret,token_url|sa_json}
  allowed_hosts text default '[]',         -- json array; leftmost wildcard allowed
  path_prefixes text default '[]',
  methods       text default '[]',         -- empty = all
  headers       text default '[]',         -- json [{name,prefix}] extra headers (values live in secret_enc.headers)
  writes        text default 'confirm',    -- confirm (writes wait for a human) | auto (writes run) | all (reads wait too)
  notes         text default '',           -- usage notes shown to the model
  status        text default 'active',     -- active | pending | disabled
  last_used     text,
  created_by    text,
  created_at    text default (datetime('now')),
  updated_at    text default (datetime('now'))
, repo text default '', github_installation_id integer not null default 0, allow_grants integer default 0, test_cmd text default '', recipe text default '');
CREATE TABLE console_invites (
  id            integer primary key autoincrement,
  token_hash    text not null unique,        -- sha-256; the raw token exists only in the link
  team_id       text not null default '',    -- the workspace slack_user_id belongs to
  slack_user_id text default '',             -- who it was sent to, when the bot DM'd it
  email         text default '',             -- a label, not authority
  role          text not null default 'viewer',
  created_by    text default '',
  created_at    text default (datetime('now')),
  expires_at    text not null,
  accepted_at   text, accepted_by text default '',
  revoked_at    text
);
CREATE TABLE console_roles (   -- roles an org defined; built-ins stay in code
  id          integer primary key autoincrement,
  org_id      integer not null,
  key         text not null,
  label       text not null,
  permissions text not null default '[]',
  created_by  text default '',
  created_at  text default (datetime('now'))
);
CREATE TABLE console_users (
  id            integer primary key autoincrement,
  org_id  integer not null,
  team_id       text not null default '',   -- which Slack workspace this id belongs to
  slack_user_id text not null,              -- who they are; Slack is the identity provider
  email         text default '',
  name          text default '',
  role          text not null default 'viewer',
  created_by    text default '',            -- '' for a workspace admin who signed themselves in
  created_at    text default (datetime('now')),
  last_seen     text
);
CREATE TABLE doc_chunks (
  id          integer primary key autoincrement,
  org_id      integer not null,
  source      text, doc_id text, url text, title text, heading text,
  chunk       text not null,
  hash        text not null,
  acl         text default '',
  embedding   blob,
  updated_at  text default (datetime('now'))
);
CREATE TABLE document_folders (
  id         integer primary key autoincrement,
  org_id     integer not null,
  path       text not null,               -- relative, no trailing slash
  created_at text default (datetime('now'))
);
CREATE TABLE documents (
  id          integer primary key autoincrement,
  org_id integer not null,
  path        text not null,               -- relative path inside DOCS_DIR / bucket docs/
  name        text not null,
  size        integer default 0,
  uploaded_by text,
  scope       text default '',             -- '' = workspace-wide, else channel id
  status      text default 'pending',      -- pending | indexed | error
  chunks      integer default 0,
  last_error  text default '',
  updated_at  text default (datetime('now'))
);
CREATE TABLE domains (
  id        integer primary key autoincrement,
  org_id integer not null,
  bundle_id integer not null,
  host      text not null,
  ports     text default '443'
);
CREATE TABLE drive_sync_files (
  id       integer primary key autoincrement,
  org_id   integer not null,
  sync_id  integer not null,
  file_id  text not null,                    -- Drive file id
  path     text not null,                    -- the document it became
  version  text default '',
  seen_at  text
);
CREATE TABLE drive_syncs (
  id            integer primary key autoincrement,
  org_id        integer not null,
  connection_id integer not null,
  folder_id     text not null,               -- Drive folder id, extracted from whatever was pasted
  folder_name   text default '',             -- what Drive calls it, for the console to show
  dest          text not null default '',    -- documents folder it lands in ('' = root)
  scope         text not null default '',    -- channel scope every file it brings in gets
  recurse       integer not null default 1,  -- walk subfolders, keeping their shape
  enabled       integer not null default 1,
  last_run      text,
  last_status   text default '',             -- ok | error | running
  last_error    text default '',
  last_added    integer default 0, last_updated integer default 0, last_removed integer default 0,
  created_by    text,
  created_at    text default (datetime('now'))
);
CREATE TABLE email_tokens (
  token_hash text primary key,                   -- sha-256 of the token in the link
  kind       text not null,                      -- verify | reset | invite
  user_id    integer not null default 0,
  org_id     integer not null default 0,
  email      text default '',                    -- invite: who it was addressed to, when a mailbox
  slack_user_id text default '',                 -- invite: who it was addressed to, when a Slack account
  role       text default '',                    -- invite: the role it grants
  label      text default '',                    -- share link: what its maker called it
  domain     text default '',                    -- share link: the email domain it will accept, if any
  max_uses   integer not null default 1,         -- 1 for a personal invitation, -1 for an uncapped share link
  uses       integer not null default 0,
  created_by integer not null default 0,
  created_at text default (datetime('now')),
  expires_at text not null,
  used_at    text
);
CREATE TABLE file_texts (
  team_id text not null default '', file_id text not null, name text, text text,
  created_at text default (datetime('now')),
  primary key (team_id, file_id)
);
CREATE TABLE github_installs (   -- one GitHub App installation, bound to one organisation
  installation_id integer primary key,          -- GitHub's id for it; the row exists to say whose it is
  org_id integer not null,
  account_login text default '', account_id integer default 0, account_type text default '',
  repo_selection text default '',               -- "all" or "selected", as GitHub reports it
  permissions text default '',                  -- what was granted, as json, for "needs reinstall" advice
  app_slug text default '', installed_by text default '',
  suspended_at text, status text default 'active', last_error text,
  installed_at text default (datetime('now')), revoked_at text );
CREATE TABLE investigations (         -- questions handed to the digging lane (investigations.go)
  id integer primary key autoincrement,
  status text not null default 'queued',           -- queued|running|done|failed|stopped
  org_id integer not null,
  team_id text not null default '', channel text not null, thread_ts text not null,
  requester text default '',                       -- who asked, so the answer can name them
  question text not null, brief text default '',   -- what to find out, and what the asking turn already knew
  rounds integer default 0, minutes integer default 0,   -- the budget it was queued with, held so a resumed run keeps it
  attempts integer default 0, lease_until integer not null default 0,
  tokens_in integer default 0, tokens_out integer default 0, cost_usd real default 0,
  error text default '',
  created_at text default (datetime('now')), started_at text default '', finished_at text default ''
);
CREATE TABLE job_events (
  id integer primary key autoincrement, org_id integer not null, job_id integer not null, seq integer not null,
  kind text not null,                 -- phase|log|tests|usage|heartbeat|warn
  phase text default '', status text default '',   -- phase: clone|test_before|engine|test_after|commit|push|pr ; status: started|ok|failed|skipped
  message text default '', data text default '{}', at text default '',
  tokens_in integer default 0, tokens_out integer default 0, cost_usd real default 0,
  created_at text default (datetime('now'))
);
CREATE TABLE job_files (
  org_id integer not null, job_id integer not null, kind text not null,    -- diff|log
  bytes integer default 0, content text default '', file_id text default '', permalink text default '',
  created_at text default (datetime('now'))
);
CREATE TABLE jobs (                   -- fix jobs handed to the worker container (jobs.go)
  id integer primary key autoincrement,
  status text not null default 'queued',           -- queued|starting|running|stale|cancelling|succeeded|failed|cancelled|timeout
  org_id integer not null,
  team_id text not null default '', channel text not null, thread_ts text not null,
  requester text default '', approved_by text default '', approval text default '',   -- 'confirm' | 'rule:<text>'
  connection_id integer not null, repo text not null, base_branch text default '', branch text default '', title text default '',
  spec text not null,                              -- json JobSpec (redacted)
  engine text default '', model text default '', budget_usd real default 0, timeout_s integer default 0, draft_pr integer default 1,
  dispatcher text default '', execution_ref text default '',   -- 'cloudrun'|'local'; execution name or pid:N
  token_hash text default '', token_expires text, claimed_at text, claim_count integer default 0, worker_info text default '',
  status_ts text default '', phase text default '', last_seq integer default 0, last_event_at text,
  cancel_requested integer default 0, cancel_by text default '', cancel_reason text default '', cancel_requested_at text,
  result text default '', pr_url text default '', error text default '',
  cost_usd real default 0, tokens_in integer default 0, tokens_out integer default 0,
  llm_key_hash text default '', llm_key_enc blob,  -- per-job OpenRouter key: hash for the API, sealed for a retried claim
  created_at text default (datetime('now')), started_at text, finished_at text
);
CREATE TABLE login_challenges (
  token      text primary key,
  user_id    integer not null,
  tries      integer not null default 0,
  created_at text default (datetime('now')),
  expires_at text not null
, via text not null default 'password');
CREATE TABLE memberships (
  id         integer primary key autoincrement,
  user_id    integer not null,
  org_id     integer not null,
  role       text not null default 'viewer',
  created_by integer not null default 0,
  created_at text default (datetime('now'))
);
CREATE TABLE memories (
  id          integer primary key autoincrement,
  org_id      integer not null,
  team_id     text not null default '',
  scope       text not null,            -- 'channel:C0…' | 'team:T0…' | 'workspace:1'
  text        text not null,
  created_by  text,
  created_at  text default (datetime('now'))
);
CREATE TABLE oauth_states (        -- install-flow state; a table, not a map, so it survives a restart
  state        text primary key,
  org_id integer not null,
  created_by   text default '',
  created_at   text default (datetime('now')),
  expires_at   text not null
);
CREATE TABLE orgs (                -- the account: one per customer
  id          integer primary key autoincrement,
  -- The identifier everything outside the process uses: a random 128-bit value in hex. The
  -- integer above stays the join key for the 87 org_id columns below, but it never crosses the
  -- wire, because a serial says how many customers signed up before you and lets anyone who
  -- learns one id guess its neighbours. Backfilled and made unique in migrate().
  public_id   text not null default '',
  name        text not null,                     -- what they typed at signup
  slug        text not null,                     -- url-safe, unique, derived from the name
  status      text not null default 'active',    -- active | suspended
  plan        text not null default 'free',      -- free | pro (plans.go); only the operator changes it
  plan_budget_usd real not null default 0,       -- the monthly budget granted with a pro plan; 0 = the deployment's ceiling
  created_by  integer not null default 0,        -- users.id of whoever signed up
  created_at  text default (datetime('now'))
);
CREATE TABLE pending_writes (
  id integer primary key autoincrement,
  org_id integer not null,
  team_id text not null default '',
  channel text, thread_ts text, requester text, request text, -- json of the proxied request
  status text default 'pending', created_at text default (datetime('now'))
);
CREATE TABLE personal_memories (
  id          integer primary key autoincrement,
  org_id      integer not null,
  team_id     text not null,
  owner       text not null,            -- a Slack user id, never ''
  text        text not null,
  created_at  text default (datetime('now')),
  updated_at  text default (datetime('now'))
);
CREATE TABLE proxy_audit (
  id            integer primary key autoincrement,
  org_id integer not null,
  team_id text not null default '', channel text, thread_ts text, requester text,
  connection_id integer, method text, host text, path text,
  status integer, ms integer, blocked text default '',
  created_at    text default (datetime('now'))
, access_request_id integer default 0);
CREATE TABLE routine_runs (
  id          integer primary key autoincrement,
  org_id      integer not null,
  routine_id  integer not null,
  team_id     text not null default '',
  channel     text not null default '',
  thread_ts   text not null default '',
  status      text not null default '',   -- posted | quiet | failed | skipped
  reason      text default '',            -- the model's own note when it stayed quiet
  output      text default '',            -- the full answer, kept even when nothing was posted
  error       text default '',
  tokens_in   integer default 0,
  tokens_out  integer default 0,
  cost_usd    real default 0,
  started_at  text,
  finished_at text,
  ms          integer default 0
);
CREATE TABLE routines (
  id          integer primary key autoincrement,
  org_id      integer not null,
  team_id     text not null default '',
  channel     text not null,
  cron        text not null,
  tz          text default 'Asia/Kathmandu',
  prompt      text not null,
  created_by  text,
  enabled     integer default 1,
  next_run    text,
  last_run    text,
  last_error  text
, result_ts text, notify text not null default 'always', notify_when text default '', last_status text default '', auto_confirm integer not null default 0, steps text not null default '[]', finish text not null default 'answer', model text not null default '');
CREATE TABLE scope_bundles (
  org_id integer not null,             -- denormalised so the join can be filtered
  scope_id integer not null, bundle_id integer not null,
  primary key (scope_id, bundle_id)
);
CREATE TABLE scope_connections (   -- one-off grants: a connection without the rest of its bundle
  org_id integer not null,             -- denormalised so the join can be filtered
  scope_id integer not null, connection_id integer not null,
  primary key (scope_id, connection_id)
);
CREATE TABLE scopes (
  id            integer primary key autoincrement,
  org_id  integer not null,
  kind          text not null,             -- 'workspace' (the account) | 'team' | 'channel'
  team_id       text not null default '',  -- '' for kind='workspace'; T… otherwise
  slack_id      text not null default '',  -- '' for kind='workspace'; T… for a team; C…/G… for a channel
  name          text,
  is_private    integer not null default 0, -- a private channel; the C-prefix no longer tells you
  instructions  text default '',
  default_model text default '',
  member_edits  text default 'inherit',    -- inherit | allow | block
  read_all      text default 'inherit',    -- inherit | on | off  (judge every message: reply, react, or nothing)
  max_tool_rounds integer not null default 0, -- tool rounds a turn here may spend; 0 = inherit
  created_at    text default (datetime('now'))
, link_epoch integer not null default 0, monthly_budget_usd real default 0, allow_rules text default '[]', default_repo text default '', approvers text default '', left_at text not null default '');
CREATE TABLE seen_events (
  key text primary key, created_at text default (datetime('now')),
  owner text not null default ''
);
CREATE TABLE sessions (
  team_id     text not null default '',
  channel     text not null,
  thread_ts   text not null,
  kind        text not null,            -- 'channel' | 'dm' | 'routine'
  model       text,
  status      text default 'active',    -- 'active' | 'archived'
  muted       integer default 0,
  title       text,
  tool_calls  integer default 0,
  created_at  text default (datetime('now')),
  last_active text default (datetime('now')), summary text default '', summary_upto text default '',
  primary key (team_id, channel, thread_ts)
);
CREATE TABLE settings (
  org_id integer not null, key text not null,
  value text not null, updated_at text default (datetime('now')),
  primary key (org_id, key)
);
CREATE TABLE setup_links (
  token text primary key, org_id integer not null, bundle_id integer, preset text, name text,
  created_by text, status text default 'pending', connection_id integer,
  created_at text default (datetime('now')), expires_at text not null
);
CREATE TABLE skills (
  id        integer primary key autoincrement,
  org_id integer not null,
  bundle_id integer not null,
  name      text not null,
  content   text not null,              -- markdown the model reads when the bundle applies
  enabled   integer default 1,
  updated_at text default (datetime('now'))
);
CREATE TABLE slack_deliveries (
  delivery_key text primary key,
  team_id text not null,
  org_id integer not null default 0,
  kind text not null,
  payload_enc blob,
  accepted_at integer not null,
  lease_until integer not null default 0,
  done_at integer not null default 0,
  attempts integer not null default 0,
  dead_at integer not null default 0,
  last_error text not null default ''
);
CREATE TABLE teams (               -- one connected Slack workspace, installed over OAuth
  team_id       text primary key,                -- T…
  org_id  integer not null,
  name          text default '',
  domain        text default '',
  icon          text default '',                 -- image_132 from team.info, for the console rail
  enterprise_id text default '',
  bot_token_enc blob,                            -- sealed xoxb (crypto.go), never stored in the clear
  key_version   integer not null default 1,      -- which MASTER_KEY sealed it, so a re-key can find it
  bot_user_id   text default '',                 -- differs per install: the mention test depends on it
  bot_id        text default '',
  scopes        text default '',                 -- what Slack actually granted, for a reinstall hint
  email_scope   integer default 0,               -- users:read.email probe, run per install
  dm_scope      integer default 0,               -- im:write probe, run per install
  installed_by  text default '',
  status        text default 'active',           -- active | revoked
  last_error    text default '',
  installed_at  text default (datetime('now')),
  revoked_at    text
);
CREATE TABLE tool_calls (
  id          integer primary key autoincrement,
  org_id integer not null,
  team_id text not null default '', channel text, thread_ts text, name text, args text, result text, ok integer, ms integer,
  created_at  text default (datetime('now'))
);
CREATE TABLE turns (
  id          integer primary key autoincrement,
  team_id     text not null default '',
  channel     text not null,
  thread_ts   text not null,
  role        text not null,            -- 'user' | 'assistant' | 'tool' | 'note'
  user_id     text,
  content     text not null,
  slack_ts    text,
  tokens_in   integer default 0,
  tokens_out  integer default 0,
  created_at  text default (datetime('now'))
);
CREATE TABLE usage (
  id          integer primary key autoincrement,
  org_id integer not null,
  team_id text not null default '', channel text, thread_ts text, model text,
  tokens_in integer, tokens_out integer, cost_usd real,
  created_at  text default (datetime('now'))
, user_id text default '');
CREATE TABLE user_connections (
  id            integer primary key autoincrement,
  org_id        integer not null,
  conn_id       integer not null,
  team_id       text not null default '',
  slack_user_id text not null,
  account       text default '',           -- the Google address, a label and never a join key
  secret_enc    blob,                      -- sealed OAuthState: access token, refresh token, expiry
  status        text default 'active',
  created_at    text default (datetime('now')),
  updated_at    text default (datetime('now')),
  last_used     text
, instructions text not null default '');
CREATE TABLE user_identities (
  id         integer primary key autoincrement,
  user_id    integer not null,
  provider   text not null,                      -- 'password' | 'slack'
  subject    text not null,                      -- password: the user id; slack: 'T…:U…'
  created_at text default (datetime('now'))
);
CREATE TABLE user_recovery_codes (
  id        integer primary key autoincrement,
  user_id   integer not null,
  code_hash text not null,
  used_at   text
);
CREATE TABLE user_totp (
  user_id      integer primary key,           -- users.id
  secret_enc   blob not null,                 -- sealed (crypto.go), never readable from a dump
  confirmed_at text,                          -- null until they have typed a code back
  last_step    integer not null default 0,    -- the 30s window last spent, so a code is single use
  created_at   text default (datetime('now'))
);
CREATE TABLE users (
  id             integer primary key autoincrement,
  public_id      text not null default '',       -- the outside's name for this person; see orgs.public_id
  email          text not null,                  -- stored lowercased
  email_verified integer not null default 0,
  password_hash  text default '',                -- bcrypt; '' when they only sign in with Slack
  name           text default '',
  status         text not null default 'active', -- active | disabled
  created_at     text default (datetime('now')),
  last_seen      text
);
CREATE TABLE web_keys (
  org_id     integer not null,
  provider   text not null,
  secret_enc blob,
  updated_by text not null default '',
  updated_at text default (datetime('now')),
  primary key (org_id, provider)
);
CREATE INDEX access_requests_open on access_requests(status, expires_at);
CREATE INDEX access_requests_thread on access_requests(channel, thread_ts);
CREATE UNIQUE INDEX api_keys_hash on api_keys(key_hash);
CREATE INDEX api_keys_org on api_keys(org_id);
CREATE UNIQUE INDEX approval_members_uniq on approval_members(org_id, role_id, ref);
CREATE INDEX artifacts_thread on artifacts(team_id, channel, thread_ts);
CREATE INDEX artifacts_time on artifacts(created_at);
CREATE INDEX connections_bundle on connections(bundle_id);
CREATE UNIQUE INDEX console_roles_key on console_roles(org_id, key);
CREATE UNIQUE INDEX console_users_key on console_users(team_id, slack_user_id);
CREATE INDEX doc_chunks_doc on doc_chunks(org_id, doc_id);
CREATE UNIQUE INDEX document_folders_path on document_folders(org_id, path);
CREATE UNIQUE INDEX documents_path on documents(org_id, path);
CREATE UNIQUE INDEX drive_sync_files_file on drive_sync_files(org_id, sync_id, file_id);
CREATE INDEX drive_sync_files_sync on drive_sync_files(org_id, sync_id);
CREATE UNIQUE INDEX drive_syncs_folder on drive_syncs(org_id, connection_id, folder_id);
CREATE INDEX email_tokens_user on email_tokens(user_id, kind);
CREATE INDEX github_installs_org on github_installs(org_id);
CREATE INDEX investigations_pending on investigations(status, lease_until, id);
CREATE INDEX investigations_thread on investigations(org_id, team_id, channel, thread_ts, status);
CREATE UNIQUE INDEX job_events_seq on job_events(job_id, seq);
CREATE INDEX job_files_job on job_files(job_id);
CREATE INDEX jobs_status on jobs(status);
CREATE INDEX jobs_thread on jobs(team_id, channel, thread_ts);
CREATE UNIQUE INDEX memberships_key on memberships(user_id, org_id);
CREATE INDEX memories_scope on memories(scope);
CREATE UNIQUE INDEX orgs_public_id on orgs(public_id);
CREATE UNIQUE INDEX orgs_slug on orgs(slug);
CREATE INDEX personal_memories_owner on personal_memories(org_id, team_id, owner, id);
CREATE INDEX proxy_audit_time on proxy_audit(created_at);
CREATE INDEX routine_runs_by_routine on routine_runs(org_id, routine_id, id desc);
CREATE UNIQUE INDEX scopes_key on scopes(org_id, kind, team_id, slack_id);
CREATE INDEX skills_bundle on skills(bundle_id);
CREATE INDEX slack_deliveries_pending on slack_deliveries(done_at, lease_until, accepted_at);
CREATE INDEX slack_deliveries_team on slack_deliveries(team_id, done_at);
CREATE INDEX turns_thread on turns(team_id, channel, thread_ts, id);
CREATE UNIQUE INDEX user_connections_key on user_connections(conn_id, team_id, slack_user_id);
CREATE UNIQUE INDEX user_identities_key on user_identities(provider, subject);
CREATE INDEX user_recovery_codes_user on user_recovery_codes(user_id);
CREATE UNIQUE INDEX users_email on users(email);
CREATE UNIQUE INDEX users_public_id on users(public_id);
