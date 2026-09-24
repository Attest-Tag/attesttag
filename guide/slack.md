# Using it in Slack

What the bot does once it is in a channel: how a conversation works, the
tools it can reach without any configuration, and the bang commands.

## Talking to it

- **Mention** `@attest_tag` in any channel it has been invited to, or **DM** it. The DM view
  is a Slack assistant pane with suggested prompts, and each DM thread is titled from its
  first message.
- **Threads are the unit of conversation.** Follow-ups in a thread the bot is already active
  in need no mention. Slack is the source of truth: the bot rebuilds the model's context from
  the thread on every turn, so edits and deletions are noticed and recorded as notes.
- **Read every message** (off by default) lets the bot join a channel conversation without a
  mention. A cheap classifier judges each message against the channel's instructions and
  answers with a reply, an emoji reaction, or nothing at all — nothing being the usual answer.
  Set it per channel on the Configure page or in the console; a channel inherits from its
  workspace and the workspace from the account.

### Email intake

- **Email intake** (off by default) answers mail forwarded to a channel's Slack email address.
  Point a group address such as `support@acme.com` at the channel's Slack address and a
  customer's mail lands here as a thread; Slack posts it as Slackbot with the subject as the
  title and the body attached, which is why the setting is needed at all — nobody mentioned
  anyone, and there is no text for the classifier above to judge. A mail answered this way is a
  turn **nobody in the workspace started**, and it is deliberately weaker than a person's:
  - it runs on the channel's own connections, never on anyone's personal Google or GitHub account;
  - it cannot create or delete routines, save or delete memories, connect accounts, ask for
    access, DM anyone, or search or read the web;
  - no allow rule applies to it, so every write is held whatever the rules say — unless the
    channel's **allow rules on forwarded email** setting is switched on, which is off everywhere
    until an admin asks for it. With it on, a rule that covers a write runs it straight away, and
    two narrowings come with that: the rule is judged on where the write goes — the service, the
    credential and the verb — never on anything the mail itself says, and **a fix job is never
    covered**, so opening a pull request always waits for an approver. One mail gets at most
    `LIMIT_EMAIL_AUTO_WRITES_PER_TURN` (3) of them, and each leaves a `write.auto_ran` row in the
    audit log naming the rule that allowed it;
  - a held write goes to a **named approver** as a request that waits a week rather than to a
    Confirm button that lapses in five minutes. The card is DM'd to each approver *and* posted in
    the thread with Approve and Deny on it, so whoever is watching the channel can release it
    where the conversation already is; who may press is checked on the press, so a card everyone
    can see is still not a card everyone can act on. Set up an approval role first, or there will
    be nobody to ask — including for a fix job, which is approved by the role that holds the
    repository it would branch;
  - a channel takes at most `LIMIT_EMAIL_TURNS_PER_CHANNEL_PER_HOUR` (12) of them an hour.

  Admins set it in the console, per channel and inherited like "read every message". Anyone in
  the channel can switch it off from the Configure page; only an admin can switch it back on.

### Streaming, stopping and files in threads

- **Streaming.** Answers stream into the thread while a status line shows what the bot is
  doing (reading the thread, searching docs, calling a service).
- **Stopping a run.** Reply `stop` (or `cancel`, `abort`, `never mind`) in the thread while the
  bot is working, or say `!stop`. It cancels the model call and any tool in flight, closes the
  half-written reply with *Stopped by @you*, and drops anything the turn was holding for
  confirmation. What the turn spent before it was stopped is still recorded.
- **Files in threads.** Attached text files and PDFs are extracted and put in context
  (PDFs via `pdftotext`). Images go to the model when it can read them; the turn keeps its
  model either way, so a text-only model gets each image's file name instead, with a note that
  it could not see it. A forwarded email's own attachments come too: Slack hangs those off the
  mail rather than off the message, so they are read from the mail and joined to the turn's files.
  What reaches the model is bounded, and what does not is named rather than guessed at: an image
  over 4 MB, a file over 12 MB, and a type nothing can extract are each reported by name alone,
  a PDF whose text runs past 32 MiB or takes more than a minute to extract is named as unreadable,
  and extracted text is cut at 60,000 characters with the model told it is looking at the first
  part of a longer file. Only the first four files on a message are read; the model is told the
  rest were left out.
- **Evidence follows the ticket.** A ClickUp task the bot files from a thread carries that
  thread's files — the customer's screenshot, the log — uploaded to the task once it exists. It
  happens on the same approval as the task: whoever approved filing a ticket about a screenshot
  approved the screenshot going on it. The forwarded mail itself is left off, since its text is
  already the description. The card says how many files it will carry.

### Long threads, long answers and the reply footer

- **Long threads** are windowed: the most recent `history_limit` messages go to the model
  verbatim and the older part is condensed into a running summary stored on the session.
- **Long answers** (over `long_answer_chars`, default 16,000 characters; `0` never) are posted
  as a short lead plus a Markdown file, instead of a wall of text. A routine that posts every run
  also files its answer that way once it is longer than one Slack message holds, whatever the
  setting says.
- **Reply footer.** Channel replies end with a small context line, for example
  `basic · 1.2k in · 340 out · $0.0012 · Configure`. It names the tier the answer ran on —
  basic, advanced, or custom for any other model — not the model itself, and a cost that is an
  estimate rather than the provider's charge is marked `~`. The Configure link opens a
  member-facing page for that channel (see [Admin console](console.md)). DMs and routine posts
  have no footer. The line is marked, and left out when the thread is read back, so earlier
  footers never reach the model. Setting `show_cost` to `0` drops the dollars from that line —
  the tier, the tokens and the link stay, and spend is still recorded and still reported in the
  console.

## Native tools

| tool | what it does |
|---|---|
| `read_thread`, `read_channel_history` | thread and channel messages, with names resolved |
| `slack_search` | workspace search across public channels (`search:read.public`). Only on a turn somebody just started with a message: Slack issues the search token with the message, so routines and investigations cannot search |
| `list_channels`, `list_pins`, `get_user` | channel list, pinned items, user lookup |
| `react` | put an emoji on a message in this channel — the one that started the thread, unless told otherwise — when the channel's instructions ask for one. It needs no Confirm: a reaction changes nothing else and is undone with a click |
| `search_docs` | RAG over the documents folder or bucket (see [Documents](configuration.md#documents)) |
| `web_search`, `fetch_url` | web search and page fetch, with an SSRF guard. Free by default (DuckDuckGo HTML plus a plain GET); put Tavily, Exa, Firecrawl, Jina, Brave, Serper or Cloudflare behind them under **Settings → Web** |
| `run_js` | run JavaScript in a sandbox for the arithmetic and data-picking an answer turns on: totals, grouping, top-N, parsing CSV or JSON. Sealed — no network, files or credentials, ten seconds and 64 MB a call — except that in a channel with connections it gets a read-only `fetch()` to the hosts `http_request` reaches, up to 100 requests in two minutes |
| `about_me` | the bot's own manual: what it can do, every command, how to change the model it answers on, and this channel's Configure link |
| `create_artifact` | write a file and post it in the thread (see [Artifacts](#artifacts)) |
| `start_investigation` | hand a question that needs real digging to the investigation lane: it works in the same thread on a budget of its own, in the background, and posts what it finds (see [Digging](#digging-investigations)); offered when the ask is about a cause, a failure, or which records are involved |
| `start_fix_job` | hand a code change to the fix worker: a separate container clones the repository, changes it, runs the tests, pushes a branch and opens a draft PR (see [Fix jobs](fix-jobs.md)); offered only where a repository is connected |
| `remember`, `recall`, `forget` | memory (below) |
| `remember_personal`, `recall_personal`, `forget_personal` | one person's own notes (below) |
| `create_routine`, `list_routines`, `delete_routine` | scheduled prompts (below) |
| `connect_account` | `!connect` asked for in words: sends the asker a private link to connect their own account for a service that runs per person, or, where the organisation has none set up, posts the steps an admin follows (see [Google](google.md#4-everyone-connects-themselves)) |
| `request_access` | ask a named approver for access the requester cannot grant themselves, as the exact calls that would run on Approve (see [Guardrails](security.md)) |
| `http_request` and named tool packs | connected services (see [Connections](connections.md)) |
| `use_connection` | loads a remote MCP server's tools into the turn; they are not sent up front |
| `<connection>_<tool>` | tools from remote MCP servers, prefixed with the connection name, once loaded |

### Tool use the system prompt forces

The system prompt forces tool use for the obvious cases: "check our docs" or a policy
question must call `search_docs` and answer only from it, "search the web" must call
`web_search`, "what happened in this channel" must call `read_channel_history`, and a question
about the bot itself — what it can do, its commands, how to change its model — calls `about_me`,
since nothing about the bot is in the organisation's documents.

### Web access

Out of the box the web tools are curl and a regex: `web_search` scrapes DuckDuckGo's HTML
page and `fetch_url` does a plain GET and strips the tags. That is free, and it is the first
thing to be rate-limited, blocked by a bot check, or handed an empty page by a site that
renders itself in JavaScript.

**Settings → Web** puts a real engine behind both tools for the whole organisation. Searching
and reading are two choices, because the services split that way — Brave and Serper search and
cannot read a page; Cloudflare reads a page and cannot search:

| engine | search | page reader |
|---|---|---|
| Built-in | DuckDuckGo HTML | plain GET, tags stripped |
| Tavily | `/search` | `/extract` as Markdown |
| Exa | `/search`, embeddings-based rather than keyword | `/contents` |
| Firecrawl | `/v2/search` | `/v2/scrape`, rendered first |
| Jina | `s.jina.ai` | `r.jina.ai`, page as Markdown |
| Brave | independent index, `/res/v1/web/search` | — |
| Serper | Google's results as JSON | — |
| Cloudflare | — | Browser Rendering `/markdown`, needs the account id too |

(Microsoft retired the Bing Search API in August 2025, which is why the obvious name is absent.)

Each dropdown lists only the engines that can do that job, so an impossible pairing is never on
offer. Leaving the reader on *Follow search* means one choice covers both halves where it can —
pick Tavily and Tavily does both; pick Brave and reading stays built-in until you name a reader.

Keys are sealed under `MASTER_KEY`, stored per organisation **and per engine** — so switching
from Firecrawl to Cloudflare cannot send one service's key to another — and never come back to
the console. The page shows only whether one is stored, and a **Test** button makes one real
call so a bad key is found in Settings rather than in a channel. An engine that fails for a
reason the next minute might fix (rate limit, timeout, bad gateway) falls through to the
built-in path with a line in the tool result saying so; a failure that will keep failing does
not fall through, because silently going direct would hide both the bill being paid and the
egress somebody thought they had bought. That test is permanence, not a list of auth codes —
the engines disagree about which status a refused key gets (Exa and Jina 401, Serper 403,
Brave 422). Private and internal hosts are blocked before either path, so an intranet URL is
never handed to a third-party crawler.

## Artifacts

Ask for something *as a file* — an export, a report, "send that as CSV" — and the bot calls
`create_artifact`: it writes the content in full, uploads it to the thread as `md`, `txt`,
`csv`, `json`, `yaml`, or `html` (1 MB cap), and replies with one line and a link instead of
repeating the contents. Every artifact is kept, so the console's Artifacts page lists what has
been produced, with the channel and person it was made for, and can open it in Slack, download
it (`GET /api/artifacts/{id}/raw`), or delete it. That route always serves an attachment and
never renders: the content is model-written and the console origin holds the admin session.

## Memory

Say *remember for this channel: the release captain rotates weekly* and the fact is stored
and added to every future system prompt in this channel. Say *remember for the workspace: …*
in a public channel and it reaches every conversation in that Slack workspace, DMs included; in
a private channel or a DM a memory stays in that conversation. `!memory` lists them,
`!forget <words>` deletes matching ones, and the console has a Memory page. At most 40 reach one
prompt — this channel's own ahead of the workspace's — each cut to 400 characters, and an
organisation keeps at most 500.

Every memory the bot saves or deletes is followed by a link to a page that edits it, so a
memory can be fixed without a console account. It is the Configure page's Memory tab, reached
by the same signed link that already sits under every reply.

### Your own notes

Say *remember for me: I owe Priya the Q3 numbers* and it is kept for you and nobody else. A
note belongs to one Slack user rather than to a room: it comes back in every channel and DM you
use, and no teammate, admin or scheduled routine can read it. `!note <text>` adds one, `!notes`
DMs you the list with a private link to edit them, and the console's Memory page has a **Yours**
tab. They live in their own table, so no organisation-wide query, export or `/v1` route returns
one — there is deliberately no admin route that reads somebody else's.

The bot reads them on your own turns only, and only where the answer is yours alone: in a DM
they are in the prompt, and in a shared channel it has to call `recall_personal` because you
asked. What it says in a channel is still posted in that channel — private means nobody else's
question can surface your notes, not that the bot will never read one out when you ask it to.

## Routines

Say *every weekday at 9am post a digest of #support* and the bot creates a cron routine that
answers the prompt in a fresh thread at that time, in the configured timezone — at most once
every 15 minutes. The post is updated in place with the result. `!routines` lists a channel's
routines, `!routine off <id>` disables one, and the console's Routines page can edit, run now,
or delete them. **New routine** there asks the same things in a form — channel, schedule,
prompt, model, whether it posts every run, and whether its writes run without asking — for a
routine nobody wants to compose as a sentence; it starts running the moment it is created, and
the switch in the list pauses it.

A routine created by asking the bot starts with its writes held, and the bot says so: a write
that would wait for Confirm in a thread waits for it on every run too. Letting them run
unattended is the routine's **Writes** switch in the console — *Run without asking* or *Ask
first* — and never the model's to decide, because the ask to write could have come from anything
the model read. A routine made with **New routine** starts on *Run without asking*, since nobody
is there to press Confirm when a schedule fires. A connection that hands out access still waits
for a named approver either way.

Routines can also be made and changed from outside Slack: `POST /v1/routines` with a developer key,
or `create_routine` from an MCP client such as Claude, naming the channel by id or `#name`. Those
run as the bot rather than as whoever made them, so they reach the channel's connections and
nobody's own, and they start on *Ask first* like a routine asked for here: an MCP client is a
model too. See [api.md](api.md) and [mcp.md](mcp.md).

A run carries two tools a conversation does not: `send_dm`, to tell the one person a finding
concerns (up to 20 a run), and `post_to_thread`, to give each item a message of its own under
the run's header (up to 40; not on a quiet run, which posts no header). It cannot create or
delete routines.

### Routines that post only when it matters

A routine can also be told to stay out of the channel unless there is something worth saying:
set **Reply to channel** to *Only when it matters* and give it a bar — *any VM is over 80% CPU*.
It does the work either way and then says what it decided by calling `stay_quiet` or
`report_now` — a signal rather than a sentence, because a model asked to decide in prose writes
"…10 or fewer, so no report posted", and posting that is the noise the mode exists to remove. Say so when you create one and it is set up that way: *check the GCP VMs
at 9pm and only tell me if one is over 80%*. A quiet routine cannot hold a write for Confirm at
all — there is no thread for the card — so a write that would wait for one does not run: the run
log says so, and the person who created it is sent a direct message.

### A routine's model and run history

A routine can answer on a model of its own. **Model** in the console's routine editor starts on
*Default*, which follows the channel's model and then Settings like any turn, and offers
*Advanced* and the models an admin has listed under Settings → Models → *Models channels may
choose*. Nothing else is on the list, for the reason the channel page has the same one: a
routine is a standing bill, and a model typed into a form is not a model anyone chose to pay
for. The API refuses a model outside the list too.

Every run is recorded whether or not it posted — select a routine in the console to see its
history: when it ran, whether it posted, stayed quiet, failed or was skipped for budget, what it
cost, and the whole answer including the ones nobody was shown. A quiet routine that breaks
still says so: the person who created it is sent a direct message, as before. A routine made in
the console by an admin who is not signed in with Slack runs as the bot, and has nobody to send
that message to.

## Commands

Bang commands are handled before anything reaches the model.

| command | effect |
|---|---|
| `!help` | list commands |
| `!whoami` | what the bot is here: its identity and workspace, its default and advanced models, the host of the organisation's own model key if it has one, the time zone it schedules in, and the console's address. Nothing about the machine it runs on |
| `!restart` | start this thread afresh: from here on I read only the messages after this one, and nothing before it — the earlier messages stay in Slack, and the summary of them is dropped |
| `!stop` | stop the turn running in this thread right now, and any fix job running in it |
| `!jobs`, `!job cancel <id>` | list this channel's fix jobs; cancel one |
| `!mute` / `!unmute` | stop or resume replies in this thread |
| `!model advanced` / `!model <name>` / `!model default` | move this thread onto the advanced model, onto one of the models an admin offers channels (Settings → Models → *Models channels may choose*), or back to the default |
| `!memory` (or `!memories`), `!forget <words>` | show or delete memories |
| `!notes`, `!note <text>` | your own private notes: the list comes by DM, never into the channel |
| `!routines`, `!routine on <id>`, `!routine off <id>` | list or toggle routines in this channel |
| `!ingest`, `!docs` | re-index documents; show index stats |
| `!usage` | this month's spend by channel against the budget |
| `!connect` | services here that run on your own account, and a private link to connect or disconnect each |
| `!personal_instructions <text>` (or `!personal`), `!personal_instructions clear` | how the bot goes about things when it acts as your own account — *only ever look at my inbox*; with no text, shows what is set. Nobody else's turns see them |
| `!access`, `!access cancel` | your access requests still waiting on an approver; `cancel` withdraws the ones raised in this thread |

## Model routing

Each turn picks a model in this order: thread override (`!model`), the channel's default
model, then the workspace default. Nothing escalates on its own — the model a channel answers
on stays put until someone changes it. The advanced model, `z-ai/glm-5.3` by default
(`HEAVY_MODEL`, overridable in Settings), is what fix jobs run on (`worker_model` falls back to
it); a chat turn reaches it only where it was asked for, as a channel's default model (`heavy`)
or with `!model advanced` in a thread.

## Limits and alerts

- Every account starts on the **free plan**. Where the operator sets `FREE_PLAN_BUDGET_USD`
  (0, no cap, by default), that is its monthly model budget, and its own Settings page cannot
  raise it. Where `SUPPORT_EMAIL` is set, the console, the Slack refusal and the alert say to
  write there, and the operator moves the account to **pro** from the link in the message the
  console's **Upgrade** button sends (see [Plans and the operator API](plans.md)).
- A **workspace monthly budget** pauses the bot when the month's spend passes it. A pro account
  sets its own, under the operator's `PLATFORM_MONTHLY_BUDGET_USD_PER_ORG` ceiling.
- A **per-channel budget** does the same for one channel.
- A **per-user rate limit** caps requests per hour.
- An organisation has at most `PLATFORM_MAX_INFLIGHT_PER_ORG` (8) replies running at once —
  routines and investigations are not counted — and a mention past that is told to try again in
  a moment.
- A reply stops digging when its clock runs out — twenty seconds a round, at least two minutes
  and at most `TURN_MAX_MINUTES` (12) — and every turn, a routine's or an investigation's
  included, stops once it has spent `TURN_MAX_USD` ($0.50). Either way it answers from what it
  has (see [Digging](#digging-investigations)). A thread may make 100 tool calls across all its
  turns, or twice its rounds where a channel raised them, and each model call asks for at most
  32k output tokens.
- When a limit trips, an alert is posted once per hour to the `alert_channel`, if one is set.

Every completion's token usage and cost is stored — the provider's charge, or, on an
organisation's own model key where the provider reports none, an estimate from the list price —
so `!usage` and the console's Overview and Activity pages show real numbers. Removing a
workspace keeps the month's spend, so a budget cannot be reset by removing the workspace and
adding it back.

## Digging: investigations

A reply is sized for a conversation — up to fifty rounds of tool calls by default
(`MAX_TOOL_ROUNDS`, which a channel can change on its Configure page), inside twelve minutes
(`TURN_MAX_MINUTES`) and fifty cents (`TURN_MAX_USD`) — because the person is watching the
thread. Some questions are not that shape. "What is causing these failures?", asked under an
alert, means pulling logs, narrowing, cross-checking, and going back for more.

Three things happen for those:

- **A reply that runs out answers anyway.** On its last round the tools are taken away and the
  model is told to answer from what it has, naming what it could not check. It used to end on
  "I stopped after too many tool calls", throwing away everything the run had found. The same
  landing happens when the clock runs out rather than the rounds, or when the turn has spent
  `TURN_MAX_USD`.
- **A turn that repeats itself is landed early.** The same tool call with the same arguments
  runs at most three times in one turn — the second and third come back under a note saying it
  already ran — and a fourth is refused without running, since its result is already in the
  transcript. A call that differs from an earlier one only in its numbers — the next page of the
  same query — and returns exactly what that one returned counts as asking nothing new. A turn
  whose last four rounds asked for nothing new has its tools withdrawn and writes up, the same
  way. Nothing here waits for the round budget: a routine allowed two hundred rounds that gets
  stuck alternating two searches ends around round ten, and every round it would have spent
  re-sent the whole transcript, so the loop was the expensive part.
- **A question that needs digging is handed to the investigation lane.** When someone asks about
  a cause, a failure, or which records are involved in one, the turn is offered
  `start_investigation`: it writes a brief — the question, the alert text, the figures, the time
  window — and the question is queued. A small pool of workers picks it up, works in the same
  thread on a budget of its own (`investigation_rounds` and `investigation_minutes`, 40 rounds and
  12 minutes by default, under the same `TURN_MAX_USD`), and posts one answer there: what the
  cause is, the evidence it rests on, and what it could not establish.

### Investigation limits and the queue

The lane's runs do not count against the per-organisation in-flight cap, so a ten-minute
question is never the reason a one-line question elsewhere is refused. An organisation has at
most two investigations queued or running by default (`investigation_max_open`), and a thread
one. The queue is a table, not a goroutine: a run whose container is replaced mid-deploy leaves
a row whose lease expires, and the next container picks it up rather than leaving the thread
waiting forever. `stop` in the thread calls one off, queued or running, and a channel that would
rather dig inline can raise its own tool rounds on its Configure page instead.
