-- What people asked the console assistant, and what it said back.
--
-- The assistant's spend already lands in `usage` and its reads in `tool_calls`, so Activity
-- could show that a console turn happened, on which model, for how much, and which resources it
-- read — everything except the two things a person reading it actually wants, which are the
-- question and the answer.
--
-- Its own table rather than a row in `turns`, and that is the whole reason this migration
-- exists. `turns` is keyed on team_id with no org_id at all: every query against it is narrowed
-- by workspace, which is what stands in for the organisation there. A console account has no
-- workspace, so console rows would go in with team_id = '' and any organisation could read any
-- other's by asking for that same empty key. Adding org_id to `turns` instead would mean
-- backfilling and re-proving every query the Slack path makes against it, to hold something
-- that is not a Slack turn and has no thread, no channel and no Slack user.
--
-- Append-only, like the audit log and for the same reason: it is the record of what was asked,
-- and a record somebody can edit afterwards is not one. The retention sweep is the only thing
-- that removes a row, on the organisation's own data_retention_days.
--
-- The conversation id is the browser's, minted per conversation and sent with each question, so
-- a thread reads back as a thread. It is not trusted for anything — nothing is looked up by it,
-- every row carries its own org_id, and the worst a forged one can do is group somebody's own
-- questions oddly in their own organisation's Activity page.
create table assistant_turns (
  id           integer primary key autoincrement,
  org_id       integer not null,
  created_at   text not null default (strftime('%Y-%m-%d %H:%M:%S','now')),
  -- The console account that asked, by its public id, and the name to show when the account is
  -- later removed — written out now for the same reason the audit log writes its actor out.
  actor        text not null default '',
  actor_name   text not null default '',
  conversation text not null default '',
  question     text not null default '',
  reply        text not null default '',
  model        text not null default '',
  -- Where they were standing when they asked. It is what makes a question legible six weeks
  -- later: "what can this reach" means nothing without the page it was asked from.
  page         text not null default '',
  tokens_in    integer not null default 0,
  tokens_out   integer not null default 0,
  cost_usd     real not null default 0,
  tool_calls   integer not null default 0,
  proposals    integer not null default 0,
  error        text not null default ''
);

create index assistant_turns_org_idx on assistant_turns (org_id, id desc);
