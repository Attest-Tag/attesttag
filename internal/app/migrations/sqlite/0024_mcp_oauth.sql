-- The MCP server's own OAuth: attest_tag as the authorization server an MCP client — Claude,
-- Cursor, VS Code — sends a person to, so connecting one is signing in and pressing Allow rather
-- than pasting a developer key into it (mcp_oauth.go). A key still works at /mcp as well; these
-- tables are only the other door.
--
-- mcp_clients is every client that registered itself (RFC 7591). Global rather than per
-- organisation: a client registers before anybody has signed in, and one client is used by people
-- in many organisations. Every client proves itself with PKCE; the few that ask for a secret
-- as well get one, kept as a hash.
--
--   id             the client_id handed back at registration, random
--   name           what the client calls itself. The consent screen shows it beside the address
--                  the person will be sent back to, which is the part they can actually check
--   redirect_uris  a JSON array, compared exactly (a loopback address may change its port)
--   auth_method    how it authenticates at the token endpoint: none, the default and what MCP
--                  clients use, or client_secret_basic / client_secret_post for the few that
--                  register as confidential. PKCE is required either way
--   secret_hash    sha-256 of the secret handed out at registration, when there is one
create table mcp_clients (
  id            text primary key,
  name          text not null default '',
  redirect_uris text not null,
  auth_method   text not null default 'none',
  secret_hash   text not null default '',
  created_at    text not null,
  last_used_at  text not null default ''
);

-- mcp_codes is an authorization code between the consent screen and the token endpoint: found by
-- its hash, single use, ten minutes. grant_id is the grant its exchange made, so that a second
-- attempt to exchange it can end that grant, as RFC 6749 asks.
create table mcp_codes (
  code_hash       text primary key,
  client_id       text not null,
  org_id          integer not null,
  user_id         integer not null,
  redirect_uri    text not null,
  code_challenge  text not null,
  scope           text not null default '',
  resource        text not null default '',
  expires_at      text not null,
  used_at         text not null default '',
  grant_id        integer not null default 0
);

-- mcp_grants is one person's consent for one client in one organisation: the connected app the
-- console lists and revokes. It acts as that person exactly as a developer key does, re-resolved
-- on every request. It carries the current access and refresh tokens as hashes; a refresh
-- replaces both, and keeps the refresh token it replaced, because a second use of that one means
-- somebody else holds a copy — and ends the grant.
create table mcp_grants (
  id                  integer primary key autoincrement,
  org_id              integer not null,
  user_id             integer not null,
  client_id           text not null,
  client_name         text not null default '',
  redirect_uri        text not null default '',
  scope               text not null default '',
  resource            text not null default '',
  access_hash         text not null,
  access_expires_at   text not null,
  refresh_hash        text not null,
  refresh_expires_at  text not null,
  prev_refresh_hash   text not null default '',
  created_at          text not null,
  last_used_at        text not null default '',
  revoked_at          text not null default ''
);
create unique index mcp_grants_access on mcp_grants (access_hash);
create unique index mcp_grants_refresh on mcp_grants (refresh_hash);
create index mcp_grants_prev_refresh on mcp_grants (prev_refresh_hash);
create index mcp_grants_org on mcp_grants (org_id, id);
