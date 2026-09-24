import type { Locator } from "playwright";
import type { Guide, LongForm, Surfaces } from "../types";
import {
  assertDb,
  assertWorkspaceState,
  channel,
  command,
  dbCount,
  dbValue,
  escapeRe,
  ORG,
  orgPlan,
  outDir,
  partsDirFor,
} from "./shared";

// "Cost, models and budget" — where the money goes, which model spends it, and
// where it stops.
//
// Three parts, recorded in the organisation the deep dive founded:
//
//   1-overview  the console's overview: the tiles, one followed to its rows,
//               spend by channel
//   2-models    Settings → Models: the default model's searchable picker, the
//               advanced model, the monthly budget and the plan;
//               Settings → Workers: the per-job budget
//   3-slack     !usage, then !model advanced in a thread, closing on the footer
//
// Environment (video/.env, gitignored; read when called, never at module scope):
//
//   VIDEO_ADVANCED_MODEL   optional. The model id 2-models sets as the advanced
//                          model, e.g. z-ai/glm-5.3. Unset, the picker's
//                          current value is picked again — the instance's
//                          HEAVY_MODEL, or the product's own default — and
//                          nothing is saved. Set it to a model the picker
//                          lists: the take stops if the endpoint does not
//                          serve it, rather than store an id "as typed" that
//                          every advanced turn would then fail on.
//
// Reads over writes. The one write is the advanced model, and only when
// VIDEO_ADVANCED_MODEL names a different one from what is in force; the
// default model is opened, searched, and left exactly as it was. The two Slack
// commands write a session field on their own thread and nothing else. A
// re-take leaves the same two command threads again in the channel.
//
// The budget lines are written for the free plan, which is what sign-up
// founds: a fixed budget, a read-only field, the support email that moves an
// account to Pro. Part 1's `before` stops on a pro organisation rather than
// narrate a field that is open as one that is not.
//
// Nothing here asserts on what the model said; no scene waits on a model turn
// at all. Bang commands are answered without one.

const partsDir = partsDirFor("cost");
const TITLE = "Cost, models and budget";

const advancedModel = () => process.env.VIDEO_ADVANCED_MODEL ?? "";

/** One stored setting, or "" when the organisation never stored it. */
const settingValue = (key: string) =>
  dbValue(`select value from settings where org_id=${ORG()} and key='${key}';`);

/** This month's spend on the shared key, as MonthSpend() in store.go counts it. */
function monthSpend(): number {
  return (
    Number(
      dbValue(
        `select coalesce(sum(cost_usd),0) from usage where org_id=${ORG()} and created_at >= strftime('%Y-%m-01 00:00:00','now');`,
      ),
    ) || 0
  );
}

/**
 * The model id a ModelCombobox trigger shows (model-combobox.tsx renders the
 * value in a font-mono span; an empty value shows its emptyLabel instead, in
 * no span at all). count() first: textContent() on a locator that matches
 * nothing waits thirty seconds before it gives up.
 */
async function modelShown(trigger: Locator): Promise<string> {
  const mono = trigger.locator(".font-mono");
  if ((await mono.count()) === 0) return "";
  return ((await mono.first().textContent({ timeout: 2_000 }).catch(() => "")) ?? "").trim();
}

/** The picker's search box, once a ModelCombobox is open (model-combobox.tsx). */
const modelSearch = (s: Surfaces) => s.page.getByPlaceholder("Search models, or type an id…");

// --- 1. the overview -----------------------------------------------------------

const overview: Guide = {
  id: "1-overview",
  title: TITLE,
  before: async () => {
    assertWorkspaceState();
    if (orgPlan() !== "free") {
      throw new Error(
        "The budget lines are written for the free plan (a fixed budget, a read-only field, the support email), " +
          "and this organisation is on the pro plan. Record in the organisation the deep dive founded, or " +
          "rewrite the budget scene in 2-models.",
      );
    }
    if (monthSpend() <= 0) {
      throw new Error(
        "Nothing has been spent this month, so the spend tile reads zero and the top-channels table is empty. " +
          "Ask the bot something first — any Slack walkthrough does — then record this one.",
      );
    }
  },
  scenes: [
    {
      chapter: "Spend, at a glance",
      say: `The overview. Spend this month against the budget; turns today and over the last seven days; and what the bot has to work with: documents, access bundles, Slack scopes, routines, memories.`,
      act: async (s) => {
        await s.app.goto("/");
        await s.wait(700);
        await s.app.point(s.page.getByRole("link", { name: /^spend this month/i }));
        await s.wait(600);
      },
    },
    {
      chapter: "Numbers link to rows",
      say: `Every number links to its rows. Spend this month opens this month's activity: each turn, its tokens, its cost. The tile carries its window, so the rows are the ones it counted.`,
      act: async (s) => {
        // The tile is a link to /activity?range=month (overview-page.tsx).
        await s.app.click(s.page.getByRole("link", { name: /^spend this month/i }), { settle: 800 });
        await s.page.waitForURL(/\/activity/, { timeout: 20_000 });
        await s.wait(1_400);
        // The time filter, landed on "This month" by the tile's query string.
        await s.app.point(s.page.getByRole("combobox", { name: /^filter by time$/i }));
        await s.wait(600);
      },
    },
    {
      say: `Back on the overview, spend by channel: turns, tokens in and out, cost. Each channel links to its own rows for the month, filtered to that channel.`,
      act: async (s) => {
        await s.app.goto("/");
        await s.wait(400);
        await s.app.scroll(320);
        // The channel's name in "Top channels this month" is a link to
        // /activity?channel=…&range=month; nothing in the nav carries the
        // channel's name, so the first match is the table's.
        await s.app.point(s.page.getByRole("link", { name: new RegExp(escapeRe(channel()), "i") }));
        await s.wait(700);
      },
    },
  ],
};

// --- 2. models and budget ------------------------------------------------------

const models: Guide = {
  id: "2-models",
  title: TITLE,
  before: async () => {
    assertWorkspaceState();
  },
  scenes: [
    {
      chapter: "Which model",
      say: `Settings, Models. The model for everyday replies is whatever the endpoint serves, and a channel can override it on its own page.`,
      act: async (s) => {
        await s.app.goto("/settings");
        await s.wait(600);
        await s.app.click(s.page.getByRole("tab", { name: /^models$/i }), { settle: 900 });
      },
    },
    {
      say: `Open it and type a fragment. The list is what the endpoint lists, with each model's context window and its price per million tokens. Pick one, and the next message uses it. Nothing to redeploy. Leave it as it is.`,
      act: async (s) => {
        // #model is the trigger (settings-page.tsx); the list is fetched from
        // GET /api/models, the provider's own catalogue. Opened, searched, and
        // closed with Escape: nothing is picked, so the organisation stays on
        // the model it had.
        await s.app.click(s.page.locator("#model"), { settle: 700 });
        await s.app.type(modelSearch(s), "glm", 70);
        await s.wait(1_800);
        await s.page.keyboard.press("Escape");
        await s.wait(500);
      },
    },
    {
      chapter: "The advanced model",
      say: `The advanced model is the second one. Fix jobs run on it, and a thread can switch to it with one command. It is picked the same way.`,
      act: async (s) => {
        const trigger = s.page.locator("#heavy_model");
        const shown = await modelShown(trigger);
        const want = advancedModel() || shown;
        if (!want) {
          throw new Error(
            "No advanced model is in force and VIDEO_ADVANCED_MODEL is not set. Set it in video/.env to a model " +
              "the picker lists, so 3-slack's `!model advanced` has something to switch to.",
          );
        }
        await s.app.click(trigger, { settle: 700 });
        await s.app.type(modelSearch(s), want, 40);
        // A listed model's row starts with its id; the "Use … as typed" row that
        // appears for an id the endpoint does not list starts with "Use", so
        // this cannot pick it — and an id the endpoint does not serve stops
        // the take here rather than fail every advanced turn afterwards.
        const option = s.page.getByRole("option", { name: new RegExp(`^${escapeRe(want)}(\\s|$)`) }).first();
        await option.waitFor({ state: "visible", timeout: 20_000 }).catch(() => {
          throw new Error(
            `The endpoint does not list ${want}. Set VIDEO_ADVANCED_MODEL to a model the picker lists (open ` +
              `Settings → Models → Advanced model and type a fragment).`,
          );
        });
        await s.app.click(option, { settle: 800 });
        const after = await modelShown(trigger);
        if (after !== want) throw new Error(`The advanced model reads "${after}" after picking ${want}.`);
        // Picking the model already in force changes nothing, so the tab's
        // Save stays disabled — which clickIfPresent treats as absent — and
        // there is no row to assert. A change is saved and then asserted.
        const saved = await s.app.clickIfPresent(s.page.getByRole("button", { name: /^save \d+ change/i }), {
          settle: 1_500,
        });
        if (saved) {
          assertDb(
            `select count(*) from settings where org_id=${ORG()} and key='heavy_model' and value='${want.replace(/'/g, "''")}';`,
            1,
            "The advanced model was not saved",
          );
        }
      },
    },
    {
      chapter: "The budget",
      say: `The monthly budget. A free account has a fixed one, and the field opens once the account is moved to Pro; Upgrade asks support for that. When the budget is spent, the bot stops replying until the month rolls over. A channel can carry a cap of its own, on its page.`,
      act: async (s) => {
        // aria-label "Monthly budget in USD", disabled on the free plan and
        // showing the figure the bot stops at (settings-page.tsx). Beneath it,
        // UpgradeButton: pointed at, never pressed — pressing it mails
        // support. It is absent once the account is on the pro plan.
        await s.app.point(s.page.locator("#monthly_budget_usd"));
        await s.wait(900);
        const ask = s.page.getByRole("button", { name: /^upgrade$/i });
        if (await ask.count()) {
          await s.app.point(ask.first());
          await s.wait(700);
        }
      },
    },
    {
      chapter: "The worker's budget",
      say: `Workers. Each fix job has a budget of its own. The worker stops at that figure, and the bot cancels a job that runs a quarter past it.`,
      act: async (s) => {
        await s.app.click(s.page.getByRole("tab", { name: /^workers$/i }), { settle: 900 });
        // aria-label "Per-job budget in USD" (settings-page.tsx).
        await s.app.point(s.page.locator("#worker_job_budget_usd"));
        await s.wait(800);
      },
    },
  ],
};

// --- 3. in Slack ---------------------------------------------------------------

const slack: Guide = {
  id: "3-slack",
  title: TITLE,
  before: async () => {
    assertWorkspaceState();
    // `!model advanced` answers "No advanced model is configured" when the
    // effective HeavyModel is empty (commands.go), and the line here is
    // written for the configured case. The stored row wins over the
    // instance's HEAVY_MODEL and the product's own default, so a stored empty
    // value is the one way to reach the other reply.
    const rows = dbCount(`select count(*) from settings where org_id=${ORG()} and key='heavy_model';`);
    const value = settingValue("heavy_model");
    if (rows && !value) {
      throw new Error(
        "The advanced model is stored as nothing, so `!model advanced` would answer that none is configured. " +
          "Set one under Settings → Models — 2-models does.",
      );
    }
    if (advancedModel() && value !== advancedModel()) {
      throw new Error(
        `VIDEO_ADVANCED_MODEL is ${advancedModel()} but the organisation's advanced model is ` +
          `${value || "the instance default"} — run 2-models first.`,
      );
    }
    if (settingValue("show_cost") === "0") {
      throw new Error(
        "Show cost in replies is off, so the footer carries no dollars and the closing line is wrong. " +
          "Switch it on under Settings → Models.",
      );
    }
  },
  scenes: [
    {
      chapter: "Usage from Slack",
      say: `In Slack, usage answers with this month's spend against the budget, and each channel's calls, tokens and cost. Anyone in the channel can ask.`,
      act: async (s) => {
        // Slack boots blank coming off the seam; open it inside the cut that
        // opens the scene.
        await s.offCamera(async () => {
          await s.slack.openChannel(channel());
          await s.slack.closeThread();
        });
        await command(s, "!usage");
      },
    },
    {
      chapter: "Switch a thread's model",
      // The thread pane has no composer this rig can reach — compose() types
      // into the first message box on the page, which is the channel's, and
      // send() closes the pane first — so no question is asked in the thread.
      // The reply names the model the thread now uses; the take ends on it.
      say: `One thread can switch to the advanced model: model advanced, in the thread. The next answer here comes from it, and its footer says so. Model default switches back.`,
      act: async (s) => {
        // "This thread now uses the advanced model `…`." (commands.go)
        await command(s, "!model advanced");
      },
    },
    {
      say: `The footer on every answer is the bill, line by line: the model, the tokens in, the tokens out, the cost. The overview is those lines, added up, and the budget is where they stop.`,
      act: async (s) => {
        await s.wait(1_200);
      },
    },
  ],
};

export const COST: LongForm = {
  id: "cost",
  title: TITLE,
  cardChapters: [
    "Spend, at a glance",
    "Numbers link to rows",
    "Which model",
    "The advanced model",
    "The budget",
    "Usage from Slack",
    "Switch a thread's model",
  ],
  partsDir,
  outDir,
  parts: [overview, models, slack],
  // The default model's picker open on a searched list — the product's answer
  // to "which model", in one frame.
  poster: { chapter: "Which model", offset: 12 },
};
