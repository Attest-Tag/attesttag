import type { Locator } from "playwright";
import type { Guide, LongForm, Surfaces } from "../types";
import {
  BUNDLE,
  assertDb,
  assertDeepDiveState,
  channel,
  dbCount,
  githubRepo,
  openChannelPage,
  outDir,
  partsDirFor,
  pointIfPresent,
  retype,
} from "./shared";

// "What a channel may reach" — the walkthrough.
//
// Console only, two parts. Records in the organisation the deep dive founded
// (guides/deep-dive.ts), against the state it leaves behind: the demo workspace
// connected, the repository attached to the channel directly, and the
// Engineering bundle holding the ClickUp credential, attached to the channel.
//
//   1-scope    /workspaces: the rail, one channel's page, what it may reach,
//              an instruction typed and saved, how the channel behaves
//   2-bundle   /bundles: inside the Engineering bundle — a domain, its
//              instructions, a skill — and where it is attached
//
// Environment: nothing beyond the deep dive's. VIDEO_CHANNEL names the channel,
// VIDEO_GITHUB_REPO the repository the channel page lists (both read through
// shared.ts, when called).
//
// What a take leaves behind, and re-takes:
//
//   1-scope writes one field (the channel's instructions). A re-take types the
//   same text over an emptied box; the Save button is then disabled because
//   nothing changed, the click is skipped, and the assert reads the row the
//   earlier take wrote. Nothing duplicates.
//
//   2-bundle ADDS a domain and a skill, and both would show twice on a second
//   take — two "Ticket conventions" rows in the Skills tab is a wasted render.
//   So its `before` refuses to record over a previous take's leftovers and says
//   what to delete (see assertNoLeftovers). The bundle's instructions are one
//   field, handled like the channel's.
//
// Two rules this file obeys, from deep-dive.ts:
//
//   A scene holds for exactly as long as its line takes to speak; `act` must
//   finish faster than the line. Nothing here waits on the bot.
//
//   Every console scene that changes something ends with assertDb: a row
//   exists, or the take stops with the name of the step that failed.

const partsDir = partsDirFor("reach");
const TITLE = "What a channel may reach";

const CHANNEL_INSTRUCTION = "Answer in bullets. Cite the message each point came from.";
const BUNDLE_INSTRUCTION = "Prefer the list endpoints; never create a space.";
const DOMAIN = "docs.clickup.com";
const SKILL_NAME = "Ticket conventions";
const SKILL_CONTENT =
  "# When to use\n\n- Any time a ticket is created\n\n# Steps\n\n1. Title under 60 characters\n2. First line of the description names the Slack thread";

// ---------------------------------------------------------------------------
// Helpers local to this walkthrough.

/**
 * The Engineering card on /bundles. The page draws one card per bundle and
 * the deep dive leaves two (Engineering, and the Repositories bundle the
 * channel's repository connection lives in), each with a "Configure" button
 * — so the button is reached through its card, never by name alone. The card
 * is the innermost rounded box holding a span reading exactly the bundle's
 * name (bundle-card.tsx: header button > span > span.font-semibold); `.last()`
 * takes the innermost, since document order lists ancestors first.
 */
function bundleCard(s: Surfaces): Locator {
  return s.page.locator("div.rounded-xl").filter({ has: s.page.locator(`span:text-is("${BUNDLE}")`) }).last();
}

/** The bundle editor dialog (bundle-edit-dialog.tsx), which sits above the page and below the skill dialog. */
const editor = (s: Surfaces) => s.page.getByRole("dialog").first();

/**
 * 2-bundle adds a domain and a skill. Recording it again over a previous take
 * puts two of each in the frame, so refuse, and say exactly what to remove —
 * in the console, since the database belongs to the running bot.
 */
function assertNoLeftovers(): void {
  const where = `/bundles → ${BUNDLE} → Configure`;
  const wants: [string, string][] = [
    [`select count(*) from domains where host='${DOMAIN}';`, `the domain ${DOMAIN} (${where} → Domains → the bin icon on its row)`],
    [`select count(*) from skills where name='${SKILL_NAME}';`, `the skill "${SKILL_NAME}" (${where} → Skills → the bin icon on its row)`],
  ];
  for (const [sql, what] of wants) {
    if (dbCount(sql) > 0) {
      throw new Error(`A previous take of 2-bundle left ${what}. Delete it before re-recording, or two will be on camera.`);
    }
  }
}

// --- 1. the channel's page ------------------------------------------------------

const scope: Guide = {
  id: "1-scope",
  title: TITLE,
  before: async () => {
    assertDeepDiveState();
  },
  scenes: [
    {
      chapter: "Workspace and channels",
      say: `The console, on Workspaces. One account, the Slack workspace connected to it, and under that the channels the bot is in. Access resolves top down: what is attached to the workspace reaches every channel, and a channel adds to it.`,
      act: async (s) => {
        await s.app.goto("/workspaces");
        await s.wait(1_400);
        // Rail entries are buttons, not links (openChannelPage in shared.ts).
        // The account row's sub-line reads "all workspaces"; a Slack workspace's
        // reads its slack.com domain; a channel's its C… id.
        await pointIfPresent(s, s.page.getByRole("button", { name: /all workspaces/i }));
        await s.wait(700);
        await pointIfPresent(s, s.page.getByRole("button", { name: /\.slack\.com/i }));
        await s.wait(700);
        await pointIfPresent(s, s.page.getByRole("button", { name: new RegExp(`^${channel()}`, "i") }));
      },
    },
    {
      chapter: "One channel's access",
      say: `Open the channel. Under Access: the Engineering bundle, attached here, with everything in it. Under Repositories: the repository connected to this channel directly, and nowhere else.`,
      act: async (s) => {
        await openChannelPage(s);
        // The attached bundle is a chip reading its name (attached-chip.tsx).
        await pointIfPresent(s, s.page.getByText(BUNDLE, { exact: true }));
        await s.wait(900);
        // The repository row shows owner/name with an "attached here" chip
        // (scope-detail.tsx, Connected repositories).
        await pointIfPresent(s, s.page.getByText(githubRepo(), { exact: true }));
        await s.wait(600);
      },
    },
    {
      say: `The access summary resolves it. Each host, the connection that reaches it, the credential type, the writes policy — confirm, so a write waits for a person — and where the grant came from.`,
      act: async (s) => {
        // A Disclosure: a button carrying its label and a count (disclosure.tsx).
        await s.app.click(s.page.getByRole("button", { name: /^access summary/i }), { settle: 1_000 });
        await pointIfPresent(s, s.page.getByRole("columnheader", { name: /^writes$/i }));
        await s.wait(800);
      },
    },
    {
      chapter: "Instructions",
      say: `Instructions for this channel alone, added on top of what comes down from the workspace and from every attached bundle. Type one and save it. The next message here reads it.`,
      act: async (s) => {
        await retype(s, 'textarea[aria-label="Custom instructions"]', CHANNEL_INSTRUCTION);
        await s.wait(400);
        // Disabled when the text equals what is saved — a re-take — and
        // clickIfPresent treats disabled as absent. The assert below reads the
        // row either way, so the scene cannot narrate a save that did not land.
        await s.app.clickIfPresent(s.page.getByRole("button", { name: /^save instructions$/i }), { settle: 1_400 });
        assertDb(
          `select count(*) from scopes where kind='channel' and instructions like '%${CHANNEL_INSTRUCTION.replace(/'/g, "''")}%';`,
          1,
          "The channel's instructions were not saved",
        );
        // What it inherits, when there is anything to inherit: a Disclosure
        // that exists only then (scope-detail.tsx, "Also in force here").
        await s.app.clickIfPresent(s.page.getByRole("button", { name: /^also in force here/i }).first(), { settle: 900 });
      },
    },
    {
      chapter: "How it behaves",
      say: `Behaviour. The default model for this channel: any model the endpoint serves, or the global one from Settings.`,
      act: async (s) => {
        await s.app.point(s.page.getByRole("combobox", { name: /^default model$/i }));
        await s.wait(800);
      },
    },
    {
      say: `Read every message. Off, it answers only when mentioned. On, a classifier reads each message against the instructions and decides: answer, react, or say nothing. Each one costs a small call, so it stays off until someone asks.`,
      act: async (s) => {
        await s.app.point(s.page.getByRole("combobox", { name: /^read every message$/i }));
        await s.wait(1_000);
      },
    },
    {
      say: `Member edits: whether channel members may change its instructions and memory from Slack. And a monthly budget for this channel alone. Zero means no cap of its own; the workspace and account budgets still apply.`,
      act: async (s) => {
        await s.app.point(s.page.getByRole("combobox", { name: /^member edits$/i }));
        await s.wait(1_200);
        await s.app.point(s.page.locator('input[aria-label="Monthly budget in USD"]'));
        await s.wait(800);
      },
    },
  ],
};

// --- 2. inside the bundle -------------------------------------------------------

const bundle: Guide = {
  id: "2-bundle",
  title: TITLE,
  before: async () => {
    assertDeepDiveState();
    assertNoLeftovers();
  },
  scenes: [
    {
      chapter: "Inside a bundle",
      say: `Bundles. Engineering holds one credential, ClickUp: the host it may call, the credential type, and when the bot last used it. The header says where it is used. One place: this channel.`,
      act: async (s) => {
        await s.app.goto("/bundles");
        await s.wait(1_200);
        const card = bundleCard(s);
        // Open or closed is remembered per browser (bundles-page.tsx); open it
        // if the profile left it closed, so the connection table is in frame.
        const header = card.locator("button[aria-expanded]").first();
        if ((await header.getAttribute("aria-expanded").catch(() => "true")) === "false") {
          await s.app.click(header, { settle: 800 });
        }
        await pointIfPresent(s, card.getByRole("row").filter({ hasText: /clickup/i }));
        await s.wait(900);
        await pointIfPresent(s, card.getByText(/used in|not attached/i));
      },
    },
    {
      say: `Configure it. Domains are hosts the bot may read with no credential at all: documentation, a status page. Add ClickUp's docs.`,
      act: async (s) => {
        await s.app.click(bundleCard(s).getByRole("button", { name: /^configure$/i }), { settle: 1_000 });
        await s.app.click(editor(s).getByRole("tab", { name: /^domains$/i }), { settle: 800 });
        // #domain-host, and the form's own "Add" (bundle-edit-dialog.tsx, DomainsTab).
        await s.app.typeVerified("#domain-host", DOMAIN);
        await s.app.click(editor(s).getByRole("button", { name: /^add$/i }), { settle: 1_400 });
        assertDb(`select count(*) from domains where host='${DOMAIN}';`, 1, "The domain was not added");
      },
    },
    {
      say: `Instructions travel with the bundle into every channel it is attached to, on top of the channel's own.`,
      act: async (s) => {
        await s.app.click(editor(s).getByRole("tab", { name: /^instructions$/i }), { settle: 800 });
        await retype(s, "#bundle-instructions", BUNDLE_INSTRUCTION);
        await s.wait(300);
        // Disabled when nothing changed (a re-take); the assert reads the row either way.
        await s.app.clickIfPresent(editor(s).getByRole("button", { name: /^save instructions$/i }), { settle: 1_400 });
        assertDb(
          `select count(*) from bundles where name='${BUNDLE}' and instructions like '%${BUNDLE_INSTRUCTION.replace(/'/g, "''")}%';`,
          1,
          "The bundle's instructions were not saved",
        );
      },
    },
    {
      chapter: "Skills",
      say: `Skills are Markdown the model reads wherever this bundle applies: how to use a tool well, house conventions, a runbook. Add one: the conventions for a ticket.`,
      act: async (s) => {
        await s.app.click(editor(s).getByRole("tab", { name: /^skills$/i }), { settle: 800 });
        await s.app.click(editor(s).getByRole("button", { name: /^add skill$/i }), { settle: 900 });
        // The skill dialog opens above the editor (skill-dialog.tsx: #skill-name,
        // #skill-content, a submit reading "Save"). Newlines in the content are
        // typed as Enter, which a textarea keeps as newlines.
        await s.app.typeVerified("#skill-name", SKILL_NAME);
        await s.app.typeVerified("#skill-content", SKILL_CONTENT, 22);
        await s.app.click(s.page.getByRole("dialog").last().getByRole("button", { name: /^save$/i }), { settle: 1_400 });
        assertDb(`select count(*) from skills where name='${SKILL_NAME}';`, 1, "The skill was not saved");
      },
    },
    {
      say: `It is appended to the prompt in every scope the bundle covers, capped, so a long skill file cannot crowd out the thread.`,
      act: async (s) => {
        await pointIfPresent(s, editor(s).getByText(SKILL_NAME, { exact: true }));
        await s.wait(1_200);
      },
    },
    {
      say: `Back on the card: one credential, one domain, instructions, one skill. Attached in one place.`,
      act: async (s) => {
        await s.page.keyboard.press("Escape").catch(() => {});
        await s.wait(900);
        // The header's summary line counts what the bundle holds (bundle-card.tsx, summarize).
        await pointIfPresent(s, bundleCard(s).getByText(/credential/));
        await s.wait(900);
      },
    },
    {
      say: `A credential lives in a bundle. A bundle is attached to the workspace, or to one channel. That is the whole of what a channel may reach.`,
      act: async (s) => {
        await s.wait(1_500);
      },
    },
  ],
};

export const REACH: LongForm = {
  id: "reach",
  title: TITLE,
  cardChapters: [
    "Workspace and channels",
    "One channel's access",
    "Instructions",
    "How it behaves",
    "Inside a bundle",
    "Skills",
  ],
  partsDir,
  outDir,
  parts: [scope, bundle],
  // The access summary table, open: hosts, connections, the writes policy and
  // where each grant came from. The frame that says what the title says.
  poster: { chapter: "One channel's access", offset: 14 },
};
