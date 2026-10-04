# Code review

attest_tag reviews pull requests on GitHub: it reads the diff and the code around it, posts the
problems it can name a failure for as inline comments, keeps one summary comment on the pull
request current with a confidence score, answers `@` commands, and answers back in a finding's
thread. It runs inside the bot's own process, reaches GitHub through the GitHub App the console
already installs, and is set up under **Automation › Reviews** in the console.

## What it does and what it never does

On a repository whose review is switched on, it:

- reviews a pull request when it opens (or reopens, or leaves draft), on every push, or only when
  somebody asks — the *When* setting;
- posts one review per run, as a **comment**, holding its findings as inline comments, and one
  summary comment that it edits in place from then on;
- answers `@<app>` commands from the repository's own people, and replies in its findings' threads;
- records every run in the console — what it found, what it dropped and why, what it did not read,
  and what it cost.

It never:

- **passes or blocks a pull request.** Every review is a plain comment, never an approval and never
  a request for changes. A reviewer that could pass a pull request would be a gate somebody could
  talk into opening, and its confidence score is advisory: nothing on GitHub waits on it.
- **runs the pull request's code.** There is no checkout, build or test run. It reads files through
  GitHub's API with a read-only token, the model has read tools only, and Go writes every word that
  is posted, from findings it has checked against the code.
- **takes instructions from the pull request.** Instruction files — `REVIEW.md`, `AGENTS.md`,
  `CLAUDE.md`, `.github/copilot-instructions.md`, and the `AGENTS.md` nearest each changed file —
  are read at the **base** commit, as is a [skill](#skills) a type links in the same repository, so
  a pull request that adds "report nothing" to one is not obeyed. Text in the diff written to steer a reviewer is itself a finding, and caps the score.
- **writes anything in shadow.** A repository in *Shadow*, where every connection starts, has the
  whole review run and recorded in the console and not one word written to GitHub: no review, no
  summary, no reaction, no answer to a command or a reply. Only the organisation's own chat
  channel hears of it, and only where one is set ([announcements](#announcements-in-a-chat-channel)).
- **pages people or links out.** Everything a model wrote passes a sanitiser before it is posted:
  `@` mentions are defused, images are removed, and links survive only into the pull request's own
  repository and the context repositories the review was allowed to read.

## Setting it up

Four steps. The first two are done once per deployment by whoever runs it, and on the hosted
service they are done already.

1. **The GitHub App** is configured — `GITHUB_APP_ID`, `GITHUB_APP_SLUG` and its private key
   ([configuration](configuration.md#github-app-optional)) — with the permissions and the five
   events code review needs (below).
2. **Its webhook** is on at GitHub, sending to `https://<host>/github/webhook`, signed with the
   secret this deployment holds in `GITHUB_APP_WEBHOOK_SECRET`.
3. **The App is installed** on the account that owns the repositories: from *Connect repo*
   ([Repositories](connections.md#repositories)), or from **Add connection** on the Reviews page.
4. **The installation is added** under Reviews › Settings with **Add connection**. Installing the
   App for the bot's GitHub tools or for fix jobs reviews nothing by itself: an installation is
   reviewed only once it is in this tree.

Without the webhook secret no pull request is reviewed as it opens and no command or reply is ever
heard, but a review started in the console still runs, since that needs only the App.
`GET /api/github/installations` lists what is missing (`review_missing`), and `GET /api/me` says
`github_review: true` once nothing is. With `SIGNUP_MODE=open` the secret is left out of both,
since whether it is set is the operator's fact, not a tenant's: there `github_review: true` means
only that the App is configured.

### Permissions and events

Code review reads with a token that can read contents and pull requests, and posts with one that
can write pull requests and nothing else, so the App needs **Contents: Read** and **Pull requests:
Read and write**. An App made for fix jobs has both already, with Contents read and write. Under
*Permissions & events* it has to be subscribed to the five events
[configuration](configuration.md#the-apps-webhook-and-permissions-for-code-review) lists: Pull
request, Issue comment, Pull request review, Pull request review comment and Pull request review
thread. An App subscribed to Pull request alone gets its reviews and nothing else — commands and
replies are never delivered, and nothing says that they are missing.

Adding a permission to an App asks every account that installed it to accept the change at GitHub,
and until its owner does, that installation's tokens lack it. While an installation lacks what code
review needs, the connection's status card in the console says so, with a link to accept
(`missing_permissions` on the installation), and so does *Connect repo* when that installation is
picked; GitHub's `new_permissions_accepted` event clears it.
An installation whose permissions were never recorded shows no warning either way
(`permissions_known: false`).

### The webhook URL and its secret

In the App's settings at GitHub: Webhook **Active**, the URL `https://<host>/github/webhook`,
content type `application/json`, and a secret, which is `GITHUB_APP_WEBHOOK_SECRET` here. GitHub
sends a `ping` when the webhook is saved, and the log line *GitHub webhook ping: the URL and the
secret agree* is the proof that the two match. Every delivery is checked against the secret, over
its exact bytes, and against `GITHUB_APP_ID`, before anything in it is believed. On a deployment of
its own — not open signup — *Connect repo* shows the URL this deployment expects and whether the
secret is set (`webhook_url` and `webhook_secret_set` on `GET /api/github/installations`).

To rotate the secret without losing the deliveries in between: put the new one in
`GITHUB_APP_WEBHOOK_SECRET` and the old one in `GITHUB_APP_WEBHOOK_SECRET_PREVIOUS`, deploy, change
the secret at GitHub, then unset `_PREVIOUS` and deploy again. A previous secret with no current one
is ignored.

Only what can be used is kept: an installation's own events, and pull-request events on a
repository whose installation is in an organisation's review tree and whose review is not *Off*.
Everything else is answered `200 ignored: <why>`, which GitHub shows beside the delivery under the
App's *Recent Deliveries*. GitHub never retries a failed delivery by itself, so a delivery that
could not be stored is answered 503 and waits there for **Redeliver**; a redelivery of one already
stored does nothing twice. A pull request whose delivery never arrived — the deployment was down —
is found by a catch-up that lists each reviewed repository's open pull requests every ten minutes.
A command or a reply written meanwhile is not recovered, and has to be written again.

### Who has code review

`CODE_REVIEW` decides which organisations on a deployment have code review at all:

- `all`, the default: every one. A self-host is one organisation, spending its own model key, with
  no plan to gate it on — leave it unset.
- `pro`: organisations on the pro or the enterprise plan. This is what the hosted service runs, so
  a free account there has the settings and nothing reviewed.
- `enterprise`: the enterprise plan only.
- `off`: none. Automation › Reviews leaves the console's rail and its routes answer 404, a pull
  request's delivery is answered `200 ignored: code review is off on this deployment`, and no
  review runs.

An organisation whose plan does not have it starts nothing that spends. A pull request that opens
or is pushed to is not reviewed, and the reason it records is `plan`; a command from its own people
is answered once a day per pull request, in words that name no plan; replies in its findings'
threads are not answered; and Start review, Run again and Try on a PR answer `402` with the plan
that would have it. What it had stays — its settings, its history, the summaries on its pull
requests — so moving the account onto such a plan picks up where it left off. A push whose delivery
was missed meanwhile is not caught up: the summary goes on naming the old head until the next push,
or until the plan is back. In the console the
Reviews page still reads, under a strip saying *Code review needs the Pro plan* with the way there:
*Choose a plan* on a deployment that sells plans, else *Upgrade*, which asks support; Start review,
Run again and Try are off. `GET /api/me` says which as `code_review`: `available`, and when not,
`reason` (`off` or `plan`) and `needs` (`pro` or `enterprise`). A typo in the variable reads as
`pro` — the narrow reading, so a mistyped hosted setting never opens reviews to every free account —
and the boot log says so.

### Add a connection

Reviews › Settings › **Add connection** opens a menu:

- **Already connected, not reviewed yet** lists the GitHub App installations the organisation
  already holds. Picking one adds it, starting from the built-in defaults or from a copy of another
  connection's settings. It is never added *Live*: a copy of a live connection comes in *Shadow*,
  with every branch rule's *post live* dropped, since going live is decided per connection, on
  purpose; a copy of one that is *Off* stays *Off*. Adding back a connection that was stopped
  restores its own settings instead, which needs `connections.manage` if they post live, review
  every push or announce in a channel. A copy that sets a model, max $, forks, context repositories,
  a channel or *every push* needs `connections.manage` too.
- **Install the GitHub App on another account** goes through the install flow at GitHub, and comes
  back here with Add connection open for the new installation.
- **GitLab** is listed for later, and cannot be picked yet.

A repository connected with a pasted token is never offered: without the App, GitHub sends no
events about it.

A connection's panel opens with a status card — who installed the App and when, whether the
installation is active, suspended or uninstalled, when the last delivery arrived, and the warning
when GitHub needs new permissions accepted — then the connection's settings, then **Stop reviewing
this connection**. Stopping keeps its history, groups and settings, and adding it again restores
them. An installation uninstalled at GitHub stops being reviewed by itself and shows as
uninstalled, with nothing deleted; installing the App again makes a new installation, which is
added afresh. A suspended one hears nothing from GitHub until it is unsuspended, and is then
reviewed as before. The API is `POST /api/review-settings` with
`{"kind": "connection", "installation_id": …, "copy_from": "<connection id>"}`, and
`DELETE /api/review-settings/{id}` and `POST /api/review-settings/{id}/restore` to stop and restore.

### Adding and removing repositories

A connection's tree holds the repositories the organisation saved through the App for its
installation. **Add repositories…**, in the connection's ⋯ menu, lists the others the installation
reaches at GitHub, read live: each marked *Private* or *Public*, those removed from code review among
them, with a search above and up to 50 ticked at a time. Adding one saves it as one of the
organisation's App connections under Access bundles › Repositories — attached to no channel, as
installing the App does — once GitHub's list says the installation reaches it, and it is then
reviewed under the connection's settings at once. GitHub's list is read 300 at a time; an
installation that reaches more says so, and one not shown is added by typing its owner/name in the
search, when GitHub, asked about it by name, says the installation reaches it. A repository the App
was never given is granted on the installation's page at GitHub, which the dialog links to. Adding
saves connections, so it needs `connections.manage`. The API is `GET /api/review-settings/{connection}/available` and
`POST /api/review-settings/{connection}/repos` with `{"repos": ["owner/name", …]}`, which answers
what was added, restored, already there, or refused and why.

**Remove from reviews…**, in a repository's ⋯ menu, asks first, then takes it out of code review:
it leaves the tree for the connection's **Removed (n)** list, folded at its foot; nothing on it is
reviewed — not when a pull request opens, not when somebody asks — and its pull requests' deliveries
are not kept. Access bundles › Repositories says *review: off (removed from reviews)*. Its settings
and group stay, and so does its connection: Slack tools and fix jobs go on reaching it. Removing
needs `reviews.manage`. **Restore** in that list is judged like restoring a stopped connection, and
needs `connections.manage` when the repository would then post live, review every push, announce in
a channel or run a branch rule's own model; adding it again always needs `connections.manage`, as
adding does. The API is `POST /api/review-settings/{id}/remove`
and `…/restore`, a repository addressed as `{connection}?repo=owner/name` when it has no row of its
own.

## The settings tree

A **connection** is one installation of the GitHub App. Under it, **groups** are optional sets of
its repositories a team makes in the console — a repository is in at most one, and deleting a group
moves its repositories back under the connection — and then the **repositories**: every one the
organisation reaches through that installation, whether anything is set on it or not.

```
octo-org · GitHub App      ← connection: everything below inherits from it
├─ Frontend                ← group
│  └─ acme/web
├─ Backend
│  └─ acme/api
└─ acme/docs               ← in no group: inherits straight from the connection
```

A value is resolved **repository → group → connection → built-in default**:

- **A single value** comes from the nearest level that sets one, so a repository can override its
  group and a group its connection.
- **The four lists add up**, connection first: instructions, authors to skip, paths to ignore and
  context repositories. A connection's "skip `renovate[bot]`" still applies to a repository that
  adds one more author, so nobody restates the basics, and nobody loses them by adding to the list.
- **Branch rules** are the exception: an ordered list where the first match wins cannot be merged,
  so the nearest level that has any rules supplies the whole list.

Every field below the connection says where its value comes from — "Inherit: High · from
Frontend", or "set here" with a **Reset** that goes back to inheriting — and a repository's panel
ends with an **Effective** line: what will actually run. A repository the installation gains later
is reviewed under its connection's settings at once, so *Live* on a connection reviews every
repository that installation can see. `GET /api/review-settings/{id}` returns the level's `own`
values, the `effective` ones with each field's `source`, and, given `?base=&head=`, the branch rule
those branches meet. `PUT` takes the level's settings whole, or with `fields` naming the ones it
changes, the rest kept as stored — as each save in the console does — so a page open since somebody
else's save cannot undo it. Under
Access bundles › Repositories each repository saved through the App says *review: live*, *shadow*
or *off*; its menu's **Code review settings…** and **Set review** in the selection bar open it here.

### Settings and their defaults

| setting | built-in default | what it is |
|---|---|---|
| Review | *Shadow* | *Off*, *Shadow* (recorded in the console) or *Live* (posted to GitHub) |
| When | when a pull request opens | *Only when asked*; *When a pull request opens* (also reopened, and ready for review); or *On every push*, which waits 90 seconds for the next push and holds new minor findings to what changed since the last review |
| Drafts | skip drafts | *Skip drafts* or *Review drafts*: whether a draft is reviewed without anybody asking |
| Forks | when a member asks | *When a member asks* or *Never* ([forks](#pull-requests-from-forks)) |
| Strictness | medium | how sure the verifier must be to keep a finding: 60, 70 or 85 out of 100 for low, medium and high |
| Max comments | 8 | inline comments per review, 1 to 20; the rest are listed in the summary |
| Comment header | none | text above every inline comment, up to 400 characters |
| Model | Advanced | the default model, the advanced one, or one offered to channels; it verifies every finding, and finds them for every type without a model of its own |
| Max $ per review | $1.00 | $0.10 to $5.00, verification included ([money](#money-and-budgets)) |
| Instructions | none | what the team wants checked, one entry each, put in the prompt as the team's criteria |
| Authors to skip | none | GitHub logins or globs (`*-bot`, `renovate[bot]`) never reviewed without somebody asking; a pull request a bot opened is skipped anyway |
| Paths to ignore | none | path globs left out of every review (`dist/**`, `**/*.snap`) |
| Context repositories | none | the organisation's other repositories connected through the App, which the review may read, and quote, for contracts that cross them — up to five per review |
| Channel | none | the chat channel each review and the merge are announced in ([announcements](#announcements-in-a-chat-channel)) |
| Notify on | all four | which events that channel hears of: *Started*, *Finished*, *Failed or not run*, *Merged* |
| Branch rules | General on every branch | [branch rules](#branch-rules) |

### Who may change what

Changing the settings needs `reviews.manage`, which the built-in admin and editor roles hold;
`reviews.view` reads them, and the viewer role holds that. What posts, spends or reaches further
needs `connections.manage` as well, which only admin holds: *Live*, *every push*, forks, context
repositories, the model, max $, the channel reviews are announced in (not *Notify on*, which only
picks what it hears), a branch rule that posts live, reviews every push or names a model or a
channel, adding repositories, and code review's budgets. It
is judged on what a change makes effective at every repository and branch under the level changed,
so it cannot be reached sideways — by resetting a repository so it inherits a connection's *Live*,
by moving it out of a group, or by deleting a rule that held some branches in *Shadow*.
[Guardrails](security.md#who-may-turn-code-review-up) has the whole list.

A save is made over the level as it read it. If somebody saved any of that level's settings while
it was being checked, it is refused with 409 and writes nothing, so it never puts back what they
changed, a field it did not name included.

## Review types

A review type is a rubric: what the review is for, which files it looks at, its strictness, and the
rules the finder checks the change against. Each type runs a finder pass of its own; their
candidates then go through one set of checks and one verifier, a problem two types both raise
becomes one finding carrying both names, and the result is still one review and one summary; when
more than one type ran, each finding names its types and a line under *Open findings* counts the
open findings per type. Five ship built in:

| type | key | looks for | inline comments |
|---|---|---|---|
| General | `general` | logic that does not do what the code around it says, broken contracts, data loss, crashes, races, leaked resources, inputs nobody handles, work that is unsafe to run twice, new behaviour nothing tests | P0 to P2 |
| Security | `security` | missing authentication or authorisation, data crossing between tenants, injection, secrets, server-side request forgery, unsafe deserialisation, open redirects, cross-site scripting, cookies and sessions, CORS, cryptography, webhooks, dependency and CI changes | P0 to P2 |
| Tests | `tests` | new branches no test reaches, tests that cannot fail, assertions removed or weakened, expected values edited to match a bug, missing negative cases, tests that depend on time, order or each other, mocks that no longer match | P0 to P2 |
| Performance | `performance` | queries and remote calls repeated in a loop, unbounded reads, lists and loops, queries and migrations no index serves, quadratic work on a busy path, expensive work repeated, needless re-renders, blocking work on a hot path | P0 to P2 |
| Release summary | `release` | a large merge into a production branch: what ships by area, migrations, configuration, deploy order, old and new versions running side by side, rollback. The summary is the result; only a P0 is commented on inline | P0 only |

General runs when nothing says otherwise. A type's key is the word branch rules, the Start review
dialog and commands use: `@<app> review security`. Each type's findings below its inline minimum
are listed in the summary under *More notes*.

### Editing a type and its rules

Reviews › **Types** lists every type — built-in, edited, custom or off — with how many branch
rules name it. A type has a name; *what this review is for*, one paragraph of up to 2,000
characters; *only look at files*, path globs, empty for every file; a strictness; its own model and
max $, both needing `connections.manage`; an on/off switch; and up to 40 **rules**. A rule is one
line of up to 400 characters with a severity cap (the most severe a finding citing it may be),
optional paths, an optional bad and good example, an on/off switch, and where it came from:
*built-in*, *team* (written in the console) or *learned* (proposed from a reply in a thread).

- **Editing a built-in** makes the organisation's own copy of it. Rules a later release adds to the
  built-in still run in the copy and are shown as new, so a team can switch one off. **Reset to
  built-in** saves the shipped text as the copy's newest version.
- **A rule switched off** is kept, so it can come back, and so the rules after it keep their ids:
  a finding cites rules as `R1`, `R2`, by their place in the list.
- **New type** starts blank or as a copy of any type. Its key — 2 to 30 lowercase letters, digits
  and hyphens, not a built-in's — names it in branch rules and commands.
- **A type switched off** stops running and leaves the Start review dialog. It is kept, and a branch
  rule that names it says so in the console; a pull request whose rule names it lists it under *Not
  reviewed* as turned off.
- **Proposed rules** come from a `remember …` reply in a finding's thread and do nothing until
  somebody approves it and saves the type, which makes it part of the next version; **Reject**,
  saved the same way, keeps it in the list, off.

Rules and purposes are trusted text, written by people with `reviews.manage` and put into the prompt
framed as the team's review criteria. They can add no tool, change nothing about posting, and reach
no other repository.

### Skills

A type can follow **skills** as well as its rules: folders in GitHub repositories holding a
`SKILL.md` and the Markdown beside it, the way agent skills are written — a team's review standards,
a checklist for one framework. Under **Skills** in the type editor, paste a folder's github.com
address, or give its repository, its folder and, if you like, a branch, tag or commit; up to five
per type. A repository may hold many skills, and only the folder named is read: its `SKILL.md`
first, then its other `.md` and `.txt` files, three folders deep. Scripts beside them are not read,
and nothing is ever run.

| the repository | read with | at |
|---|---|---|
| left empty: the one under review | the review's own read token | the pull request's **base** commit, like the instruction files: a pull request that edits the skill is not reviewed by its edit |
| one of the organisation's App connections | that connection's read token | the branch, tag or commit named, else the default branch, pinned for the whole review |
| any other public repository | no credentials at all | the same; the commit a branch names is remembered for ten minutes, and a commit's files while the deployment runs, since GitHub allows a server 60 reads an hour without credentials, of which one organisation may make 30. When none are left, a review reads the commit it last knew, and says so |

A private repository must be one of the organisation's App connections, and is never read for a
pull request in a public repository, where whatever the review read may be quoted. A public
repository somebody else owns changes whenever they push; **Pin to** fixes a link to the commit it
read, so their next push does not reach your reviews.

**Check** reads a link the way a review would and shows the skill's name and description from its
`SKILL.md` front matter, its files, the commit, and anything worth a warning; a link to the
repository under review is checked in one of your repositories, at its default branch. The finder
gets a type's skills after its rules, at most 24,000 characters of them, and a finding that rests
on one cites it as `S1`, `S2` by its place in the list: the inline comment names the skill under
*Why this was flagged*, the verifier is shown what it asks for, and the summary lists the skills the
review followed. Like a rule, a skill is framed as the team's criteria: it adds no tool, widens
nothing the reviewer may read, and changes nothing about posting. A run's details in **History** say
which commit each skill was read at, or why it was not, and what the skills said is part of what
makes two reviews the same, so an edited skill is never answered from an earlier review.

### Versions and Try on a PR

Every save is a new version (v1, v2…), and every run records the version of each type it ran, so an
old review stays explainable after the type changes. **History** shows each version as it read, and
**Revert** saves an old one as the newest — the history only ever grows.

**Try on a PR** runs a type as it stands in the editor, before it is saved, on a pull request you
pick. A try is always recorded in shadow, whatever the repository's mode: its findings show in the
console and nothing is posted. It is not the pull request's review either — the summary, the score,
`status` and the next real review take no notice of it. It goes through the same gate as a review
(the connection added, fork policy, the money, the throttles) and needs `reviews.manage`.

## Branch rules

Which types a pull request gets is chosen by its branches. A level's branch rules are an ordered
list, and the first rule whose **base** (the branch the pull request merges into) and **head** (the
branch it comes from) both match wins; its types run in the order listed. The last row is always
*any → any*, the fallback for everything else, and cannot be moved or removed: a list that ended
anywhere else would leave pull requests that match nothing unreviewed, without a word.

For a team that merges features into `testing` and releases `testing` into `main`:

| # | head → base | types | overrides |
|---|---|---|---|
| 1 | `testing` → `main` | Release summary, Security | |
| 2 | `hotfix/*` → `main` | General, Security | post *Live* |
| 3 | any → `testing` | General, Security | |
| 4 | any → any | General | |

The narrow rule goes above the broad one: rule 1 below a rule for any → `main` would never be
reached. In a glob `*` matches within one segment (`release/*` is `release/1.2`, not
`release/1.2/rc`), `**` matches across `/` (`dependabot/**`), `?` matches one character, matching
is case-sensitive, and an empty base or head is any branch.

A rule may also override *When*, strictness, the model, the channel its pull requests are announced
in, and whether they are posted *Live* or recorded in *Shadow*. An override applies only where the rule list was set at the same
level as the value it replaces, or nearer the repository: a connection's rule that says *Live* does
not overrule a repository somebody set to *Shadow*, and no rule switches on a repository that is
*Off*. A level holds up to 20 rules of up to 10 types and 10 labels each. The rule that matched is
recorded on the run and named in the summary's footer (`Rule: any → testing`); types named in a
command or in the Start review dialog replace the rule's for that one run.

The list is saved whole, so a save says which list it replaces. If somebody saved the rules since
the page read them — at this level, or above where it inherits them — nothing is written: the
console says *Branch rules changed since you opened them* and keeps your edits over their list, and
Save then replaces theirs on purpose. Through the API, send `GET`'s `digests.branch_rules` back as
`expect` with `fields: ["branch_rules"]`; a stale one answers 409.

### Label rules

A rule with **labels** adds review types instead of choosing them: a pull request carrying any of
its labels — compared without regard to case — gets the rule's types after its branch rule's, and
every label rule it meets adds its own. With `security-review` → Security and `perf` → Performance
above the fallback, any branch gets those types when somebody labels a pull request for them. A
label rule may name a base and a head to narrow it, needs at least one type, and overrides nothing:
*When*, strictness, where it posts, the model and the channel stay the branch rule's. Types named in
a command or the Start review dialog replace a label's as well.

The run records the label beside the rule — `Rule: any → testing +label:security-review` — and a
review with a label's types is a different review from one without. A label put on a pull request
already reviewed reviews what it adds and nothing else, on the head as it is, 90 seconds later —
unless a review of that head has run those types or is queued to. It is held to *When*, drafts, the
authors to skip and forks like any review nobody asked for, and is not run while the pull request's
automatic reviews are [paused](#pausing-automatic-reviews); one a person's label asks for does not
count towards the pause, one a bot's label asks for does. Taking the label off, or changing its rule,
during the 90 seconds ends its review having spent nothing; taking it off later changes nothing
already reviewed.
`GET /api/review-settings/{id}?base=&head=&labels=` says what a pull request between those branches
with those labels would run, and Start review ticks a pull request's types with its labels' added.

In the console each rule has a **Labels** box under its branches, labels separated by commas. A rule
with labels reads `label perf → +Performance`, offers its types and no overrides, and stays above
the fallback; changing which rules have labels is changing the list's shape, held like its branches.

### Changing types and rules from the assistant

On Reviews the [console assistant](console.md#the-console-assistant) proposes these changes to
somebody with `reviews.view` and `reviews.manage`, for the type or level on screen unless told
another. A card writes nothing; Confirm makes the page's own save, held to the page's permissions,
and the card says when that uses your `connections.manage`.

- **A type's rules go by number**, `R1`, `R2`…, as the Types tab and findings cite them: "turn off
  R5", "make R3 P2". A new rule goes at the end. Removing a rule switches it off, so every number
  after it still means the same rule. A learned rule is approved or rejected only when named, and
  the assistant says who wrote it on GitHub, unless it was reworded here since.
- **A new type** starts blank or as a copy, whose card lists the skills, model and budget it
  brings. A branch rule that runs it is a second card, which waits until the first is confirmed.
  The first change to an unedited built-in saves the organisation's copy as v1 and the change as v2.
- **A level's branch rules go by position**, 1 at the top. Rules added, edited, removed and moved at
  one level go on one card, each change counting positions in the list the one before it left. A
  new rule goes just above the fallback unless given a place, and the fallback stays last.
  *Inherit*, on a card of its own, drops the level's list so it runs the one above again.
- **A level that inherits** gets its own copy of the list with the change in it, and the card says
  so: later changes above stop reaching it. To change what every repository of a connection runs,
  ask for the connection.
- **A stale card writes nothing.** Confirm sends the type's version — and a copy, the version of
  the type it copies — or the digest of the level's branch rules, inherited ones included. If
  somebody saved one since, the save answers 409 and the card says *Changed since this was proposed
  — ask again*.
- **Stays on the page:** a type's model, budget and skills, a rule's examples, Reset, Revert and Try
  on a PR; a branch rule's *When*, posting, model and channel, and label rules; a level's other
  settings; adding connections, groups and repositories.

The card's audit row and the save's share a `proposal_id`; a branch-rule card's row also keeps the
list before and after, which settings have no history of otherwise.

## What a review looks like on GitHub

### The review and its inline comments

Each run posts one review, as a comment, against the commit it read. Its body is empty but for a
hidden signed marker, and its findings are its inline comments; a run with nothing to say inline
posts no review at all. A comment reads:

```
[the comment header, when one is set]
**Security · P1 · Tenant check missing on list query**
the trigger, then what happens
Evidence: file and line links, pinned to the commit that was read
a suggestion block, when there is a sound one
▸ Why this was flagged — the category, the rules it cites, the verifier's confidence
▸ Prompt for your coding agent
Reply here if this is wrong or intended — every reply gets a verdict.
```

Severity says what happens, not how sure the model is. **P0** is a security hole, data loss, or a
crash or outage on a reachable path, or a committed secret. **P1** is wrong behaviour under a
concrete, plausible trigger. **P2** is maintainability, a convention a cited rule asks for,
performance, or a missing test for new logic.

Before anything is posted, Go checks every finding — its quotes are in the code, its lines sit
inside one hunk GitHub will take a comment on — and a verifier tries to refute it, keeping it only
at the confidence the strictness asks for: every finding except the two Go raises itself from the
diff, a committed credential and text addressed to an AI reviewer, which need no model to confirm. At most *Max comments* go inline, and at most three P2s;
the rest are listed in the summary. A suggestion is offered only on the pull request's side of the
diff, replacing exactly the finding's lines, and never on a workflow or an action
(`.github/workflows/**`, `.github/actions/**`, `action.yml`): a one-click change to what runs with
the repository's secrets is not the reviewer's to hand out. The agent prompt is built from the
location, the severity and the title only, never the scenario, so text a pull request smuggled into
a finding cannot become an instruction to an agent somebody points at the comment.

### The summary comment

One conversation comment per pull request, created by the first review and edited in place after
that. Go renders it from what the database holds, so it changes the moment a finding does:

- a heading with the score, `attest_tag review · Confidence 3/5 (advisory)`, and one sentence on
  the worst open finding: *Do not merge yet*, *Merge after fixing*, *Only minor findings*, or *No
  blocking issues found*;
- **Summary**, the model's account of what the pull request does, and **Open findings**, worst
  first, each linked to its comment, marked open, disputed, or claimed fixed in a commit, and with
  the lines of code it points at folded under *Code*;
- when more than one type ran, a line naming the types and how many open findings each raised;
- *Outside the diff*, *More notes*, *Beside a masked credential*, *In unchanged files* (P0s only),
  *Notes* (said and never scored — a credential-shaped string in a test fixture) and *Pre-existing*
  (not introduced by this pull request, never scored, at most two);
- **Not reviewed**: the changed files it did not read — binary, generated, ignored, too large, or
  past the money — and any review type that did not run, with why;
- **Possibly outdated**: findings in files that changed after the reviewed commit, kept off the
  diff;
- **Fixed** and **Outdated**, folded: what a later review found fixed, with the commit, and what it
  found gone from the pull request ([below](#when-a-push-fixes-a-finding)), neither scored;
- a footer: how many reviews, the commit last reviewed, the head if it has moved since (with
  `@<app> review` to review it), the branch rule, and the cost — only on a private repository, and
  only where the organisation shows costs.

A review that failed says what kind of failure — the budget, the model provider, GitHub, a timeout —
and shows no score; the error itself stays in the console.

### The confidence score

The score is computed in Go from the open findings, never asked of the model:

| open findings | score |
|---|---|
| none | 5 |
| only P2s | 4 |
| one P1 | 3 |
| two or more P1s | 2 |
| one P0 | 1 |
| two or more P0s | 0 |

Pre-existing findings and notes never count. A disputed finding, or one somebody says is fixed,
still does; a withdrawn one, one whose thread a person resolved, and one a later review found fixed
or gone do not. The score is capped at
4 when the review could not read every reviewable changed line, or when the diff carries text
written to steer the reviewer — talking a reviewer out of reporting anything is the easiest attack
on one — and the summary says which. It is advisory: nothing on GitHub reads it, and there is no
status check.

### When a push fixes a finding

Each review of a newer head first looks again at every open or disputed finding earlier reviews
raised. Go looks for the lines each was about within 50 lines of where they were, in a file the pull
request renamed too, and only where they are once:

- **there, with the code around them as it was**: it stands, moved to where it is now;
- **its file gone**, and its code in nothing the pull request adds: *outdated*, no longer scored;
- **changed, or claimed fixed in its thread**: one check on the verifier's model, shown the finding
  and the code then and now — never the pull request's description or the thread, which are the
  author's case for a fix, not evidence of one. *Fixed* closes it; otherwise it stays open where
  the check looked, and is checked again only once that code changes again.

At most ten checks a review, the most severe first and claimed fixes first among equals, paid from
the verifier's share of *Max $*; one left unchecked stays open, and the run's detail says why. No
check closes anything on a pull request carrying text addressed to an AI reviewer, nor a P0 or P1
claimed fixed on unchanged code from outside the repository. A fixed finding whose code comes back
is open again, its thread told *Back at*. A claim made at the head already reviewed is checked once
a newer head is.

What closed moves to *Fixed* or *Outdated* in the summary, and the score follows. On a live pull
request each fixed finding's thread gets *Fixed in `abc1234`* and is then resolved, best effort
([threads](#threads-resolved-on-github)), and a claim the check refuted gets *Still present at
`abc1234`* once. The channel's reply counts them: "2 fixed, 1 open".

## Commands

A comment on a pull request whose first line (not blank, not quoted) starts with the App's handle
is a command:

| command | what it does |
|---|---|
| `@<app> review` | reviews the head, with the types its branch rule picks. On a head already reviewed with the same types and settings it says so and spends nothing |
| `@<app> review security` | the same, with the types named (up to ten keys) instead of the rule's |
| `@<app> full review` | reviews again from scratch, even a commit already reviewed: for owners and members of the organisation, once per commit a day |
| `@<app> status` | the score and why, the open findings by severity, the commit last reviewed and whether the head has moved, and why the last request was not reviewed — from what is stored, with no model call. `@<app> why is the score 3?` asks the same |
| `@<app> help` | the commands and the review types here. A bare mention asks for it too |
| `@<app> pause`, `@<app> resume` | stop and start the pull request's automatic reviews, for owners and members of the organisation ([pausing](#pausing-automatic-reviews)) |

`<app>` is the App's slug (`GITHUB_APP_SLUG`), and `@<app>[bot]` works as well. The mention has to
be followed by a space, a comma, a colon or the end of the line, so a development App called
`<app>-dev` installed beside it never answers for this one. A mention mid-sentence, in
a quote or in code is talking about the bot rather than to it, and an edited comment is not a new
command. Anything else after the mention is a question, which is not answered yet: the bot points
at help, once an hour.

Commands are taken from the repository's own people: whoever GitHub marks on the comment as its
owner, a member of the organisation that owns it, or somebody invited to collaborate on it. Anybody
else is ignored on a private repository, and on a public one gets one line a day per pull request
saying who can ask. One person may send ten commands an hour. A command is picked up with an eyes
reaction, and skips *When*, drafts and the authors to skip — a person asked — but not fork policy,
the money or the throttles. On a pull request whose review is recorded in shadow a command is acted
on and nothing is written to GitHub. Each is audited (`review.command`) with what it came to.

### Pausing automatic reviews

A pull request's automatic reviews — when it opens, on each push where *When* is every push, and
the catch-up's — pause by themselves after five. The sixth is not run: the pull request records
`paused`, the summary's footer says *Automatic reviews paused after 5 — `@<app> resume`*, and
nothing else is posted. `@<app> pause` pauses them sooner and `@<app> resume` starts them again,
with the count back at nothing; both are for owners and members of the organisation, like a full
review, and are audited. A review somebody asks for — a command, the console — runs whether they
are paused or not, and is not counted; a label's does not run while they are. `status` says when
they are paused and why.

In the console a paused pull request says *Paused* in a review's detail and in its repository's list
of open pull requests — which lists it even when its head was reviewed — with **Resume** for
`reviews.manage`: the same as `@<app> resume`, audited as `review.resumed`, and the summary's footer
follows. The API is `paused`, `paused_by` (`auto` or `member`) and `auto_reviews` on
`GET /api/reviews/{id}` and `GET /api/review-pulls`, and `POST /api/review-pulls/resume` with
`{"repo": "owner/name", "pr": 7}`.

## Replies in a finding's thread

Replies are answered only on a pull request whose review is posted live. A reply to one of the
review's inline comments is sorted, on the default model, into one of five kinds:

- **thanks** gets a +1 reaction and nothing else.
- **fixed** ("fixed in abc1234") gets no answer: the claim is recorded and shown in the summary, and
  the finding still counts until the next review of a newer head checks it — closed as fixed, with
  *Fixed in* in its thread, or answered *Still present* once — or a person resolves its thread.
- **remember** ("remember that we always …") proposes a rule on the finding's review type, from the
  repository's own people only. It does nothing until somebody accepts it under Reviews › Types.
- **pushback** is judged on the review's model, reading the code at the head with read-only tools:
  withdraw, downgrade, or keep. The answer opens with the verdict and ends with what happened to the
  finding and the score now, both written by Go, so the model's words can explain a change but never
  claim one that was not made. A second pushback that brings nothing new marks the finding
  *disputed*, and it still counts until a person resolves the thread or a later review finds it
  [fixed or gone](#when-a-push-fixes-a-finding).
- **question** is answered the same way, and never changes the finding.

A reply is never answered to a bot, a thread gets three answers at most, a pull request twenty a
day, and one person twenty an hour, or five for somebody who is not one of the repository's own
people. Each answer holds $0.15 of the review budgets while it runs. Every change to a finding is
audited (`review.finding_changed`, naming whose word it was), and the summary and the score follow.

### Who may withdraw a P0 or P1

A P0 or P1 is withdrawn or downgraded only on a reply from one of the repository's own people, or
from the author of a pull request opened from the same repository — never from a fork — and a
withdrawal only on a verdict reached by reading the code at the head. Anybody else gets the
reasoning and an unchanged finding. They can argue a P2 down, and then only when a second look at
the code, shown the finding and the head and not a word of the thread, refutes it as well: a
verdict reached reading a stranger's text must not rest on that text.

Resolving the thread on GitHub is held to the same. A person with write access who resolves it
takes the finding out of the score, and unresolving it puts it back; the author of a pull request
from a fork who resolves the thread of a P0 or P1 leaves it open and counted, and the audit log says
so (`review.finding_kept`).

### Threads resolved on GitHub

A finding that closes has its thread resolved on GitHub, after its answer: one a later review finds
fixed, after *Fixed in*, and one withdrawn on a reply, after *Withdrawn*. The thread is the one a
thread delivery recorded, or else found by listing the pull request's threads through GitHub's
GraphQL API, and each one resolved is audited (`review.thread_resolved`). The bot never unresolves
one: a finding back after its fix was undone is answered *Back at*, and its thread is left for a
person. Nothing is resolved in shadow.

Resolving goes out with the token reviews are posted with, *Pull requests: Read and write*. GitHub
does not document which permission an App needs for it; if GitHub refuses it for want of one, the
answer in the thread still goes out and the thread stays open, the log says so once a day, and the
connection's status card says when it was last refused (`threads_refused_at`) until a thread of
that installation resolves again.

## Starting a review by hand

From the console, with `reviews.manage`:

- **Reviews › History › Start review** opens a dialog: a repository → one of its open pull
  requests, read live from GitHub with its base branch → the review types, ticked as the pull
  request's branch rule and its labels would pick them → **Post** *Live* or *Shadow* → **Read** *Whole PR* or
  *Since last review* → the estimated cost → **Start review**. Left as the rule set them, the
  types and where it posts are the rule's, and the dialog says when review is *Off* for the
  repository, which refuses a start from here too.
- A repository's panel under Settings has **Review a PR…**, the same dialog with the repository
  chosen, and lists its open pull requests that have not been reviewed yet, and those whose
  automatic reviews are paused, with **Resume**.
- A past review in History has **Run again** — the same types and destination, on the head as it is
  now — and **Run with other types…**.

A review somebody asks for skips *When*, drafts and the authors to skip, and still needs the
connection added, fork policy, the budgets and the throttles. Posting *Live* on a repository in
*Shadow* needs `connections.manage`. A review of a head already reviewed with the same types and
settings, to the same place, is answered from that review at no cost, and the dialog says so;
**Run anyway** reviews it again from scratch. *Since last review* still reads the whole pull
request, and holds new minor findings to the files that changed since. **Run again** leaves out a
type turned off since the review it repeats, and says so.

The estimate is a range from the pull request's size, the types and the models' list prices, not a
quote; the run is held to its max $ whatever it says. The API is `POST /api/reviews`
(`{repo, pr, types, post, scope, force}`), `POST /api/reviews/{id}/rerun`,
`GET /api/review-pulls?repo=` and `GET /api/reviews/estimate?repo=&pr=&types=`.

### From a Slack or Teams channel

Ask the bot in a channel — "review acme/web#12", "run a security review on PR 12" — and it starts
one with `github_start_review`, a tool of the GitHub tool pack. The channel decides it:

- The repository has to be one the channel is granted in **Access bundles** — the same list the
  bot's other GitHub tools read from. A repository the channel is not granted is refused by name, and
  nothing is read or queued; a channel with one repository may leave its name out.
- The repository's App connection has to be in the review tree, as for any review.
- Only a person typing starts one: not a turn a forwarded mail or a routine started, and not the
  console's Playground.

It is then held to what a review started from the console is held to — the plan, fork policy, the
budgets and the throttles — and skips *When*, drafts and the authors to skip. It posts where the
repository's mode says; somebody in a channel can ask for *Shadow* on a live repository, never *Live*
on a shadow one. Types may be named (`["security"]`, or their names). A head already reviewed with the
same types and settings is answered from that review, and asking twice in one thread is the run
already queued. The bot answers with what was queued, the pull request and the run in the console;
the review itself lands on GitHub, and in the channel set to hear about it. History lists it with the
trigger *Slack or Teams*, and the audit log as `review.started` by that person.

## Shadow and live

Every connection starts in *Shadow*. The review runs whole — the same model calls, the same checks,
the same cost — and is recorded in the console, and nothing is written to GitHub: no review, no
summary, no reaction to a command, no answer to a command or a reply, no note that the budget ran
out. A team adds a connection, reads what the reviewer would have said beside what its people or its
current reviewer said, and switches it to *Live* once it agrees.

*Live* is the one write attest_tag makes that never goes through the Confirm gate — no allow rule or
`auto` setting is involved ([Guardrails](security.md#code-review-posts-without-a-confirm)) — so
switching it on needs `connections.manage`. Where a review goes is decided per pull request: the repository's mode with
its branch rule applied, so a rule can post one branch live on a repository in shadow, or keep one
branch in shadow on a live one, and commands and replies on that pull request follow its review.
A review started in the console can also be recorded in shadow on a live repository, and that is
never a reason to say anything on the pull request: asked for the `status` there, the bot says only
that a review exists in the console, and nothing of what it found.

## Announcements in a chat channel

Every settings panel has **Notifications**, right under *Reviewing*. Its **Channel** is any channel
of the organisation's connected Slack or Teams workspaces that the bot is in, picked from a list. It
is inherited like any single value — *Inherit*, a channel, or *No announcements* to turn off one set
above — with a chip saying where the one in force is set. In **Branch rules**, a rule's **Notify**
sends the pull requests it matches to a channel of their own (a level nearer the repository that
names its own channel keeps it), or *No announcements* keeps them quiet; *—* uses the settings'
channel. A channel shared with another organisation — Slack Connect — is refused, and one shared
since is never posted in. Changing either needs `connections.manage`, and without it both are shown
disabled with why: the message carries a private repository's findings to whoever reads it. Through
the API the field is `notify`, `{"team": "<workspace>", "channel": "<id>"}`, the team left out when
only one workspace has that channel, and `{}` for none.

**Notify on** picks which events that channel hears of: *Started*, *Finished*, *Failed or not run*
and *Merged*, all four unless a level says otherwise. The set is inherited whole, like a single
value — set at a repository, it replaces its connection's rather than adding to it — and none ticked
tells the channel nothing. A branch rule's own channel hears the same events. It sends nothing
anywhere new, so `reviews.manage` is enough. Through the API it is `notify_on`, a list of `started`,
`finished`, `failed` and `merged`.

### The message and its replies

Each pull request gets **one message**, posted at its first announcement and edited in place after
that to how it stands now: the title, *from → into*, the commit last reviewed, *Confidence N/5*, what
is happening to it, the open findings by severity and the worst three linked to their lines, and
links to the summary on GitHub and — on a deployment with an https address — the review in the
console. Every later event is also one short reply in that message's thread, but a start:

- a review starting, once it is through the gate and its money is held: the message says
  "Reviewing `abc1234` (General, Security)…" — *in shadow* where it is recorded, not posted — with
  the last review's open findings still listed. Nothing is replied: the result is, minutes later;
- every review that finishes, posted or recorded in *Shadow* — the channel is the organisation's
  own, not GitHub, and the message says *shadow*: "Re-reviewed `abc1234`: 2 fixed, 1 open,
  score 2 → 3";
- the pull request being merged: "Merged into `main` by octocat — 1 P1 still open". One no review
  ever read and the channel never heard of — a dependency bump, a draft — is merged in silence.

Nothing in it calls a model. Titles, branches and findings are held to one line, with mentions and
links defused. Announcing a start runs beside the review and never holds it up. A post the platform
refuses never fails the review: it is audited (`review.notify_failed`), the alert channel hears once
an hour, and the next event tries again; a message somebody deleted is posted afresh by the next
event the channel is told of.

### Failed reviews and reviews that did not run

A review that fails — the model provider, GitHub, a timeout, an internal error — puts the message
back to how the last review left it, with "Review of `abc1234` failed: the model provider did not
answer", and replies the same with a link to the run in the console. The error itself stays in the
console.

A review that did not run is announced only when somebody has to act: code review's budget is
spent, the pull request's automatic reviews are paused, or the organisation's plan has no code
review — the plan only for a pull request nothing else would have stopped. Each is said once,
however many pushes meet it, until something else is. A draft, a bot's pull request, an excluded
author, or nothing left to review once the ignored paths are set aside is the settings at work, and
says nothing — except that a review whose start was announced always says how it ended, so the
message never stays on *Reviewing…*: even one cancelled in the queue as its pull request closed, or
given up after the instance running it stopped. With *Failed or not run* left out of **Notify on**,
the message is still put back, quietly, with no reply.

## Money and budgets

Code review spends from budgets of its own, apart from the bot's conversations, so a busy
repository cannot spend what Slack and Teams run on:

- **Max $ per review** (`max_usd`, $1.00 by default, $0.10 to $5.00) is one review's ceiling,
  verification included. The finder may spend 55% of it and the verifier the rest; a type that runs
  low stops on what it found, and files no pass finished are listed as not reviewed. It is a soft
  cap: the calls that land a pass already under way are paid for rather than thrown away.
- **The monthly review budget** (`review_monthly_budget_usd`) is half the account's monthly budget
  unless set, and none when the account has none.
- **The daily review cap** (`review_daily_usd`) is $10 unless set.

Before it starts, a review holds its max $ against all three and against the account's own budget
and credit, counting what reviews already running hold. One that does not fit is skipped as
`budget`: the admins' alert channel hears about it, once an hour at most, and a pull request on a
live repository gets one short note a day, with no figures in it, saying the budget is spent for
now. `0` is no cap of its own; the account's limits still apply. Both are under Settings › Billing ›
**Code review budget** — Settings › Models on a deployment that sells no plans — beside the
account's monthly budget, where an empty box follows the default and **Reset** goes back to it
(`PUT /api/settings`). Changing either needs `connections.manage` as well as `settings.manage`.

On the organisation's **own model key** nothing is drawn from credit, and the budgets are measured
on that key's spend — once its **Code reviews** switch is on ([plans](plans.md#its-own-model-key));
until then every review is skipped as `own_key_off`. Spend shows on Activity, filed under the
repository and the pull request and charged to the pull request's author as `github:<login>`, who
never becomes a seat.

### What a review typically costs

Rough figures for one review type on a pull request of about ten files and 400 changed lines, at
OpenRouter's list prices in October 2026, without prompt caching:

| finder / verifier | per review |
|---|---|
| Advanced model for both (`z-ai/glm-5.3`, the default) | $0.45 to $0.90 |
| `z-ai/glm-5.3-flash` finds, Advanced verifies | $0.15 to $0.33 |
| `z-ai/glm-5.3-flash` for both | $0.03 to $0.07 |

The finder runs on a type's own model when it has one, else on the settings' model, which also
verifies; a cheaper finder is a type model set to the default model or a cheaper one offered to
channels. Each further
review type adds about one more finder pass. Max $ applies to the whole run, and types run in rule
order, so one the money runs out on is listed under *Not reviewed*. A reply in a thread holds
$0.15. The Start review dialog estimates one pull request's range before anything is spent.

## Limits and throttles

| limit | value |
|---|---|
| Reviews a day | 8 on one pull request and 40 on one repository, counting commands, console starts and automatic reviews alike; past that a review is skipped as `throttle` |
| Commands | 10 an hour from one person; a full review once per commit a day |
| Replies answered | 3 in one thread, 20 on one pull request a day, 20 an hour from one person (5 from somebody who is not one of the repository's people) |
| Files read | the 80 most relevant changed files; the rest are listed as not reviewed, which caps the score |
| Inline comments | *Max comments* (8 by default, at most 20), of which at most 3 are P2s; a review of a later head adds at most one new P2 |
| Pre-existing findings | 2 listed per review |
| Context repositories | 5 read per review |
| Skills | 5 per type; 20 files, three folders deep and 256 KB read from one skill; 24,000 characters of a type's skills given to its finder; 30 reads an hour of public repositories without credentials, and 120 Checks an hour, per organisation |
| A push review | waits 90 seconds for the next push; a label's review waits as long |
| Automatic reviews | 5 on one pull request, then paused until `@<app> resume` |
| Branch rules | 20 per level; 10 types and 10 labels per rule, a label up to 50 characters |
| Review types | 40 rules each, 400 characters per rule |
| Settings lists | 50 entries each, 400 characters per entry |
| Webhook inbox | 4,096 deliveries waiting for the deployment, 64 per organisation |

## Pull requests from forks

A fork's code is a stranger's, and reviewing it spends the organisation's money, so a pull request
from a fork is never reviewed without somebody asking. With *Forks: When a member asks*, the
default, a `@<app> review` from the repository's own people or a console start reviews it; with
*Never* nothing does. Changing it needs `connections.manage`. The author of a pull request from a fork can neither
withdraw nor downgrade a P0 or P1 by replying, nor close one by resolving its thread: they may have
no access to the repository at all.

## Troubleshooting

### GitHub lists a delivery as failed

The App's settings at GitHub, under *Advanced › Recent Deliveries*, show every delivery with the
answer it got:

- **401 invalid signature** — the secret at GitHub is neither `GITHUB_APP_WEBHOOK_SECRET` nor
  `_PREVIOUS`. Make the two the same. After sixty failures in an hour from one address the answer is
  429.
- **400** — the delivery was sent for another App (its target id is not `GITHUB_APP_ID`: one secret
  reused on two Apps), or its headers or its payload are malformed.
- **413** — a body over the cap for its event.
- **503** — `GITHUB_APP_WEBHOOK_SECRET` is unset, the deployment is in maintenance, the database
  could not be read in time, the inbox is full, or too many bodies were being read at once.

Every one of these can be sent again with **Redeliver** once its cause is gone; a delivery that was
already stored is not acted on twice.

### Delivered, but the pull request was not reviewed

A `200` whose body begins `ignored:` says why: *this installation is not connected to an
organisation here* (the App was installed at GitHub but the install was never finished in the
console), *this installation is not reviewed* (it is not under Reviews › Settings), *review is off
for this repository*, *made by a bot*, *a comment on an issue, not on a pull request*, or *code
review is off on this deployment* (`CODE_REVIEW=off`).

A delivery that was kept can still end in no review. The reason is stored on the pull request, shown
in the console and by `@<app> status`:

- `trigger`, `draft`, `bot`, `excluded_author` — the *When* setting, drafts or the authors to skip
  turned away a review nobody asked for; a command is not turned away by these;
- `fork`, `no_rule`, `types_off` — a fork, no matching rule, or every type the rule names off;
- `budget`, `throttle` — the money or the daily caps;
- `own_key_off` — the own model key's *Code reviews* switch is off;
- `plan` — the organisation's plan does not have code review ([who has it](#who-has-code-review));
- `paused` — its automatic reviews are paused ([pausing](#pausing-automatic-reviews));
- `installation_mismatch` — the organisation's App connection names the repository under another
  installation: it moved accounts;
- `removed` — the repository was removed from code review;
- `nothing_to_review` — every changed file is ignored, binary or generated.

Commands and replies are answered only where the pull request's review is posted live: on one in
shadow, silence is the design.

### Reviews that stop short

- **GitHub refuses the anchors** with a 422 that does not say which: the files are read again, the
  anchors that moved go to the summary, and the review is tried once more. If that fails too,
  nothing is posted inline and every finding is in the summary.
- **Missing permissions**: the connection's status card warns until the installation's owner accepts
  the App's new permissions at GitHub. An eyes reaction that never appears on a command is GitHub
  refusing the reaction, and changes nothing else.
- **"review failed: the model provider did not answer"** — the provider failed twice in a row. A key
  at its limit fails every review the same way until somebody acts, so the alert channel hears about
  it, once an hour at most.
- **GitHub's rate limit** puts the run back for the time GitHub gives, and what the model found is
  posted then without being paid for again.

## Self-hosting

- GitHub has to reach the deployment: it needs a public https address
  ([HTTPS](../deploy/docs/https.md)), and a deployment on localhost never gets a delivery.
- One App, one webhook URL. Two deployments sharing an App — a laptop and production — share its
  webhook, so only one of them hears GitHub; give each its own App and its own secret.
- On a deployment that does not take open sign-ups, the console shows the exact webhook URL to
  paste and whether the secret is set (`webhook_url` and `webhook_secret_set` on
  `GET /api/github/installations`). With `SIGNUP_MODE=open` it shows neither: a tenant neither set
  up the App nor can.
- The catch-up runs only beside the webhook: with no secret nothing is reviewed automatically at
  all.
- Reviews run in the bot's own process, two at a time per instance, outside the cap on
  conversations. More instances share the queue, and a pull request has one review running at a
  time whichever instance runs it.

## Not built yet

- Answers to free-form questions put to `@<app>`.
- A status check, `/v1` routes and MCP tools for reviews.
- Recovering a command or a reply sent while the deployment was down.
- Settings kept in a file in the repository.
- `@<app> review path:<glob>`, reviewing part of a pull request, and a summary-only review of one
  too large to read whole.
- A copy of the summary in the pull request's description.
- `remember` and `forget` as commands of their own, rules learned for one repository only, and how
  often each rule is cited, withdrawn or fixed.
- `@<app> fix`, handing a finding to a fix job.
- Polling GitHub for a deployment its webhook cannot reach.
