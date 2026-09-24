import type { BrowserContext, Locator, Page } from "playwright";

// The recording surface handed to each scene. Playwright drives the app at
// machine speed and renders no cursor, neither of which reads as a product
// demo — everything here exists to slow it down to something a viewer can
// follow, and to draw a pointer that shows where the click landed.

/** Injected into every document so the cursor survives navigations. */
const CURSOR_SCRIPT = `
(() => {
  const draw = () => {
    if (document.getElementById("__demo_cursor__")) return;
    const style = document.createElement("style");
    style.textContent = \`
      #__demo_cursor__ {
        position: fixed; left: 0; top: 0; z-index: 2147483647;
        width: 22px; height: 22px; margin: -3px 0 0 -3px;
        pointer-events: none; will-change: transform;
        transition: transform 40ms linear;
      }
      #__demo_cursor__ svg { display: block; filter: drop-shadow(0 1px 2px rgba(0,0,0,.45)); }
      /* The dev server's floating indicator sits over the sidebar footer. */
      nextjs-portal { display: none !important; }
      .__demo_ping__ {
        position: fixed; z-index: 2147483646; pointer-events: none;
        width: 34px; height: 34px; margin: -17px 0 0 -17px;
        border-radius: 9999px; border: 2px solid rgba(99,91,255,.9);
        animation: __demo_ping__ 500ms ease-out forwards;
      }
      @keyframes __demo_ping__ {
        from { transform: scale(.35); opacity: 1 }
        to   { transform: scale(1);   opacity: 0 }
      }
    \`;
    document.head.appendChild(style);
    const cursor = document.createElement("div");
    cursor.id = "__demo_cursor__";
    cursor.innerHTML =
      '<svg viewBox="0 0 22 22" xmlns="http://www.w3.org/2000/svg">' +
      '<path d="M4 2 L4 17 L8.2 13.2 L10.9 19.4 L13.9 18.1 L11.2 12 L16.6 12 Z"' +
      ' fill="#fff" stroke="#111" stroke-width="1.2" stroke-linejoin="round"/></svg>';
    document.body.appendChild(cursor);

    addEventListener("mousemove", (e) => {
      cursor.style.transform = "translate(" + e.clientX + "px," + e.clientY + "px)";
    }, { passive: true });

    addEventListener("mousedown", (e) => {
      const ping = document.createElement("div");
      ping.className = "__demo_ping__";
      ping.style.left = e.clientX + "px";
      ping.style.top = e.clientY + "px";
      document.body.appendChild(ping);
      setTimeout(() => ping.remove(), 520);
    }, { passive: true, capture: true });
  };
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", draw);
  } else {
    draw();
  }
})();
`;

/** Takes a context (not a page) so the cursor survives every navigation. */
export async function installCursor(target: BrowserContext): Promise<void> {
  await target.addInitScript(CURSOR_SCRIPT);
}

export class Stage {
  constructor(
    readonly page: Page,
    readonly baseUrl: string,
  ) {}

  /** Where the pointer currently is, so moves start from the right place. */
  private at = { x: 640, y: 400 };

  wait(ms: number): Promise<void> {
    return this.page.waitForTimeout(ms);
  }

  /** Glide the pointer to a point, at roughly hand speed. */
  async moveTo(x: number, y: number): Promise<void> {
    const distance = Math.hypot(x - this.at.x, y - this.at.y);
    // ~1 step per 12px, floored so short hops still animate.
    const steps = Math.max(12, Math.round(distance / 12));
    await this.page.mouse.move(x, y, { steps });
    this.at = { x, y };
  }

  /** Scroll an element into view, glide to it, pause, then click. */
  async click(target: Locator | string, opts: { settle?: number } = {}) {
    const locator = typeof target === "string" ? this.page.locator(target) : target;
    const el = locator.first();
    await el.waitFor({ state: "visible", timeout: 20_000 });
    await el.scrollIntoViewIfNeeded();
    await this.wait(180);
    const box = await el.boundingBox();
    if (!box) throw new Error(`No bounding box for ${el}`);
    await this.moveTo(box.x + box.width / 2, box.y + box.height / 2);
    await this.wait(220); // let the hover state read before the click
    await el.click();
    await this.wait(opts.settle ?? 450);
  }

  /** Click into a field and type it out at a human cadence. */
  async type(target: Locator | string, text: string, delay = 42) {
    const locator = typeof target === "string" ? this.page.locator(target) : target;
    const el = locator.first();
    await this.click(el, { settle: 120 });
    await el.pressSequentially(text, { delay });
    await this.wait(280);
  }

  /**
   * Type into a React-controlled field and make sure the text survived.
   *
   * React owns these inputs, and anything typed before hydration is discarded
   * when the client takes over — silently. The form then posts empty fields,
   * the server answers 400, the page stays where it was, and the recording
   * carries on into scenes that make no sense. That is not a hypothetical: it
   * is how a whole take of the signup part was lost, and the rig this was
   * ported from carries a retry loop in its sign-in for the same reason.
   *
   * So: type at hand speed, because that is what the camera is for; then read
   * the value back. If it did not stick, set it directly — instant and barely
   * visible, and a slightly abrupt frame beats a dead take — and read it back
   * again. Still wrong is a real error, raised here rather than three scenes
   * later.
   */
  async typeVerified(selector: string, text: string, delay = 42) {
    const field = this.page.locator(selector).first();
    await this.click(field, { settle: 120 });
    await field.pressSequentially(text, { delay });
    await this.wait(280);

    if ((await field.inputValue().catch(() => "")) === text) return;
    await field.fill(text);
    await this.wait(220);
    if ((await field.inputValue().catch(() => "")) === text) return;

    throw new Error(
      `${selector} would not hold its value. The field is React-controlled ` +
        `and is being reset after it is typed into — give the page longer to ` +
        `hydrate before this scene.`,
    );
  }

  /** Navigate and wait for the app to settle. */
  async goto(pathOrUrl: string) {
    const url = pathOrUrl.startsWith("http")
      ? pathOrUrl
      : `${this.baseUrl}${pathOrUrl}`;
    // 60s, not Playwright's default 30s. The recorder drives a *dev* server,
    // which compiles a route the first time it is asked for while also serving
    // the recording — two renders died on a cold `/cases` that came back in
    // well under a second the moment it was warm. This costs nothing when
    // things are healthy: the scene still holds only as long as its narration.
    await this.page.goto(url, {
      waitUntil: "domcontentloaded",
      timeout: 60_000,
    });
    await this.page.waitForLoadState("networkidle").catch(() => {});
    // React controls these inputs; typing before hydration silently loses the
    // text when the client takes over. The pause buys hydration a beat, and
    // doubles as the settle a viewer needs after a page change anyway.
    await this.wait(900);
  }

  /**
   * Click only if the control is on screen. For controls the app renders
   * conditionally — a scene shouldn't abort the whole recording because a
   * list happened to be empty.
   */
  async clickIfPresent(target: Locator | string, opts: { settle?: number } = {}) {
    const locator = typeof target === "string" ? this.page.locator(target) : target;
    const el = locator.first();
    if (!(await el.isVisible().catch(() => false))) return false;
    // Disabled counts as absent. Otherwise Playwright treats a greyed-out
    // button as "not yet actionable" and retries for the full timeout — a
    // disabled "Approve & save" cost a whole render that way.
    if (await el.isDisabled().catch(() => false)) return false;
    await this.click(el, opts);
    return true;
  }

  /**
   * Draw the eye to a region without clicking — used when the narration is
   * describing something already on screen.
   */
  async point(target: Locator | string) {
    const locator = typeof target === "string" ? this.page.locator(target) : target;
    const el = locator.first();
    await el.waitFor({ state: "visible", timeout: 20_000 });
    await el.scrollIntoViewIfNeeded();
    const box = await el.boundingBox();
    if (!box) return;
    await this.moveTo(box.x + box.width / 2, box.y + box.height / 2);
    await this.wait(300);
  }

  /**
   * Press, glide, release — a real drag rather than an instant jump. Needed
   * for canvas connections (react-flow wires blocks by dragging one handle
   * onto another; adding a block never connects it).
   */
  async dragBetween(from: Locator | string, to: Locator | string) {
    const pick = (t: Locator | string) =>
      typeof t === "string" ? this.page.locator(t).first() : t.first();
    const a = pick(from);
    const b = pick(to);
    await a.waitFor({ state: "visible", timeout: 20_000 });
    await b.waitFor({ state: "visible", timeout: 20_000 });
    const boxA = await a.boundingBox();
    const boxB = await b.boundingBox();
    if (!boxA || !boxB) throw new Error("Cannot drag: element has no box");

    await this.moveTo(boxA.x + boxA.width / 2, boxA.y + boxA.height / 2);
    await this.wait(250);
    await this.page.mouse.down();
    await this.wait(150);
    await this.moveTo(boxB.x + boxB.width / 2, boxB.y + boxB.height / 2);
    await this.wait(250);
    await this.page.mouse.up();
    await this.wait(500);
  }

  /** Slow scroll, so the viewer can track what moved. */
  async scroll(deltaY: number, steps = 14) {
    const per = deltaY / steps;
    for (let i = 0; i < steps; i++) {
      await this.page.mouse.wheel(0, per);
      await this.wait(28);
    }
    await this.wait(200);
  }
}
