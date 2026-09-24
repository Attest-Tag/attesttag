/**
 * Removes the recorder's own leftovers from the demo channel, so a take opens
 * on a channel that holds only the seeded conversation.
 *
 * Run:  npm run tidy
 *
 * Every failed take posts something: a question the bot answered, a thread
 * under it, a `/invite` that never sent. It all sits between the backstory and
 * whatever the next take posts, and it is on camera. This deletes:
 *
 *   - every message that mentions the bot (our questions, from any take)
 *   - every message the bot posted (its answers)
 *   - anything starting with a slash command
 *
 * and nothing else. The backstory — Jack's and alex's ordinary messages —
 * and Slack's own "was added to the channel" line are left exactly as they
 * are.
 *
 * Thread replies are deleted before their parent. Deleting a parent that still
 * has replies leaves a "This message was deleted" tombstone with the thread
 * hanging off it, which is worse than the message was.
 */
import path from "node:path";
import { config } from "dotenv";
import type { Locator, Page } from "playwright";
import { firstPage, launchProfile } from "./profile";
import { SEL, SlackStage } from "./slack";

config({ path: path.join(process.cwd(), ".env") });

const CHANNEL = process.env.VIDEO_CHANNEL ?? "attesttag-demo";
const BOT = (process.env.VIDEO_BOT_NAME ?? "attestTag").toLowerCase().replace(/[^a-z0-9]/g, "");

const norm = (v: string) => v.toLowerCase().replace(/[^a-z0-9]/g, "");

/** The sender named on a row, without waiting for one that is not there. */
async function senderOf(row: Locator): Promise<string> {
  const name = row.locator(SEL.messageSender).first();
  if ((await name.count().catch(() => 0)) === 0) return "";
  return (await name.textContent({ timeout: 2_000 }).catch(() => "")) ?? "";
}

/**
 * Is this row ours to remove? `inherited` is the sender of the nearest named
 * row above — Slack hides the name on a grouped consecutive message, and a
 * fix job's result is exactly one of those: nameless, under the bot's card.
 */
async function isJunk(row: Locator, inherited = ""): Promise<boolean> {
  const own = norm(await senderOf(row));
  const sender = own || inherited;
  const text = ((await row.locator(SEL.messageText).allTextContents().catch(() => [])) ?? []).join(" ");
  if (sender && sender.includes(BOT)) return true;
  if (/^\s*\//.test(text)) return true;
  // A mention renders as the handle text; match it loosely.
  return norm(text).includes("@" + BOT) || norm(text).includes(BOT + "what") || /@attest/i.test(text);
}

/** Hover a row, open its actions, delete it, confirm. */
async function deleteRow(page: Page, row: Locator): Promise<boolean> {
  await row.scrollIntoViewIfNeeded().catch(() => {});
  await page.waitForTimeout(300);
  // Near the row's top-left: a row taller than the viewport (a job log dump)
  // is scrolled to align its top, so 20px in is on screen; its centre is not.
  const hovered = await row.hover({ position: { x: 40, y: 20 }, timeout: 8_000 }).then(() => true).catch(() => false);
  if (!hovered) return false;
  await page.waitForTimeout(600);
  const more = page.locator('[data-qa="more_message_actions"]').first();
  if (!(await more.isVisible().catch(() => false))) return false;
  await more.click();
  await page.waitForTimeout(700);
  const del = page.locator('[data-qa="delete_message"]').first();
  if (!(await del.isVisible().catch(() => false))) {
    await page.keyboard.press("Escape");
    return false;
  }
  await del.click();
  await page.waitForTimeout(800);
  // The confirmation dialog.
  const confirm = page.getByRole("dialog").getByRole("button", { name: /^delete$/i }).first();
  if (await confirm.isVisible().catch(() => false)) {
    await confirm.click();
  } else {
    await page.getByRole("button", { name: /^delete$/i }).last().click().catch(() => {});
  }
  await page.waitForTimeout(1_000);
  return true;
}

async function main() {
  // --last: remove only the newest junk parent (and its replies). For putting
  // one failed part back, without erasing the answered questions the parts
  // before it legitimately left in the channel.
  const onlyLast = process.argv.includes("--last");
  // --match <regex>: only parents whose text matches (and their threads). For
  // putting one failed part back without touching what the parts before it
  // legitimately left in the channel.
  // --first: with --match, take the OLDEST matching parent instead of the
  // newest — for a duplicate question left by an abandoned run, where the
  // newest is the take that counts.
  const oldest = process.argv.includes("--first");
  const mi = process.argv.indexOf("--match");
  const only = mi >= 0 && process.argv[mi + 1] ? new RegExp(process.argv[mi + 1], "i") : null;
  const ctx = await launchProfile({ headless: false });
  const page = await firstPage(ctx);
  const slack = new SlackStage(page);
  await slack.open();
  await slack.openChannel(CHANNEL);
  await page.waitForTimeout(2_000);

  let removed = 0;
  for (let pass = 0; pass < 40; pass++) {
    // Close any open thread so the channel rows are what we iterate.
    await page.keyboard.press("Escape").catch(() => {});
    await page.waitForTimeout(400);
    const rows = page.locator(SEL.message);
    const n = await rows.count();

    // 1. Threads first. Any junk parent — or a tombstone, a parent deleted
    //    while it still had replies — with a thread bar: open it and delete
    //    the replies from the PANE'S OWN LIST, newest first, re-resolving
    //    "the last row" before every action. Holding an index into Slack's
    //    virtualised list does not survive a single deletion; scoping to the
    //    pane and asking for `.last()` again each time does.
    let opened = false;
    const scan = oldest ? [...Array(n).keys()] : [...Array(n).keys()].reverse();
    for (const i of scan) {
      const row = rows.nth(i);
      const tomb = /This message was deleted/i.test((await row.innerText().catch(() => "")) ?? "");
      const text0 = ((await row.locator(SEL.messageText).allTextContents().catch(() => [])) ?? []).join(" ");
      if (only && !tomb && !only.test(text0)) continue;
      const bar = row.locator(SEL.viewThread).first();
      if ((tomb || (await isJunk(row))) && (await bar.isVisible().catch(() => false))) {
        await bar.click();
        await page.waitForTimeout(2_000);
        opened = true;
        const pane = page.locator('[data-qa="slack_kit_list"]').last();
        for (let k = 0; k < 12; k++) {
          const count = await pane.locator(SEL.message).count();
          if (count <= 1) break; // only the parent is left in the pane
          const last = pane.locator(SEL.message).last();
          // Never delete a human's reply: the parent is excluded by count, and
          // a reply that names a person is theirs.
          const who = norm(await senderOf(last));
          if (who && !who.includes(BOT)) break;
          if (await deleteRow(page, last)) { removed++; console.log(`  – thread reply`); }
          else break;
        }
        break;
      }
    }
    if (opened) continue;

    // 2. No threads left to clear: delete the newest junk parent.
    let target: Locator | null = null;
    const order = oldest ? [...Array(n).keys()] : [...Array(n).keys()].reverse();
    for (const i of order) {
      const row = rows.nth(i);
      if (only) {
        const text0 = ((await row.locator(SEL.messageText).allTextContents().catch(() => [])) ?? []).join(" ");
        if (!only.test(text0)) continue;
      }
      if (await isJunk(row)) { target = row; break; }
    }
    if (!target) break;
    const text = ((await target.locator(SEL.messageText).allTextContents().catch(() => [])) ?? []).join(" ").slice(0, 60);
    if (await deleteRow(page, target)) { removed++; console.log(`  – ${text}`); }
    else { console.log(`  ! could not delete: ${text}`); break; }
    if (onlyLast || oldest) break; // --first means ONE, or the next-oldest goes too
  }

  console.log(`\nRemoved ${removed} message(s) from #${CHANNEL}.`);
  await page.keyboard.press("Escape").catch(() => {});
  await page.waitForTimeout(600);
  const rows = page.locator(SEL.message);
  const n = await rows.count();
  let junkLeft = 0, tombstones = 0, last = "";
  for (let i = 0; i < n; i++) {
    const r = rows.nth(i);
    const own = norm(await senderOf(r));
    if (own) last = own;
    const text1 = ((await r.locator(SEL.messageText).allTextContents().catch(() => [])) ?? []).join(" ");
    if (only ? only.test(text1) && !last.includes(BOT) : !onlyLast && (await isJunk(r, last))) junkLeft++;
    if (/This message was deleted/i.test((await r.innerText().catch(() => "")) ?? "")) tombstones++;
  }
  console.log(`${n} message(s) remain · junk left: ${junkLeft} · tombstones: ${tombstones}`);
  await page.waitForTimeout(800);
  await ctx.close();
  // A non-zero exit is the only thing a shell `&&` can see.
  if (junkLeft > 0 || tombstones > 0) process.exit(1);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
