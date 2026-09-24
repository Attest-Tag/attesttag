import type { Locator } from "playwright";
import { Stage } from "./stage";

// Driving the real Slack web client.
//
// Everything here exists because Slack is not our app: we cannot add a
// `data-testid`, we cannot make it settle deterministically, and its DOM is
// not a contract we are party to. Two consequences shape this file.
//
// **Every selector Slack owns lives in SEL below, and nowhere else.** When a
// release moves one, the fix is one line in one map rather than a hunt through
// seven guide scripts. Run `npm run scout` after any Slack update to check them
// before you spend a render finding out.
//
// **Nothing here asserts on what the bot said.** The answer is model output —
// it differs between takes, and a script that waits for particular words waits
// forever on the take where it phrased things differently. We wait for the
// reply to *stop growing*, which is a property of the transport rather than of
// the model.

/**
 * Slack's own test hooks. `data-qa` attributes are what Slack's client is
 * instrumented with, which makes them far steadier than class names (those are
 * hashed and change every release) — but they are still Slack's internal
 * business and carry no compatibility promise to us.
 *
 * Verified against the web client on the date in the comment. `scout.ts`
 * re-checks every one of them and prints what is missing.
 */
export const SEL = {
  /**
   * The message box. Contenteditable (Quill), so `fill()` does nothing.
   *
   * The container carries the hook; the editable element is inside it. There
   * is no `message_input` — that was a guess, and it cost a boot timeout that
   * looked like a signed-out client on a client that was signed in.
   */
  composer: '[data-qa="message_input_container"] [contenteditable="true"]',
  /** One message row, in the channel or in an open thread. */
  message: '[data-qa="message_container"]',
  /** The blocks inside one message — the text the bot streams into. */
  messageText: '[data-qa="message-text"]',
  /** Sender name on a message row, for telling the bot's replies from ours. */
  messageSender: '[data-qa="message_sender_name"]',
  /**
   * A channel row in the left sidebar.
   *
   * The hook carries the channel name in it — `channel_sidebar_name_social` —
   * so this is a prefix match. `openChannel` builds the exact one.
   */
  sidebarChannel: '[data-qa^="channel_sidebar_name_"]',
  /**
   * The "N replies / View thread" bar under a message that has a thread.
   *
   * This is the important one. The bot answers **in a thread**, so a channel
   * view never gains its reply — the bar appears instead, and clicking it is
   * the only way to reach the answer. Reading the channel and waiting reports
   * zero characters forever while the server log shows a completed turn.
   */
  viewThread: '[data-qa="reply_bar_view_thread"]',
  /** "Reply in thread" on a message's hover toolbar, for starting one. */
  replyInThread: '[data-qa="start_thread"]',
  /**
   * The @-mention autocomplete.
   *
   * `[role="listbox"]`, not `[data-qa="autocomplete-list"]` — that hook does
   * not exist, so the list was never detected as open, Tab was never pressed,
   * and the mention never became a mention. The failure surfaced as "that
   * handle matches no one in this workspace" about a handle that was correct,
   * which sent us looking at Slack app settings instead of at this line.
   *
   * TRANSIENT: only while a mention is being typed.
   */
  autocomplete: '[role="listbox"]',
  /** The client has finished booting when the message pane exists. */
  booted: '[data-qa="message_pane"]',
} as const;

/**
 * Chrome that is Slack's, not ours, and that a viewer does not need to see.
 *
 * Deliberately short. It is tempting to tidy Slack until the shot is perfect,
 * but every rule here is a small lie about what the product looks like in use
 * — so this hides only things that are *about the recording account* (its own
 * unread badges, its call banners, the "get the desktop app" nags) and never
 * anything about the bot or the conversation.
 */
const HIDE_CHROME = `
  [data-qa="notification_bar"],
  [data-qa="desktop_app_prompt"],
  [data-qa="top_nav_help"] { display: none !important; }
`;

/**
 * Strip the recording account's own clutter out of the frame, with CSS alone.
 *
 * What goes: the trial banner; the direct-message, app and starred sections;
 * drafts, directories, and every "add"/"invite" row; and every sidebar row
 * that is not one of the channels named in VIDEO_SIDEBAR_KEEP. What stays:
 * those channels, so the workspace still reads as a workspace.
 *
 * Applied from `open()` with `addStyleTag`, after the client has booted. An
 * init script did not work here — Slack's boot never left the tag in the
 * document — and a MutationObserver stalled the client outright. The style
 * engine applies `:has()` to virtualised rows as they appear, so one tag is
 * enough.
 */
function chromeCss(): string {
  const keep = (process.env.VIDEO_SIDEBAR_KEEP ?? process.env.VIDEO_CHANNEL ?? "attesttag-demo")
    .split(",").map((c) => c.trim()).filter(Boolean);
  const notKept = keep.map((c) => `:not(:has([data-qa="channel_sidebar_name_${c}"]))`).join("");
  return `
    [data-qa="sidebar_menu_header_banner"],
    [data-qa="workspace-banner-download-app"],
    [data-qa="virtual-list-item"]:has([data-qa^="preseeded_app_row_"]),
    .p-channel_sidebar__static_list__item:not(:has([data-qa])),
    [data-qa="virtual-list-item"]:has([data-qa$="__direct_messages"]),
    [data-qa="virtual-list-item"]:has([data-qa$="__recent_apps"]),
    [data-qa="virtual-list-item"]:has([data-qa$="__stars"]),
    [data-qa="virtual-list-item"]:has([data-qa^="channel_sidebar_add_more_items"]),
    [data-qa="virtual-list-item"]:has([data-qa="sidebar_invite_cta"]),
    [data-qa="virtual-list-item"]:has([data-qa="channel_sidebar_directories_link"]),
    [data-qa="virtual-list-item"]:has([data-qa^="channel_sidebar_name_"])${notKept}
      { display: none !important; }
  `;
}

/** Kept for callers; the filter is now applied by open() itself. */
export async function keepOnlyChannels(
  _context: import("playwright").BrowserContext,
  _keep: readonly string[],
): Promise<void> {}

/** Bot replies stream. This is how long "no new text" means "it has finished". */
const STREAM_SETTLE_MS = 2_500;
/** Hard ceiling on one answer, including tool calls. */
const STREAM_TIMEOUT_MS = 180_000;

export class SlackStage extends Stage {
  /**
   * The bot's display name in this workspace, as Slack renders it on a message
   * row. Read from the environment because a demo workspace may have installed
   * the app under a different name than production's.
   */
  readonly botName: string;

  constructor(page: Stage["page"], botName = process.env.VIDEO_BOT_NAME ?? "attest_tag") {
    super(page, "https://app.slack.com");
    this.botName = botName;
  }

  /** Open the client and wait for it to actually boot. */
  async open(): Promise<void> {
    await this.goto("https://app.slack.com/client");
    await this.page.waitForSelector(SEL.booted, { timeout: 60_000 });
    await this.page.addStyleTag({ content: HIDE_CHROME }).catch(() => {});
    if (process.env.VIDEO_NO_SIDEBAR_FILTER !== "1") {
      await this.page.addStyleTag({ content: chromeCss() }).catch(() => {});
    }
    // The client paints its shell before the message pane has content. Nothing
    // here is worth asserting on, so this is a plain settle.
    await this.wait(2_000);
    await this.dismissChannelSearch();
  }

  /**
   * Close the "Search messages in this channel" bar if the client restored
   * one. It is per-account state Slack keeps from a ⌘F somebody pressed in
   * the recording window, and it came back on the next boot, across the top
   * of a take's opening shot. Best effort: Escape first, then the bar's own
   * close control; a bar that will not go is warned about, not fatal.
   */
  private async dismissChannelSearch(): Promise<void> {
    const bar = this.page.getByText(/^Search messages in this channel/).first();
    if (!(await bar.isVisible().catch(() => false))) return;
    await this.page.keyboard.press("Escape").catch(() => {});
    await this.wait(300);
    if (!(await bar.isVisible().catch(() => false))) return;
    const close = bar
      .locator("xpath=ancestor::*[.//button][1]")
      .getByRole("button", { name: /close|clear|dismiss|cancel/i })
      .last();
    if (await close.isVisible().catch(() => false)) {
      await close.click().catch(() => {});
      await this.wait(400);
    }
    if (await bar.isVisible().catch(() => false)) console.warn("      ! the in-channel search bar is still open");
  }

  /**
   * Click a channel in the sidebar by its name, without the leading #.
   *
   * Addressed by the hook that carries its own name rather than by matching
   * visible text, because the text carries an unread count and a mute state
   * and neither is ours to predict. A channel that is scrolled out of the
   * sidebar, or in a collapsed section, has no row at all — hence the message
   * on the failure, which is otherwise a bare timeout on a selector that looks
   * fine.
   */
  async openChannel(name: string): Promise<void> {
    // Open the client first if this page is not on it yet.
    //
    // Every part is its own browser run, and a part that opens in Slack starts
    // on a title card painted with setContent — not on Slack at all. Only the
    // first part happened to call open() explicitly, so every later Slack part
    // asked a blank document for its sidebar and failed with "(none — is the
    // client booted?)", which reads like a broken selector and is not one.
    // Healing it here means a scene can just say which channel it wants.
    if (!(await this.page.locator(SEL.booted).first().isVisible().catch(() => false))) {
      await this.open();
    }
    const row = this.page.locator(
      `[data-qa="channel_sidebar_name_${cssEscape(name)}"]`,
    );
    if (!(await row.first().isVisible().catch(() => false))) {
      const seen = await this.page
        .locator(SEL.sidebarChannel)
        .evaluateAll((els) =>
          els.map((e) => e.getAttribute("data-qa")?.replace("channel_sidebar_name_", "")),
        )
        .catch(() => []);
      throw new Error(
        `No #${name} in this workspace's sidebar.\n` +
          `In it: ${seen.filter(Boolean).join(", ") || "(none — is the client booted?)"}\n` +
          `Set VIDEO_CHANNEL in video/.env, or create the channel and invite the bot.`,
      );
    }
    await this.click(row, { settle: 1_200 });
    await this.dismissChannelSearch();
  }

  /**
   * Type a line into the composer and send it.
   *
   * `@name` is the whole reason this is not two lines. Typing the characters
   * of a mention does NOT make a mention: Slack turns it into one only when
   * you pick the person out of the autocomplete, and a message that merely
   * *says* "@attest_tag" in plain text never reaches the bot at all. That
   * failure is silent and looks exactly like the bot being down — the composer
   * accepts it, the message posts, and nothing happens for the rest of the
   * take.
   *
   * So: type up to the mention, let the list open, commit it with Tab, then
   * carry on with the rest of the line.
   */
  async compose(text: string, opts: { send?: boolean } = {}): Promise<void> {
    // Make sure there is somewhere to type first.
    //
    // Every part is its own browser run and opens on a title card, so a part
    // whose first scene speaks straight into a channel has no Slack loaded at
    // all. Healing it in openChannel was not enough — a scene that only calls
    // ask() never goes through openChannel — so it belongs here, where the
    // composer is actually needed. VIDEO_CHANNEL is the one the walkthrough
    // records in; a scene that wants a different one opens it explicitly and
    // this does nothing.
    if (!(await this.page.locator(SEL.composer).first().isVisible().catch(() => false))) {
      await this.openChannel(process.env.VIDEO_CHANNEL ?? "attest-demo");
    }
    const box = this.page.locator(SEL.composer).first();
    await this.click(box, { settle: 150 });

    // Clear whatever is in there first.
    //
    // Slack persists the draft server-side, so anything a previous run failed
    // to send is still sitting in the composer when the next one starts — and
    // it types on top of it. The compounding effect is vicious: the second
    // attempt's "@mention" is no longer at a word boundary, so the autocomplete
    // never opens, so that attempt fails too and leaves more behind. Every
    // failure after the first was caused by the first one's leftovers, and the
    // error each time blamed the handle.
    await box.press("End");
    await this.page.keyboard.press("Meta+KeyA");
    await this.page.keyboard.press("Backspace");
    await this.wait(250);

    // Split on mentions, keeping them: ["ask ", "@attest_tag", " about …"].
    for (const part of text.split(/(@[A-Za-z0-9._-]+)/).filter(Boolean)) {
      if (!part.startsWith("@")) {
        await box.pressSequentially(part, { delay: 38 });
        continue;
      }
      await box.pressSequentially(part, { delay: 55 });
      // Commit the mention with Tab ONLY while the autocomplete is actually
      // open. Tab is not a "commit" key — it is the focus key, and the list
      // merely intercepts it. Press it with no list up and focus leaves the
      // composer entirely; the Enter that follows then goes to whatever landed
      // in focus instead, so nothing sends, the text stays in the draft, and
      // the next part types on top of it. That is how one take ended with four
      // questions concatenated in an unsent composer.
      // Wait for an OPTION, not for the listbox. `[role="listbox"]` matches
      // more than one thing on a Slack page, and `.first()` can resolve to a
      // different one that is never visible — so the list looked shut while
      // the mention picker was open right there. An option is the thing we
      // actually need, and there is only one kind of it here.
      const option = this.page.getByRole("option").first();
      const opened = await option
        .waitFor({ state: "visible", timeout: 8_000 })
        .then(() => true)
        .catch(() => false);
      if (opened) {
        await this.wait(700);
        await box.press("Tab");
        await this.wait(350);
      } else {
        // No list: the handle matched nobody. Say so now — the alternative is
        // a message that posts as plain text and a bot that never answers.
        throw new Error(
          `No mention autocomplete for "${part}". That handle matches no one ` +
            `in this workspace, so it would post as literal text and the bot ` +
            `would never hear it. Check VIDEO_BOT_NAME.`,
        );
      }
    }

    await this.wait(420); // let the finished line read before it vanishes
    if (opts.send !== false) {
      // Do NOT click to refocus. A click places the caret wherever it lands,
      // and landing it in or beside the mention token reopens the autocomplete
      // — at which point Enter picks from the list instead of sending, and the
      // message sits in the composer looking as though Enter did nothing.
      //
      // Focus without moving the caret, put it at the end, and dismiss any
      // list that is still up before committing.
      await box.focus();
      await box.press("End");
      await this.wait(200);
      if (await this.page.getByRole("option").first().isVisible().catch(() => false)) {
        await box.press("Escape");
        await this.wait(250);
      }
      await box.press("Enter");
      await this.wait(900);

      // Verify it actually sent, by checking our own words are gone rather
      // than that the box is "empty". An emptied Quill composer is not an
      // empty string — it keeps a placeholder node and zero-width characters,
      // so an emptiness test fires on a message that sent perfectly well.
      // What cannot be there afterwards is the text we just typed.
      const tail = this.fragment(text);
      const left = ((await box.textContent().catch(() => "")) ?? "").replace(/\s+/g, " ");
      if (tail.test(left)) {
        throw new Error(
          `The message did not send — the composer still holds "${left.slice(0, 80)}".\n` +
            `Enter went somewhere other than the composer.`,
        );
      }
    }
  }

  /**
   * Rows to read a reply from: every message row on screen.
   *
   * Deliberately not scoped to a pane. Scoping it to the thread pane meant
   * guessing that pane's hook, and the guess was wrong — so it matched
   * nothing, fell back to the channel, and read a view the bot never posts
   * into. Taking the last bot-authored row out of everything on screen is
   * correct whether the answer is in a thread or in the channel, and needs no
   * hook for the container at all.
   */
  private replyRows(): Locator {
    return this.page.locator(SEL.message);
  }

  /**
   * Open a thread on the last message in the channel — the one we just sent.
   *
   * Slack only paints the hover toolbar while the pointer is over the row, and
   * `stage.click` glides the pointer there anyway, so the hover is a side
   * effect of how we click rather than something to arrange separately.
   */
  async openThreadOnLast(): Promise<void> {
    const last = this.page.locator(SEL.message).last();
    await last.scrollIntoViewIfNeeded();
    const box = await last.boundingBox();
    if (box) await this.moveTo(box.x + box.width - 120, box.y + 14);
    await this.wait(400);
    await this.click(this.page.locator(SEL.replyInThread).first(), {
      settle: 1_000,
    });
    await this.wait(1_200);
  }

  /**
   * Ask the bot something in the current channel and wait for the answer.
   *
   * Composing and then reading the channel does not work, and the way it
   * fails is instructive: the bot replies **in a thread** on the message that
   * mentioned it — "threads are the unit of conversation" is the product's
   * first principle, not a detail — so the channel view never gains a reply at
   * all. The server log shows a completed turn with tokens spent while the
   * recorder sits there reporting zero characters, which reads exactly like a
   * bot that is ignoring you.
   *
   * So: post, open the thread the reply is growing in, then read it. That is
   * also the better shot. The answer arriving inside a thread, beside the
   * question, is the thing the product actually looks like.
   */
  async ask(text: string, opts: { timeoutMs?: number; settleMs?: number; requireFooter?: boolean } = {}): Promise<string> {
    await this.send(text);
    return this.awaitReply(text, opts);
  }

  /** What `send` posted and saw, for `awaitReply` to pick the right row. */
  private sent: { text: string; before: string; rows: number; at: number } | null = null;

  /** Rows in the channel carrying the last few words of `text`. */
  private rowsSaying(text: string): Locator {
    return this.page.locator(SEL.message).filter({ hasText: this.fragment(text) });
  }

  /**
   * Post the question and return as soon as it has gone. The half of `ask`
   * that belongs on camera; `awaitReply` is the half that may not.
   */
  async send(text: string): Promise<void> {
    // Close whatever thread is open first. Slack restores the last thread pane
    // on load, so a fresh part can start with the previous answer on screen —
    // and then everything below reads that pane instead of the channel.
    await this.closeThread();
    // Snapshot BEFORE the question goes out. Taking it later — after the
    // thread is opened — captures the answer we are waiting for and then waits
    // for that to change, which it never does.
    const before = await this.lastBotText();
    // And count the rows that already say what we are about to say. A take
    // whose question is still in the channel from an earlier take — same
    // words, already answered, bar already showing — is otherwise picked as
    // "our" row the instant it is looked for, and the wait then sits on the
    // old thread until it times out.
    const rows = await this.rowsSaying(text).count();
    const at = Date.now();
    await this.compose(text);
    this.sent = { text, before, rows, at };
    console.log(`      ask: sent +${((Date.now() - at) / 1000).toFixed(1)}s`);
  }

  /**
   * Wait for the bot to answer the message `send` posted, open its thread,
   * and return the settled reply text. The pane is left open on the answer.
   */
  async awaitReply(text: string, opts: { timeoutMs?: number; settleMs?: number; requireFooter?: boolean } = {}): Promise<string> {
    const sent = this.sent?.text === text ? this.sent : null;
    const t0 = sent?.at ?? Date.now();
    const at = (label: string) => console.log(`      ask: ${label} +${((Date.now() - t0) / 1000).toFixed(1)}s`);
    const before = sent?.before ?? (await this.lastBotText());

    // Our row is the one that was not there before `send`. Then wait for the
    // reply bar on THAT row — not on the last bar in the channel. Once a
    // channel holds two answered questions, the last bar is the previous
    // question's, it is visible immediately, and clicking it opens the
    // previous thread: the recorder then sits on a reply it was told to
    // ignore and times out while the log shows the new answer arriving in
    // the other thread.
    const rows = this.rowsSaying(text);
    if (sent) {
      const deadline = Date.now() + 30_000;
      while ((await rows.count()) <= sent.rows && Date.now() < deadline) await this.wait(300);
    }
    // …and require the row to carry a bar. When a thread pane is open its
    // first row is a copy of the parent WITHOUT a bar, and it matches the
    // same text; `.last()` picked that copy and waited on nothing.
    const mine = rows.filter({ has: this.page.locator(SEL.viewThread) }).last();
    const bar = mine.locator(SEL.viewThread).first();
    await bar.waitFor({ state: "visible", timeout: 120_000 }).catch(() => {});
    if (await bar.isVisible().catch(() => false)) {
      at("reply bar on our row");
      await this.click(bar, { settle: 1_200 });
      await this.ensureThreadLoaded();
      at("thread open");
    } else {
      at("no reply bar on our row after 120s");
      // No thread on our message: it may have answered in-channel, or not
      // started. Let waitForBotReply decide so the error is about the reply.
      await this.wait(1_000);
    }
    const reply = await this.waitForBotReply({ ...opts, ignore: before });
    at(`reply settled (${reply.length} chars)`);
    return reply;
  }

  /**
   * Is a thread pane open beside the channel? `threads_flexpane` exists only
   * while one is — which is why a static scout reports it missing.
   */
  private async paneOpen(): Promise<boolean> {
    return this.page.locator('[data-qa="threads_flexpane"]').first().isVisible().catch(() => false);
  }

  /**
   * Close the thread pane if one is open. Escape does it when focus is in the
   * pane, and does nothing when focus is in the composer — which is where
   * `compose` leaves it — so the pane's own close button is the fallback.
   */
  async closeThread(): Promise<void> {
    await this.page.keyboard.press("Escape").catch(() => {});
    await this.wait(400);
    if (!(await this.paneOpen())) return;
    const close = this.page.locator('[data-qa="close_flexpane"]').first();
    if (await close.isVisible().catch(() => false)) {
      await close.click().catch(() => {});
      await this.wait(500);
    }
    if (await this.paneOpen()) console.warn("      ! a thread pane is still open");
    else await this.wait(300);
  }

  /**
   * The last few words of a message as a regex that tolerates whatever sits
   * between them. Built from words, not characters, because Slack renders the
   * punctuation we strip: a fragment of "land and what" never matches a row
   * reading "land, and what", the reply bar is never found, and the recorder
   * waits on the wrong thread. `\W+` between words absorbs the comma.
   *
   * Public: a guide that waits for the reply bar on its own row wants the
   * same rule, and a copy of it drifts.
   */
  fragment(text: string): RegExp {
    const words = text
      .replace(/@[A-Za-z0-9._-]+/g, " ")
      .replace(/[^A-Za-z0-9 ]/g, " ")
      .split(/\s+/)
      .filter(Boolean)
      .slice(-5);
    return new RegExp(words.map((w) => w.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")).join("\\W+"), "i");
  }

  /**
   * Wait for the bot's answer to finish arriving.
   *
   * Never waits for particular words. The answer is model output: it is
   * different on every take, and three takes of the same question is exactly
   * how you discover that. What is stable is the shape of the transport — text
   * arrives in bursts while the run is live and then stops — so this polls the
   * last bot message and calls it done when its length has held still for
   * STREAM_SETTLE_MS.
   *
   * Returns the text, for a scene that wants to point at part of it. Do not
   * assert on it.
   */
  async waitForBotReply(
    opts: { settleMs?: number; timeoutMs?: number; ignore?: string; requireFooter?: boolean } = {},
  ): Promise<string> {
    const settleMs = opts.settleMs ?? STREAM_SETTLE_MS;
    const timeoutMs = opts.timeoutMs ?? STREAM_TIMEOUT_MS;
    // Optional, and off by default. The footer looked like the product's own
    // "done" signal, and it is — for long answers. Held cards and short
    // acknowledgements ("remembered") carry none, and a wait that insists on
    // one times out on three kinds of reply that were already complete. The
    // signal that is present in every case is the one below: while the bot
    // works, its row says "is working…"; when that line is gone and the text
    // has stopped changing, it is finished.
    const requireFooter = opts.requireFooter ?? false;
    const started = Date.now();
    // Anything already on screen when we start is a PREVIOUS answer, and
    // returning it is how a scene "succeeds" in two seconds having waited for
    // nothing. Three parts were filmed that way — the shot showed a stale
    // reply to a question asked minutes earlier, and the exit code was 0.
    // What was on screen before the question. `ask` passes the snapshot it
    // took at the right moment; a bare call falls back to reading it now.
    const before = opts.ignore ?? (await this.lastBotText());
    let lastText = "";
    let lastChange = Date.now();
    let lastBeat = Date.now();
    const trace = (what: string) =>
      console.log(`      wait: ${what} +${((Date.now() - started) / 1000).toFixed(1)}s`);

    for (;;) {
      const text = await this.lastBotText();
      if (Date.now() - lastBeat > 10_000) {
        lastBeat = Date.now();
        trace(`still waiting · ${text.length} chars · same as before: ${text === before} · working: ${await this.lastBotIsWorking()}`);
      }
      // Ignore the reply that was there before the question was asked.
      if (text === before) {
        if (Date.now() - started > timeoutMs) {
          throw new Error(
            `The bot did not post a NEW reply in ${Math.round(timeoutMs / 1000)}s.\n` +
              `What is on screen is the answer that was already there before ` +
              `the question was asked, so the question never reached it — ` +
              `check the message actually posted and carries a highlighted ` +
              `@${this.botName}.`,
          );
        }
        await this.wait(400);
        continue;
      }
      if (text !== lastText) {
        trace(`text changed → ${text.length} chars "${text.replace(/\s+/g, " ").slice(0, 50)}"`);
        lastText = text;
        lastChange = Date.now();
      }
      if (lastText && Date.now() - lastChange >= settleMs) {
        const working = await this.lastBotIsWorking();
        if (!working && (!requireFooter || (await this.lastBotHasFooter()))) return lastText;
        // Stable but still marked as working: the status placeholder. Wait.
      }
      if (Date.now() - started > timeoutMs) {
        throw new Error(
          `The bot did not finish a reply in ${Math.round(timeoutMs / 1000)}s. ` +
            `Got ${lastText.length} characters` +
            (lastText ? ` and its row still said "is working"` : ``) +
            `.\n` +
            `If it got NOTHING: the mention probably went in as plain text — ` +
            `see the note on compose(). Check the message on screen shows a ` +
            `highlighted @${this.botName} and not the bare characters.`,
        );
      }
      await this.wait(400);
    }
  }

  /** The newest message row whose sender is the bot, or null. */
  /**
   * The sender named on a row, or null when the row shows none.
   *
   * `count()` first, because `textContent()` on a locator that matches
   * nothing WAITS for it — the default thirty seconds — before the catch
   * hands back null. A grouped row has no name by design (the bot's "Edit
   * what I remember here" follow-up is one), and every walk below that met
   * one paid thirty seconds per call: a reply on screen the whole time took
   * two and a half minutes to "settle".
   */
  private async senderOf(row: Locator): Promise<string | null> {
    const name = row.locator(SEL.messageSender).first();
    if ((await name.count().catch(() => 0)) === 0) return null;
    return name.textContent({ timeout: 2_000 }).catch(() => null);
  }

  private async lastBotRow(): Promise<Locator | null> {
    const rows = this.replyRows();
    const n = await rows.count().catch(() => 0);
    const norm = (v: string) => v.toLowerCase().replace(/[^a-z0-9]/g, "");
    // Slack hides the sender on a message that follows another from the same
    // author, so a row with no name belongs to whoever is named above it.
    // Walking backwards, the newest unnamed row is the answer IF the first
    // named row we reach is the bot's. Returning the named row instead — as
    // this used to — hands back the bot's FIRST message of a run, and a wait
    // for "something new" then never sees the result that arrived after it.
    // A fix job's result is exactly that: posted minutes after the card,
    // grouped under it, nameless.
    let newestUnnamed: Locator | null = null;
    for (let i = n - 1; i >= 0 && i > n - 12; i--) {
      const row = rows.nth(i);
      const sender = await this.senderOf(row);
      if (!sender || !sender.trim()) {
        newestUnnamed ??= row;
        continue;
      }
      const isBot = norm(sender).includes(norm(this.botName));
      if (!isBot) return null;
      return newestUnnamed ?? row;
    }
    return null;
  }

  /**
   * Slack sometimes fails to load a thread on the first try and shows
   * "Couldn't load thread" with a Try Again button — on camera, in place of
   * the answer. Press it, a few times if needed.
   */
  private async ensureThreadLoaded(): Promise<void> {
    for (let i = 0; i < 4; i++) {
      await this.wait(900);
      const broken = this.page.getByText(/couldn.t load thread/i).first();
      if (!(await broken.isVisible().catch(() => false))) return;
      await this.click(this.page.getByRole("button", { name: /try again/i }).first(), { settle: 1_500 });
    }
    throw new Error("Slack could not load the thread after four tries.");
  }

  /**
   * Open the thread on an earlier message of ours, found by its own words.
   * For a part that picks up a conversation a previous part started — the
   * fix job's result lands minutes after the confirm, and that wait is the
   * one thing in the walkthrough not worth filming.
   */
  async openThreadOf(text: string): Promise<void> {
    await this.page.keyboard.press("Escape").catch(() => {});
    await this.wait(400);
    // Only a row with a bar: an open pane's copy of the parent has none.
    const row = this.page
      .locator(SEL.message)
      .filter({ hasText: this.fragment(text) })
      .filter({ has: this.page.locator(SEL.viewThread) })
      .last();
    const bar = row.locator(SEL.viewThread).first();
    await bar.waitFor({ state: "visible", timeout: 20_000 });
    await this.click(bar, { settle: 1_200 });
    await this.ensureThreadLoaded();
  }

  /**
   * Has the reply finished? The product's own signal is the footer every
   * channel reply ends with — model, tokens, cost, and a "Configure" link —
   * which is rendered only once the answer is complete. While the bot works,
   * its row holds a status placeholder ("Reading a thread", "is working…")
   * that is short, stable, and exactly what a "stopped growing" test accepts.
   * One take ended every Slack scene on that placeholder; the answers arrived
   * after the camera had moved on.
   */
  private async lastBotHasFooter(): Promise<boolean> {
    const row = await this.lastBotRow();
    if (!row) return false;
    const text = (await row.innerText().catch(() => "")) ?? "";
    return /\bConfigure\b/.test(text);
  }

  /** Is the newest bot row still the in-progress placeholder? */
  private async lastBotIsWorking(): Promise<boolean> {
    const row = await this.lastBotRow();
    if (!row) return false;
    const text = (await row.innerText().catch(() => "")) ?? "";
    return /\bis working\b/i.test(text);
  }

  /** Text of the newest message whose sender is the bot, or "". */
  /** Public: a scene that replies inside a thread wants the same snapshot `send` takes. */
  async lastBotText(): Promise<string> {
    const rows = this.replyRows();
    const n = await rows.count().catch(() => 0);
    for (let i = n - 1; i >= 0 && i > n - 12; i--) {
      const row = rows.nth(i);
      const sender = await this.senderOf(row);
      // Slack omits the sender on consecutive messages from one author, so a
      // row with no name belongs to whoever was named above it. Walking
      // backwards, the first *named* row we hit decides.
      // Compare loosely. The handle is `attestTagTesting`, the row may render
      // "Attest Tag Testing", and an exact match on either spelling misses the
      // other — which looks exactly like a bot that did not reply.
      const norm = (v: string) => v.toLowerCase().replace(/[^a-z0-9]/g, "");
      const isBot = sender ? norm(sender).includes(norm(this.botName)) : null;
      if (isBot === false) return "";
      if (isBot) {
        return (
          (await row
            .locator(SEL.messageText)
            .allTextContents()
            .catch(() => []))
            .join("\n")
            .trim() ?? ""
        );
      }
    }
    return "";
  }

  /**
   * Press a button inside a Block Kit message — the confirm card a write stops
   * on. Matched on its visible label, which is ours (we author the blocks), so
   * unlike everything else in this file it is a selector we control.
   */
  async clickCardButton(label: string | RegExp): Promise<void> {
    const button = this.page
      .locator(SEL.message)
      .last()
      .getByRole("button", { name: label });
    await this.click(button, { settle: 1_200 });
  }
}

/** Quote a channel name for an attribute selector. */
function cssEscape(s: string): string {
  return s.replace(/["\\]/g, "\\$&");
}
