"use client";

import { useEffect, useRef } from "react";
import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { recallTab, rememberTab } from "@/hooks/use-sticky-tab";
import { cn } from "@/lib/utils";

export type TabNavItem = {
  href: string;
  label: string;
  /**
   * How the tab decides it is active. "prefix" (default) keeps the tab
   * highlighted on nested routes; "exact" is for index tabs that would
   * otherwise match every sibling (e.g. /settings, a case overview).
   */
  match?: "exact" | "prefix";
};

// Route-level underline tab bar — the only sanctioned tab nav for app
// sections. For stateful in-page panels use ui/tabs; for compact view
// toggles use SegmentedControl. See DESIGN.md.
export function TabNav({
  tabs,
  className,
  memory,
  onRemember,
}: {
  tabs: TabNavItem[];
  className?: string;
  /**
   * Remember which tab was last open under this key, and land on it next time
   * someone arrives at the section's index route — from the sidebar, a
   * bookmark, a reload. Leaving a section and coming back should not cost the
   * clicks it took to get where you were.
   *
   * Only the index route is ever redirected: a link straight to a sub-tab is
   * someone saying where they want to be, and hijacking that would make every
   * URL in the app a suggestion. A stored tab that is no longer offered — a
   * route renamed, a tab hidden by a permission this member has since lost —
   * is ignored rather than followed.
   *
   * Scope the key to what "back here" means. Settings and Research are one
   * section each; a case's tabs are keyed per case, because opening a
   * *different* case should still start at its overview.
   *
   * This restore runs client-side, after the index route has painted, so the
   * reader sees the landing and then the jump. A section that wants to skip
   * the jump mirrors the memory somewhere the server can read via
   * `onRemember` and redirects in its index page — the case bar does (see
   * CaseTabs); this effect then stays as the fallback for memory recorded
   * before the mirror existed.
   */
  memory?: string;
  /**
   * Called wherever `memory` is recorded — on arrival at a tab and on the
   * click itself. The click-time call matters for the same reason the
   * onClick write below exists: a server that restores from the mirror must
   * see an index-tab click before the navigation it triggers, or the index
   * tab bounces straight back and can never be selected.
   */
  onRemember?: (href: string) => void;
}) {
  const pathname = usePathname();
  const router = useRouter();
  const restored = useRef(false);

  const isActive = (tab: TabNavItem) =>
    tab.match === "exact"
      ? pathname === tab.href
      : pathname.startsWith(tab.href);
  const active = tabs.find(isActive);

  useEffect(() => {
    if (!memory && !onRemember) return;
    if (memory && !restored.current) {
      restored.current = true;
      const index = tabs.find((t) => t.match === "exact");
      const stored = recallTab(memory);
      if (
        index &&
        pathname === index.href &&
        stored &&
        stored !== pathname &&
        tabs.some((t) => t.href === stored)
      ) {
        // replace, not push: the tab you are restored to is where you meant to
        // land, so Back should leave the section rather than bounce off the
        // index route it passed through on the way.
        router.replace(stored);
        return;
      }
    }
    if (active) {
      if (memory) rememberTab(memory, active.href);
      onRemember?.(active.href);
    }
  }, [active, memory, onRemember, pathname, router, tabs]);

  return (
    <nav className={cn("flex gap-1 border-b", className)}>
      {tabs.map((tab) => (
        <Link
          key={tab.href}
          href={tab.href}
          // Every tab in this bar is on screen at once, so the default would
          // prefetch all of them the moment the bar renders — thirteen full RSC
          // renders of a case's sub-pages, each with its own queries, for the
          // one the reader is about to click.
          //
          // It also wedges the app. A case page reached by `redirect()` out of
          // a server action renders this bar mid-transition, and the prefetch
          // wave races the navigation that is still completing: the router
          // aborts it, the transition never settles, and the form that
          // submitted stays disabled forever with the URL never changing.
          // Reproduced at 4/20 on `/cases/new` with a single worker and no load
          // — 0/20 with these prefetches blocked. See e2e/support/cases.ts.
          prefetch={false}
          // Written on the click rather than left to the effect, because this
          // is the one move the effect cannot read: clicking the *index* tab
          // from a sibling. Recorded after the fact, a remounted bar would find
          // the old tab still stored and send you straight back to it — the
          // index tab would be unclickable.
          onClick={() => {
            if (memory) rememberTab(memory, tab.href);
            onRemember?.(tab.href);
          }}
          className={cn(
            "-mb-px border-b-2 px-3 py-2 text-sm font-medium transition-colors",
            isActive(tab)
              ? "border-primary text-foreground"
              : "border-transparent text-muted-foreground hover:text-foreground",
          )}
        >
          {tab.label}
        </Link>
      ))}
    </nav>
  );
}
