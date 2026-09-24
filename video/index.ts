/**
 * Records the product walkthroughs by driving real Slack and the real console,
 * narrates them, and writes the mp4 + poster + chapter manifest the site's
 * /walkthroughs page reads.
 *
 * Run:  npm run video -- deep-dive                  every part, then stitch
 *       npm run video -- deep-dive 3-connect        re-record one part
 *       npm run video -- deep-dive stitch           just re-join what is on disk
 *       npm run video:deep                          the same, by its own script
 *
 * Every long-form walkthrough is named the same way — `npm run video -- <id>`
 * with the ids in guides/registry.ts — and each has a `video:<id>` script.
 *
 * Before the first run:
 *       npm run signin      hands you a browser; you sign in; it saves the session
 *       npm run scout       checks Slack's DOM before a word of script is trusted
 *
 * Needs ffmpeg on PATH (`brew install ffmpeg`) and OPENROUTER_API_KEY in .env
 * for the voice. See README.md.
 */
import path from "node:path";
import { config } from "dotenv";
import { LONG_FORM } from "./guides/registry";
import { render } from "./render";
import { stitch } from "./stitch";
import type { LongForm } from "./types";

// Both files, in the precedence the bot itself uses. The empty-value pass
// matters: a key declared with no value counts as "set" to dotenv, so it would
// shadow the real one. (And note the trap recorded in the repo's own memory —
// a stale OPENROUTER_API_KEY exported in ~/.zshrc beats both of these and
// fails as a 401 "User not found" that looks nothing like the cause.)
config({ path: path.join(process.cwd(), "..", ".env.testing") });
for (const [key, value] of Object.entries(process.env)) {
  if (value === "") delete process.env[key];
}
config({ path: path.join(process.cwd(), ".env") });

/** Every long-form walkthrough, by the id you name on the command line. */

/**
 * Render a long walkthrough's parts and join them.
 *
 * Cards belong to the whole video, not to each part: only the first opens on
 * the title card, only the last closes on the end card, and every part between
 * opens on the bare seam. Seven repeats of the title card would read as seven
 * videos rather than one.
 *
 * With no argument this records all of them, which is a long job spending real
 * model budget in a real workspace. Naming parts re-records only those and
 * then stitches whatever is on disk — which is the entire reason a walkthrough
 * this long is split up at all.
 */
async function renderLongForm(guide: LongForm, wanted: string[]) {
  const { parts, partsDir, outDir, id, title } = guide;
  const stitchOnly = wanted.includes("stitch");
  const todo = stitchOnly
    ? []
    : wanted.length
      ? parts.filter((p) => wanted.includes(p.id))
      : parts;

  if (!stitchOnly && !todo.length) {
    throw new Error(
      `No ${id} part matched ${wanted.join(", ")}.\n` +
        `Known: ${parts.map((p) => p.id).join(", ")}\n` +
        `Or pass "stitch" to join what is already rendered.`,
    );
  }

  for (const part of todo) {
    console.log(`\n=== ${part.id} ===`);
    const first = part.id === parts[0].id;
    // The title card lists the whole walkthrough's chapters, and only the
    // first part paints it — but that part knows its own chapters alone, so
    // the list on the LongForm is handed to it here. A part that carries its
    // own list keeps it.
    const opener = first && !part.cardChapters ? { ...part, cardChapters: guide.cardChapters } : part;
    await render(opener, {
      outDir: partsDir,
      titleCard: first,
      endCard: part.id === parts[parts.length - 1].id,
    });
  }

  console.log(`\n=== ${id} ===`);
  await stitch({
    title: guide.title,
    poster: guide.poster,
    id,
    parts: parts.map((p) => p.id),
    dir: partsDir,
    outDir,
    recordCommand: (partId) => `npm run video -- ${id} ${partId}`,
  });

  console.log(`\n"${title}" is ready.`);
  console.log(
    `Copy ${id}.json and ${id}.jpg from ${outDir} into the site's\n` +
      `public/walkthroughs/ and commit them there, then push the mp4:\n` +
      `  npm run video:publish -- ${id}`,
  );
}

async function main() {
  const argv = process.argv.slice(2);

  // `npm run video -- <id> <part…>` (or `npm run video:<id> -- <part…>`)
  // arrives as [id, …].
  const longForm = LONG_FORM.find((g) => g.id === argv[0]);
  if (longForm) {
    await renderLongForm(longForm, argv.slice(1));
    return;
  }

  throw new Error(
    `Nothing matched ${argv.join(" ") || "(no arguments)"}.\n` +
      `The walkthroughs are long-form and recorded in parts:\n` +
      LONG_FORM.map((g) => `  npm run video -- ${g.id}`).join("\n"),
  );
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
