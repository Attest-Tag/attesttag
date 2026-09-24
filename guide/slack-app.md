# Make your own Slack app

attest_tag is not a Slack app you install from a marketplace — it is a server you run, and a
Slack app *you* create and own, pointed at it. That is the whole trick: the app belongs to your
workspace, the bot token never leaves your deployment, and nobody else's terms apply to it.

This page takes you from an empty [api.slack.com/apps](https://api.slack.com/apps) to a bot that
answers in a channel. It takes about ten minutes, and eight of them are waiting for a tunnel.

- [Before you start](#before-you-start)
- [1. Create the app from the manifest](#1-create-the-app-from-the-manifest)
- [2. Check the two switches the manifest cannot always set](#2-check-the-two-switches-the-manifest-cannot-always-set)
- [3. Copy the credentials into your deployment](#3-copy-the-credentials-into-your-deployment)
- [4. Point Slack at your server](#4-point-slack-at-your-server)
- [5. Install it into the workspace](#5-install-it-into-the-workspace)
- [6. Invite it to a channel](#6-invite-it-to-a-channel)
- [What each permission is for](#what-each-permission-is-for)
- [What each event is for](#what-each-event-is-for)
- [Building the app by hand](#building-the-app-by-hand)
- [More than one workspace](#more-than-one-workspace)
- [A second app for development](#a-second-app-for-development)
- [When it does not work](#when-it-does-not-work)

## Before you start

**You need a public HTTPS address.** This is the one hard requirement and the one that trips
everybody up. Slack delivers events by POSTing to a URL you give it, and it will not deliver to
`localhost`, will not accept a self-signed certificate, and will not accept an `http://`
redirect URL. There is no Socket Mode fallback in this codebase — Socket Mode was removed, and
an app configured for it looks connected and answers nothing.

So: get the address first. [`deploy/docs/https.md`](../deploy/docs/https.md) covers the three
ways. The shortest is a Cloudflare quick tunnel, which needs no account, no domain and no
certificate:

```bash
docker compose --profile quicktunnel up -d
docker compose logs quicktunnel      # prints the https URL
```

Whatever you use, you end up with one origin — call it `BASE_URL` — with no trailing slash, and
every URL below hangs off it. A quick tunnel's address changes each time it restarts, which is
fine for a first run and wrong for anything you want to keep; a named tunnel or a real domain
fixes it.

**You need to be able to install an app into the workspace.** On most workspaces that means
being an owner or admin, or being allowed to request an app and having someone approve it.
Creating the app is open to any member; *installing* it is the step that needs the authority.

## 1. Create the app from the manifest

The manifest is the whole app definition — name, bot user, scopes, events, redirect URLs,
assistant pane — in one JSON file, so pasting it replaces about twenty clicks across six
settings pages. It lives at [`deploy/slack/manifest.json`](../deploy/slack/manifest.json) with
`${BASE_URL}` left as a placeholder, and a script fills in your origin:

```bash
BASE_URL=https://YOUR_PUBLIC_HOST ./deploy/slack/manifest.sh
```

Copy what it prints. Then at [api.slack.com/apps](https://api.slack.com/apps) choose **Create
New App → From a manifest**, pick the workspace, paste the JSON, and confirm.

The manifest is checked into the repository rather than written out in this page on purpose:
the test `TestManifestAsksForTheScopesTheCodeAsksFor` compares its scope list against the
`botScopes` list the server actually sends to Slack, in both directions, and fails the build if
they drift. A scope the code needs and the manifest omits is an install that half works. A scope
the manifest asks for and the code never uses is a permission you cannot justify to whoever
approves the app.

## 2. Check the two switches the manifest cannot always set

Slack's app dashboard has been renaming things faster than its manifest schema, so two settings
are worth opening and looking at even after a clean manifest install.

**Agents & AI Apps.** In the left sidebar, make sure the feature is on. This is what gives the
bot a proper assistant pane in DMs — suggested prompts, a threaded side panel, a status line
while it works — rather than a plain chat window. Slack is midway through renaming
`assistant_view` to `agent_view`; use whichever name your dashboard offers.

**App Home → Show Tabs → Messages Tab.** Turn the tab on, and then tick the checkbox
underneath it: **Allow users to send Slash commands and messages from the messages tab**.

That checkbox is the single most common reason a freshly made bot cannot be DMed. The toggle
alone only lets the app *post* into the DM; without the checkbox Slack greys out the compose box
and shows "Sending messages to this app has been turned off". In the manifest it is
`messages_tab_read_only_enabled: false`, which apps created from the manifest get for free — but
an app created before you had the manifest, or edited since, may not. On an existing app, fix it
under **App Manifest** or in the UI, then **Reinstall to Workspace** and restart your Slack
client; the greyed-out box is cached and will not come back on its own.

## 3. Copy the credentials into your deployment

Three values, all from **Basic Information → App Credentials**:

| Slack calls it | put it in | what it does |
|---|---|---|
| Signing Secret | `SLACK_SIGNING_SECRET` | verifies that an event really came from Slack, before anything parses it. Required |
| Client ID | `SLACK_CLIENT_ID` | the OAuth install, and Sign in with Slack. Required |
| Client Secret | `SLACK_CLIENT_SECRET` | the other half of the same. Required |

There is **no bot token to copy**. Older setups had you paste an `xoxb-…`; here the token
arrives from the OAuth install in step 5 and is stored sealed under `MASTER_KEY`, one per
connected workspace. There is no app-level `xapp-…` token either — that was Socket Mode.

Put all three in the environment file your deployment reads — `.env`, which compose reads and
`deploy/gcp/cloudrun.sh` is given with `ENV_FILE=.env` (see [`configuration.md`](configuration.md)
and [Deploy](deploy.md#the-env-file)) — replacing the empty line each already has. Then redeploy,
or recreate the container with `docker compose up -d`; `docker compose restart` keeps the
environment the container was created with.

Set `ADMIN_BASE_URL` to `BASE_URL` in the same file. Sign-in redirects and every link the bot
posts are built from it; left unset, the deployment learns its origin from the first address
the console is signed in on, and keeps it. A quick tunnel's name changes on every restart, so
with one, `ADMIN_BASE_URL` and the app's URLs have to follow it each time.

## 4. Point Slack at your server

An app created from the manifest already has these filled in, and this is the step to re-check
if you created the app before the server had an address. All four live in the dashboard:

| setting | value |
|---|---|
| Event Subscriptions → Request URL | `<BASE_URL>/slack/events` |
| Interactivity & Shortcuts → Request URL | `<BASE_URL>/slack/interactions` |
| OAuth & Permissions → Redirect URLs | `<BASE_URL>/slack/oauth/callback` |
| OAuth & Permissions → Redirect URLs | `<BASE_URL>/api/auth/callback` |

Both Request URLs use the same signing secret. Slack verifies the Event Subscriptions URL with a
challenge the moment you save it — so your server has to be running and reachable *before* you
paste it, or the field refuses the value with a red cross and no useful explanation. The
Interactivity URL is saved unchecked; a wrong one shows only when a button does nothing.

**Interactivity must be on.** Without it the Confirm cards the bot posts before a write still
appear, and pressing a button does nothing at all. (Replying `confirm` in the thread keeps
working, which makes this a confusing thing to debug rather than an obvious one.)

**Both redirect URLs, not one.** They do different jobs: `/slack/oauth/callback` is how a
workspace gets connected, and `/api/auth/callback` is Sign in with Slack for the admin console.
Registering only the first is the classic half-broken install — everything works until somebody
tries to sign in, and the error page explains nothing.

**Socket Mode: off.** It should already be off from the manifest. If it is on, events go to a
websocket this server does not open, and the bot silently receives nothing.

## 5. Install it into the workspace

Not from the Slack dashboard's **Install to Workspace** button — from your own console, so that
the token it hands back is captured and sealed:

1. Start the server and open `/admin/`.
2. Sign up. **The first sign-up founds the deployment** and makes you its admin; everybody after
   that arrives by invitation.
3. Go to **Workspaces → Add workspace → Slack** (on a new account, the setup walk's **Add to
   Slack** does the same).
4. Approve the consent screen.

You land back in the console with the workspace connected. Repeat for as many workspaces as you
like — each keeps its own channels, instructions, budget and memories.

## 6. Invite it to a channel

```
/invite @attest_tag
```

Then mention it. If it answers, you are done. `curl <BASE_URL>/health` is a good second check: it
answers `ok` as soon as the process is listening, and `/admin/` answers 503 until the database is
open. With `HEALTH_SECRET` set, send it as an `X-Health-Secret` header (or `?secret=`) and
`/health` also reports whether this process holds the database, the workspace count, the
documents indexed and the month's spend.

A bot only sees channels it has been invited to. Inviting it to a private channel works the same
way and needs no extra permission — the private-channel scopes are already in the manifest.

## What each permission is for

The manifest asks for nineteen bot scopes. That is a lot to wave through, so here is what each
one buys, which is also what you lose by removing it. The list is pinned to the code by the test
mentioned above, so it stays true.

| scope | what stops working without it |
|---|---|
| `app_mentions:read` | @mentions. This is the main way people talk to it |
| `assistant:write` | the DM assistant pane: suggested prompts, the status line while it works |
| `chat:write` | posting anything at all |
| `chat:write.customize` | nothing yet: it is requested, and no message the bot posts sets a name or icon of its own today |
| `channels:history` | reading the thread it was mentioned in, in a public channel |
| `groups:history` | the same in a private channel |
| `im:history`, `mpim:history` | the same in a DM and a group DM |
| `channels:read`, `groups:read` | listing channels, and the `conversations.info` call that decides whether a conversation is private before reading it for somebody |
| `im:read` | DM conversation info |
| `im:write` | opening a DM with someone who has never messaged the bot. Access-request cards, routine failure notices and personal connect links all go out this way, so an install without it will quietly fail to reach an approver. The server logs loudly at boot when it is missing |
| `users:read` | turning user ids into names, everywhere |
| `users:read.email` | the organisation's email-domain list (Settings → Security; `ALLOWED_EMAIL_DOMAINS` is the default where it keeps none). While a list is set, nobody can use the bot without this scope |
| `pins:read` | the `list_pins` tool |
| `files:read` | reading files people attach to a thread |
| `files:write` | uploading artifacts, and posting long answers as a Markdown file instead of a wall of text |
| `reactions:write` | answering with an emoji instead of a reply, which is what read-every-message mode does most of the time, and the `react` tool a channel's instructions can ask for |
| `search:read.public` | the `slack_search` tool, across public channels |

**Two scopes are deliberately absent.** `channels:leave` and `groups:leave` would let the console
remove the bot from a channel by leaving it. They exist, but Slack does not offer them in the
OAuth scope picker for an app of this shape, and asking for one fails the *entire* install with
"Invalid permissions requested" — so nothing connects at all. Removing a channel degrades on its
own when the scope is absent, and a workspace that granted it under an older install keeps it and
keeps working.

## What each event is for

| event | why |
|---|---|
| `app_mention` | somebody tagged the bot |
| `message.channels`, `message.groups`, `message.im`, `message.mpim` | follow-ups in a thread the bot is already in, without re-tagging it; noticing edits and deletions; read-every-message mode; and mail forwarded to a channel's Slack address where email intake is on. The router drops everything that is not a DM, a mention, a reply in an active thread, a message the classifier says is meant for the bot, or such a mail |
| `assistant_thread_started` | the DM assistant pane opening, which is when its suggested prompts are set |
| `assistant_thread_context_changed` | subscribed, and not used yet |
| `app_home_opened` | subscribed, and not used yet: nothing is published to the Home tab |
| `app_uninstalled`, `tokens_revoked` | the workspace removed the app or revoked the token. The cached client is evicted rather than left retrying with a dead token |

## Building the app by hand

If you would rather click than paste, or you have an app already and want to bring it up to
date, the manifest above is the checklist: create an app **From scratch**, then work through
**OAuth & Permissions** (the nineteen bot scopes and both redirect URLs), **Event
Subscriptions** (the Request URL and the ten bot events), **Interactivity & Shortcuts** (on, with
its Request URL), **App Home** (Messages Tab on, read-only *off*), **Agents & AI Apps** (on), and
**Socket Mode** (off).

Easier than any of that: open **App Manifest** on the existing app and paste the generated JSON
over what is there. Slack diffs it and tells you what will change, including which scopes need a
reinstall.

## More than one workspace

One deployment serves as many Slack workspaces as you connect to it. Access resolves through
three levels, widest first:

| Level | What it is | What it covers |
|---|---|---|
| **Workspace** | this account — one row, always there | every connected Slack workspace |
| **Team** | one connected Slack workspace | every channel in it |
| **Channel** | one channel the bot is in | that conversation |

Instructions concatenate widest to narrowest and the narrowest rule wins, so a bundle attached
at the Workspace level is reachable from every connected Slack workspace — which is the thing
worth knowing before pasting a credential there. Bundles, connections, documents and skills
belong to the account; scopes, channels, sessions, memories and budgets belong to a workspace.

Slack only guarantees a channel id is unique *within* a workspace, and a Slack Connect channel
carries the same `C…` id in each workspace it is shared into, so everything is keyed by
`(workspace, channel)` rather than by channel alone.

To hand the app to workspaces that are not yours, **Manage Distribution → Activate Public
Distribution**. HTTP delivery removes the Socket Mode transport restriction that used to block
this; listing on the Slack Marketplace is a separate thing and still needs Slack's review.

## A second app for development

Make a **separate Slack app, with its own signing secret**, for local work. Each app carries its
own Events and Interactivity Request URLs, so a development app points at your tunnel and the
real one points at your deployment, and neither can answer in the other's channels.

Point your local run at it with `.env.testing` — a separate dotenv file with its own
`DB_PATH=testing.db`, so a local run cannot write to the deployed database either. Details in
[`development.md`](development.md).

**Do not repoint a production app's Request URLs at a development machine.** It is the fastest
way to make the real bot go silent, and the symptom is Slack disabling the event subscription
for you after enough failures.

## When it does not work

| what you see | what it is |
|---|---|
| The Event Subscriptions Request URL refuses to save | your server is not reachable at that address yet. Slack sends the challenge synchronously. Start it, check `curl <BASE_URL>/health` from somewhere that is not your machine, then save |
| "Sending messages to this app has been turned off", compose box greyed out | the Messages Tab checkbox in step 2. Fix, reinstall, restart the Slack client |
| Mentions do nothing, no errors anywhere | Socket Mode is on, or the Event Subscriptions Request URL is unset or stale. Check both |
| Buttons on Confirm cards do nothing | Interactivity is off, or its Request URL is missing or wrong. Reply `confirm` in the thread meanwhile |
| Sign in with Slack fails with an unhelpful page | `/api/auth/callback` is not in the redirect URLs, or the deployment's public origin (`ADMIN_BASE_URL`, or the address the console was first signed in on) is not the one registered |
| Install fails: "Invalid permissions requested" | a scope Slack will not grant an app of this shape — see the note about `channels:leave` above |
| It answers in some channels and not others | it has not been invited to those channels |
| It stops answering after your server was down a while | Slack disables an endpoint that fails for long enough. Re-enable it under Event Subscriptions, and reinstall if events still do not arrive |
| It turns some people away | their address is not on the organisation's email domains (Settings → Security; a new account starts with the company domain it signed up with, and `ALLOWED_EMAIL_DOMAINS` applies only where it keeps no list), Slack will not show their email, or they are a guest or a Slack Connect member, who are refused unless Settings → Security lets them in. On a mention or a DM it says which |

## Next

- [Using it in Slack](slack.md) — what it can actually do once it answers
- [Configuration](configuration.md) — every environment variable and console setting
- [Admin console](console.md) — where you manage all of it
