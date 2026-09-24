# Walkthrough recorder

The videos on `/walkthroughs` are not screencasts anyone recorded by hand.
`npm run video:deep` signs in with a session **you** captured, drives **real
Slack** and the **real console** with a synthetic cursor, narrates it, and
writes three files into `out/`:

| File          | What it is                              |
| ------------- | --------------------------------------- |
| `<id>.mp4`    | H.264 + AAC, 1440×900, ready to serve   |
| `<id>.jpg`    | Poster frame                            |
| `<id>.json`   | Duration + measured chapter offsets     |

The manifest is read by `src/lib/walkthroughs.server.ts` **in the site's own
repository** — `Attest-Tag/attesttag-website`, which is where the page and
these three files' destination now live — so the chapter list on the page is
always the offsets the narration actually landed at.

**The poster and the manifest are committed there; the mp4s are not** — they
are tens of MB apiece and gitignored in both repositories, and reach the
deployed site through `npm run video:publish`, which uploads to the bucket the
page streams from. So a finished render is two steps: copy `<id>.json` and
`<id>.jpg` into that repository's `public/walkthroughs/` and commit them, then
publish the mp4. Point `WALKTHROUGHS_DIR` at that folder and the renders land
there in the first place, which is what anyone with both checkouts should do:

```bash
export WALKTHROUGHS_DIR=~/code/attesttag-website/public/walkthroughs
```

## Before the first run

```bash
brew install ffmpeg
npm install

npm run signin      # opens a real browser; you sign in; the profile keeps it
npm run preflight   # every prerequisite in one go; names what each one blocks
npm run check       # tests the credentials; prints no token, or part of one
npm run smoke       # renders 30s on a public page to prove the pipeline
npm run scout       # checks Slack's DOM before a word of script is trusted
npm run setup -- --seed   # makes the demo channel, invites the bot, seeds it
npm run tidy              # deletes the recorder's own leftovers from the channel
```

`npm run signin` opens a window and waits. **No password goes through this
code** — it copies the cookies and local storage your own sign-in leaves
behind, into `.auth/`, which is gitignored and mode 0600. A Slack `d` cookie is
a live session for anyone holding it; treat that directory as a credential. It
expires on its own, and a render that starts failing at sign-in wants
`npm run signin` again rather than a hunt for a selector that did not change.

### Which console: history, and the guard that replaced it

**What follows is how the recorder used to be run, kept for its reasoning.** It
now refuses `app.attesttag.com` unless `VIDEO_ALLOW_PRODUCTION=1`
(`assertNotProductionByAccident` in `render.ts`, and the entry in
[`LEARNINGS.md`](LEARNINGS.md)): a render founds an organisation, installs a
workspace, stores a credential and opens a pull request for real, so it runs
against the instance `VIDEO_CONSOLE_URL` in `.env` names — the Funnel host in the
table below.

`video/.env` used to point `VIDEO_CONSOLE_URL` at **deployed production**. The
obvious-looking alternative — run the binary locally behind a Tailscale Funnel,
against a wipeable `testing.db` — does not work, for a structural reason:

**A Slack app has one Request URL.** Recording against a local instance means
repointing the production app at the Funnel host, which stops event delivery
for every real workspace while you record. And if that host 502s even briefly,
Slack's >95%-failure rule temporarily disables the app until somebody
re-enables and reinstalls it. Socket Mode is not the escape hatch it looks
like: two clients on one app both connect, and Slack round-robins events
between them, so half of them land in the instance you are not recording.

The local instance is also, separately, not in the demo workspace at all —
`bot.log` has it connected to team `T0DEMO0001` while the demo workspace is
`T0DEMO0002`, which is why an early scout run sat for three minutes waiting
for a reply from a bot that was never going to hear the question.

**What local was for is solved better by the product.** The worry was real
customer data on camera. `0-signup` founds a *new organisation*, and
organisations cannot see each other — so every console shot contains that fresh
org's data and nothing else. That is the isolation the product is built on
rather than an accident of which database was attached.

And Playwright records the *page*, not the browser: there is no address bar in
the output, so nothing in the video says which host it was.

### Credentials, and the rule about them

The walkthrough puts two real credentials in on camera, from `video/.env`:

| | what | why that one |
| --- | --- | --- |
| **GitHub** | fine-grained PAT, one throwaway repo, `contents:read` | the read half — chapter 4 asks what changed |
| **ClickUp** | personal API token from a **fresh** account, one workspace | the write half — chapter 5 raises a ticket |

**Revoke both the moment the recording is done.** A secret that appears on
camera has to die on camera: the video is public and permanent, and "revoke it
when the integration retires" is the right lesson anyway. The console's secret
inputs are all `type="password"`, so a token is dots in every frame — but do
not click a reveal toggle while recording, and do not use a credential you
would mind revoking.

ClickUp is the write half rather than GitHub deliberately. Creating a ticket is
additive, so a bad take costs a stray task instead of a closed issue somebody
has to reopen; and the seeded backstory sets it up, with somebody saying the
reconciliation change is "worth its own ticket" and nobody raising it.

`npm run check` tests both against the same calls the console's connection test
makes, and prints what they can reach without printing any part of a token.

### Which Slack workspace

**A throwaway demo workspace with seeded backstory. Never a real one.**

Everything in that sidebar is in a public marketing video for as long as the
video is up: channel names, unread badges, colleagues, the DM list. And every
take posts real messages into whatever workspace it is signed in to.

The console side has the same problem and the same fix — install the app into
the demo workspace from a **fresh account**, because `saveInstall` binds a
workspace to whichever org installed it, and the console shots then contain
that org's data and nobody else's.

### Start the local instance clean, or 0-signup fails on camera

`0-signup` founds an organisation and installs the demo Slack workspace into
it. That install is **refused** when the workspace already belongs to a
different organisation: `SaveTeam` upserts with
`where teams.org_id = excluded.org_id` and returns `ErrTeamOwnedElsewhere`
otherwise. It is a deliberate isolation guard — connecting a workspace somebody
else has connected would mean taking it from them — and it fails at the most
important beat in the walkthrough.

So a full run starts from an empty database, which the project already has a
flag for:

```bash
FRESH_DB=1 ./start_dev.sh     # from the repo root
```

The Slack-side install is untouched; `0-signup`'s OAuth recreates the row under
the organisation it has just founded. It also gives the honest opening shot: an
empty console, because it really is empty.

`npm run preflight` checks this and names the organisation the workspace is
currently bound to, so it is caught before a render rather than during one.

## Running it

```bash
npm run video:deep                      # all ten parts, then stitch
npm run video:deep -- 3-connect         # re-record one part; stitch runs after
npm run video:deep -- stitch            # just re-join what is on disk
npm run video:publish -- deep-dive      # push the mp4 to the bucket
npm run video:poster -- deep-dive   # re-make the poster card from the stitched mp4
```

### Re-cutting a take instead of re-recording it

A take that filmed the product doing the right things but at the wrong pace —
a minute of still frame while Slack retried a delivery, a pane that failed to
load sitting beside the composer — does not need a second take. `recut.ts`
starts from the raw `.webm` in `.video-work/<part>/`, keeps the seconds that
show something, drops the rest, and lays new narration under each moment:

```bash
npm run video:recut -- deep-dive                # every re-cut part, then stitch
npm run video:recut -- deep-dive 5-hold         # one part, then stitch
npm run video:recut -- deep-dive stitch         # just re-join what is on disk
```

The script is `recuts/deep-dive.ts`: per part, a list of scenes anchored to a
second in the footage (`at`), the last second worth showing (`until`), and the
line said over it. A line longer than its footage holds the last frame; footage
longer than its line is cut, kept, or fast-forwarded (`rest`). A scene can
`zoom` into a region of the frame for a beat the full frame would spoil, and a
scene with no footage at all is a `card`: the part's title and a sentence on
it, rendered like the title and end cards and held for a beat, in place of the
bare seam the take painted. Parts with no re-cut are stitched from disk as
they are, and the stitched manifest carries a `version` (the exact stitch),
which is what the site now busts the mp4's cache with — the date alone let two
cuts on one day share a URL.

Every number in that file was read off the frames, and that is how to change
one: pull a contact sheet of the raw footage first
(`ffmpeg -i <webm> -vf "fps=1,scale=430:-1,tile=5x20" -frames:v 1 sheet.jpg`),
and look at the result the same way. Two traps the tool already knows about:
ffmpeg's `tpad` adds nothing after a `setpts` (the pad goes first), and a
filter that drops a piece still exits 0 — so the encode is checked against the
length the pieces add up to.

Environment, all optional and read from `../.env.testing` then `.env`:

| Variable             | Default                               |                        |
| -------------------- | ------------------------------------- | ---------------------- |
| `VIDEO_CONSOLE_URL`  | set in `.env` to the Funnel host      | which console to record |
| `VIDEO_CHANNEL`      | `engineering`                         | channel to record in   |
| `VIDEO_BOT_NAME`     | `attest_tag`                          | as Slack renders it    |
| `VIDEO_HEADLESS`     | unset (headed)                        | `1` to hide the window |
| `VIDEO_TTS`          | `gemini` if a key is set, else `say`  | `gemini`\|`gptaudio`\|`say` |
| `VIDEO_VOICE`        | `Charon`                              | Gemini voice           |
| `VIDEO_TTS_STYLE`    | calm/warm/unhurried                   | style direction        |
| `VIDEO_SIGNUP_EMAIL` | —                                     | **required by `0-signup`** |
| `VIDEO_SIGNUP_PASSWORD` | —                                  | **required by `0-signup`** |
| `VIDEO_SIGNUP_ORG` / `_NAME` | `Northwind` / `Sam Reyes`     | what the form is filled with |
| `VIDEO_GITHUB_TOKEN` | —                                     | **required by `3-connect`** · read half |
| `VIDEO_GITHUB_REPO`  | —                                     | named on camera |
| `VIDEO_CLICKUP_TOKEN`| —                                     | **required by `3-connect`/`5-hold`** · write half |

## The other walkthroughs

The deep dive is the long one, and the only one on attesttag.com. Everything
else is a short walkthrough of one thing, **listed only in the console's Get
started page** (`Copy.consoleOnly` in `walkthroughs.server.ts`), on the two
shelves that page groups them by: **In Slack** (for anyone in a channel the
bot is in) and **The console** (for the person setting it up). They are
recorded, published and served exactly like the deep dive — poster and
manifest committed under the site repository's `public/walkthroughs/`, mp4
pushed to the bucket, the console reading `/api/walkthroughs` — the site just
does not list them. Each is a `LongForm` of its own in `guides/`, registered in
`guides/registry.ts`, and run the same way:

```bash
npm run video -- <id>                   # every part, then stitch
npm run video -- <id> <part>            # re-record one part; stitch runs after
npm run video -- <id> stitch            # just re-join what is on disk
npm run video:<id>                      # the same, by its own script
```

| id           | title                                   | shelf       |
| ------------ | --------------------------------------- | ----------- |
| `ask`        | Ask it something                        | In Slack    |
| `memory`     | Teach it, and make it forget            | In Slack    |
| `personal`   | Your own mail and calendar              | In Slack    |
| `access`     | Ask for access you don't have           | In Slack    |
| `automation` | Routines and fix jobs                   | In Slack    |
| `reach`      | What a channel may reach                | The console |
| `guardrails` | Try to break it                         | The console |
| `documents`  | Documents and Drive                     | The console |
| `cost`       | Cost, models and budget                 | The console |
| `signin`     | Sign-in, two-factor, and who gets in    | The console |
| `api`        | The API, and a key that is a person     | The console |

### They record after the deep dive, in the organisation it founded

None of them signs up or connects a workspace. Each one starts from what
`npm run video:deep` leaves behind — the workspace connected, the bot in the
channel with the seeded week, the repository and the Engineering bundle
attached — and its first part checks for that in `before`
(`assertDeepDiveState()` in `guides/shared.ts`) and names the deep-dive part
that is missing rather than timing out on a selector three scenes in. So the
order is: fresh database, `npm run video:deep`, then any of these, in any
order, as many times as you like.

The corollary from "What a recording leaves behind" applies twice over: these
walkthroughs create things — a memory, a routine, a tier, a skill, a key — in
an organisation that is not thrown away between their takes. Where a scene
creates something it either deletes it in a later scene of the same
walkthrough or says in a comment that a second take leaves a duplicate in the
frame. Read the comment before re-recording a part on its own.

### What some of them need beyond the deep dive

All optional until the walkthrough that wants them runs; each one's first part
throws with the name of the missing variable.

| Variable                    | Wanted by      |                                                                 |
| --------------------------- | -------------- | --------------------------------------------------------------- |
| `VIDEO_CHANNEL_2`           | `access`       | a second channel with the bot in it and **nothing attached** — `VIDEO_CHANNEL=attesttag-support npm run setup` once (no `--seed`); default `attesttag-support` |
| `VIDEO_APPROVER`            | `access`       | Slack user id of a **second** member of the demo workspace; they receive the approval card. The recorder cannot approve its own request, so the press itself is not on camera |
| `VIDEO_APPROVER_HANDLE`     | `access`       | that person's handle as Slack's @-picker lists it (one token). The ask tags them: a tagged approver is what puts the request-access instruction in front of the model |
| `VIDEO_SIDEBAR_KEEP`        | `access`       | `attesttag-demo,attesttag-support`, so both channels stay visible |
| `VIDEO_GOOGLE_CLIENT_ID` / `_SECRET` | `personal` | a "Web application" OAuth client from a throwaway GCP project (Gmail + Calendar APIs on), with one redirect URI: `<VIDEO_CONSOLE_URL origin>/connect/callback`. The recording profile must also be signed in to a **throwaway Google account** listed as a test user on that client, with an event on tomorrow's calendar — its address is on Google's consent screen, on camera |
| `VIDEO_GDRIVE_SA_JSON`      | `documents` (part 3 only) | path to a service-account key from a throwaway project |
| `VIDEO_GDRIVE_FOLDER`       | `documents` (part 3 only) | a Drive folder URL **shared with that service account's address**, holding two or three harmless files |
| `VIDEO_INVITE_EMAIL`        | `signin`       | a throwaway address to invite to the console on camera; plus-addressed with the minute, since the server allows three invitations per address per day |
| `VIDEO_ADVANCED_MODEL`      | `cost` (optional) | the model id 2-models sets as the advanced model; unset, the picker's current value is picked again and nothing is saved |

The rule about secrets on camera covers all of these: the Google client
secret, the service-account key and the API key the `api` walkthrough mints
are all revoked the moment the recording is done — the `api` one on camera,
which is the point of that chapter.

### Order matters for three of them

- **`signin` goes last.** It enrols two-factor on the founder's account and
  then requires it for the organisation. The recorder's own session survives
  (confirming the code marks it MFA-verified), so anything recorded after it
  in the same session still works — but a fresh `npm run signin` into that
  organisation would be asked for a code nobody has an authenticator for.
  Record it last, or plan a fresh database after it.
- **`guardrails` and `automation` before the tokens are revoked.** Both read
  the repository and the tracker on the deep dive's credentials; the README
  above says to revoke those the moment the recording is done, so "the
  recording" means all of it.
- **`automation` changes what "the newest job" is.** It dispatches a fix job
  and cancels it. `waitForJob()` in `guides/shared.ts` reads the newest job,
  so re-recording the deep dive's `7-memory` on its own afterwards would see
  a cancelled job and stop; re-record `6-fix` with it.

Each guide's header comment says what a re-take of each part leaves in the
frame and what to delete first — `reach`'s bundle part and `documents`' Drive
part refuse to record over their own leftovers rather than film two of
everything.

## The voice

Narration is **Gemini 3.1 Flash TTS** through OpenRouter's dedicated
`/api/v1/audio/speech` endpoint, used whenever `OPENROUTER_API_KEY` is set.
`Charon` is Gemini's "informative" voice, which is what a walkthrough wants;
`Sulafat` (warm), `Algieba` (smooth) and `Kore` (firm) are the other plausible
narrators. Changing the voice re-measures every line and rewrites the chapter
offsets to match; nothing else needs touching.

> A stale `OPENROUTER_API_KEY` exported in `~/.zshrc` beats both env files and
> fails as an OpenRouter 401 "User not found", which looks nothing like its
> cause. `unset OPENROUTER_API_KEY` in the shell if you see that.

## How it fits together

- **[`guides/deep-dive.ts`](guides/deep-dive.ts)** — the script: ten parts, each
  a list of scenes with a narration line and an optional `act`. **A scene stays
  on screen for exactly as long as its line takes to speak**, so pacing is
  edited by editing words, not by tuning waits. Scenes with a `chapter` become
  entries in the page's chapter list.
- **[`slack.ts`](slack.ts)** — driving the real Slack client. **Every selector
  Slack owns lives in `SEL` here and nowhere else**, because Slack's DOM is not
  a contract we are party to and a release that moves one should cost a single
  line. It is also where `compose()` handles the trap that eats the most takes:
  typing the characters of `@attest_tag` does *not* make a mention, and a
  message that only looks like one never reaches the bot at all.
- **[`stage.ts`](stage.ts)** — the hand-speed helpers both surfaces share.
  Playwright at native speed with no visible pointer reads nothing like a demo,
  so `click` glides the cursor and pauses on hover, `type` goes key by key, and
  an SVG cursor with a click ripple is injected into every document.
- **[`narrate.ts`](narrate.ts)** — text to audio, and the measured duration of
  each line.
- **[`render.ts`](render.ts)** — synthesises every line first (their durations
  set the schedule), records one continuous page, then lays the clips onto the
  timeline at the offsets each scene actually started at (`adelay` + `amix`, so
  a dozen scenes accumulate no drift) and encodes.
- **[`stitch.ts`](stitch.ts)** — joins the parts and merges their chapter lists.
- **[`smoke.ts`](smoke.ts)** — renders thirty seconds against a page that needs
  no sign-in. It exercises everything the walkthrough depends on and none of
  what the walkthrough is about: synthesis, measurement, the cursor, hand-speed
  clicking, one continuous recording, the offset mux, the encode, the poster
  and the manifest. Run it after touching anything in the pipeline. A real part
  spends minutes of paid speech before it draws a frame and only meets ffmpeg
  at the very end, so a pipeline fault there costs the whole take; here it
  costs three lines.
- **[`scout.ts`](scout.ts)**, **[`check-creds.ts`](check-creds.ts)** and
  **[`cards-preview.ts`](cards-preview.ts)** — the three things that let you
  check something without spending a render on it. `check` is the one to run
  habitually: finding out at chapter four that the PAT was scoped to the wrong
  repository costs the take and a set of paid narration.
- **[`setup-workspace.ts`](setup-workspace.ts)** — makes the demo channel,
  invites the bot, and posts the week of backstory that two chapters depend on
  the bot having read.

### One page, two products

Playwright records a *page*, not a screen — so cutting between Slack and the
console would normally mean two video files and a stitch at every seam. It does
not here, because a storage state is only cookies and origins and two of them
for two different hosts merge without conflict. `loadStorageState()` in
`render.ts` hands one context both sessions, and one page then navigates from a
Slack thread to the console and back with the recording running throughout.

## Before scripting the next part

Read [LEARNINGS.md](LEARNINGS.md). Hydration races, Slack hooks that are not
the ones you would guess, product gates that stop a take dead, and the four
different ways a check can look like verification and never fail. Every entry
there cost at least one wasted render.

## Rules that cost a take to learn

**Scout before you script.** Half the surface belongs to Slack. `npm run scout`
prints every selector present or missing, lists the `data-qa` values actually on
screen when one misses, and with `--ask` times a real round trip — which is the
number a chapter's narration has to be written to, and is not in any source
file.

**A wait that is not the product's goes off camera.** `s.offCamera(fn)` runs
`fn` once the scene's line has been spoken and cuts those frames from the
encode, shifting every later offset up to match. Split the round trip around
it: `slack.send()` on camera, `slack.awaitReply()` inside, `openThreadOf()`
after. Slack's retry schedule is not the product; four seconds of the bot
thinking is, and the parts that show it are the honest ones.

**Never assert on anything the model produced.** The answer is different on
every take. `waitForBotReply` waits for the reply to stop *growing*, which is a
property of the transport rather than of the model.

**Check the frames, not the exit code.** A render can exit 0 having filmed a
blank pane, a Slack "reconnecting" banner, or a thread that never got an answer.
Watch the mp4 before you publish it.

**One render at a time**, enforced by a pid lock. Takes share a workspace and an
org, so two at once post into the same channel.

**Cheap preconditions before expensive ones.** Synthesis runs first and costs
minutes of paid API calls; `render()` pings both hosts and loads both sessions
before it synthesises a line.

## What a recording leaves behind

It drives the *real* product: it genuinely founds an organisation, posts
messages, creates a bundle and a connection, confirms a write, and stores a
memory — and every answer spends real tokens.

**A full run cleans up after itself, by construction.** `0-signup` founds a new
organisation every take, and organisations cannot see each other, so last
take's bundles and memories are invisible from inside this take's console.
Nothing accumulates in the shot; only `testing.db` grows.

**Re-recording a single part does not.** A second take of `3-connect` inside
the same organisation leaves two bundles called Engineering, and the second one
is in the frame. Either delete what the previous take made before re-recording
it, or re-run from `0-signup` — which means re-running everything, for the
reason in the header comment of
[`guides/deep-dive.ts`](guides/deep-dive.ts).

To start completely clean, stop the bot, move `../testing.db` aside, and start
it again. **Check with whoever is using it first** — that binary is often
running for something else.
