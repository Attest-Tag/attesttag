
import type { Guide, LongForm } from "../types";
import {
  BUNDLE,
  assertDb,
  assertWorkspaceConnected,
  bot,
  channel,
  clearEmailAllowlist,
  clickupRow,
  confirmEmailInDatabase,
  githubRepo,
  githubToken,
  clickupToken,
  jobCount,
  openChannelPage,
  outDir,
  partsDirFor,
  signupEmail,
  signupName,
  signupOrg,
  signupPassword,
  siteUrl,
  waitForJob,
  waitSignedIn,
} from "./shared";

// "From an empty account to a pull request" — the walkthrough.
//
// Recorded in ten parts and stitched. A twenty-minute unbroken take means one
// browser session surviving twenty minutes of Slack and one model call at
// minute nineteen being able to throw the whole thing away; parts make that
// cost one two-minute take.
//
// The parts are a dependency chain:
//
//   00-website   the site; needs nothing
//   0-signup     founds the organisation and connects the workspace
//   1-invite     the first answer
//   2-thread     reads the seeded conversation
//   3-connect    repository on the channel, bundle for the tracker, both attached
//   4-reach      proves the repository connection reads
//   5-hold       the small write: a ticket, held for a person
//   6-fix        the large write: a brief, a container, a DRAFT pull request
//   7-memory     needs something to have happened worth remembering
//   8-record     the ledger of everything above, so it films last
//
// 0-signup cannot be re-recorded alone: it founds a NEW organisation and the
// workspace cannot move between organisations (SaveTeam refuses). Re-record it
// from an empty database — FRESH_DB=1 ./start_dev.sh — and everything after it.
//
// Two rules this file obeys:
//
//   A scene holds for exactly as long as its line takes to speak; `act` must
//   finish faster than the line, except where it waits on the bot. The bot
//   round trip measured 23s, so bot-wait lines are written to about that.
//
//   Nothing here asserts on what the model said. `ask` waits for the reply
//   to stop growing, which is a property of the transport.
//
// Narration is direct: short declarative sentences, present tense, second
// person. It says what is on screen and why it matters, and nothing else.

const partsDir = partsDirFor("deep-dive");
const TITLE = "From an empty account to a pull request";

// --- 00. the site -------------------------------------------------------------

const website: Guide = {
  id: "00-website",
  title: TITLE,
  scenes: [
    {
      chapter: "What it does",
      say: `attest_tag is an AI teammate for Slack. Mention it in a channel. It reads the thread, searches your documents, and calls the services you connect.`,
      act: async (s) => {
        await s.page.goto(siteUrl(), { waitUntil: "domcontentloaded" });
        await s.wait(1_500);
      },
    },
    {
      say: `It runs on open-weight models through any OpenAI-compatible endpoint. Every answer shows the model, the tokens, and the cost.`,
      act: async (s) => {
        await s.app.scroll(720, 18);
        await s.wait(1_000);
      },
    },
    {
      say: `Reading is free. Writing is not. Anything that changes a system stops for a person first, and the record shows who approved it.`,
      act: async (s) => {
        await s.app.scroll(820, 18);
        await s.wait(1_000);
      },
    },
    {
      say: `Here is all of it, from an empty account to a pull request.`,
      act: async (s) => {
        await s.app.scroll(600, 16);
        await s.wait(800);
      },
    },
  ],
};

// --- 0. sign up ----------------------------------------------------------------

const signup: Guide = {
  id: "0-signup",
  title: TITLE,
  scenes: [
    {
      chapter: "Sign up",
      say: `Start from nothing. No account, no workspace connected.`,
      act: async (s) => {
        await s.app.goto("/signup");
      },
    },
    {
      say: `An organisation, a name, an email address, a password.`,
      act: async (s, ctx) => {
        await s.wait(1_200);
        const email = signupEmail();
        ctx.signupEmail = email;
        await s.app.typeVerified("#org", signupOrg());
        await s.app.typeVerified("#name", signupName());
        await s.app.typeVerified("#email", email);
        await s.app.typeVerified("#password", signupPassword(), 55);
      },
    },
    {
      say: `Sign-up starts the session immediately.`,
      act: async (s, ctx) => {
        await s.app.click('button[type="submit"]');
        await waitSignedIn(s);
        confirmEmailInDatabase(ctx.signupEmail);
        clearEmailAllowlist();
        await s.wait(1_200);
      },
    },
    {
      chapter: "Connect Slack",
      say: `The console is empty until a Slack workspace is connected.`,
      act: async (s) => {
        await s.app.goto("/onboarding");
        await s.wait(900);
      },
    },
    {
      say: `This is Slack's consent screen. It lists exactly what the app is asking for. The decision is made here.`,
      act: async (s) => {
        await s.app.click(s.page.getByRole("link", { name: /add to slack/i }));
        await s.page.waitForURL(/slack\.com/, { timeout: 45_000 });
        await s.wait(2_500);
      },
    },
    {
      say: `Allow it. One workspace connected, with the person who connected it on record.`,
      act: async (s) => {
        await s.app.clickIfPresent(s.page.getByRole("button", { name: /^allow/i }));
        await s.page.waitForURL((u) => !/slack\.com/.test(u.host), { timeout: 60_000 }).catch(() => {});
        await s.wait(2_500);
        assertWorkspaceConnected();
      },
    },
  ],
};

// --- 1. the first answer -------------------------------------------------------

const invite: Guide = {
  id: "1-invite",
  title: TITLE,
  scenes: [
    {
      chapter: "In Slack",
      say: `This is Slack. Everything from here happens inside it.`,
      act: async (s) => {
        await s.slack.open();
        await s.slack.openChannel(channel());
      },
    },
    {
      say: `An engineering channel with a week of conversation, and one extra member. You invite it like a person. That is the whole setup.`,
      act: async (s) => {
        await s.slack.scroll(-500, 14);
        await s.wait(900);
        await s.slack.scroll(500, 14);
        await s.wait(700);
      },
    },
    {
      chapter: "The first answer",
      // Bot-wait scene, written to the measured round trip.
      say: `Ask it something. The status line shows what it is doing: reading the channel, searching documents, calling a service. The answer streams in as it is produced. When it finishes, the footer shows the model, the tokens in and out, the cost, and a link to configure this channel. Every answer carries that line.`,
      act: async (s) => {
        await s.slack.ask(`@${bot()} what has this channel been arguing about this week?`);
      },
    },
  ],
};

// --- 2. the thread -------------------------------------------------------------

const thread: Guide = {
  id: "2-thread",
  title: TITLE,
  scenes: [
    {
      chapter: "It reads the whole thread",
      say: `The thread is the unit of conversation. A week of one channel: a decision argued, revised, and half settled.`,
      act: async (s) => {
        await s.slack.openChannel(channel());
        await s.wait(800);
        await s.slack.scroll(-900, 18);
        await s.wait(700);
        await s.slack.scroll(900, 18);
      },
    },
    {
      // Bot-wait scene.
      say: `Ask for something that needs all of it. Nothing was pasted or summarised first. It rebuilds the conversation from Slack on every turn, so an edit or a deletion is noticed instead of surviving in a stale copy.`,
      act: async (s) => {
        await s.slack.ask(`@${bot()} where did this land, and what is still open?`);
      },
    },
    {
      say: `It says where each point came from: the message, the person, the day.`,
      act: async (s) => {
        await s.wait(1_000);
        await s.slack.scroll(260);
      },
    },
  ],
};

// --- 3. connect ----------------------------------------------------------------

const connect: Guide = {
  id: "3-connect",
  title: TITLE,
  scenes: [
    {
      chapter: "Connect a repository",
      say: `To reach anything beyond Slack, open the console, on the page for this channel.`,
      act: async (s) => {
        await openChannelPage(s);
      },
    },
    {
      say: `A repository is attached to the channel directly. Paste a token once. It lists the repositories that token can actually see.`,
      act: async (s) => {
        if (!githubToken()) throw new Error("Set VIDEO_GITHUB_TOKEN in video/.env.");
        await s.app.click(s.page.getByRole("button", { name: /connect repo/i }).first());
        await s.wait(1_000);
        await s.app.typeVerified('input[placeholder="github_pat_…"]', githubToken(), 22);
        await s.wait(500);
        await s.app.click(s.page.getByRole("button", { name: /find repositories/i }).first());
        // The list arrives when GitHub answers; wait for it rather than for a
        // fixed beat, and scroll the one we want into view.
        await s.page.getByRole("option", { name: new RegExp(githubRepo(), "i") }).first()
          .waitFor({ state: "visible", timeout: 30_000 });
        await s.wait(800);
      },
    },
    {
      chapter: "The model never holds the key",
      say: `The token goes in and does not come out. The model never sees it. Requests go through a proxy that attaches the credential on the way past. The model knows the repository exists and that it may call it. Nothing more.`,
      act: async (s) => {
        await s.app.click(s.page.getByRole("option", { name: new RegExp(githubRepo(), "i") }).first());
        await s.wait(700);
        // The popover's confirm reads "Connect" on a channel page ("Save" on
        // the bundles page). Exact, so "Connect repo" on the page behind it
        // cannot match.
        await s.app.click(s.page.getByRole("button", { name: /^connect$/i }).last());
        await s.wait(2_500);
        assertDb("select count(*) from connections where coalesce(repo,'') != '';", 1, "The repository was not connected");
      },
    },
    {
      chapter: "A bundle for the tracker",
      say: `Other services are grouped into bundles: credentials, allowed hosts, and instructions, attached to a workspace or to one channel. One for the tracker.`,
      act: async (s) => {
        await s.app.goto("/bundles");
        await s.wait(1_000);
        // "Create bundle" is the empty state's button and exists only while the
        // page has nothing to list. Once a repository connection is saved the
        // page lists it, and only the header's "Create" remains.
        await s.app.click(s.page.getByRole("button", { name: /^create( bundle)?$/i }).first());
        await s.wait(900);
        // The header "Create" opens a menu of things to create; the empty
        // state's "Create bundle" opens the dialog directly. Take the menu
        // item when there is one, then wait for the dialog either way.
        await s.app.clickIfPresent(s.page.getByRole("menuitem", { name: /bundle/i }).first());
        await s.page.locator("#name-dialog-input").waitFor({ state: "visible", timeout: 15_000 });
        await s.app.typeVerified("#name-dialog-input", BUNDLE);
        await s.app.click(s.page.getByRole("button", { name: /^Create$/ }).first());
        await s.wait(1_600);
        assertDb("select count(*) from bundles;", 1, "The bundle was not created");
      },
    },
    {
      say: `Same proxy, same rule. It is tested before it is saved: one real call to the service.`,
      act: async (s) => {
        if (!clickupToken()) throw new Error("Set VIDEO_CLICKUP_TOKEN in video/.env.");
        await s.app.click(clickupRow(s), { settle: 1_200 });
        await s.app.typeVerified("#rec-secret", clickupToken(), 24);
        await s.wait(600);
        await s.app.clickIfPresent(s.page.getByRole("button", { name: /test connection/i }).first());
        await s.wait(3_500);
        await s.app.click(s.page.getByRole("button", { name: /^Connect$/ }).last());
        await s.wait(2_000);
        assertDb("select count(*) from connections where preset='clickup';", 1, "The ClickUp credential was not saved");
      },
    },
    {
      say: `Both attached to this one channel, and nowhere else. Allowed hosts, allowed methods, and a writes policy that starts at ask a person.`,
      act: async (s) => {
        await openChannelPage(s);
        // The Access section's "Add" opens a command palette listing bundles
        // and connections; picking one attaches it.
        await s.app.click(s.page.getByRole("button", { name: /^add$/i }).first());
        await s.wait(900);
        await s.app.typeVerified('input[placeholder="Search bundles and connections…"]', BUNDLE);
        await s.wait(700);
        await s.app.click(s.page.getByRole("option", { name: new RegExp(BUNDLE, "i") }).first());
        await s.wait(2_000);
        assertDb("select count(*) from scope_bundles;", 1, "The bundle was not attached to the channel");
        await s.app.scroll(240);
        await s.wait(900);
      },
    },
  ],
};

// --- 4. reach ------------------------------------------------------------------

const reach: Guide = {
  id: "4-reach",
  title: TITLE,
  scenes: [
    {
      chapter: "It reaches the repository",
      // Bot-wait scene.
      say: `Back in the channel. Nothing has changed except what it can reach. Watch the status line: it is calling the repository. One action you allowed, on one host you named. The answer comes from the repository, not from the model's memory of one.`,
      act: async (s) => {
        await s.slack.ask(`@${bot()} what changed in ${githubRepo()} since Monday, and is anything still unreviewed?`);
      },
    },
    {
      say: `A credential the channel holds. A model that may use it and cannot see it.`,
      act: async (s) => {
        await s.wait(1_500);
      },
    },
  ],
};

// --- 5. the hold ---------------------------------------------------------------

const hold: Guide = {
  id: "5-hold",
  title: TITLE,
  scenes: [
    {
      chapter: "A write stops for a person",
      // Bot-wait scene.
      say: `Reading is the easy half. Earlier, someone said the reconciliation change needed a ticket, and nobody raised it. Ask for it.`,
      act: async (s) => {
        await s.slack.ask(`@${bot()} raise a ticket for the reconciliation change we said we needed, with the context from this thread.`);
      },
    },
    {
      say: `It stops. The card shows the exact request: the method, the URL, the body, with the title and description it wrote from the thread. Nothing has happened yet.`,
      act: async (s) => {
        await s.wait(2_000);
      },
    },
    {
      say: `Who may confirm is set in the console. The person who confirms is recorded next to what they approved.`,
      act: async (s) => {
        await s.slack.clickCardButton(/confirm|approve/i);
        await s.wait(2_500);
      },
    },
    {
      say: `It runs, and posts the link. The model proposed. A person decided. The record has both.`,
      act: async (s) => {
        await s.wait(2_000);
      },
    },
  ],
};

// --- 6. the fix job ------------------------------------------------------------

const fix: Guide = {
  id: "6-fix",
  title: TITLE,
  scenes: [
    {
      chapter: "Fix it, and raise a PR",
      // Bot-wait scene.
      say: `The large write. The thread already knows what is wrong with the retry path. Ask for a fix and a pull request.`,
      act: async (s) => {
        // The reply is the held card, not an answer, and the fix-job card
        // carries no reply footer (the ClickUp card does). Stable is done.
        await s.slack.ask(FIX_QUESTION(), { requireFooter: false, settleMs: 5_000 });
      },
    },
    {
      say: `It does not run code itself. It writes a brief: what to change, the evidence from this thread, and what done looks like. The brief is held behind a card that states the repository, the base branch, and the rule the worker runs under.`,
      act: async (s) => {
        await s.wait(2_500);
      },
    },
    {
      chapter: "What the worker may do",
      say: `One new branch. One draft pull request. It never merges, and it never pushes to the base branch. That is what the worker is able to do, not an instruction a model could talk itself out of.`,
      act: async (s) => {
        await s.wait(2_500);
      },
    },
    {
      say: `Confirm. A separate container clones the repository, makes the change, runs the tests, and pushes a branch. The thread shows each stage. It takes as long as your tests take.`,
      // The job takes minutes. Holding the shot for that is dead air, so the
      // part ends here and the next one opens on the result — the wait
      // happens between parts, off camera, and the narration already says it
      // takes as long as it takes.
      act: async (s) => {
        // Prove the click reached the bot. A button press is a Slack
        // interaction event; one take lost it to a slow acknowledgement
        // ("Slack redelivery … reason=http_error") and the part ended on a
        // narration of a job that never started. Verify a job row appeared,
        // and press once more if it did not.
        const before = jobCount();
        await s.slack.clickCardButton(/confirm|approve|start/i);
        for (let attempt = 0; ; attempt++) {
          const deadline = Date.now() + 20_000;
          while (Date.now() < deadline && jobCount() <= before) await s.wait(1_000);
          if (jobCount() > before) break;
          if (attempt >= 1) throw new Error("Confirm was pressed twice and no fix job was dispatched.");
          await s.slack.clickCardButton(/confirm|approve|start/i);
        }
        await s.wait(3_000);
      },
    },
  ],
};

// --- 7. memory -----------------------------------------------------------------

const FIX_QUESTION = () => `@${bot()} fix the exhausted-budget path so it queues instead of failing closed, and raise a PR.`;

const memory: Guide = {
  id: "7-memory",
  title: TITLE,
  // The fix job confirmed at the end of 6-fix takes minutes. Wait for it here,
  // before a line is synthesised, so the opener shows a finished result.
  before: () => waitForJob(),
  scenes: [
    {
      chapter: "A draft pull request",
      // On the console's Jobs page, not in the Slack thread. The thread now
      // carries the worker's log, and Slack loads it slowly or not at all: one
      // take opened on a blank pane, another on "Couldn't load thread". The
      // job record shows the same result and is ours to render.
      say: `The result, on the Jobs page: the job, what it changed, what it cost, and a draft pull request waiting for a person to read it. Not merged. Not on the base branch.`,
      act: async (s) => {
        await s.app.goto("/jobs");
        await s.wait(1_400);
        await s.app.click(s.page.getByRole("row").filter({ hasText: /queue|exhausted|budget/i }).first(), { settle: 1_600 });
        await s.wait(1_200);
      },
    },
    {
      chapter: "Memory",
      // Bot-wait scene — the wait is off camera. Two takes of this held a
      // still frame for over a minute while Slack retried the delivery; the
      // acknowledgement itself takes the bot four seconds.
      say: `It can be told to remember. Conventions, decisions, the reason something is the way it is.`,
      act: async (s) => {
        const q = `@${bot()} remember that we deploy on Tuesdays and never on a Friday.`;
        // Coming from the console, Slack boots blank for seconds and then
        // restores whatever pane it last had open — the fix thread, which
        // could not load. Neither belongs on camera.
        await s.offCamera(async () => {
          await s.slack.openChannel(channel());
          await s.slack.closeThread();
        });
        await s.slack.send(q);
        await s.wait(1_200);
        await s.offCamera(async () => {
          await s.slack.awaitReply(q);
          await s.slack.closeThread();
        });
        await s.slack.openThreadOf(q);
        await s.wait(2_500);
      },
    },
    {
      say: `Every memory is in the console, with who said it and when. Any of them can be deleted.`,
      act: async (s) => {
        await s.app.goto("/memory");
        await s.wait(1_200);
      },
    },
    {
      say: `A fact learned in a public channel is shared. A fact learned in a private channel or a direct message stays there. Where it was said decides.`,
      act: async (s) => {
        await s.app.scroll(300);
        await s.wait(900);
      },
    },
  ],
};

// --- 8. the record -------------------------------------------------------------

const record: Guide = {
  id: "8-record",
  title: TITLE,
  scenes: [
    {
      chapter: "Which model",
      say: `Which model. Any model your endpoint serves. Change it here, and the next message uses it. Nothing to redeploy.`,
      act: async (s) => {
        await s.app.goto("/settings");
        await s.wait(900);
        await s.app.clickIfPresent(s.page.getByRole("tab", { name: /models/i }));
        await s.wait(900);
        await s.app.clickIfPresent(s.page.getByRole("combobox").first());
        await s.wait(1_600);
        await s.page.keyboard.press("Escape").catch(() => {});
        await s.wait(500);
      },
    },
    {
      chapter: "Who approved what",
      say: `Everything you have watched is in one place. Every question, every tool call, every write and who confirmed it, every token and its cost.`,
      act: async (s) => {
        await s.app.goto("/activity");
        await s.wait(1_400);
      },
    },
    {
      say: `Budgets per workspace and per channel. When one is spent, the bot stops.`,
      act: async (s) => {
        await s.app.scroll(360);
        await s.wait(1_000);
      },
    },
    {
      say: `It reads what the team reads. It reaches what you allow. It never holds the key. It stops before it changes anything.`,
      act: async (s) => {
        await s.app.goto("/");
        await s.wait(1_500);
      },
    },
  ],
};

export const DEEP_DIVE: LongForm = {
  id: "deep-dive",
  title: TITLE,
  cardChapters: [
    "What it does",
    "Sign up and connect Slack",
    "The first answer",
    "Connect a repository",
    "A write stops for a person",
    "Fix it, and raise a PR",
    "A draft pull request",
    "Who approved what",
  ],
  partsDir,
  outDir,
  parts: [website, signup, invite, thread, connect, reach, hold, fix, memory, record],
  // The memory thread, answered: a short reply with its footer, in a channel
  // with a conversation in it. The most legible frame of the product doing
  // the thing.
  poster: { chapter: "Memory", offset: 12 },
};
