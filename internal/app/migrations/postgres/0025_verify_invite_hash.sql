-- The Postgres twin of ../sqlite/0025_verify_invite_hash.sql: which share link a verification link
-- is waiting to join through, by the hash its row is stored under.
alter table email_tokens add column invite_hash text not null default '';
