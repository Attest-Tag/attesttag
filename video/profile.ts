import path from "node:path";
import { chromium, type BrowserContext } from "playwright";

/**
 * One persistent Chrome profile, signed into Slack and the console, reused by
 * every script here.
 *
 * ## Why a profile and not a storage state
 *
 * The first version of this captured `context.storageState()` — a snapshot of
 * cookies and local storage — and replayed it into a fresh context per run.
 * That works, and it expires: a snapshot is a photograph of a session at one
 * instant, and Slack rotates what it hands you as you use it. Replaying an old
 * photograph means signing in again every few days, usually discovered in the
 * middle of a render.
 *
 * A profile is the session itself, on disk, where the browser maintains it.
 * Sign in once and it keeps working the way your own browser does — because it
 * is the same mechanism.
 *
 * It also deletes a problem rather than solving it. Recording a walkthrough of
 * this product means crossing between Slack and the console mid-take, and
 * Playwright records a page rather than a screen, so both have to be signed in
 * inside ONE context. With snapshots that meant merging two of them and hoping
 * the origins did not collide. With a profile there is nothing to merge: it is
 * one browser that happens to be logged into two sites, which is what every
 * browser on earth already is.
 *
 * ## What is in here
 *
 * A live session for whatever you signed into — treat `.auth/` as a
 * credential. It is gitignored. Deleting the directory signs everything out
 * and costs one `npm run signin`.
 */
export const PROFILE_DIR = path.join(process.cwd(), ".auth", "profile");

export const VIEWPORT = { width: 1440, height: 900 };

export type LaunchOptions = {
  headless?: boolean;
  /** Directory to write the screen recording into. Omit to record nothing. */
  recordVideoDir?: string;
  /** Retina capture, downscaled on encode. */
  deviceScaleFactor?: number;
};

/**
 * Open the profile.
 *
 * `channel: "chrome"` — the installed browser, not Playwright's bundled
 * Chromium. Slack serves a degraded client to browsers it does not recognise,
 * and a profile written by one engine and opened by another is a sign-in that
 * silently is not one.
 *
 * Only one process may hold a profile at a time: Chrome takes a lock on the
 * directory. That is a feature here, since two renders at once would post into
 * the same channel anyway — but it is why `npm run signin` has to be closed
 * before a render starts, and the error when it is not says nothing useful, so
 * it is caught and re-thrown below.
 */
export async function launchProfile(
  opts: LaunchOptions = {},
): Promise<BrowserContext> {
  try {
    const context = await chromium.launchPersistentContext(PROFILE_DIR, {
      channel: "chrome",
      headless: opts.headless ?? false,
      viewport: VIEWPORT,
      deviceScaleFactor: opts.deviceScaleFactor,
      // Force light. Slack's theme can be set to follow the operating system,
      // and on a Mac in dark mode that makes every Slack frame dark while the
      // console — which has its own light theme — stays light. Two halves of
      // one video that do not look like the same product.
      //
      // Emulating the colour scheme fixes it for the recording without
      // touching anybody's actual preferences, which is the right place for a
      // decision that is about the camera and not about the product.
      colorScheme: "light",
      args: ["--hide-scrollbars"],
      ...(opts.recordVideoDir
        ? { recordVideo: { dir: opts.recordVideoDir, size: VIEWPORT } }
        : {}),
    });
    // Slack shows a "needs your permission to enable notifications" bar across
    // the foot of every frame until the permission is decided. Granting it up
    // front means the bar never exists, which beats hiding it.
    await context.grantPermissions(["notifications"], { origin: "https://app.slack.com" });
    return context;
  } catch (err) {
    throw new Error(
      `Could not open the recording profile at ${PROFILE_DIR}.\n` +
        `If something else already has it open — a sign-in window, another ` +
        `render — close that first; Chrome allows one process per profile.\n\n` +
        String(err),
    );
  }
}

/** The page to drive: the one the profile opened with, or a new one. */
export async function firstPage(context: BrowserContext) {
  return context.pages()[0] ?? (await context.newPage());
}
