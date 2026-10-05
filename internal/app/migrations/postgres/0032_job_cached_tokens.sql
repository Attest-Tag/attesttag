-- The Postgres twin of ../sqlite/0032_job_cached_tokens.sql: the part of a fix job's tokens_in
-- the provider served from its prompt cache.
alter table jobs add column tokens_cached integer not null default 0;
alter table job_events add column tokens_cached integer not null default 0;
