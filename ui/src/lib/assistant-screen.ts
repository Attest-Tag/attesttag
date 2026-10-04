import { useEffect, useId } from "react";

/**
 * What the assistant is told the page has open, beyond its path: the review type on Reviews › Types,
 * the level on Reviews › Settings. The server re-reads it inside the organisation and words it for
 * the model (internal/app/assistant_focus.go, whose registry these kinds and params must match), so
 * "add a rule to this type" needs no "which type?".
 *
 * Like the ?scope= the panel already sends, it is read from the address bar at the moment the
 * question is sent: the panel sits beside every route and cannot be handed the page's state, and
 * what should ride along is what was on screen when the question was asked. Two things the address
 * does not hold are published here instead, by the page: a readable name for what it names (a
 * connection's id says nothing to the person reading the chip), and whether an editor on it holds
 * unsaved edits — which the card for that same thing then warns would conflict with Confirm.
 */
export type ScreenFocus = { kind: string; params: Record<string, string> };

/**
 * Said on the window by Reviews whenever what it has open changes — a tab, a type, a level, or an
 * editor's unsaved state — so the panel's chip is redrawn the moment the address it reads is
 * rewritten, and never names a different thing than the question then carries.
 */
export const REVIEWS_SELECTION_EVENT = "attest-tag:reviews-selection";

const TABS: Record<string, string> = { history: "History", settings: "Settings", types: "Types" };

// The name the page last gave the thing its address names, kept with that address value so a name
// left over from the node before can never be put on the one after.
let named: { value: string; label: string } | null = null;
// Editors on screen that hold unsaved edits. A set, not a flag, so one editor going clean or away
// does not speak for another still open beside it.
const dirtyEditors = new Set<string>();

function onReviews(pathname: string): boolean {
  return pathname.replace(/\/+$/, "") === "/reviews";
}

function announce() {
  if (typeof window !== "undefined") window.dispatchEvent(new Event(REVIEWS_SELECTION_EVENT));
}

/**
 * Called by a Reviews tab right after it writes what it has open into the address: `value` is the
 * ?type= or ?node= it wrote, `label` what a person calls it. With neither, only the tab changed.
 */
export function announceReviewsSelection(value?: string, label?: string) {
  if (value) named = { value, label: label || value };
  announce();
}

/**
 * Publishes an editor's unsaved state while it is mounted. Only an editor whose save a card's
 * Confirm would collide with uses it: a review type's, a level's branch rules.
 */
export function useDirtyOnScreen(dirty: boolean) {
  const id = useId();
  useEffect(() => {
    if (!dirty) return;
    dirtyEditors.add(id);
    announce();
    return () => {
      dirtyEditors.delete(id);
      announce();
    };
  }, [dirty, id]);
}

/**
 * The focus the question carries, chosen by the tab the address names: a type on Types, a level on
 * Settings, else the tab alone. A ?node= left in the address by Settings is therefore never sent from
 * Types, where it names nothing on screen. Null off the pages that have one.
 */
export function focusOnScreen(pathname: string): ScreenFocus | null {
  if (typeof window === "undefined" || !onReviews(pathname)) return null;
  const q = new URLSearchParams(window.location.search);
  const tab = q.get("tab") || "history";
  const dirty: Record<string, string> = dirtyEditors.size > 0 ? { dirty: "1" } : {};
  const type = q.get("type");
  if (tab === "types" && type) return { kind: "review_type", params: { type, ...dirty } };
  const node = q.get("node");
  const conn = q.get("conn");
  if (tab === "settings" && node) {
    return { kind: "review_node", params: { node, ...(conn ? { conn } : {}), ...dirty } };
  }
  return { kind: "reviews", params: { tab: TABS[tab] ? tab : "history" } };
}

/** The chip's words for a focus: "Reviews › Types › general", "Reviews › Settings › acme/web". */
export function focusLabel(focus: ScreenFocus | null): string {
  if (!focus) return "";
  const p = focus.params;
  const value = focus.kind === "review_type" ? p.type : focus.kind === "review_node" ? p.node : "";
  const tab = focus.kind === "review_type" ? "types" : focus.kind === "review_node" ? "settings" : p.tab;
  const parts = ["Reviews", TABS[tab] ?? "History"];
  // A connection's or a group's id, before the tab has said what it is called, is left out rather
  // than shown: thirty-two hex digits name nothing to the person reading the chip.
  const label = named?.value === value ? named.label : /^[0-9a-f]{32}$/.test(value) ? "" : value;
  if (label) parts.push(label);
  return parts.join(" › ") + (p.dirty === "1" ? " · unsaved edits" : "");
}

/**
 * The query a card's Open link names when the page it links to opens things in place — today only
 * Reviews, whose tabs read the address only as they mount — else null. Such a page is handed the
 * selection (CONSOLE_CHANGED_EVENT's select) rather than linked to.
 */
export function selectOf(href: string): Record<string, string> | null {
  const [path, query = ""] = href.split("?");
  return onReviews(path) ? Object.fromEntries(new URLSearchParams(query)) : null;
}

/**
 * selectOf, when that page is the one on screen. A Next link to the route already open changes the
 * address and leaves the page as it was, so there the card hands over the selection instead; from
 * any other page an ordinary link is right.
 */
export function selectInPlace(href: string, pathname: string): Record<string, string> | null {
  return onReviews(pathname) ? selectOf(href) : null;
}

/**
 * Whether a card just confirmed may move the page to what it made, unasked: only within the tab
 * already on screen, and never over unsaved edits. Reviews unmounts a tab it leaves and remounts the
 * editor of a type it switches to, so either move would throw a draft away without a word — and a
 * type created from Settings has a branch-rule card behind it about the level still open there. The
 * card's own Open link is the way there when the person chooses it.
 */
export function mayMoveTo(select: Record<string, string> | null): boolean {
  if (!select || dirtyEditors.size > 0 || typeof window === "undefined") return false;
  return select.tab === (new URLSearchParams(window.location.search).get("tab") || "history");
}
