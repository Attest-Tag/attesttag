-- The Postgres twin of ../sqlite/0005_email_intake.sql. Same column, same default; see the
-- SQLite file for what it is and why it is off until it is asked for.
alter table scopes add column email_intake text default 'inherit';  -- inherit | on | off
