-- The audit log: who did what, from where, and what came of it.
--
-- Everything else that records history here records what the bot did — turns, tool calls,
-- proxied requests. Nothing recorded what the people did: who signed in and from which address,
-- who changed a credential, who gave somebody admin, who approved a write in Slack, who exported
-- the activity table. Those were slog lines, which live in whatever the platform keeps of stdout
-- and are gone from anything the organisation itself can open.
--
-- One table for all of it rather than a column on each of the tables it concerns, because the
-- questions asked of it cut across them: "everything this person did last week", "every
-- credential change this quarter", "every sign-in from outside the office". Append-only by
-- convention — nothing in the code updates or deletes a row except the organisation's own
-- retention policy (audit_retention_days, separate from data_retention_days deliberately) and
-- the deletion of the organisation itself.
--
-- The actor is written out in full at the time — id, email, name — rather than joined at read
-- time: a person removed from the organisation, or an account deleted, must still be legible in
-- the record of what they did.
create table audit_log (
  id           integer primary key autoincrement,
  org_id       integer not null,
  created_at   text not null,
  action       text not null,                  -- dotted verb: 'auth.sign_in', 'connection.created', …
  outcome      text not null default 'ok',     -- 'ok' | 'denied' | 'failed'
  via          text not null default '',       -- 'console' | 'api_key' | 'slack' | 'operator' | 'system'
  actor_id     integer not null default 0,     -- users.id, 0 when the actor has no account here
  actor_public text not null default '',       -- users.public_id: what the console names a person by
  actor_email  text not null default '',
  actor_name   text not null default '',
  actor_slack  text not null default '',       -- the Slack user id, when the act happened in Slack
  team_id      text not null default '',       -- the workspace it happened in, when it happened in one
  target_kind  text not null default '',       -- 'connection', 'member', 'scope', 'route', …
  target_id    text not null default '',
  target_name  text not null default '',       -- what it was called at the time; ids outlive names
  ip           text not null default '',
  user_agent   text not null default '',
  details      text not null default '{}'      -- json: what changed, never a secret
);
-- The listing is always one organisation's newest rows first, and the export walks them by id.
create index audit_log_org_id on audit_log(org_id, id);
