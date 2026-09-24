"use client";

import { useState } from "react";
import { GitBranch } from "lucide-react";
import { StatusChip } from "@/components/core/status-chip";
import {
  RepoSelectionBar,
  RepoSourceHeader,
  groupRepos,
} from "@/components/scopes/repo-sources";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { useStickyFlags } from "@/hooks/use-sticky";
import type { Bundle, Connection, GithubInstall, RepoRow } from "@/lib/api";

/** Only a repository attached to this scope on its own can be taken off it here: one that
 * arrives through a bundle, or from the workspace above, is removed where it was attached. */
export const removableRepo = (row: RepoRow) => row.origin === "attached here" && row.via === "connection";

// The repositories one channel or workspace can reach, grouped by the credential behind each,
// the same way the Bundles page groups the saved ones. Rendered twice over the same data: in
// the scope's own Repositories card, and inside the manager dialog, which adds a filter and
// nothing else — so the two cannot drift.
export function ScopeRepoList({
  rows,
  installs,
  connectionOf,
  busy,
  removeLabel,
  onRemove,
}: {
  rows: RepoRow[];
  installs: GithubInstall[] | null;
  connectionOf: (id: number) => { bundle: Bundle; connection: Connection } | null;
  busy: boolean;
  /** "Remove from this channel" — the scope names itself, so the caller writes this. */
  removeLabel: string;
  onRemove: (rows: RepoRow[]) => void;
}) {
  // Folded groups are shared with the Bundles page: the same credential, folded in one place,
  // is folded in both.
  const groupOpen = useStickyFlags("repos:groups");
  const [picked, setPicked] = useState<number[]>([]);

  const sources = groupRepos(
    rows,
    (row) => {
      const c = connectionOf(row.connection_id)?.connection;
      return {
        repo: row.repo,
        installationID: c?.github_installation_id,
        fp: c?.secret_fp,
        credType: c?.cred_type,
      };
    },
    installs,
  );
  const live = new Set(rows.filter(removableRepo).map((r) => r.connection_id));
  const selected = picked.filter((id) => live.has(id));
  const chosen = rows.filter((r) => selected.includes(r.connection_id));

  return (
    <div className="overflow-hidden rounded-lg border">
      {sources.map((source) => {
        const ids = source.rows.filter(removableRepo).map((r) => r.connection_id);
        const on = ids.filter((id) => selected.includes(id)).length;
        const open = groupOpen.get(source.key, true);
        return (
          <div key={source.key} className="border-b last:border-b-0">
            <RepoSourceHeader
              source={source}
              open={open}
              onOpenChange={(next) => groupOpen.set(source.key, next)}
              // A group of nothing but inherited repositories has nothing to tick: those are
              // removed where they were attached, not here.
              checked={
                ids.length === 0 ? undefined : on === 0 ? false : on === ids.length ? true : "indeterminate"
              }
              onCheckedChange={(next) =>
                setPicked((cur) =>
                  next ? [...new Set([...cur, ...ids])] : cur.filter((id) => !ids.includes(id)),
                )
              }
            />
            {open &&
              source.rows.map((row) => (
                <div
                  key={row.connection_id}
                  className="flex items-center justify-between gap-3 border-t px-3 py-2"
                >
                  <span className="flex min-w-0 items-center gap-2">
                    {removableRepo(row) ? (
                      <Checkbox
                        checked={selected.includes(row.connection_id)}
                        onCheckedChange={() =>
                          setPicked((cur) =>
                            cur.includes(row.connection_id)
                              ? cur.filter((id) => id !== row.connection_id)
                              : [...cur, row.connection_id],
                          )
                        }
                        aria-label={`Select ${row.repo}`}
                      />
                    ) : (
                      <span className="size-5 shrink-0" />
                    )}
                    <GitBranch className="size-4 shrink-0 text-muted-foreground" />
                    <span className="truncate text-sm font-medium">{row.repo}</span>
                    {/* "Attached here" is the default in this list and says nothing; what is
                        worth a chip is a repository that came from somewhere else, because
                        that is the one the Remove button is missing from. */}
                    {row.origin !== "attached here" && <StatusChip variant="info">{row.origin}</StatusChip>}
                    {row.via === "bundle" && (
                      <span className="hidden text-xs text-muted-foreground sm:inline">via {row.bundle}</span>
                    )}
                  </span>
                  {removableRepo(row) && (
                    <Button variant="ghost" size="sm" disabled={busy} onClick={() => onRemove([row])}>
                      Remove
                    </Button>
                  )}
                </div>
              ))}
          </div>
        );
      })}
      <RepoSelectionBar count={selected.length} onClear={() => setPicked([])}>
        <Button variant="outline" size="sm" disabled={busy} onClick={() => onRemove(chosen)}>
          {removeLabel}
        </Button>
      </RepoSelectionBar>
    </div>
  );
}
