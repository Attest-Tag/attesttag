import path from "node:path";
import type { RecutLongForm, RecutPart, RecutScene } from "../recut";
import { DEEP_DIVE } from "../guides/deep-dive";

// The deep dive, re-cut from the take of 2026-09-12.
//
// That take is in `.video-work/<part>/*.webm`, one raw recording per part, and
// it filmed everything the product did — the trouble was what it filmed around
// it. Slack's delivery through the tunnel meant the recorder sat on a still
// frame for up to a minute after every question while the line describing the
// answer ran out (LEARNINGS.md, "Slack's first delivery fails"); the client
// restored a thread pane that could not load and kept it on screen for a
// minute at a time; and it booted onto an archived channel before finding the
// right one. None of that is the product, and none of it is here.
//
// Every number below is a second in the raw footage, read off the frames:
// pull a contact sheet (`ffmpeg -i <webm> -vf fps=1,tile=5x20`) before moving
// one. The narration is written to what is on screen at that second.
//
// Every part but the first opens on a card naming what it is about; the take's
// own seam frames (the bare mark) are not used.

const partsDir = DEEP_DIVE.partsDir;
const raw = (part: string, file: string) => path.join(".video-work", part, file);

/** What the next part is about, before it starts. */
const card = (title: string, blurb: string): RecutScene => ({ card: { title, blurb } });

/**
 * A typing shot with the thread pane cropped out. The client restored a pane
 * reading "Couldn't load thread" beside the composer in two parts; the
 * composer, the last messages and the sidebar are all in the left two thirds
 * of the frame, so that is the frame for those seconds. 960×600 at 1440×900 is
 * the same 16:10, scaled back up by half again.
 */
const composer = { x: 0, y: 300, w: 960, h: 600 };

// --- 00. the site --------------------------------------------------------------

const website: RecutPart = {
  id: "00-website",
  source: raw("00-website", "page@111189aca66c3bd93545ab935faea64c.webm"),
  scenes: [
    // The title card, with the chapter list, is in the footage: 0 to 2.6.
    { at: 0, until: 2.6 },
    {
      at: 2.6,
      until: 12.6,
      rest: "keep",
      chapter: "What it does",
      say: `attest_tag is an AI teammate for Slack. Mention it in a channel. It reads the thread, searches your documents, and calls the services you connect.`,
    },
    {
      // The page scrolls to the product shot at 12.6.
      at: 12.6,
      until: 22.1,
      rest: "keep",
      say: `It runs on open-weight models through any OpenAI-compatible endpoint. Every answer shows the model, the tokens, and the cost.`,
    },
    {
      // Scrolled to the feature grid at 22.1; held on it for the longer line.
      at: 22.1,
      until: 33.3,
      say: `Reading is pre-approved. Writing needs approval: anything that changes a system stops for a person first, and the record shows who approved it. You can add a rule to bypass that as needed.`,
    },
    {
      at: 33.3,
      rest: "keep",
      say: `Here is all of it, from an empty account to a pull request.`,
    },
  ],
};

// --- 0. sign up ----------------------------------------------------------------

const signup: RecutPart = {
  id: "0-signup",
  source: raw("0-signup", "page@9e0d5674caa7737e2f91f32f6f747cc2.webm"),
  scenes: [
    card("Sign up, and connect Slack", "An empty account, one workspace, and the person who connected it on record."),
    {
      // The sign-up form, from 1.5; typing starts at 6.
      at: 1.5,
      until: 6.0,
      chapter: "Sign up",
      say: `Start from nothing. No account, no workspace connected.`,
    },
    {
      // The four fields typed, 6 to 15.
      at: 6.0,
      until: 15.5,
      rest: "keep",
      say: `An organisation, a name, an email address, a password. That is the whole form.`,
    },
    {
      // Create organisation at 16; the onboarding page at 17.
      at: 15.5,
      until: 17.5,
      rest: "keep",
      say: `Sign-up starts the session immediately.`,
    },
    {
      // The onboarding page: connect a workspace, then invite the bot. Add to
      // Slack is pressed at 25.3.
      at: 17.5,
      until: 25.3,
      rest: "keep",
      chapter: "Connect Slack",
      say: `The console is empty until a Slack workspace is connected. One button starts the install.`,
    },
    {
      // Slack's consent screen from 26; Allow is pressed at 33.
      at: 25.3,
      until: 33.0,
      rest: "keep",
      say: `This is Slack's consent screen. It lists exactly what the app is asking for. The decision is made here.`,
    },
    {
      // Back on the onboarding page at 34: connected, next step lit.
      at: 33.0,
      say: `Allow it. One workspace connected, with the person who connected it on record.`,
    },
  ],
};

// --- 1. the first answer -------------------------------------------------------

const invite: RecutPart = {
  id: "1-invite",
  source: raw("1-invite", "page@fcd2ce1fbe3e02055bc12510f0b8adb9.webm"),
  scenes: [
    card("In Slack: the first answer", "Mention it in a channel it is in. It answers in a thread, and every answer says what it cost."),
    {
      // The channel, filtered sidebar, nothing else open — the only clean
      // frames before the broken pane appears at 8.6. Held on the last one.
      at: 5.3,
      until: 8.4,
      chapter: "In Slack",
      say: `This is Slack. An engineering channel with a week of conversation in it, and one extra member, invited like a person. Everything from here happens inside it.`,
    },
    {
      // Typing, 20.8 to 26.0; sent at 26.3; the message is in the channel by 26.5.
      at: 20.6,
      until: 26.9,
      zoom: composer,
      rest: "keep",
      say: `Ask it something. Mention it, and type the question: what has this channel been arguing about this week?`,
    },
    {
      // The reply bar was clicked at 28.5; the pane says "Loading thread" for a
      // beat and opens on the status line at 30. The status holds to 50.9.
      at: 29.2,
      until: 50.9,
      chapter: "The first answer",
      say: `It answers in a thread on your message. The status line shows what it is doing: reading the channel, searching your documents, calling a service.`,
    },
    {
      // Streams in 50.9 to 52.9, footer by 53.
      at: 50.9,
      say: `The answer streams in as it is produced. When it finishes, the footer shows the model, the tokens in and out, the cost, and a link to configure this channel. Every answer carries that line.`,
    },
  ],
};

// --- 2. the thread -------------------------------------------------------------

const thread: RecutPart = {
  id: "2-thread",
  source: raw("2-thread", "page@ece34f977d2de2eb1a83d3727f9676a6.webm"),
  scenes: [
    card("It reads the whole thread", "A week of conversation, rebuilt from Slack on every turn, with every point traced to who said it."),
    {
      // The channel with the first answer still open beside it; the scroll up
      // through the week and back runs 9 to 12.
      at: 8.0,
      until: 12.6,
      chapter: "It reads the whole thread",
      say: `The thread is the unit of conversation. A week of one channel: a decision argued, revised, and half settled. Nothing was pasted or summarised first.`,
    },
    {
      // Typing 12.7 to 16.5, sent at about 17.5.
      at: 12.6,
      until: 17.8,
      rest: "keep",
      say: `Ask for something that needs all of it: where did this land, and what is still open?`,
    },
    {
      // Bar at 19.5, thread open at 20.8, streaming 24 to 26.2.
      at: 19.3,
      until: 26.3,
      say: `It rebuilds the conversation from Slack on every turn, so an edit or a deletion is noticed instead of surviving in a stale copy.`,
    },
    {
      // The finished answer; the pane scrolls a little at 27.
      at: 26.3,
      say: `It says where each point came from: the message, the person, the day.`,
    },
  ],
};

// --- 3. connect ----------------------------------------------------------------

const connect: RecutPart = {
  id: "3-connect",
  source: raw("3-connect", "page@dc342e519caad3f8b73acbe4287afff6.webm"),
  scenes: [
    card("Connect a repository and a tracker", "Credentials go in through the console and never come out. The model may call the service and cannot see the key."),
    {
      // Workspaces page, then the click that opens the channel page at 5.5.
      at: 1.84,
      until: 8.2,
      rest: "keep",
      chapter: "Connect a repository",
      say: `To reach anything beyond Slack, open the console, on the page for this channel.`,
    },
    {
      // Connect repo at 9, the token typed 10 to 12, Find repositories at 12,
      // the list at 13.
      at: 8.2,
      until: 16.0,
      say: `A repository is attached to the channel directly. Paste a token once, and it lists the repositories that token can actually see.`,
    },
    {
      // The repository picked at 16, Connect at 17, the toast; then still.
      at: 16.0,
      until: 28.0,
      chapter: "The model never holds the key",
      say: `The token goes in and does not come out. The model never sees it. Requests go through a proxy that attaches the credential on the way past. The model knows the repository exists and that it may call it. Nothing more.`,
    },
    {
      // Bundles page at 28, the dialog at 34, Engineering typed, the bundle
      // page at 38.
      at: 28.0,
      until: 38.5,
      chapter: "A bundle for the tracker",
      say: `Other services are grouped into bundles: credentials, allowed hosts, and instructions, attached to a workspace or to one channel. One for the tracker.`,
    },
    {
      // ClickUp's row at 39, its dialog at 41, the token 42 to 44, Test
      // connection at 45 and its answer at 46.
      at: 39.5,
      until: 47.0,
      rest: "keep",
      say: `Same proxy, same rule. It is tested before it is saved: one real call to the service.`,
    },
    {
      // Connect at 50, the dialog gone at 51.
      at: 48.5,
      until: 52.5,
      say: `Save it. From here on, requests to the tracker carry this credential, and the model never sees it.`,
    },
    {
      // Back through Workspaces to the channel page at 58; Add at 63, the
      // bundle picked at 65, attached at 66.
      at: 56.5,
      until: 66.5,
      rest: "keep",
      say: `Both attached to this one channel, and nowhere else. Open the channel page, and add the bundle to it.`,
    },
    {
      // The chip in Access, the toast, then the page scrolled to the rest.
      at: 66.5,
      say: `Allowed hosts, allowed methods, and a writes policy that starts at ask a person. Everything this channel can reach is on this one page.`,
    },
  ],
};

// --- 4. reach ------------------------------------------------------------------

const reach: RecutPart = {
  id: "4-reach",
  source: raw("4-reach", "page@9e6490364e7c106ea675ccc869f19cfa.webm"),
  scenes: [
    card("It reaches the repository", "One action you allowed, on one host you named. The answer comes from the repository, not from the model's memory of one."),
    {
      // The channel; the previous answer's pane returns at 10; typing 11.5 to
      // 16.5; sent at 17.5.
      at: 9.0,
      until: 18.0,
      chapter: "It reaches the repository",
      say: `Back in the channel. Nothing has changed except what it can reach. Ask what changed in the repository since Monday, and whether anything is still unreviewed.`,
    },
    {
      // Bar at 27.5, the new thread at 28.7 with "github get pr" on its
      // status line, held to 49.
      at: 27.3,
      until: 49.0,
      say: `Watch the status line: it is calling the repository. One action you allowed, on one host you named.`,
    },
    {
      // Streams in 49 to 50.6, footer at 51.
      at: 49.0,
      say: `The answer comes from the repository, not from the model's memory of one. A credential the channel holds. A model that may use it and cannot see it.`,
    },
  ],
};

// --- 5. the hold ---------------------------------------------------------------

const hold: RecutPart = {
  id: "5-hold",
  source: raw("5-hold", "page@39a08ba48d8526875852491b81fd50eb.webm"),
  scenes: [
    card("A write stops for a person", "A ticket drafted from the thread, held behind a card until someone confirms it. Then it runs, and the record has both."),
    {
      // Typing 11.7 to 17, sent at 18.5.
      at: 9.0,
      until: 19.0,
      rest: "keep",
      chapter: "A write stops for a person",
      say: `Reading is the easy half. Earlier, someone said the reconciliation change needed a ticket, and nobody raised it. Ask for it.`,
    },
    {
      // Bar at 24, the thread at 25 working, the card at 28.5; then the card,
      // still, for seventy seconds.
      at: 23.5,
      until: 40.0,
      say: `It stops. The card shows exactly what it wants to do: the title and description it wrote from the thread, and the request it will send. Nothing has happened yet.`,
    },
    {
      // The pane scrolls to the buttons at 106.5, Confirm is pressed at 108,
      // "Confirmed by" at 109.
      at: 105.5,
      until: 110.5,
      say: `Who may confirm is set in the console. The person who confirms is recorded next to what they approved.`,
    },
    {
      // "Ticket created" with its link at 111.
      at: 110.5,
      say: `It runs, and posts the link. The model proposed. A person decided. The record has both.`,
    },
  ],
};

// --- 6. the fix job ------------------------------------------------------------

const fix: RecutPart = {
  id: "6-fix",
  source: raw("6-fix", "page@8cb816ab308165c01e6c0aded6ca0606.webm"),
  scenes: [
    card("Fix it, and raise a PR", "A brief written from the thread, a separate container that makes the change and runs the tests, and a draft pull request. Never a merge."),
    {
      // The channel, clean, from 6 until the broken pane appears at 9.2.
      at: 6.0,
      until: 8.9,
      chapter: "Fix it, and raise a PR",
      say: `The large write. The thread already knows what is wrong with the retry path.`,
    },
    {
      // Typing 11.5 to 17.7, sent at 18, in the channel at 18.5.
      at: 11.4,
      until: 19.2,
      zoom: composer,
      rest: "keep",
      say: `Ask for a fix and a pull request: fix the exhausted-budget path so it queues instead of failing closed, and raise a PR.`,
    },
    {
      // The thread at 23 with "Preparing a fix job" on the status line, the
      // brief at 26.3; then still, for a minute.
      at: 22.8,
      until: 40.0,
      say: `It does not run code itself. It writes a brief: what to change, the evidence from this thread, and what done looks like. The brief is held behind a card that states the repository, the base branch, and the rule the worker runs under.`,
    },
    {
      at: 40.0,
      until: 60.0,
      chapter: "What the worker may do",
      say: `One new branch. One draft pull request. It never merges, and it never pushes to the base branch. That is what the worker is able to do, not an instruction a model could talk itself out of.`,
    },
    {
      // The pane scrolls to the buttons at 86, Confirm at 88, the job queued
      // at 89 and running at 90.
      at: 85.5,
      rest: "keep",
      say: `Confirm. A separate container clones the repository, makes the change, runs the tests, and pushes a branch. The thread shows each stage. It takes as long as your tests take.`,
    },
  ],
};

// --- 7. memory -----------------------------------------------------------------

const memory: RecutPart = {
  id: "7-memory",
  source: raw("7-memory", "page@90a35352a28b588c13d2883486e9526f.webm"),
  scenes: [
    card("The result, and memory", "The draft pull request on the Jobs page, and a fact the bot is told to keep — and where that fact is allowed to travel."),
    {
      // The Jobs list at 2, the row opened at 4, the job page to 14.4.
      at: 2.0,
      until: 14.4,
      chapter: "A draft pull request",
      say: `The result, on the Jobs page: the job, what it changed, what it cost, and a draft pull request waiting for a person to read it. Not merged. Not on the base branch.`,
    },
    {
      // Slack, booted, with the pane that could not load already dismissed:
      // the channel, clean, 21 to 24.7, the pointer crossing it. The "@"
      // picker opens at 25.0, and a hold must not land on it.
      at: 21.0,
      until: 24.7,
      chapter: "Memory",
      say: `It can be told to remember: conventions, decisions, the reason something is the way it is.`,
    },
    {
      // Typing 25 to 29.5, sent at 30.
      at: 24.8,
      until: 30.5,
      say: `Say remember, and what to keep: we deploy on Tuesdays and never on a Friday.`,
    },
    {
      // The thread opens at 33 on "Noted — saved to this channel", complete
      // with its footer by 34.
      at: 32.5,
      until: 37.5,
      say: `Noted, it says, and where it was saved.`,
    },
    {
      // The same thread, reopened at 40 and held to 44.
      at: 40.2,
      until: 44.2,
      say: `A fact learned in a public channel is shared with the channel. One learned in a private channel or a direct message stays there. Where it was said decides.`,
    },
    {
      // The console's Memory page, one row.
      at: 44.3,
      say: `Every memory is in the console, with who said it and when. Any of them can be deleted.`,
    },
  ],
};

// --- 8. the record -------------------------------------------------------------

const record: RecutPart = {
  id: "8-record",
  source: raw("8-record", "page@33587b057601cc852bde310b57d40a99.webm"),
  scenes: [
    card("Which model, and who approved what", "Any model your endpoint serves, changed in one place. Every question, tool call, write and cost, in one ledger."),
    {
      // Settings, General, from 1.5; the Models tab at 5.2 and its picker
      // open 6 to 9; closed at 9.5.
      at: 1.5,
      until: 10.0,
      chapter: "Which model",
      say: `Which model. Any model your endpoint serves. Change it here, and the next message uses it. Nothing to redeploy.`,
    },
    {
      // The Activity page from 10.3, its table filling by 11.
      at: 10.2,
      until: 20.0,
      chapter: "Who approved what",
      say: `Everything you have watched is in one place. Every question, every tool call, every write and who confirmed it, every token and its cost.`,
    },
    {
      // The table, scrolled at 20 to 22.4, then still until the dashboard
      // navigation at 26.45 — four frames of loading splash sit between the
      // two pages, and a hold on one of them is six seconds of blank.
      at: 20.0,
      until: 26.4,
      say: `Each row is one turn: the channel, the model, tokens in and out, and what it cost. Budgets are per workspace and per channel, and when one is spent, the bot stops.`,
    },
    {
      // The dashboard, painted from 26.6.
      at: 26.7,
      until: 36.5,
      say: `It reads what the team reads. It reaches what you allow. It never holds the key. It stops before it changes anything.`,
    },
    // The end card, as recorded.
    { at: 36.6 },
  ],
};

export const DEEP_DIVE_RECUT: RecutLongForm = {
  id: DEEP_DIVE.id,
  title: DEEP_DIVE.title,
  parts: DEEP_DIVE.parts.map((p) => p.id),
  recuts: [website, signup, invite, thread, connect, reach, hold, fix, memory, record],
  partsDir,
  outDir: DEEP_DIVE.outDir,
  // The memory thread, answered: a short reply with its footer beside a
  // channel with a conversation in it — the most legible frame of the product
  // doing the thing.
  poster: { part: "7-memory", scene: 4, offset: 2.0 },
};
