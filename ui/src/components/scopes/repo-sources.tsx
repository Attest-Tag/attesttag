"use client";

import { ChevronRight, Package, KeyRound } from "lucide-react";
import { StatusChip } from "@/components/core/status-chip";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { useApi, type Bundle, type Connection, type GithubInstall, type GithubInstalls } from "@/lib/api";
import { cn } from "@/lib/utils";

// Repositories arrive in twenties, and an account reaches them through two or three credentials:
// a GitHub App installation whose admin ticked a list at GitHub, a token pasted for one
// organisation, a personal token for a handful of side repositories. Flattened into one
// alphabetical list they are indistinguishable, and every action is one row at a time — which is
// the whole of the complaint. Grouped by the credential behind them, the list says where each
// repository came from, and a group can be taken in one tick.
//
// The grouping key is exact for an installation: `github_installation_id` is stored on the
// connection and the installations endpoint names the account. A token-backed repository carries
// no such link — connectRepo seals its own copy of the token per repository — so those group by
// owner instead. That is right for every account that pastes one token per organisation and
// wrong only for one token spanning two owners, where it splits a group that could have been one.

export type RepoSource<T> = {
  key: string;
  kind: "app" | "token";
  /** The account an installation sits on, or the owner a token's repositories belong to. */
  title: string;
  badge: string;
  /** What the credential is, for the muted line beside the title. */
  meta: string;
  /** Set when the credential itself is in trouble: every row under it is unreachable at once. */
  trouble?: string;
  rows: T[];
};

const ownerOf = (repo: string) => (repo.includes("/") ? repo.slice(0, repo.indexOf("/")) : repo);

/**
 * Group repository rows by the credential that opens them.
 *
 * `installs` is null until the installations endpoint has answered — the difference matters,
 * because an installation nobody can name yet is not the same as one this account has lost.
 */
export function groupRepos<T>(
  rows: T[],
  describe: (row: T) => { repo: string; installationID?: number; fp?: string; credType?: string },
  installs: GithubInstall[] | null,
): RepoSource<T>[] {
  const groups = new Map<string, RepoSource<T>>();
  for (const row of rows) {
    const { repo, installationID, fp = "", credType } = describe(row);
    const installID = installationID ?? 0;
    // An app-backed row with no id — copied before the id travelled with a copy — is still an
    // app, and saying "Access token" about it would be wrong rather than merely vague.
    const app = installID > 0 || credType === "github_app";
    const install = installs?.find((i) => i.installation_id === installID);
    const key = app
      ? `app:${installID}`
      : fp !== ""
        ? `token:${fp}`
        : `owner:${ownerOf(repo).toLowerCase()}`;
    let group = groups.get(key);
    if (!group) {
      group = app
          ? {
              key,
              kind: "app",
              title: install?.account_login || (installID > 0 ? `Installation ${installID}` : "GitHub App"),
              badge: "GitHub App",
              meta:
                installID === 0
                  ? "A GitHub App installation this copy no longer names"
                  : install?.repo_selection === "all"
                    ? `Installation ${installID} · every repository in this account`
                    : `Installation ${installID} · the repositories its admin chose at GitHub`,
              trouble:
                installID === 0
                  ? undefined
                  : install?.status === "suspended"
                  ? "Suspended at GitHub. Nothing under it can be reached until it is restored there."
                  : installs !== null && install === undefined
                    ? "This installation is not connected here any more. Install the app again, or remove these."
                    : undefined,
              rows: [],
            }
          : {
              key,
              kind: "token",
              title: ownerOf(repo),
              badge: "Access token",
              meta:
                fp !== ""
                  ? `Token ····${fp.slice(-4)} · the repositories connected with it`
                  : "Connected before the token was recorded, so grouped by owner",
              rows: [],
            };
      groups.set(key, group);
    }
    group.rows.push(row);
  }
  // One token across two organisations is one credential and one group, so its heading cannot
  // be the owner of whichever repository happened to come first.
  for (const group of groups.values()) {
    if (group.kind !== "token") continue;
    const owners = [...new Set(group.rows.map((r) => ownerOf(describe(r).repo)))];
    if (owners.length > 1) group.title = `${owners[0]} and ${owners.length - 1} more`;
  }
  // Installations first — they are the route that needs no secret, and the one an account with
  // both tends to think of as its main one. Owners follow, alphabetically either way.
  return [...groups.values()].sort(
    (a, b) => (a.kind === b.kind ? 0 : a.kind === "app" ? -1 : 1) || a.title.localeCompare(b.title),
  );
}

/** A bundle whose every credential is a GitHub repository — the one the repository flows file
 * into. Recognised by what is in it rather than by its name, which anyone can change; the name
 * only decides the empty case, where there is nothing to look at and the server's own name for
 * that bundle is the best evidence of what the next thing in it will be. */
export function isRepoBundle(b: Bundle): boolean {
  const connections = b.connections ?? [];
  if (connections.length === 0) return b.name.toLowerCase() === "repositories";
  return connections.every((c) => c.repo);
}

/** How many credentials the repositories in a bundle come from, for its one-line summary. */
export function countRepoSources(connections: Connection[]): number {
  const keys = new Set(
    connections
      .filter((c) => c.repo)
      .map((c) =>
        c.github_installation_id > 0
          ? `app:${c.github_installation_id}`
          : c.secret_fp
            ? `token:${c.secret_fp}`
            : `owner:${ownerOf(c.repo).toLowerCase()}`,
      ),
  );
  return keys.size;
}

/**
 * The installations, fetched once per page and only where some repository actually names one.
 * A console with no GitHub App in it should not call an endpoint to be told so.
 */
export function useInstallations(needed: boolean) {
  const res = useApi<GithubInstalls>(needed ? "/api/github/installations" : null);
  // A deployment with no app configured answers this fine; anything else failing must not take
  // the list down with it, so a failure reads as "no installations named yet", which only
  // changes what a group header is called.
  return res.data?.installations ?? (res.error ? [] : null);
}

/** The header of one source group: what the credential is, how many repositories, one tick for all. */
export function RepoSourceHeader<T>({
  source,
  open,
  onOpenChange,
  checked,
  onCheckedChange,
  action,
}: {
  source: RepoSource<T>;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Absent where the list has no selection at all (a channel showing only inherited rows). */
  checked?: boolean | "indeterminate";
  onCheckedChange?: (checked: boolean) => void;
  /** Rendered before the collapse control — "Manage at GitHub", "Rotate". */
  action?: React.ReactNode;
}) {
  const n = source.rows.length;
  const Icon = source.kind === "app" ? Package : KeyRound;
  return (
    <>
      <div className="@container/source flex items-center gap-2.5 bg-muted/50 px-3 py-2">
        {checked !== undefined && (
          <Checkbox
            checked={checked}
            onCheckedChange={(v) => onCheckedChange?.(v === true)}
            aria-label={`Select every repository from ${source.title}`}
          />
        )}
        <Icon className="size-4 shrink-0 text-muted-foreground" />
        {/* One line, always: wrapping put the count under the name and left the tick floating
            between two rows. */}
        <div className="flex min-w-0 flex-1 items-center gap-2">
          <span className="truncate text-sm font-semibold">{source.title}</span>
          <StatusChip variant={source.kind === "app" ? "info" : "neutral"} className="shrink-0">
            {source.badge}
          </StatusChip>
          <span className="shrink-0 text-xs whitespace-nowrap text-muted-foreground">
            {n} repositor{n === 1 ? "y" : "ies"}
          </span>
        </div>
        {/* Which credential this is, in full — but only where the row is wide enough to hold it
            beside the name. A viewport breakpoint cannot tell: the Bundles card runs the width
            of the page while a channel's list sits in the right-hand third of a settings row,
            and at lg: the same header had room in one place and was truncating the account name
            to "att…" in the other. The container query asks the row itself. */}
        <span className="hidden min-w-0 truncate text-xs text-muted-foreground @3xl/source:block">
          {source.meta}
        </span>
        {action}
        <button
          type="button"
          onClick={() => onOpenChange(!open)}
          aria-expanded={open}
          aria-label={`${open ? "Collapse" : "Expand"} ${source.title}`}
          className="shrink-0 rounded-md p-0.5 text-muted-foreground hover:text-foreground"
        >
          <ChevronRight className={cn("size-4 transition-transform", open && "rotate-90")} />
        </button>
      </div>
      {source.trouble && (
        <p className="border-t bg-warning-soft px-3 py-1.5 text-xs text-warning">{source.trouble}</p>
      )}
    </>
  );
}

/** What is ticked, and what can be done with it. Shown only while something is. */
export function RepoSelectionBar({
  count,
  onClear,
  children,
}: {
  count: number;
  onClear: () => void;
  children: React.ReactNode;
}) {
  if (count === 0) return null;
  return (
    <div className="flex flex-wrap items-center gap-2 border-t bg-accent px-3 py-2">
      <span className="text-sm font-semibold tabular-nums">{count} selected</span>
      {children}
      <Button variant="link" size="sm" className="ml-auto h-auto p-0 text-xs" onClick={onClear}>
        Clear
      </Button>
    </div>
  );
}
