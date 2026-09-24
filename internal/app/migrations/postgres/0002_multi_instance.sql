-- The Postgres twin of ../sqlite/0002_multi_instance.sql. Same tables, same columns; integer
-- becomes bigint because these hold UnixNano, and blob becomes bytea.
--
-- See the SQLite file for what each of these replaces and why.

create table leader_leases (
  role       text primary key,
  holder     text not null,
  expires_at bigint not null
);

create table oauth_pendings (
  state        text primary key,
  org_id       bigint not null,
  user_id      bigint not null default 0,
  conn_id      bigint not null,
  verifier_enc bytea not null,
  created_at   text default (to_char(now() at time zone 'utc', 'YYYY-MM-DD HH24:MI:SS')),
  expires_at   text not null
);

create table throttle_events (
  key text not null,
  at  bigint not null
);
create index throttle_events_key on throttle_events(key, at);

alter table routines add column run_lease bigint not null default 0;
alter table routines add column run_holder text not null default '';

alter table sessions add column stop_requested_at text;
