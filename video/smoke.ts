/**
 * Renders a thirty-second video against pages that need no sign-in, to prove
 * the machinery works before a real take spends anything on it.
 *
 * Run:  npm run smoke
 *
 * It exercises every part of the pipeline the walkthrough depends on and the
 * walkthrough's own content depends on none of: synthesising narration and
 * measuring it, holding each scene for exactly its line, drawing the cursor,
 * gliding and clicking at hand speed, recording one continuous page, laying
 * the clips onto the timeline at measured offsets, encoding, cutting a poster,
 * and writing a chapter manifest.
 *
 * The point is what it costs to find a broken encoder. A real part is several
 * minutes of paid speech synthesis before it draws a frame, and it finds out
 * about ffmpeg at the very end — so a pipeline fault costs the whole take.
 * This costs three lines of narration.
 *
 * Output goes to video-out/, which is gitignored scratch and which the site
 * never reads.
 */
import path from "node:path";
import { config } from "dotenv";
import { render } from "./render";
import type { Guide } from "./types";

config({ path: path.join(process.cwd(), "..", ".env.testing") });
for (const [k, v] of Object.entries(process.env)) if (v === "") delete process.env[k];
config({ path: path.join(process.cwd(), ".env") });

/** The marketing site, which is public and needs nothing signed in. */
const SITE = process.env.VIDEO_SMOKE_URL ?? "http://localhost:3100";

const smoke: Guide = {
  id: "smoke",
  title: "Pipeline smoke test",
  cardChapters: ["It speaks", "It moves", "It encodes"],
  scenes: [
    {
      chapter: "It speaks",
      say: `This is a smoke test, not a walkthrough. If you can hear this sentence, the narration was synthesised, measured, and laid onto the timeline at the offset this scene actually started at.`,
      act: async (s) => {
        await s.app.goto(SITE);
      },
    },
    {
      chapter: "It moves",
      say: `And if the pointer glided across the page and paused before it pressed, rather than teleporting, the cursor was injected and the stage is driving at hand speed.`,
      act: async (s) => {
        await s.app.scroll(420);
        await s.app.clickIfPresent(
          s.page.getByRole("link", { name: /walkthroughs/i }).first(),
        );
        await s.wait(600);
      },
    },
    {
      chapter: "It encodes",
      say: `What is left is ffmpeg, a poster frame, and a chapter manifest with three entries in it. If this file plays with sound, every part of the pipeline the real recording needs is working.`,
      act: async (s) => {
        await s.app.scroll(300);
        await s.wait(800);
      },
    },
  ],
};

render(smoke, { outDir: path.join(process.cwd(), "video-out") }).catch((err) => {
  console.error(err);
  process.exit(1);
});
