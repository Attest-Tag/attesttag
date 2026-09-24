-- The Postgres twin of ../sqlite/0020_org_model_keys.sql. Same columns; integer becomes bigint and
-- blob becomes bytea, as everywhere else in this dialect.
--
-- See the SQLite file for what each column holds and why the key is never read back.
create table org_model_keys (
  org_id         bigint primary key,
  preset         text not null default '',
  base_url       text not null,
  key_enc        bytea not null,
  key_hint       text not null default '',
  key_fp         text not null default '',
  default_model  text not null default '',
  embed_model    text not null default '',
  fix_jobs       bigint not null default 1,
  updated_by     text not null default '',
  created_at     text not null,
  updated_at     text not null,
  last_ok_at     text not null default '',
  last_error     text not null default '',
  last_error_at  text not null default ''
);
