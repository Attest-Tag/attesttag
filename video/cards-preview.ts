/**
 * Paints the title, seam and end cards to PNGs so they can be looked at
 * without recording anything.
 *
 * Run:  npx tsx cards-preview.ts  [out-dir]
 *
 * The cards are the first and last thing a viewer sees and they are the one
 * part of a walkthrough that has nothing to do with the app — so checking a
 * wrapped heading by re-rendering twenty minutes of video, which is the only
 * other way to see one, is absurd. This takes about two seconds.
 */
import { mkdir } from "node:fs/promises";
import path from "node:path";
import { chromium } from "playwright";
import { endCard, interstitialCard, titleCard } from "./cards";

const VIEWPORT = { width: 1440, height: 900 };

const TITLE = "One week in a channel, start to finish";
const CHAPTERS = [
  "Invite it to the channel",
  "Ask it something real",
  "Connect a repository",
  "A write stops for a human",
  "Who approved what",
];

async function main() {
  const outDir = path.resolve(process.argv[2] ?? "card-preview");
  await mkdir(outDir, { recursive: true });

  // The installed Chrome, like every other launch in this folder — so nothing
  // here needs Playwright's bundled browsers downloaded at all.
  const browser = await chromium.launch({ channel: "chrome" });
  const page = await browser.newPage({
    viewport: VIEWPORT,
    deviceScaleFactor: 2,
  });

  for (const [name, html] of [
    ["title", titleCard(TITLE, CHAPTERS)],
    ["seam", interstitialCard()],
    ["end", endCard(TITLE)],
  ] as const) {
    await page.setContent(html);
    const file = path.join(outDir, `card-${name}.png`);
    await page.screenshot({ path: file });
    console.log(`  ${file}`);
  }

  await browser.close();
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
