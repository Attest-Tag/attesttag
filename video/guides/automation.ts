import type { Locator } from "playwright";
import type { Guide, LongForm, Surfaces } from "../types";
import { SEL } from "../slack";
import {
  assertDb,
  assertDeepDiveState,
  bot,
  channel,
  command,
  dbCount,
  dbValue,
  jobCount,
  ORG,
  orgPlan,
  outDir,
  partsDirFor,
  sleep,
  watchAnswer,
} from "./shared";

// "Routines and fix jobs" — the walkthrough of the two things that run without
// a person typing: a schedule, and a worker.
//
// Three parts, recorded in the organisation the deep dive founded:
//
//   1-routine   Slack: a schedule asked for in plain words (create_routine),
//               then !routines
//   2-console   the console: the routine's row, its edit dialog, Run now, the
//               run record; then Slack, where the run posted; then !routine off
//   3-jobs      Slack: !jobs, a second fix job confirmed and cancelled at once;
//               the console's Jobs page; the closing line on /routines
//
// Environment: nothing beyond what shared.ts reads (VIDEO_CHANNEL,
// VIDEO_BOT_NAME). FREE_PLAN_BUDGET_USD is read if the instance's .env.testing
// sets it, for the same check the fix worker makes before it offers a card.
//
// What each part leaves behind, and what a re-take shows. 1-routine creates a
// routine; 2-console pauses it (`!routine off`), so `!routines`, which lists
// only enabled ones, is clean on the next take — but the console's /routines
// page lists paused routines too, so a re-take has last take's routine in the
// list, paused. Every console scene here finds ITS routine by id (the newest
// row), so the duplicate is on camera and never acted on. 3-jobs dispatches a
// real fix job and cancels it within seconds; the cancelled job stays on
// /jobs beside the deep dive's finished one, and a re-take adds another. Note
// for the deep dive: shared.ts's waitForJob() reads the NEWEST job, so after
// this walkthrough a re-record of the deep dive's 7-memory alone would see a
// cancelled job and stop — re-record 6-fix with it.
//
// The waits that are not the product's are cut (`offCamera`): Slack booting
// off a title card, a routine run (a model turn with tools, tens of seconds),
// and the worker acknowledging a cancel (its next event, or the reconciler at
// 90 s). The two bot round trips that ARE the product — the routine being
// created, the fix-job card — are on camera, written to ~55 words each.
//
// Nothing here asserts on what the model said. The routine's post is found by
// the header the PRODUCT writes on it (":alarm_clock: *Routine #N* — …",
// routines.go), never by the answer under it.

const partsDir = partsDirFor("automation");
const TITLE = "Routines and fix jobs";

const ROUTINE_ASK = () =>
  `@${bot()} every weekday at 9am, post the questions in this channel that nobody has answered yet`;
const FIX_ASK = () =>
  `@${bot()} fix the flaky retry-budget test so it stops depending on wall-clock time, and raise a PR`;

/** The newest routine's id as digits, or "" when there is none. */
function newestRoutineId(): string {
  const id = dbValue(`select id from routines where org_id=${ORG()} order by id desc limit 1;`);
  return /^\d+$/.test(id) ? id : "";
}

/** The newest fix job's id as digits, or "" when there is none. */
function newestJobId(): string {
  const id = dbValue(`select id from jobs where org_id=${ORG()} order by id desc limit 1;`);
  return /^\d+$/.test(id) ? id : "";
}

/**
 * Block until the routine has finished one more run than it had, and that run
 * posted. "Run now" is POST /api/routines/{id}/run, which answers 202 and runs
 * the routine in the background; the routine_runs row is written after the
 * post has been settled into the channel (runRoutineNow → AddRoutineRun), so
 * a row is proof the message exists. A run that ended quiet, failed or
 * skipped posted nothing, and the Slack scene after this would film the
 * previous take's message — so that is a stop, with the run's own reason.
 */
async function waitForRoutineRun(id: string, runsBefore: number, timeoutMs = 8 * 60_000): Promise<void> {
  const started = Date.now();
  while (dbCount(`select count(*) from routine_runs where routine_id=${id};`) <= runsBefore) {
    if (Date.now() - started > timeoutMs) {
      throw new Error(
        `Routine #${id} did not finish a run in ${Math.round(timeoutMs / 60_000)} min. A run is a full ` +
          `model turn with tools (up to routine_minutes, 30 by default); look at bot.log for it.`,
      );
    }
    await sleep(3_000);
  }
  const status = dbValue(`select status from routine_runs where routine_id=${id} order by id desc limit 1;`);
  if (status !== "posted") {
    const why = dbValue(
      `select coalesce(nullif(error,''), reason) from routine_runs where routine_id=${id} order by id desc limit 1;`,
    );
    throw new Error(
      `The run of routine #${id} ended "${status}"${why ? `: ${why}` : ""} — nothing was posted in the ` +
        `channel, so the next scene has nothing to show.`,
    );
  }
}

/**
 * Wait for a job to reach a terminal status, and return whatever it reached.
 * Not a check — the scene that calls this asserts the status itself — only
 * the wait for the chip on /jobs to read "cancelled" rather than
 * "cancelling": a running worker learns of the cancel at its next event, and
 * the reconciler kills it at 90 s if it never answers (jobs.go cancel()).
 */
async function waitForJobToStop(id: string, timeoutMs: number): Promise<string> {
  const started = Date.now();
  for (;;) {
    const status = dbValue(`select coalesce(status,'') from jobs where id=${id};`);
    if (/^(succeeded|failed|cancelled|timeout)$/.test(status) || Date.now() - started > timeoutMs) return status;
    await sleep(3_000);
  }
}

/**
 * The fix worker refuses a job when this month's spend plus the per-job budget
 * would pass the account's monthly budget (budgetRoom in jobs.go), and the
 * refusal arrives as the model's reply instead of the card — a take that ends
 * on a selector timeout for a Confirm button that was never posted. The free
 * plan's budget is fixed (FREE_PLAN_BUDGET_USD, 5 unless set); a pro account's
 * is the operator's own decision, so only the free case is checked here.
 */
function assertJobBudgetRoom(): void {
  if (orgPlan() !== "free") return;
  const free = Number(process.env.FREE_PLAN_BUDGET_USD) || 5;
  const own = Number(dbValue(`select value from settings where org_id=${ORG()} and key='monthly_budget_usd';`)) || 0;
  const budget = own > 0 && own < free ? own : free;
  const spent =
    Number(
      dbValue(
        `select coalesce(sum(cost_usd),0) from usage where org_id=${ORG()} and created_at >= strftime('%Y-%m-01 00:00:00','now');`,
      ),
    ) || 0;
  const job = Number(dbValue(`select value from settings where org_id=${ORG()} and key='worker_job_budget_usd';`)) || 3;
  if (spent + job > budget) {
    throw new Error(
      `The account has $${(budget - spent).toFixed(2)} of its $${budget.toFixed(2)} monthly budget left, less ` +
        `than the $${job.toFixed(2)} a fix job is given, so the fix-job card would be refused. Lower the ` +
        `per-job budget under Settings → Workers, or move the organisation to Pro.`,
    );
  }
}

/**
 * The routine's row on /routines, found by its id. The page lists every
 * routine the organisation has, paused ones included, so after a re-take
 * there are two with the same prompt; the id is the one thing that differs.
 * `\b` so #1 cannot match #12.
 */
const routineRow = (s: Surfaces, id: string): Locator =>
  s.page.getByRole("row").filter({ hasText: new RegExp(`#${id}\\b`) }).first();

/** Open the row's "Routine actions" menu and pick one item (routines-page.tsx). */
async function routineAction(s: Surfaces, id: string, item: RegExp): Promise<void> {
  await s.app.click(routineRow(s, id).getByRole("button", { name: /^routine actions$/i }), { settle: 700 });
  await s.app.click(s.page.getByRole("menuitem", { name: item }), { settle: 900 });
}

// --- 1. the routine ------------------------------------------------------------

const routine: Guide = {
  id: "1-routine",
  title: TITLE,
  before: async () => {
    assertDeepDiveState();
  },
  scenes: [
    {
      chapter: "A routine in plain words",
      say: `Some questions come up every morning. Instead of asking each time, ask once for a schedule. Nothing to configure first: the channel, the bot, and a sentence.`,
      act: async (s, ctx) => {
        // Slack boots blank coming off the title card; open it inside the cut
        // that opens the scene, so the line starts on a channel. The ask goes
        // out under this line; the next scene opens on its reply. The routine
        // count is taken BEFORE the send: a fast answer could land during the
        // next line, and a count read then would already include it.
        await s.offCamera(async () => {
          await s.slack.openChannel(channel());
          await s.slack.closeThread();
        });
        ctx.routinesBefore = String(dbCount(`select count(*) from routines where org_id=${ORG()};`));
        await s.wait(600);
        await s.slack.send(ROUTINE_ASK());
      },
    },
    {
      // Bot-wait scene, written to the round trip. This is a model turn:
      // create_routine returns one line ("routine #N created; next run …")
      // that the model relays, and the answer ends with the footer like every
      // channel reply — footer() in agent.go is empty only for DMs and for the
      // routine's own runs — so the footer is the done signal here.
      say: `Every weekday at nine, post the questions nobody has answered. It turns the sentence into a schedule: a cron line, the timezone, and the prompt to run each time, and it replies with the routine's number and when the first run is due. A routine posts where it was created. This channel, without anyone asking again.`,
      act: async (s, ctx) => {
        // The question is already in the channel (previous scene). Slack's
        // delivery is cut; the answer streams in on camera, ending on the
        // footer.
        await watchAnswer(s, ROUTINE_ASK());
        assertDb(`select count(*) from routines where org_id=${ORG()};`, Number(ctx.routinesBefore) + 1, "The routine was not created");
        // Part 2 runs it and films the post, and part 3 closes on the "Writes
        // run without asking" label. The tool defaults to both (notify
        // "always", auto_confirm true); a model that chose otherwise made a
        // routine the rest of this walkthrough cannot show.
        const id = newestRoutineId();
        assertDb(
          `select count(*) from routines where id=${id} and notify='always' and auto_confirm=1;`,
          1,
          `Routine #${id} was created as post-only-when-needed or with held writes, which parts 2 and 3 cannot show — re-record 1-routine`,
        );
      },
    },
    {
      say: `Routines lists what is scheduled here: the id, the cron line, the timezone, and the next run. A routine belongs to the channel it was created in.`,
      act: async (s) => {
        await command(s, "!routines");
      },
    },
  ],
};

// --- 2. the console, and what it posted ----------------------------------------

const console_: Guide = {
  id: "2-console",
  title: TITLE,
  before: async () => {
    const id = newestRoutineId();
    if (!id) throw new Error("No routine exists — run `npm run video:automation -- 1-routine` first.");
    if (!dbCount(`select count(*) from routines where id=${id} and enabled=1 and notify='always';`)) {
      throw new Error(
        `Routine #${id} is paused or posts only when needed; this part runs it and films the post. ` +
          `Run 1-routine again so the newest routine is a live one.`,
      );
    }
  },
  scenes: [
    {
      chapter: "The routine in the console",
      say: `The same routine in the console: the channel it posts to, the schedule, whether it replies every run or only when it matters, and the next run. The switch pauses it.`,
      act: async (s) => {
        const id = newestRoutineId();
        await s.app.goto("/routines");
        await s.wait(600);
        // aria-label "Routine N enabled" (routines-page.tsx); pointed at, not pressed.
        await s.app.point(routineRow(s, id).getByRole("switch"));
        await s.wait(600);
      },
    },
    {
      say: `Edit opens everything the sentence became. The prompt, the cron line, the timezone, which model answers, and when it may post. Writes run without asking, because nobody is there to press Confirm at nine in the morning; an admin can set Ask first. Close it without saving.`,
      act: async (s) => {
        const id = newestRoutineId();
        await routineAction(s, id, /^edit$/i);
        const dialog = s.page.getByRole("dialog");
        // Field ids from routine-edit-dialog.tsx.
        for (const sel of ["#routine-prompt", "#routine-cron", "#routine-tz", "#routine-model"]) {
          await s.app.point(dialog.locator(sel));
          await s.wait(350);
        }
        await s.app.point(dialog.getByText(/^Reply to channel$/));
        await s.wait(350);
        await s.app.point(dialog.getByText(/^Writes$/));
        await s.wait(700);
        await s.app.click(dialog.getByRole("button", { name: /^cancel$/i }), { settle: 800 });
      },
    },
    {
      chapter: "Run it now",
      say: `Run it now rather than wait for the morning. A run is a full turn with the channel's tools: it reads the channel, works out what is still open, and posts. It takes as long as a question takes.`,
      act: async (s) => {
        const id = newestRoutineId();
        const runsBefore = dbCount(`select count(*) from routine_runs where routine_id=${id};`);
        await routineAction(s, id, /^run now$/i);
        await s.wait(1_500); // the toast: "Running now — the result lands in the channel"
        // The run is the model's time, not the product's shape; it goes off
        // camera once the line has been spoken. The page is reloaded inside
        // the cut so the row's "Last run" shows the run that just finished.
        await s.offCamera(async () => {
          await waitForRoutineRun(id, runsBefore);
          await s.app.goto("/routines");
          await s.wait(600);
        });
      },
    },
    {
      say: `Every run is recorded: when it ran, whether it posted, how long it took, what it cost, and the answer. A run that stays quiet is recorded the same way, with what it found.`,
      act: async (s) => {
        const id = newestRoutineId();
        await routineAction(s, id, /^runs$/i);
        await s.wait(1_200);
        // "Answer" is the run row's disclosure (routine-runs-dialog.tsx); the
        // run is proven posted already, so a missing disclosure is not a stop.
        await s.app.clickIfPresent(s.page.getByRole("dialog").getByRole("button", { name: /^answer$/i }), {
          settle: 900,
        });
        await s.wait(800);
      },
    },
    {
      chapter: "What it posted",
      say: `In Slack, the run is a message of its own in the channel: the routine's number, the instruction, and the answer under it. Not a reply to anyone. People reply to it like any other message.`,
      act: async (s) => {
        const id = newestRoutineId();
        // Coming from the console, Slack boots blank and then restores the
        // last thread pane; neither belongs on camera, so the cut opens the
        // scene. The post is found by the header the product writes on every
        // run (":alarm_clock: *Routine #N* — prompt", routines.go), which is
        // ours, not the model's. waitForBotReply was the wrong tool for this
        // seam: the run is already on screen when Slack opens, so the snapshot
        // it takes would BE the post, and it would wait for it to change.
        await s.offCamera(async () => {
          await s.slack.openChannel(channel());
          await s.slack.closeThread();
          const post = s.page.locator(SEL.message).filter({ hasText: new RegExp(`Routine #${id}\\b`) }).last();
          await post.waitFor({ state: "visible", timeout: 60_000 }).catch(() => {
            throw new Error(
              `The run of routine #${id} is recorded as posted, but no message reading "Routine #${id}" is on ` +
                `screen in #${channel()}. Check the channel; if it is there, Slack's list did not paint it.`,
            );
          });
          await post.scrollIntoViewIfNeeded();
        });
        await s.wait(1_200);
      },
    },
    {
      chapter: "Switch it off",
      say: `Switch it off from Slack: routine off, and its number. It stays in the console, paused, with its runs.`,
      act: async (s) => {
        const id = newestRoutineId();
        await command(s, `!routine off ${id}`);
        assertDb(`select count(*) from routines where id=${id} and enabled=0;`, 1, `Routine #${id} was not disabled`);
      },
    },
  ],
};

// --- 3. fix jobs ---------------------------------------------------------------

const jobs: Guide = {
  id: "3-jobs",
  title: TITLE,
  before: async () => {
    assertDeepDiveState();
    if (!newestRoutineId()) throw new Error("No routine exists — run `npm run video:automation -- 1-routine` first.");
    // precheck() in jobs.go refuses a new job while worker_max_jobs (2) are in
    // flight, and a cancel that the worker has not acknowledged still counts.
    const active = dbCount(
      `select count(*) from jobs where org_id=${ORG()} and status in ('queued','starting','running','stale','cancelling');`,
    );
    if (active) {
      throw new Error(
        `${active} fix job(s) are still in flight; the card for a new one would be refused. Wait for them, or ` +
          `cancel them on /jobs, then record this part.`,
      );
    }
    assertJobBudgetRoom();
  },
  scenes: [
    {
      chapter: "Fix jobs",
      say: `Fix jobs are the other kind of automation. Jobs lists what has run in this channel: the earlier job, its repository, its status, what it cost, and the pull request it opened.`,
      act: async (s) => {
        await s.offCamera(async () => {
          await s.slack.openChannel(channel());
          await s.slack.closeThread();
        });
        await command(s, "!jobs");
      },
    },
    {
      // Bot-wait scene. The reply is the held card, not an answer, and the
      // fix-job card carries no reply footer; stable is done.
      say: `Ask for a second change: make the retry-budget test stop depending on wall-clock time, and raise a PR. It writes the brief and stops at the card: the repository, the base branch, the change, what done looks like, and the rule the worker runs under. Nothing has started. Whether it does is a person's decision.`,
      act: async (s) => {
        await s.slack.ask(FIX_ASK(), { requireFooter: false, settleMs: 5_000 });
      },
    },
    {
      say: `Confirm. The job goes to a worker in its own container, and a checklist in the thread follows each stage.`,
      act: async (s) => {
        // Prove the click reached the bot, the way 6-fix in the deep dive
        // does: a button press is a Slack interaction event, and one take
        // lost it to a slow acknowledgement. A job row appears, or press once
        // more, or stop.
        const before = jobCount();
        await s.slack.clickCardButton(/confirm|approve|start/i);
        for (let attempt = 0; ; attempt++) {
          const deadline = Date.now() + 20_000;
          while (Date.now() < deadline && jobCount() <= before) await s.wait(1_000);
          if (jobCount() > before) break;
          if (attempt >= 1) throw new Error("Confirm was pressed twice and no fix job was dispatched.");
          await s.slack.clickCardButton(/confirm|approve|start/i);
        }
        await s.wait(2_000);
      },
    },
    {
      say: `A person started it, and a person can stop it. Job cancel, and its number. The worker stops at its next step, and nothing more is pushed.`,
      act: async (s) => {
        const id = newestJobId();
        // "Cancelling fix job #N." (commands.go). A job that had already
        // ended answers "is not running" instead, and the row below says so.
        await command(s, `!job cancel ${id}`);
        assertDb(`select count(*) from jobs where id=${id} and status like 'cancel%';`, 1, `Fix job #${id} was not cancelled`);
      },
    },
    {
      chapter: "The record",
      say: `On the Jobs page the cancelled job sits beside the finished one. Open it: the brief it was given, the phases it reached, who confirmed it, what it cost before it stopped, and every event with a time on it.`,
      act: async (s) => {
        const id = newestJobId();
        await s.offCamera(async () => {
          // The chip reads "cancelling" until the worker answers; that wait is
          // the worker's, not the product's shape, and is cut.
          await waitForJobToStop(id, 150_000);
          await s.app.goto("/jobs");
        });
        await s.wait(600);
        await s.app.click(s.page.getByRole("row").filter({ hasText: new RegExp(`#${id}\\b`) }).first(), {
          settle: 1_600,
        });
        await s.wait(1_000);
        await s.app.scroll(260);
        assertDb(`select count(*) from jobs where id=${id} and status like 'cancel%';`, 1, `Fix job #${id} is not cancelled`);
      },
    },
    {
      chapter: "Unattended, recorded",
      say: `Routines run with nobody watching, so a routine's writes run without a Confirm card, unless an admin turns that off. Every run is recorded, and so is every job. Nothing runs unseen.`,
      act: async (s) => {
        const id = newestRoutineId();
        await s.app.goto("/routines");
        await s.wait(600);
        // The label under the channel cell, shown while auto_confirm is on
        // (routines-page.tsx) — which 1-routine asserted for this routine.
        await s.app.point(routineRow(s, id).getByText(/writes run without asking/i));
        await s.wait(800);
      },
    },
  ],
};

export const AUTOMATION: LongForm = {
  id: "automation",
  title: TITLE,
  cardChapters: [
    "A routine in plain words",
    "The routine in the console",
    "Run it now",
    "What it posted",
    "Switch it off",
    "Fix jobs",
    "The record",
  ],
  partsDir,
  outDir,
  parts: [routine, console_, jobs],
  // The channel with the run's own message in it — the routine's number, the
  // instruction, the answer — which is the product doing the thing.
  poster: { chapter: "What it posted", offset: 3 },
};
