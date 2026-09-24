-- Where `!restart` cut a thread: the platform's own id for the message that asked — a Slack ts, a
-- Teams message id. A turn reads the thread only from after it (afterRestart, agent.go).
--
-- The thread a turn answers from is fetched from Slack every time, whole, so a restart that only
-- marked rows here forgot nothing: the reply after "Fresh start" was written from everything the
-- thread had ever said. The cut has to be a point in the thread itself, remembered on the session.
--
-- '' is no restart, which is every existing session.
alter table sessions add column restart_ts text not null default '';
