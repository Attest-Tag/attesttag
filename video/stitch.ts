/**
 * Joins the parts of a long guide into one mp4, with one merged chapter list.
 *
 * Long walkthroughs are recorded in parts. A twenty-five-minute unbroken take
 * means every narration line is synthesised up front and one browser run has to
 * survive twenty-five minutes of the app — and LEARNINGS.md is largely a list of
 * renders lost to a single selector at minute nineteen. Parts make that failure
 * cost one four-minute take: re-record the part, re-run this, done.
 *
 * The join itself is `-c copy` over the concat demuxer, which is safe *only*
 * because every part came out of the same `mux()` in render.ts and therefore
 * carries identical codec parameters. If those ever diverge this has to become
 * a re-encode; `assertSameShape` below is what will tell you, rather than
 * leaving you to notice a garbled seam in the finished file.
 */
import { execFile } from "node:child_process";
import { mkdir, readFile, rm, stat, writeFile } from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";
import { probeDuration } from "./narrate";
import { renderPoster, type PosterSpec } from "./poster";

const run = promisify(execFile);

type Chapter = { title: string; startSeconds: number };
type Manifest = {
  id: string;
  durationSeconds: number;
  renderedAt: string;
  chapters: Chapter[];
};

/** The codec parameters a `-c copy` concat requires every part to share. */
type Shape = {
  width: number;
  height: number;
  codec: string;
  pixFmt: string;
  audioCodec: string;
  sampleRate: string;
  channels: number;
};

async function probeShape(file: string): Promise<Shape> {
  const { stdout } = await run("ffprobe", [
    "-v", "error",
    "-show_entries",
    "stream=codec_type,codec_name,width,height,pix_fmt,sample_rate,channels",
    "-of", "json",
    file,
  ]);
  const streams: Record<string, unknown>[] = JSON.parse(stdout).streams ?? [];
  const video = streams.find((s) => s.codec_type === "video");
  const audio = streams.find((s) => s.codec_type === "audio");
  if (!video) throw new Error(`${path.basename(file)} has no video stream.`);
  if (!audio) {
    // Silence is a real failure mode: a render whose narration never muxed
    // exits 0 and looks fine in a thumbnail.
    throw new Error(
      `${path.basename(file)} has no audio stream — its narration did not mux.`,
    );
  }
  return {
    width: Number(video.width),
    height: Number(video.height),
    codec: String(video.codec_name),
    pixFmt: String(video.pix_fmt),
    audioCodec: String(audio.codec_name),
    sampleRate: String(audio.sample_rate),
    channels: Number(audio.channels),
  };
}

function assertSameShape(parts: { id: string; shape: Shape }[]) {
  const [first, ...rest] = parts;
  for (const part of rest) {
    for (const key of Object.keys(first.shape) as (keyof Shape)[]) {
      if (part.shape[key] !== first.shape[key]) {
        throw new Error(
          `${part.id} does not match ${first.id} on ${key} ` +
            `(${part.shape[key]} vs ${first.shape[key]}). A stream-copy concat ` +
            `needs identical parameters — re-record the odd part, or change ` +
            `this to a re-encode.`,
        );
      }
    }
  }
}

/** `MM:SS`, or `H:MM:SS` past an hour — the form YouTube parses. */
export function timecode(seconds: number): string {
  const whole = Math.max(0, Math.floor(seconds));
  const h = Math.floor(whole / 3600);
  const m = Math.floor((whole % 3600) / 60);
  const s = whole % 60;
  const mm = h ? String(m).padStart(2, "0") : String(m);
  return `${h ? `${h}:` : ""}${mm}:${String(s).padStart(2, "0")}`;
}

/**
 * Lay each part's chapters onto the stitched timeline.
 *
 * A part's own first chapter is forced to 0 by render.ts, so that the opening
 * seconds of a standalone video are never in a dead zone the player can't seek
 * back into. Offsetting by the running total keeps that true here: the marker
 * lands on the seam card that opens the part, not a second into its first
 * scene.
 *
 * The offsets come from each part's *measured* duration, never from the sum of
 * its narration — the encode rounds, and by part seven a guessed total is
 * visibly behind the picture.
 */
export function mergeChapters(
  parts: { manifest: Manifest; measured: number }[],
): { chapters: Chapter[]; total: number } {
  const chapters: Chapter[] = [];
  let offset = 0;
  for (const { manifest, measured } of parts) {
    for (const chapter of manifest.chapters) {
      chapters.push({
        title: chapter.title,
        startSeconds: Number((offset + chapter.startSeconds).toFixed(2)),
      });
    }
    offset += measured;
  }
  return { chapters, total: offset };
}

export async function stitch(opts: {
  id: string;
  parts: string[];
  /** Where the part renders live. */
  dir: string;
  /**
   * Where the joined mp4, poster and manifest go. Defaults to `dir`.
   *
   * Split from `dir` so the parts can stay in a scratch directory while the
   * finished video lands in the published one (paths.ts) — `npm run
   * video:publish` globs every mp4 it finds there, and it has no business
   * uploading seven part files alongside the one anybody watches.
   */
  outDir?: string;
  /**
   * How to record a missing part, for the error below. It is a parameter
   * because a stitched walkthrough's parts are not themselves walkthroughs — so the
   * obvious `npm run video -- <part>` is exactly the thing that will not work,
   * and an error that suggests it sends you looking in the wrong place.
   */
  recordCommand?: (partId: string) => string;
  /** For the poster card: the video's title, and which frame to set in it. */
  title: string;
  poster?: PosterSpec;
}): Promise<void> {
  const { id, parts, dir } = opts;
  const outDir = opts.outDir ?? dir;
  const recordCommand =
    opts.recordCommand ?? ((partId: string) => `npm run video -- ${partId}`);
  await mkdir(dir, { recursive: true });
  await mkdir(outDir, { recursive: true });

  // Read everything before touching ffmpeg, so a missing part fails in a
  // sentence rather than half way through a concat.
  const loaded = [];
  for (const partId of parts) {
    const mp4 = path.join(dir, `${partId}.mp4`);
    const json = path.join(dir, `${partId}.json`);
    if (!(await stat(mp4).catch(() => null))) {
      throw new Error(
        `Missing ${path.relative(process.cwd(), mp4)}. Record it first:\n` +
          `  ${recordCommand(partId)}`,
      );
    }
    const manifest = JSON.parse(await readFile(json, "utf8")) as Manifest;
    loaded.push({
      id: partId,
      mp4,
      manifest,
      shape: await probeShape(mp4),
      measured: await probeDuration(mp4),
    });
  }

  assertSameShape(loaded);

  const { chapters, total } = mergeChapters(loaded);

  // The concat demuxer wants a file of paths, not a filter graph — it is the
  // only concat that can stream-copy.
  const listFile = path.join(dir, `.${id}.concat.txt`);
  await writeFile(
    listFile,
    loaded.map((p) => `file '${p.mp4.replace(/'/g, "'\\''")}'`).join("\n") + "\n",
  );

  const outFile = path.join(outDir, `${id}.mp4`);
  console.log(`Stitching ${loaded.length} parts → ${path.basename(outFile)}…`);
  await run(
    "ffmpeg",
    [
      "-y",
      "-f", "concat",
      "-safe", "0",
      "-i", listFile,
      "-c", "copy",
      // Each part starts its own timestamps at zero; without this the joins
      // carry them through and players seek to the wrong place.
      "-fflags", "+genpts",
      "-movflags", "+faststart",
      outFile,
    ],
    { maxBuffer: 64 * 1024 * 1024 },
  );
  await rm(listFile, { force: true });

  const durationSeconds = await probeDuration(outFile);

  // AAC priming adds a few milliseconds at every join, so an exact match is not
  // expected — but a whole second means a part was dropped or re-timed, and the
  // chapter offsets below would all be wrong.
  const drift = Math.abs(durationSeconds - total);
  if (drift > 1) {
    console.warn(
      `      ! stitched to ${durationSeconds.toFixed(1)}s but the parts measure ` +
        `${total.toFixed(1)}s (${drift.toFixed(1)}s apart) — chapter offsets ` +
        `past the first seam are suspect.`,
    );
  }

  // The poster is a composed card around a frame from the video, not the
  // frame alone — see poster.ts for why.
  await renderPoster({
    video: outFile,
    outFile: path.join(outDir, `${id}.jpg`),
    title: opts.title,
    durationSeconds,
    chapters,
    poster: opts.poster,
  });

  await writeFile(
    path.join(outDir, `${id}.json`),
    JSON.stringify(
      {
        id,
        durationSeconds: Number(durationSeconds.toFixed(2)),
        renderedAt: new Date().toISOString().slice(0, 10),
        // The site busts the mp4's year-long cache with this. The date alone
        // is not enough: a second cut on the same day would reuse the same
        // URL, and every browser that had seen the first would keep it.
        version: new Date().toISOString().replace(/\.\d+Z$/, "Z"),
        parts: loaded.map((p) => ({
          id: p.id,
          durationSeconds: Number(p.measured.toFixed(2)),
        })),
        chapters,
      },
      null,
      2,
    ) + "\n",
  );

  // Paste-able chapter markers. This guide is a standalone asset rather than a
  // library entry, so nothing in the app renders its chapter list — the only
  // place these are going is a video description box.
  await writeFile(
    path.join(outDir, `${id}.chapters.txt`),
    chapters
      .map((c) => `${timecode(c.startSeconds)} ${c.title}`)
      .join("\n") + "\n",
  );

  const mb = (await stat(outFile)).size / 1024 / 1024;
  console.log(
    `\n${path.relative(process.cwd(), outFile)} — ` +
      `${timecode(durationSeconds)}, ${chapters.length} chapters, ${mb.toFixed(1)} MB.`,
  );
  for (const c of chapters) console.log(`  ${timecode(c.startSeconds)}  ${c.title}`);
}
