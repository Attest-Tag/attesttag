# attest_tag documentation

The [README](../README.md) is the two-minute version. This is the rest of it.

> **Why not `docs/`?** That directory is the bot's own document corpus — whatever is in it gets
> chunked, embedded and answered from. Prose about the software would come back as company
> policy, so the documentation lives here instead.

## Getting it running

| | |
|---|---|
| [**Make your own Slack app**](slack-app.md) | From an empty dashboard to a bot answering in a channel. Every scope explained, and what breaks without it |
| [**Microsoft Teams**](msteams.md) | The same bot in Teams: registering it with Microsoft, connecting an organisation, what its admin has to allow, and what is different from Slack |
| [**Installing it with a coding agent**](install-with-an-agent.md) | Clone, point your agent at the directory, say where you want it. The runbook it follows, and the three things only you can do |
| [Deploying it](../deploy/README.md) | A folder per platform — Docker, GCP, AWS, Azure, Kubernetes — storage, and the three things that decide whether a deployment works |
| [Getting an HTTPS address](../deploy/docs/https.md) | Not optional. Three ways, the shortest needs no account |
| [Storage](../deploy/docs/storage.md) | Local by default; bring a Postgres and a bucket when you want more |
| [Building from source](development.md) | Go and Node toolchains, running it locally, the tests and the evals |

## Using it

| | |
|---|---|
| [Using it in Slack](slack.md) | Threads, tools, artifacts, memory, routines, bang commands, budgets, investigations |
| [Connections](connections.md) | Reaching GitHub, ClickUp, Google Workspace and the rest without the model ever holding a credential |
| [**Google**](google.md) | Two Google integrations that get confused for each other: mail and calendar on each person's own account, and a Drive folder mirrored into Documents |
| [Fix jobs](fix-jobs.md) | Handing a code change to a worker that clones, edits, tests and opens a draft pull request |
| [Admin console](console.md) | The pages, signing in, and the member-facing Configure page |
| [Developer API](api.md) | `/v1`, authenticated with a key that carries its maker's access and nothing more |
| [MCP server](mcp.md) | The same API as tools for Claude, Cursor and any MCP client, connected with OAuth or a key |

## Running it

| | |
|---|---|
| [Configuration](configuration.md) | Every environment variable, every console setting, and how documents are indexed |
| [Deploy](deploy.md) | What every deployment needs, the env file and the hosted service's, the image, launchd, plain `docker run`, Cloud Run in detail, upgrading |
| [Plans and the operator API](plans.md) | Only for a deployment handing accounts to strangers. Off by default |
| [Billing](billing.md) | Selling a plan by plan size and prepaid credit, with Stripe. Off by default |
| [Guardrails](security.md) | What stops a prompt-injected tool result, a stolen key, or an approver approving their own request |

## Understanding it

| | |
|---|---|
| [Architecture](architecture.md) | The process, what every file is for, and what the database holds |
| [Contributing](../CONTRIBUTING.md) | Getting it running, what the guard tests are for, house style |
| [Security policy](../SECURITY.md) | Where to send a vulnerability, and what counts |
