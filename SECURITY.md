# Security

## Reporting a vulnerability

**Please do not open a public issue.**

Use GitHub's private vulnerability reporting — the **Report a vulnerability** button under this
repository's Security tab — or write to **security@attesttag.com**.

Tell me what you did, what happened, and what you expected. A proof of concept helps enormously
and does not need to be polished. I will acknowledge within a few days and tell you what I think
the impact is; if I disagree that it is a vulnerability I will say why rather than going quiet.

There is no bounty. I will credit you in the advisory for the fix unless you would rather I did
not.

## Supported versions

The latest release and `main`. This is a young project and there is no long-term support branch:
fixes land on `main` and go out in the next release rather than being backported.

## What this software is responsible for

Worth knowing before you go looking, because it shapes what counts as a vulnerability here.

**It holds other people's credentials.** Connection secrets, Slack bot tokens, per-user OAuth
tokens, TOTP secrets, single sign-on client secrets and organisations' own model keys are sealed
with AES-256-GCM under `MASTER_KEY`. The model never sees them: a tool call goes through a proxy
that injects the credential at the edge, over https only. Anything that gets a secret in front of
the model, into a log, or across an organisation boundary is a serious finding.

**It is multi-tenant.** One deployment can hold many organisations — the hosted service does —
and the boundary between them is carried by every query. `TestEveryPerOrgQueryIsScoped` fails
the build on a statement that touches a tenant's table without an organisation in its predicate,
and `isolation_test.go` asserts the boundary holds for lists, reads by id, writes, attachments,
sessions, alerts and spend. A path that crosses it is the highest-severity thing you can find.

**It issues credentials that act as people.** Developer keys, and MCP connections made through its
own OAuth (dynamic registration, PKCE, rotating refresh tokens), act as the person who made or
approved them, with that person's role in that organisation, re-read on every request; only their
hashes are stored. A token obtained without the person's consent, one that reaches past its
person's permissions or organisation, or a spent refresh token that still works, is a serious
finding.

**It runs code the model wrote.** JavaScript from the model runs in QuickJS compiled to
WebAssembly under wazero — no syscalls, no filesystem, no network except a host-provided fetch
with its own budget. Fix jobs are different: they clone a repository and run *its* code, which
is why the worker drops to an unprivileged user before doing so, and why its git runs as that
same user so a config key the repository plants cannot execute as root. That split happens in
every container mode; `WORKER_MODE=local` is a developer's subprocess and runs the repository's
code as the bot's own user. The engine is given a model credential: a per-job key capped at the
job's budget when `OPENROUTER_PROVISIONING_KEY` is set, otherwise the shared key
(`WORKER_LLM_API_KEY`, else `OPENROUTER_API_KEY`) — set the provisioning key on any deployment
that runs fix jobs and is open to signup, and the dispatcher refuses the shared key there. An
organisation that brought its own model key and allowed fix jobs on it hands the worker that
key, which cannot be capped per job.

**It reads untrusted text.** Documents, web pages, Slack and Teams messages, mail forwarded into
a channel and API responses all reach the model, and some of them will try to give it
instructions. `docs/injection-test.md` is a deliberate prompt-injection payload used for
testing, not a mistake — please leave it there. Prompt injection that causes the model to say
something silly is expected. Prompt injection that reaches a credential, writes without the
confirmation step, or crosses a tenant boundary is a vulnerability, and I want to hear about it.

## If you run this yourself

Three things decide whether a self-hosted deployment is safe. The first two default to the
careful answer; the third is yours to set:

- **`MASTER_KEY` unlocks every stored credential.** Back it up somewhere other than the machine
  it runs on. Lose it and every workspace has to be connected again, every connection and stored
  key entered again, and everybody has to enrol their second factor again. The container image
  refuses to start without one rather than inventing one it would lose.
- **`SIGNUP_MODE` defaults to `first-run`** — you sign up once and everybody else is invited.
  Set it to `open` only if you actually mean to run a service for strangers.
- **Set `ADMIN_BASE_URL`** (or `PUBLIC_ORIGIN_HOSTS`) when you are behind a reverse proxy.
  Otherwise the first signed-in console request pins the public origin for the whole deployment,
  and that origin is what sign-in redirects, password-reset and invitation links are built from.

Each organisation holds the bot to its own people with the email-domain list under Settings →
Security, which starts as the domain its founder signed up with (unless that is a public mail
provider); `ALLOWED_EMAIL_DOMAINS` is only the default for an organisation that keeps no list of
its own. Even with neither, the bot refuses guests and members from other workspaces (Slack
Connect, or another Microsoft 365 organisation) unless an admin lets them in on the same page.
