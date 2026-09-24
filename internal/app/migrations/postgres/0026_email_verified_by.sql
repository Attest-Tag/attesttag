-- The Postgres twin of ../sqlite/0026_email_verified_by.sql: how an account's address came to be
-- confirmed, and which organisation's invitation confirmed it, so a domain-limited link can refuse
-- a proof its redeemer could have arranged.
alter table users add column email_verified_by text not null default '';
alter table users add column email_verified_org bigint not null default 0;
