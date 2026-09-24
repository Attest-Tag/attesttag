-- The Postgres twin of ../sqlite/0008_email_auto_writes.sql. Same column, same default; see the
-- SQLite file for what it is and why it is off until it is asked for.
alter table scopes add column email_auto_writes text default 'inherit';  -- inherit | on | off
