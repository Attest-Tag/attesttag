-- The Postgres twin of ../sqlite/0010_usage_cache_tokens.sql. Same two columns; integer becomes
-- bigint, as everywhere else in this dialect.
--
-- See the SQLite file for what each column holds, why neither is money, and why they default to
-- zero rather than to null.
alter table usage add column cached_in bigint not null default 0;
alter table usage add column tokens_reasoning bigint not null default 0;
