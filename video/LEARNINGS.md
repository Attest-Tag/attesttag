# Recording walkthroughs: what bit us

Every entry here cost at least one wasted render — a few minutes of paid speech
synthesis, a browser run against the real product, and in several cases a
finished video that was confidently wrong.

Read this before scripting anything new. About half of what follows is a lesson
the tooling this was ported from had already learned once, ported along with the
code and then walked into anyway.

---

## Check the frames, not the exit code

**Three parts rendered green, exited 0, wrote posters and manifests, and filmed
nothing happening.** The composer held four questions concatenated and unsent;
the thread pane showed an answer to a question asked nineteen minutes earlier.
Every automated signal said success.

A render that finishes is not a render that worked. Pull frames before you
believe anything:

```bash
ffmpeg -ss 58 -i video-out/deep-dive/4-reach.mp4 -frames:v 1 out.jpg
```

The cheapest corroborating signal is the product's own log. `bot.log` shows a
`turn` line with tokens and cost for every question the bot actually answered.
No `turn` lines during a render that claims the bot answered is the whole story
in one grep.

## Checks that cannot fail are worse than no checks

Four separate faults this session were the same mistake: a check that looked
like verification but could not report failure.

| the check | why it always passed |
| --- | --- |
| `waitForURL(u => !/signup/.test(u.pathname))` after sign-up | a *failed* sign-up leaves the path unchanged, and a bounce to `/login` changes it without anybody signing up |
| `res.ok` on `/api/me` | that endpoint answers **200 while signed out** — its job is to report which login methods exist, which you must be able to ask before you have an account |
| `clickIfPresent(Allow)` + `.catch(() => {})` | a refused install rendered a finished video whose closing line said "one workspace, connected" over Slack's red error page |
| reading the last bot message | the previous answer was still on screen, so the scene "succeeded" in two seconds having waited for nothing |

Assert on the thing that actually matters, and make it something that can only
be true afterwards: a session (`signed_in === true`), a row (`select count(*)
from teams`), an empty composer. `render.ts` and `guides/deep-dive.ts` now do.

**A recorder that can narrate success over a failure is worse than one that
crashes.** Every scene that claims an outcome now checks for it.

## Environment, and where it is read

**Read `process.env` when called, never at module scope.** `index.ts` loads the
env files *after* its imports have evaluated, so

```ts
const CONSOLE_URL = process.env.VIDEO_CONSOLE_URL ?? "https://app.attesttag.com";
```

resolves to the fallback on every run. This bit three files. `narrate.ts`
carries the warning; `guides/deep-dive.ts` was fixed for it and the lesson was
not generalised; `render.ts` was found third — **by somebody watching the
browser window and noticing the recorder was driving production.**

That one is the reason `assertNotProductionByAccident()` exists. A render is
not a read-only tour: it founds an organisation, installs a Slack workspace,
stores a credential and opens a pull request, for real, against whatever host
it is pointed at. It now refuses `app.attesttag.com` unless
`VIDEO_ALLOW_PRODUCTION=1`.

**When you find a bug of this shape, grep for its siblings.** Fixing one and
moving on is what let the third one reach production.

## Driving Slack

**Tab is not a commit key.** Typing the characters of `@attest_tag` does not
make a mention — Slack makes one when you pick the person out of the
autocomplete. Tab commits it *only while that list is open*; with no list up,
Tab is the focus key and moves focus out of the composer entirely, so the Enter
that follows goes somewhere else, nothing sends, and the text stays in the
draft for the next part to type on top of. Press Tab only when the list is
genuinely visible, and throw if it never opened: a handle that matches nobody
posts as literal text and the bot never hears it.

**Verify the send.** An empty composer is the only evidence Enter did anything.

**The bot replies in a thread.** "Threads are the unit of conversation" is the
product's first principle, not a detail — so a channel view never gains the
reply. Post, wait for the `reply_bar_view_thread` bar the answer creates, click
it, then read. Reading the channel reports zero characters forever while the
server log shows completed turns.

**Snapshot what is on screen before asking.** Otherwise the previous answer is
indistinguishable from this one's.

**Never assert on what the model said.** The answer differs every take. Wait
for the reply to stop *growing* — a property of the transport, not the model.

**Every selector Slack owns lives in `SEL` in `slack.ts` and nowhere else**, so
a release that moves one costs a single line. Several of the obvious guesses
are wrong:

| guessed | actually |
| --- | --- |
| `message_input` | `message_input_container` (the editable is inside it) |
| `threads_flexpane` | does not exist — read every `message_container` instead |
| `channel_sidebar_name_button` | `channel_sidebar_name_<channel>`, a prefix match |
| — | `message_pane` is the "client has booted" signal |

**Some selectors are transient** — `autocomplete-list` only exists mid-mention,
`start_thread` only on hover. Absent on a resting screen is correct, and
reporting it as missing sends you hunting for a replacement for something fine.
`scout.ts` marks these.

**Run `npm run scout` before scripting anything, and after any Slack update.**
It checks every selector, prints the `data-qa` values actually on screen when
one misses, and with `--ask` times a real round trip — the number a bot-wait
chapter's narration has to be written against. Measured here: **19.2s for 367
characters.**

**The sidebar is the recording account's, not the product's.** Other teams'
channels, the DM list, unread badges — none of it is a fact about the product
and all of it is in a public video. `keepOnlyChannels` hides the DM and app
sections, drafts, starred and the trial banner, and keeps the channel list so
the workspace still reads as one. It is pure CSS via `:has()` — the sidebar is
virtualised, so a stylesheet applies to rows as they appear, and the style
engine is built for exactly that. (The first version used a MutationObserver;
see below for how that went.) **Wire it into `render.ts`, not just the setup
script** — it was written for the recorder and then only called from
`setup-workspace.ts`, so every early take filmed the lot.

**Slack keeps the draft.** A composer's unsent text is stored server-side and
survives a closed browser, so every failed attempt types on top of the last
one's leftovers. After the first failure the `@` is no longer at a word
boundary, the autocomplete never opens, and every later attempt fails for a
reason that did not exist yet — while the error each time blames the handle.
`compose()` now clears the box before typing. Four consecutive "fixes" were
chasing this one.

**Do not click the composer to refocus before Enter.** A click puts the caret
wherever it lands; in or beside the mention token that reopens the picker, and
Enter then selects from it instead of sending. `focus()` then `End`.

**`[role="listbox"]` is the autocomplete — and `.first()` is not.** Slack has
more than one listbox in the document; the first is never the mention picker.
Wait for a visible `[role="option"]` instead, which is the thing you need.

**"Stopped growing" is not "finished".** While the bot works, its row holds a
short, stable status placeholder — "Reading a thread", "is working…" — which is
exactly what a stopped-growing test accepts. Every Slack scene in one take
ended on that placeholder; the answers arrived after the camera had moved on,
and `bot.log` showed the turns. The product's own done-signal is the reply
footer (`glm-5.3-flash · 15k in · 501 out · $0.0016 · Configure`), rendered
only once the answer is complete. `waitForBotReply` now requires it, except for
the fix worker's messages, which never carry one.

**`addInitScript` did not leave a style tag in Slack's document; `addStyleTag`
after boot does.** The cursor uses the latter and always worked; the chrome
filter used the former and silently never applied. Apply chrome from `open()`.

**Slack's banners rotate.** Dismiss the notifications one and a "download the
app" one takes its place; close that and it is back after a reload. Grant the
notification permission on the context and hide
`[data-qa="workspace-banner-download-app"]` in CSS. The app rows are
`preseeded_app_row_*`; the starred placeholder is the only sidebar row with no
hooked descendant, which is the only handle it offers.

**Wait for the reply bar on the message you posted, not the last bar in the
channel.** Once a channel holds two answered questions, `.last()` is the
previous question's bar, it is visible immediately, and clicking it opens the
previous thread — the recorder then waits on a reply it was told to ignore
while `bot.log` shows the new one landing elsewhere. Scope the bar to a row
matched on the question's own text.

**Slack restores the last open thread on load.** A fresh part can begin with
the previous answer already in the pane. Press Escape before snapshotting.

**Do not clean up a channel blind.** A "delete the newest junk" pass took the
previous part's answer; deleting a parent over an intact reply then left a
tombstone. List the rows first, then delete replies before parents. `tidy`
does both now.

**A held write is a reply with no footer.** The fix-job card ("Preparing a fix
job: …") is complete the moment it appears and never gets a footer — a wait
that requires one times out on a card that is already sitting there. The
ClickUp card does carry one. Know which replies are answers and which are
cards, and wait accordingly.

**Snapshot before asking, not after opening the thread.** `ask()` opens the
thread the reply lands in; a snapshot taken after that captures the new answer
as "already there" and then waits for it to change. The bot answers, the log
shows the turn, and the recorder times out.

**A MutationObserver on Slack's body will freeze the client.** Slack mutates
continuously; running a document-wide `querySelectorAll` per mutation stalled
it so hard it never booted, which reported as "no channels in the sidebar".
The chrome filter is now pure CSS via `:has()`.

**The moon icon is focus mode, not the theme.** Theme is under the avatar →
Preferences → Appearance, is stored per account server-side, and ignores
`prefers-color-scheme` — so `colorScheme: "light"` on the browser does nothing.

**Grant notifications; don't hide the bar.** `context.grantPermissions(
["notifications"])` on app.slack.com means the "needs your permission" strip
never exists.

**A slash command opens the command palette, which swallows Enter.** `/invite`
never sent. And the bot was already a member, so the line described something
that could not happen. Narrate what is there.

**Slack's first delivery fails, and the retry is a minute away.** Through the
Tailscale Funnel the bot is recorded behind, Slack's first attempt at every
event dies with `http_error` before it reaches the process — the bot's log has
only the redeliveries (`X-Slack-Retry-Num`), never the originals — and Slack
retries at about one second, then one minute, then five. When the one-second
retry gets through, a reply lands in four seconds; when it does not, seventy.
Over one day's log: 8 replies under 10s, 10 between 11 and 60, 3 over 60 — and
the two takes of the memory question both drew the long one. Nothing on this
side reproduces it: the same ingress, both HTTP versions, idle windows and
concurrent connections all answer in 100ms. So it is not fixed; it is taken
off camera — see `offCamera` under Recording mechanics.

**A question still in the channel from an earlier take is "our" row.** Two
takes of one scene post the same words, and the first one already carries a
`1 reply` bar. `ask()` looked for "the last row with these words and a bar",
found the old one the instant it looked, opened the old thread, and sat on an
answer it had been told to ignore until the timeout. `send()` now counts the
rows that already say the words before it types, and `awaitReply()` waits for
one more to appear. (Tidy between takes anyway — the duplicate is on camera.)

**`textContent()` on nothing waits thirty seconds.** A Playwright locator
that matches no element does not return null; it waits for one, for the
default timeout, and only then does the `.catch(() => null)` run. Slack hides
the sender on a grouped row by design — the bot's "Edit what I remember here"
follow-up is one — so every walk back through the rows paid thirty seconds per
unnamed row per call, and a reply that was on screen the whole time took two
and a half minutes to "settle". `count()` first; it never waits.

**The thread pane is `threads_flexpane`, its close button `close_flexpane`.**
Both exist only while a pane is open — the static scout calls them missing —
and a pane is open more often than you think: Slack restores the last one on
load, including one that says "Couldn't load thread". Escape closes it only
when focus is inside it, which after `compose()` it never is.

**Slack boots blank.** Coming from the console, the client paints a white
frame for seconds before the channel. Open it inside an `offCamera` that opens
the scene, and the line starts on the first frame that shows something.

## Driving the console

**It is mounted under `/admin/`.** `/bundles` returns a perfectly good 404 page,
so the recording navigates there, sees a document, carries on, and dies twenty
seconds later on a selector that was correct the whole time. `consoleBase()`
carries the prefix; `npm run preflight` probes the routes.

**React-controlled inputs discard text typed before hydration.** The form then
posts empty fields, the server answers 400, the page does not move, and the
recording sails into scenes that cannot work. `stage.typeVerified` types at
hand speed, reads the value back, repairs it instantly if it did not stick, and
throws if it still will not hold.

**Identical accessible names are a trap with teeth.** Every preset row in a
bundle's Credentials tab has a button reading "Connect" — twenty-odd of them.
Matching the name reaches the first, which is ClickUp, so the script would have
typed the **GitHub token into ClickUp's credential form** and then pressed
"Test connection", sending a GitHub PAT to `api.clickup.com`. Scope to the row
that names the service.

**Probe, do not guess.** One throwaway script that opens a page and dumps its
buttons, inputs and dialogs costs under a minute and found: the real button is
"Create bundle", the field is `#name-dialog-input`, the service search does not
filter when set programmatically (React never sees it), and there is a **"Test
connection"** button nobody had scripted — which turned out to be the strongest
beat in that chapter.

**Never hold an index into Slack's list across an action.** A row found as
`nth(14)` is gone by the time the delete runs; the virtualised list reshuffles
on every change. Scope to the thread pane (`[data-qa="slack_kit_list"]`, the
last one on the page) and ask for `.last()` again before every action. And
hover a row near its top-left: a job-log dump is taller than the viewport, so
its centre is off screen and the toolbar never appears.

**A "delete the oldest match" that keeps going deletes them all.** After the
oldest goes, the next one is the oldest. `tidy --first` removed both copies of
a question when it was meant to remove the duplicate; it stops after one now.

**Delete replies before their parent.** Deleting a thread parent with replies
leaves "This message was deleted" with the thread hanging off it. `npm run
tidy` removes the recorder's leftovers in the right order.

**`clickIfPresent` is a silent no-op by design, so never let a scene end on
one.** Part 3 "succeeded" three times over while the repository never
connected and the bundle never attached — every optional click missed and the
narration described writes that did not happen. Every console scene that
changes something now ends with `assertDb(...)`: a row exists, or the take
stops with the name of the step that failed.

**The console's workspace rail entries are buttons, not links.** A link-role
match finds nothing, the scene stays on the workspace page, and three writes
land on the wrong scope. Probe roles; do not infer them from what a thing
looks like.

**Gate on the script's exit code, not `tail`'s.** `tsx … | tail -4 && render`
starts the render whatever the script did, because the pipeline's status is
the last command's. Use `set -o pipefail` or run the check without a pipe.

## The product's own gates will stop you

These are not bugs. They are the product working, and each one halts a take.

**Email verification.** Connecting a workspace requires a confirmed address —
but only when a mail provider is configured (`needsVerifiedEmail` returns early
otherwise). The recording invents a fresh address every take, so the
confirmation link goes somewhere nobody can open. The recorder does what
clicking it does: one column, one row, gate left switched on.

**The sign-up allowlist.** Sign-up seeds `allowed_email_domains` from the
sign-up address. A throwaway at `example.com` therefore refuses every real
member of the Slack workspace — logged server-side as `bot use refused: email
domain`, silent in Slack, indistinguishable from a bot that is ignoring you.
The recorder clears it, which is also what production ships with.

**A workspace cannot move between organisations.** `SaveTeam` upserts `where
teams.org_id = excluded.org_id` and returns `ErrTeamOwnedElsewhere` otherwise —
deliberate isolation. So `0-signup`, which founds an org and installs the
workspace into it, only works from an empty database: `FRESH_DB=1
./start_dev.sh`.

**OAuth scopes are all-or-nothing.** A scope the Slack app is not configured to
grant fails the *entire* install with "Invalid permissions requested" and
nothing connects. `channels:leave`/`groups:leave` were not offered in the OAuth
picker for this app, so they came out of `botScopes` — and out of the README
manifest and the marketplace page with them, because the code comment says all
three must stay in step.

## Recording mechanics

**Playwright records the page, not the browser.** There is no address bar in
the output, so nothing says which host it was — which is what makes recording
against a local instance free, and what made the production mishap invisible.

**A persistent Chrome profile beats a `storageState` snapshot.** A snapshot is
a photograph of a session and expires, usually discovered mid-render. A profile
is the session. One profile signed into both Slack and the console also removes
the need to merge two states — and one page logged into two sites is what lets
a take cross from a Slack thread to the console without a cut.

**Every part is its own browser run, and opens on a title card** painted with
`setContent`. A part whose first scene speaks into a channel has no Slack
loaded at all. Heal it where the thing is needed — `compose()` ensures a
channel is open — not in one helper that some scenes bypass.

**Do not pipe a render through `tail`.** It buffers until exit, so there is no
live progress and a monitor watching the file never fires.

**One render at a time**, enforced by a pid lock — and Chrome locks the profile
directory, so a killed run leaves a `SingletonLock` the next one trips over.

**Waits go off camera; the video is cut, not held.** `s.offCamera(fn)` runs
`fn` after the scene's line has finished speaking and drops the frames it took
from the encode (`fps=30,select='not(between(t,a,b))',setpts=N/(30*TB)` in
front of the mux); every later scene's offset moves up by the cut, so the
narration and the chapter manifest stay aligned. Split the bot round trip in
two around it — `send()` on camera, `awaitReply()` + `closeThread()` inside the
cut, `openThreadOf()` on camera — and the seam is a channel with our message,
then the same channel with a `1 reply` bar under it, then the click that opens
the answer. A minute of Slack retrying disappears; the four seconds the product
actually takes is the honest number, and other parts still show real waits.

**Slack's thread pane is a bad opener.** The fix-job thread carries the
worker's log; asking Slack to load it as a part's first shot gave a blank pane
one take and "Couldn't load thread" the next. The console's Jobs page shows
the same result — status, cost, the pull request — and is ours to render.

## Writing the script

**Narrate only what is on screen.** A line read "here is a thread that has been
running for two days, with three people in it" over a channel that had neither.
Had the selector happened to work, that video would have shipped. Describe what
is actually there — it is both true and usually the better shot.

**Write bot-wait lines to the measured round trip** — or put the wait off
camera. Longer than the wait is fine (the answer sits on screen); shorter holds
the shot in silence, and the round trip here is anywhere from four seconds to
seventy, so a line cannot be written to it. `offCamera` is the answer for any
wait that is not the product's own.

**Secrets that appear on camera have to die on camera.** Both credentials go in
as throwaways scoped to one repository and one fresh tracker, and are revoked
when the recording is done. Every secret field in the console is
`type="password"`, so they render as dots — do not click a reveal toggle.

**Reuse what the project already has.** A `reset-db.ts` was written and then
deleted on finding `FRESH_DB=1 ./start_dev.sh`, which already stops the old
process and rebuilds.
