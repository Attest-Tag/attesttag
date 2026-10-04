# Admin console

Served by the same binary at `/admin/` (the root URL redirects there). A Next.js static export
in the Attest design system, embedded into the Go binary at build time.

| page | what you do there |
|---|---|
| Overview | spend this month against budget, turns, tool calls, documents, memories, routines |
| Workspaces | the organisation (the *all workspaces* row), each connected workspace and its channels: instructions, attached bundles and one-off connections, repositories and the default one, default model, read every message, email intake, member edits, allow rules, monthly budget; add, disconnect or remove a Slack workspace or Microsoft Teams tenant. Without `connections.view`, a scope shows its own settings but not the connections, repositories and inherited instructions and rules behind it |
| Access bundles | bundles, connections (Connect dialog with presets, test, curl preview, rotate secret, copy to another bundle), domains, skills, MCP sign-in; each repository saved through the GitHub App says *review: live*, *shadow* or *off*, and **Code review settings…** in its menu, or **Set review** for a selection, opens it under Reviews › Settings |
| Approvers | approval tiers: a name, a rank, the people who hold it, and what it may grant from — whole bundles or single connections. Changing them needs `approvers.manage` ([security](security.md)) |
| Access requests | who asked for what, who approved it and what ran; close one out with a reason (`access_requests.close`). Approving happens in Slack |
| Documents | folders (make, upload a whole one, move documents and folders between them), upload, edit plain-text documents in place, scope a document to a channel, reindex; **Google Drive** folders followed and kept in step ([setup](google.md#drive-sync)), the connection behind each shown only to holders of `connections.view`. A PDF whose text passes 32 MiB, or takes more than a minute to extract, is left out of the index |
| Memory | **Shared**: list, add, edit, delete the organisation's memories. **Yours**: your own private notes, which only you can see — no permission unlocks somebody else's |

## Pages: automation, activity and audit

| page | what you do there |
|---|---|
| Routines | list, edit (channel, schedule, prompt — full screen when it is long — which model answers, whether it replies always or only when it matters, and whether its writes ask first), run history for each, run now, enable or disable, delete. *New routine* starts on *Run without asking*; one created through the bot starts on *Ask first* ([security](security.md)) |
| Jobs | fix jobs handed to the worker: status, pull request, cost, brief, events, diff and log; cancel a running one |
| Reviews | code review of pull requests on GitHub, in three tabs: **History** (every review and try, *Start review*, and each review's findings, what it dropped and what it did not read, with *Run again*), **Settings** (the GitHub connections, their groups and repositories, with where every value is inherited from, and *Add connection*) and **Types** (review types and their rules, versions, *Try on a PR*). Needs `reviews.view`; changing anything or starting a review needs `reviews.manage`, and what posts live or spends needs `connections.manage` as well ([code review](code-review.md)) |
| Artifacts | files the bot made in threads: open in Slack, download, delete |
| Activity | tabs for **Turns**, **Tool calls**, **Proxy** (proxied requests), **Assistant** (questions put to the console assistant) and **People**; the CSV export holds the turns |
| Audit log | who did what: sign-ins and refused sign-ins (with the address), every change made in the console or through the API, approvals pressed in Slack, exports, retention sweeps. Filter by window, action, outcome and person; CSV export; `GET /v1/audit` for a collector. Needs `audit.view`, which admin holds ([security](security.md#the-audit-log)) |

## Pages: developers, playground and settings

| page | what you do there |
|---|---|
| API keys | developer keys for the `/v1` API: create (shown once, with an optional expiry date), see when each was last used, revoke ([api](api.md)) |
| API reference | every `/v1` endpoint, with the curl to copy and the response to expect |
| MCP | the address for MCP clients such as Claude and Cursor, how to connect one with OAuth or a key, the apps people have connected (disconnect them here), and each tool with the permission it needs ([mcp](mcp.md)) |
| Get started | recorded walkthroughs of the product, chapter by chapter |
| Playground | ask a channel something from the console and see what its settings do: a real turn with the channel's own access, model and budget, reporting the tools it called, the rounds it used and what it cost. Nothing that changes data is sent — not even a write an allow rule covers or one through a connection whose writes are `auto`, which the channel would run without asking — and the reply shows what would have been sent and what the channel would do with it. It remembers nothing, makes no routine and posts nothing in the channel. Needs `scopes.manage` |
| Settings | nine tabs: **General** (your name, address, account), **Security** (your password, two-factor, connected Slack and, where offered, Microsoft; and for holders of `settings.manage`, which sign-in methods the organisation accepts, single sign-on, whether two-factor is required, the email domains the bot answers, and whether guests and Slack Connect members may use it), **Workspace** (organisation name, behaviour, allow rules, self-approval for testing, alerts, how long activity and the audit log are kept, and deleting the account), **Models** (*Your model key*, where the deployment offers one: folded until opened, or a locked *Enterprise plan* line where the plan does not allow it, with its *Fix jobs* and *Code reviews* switches; models and limits, and code review's budget where there is no Billing tab), **Web** (web search), **Workers** (fix worker), **Billing** (plan and size, credit against the month's spend, top-ups, the card in Stripe's portal, the workspace's own monthly budget, and code review's monthly budget and daily cap — only on a deployment that sells plans), **Users**, **Roles** |

## Reviews: History, Settings and Types

**Reviews** (Automation › Reviews) is where [code review](code-review.md) is set up and read.

- **History** lists every review and try, newest first: the pull request, the score, the findings,
  the cost, and whether it was posted, recorded in shadow, skipped or failed. A review opens to its
  findings and how each stands now, the candidates it dropped and why, the files it did not read,
  and the branch rule and types it ran under. **Run again** and **Run with other types…** review the
  pull request's head as it is now. **Start review** picks a repository, one of its open pull
  requests, the review types, *Live* or *Shadow*, the whole pull request or only the changes since
  the last review, and shows the estimated cost before it starts.
- **Settings** is laid out like Workspaces: the GitHub connections in a tree — each connection,
  then its groups, then its repositories — with **Add connection** at the bottom, and the selected
  level's settings beside it. A connection's panel opens with its installation's status: who
  installed it, whether it is active, when GitHub last delivered, and whether new permissions wait
  to be accepted. Every field below the connection says where its value is inherited from, or that
  it is set here, with **Reset**; a repository's panel ends with the **Effective** line, **Review a
  PR…**, and its open pull requests that have not been reviewed yet.
- **Types** lists the built-in, edited and custom review types and edits one: its purpose, the
  files it looks at, its strictness, the [skills](code-review.md#skills) it follows — folders in
  GitHub repositories, each checked as it is linked — and its rules with their severity caps and
  switches, including rules proposed from replies in a thread, which do nothing until somebody
  accepts them. **Try on a
  PR** runs the type as edited before it is saved; every save is a version, which **History** lists
  and can revert to, and an edited built-in has **Reset to built-in**.

## Model and timezone dropdowns

**Choosing models.** Every model field (Settings and a scope's default model) is a searchable
dropdown of what the model endpoint serves — the organisation's own, when it brought a key —
fetched from the OpenAI-compatible `GET /models` (plus OpenRouter's `GET /embeddings/models` for
the embedding field) via `GET /api/models`, cached for ten minutes (`?refresh=1` refetches, up to
six times in ten minutes per organisation). OpenRouter entries show the display name, context
window and price per million tokens; a plain OpenAI-style endpoint shows ids only. An id the
list does not carry can still be typed in. Every model call asks for
at most 32,768 output tokens, which no real answer or round of tool calls needs and which turns
a runaway generation into a bounded cost; a provider clamps it to a smaller model's own limit.
The timezone fields (Settings, and a routine's own zone) are the same kind of dropdown over the
browser's IANA zone list, showing each zone's current UTC offset.

## The console assistant

**Console assistant.** A panel in the console answers questions about the console, the
organisation's own data and this guide, and stages changes the person confirms by hand; it
writes nothing itself ([security](security.md)). Anyone who can change channels, approvers or code
review (`scopes.manage`, `approvers.manage`, or `reviews.manage` with `reviews.view`) may use it,
and each thing it can read or propose is gated by the permission the console's own route for it
needs. It takes 60
questions a person an hour and three at a time per organisation, with six rounds of tool calls
each. On the deployment's model key, the model it answers on can only be the default, the
advanced one or one the organisation offers its channels; on the organisation's own key, anything
that key's endpoint serves. Its turns are on Activity's **Assistant** tab, and its tool calls on
**Tool calls**, where anyone with `activity.view` sees what ran and how it went. A call's
arguments, like the question and the answer, are shown only to somebody who also holds
`audit.view` and the permission the call was about; its results, to nobody. A card is confirmed
only in the organisation it was proposed in: after a switch to another, from any tab, Confirm says
so and saves nothing.

**On Reviews.** The chip above the question names what the page has open — a review type on
**Types**, a connection, group or repository on **Settings** — and that is the one the assistant
takes "this" to mean. There, and only there, it reads the review types rule by rule and the
settings tree level by level, and for somebody holding `reviews.view` and `reviews.manage` it
proposes changes to a type's rules, a new type, or changes to one level's branch rules
([how](code-review.md#changing-types-and-rules-from-the-assistant)). Each card is checked against
what it was read from: the type's version, or the level's branch rules, inherited ones included.
If somebody saved either before Confirm, nothing is written and the card says *Changed since this
was proposed — ask again*.

## Signing in to the console

**Signing in.** Four ways, and they are separate credentials rather than four doors to one
([security](security.md) has why):

1. **An email and a password.** Sign up, name your organisation, and you are its admin — where
   the deployment takes sign-ups at all (`SIGNUP_MODE`, [configuration](configuration.md); by
   default only the first). Anyone else joins by invitation. Passwords are bcrypt at cost 10, at
   least ten and at most 72 bytes — bcrypt refuses more rather than truncating, so the limit is
   checked in words before hashing, and both ends are counted in bytes because emoji reach the
   top in eighteen characters. A predictable password is refused whatever its length: a common
   one, decorated or not, a run along the keyboard, a shorter one written out twice, or one built
   from the account's own address, name or organisation.
2. **Sign in with Slack** (OpenID Connect). To turn it on,
   copy the app's client id and secret (Basic Information → App Credentials) into
   `SLACK_CLIENT_ID` and `SLACK_CLIENT_SECRET`, and register
   `<public origin>/api/auth/callback` under OAuth & Permissions → Redirect URLs. The public
   origin is `ADMIN_BASE_URL`, or, unset, the first address other than loopback that the console
   was signed in on (only the hosts in `PUBLIC_ORIGIN_HOSTS`, when that is set); until then it is
   the listen address, which works only on the bot's own machine. A sign-in started on any other
   address is sent to the public origin first, so it comes back to the host that holds its state
   cookie. Slack only accepts https redirect URLs: for a local bot behind a tunnel such as
   `tailscale serve --bg 8090`, set `ADMIN_BASE_URL` to the tunnel's https address (`make run`
   sets it to `http://127.0.0.1:8090`, which Slack will not accept), and register both the local
   and the deployed callback on the app. A refused sign-in lands back on the login card with the
   reason, such as: the organisation does not accept Slack, the address already has an account
   (sign in the usual way and connect Slack from Settings), an invitation meant for somebody else
   or limited to a domain, or sign-ups closed.

### Microsoft and single sign-on

3. **Sign in with Microsoft**, where `MSTEAMS_SIGNIN` is set
   ([msteams.md](msteams.md#signing-in-to-the-console-with-microsoft)).
4. **Single sign-on** through the organisation's own OpenID Connect provider, set up under
   Settings → Security → Single sign-on, which shows the redirect URI to register with the
   provider. Type your work address on the login card and choose single sign-on; started on any
   address but the public origin, it answers with where to sign in instead.

## Sessions, bearer tokens and scripts

Sessions are opaque tokens in an HttpOnly cookie, and the same token works as a bearer for
curl and scripts, so a one-off script acts as a real person in a real organisation and its reads
are scoped like theirs. For anything that has to keep working, mint an API key instead
([api.md](api.md)): a session expires after a week, a key lasts until it is revoked or reaches
the expiry date it was given, if it was given one, and a key can be revoked on its own without
signing anybody out.

```bash
SESSION=$(curl -s -c - -X POST http://127.0.0.1:8080/api/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"…"}' | awk '/attest_admin/{print $7}')
curl -s -H "Authorization: Bearer $SESSION" http://127.0.0.1:8080/api/overview
```

## Configure page for channel members

**Member Configure page.** Every channel reply's footer links to `/configure/<workspace>/<channel>`.
That footer link is **shared** — everyone who can read the reply holds it, which in a Slack Connect
channel includes another company's members — so it only **shows** the settings (model, channel
instructions, tools, memory, routines) and never reveals which services the channel can reach. To
**change** anything a member sends `!configure` in the channel and the bot DMs them a **personal**
link, signed for that one person; that link can toggle read-every-message, turn email intake off,
choose a model, set the tool-round budget, write channel instructions, edit or delete the channel's
memories, and enable or disable routines. Both links carry a token signed for the workspace and
channel under a key derived from `MASTER_KEY`; the personal one also names its holder inside the MAC.
The footer link is withheld entirely in an externally shared (Slack Connect) channel. Links expire a
day after the reply that printed them, and `POST /api/scopes/{id}/configure-links/revoke`
(`scopes.manage`) voids every link printed so far — the console has no button for that. Admins can
lock even the personal link per scope with `member_edits = block`, leaving edits to the console.
