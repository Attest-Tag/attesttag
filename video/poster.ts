/**
 * The poster: the still a walkthrough shows before anyone presses play.
 *
 * Run:  npm run video:poster -- deep-dive            (after a stitch, if the
 *                                                     design or the frame changes)
 *
 * A raw frame makes a poor one. The frame that used to be grabbed — a beat into
 * the second chapter — was the marketing site with a sign-up form on it, which
 * on the page reads as a screenshot that failed to load rather than as a video.
 * This composes a card instead: the mark, the title, the running time, and a
 * real frame from the recording set into a window, so the thumbnail says what
 * the video is and what the product looks like in the same glance. It is
 * rendered by the same means as the title and end cards — a document handed to
 * a browser — so it matches them.
 *
 * `stitch` calls this once the mp4 exists; the CLI re-runs it alone.
 */
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { chromium } from "playwright";
import { posterCard } from "./cards";
import { LONG_FORM } from "./guides/registry";
import { VIEWPORT } from "./profile";

const run = promisify(execFile);

/** The chapter list as the stitch manifest writes it. */
type Chapter = { title: string; startSeconds: number };

export type PosterSpec = {
  /** The chapter whose frame goes on the card, and how far into it. */
  chapter?: string;
  offset?: number;
};

export function timecode(total: number): string {
  const s = Math.max(0, Math.round(total));
  const m = Math.floor(s / 60);
  return `${m}:${String(s % 60).padStart(2, "0")}`;
}

/** Where in the video the frame comes from: the named chapter plus an offset, else a beat into the second chapter. */
export function posterTime(chapters: Chapter[], spec: PosterSpec | undefined): number {
  const named = spec?.chapter ? chapters.find((c) => c.title === spec.chapter) : undefined;
  if (spec?.chapter && !named) {
    throw new Error(
      `The poster asks for a chapter called "${spec.chapter}", and the video has none.\n` +
        `Chapters: ${chapters.map((c) => c.title).join(" · ")}`,
    );
  }
  return (named ?? chapters[1] ?? chapters[0]).startSeconds + (spec?.offset ?? 1.5);
}

export async function renderPoster(opts: {
  video: string;
  outFile: string;
  title: string;
  durationSeconds: number;
  chapters: Chapter[];
  poster?: PosterSpec;
}): Promise<void> {
  const at = posterTime(opts.chapters, opts.poster);
  const work = await mkdtemp(path.join(tmpdir(), "poster-"));
  try {
    // The frame, as a data URL: the card is a standalone document with no
    // server behind it, so the image has to travel inside the HTML.
    const framePath = path.join(work, "frame.jpg");
    await run("ffmpeg", ["-y", "-loglevel", "error", "-ss", String(at), "-i", opts.video, "-frames:v", "1", "-q:v", "2", framePath]);
    const frame = `data:image/jpeg;base64,${(await readFile(framePath)).toString("base64")}`;

    const meta = `${timecode(opts.durationSeconds)} · ${opts.chapters.length} chapters · a real Slack workspace and the real console`;
    const html = posterCard({ title: opts.title, meta, frame });

    // Headless, no profile: the card needs no session, and the recording
    // profile may be busy filming.
    const browser = await chromium.launch({ channel: "chrome", headless: true });
    try {
      const page = await browser.newPage({ viewport: VIEWPORT, deviceScaleFactor: 2 });
      await page.setContent(html);
      await page.evaluate(() => document.fonts.ready);
      const raw = path.join(work, "poster@2x.png");
      await page.screenshot({ path: raw, type: "png" });
      // Down from retina to the viewport size — sharper text than rendering at
      // 1x, and a file the page can afford to load before the video.
      await run("ffmpeg", ["-y", "-loglevel", "error", "-i", raw, "-vf", `scale=${VIEWPORT.width}:-1`, "-q:v", "3", opts.outFile]);
    } finally {
      await browser.close();
    }
  } finally {
    await rm(work, { recursive: true, force: true });
  }
  console.log(`  poster: ${path.basename(opts.outFile)} from ${timecode(at)}`);
}

// CLI: re-make the poster for a stitched walkthrough from its manifest.
if (process.argv[1] && path.resolve(process.argv[1]) === path.resolve(__filename)) {
  (async () => {
    const id = process.argv[2];
    const guide = LONG_FORM.find((g) => g.id === id);
    if (!guide) {
      console.error(`Which walkthrough? One of: ${LONG_FORM.map((g) => g.id).join(", ")}`);
      process.exit(2);
    }
    const manifest = JSON.parse(await readFile(path.join(guide.outDir, `${guide.id}.json`), "utf8")) as {
      durationSeconds: number;
      chapters: Chapter[];
    };
    await renderPoster({
      video: path.join(guide.outDir, `${guide.id}.mp4`),
      outFile: path.join(guide.outDir, `${guide.id}.jpg`),
      title: guide.title,
      durationSeconds: manifest.durationSeconds,
      chapters: manifest.chapters,
      poster: guide.poster,
    });
  })().catch((err) => {
    console.error(err instanceof Error ? err.message : err);
    process.exit(1);
  });
}
