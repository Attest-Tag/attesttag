"use client";

import { useEffect, useState } from "react";
import { Copy, GitBranch, MoreHorizontal, Pencil, RefreshCw, Terminal, Trash2, Zap } from "lucide-react";
import { StatusChip } from "@/components/core/status-chip";
import {
  testConnection,
  type ConnectionAction,
} from "@/components/bundles/connection-table";
import {
  RepoSelectionBar,
  RepoSourceHeader,
  groupRepos,
  type RepoSource,
} from "@/components/scopes/repo-sources";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { useStickyFlags } from "@/hooks/use-sticky";
import { api, errorMessage, type Connection, type GithubInstall } from "@/lib/api";
import { formatRelativeLong } from "@/lib/format";
import { toast } from "sonner";
import { cn } from "@/lib/utils";

/** What the selection bar can do to several repositories at once. */
export type RepoBulkAction = "attach" | "writes" | "recipe" | "delete" | "added";

// The Repositories bundle, grouped by the credential each repository came through. It replaces
// the generic connection table for that one bundle: the Access column there repeats
// api.github.com on every row, and the source — the only thing that differs — was a bare
// cred_type chip. Here the source is the heading, and picking is what the page is for.
export function RepoGroupList({
  connections,
  installs,
  onAction,
  onBulk,
  checkGitHub = false,
}: {
  connections: Connection[];
  /** null until the installations endpoint answers; a group is named by its id until then. */
  installs: GithubInstall[] | null;
  onAction: (action: ConnectionAction, connection: Connection) => void;
  onBulk: (action: RepoBulkAction, connections: Connection[]) => void;
  /** Ask GitHub what each credential can see now, and offer what is not saved yet. One request
   * per source, so only where somebody has deliberately opened the manager — not on every visit
   * to the Bundles page. */
  checkGitHub?: boolean;
}) {
  // Collapsed groups are how one person left one screen, so they live in this browser.
  const groupOpen = useStickyFlags("repos:groups");
  const [picked, setPicked] = useState<number[]>([]);

  // Rendered in the order handed over: the manager dialog sorts by name or by last use, and a
  // second sort in here would quietly undo it.
  const sources = groupRepos(
    connections,
    (c) => ({
      repo: c.repo,
      installationID: c.github_installation_id,
      fp: c.secret_fp,
      credType: c.cred_type,
    }),
    installs,
  );
  // A repository deleted elsewhere must not stay in the selection and be acted on again.
  const live = new Set(connections.map((c) => c.id));
  const selected = picked.filter((id) => live.has(id));
  const isPicked = (id: number) => selected.includes(id);
  const toggle = (id: number) =>
    setPicked((cur) => (cur.includes(id) ? cur.filter((x) => x !== id) : [...cur, id]));
  const setGroup = (ids: number[], on: boolean) =>
    setPicked((cur) => (on ? [...new Set([...cur, ...ids])] : cur.filter((id) => !ids.includes(id))));
  const chosen = connections.filter((c) => isPicked(c.id));

  return (
    <div>
      {sources.map((source) => {
        const ids = source.rows.map((c) => c.id);
        const on = ids.filter(isPicked).length;
        const open = groupOpen.get(source.key, true);
        return (
          <div key={source.key} className="border-b last:border-b-0">
            <RepoSourceHeader
              source={source}
              open={open}
              onOpenChange={(next) => groupOpen.set(source.key, next)}
              checked={on === 0 ? false : on === ids.length ? true : "indeterminate"}
              onCheckedChange={(next) => setGroup(ids, next)}
              action={
                checkGitHub ? (
                  <NewAtGitHub source={source} saved={connections} onAdded={() => onBulk("added", [])} />
                ) : undefined
              }
            />
            {open &&
              source.rows.map((c) => {
                const active = (c.status || "active") === "active";
                const alone = c.scope_ids?.length ?? 0;
                return (
                  <div
                    key={c.id}
                    className={cn(
                      "flex items-center gap-2.5 border-t px-3 py-1.5",
                      isPicked(c.id) && "bg-accent/60",
                    )}
                  >
                    <Checkbox
                      checked={isPicked(c.id)}
                      onCheckedChange={() => toggle(c.id)}
                      aria-label={`Select ${c.repo}`}
                    />
                    <GitBranch className="size-4 shrink-0 text-muted-foreground" />
                    <div className="min-w-0 flex-1">
                      <p className="truncate text-sm font-medium">
                        <span className="font-normal text-muted-foreground">
                          {c.repo.slice(0, c.repo.indexOf("/") + 1)}
                        </span>
                        {c.repo.slice(c.repo.indexOf("/") + 1)}
                      </p>
                      <p className="truncate text-xs text-muted-foreground">
                        {c.writes === "auto" ? "writes automatic" : c.writes === "all" ? "every call confirmed" : "writes confirmed in Slack"}
                        {alone > 0 && ` · on its own in ${alone} scope${alone === 1 ? "" : "s"}`}
                      </p>
                    </div>
                    {!active && <StatusChip variant="warning">{c.status}</StatusChip>}
                    <span className="hidden text-xs whitespace-nowrap text-muted-foreground sm:block">
                      {c.last_used ? `Used ${formatRelativeLong(c.last_used)}` : "Never used"}
                    </span>
                    <DropdownMenu>
                      <DropdownMenuTrigger asChild>
                        <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${c.repo}`}>
                          <MoreHorizontal />
                        </Button>
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="end">
                        <DropdownMenuItem onClick={() => onAction("edit", c)}>
                          <Pencil className="size-4" /> Edit
                        </DropdownMenuItem>
                        <DropdownMenuItem onClick={() => onAction("rotate", c)}>
                          <RefreshCw className="size-4" /> Rotate secret
                        </DropdownMenuItem>
                        <DropdownMenuItem onClick={() => testConnection(c)}>
                          <Zap className="size-4" /> Test
                        </DropdownMenuItem>
                        <DropdownMenuItem onClick={() => onAction("curl", c)}>
                          <Terminal className="size-4" /> Show curl
                        </DropdownMenuItem>
                        <DropdownMenuItem onClick={() => onAction("copy", c)}>
                          <Copy className="size-4" /> Copy to bundle…
                        </DropdownMenuItem>
                        <DropdownMenuSeparator />
                        <DropdownMenuItem variant="destructive" onClick={() => onAction("delete", c)}>
                          <Trash2 className="size-4" /> Delete
                        </DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  </div>
                );
              })}
          </div>
        );
      })}
      <RepoSelectionBar count={selected.length} onClear={() => setPicked([])}>
        <Button variant="outline" size="sm" onClick={() => onBulk("attach", chosen)}>
          Attach to a channel…
        </Button>
        <Button variant="outline" size="sm" onClick={() => onBulk("writes", chosen)}>
          Set writes
        </Button>
        <Button variant="outline" size="sm" onClick={() => onBulk("recipe", chosen)}>
          Set recipe
        </Button>
        <Button variant="outline" size="sm" onClick={() => onBulk("delete", chosen)}>
          Remove
        </Button>
      </RepoSelectionBar>
    </div>
  );
}

// What the credential can reach that is not saved yet. An admin adds four repositories to the
// installation at GitHub and the console has never mentioned it — the installation's own
// repository list is one request away, and the difference is the answer.
function NewAtGitHub({
  source,
  saved,
  onAdded,
}: {
  source: RepoSource<Connection>;
  /** Every repository in this bundle, not just this group's: a token that reaches three owners
   * would otherwise offer back the ones saved under another heading. */
  saved: Connection[];
  onAdded: () => void;
}) {
  const [missing, setMissing] = useState<string[] | null>(null);
  const [busy, setBusy] = useState(false);
  const first = source.rows[0];
  const key = first?.github_installation_id ? `i${first.github_installation_id}` : `c${first?.id}`;

  useEffect(() => {
    if (!first) return;
    let live = true;
    const auth =
      first.github_installation_id > 0
        ? { installation_id: first.github_installation_id }
        : { connection_id: first.id };
    api
      .post<{ repos: { repo: string }[] }>("/api/github/repos", auth)
      .then((res) => {
        if (!live) return;
        const have = new Set(saved.map((c) => c.repo.toLowerCase()));
        setMissing((res.repos ?? []).map((r) => r.repo).filter((r) => !have.has(r.toLowerCase())));
      })
      // A token GitHub now rejects, a suspended installation, a deployment with no network:
      // the group's own trouble line says so, and this row simply has nothing to add.
      .catch(() => live && setMissing([]));
    return () => {
      live = false;
    };
  }, [key]); // eslint-disable-line react-hooks/exhaustive-deps -- one ask per credential

  if (!first || missing === null || missing.length === 0) return null;
  const add = async () => {
    setBusy(true);
    try {
      const auth =
        first.github_installation_id > 0
          ? { installation_id: first.github_installation_id }
          : { connection_id: first.id };
      const res = await api.post<{ repos: string[]; failed: { repo: string; error: string }[] }>(
        "/api/repos",
        { ...auth, repos: missing },
      );
      toast.success(
        res.repos.length === 1 ? `Saved ${res.repos[0]}` : `Saved ${res.repos.length} repositories`,
      );
      for (const f of res.failed ?? []) toast.error(`${f.repo}: ${f.error}`);
      setMissing([]);
      onAdded();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Button
      variant="link"
      size="sm"
      className="h-auto shrink-0 p-0 text-xs"
      disabled={busy}
      onClick={add}
    >
      {missing.length} new at GitHub · add {missing.length === 1 ? "it" : "them"}
    </Button>
  );
}
