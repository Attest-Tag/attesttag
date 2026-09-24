"use client";

import { ChevronRight, MoreHorizontal, PackageOpen, Pencil, SlidersHorizontal, Trash2 } from "lucide-react";
import { ConnectionTable, type ConnectionAction } from "@/components/bundles/connection-table";
import { RepoGroupList, type RepoBulkAction } from "@/components/bundles/repo-group-list";
import { countRepoSources, isRepoBundle } from "@/components/scopes/repo-sources";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import type { Bundle, Connection, GithubInstall } from "@/lib/api";
import { cn } from "@/lib/utils";

export type BundleAction = "rename" | "delete" | "manage" | "tabs" | "attach";

function summarize(b: Bundle): string {
  const parts: string[] = [];
  const creds = b.connections?.length ?? 0;
  const domains = b.domains?.length ?? 0;
  const packs = b.tool_packs?.length ?? 0;
  const skills = b.skills?.length ?? 0;
  if (isRepoBundle(b)) {
    // "33 credentials" reads as thirty-three secrets to keep track of. It is thirty-three
    // repositories behind two or three, and which those are is the thing worth saying here.
    const sources = countRepoSources(b.connections ?? []);
    parts.push(`${creds} repositor${creds === 1 ? "y" : "ies"}`);
    parts.push(`${sources} source${sources === 1 ? "" : "s"}`);
  } else parts.push(`${creds} credential${creds === 1 ? "" : "s"}`);
  if (domains > 0) parts.push(`${domains} domain${domains === 1 ? "" : "s"}`);
  if (b.instructions.trim()) parts.push("instructions");
  if (packs > 0) parts.push(`${packs} tool pack${packs === 1 ? "" : "s"}`);
  if (skills > 0) parts.push(`${skills} skill${skills === 1 ? "" : "s"}`);
  return parts.join(" · ");
}

// One collapsible bundle: name, a one-line summary and where it's used in the
// header; its connections underneath once opened.
export function BundleCard({
  bundle,
  open,
  onOpenChange,
  onAction,
  onConnectionAction,
  onRepoBulk,
  installs,
  canAttach,
}: {
  bundle: Bundle;
  /** Open or closed is the page's to hold: it is remembered across visits, per bundle. */
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onAction: (action: BundleAction, bundle: Bundle) => void;
  onConnectionAction: (action: ConnectionAction, connection: Connection) => void;
  /** Several repositories at once, from the grouped list's selection bar. */
  onRepoBulk: (action: RepoBulkAction, connections: Connection[]) => void;
  /** Names the GitHub App installations a repository group came through; null until loaded. */
  installs: GithubInstall[] | null;
  /** Whether this session may attach the bundle itself — scopes.manage, not bundles.manage. */
  canAttach: boolean;
}) {
  const used = bundle.used_in;
  // Scopes that took one of this bundle's connections on its own, without the bundle.
  const direct = new Set((bundle.connections ?? []).flatMap((c) => c.scope_ids ?? [])).size;
  const oneOff = direct > 0 ? `${direct} one-off grant${direct === 1 ? "" : "s"}` : "";
  // The repository bundle is not offered as a unit here, for the reason the channel's Add
  // popover doesn't offer it either: attaching it grants every repository the account has,
  // including the ones added to it next week. Its repositories are attached a few at a time,
  // from the selection bar below.
  const placeable = canAttach && !isRepoBundle(bundle);
  // Where the line is a button, its first clause is about the bundle as a unit — which is what
  // pressing it changes — so a bundle attached nowhere while two of its connections are granted
  // on their own says that, rather than "Not attached anywhere" beside two grants. Where it is
  // only text, it stays the sentence it has always been.
  const usedLabel =
    used > 0
      ? `Used in ${used} place${used === 1 ? "" : "s"}`
      : placeable && direct > 0
        ? "Not attached as a bundle"
        : "Not attached anywhere";
  const usage =
    used === 0 && direct === 0
      ? usedLabel
      : [used > 0 ? usedLabel : "", oneOff].filter(Boolean).join(" · ");

  return (
    <div className="overflow-hidden rounded-xl border bg-card shadow-sm">
      <div className="flex items-center gap-2 px-3 py-2.5">
        <button
          type="button"
          onClick={() => onOpenChange(!open)}
          aria-expanded={open}
          className="flex min-w-0 flex-1 items-center gap-2.5 rounded-md text-left"
        >
          <ChevronRight
            className={cn(
              "size-4 shrink-0 text-muted-foreground transition-transform",
              open && "rotate-90",
            )}
          />
          <span className="min-w-0">
            <span className="block truncate text-sm font-semibold">{bundle.name}</span>
            <span className="block truncate text-xs text-muted-foreground">{summarize(bundle)}</span>
          </span>
        </button>
        <span className="flex shrink-0 items-center gap-1 text-xs text-muted-foreground">
          {placeable ? (
            <button
              type="button"
              onClick={() => onAction("attach", bundle)}
              aria-label={`Choose where ${bundle.name} is attached`}
              className="rounded-md underline-offset-4 outline-none hover:text-foreground hover:underline focus-visible:ring-[3px] focus-visible:ring-ring/50"
            >
              {usedLabel}
            </button>
          ) : (
            usage
          )}
          {placeable && oneOff && <span>· {oneOff}</span>}
        </span>
        <Button variant="outline" size="sm" onClick={() => onAction("manage", bundle)}>
          <PackageOpen className="size-4" />
          Configure
        </Button>
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${bundle.name}`}>
              <MoreHorizontal />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <DropdownMenuItem onClick={() => onAction("rename", bundle)}>
              <Pencil className="size-4" /> Rename
            </DropdownMenuItem>
            {/* Configure on this one opens the repository manager, so the four tabs that are
                not repositories — its instructions, domains, tool packs and skills — are
                reached from here rather than being unreachable. */}
            {isRepoBundle(bundle) && (
              <DropdownMenuItem onClick={() => onAction("tabs", bundle)}>
                <SlidersHorizontal className="size-4" /> Instructions, domains and tool packs
              </DropdownMenuItem>
            )}
            <DropdownMenuSeparator />
            <DropdownMenuItem variant="destructive" onClick={() => onAction("delete", bundle)}>
              <Trash2 className="size-4" /> Delete
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
      {open && (
        <div className="border-t">
          {isRepoBundle(bundle) ? (
            <RepoGroupList
              connections={[...(bundle.connections ?? [])].sort((a, b) => a.repo.localeCompare(b.repo))}
              installs={installs}
              onAction={onConnectionAction}
              onBulk={onRepoBulk}
            />
          ) : (
            <ConnectionTable
              connections={bundle.connections ?? []}
              bundleName={bundle.name}
              onAction={onConnectionAction}
            />
          )}
        </div>
      )}
    </div>
  );
}
