-- The Postgres twin of ../sqlite/0019_enterprise_terms.sql. Same columns; integer becomes bigint,
-- as everywhere else in this dialect.
--
-- See the SQLite file for what each column holds, why an enterprise deal is a row rather than an
-- entry in STRIPE_SIZES, and why the row exists only while the account is on the enterprise plan.
create table enterprise_terms (
  org_id         bigint primary key,
  user_limit     bigint not null default 0,
  job_limit      bigint not null default 0,
  fee_minor      bigint not null default 0,
  fee_interval   text not null default 'month',
  included_minor bigint not null default 0,
  price_id       text not null default '',
  pay_url        text not null default '',
  note           text not null default '',
  updated_by     text not null default '',
  created_at     text not null,
  updated_at     text not null
);
