/**
 * Everything a render needs, checked in one go.
 *
 * Run:  npm run preflight
 *
 * A render costs a few minutes of paid narration before it draws a frame, and
 * the failures worth catching are all of the boring kind: a tool missing from
 * PATH, a session that expired, a key shadowed by a stale shell export. Each
 * of those has cost a take at least once in the rig this was ported from.
 *
 * Prints no secret, and no part of one.
 */
import { execFileSync } from "node:child_process";
import { existsSync } from "node:fs";
import path from "node:path";
import { config } from "dotenv";
import { firstPage, launchProfile, PROFILE_DIR } from "./profile";

config({ path: path.join(process.cwd(), "..", ".env.testing") });
for (const [k, v] of Object.entries(process.env)) if (v === "") delete process.env[k];
config({ path: path.join(process.cwd(), ".env") });

const rows: { ok: boolean; what: string; detail: string; blocks: string }[] = [];
const add = (ok: boolean, what: string, detail: string, blocks = "") =>
  rows.push({ ok, what, detail, blocks });

function haveTool(cmd: string): boolean {
  try {
    execFileSync("command", ["-v", cmd], { shell: "/bin/sh", stdio: "ignore" });
    return true;
  } catch {
    return false;
  }
}

async function main() {
  // --- tools ---
  add(haveTool("ffmpeg"), "ffmpeg", "encodes the video", "everything");
  add(haveTool("ffprobe"), "ffprobe", "measures narration", "everything");
  for (const t of ["git", "uv", "qwen"]) {
    add(
      haveTool(t),
      t,
      "the fix worker runs as a subprocess (WORKER_MODE=local)",
      "6-fix",
    );
  }

  // --- the voice ---
  const key = process.env.OPENROUTER_API_KEY ?? "";
  add(Boolean(key), "OPENROUTER_API_KEY", "Gemini TTS for the narration", "everything");
  if (key) {
    const res = await fetch("https://openrouter.ai/api/v1/auth/key", {
      headers: { Authorization: `Bearer ${key}` },
    }).catch(() => null);
    add(
      Boolean(res?.ok),
      "OpenRouter accepts the key",
      res?.ok
        ? "authenticated"
        : `${res?.status ?? "no answer"} — a stale OPENROUTER_API_KEY exported in ` +
          `~/.zshrc beats both env files and fails exactly like this`,
      "everything",
    );
  }

  // --- credentials the script types on camera ---
  add(
    Boolean(process.env.VIDEO_GITHUB_TOKEN),
    "VIDEO_GITHUB_TOKEN",
    "run `npm run check` for what it can reach",
    "3-connect, 4-reach, 6-fix",
  );
  add(
    Boolean(process.env.VIDEO_CLICKUP_TOKEN),
    "VIDEO_CLICKUP_TOKEN",
    "run `npm run check` for what it can reach",
    "3-connect, 5-hold",
  );
  add(
    Boolean(process.env.VIDEO_SIGNUP_EMAIL && process.env.VIDEO_SIGNUP_PASSWORD),
    "VIDEO_SIGNUP_EMAIL / _PASSWORD",
    "0-signup founds an account on camera; use a throwaway",
    "0-signup",
  );

  // --- hosts ---
  for (const [name, url] of [
    ["Slack", "https://app.slack.com"],
    ["console", process.env.VIDEO_CONSOLE_URL ?? ""],
  ] as const) {
    if (!url) {
      add(false, `${name} URL`, "VIDEO_CONSOLE_URL is not set", "everything");
      continue;
    }
    const res = await fetch(url, { redirect: "manual" }).catch(() => null);
    add(Boolean(res), `${name} reachable`, res ? `${res.status}` : url, "everything");
  }

  // --- the console routes the guide navigates to ---
  //
  // This exists because of how quietly the wrong prefix fails. The console is
  // mounted under /admin/, and a request for /bundles returns a perfectly good
  // 404 PAGE with status 200-ish behaviour — so a render navigates there, sees
  // a document, carries on, and dies twenty seconds later on a selector that
  // was correct the whole time. Checking the routes costs one request each.
  const consoleBase = (process.env.VIDEO_CONSOLE_URL ?? "").replace(/\/+$/, "") + "/admin";
  if (process.env.VIDEO_CONSOLE_URL) {
    const routes = ["/signup", "/onboarding", "/bundles", "/activity", "/settings", "/memory"];
    const bad: string[] = [];
    for (const route of routes) {
      const res = await fetch(consoleBase + route, { redirect: "follow" }).catch(() => null);
      if (!res || res.status >= 400) bad.push(`${route} (${res?.status ?? "no answer"})`);
    }
    add(
      bad.length === 0,
      "console routes",
      bad.length ? `not served: ${bad.join(", ")}` : `all ${routes.length} resolve under /admin`,
      "3-connect, 7-memory, 8-record",
    );
  }

  // --- the isolation guard that decides whether 0-signup can run ---
  //
  // SaveTeam upserts `where teams.org_id = excluded.org_id` and returns
  // ErrTeamOwnedElsewhere otherwise, so a workspace already bound to an
  // organisation cannot be installed into a new one. 0-signup founds a new
  // organisation and installs this workspace into it — which means a bound
  // workspace fails that part, on camera, at its most important beat.
  const dbPath = path.join(process.cwd(), "..", "testing.db");
  if (existsSync(dbPath)) {
    let bound = "";
    try {
      bound = execFileSync(
        "sqlite3",
        [dbPath, "select team_id || ' → org ' || org_id from teams;"],
        { encoding: "utf8" },
      ).trim();
    } catch {
      bound = "";
    }
    add(
      !bound,
      "workspace unbound",
      bound
        ? `${bound} — 0-signup would fail with ErrTeamOwnedElsewhere. ` +
          `Start clean:  FRESH_DB=1 ./start_dev.sh`
        : "no team rows; 0-signup can install freshly",
      "0-signup",
    );
  }

  // --- the profile ---
  add(existsSync(PROFILE_DIR), "recording profile", PROFILE_DIR, "everything");
  if (existsSync(PROFILE_DIR)) {
    // Headed. A headless open of this profile has been seen NOT to carry the
    // Slack session, which would silently record a signed-out client.
    const context = await launchProfile({ headless: false });
    try {
      const page = await firstPage(context);

      await page
        .goto("https://app.slack.com/client", { waitUntil: "domcontentloaded" })
        .catch(() => {});
      const slackIn = await page
        .locator('[data-qa="message_input_container"] [contenteditable="true"]')
        .first()
        // Slack first, and generously. Checking it AFTER a console navigation
        // reported it signed out while it was signed in — a false negative
        // that sends someone to re-authenticate something that was fine, which
        // is a worse failure than no check at all.
        .waitFor({ state: "visible", timeout: 120_000 })
        .then(() => true)
        .catch(() => false);
      add(slackIn, "Slack signed in", slackIn ? "yes" : "npm run signin -- slack", "everything");

      if (slackIn) {
        const channel = process.env.VIDEO_CHANNEL ?? "attest-demo";
        // waitFor, not isVisible. Slack opens straight into the last channel
        // and paints the sidebar after the composer, so an instant check runs
        // before the rows exist and reports a channel missing that is on
        // screen a second later.
        const there = await page
          .locator(`[data-qa="channel_sidebar_name_${channel}"]`)
          .first()
          .waitFor({ state: "visible", timeout: 20_000 })
          .then(() => true)
          .catch(() => false);
        if (there) {
          add(true, `#${channel} in the sidebar`, "yes", "everything");
        } else {
          const seen = await page
            .locator('[data-qa^="channel_sidebar_name_"]')
            .evaluateAll((els) =>
              els.map((e) =>
                e.getAttribute("data-qa")?.replace("channel_sidebar_name_", ""),
              ),
            )
            .catch(() => []);
          add(
            false,
            `#${channel} in the sidebar`,
            `not there. in it: ${seen.filter(Boolean).join(", ") || "(none)"} — npm run setup`,
            "everything",
          );
        }
      }

      await page
        .goto(`${process.env.VIDEO_CONSOLE_URL}/admin/`, {
          waitUntil: "domcontentloaded",
        })
        .catch(() => {});
      const signedIn = await page
        .evaluate(async () => {
          const r = await fetch("/api/me", { credentials: "include", cache: "no-store" });
          if (!r.ok) return false;
          return ((await r.json()) as { signed_in?: boolean }).signed_in === true;
        })
        .catch(() => false);
      // Worth knowing before anyone runs `signin`: 0-signup founds an account
      // on camera, and signing up starts a session there and then — so a run
      // that begins at 0-signup signs the console in as part of the recording,
      // and this row satisfies itself. It only has to be done by hand when
      // recording a later part on its own.
      add(
        signedIn,
        "console signed in",
        signedIn
          ? "yes"
          : "npm run signin -- console — OR just set the signup credentials " +
            "above, since 0-signup signs in by founding the account",
        "3-connect, 7-memory, 8-record (unless 0-signup runs first)",
      );

    } finally {
      await context.close();
    }
  }

  // --- report ---
  const pad = Math.max(...rows.map((r) => r.what.length));
  console.log("");
  for (const r of rows) {
    console.log(`  ${r.ok ? "✓" : "✗"} ${r.what.padEnd(pad)}  ${r.detail}`);
  }
  const blocked = rows.filter((r) => !r.ok);
  if (!blocked.length) {
    console.log("\n  Ready. Next:  npm run scout -- --ask   then   npm run video:deep\n");
    return;
  }
  console.log(`\n  ${blocked.length} thing(s) missing. They block:`);
  const parts = new Set(blocked.flatMap((r) => r.blocks.split(", ").filter(Boolean)));
  for (const p of parts) console.log(`    ${p}`);
  console.log(
    "\n  Parts not listed can still be recorded:  npm run video:deep -- <part…>\n",
  );
  process.exitCode = 1;
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
