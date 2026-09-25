# Connections: reaching outside services

Connections let the bot call services outside Slack without the model ever holding a
credential. The pieces, mirroring Claude Tag's Access bundles:

- **Scope**: the whole organisation (the *all workspaces* row on the Workspaces page), one
  connected workspace, or one channel. Scopes have their own instructions, default model,
  read-every-message setting, tool rounds, monthly budget, allow rules, email intake, default
  repository, a lock that stops channel members editing the Configure page, and what is attached
  to them: whole bundles and one-off connections.
- **Bundle**: a named group of connections, credential-less domains, instructions, and skills
  that is attached to one or more scopes. A channel gets the union of what is attached to it,
  to its workspace and to the organisation; when two connections cover the same URL, the one
  attached at the narrowest level is used.
- **Connection**: one credential for one service, named `bundle/connection` everywhere. It has
  a preset, a credential type (`bearer`, `basic`, `header`, `query` parameter, `oauth2_cc` client
  credentials, `gcp_sa` service account, `aws_sigv4` access key, `oauth_user` for each person's
  own account, `github_app` installation, or `mcp`), allowed hosts, optional path prefixes and
  methods, extra headers, and a writes policy: `confirm` (default), `auto`, or `all`. Reading
  through a connection is never held; `auto` lets writes through too, and `all` is the override
  that holds every call, reads included. A connection can also be attached to a scope
  on its own, without the rest of its bundle: that grants just the connection (plus its
  preset's tool pack, when the bundle enables it), not the bundle's instructions, skills or
  domains.
- **Domain**: a host the bot may fetch through the proxy without any credential. Reads go
  straight through; a write waits for Confirm, as it does on a `confirm` connection.
- **Skill**: a Markdown file of instructions attached to a bundle (how to use a tool well,
  runbooks, house conventions). Enabled skills are appended to the system prompt wherever
  the bundle applies.

## Presets and tool packs

Presets fill in hosts, auth style, test request, and usage notes for common services, grouped
the way the Connect list shows them: ClickUp, Linear, Jira and Confluence, Asana; GitHub,
GitLab, Bitbucket; GCP logs and monitoring (read-only), Sentry, Datadog, Grafana, New Relic,
PagerDuty; Google Cloud, AWS, Azure; CircleCI, Vercel, Cloudflare; Zendesk, Intercom;
Notion, Google Drive, Airtable, Figma; HubSpot; Stripe; Google Workspace (each person's own
Gmail, Calendar, Drive and contacts) — plus a custom HTTP API and a custom MCP server. A Drive
credential can also feed Documents directly, which is a different thing from asking Drive a
question — see [Drive sync](google.md#drive-sync). Five of them also ship **tool packs**, thin
named tools that make an open-weight model reliable on the
common asks: `clickup_search_tasks`, `github_get_pr`, `gcp_query_logs`, `sentry_issues`,
`hubspot_search` and friends. Packs are enabled per bundle.

## Connections people sign into for themselves

> Setting one up, step by step: [**Google**](google.md#google-workspace).

Most connections are one credential an admin pastes and a whole channel spends. That is the
wrong shape for a mailbox or a calendar: the answer to *what is on my calendar* depends on who
asked, and one person must never read another's mail through a shared token. **Google Workspace**
is the first connection of the other kind. The admin registers an OAuth client — in
their own Google Cloud project, so an *Internal* consent screen keeps Google's verification and
the restricted-scope assessment out of the way — and pastes its client id and secret. That pair
reaches nobody's account on its own.

One sign-in covers the whole connection, so the dialog is where an admin decides what that
sign-in asks for: **Calendar**, **Gmail**, **Drive** and **Contacts and directory** are separate
checkboxes, and Calendar, Gmail and Drive each have a second box for whether they may write as
well as read. Drive is the one that starts unticked — every other part is bounded by its service,
while a Drive grant reaches every file the person can open. A part
left unticked is a scope nobody is ever asked to grant and a host the proxy will not carry, so
"Calendar, read only, no mail" is refused by Google itself rather than by a rule the model is
trusted to keep. Writing is scoped rather than method-gated on purpose: a read-only calendar
still has to `POST` to `freeBusy` to answer *when is everyone free*.

### Each person connects their own account

Everyone then signs in for themselves. The first time somebody asks about their calendar the bot
sends them a private message with a Connect button; `!connect` in a channel lists the same links
again. The link is a capability minted for one person and one connection, good for thirty
minutes, and it never goes near the model — a tool result is prompt text, and prompt text travels
to whoever serves the model. Their refresh token is sealed against their Slack identity and used
only for calls they themselves set off, which includes the routines they created. On Activity, a
call that spent somebody's own connection — an `http_request`, or a `run_js` script whose `fetch()`
went through it — is listed by name, outcome and duration with a lock beside it, and its arguments
and result are withheld from everyone, admins included: what came back is that person's mail or
calendar, and Activity is open to every member holding `activity.view`. The log keeps whose account
it was and the connection; for an `http_request`, also the method, the endpoint without its query
string, the HTTP status and the size of the reply, or the first sentence of why nothing came back
(held for a Confirm, waiting on an approver, not connected yet); for a script, only the size of its
answer.

### Writes from a person's own account

Writes still stop for a human, so *find half an hour with Priya on Thursday and book it* reads
`freeBusy` freely — the proxy knows that one path is a read despite answering to `POST`, because
a Confirm card people press in order to *look* at something teaches them to press Confirm without
reading — and then shows the event it means to create with Confirm and Cancel under it, answering
with the Meet link once it has run. Name the hour yourself and it skips the looking: *set up a
call at 7am with everyone on this thread* goes straight to the same card, with the thread's own
people as the attendees. A name rather than an address — *Orle from ClearOne* — is a contacts
search first. An approver may press Confirm on somebody else's write; it still runs out of the
asker's account, never the approver's.

## How the proxy works

The model sees `http_request` (and the packs), never the secret. Every call goes through an
in-process proxy that:

1. Resolves what the channel may reach from its scopes and bundles (cached for a minute,
   invalidated on any config change).
2. Matches the URL against a connection's allowed hosts, path prefixes, and methods, or a
   credential-less domain. Anything else is refused with a reason the model can act on. A host
   matches exactly, or `*.example.com` matches any subdomain of `example.com` but not
   `example.com` itself. A path prefix is compared as a literal string — there is no wildcard —
   and a method without regard to case; an empty list of either allows everything. A path with a
   `.` or `..` segment is refused rather than resolved, because the path that was checked has to
   be the path that is sent. Two connections may share a host — Drive and Calendar are both
   `www.googleapis.com` — so a rule that matches the host but not the path or the method is
   passed over rather than treated as the answer. Where several still match, the longest matching
   prefix wins (a trailing slash does not count), then the asker's own account over a shared
   credential, then the narrowest grant; a request may name the connection it means instead. For
   somebody who has not connected their own account, a read goes to a shared credential meant for
   the same service — labelled as the shared view — and a write asks them to connect. Only public
   addresses on ports 80 and 443 are reachable, and the address is checked again when the
   connection is dialled.
3. Injects the credential at the network edge, and only over https: a URL a connection matches
   on plain http is refused. An `Authorization`, `Cookie` or `Host` header the model wrote is
   dropped. Requests time out after 30 seconds and redirects are not followed, and a request that
   fails on the way is reported by its method, host and cause, never its URL, which for a
   query-parameter credential carries the secret.

### Writes and the Confirm card

4. Holds **writes** on a `confirm` connection and on a credential-less domain: a domain spends
   nothing of ours, but the guarantee is about changing things, not about credentials. A write is
   any method but GET, HEAD and OPTIONS, except a POST whose path shows it only reads: a last
   segment of `search`, `query`, `_search`, `_msearch`, `_count` or `_mget`, on any host; a Google
   method in the `collection:verb` spelling whose verb is `list`, `get`, `batchGet`, `batchRead`,
   `read`, `search`, `lookup`, `aggregate`, `count` or `query` (Cloud Logging's `entries:list`),
   on a `*.googleapis.com` host only; and two named paths, Calendar's `freeBusy` and HubSpot's
   `/crm/v3/objects/{type}/batch/read`. PUT, PATCH and DELETE are always writes. GraphQL is not
   recognised: a Linear or New Relic query is a POST to `/graphql`, and it waits for Confirm like
   a mutation unless an allow rule covers it or the connection's writes are `auto`. A request that
   asks the server to treat it as another verb — an `X-HTTP-Method-Override`, `X-Method-Override`
   or `X-HTTP-Method` header, or `_method` (or `_httpmethod`) in the query string or the body,
   naming a write — is refused before it is sent, because the gate reads the method.
   Reads are never held — a channel that may reach a service may look at it. The bot posts what
   it wants to do and, under that reply, a card with three buttons: **Confirm** runs it,
   **Cancel** drops it, **Something else…** drops it and hands the thread back — the person just
   says what they want in the thread, and their next message is the next turn. Replying
   `confirm` or `cancel` in the thread still works. Either way, only the person who asked or an
   approver in that workspace may answer, and it runs as the person who asked. Pending writes
   expire after five minutes. A connection marked *Allow access grants* sends its writes to a
   named approver instead ([Guardrails](security.md)). The same flow covers MCP tools that write,
   and there the tool's name decides: a change verb anywhere in it (`create`, `update`, `delete`,
   `send`…) makes it a write even beside a look verb, a look verb alone (`get`, `list`, `search`…)
   makes it a read, and a name with neither is a write. A server's `readOnlyHint` is not trusted,
   being the server's claim about itself; its `destructiveHint` is, since it only adds caution.

### What comes back to the model

5. Scrubs the connection's own credential values from the response, redacts anything that
   looks like a secret, cuts what goes back to the model at 48 KB and says so — so it narrows
   the query, pages, or moves the job into `run_js`, whose `fetch()` reads up to 10 MB and returns
   only the answer — and records the request on the Activity page's **Proxy** tab.

### Pre-approving writes with allow rules

**Allow rules** short-circuit the hold in step 4. Admins write plain sentences in the console
under Settings → Workspace (*Auto mode allow rules*: "Creating tasks in ClickUp is expected and
approved."), and on the Workspaces page for the organisation, a workspace or a channel, where
they apply on top of the rules above. Before a write is held, a strict model check asks whether a
rule clearly covers that exact action (service, kind of change, scope of effect); only then does
it run straight away, and the reply says which rule applied. Anything unclear, and every write on
a channel without rules, still waits for a human. The Configure page shows channel members what
is pre-approved.

## Remote MCP servers

A connection of type `mcp` points at a remote MCP server. Its tools are listed once per
connection (cached 10 minutes) and offered to the model with the connection name as a
prefix, but not on every call: a server like ClickUp's lists 60-odd tools, about 20k input
tokens per round. The first call carries only the connection names and a `use_connection`
tool; the model loads a connection's tools when a request needs them, and the definitions
are sent from the next round on. A message that names the connection ("create a ClickUp
task…"), or a thread that already called its tools, loads them before the first call so no
round is spent asking. Auth is a bearer token or OAuth 2.0 authorization code per the MCP spec: resource
discovery, authorization-server metadata, dynamic client registration when needed, PKCE, and
automatic refresh. Every request carries the token the connection holds at that moment, so a
renewed one is in use from the next call, and a reply over 10 MiB fails the call that asked for
it. Sign in from the console with the Connect dialog.

**Test connection** in the Connect dialog performs a real handshake against the server url
(initialize + `tools/list`) and reports the tools it found, so a wrong url or a rejected token
shows up before you save. Other connection types make the preset's check call through the
proxy and report the HTTP status.

## Repositories

The Workspaces page has a **Repositories** section on the organisation, on each workspace and on
every channel. *Connect repo* reaches GitHub one of two ways: through a GitHub App installation,
where the account's admin chose the repositories at GitHub and nothing is pasted, or with an
access token, pasted or reused from a repository already saved. It lists what the installation or
token can reach, you tick one or several (or type `owner/name` for one GitHub will not list), and
the server checks each against GitHub before storing it as a `github` connection in the
`Repositories` bundle (created on demand, GitHub tool pack on) — one connection per repository,
a pasted token sealed, an installation's token minted per call and never stored — and attaching
it to that scope on its own, so the bot gets the `github_*` tools there. A repository already
saved can be added to a scope as it is. *Default repository* picks which connected repository
the bot assumes when a question does not name one: the
organisation or a workspace sets one for everything below it, a narrower scope can pick its own,
and the system prompt tells the model both the connected repositories and the default. API, all
under `connections.manage`: `POST /api/github/repos` lists what a `token`, a saved repository's
`connection_id` or an `installation_id` can reach; `POST /api/scopes/{id}/repos` connects `repo`
or `repos` with one of those three, or attaches saved `connection_ids`; `POST /api/repos` saves
without attaching anywhere. `default_repo` is set on `PUT /api/scopes/{id}`. Each repository row
also carries a **recipe** — how the fix worker sets it up, builds it and
tests it (`recipe` on `PUT /api/connections/{id}`); leave it empty and every job works it out
from the clone, per package. What the first job found is kept on the row only to route later jobs
to the right worker image — it never decides what a later job runs. The older single `test_cmd`
still means what it always did.

## Setup links

An admin who does not hold a secret can have the person who does paste it themselves, so the
secret never passes through chat. `POST /api/bundles/{id}/setup-links` with `{preset, name}`
returns a one-time link to `/setup/<token>`, good for seven days, for a preset with a single
secret (not a custom API or an MCP server). The person pastes the secret there and the connection
is created in that bundle as `pending`, which the proxy never uses. The console shows the
connection as pending and has no control for making links or for activating one; activate it
with `PUT /api/connections/{id}` and `{"status": "active"}`. `GET /api/bundles/{id}/setup-links`
lists a bundle's links and `DELETE /api/setup-links/{token}` revokes one; all of these need
`connections.manage`.
