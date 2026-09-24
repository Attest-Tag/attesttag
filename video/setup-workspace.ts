/**
 * Makes the channel the walkthrough is recorded in, and puts a week of
 * plausible backstory in it.
 *
 * Run:  npx tsx setup-workspace.ts            create the channel, invite the bot
 *       npx tsx setup-workspace.ts --seed     …and post the backstory
 *
 * **This posts into whatever workspace the profile is signed into.** It is a
 * separate script from the recorder, and the seeding is behind a flag, for
 * that reason.
 *
 * ## Why a new channel rather than an existing one
 *
 * An existing channel has real conversation in it, and the walkthrough is a
 * public video. A channel made for the recording contains only what this file
 * put there, which is the only way to be sure. Every other channel in the
 * workspace is hidden from the sidebar while recording — see `keepOnlyChannels`
 * in slack.ts — but the channel we are *in* is the one thing that cannot be
 * hidden, so it has to be clean by construction.
 */
import path from "node:path";
import { config } from "dotenv";
import { firstPage, launchProfile } from "./profile";
import { installCursor } from "./stage";
import { SEL, SlackStage } from "./slack";

config({ path: path.join(process.cwd(), ".env") });

const CHANNEL = process.env.VIDEO_CHANNEL ?? "attest-demo";
const BOT = process.env.VIDEO_BOT_NAME ?? "attest_tag";

/**
 * A week of an engineering channel, invented whole.
 *
 * It has to carry real weight, because two chapters of the walkthrough turn on
 * the bot having read it: one asks what the channel decided, and one asks what
 * is still open. A thread of "hi" and "thanks" makes those chapters a lie —
 * the bot would be summarising nothing, and it would show.
 *
 * So there is a decision in here that was made and then revised, a question
 * nobody answered, and a number somebody will want quoted back. No real
 * customer, person, repository or incident appears.
 */
const BACKSTORY: string[] = [
  "Morning — kicking off the week. Retry budget work is the main thing; the checkout timeouts from Thursday are still unexplained.",
  "I pulled the numbers. 0.4% of checkout calls timed out over the weekend, all of them on the payments path, none anywhere else.",
  "0.4% is about 900 requests. Small, but they're all at the worst possible moment.",
  "Proposal: bump the retry budget on the payments client from 2 to 5 and move to exponential backoff with jitter.",
  "Careful with 5. If the downstream is already struggling, five retries per request is how a brownout becomes an outage.",
  "Fair. What if we cap total retry time rather than retry count? 2.5s budget per request, however many attempts fit.",
  "I like that better. It's bounded from the caller's side, which is the side that actually cares.",
  "Agreed — let's go with a 2.5s total budget and jittered backoff, and drop the fixed count entirely.",
  "One thing we haven't settled: what do we do when the budget is exhausted? Fail closed, or fall back to the queue?",
  "Queue, I'd think. A delayed charge is recoverable, a dropped one is a support ticket.",
  "That needs a change on the reconciliation side too, so it's not free. Worth its own ticket.",
  "Raised it. Nobody's picked it up yet.",
  "Deploy question while we're here — are we still Tuesdays only? I have this ready but I'm not shipping it on a Friday.",
  "Tuesdays only, yes. Friday deploys are how you spend a weekend on a rollback.",
  "Then it goes out Tuesday. I'll write the runbook entry before then.",
];

async function main() {
  const seed = process.argv.includes("--seed");

  // No keepOnlyChannels here, deliberately. This script is not a recording,
  // and hiding the channel list while trying to add a channel to it is how the
  // first version of this failed.
  const context = await launchProfile({ headless: false });
  await installCursor(context);
  const page = await firstPage(context);
  const slack = new SlackStage(page);

  await slack.open();

  // Does it already exist? Cheaper than handling the "name taken" dialog, and
  // it makes this script safe to run twice.
  const existing = page.locator(
    `[data-qa="channel_sidebar_name_${CHANNEL}"]`,
  );
  if (await existing.first().isVisible().catch(() => false)) {
    console.log(`#${CHANNEL} already exists.`);
  } else {
    console.log(`Creating #${CHANNEL}…`);
    // Two ways in, both hooks the scout actually saw on screen.
    //
    // The "Add channels" row at the foot of the Channels section is the one to
    // try first: it is always rendered. The "+" beside the section heading is
    // only painted while the pointer is over the heading, which is why
    // clicking it straight off is a thirty-second timeout on a selector that
    // is perfectly correct.
    const entries = [
      { name: "add-channels row", locator: page.locator('[data-qa="channel_sidebar_add_more_items_channel"]') },
      { name: "section +", locator: page.locator('[data-qa="section_heading_button_plus__channels"]') },
    ];
    let opened = false;
    for (const entry of entries) {
      if (entry.name === "section +") {
        await page
          .locator('[data-qa="channel_sidebar__section_heading_label__channels"]')
          .first()
          .hover()
          .catch(() => {});
        await page.waitForTimeout(600);
      }
      if (!(await entry.locator.first().isVisible().catch(() => false))) continue;
      await entry.locator.first().click().catch(() => {});
      await page.waitForTimeout(1_200);
      opened = true;
      console.log(`  (opened via the ${entry.name})`);
      break;
    }
    if (!opened) {
      throw new Error(
        `Could not open Slack's create-channel menu.\n\n` +
          `Not worth fighting — Slack redraws this menu regularly and it\n` +
          `differs by plan. Create #${CHANNEL} by hand, then re-run: this\n` +
          `script will find it, invite the bot, and post the backstory.`,
      );
    }
    await page
      .getByRole("menuitem", { name: /create.*channel|new channel/i })
      .first()
      .click()
      .catch(() => {});
    await page.waitForTimeout(1_500);

    const nameField = page
      .getByRole("textbox", { name: /name/i })
      .or(page.locator('input[data-qa="channel-name-input"]'))
      .first();
    await nameField.waitFor({ state: "visible", timeout: 20_000 });
    await nameField.pressSequentially(CHANNEL, { delay: 55 });
    await page.waitForTimeout(800);

    // Slack has shown this as a one-step dialog and as a two-step wizard
    // (name, then visibility) depending on the plan and the week, so both
    // buttons are pressed if present and neither is required.
    for (const label of [/^next$/i, /^create$/i]) {
      const button = page.getByRole("button", { name: label }).first();
      if (await button.isVisible().catch(() => false)) {
        await button.click().catch(() => {});
        await page.waitForTimeout(2_000);
      }
    }
    // The "add people" step, if it appeared. Skipping it leaves the channel
    // made and empty, which is what we want.
    await page
      .getByRole("button", { name: /skip|done|finish/i })
      .first()
      .click()
      .catch(() => {});
    await page.waitForTimeout(3_000);
  }

  await slack.openChannel(CHANNEL).catch(() => {
    throw new Error(
      `#${CHANNEL} is still not in the sidebar. Create it by hand and re-run.`,
    );
  });

  console.log(`Inviting @${BOT}…`);
  await slack.compose(`/invite @${BOT}`);
  await page.waitForTimeout(3_000);

  if (!seed) {
    console.log("\nSkipped the backstory. Re-run with --seed to post it.");
  } else {
    console.log(`Posting ${BACKSTORY.length} messages…`);
    for (const [i, line] of BACKSTORY.entries()) {
      // No mentions in the backstory, so this never touches the autocomplete.
      await slack.compose(line);
      await page.waitForTimeout(700);
      if ((i + 1) % 5 === 0) console.log(`  ${i + 1}/${BACKSTORY.length}`);
    }
    console.log(
      "\n  All of it is from one account, which is the limitation worth\n" +
        "  knowing about: a 'team discussion' in one voice reads oddly on\n" +
        "  camera. Invite a second person to the workspace and re-run the\n" +
        "  second half as them if that matters.",
    );
  }

  const left = await page
    .locator(SEL.sidebarChannel)
    .evaluateAll((els) =>
      els
        .filter((e) => (e as HTMLElement).offsetParent !== null)
        .map((e) => e.getAttribute("data-qa")),
    )
    .catch(() => []);
  console.log(`\nVisible in the sidebar while recording: ${left.join(", ") || "(none)"}`);

  await page.waitForTimeout(3_000);
  await context.close();
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
