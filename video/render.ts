import { execFile } from "node:child_process";
import { mkdir, readdir, readFile, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";
import { walkthroughsDir } from "./paths";
import { firstPage, launchProfile } from "./profile";
import {
  assertVoiceAvailable,
  describeVoice,
  probeDuration,
  synthesise,
} from "./narrate";
import { endCard, interstitialCard, titleCard } from "./cards";
import { installCursor, Stage } from "./stage";
import { keepOnlyChannels, SlackStage } from "./slack";
import type { Guide, Surfaces } from "./types";

const run = promisify(execFile);

const ROOT = process.cwd();
const WORK_ROOT = path.join(ROOT, ".video-work");
const LOCK = path.join(WORK_ROOT, "render.lock");
/**
 * Where a published walkthrough lands (paths.ts). A render that is not meant
 * for the site passes its own directory (see RenderOptions.outDir) and stays
 * invisible to the page and to `npm run video:publish`.
 */

/**
 * Read WHEN CALLED, never at module scope.
 *
 * index.ts loads the env files after its imports have evaluated, so a
 * module-level `const CONSOLE_URL = process.env.VIDEO_CONSOLE_URL ?? "…"`
 * resolves to the FALLBACK on every run — and the fallback here is
 * production. That is not a stale value, it is the recorder quietly driving
 * the live product while `npm run preflight` (which loads dotenv before it
 * reads anything) reports the local host. It was caught by somebody watching
 * the browser window, which is not a control.
 *
 * narrate.ts carries this warning and guides/deep-dive.ts was fixed for it.
 * This was the third place, and the most expensive one.
 */
const consoleUrl = () =>
  process.env.VIDEO_CONSOLE_URL ?? "https://app.attesttag.com";
/**
 * The console is mounted under /admin/, not at the root — the Go binary serves
 * the Slack endpoints and the API from the root and the exported console under
 * that prefix. So the app Stage's base carries the prefix, and a scene writes
 * `goto("/bundles")` rather than `goto("/admin/bundles")`.
 *
 * Worth one line of explanation because the failure is quiet: `/bundles` is a
 * 404 page that renders fine, so the recording carries on and the scene times
 * out twenty seconds later on a selector that was always correct.
 */
const consoleBase = () => consoleUrl().replace(/\/+$/, "") + "/admin";

// 16:10. Wide enough that Slack keeps its sidebar, the message pane and an
// open thread pane side by side — below about 1400 Slack collapses the thread
// into an overlay, and the shot stops showing the thing that makes the product
// make sense (a conversation with an answer growing inside it).
const VIEWPORT = { width: 1440, height: 900 };

const LEAD_IN_MS = 2600;
const CONTINUATION_LEAD_IN_MS = 1200;
const OUTRO_MS = 2800;
const TAIL_MS = 750;

/**
 * ## One browser, two products
 *
 * Recording a walkthrough of this product means crossing from a Slack thread
 * into the console and back mid-take, and Playwright records a *page* rather
 * than a screen — so both have to be signed in inside ONE context or every
 * crossing is a cut.
 *
 * That used to mean merging two storage-state snapshots and trusting their
 * origins not to collide. It is now nothing at all: the profile in profile.ts
 * is one browser that happens to be logged into two sites, which is what every
 * browser already is. See profile.ts for why a profile beats a snapshot.
 */

/**
 * Lay the narration clips onto one audio track at their measured offsets and
 * mux with the screen recording. `adelay` per clip + `amix` is exact to the
 * millisecond — concatenating generated silence would accumulate rounding
 * error over the forty-odd scenes in a long part.
 */
async function mux(
  videoFile: string,
  clips: { file: string; offsetMs: number }[],
  outFile: string,
  cuts: { from: number; to: number }[] = [],
) {
  const inputs = clips.flatMap((c) => ["-i", c.file]);
  // Off-camera intervals are dropped here, on the source's own clock, before
  // anything is laid over it: the clip offsets given to this function are
  // already measured on the shortened timeline. `fps` first, because
  // Playwright's capture is variable-rate and `select` on a regular grid is
  // the only way the seam lands on a whole frame; `setpts` then closes the
  // gap the dropped frames leave.
  const dropped = cuts
    .map((c) => `between(t,${(c.from / 1000).toFixed(3)},${(c.to / 1000).toFixed(3)})`)
    .join("+");
  const video = cuts.length
    ? `[0:v]fps=30,select='not(${dropped})',setpts=N/(30*TB)[vout];`
    : "";
  const delays = clips
    .map(
      (c, i) =>
        `[${i + 1}:a]adelay=delays=${Math.round(c.offsetMs)}:all=1[a${i}]`,
    )
    .join(";");
  const mixIn = clips.map((_, i) => `[a${i}]`).join("");
  const filter = `${video}${delays};${mixIn}amix=inputs=${clips.length}:normalize=0:dropout_transition=0[aout]`;

  await run("ffmpeg", [
    "-y",
    "-i", videoFile,
    ...inputs,
    "-filter_complex", filter,
    "-map", cuts.length ? "[vout]" : "0:v",
    "-map", "[aout]",
    "-c:v", "libx264",
    "-preset", "slow",
    "-crf", "22",
    "-pix_fmt", "yuv420p",
    "-r", "30",
    "-c:a", "aac",
    "-b:a", "128k",
    "-ac", "2",
    "-movflags", "+faststart",
    outFile,
  ], { maxBuffer: 64 * 1024 * 1024 });
}

/**
 * Check the cheap preconditions before the expensive ones. Synthesis runs
 * first and costs a couple of minutes of paid API calls; discovering only
 * afterwards that the console is down throws all of it away.
 */
/**
 * Refuse to drive production unless somebody said so out loud.
 *
 * The walkthrough is not a read-only tour. It founds an organisation, installs
 * a Slack workspace, creates a bundle, stores a credential, confirms a write
 * and opens a pull request — all of it for real, against whatever host it is
 * pointed at. Pointed at production, that is a stray tenant in the live
 * database and a demo workspace taken off whoever owned it.
 *
 * It has already happened once, from a module-scope `?? "https://app.…"`
 * fallback that resolved before dotenv ran. The env var was set correctly and
 * every check that loaded dotenv first agreed it was set correctly; the
 * recorder drove production anyway, and it was caught by somebody watching the
 * browser window rather than by anything here. Hence a guard rather than
 * another careful comment.
 *
 * VIDEO_ALLOW_PRODUCTION=1 opts in, for the day that is genuinely wanted.
 */
function assertNotProductionByAccident() {
  const url = consoleUrl();
  const isProd = /(^|\/\/)app\.attesttag\.com/.test(url);
  if (!isProd || process.env.VIDEO_ALLOW_PRODUCTION === "1") return;
  throw new Error(
    `Refusing to record against ${url}.\n\n` +
      `A render is not a tour: it founds an organisation, installs a Slack\n` +
      `workspace, stores a credential and opens a pull request — for real.\n` +
      `Against production that is a stray tenant in the live database.\n\n` +
      `Set VIDEO_CONSOLE_URL in video/.env to the local instance, or pass\n` +
      `VIDEO_ALLOW_PRODUCTION=1 if you genuinely mean production.`,
  );
}

async function assertReachable() {
  for (const url of ["https://app.slack.com", consoleUrl()]) {
    const res = await fetch(url, { redirect: "manual" }).catch(() => null);
    if (!res) throw new Error(`Nothing is answering at ${url}.`);
  }
}

/**
 * One render at a time. Takes share a Slack workspace and a console org, so
 * two at once post into the same channel and fight over the same thread.
 */
async function acquireLock() {
  await mkdir(WORK_ROOT, { recursive: true });
  const held = await readFile(LOCK, "utf8").catch(() => null);
  if (held) {
    const pid = Number(held.trim());
    let alive = false;
    try {
      process.kill(pid, 0); // signal 0 tests for existence only
      alive = true;
    } catch {
      alive = false;
    }
    if (alive) {
      throw new Error(
        `Another render is already running (pid ${pid}). Renders share the ` +
          `demo workspace, so they cannot overlap. Wait for it, or kill it.`,
      );
    }
    console.warn(`  (clearing a stale lock from pid ${pid})`);
  }
  await writeFile(LOCK, String(process.pid));
}

export type RenderOptions = {
  /** Where the mp4, poster and manifest land. Defaults to the site's folder. */
  outDir?: string;
  /**
   * Open on the full title card (the default) or, for a part that continues
   * another, on the bare seam. False also shortens the lead-in.
   */
  titleCard?: boolean;
  /** Close on the end card. False for any part that is not the last one. */
  endCard?: boolean;
};

export async function render(guide: Guide, options: RenderOptions = {}) {
  const outDir = options.outDir ?? walkthroughsDir();
  const wantTitleCard = options.titleCard ?? true;
  const wantEndCard = options.endCard ?? true;

  assertNotProductionByAccident();
  if (guide.before) await guide.before();
  await assertVoiceAvailable();
  await assertReachable();
  await acquireLock();

  // Per guide, so a stray concurrent run can only corrupt its own files.
  const WORK = path.join(WORK_ROOT, guide.id);
  await rm(WORK, { recursive: true, force: true });
  await mkdir(WORK, { recursive: true });
  await mkdir(outDir, { recursive: true });

  // 1. Voice-over first: a scene holds for exactly as long as its line takes
  //    to speak, so every duration has to be known before a frame is drawn.
  console.log(`Synthesising ${guide.scenes.length} lines with ${describeVoice()}…`);
  const clips = [];
  for (const [i, scene] of guide.scenes.entries()) {
    clips.push(await synthesise(scene.say, WORK, `line-${String(i).padStart(2, "0")}`));
  }
  const spokenTotal = clips.reduce((s, c) => s + c.seconds, 0);
  console.log(`  ${spokenTotal.toFixed(1)}s of narration.`);

  // 2. Drive the real thing, recording as we go.
  //
  // Headed by default: Slack is the reason, since a headless client is a
  // client Slack may treat differently, and there is nobody to notice if it
  // does. VIDEO_HEADLESS=1 overrides when nobody is at the machine — check the
  // frames before trusting a headless take.
  const context = await launchProfile({
    headless: process.env.VIDEO_HEADLESS === "1",
    recordVideoDir: WORK,
    deviceScaleFactor: 2, // retina capture, downscaled on encode
  });
  // On the context, not the page, so the cursor re-injects on every navigation
  // — a take crosses from Slack to the console and back several times.
  await installCursor(context);
  // Hide the recording account's other channels, its DMs and its app list.
  // This was written for the recorder and then never wired into it — so every
  // take so far filmed the whole sidebar: other teams' channels, who the
  // account is in a DM with, what it has not read. None of that is a fact
  // about the product and all of it is in a public video.
  // VIDEO_NO_SIDEBAR_FILTER=1 turns this off. It is a debugging affordance
  // with a reason: this script injects into Slack's own document, so when the
  // client misbehaves it is the first thing to rule out — and it has been the
  // culprit once already.
  if (process.env.VIDEO_NO_SIDEBAR_FILTER !== "1") {
    await keepOnlyChannels(context, [process.env.VIDEO_CHANNEL ?? "attesttag-demo"]);
  }
  const page = await firstPage(context);

  const app = new Stage(page, consoleBase());

  const t0 = Date.now();
  const elapsed = () => Date.now() - t0;
  const holdUntil = async (ms: number) => {
    while (elapsed() < ms) await app.wait(Math.min(200, ms - elapsed()));
  };

  // Intervals to drop on encode, on the recording's clock. See Surfaces.
  const cuts: { from: number; to: number }[] = [];
  // The current scene: when it began, when its line will have finished (on
  // the recording's clock, so a cut that opens the scene pushes it out), and
  // how much of it has been cut so far.
  let sceneStart = 0;
  let narrationEnd = 0;
  let cutInScene = 0;

  const surfaces: Surfaces = {
    page,
    slack: new SlackStage(page),
    app,
    wait: (ms) => app.wait(ms),
    offCamera: async (fn) => {
      // A cut may not fall under the line — except at the very start of the
      // scene, before any of it has played: then the line simply begins on
      // the first frame after the cut, and its end moves out by as much.
      const opensScene = elapsed() - sceneStart < 300 && cutInScene === 0;
      if (!opensScene) await holdUntil(narrationEnd);
      const from = opensScene ? sceneStart : elapsed();
      await fn();
      const to = elapsed();
      cuts.push({ from, to });
      cutInScene += to - from;
      if (from < narrationEnd) narrationEnd += to - from;
      console.log(`      ✂ ${((to - from) / 1000).toFixed(1)}s off camera${opensScene ? " (opens the scene)" : ""}`);
    },
  };

  const offsets: number[] = [];
  const ctx: Record<string, string> = {};

  const chapterNames =
    guide.cardChapters ??
    guide.scenes.map((s) => s.chapter).filter((c): c is string => Boolean(c));

  try {
    // Frame 1 is a card either way. `setContent` paints inline HTML with no
    // navigation, so nothing blank ever gets in front of it.
    if (wantTitleCard) {
      await page.setContent(titleCard(guide.title, chapterNames));
      await holdUntil(LEAD_IN_MS);
    } else {
      await page.setContent(interstitialCard());
      await holdUntil(CONTINUATION_LEAD_IN_MS);
    }
    for (const [i, scene] of guide.scenes.entries()) {
      const start = elapsed();
      offsets.push(start);
      sceneStart = start;
      narrationEnd = start + clips[i].seconds * 1000;
      cutInScene = 0;
      console.log(
        `  [${(start / 1000).toFixed(1)}s] ${scene.chapter ?? scene.say.slice(0, 48)}`,
      );
      if (scene.act) await scene.act(surfaces, ctx);
      // A scene that outruns its line holds the shot in silence. A second or
      // two is normal — a model answering is the one place here where it can
      // legitimately be twenty, which is why the narration over those scenes
      // is written long. A minute is a bug.
      const spoken = clips[i].seconds * 1000;
      const overrun = elapsed() - start - cutInScene - spoken;
      if (overrun > 20_000) {
        console.warn(
          `      ! held ${(overrun / 1000).toFixed(0)}s of silence after this line`,
        );
      }
      await holdUntil(narrationEnd + TAIL_MS);
    }
    if (wantEndCard) {
      await page.setContent(endCard(guide.title));
      await surfaces.app.wait(OUTRO_MS);
    }
  } finally {
    await context.close(); // flushes the video file and the profile
  }

  // 3. Mux. Playwright names the file after an internal id, so find it.
  const webm = (await readdir(WORK)).find((f) => f.endsWith(".webm"));
  if (!webm) throw new Error("Playwright produced no video file.");
  const videoFile = path.join(WORK, webm);

  // A scene's offset moves up by every cut that closed before it started. A
  // cut lives inside its scene's act, after that scene's line, so a scene's
  // own cut never moves its own offset.
  const cutBefore = (t: number) =>
    cuts.filter((c) => c.to <= t).reduce((sum, c) => sum + (c.to - c.from), 0);
  const cutOffsets = offsets.map((t) => t - cutBefore(t));

  const outFile = path.join(outDir, `${guide.id}.mp4`);
  console.log("Encoding…");
  await mux(
    videoFile,
    clips.map((c, i) => ({ file: c.file, offsetMs: cutOffsets[i] })),
    outFile,
    cuts,
  );

  const durationSeconds = await probeDuration(outFile);

  // 4. Poster, taken a beat into the second chapter so it shows the product
  //    rather than the title shot.
  const posterAt = (cutOffsets[1] ?? cutOffsets[0]) / 1000 + 1.5;
  await run("ffmpeg", [
    "-y", "-ss", String(posterAt), "-i", outFile,
    "-frames:v", "1", "-q:v", "3",
    path.join(outDir, `${guide.id}.jpg`),
  ]);

  // 5. Manifest — the offsets the player's chapter list links to.
  const chapters = guide.scenes
    .map((scene, i) => ({ scene, start: cutOffsets[i] }))
    .filter((row) => row.scene.chapter)
    .map((row, i) => ({
      title: row.scene.chapter!,
      // The first chapter always starts at zero, whatever scene carries the
      // marker — otherwise the opening belongs to no chapter and the player
      // has a dead zone it cannot seek back into.
      startSeconds: i === 0 ? 0 : Number((row.start / 1000).toFixed(2)),
    }));

  await writeFile(
    path.join(outDir, `${guide.id}.json`),
    JSON.stringify(
      {
        id: guide.id,
        durationSeconds: Number(durationSeconds.toFixed(2)),
        renderedAt: new Date().toISOString().slice(0, 10),
        chapters,
      },
      null,
      2,
    ) + "\n",
  );

  await rm(LOCK, { force: true });

  console.log(
    `\n${path.relative(ROOT, outFile)} — ${durationSeconds.toFixed(1)}s, ${chapters.length} chapters.`,
  );
}
