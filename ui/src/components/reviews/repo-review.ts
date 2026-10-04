"use client";

import { useMemo } from "react";
import { useAuth } from "@/components/shell/auth-provider";
import { useApi, type ReviewConnectionNode, type ReviewMode, type ReviewSettingsTree } from "@/lib/api";
import { reposOf, stoppedBecause } from "@/components/reviews/review-format";

// How code review stands on a repository, for pages that are not Reviews: Access bundles' repository
// rows say "review: shadow" and link to the repository's settings. Read from the same tree Reviews ›
// Settings draws, so the word on a row is the chip on the rail, and resolved the way the lane gates a
// pull request — a connection that is stopped, uninstalled or suspended at GitHub reviews nothing,
// whatever mode its settings hold.

export type RepoReview = {
  /** What a pull request opened on the repository now would get. */
  mode: ReviewMode;
  /** Why it is off when its settings say otherwise, in a few words a row can show; absent when the mode is the settings'. */
  why?: string;
  /** Reviews › Settings with this repository selected — or Add connection for its installation, when that is not in the tree. */
  repoHref: string;
  /** The same, on the repository's connection. */
  connHref: string;
};

/** A repository by name and the installation it was saved through; null when code review cannot say. */
export type RepoReviewLookup = (repo: string, installationID: number) => RepoReview | null;

const settingsHref = (params: Record<string, string>) =>
  `/reviews?${new URLSearchParams({ tab: "settings", ...params }).toString()}`;

export function repoReviewLookup(tree: ReviewSettingsTree): RepoReviewLookup {
  const byKey = new Map<string, { conn: ReviewConnectionNode; mode: ReviewMode; removed?: boolean }>();
  const connOf = new Map<number, ReviewConnectionNode>();
  for (const conn of tree.connections) {
    connOf.set(conn.installation_id, conn);
    for (const r of reposOf(conn)) {
      byKey.set(`${conn.installation_id}:${r.repo ?? ""}`, { conn, mode: r.mode ?? "off" });
    }
    // Out of the tree, and not under the connection's mode either: nothing on it is reviewed.
    for (const r of conn.removed) {
      byKey.set(`${conn.installation_id}:${r.repo ?? ""}`, { conn, mode: "off", removed: true });
    }
  }
  return (repo, installationID) => {
    if (!repo || installationID <= 0) return null;
    const name = repo.toLowerCase();
    const hit = byKey.get(`${installationID}:${name}`);
    const conn = hit?.conn ?? connOf.get(installationID);
    if (!conn) {
      // Installed, and never added to Reviews: installing the App reviews nothing by itself. Both
      // links open Add connection for that installation, which is the one thing to do about it.
      const add = settingsHref({ add: String(installationID) });
      return { mode: "off", why: "account not added", repoHref: add, connHref: add };
    }
    const connHref = settingsHref({ node: conn.id });
    // A removed repository has no panel of its own: its connection's, where its Restore is.
    if (hit?.removed) return { mode: "off", why: "removed from reviews", repoHref: connHref, connHref };
    const repoHref = hit ? settingsHref({ node: name, conn: conn.id }) : connHref;
    // The rail's words for it, so the row here and the tree there say the same thing.
    const stopped = stoppedBecause(conn);
    if (stopped) return { mode: "off", why: stopped.short, repoHref, connHref };
    return { mode: hit?.mode ?? conn.mode ?? "off", repoHref, connHref };
  };
}

/**
 * The lookup for a page that lists repositories, or null while the tree is loading, where this member
 * may not read code review's settings, or where they could not be read — a row then says nothing about
 * review rather than something wrong. `wanted` keeps a page with no App-backed repository from asking.
 */
export function useRepoReviews(wanted: boolean): RepoReviewLookup | null {
  const { me } = useAuth();
  // /api/me sends only the keys a role holds: absent is no. Until it answers the tree is not asked
  // for, unlike a gate on a control, because a viewer without the permission would only get a 403.
  const perms = me?.user?.permissions;
  // Nor on a deployment without code review, whose routes answer 404: no row says "review: off" for a
  // feature that is not there.
  const may = !!perms && perms["reviews.view"] === true && me?.code_review?.reason !== "off";
  const { data } = useApi<ReviewSettingsTree>(wanted && may ? "/api/review-settings" : null);
  return useMemo(() => (data ? repoReviewLookup(data) : null), [data]);
}
