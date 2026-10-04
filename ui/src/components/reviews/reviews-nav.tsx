"use client";

import { createContext, useContext } from "react";

// Moving between the Reviews page's own tabs. Its own module so a panel deep in one tab can link
// to another without importing the page that renders it.

export const REVIEW_TABS = ["history", "settings", "types"] as const;
export type ReviewTab = (typeof REVIEW_TABS)[number];

export function isReviewTab(v: string | null): v is ReviewTab {
  return REVIEW_TABS.includes(v as ReviewTab);
}

/**
 * Switches tab with what the target should open on — a run, a repository. The tabs read their
 * part of the URL as they mount, so this writes the URL first and switches second; a Next link to
 * the same route would change the address and leave the page as it was.
 */
export type ReviewsNav = (tab: ReviewTab, params?: Record<string, string>) => void;

export const NavContext = createContext<ReviewsNav>(() => {});

export function useReviewsNav(): ReviewsNav {
  return useContext(NavContext);
}

/** A link to one of the page's tabs that still opens in a new browser tab on a middle click. */
export function ReviewsTabLink({
  tab,
  params,
  className,
  children,
}: {
  tab: ReviewTab;
  params?: Record<string, string>;
  className?: string;
  children: React.ReactNode;
}) {
  const go = useReviewsNav();
  const qs = new URLSearchParams({ tab, ...params }).toString();
  return (
    <a
      href={`/admin/reviews/?${qs}`}
      className={className}
      onClick={(e) => {
        if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
        e.preventDefault();
        go(tab, params);
      }}
    >
      {children}
    </a>
  );
}
