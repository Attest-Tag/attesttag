package app

// The schema as it stood before versioned migrations, kept only so that
// TestAnExistingSchemaIsAdoptedNotRebuilt can build a database the way the old code did and
// prove the upgrade path works on it.
//
// It is in a test file because that is the only thing that reads it. Production builds the
// schema from migrations/, and a second copy of fifty-nine tables sitting in the binary is a
// second copy somebody eventually edits, wondering why nothing changes.

const ddl = `
-- Note on org_id below: it is "not null" with NO default anywhere it appears. A default is what
-- turned a forgotten column into a silent cross-tenant write — rows landed in organisation 1 and
-- were read back by whoever happened to be organisation 1. An insert that forgets the
-- organisation must fail loudly instead.
create table if not exists orgs (                -- the account: one per customer
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
create unique index if not exists orgs_slug on orgs(slug);

-- A person. One row per human, however many orgs they belong to, so an invitation to a second
-- org is a membership rather than a second account with a second password to forget.
--
-- The email is a delivery address and a label, NEVER a join key: an account is never adopted
-- because a sign-in asserted a matching address. Slack's OIDC email is set by the workspace's
-- own admin (and by its SAML IdP), so treating it as proof of control would let a workspace
-- admin mint an assertion for any address they like. Linking a second sign-in method happens
-- from inside an already-authenticated session, never at the door.
create table if not exists users (
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
create unique index if not exists users_email on users(email);

-- How a user proves who they are. One row per method, so somebody can hold both a password and
-- a Slack sign-in without either one being able to claim the other's account.
create table if not exists user_identities (
  id         integer primary key autoincrement,
  user_id    integer not null,
  provider   text not null,                      -- 'password' | 'slack'
  subject    text not null,                      -- password: the user id; slack: 'T…:U…'
  created_at text default (datetime('now'))
);
create unique index if not exists user_identities_key on user_identities(provider, subject);

-- Which orgs a person belongs to, and as what. This is the only source of authority: no role
-- is ever inferred from an email domain or from a Slack workspace-admin flag.
create table if not exists memberships (
  id         integer primary key autoincrement,
  user_id    integer not null,
  org_id     integer not null,
  role       text not null default 'viewer',
  created_by integer not null default 0,
  created_at text default (datetime('now'))
);
create unique index if not exists memberships_key on memberships(user_id, org_id);

-- One-time links sent by email: address verification, password reset, and invitations. Only the
-- hash is stored, so a database copy cannot be replayed into somebody's account.
create table if not exists email_tokens (
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
create index if not exists email_tokens_user on email_tokens(user_id, kind);
create table if not exists teams (               -- one connected Slack workspace, installed over OAuth
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
create table if not exists github_installs (   -- one GitHub App installation, bound to one organisation
  installation_id integer primary key,          -- GitHub's id for it; the row exists to say whose it is
  org_id integer not null,
  account_login text default '', account_id integer default 0, account_type text default '',
  repo_selection text default '',               -- "all" or "selected", as GitHub reports it
  permissions text default '',                  -- what was granted, as json, for "needs reinstall" advice
  app_slug text default '', installed_by text default '',
  suspended_at text, status text default 'active', last_error text,
  installed_at text default (datetime('now')), revoked_at text );
-- Deliberately no sealed token here, unlike teams.bot_token_enc. An installation token is
-- re-mintable from the app key at will and dies in an hour, so storing one would buy nothing
-- and leave a second thing to rotate. The row is a pointer, not a credential.
create index if not exists github_installs_org on github_installs(org_id);
create table if not exists oauth_states (        -- install-flow state; a table, not a map, so it survives a restart
  state        text primary key,
  org_id integer not null,
  created_by   text default '',
  created_at   text default (datetime('now')),
  expires_at   text not null
);
create table if not exists sessions (
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
  last_active text default (datetime('now')),
  primary key (team_id, channel, thread_ts)
);
create table if not exists turns (
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
create index if not exists turns_thread on turns(team_id, channel, thread_ts, id);
create table if not exists memories (
  id          integer primary key autoincrement,
  org_id      integer not null,
  team_id     text not null default '',
  scope       text not null,            -- 'channel:C0…' | 'team:T0…' | 'workspace:1'
  text        text not null,
  created_by  text,
  created_at  text default (datetime('now'))
);
create index if not exists memories_scope on memories(scope);
-- One person's own notes. Deliberately a table and not another scope string above: a scope is a
-- value the model types, and the one thing that must not turn on the model spelling a string
-- correctly is who may read a row. Here the owner is a column and every read takes it as an
-- argument, so no query written for the organisation's memory can return one of these by
-- accident -- including the queries nobody has written yet. The five that would each have needed
-- a new "scope not like" guard are AllMemories, UpdateMemory, DeleteMemory, OverviewStats and
-- org cap in AddMemory; none of them changed.
--
-- owner is a Slack user id and team_id is not optional beside it: Slack only promises user ids
-- are unique inside a workspace, which is why the scope strings above carry the workspace and why
-- user_connections keys on it too. org_id is not null with no default, for the reason at the top.
create table if not exists personal_memories (
  id          integer primary key autoincrement,
  org_id      integer not null,
  team_id     text not null,
  owner       text not null,            -- a Slack user id, never ''
  text        text not null,
  created_at  text default (datetime('now')),
  updated_at  text default (datetime('now'))
);
-- The only read there is: one person's notes, oldest first. The owner columns lead, so a query
-- that forgets one cannot use the index -- the cheap version of "cannot forget one".
create index if not exists personal_memories_owner on personal_memories(org_id, team_id, owner, id);
create table if not exists routines (
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
);
-- One row per routine run, including the runs that decided to stay out of the channel.
-- Before this, a run's only trace was next_run/last_run/last_error on the row above, each
-- overwritten by the next run, and the output existed nowhere but Slack.
--
-- thread_ts is the run key: the Slack ts of the message it posted, or a synthetic
-- "routine:<id>:<nanos>" when it stayed quiet and there is no message. Either way it is what
-- usage, tool_calls, turns and sessions were written under, so a run's spend and tool calls
-- are recoverable from it.
create table if not exists routine_runs (
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
create index if not exists routine_runs_by_routine on routine_runs(org_id, routine_id, id desc);
-- The retrieval corpus. org_id is load-bearing rather than bookkeeping: AllChunks feeds every
-- answer the bot gives, so a chunk without an owner is one org's uploaded document answering
-- another org's question.
create table if not exists doc_chunks (
  id          integer primary key autoincrement,
  org_id      integer not null,
  source      text, doc_id text, url text, title text, heading text,
  chunk       text not null,
  hash        text not null,
  acl         text default '',
  embedding   blob,
  updated_at  text default (datetime('now'))
);
create index if not exists doc_chunks_doc on doc_chunks(org_id, doc_id);
create table if not exists tool_calls (
  id          integer primary key autoincrement,
  org_id integer not null,
  team_id text not null default '', channel text, thread_ts text, name text, args text, result text, ok integer, ms integer,
  created_at  text default (datetime('now'))
);
create table if not exists usage (
  id          integer primary key autoincrement,
  org_id integer not null,
  team_id text not null default '', channel text, thread_ts text, model text,
  tokens_in integer, tokens_out integer, cost_usd real,
  created_at  text default (datetime('now'))
);
create table if not exists seen_events (
  key text primary key, created_at text default (datetime('now')),
  owner text not null default ''
);
create table if not exists settings (
  org_id integer not null, key text not null,
  value text not null, updated_at text default (datetime('now')),
  primary key (org_id, key)
);
create table if not exists scopes (
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
);
-- Slack channel ids are unique to a team, not to Slack: a Slack Connect channel carries the
-- same C-id in both workspaces. The key is therefore composite, never slack_id alone.
create unique index if not exists scopes_key on scopes(org_id, kind, team_id, slack_id);
create table if not exists bundles (
  id           integer primary key autoincrement,
  org_id integer not null,
  name         text not null,
  instructions text default '',
  tool_packs   text default '[]',          -- json array of preset ids whose packs are enabled
  created_by   text,
  created_at   text default (datetime('now')),
  updated_at   text default (datetime('now'))
);
create table if not exists scope_bundles (
  org_id integer not null,             -- denormalised so the join can be filtered
  scope_id integer not null, bundle_id integer not null,
  primary key (scope_id, bundle_id)
);
create table if not exists scope_connections (   -- one-off grants: a connection without the rest of its bundle
  org_id integer not null,             -- denormalised so the join can be filtered
  scope_id integer not null, connection_id integer not null,
  primary key (scope_id, connection_id)
);
create table if not exists connections (
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
);
create index if not exists connections_bundle on connections(bundle_id);
-- A connection whose cred_type is oauth_user holds only the org's OAuth client; the credential
-- that actually spends is one row per person here. The key is the Slack identity pair, because
-- that is what a tool call carries (Call.UserID plus the workspace) — a console account is not
-- resolved during a turn and most people who use the bot never have one.
create table if not exists user_connections (
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
);
create unique index if not exists user_connections_key on user_connections(conn_id, team_id, slack_user_id);
-- The half-finished sign-in, between the redirect out to the provider and the callback back. A
-- table and not a map, for the reason oauth_states above is one: a deploy in the middle of
-- somebody's consent screen must not lose them.
create table if not exists connect_states (
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
create table if not exists domains (
  id        integer primary key autoincrement,
  org_id integer not null,
  bundle_id integer not null,
  host      text not null,
  ports     text default '443'
);
create table if not exists proxy_audit (
  id            integer primary key autoincrement,
  org_id integer not null,
  team_id text not null default '', channel text, thread_ts text, requester text,
  connection_id integer, method text, host text, path text,
  status integer, ms integer, blocked text default '',
  created_at    text default (datetime('now'))
);
create index if not exists proxy_audit_time on proxy_audit(created_at);
create table if not exists documents (
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
create unique index if not exists documents_path on documents(org_id, path);
-- Folders people made in the console. A folder is otherwise just a prefix of a document
-- path, so this table exists to keep the empty ones: nothing on disk (and nothing at all in
-- a bucket) remembers a folder with no files in it yet.
create table if not exists document_folders (
  id         integer primary key autoincrement,
  org_id     integer not null,
  path       text not null,               -- relative, no trailing slash
  created_at text default (datetime('now'))
);
create unique index if not exists document_folders_path on document_folders(org_id, path);
-- A folder in Google Drive kept in step with a folder in Documents. The credential is a
-- connection (the gdrive preset), so the sync spends what an admin already configured and
-- nothing else; deleting that connection takes its syncs with it.
create table if not exists drive_syncs (
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
create unique index if not exists drive_syncs_folder on drive_syncs(org_id, connection_id, folder_id);
-- What each sync has actually put in Documents: one row per Drive file it owns. This is what
-- makes the mirror honest in both directions — a file that stops coming back from Drive is a
-- document to delete, and a document with no row here was uploaded by a person and is never
-- touched. The version is Drive's own (modifiedTime, plus md5 where Drive has one), so an
-- unchanged file is skipped without downloading it again.
create table if not exists drive_sync_files (
  id       integer primary key autoincrement,
  org_id   integer not null,
  sync_id  integer not null,
  file_id  text not null,                    -- Drive file id
  path     text not null,                    -- the document it became
  version  text default '',
  seen_at  text
);
create unique index if not exists drive_sync_files_file on drive_sync_files(org_id, sync_id, file_id);
create index if not exists drive_sync_files_sync on drive_sync_files(org_id, sync_id);
create table if not exists admin_sessions (
  token text primary key,
  id integer not null default 0,          -- users.id
  org_id integer not null default 0,      -- the organisation this session is acting in
  user_id text, name text, email text,
  created_at text default (datetime('now')), expires_at text not null
);
-- The second factor on a console sign-in. One row per account, holding the shared secret sealed
-- with the master key; the row exists while enrolment is half-finished and confirmed_at is what
-- makes it real, so an abandoned enrolment never becomes something you are asked for.
create table if not exists user_totp (
  user_id      integer primary key,           -- users.id
  secret_enc   blob not null,                 -- sealed (crypto.go), never readable from a dump
  confirmed_at text,                          -- null until they have typed a code back
  last_step    integer not null default 0,    -- the 30s window last spent, so a code is single use
  created_at   text default (datetime('now'))
);

-- What gets somebody back in when the phone is gone: ten single-use codes, kept as hashes.
create table if not exists user_recovery_codes (
  id        integer primary key autoincrement,
  user_id   integer not null,
  code_hash text not null,
  used_at   text
);
create index if not exists user_recovery_codes_user on user_recovery_codes(user_id);

-- A sign-in that has passed the password and still owes its second factor. It is not a session:
-- it authorises exactly one thing, expires in minutes, and is burned on use or on too many
-- wrong codes, so a stolen challenge is not a stolen account.
create table if not exists login_challenges (
  token      text primary key,
  user_id    integer not null,
  tries      integer not null default 0,
  created_at text default (datetime('now')),
  expires_at text not null
);
-- A developer API key: a credential somebody mints in the console and gives to a script.
--
-- Two ids, and both matter. org_id is whose data the key reaches; user_id is whose authority it
-- carries, so a key can never do more than the person who made it, and stops working the moment
-- they lose their membership. Only the sha-256 of the key is here — the raw one exists once, in
-- the response that created it.
create table if not exists api_keys (
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
create unique index if not exists api_keys_hash on api_keys(key_hash);
create index if not exists api_keys_org on api_keys(org_id);
create table if not exists setup_links (
  token text primary key, org_id integer not null, bundle_id integer, preset text, name text,
  created_by text, status text default 'pending', connection_id integer,
  created_at text default (datetime('now')), expires_at text not null
);
create table if not exists skills (
  id        integer primary key autoincrement,
  org_id integer not null,
  bundle_id integer not null,
  name      text not null,
  content   text not null,              -- markdown the model reads when the bundle applies
  enabled   integer default 1,
  updated_at text default (datetime('now'))
);
create index if not exists skills_bundle on skills(bundle_id);
create table if not exists file_texts (
  team_id text not null default '', file_id text not null, name text, text text,
  created_at text default (datetime('now')),
  primary key (team_id, file_id)
);
create table if not exists alerts_sent (
  org_id integer not null,
  key text not null, created_at text default (datetime('now')),
  primary key (org_id, key)
);
create table if not exists pending_writes (
  id integer primary key autoincrement,
  org_id integer not null,
  team_id text not null default '',
  channel text, thread_ts text, requester text, request text, -- json of the proxied request
  status text default 'pending', created_at text default (datetime('now'))
);
create table if not exists artifacts (
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
create index if not exists artifacts_time on artifacts(created_at);
create index if not exists artifacts_thread on artifacts(team_id, channel, thread_ts);
create table if not exists access_requests (
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
);
create index if not exists access_requests_open on access_requests(status, expires_at);
create index if not exists access_requests_thread on access_requests(channel, thread_ts);
create table if not exists console_users (
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
-- A Slack user id is unique to its workspace, so the console identity is the pair.
create unique index if not exists console_users_key on console_users(team_id, slack_user_id);
create table if not exists console_roles (   -- roles an org defined; built-ins stay in code
  id          integer primary key autoincrement,
  org_id      integer not null,
  key         text not null,
  label       text not null,
  permissions text not null default '[]',
  created_by  text default '',
  created_at  text default (datetime('now'))
);
create unique index if not exists console_roles_key on console_roles(org_id, key);
create table if not exists console_invites (
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
create table if not exists approval_roles (
  id         integer primary key autoincrement,
  org_id     integer not null,
  name       text not null,                -- "Approver", "Super admin"
  rank       integer not null default 1,   -- higher grants more; a tier covers everything below it
  bundle_id  integer not null default 0,   -- the bundle its members may grant from
  created_at text default (datetime('now'))
);
create table if not exists approval_members (
  id         integer primary key autoincrement,
  org_id     integer not null,
  role_id    integer not null,
  ref        text not null,                -- Slack user id or email, as entered
  created_at text default (datetime('now'))
);
create unique index if not exists approval_members_uniq on approval_members(org_id, role_id, ref);
-- What a tier may grant from, recorded the way a channel's access is: whole bundles, and
-- connections picked on their own. (approval_roles.bundle_id is the older single-bundle form;
-- foldRoleBundleIntoGrants moves it here and it is not read again.)
create table if not exists approval_role_bundles (
  org_id  integer not null,             -- denormalised so the join can be filtered
  role_id integer not null, bundle_id integer not null,
  primary key (role_id, bundle_id)
);
create table if not exists approval_role_connections (   -- one-off: a connection without the rest of its bundle
  org_id  integer not null,
  role_id integer not null, connection_id integer not null,
  primary key (role_id, connection_id)
);
create table if not exists access_request_cards (
  org_id     integer not null,
  request_id integer not null,
  approver   text not null,
  channel    text not null,               -- the DM channel the card landed in
  ts         text not null,               -- the card's ts, for chat.update
  primary key (request_id, approver)
);
create table if not exists investigations (         -- questions handed to the digging lane (investigations.go)
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
-- The queue read: pending work is a row whose lease has run out, whatever its status says. A
-- run whose container died is therefore picked up by the next one rather than being lost.
create index if not exists investigations_pending on investigations(status, lease_until, id);
create index if not exists investigations_thread on investigations(org_id, team_id, channel, thread_ts, status);
create table if not exists jobs (                   -- fix jobs handed to the worker container (jobs.go)
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
create index if not exists jobs_thread on jobs(team_id, channel, thread_ts);
create index if not exists jobs_status on jobs(status);
create table if not exists job_events (
  id integer primary key autoincrement, org_id integer not null, job_id integer not null, seq integer not null,
  kind text not null,                 -- phase|log|tests|usage|heartbeat|warn
  phase text default '', status text default '',   -- phase: clone|test_before|engine|test_after|commit|push|pr ; status: started|ok|failed|skipped
  message text default '', data text default '{}', at text default '',
  tokens_in integer default 0, tokens_out integer default 0, cost_usd real default 0,
  created_at text default (datetime('now'))
);
create unique index if not exists job_events_seq on job_events(job_id, seq);
create table if not exists job_files (
  org_id integer not null, job_id integer not null, kind text not null,    -- diff|log
  bytes integer default 0, content text default '', file_id text default '', permalink text default '',
  created_at text default (datetime('now'))
);
create index if not exists job_files_job on job_files(job_id);
-- The web providers' API keys (Settings → Web), sealed like every other credential. Which
-- provider is chosen and its account id are ordinary settings; only the key is a secret, and a
-- secret does not belong in a table the console reads back. Keyed by provider as well as by
-- organisation, so switching from Firecrawl to Cloudflare and back does not lose either key —
-- and, more to the point, so one provider's key can never be sent to another one.
create table if not exists web_keys (
  org_id     integer not null,
  provider   text not null,
  secret_enc blob,
  updated_by text not null default '',
  updated_at text default (datetime('now')),
  primary key (org_id, provider)
);
`

const slackDeliveryDDL = `
create table if not exists slack_deliveries (
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
create index if not exists slack_deliveries_pending on slack_deliveries(done_at, lease_until, accepted_at);
create index if not exists slack_deliveries_team on slack_deliveries(team_id, done_at);
`
