import { BRAND } from "./brand";

// The title, seam and end cards.
//
// These are whole documents handed to `page.setContent`, not overlays drawn
// over the app. That is forced by *when* they have to exist: Playwright starts
// recording the moment the page is created, and at that point there is no
// document to overlay — which is why the opening second of a video made the
// other way is blank. `setContent` paints inline HTML with no navigation and
// no network, so the very first frame is already the card.
//
// The palette is transcribed from the light theme in the site's src/app/globals.css
// (the attesttag-website repository) rather than read from it: a standalone document has no access to the site's
// tokens, and pulling in Tailwind to render three cards would be a build step
// in the middle of a recorder. If those base colours change, change them here.
const C = {
  bg: "#f8f8fc",
  fg: "#191c2b",
  muted: "#5c6070",
  primary: "#5a50c8",
  accent: "#edecf8",
} as const;

const FONT =
  '-apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, sans-serif';
// The name is a Slack handle, and the site sets it in mono everywhere it
// appears. A card that set it in the body face would be the one place on the
// product where it reads as a word rather than as something you type.
const MONO = 'ui-monospace, SFMono-Regular, "SF Mono", Menlo, monospace';

/**
 * The Attest Spiral, inline. Same two paths as the site's
 * src/components/core/logo-mark.tsx — the bubble in ink, the check in
 * iris — with the hexes baked in, since there is no stylesheet here to inherit
 * a theme class from.
 */
const MARK = `
  <div class="mark">
    <svg class="spiral" viewBox="0 0 256 256" aria-hidden>
      <g fill="none" stroke-width="30" stroke-linecap="butt" stroke-linejoin="round">
        <path d="M210.9 112.7 A48 48 0 0 0 180 28 L76 28 A48 48 0 0 0 28 76 L28 180 A48 48 0 0 0 76 228 L180 228 A48 48 0 0 0 223.5 200.3" stroke="${C.fg}"/>
        <path d="M97.6 121.6 L140 172.2 L210.9 112.7" stroke="${C.primary}"/>
      </g>
    </svg>
    <span class="name">${BRAND.name}</span>
  </div>`;

function doc(body: string): string {
  return `<!doctype html><html><head><meta charset="utf-8"><style>
    * { box-sizing: border-box; }
    html, body { height: 100%; margin: 0; }
    body {
      display: flex; flex-direction: column; align-items: center;
      justify-content: center; gap: 22px; padding: 64px; text-align: center;
      background: ${C.bg}; color: ${C.fg}; font-family: ${FONT};
      -webkit-font-smoothing: antialiased;
    }
    .mark { display: flex; align-items: center; gap: 10px; }
    .spiral { width: 30px; height: 30px; }
    .name { font-family: ${MONO}; font-size: 17px; letter-spacing: -0.01em; }
    .rule { width: 44px; height: 2px; border-radius: 1px; background: ${C.primary}; }
    h1 {
      margin: 0; font-size: 40px; line-height: 1.15; font-weight: 600;
      letter-spacing: -0.02em; max-width: 22ch;
    }
    ol { list-style: none; margin: 6px 0 0; padding: 0; display: flex;
         flex-direction: column; gap: 9px; align-items: flex-start; }
    li { display: flex; align-items: center; gap: 10px; font-size: 15px; color: ${C.muted}; }
    .n {
      display: flex; width: 20px; height: 20px; border-radius: 4px;
      align-items: center; justify-content: center; background: ${C.accent};
      color: ${C.fg}; font-size: 11px; font-variant-numeric: tabular-nums;
    }
    p { margin: 0; font-size: 16px; line-height: 1.6; max-width: 36ch; color: ${C.muted}; }
    strong { color: ${C.fg}; font-weight: 600; }
    .mono { font-family: ${MONO}; font-size: 15px; color: ${C.fg}; }
  </style></head><body>${body}</body></html>`;
}

/**
 * Opening card: what this is, and the map of what is coming. The list earns
 * its place — it tells a viewer in one frame whether this is the video they
 * wanted, rather than after four minutes of finding out.
 */
export function titleCard(title: string, chapters: string[]): string {
  const items = chapters
    .map((name, i) => `<li><span class="n">${i + 1}</span>${name}</li>`)
    .join("");
  return doc(`
    ${MARK}
    <div class="rule"></div>
    <h1>${title}</h1>
    <ol>${items}</ol>`);
}

/**
 * The seam between two parts of a stitched walkthrough. Long videos are
 * recorded in parts so a failure costs one part rather than the whole take,
 * and every part still has to paint *something* at t0 — a part opening on a
 * blank document is the bug `setContent` exists to fix. This is what it
 * paints: the mark alone, held for about a second. Six repeats of the title
 * card would read as six videos rather than one.
 */
export function interstitialCard(): string {
  return doc(`${MARK}<div class="rule"></div>`);
}

/**
 * The poster: what a walkthrough shows before play is pressed. Title on the
 * left, a real frame from the recording set into a window on the right, cut
 * off by the edge the way a product shot is. `frame` is a data URL — the
 * card is a document with no server behind it.
 */
export function posterCard(opts: { title: string; meta: string; frame: string }): string {
  return doc(`
    <style>
      body.poster {
        display: grid; grid-template-columns: 560px 1fr; align-items: center;
        gap: 56px; padding: 0 0 0 88px; text-align: left; overflow: hidden;
      }
      .poster .left { display: flex; flex-direction: column; align-items: flex-start; gap: 22px; }
      .poster h1 { font-size: 54px; line-height: 1.1; max-width: 12ch; }
      .poster .meta { font-size: 18px; color: ${C.muted}; max-width: 30ch; line-height: 1.5; }
      .poster .right { position: relative; height: 100%; }
      .poster .window {
        position: absolute; top: 50%; left: 0; transform: translateY(-50%);
        width: 1040px; border-radius: 16px; overflow: hidden; background: #fff;
        border: 1px solid #e2e2ec; box-shadow: 0 34px 90px rgba(25, 28, 43, 0.20);
      }
      /* The frame is the recording's own 1440×900. The window's visible part
         is what is left of the canvas — about 736px — and it should run from
         the channel's left edge (Slack's rail and sidebar end at about a
         quarter of the frame) to the frame's right edge, so the header, the
         avatars and the thread pane with the answer are all on the card:
         scale the frame to 1000 and crop the left 264. */
      .poster .window img { display: block; width: 1000px; margin-left: -264px; }
    </style>
    <div class="left">
      ${MARK}
      <div class="rule"></div>
      <h1>${opts.title}</h1>
      <p class="meta">${opts.meta}</p>
    </div>
    <div class="right"><div class="window"><img src="${opts.frame}" alt=""></div></div>`)
    .replace("<body>", '<body class="poster">');
}

/**
 * The card between two parts of a stitched walkthrough, when the part has a
 * name: what the next few minutes are about, in a line, and a sentence on
 * what happens in them. The bare seam (`interstitialCard`) read as an empty
 * frame; a viewer skimming wants to know what is coming before it arrives.
 */
export function sectionCard(title: string, blurb?: string): string {
  return doc(`
    <style>
      h1.section { font-size: 34px; max-width: 26ch; }
      p.blurb { font-size: 17px; max-width: 44ch; }
    </style>
    ${MARK}
    <div class="rule"></div>
    <h1 class="section">${title}</h1>
    ${blurb ? `<p class="blurb">${blurb}</p>` : ""}`);
}

/** Closing card: somewhere deliberate to land, and where the rest lives. */
export function endCard(title: string): string {
  return doc(`
    ${MARK}
    <div class="rule"></div>
    <h1>${title}</h1>
    <p>Every walkthrough is at <span class="mono">${BRAND.domain}/walkthroughs</span>.</p>`);
}
