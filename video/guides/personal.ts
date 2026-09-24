import type { Guide, LongForm, Surfaces } from "../types";
import { SEL } from "../slack";
import {
  BUNDLE,
  assertDb,
  assertDeepDiveState,
  bot,
  channel,
  dbCount,
  dbValue,
  ORG,
  outDir,
  partsDirFor,
  presetRow,
  waitForDb,
} from "./shared";

// "Your own mail and calendar" — the walkthrough.
//
// Three parts, console → Slack → console, recorded in the organisation the deep
// dive founded:
//
//   1-admin    the admin registers the organisation's Google OAuth client on the
//              Engineering bundle: a client id and a secret, which reach nobody's
//              account on their own
//   2-connect  one person connects their own account from Slack and asks about
//              their calendar; the answer comes through their token, nobody else's
//   3-proof    what the admin can see of it afterwards, and what they cannot
//
// Environment, beyond what the deep dive needs. All of it is read when called —
// see LEARNINGS.md, "Environment, and where it is read".
//
//   VIDEO_GOOGLE_CLIENT_ID      A Google OAuth client of type "Web application", from a
//   VIDEO_GOOGLE_CLIENT_SECRET  THROWAWAY Google Cloud project with the Gmail API and the
//                               Google Calendar API enabled, and exactly one authorised
//                               redirect URI: the console's callback,
//                               `<VIDEO_CONSOLE_URL origin>/connect/callback`
//                               (internal/app/oauth_user.go, connectRoutes and
//                               connectRedirectURI; the Connect dialog prints the exact
//                               string). The client id is on camera in the clear — it is
//                               not a secret — and the secret renders as dots. Delete the
//                               project when the recording is done.
//
// And one thing no variable carries: the recording profile (`npm run signin`) must be
// signed in to a THROWAWAY Google account, listed as a test user on that OAuth client,
// with an event or two on tomorrow's calendar. Its address is on camera twice, on
// Google's consent screen and on the "Connected as …" page, so it has to be one that
// can be thrown away with the video.
//
// Where the link comes from. `!connect` answers in the thread but sends the connect
// link by direct message — connectCommand posts the card to c.UserID, because a connect
// link is a capability and a channel is not a private place. A URL button opens a new
// tab, which the recorder does not film, so 2-connect reads the button's href out of
// the bot's DM and navigates the recorded page to it. Reaching the DM means leaving the
// filtered client view (a plain navigation drops the sidebar CSS open() applies), so
// that happens off camera; the thread reply saying the link went by direct message is
// the shot.
//
// Re-takes. 1-admin creates the Google connection, and a second one in the same bundle
// makes `!personal_instructions` refuse to guess which account the text is for — so
// `before` stops when one already exists rather than filming a duplicate: delete it
// from the Engineering card's row menu (Delete), which also revokes and forgets every
// personal sign-in under it, and re-record from 1-admin. Tidy the channel between
// takes (`npm run tidy`): a duplicate `!connect` row is on camera, and ask() opens the
// thread of whichever row with a reply bar it finds last.

const partsDir = partsDirFor("personal");
const TITLE = "Your own mail and calendar";

// ---------------------------------------------------------------------------
// Environment. Read when called, never at module scope.
const googleClientId = () => process.env.VIDEO_GOOGLE_CLIENT_ID ?? "";
const googleClientSecret = () => process.env.VIDEO_GOOGLE_CLIENT_SECRET ?? "";

/** Proxied calls to Google that went through — what "answered through your own account" leaves behind. */
const GOOGLE_CALLS = "select count(*) from proxy_audit where host like '%googleapis.com' and coalesce(blocked,'')='';";

// The three lines this walkthrough sends. Functions, because the handle comes from the
// environment; the same text is what openThreadOf() finds the row by afterwards.
const CONNECT = () => `@${bot()} !connect`;
const CALENDAR = () => `@${bot()} what's on my calendar tomorrow?`;
const INSTRUCT = () =>
  `@${bot()} !personal_instructions only ever read my calendar and inbox — never send, create or delete anything`;

// ---------------------------------------------------------------------------
// Helpers.

/**
 * The header row of one bundle's card on /bundles: the innermost element holding both
 * the card's toggle (a button whose name starts with the bundle's name — bundle-card.tsx)
 * and its "Configure" button. Every card has a "Configure", so the name alone reaches
 * whichever card is first.
 */
function bundleHeader(s: Surfaces, name: string) {
  return s.page
    .locator("div")
    .filter({ has: s.page.getByRole("button", { name: new RegExp(`^${name}`) }) })
    .filter({ has: s.page.getByRole("button", { name: /^configure$/i }) })
    .last();
}

/** Open a bundle card's connection table if it is folded. Open or closed is remembered per browser. */
async function expandBundle(s: Surfaces, name: string): Promise<void> {
  const toggle = s.page.getByRole("button", { name: new RegExp(`^${name}`) }).first();
  await toggle.waitFor({ state: "visible", timeout: 20_000 });
  if ((await toggle.getAttribute("aria-expanded")) !== "true") await s.app.click(toggle, { settle: 900 });
}

/**
 * Untick one box in the Connect dialog's "What to connect" list, found by its label's
 * text (connect-dialog.tsx, optionChecklist). A Radix checkbox is a button with
 * aria-checked, so this reads the state first: clicking blindly toggles.
 */
async function untick(s: Surfaces, label: RegExp): Promise<void> {
  const box = s.page.locator("label").filter({ hasText: label }).getByRole("checkbox").first();
  await box.waitFor({ state: "visible", timeout: 15_000 });
  if ((await box.getAttribute("aria-checked")) === "true") await s.app.click(box, { settle: 500 });
}

/**
 * The newest connect link on screen: the Open button of the last card in view, or failing
 * that the last such link anywhere on the page. A Block Kit URL button is an anchor, and
 * the token has a fixed shape (oauth_user.go, mintConnectToken: v1.<org>.<conn>.…), so
 * anything else with "/connect/" in it is not the link.
 */
async function connectHref(s: Surfaces, timeoutMs = 15_000): Promise<string> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const inLastCard = s.page.locator(SEL.message).last().locator('a[href*="/connect/"]').last();
    const anywhere = s.page.locator('a[href*="/connect/"]').last();
    for (const a of [inLastCard, anywhere]) {
      if ((await a.count().catch(() => 0)) === 0) continue;
      const href = (await a.getAttribute("href").catch(() => null)) ?? "";
      if (/\/connect\/v1\./.test(href)) return href;
    }
    if (Date.now() > deadline) return "";
    await s.wait(500);
  }
}

/**
 * The connect link out of the bot's direct message.
 *
 * Off camera, always: two of the three ways in are plain navigations, which drop the
 * sidebar filter open() applied, and the recording account's DM list is not for a public
 * video. The first way keeps the client intact and works only when the DM is listed and
 * kept; the second puts the bot's user id where a channel id goes, which the client
 * resolves to the DM; the third is Slack's documented deep link for the same thing.
 */
async function connectLinkFromDM(s: Surfaces): Promise<string> {
  const team = dbValue("select team_id from teams where status='active' order by installed_at desc limit 1;");
  const botUser = dbValue("select bot_user_id from teams where status='active' order by installed_at desc limit 1;");
  const ways: [string, () => Promise<void>][] = [
    [
      "the sidebar row",
      async () => {
        await s.slack.openChannel(bot());
      },
    ],
    [
      `app.slack.com/client/${team}/${botUser}`,
      async () => {
        await s.page.goto(`https://app.slack.com/client/${team}/${botUser}`, { waitUntil: "domcontentloaded", timeout: 60_000 });
        await s.page.waitForSelector(SEL.booted, { timeout: 60_000 });
      },
    ],
    [
      "slack.com/app_redirect",
      async () => {
        await s.page.goto(`https://slack.com/app_redirect?team=${team}&channel=${botUser}`, { waitUntil: "domcontentloaded", timeout: 60_000 });
        // A browser without the desktop app may be parked on "open in your browser" first.
        await s.app.clickIfPresent(s.page.getByRole("link", { name: /browser/i }).first());
        await s.page.waitForSelector(SEL.booted, { timeout: 60_000 });
      },
    ],
  ];
  const tried: string[] = [];
  for (const [name, open] of ways) {
    tried.push(name);
    try {
      await open();
    } catch {
      continue;
    }
    await s.wait(2_500);
    const href = await connectHref(s);
    if (href) return href;
  }
  throw new Error(
    `Could not find the connect link in the bot's direct message (tried ${tried.join(", ")}).\n` +
      `The bot posts it there when \`!connect\` runs (oauth_user.go, connectCommand). Check the DM with ` +
      `@${bot()} holds a card with an Open button, and that the recording profile can open that DM.`,
  );
}

/**
 * Google's side of the sign-in, until the browser is sent back to the callback.
 *
 * The pages vary — an account chooser, the "Google hasn't verified this app" interstitial,
 * then the consent screen with a box per scope — so this presses whichever is on screen,
 * and it ticks every scope box before pressing Continue: Google leaves them unticked, and
 * a grant without the calendar scope is a connection that cannot answer the next scene.
 * It returns only once the URL has left Google, or throws; every click in here is a
 * clickIfPresent, and a scene must never end on one of those.
 */
async function consentAtGoogle(s: Surfaces): Promise<void> {
  const atGoogle = () => {
    try {
      return /google\.com$/i.test(new URL(s.page.url()).hostname);
    } catch {
      return false;
    }
  };
  const deadline = Date.now() + 120_000;
  while (atGoogle()) {
    if (Date.now() > deadline) {
      throw new Error(
        `Still on ${s.page.url()} after two minutes. Google is asking for something this script ` +
          `cannot answer — a password, a second factor, a "verify it's you" — so sign the recording ` +
          `profile into the throwaway Google account by hand (npm run signin) and re-record 2-connect.`,
      );
    }
    // Every unticked scope box first, then whatever moves the page on.
    const boxes = s.page.getByRole("checkbox");
    for (let i = 0, n = await boxes.count().catch(() => 0); i < n; i++) {
      const box = boxes.nth(i);
      const visible = await box.isVisible().catch(() => false);
      if (visible && !(await box.isChecked().catch(() => true))) await s.app.click(box, { settle: 500 });
    }
    if (await s.page.getByRole("heading", { name: /choose an account/i }).isVisible().catch(() => false)) {
      if (await s.app.clickIfPresent(s.page.locator("[data-identifier]").first(), { settle: 2_500 })) continue;
      if (await s.app.clickIfPresent(s.page.getByRole("link", { name: /@/ }).first(), { settle: 2_500 })) continue;
    }
    // The unverified-app interstitial gives a test user a plain Continue, and anyone else
    // "Advanced" and then "Go to … (unsafe)". The consent screen ends on Continue or Allow.
    if (await s.app.clickIfPresent(s.page.getByRole("button", { name: /^(continue|allow)$/i }).first(), { settle: 2_500 })) continue;
    if (await s.app.clickIfPresent(s.page.getByRole("link", { name: /unsafe/i }).first(), { settle: 2_500 })) continue;
    if (await s.app.clickIfPresent(s.page.getByRole("button", { name: /^advanced$/i }).first(), { settle: 1_200 })) continue;
    if (await s.app.clickIfPresent(s.page.getByRole("link", { name: /^advanced$/i }).first(), { settle: 1_200 })) continue;
    await s.wait(1_000);
  }
}

// --- 1. the admin's half ----------------------------------------------------------

const admin: Guide = {
  id: "1-admin",
  title: TITLE,
  before: async () => {
    assertDeepDiveState();
    const missing = (
      [
        ["VIDEO_GOOGLE_CLIENT_ID", googleClientId()],
        ["VIDEO_GOOGLE_CLIENT_SECRET", googleClientSecret()],
      ] as const
    )
      .filter(([, v]) => !v)
      .map(([k]) => k);
    if (missing.length) {
      throw new Error(
        `1-admin registers a Google OAuth client on camera and needs ${missing.join(" and ")} in video/.env:\n` +
          `a "Web application" client from a throwaway Google Cloud project whose one authorised redirect URI\n` +
          `is <VIDEO_CONSOLE_URL origin>/connect/callback. See the header of guides/personal.ts.`,
      );
    }
    if (dbCount("select count(*) from settings where key='public_origin' and value != '';") < 1) {
      throw new Error(
        "The server has not learned its public origin yet, so it can mint no connect link and build no OAuth\n" +
          "callback. Open the console once over VIDEO_CONSOLE_URL (any deep-dive part does) and try again.",
      );
    }
    if (dbCount(`select count(*) from connections where org_id = ${ORG()} and preset='google';`) > 0) {
      throw new Error(
        "A Google Workspace connection already exists in this organisation — an earlier take of 1-admin.\n" +
          "A second one would make `!personal_instructions` refuse to guess which account the text is for, so\n" +
          `delete it first: /bundles → the ${BUNDLE} card → the row's menu → Delete. That also revokes and\n` +
          "forgets every personal sign-in under it. Then re-record from 1-admin.",
      );
    }
  },
  scenes: [
    {
      chapter: "One connection, many people",
      say: `Some services are not one credential for a whole channel. A mailbox or a calendar belongs to a person, and the answer depends on who asked. Start in the console, on the Engineering bundle.`,
      act: async (s) => {
        await s.app.goto("/bundles");
        await s.wait(1_200);
        await s.app.click(bundleHeader(s, BUNDLE).getByRole("button", { name: /^configure$/i }), { settle: 1_200 });
      },
    },
    {
      say: `Google Workspace is one of these. The dialog says so: there is no shared account. Everyone signs in for themselves, and the bot only ever acts as whoever asked it something.`,
      act: async (s) => {
        // The Credentials tab lists two dozen services; the search brings the row up.
        // search-field.tsx: the input carries the placeholder as its label.
        await s.app.typeVerified('input[placeholder="Find a service"]', "Google");
        await s.wait(600);
        await s.app.click(presetRow(s, /Google Workspace/), { settle: 1_400 });
      },
    },
    {
      say: `What to connect is decided here, before anyone signs in. Gmail and Calendar, to read. Booking events comes off, and contacts come off. A part left out is a scope nobody is ever asked to grant.`,
      act: async (s) => {
        // The preset ticks all three parts and Calendar's write box (presets.go, google);
        // the labels are the option's write_label and label (connect-dialog.tsx).
        await untick(s, /may also book, move and cancel events/i);
        await untick(s, /contacts and directory/i);
        await s.wait(700);
      },
    },
    {
      chapter: "A client, not a credential",
      say: `The admin holds only the organisation's OAuth client: a client id and a secret, which reach nothing on their own. The redirect address is registered with Google once. Nothing on this form can read anyone's mail.`,
      act: async (s) => {
        // #rec-secret is the client id for an oauth_user preset; the secret input is the
        // password field beside it (connect-dialog.tsx, RecommendedSecret).
        await s.app.typeVerified("#rec-secret", googleClientId(), 12);
        await s.app.typeVerified('input[aria-label="Client secret"]', googleClientSecret(), 30);
        await s.wait(400);
        // CAUTION before the first render: #redirect-uri prints the callback
        // built from the host being recorded — the Funnel host, when this is
        // recorded locally — and the api walkthrough's terminal card had the
        // same leak. Either record this part against production
        // (VIDEO_ALLOW_PRODUCTION=1) or keep the field out of frame; do not
        // narrate over a URL that names the recording machine.
        await s.app.point("#redirect-uri");
        await s.wait(900);
      },
    },
    {
      say: `Connect. The bundle lists it, and the row says what is true: nobody connected yet. The credential that will spend is one per person, and none exists.`,
      act: async (s) => {
        // The dialog's own submit reads "Connect"; the bundle editor behind it has one per
        // row. Exact, and last in the document, as the deep dive does it.
        await s.app.click(s.page.getByRole("button", { name: /^Connect$/ }).last());
        await s.wait(2_000);
        assertDb(`select count(*) from connections where org_id = ${ORG()} and preset='google' and status='active';`, 1, "The Google Workspace connection was not saved");
        // Close the bundle editor so the card's table is the shot.
        await s.page.keyboard.press("Escape");
        await s.wait(700);
        if (await s.page.getByRole("dialog").first().isVisible().catch(() => false)) {
          await s.page.keyboard.press("Escape");
          await s.wait(700);
        }
        await expandBundle(s, BUNDLE);
        // connection-table.tsx, MembersChip: the per-person marker on an oauth_user row.
        await s.app.point(s.page.getByText(/nobody connected yet/i).first());
        await s.wait(800);
      },
    },
  ],
};

// --- 2. one person connects --------------------------------------------------------

const connect: Guide = {
  id: "2-connect",
  title: TITLE,
  before: async () => {
    if (dbCount(`select count(*) from connections where org_id = ${ORG()} and preset='google' and status='active';`) < 1) {
      throw new Error("No Google Workspace connection in this organisation — record 1-admin first.");
    }
  },
  scenes: [
    {
      chapter: "Connect from Slack",
      say: `Back in the channel, as one of its members. Ask what here runs on your own account rather than on a credential an admin set up.`,
      act: async (s) => {
        // Slack boots blank coming from a title card; open it inside the cut that opens
        // the scene. Then the command on camera, the retry-prone wait off it, and the
        // thread opened on the answer — the seam 7-memory uses.
        await s.offCamera(async () => {
          await s.slack.openChannel(channel());
          await s.slack.closeThread();
        });
        await s.slack.send(CONNECT());
        await s.wait(800);
        await s.offCamera(async () => {
          await s.slack.awaitReply(CONNECT(), { requireFooter: false, settleMs: 2_500 });
          await s.slack.closeThread();
        });
        await s.slack.openThreadOf(CONNECT());
        await s.wait(1_200);
      },
    },
    {
      say: `Google Workspace: Gmail and Calendar, not connected yet, with what each part lets you ask for once it is. The link to connect it went to you as a direct message, not into the channel. It is minted for you alone, lasts thirty minutes, and the model never sees it.`,
      act: async (s) => {
        await s.wait(2_000);
      },
    },
    {
      chapter: "Your account, your consent",
      say: `The link opens this page, for you alone. It lists what the bot may reach, part by part, in plain words: read your mail, read your calendar. Nothing the admin left out is offered, and you can untick more. It acts only when you ask it to, and only as you.`,
      act: async (s, ctx) => {
        // The DM is not for the camera (see the header); the page the link opens is.
        await s.offCamera(async () => {
          ctx.connectLink = await connectLinkFromDM(s);
          await s.app.goto(ctx.connectLink);
        });
        await s.wait(600);
        // oauth_user.go, connectPage: one .box per grant, each with a checkbox.
        await s.app.point(s.page.getByRole("checkbox").first());
        await s.wait(1_200);
        await s.app.point(s.page.getByRole("checkbox").last());
      },
    },
    {
      say: `Continue, and Google takes over. This is Google's own consent screen: the app, the account, and each scope in Google's words. Allow it. The tokens come back to the server and are sealed against your Slack identity. The admin never sees them, and neither does the model.`,
      act: async (s) => {
        await s.app.click(s.page.getByRole("button", { name: /^continue$/i }).first(), { settle: 800 });
        await s.page.waitForURL(/accounts\.google\.com/, { timeout: 45_000 });
        await s.wait(1_500);
        await consentAtGoogle(s);
        // Back on the callback: "Connected as …" or "You're connected." (oauth_user.go,
        // handleConnectCallback). "Nothing was connected" would also say connected, hence
        // the anchor — and the row below is the proof either way.
        await s.page
          .getByRole("heading", { name: /^(connected as|you're connected)/i })
          .first()
          .waitFor({ state: "visible", timeout: 30_000 });
        await s.wait(1_500);
        assertDb(
          `select count(*) from user_connections where org_id = ${ORG()} and status='active' and length(coalesce(secret_enc,''))>0;`,
          1,
          "The sign-in did not leave a personal credential behind",
        );
      },
    },
    {
      chapter: "Answered as you",
      // Bot-wait scene, on camera: the status line is the shot.
      say: `Back in Slack. Ask about tomorrow. Watch the status line: it is calling the calendar through the proxy, with the token that belongs to the person who asked. Anyone else in this channel asking the same question gets their own calendar, or a link to connect one. Nobody reads another person's mail or calendar through the bot.`,
      act: async (s) => {
        await s.offCamera(async () => {
          await s.slack.openChannel(channel());
          await s.slack.closeThread();
        });
        const calls = dbCount(GOOGLE_CALLS);
        // A model answer: done means the footer, not a status card that stopped changing.
        await s.slack.ask(CALENDAR(), { requireFooter: true });
        // Not the model's words — the proxy's ledger. A take where the answer came from
        // anywhere but the person's own account would narrate something untrue.
        await waitForDb(GOOGLE_CALLS, calls + 1, 20_000, "The answer did not go through the person's own Google account: no proxied googleapis.com request was recorded");
      },
    },
    {
      chapter: "Your own instructions",
      say: `You can tell it how to behave as you. These instructions reach the model only on your turns, and nobody else in the channel sees them.`,
      act: async (s) => {
        await s.slack.send(INSTRUCT());
        await s.wait(600);
        await s.offCamera(async () => {
          await s.slack.awaitReply(INSTRUCT(), { requireFooter: false, settleMs: 2_500 });
          await s.slack.closeThread();
        });
        await s.slack.openThreadOf(INSTRUCT());
        await s.wait(1_000);
        assertDb(`select count(*) from user_connections where org_id = ${ORG()} and instructions != '';`, 1, "The personal instructions were not saved");
      },
    },
    {
      say: `Saved, for one connection and one person. They travel with your account, not with the channel.`,
      act: async (s) => {
        await s.wait(1_500);
      },
    },
  ],
};

// --- 3. what the admin sees --------------------------------------------------------

const proof: Guide = {
  id: "3-proof",
  title: TITLE,
  before: async () => {
    if (dbCount(`select count(*) from user_connections where org_id = ${ORG()} and status='active' and length(coalesce(secret_enc,''))>0;`) < 1) {
      throw new Error("Nobody has connected a Google account in this organisation — record 2-connect first.");
    }
  },
  scenes: [
    {
      chapter: "What the admin sees",
      say: `Back in the console, on the Engineering bundle. The admin sees that the connection exists and how many people have connected: one. Not whose mail, not a token, not what was asked.`,
      act: async (s) => {
        await s.app.goto("/bundles");
        await s.wait(1_200);
        await expandBundle(s, BUNDLE);
        // connection-table.tsx, MembersChip: "1 person connected" / "N people connected".
        await s.app.point(s.page.getByText(/\d+ (person|people) connected/i).first());
        await s.wait(1_000);
      },
    },
    {
      say: `The activity page, filtered to this channel: the turn, with its tokens and cost, and under Proxy the calls it made. Host, path, status, time. The request is recorded. Its content is not.`,
      act: async (s) => {
        await s.app.goto("/activity");
        await s.wait(1_200);
        // activity-page.tsx: two selects with aria-labels; the channel option is the
        // scope's name, which carries the #.
        await s.app.click(s.page.getByRole("combobox", { name: /filter by channel/i }), { settle: 700 });
        await s.app.click(s.page.getByRole("option", { name: new RegExp(channel(), "i") }).first(), { settle: 1_400 });
        await s.wait(1_000);
        await s.app.click(s.page.getByRole("tab", { name: /^proxy/i }), { settle: 1_200 });
        // proxy-table.tsx: host in its own cell.
        await s.app.point(s.page.getByRole("cell", { name: /googleapis\.com/ }).first());
        await s.wait(800);
      },
    },
    {
      say: `The person connected it. The person can disconnect it, from the same page the link opens, and the grant is handed back to Google. One client for the organisation. One credential per person. Nobody reads another's mail through the bot.`,
      act: async (s) => {
        await s.app.scroll(200);
        await s.wait(1_500);
      },
    },
  ],
};

export const PERSONAL: LongForm = {
  id: "personal",
  title: TITLE,
  cardChapters: [
    "One connection, many people",
    "A client, not a credential",
    "Connect from Slack",
    "Your account, your consent",
    "Answered as you",
    "What the admin sees",
  ],
  partsDir,
  outDir,
  parts: [admin, connect, proof],
  // The calendar answer arriving in its thread, status line and footer: the product doing
  // the thing, as one person.
  poster: { chapter: "Answered as you", offset: 14 },
};
