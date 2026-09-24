# Developer API

`/v1`, authenticated with a key rather than a session. It is a narrow surface over what the
console already does — the knowledge the bot answers from, what it produced, and what it spent —
rather than a second way to configure the product. Connection credentials are the line: no
permission reaches them, because the point of holding them is that they leave through the proxy
and nowhere else.

The same routes are tools for Claude, Cursor and other MCP clients, connected with OAuth or a key:
see [MCP server](mcp.md).

**A key is a person, not a role.** It carries the authority of whoever minted it and never a
scrap more: the same membership, the same console role, the same permissions, re-resolved on
every request. An integration therefore cannot reach further than the person who set it up, and
when they leave the organisation their scripts stop with them rather than outliving their access.
Two things a session has that a key deliberately does not are the organisation's sign-in policy
and its two-factor requirement — both are properties of somebody sitting at a keyboard, and a
nightly job has neither. What bounds a key instead is its own expiry, if it was given one when it
was made, and the revoke button.

Keys are `atk1.<32 random bytes>`; only the sha-256 is stored, so a copy of the database is not a
set of working keys, and the raw key exists exactly once — in the response that created it. Every
key is capped at 240 requests a minute (per process; several Cloud Run instances multiply that).

```bash
export BASE=https://your-console-host
export ATTESTTAG_KEY=atk1.…                     # Developer → API keys → Create key

curl $BASE/v1/whoami -H "Authorization: Bearer $ATTESTTAG_KEY"

# Keep what the bot reads in step with somewhere else. The write is stored immediately;
# indexing follows in the background.
curl $BASE/v1/documents/runbooks/deploys.md \
  -X PUT \
  -H "Authorization: Bearer $ATTESTTAG_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"content": "# Deploys\n\nRun make deploy from main.\n"}'

# A PDF, or several files at once (repeat files=), uploaded into a folder.
curl $BASE/v1/documents \
  -H "Authorization: Bearer $ATTESTTAG_KEY" \
  -F folder=policies -F files=@handbook.pdf
```

## Endpoints and the permissions they need

"A key" means any working key; a permission means its owner's role must hold that too.

### The key, the workspaces and the channels

| endpoint | what it does | needs |
|---|---|---|
| `GET /v1/whoami` | the organisation, the account and the permissions this key holds | a key |
| `GET /v1/workspaces` | connected workspaces: Slack workspaces and Microsoft Teams tenants | a key |
| `GET /v1/scopes` | organisation, workspace and channel settings | a key |

### Documents and memory

| endpoint | what it does | needs |
|---|---|---|
| `GET /v1/documents` | what the bot can search | a key |
| `POST /v1/documents` | upload files, PDFs included, then re-index | `documents.manage` |
| `GET /v1/documents/{path}` | one text document's contents | a key |
| `PUT /v1/documents/{path}` | write a document's `content`, set its channel `scope`, or both, then re-index | `documents.manage` |
| `DELETE /v1/documents/{path}` | remove a document | `documents.manage` |
| `GET /v1/memories` | facts kept per workspace and channel | a key |
| `POST /v1/memories` | remember something | `memory.manage` |
| `PUT /v1/memories/{id}` | change what a memory says | `memory.manage` |
| `DELETE /v1/memories/{id}` | forget something | `memory.manage` |

`PUT` takes text in JSON; a PDF goes through `POST`. An upload is the multipart form the console's
Upload button sends: one `files` part per file, `folder` for where the batch lands, `paths` (one per
file, in order) when a file should keep a path of its own, and `scope` for the channel it is limited
to. The bot reads `.csv .htm .html .json .markdown .md .pdf .rst .txt`; one file of another type
refuses the whole batch before anything is stored. A request may be 32 MB; a file already at a
path is replaced. The answer is `201 {"ok": true, "saved": [paths], "indexing": true}`.

### What the bot made and ran

| endpoint | what it does | needs |
|---|---|---|
| `GET /v1/artifacts`, `GET /v1/artifacts/{id}` | files the bot made; the second carries the body | `artifacts.view` |
| `GET /v1/routines` | scheduled prompts | a key |
| `GET /v1/routines/{id}/runs` | one routine's run history, quiet runs included | a key |
| `POST /v1/routines/{id}/run` | run one now (202; the reply lands in its channel) | `routines.manage` |
| `POST /v1/routines` | make one: `channel` (its id or `#name`), `cron`, `prompt`, and optionally `timezone`, `notify`, `notify_when`, `model` | `routines.manage` |
| `PUT /v1/routines/{id}` | change what is sent; `enabled: false` pauses it | `routines.manage` |
| `DELETE /v1/routines/{id}` | delete it and its run history | `routines.manage` |
| `GET /v1/jobs`, `GET /v1/jobs/{id}` | fix jobs: status, pull request, cost | `jobs.view` |
| `GET /v1/access-requests` | who asked for what, and how it was closed | `access_requests.view` |

A routine goes in the channel you name, which must be one the bot is in: `GET /v1/scopes` lists
them, and `team_id` settles a name that is in more than one workspace. A routine made here runs as
the bot, never as the person calling, so it reaches the channel's connections and nobody's own.
Its writes ask first (`auto_confirm: false`); letting them run without asking is switched on in the
console. Rewriting a routine that ran as someone takes it off them, as the console's editor does,
and the answer says whose it was in `was_running_as`.

### Activity, audit and spend

| endpoint | what it does | needs |
|---|---|---|
| `GET /v1/activity` | turns with tokens and cost, each with an `id`; pages like the audit log | `activity.view` |
| `GET /v1/audit` | the audit log: sign-ins, changes, approvals, exports, with who and from where | `audit.view` |
| `GET /v1/usage` | spend against budget, and what the bot has to work with. `paused_by` is empty while the bot can answer, then `budget` or `credit`, naming which limit stopped it — the field to poll if you want to know the moment the bot goes quiet; `credit_balance_usd` is 0 unless credit is in use | a key |
| `GET /v1/billing` | the plan, what it costs, when it renews, and the credit left ([`billing.md`](billing.md)). It never carries Stripe identifiers. Only on a deployment that sells plans: elsewhere it answers 404, like the rest of billing | a key |

`/v1/activity` and `/v1/audit` page the same way, by `id`. With no cursor the newest rows come first
and `?before=<id>` carries on back through history; `?after=<id>` walks forward from a row you have
seen, oldest first, which is what a collector wants. `last_id` is the highest id so far — on an
empty page, the mark you sent — so the next poll is always `?after=<last_id>`. Both take `?limit`
(100 by default, 500 at most) and `?range=today|7d|month`.

## Errors and who may mint keys

Every failure is `{"error": "…"}` with a status: 400 for a body that cannot be read, a field that
is missing, a document path that climbs out of the organisation's folder or names a hidden file,
or an upload with no files or a type the bot does not read; 401 for a key that is missing,
unknown, revoked, expired, or whose owner has left or been disabled; 403 when their role does not
hold the permission; 404 for a record outside the organisation — a key must not be able to tell a
record it may not have from one that does not exist; 413 for an upload over 32 MB; 415 for
reading or writing a document that is not a text type through `GET` or `PUT`; 429 past the rate
limit.
`DELETE /v1/memories/{id}` answers 200, and `GET /v1/routines/{id}/runs` an empty list, for an
id the organisation does not have, which says no more than a 404 would. A console session token
is refused here and a key is refused on `/api/*`: the two credentials open different doors, and
neither is accepted at the other's.

Minting keys needs `api_keys.manage`, held by **admin** and **editor**, and a confirmed email
address where the deployment sends mail. Granting it widens nothing on its own — a key can only
ever carry access its maker already has.
