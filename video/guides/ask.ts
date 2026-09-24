import type { Guide, LongForm, Surfaces } from "../types";
import { SEL } from "../slack";
import {
  assertDb,
  assertWorkspaceState,
  bot,
  channel,
  dbCount,
  outDir,
  partsDirFor,
  postCommand,
  showReply,
  watchAnswer,
} from "./shared";

// "Ask it something" — the walkthrough.
//
// Slack only, but for one crossing to the console's Artifacts page at the end.
// Recorded in the organisation the deep dive founded, in the seeded channel,
// and it needs nothing the deep dive did not leave behind: no connection, no
// bundle, no env var beyond VIDEO_CHANNEL / VIDEO_BOT_NAME. Nothing here
// creates a row that a later scene would have to delete, apart from the
// messages themselves (`npm run tidy` between takes) and one artifact row the
// console scene is about — a re-take adds another to the Artifacts table, and
// the newest is the one on camera either way.
//
//   1-mention   four answers: the first, a sum, a search, the web
//   2-control   the bang commands, a file, and the Artifacts page
//
// Every bot round trip is two scenes. Slack's first delivery of an event to
// this bot fails more often than not through the tunnel it is recorded
// behind, and the retry lands anywhere from four seconds to seventy later
// (LEARNINGS.md) — none of which is the product. So the question is posted on
// camera under one line, and the next scene OPENS with a cut that waits for
// the reply to exist; the line then begins on the frame the thread is about
// to open. For a model answer the cut ends the moment the bot's reply message
// exists, and the rest — the status line, the text streaming in, the footer —
// is the product's own time and stays on camera. For a bang command, which
// is posted whole with no model behind it, the whole round trip is in the cut
// and the thread opens on the finished reply.
//
// Nothing here asserts on what the model wrote. Where a line claims that a
// tool ran — the sandbox, the file — the database is asked for the tool-call
// or artifact row afterwards, and the take stops if it is not there: a video
// narrating a calculation that never happened is worse than no video.

const partsDir = partsDirFor("ask");
const TITLE = "Ask it something";

// The questions, built when used: bot() reads the environment when called.
// Each is answerable from the seeded week in the channel (brief: messages
// 2, 3, 6 and 8) or, for the web one, from anywhere.
const Q_DECIDED = () => `@${bot()} what did we decide about retries this week, in three bullets?`;
// Four figures and two steps each, and the request says to work it out in
// JavaScript: the first take asked without saying so and the model did the
// sums inline — correctly — so the scene's assert stopped the take. The
// narration ("it does not do arithmetic in its head") describes what happens
// when it is asked to compute, which is what the line now asks for.
const Q_NUMBER = () => `@${bot()} 0.4% of checkout calls timed out. At 225,000 checkouts a day, how many is that per day, per hour and per minute, and over a two-day weekend? Work it out in JavaScript rather than in your head.`;
const Q_WHO = () => `@${bot()} who first suggested capping total retry time instead of retry count, and when?`;
// "search the web" is one of the phrases forcedTool() (agent.go) turns into
// a required web_search on the first round, so the status line will show it.
const Q_WEB = () => `@${bot()} search the web for Slack's API rate limit tiers and give me the link.`;
// Not "for the runbook": "runbook" is in wantDocsRe (agent.go) and would force
// a docs search on the first round before the file is written.
const Q_FILE = () => `@${bot()} write the retry decision up as a short markdown file for the wiki.`;

// ---------------------------------------------------------------------------
// The round trip itself — postCommand, watchAnswer, showReply — is in
// shared.ts; the header comment above says why it is two scenes.

/** The newest answer the bot has posted — the row AddTurn writes the moment an answer is posted (agent.go). */
const lastAnswerId = () => dbCount("select coalesce(max(id),0) from turns where role='assistant';");

/**
 * Hold until the bot has posted an answer newer than `after`, asking the
 * database rather than the thread. For the file scene only: create_artifact
 * uploads the file into the thread as its own message while the answer is
 * still streaming (artifacts.go → uploadContent), and the recorder's
 * "newest bot row" reading has not been proven against a file row sitting
 * under a stream. The turns row is written once, when the answer is up.
 */
// shared candidate
async function waitForAnswerAfter(s: Surfaces, after: number, timeoutMs = 180_000): Promise<void> {
  const started = Date.now();
  while (lastAnswerId() <= after) {
    if (Date.now() - started > timeoutMs) {
      throw new Error(
        `The bot posted no answer in ${Math.round(timeoutMs / 1000)}s. Check the message on ` +
          `screen carries a highlighted @${bot()}, and bot.log for a turn line.`,
      );
    }
    await s.wait(1_000);
  }
}

// --- 1. mention ----------------------------------------------------------------

const mention: Guide = {
  id: "1-mention",
  title: TITLE,
  before: async () => {
    assertWorkspaceState();
  },
  scenes: [
    {
      chapter: "Mention it",
      say: `This is Slack, and a channel with a week of conversation in it. The bot is a member, invited like a person. Mention it in any channel it is in.`,
      act: async (s) => {
        // Slack paints a blank frame for seconds while it boots, and then
        // restores whatever thread pane it last had open — the first take
        // opened on a "Couldn't load thread" pane beside the channel. Both
        // belong inside the cut; the line starts on the channel alone.
        await s.offCamera(async () => {
          await s.slack.open();
          await s.slack.openChannel(channel());
          await s.slack.closeThread();
        });
        await s.slack.scroll(-500, 14);
        await s.wait(700);
        await s.slack.scroll(500, 14);
      },
    },
    {
      say: `Ask it something the channel has already argued about: what did we decide about retries this week, in three bullets.`,
      act: async (s) => {
        await s.slack.send(Q_DECIDED());
      },
    },
    {
      // Bot-wait scene, on camera: the streaming is the subject. Written
      // long so the shot is not silent while it works.
      say: `The status line shows what it is doing: reading the channel, searching, running a tool. The answer streams in as it is produced, in a thread on your message. When it finishes, the footer shows the model, the tokens in and out, the cost, and a link to configure this channel.`,
      act: async (s) => {
        await watchAnswer(s, Q_DECIDED());
      },
    },
    {
      chapter: "The footer",
      // A held shot on the answer. The footer is a context block ending in a
      // "Configure" link — the same word slack.ts's lastBotHasFooter looks
      // for — so the pointer is sent to that link in the newest row that has
      // one. Pointing is cosmetic: if the link is not on screen the shot
      // holds on the answer and says so in the log, rather than dying.
      say: `Model, tokens in and out, what it cost, and a link to configure this channel. Every answer carries that line.`,
      act: async (s) => {
        const link = s.page
          .locator(SEL.message)
          .filter({ hasText: /\bConfigure\b/ })
          .last()
          .getByRole("link", { name: /configure/i })
          .last();
        if (await link.isVisible().catch(() => false)) await s.slack.point(link);
        else console.warn("      ! no Configure link on screen to point at; holding on the answer");
        await s.wait(1_200);
      },
    },
    {
      chapter: "A number, worked out",
      say: `Now a number the thread does not contain. Four tenths of a percent of checkout calls timed out. At two hundred and twenty-five thousand checkouts a day, how many is that? Work it out in code.`,
      act: async (s, ctx) => {
        // Where the tool log stands before the question, so the next scene
        // can prove the calculation ran rather than narrate that it did.
        ctx.toolCallsBefore = String(dbCount("select coalesce(max(id),0) from tool_calls;"));
        await s.slack.send(Q_NUMBER());
      },
    },
    {
      // Bot-wait scene, on camera.
      say: `It does not do arithmetic in its head. The status line says running a calculation: the sum runs as code in a sealed sandbox — no filesystem, no credentials, ten seconds — and the result comes back into the answer. A computed figure, not a plausible one, and the code that produced it is on record.`,
      act: async (s, ctx) => {
        await watchAnswer(s, Q_NUMBER());
        // The line says a calculation ran. Only the tool log can say so —
        // the model may state a sum instead — and a take that narrated a
        // sandbox over an answer done in its head would be a lie on camera.
        assertDb(
          `select count(*) from tool_calls where name='run_js' and id > ${ctx.toolCallsBefore};`,
          1,
          "The answer was worked out without run_js, and the narration says it ran a calculation — re-take (the model decides; a harder sum makes it likelier)",
        );
      },
    },
    {
      chapter: "Search the workspace",
      say: `Something a summary would lose: who first suggested capping total retry time instead of retry count, and when.`,
      act: async (s) => {
        await s.slack.send(Q_WHO());
      },
    },
    {
      // Bot-wait scene, on camera. Whether it searches (slack_search) or
      // reads the channel (read_channel_history) is the model's call —
      // tools_slack.go — so the line allows both and claims neither.
      say: `It goes back to the source: searching the workspace, or reading the channel, and either way only what the person asking can already see. A private channel you are not in stays closed. The answer points at where that was said: the message, the person, the day.`,
      act: async (s) => {
        await watchAnswer(s, Q_WHO());
      },
    },
    {
      chapter: "On the web",
      say: `It can reach the public web too. Say search the web, and ask for the link.`,
      act: async (s) => {
        await s.slack.send(Q_WEB());
      },
    },
    {
      // Bot-wait scene, on camera. The search is forced by the wording
      // (forcedTool, agent.go); what it returns is the web's, and the line
      // says nothing about it.
      say: `Saying search the web makes the search compulsory, and the status line shows it. Asking for the link is the habit worth keeping: an answer from the web should point at its source, so it can be checked rather than trusted.`,
      act: async (s) => {
        await watchAnswer(s, Q_WEB());
      },
    },
  ],
};

// --- 2. control ----------------------------------------------------------------

const control: Guide = {
  id: "2-control",
  title: TITLE,
  scenes: [
    {
      chapter: "The commands",
      say: `Some things need no model at all: a handful of commands, answered as plain text, spending nothing. Ask for help.`,
      act: async (s, ctx) => {
        // A fresh part opens on a title card; Slack boots blank behind it and
        // restores whatever thread pane it last had open. Neither belongs on
        // camera, so the cut opens the scene.
        await s.offCamera(async () => {
          await s.slack.openChannel(channel());
          await s.slack.closeThread();
        });
        ctx.help = await postCommand(s, "!help");
      },
    },
    {
      // The reply is the list in commands.go's !help arm, verbatim.
      say: `The list: restart, stop, mute and unmute, a model override for one thread, memory, notes, routines, jobs, usage. All of it works in a direct message too.`,
      act: async (s, ctx) => {
        await showReply(s, ctx.help);
      },
    },
    {
      chapter: "Mute, restart",
      say: `A thread it should stay out of: mute it, and it stops replying there until it is asked back.`,
      act: async (s, ctx) => {
        ctx.mute = await postCommand(s, "!mute");
      },
    },
    {
      // "Muted in this thread. `!unmute` to bring me back." (commands.go).
      // A muted session skips the model and nothing else (bot.go: the muted
      // check sits after the command dispatch), which is what the line says.
      say: `Muted in this thread, it says, with the word that brings it back. Commands still work in a muted thread; only the answers stop. Unmute it.`,
      act: async (s, ctx) => {
        await showReply(s, ctx.mute);
        ctx.unmute = await postCommand(s, "!unmute");
      },
    },
    {
      // "Unmuted." (commands.go).
      say: `Unmuted, in one word. Restart is the other one to know: it drops what this thread has built up and starts fresh from the next message.`,
      act: async (s, ctx) => {
        await showReply(s, ctx.unmute);
        ctx.restart = await postCommand(s, "!restart");
      },
    },
    {
      // "Fresh start. I'll only look at messages from here on." (commands.go:
      // the session is archived and a new one started on the same thread).
      say: `Fresh start, it says: from here on it only reads what is posted after this. The thread stays where it is.`,
      act: async (s, ctx) => {
        await showReply(s, ctx.restart);
      },
    },
    {
      chapter: "As a file",
      say: `Some answers are better as a file than as a message. Ask for the retry decision written up as a short markdown file.`,
      act: async (s, ctx) => {
        ctx.answerBefore = String(lastAnswerId());
        ctx.artifactsBefore = String(dbCount("select coalesce(max(id),0) from artifacts;"));
        await s.slack.send(Q_FILE());
      },
    },
    {
      // create_artifact (artifacts.go) writes the file, uploads it into this
      // thread, records it, and tells the model to say in one line what it
      // holds rather than repeat it. The wait is on the database — see
      // waitForAnswerAfter — and the thread opens on the finished result.
      say: `It writes the whole file itself and posts it in the thread. The file is the answer; the reply only points at it. Every file it makes is also kept in the console.`,
      act: async (s, ctx) => {
        await s.offCamera(async () => {
          await waitForAnswerAfter(s, Number(ctx.answerBefore));
          // The line says a file was posted. The artifact row is written by
          // the tool that posted it (artifacts.go, AddArtifact).
          assertDb(
            `select count(*) from artifacts where id > ${ctx.artifactsBefore};`,
            1,
            "No file was made — the model answered in text instead of calling create_artifact; re-take",
          );
        });
        await s.slack.openThreadOf(Q_FILE());
        await s.wait(2_000);
      },
    },
    {
      chapter: "Every file, in the console",
      // ui/src/components/artifacts/artifacts-page.tsx: one table, newest
      // first (store.go: order by id desc), columns Made · Title · Format ·
      // Channel · Asked by · Size, a row that expands to its contents, and an
      // "Open in Slack" link beside each row that has a permalink.
      say: `Every file it has made, in one table: when, the title, the format, the channel, who asked, the size. Open a row for the contents; the link beside it opens the thread it was posted in.`,
      act: async (s) => {
        await s.app.goto("/artifacts");
        await s.wait(1_400);
        // Row 0 is the header; row 1 is the newest artifact — the file the
        // previous scene proved exists.
        await s.app.click(s.page.getByRole("row").nth(1), { settle: 1_600 });
        await s.wait(1_200);
        const open = s.page.getByRole("row").nth(1).getByRole("link", { name: /open in slack/i });
        if (await open.isVisible().catch(() => false)) await s.app.point(open);
        await s.wait(800);
      },
    },
  ],
};

export const ASK: LongForm = {
  id: "ask",
  title: TITLE,
  cardChapters: [
    "Mention it",
    "The footer",
    "A number, worked out",
    "Search the workspace",
    "On the web",
    "The commands",
    "As a file",
    "Every file, in the console",
  ],
  partsDir,
  outDir,
  parts: [mention, control],
  // The first answer, finished, with the pointer on its footer: the one
  // frame guaranteed to hold a complete answer whatever the round trip took,
  // since the chapter only begins once the answer has settled.
  poster: { chapter: "The footer", offset: 3 },
};
