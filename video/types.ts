import type { Page } from "playwright";
import type { Stage } from "./stage";
import type { SlackStage } from "./slack";

/**
 * What a scene is handed.
 *
 * Two stages over ONE page, not two pages. The product is half a Slack app and
 * half a console, and a walkthrough that could not cross between them would be
 * describing the product rather than showing it — so a scene can drive either,
 * and the recording never notices the difference. See loadStorageState() in
 * render.ts for why one page can be signed into both.
 */
export type Surfaces = {
  page: Page;
  /** The real Slack web client. */
  slack: SlackStage;
  /** The admin console. `app`, not `console`, which is taken. */
  app: Stage;
  /**
   * Hold, without saying which surface is holding. Both stages have their own
   * `wait`, but a scene that just lets the shot breathe belongs to neither,
   * and `s.app.wait()` in the middle of a Slack scene reads like a mistake.
   */
  wait: (ms: number) => Promise<void>;
  /**
   * Run something the viewer should not sit through, and cut it out of the
   * finished video.
   *
   * The reason this exists: Slack's first delivery of an event to this bot
   * fails more often than not (through the tunnel it is recorded behind — see
   * LEARNINGS.md), and Slack retries a minute later. The reply then lands
   * anywhere from four seconds to seventy after the question, and a scene
   * that waits for it on camera holds a still frame for the difference. The
   * wait starts only once this scene's line has been spoken, so no narration
   * is ever cut; the frames between the call and its return are dropped on
   * encode and every later offset moves up to close the gap.
   */
  offCamera: (fn: () => Promise<void>) => Promise<void>;
};

/** Values a scene stashes for later scenes — a channel id, an invite link. */
export type SceneContext = Record<string, string>;

export type Scene = {
  /**
   * Starts a chapter in the player's chapter list at this scene's offset.
   * Omit to fold the scene into the chapter above it.
   */
  chapter?: string;
  /**
   * The narration. This alone sets how long the scene stays on screen: the
   * recorder measures the synthesised audio and holds the shot for exactly
   * that long, so pacing is edited by editing the words.
   *
   * A scene that waits on the bot is the exception worth planning for. The
   * answer takes as long as it takes, and `act` returning late just holds the
   * shot in silence — so write those lines to the length `npm run scout --ask`
   * measured, not to the length they read well at.
   */
  say: string;
  /** Drives the product while the line is spoken. Omit for a held shot. */
  act?: (s: Surfaces, ctx: SceneContext) => Promise<void>;
};

export type Guide = {
  /** Slug — names the mp4, poster and manifest in the output directory. */
  id: string;
  /**
   * Title for the opening and closing cards, and for the page. Required, unlike
   * the rig this was ported from: there the card read its title out of the
   * app's own catalogue, which meant registering a guide before its first
   * render existed broke the recorder's boot. One field on the guide is worth
   * more than the indirection was.
   */
  title: string;
  /**
   * What the opening card lists, when the scenes below are not the whole
   * story. A stitched part knows only its own two chapters, and a card
   * promising two chapters in front of a twenty-minute video is worse than no
   * card — the list is there so a viewer can tell in one frame whether this is
   * the video they wanted.
   *
   * Keep it short. The card holds for a couple of seconds, so six headings
   * read and twelve do not.
   */
  cardChapters?: string[];
  /**
   * Runs before the part synthesises a line or opens a browser. For waiting
   * on something the previous part started — a fix job takes minutes, and
   * holding a shot for that is dead air; waiting here costs nothing on
   * camera and no narration.
   */
  before?: () => Promise<void>;
  scenes: Scene[];
};

/**
 * A walkthrough too long to record in one take.
 *
 * The parts are a hard dependency chain, not a preference: the first one
 * creates the channel and the connection every part after it uses. State
 * crosses that boundary through a ctx file on disk, so a middle part can be
 * re-recorded on its own.
 */
export type LongForm = {
  id: string;
  title: string;
  cardChapters: string[];
  /** Scratch directory for the part renders. Gitignored. */
  partsDir: string;
  /** Where the stitched result lands. */
  outDir: string;
  parts: Guide[];
  /**
   * Which frame goes on the poster card: a chapter by name and seconds into
   * it. Omitted, the card takes a beat into the second chapter. Pick a frame
   * where the product is doing the thing — an answer in a thread — rather
   * than a page of small text; the card's title carries the words.
   */
  poster?: { chapter: string; offset?: number };
};
