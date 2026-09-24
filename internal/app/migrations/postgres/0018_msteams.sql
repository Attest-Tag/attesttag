-- The Postgres twin of ../sqlite/0018_msteams.sql: the Bot Framework service URL on a Teams
-- tenant's row, the conversation log attest_tag keeps for Teams (Microsoft gives an application no
-- way to read a thread back), who the bot has met in each tenant, and the one-time codes that join
-- a Teams tenant to an organisation. See the SQLite file for why each exists. integer becomes
-- bigint, as everywhere else in this dialect.
alter table teams add column service_url text not null default '';

create table msteams_messages (
  team_id   text not null,
  channel   text not null,
  thread    text not null,
  message_id text not null,
  user_id   text not null default '',
  user_name text not null default '',
  is_bot    bigint not null default 0,
  text      text not null default '',
  files     text not null default '',
  at        text not null,
  primary key (team_id, channel, message_id)
);
create index msteams_messages_thread on msteams_messages(team_id, channel, thread, at);

create table msteams_users (
  team_id      text not null,
  user_id      text not null,
  teams_id     text not null default '',
  name         text not null default '',
  email        text not null default '',
  conversation text not null default '',
  updated_at   text not null,
  primary key (team_id, user_id)
);

create table link_codes (
  code_hash  text primary key,
  org_id     bigint not null,
  platform   text not null,
  created_by bigint not null,
  expires_at text not null,
  used_at    text,
  used_by    text not null default ''
);
