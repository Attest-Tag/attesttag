-- The Postgres twin of ../sqlite/0023_session_restart.sql: where `!restart` cut a thread.
alter table sessions add column restart_ts text not null default '';
