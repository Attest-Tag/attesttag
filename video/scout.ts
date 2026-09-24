/**
 * Checks what is actually in Slack's DOM before a word of script is written.
 *
 * Run:  npm run scout                  read-only; posts nothing
 *       npm run scout -- --ask         also asks the bot once, and times it
 *
 * This exists because of the first rule in LEARNINGS.md: the app is rarely
 * what the brief assumes, and finding that out by recording the whole guide
 * costs twenty minutes and a set of paid narration a go. It is worse here than
 * it was for a first-party app — half the surface belongs to Slack, who change
 * it whenever they like and owe us no notice.
 *
 * Two jobs:
 *
 *   1. Every selector in slack.ts's SEL map, present or missing, with a count.
 *      A miss prints nearby `data-qa` values so the replacement is usually
 *      visible in the output rather than needing a second run.
 *
 *   2. With --ask: how long the bot actually takes. A chapter is written very
 *      differently for an eight-second answer than a ninety-second one, and
 *      that number cannot be read off the source — it depends on the model,
 *      the tools the question happens to call, and the day.
 *
 * --ask posts a real message into whichever workspace the profile is signed
 * in to. It is off by default for that reason.
 */
import path from "node:path";
import { config } from "dotenv";
import { firstPage, launchProfile } from "./profile";
import { installCursor } from "./stage";
import { SEL, SlackStage } from "./slack";

config({ path: path.join(process.cwd(), ".env") });

// Falls back to the channel the walkthrough is recorded in, not to a name
// nobody has created — a scout that opens a channel that does not exist fails
// on the one step that was meant to be checking something else.
const CHANNEL =
  process.env.VIDEO_SCOUT_CHANNEL ?? process.env.VIDEO_CHANNEL ?? "attest-demo";
const QUESTION =
  process.env.VIDEO_SCOUT_QUESTION ??
  `@${process.env.VIDEO_BOT_NAME ?? "attest_tag"} in one sentence, what did this channel decide about the retry budget?`;

async function main() {
  const ask = process.argv.includes("--ask");

  const context = await launchProfile({ headless: false });
  await installCursor(context);
  const page = await firstPage(context);
  const slack = new SlackStage(page);

  console.log("Opening the client…");
  await slack.open().catch((err) => {
    throw new Error(
      `Could not boot a signed-in Slack client.\n${err}\n\n` +
        `If it is signed out: npm run signin -- slack`,
    );
  });

  // --- 1. the selectors -------------------------------------------------
  // These exist only while something is happening — a thread open, a mention
  // half-typed. Absent on a resting screen is correct, and reporting it as a
  // miss sends you hunting for a replacement for a selector that is fine.
  const TRANSIENT = new Set(["autocomplete", "viewThread", "replyInThread"]);

  console.log("\nSelectors");
  const missing: string[] = [];
  for (const [name, selector] of Object.entries(SEL)) {
    const count = await page.locator(selector).count().catch(() => 0);
    const mark = count ? "✓" : TRANSIENT.has(name) ? "·" : "✗";
    const note = !count && TRANSIENT.has(name) ? "  (transient — checked in use)" : "";
    console.log(`  ${mark} ${name.padEnd(16)} ${count} × ${selector}${note}`);
    if (!count && !TRANSIENT.has(name)) missing.push(name);
  }

  if (missing.length) {
    // Print the hooks that DO exist, so the replacement is usually right here
    // rather than needing another run. Capped: a booted client has hundreds.
    const present: string[] = await page
      .locator("[data-qa]")
      .evaluateAll((els) =>
        Array.from(
          new Set(els.map((e) => e.getAttribute("data-qa") ?? "")),
        ).filter(Boolean),
      )
      .catch(() => []);
    console.log(`\n  ${missing.length} missing. data-qa values on this screen:`);
    for (const v of present.slice(0, 120)) console.log(`    ${v}`);
    if (present.length > 120) console.log(`    … and ${present.length - 120} more`);
  }

  // --- 2. what the workspace holds --------------------------------------
  const channels = await page
    .locator(SEL.sidebarChannel)
    .allTextContents()
    .catch(() => []);
  console.log(`\nChannels in the sidebar (${channels.length})`);
  for (const c of channels) console.log(`  ${c.trim()}`);

  // --- 3. how long the bot takes ----------------------------------------
  if (!ask) {
    console.log("\nSkipped the round trip. Pass --ask to time one (it posts a message).");
  } else {
    console.log(`\nAsking in #${CHANNEL}…`);
    await slack.openChannel(CHANNEL);
    const t0 = Date.now();
    const sent = Date.now();
    const text = await slack.ask(QUESTION);
    const done = Date.now();
    console.log(`  opened channel in ${((sent - t0) / 1000).toFixed(1)}s`);
    console.log(`  asked + answered in ${((done - sent) / 1000).toFixed(1)}s (typing, thread open and settle included)`);
    console.log(`  ${text.length} characters`);
    console.log(
      `\n  Write the narration over this scene to about ` +
        `${Math.ceil((done - sent) / 1000)}s — at ~145 wpm that is roughly ` +
        `${Math.ceil(((done - sent) / 1000 / 60) * 145)} words.`,
    );
  }

  console.log("\nLeaving the window open for 30s so you can look around.");
  await page.waitForTimeout(30_000);
  await context.close();
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
