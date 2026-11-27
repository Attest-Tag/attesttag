-- Record which organisation's IdP authenticated an SSO session or login challenge.
--
-- Without sso_org_id, an SSO session created via Organisation A's IdP only carried via='sso',
-- allowing handleSwitchOrg and requireAdmin to accept it in Organisation B (even when Org B
-- enforces auth_policy='sso' with its own separate IdP), and allowing a post-TOTP SSO login
-- to land in a different organisation than the IdP that authenticated it.
alter table admin_sessions add column sso_org_id integer not null default 0;
alter table login_challenges add column sso_org_id integer not null default 0;
