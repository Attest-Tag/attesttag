import type { Guide, LongForm, Surfaces } from "../types";
import { SEL } from "../slack";
import {
  BUNDLE,
  assertDb,
  assertDeepDiveState,
  bot,
  channel,
  dbCount,
  ORG,
  outDir,
  partsDirFor,
  waitForDb,
} from "./shared";

// "Ask for access you don't have" — the walkthrough.
//
// Three parts, recorded in the organisation the deep dive founded:
//
//   1-tier     the console: who may approve, and what a tier may grant from
//   2-ask      Slack, in a second channel with nothing attached: an ask the channel
//              cannot do, the request it becomes, and what the requester has waiting
//   3-record   the console's record of it; the requester withdraws it from the thread;
//              the record says so
//
// One constraint shapes the whole thing: a requester cannot approve their own request,
// and the recorder is one Slack account. So the Approve press is never on camera. What
// is: who may approve being set up, the ask, the thread card that says who it is waiting
// on, the requester's own list, the console's record, and the withdrawal.
//
// Environment, beyond what the deep dive needs. All of it is read when called — see
// LEARNINGS.md, "Environment, and where it is read".
//
//   VIDEO_CHANNEL_2         (default attesttag-support) A second channel in the demo
//                           workspace, with the bot invited and NOTHING attached to it:
//                           no bundle, no repository. Make it once, without the backstory:
//                             VIDEO_CHANNEL=attesttag-support npm run setup
//                           and tidy it between takes the same way:
//                             VIDEO_CHANNEL=attesttag-support npm run tidy
//   VIDEO_APPROVER          The Slack user id (U…) of a SECOND member of the demo
//                           workspace — not the recording account, which is the
//                           requester and may not approve its own ask. They receive the
//                           approval card by direct message.
//   VIDEO_APPROVER_HANDLE   That person's handle as Slack's @-picker lists it first for
//                           those characters: one token, e.g. `jane.doe`. The ask tags
//                           them. An approver tagged in the message is what puts the
//                           "call request_access" instruction in front of the model
//                           (agent.go, taggedApprovers); untagged, it may answer that it
//                           cannot reach ClickUp here and stop.
//   VIDEO_SIDEBAR_KEEP      Must list both channels, or the second is hidden by the
//                           sidebar filter and cannot be opened:
//                             VIDEO_SIDEBAR_KEEP=attesttag-demo,attesttag-support
//
// Re-takes. 1-tier creates a tier. A second tier on the page is on camera and changes
// which one a request routes to, so `before` stops when any tier exists and says which
// to delete (the trash icon on /approvers). 2-ask leaves a pending request; 3-record
// withdraws it. A person may hold three open requests at once (access_requests.go,
// maxOpenPerPerson), so 2-ask also stops when three are already waiting.

const partsDir = partsDirFor("access");
const TITLE = "Ask for access you don't have";

// ---------------------------------------------------------------------------
// Environment. Read when called, never at module scope.
const channel2 = () => process.env.VIDEO_CHANNEL_2 ?? "attesttag-support";
const approver = () => (process.env.VIDEO_APPROVER ?? "").trim().toUpperCase();
const approverHandle = () => (process.env.VIDEO_APPROVER_HANDLE ?? "").trim().replace(/^@/, "");
const keptChannels = () =>
  (process.env.VIDEO_SIDEBAR_KEEP ?? process.env.VIDEO_CHANNEL ?? "attesttag-demo")
    .split(",")
    .map((c) => c.trim())
    .filter(Boolean);

const TIER = "Approvers";

// Functions: ORG() reads the environment, which index.ts loads after imports.
const REQUESTS = () => `select count(*) from access_requests where org_id = ${ORG()};`;
const PENDING = () => `select count(*) from access_requests where org_id = ${ORG()} and status='pending' and expires_at > datetime('now');`;
const CANCELLED = () => `select count(*) from access_requests where org_id = ${ORG()} and status='cancelled';`;

// The lines this walkthrough sends. Functions, because the handles come from the
// environment; the same text is what openThreadOf() finds the row by in 3-record.
//
// The ask avoids the word "access" and any #channel. ask() finds a row by the last five
// words of the message, and `!access` is found by the one word "access": a row that
// already says it, with a reply bar, would be taken for that command's. And a typed
// "#channel" opens Slack's channel picker, which compose() does not handle.
const ASK = () =>
  `@${bot()} @${approverHandle()} this channel can't reach ClickUp. Could you request it for me, so the reconciliation change discussed in attesttag-demo gets its ticket?`;
const LIST = () => `@${bot()} !access`;
const CANCEL = () => `@${bot()} !access cancel`;

// ---------------------------------------------------------------------------
// Helpers.

/**
 * The card of one approval tier on /approvers: the innermost element holding the tier's
 * heading and an "Add" button (approvers-page.tsx, RoleCard). The page has two "Add"s per
 * tier — "Grants from" first, Members second — and one more per tier on the page.
 */
function tierCard(s: Surfaces, name: string) {
  return s.page
    .locator("div")
    .filter({ has: s.page.getByRole("heading", { name: new RegExp(`^${name}$`) }) })
    .filter({ has: s.page.getByRole("button", { name: /^add$/i }) })
    .last();
}

/**
 * Type a line into the OPEN thread's composer and send it.
 *
 * SlackStage.compose types into the first composer on the page, which is the channel's,
 * and send() closes any thread first — so nothing in slack.ts can reply inside a thread.
 * `!access cancel` has to: CancelAccessRequests matches on the thread the request was
 * raised in (store_access.go), and a top-level message is its own thread. With a pane
 * open there are two composers, and the pane's is the last. The rest follows compose():
 * clear the draft, commit each @mention through the picker and only while it is open,
 * dismiss any list before Enter, and prove the send by the words being gone.
 */
async function replyInThread(s: Surfaces, text: string): Promise<void> {
  const boxes = s.page.locator(SEL.composer);
  if ((await boxes.count().catch(() => 0)) < 2) {
    throw new Error("No thread pane is open, so there is no thread composer to reply in — openThreadOf() first.");
  }
  const box = boxes.last();
  await s.slack.click(box, { settle: 150 });
  await box.press("End");
  await s.page.keyboard.press("Meta+KeyA");
  await s.page.keyboard.press("Backspace");
  await s.wait(250);
  for (const part of text.split(/(@[A-Za-z0-9._-]+)/).filter(Boolean)) {
    if (!part.startsWith("@")) {
      await box.pressSequentially(part, { delay: 38 });
      continue;
    }
    await box.pressSequentially(part, { delay: 55 });
    const option = s.page.getByRole("option").first();
    const opened = await option
      .waitFor({ state: "visible", timeout: 8_000 })
      .then(() => true)
      .catch(() => false);
    if (!opened) {
      throw new Error(
        `No mention autocomplete for "${part}" in the thread composer. That handle matches no one, ` +
          `so it would post as plain text and the bot would never hear it.`,
      );
    }
    await s.wait(700);
    await box.press("Tab");
    await s.wait(350);
  }
  await s.wait(420);
  await box.focus();
  await box.press("End");
  await s.wait(200);
  if (await s.page.getByRole("option").first().isVisible().catch(() => false)) {
    await box.press("Escape");
    await s.wait(250);
  }
  await box.press("Enter");
  await s.wait(900);
  const words = text
    .replace(/@[A-Za-z0-9._-]+/g, " ")
    .replace(/[^A-Za-z0-9 ]/g, " ")
    .split(/\s+/)
    .filter(Boolean)
    .slice(-3);
  const tail = new RegExp(words.join("\\W+"), "i");
  const left = ((await box.textContent().catch(() => "")) ?? "").replace(/\s+/g, " ");
  if (tail.test(left)) {
    throw new Error(`The reply did not send — the thread composer still holds "${left.slice(0, 80)}".`);
  }
}

// --- 1. who may approve ------------------------------------------------------------

const tier: Guide = {
  id: "1-tier",
  title: TITLE,
  before: async () => {
    assertDeepDiveState();
    if (!approver()) {
      throw new Error(
        "1-tier adds an approver on camera and needs VIDEO_APPROVER in video/.env: the Slack user id (U…)\n" +
          "of a second member of the demo workspace — not the recording account. See the header of guides/access.ts.",
      );
    }
    if (!/^[UW][A-Z0-9]{4,}$/.test(approver())) {
      throw new Error(`VIDEO_APPROVER must be a Slack user id (U…), not "${approver()}".`);
    }
    if (!approverHandle()) {
      throw new Error(
        "2-ask tags the approver and needs VIDEO_APPROVER_HANDLE in video/.env: their handle as Slack's\n" +
          "@-picker lists it, one token. See the header of guides/access.ts.",
      );
    }
    if (!keptChannels().includes(channel2())) {
      throw new Error(
        `#${channel2()} is hidden by the sidebar filter. Set VIDEO_SIDEBAR_KEEP=${channel()},${channel2()} in video/.env.`,
      );
    }
    if (dbCount(`select count(*) from connections where org_id = ${ORG()} and preset='clickup' and status='active';`) < 1) {
      throw new Error(`No active ClickUp connection in the ${BUNDLE} bundle, so no tier could grant the ask — run \`npm run video:deep -- 3-connect\` first.`);
    }
    if (dbCount("select count(*) from teams where status='active' and dm_scope=1;") < 1) {
      throw new Error(
        "The workspace install lacks the im:write scope, so an approval card cannot be delivered by DM\n" +
          "(approvers.go, checkDMScope). Reconnect the workspace from /workspaces and try again.",
      );
    }
    if (dbCount(`select count(*) from approval_roles where org_id = ${ORG()};`) > 0) {
      throw new Error(
        "An approval tier already exists in this organisation — an earlier take, or a hand-made one.\n" +
          "A second tier is on camera and can change which tier a request routes to, so delete every\n" +
          "tier first: /approvers → the trash icon on each card → Delete. Then re-record from 1-tier.",
      );
    }
  },
  scenes: [
    {
      chapter: "Who may approve",
      say: `Sometimes the person asking is not the person who may say yes. The Approvers page is where that is set. Add a tier.`,
      act: async (s) => {
        await s.app.goto("/approvers");
        await s.wait(1_200);
        // The empty state's button and the page's outline button share the name.
        await s.app.click(s.page.getByRole("button", { name: /^add a tier$/i }).first(), { settle: 900 });
      },
    },
    {
      say: `A tier has a name and a rank. Rank is only an order: a higher tier can approve anything a lower one can, and a request no lower tier can grant escalates to one that can.`,
      act: async (s) => {
        // approvers-page.tsx: #role-name, #role-rank (already 1, the first tier's natural
        // place), and the form's own "Add" — the tier cards below each carry two more.
        await s.app.typeVerified("#role-name", TIER);
        await s.app.point("#role-rank");
        await s.wait(600);
        await s.app.click(
          s.page.locator("form").filter({ has: s.page.locator("#role-name") }).getByRole("button", { name: /^add$/i }),
          { settle: 1_500 },
        );
        assertDb(`select count(*) from approval_roles where org_id = ${ORG()} and name='${TIER}';`, 1, "The tier was not created");
      },
    },
    {
      chapter: "What a tier grants",
      say: `A tier grants from what it lists: whole bundles, or single connections. This one grants from the Engineering bundle, so an approved request runs under Engineering's connections and nothing else.`,
      act: async (s) => {
        // "Grants from" → Add opens the same command palette a channel page uses
        // (add-access-popover.tsx); bundles are listed before connections, so the first
        // option matching the name is the bundle itself.
        await s.app.click(tierCard(s, TIER).getByRole("button", { name: /^add$/i }).first(), { settle: 900 });
        await s.app.typeVerified('input[placeholder="Search bundles and connections…"]', BUNDLE);
        await s.wait(600);
        await s.app.click(s.page.getByRole("option", { name: new RegExp(BUNDLE, "i") }).first(), { settle: 1_500 });
        assertDb(
          `select count(*) from approval_role_bundles b join approval_roles r on r.id=b.role_id where r.org_id = ${ORG()} and r.name='${TIER}';`,
          1,
          "The bundle was not added to the tier",
        );
      },
    },
    {
      say: `Members: a Slack user id or an email address, resolved against the connected workspace. Who was tagged in a message is only a hint. It is checked against this list, and someone not on it cannot approve.`,
      act: async (s) => {
        // approvers-page.tsx: the members input is #m-<role id>; its form's "Add" submits.
        await s.app.typeVerified('input[id^="m-"]', approver(), 30);
        await s.wait(400);
        await s.app.click(
          s.page.locator("form").filter({ has: s.page.locator('input[id^="m-"]') }).getByRole("button", { name: /^add$/i }),
          { settle: 1_800 },
        );
        assertDb(
          `select count(*) from approval_members m join approval_roles r on r.id=m.role_id where r.org_id = ${ORG()} and r.name='${TIER}' and upper(m.ref)='${approver()}';`,
          1,
          "The approver was not added — is VIDEO_APPROVER a person in the demo workspace?",
        );
        await s.wait(800);
      },
    },
  ],
};

// --- 2. the ask ----------------------------------------------------------------------

const ask: Guide = {
  id: "2-ask",
  title: TITLE,
  before: async () => {
    if (!approverHandle()) throw new Error("2-ask tags the approver and needs VIDEO_APPROVER_HANDLE in video/.env.");
    if (!/^[UW][A-Z0-9]{4,}$/.test(approver())) {
      throw new Error(`VIDEO_APPROVER must be the Slack user id (U…) of the person on the tier, not "${approver()}".`);
    }
    if (!keptChannels().includes(channel2())) {
      throw new Error(`#${channel2()} is hidden by the sidebar filter. Set VIDEO_SIDEBAR_KEEP=${channel()},${channel2()} in video/.env.`);
    }
    if (
      dbCount(
        `select count(*) from approval_members m join approval_roles r on r.id=m.role_id where r.org_id = ${ORG()} and r.name='${TIER}' and upper(m.ref)='${approver()}';`,
      ) < 1
    ) {
      throw new Error(`No "${TIER}" tier with VIDEO_APPROVER on it — record 1-tier first.`);
    }
    if (dbCount(PENDING()) >= 3) {
      throw new Error(
        "Three access requests are already waiting, which is as many as one person may hold open, so the\n" +
          "next ask would be refused. Withdraw them (`!access cancel` in each thread) or close them on\n" +
          "/access-requests, then re-record 2-ask.",
      );
    }
  },
  scenes: [
    {
      chapter: "Ask for it",
      say: `A different channel. Nothing is attached to it: no bundle, no repository. The bot can read Slack here and reach nothing else.`,
      act: async (s) => {
        // Slack boots blank coming from a title card: open it inside the cut that opens
        // the scene. Opening the channel explicitly is what keeps compose() from healing
        // itself into VIDEO_CHANNEL.
        await s.offCamera(async () => {
          await s.slack.openChannel(channel2());
          await s.slack.closeThread();
        });
        await s.wait(1_200);
      },
    },
    {
      // Bot-wait scene, on camera: the status line is the shot. The reply is the answer
      // and then a card, and the card carries no footer.
      say: `Ask for something this channel cannot do, and tag the person who may approve it. The tag is a hint, not a decision: it is checked against the tier. Watch the status line. It runs nothing. It records the exact calls it would run, and sends them to the approver as a direct message carrying the same card.`,
      act: async (s) => {
        const was = dbCount(REQUESTS());
        await s.slack.ask(ASK(), { requireFooter: false, settleMs: 5_000 });
        // Not the model's words — the row. A take where it answered without raising a
        // request would narrate a card that is not there.
        await waitForDb(
          REQUESTS(),
          was + 1,
          20_000,
          "No access request was raised: the model answered without calling request_access. Check the tagged handle (VIDEO_APPROVER_HANDLE) is the person on the tier (VIDEO_APPROVER), then re-record 2-ask",
        );
        assertDb(PENDING(), 1, "The access request is not pending");
        await s.wait(1_000);
      },
    },
    {
      chapter: "Nothing runs yet",
      say: `The card in the thread says who it is waiting on and which request this is. Nothing runs until they press Approve, and the person who asked cannot approve their own. The approver's card lists the same calls, built from what was recorded, not from what the model wrote about them.`,
      act: async (s) => {
        await s.wait(1_500);
        await s.slack.scroll(200);
        await s.wait(1_000);
      },
    },
    {
      chapter: "Your open requests",
      say: `You can see what you have waiting, who it is waiting on, and when it expires, unless somebody answers first.`,
      act: async (s) => {
        await s.slack.send(LIST());
        await s.wait(600);
        await s.offCamera(async () => {
          await s.slack.awaitReply(LIST(), { requireFooter: false, settleMs: 2_500 });
          await s.slack.closeThread();
        });
        await s.slack.openThreadOf(LIST());
        await s.wait(1_000);
      },
    },
    {
      say: `One request, the approver it went to, the expiry, and how to take it back: from the thread it was raised in.`,
      act: async (s) => {
        await s.wait(1_800);
      },
    },
  ],
};

// --- 3. the record, and the withdrawal ----------------------------------------------

const record: Guide = {
  id: "3-record",
  title: TITLE,
  before: async () => {
    if (dbCount(PENDING()) < 1) throw new Error("No access request is waiting — record 2-ask first.");
  },
  scenes: [
    {
      chapter: "The record",
      say: `The console keeps the record. Who asked, for what, in which channel, its status, who answered, and how many steps it would run.`,
      act: async (s) => {
        await s.app.goto("/access-requests");
        await s.wait(1_200);
        // access-requests-page.tsx: a row opens its detail; newest first, so the first
        // pending row is this walkthrough's.
        await s.app.click(s.page.getByRole("row").filter({ hasText: /pending/i }).first(), { settle: 1_500 });
      },
    },
    {
      say: `Open it: the requester's own words, kept apart and marked unverified; the calls exactly as they would run; the approvers; the expiry. Approving is not offered here. That is a Slack act, by a named person, on the card in their direct messages.`,
      act: async (s) => {
        await s.app.point(s.page.getByText(/what runs on approval/i).first());
        await s.wait(1_200);
        await s.app.point(s.page.getByText(/^approvers$/i).first());
        await s.wait(1_000);
        await s.app.point(s.page.getByText(/close this out/i).first());
        await s.wait(800);
      },
    },
    {
      chapter: "Withdraw it",
      say: `Back in the thread it was raised in. Withdraw it. Only the person who asked can, and only their own. Every copy of the approver's card is rewritten: withdrawn, nothing ran.`,
      act: async (s) => {
        // Slack boots blank coming from the console; the thread has to be the one the
        // request was raised in, and that is where the reply goes (see replyInThread).
        await s.offCamera(async () => {
          await s.slack.openChannel(channel2());
          await s.slack.openThreadOf(ASK());
        });
        // The snapshot `send` would take, for waitForBotReply's `ignore`.
        const before = await s.slack.lastBotText();
        const cancelled = dbCount(CANCELLED());
        await replyInThread(s, CANCEL());
        await s.offCamera(async () => {
          await s.slack.waitForBotReply({ requireFooter: false, settleMs: 2_500, ignore: before });
        });
        await waitForDb(CANCELLED(), cancelled + 1, 20_000, "The request was not withdrawn — `!access cancel` did not land in the thread it was raised in");
        await s.wait(1_500);
      },
    },
    {
      say: `The record says cancelled. Who asked, who was asked, what would have run, and how it ended, all in one place.`,
      act: async (s) => {
        await s.app.goto("/access-requests");
        await s.wait(1_200);
        await s.app.point(s.page.getByText(/^cancelled$/i).first());
        await s.wait(1_500);
      },
    },
  ],
};

export const ACCESS: LongForm = {
  id: "access",
  title: TITLE,
  cardChapters: [
    "Who may approve",
    "What a tier grants",
    "Ask for it",
    "Nothing runs yet",
    "The record",
    "Withdraw it",
  ],
  partsDir,
  outDir,
  parts: [tier, ask, record],
  // The thread with the answer and the "Waiting on …" card under it: the request, held.
  poster: { chapter: "Nothing runs yet", offset: 3 },
};
