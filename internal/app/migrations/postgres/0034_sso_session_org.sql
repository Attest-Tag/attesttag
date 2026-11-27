-- The Postgres twin of ../sqlite/0034_sso_session_org.sql: bind SSO sessions and login
-- challenges to the organisation whose IdP authenticated them.
alter table admin_sessions add column sso_org_id bigint not null default 0;
alter table login_challenges add column sso_org_id bigint not null default 0;
