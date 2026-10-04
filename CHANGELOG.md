# Changelog

What each release of attest_tag changed, and what, if anything, an upgrade asks of you. Each
[GitHub release](https://github.com/Attest-Tag/attesttag/releases) opens with its section of
this file.

Versions follow [semantic versioning](https://semver.org). Before 1.0 that means:

- A **patch** release (0.1.1) only fixes things and never migrates the database. A deployment
  pinned to its minor version (`ATTEST_VERSION=0.1` in compose's `.env`, `image.tag: "0.1"` in
  the chart) can restart onto one without reading anything first.
- A **minor** release (0.2.0) may migrate the database or change something you have to act on,
  and its section here says which. Migrations apply by themselves at startup and there are no
  down migrations, so back the database up before a minor upgrade: going back means restoring
  that backup.

## 0.2.0 (2026-10-04)

Like 0.1.0, the source alone: no images, Helm chart or binaries are published for it, so build from
the checkout as the deploy guides say.

**Code review.** attest_tag reviews pull requests on GitHub, through the GitHub App it already
installs. → [Code review](guide/code-review.md)

- **One review per run, as a comment.** Inline findings, each checked by Go against the code and,
  when a model raised it, by a second model call that tries to refute it, and one summary comment
  edited in place, with a confidence score from 0 to 5 that Go computes from the open findings. It
  never approves or requests changes, and never runs the pull request's code.
- **Settings by connection, group and repository**, under Automation › Reviews. An installation
  added there starts in shadow, which records the whole review in the console and writes nothing to
  GitHub. A single value comes from the nearest level that sets it; lists add up.
- **Review types and branch rules.** General, Security, Tests, Performance and Release summary ship
  built in; a team can edit them, add its own, keep every version, and try an edit on a pull request
  before saving it. Ordered rules on the base and head branch choose which types a pull request
  gets, and label rules add types for a label on it — put on later, a label reviews what it adds.
- **Skills.** A type can follow up to five skill folders — a `SKILL.md` and the Markdown beside it —
  from the repository under review (read at the pull request's base), one of the organisation's
  connected repositories, or any public repository, pinned to a commit if you like; a finding that
  rests on one cites it, and the summary names the skills the review followed.
- **Commands and replies.** `@<app> review`, `full review`, `status`, `help`, `pause` and `resume`
  from the repository's own people, and a verdict on every reply in a finding's thread, which can
  withdraw or downgrade the finding within limits on who may move a P0 or P1. A finding that is
  fixed or withdrawn has its thread resolved on GitHub where the App's token is allowed to (best
  effort; the connection card says when GitHub refuses). Automatic reviews of one pull request pause
  by themselves after five, and the summary says so.
- **Re-reviews close what a push fixed.** Each later review checks the earlier findings at the new
  head: a fixed one moves to the summary's *Fixed* with *Fixed in* in its thread, one whose code is
  gone to *Outdated*, and a "fixed in" claim is checked rather than taken at its word.
- **Start review** in the console, with an estimated cost, and **Run again** on a past review. A
  paused pull request says *Paused* there, with **Resume**; a branch rule takes **Labels**, and reads
  `label perf → +Performance`; and where the plan has no code review the Reviews page says which plan
  has it, with the way to upgrade, while `CODE_REVIEW=off` takes the page out of the console.
- **Reviews asked for in Slack or Teams.** "review acme/web#12" in a channel starts one through the
  bot's GitHub tools, for a repository that channel is granted in Access bundles and nothing else,
  under the same gate, budgets and throttles as the console's Start review. → [Code review](guide/code-review.md#from-a-slack-or-teams-channel)
- **Announcements in a chat channel.** A level's *Channel*, or a branch rule's *Notify*, names a
  Slack or Teams channel the bot is in; each pull request gets one message there, edited as reviews
  finish and when it is merged, with each of those a reply in its thread. Choosing it needs
  `connections.manage`, and a channel shared with another organisation (Slack Connect) is never
  posted in.
- **Starts, failures and Notify on.** A review's start edits the message to *Reviewing `abc1234`…*
  with no reply and without holding the review up; a failed review puts it back and says which kind
  of failure, never the error; a review the budget, a pause or the plan stopped is said once. *Notify
  on* picks which of starts, results, failures and merges a channel hears, inherited whole, and
  needs only `reviews.manage`. A console link that names a settings node opens on Settings.
- **Adding and removing repositories.** *Add repositories…* on a connection lists what its
  installation reaches at GitHub and saves the ones ticked; *Remove from reviews…* takes one out of
  code review while its connection stays for Slack tools and fix jobs, and *Restore* puts it back.
- **The console assistant on Reviews.** With a type or a level open, ask it to add, change or switch
  off a type's rules, make a new type, or add, change, move or remove a level's branch rules. It
  proposes a card and writes nothing; Confirm makes the page's own save under the person's own
  permissions, and is refused with nothing written if the type, the type a new one copies, or the
  rules changed since; the page then reads again and shows the change that won. It names who wrote
  a rule learned on GitHub, while the rule keeps their words, and which skills a type follows; skills,
  models, budgets and posting stay on the page. A card is confirmed only in the organisation it
  was proposed in, draws every path pattern it adds or removes and the model and budget a copy
  brings, and says when Confirm uses your `connections.manage`; Activity shows the assistant's
  tool-call arguments only to a reader who also holds `audit.view` and what the call was about,
  and only such a reader may search what it was asked.
  The assistant is offered to anyone with `reviews.view` and `reviews.manage` now, as well as to
  channel and approver admins, and what it is told about code review is sent from the Reviews page
  only. → [Changing types and rules from the assistant](guide/code-review.md#changing-types-and-rules-from-the-assistant)
- **A settings save never undoes a newer one.** If somebody saved a level's rules, or the rules it
  inherits, since the page read them, Save is refused and the console keeps your edits over theirs;
  through the API, `digests.branch_rules` sent back as `expect` does the same. A save of any field
  that lands while another save of the level is being checked refuses that one too, rather than
  being put back by it.
- **Money of its own:** a maximum per review, a monthly review budget and a daily cap, each held
  before a review starts, and a *Code reviews* switch on an organisation's own model key.
  `CODE_REVIEW` says which organisations have it at all: every one (`all`, the default), those on
  the pro or enterprise plan, the enterprise plan only, or none (`off`).
- **The one write that never goes through Confirm.** A review on a repository set to *Live* is
  posted with nobody pressing anything, and no allow rule or `auto` setting is involved; *Live*, the model and the money need `connections.manage`.
  [Guardrails](guide/security.md#code-review-posts-without-a-confirm) says what stands in for the
  Confirm.

**Security fixes**, found by an audit of the code review change, of which the first two were in
0.1.0 too:

- Attaching a GitHub App installation to an organisation now needs its account's owner, or somebody
  with admin on every repository it covers. It used to need only that the installation was visible
  to the person, so an organisation's member or outside collaborator could attach one nobody had
  claimed yet, with its tokens.
- A member without `audit.view` can no longer search the console assistant's questions and replies,
  which told them, a guess at a time, what a redacted reply said.
- A label rule no longer hides a branch-rule change from the check that keeps live posting with
  `connections.manage`.

What an upgrade asks of you:

- Five migrations, `0027_code_review`, `0028_review_lane`, `0029_review_snippet`,
  `0030_review_notify` and `0031_review_type_skills`, all additive and applied at startup.
  Back the database up first, as for any minor release.
- Nothing more, unless you want code review. Then set `GITHUB_APP_WEBHOOK_SECRET`, turn the App's
  webhook on at GitHub with the five events
  [configuration](guide/configuration.md#the-apps-webhook-and-permissions-for-code-review) lists,
  and add an installation under Automation › Reviews.
- Connecting a GitHub App installation (Connect GitHub, or the install redirect) takes the account's
  owner or an admin of every repository it covers; somebody who only requested the install is told
  to ask one of them. Installations already connected are not affected.
- Two new permissions: `reviews.view`, which the viewer, editor and admin roles hold, and
  `reviews.manage`, which editor and admin hold. A role you made yourself has neither until somebody
  ticks them.

## 0.1.0 (2026-09-28)

The first release. It is the code that has run one organisation's Slack for months and runs the
hosted service, with everything that was specific to that deployment turned into configuration.

It is the source alone: no images, Helm chart or binaries are published for it yet, so build
from the checkout as the deploy guides say (`docker build -t attesttag-local .`, or
`make build`).

- **Answers in Slack and Microsoft Teams**, in threads and DMs, on open-weight models through any
  OpenAI-compatible endpoint (OpenRouter and GLM-5.3-Flash by default).
- **Tools with no setup:** Slack history and search, your documents, web search and fetch, memory
  per channel or per person, and routines it schedules when asked.
- **Connections without handing over a key.** Credentials for thirty-odd services and remote MCP
  servers are sealed at rest and injected by a proxy at the network edge, so the model never
  sees one, and every write waits for somebody to press Confirm.
- **Fix jobs:** an isolated worker clones a repository, makes the change, runs its tests and
  opens a draft pull request. It never merges.
- **An admin console** for scopes, connections, documents, routines, activity, the audit log and
  budgets, with password, Slack, Microsoft or OpenID Connect sign-in and two-factor.
- **A `/v1` API and an MCP server**, with keys that carry exactly their maker's access.
- **SQLite or Postgres.** SQLite by default, replicated to any S3-compatible bucket if you give
  it one; Postgres when you want more than one instance.
- **Runs anywhere a container does:** Docker Compose, a Helm chart, and scripts for Cloud Run,
  ECS Fargate and Azure Container Apps. Or run the binary itself.

What is still rough, and what is wanted next, is in the README's
[Status](https://github.com/Attest-Tag/attesttag#status).
