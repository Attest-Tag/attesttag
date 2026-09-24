/**
 * Re-cut a recorded walkthrough from its raw footage, with new narration.
 *
 * Run:  npm run video:recut -- deep-dive               every re-cut part, then stitch
 *       npm run video:recut -- deep-dive 5-hold        one part, then stitch
 *       npm run video:recut -- deep-dive stitch        just re-join what is on disk
 *
 * `render.ts` films a part once and lays the narration over it as it goes: a
 * scene holds for as long as its line takes to speak, and a wait on the bot
 * holds the shot in silence. Through the tunnel the bot is recorded behind,
 * that wait is anywhere from four seconds to seventy (LEARNINGS.md, "Slack's
 * first delivery fails"), so the take carries a minute of still frame with the
 * narration describing an answer that has not arrived. A re-cut fixes that
 * without a second take: it starts from the raw `.webm` Playwright wrote,
 * keeps the frames that show the product doing something, drops the ones that
 * show it waiting for Slack, and puts a new line under each moment.
 *
 * The unit is still a scene, but a scene here is anchored to a moment in the
 * footage rather than to whatever the recorder happened to be doing:
 *
 *   at      source second the scene begins on — the frame the line starts over
 *   until   source second its footage may run to; anything after it and before
 *           the next scene's `at` is dropped (a broken pane, a blank boot)
 *   say     the line; the scene holds for as long as it takes to speak, plus a
 *           beat. Footage shorter than that is held on its last frame; footage
 *           longer than that is `rest`: cut (default), kept, or fast-forwarded
 *   zoom    crop the scene to a region and scale it back up — for a beat the
 *           full frame would spoil (a typing shot beside a pane that failed)
 *   card    no footage at all: a title card saying what the next part is about,
 *           rendered the way the title and end cards are and held for a beat
 *
 * Every cut, hold, zoom and card happens inside one ffmpeg filter graph over
 * the raw footage: split, trim, pad, concat, then the lines laid on at the
 * offsets the pieces add up to, and the audio padded to the picture's length
 * so a stitched file has no gaps for a player to stumble over. Encoding
 * matches `render.ts` exactly, so a re-cut part stitches with parts that were
 * not re-cut.
 *
 * What it cannot do: show something the take did not film. A scene's `at` has
 * to be a frame that exists — pull frames before trusting a number
 * (`ffmpeg -ss <t> -i <webm> -frames:v 1 out.jpg`), and check the result the
 * same way. A re-cut that exits 0 has proved nothing about the picture.
 */
import { execFile } from "node:child_process";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";
import { config } from "dotenv";
import { chromium } from "playwright";
import { sectionCard } from "./cards";
import { assertVoiceAvailable, describeVoice, probeDuration, synthesise } from "./narrate";
import { stitch } from "./stitch";
import { VIEWPORT } from "./profile";

const run = promisify(execFile);

export type Zoom = { x: number; y: number; w: number; h: number };

export type RecutScene = {
  /** Source second the scene starts on. Not for a `card`. */
  at?: number;
  /** Source second its footage ends. Default: the next scene's `at`, or the end. */
  until?: number;
  /** The line. Omit for a silent beat of `hold` seconds. */
  say?: string;
  /** Silent beat only: seconds to keep (`until - at` for footage, 1.8 for a card). */
  hold?: number;
  /** Starts a chapter in the player's list at this scene. */
  chapter?: string;
  /**
   * Footage left inside [at, until) once the line and its tail have played:
   * dropped, kept at normal speed, or played at this many times speed.
   */
  rest?: "cut" | "keep" | number;
  /** Seconds kept at normal speed after the line ends. Default 0.75. */
  tail?: number;
  /** Crop this scene to a region of the frame and scale it back to full size. */
  zoom?: Zoom;
  /** A title card instead of footage: what the part is about. */
  card?: { title: string; blurb?: string };
};

export type RecutPart = {
  id: string;
  /** The raw recording, relative to video/. */
  source: string;
  scenes: RecutScene[];
};

export type RecutLongForm = {
  id: string;
  title: string;
  /** Parts in order. Ones without a re-cut are stitched from disk as they are. */
  parts: string[];
  recuts: RecutPart[];
  /** Where the part renders live and where the stitched result lands. */
  partsDir: string;
  outDir: string;
  /** The poster frame: a scene of a re-cut part, and seconds into it. */
  poster?: { part: string; scene: number; offset?: number };
};

const TAIL_S = 0.75;
const CARD_S = 1.8;

/** One stretch of the output: cut from the source, or a still. */
type Piece =
  | {
      kind: "footage";
      from: number;
      to: number;
      speed: number;
      /** Seconds the last frame is held for, after the footage. */
      pad: number;
      zoom?: Zoom;
    }
  | { kind: "still"; scene: number; seconds: number };

type Timeline = {
  pieces: Piece[];
  /** Output second each scene starts at. */
  starts: number[];
  total: number;
};

const outputLength = (p: Piece) =>
  p.kind === "still" ? p.seconds : (p.to - p.from) / p.speed + p.pad;

/**
 * Lay the scenes end to end. Each one contributes a normal-speed piece for its
 * line (padded on its last frame if the footage runs out first), then whatever
 * `rest` says about the footage left over. A card is a still of its own.
 */
export function timeline(scenes: RecutScene[], lineSeconds: number[], sourceEnd: number): Timeline {
  const pieces: Piece[] = [];
  const starts: number[] = [];
  let t = 0;
  scenes.forEach((scene, i) => {
    starts.push(t);
    if (scene.card) {
      const still: Piece = { kind: "still", scene: i, seconds: scene.hold ?? CARD_S };
      pieces.push(still);
      t += outputLength(still);
      return;
    }
    if (scene.at === undefined) throw new Error(`Scene ${i} has neither footage (at) nor a card.`);
    const next = scenes.slice(i + 1).find((s) => s.at !== undefined)?.at ?? sourceEnd;
    const until = Math.min(scene.until ?? next, sourceEnd);
    if (until <= scene.at) {
      throw new Error(`Scene ${i} of the part has no footage: at ${scene.at}, until ${until}.`);
    }
    const avail = until - scene.at;
    const need = scene.say
      ? lineSeconds[i] + (scene.tail ?? TAIL_S)
      : (scene.hold ?? avail);
    const shown = Math.min(avail, need);
    const main: Piece = { kind: "footage", from: scene.at, to: scene.at + shown, speed: 1, pad: Math.max(0, need - avail), zoom: scene.zoom };
    pieces.push(main);
    t += outputLength(main);
    const left = avail - shown;
    const rest = scene.rest ?? "cut";
    if (left > 0.05 && rest !== "cut") {
      const speed = rest === "keep" ? 1 : rest;
      if (!(speed >= 1)) throw new Error(`Scene ${i}: rest must be "cut", "keep" or a speed of at least 1.`);
      const extra: Piece = { kind: "footage", from: main.to, to: until, speed, pad: 0, zoom: scene.zoom };
      pieces.push(extra);
      t += outputLength(extra);
    }
  });
  return { pieces, starts, total: t };
}

/**
 * The filter graph: every footage piece trimmed from one decode of the
 * source, every card from its own still input, then all of them joined.
 * `stillInput` maps a still piece to the index of the ffmpeg input it is.
 */
function videoFilter(pieces: Piece[], stillInput: (p: Piece & { kind: "still" }) => number): string {
  const footage = pieces.filter((p) => p.kind === "footage");
  const split = `[0:v]fps=30,split=${footage.length}${footage.map((_, i) => `[s${i}]`).join("")}`;
  let f = 0;
  const parts = pieces.map((p, i) => {
    if (p.kind === "still") {
      return `[${stillInput(p)}:v]scale=${VIEWPORT.width}:${VIEWPORT.height}:flags=lanczos,format=yuv420p,setsar=1,trim=duration=${p.seconds.toFixed(3)},setpts=PTS-STARTPTS[p${i}]`;
    }
    // Order matters here. `setpts` marks the frame rate unknown downstream,
    // and `tpad` pads by frame count, so a pad placed after a `setpts` adds
    // nothing at all — quietly, with exit code 0 and a video stream sixteen
    // seconds shorter than its audio. The pad goes first; the timestamps are
    // rebased after it.
    const steps = [`trim=start=${p.from.toFixed(3)}:end=${p.to.toFixed(3)}`];
    if (p.zoom) {
      steps.push(`crop=${p.zoom.w}:${p.zoom.h}:${p.zoom.x}:${p.zoom.y}`);
      steps.push(`scale=${VIEWPORT.width}:${VIEWPORT.height}:flags=lanczos`);
    }
    if (p.pad > 0.01) steps.push(`tpad=stop_mode=clone:stop_duration=${p.pad.toFixed(3)}`);
    steps.push("setpts=PTS-STARTPTS");
    if (p.speed !== 1) steps.push(`setpts=PTS/${p.speed}`);
    return `[s${f++}]${steps.join(",")}[p${i}]`;
  });
  const join = `${pieces.map((_, i) => `[p${i}]`).join("")}concat=n=${pieces.length}:v=1:a=0[vout]`;
  return [split, ...parts, join].join(";");
}

async function encode(
  source: string,
  pieces: Piece[],
  stills: { scene: number; file: string; seconds: number }[],
  clips: { file: string; offsetS: number }[],
  totalS: number,
  outFile: string,
) {
  // Inputs: the source, then one looped still per card, then the lines.
  const stillArgs = stills.flatMap((s) => ["-loop", "1", "-framerate", "30", "-t", s.seconds.toFixed(3), "-i", s.file]);
  const clipArgs = clips.flatMap((c) => ["-i", c.file]);
  const stillInput = (p: Piece & { kind: "still" }) => 1 + stills.findIndex((s) => s.scene === p.scene);
  const clipBase = 1 + stills.length;
  const delays = clips
    .map((c, i) => `[${clipBase + i}:a]adelay=delays=${Math.round(c.offsetS * 1000)}:all=1[a${i}]`)
    .join(";");
  const mixIn = clips.map((_, i) => `[a${i}]`).join("");
  // `apad` runs the audio on to the picture's end (`-t` below cuts it there),
  // so every part's audio is as long as its video. The stitch is a stream
  // copy, and a player that meets a track ending early at a seam is a player
  // that may drift for the rest of the file.
  const audio = clips.length
    ? `;${delays};${mixIn}amix=inputs=${clips.length}:normalize=0:dropout_transition=0,apad[aout]`
    : "";
  const filter = `${videoFilter(pieces, stillInput)}${audio}`;
  const graph = path.join(path.dirname(outFile), `.${path.basename(outFile, ".mp4")}.filter`);
  await writeFile(graph, filter);
  await run(
    "ffmpeg",
    [
      "-y", "-loglevel", "error",
      "-i", source,
      ...stillArgs,
      ...clipArgs,
      "-filter_complex_script", graph,
      "-map", "[vout]",
      ...(clips.length ? ["-map", "[aout]"] : []),
      "-t", totalS.toFixed(3),
      // Identical to mux() in render.ts, so stitch's shape check passes
      // against parts that were not re-cut.
      "-c:v", "libx264", "-preset", "slow", "-crf", "22", "-pix_fmt", "yuv420p", "-r", "30",
      "-c:a", "aac", "-b:a", "128k", "-ac", "2",
      "-movflags", "+faststart",
      outFile,
    ],
    { maxBuffer: 64 * 1024 * 1024 },
  );
}

/**
 * The narration, synthesised once per distinct line. A re-cut is re-run far
 * more often than a take is — every timing nudge is a re-run — so the clips
 * are kept beside the text they were made from and reused while it matches.
 */
async function narration(part: RecutPart, work: string) {
  const clips: ({ file: string; seconds: number } | null)[] = [];
  for (const [i, scene] of part.scenes.entries()) {
    if (!scene.say) {
      clips.push(null);
      continue;
    }
    const name = `line-${String(i).padStart(2, "0")}`;
    const wav = path.join(work, `${name}.wav`);
    const txt = path.join(work, `${name}.txt`);
    const had = await readFile(txt, "utf8").catch(() => null);
    if (had === scene.say) {
      clips.push({ file: wav, seconds: await probeDuration(wav) });
      continue;
    }
    const clip = await synthesise(scene.say, work, name);
    await writeFile(txt, scene.say);
    clips.push(clip);
  }
  return clips;
}

/**
 * The cards, rendered the way the poster is: a document handed to a headless
 * browser, screenshotted at retina size and scaled down by the encode. Cached
 * beside their text like the lines.
 */
async function cards(part: RecutPart, work: string) {
  const out: { scene: number; file: string; seconds: number }[] = [];
  const wanted = part.scenes.map((s, i) => ({ s, i })).filter(({ s }) => s.card);
  if (!wanted.length) return out;
  let browser: import("playwright").Browser | null = null;
  try {
    for (const { s, i } of wanted) {
      const name = `card-${String(i).padStart(2, "0")}`;
      const png = path.join(work, `${name}.png`);
      const txt = path.join(work, `${name}.txt`);
      const key = JSON.stringify(s.card);
      const seconds = s.hold ?? CARD_S;
      if ((await readFile(txt, "utf8").catch(() => null)) === key) {
        out.push({ scene: i, file: png, seconds });
        continue;
      }
      browser ??= await chromium.launch({ channel: "chrome", headless: true });
      const page = await browser.newPage({ viewport: VIEWPORT, deviceScaleFactor: 2 });
      await page.setContent(sectionCard(s.card!.title, s.card!.blurb));
      await page.evaluate(() => document.fonts.ready);
      await page.screenshot({ path: png, type: "png" });
      await page.close();
      await writeFile(txt, key);
      out.push({ scene: i, file: png, seconds });
    }
  } finally {
    await browser?.close();
  }
  return out;
}

export async function recutPart(guide: RecutLongForm, part: RecutPart) {
  const source = path.resolve(part.source);
  const work = path.join(process.cwd(), ".video-work", "recut", part.id);
  await mkdir(work, { recursive: true });
  await mkdir(guide.partsDir, { recursive: true });

  console.log(`\n=== ${part.id} (re-cut of ${path.relative(process.cwd(), source)}) ===`);
  const sourceEnd = await probeDuration(source);

  console.log(`Narration: ${part.scenes.filter((s) => s.say).length} lines with ${describeVoice()}…`);
  const clips = await narration(part, work);
  const stills = await cards(part, work);
  const lineSeconds = clips.map((c) => c?.seconds ?? 0);

  const tl = timeline(part.scenes, lineSeconds, sourceEnd);
  for (const [i, scene] of part.scenes.entries()) {
    const label = scene.chapter ?? scene.card?.title ?? scene.say?.slice(0, 48) ?? "(silent)";
    const piece = tl.pieces.find((p) => (p.kind === "still" ? p.scene === i : p.from === scene.at));
    const note = piece?.kind === "footage" && piece.pad > 0.01 ? ` · held ${piece.pad.toFixed(1)}s` : "";
    const src = scene.card ? "  card " : `${scene.at!.toFixed(1).padStart(6)}s`;
    console.log(`  [${tl.starts[i].toFixed(1).padStart(5)}s] ← ${src}  ${label}${note}`);
  }

  const outFile = path.join(guide.partsDir, `${part.id}.mp4`);
  console.log(`Encoding ${tl.pieces.length} pieces → ${(tl.total).toFixed(1)}s…`);
  await encode(
    source,
    tl.pieces,
    stills,
    clips.flatMap((c, i) => (c ? [{ file: c.file, offsetS: tl.starts[i] }] : [])),
    tl.total,
    outFile,
  );
  const durationSeconds = await probeDuration(outFile);
  // A filter that quietly drops a pad or a piece leaves the exit code alone;
  // the length is the one thing that cannot lie about it.
  const { stdout: vlen } = await run("ffprobe", ["-v", "error", "-select_streams", "v", "-show_entries", "stream=duration", "-of", "csv=p=0", outFile]);
  if (Math.abs(Number(vlen) - tl.total) > 0.5) {
    throw new Error(`${part.id}: the picture runs ${Number(vlen).toFixed(1)}s but the pieces add up to ${tl.total.toFixed(1)}s — a piece or a pad was lost in the filter graph.`);
  }

  // The poster of a standalone part, kept for parity with render.ts.
  const posterAt = (tl.starts[1] ?? 0) + 1.5;
  await run("ffmpeg", ["-y", "-loglevel", "error", "-ss", String(posterAt), "-i", outFile, "-frames:v", "1", "-q:v", "3", path.join(guide.partsDir, `${part.id}.jpg`)]);

  const chapters = part.scenes
    .map((scene, i) => ({ scene, start: tl.starts[i] }))
    .filter((row) => row.scene.chapter)
    .map((row, i) => ({
      title: row.scene.chapter!,
      startSeconds: i === 0 ? 0 : Number(row.start.toFixed(2)),
    }));
  await writeFile(
    path.join(guide.partsDir, `${part.id}.json`),
    JSON.stringify(
      {
        id: part.id,
        durationSeconds: Number(durationSeconds.toFixed(2)),
        renderedAt: new Date().toISOString().slice(0, 10),
        recut: true,
        chapters,
        scenes: part.scenes.map((s, i) => ({
          at: s.at ?? null,
          card: s.card?.title ?? null,
          startSeconds: Number(tl.starts[i].toFixed(2)),
          say: s.say ?? null,
        })),
      },
      null,
      2,
    ) + "\n",
  );
  console.log(`${path.relative(process.cwd(), outFile)} — ${durationSeconds.toFixed(1)}s, ${chapters.length} chapters.`);
}

/** Resolve the poster spec to the chapter-and-offset form stitch wants. */
async function posterSpec(guide: RecutLongForm) {
  if (!guide.poster) return undefined;
  const { part, scene, offset = 1.5 } = guide.poster;
  const manifest = JSON.parse(await readFile(path.join(guide.partsDir, `${part}.json`), "utf8")) as {
    chapters: { title: string; startSeconds: number }[];
    scenes?: { startSeconds: number }[];
  };
  const at = manifest.scenes?.[scene]?.startSeconds;
  if (at === undefined) {
    // The part the poster comes from has not been re-cut yet; the stitch
    // still has to happen, so fall back to stitch's own default frame.
    console.warn(`      ! ${part} has no scene ${scene} yet (not re-cut?) — poster from the default frame`);
    return undefined;
  }
  const chapter = [...manifest.chapters].reverse().find((c) => c.startSeconds <= at) ?? manifest.chapters[0];
  return { chapter: chapter.title, offset: at - chapter.startSeconds + offset };
}

export async function recutLongForm(guide: RecutLongForm, wanted: string[]) {
  const stitchOnly = wanted.includes("stitch");
  const todo = stitchOnly
    ? []
    : wanted.length
      ? guide.recuts.filter((p) => wanted.includes(p.id))
      : guide.recuts;
  if (!stitchOnly && !todo.length) {
    throw new Error(
      `No re-cut of ${guide.id} matched ${wanted.join(", ")}.\n` +
        `Known: ${guide.recuts.map((p) => p.id).join(", ")}\nOr pass "stitch".`,
    );
  }
  if (todo.length) await assertVoiceAvailable();
  for (const part of todo) await recutPart(guide, part);

  console.log(`\n=== ${guide.id} ===`);
  await stitch({
    id: guide.id,
    title: guide.title,
    parts: guide.parts,
    dir: guide.partsDir,
    outDir: guide.outDir,
    poster: await posterSpec(guide),
    recordCommand: (partId) =>
      guide.recuts.some((p) => p.id === partId)
        ? `npm run video:recut -- ${guide.id} ${partId}`
        : `npm run video -- ${guide.id} ${partId}`,
  });
}

if (process.argv[1] && path.resolve(process.argv[1]) === path.resolve(__filename)) {
  config({ path: path.join(process.cwd(), "..", ".env.testing") });
  for (const [key, value] of Object.entries(process.env)) if (value === "") delete process.env[key];
  config({ path: path.join(process.cwd(), ".env") });
  (async () => {
    const [id, ...rest] = process.argv.slice(2);
    const { RECUTS } = await import("./recuts/registry");
    const guide = RECUTS.find((g) => g.id === id);
    if (!guide) {
      throw new Error(`Which walkthrough? One of: ${RECUTS.map((g) => g.id).join(", ") || "(none)"}`);
    }
    await recutLongForm(guide, rest);
  })().catch((err) => {
    console.error(err instanceof Error ? err.message : err);
    process.exit(1);
  });
}
