import path from "node:path";

/**
 * Where a finished walkthrough lands: the mp4, its poster and its manifest.
 *
 * Renders used to be written straight into `website/public/walkthroughs/`,
 * because the site was a folder in this repository. It is its own repository
 * now, so the default is a directory here and the two committed files are
 * copied across by hand — `npm run video` prints the copy at the end of a
 * render.
 *
 * Set `WALKTHROUGHS_DIR` to that checkout's `public/walkthroughs` and the old
 * one-step flow is back for whoever has both trees on disk. It is read from the
 * shell, not from the dotenv files: the guide objects are built while their
 * module is imported, which happens before `index.ts` loads any `.env`.
 */
export function walkthroughsDir(): string {
  return process.env.WALKTHROUGHS_DIR || path.join(process.cwd(), "out");
}
