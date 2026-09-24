/**
 * Uploads a rendered walkthrough to the public bucket the site serves it from.
 *
 * Run:  npm run video:publish                 every render on disk
 *       npm run video:publish -- deep-dive
 *
 * The mp4s are gitignored — tens of megabytes apiece, re-made whenever a
 * narration line changes — so the deployed site image ships posters and
 * manifests and no video at all. This is the only thing that puts a render in
 * front of anyone who did not record it.
 *
 * Objects go up immutable with a year-long max-age and a re-record reuses the
 * filename, so src/lib/walkthroughs.server.ts busts the cache with
 * `?v=<renderedAt>` out of the manifest. That means a re-record has to be
 * published *and* its manifest committed, or every browser stays on the old
 * cut. The mtime check below exists to catch exactly that, because the failure
 * is silent: the upload succeeds and nothing anyone can see changes.
 *
 * Needs an authenticated gcloud.
 */
import { execFileSync } from "node:child_process";
import { readdirSync, statSync } from "node:fs";
import path from "node:path";
import { walkthroughsDir } from "./paths";

const BUCKET = process.env.WALKTHROUGH_BUCKET ?? "attesttag-walkthroughs";
const DIR = walkthroughsDir();

/** Matches what src/lib/walkthroughs.server.ts assumes about these objects. */
const CACHE_CONTROL = "public, max-age=31536000, immutable";

function main() {
  const wanted = process.argv.slice(2);
  const ids = readdirSync(DIR)
    .filter((f) => f.endsWith(".mp4"))
    .map((f) => path.basename(f, ".mp4"))
    .filter((id) => !wanted.length || wanted.includes(id))
    .sort();

  const missing = wanted.filter((id) => !ids.includes(id));
  if (missing.length) {
    console.error(`✗ No render in ${path.relative(process.cwd(), DIR)} for: ${missing.join(", ")}`);
    console.error("  Record it first:  npm run video:deep");
    process.exit(1);
  }
  if (!ids.length) {
    console.error(`✗ No .mp4 files in ${path.relative(process.cwd(), DIR)} — nothing to publish.`);
    process.exit(1);
  }

  const stale = ids.filter((id) => {
    try {
      return (
        statSync(path.join(DIR, `${id}.mp4`)).mtimeMs >
        statSync(path.join(DIR, `${id}.json`)).mtimeMs + 1000
      );
    } catch {
      return true; // no manifest at all — the page would skip it anyway
    }
  });
  if (stale.length) {
    console.error(`✗ Re-recorded without a fresh manifest: ${stale.join(", ")}`);
    console.error("  The page keys its cache-buster off the manifest's renderedAt,");
    console.error("  so publishing now would serve the OLD cut to everyone.");
    console.error("  Re-run the render so the manifest is rewritten too.");
    process.exit(1);
  }

  console.log(`▸ Publishing ${ids.length} walkthrough(s) to gs://${BUCKET}/…`);
  for (const id of ids) {
    const file = path.join(DIR, `${id}.mp4`);
    const mb = (statSync(file).size / 1024 / 1024).toFixed(1);
    execFileSync(
      "gcloud",
      [
        "storage",
        "cp",
        file,
        `gs://${BUCKET}/${id}.mp4`,
        `--cache-control=${CACHE_CONTROL}`,
      ],
      { stdio: ["ignore", "ignore", "inherit"] },
    );
    console.log(`  ✓ ${id}.mp4 (${mb} MB)`);
  }
  console.log(
    "\n  Posters and manifests are committed and served from the site image —\n" +
      "  only the mp4s live in the bucket. Commit any changed .json so the\n" +
      "  deployed site links to what you just uploaded.",
  );
}

main();
