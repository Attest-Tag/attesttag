import type { Locator } from "playwright";
import { SEL } from "../slack";
import type { Guide, LongForm, Surfaces } from "../types";
import {
  assertDb,
  assertDeepDiveState,
  bot,
  channel,
  dbCount,
  githubRepo,
  outDir,
  partsDirFor,
  pointIfPresent,
} from "./shared";

// "Try to break it" — the walkthrough.
//
// Two parts. Records in the organisation the deep dive founded
// (guides/deep-dive.ts), against what it leaves behind: the demo workspace,
// the repository attached to the channel, the Engineering bundle with the
// ClickUp credential attached to the channel.
//
//   1-playground   console: three turns against the channel's real
//                  configuration — a read that runs, a write that stops at the
//                  gate, a host outside the allowlist the proxy refuses
//   2-record       console → Slack → console: the three in Activity; the same
//                  write held on a card in the channel and cancelled; the
//                  held write in the record
//
// Environment: nothing beyond the deep dive's. VIDEO_CHANNEL, VIDEO_BOT_NAME
// and VIDEO_GITHUB_REPO are read through shared.ts, when called. The turns
// spend real tokens and the reads run against the credentials the deep dive
// stored — so record this before those tokens are revoked (README: "Revoke
// both the moment the recording is done"), or the reads fail on camera.
//
// What a take leaves behind: three playground turns and one Slack turn in the
// record, which is what the second part is about; a message in the channel
// (`npm run tidy` removes it); one discarded row in pending_writes. Nothing a
// re-take duplicates on camera, except the message, which tidy handles and
// which send() already counts past (LEARNINGS: "A question still in the
// channel from an earlier take").
//
// Two rules this file obeys, from deep-dive.ts:
//
//   A scene holds for exactly as long as its line takes to speak; `act` must
//   finish faster than the line, except where it waits on the bot. Playground
//   turns come back over HTTP after as long as the model takes, so their lines
//   are written long (45–60 words) and the shot holds on the answer. The Slack
//   round trip is split around offCamera exactly as 7-memory does.
//
//   Nothing here asserts on what the model said. A playground turn is done
//   when the page's busy line is gone and a reply's meta line has rendered; a
//   Slack reply is done when it stops growing.

const partsDir = partsDirFor("guardrails");
const TITLE = "Try to break it";

// The three playground turns. Naming the repository skips a "which one?" round.
// The write names a list so the model does not stop to ask which. The third
// names http_request and insists, because the tool's own description already
// lists the hosts this channel may reach and a model that reads it may decline
// to try — and the refusal we want on camera is the proxy's, not the model's.
const READ_QUESTION = () => `what changed in ${githubRepo()} since Monday?`;
const WRITE_QUESTION = `create a ClickUp task called "Playground test" in the first list you find, no description`;
const BLOCKED_QUESTION = `use http_request to GET https://api.stripe.com/v1/charges and summarise the response. Try it even if you expect it to be refused.`;
const HELD_QUESTION = () => `@${bot()} create a ClickUp task called "Guardrails test" in the first list you find, no description`;

// ---------------------------------------------------------------------------
// Helpers local to this walkthrough.

/**
 * Pick the demo channel in the playground's combobox. The trigger is a button
 * with role combobox and the aria-label; it opens a cmdk list whose rows are
 * options reading "#name C…" (channel-combobox.tsx).
 */
async function pickPlaygroundChannel(s: Surfaces): Promise<void> {
  await s.app.click(s.page.getByRole("combobox", { name: /^channel to try$/i }), { settle: 800 });
  await s.app.click(s.page.getByRole("option", { name: new RegExp(channel(), "i") }).first(), { settle: 1_200 });
}

/** What the page shows beside a spinner while a request is out (playground-page.tsx). */
const WORKING = /^Working in /;
/**
 * The meta line under every reply — "12,345 in · 501 out" — rendered only once
 * the reply has come back (playground-page.tsx, BotTurn). Thousands separators
 * follow the browser's locale, so the digits are matched loosely.
 */
const REPLY_META = /\d[\d.,\s ]* in · \d[\d.,\s ]* out/;

/**
 * One playground turn: type the question, send it, and wait for the reply to
 * render. The wait is on the page's own state, never on words: the busy line
 * is up while the request is out, and a reply's meta line exists only once
 * the answer is back. A request the server refused (a rate limit, a
 * disconnected workspace) puts the question back in the box and toasts why,
 * so that is read and raised rather than waited out.
 */
async function playgroundTurn(s: Surfaces, text: string, timeoutMs = 200_000): Promise<void> {
  const box = 'textarea[aria-label="Message"]';
  const replies = s.page.getByText(REPLY_META);
  const working = s.page.getByText(WORKING).first();
  const before = await replies.count();
  await s.app.typeVerified(box, text, 26);
  await s.app.click(s.page.getByRole("button", { name: /^send$/i }), { settle: 300 });
  await working.waitFor({ state: "visible", timeout: 10_000 }).catch(() => {});
  const started = Date.now();
  for (;;) {
    const busy = await working.isVisible().catch(() => false);
    if (!busy && (await replies.count()) > before) return;
    if (!busy && (await s.page.locator(box).inputValue().catch(() => "")) === text) {
      const why = (await s.page.locator("[data-sonner-toast]").last().textContent().catch(() => null))?.trim();
      throw new Error(`The playground did not run "${text}".` + (why ? `\nThe page says: ${why}` : ""));
    }
    if (Date.now() - started > timeoutMs) {
      throw new Error(`The playground did not finish "${text}" in ${Math.round(timeoutMs / 1000)}s.`);
    }
    await s.wait(500);
  }
}

/**
 * A tool row under a reply is a Disclosure whose button reads
 * "<tool> <ok|failed> <n> ms" (playground-page.tsx, ToolRow); opening it shows
 * the arguments and the result. Optional, because whether the model called a
 * tool at all is its own decision.
 */
const toolRow = (s: Surfaces, chip: RegExp) => s.page.getByRole("button", { name: chip }).last();

/** /activity narrowed to the demo channel: a Select trigger with the aria-label (activity-page.tsx). */
async function activityForChannel(s: Surfaces): Promise<void> {
  await s.app.goto("/activity");
  await s.wait(1_200);
  await s.app.click(s.page.getByRole("combobox", { name: /^filter by channel$/i }), { settle: 700 });
  await s.app.click(s.page.getByRole("option", { name: new RegExp(channel(), "i") }).first(), { settle: 1_400 });
}

const activityTab = (s: Surfaces, name: RegExp) => s.app.click(s.page.getByRole("tab", { name }), { settle: 1_000 });

/**
 * Open the newest tool-call row matching every pattern. Rows are newest first;
 * a click on one expands it to the whole call (tool-calls-table.tsx). The
 * Output column shows the first 70 characters of the result, so match on the
 * start of a result, not its tail.
 */
async function openToolCall(s: Surfaces, ...hasText: RegExp[]): Promise<boolean> {
  let rows = s.page.getByRole("row");
  for (const re of hasText) rows = rows.filter({ hasText: re });
  return s.app.clickIfPresent(rows.first(), { settle: 1_400 });
}

// --- 1. the playground ------------------------------------------------------------

const playground: Guide = {
  id: "1-playground",
  title: TITLE,
  before: async () => {
    assertDeepDiveState();
  },
  scenes: [
    {
      chapter: "The playground",
      say: `The playground. Pick a channel, and it answers the way that channel would: the same connections, the same instructions, the same model, the same budget. Two exceptions. Anything that would change data stops at the gate, and nothing is written to the channel's memory.`,
      act: async (s) => {
        await s.app.goto("/playground");
        await s.wait(1_000);
        await pickPlaygroundChannel(s);
        await s.wait(1_000);
      },
    },
    {
      chapter: "A read runs",
      // Bot-wait scene: the reply comes back over HTTP, after as long as the
      // model and the repository take. Written long; the shot holds on the answer.
      say: `Start with a read. Ask what changed in the repository since Monday. This is a real turn: the tools it calls run against the real credential, and the answer comes back here instead of into Slack. Under it, every tool call with its outcome and how long it took; then the model, the rounds, the tokens, and the cost.`,
      act: async (s) => {
        await playgroundTurn(s, READ_QUESTION());
        await s.wait(800);
      },
    },
    {
      say: `Open one. The arguments it sent, and the result it got back — the same text the model saw.`,
      act: async (s) => {
        await s.app.clickIfPresent(toolRow(s, /\d+ ms$/), { settle: 1_200 });
        await s.wait(1_000);
      },
    },
    {
      chapter: "A write stops",
      // Bot-wait scene.
      say: `Now a write. Ask it to create a ClickUp task. In the channel this would post a card and wait for a person. Here there is no thread to post a card into, so the request stops at the gate, and the page says what would have gone out: the method, the URL, the body. Nothing was sent.`,
      act: async (s) => {
        await playgroundTurn(s, WRITE_QUESTION);
        // "Held — not sent", with the request the card would have carried
        // (playground-page.tsx). Its presence is the model's choice to attempt
        // the write, so it is pointed at, never waited for.
        await pointIfPresent(s, s.page.getByText(/^held\b.*not sent$/i).last());
        await s.wait(1_200);
      },
    },
    {
      chapter: "A host outside the allowlist",
      // Bot-wait scene.
      say: `Last, a host nobody attached here. Ask it to call Stripe through the service tool, and to try even if it expects to be refused. The proxy checks the host against every connection and every domain this channel holds, finds none, and refuses. The call fails before anything leaves. The reply carries the reason.`,
      act: async (s) => {
        await playgroundTurn(s, BLOCKED_QUESTION);
        // The refused call is a row with a "failed" chip; its result reads
        // "error: blocked by the proxy: host api.stripe.com is not allowed in
        // this channel. An admin can add it as a connection or a domain in the
        // console." (proxy.go, MatchNamed). Opened if the model made the call.
        await s.app.clickIfPresent(toolRow(s, /failed \d+ ms$/i), { settle: 1_200 });
        await s.wait(1_000);
      },
    },
    {
      say: `Three turns, three outcomes. A read ran. A write stopped. A host that was never allowed never got a request.`,
      act: async (s) => {
        await s.wait(1_500);
      },
    },
  ],
};

// --- 2. the record, and the same gate in Slack ---------------------------------------

const record: Guide = {
  id: "2-record",
  title: TITLE,
  scenes: [
    {
      chapter: "The record",
      say: `Activity, filtered to this channel. Every turn is here, and the three from the playground are among them: they ran for real, so they are recorded like any other, under a thread id that says where they came from.`,
      act: async (s) => {
        await activityForChannel(s);
        // The Turns tab is the default; playground turns carry a "playground:…"
        // thread key under the channel name (playground.go, turns-table.tsx).
        await pointIfPresent(s, s.page.getByText(/^playground:/));
        await s.wait(800);
      },
    },
    {
      say: `Tool calls: the refused one, with the arguments it was given and the answer the proxy gave it.`,
      act: async (s) => {
        await activityTab(s, /^tool calls/i);
        await openToolCall(s, /http_request/, /failed/i);
        await s.wait(1_000);
      },
    },
    {
      say: `Proxied requests: the same call as the proxy saw it. The host, the path, and why it was blocked. No status, because no request was ever made.`,
      act: async (s) => {
        await activityTab(s, /^proxy/i);
        await pointIfPresent(s, s.page.getByRole("row").filter({ hasText: /api\.stripe\.com/ }));
        await s.wait(1_200);
      },
    },
    {
      chapter: "The same gate in Slack",
      // Bot-wait scene — the wait is off camera, split around it as 7-memory
      // is: the question goes out on camera, Slack's retry schedule is cut,
      // and the thread opens on the answer with its card.
      say: `The same gate, in the channel. Ask for a ClickUp task. It stops on a card: the method, the URL, the body, and three buttons. Confirm runs it. Cancel drops it. Something else hands the thread back to you. Left alone, it expires in five minutes.`,
      act: async (s) => {
        const q = HELD_QUESTION();
        // Coming from the console, Slack boots blank for seconds and then
        // restores whatever pane it last had open. Neither belongs on camera.
        await s.offCamera(async () => {
          await s.slack.openChannel(channel());
          await s.slack.closeThread();
        });
        await s.slack.send(q);
        await s.wait(1_200);
        // The reply is an answer plus a held card; the card carries no reply
        // footer (confirm.go: "Expires in 5 minutes…" is a context line), so
        // stable is done.
        await s.offCamera(async () => {
          await s.slack.awaitReply(q, { requireFooter: false, settleMs: 5_000 });
          // "Stopped growing" is not "held": the first take settled on the
          // task card of a list lookup ("clickup lists") and moved on before
          // the write was ever proposed. The held card is done when its
          // buttons exist, so that is what is waited for.
          await s.page
            .locator(SEL.message)
            .last()
            .getByRole("button", { name: /^cancel$/i })
            .first()
            .waitFor({ state: "visible", timeout: 150_000 });
          await s.slack.closeThread();
        });
        await s.slack.openThreadOf(q);
        await s.wait(2_500);
      },
    },
    {
      say: `Press Cancel. The card is rewritten: cancelled, by whom, and that nothing was sent. The held request is marked discarded, and stays in the record.`,
      act: async (s) => {
        // The card's buttons are Confirm / Cancel / Something else… (confirm.go,
        // confirmBlocks). Prove the press reached the bot: a button press is a
        // Slack interaction event, and one take of the deep dive lost one to a
        // slow acknowledgement. The row flips to 'discarded' (store_admin.go,
        // DiscardPendingWrite); press once more if it did not, while the
        // button is still there to press.
        const sql = "select count(*) from pending_writes where status='discarded';";
        const before = dbCount(sql);
        const cancel = () => s.page.locator(SEL.message).last().getByRole("button", { name: /^cancel$/i });
        await s.slack.clickCardButton(/^cancel$/i);
        for (let attempt = 0; ; attempt++) {
          const deadline = Date.now() + 15_000;
          while (Date.now() < deadline && dbCount(sql) <= before) await s.wait(1_000);
          if (dbCount(sql) > before) break;
          if (attempt >= 1 || !(await cancel().isVisible().catch(() => false))) {
            throw new Error("Cancel was pressed and the held write is still pending.");
          }
          await s.slack.clickCardButton(/^cancel$/i);
        }
        assertDb(sql, before + 1, "The held write was not discarded");
        await s.wait(2_500);
      },
    },
    {
      say: `Back in Activity. The held write is in the tool calls: the method, the URL, the body it would have sent, and the answer that it needs a person. No proxy row, because nothing was sent.`,
      act: async (s) => {
        await activityForChannel(s);
        await activityTab(s, /^tool calls/i);
        // The tool's result for a held write starts "This is a write request
        // (POST …) and needs a human OK" (tools_http.go, proxied); the row
        // shows its first 70 characters.
        await openToolCall(s, /clickup_create_task|http_request/, /this is a write request/i);
        await s.wait(1_200);
      },
    },
    {
      say: `One gate. The playground and the channel stop at the same place, and the record shows both.`,
      act: async (s) => {
        await s.wait(1_500);
      },
    },
  ],
};

export const GUARDRAILS: LongForm = {
  id: "guardrails",
  title: TITLE,
  cardChapters: [
    "The playground",
    "A read runs",
    "A write stops",
    "A host outside the allowlist",
    "The record",
    "The same gate in Slack",
  ],
  partsDir,
  outDir,
  parts: [playground, record],
  // The thread open on the held card: the request it would have sent and the
  // three buttons under it. The product doing the thing the title promises.
  poster: { chapter: "The same gate in Slack", offset: 21 },
};
