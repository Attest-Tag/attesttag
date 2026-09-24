# MCP server

attest_tag is an MCP server at `https://your-console-host/mcp` (streamable HTTP, stateless).
Claude, Claude Desktop, Claude Code, Cursor, VS Code and any other MCP client can use it to read
and change what the bot knows, as whoever connects it. Developer → MCP in the console shows the
address, who has connected what, and which tools your role may use.

Every tool is one route of the [developer API](api.md), run in-process with the same permission,
so a client can do exactly what its person can do through `/v1` and nothing more. There are two
ways in: connect with OAuth, which is signing in and pressing Allow, or send a developer key.

## Connecting with OAuth

Give the client the server URL:

- **Claude and Claude Desktop**: Settings → Connectors → Add custom connector, then paste the
  URL.
- **Claude Code**: `claude mcp add --transport http attest-tag https://your-console-host/mcp`,
  then run `/mcp`, pick attest-tag and choose Authenticate.
- **Cursor and VS Code**: add the URL as an HTTP MCP server; each opens a browser to sign in.

The client registers itself and opens the console's consent page. If you are not signed in, you
sign in first, and the organisation's sign-in policy and two-factor rule apply. The page names the
app, your account, the organisation, and the address it will send you back to. That address is
the part to check: the app's name is whatever it called itself.

Approving takes what minting an API key takes: the `api_keys.manage` permission (admin and editor)
and, where the deployment sends mail, a confirmed email address. The connection acts as you, in
the organisation you were signed in to, and your role is re-read on every request.

An access token lasts an hour. The client renews it with a refresh token, which lasts 30 days from
its last use and changes each time it is used. If an old refresh token is presented again,
somebody else has a copy, so the connection is ended and you connect again.

## Connecting with an API key

For scripts, CI and clients configured with a header. Make a key under Developer → API keys:

```bash
claude mcp add --transport http attest-tag https://your-console-host/mcp \
  --header "Authorization: Bearer $ATTESTTAG_KEY"
```

A client configured with JSON (Claude Code's `.mcp.json`, Cursor's `mcp.json`) takes the same URL
with an `Authorization: Bearer <key>` header. The key is the person who made it, exactly as it is
on `/v1`, and revoking it cuts the client off.

## Tools and the permissions they need

| tool | route | needs |
|---|---|---|
| `whoami`, `list_workspaces`, `list_scopes` | `GET /v1/whoami`, `/workspaces`, `/scopes` | a connection |
| `list_documents`, `read_document` | `GET /v1/documents`, `/documents/{path}` | a connection |
| `write_document`, `delete_document` | `PUT`, `DELETE /v1/documents/{path}` | `documents.manage` |
| `list_memories` | `GET /v1/memories` | a connection |
| `add_memory`, `update_memory`, `delete_memory` | `POST`, `PUT`, `DELETE /v1/memories` | `memory.manage` |
| `list_artifacts`, `get_artifact` | `GET /v1/artifacts`, `/artifacts/{id}` | `artifacts.view` |
| `list_routines`, `list_routine_runs` | `GET /v1/routines`, `/routines/{id}/runs` | a connection |
| `run_routine` | `POST /v1/routines/{id}/run` | `routines.manage` |
| `create_routine`, `update_routine`, `delete_routine` | `POST /v1/routines`, `PUT`, `DELETE /v1/routines/{id}` | `routines.manage` |
| `list_jobs`, `get_job` | `GET /v1/jobs`, `/jobs/{id}` | `jobs.view` |
| `list_access_requests` | `GET /v1/access-requests` | `access_requests.view` |
| `list_activity` | `GET /v1/activity` | `activity.view` |
| `list_audit` | `GET /v1/audit` | `audit.view` |
| `get_usage`, `get_billing` | `GET /v1/usage`, `/billing` | a connection |

A client is only offered the tools its person's role may use. A tool whose route a deployment
does not serve is not offered either. Uploading a PDF is not a tool; use `POST /v1/documents`.

A routine made over MCP posts in the channel the client names, by id or `#name`; `list_scopes`
shows the ones the bot is in. It runs as the bot with that channel's connections, and its writes
ask first until someone switches that off in the console. See [api.md](api.md) for the fields.

## Disconnecting, and what the audit log shows

Developer → MCP lists every connected app with the person it acts as. The person can disconnect
their own, and anyone who may manage API keys can disconnect any. Removing someone from the
organisation, or a password reset or change, ends their connections as it revokes their keys.

Calls are rate limited like keys, at 240 a minute per connection. A write through a tool is
recorded as the same write through `/v1` would be, marked via `mcp`, with the key or connection it
came through. Approving an app is `mcp.approved`, disconnecting it is `mcp.revoked`, and a
connection ended because an old refresh token came back is `mcp.replay_ended`.

## For client authors: the OAuth endpoints

The deployment is its own authorization server, per the MCP authorization spec:

- `/.well-known/oauth-protected-resource/mcp` (RFC 9728), which a 401 from `/mcp` points at;
- `/.well-known/oauth-authorization-server` (RFC 8414);
- `POST /oauth/register`, open dynamic registration (RFC 7591);
- `GET /oauth/authorize`, the authorization code grant. PKCE with S256 is required. `resource`
  (RFC 8707) must be this server if sent, and every redirect back carries `iss` (RFC 9207);
- `POST /oauth/token`, for `authorization_code` and `refresh_token`;
- `POST /oauth/revoke` (RFC 7009).

Clients are public by default, and PKCE stands behind them. A client that registers with
`client_secret_basic` or `client_secret_post` gets a secret and must use it. A redirect address
may be https, http to localhost on any port (RFC 8252), or an app's own scheme. An authorization
code lasts ten minutes and works once; using it a second time ends what the first use made.
Access tokens (`ato1.…`) open `/mcp` only: `/v1` takes keys, and the console takes sessions.
