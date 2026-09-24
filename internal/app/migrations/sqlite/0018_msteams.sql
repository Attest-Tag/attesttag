-- What a Microsoft Teams tenant needs stored that a Slack workspace does not.
--
-- teams.service_url. Slack has one API host; the Bot Framework has one per region, and every
-- activity says which one this tenant's conversations live on. A reply can use the one the
-- message arrived with, but a routine, a job report or an approval card posts with no message to
-- read it from — so the last one seen is kept on the tenant's row.
alter table teams add column service_url text not null default '';

-- msteams_messages is the tenant's conversation log, kept by attest_tag itself.
--
-- Slack will hand a bot any thread it is in, and the agent rebuilds every turn from that. A Teams
-- bot has no such call on its own identity: reading a channel back needs Graph and a consent the
-- tenant may never give, and a one-to-one chat has no thread to read at all. Without a log, every
-- turn would see only the message in front of it and forget the one before.
--
-- So every message the bot receives, and every one it posts, is written here, and a Teams
-- workspace's thread is read back from this table. With the resource-specific consent a team
-- owner grants at install, the bot receives every message in the team's channels, which makes the
-- log whole from the day it was installed. It is also the only search index Teams will ever give
-- this app: Microsoft offers no message search to an application identity.
--
-- text is stored in attest_tag's own dialect (<@id> mentions), the way a Slack message arrives,
-- so everything that reads a thread reads this one the same way. at is a sortable UTC timestamp:
-- Teams message ids are not promised to sort, and a thread must. files is what came attached, as
-- JSON — each one's name, type, and where it can be fetched from — since, again, nothing else
-- would say so when the thread is read back.
create table msteams_messages (
  team_id   text not null,               -- msteams:<tenant id>
  channel   text not null,               -- conversation id, without any ;messageid=
  thread    text not null,               -- root message id in a channel; 'chat' in a chat
  message_id text not null,              -- the activity id
  user_id   text not null default '',    -- Entra object id; the bot's own posts carry its bot id
  user_name text not null default '',
  is_bot    integer not null default 0,
  text      text not null default '',
  files     text not null default '',
  at        text not null,
  primary key (team_id, channel, message_id)
);
create index msteams_messages_thread on msteams_messages(team_id, channel, thread, at);

-- msteams_users is who the bot has met in a tenant: the stable Entra object id attest_tag uses
-- as a person's id, the per-bot 29: id Teams addresses them by, and the one conversation the
-- roster can be asked about them in. The Bot Framework will describe a member only in the context
-- of a conversation they are in, so the last one seen is kept to ask in.
create table msteams_users (
  team_id      text not null,
  user_id      text not null,              -- Entra object id
  teams_id     text not null default '',   -- 29:…
  name         text not null default '',
  email        text not null default '',
  conversation text not null default '',
  updated_at   text not null,
  primary key (team_id, user_id)
);

-- link_codes join a chat tenant to an organisation. A Slack install starts in the console, so the
-- organisation is known when Slack calls back. A Teams app is installed in the Teams admin centre,
-- where nothing says which account it is for — so the console mints a short code and someone in
-- the tenant sends it to the bot. The code proves the organisation; the Bot Framework's signature
-- on the message that carries it proves the tenant.
--
-- Stored as a hash, single use, short-lived. used_by records which workspace spent it.
create table link_codes (
  code_hash  text primary key,
  org_id     integer not null,
  platform   text not null,
  created_by integer not null,
  expires_at text not null,
  used_at    text,
  used_by    text not null default ''
);
