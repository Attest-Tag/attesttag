"use client";

import { useEffect, useRef, useState } from "react";

/**
 * Where a tab bar's choice is kept. One prefix for all of them, so a stray key
 * is recognisable in devtools and the shapes can't collide: `TabNav` stores a
 * pathname, the cases screen a query string, `useStickyTab` a tab value.
 *
 * Both wrappers swallow their errors. Storage throws in Safari's private mode
 * and when a quota is full, and losing your place is not a reason to take a
 * screen down with it.
 */
export function rememberTab(key: string, value: string): void {
  try {
    window.localStorage.setItem(`tab:${key}`, value);
  } catch {
    /* private mode, or a full quota */
  }
}

export function recallTab(key: string): string | null {
  try {
    return window.localStorage.getItem(`tab:${key}`);
  } catch {
    return null;
  }
}

/**
 * A tab choice that outlives the screen it was made on.
 *
 * Route-level tabs (`TabNav`) need none of this — their position is the URL, so
 * a reload, a bookmark and the back button all restore it for free. This is for
 * the in-page kind (`ui/tabs`), whose selection is React state and therefore
 * resets to the default every time the screen is mounted: pick Letters on
 * Templates & Forms, open one to edit it, come back, and you are on Templates
 * again.
 *
 * Same shape as `useState`. The stored value is read *after* mount rather than
 * in the initializer, for the reason ShellProvider reads `agentPanelOpen` that
 * way — the server renders `fallback`, and a different first client render is a
 * hydration mismatch.
 *
 * `allowed` is the tabs actually on screen. A stored value missing from it is
 * ignored rather than selected, so a renamed tab, or one hidden by a permission
 * this member no longer has, leaves someone on the default instead of on a
 * blank panel.
 */
export function useStickyTab(
  key: string,
  fallback: string,
  allowed?: readonly string[],
): [string, (next: string) => void] {
  const [tab, setTab] = useState(fallback);
  const synced = useRef(false);

  useEffect(() => {
    // Mount-only: a one-shot sync from an external store, not a reactive loop.
    // `allowed` is a fresh array on most renders, so without the guard this
    // would fight anyone who changed tab.
    if (synced.current) return;
    synced.current = true;
    const stored = recallTab(key);
    if (!stored || stored === fallback) return;
    if (allowed && !allowed.includes(stored)) return;
    /* eslint-disable react-hooks/set-state-in-effect */
    setTab(stored);
    /* eslint-enable react-hooks/set-state-in-effect */
  }, [allowed, fallback, key]);

  return [
    tab,
    (next: string) => {
      setTab(next);
      rememberTab(key, next);
    },
  ];
}
