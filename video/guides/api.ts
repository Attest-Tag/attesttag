import { BRAND } from "../brand";
import type { Guide, LongForm, SceneContext, Surfaces } from "../types";
import { assertDb, assertWorkspaceState, outDir, partsDirFor } from "./shared";

// "The API, and a key that is a person" — the walkthrough.
//
// One part, on purpose. `ctx` lives for one render — render.ts makes a fresh
// one per part — and the key minted at the start has to be the key the
// request at the end carries, so this is a single take, with the chapters
// carrying the structure a split would have.
//
// It needs nothing beyond the workspace the deep dive connected, and reads no
// environment of its own.
//
// What a take leaves behind: one api_keys row named "Nightly document sync",
// revoked before the take ends. Revoked rows stay in the table, so a second
// take films the first take's row underneath the new one — which is why the
// row this script works on is found by the key's own prefix, never its name.
//
// The key is on camera for a few seconds, in the console's one-time reveal.
// It is revoked on camera before the take ends, and it is never logged.
//
// Two rules this file obeys, from deep-dive.ts: a scene holds for as long as
// its line takes to speak, and every console write ends in assertDb.

const partsDir = partsDirFor("api");
const TITLE = "The API, and a key that is a person";

const KEY_NAME = "Nightly document sync";
/** The one harmless GET the take makes for real: numbers only, no address in the reply. */
const ENDPOINT = "/v1/usage";

// ---------------------------------------------------------------------------
// Helpers local to this walkthrough.

/** What the console shows of a key: "atk1." and six characters (api_keys.go apiKeyDisplayPrefix). */
const prefixOf = (raw: string) => raw.slice(0, "atk1.".length + 6);

/** The minted key's row, found by its prefix — the name repeats across takes. */
const keyRow = (s: Surfaces, ctx: SceneContext) =>
  s.page.getByRole("row").filter({ hasText: prefixOf(ctx.apiKey) }).first();

// The card's palette, transcribed from cards.ts (which keeps it private) for
// the same reason cards.ts transcribes it from the site: a document handed to
// setContent has no stylesheet to inherit from. If those base colours change,
// change these. The two lifted tints are the iris and the ink, lightened for
// a dark ground that the light cards never need.
const C = {
  bg: "#f8f8fc",
  fg: "#191c2b",
  muted: "#5c6070",
  accent: "#edecf8",
  /** The iris, lifted for legibility on ink. */
  irisLight: "#c9c4f4",
} as const;
const FONT = '-apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, sans-serif';
const MONO = 'ui-monospace, SFMono-Regular, "SF Mono", Menlo, monospace';

function escapeHtml(s: string): string {
  return s.replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c] as string);
}

/** JSON with its keys and strings tinted. Escaped first; the tint is only markup around it. */
function paintJson(pretty: string): string {
  return escapeHtml(pretty)
    .replace(/^(\s*)(&quot;[^&]*?&quot;)(:)/gm, '$1<span class="k">$2</span>$3')
    .replace(/(:\s)(&quot;.*?&quot;)/g, '$1<span class="s">$2</span>');
}

/**
 * A terminal, painted with setContent the way the title cards are: the
 * request as a shell would show it, the key cut down to the prefix the
 * console itself shows, and the response exactly as the server sent it.
 * Nothing on it is a mock — the body is the real reply to the real request
 * the scene just made.
 */
function terminalCard(o: { origin: string; prefix: string; status: number; body: string }): string {
  let pretty = o.body;
  try {
    pretty = JSON.stringify(JSON.parse(o.body), null, 2);
  } catch {
    // Not JSON: shown as it came.
  }
  const curl = `curl ${o.origin}${ENDPOINT} \\\n  -H "Authorization: Bearer ${o.prefix}…"`;
  return `<!doctype html><html><head><meta charset="utf-8"><style>
    * { box-sizing: border-box; }
    html, body { height: 100%; margin: 0; }
    body {
      display: flex; align-items: center; justify-content: center; padding: 56px;
      background: ${C.bg}; color: ${C.fg}; font-family: ${FONT};
      -webkit-font-smoothing: antialiased;
    }
    .term {
      width: 1120px; max-height: 100%; overflow: hidden; border-radius: 14px;
      background: ${C.fg}; color: ${C.bg}; box-shadow: 0 34px 90px rgba(25, 28, 43, 0.22);
    }
    .bar {
      display: flex; align-items: center; gap: 8px; padding: 12px 16px;
      background: rgba(255,255,255,.06); font-family: ${MONO}; font-size: 13px; color: ${C.accent};
    }
    .dot { width: 11px; height: 11px; border-radius: 50%; background: rgba(255,255,255,.18); }
    .title { margin-left: 8px; opacity: .8; }
    pre {
      margin: 0; padding: 22px 26px 26px; font-family: ${MONO}; font-size: 15px; line-height: 1.5;
      white-space: pre-wrap; word-break: break-all;
    }
    .p { color: ${C.muted}; }
    .k { color: ${C.irisLight}; }
    .s { color: ${C.accent}; }
    .status { color: ${C.irisLight}; }
  </style></head><body>
    <div class="term">
      <div class="bar"><span class="dot"></span><span class="dot"></span><span class="dot"></span><span class="title">${escapeHtml(BRAND.name)} — ${escapeHtml(ENDPOINT)}</span></div>
      <pre><span class="p">$</span> ${escapeHtml(curl)}
<span class="status">HTTP ${o.status}</span>
${paintJson(pretty)}
<span class="p">$</span> ▌</pre>
    </div>
  </body></html>`;
}

/**
 * Replace the recording host with the product's public origin wherever the
 * page prints it. The reference page builds its shell setup from the
 * browser's own location ("export BASE=https://…"), which on a local take
 * names the machine this was filmed on — the leak README warns about and the
 * one the terminal card above already avoids. The clipboard still gets the
 * real text; only what is on camera changes, and only where the origin
 * appears verbatim.
 */
async function maskRecordingOrigin(s: Surfaces): Promise<void> {
  const from = new URL(s.page.url()).origin;
  if (from === BRAND.appUrl) return;
  await s.page.evaluate(
    ({ from, to }) => {
      const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
      const hits: Text[] = [];
      for (let n = walker.nextNode(); n; n = walker.nextNode()) {
        if ((n as Text).data.includes(from)) hits.push(n as Text);
      }
      for (const t of hits) t.data = t.data.split(from).join(to);
    },
    { from, to: BRAND.appUrl },
  );
}

// --- 1. key --------------------------------------------------------------------

const key: Guide = {
  id: "1-key",
  title: TITLE,
  before: async () => {
    assertWorkspaceState();
  },
  scenes: [
    {
      chapter: "Make a key",
      say: `Scripts do not sign in; they carry a key. Keys are made here, under Developer, and open one door: the API under slash v one. Whatever a key can reach, a browser session could reach too; never more.`,
      act: async (s) => {
        await s.app.goto("/developer/api-keys");
        await s.wait(1_200);
        // api-keys-panel.tsx: the header's "Create key"; the empty state
        // repeats it, so the header's is `.first()`.
        await s.app.click(s.page.getByRole("button", { name: /^create key$/i }).first(), { settle: 900 });
        await s.page.locator("#key-name").waitFor({ state: "visible", timeout: 15_000 });
      },
    },
    {
      say: `A name, so you know what to revoke later. An expiry. Never is the default, for a reason: an expiry nobody diarised is an outage on a date nobody remembers. Thirty days here.`,
      act: async (s, ctx) => {
        await s.app.typeVerified("#key-name", KEY_NAME);
        await s.app.click("#key-expiry", { settle: 800 });
        await s.app.click(s.page.getByRole("option", { name: /in 30 days/i }).first(), { settle: 600 });
        // The dialog's submit repeats the header's label, so it is scoped.
        await s.app.click(s.page.getByRole("dialog").getByRole("button", { name: /^create key$/i }));
        // The reveal: the raw key in a <code>, once (api-keys-panel.tsx). The
        // table shows earlier takes' keys in <code> too, as a prefix and an
        // ellipsis, so the match wants the whole key. Read into ctx for the
        // request later; never printed.
        const shown = s.page.locator("code").filter({ hasText: /^atk1\.[A-Za-z0-9_-]{40,}$/ }).first();
        await shown.waitFor({ state: "visible", timeout: 20_000 });
        const raw = ((await shown.textContent()) ?? "").trim();
        if (!/^atk1\.[A-Za-z0-9_-]{40,}$/.test(raw)) throw new Error("The reveal did not show a key of the expected shape.");
        ctx.apiKey = raw;
        assertDb(`select count(*) from api_keys where key_prefix='${prefixOf(raw)}' and revoked_at is null;`, 1, "The API key was not created");
        await s.wait(600);
      },
    },
    {
      chapter: "Shown once",
      say: `It is shown once. Only a hash is stored, so it cannot be shown again; lose it and the answer is a new key. Copy it, and the banner stays until you say you have.`,
      act: async (s, ctx) => {
        await s.app.click(s.page.getByRole("button", { name: "Copy key" }), { settle: 1_200 });
        await s.app.click(s.page.getByRole("button", { name: /saved it/i }), { settle: 1_000 });
        await s.app.point(keyRow(s, ctx));
      },
    },
    {
      say: `The row: the name, a prefix that is enough to recognise it and useless to use, who it acts as, when it was last used, and when it expires.`,
      act: async (s, ctx) => {
        await s.app.point(keyRow(s, ctx).getByRole("cell").nth(1));
        await s.wait(700);
        await s.app.point(keyRow(s, ctx).getByRole("cell").nth(4));
        await s.wait(600);
      },
    },
    {
      say: `A key is a person, not a role. It carries the authority of the account that made it and nothing more, resolved again on every request; when that person leaves, their scripts stop with them.`,
      act: async (s, ctx) => {
        await s.app.point(keyRow(s, ctx).getByRole("cell").nth(2));
        await s.wait(1_000);
      },
    },
    {
      say: `No sign-in policy or two-factor applies to a key; a nightly job has no keyboard, and cannot be shown an enrolment screen. Its expiry and this button bound it instead.`,
      act: async (s, ctx) => {
        await s.app.point(keyRow(s, ctx).getByRole("button", { name: /^revoke$/i }));
        await s.wait(1_000);
      },
    },
    {
      chapter: "The reference",
      say: `The reference. Two lines of shell setup first: where the console is, and the key. Copy them, and every example below runs as it is written.`,
      act: async (s) => {
        await s.app.goto("/developer/api-reference");
        await s.wait(1_000);
        await maskRecordingOrigin(s);
        // api-reference.tsx: a CopyButton labelled "Copy shell setup".
        await s.app.click(s.page.getByRole("button", { name: "Copy shell setup" }), { settle: 1_000 });
        await s.app.scroll(420, 16);
      },
    },
    {
      say: `Every endpoint, with the request to copy and the response to expect. Documents, memory, artifacts, routines, jobs, activity and spend: reads where reading is safe, and one write that earns the API, keeping documents in step with a source of truth somewhere else. Spend and totals is one GET.`,
      act: async (s) => {
        // Each row is a button named by its method, path and summary; it
        // opens in place with a "Request" and a "Response" snippet.
        await s.app.click(s.page.getByRole("button", { name: /\/v1\/usage\b/ }).first(), { settle: 1_000 });
        await s.app.point(s.page.getByRole("button", { name: "Copy request" }).first());
        await s.wait(1_000);
      },
    },
    {
      chapter: "A real request",
      say: `Now the request itself, with the key from a minute ago. One GET, and the answer: this month's spend against the budget, the plan, and what the bot has to work with. A terminal, with the real reply.`,
      act: async (s, ctx) => {
        const bearer = ctx.apiKey;
        if (!bearer) throw new Error("No key in ctx — the reveal scene did not read one.");
        // From the console's own origin: the API is served from the root of
        // the host the console sits under (/admin), so there is no second
        // host and no CORS. The key travels in the header, never in the URL.
        const res = await s.page.evaluate(
          async (p: { endpoint: string; bearer: string }) => {
            const r = await fetch(location.origin + p.endpoint, {
              headers: { Authorization: "Bearer " + p.bearer },
              cache: "no-store",
            });
            return { origin: location.origin, status: r.status, body: await r.text() };
          },
          { endpoint: ENDPOINT, bearer },
        );
        if (res.status !== 200) throw new Error(`${ENDPOINT} answered ${res.status}: ${res.body.slice(0, 200)}`);
        // The request went to the instance being recorded; the card prints the
        // product's public origin instead of res.origin. Playwright records no
        // address bar, and this line was the one frame that named the host —
        // README, "The one leak path left is a link the bot posts".
        await s.page.setContent(terminalCard({ origin: BRAND.appUrl, prefix: prefixOf(bearer), status: res.status, body: res.body }));
        // The server stamps last_used_at on a key's first use
        // (store_api_keys.go TouchAPIKey): the request authenticated as this
        // key, or it did not.
        assertDb(
          `select count(*) from api_keys where key_prefix='${prefixOf(bearer)}' and last_used_at is not null;`,
          1,
          "The request did not authenticate with the new key",
        );
        await s.wait(2_000);
      },
    },
    {
      chapter: "Revoke it",
      say: `A secret that appears on camera dies on camera. Revoke it: anything still using it stops now, and it cannot be turned back on. The same access as the person who made it, bounded by an expiry and this button.`,
      act: async (s, ctx) => {
        await s.app.goto("/developer/api-keys");
        await s.wait(900);
        await s.app.click(keyRow(s, ctx).getByRole("button", { name: /^revoke$/i }), { settle: 900 });
        // confirm-dialog.tsx: an AlertDialog whose action repeats the label.
        await s.app.click(s.page.getByRole("alertdialog").getByRole("button", { name: /^revoke$/i }), { settle: 1_500 });
        assertDb(`select count(*) from api_keys where key_prefix='${prefixOf(ctx.apiKey)}' and revoked_at is not null;`, 1, "The key was not revoked");
        await s.app.point(keyRow(s, ctx).getByText(/^revoked$/i).first());
        await s.wait(1_500);
      },
    },
  ],
};

export const API: LongForm = {
  id: "api",
  title: TITLE,
  cardChapters: ["Make a key", "Shown once", "The reference", "A real request", "Revoke it"],
  partsDir,
  outDir,
  parts: [key],
  // The terminal with the real reply on it: the one frame that is the API
  // rather than a page about it.
  poster: { chapter: "A real request", offset: 5 },
};
