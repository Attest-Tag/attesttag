# Installing it with a coding agent

You can hand this to Claude Code, Codex, Cursor, Aider, Copilot CLI — anything that reads a
repository and runs shell commands — and say where you want it. Two commands and one sentence:

```bash
git clone https://github.com/Attest-Tag/attesttag && cd attesttag
```

Open your agent **in that directory**, so the repository is its working directory, then say:

> Read `AGENTS.md`, then install this on Google Cloud.

Swap in AWS, Azure, Kubernetes, or "this machine with Docker". The agent reads
[`AGENTS.md`](../AGENTS.md), which sends it to the runbook at the bottom of this page.

This page is written for both of you. The top half tells you what to expect and what it will
stop and ask for; the runbook below is what the agent follows.

---

## What it cannot do for you

Three things need a human, and an agent that claims otherwise is wrong:

**Creating the Slack app.** It lives in your workspace under your account. The agent prints the
manifest and tells you exactly where to paste it; you click. It takes about a minute.

**Signing in to your cloud.** `gcloud auth login`, `aws configure`, `az login` — all of them open
a browser and want your credentials. Do that first, or the agent will stop on the first command
and ask.

**Backing up `MASTER_KEY`.** It seals every stored credential with AES-256-GCM. If it is lost,
every Slack workspace and every saved connection has to be reconnected by hand — there is no
recovery, by design. The agent will generate one and then stop until you say you have put it
somewhere other than that machine. Do not skip this; it is the one step with no second chance.

## What you will need

| | |
|---|---|
| A Slack workspace | and the ability to install an app into it (owner, admin, or an approver) |
| An LLM key | [openrouter.ai/keys](https://openrouter.ai/keys), or any OpenAI-compatible endpoint via `LLM_BASE_URL` |
| For a cloud install | an authenticated CLI and a project/account that can be billed |
| For AWS | a domain you can add a CNAME to — the load balancer has no certificate without one |
| For Azure | somewhere durable for the data: an S3-compatible bucket elsewhere (R2, B2, Wasabi) or a Postgres, which the script can create for you |
| For Kubernetes | an ingress controller, and cert-manager or a TLS certificate for the host |
| For GCP | nothing extra; Cloud Run hands out an https hostname |

Microsoft Teams is optional and comes after: [msteams.md](msteams.md) adds it to a running
deployment.

## Rules it has been given

The runbook tells the agent, in writing, to:

- **Never print a secret**, never paste one into chat, never commit one. `.env` is gitignored
  (`.env*`), and it writes values there with a shell redirect rather than echoing them back.
- **Stop before creating anything billable** and show you the list first.
- **Never deploy to a project or account you did not name.** Not the CLI's currently-selected
  one, unless you said so.
- **Never run `deploy/gcp/cloudrun.sh` against the maintainer's hosted service.** It reads
  `.env.prod` when one exists, which is that service's file; a self-host passes `ENV_FILE=.env`
  and never sets `HOSTED=1`, which turns on that service's public policy.
- **Stop and report** rather than improvise, if a script fails.

Agents are not bound by a document — these are instructions, not a sandbox. Run it where you can
see what it does, and read the confirmation prompts rather than clicking through them.

## Following along

The whole thing is about ten to twenty minutes, most of it waiting for an image to build:

1. **Preflight** — checks the tools, your CLI login, and whether the container image can be
   pulled or has to be built from source.
2. **Secrets** — runs `deploy/local/bootstrap.sh`, which writes `.env` and generates
   `MASTER_KEY`. **Stops here** for your backup, and for your LLM key.
3. **The public origin** — how it gets one depends on the platform; see the table in the runbook.
4. **The Slack app** — prints the filled-in manifest, waits while you create the app, then takes
   the three credentials back from you.
5. **Deploy** — one script per platform, all idempotent.
6. **Verify** — `/health`, then the console, then a message in Slack.

## When it says it is done

Check it yourself; it takes thirty seconds:

```bash
curl -fsS https://YOUR_ORIGIN/health          # 200, body: ok
```

Then open `https://YOUR_ORIGIN/`, sign up — **the first sign-up founds the deployment**, and
everyone after arrives by invitation — connect your Slack workspace on the Workspaces page
(**Add workspace → Slack**), and `@mention` the bot in a channel. If it answers, it is installed.

If the bot is silent in Slack, the deployment is almost certainly fine and the Slack app's URLs
are wrong: they must point at the origin you actually deployed to. [Make your own Slack
app](slack-app.md) covers every field.

---

# The runbook

**Everything below is addressed to the agent.**

You are installing attest_tag, a Go binary with an embedded admin console that answers in Slack
(and optionally Microsoft Teams). Work through these steps in order. Do not skip a stop point. If
a step fails, stop and report what failed and what you saw — do not improvise around a failing
deploy script.

## Rules

1. Never print, echo, log or commit a secret, and never read one back to the user. Write a secret
   into `.env` by **replacing its line** rather than appending one: the template already holds an
   empty line for each key, and one line per key is the only shape every reader agrees on — the
   deploy scripts take the last line with a value, the bot the last line. The pattern, used
   throughout below:
   `{ grep -v '^KEY=' .env; printf '%s\n' "KEY=$VALUE"; } > .env.new && chmod 600 .env.new && mv .env.new .env`.
   `.env*` is already gitignored; confirm that before writing.
2. Never create a billable cloud resource before showing the user what will be created and
   getting an explicit yes.
3. Deploy only to the project, account or subscription the user named. If they did not name one,
   ask — do not use whatever the CLI currently has selected, and pass the region or location they
   chose explicitly: the scripts otherwise default to `us-central1` (GCP), `us-east-1` (AWS) and
   `eastus` (Azure).
4. `deploy/gcp/cloudrun.sh` and `deploy/plan.sh` read `.env.prod` when it exists, which is the
   maintainer's hosted service, and `.env` otherwise. A self-host always passes `ENV_FILE=.env`
   and never sets `HOSTED=1`, which turns on the hosted service's public policy (open signup, a
   support address at attesttag.com). Without it the deployment is one organisation.
5. Do not edit the deploy scripts to get past an error. They are idempotent; fix the cause and
   rerun.
6. `MASTER_KEY` is unrecoverable. Never regenerate one for a deployment that already has data.

## Step 0 — preflight

Confirm the target platform with the user if it is not already clear. Then:

```bash
# Tools. Only the rows for the chosen platform have to pass.
openssl version                                       # every platform: bootstrap.sh generates MASTER_KEY with it
docker --version                                      # Docker and AWS (the image is pushed to ECR)
zsh --version && gcloud version && gcloud config get-value project   # GCP: the scripts are zsh
python3 --version && aws --version && aws sts get-caller-identity    # AWS
python3 --version && az version && az account show                   # Azure
helm version && kubectl config current-context                       # Kubernetes
```

Then, except on GCP — which always builds this checkout on Cloud Build — check whether the
published image can be pulled anonymously:

```bash
docker manifest inspect ghcr.io/attest-tag/attesttag:latest >/dev/null 2>&1 && echo published || echo "build from source"
```

If it cannot be pulled, the image has not been released yet. That is expected on a fresh
checkout and is not an error — build locally instead:

- **Docker/compose:** `docker build -t attesttag-local .` now, and after step 1 add
  `ATTEST_IMAGE=attesttag-local` to `.env` (an `export` does not survive to a later command).
- **AWS:** pass `IMAGE_SOURCE=build` to `fargate.sh`, which builds this checkout and pushes to ECR.
- **Azure:** build and push to a registry the Container App can pull from **anonymously** — the
  app is created with no registry credentials — and pass `IMAGE=<that image>`.
- **Kubernetes:** build, push to a registry the cluster can read, and set `image.repository` and
  `image.tag` (and `imagePullSecrets` for a private registry).

The Dockerfile builds the console itself. Outside a container, `make build` builds the console
before the binary, which is embedded with `//go:embed`; a plain `go build` on a tree that has
never built the console either fails or produces a binary whose console does not load.

## Step 1 — secrets

```bash
./deploy/local/bootstrap.sh
```

It writes `.env` from `deploy/env/selfhost.env.example` and generates `MASTER_KEY`. It refuses
to overwrite an existing `.env` — if it refuses, do not move the old file aside without asking;
that key may be the only thing that can read an existing deployment's data.

**STOP.** Print the `MASTER_KEY` line's *location* (not its value) and tell the user to copy it
somewhere other than this machine. Wait for confirmation before continuing.

Then the LLM key. **Prefer the version where you never see it**: give the user this command
to run in their own terminal, rather than asking them to paste the key to you.

```bash
read -rs V && { grep -v '^OPENROUTER_API_KEY=' .env; printf '%s\n' "OPENROUTER_API_KEY=$V"; } > .env.new && chmod 600 .env.new && mv .env.new .env; unset V
```

(`read` waits on a terminal, so this only works when the user runs it — not as a tool call.)
If they paste the key to you anyway, write it in the same way and never repeat it back:

```bash
{ grep -v '^OPENROUTER_API_KEY=' .env; printf '%s\n' 'OPENROUTER_API_KEY=<value>'; } > .env.new && chmod 600 .env.new && mv .env.new .env
```

Who the bot answers is set per organisation after sign-up, under Settings → Security: a new
organisation starts with the email domain its founder signed up with (unless that is a public
mail provider such as gmail.com), and guests and members from other workspaces are refused unless
an admin lets them in. `ALLOWED_EMAIL_DOMAINS` in `.env` is only the default for an organisation
that keeps no list; leave it unset unless the user asks for one.

## Step 2 — the public origin

Slack will not deliver to localhost and will not accept an http redirect URL. How you get an
origin, and *when you know it*, differs — and that decides whether this is a one-pass or
two-pass install:

| Platform | Origin | Known before deploying? |
|---|---|---|
| Docker, quick tunnel | `*.trycloudflare.com` from the tunnel's logs | after `up -d`, before any Slack setup — one pass |
| Docker, own domain | yours, via the `tunnel` or `caddy` profile | yes — one pass |
| AWS | the `DOMAIN` you supply; the script requires it | yes — one pass |
| Azure | `*.azurecontainerapps.io`, or a `DOMAIN` you supply | only with a `DOMAIN` |
| GCP | the assigned `*.run.app`, or a custom domain | only with a custom domain |
| Kubernetes | the `ingress.host` you supply | yes — one pass |

Pin the origin once you know it, because every link the bot builds and every sign-in redirect
comes from it: on Docker and GCP put `ADMIN_BASE_URL=https://<origin>` in `.env` (replacing the
line, as above); on Azure it comes from `DOMAIN`; AWS and the Helm chart set it from the domain
and host they were given.

**Docker, quick tunnel:** the bot's container restarts in a loop until `.env` holds the Slack
values and the model key — that is expected. The tunnel prints its address regardless.

**Two-pass** (GCP or Azure without a domain): the deploy scripts refuse to run without
`SLACK_SIGNING_SECRET`, `SLACK_CLIENT_ID` and `SLACK_CLIENT_SECRET`, and you cannot have them
before the app exists — which needs the URL. So write a throwaway placeholder into each of the
three lines, deploy, read the real origin out of the deploy output, do step 3, replace the
placeholders with the real values, and rerun the same script:

```bash
for K in SLACK_SIGNING_SECRET SLACK_CLIENT_ID SLACK_CLIENT_SECRET; do
  { grep -v "^$K=" .env; printf '%s\n' "$K=placeholder"; } > .env.new && chmod 600 .env.new && mv .env.new .env
done
```

The scripts are idempotent, and the second pass is the normal path, not a workaround — on GCP it
rebuilds the image on Cloud Build, so it takes as long as the first.

Tell the user which of the two they are on before you start, so a second deploy is not a surprise.

## Step 3 — the Slack app

This is the user's to do. Print the manifest with their origin filled in:

```bash
BASE_URL=https://THEIR_ORIGIN ./deploy/slack/manifest.sh
```

Give them these instructions verbatim:

> Go to api.slack.com/apps → **Create New App** → **From a manifest**, pick the workspace, paste
> the JSON above, and create it. Do **not** press *Install to Workspace*: the workspace is
> connected from the console once it is running, which is how the bot receives its token. From
> **Basic Information → App Credentials**, copy the **Signing Secret**, **Client ID** and
> **Client Secret**.

Three values come back: `SLACK_SIGNING_SECRET`, `SLACK_CLIENT_ID`, `SLACK_CLIENT_SECRET`. Same
preference as the LLM key — offer them the terminal version first, so the secrets never enter
the transcript:

```bash
for K in SLACK_SIGNING_SECRET SLACK_CLIENT_ID SLACK_CLIENT_SECRET; do
  printf '%s: ' "$K"; read -rs V; echo
  { grep -v "^$K=" .env; printf '%s\n' "$K=$V"; } > .env.new && chmod 600 .env.new && mv .env.new .env
done; unset V
```

If they paste them to you instead, replace each line the same way and never echo them back.

`guide/slack-app.md` explains every scope and what breaks without it — read it if they ask.

## Step 4 — deploy

Show the user what will be created, get a yes, then run exactly one of these from the repository
root. Each folder's README covers the detail.

```bash
# Docker — this machine, or one VM
docker compose --profile quicktunnel up -d
docker compose logs quicktunnel          # the https URL

# Google Cloud — Cloud Run
ENV_FILE=.env PROJECT=<their-project> REGION=<their-region> ./deploy/gcp/cloudrun.sh

# AWS — ECS Fargate behind a load balancer
AWS_PROFILE=<their-profile> AWS_REGION=<their-region> DOMAIN=bot.example.com ./deploy/aws/fargate.sh

# Azure — Container Apps (select the subscription first: az account set --subscription <id>)
LOCATION=<their-location> DOMAIN=bot.example.com ./deploy/azure/containerapps.sh

# Kubernetes — the secrets go in a Secret of your own, never on the command line
kubectl create namespace attest-tag
kubectl create secret generic attest-tag-secrets -n attest-tag \
  --from-env-file=<(grep -E '^(MASTER_KEY|SLACK_SIGNING_SECRET|SLACK_CLIENT_ID|SLACK_CLIENT_SECRET|OPENROUTER_API_KEY)=.' .env)
helm install attest-tag ./deploy/helm/attest-tag -n attest-tag \
  --set ingress.host=bot.example.com --set existingSecret=attest-tag-secrets
```

On Docker, after step 3 run the same `docker compose --profile quicktunnel up -d` again: it
recreates the bot with the new `.env` and leaves the tunnel, and its address, alone. (`restart`
does not reread `.env`.)

### Platform requirements to check first

Platform-specific requirements you must check *before* running, because the script will refuse
and the reason is not obvious from the error alone:

- **Azure** needs somewhere durable, because Azure Blob has no S3 API and Azure Files is SMB,
  which SQLite must not be run on. Ask which of three the user wants: `DOCS_S3_URL` with
  `DOCS_S3_KEY_ID` and `DOCS_S3_SECRET` (a bucket at R2, B2, Wasabi or MinIO); `DATABASE_URL` (an
  Azure Database for PostgreSQL they already have); or `CREATE_DATABASE=1`, which makes one. The
  last is billable: run without a terminal, it prints what it will create and what it costs and
  then refuses — show the user that, get a yes, and rerun with `ASSUME_YES=1`.
- **AWS** needs `DOMAIN`, and the first run **exits non-zero on purpose** after printing an ACM
  validation CNAME. That is not a failure — by then it has created the bucket, the key, the
  secrets and the image, and the load balancer and service come on the rerun. Give the record to
  the user, wait until they have added it, and rerun the same command.
- **Kubernetes** needs the ingress to terminate TLS for the host: cert-manager (add its issuer as
  an ingress annotation), or a TLS Secret named `attest-tag-attest-tag-tls` in the namespace.

If this is a two-pass install, do step 3 now and then rerun the deploy command.

## Step 5 — verify

Do not report success until all four pass:

```bash
curl -fsS https://ORIGIN/health                    # 200, body: ok
```

1. `/health` returns 200.
2. The Slack app's Event Subscriptions and Interactivity request URLs point at the origin you
   actually deployed to. On a two-pass install these are the placeholder until you fix them.
3. The console loads at `https://ORIGIN/` and offers sign-up.
4. Tell the user to sign up — **the first sign-up founds the deployment** — connect the
   workspace on the Workspaces page (**Add workspace → Slack**), and `@mention` the bot in a
   channel.

Then report: the origin, the platform, what was created, whether the image was pulled or built,
and the reminder that `MASTER_KEY` is backed up and irreplaceable.

## When something fails

- **Bot silent in Slack, `/health` fine** — the app's request URLs are wrong, or
  `SLACK_SIGNING_SECRET` in `.env` is not the one the app shows. Both are step 3.
- **A cloud script says a Slack value is missing that is in `.env`** — there are two lines for
  that key and the first is empty. Replace the line as rule 1 says; do not append.
- **The container keeps restarting** — its log names the first variable it cannot start without.
- **Registry 403 / manifest unknown** — the image is not published; go back to step 0 and build.
- **Azure refuses to deploy** — none of `DOCS_S3_URL`, `DATABASE_URL` or `CREATE_DATABASE=1` is
  set. It prints all three options; do not work around it.
- **AWS exits after printing a CNAME** — expected. Wait for DNS, rerun.
- **`bootstrap.sh` refuses** — `.env` exists. Do not overwrite it. Ask.
- **Bot answers, then goes quiet between messages** — the platform is throttling idle CPU. The
  scheduler and inbox dispatcher run *between* requests. `deploy/docs/platforms.md` explains
  which platforms do this and what to use instead.
