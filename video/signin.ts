/**
 * Signs the recording profile into Slack and the console, by handing you a
 * real browser window and waiting while you sign in yourself.
 *
 * Run:  npm run signin            (both surfaces)
 *       npm run signin -- slack
 *       npm run signin -- console
 *
 * Nothing about your password goes through this file, or through whatever
 * wrote it. It opens a window and waits; you type into it; the browser keeps
 * the session the way it keeps every other one. That is the whole program.
 *
 * **This is meant to be a one-off.** The profile is persistent (see
 * profile.ts), so a session stays signed in the way your own browser stays
 * signed in, rather than expiring a few days after a snapshot was taken. Run
 * this again only if you are actually signed out.
 *
 * `.auth/` holds live sessions. It is gitignored; treat it as a credential.
 */
import path from "node:path";
import { config } from "dotenv";
import type { BrowserContext, Page } from "playwright";
import { firstPage, launchProfile, PROFILE_DIR } from "./profile";

config({ path: path.join(process.cwd(), ".env") });

const CONSOLE_URL = process.env.VIDEO_CONSOLE_URL ?? "https://app.attesttag.com";

type Surface = {
  id: string;
  /** Where the window opens. */
  start: string;
  /**
   * True once the sign-in has actually landed. Polled, never clicked, and
   * always a POSITIVE test — see the note on the console one below.
   */
  done: (page: Page) => Promise<boolean>;
  /** Printed to the terminal while we wait. */
  hint: string;
};

const SURFACES: Surface[] = [
  {
    id: "slack",
    start: "https://app.slack.com/client",
    // The composer exists only inside a booted, signed-in client, which makes
    // it a better "are we in?" than any URL test — Slack's sign-in walks
    // through several hosts and lands on a URL shaped like the signed-out one.
    // The hook is the container's; there is no `message_input` (LEARNINGS.md),
    // and testing for one here reported a signed-in profile as signed out
    // until the ten-minute deadline — the same selector slack.ts drives.
    done: (page) =>
      page
        .locator('[data-qa="message_input_container"] [contenteditable="true"]')
        .first()
        .isVisible()
        .catch(() => false),
    hint:
      "Sign in to the DEMO workspace — not a workspace with real colleagues in\n" +
      "  it. Everything in that sidebar ends up in a public marketing video.",
  },
  {
    id: "console",
    start: `${CONSOLE_URL}/admin/`,
    // Read the BODY, not the status. This check has now been wrong twice, in
    // two different ways, and both cost something:
    //
    //   1. "the URL is not /login" — true of about:blank and of every frame
    //      before the first navigation resolves, so it passed instantly and
    //      saved a session nobody had signed into.
    //   2. "/api/me answered 200" — also wrong. That endpoint answers 200 when
    //      signed OUT: its job is to report which login methods the console
    //      accepts, which is a question you have to be able to ask before you
    //      have an account. A signed-out body is
    //      `{"signed_in":false,"user":null,…}` with a perfectly good status.
    //
    // The field that means what it says is `signed_in`.
    done: (page) =>
      page
        .evaluate(async () => {
          const res = await fetch("/api/me", {
            credentials: "include",
            cache: "no-store",
          });
          if (!res.ok) return false;
          const me = (await res.json()) as { signed_in?: boolean };
          return me.signed_in === true;
        })
        .catch(() => false),
    hint: "Sign in to the account the demo workspace is installed under.",
  },
];

async function signIn(context: BrowserContext, surface: Surface) {
  const page = await firstPage(context);
  await page.goto(surface.start);

  console.log(`\n=== ${surface.id} ===`);
  if (await surface.done(page).catch(() => false)) {
    console.log("  ✓ already signed in — nothing to do.");
    return;
  }
  console.log(`  ${surface.hint}`);
  console.log("  Waiting. Nothing is being recorded or typed for you.\n");

  const deadline = Date.now() + 10 * 60_000;
  for (;;) {
    if (await surface.done(page).catch(() => false)) break;
    if (Date.now() > deadline) {
      throw new Error(`Timed out waiting for the ${surface.id} sign-in.`);
    }
    await page.waitForTimeout(1_500);
  }
  // A beat after the app says it is in: Slack writes several storage keys
  // during boot, and closing on the first frame can lose the tail of them.
  await page.waitForTimeout(4_000);
  console.log(`  ✓ ${surface.id} is signed in and will stay that way.`);
}

async function main() {
  const wanted = process.argv.slice(2);
  const todo = wanted.length
    ? SURFACES.filter((s) => wanted.includes(s.id))
    : SURFACES;
  if (!todo.length) {
    throw new Error(
      `No surface matched ${wanted.join(", ")}. Known: ${SURFACES.map((s) => s.id).join(", ")}`,
    );
  }

  const context = await launchProfile({ headless: false });
  try {
    for (const surface of todo) await signIn(context, surface);
  } finally {
    // Closing the context is what flushes the profile to disk. Killing the
    // window instead leaves it half-written and the next run signed out.
    await context.close();
  }

  console.log(`\nSaved in ${path.relative(process.cwd(), PROFILE_DIR)}.`);
  console.log("Next:  npm run scout    — check Slack's DOM before scripting.");
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
