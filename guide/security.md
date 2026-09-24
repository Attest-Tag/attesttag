# Guardrails

The bot reads web pages, documents and the output of other people's APIs, and then decides
what to do next. Everything here exists because one of those is eventually hostile.

## Tool output, credentials and SSRF

- **Tool output is data.** Every tool result is wrapped in `<tool_result>` blocks and the
  system prompt tells the model never to follow instructions found in tool output, web pages,
  or documents. Fake tool-result tags in model output are stripped.
- **No credentials in the model.** Secrets live sealed in the database (AES-256-GCM under
  `MASTER_KEY`) and are injected by the proxy. Responses are scrubbed of the credential
  values and of anything matching secret patterns before they reach the model or Slack. A
  credential is only ever sent over https, an `Authorization`, `Cookie` or `Host` header the
  model writes is dropped, and a request that fails on the way is reported by its method, host
  and cause, never by its URL — which, for a credential sent as a query parameter, carries it.
- **SSRF guard.** Web fetches refuse non-HTTP schemes, ports other than 80 and 443, and private,
  loopback, and link-local hosts, and the address is checked again when the connection is
  dialled. The proxy only reaches allow-listed hosts, under the same rules.

## Confirming writes

- **Human-in-the-loop writes.** Write requests through the proxy wait for a Confirm button (or
  a `confirm` reply) in the thread; reads are never held, unless a connection is set to `all`.
  A write to a credential-less domain waits too: it spends nothing of ours, but the guarantee is
  about changing things, not about credentials. What counts as a write is set out in
  [connections.md](connections.md#how-the-proxy-works); a request that asks the server to treat
  it as another verb (`X-HTTP-Method-Override` and its cousins, or a `_method` field) is refused
  before it is sent, because the gate reads the method. Only the person who asked, or somebody
  holding an approval role in that workspace, may confirm or cancel it, by button or by reply, and
  the write runs as the person who asked. Whether an MCP tool counts as a write is decided from
  its name, not from the server's own `readOnlyHint`: that is a remote server's claim about
  itself, and trusting it would let one skip the Confirm card by setting a flag. An admin who does
  trust a server sets that connection's writes to `auto`.
- **Unattended writes are a console decision.** A routine confirms its own writes only if someone
  holding `routines.manage` set it to *Run without asking* in the console; otherwise they are held
  like any turn's, unless an allow rule covers them or their connection's writes are `auto`. A
  routine created through the bot always starts on *Ask first*: the request to make one can come
  from something the model read — a document, a web page, a forwarded mail — and a routine that
  writes without asking and stays quiet is exactly what a planted instruction would want. One made
  with *New routine* in the console starts on *Run without asking*, in front of the person
  making it.

## Forwarded email

- **Forwarded mail acts under a second key.** A turn a forwarded email starts holds every write,
  because nobody in the workspace wrote the mail and the rule checker is never shown who proposed
  an action. A channel can lift that for the rules it has written — *allow rules on forwarded
  email*, off everywhere by default — and what it lifts is deliberately narrow: the checker sees
  the destination of the write and not the body, which on that lane is composed from a stranger's
  mail; a fix job is never eligible whatever the setting says; the turn gets three such writes and
  no more; and each one is recorded as `write.auto_ran` with the rule that allowed it.
- **Email intake is a lane, not a member.** When a channel takes forwarded mail
  ([Slack](slack.md)), the turn it starts is attributed to a synthetic requester — `email:<channel>`,
  which is not shaped like a Slack id — and carries none of a member's privileges: no personal
  credential (the proxy's per-person path finds no grant and fails closed), no private notes, no
  bang commands, and no way to press its own Confirm by writing the word in a mail body. The text
  of a mail is labelled in the prompt as something an app posted rather than something a
  colleague said, no allow rule can pre-approve a write it proposes unless the channel turned on
  *allow rules on forwarded email* (above), and every other write goes to a named approver as a
  request that expires in a week. Slack passes on no SPF, DKIM or
  DMARC result, so a `From:` line on the approval card is a claim the sender made about
  themselves and is labelled as one.

## Access requests and approval tiers

- **Access requests.** Some writes are not the requester's to confirm. A connection an admin marks
  *Allow access grants* raises an access request instead of an in-thread Confirm card: the calls
  are recorded, an approver gets them as a DM with Approve and Deny, and only then do exactly those
  calls run — in order, stopping at the first failure, with nothing rolled back and nothing
  retried. Such a connection must list its methods and path prefixes, and cannot have its writes
  set to `auto`, so what it can hand out is a list of paths rather than a whole host, and never
  without an approver. The gate lives in the proxy rather than in the tool that asks, because
  otherwise the same write composed through plain `http_request` would land a card in the
  requester's own thread for the requester to press. No allow rule can pre-approve a grant. Nobody
  approves their own request — enforced three times over, once in SQL — unless an admin turns on
  the self-approval switch for testing, which marks every such decision as one and says so in the
  thread. What the approver reads is rendered from the stored payload, never from what the model
  said about it, and the requester's own words are shown escaped and labelled as theirs. Approving
  is a Slack act only: the console lists requests and can close one out, but cannot grant.
- **Approval tiers.** An approver is not a name on a list: they hold a *role*, and the role lists
  what its members may grant from — whole bundles, or single connections picked on their own, the
  same two shapes a channel's access takes — so "Approver" and "Super admin" are two tiers with
  different reach, set up under **Approvers** in the console. Rank orders them, and a higher tier
  covers everything a lower one can. A request is routed to the least powerful tier whose grants
  cover **every** call in it; tagging someone picks between tiers that can all do the job, and
  escalates when the person tagged cannot. A plan no single tier covers is refused rather than
  split, so one approval is always one decision. Execution runs under the tier's own grants — not
  the approver's own reach and not the channel's connections — so what runs is what the card
  described, even when a super admin answers a request meant for a plain approver.

## Console members, invitations and roles

- **Who may open the console.** A membership opens it, and there are four ways to one: founding
  an organisation, an invitation, a share link, and single sign-on (below). Being a Slack
  workspace admin grants nothing here; it decides only who may connect that workspace. An
  invitation names a Slack user (the bot sends the link by direct message) or an address (it is
  mailed; with no mail provider you get the link to pass on), and it is single-use and lasts seven
  days. A share link names nobody and carries its own terms: a role, a lifetime (seven days unless
  set, thirty at most), a number of uses (up to 500, or unlimited) and, optionally, an email
  domain. A domain alone proves nothing, because whoever redeems the link types the address. A
  password sign-up through one joins when its confirmation mail is answered and spends a use
  then, so a link withdrawn, expired or used up by then admits nobody; a Slack or Microsoft
  sign-up through one is refused. An existing account joins through one only with its address
  proved by our mail, by single sign-on at that domain or by an invitation from the same
  organisation — not by a Slack or Microsoft claim, nor an invitation from elsewhere, which
  anybody can arrange. Only a link's SHA-256 is stored, so a dump of the table cannot be
  replayed into an account. Under **Settings → Users**.
- **Console roles.** Three built-in roles — *admin*, *editor*, *viewer* — defined in code rather
  than copied into each install, so a permission added in a later release actually reaches them.
  Custom roles are rows. Two rules do the real work: you may only grant a role whose access you
  hold yourself (checked against both the role being given *and* the one being taken away, or an
  editor could demote an admin), and a change that would leave nobody able to manage users or
  credentials is refused — on the capability, not on counting people called "admin". Knowing
  which credentials exist is itself `connections.view`: without it a member still sees a scope's
  settings and the Drive syncs, but not the connections, repositories, and inherited instructions
  and rules behind them. Anything that needs `users.manage` or `api_keys.manage` also waits for a
  confirmed email address where the deployment sends mail, because an invitation lands in a
  stranger's inbox and a key lets a script act with nobody signed in.

## Two-factor and sign-in methods

- **Two-factor.** A time-based code (RFC 6238, six digits, thirty seconds) from an authenticator
  app, asked for after any sign-in — a password, Slack, Microsoft or single sign-on — once the
  account has one. The secret is sealed with the master key, an accepted code's thirty-second
  window is remembered so the same code cannot be spent twice, and ten single-use recovery
  codes are shown once at enrolment. An organisation can require it (**Settings →
  Security**): every session without one, however it signed in, is held at an enrolment screen,
  and the requirement can only be turned on by an admin who already holds a factor. Slack is no
  exemption, or anybody could sign out and come back in through Slack without a factor. An
  invitation followed on the way in is redeemed only once the code is in.
- **Sign-in methods.** An organisation chooses which credentials open its console: any of them
  (the default), email and password only, Slack only, Microsoft only (offered where Sign in with
  Microsoft is set up), or single sign-on only (once its provider's domain is verified). It is
  checked on every request rather than at sign-in, so a live session made the wrong way stops
  working when the policy changes; once no organisation a person belongs to accepts passwords,
  password resets stop being sent to them as well. Switching organisations obeys the
  destination's policy, and the setting is refused unless whoever sets it already satisfies it —
  the one control here that could lock its own author out.
- **No environment account.** There is no `ADMIN_USER`, `ADMIN_PASSWORD` or `ADMIN_TOKEN`.
  Authority is a membership joining a person to an organisation, and nothing set on the process
  outranks it — a static credential that does is exactly what a multi-tenant console must not have.

## Single sign-on and identity linking

- **Single sign-on.** An organisation can sign its people in through its own OpenID Connect
  provider (**Settings → Security → Single sign-on**). Nothing opens until the domain is proved
  with a DNS TXT record at `_attest-tag-verification.<domain>`, and only addresses at that domain
  are accepted, so one organisation's identity provider cannot vouch for anybody at another. The
  sign-in uses PKCE, and the ID token, fetched from the provider's own token endpoint over TLS
  rather than verified by signature (as OpenID Connect allows), is checked for its issuer,
  audience and nonce; a userinfo answer must name the same subject. The organisation's sign-in
  policy is asked before the code is exchanged, so where it does not accept single sign-on a
  sign-in leaves no account, membership or connected identity behind. Somebody new is provisioned
  as a viewer. An existing account at that address is linked only if it is already a member of
  the organisation — the one place an address joins two sign-ins, and only inside a domain the
  organisation proved — and a member who was removed is refused rather than provisioned again.
  The client secret is sealed like any other credential.
- **Identity linking.** An email address is a delivery address and a label, never a join key —
  single sign-on, above, is the one exception, and only inside a domain the organisation proved.
  A Slack sign-in is matched to the Slack identity it proves and to nothing else; it will not adopt
  an account that merely shares an address, and a password sign-up will not adopt a Slack-created
  one. This matters because the address in Slack's OIDC response is set by the workspace's own
  admin (and by its SAML IdP), so trusting it would let any workspace admin sign in as anybody.
  Connecting a second sign-in method is done from inside an already-authenticated session, in
  Settings, where the person has proved they hold both. Sign in with Microsoft is held to the same
  rule for a sharper reason: an Entra tenant's own admin can put any address in a token its users
  sign in with — the takeover known as nOAuth — so a Microsoft sign-in is keyed by its tenant and
  object id alone, and an address it carries starts unconfirmed.

## CSRF and login throttling

- **CSRF.** State-changing console requests carry a double-submit token: a readable cookie the page
  echoes in `X-CSRF-Token`. `SameSite=Lax` does not stop a cross-site POST arriving as a top-level
  navigation, and several of these routes spend real credentials. Bearer-authenticated calls are
  exempt — they carry no cookie, so there is nothing to ride. The posts that make or clear a
  session sit outside that token — sign-in, sign-up, sign-out, email verification, the reset
  request and the reset itself, and the start of single sign-on — so they refuse a request the
  browser marks cross-site or same-site, or whose `Origin` names another host; otherwise a page
  elsewhere could sign a visitor into the attacker's account. A caller that sends neither header,
  such as curl, is unaffected.
- **Console login throttling.** Eight failed password sign-ins from one client address within
  fifteen minutes lock that address out for fifteen minutes. Failures against one account only
  slow each further guess at it by two seconds, so nobody can lock its owner out by guessing.
  Each instance keeps its own count. Sign in with Slack is unaffected, so somebody with a linked
  Slack account always has another way in. Sign-in and the reset form both answer identically
  whether or not an address has an account, so neither is a way of finding out who has one.
  Sign-up does say when an address is taken (a 409), because the alternative leaves a person who
  forgot they had signed up waiting for an account that was never made; it is throttled on the
  address as well as the caller for that reason.

## Developer keys and MCP clients

- **A key is its maker.** A developer key acts as the person who made it, re-resolved on every
  request, so a smaller role or a removal binds its next call. It skips the sign-in policy and
  two-factor, which belong to somebody at a keyboard; its expiry and the revoke button bound it
  instead. Only its sha-256 is stored ([api](api.md)).
- **An MCP client connected with OAuth is a key made in a browser.** Approving one needs what
  minting a key needs, `api_keys.manage` and a confirmed address, on a consent page behind the
  console session, so the sign-in policy and two-factor stand in front of every approval. The page
  shows the address the app returns to, the one part an impostor cannot choose ([mcp](mcp.md)).
- **Its tokens open less than a key does.** They work at `/mcp` only. An access token lasts an hour.
  A refresh token changes every time it is used, and presenting a spent one ends the connection,
  since somebody else holds a copy. An authorization code needs PKCE (S256), lasts ten minutes and
  works once; used a second time, it ends what its first use made.
- **A tool can do no more than its route.** Every MCP tool runs the `/v1` route of the same name
  in-process, permission check and audit row included. No route reaches a connection's
  credentials or anybody's private notes, so no tool does either.
- **A routine made through either runs as the bot.** No key or MCP token is anybody's Slack
  account, so none can lend a routine someone's personal connections. Its writes start on *Ask
  first*, and only the console can switch that off.
- **Removal ends them.** Removing a member, or a password reset or change, revokes their keys and
  ends their MCP connections.

## Who may use the bot in Slack and Teams

- **Who may use it.** Each organisation keeps its own list of email domains the bot answers
  (**Settings → Security → Email domains**), which starts as its founder's sign-up domain unless
  that is a public mail provider such as gmail.com. A list limits the bot to accounts whose Slack
  email is on those domains, so guests, Slack Connect members and shared channels cannot reach
  the channel's connections through a mention. It gates button presses as well as messages, and
  refuses accounts whose email Slack will not show, so it needs the `users:read.email` bot scope.
  `ALLOWED_EMAIL_DOMAINS` (for example `example.com`) is only the default for an organisation
  that has stored no list. With no list at all, any member of the workspace can use the bot — but
  guests and external (Slack Connect / other-tenant) accounts are refused either way, unless the
  organisation turns on *Guests and Slack Connect* (`allow_external_users`).
- **Reads stay inside the asker's Slack.** The bot reads with its own token and is in channels
  the asker is not, so `read_channel_history`, `read_thread` and `list_pins` check the asking
  user's membership before touching any conversation that is not the current one and not a
  public channel. Privacy comes from `conversations.info`, not from the channel id: a public
  channel converted to private keeps its `C` prefix.
- **Microsoft Teams deliveries are proved before they are read.** Every activity on
  `/msteams/messages` carries a Bot Framework token, checked against Microsoft's published keys
  for this bot, for the channel and service URL the activity names, with a key Microsoft endorses
  for Teams. Replies go only to Bot Framework hosts, whatever an activity says; the bot's token is
  sent to Microsoft's attachment hosts and nowhere else, and a OneDrive download link is fetched
  with no credential at all ([msteams.md](msteams.md)).

## Budgets, model provider and own model key

- **Budgets and rate limits** ([slack.md](slack.md#limits-and-alerts)), with alerts. Every model
  call also asks for at most 32,768 output tokens, so a runaway generation is a bounded cost.
- **Provider policy.** With OpenRouter, `DENY_TRAINING=1` (the default) routes only to
  providers that do not train on prompts.
- **An organisation's own model key.** Sealed under `MASTER_KEY` like every other credential and
  never read back: the console, the audit log, mails and the operator are told the host, the last
  four characters and a fingerprint. Its address must be public and https, is checked again at
  dial time on every call, and follows no redirect; the stored key is only ever sent to the address
  it was saved for, so neither a save nor the Test button can carry it somewhere new without it
  being pasted again. A new key or address, or removing the key, needs `settings.manage` and proof
  of identity, is audited, and is mailed to the organisation's admins. A key that cannot be used stops that
  organisation's model calls; nothing falls back to the deployment's key.

## Console assistant, documents and fix jobs

- **The console assistant writes nothing.** A change it proposes is a card, and Confirm has the
  browser make the same request the page would, under the person's own permissions, CSRF token
  and audit row; what it can read is what that person's own console routes would show. On the
  deployment's model key it answers only on the default model, the advanced one and the models
  the organisation offers its channels, so a member cannot point the shared key at the dearest
  model in the catalogue. It takes 60 questions a person an hour, three at a time per
  organisation.
- **Documents stay inside their folder.** A document path that climbs out of the organisation's
  folder or names a hidden file is refused with a 400, from the console and the API alike. Text
  is taken from a PDF with a one-minute limit and a 32 MiB cap, and `pdftotext` is stopped the
  moment it passes either, so a small file that expands into gigabytes cannot run the shared
  instance out of memory; that PDF is left out of the index.
- **Fix jobs.** The repository's code, and every git command after the clone, run in the worker
  as an unprivileged sandbox user ([fix-jobs.md](fix-jobs.md)).
- **Audit.** Every turn, tool call, proxied request, and completion (with tokens and cost) is
  stored and visible on the Activity page, to anyone holding `activity.view` — every built-in
  role. So two kinds of tool call are recorded without their arguments or result, which are
  withheld from everyone, admins included: a person's own notes, and any call that spent
  somebody's own connection, such as their Gmail — an `http_request`, or a `run_js` script whose
  `fetch()` went through it, which the sandbox reports as it runs, since nothing before the script
  says what it will fetch. The row keeps its name, outcome and duration and whose it was. An
  `http_request` also keeps the connection, the method, the endpoint without its query string, the
  status and the size of the reply, or the first sentence of why nothing came back; a script keeps
  the connection and the size of its answer; notes keep only their size. The repeat guard's
  refusal of such a call is recorded the same way, and rows written before any of this was kept
  show a bare `(private)`. What the people did is a separate record, below.

## The audit log

The Activity page is what the bot did. The Audit log page (Insight → Audit log) is what the people
did, and it is the record a security review, a compliance questionnaire or a departing employee's
manager actually asks for. One append-only table, `audit_log`, one row per event, each saying who
(account id, email, name, or the Slack user for an act in Slack), what, from which address and
browser, through which door (console, API key, MCP client, Slack, the operator, or the process
itself), and what came of it (`ok`, `denied`, `failed`, or `refused` for an attempt to connect a
second sign-in method from a session more than ten minutes old), with a JSON `details` column for
whatever the event has to add — never a secret, a token, a password or a raw key.

### What the audit log records

What is recorded:

- **Sign-ins**, however made — password, Slack, Microsoft, single sign-on, sign-up, the second
  factor — and **refused sign-ins**: a wrong password, a wrong code, a lockout, a credential the
  organisation's policy does not accept. A refusal is attributed to the account the address
  names and written into every organisation that account belongs to; an address that names
  nobody is recorded nowhere, which is the same answer the sign-in form gives, so the log is
  not a way of finding out who has an account. Sign-outs, organisation switches, password
  changes and resets, reset requests, email verification, two-factor enrolment and removal,
  recovery-code reissues.
- **Every write through the console, the `/v1` API or the MCP server**, as the named event when the
  handler knows what it did — `connection.updated` with whether the secret was rotated,
  `member.role_changed` with both ends of the change, `settings.updated` with the keys and their new
  values — and as a plain `console.request` naming the route and the status otherwise. The plain row
  is the floor: it is written by the same middleware every authenticated route passes through, so a
  write cannot go unrecorded because a handler forgot. A person's own private notes are the one
  exception; the organisation does not get a line saying when those were edited. A write refused
  with 403 once it reaches its route — a permission the person lacks, an address they have not
  confirmed — is recorded as `denied`, by the person who was refused. Reads are not recorded,
  refused or not, and neither is a write stopped before it reaches its route: a missing CSRF token,
  a sign-in method the organisation no longer accepts, or a session held for two-factor enrolment.
- **Acts in Slack** that spend a credential on somebody's say-so: a Confirm pressed or typed
  (`write.confirmed`, with the requester and the approver, who are different people when a
  tier is involved), a cancel, pressed or typed, and an access request approved or denied — with
  whether it was self-approved.
- **Workspaces** connected and disconnected, **exports** of the activity table and of the
  audit log itself, the **operator** moving the account between plans, and every
  **retention sweep**, so "rows were deleted on this date, by policy" sits in the record
  beside the policy.

### Reading, exporting and retaining the audit log

Reading it needs `audit.view`, which among the built-in roles only **admin** holds: the log
carries addresses and sign-in history, which is more than "what the bot did", and a viewer is
not owed that. The page filters by window, action (one, or a family such as `auth.`), outcome,
person and free text, exports CSV with the same filters, and `GET /v1/audit` serves a
collector: `?after=<id>` walks forward from the last row it saw, oldest first, and `last_id` is
the mark to keep.

Nothing edits a row. Two things delete them: the organisation's own `audit_retention_days`
(off by default, floor 30 days, and deliberately not `data_retention_days`, so shortening the
bot's history never shortens the record of who did it), and the deletion of the organisation. A
failed insert is logged loudly and does not fail the request that caused it — an outage in the
name of a record nobody could then read either would be the wrong trade.
