-- Who actually used the bot, one row per person per day, so a ladder priced per user can say
-- what it is charging for.
--
-- 0009_billing.sql, written when the ladder was counted in employees, says the fee is set by
-- "headcount as the customer declares it -- never seats, nothing counts anybody". That is no
-- longer true and that file cannot be edited (an applied migration that changes is a boot
-- failure, migrations.go), so the correction is here: the bands are counted in USERS, and this
-- table is how many there were.
--
-- A user is a person in a connected Slack workspace who caused a turn to run, in the last 30
-- days. Deliberately not: a console sign-in (most admins configure the thing and never use it),
-- an API key's owner, or whoever created a routine that has been running on its own since they
-- left. Machine activity is not a user, and neither is a forwarded email -- email intake is
-- keyed per channel and not per sender on purpose, so there is no person there to count.
--
-- WHY THIS IS NOT A QUERY OVER `usage`. usage carries (org_id, team_id, user_id, created_at) and
-- looks like it would answer this, but retention.go deletes from it on the tenant's own
-- data_retention_days, whose floor is seven days. A customer could zero the number they are
-- billed on by shortening their own retention, which is not a control a customer gets to hold.
-- usage.user_id is also three namespaces in one column -- a Slack id, a console public id, and
-- the 'email:<channel>' sentinel -- with nothing saying which, and no index.
--
-- So this table is on the same footing as credit_ledger: retention.go does NOT sweep it, and the
-- note there says so rather than leaving it to be "fixed" later. It is the basis of what the
-- customer is charged, and their own retention policy deleting it is the deletion that must not
-- happen. It has its own 400-day sweep in the hourly billing loop, and it goes with the
-- organisation when the organisation goes (org_id is what store_org_delete.go looks for).
--
-- identity is 'T…:U…' and never a bare U…. Slack only promises a user id is unique inside one
-- workspace, so a bare id can address a different person in a different workspace -- the same
-- trap personal_memory_api.go documents for private notes. The workspace is part of the key.
--
-- email_hash is sha256 of the lowercased address, filled where the install granted
-- users:read.email and left empty where it did not. It is what lets one human in two connected
-- workspaces count once. It is a DEDUPE KEY AND NOT ANONYMISATION: a hash of an address at a
-- known domain is guessable, and the reason to store the hash rather than the address is only
-- that this table outlives the tenant's retention policy and should not be a directory.
create table active_users (
  org_id     integer not null,
  identity   text not null,
  day        text not null,
  team_id    text not null default '',
  email_hash text not null default '',
  turns      integer not null default 0,
  first_seen text not null,
  last_seen  text not null
);
create unique index active_users_key on active_users(org_id, identity, day);
create index active_users_window on active_users(org_id, day);

-- Backfill from whatever usage still holds, so the figure is a real one on the day this ships
-- rather than zero for a month. Approximate by construction and that is the point: rows already
-- swept are gone, and the predicate below is the best guess at "a Slack person" that a column
-- holding three namespaces allows.
--
--   team_id <> ''            drops the console assistant, which writes no workspace
--   user_id like 'U%' or 'W%'  Slack ids; W is Enterprise Grid, and leaving it out would
--                            undercount exactly the largest customers
--   not in (bot_user_id)     the bot's own id, which self-test and creatorless routines write
--
-- 'email:<channel>' fails the like, and the '' that classifier and allow-rule calls write fails
-- it too. No email_hash: it fills itself in as people come back.
insert into active_users (org_id, identity, day, team_id, turns, first_seen, last_seen)
select u.org_id, u.team_id || ':' || u.user_id, substr(u.created_at, 1, 10), u.team_id,
       count(*), min(u.created_at), max(u.created_at)
  from usage u
 where u.team_id <> ''
   and (u.user_id like 'U%' or u.user_id like 'W%')
   and u.user_id not in (select bot_user_id from teams where bot_user_id <> '')
 group by u.org_id, u.team_id, u.user_id, substr(u.created_at, 1, 10);
