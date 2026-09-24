-- The Postgres twin of ../sqlite/0013_active_users.sql. Same table, same two indexes, same
-- backfill; integer becomes bigint, as everywhere else in this dialect.
--
-- See the SQLite file for what a "user" is here, and for why this table is not a query over
-- `usage`: retention.go sweeps that one on the tenant's own policy, which would let a customer
-- shorten their retention and shrink the number they are billed on.
--
-- The backfill's substr(created_at, 1, 10) is deliberate on both sides. created_at is text in
-- 'YYYY-MM-DD HH:MM:SS', so substr is the one spelling that means the same thing in both
-- dialects -- date() would not, and dialect-specific SQL outside db_dialect.go is what
-- TestNoDialectSpecificSQL exists to refuse.
create table active_users (
  org_id     bigint not null,
  identity   text not null,
  day        text not null,
  team_id    text not null default '',
  email_hash text not null default '',
  turns      bigint not null default 0,
  first_seen text not null,
  last_seen  text not null
);
create unique index active_users_key on active_users(org_id, identity, day);
create index active_users_window on active_users(org_id, day);

insert into active_users (org_id, identity, day, team_id, turns, first_seen, last_seen)
select u.org_id, u.team_id || ':' || u.user_id, substr(u.created_at, 1, 10), u.team_id,
       count(*), min(u.created_at), max(u.created_at)
  from usage u
 where u.team_id <> ''
   and (u.user_id like 'U%' or u.user_id like 'W%')
   and u.user_id not in (select bot_user_id from teams where bot_user_id <> '')
 group by u.org_id, u.team_id, u.user_id, substr(u.created_at, 1, 10);
