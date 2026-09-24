import { execFileSync } from "node:child_process";
import { createHmac } from "node:crypto";
import path from "node:path";
import type { Locator } from "playwright";
import { walkthroughsDir } from "../paths";
import { SEL } from "../slack";
import type { Surfaces } from "../types";

// Helpers every walkthrough shares. Lifted out of guides/deep-dive.ts when the
// second walkthrough was scripted, so a fix to `assertDb` or `openChannelPage`
// lands in all of them rather than in the one file somebody remembered.
//
// Every env reader here reads process.env WHEN CALLED — see the note below and
// LEARNINGS.md ("Environment, and where it is read").

// ---------------------------------------------------------------------------
// Every one of these reads process.env WHEN CALLED. index.ts loads the env
// files after its imports have evaluated, so a module-level const resolves to
// the fallback on every run.
export const channel = () => process.env.VIDEO_CHANNEL ?? "attesttag-demo";
export const bot = () => process.env.VIDEO_BOT_NAME ?? "attestTag";
export const siteUrl = () => process.env.VIDEO_SITE_URL ?? "https://attesttag.com";

export const BUNDLE = "Engineering";

export const githubToken = () => process.env.VIDEO_GITHUB_TOKEN ?? "";
export const githubRepo = () => process.env.VIDEO_GITHUB_REPO ?? "acme/demo-repo";
export const clickupToken = () => process.env.VIDEO_CLICKUP_TOKEN ?? "";

export const signupOrg = () => process.env.VIDEO_SIGNUP_ORG ?? "Northwind";
export const signupName = () => process.env.VIDEO_SIGNUP_NAME ?? "Sam Reyes";
export const signupPassword = () => process.env.VIDEO_SIGNUP_PASSWORD ?? "";

/** Plus-addressed with the minute: a repeat address is a 409. */
export function signupEmail(): string {
  const base = process.env.VIDEO_SIGNUP_EMAIL;
  if (!base || !signupPassword()) {
    throw new Error(
      "0-signup founds an account on camera and needs VIDEO_SIGNUP_EMAIL and\n" +
        "VIDEO_SIGNUP_PASSWORD in video/.env (gitignored). Use a throwaway.",
    );
  }
  const stamp = new Date().toISOString().slice(0, 16).replace(/\D/g, "");
  const [user, domain] = base.split("@");
  return `${user}+${stamp}@${domain}`;
}

export const DB = () => path.join(process.cwd(), "..", "testing.db");

/**
 * What clicking the confirmation link does — one column, one row. The address
 * is invented for the recording, so the real link goes nowhere. The gate
 * itself (`needsVerifiedEmail`) stays switched on.
 */
export function confirmEmailInDatabase(email: string): void {
  execFileSync(
    "sqlite3",
    [DB(), `update users set email_verified=1 where email='${email.replace(/'/g, "''")}';`],
    { stdio: "ignore" },
  );
}

/**
 * Sign-up seeds `allowed_email_domains` from the sign-up address, which for an
 * invented address locks every real Slack member out ("bot use refused: email
 * domain", server-side, silent in Slack). Empty is what production ships with.
 */
export function clearEmailAllowlist(): void {
  execFileSync("sqlite3", [DB(), "delete from settings where key='allowed_email_domains';"], {
    stdio: "ignore",
  });
}

/** A team row exists, or the take stops. Never narrate success over a failure. */
export function assertWorkspaceConnected(): void {
  const rows = execFileSync("sqlite3", [DB(), "select count(*) from teams;"], {
    encoding: "utf8",
  }).trim();
  if (rows === "0") {
    throw new Error(
      "No Slack workspace connected — the install did not complete. Look at the\n" +
        "frames around the consent screen; a scope the app cannot grant fails the\n" +
        'whole install with "Invalid permissions requested".',
    );
  }
}

/** How many fix jobs exist. The confirm scene proves one was dispatched. */
export function jobCount(): number {
  return Number(execFileSync("sqlite3", [DB(), "select count(*) from jobs;"], { encoding: "utf8" }).trim() || "0");
}

/** Status of the newest fix job, or "" if there is none. */
export function latestJobStatus(): string {
  return execFileSync("sqlite3", [DB(), "select coalesce(status,'') from jobs order by id desc limit 1;"], { encoding: "utf8" }).trim();
}

/**
 * Block until the newest fix job has finished. Called from 7-memory's
 * `before`, so the minutes the worker takes pass between parts, off camera,
 * rather than as a held shot inside one.
 */
export async function waitForJob(timeoutMs = 900_000): Promise<void> {
  const started = Date.now();
  for (;;) {
    const status = latestJobStatus();
    if (status === "succeeded") return;
    if (/failed|error|cancel/i.test(status)) throw new Error(`The fix job ended with status "${status}".`);
    if (Date.now() - started > timeoutMs) throw new Error(`The fix job did not finish in ${Math.round(timeoutMs / 60000)} min (status "${status}").`);
    await new Promise((r) => setTimeout(r, 5_000));
  }
}

/** Wait for a real console session, not for a URL. */
export async function waitSignedIn(s: Surfaces, timeoutMs = 45_000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const ok = await s.page
      .evaluate(async () => {
        const r = await fetch("/api/me", { credentials: "include", cache: "no-store" });
        if (!r.ok) return false;
        return ((await r.json()) as { signed_in?: boolean }).signed_in === true;
      })
      .catch(() => false);
    if (ok) return;
    if (Date.now() > deadline) {
      const shown = await s.page
        .locator('[role="alert"], .text-destructive')
        .first()
        .textContent()
        .catch(() => null);
      throw new Error(`The sign-up did not start a session.` + (shown ? `\nThe page says: ${shown.trim()}` : ""));
    }
    await s.wait(500);
  }
}

/**
 * The "Connect" button on one preset's row. Every row has one and they all
 * read "Connect"; matching the button alone reaches whichever is first.
 */
export const presetRow = (s: Surfaces, name: RegExp) =>
  s.page.locator("li").filter({ hasText: name }).getByRole("button", { name: "Connect" }).first();
export const clickupRow = (s: Surfaces) => presetRow(s, /^ClickUp/);

/**
 * The channel's page in the console. The rail entry is a BUTTON reading
 * "attesttag-demo C0C…", not a link — a link-role match found nothing, the
 * scene silently stayed on the workspace page, and three writes went to the
 * wrong scope or nowhere.
 */
export async function openChannelPage(s: Surfaces): Promise<void> {
  await s.app.goto("/workspaces");
  await s.wait(1_400);
  await s.app.click(s.page.getByRole("button", { name: new RegExp(`^${channel()}`, "i") }).first());
  await s.wait(1_400);
}

/**
 * A write happened, or the take stops. Every console scene that changes
 * something ends by asking the database, because a click that lands on
 * nothing looks identical to one that worked — and a video narrating a change
 * that did not happen is worse than no video.
 */
export function assertDb(sql: string, min: number, what: string): void {
  const n = Number(execFileSync("sqlite3", [DB(), sql], { encoding: "utf8" }).trim() || "0");
  if (n < min) throw new Error(`${what} — the database does not show it (${sql} → ${n}).`);
}

/**
 * Where every finished walkthrough lands (paths.ts).
 *
 * A const, unlike everything above it: WALKTHROUGHS_DIR is a shell variable by
 * design rather than a dotenv key, and a guide's LongForm literal captures this
 * string as it is imported anyway, so reading it later would not help.
 */
export const outDir = walkthroughsDir();

/** Scratch directory for one walkthrough's part renders. Gitignored. */
export const partsDirFor = (id: string) => path.join(process.cwd(), "video-out", id);

/**
 * The state the deep dive leaves behind, which every later walkthrough starts
 * from: an organisation with the demo workspace connected, the repository and
 * the tracker bundle attached to the channel. Recording one of those against
 * an empty database fails at its first console scene with a selector timeout
 * that says nothing about why — so each one checks here, in `before`, and
 * names the run that is missing.
 */
export function assertDeepDiveState(): void {
  const wants: [string, number, string][] = [
    ["select count(*) from teams;", 1, "no Slack workspace is connected — run `npm run video:deep` (at least 0-signup) first"],
    ["select count(*) from connections where coalesce(repo,'') != '';", 1, "no repository is connected — run `npm run video:deep -- 3-connect` first"],
    ["select count(*) from scope_bundles;", 1, "no bundle is attached to the channel — run `npm run video:deep -- 3-connect` first"],
  ];
  for (const [sql, min, why] of wants) {
    const n = Number(execFileSync("sqlite3", [DB(), sql], { encoding: "utf8" }).trim() || "0");
    if (n < min) throw new Error(`This walkthrough records in the organisation the deep dive founded, and ${why}.`);
  }
}

/** Only the workspace, for a walkthrough that never touches a connection. */
export function assertWorkspaceState(): void {
  assertWorkspaceConnected();
}

/** One number out of the database, for a precondition or a proof. */
export function dbCount(sql: string): number {
  return Number(execFileSync("sqlite3", [DB(), sql], { encoding: "utf8" }).trim() || "0");
}

/** One string out of the database, or "". */
export function dbValue(sql: string): string {
  return execFileSync("sqlite3", [DB(), sql], { encoding: "utf8" }).trim();
}

// ---------------------------------------------------------------------------
// Slack-side helpers the shorter walkthroughs lean on.

/**
 * A bang command, the way the scripts send one: mentioned, so the bot hears
 * it in a channel like any other message, and answered as a plain text reply
 * with no footer — commands never reach the model, so nothing spends tokens
 * and nothing draws the model line. `ask` would wait for that footer forever.
 */
export async function command(s: Surfaces, text: string): Promise<string> {
  return s.slack.ask(`@${bot()} ${text}`, { requireFooter: false, settleMs: 2_500 });
}

// ---------------------------------------------------------------------------
// Two-factor, for the console walkthrough that enrols it.

/**
 * The six digits an authenticator app would show for a setup key, now.
 *
 * The enrolment card prints the key beside the QR code so a person can type
 * it into an app by hand; the recorder reads it off the page and answers the
 * same way an app would. RFC 6238 over RFC 4226, SHA-1, 30-second step, which
 * is what the server issues (internal/app/totp.go). The organisation is the
 * deep dive's throwaway, so a key on camera dies with it.
 */
export function totp(setupKey: string, at = Date.now()): string {
  const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";
  const clean = setupKey.toUpperCase().replace(/[^A-Z2-7]/g, "");
  let bits = "";
  for (const ch of clean) bits += alphabet.indexOf(ch).toString(2).padStart(5, "0");
  const key = Buffer.from((bits.match(/.{8}/g) ?? []).map((b) => parseInt(b, 2)));
  const counter = Buffer.alloc(8);
  counter.writeBigUInt64BE(BigInt(Math.floor(at / 1000 / 30)));
  const mac = createHmac("sha1", key).update(counter).digest();
  const offset = mac[mac.length - 1] & 0x0f;
  const code = ((mac[offset] & 0x7f) << 24) | (mac[offset + 1] << 16) | (mac[offset + 2] << 8) | mac[offset + 3];
  return String(code % 1_000_000).padStart(6, "0");
}

// ---------------------------------------------------------------------------
// Console-side helpers the shorter walkthroughs lean on.

/**
 * Draw the eye to something only if it is on screen. `point` waits twenty
 * seconds for a control and throws; a scene that merely gestures at a chip
 * the page renders conditionally should not lose the take over it.
 */
export async function pointIfPresent(s: Surfaces, target: Locator): Promise<boolean> {
  const el = target.first();
  if (!(await el.isVisible().catch(() => false))) return false;
  await s.app.point(el);
  return true;
}

/**
 * Type into a field that may already hold text from an earlier take.
 *
 * `typeVerified` clicks into the field and types; on a field that is not
 * empty that lands the caret mid-text and types into the middle of the old
 * value before the fallback `fill` repairs it — a strange few frames. Empty it
 * first (instant, invisible on a first take where it is already empty), then
 * type at hand speed as usual.
 */
export async function retype(s: Surfaces, selector: string, text: string, delay = 42): Promise<void> {
  const field = s.page.locator(selector).first();
  if ((await field.inputValue().catch(() => "")) !== "") await field.fill("");
  await s.app.typeVerified(selector, text, delay);
}

// ---------------------------------------------------------------------------
// Database helpers the shorter walkthroughs share.

/**
 * The organisation being recorded, as a subquery: the one that owns the
 * channel scope named by VIDEO_CHANNEL. Every table carries org_id, and a
 * database can hold more than one organisation — the first cut of this
 * picked "the newest active install", and the evening a second workspace
 * was connected under a fresh organisation, `cost` saw no spend and
 * `automation` could not find the routine it had just created. A function,
 * not a constant: the channel name is read from the environment when called.
 */
export const ORG = () =>
  `(select org_id from scopes where kind='channel' and name='#${channel().replace(/'/g, "''")}' order by id desc limit 1)`;

/**
 * The organisation's plan, read the way the product reads it: anything that is
 * not "pro" is free, and so is a database from before the column existed.
 */
export function orgPlan(): string {
  try {
    return dbValue(`select coalesce(plan,'free') from orgs where id=${ORG()};`) === "pro" ? "pro" : "free";
  } catch {
    return "free";
  }
}

/** A literal inside a RegExp — a channel name, an email address, a model id. */
export const escapeRe = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

/** A plain pause with no page to wait on — for a `before`, which has none. */
export const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

/**
 * Poll one count until it reaches `min`, or stop with the reason. For the
 * waits that belong between parts — an index one part started, a mirror a
 * Drive pass is still writing, a run a routine is still finishing.
 */
export async function waitForDb(sql: string, min: number, timeoutMs: number, why: string): Promise<void> {
  const started = Date.now();
  while (dbCount(sql) < min) {
    if (Date.now() - started > timeoutMs) {
      throw new Error(`${why} (${sql} → ${dbCount(sql)} after ${Math.round(timeoutMs / 1000)}s).`);
    }
    await sleep(2_000);
  }
}

/**
 * A bang command with the Slack wait cut out. `command()` asks and waits on
 * camera; the reply itself is instant (commands never reach the model), but
 * Slack's delivery is not (LEARNINGS: "Slack's first delivery fails, and the
 * retry is a minute away"), so the round trip is split around the cut the
 * way 7-memory does it: send on camera, wait inside the cut, open the thread
 * after. Use it where a held frame of nothing arriving would be the shot.
 */
export async function commandCut(s: Surfaces, text: string): Promise<void> {
  const q = `@${bot()} ${text}`;
  await s.slack.send(q);
  await s.wait(1_000);
  await s.offCamera(async () => {
    await s.slack.awaitReply(q, { requireFooter: false, settleMs: 2_500 });
    await s.slack.closeThread();
  });
  await s.slack.openThreadOf(q);
}

// ---------------------------------------------------------------------------
// The two-scene round trip.
//
// Slack's first delivery of an event to this bot fails more often than not
// through the tunnel it is recorded behind, and the retry lands anywhere from
// four seconds to seventy later (LEARNINGS.md) — none of which is the product.
// So a question is posted on camera under one line, and the NEXT scene opens
// with a cut that waits for the reply to exist; the line then begins on the
// frame the thread is about to open. For a model answer the cut ends the
// moment the bot's reply exists and the rest — the status line, the text
// streaming in, the footer — stays on camera, because that is the product
// working. For a bang command, posted whole with no model behind it, the whole
// round trip is in the cut and the thread opens on the finished reply.

/** A bang command, mentioned so the bot hears it in a channel like any message. Returns what was posted. */
export async function postCommand(s: Surfaces, text: string): Promise<string> {
  const q = `@${bot()} ${text}`;
  await s.slack.send(q);
  return q;
}

/**
 * Hold until the bot's first reply has put a thread bar under the message
 * `send` just posted — Slack's delivery, and the model's first token. The
 * NEWEST row saying our words is ours (`send` verified it posted), and the
 * bar is waited for on that row and no other: a row from an earlier take
 * carries the same words and a bar already, and `.last()` over rows-with-a-bar
 * would pick it the instant it was looked for (LEARNINGS.md, "A question
 * still in the channel from an earlier take is 'our' row").
 */
export async function waitForReplyBar(s: Surfaces, text: string, timeoutMs = 360_000): Promise<void> {
  // Six minutes, not two: Slack retries a failed delivery at about one
  // second, one minute and then five (LEARNINGS.md), and a take of
  // `automation` lost its first ask to a reply that arrived after a
  // three-minute wait had given up. This wait is inside a cut, so the
  // ceiling costs wall-clock and never a frame.
  const mine = s.page.locator(SEL.message).filter({ hasText: s.slack.fragment(text) }).last();
  await mine.locator(SEL.viewThread).first().waitFor({ state: "visible", timeout: timeoutMs }).catch(() => {
    throw new Error(
      `No reply bar under the message in ${Math.round(timeoutMs / 1000)}s — Slack has not ` +
        `delivered the event, or the mention posted as plain text. Check the row on screen ` +
        `shows a highlighted @${bot()}, and bot.log for the turn.`,
    );
  });
}

/**
 * Second scene of a model answer. Call it FIRST in `act`: the cut opens the
 * scene, so the line begins on the frame the thread is about to open. Then
 * the thread is opened and the answer watched in — the status line, the text,
 * the footer — on camera.
 */
export async function watchAnswer(s: Surfaces, q: string): Promise<string> {
  await s.offCamera(() => waitForReplyBar(s, q));
  // A model answer ends with the footer (model · tokens · cost · Configure),
  // and that is the only signal that separates a finished answer from the
  // status line the row holds while a tool runs — "Reading channel history"
  // is short, stable, and was accepted as an answer by the first take of
  // `ask`. Held cards and bare acknowledgements go through showReply instead.
  return s.slack.awaitReply(q, { requireFooter: true });
}

/**
 * Second scene of a reply that is complete the moment it is posted — a bang
 * command, or an acknowledgement — also first in `act`. The whole round trip
 * goes inside the cut and the thread opens on the finished reply. This is the
 * shape 7-memory in the deep dive uses for the same acknowledgement.
 */
export async function showReply(s: Surfaces, q: string): Promise<void> {
  await s.offCamera(async () => {
    await waitForReplyBar(s, q);
    await s.slack.awaitReply(q, { requireFooter: false, settleMs: 2_500 });
    await s.slack.closeThread();
  });
  await s.slack.openThreadOf(q);
  await s.wait(1_500);
}
