-- Code review: attest_tag reading a pull request when GitHub says one was opened or asked about,
-- and answering on the pull request itself. Everything it needs to remember lives here: the
-- webhook deliveries waiting to be dispatched, what each organisation asked reviews to do, the
-- rubrics a review is run against, and what each review found.
--
-- Additive only, like every migration since the baseline. Every table carries org_id, which is
-- what lets the account deletion reach it without being told (store_org_delete.go reads the
-- schema for that column) and what TestEveryPerOrgQueryIsScoped looks for in every statement.
-- None carries team_id: a review belongs to the organisation that connected the GitHub App
-- installation, never to a chat workspace, and a team_id here would make the workspace removal
-- sweep these rows when somebody disconnects Slack.
--
-- Times follow the two conventions already in this schema. Queue times — when a row may be
-- claimed, until when a lease is held — are integer UnixNano, as in slack_deliveries, because
-- they are compared with the process clock to the nanosecond and a lease is fenced on its exact
-- value. Times a person reads are 'YYYY-MM-DD HH:MM:SS' UTC text, like every other created_at,
-- and are what data_retention_days cuts on (retention.go).

-- github_deliveries is the GitHub inbox: one row per webhook delivery the receipt filter kept,
-- held until a dispatcher has done what it asks. The shape and the limits are slack_deliveries'
-- (store_slack_deliveries.go), including what that table learned on Postgres: the claim repeats
-- its outer where, and every write after it is conditional on the lease the claim took.
--
-- Only an installation's own events, and pull-request events for a repository an organisation
-- reviews — its installation in the review tree, its effective mode not off — are stored, so
-- nobody else's pull requests, and no repository somebody switched off, are ever written down here.
--
--   delivery_id      X-GitHub-Delivery, GitHub's GUID for one delivery. A redelivery — somebody
--                    pressing Redeliver, since GitHub never retries on its own — carries the same
--                    one, which is the dedup
--   org_id           the organisation the installation is bound to (github_installs); a delivery
--                    for an installation no organisation here holds is never stored
--   installation_id  the installation the delivery names
--   event, action    X-GitHub-Event and the payload's action, so the dispatcher can route without
--                    opening the payload
--   payload_enc      the body, sealed under MASTER_KEY. Dropped as soon as the delivery is done or
--                    dead: what stays behind is the receipt, which is all the dedup needs
--   accepted_at      when it arrived; the claim takes the oldest first
--   lease_until      until when a dispatcher holds it; also the not-before of a backed-off retry
--   attempts         claims so far. Five and it is dead-lettered
--   done_at, dead_at when it finished, or was given up on; 0 while pending
--   last_error       why the last attempt failed
create table github_deliveries (
  delivery_id     text primary key,
  org_id          integer not null default 0,
  installation_id integer not null default 0,
  event           text not null,
  action          text not null default '',
  payload_enc     blob,
  accepted_at     integer not null,
  lease_until     integer not null default 0,
  attempts        integer not null default 0,
  done_at         integer not null default 0,
  dead_at         integer not null default 0,
  last_error      text not null default ''
);
create index github_deliveries_pending on github_deliveries (done_at, dead_at, lease_until, accepted_at);
-- The console's "webhook receiving" line: when this installation last heard from GitHub.
create index github_deliveries_installation on github_deliveries (org_id, installation_id, accepted_at);

-- review_settings is what an organisation asked reviews to do, as a tree three levels deep: a
-- connection (one GitHub App installation), optional groups the people make under it, and
-- repositories. A value is resolved repository, then group, then connection, then the built-in
-- default, in Go — this table only holds what was set where.
--
-- The same shape as scopes, behind the Workspaces page (kind + parent), on purpose: one way of
-- saying "this level inherits from that one" is one way to get it right.
--
--   kind             'connection' | 'group' | 'repo'
--   parent_id        the connection a group belongs to; the connection or group a repository
--                    sits in. Null for a connection
--   installation_id  a connection's GitHub App installation. Keyed by the installation and never
--                    by a connections.id: deleting an attest_tag connection must not strand a
--                    review tree, and the installation is what GitHub's events name
--   name             a group's name
--   repo             a repository's 'owner/name', lower-cased: GitHub treats names without regard
--                    to case and so does every comparison of connections.repo in the code
--   settings_json    a JSON object holding only the keys set AT THIS LEVEL. A key that is absent
--                    inherits, which is what lets a connection's change reach every repository
--                    that never overrode it
--   removed_at       when reviews on a connection were stopped. The row and everything under it
--                    are kept, so adding the connection again brings its settings back
--
-- A repository gets a row the first time something is set on it or it is put in a group; until
-- then it simply inherits, which is how a repository the App gains later is reviewed at once.
create table review_settings (
  id              integer primary key autoincrement,
  org_id          integer not null,
  public_id       text not null,
  kind            text not null,
  parent_id       integer,
  installation_id integer not null default 0,
  name            text not null default '',
  repo            text not null default '',
  settings_json   text not null default '{}',
  removed_at      text,
  updated_by      text not null default '',
  created_at      text not null,
  updated_at      text not null
);
create unique index review_settings_public_id on review_settings (public_id);
-- One connection row per installation, one row per repository and one group of a name under each
-- connection. Partial, because each holds for one kind only: every group has repo = ''.
create unique index review_settings_connection on review_settings (org_id, installation_id) where kind = 'connection';
create unique index review_settings_repo on review_settings (org_id, repo) where kind = 'repo';
create unique index review_settings_group on review_settings (org_id, parent_id, name) where kind = 'group';
create index review_settings_parent on review_settings (org_id, parent_id);

-- review_types are the rubrics a review runs: General, Security and Release summary ship in the
-- binary, and an organisation's row here is either its own type or its copy of a built-in one,
-- made the first time somebody edits it (copy-on-write). A built-in nobody has edited has no row.
--
--   key                  the short name a branch rule and `@… review <key>` use; unique per
--                        organisation and never changed once saved, because a run records it
--   builtin_key          the built-in this row is a copy of; null for the organisation's own type
--   purpose              "what this review is for", the paragraph the finder is given
--   path_globs_json      a JSON array; empty means every file
--   strictness, model    '' inherits from the settings tree
--   max_usd              0 inherits. A cap on one run, not money spent, so a float like
--                        usage.cost_usd rather than the ledger's micro-dollars
--   inline_min_severity  the least severe finding posted inline ('P0' | 'P1' | 'P2'); '' inherits
--   version              bumped on every save; review_type_versions holds each one
create table review_types (
  id                  integer primary key autoincrement,
  org_id              integer not null,
  public_id           text not null,
  key                 text not null,
  builtin_key         text,
  name                text not null,
  purpose             text not null default '',
  path_globs_json     text not null default '[]',
  strictness          text not null default '',
  model               text not null default '',
  max_usd             real not null default 0,
  inline_min_severity text not null default '',
  enabled             integer not null default 1,
  version             integer not null default 1,
  updated_by          text not null default '',
  created_at          text not null,
  updated_at          text not null
);
create unique index review_types_key on review_types (org_id, key);
create unique index review_types_public_id on review_types (public_id);

-- review_type_rules are the lines a type tells the finder to check, in order.
--
--   position          the rule's place in the list, 0 first
--   severity_cap      the most severe a finding citing this rule may be ('P0' | 'P1' | 'P2')
--   source            'builtin' (came with a built-in type), 'team' (written in the console) or
--                     'learned' (proposed from a reply on a pull request)
--   status            'active' | 'proposed' | 'rejected'. Only active and enabled rules reach a
--                     prompt; a learned rule is proposed until somebody approves it
--   enabled           0 keeps a rule that was turned off, so it can come back
--   from_comment_url  for a learned rule, the review comment it was learned from
create table review_type_rules (
  id               integer primary key autoincrement,
  org_id           integer not null,
  public_id        text not null,
  type_id          integer not null,
  position         integer not null default 0,
  text             text not null,
  severity_cap     text not null default '',
  path_globs_json  text not null default '[]',
  example_bad      text not null default '',
  example_good     text not null default '',
  enabled          integer not null default 1,
  source           text not null default 'team',
  status           text not null default 'active',
  from_comment_url text not null default '',
  created_at       text not null,
  updated_at       text not null
);
create unique index review_type_rules_public_id on review_type_rules (public_id);
create index review_type_rules_type on review_type_rules (org_id, type_id, position);

-- review_type_versions is every save of a type, whole: the type and its rules as a JSON snapshot.
-- A run records the {key, version} it used, so an old review stays explainable after the rubric
-- has changed, and reverting is saving an old snapshot again as a new version.
create table review_type_versions (
  id            integer primary key autoincrement,
  org_id        integer not null,
  type_id       integer not null,
  version       integer not null,
  snapshot_json text not null,
  created_by    text not null default '',
  created_at    text not null
);
create unique index review_type_versions_key on review_type_versions (org_id, type_id, version);

-- review_prs is one pull request as the reviewer knows it: current state, not history.
--
--   is_private        0 until GitHub says otherwise. Public is the restrictive reading — a public
--                     repository may draw only on public context and never shows cost — so a row
--                     written before the repository was looked at must not assume the opposite
--   last_reviewed_sha the head the last finished review was of
--   file_hashes       a JSON object of path → blob sha at last_reviewed_sha, for incremental review
--   paused            1 after a person paused it, or after auto_reviews hit its ceiling
--   auto_reviews      automatic reviews so far; reviews_count counts every kind
--   score             the last finished review's 0–5, as review.Score computes it; -1 before the
--                     first. Not 0: 0 is a real score — two or more open P0s — and the riskiest
--                     pull requests must not read as ones nobody has looked at
--   summary_comment_id the issue comment the summary is edited in
--   body_mirror       the summary block last mirrored into the PR body, when that is turned on
--   lease_until       UnixNano. The hard per-PR fence: one run at a time per pull request
create table review_prs (
  id                 integer primary key autoincrement,
  org_id             integer not null,
  repo               text not null,
  pr_number          integer not null,
  state              text not null default 'open',
  is_fork            integer not null default 0,
  is_private         integer not null default 0,
  author_login       text not null default '',
  head_sha           text not null default '',
  last_reviewed_sha  text not null default '',
  file_hashes        text not null default '{}',
  paused             integer not null default 0,
  auto_reviews       integer not null default 0,
  reviews_count      integer not null default 0,
  score              integer not null default -1,
  summary_comment_id integer not null default 0,
  body_mirror        text not null default '',
  skip_reason        text not null default '',
  lease_until        integer not null default 0,
  updated_at         text not null
);
create unique index review_prs_key on review_prs (org_id, repo, pr_number);

-- review_runs is one piece of review work and its outcome: a review, a reply in a thread, an
-- answer to a question, or a resync of what was posted.
--
--   kind             review | reply | answer | resync
--   dedupe_key       what makes two requests the same request — the head, the types and the
--                    scope for a review; the comment for a reply — so a retried trigger or two
--                    instances racing produce one run
--   trigger          open | push | command | catchup | console | api; trigger_ref is the comment
--                    or delivery that caused it
--   head_sha         the commit reviewed, and the commit_id the review is posted against
--   cache_key        what makes a finished review reusable for a later identical request
--   types_json       [{"key":…,"version":…}] — the rubrics, at the versions, this run used
--   rule_label       the branch rule that chose them, as the console and the summary show it
--   config_hash      a hash of the effective settings, so a later settings change never
--                    rewrites what a past run was told
--   status           queued | running | posted | shadow | noop | skipped | superseded | failed |
--                    cancelled
--   not_before       UnixNano; a run is not claimed before it
--   lease_until      UnixNano; fenced like github_deliveries
--   cancel           1 when somebody asked it to stop
--   reserved_usd     the money held for it before it ran
--   not_reviewed     a JSON array of the paths it did not get to, said out loud in the summary
--   candidates, dropped, kept
--                    what the finder proposed, what verification threw away, what was posted
--   score            the review's 0–5; -1 for a run that produced none — still queued, failed,
--                    a noop, a reply or an answer — for the reason review_prs.score gives
create table review_runs (
  id               integer primary key autoincrement,
  public_id        text not null,
  org_id           integer not null,
  review_pr_id     integer not null,
  repo             text not null,
  pr_number        integer not null,
  kind             text not null default 'review',
  dedupe_key       text not null,
  trigger          text not null default '',
  trigger_ref      text not null default '',
  requested_by     text not null default '',
  head_sha         text not null default '',
  base_sha         text not null default '',
  cache_key        text not null default '',
  types_json       text not null default '[]',
  rule_label       text not null default '',
  config_hash      text not null default '',
  status           text not null default 'queued',
  not_before       integer not null default 0,
  lease_until      integer not null default 0,
  attempts         integer not null default 0,
  cancel           integer not null default 0,
  reserved_usd     real not null default 0,
  github_review_id integer not null default 0,
  files_reviewed   integer not null default 0,
  not_reviewed     text not null default '[]',
  candidates       integer not null default 0,
  dropped          integer not null default 0,
  kept             integer not null default 0,
  score            integer not null default -1,
  summary          text not null default '',
  risk             text not null default '',
  model            text not null default '',
  tokens_in        integer not null default 0,
  tokens_out       integer not null default 0,
  tokens_cached    integer not null default 0,
  cost_usd         real not null default 0,
  error            text not null default '',
  created_at       text not null,
  started_at       text not null default '',
  finished_at      text not null default ''
);
create unique index review_runs_public_id on review_runs (public_id);
create unique index review_runs_dedupe on review_runs (org_id, review_pr_id, dedupe_key);
-- The lane's claim, which serves every tenant like the investigations lane does.
create index review_runs_claim on review_runs (status, not_before, lease_until);
-- The console's history, newest first.
create index review_runs_org on review_runs (org_id, id);

-- review_findings is one problem a review raised, followed across the pull request's life: the
-- run that first raised it, the run that last saw it, where it is anchored, and what happened to
-- it since — withdrawn, fixed, resolved by a person, outdated or disputed.
--
--   review_type       the type that raised it, shown on the comment ("Security · P1 · …"), whose
--                     rules its rule_refs cite
--   review_types      a JSON array of every type that raised it, review_type first: when two
--                     types flag the same line, the shared verify step merges them into one finding
--                     with both tags ("Security + General · P1 · …") and it is listed under each
--   side, start_line, line, anchor_sha
--                     where the inline comment sits, exactly as GitHub's review API takes it
--   code_hash         a hash of the lines it is about, so a later push can tell whether they moved
--                     or changed
--   placement         inline | summary
--   severity          P0 | P1 | P2
--   rule_refs         a JSON array of the rules it cites
--   pre_existing      1 when the problem is in code the pull request did not change
--   fingerprint       what makes two findings the same finding across runs
--   verifier_confidence
--                     what the verifier said, 0–100 as everywhere in the review package; 0 when
--                     it was not recorded
--   status            open | withdrawn | fixed | resolved_by_human | outdated | disputed
--   claimed_fixed_sha a reply's "fixed in <sha>", checked before the finding is closed on it
--   github_comment_id, thread_node_id
--                     the inline comment and its review thread, which is how a reply finds its
--                     finding
--   bot_replies       how many times the bot has answered in that thread, which is capped
create table review_findings (
  id                  integer primary key autoincrement,
  public_id           text not null,
  org_id              integer not null,
  review_pr_id        integer not null,
  first_run_id        integer not null default 0,
  last_run_id         integer not null default 0,
  review_type         text not null default '',
  review_types        text not null default '[]',
  path                text not null,
  side                text not null default 'RIGHT',
  start_line          integer not null default 0,
  line                integer not null default 0,
  anchor_sha          text not null default '',
  code_hash           text not null default '',
  placement           text not null default '',
  kind                text not null default '',
  severity            text not null default '',
  category            text not null default '',
  title               text not null default '',
  body                text not null default '',
  suggestion          text not null default '',
  evidence            text not null default '',
  rule_refs           text not null default '[]',
  pre_existing        integer not null default 0,
  fingerprint         text not null default '',
  verifier_confidence integer not null default 0,
  status              text not null default 'open',
  status_reason       text not null default '',
  status_by           text not null default '',
  claimed_fixed_sha   text not null default '',
  claimed_by          text not null default '',
  github_comment_id   integer not null default 0,
  thread_node_id      text not null default '',
  bot_replies         integer not null default 0,
  created_at          text not null,
  updated_at          text not null
);
create unique index review_findings_public_id on review_findings (public_id);
create index review_findings_pr on review_findings (org_id, review_pr_id, status);
create index review_findings_comment on review_findings (org_id, github_comment_id);

-- Whether reviews may run on the organisation's own model key. Off by default, unlike fix_jobs
-- (0020): a review is started by somebody opening a pull request, not by a member of the
-- organisation pressing a button, so spending the organisation's key on it is the owner's call
-- to make, in Settings → Models, and not something an upgrade turns on for them.
alter table org_model_keys add column reviews integer not null default 0;
