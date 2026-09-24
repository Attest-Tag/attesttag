-- The Postgres twin of ../sqlite/0014_credit_allowance.sql. Same four columns; integer becomes
-- bigint, as everywhere else in this dialect.
--
-- See the SQLite file for what these hold and why the allowance is deliberately not on
-- credit_ledger: it is part of a fee rather than a purchase, it belongs to the month it was
-- billed for, and expiring it on the ledger would mean a negative "unused" row every month for
-- every subscriber in the one table nothing is allowed to sweep.
alter table billing_accounts add column allowance_micros         bigint not null default 0;
alter table billing_accounts add column allowance_granted_micros bigint not null default 0;
alter table billing_accounts add column allowance_period_end     text not null default '';
alter table billing_accounts add column allowance_band           text not null default '';
