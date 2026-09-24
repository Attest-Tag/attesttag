-- The Postgres twin of ../sqlite/0021_usage_key_owner.sql: whose key paid for a model call.
alter table usage add column key_owner text not null default 'platform';
alter table jobs add column key_owner text not null default 'platform';
