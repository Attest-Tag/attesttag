-- The Postgres twin of ../sqlite/0015_plan_size_rename.sql. Same two renames, same spelling.
--
-- See the SQLite file for why "band" had to go — it named a bracket you were sorted into, and
-- these are steps a customer picks — and for the three audit_log action strings that deliberately
-- keep the old verb because they are rows already written rather than identifiers.
alter table billing_accounts rename column band to size;
alter table billing_accounts rename column allowance_band to allowance_size;
