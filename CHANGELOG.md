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
