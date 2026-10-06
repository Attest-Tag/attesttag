"use client";

import { Fragment, useEffect, useRef, useState } from "react";
import {
  BookMarked,
  BookPlus,
  ChevronRight,
  CircleSlash,
  Folder,
  FolderInput,
  FolderPlus,
  MoreHorizontal,
  Pencil,
  Plus,
  RotateCcw,
  Square,
  Trash2,
} from "lucide-react";
import { RelativeTime } from "@/components/core/relative-time";
import { SearchField } from "@/components/core/search-field";
import { ServiceMark, ServiceTile } from "@/components/core/service-mark";
import { StatusChip, type StatusChipVariant } from "@/components/core/status-chip";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuSub,
  DropdownMenuSubContent,
  DropdownMenuSubTrigger,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { useStickySet } from "@/hooks/use-sticky";
import type { ReviewAvailableInstall, ReviewConnectionNode, ReviewGroupNode, ReviewMode, ReviewNode, ReviewSettingsTree } from "@/lib/api";
import { cn } from "@/lib/utils";
import { markInstallFromReviews } from "@/components/reviews/install-return";
import {
  MODES,
  choiceLabel,
  connectionName,
  deleteNeedsReach,
  modeVariant,
  moveNeedsReach,
  repoRestoreNeedsReach,
  repoShort,
  reposOf,
  restoreNeedsReach,
  stoppedBecause,
  type Resolved,
} from "@/components/reviews/review-format";

// The left rail of Reviews › Settings, laid out like the Workspaces rail: the GitHub App
// installations being reviewed at the top, the groups somebody made under each, and the
// repositories — in a group, or straight under their connection when they are in none — each
// inheriting from what is above it. Add connection sits at the foot, where Add workspace does.

/** What the rail's row menus and Add connection ask the page to do. */
export type TreeActions = {
  /** With a repository: the group is made and the repository moved into it, from its Move to…. */
  newGroup: (conn: ReviewConnectionNode, repo?: ReviewNode) => void;
  renameGroup: (group: ReviewGroupNode) => void;
  deleteGroup: (conn: ReviewConnectionNode, group: ReviewGroupNode) => void;
  move: (conn: ReviewConnectionNode, repo: ReviewNode, to: { id: string; name: string }) => void;
  stop: (conn: ReviewConnectionNode) => void;
  restore: (conn: ReviewConnectionNode) => void;
  add: (install: ReviewAvailableInstall) => void;
  /** Add repositories…: the installation's others at GitHub, picked and saved into the tree. */
  addRepos: (conn: ReviewConnectionNode) => void;
  /** Remove from reviews…, and Restore from the connection's Removed list. */
  removeRepo: (conn: ReviewConnectionNode, repo: ReviewNode) => void;
  restoreRepo: (conn: ReviewConnectionNode, repo: ReviewNode) => void;
};

/**
 * Why Add repositories cannot be used on a connection, in the few words a menu entry carries; null
 * when it can. It saves the organisation's connections, which is connections.manage's, and reads
 * the installation at GitHub, which an uninstalled or suspended one cannot be.
 */
export function addReposHeld(conn: ReviewConnectionNode, canReach: boolean): string | null {
  if (conn.status === "uninstalled") return "The App is uninstalled from this account";
  if (conn.status === "suspended") return "The App is suspended on this account";
  if (!canReach) return "Saves connections: needs Manage connections";
  return null;
}

/** The one thing a connection's row must say before its mode: that reviews are not happening there. */
export function connectionTrouble(c: ReviewConnectionNode): { variant: StatusChipVariant; label: string } | null {
  if (c.removed_at) return { variant: "neutral", label: "Stopped" };
  if (c.status === "uninstalled") return { variant: "danger", label: "Uninstalled" };
  if (c.status === "suspended") return { variant: "warning", label: "Suspended" };
  if (c.missing_permissions?.length) return { variant: "warning", label: "Accept permissions" };
  return null;
}

/** A repository with something set on it, rather than a row that only holds its place in a group. */
export function setsAnything(n: ReviewNode): boolean {
  return !n.inherits && Object.keys(n.settings ?? {}).length > 0;
}

function plural(n: number, one: string, many: string) {
  return `${n} ${n === 1 ? one : many}`;
}

function Row({
  depth,
  leaf = false,
  icon,
  title,
  sub,
  chip,
  active,
  muted,
  onSelect,
  toggle,
  collapsed,
  toggleLabel,
  menu,
}: {
  depth: 0 | 1 | 2;
  /** A repository: nothing under it to fold, so no fold slot, and indented to sit under its parent's name. */
  leaf?: boolean;
  icon: React.ReactNode;
  title: string;
  sub: string;
  chip?: React.ReactNode;
  active: boolean;
  /** Its connection reviews nothing — stopped, uninstalled, suspended: shown, so nothing seems to vanish, but quieter. */
  muted?: boolean;
  onSelect: () => void;
  toggle?: () => void;
  collapsed?: boolean;
  toggleLabel?: string;
  menu?: React.ReactNode;
}) {
  return (
    <li
      className={cn(
        "group flex items-center transition-colors hover:bg-secondary/60",
        active && "bg-accent/60 hover:bg-accent/60",
      )}
    >
      {depth < 2 && !leaf && (
        // Connections and groups keep the fold control's slot even when there is nothing to fold,
        // so their icons line up down the rail whichever of them happen to be empty.
        <span className={cn("flex h-12 w-6 shrink-0 items-center justify-center", depth === 1 && "ml-3")}>
          {toggle && (
            <button
              type="button"
              onClick={toggle}
              aria-expanded={!collapsed}
              aria-label={toggleLabel}
              className="flex size-full items-center justify-center text-muted-foreground hover:text-foreground"
            >
              <ChevronRight className={cn("size-3.5 transition-transform", !collapsed && "rotate-90")} />
            </button>
          )}
        </span>
      )}
      <button
        type="button"
        onClick={onSelect}
        aria-current={active ? "true" : undefined}
        className={cn(
          "flex h-12 min-w-0 flex-1 items-center gap-3 pr-2 text-left",
          !leaf ? "pl-1" : depth === 1 ? "pl-10" : "pl-16",
          muted && "opacity-60",
        )}
      >
        {icon}
        <span className="min-w-0 flex-1">
          <span className="block truncate text-sm font-medium">{title}</span>
          <span className="block truncate text-xs text-muted-foreground">{sub}</span>
        </span>
        {chip}
      </button>
      {menu ?? <span className="w-8 shrink-0" />}
    </li>
  );
}

function RowMenu({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        {/* Beside the row's own button, not inside it, like the Workspaces star: a button cannot
            nest in a button, and opening the menu must not also select. Quiet until pointed at,
            never hidden outright, so it can be found by touch. */}
        <button
          type="button"
          aria-label={label}
          className="flex h-12 w-8 shrink-0 items-center justify-center text-muted-foreground opacity-40 transition-opacity hover:opacity-100 group-hover:opacity-80 data-[state=open]:opacity-100"
        >
          <MoreHorizontal className="size-4" />
        </button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-56">
        {children}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

/**
 * The mode a row resolves to — Off under a connection that reviews nothing, whatever its settings
 * hold: the rail is where somebody looks to see what is reviewed, and a Live chip there over a
 * stopped or uninstalled connection would say the opposite of what happens.
 */
function ModeChip({ node, stopped = false }: { node: ReviewNode; stopped?: boolean }) {
  const mode = stopped ? "off" : (node.mode ?? "off");
  return <StatusChip variant={modeVariant(mode)}>{choiceLabel(MODES, mode)}</StatusChip>;
}

/** A menu entry that cannot be picked, with why under it in words: a disabled item shows no title. */
function HeldReason({ children }: { children: React.ReactNode }) {
  return <span className="block text-xs whitespace-normal text-muted-foreground">{children}</span>;
}

/**
 * Move to…'s entries, on a repository's row menu and its panel alike: the groups it could inherit
 * from, the connection itself when it is in one, and New group…, which makes a group and moves the
 * repository into it. A move that would start posting it live is held for connections.manage.
 */
export function MoveItems({
  conn,
  repo,
  group,
  canReach,
  actions,
}: {
  conn: ReviewConnectionNode;
  repo: ReviewNode;
  group?: ReviewGroupNode;
  canReach: boolean;
  actions: TreeActions;
}) {
  const targets: { id: string; name: string; mode?: ReviewMode; group?: ReviewGroupNode }[] = [
    ...(group ? [{ id: conn.id, name: "Not in a group", mode: conn.mode }] : []),
    ...conn.groups.filter((g) => g.id !== group?.id).map((g) => ({ id: g.id, name: g.name, mode: g.mode, group: g })),
  ];
  return (
    <>
      {targets.length === 0 ? (
        <DropdownMenuItem disabled>No group yet</DropdownMenuItem>
      ) : (
        targets.map((t) => {
          const held = !canReach && moveNeedsReach(conn, repo, group, t.group);
          return (
            <DropdownMenuItem key={t.id} disabled={held} onSelect={() => actions.move(conn, repo, t)}>
              {t.id === conn.id ? <Square className="size-4" /> : <Folder className="size-4" />}
              <span className="min-w-0 flex-1">
                <span className="block truncate">{t.name}</span>
                {held && <HeldReason>Posts, spends or is announced differently there: needs Manage connections</HeldReason>}
              </span>
            </DropdownMenuItem>
          );
        })
      )}
      <DropdownMenuSeparator />
      <DropdownMenuItem disabled={!!conn.removed_at} onSelect={() => actions.newGroup(conn, repo)}>
        <FolderPlus className="size-4" /> New group…
      </DropdownMenuItem>
    </>
  );
}

export function ReviewTree({
  tree,
  selected,
  onSelect,
  canManage,
  canReach,
  install,
  actions,
}: {
  tree: ReviewSettingsTree;
  selected: Resolved | null;
  onSelect: (r: Resolved) => void;
  canManage: boolean;
  canReach: boolean;
  install: { configured: boolean; url: string };
  actions: TreeActions;
}) {
  // Folded connections and groups, remembered in this browser: a connection with forty
  // repositories is folded once, not on every visit.
  const { value: collapsed, toggle, dropForNow } = useStickySet<string>("reviews:collapsed");
  // The connections whose Removed list is open. Closed until somebody opens it: what is not
  // reviewed is kept out of the way of what is.
  const { value: removedOpen, toggle: toggleRemoved } = useStickySet<string>("reviews:removed-open");
  const [query, setQuery] = useState("");
  const q = query.trim().toLowerCase();
  const searching = q !== "";
  const total = tree.connections.reduce((n, c) => n + reposOf(c).length, 0);

  // Arriving on a row inside a folded connection or group opens what hides it, for this visit only,
  // as the Workspaces rail does: the link's doing, not a decision to unfold. Only what hides it: a
  // connection or a group arrived on is a row of its own, shown however it is folded, and its fold
  // stays as it was left. Every reload arrives on the node the page had open, so unfolding that one
  // too undid a fold of the connection or group being looked at on every visit.
  const revealed = useRef(false);
  const selConn = selected?.conn.id ?? "";
  const hiddenInConn = selected != null && selected.kind !== "connection";
  const hiddenInGroup = selected?.kind === "repo" ? (selected.group?.id ?? "") : "";
  useEffect(() => {
    if (revealed.current || selConn === "") return;
    revealed.current = true;
    if (hiddenInConn) dropForNow(selConn);
    if (hiddenInGroup) dropForNow(hiddenInGroup);
    // eslint-disable-next-line react-hooks/exhaustive-deps -- once on arrival, not on every render
  }, [selConn, hiddenInConn, hiddenInGroup]);

  const hit = (repo: ReviewNode) => (repo.repo ?? "").includes(q);
  const isActive = (r: Resolved) =>
    !!selected &&
    selected.kind === r.kind &&
    selected.conn.id === r.conn.id &&
    (r.kind === "connection" ||
      (r.kind === "group" && selected.kind === "group" && selected.group.id === r.group.id) ||
      (r.kind === "repo" && selected.kind === "repo" && selected.repo.repo === r.repo.repo));

  const repoRow = (conn: ReviewConnectionNode, repo: ReviewNode, depth: 1 | 2, group?: ReviewGroupNode) => {
    const r: Resolved = { kind: "repo", conn, repo, group };
    const stopped = stoppedBecause(conn);
    return (
      <Row
        key={`${conn.id}:${repo.repo}`}
        depth={depth}
        leaf
        icon={<BookMarked className="size-4 shrink-0 text-muted-foreground" />}
        title={repoShort(repo.repo ?? "")}
        sub={stopped ? stopped.short : setsAnything(repo) ? "sets its own" : "inherits everything"}
        chip={<ModeChip node={repo} stopped={!!stopped} />}
        active={isActive(r)}
        muted={!!stopped}
        onSelect={() => onSelect(r)}
        menu={
          canManage ? (
            <RowMenu label={`Actions for ${repo.repo}`}>
              <DropdownMenuSub>
                <DropdownMenuSubTrigger>
                  <FolderInput className="size-4" /> Move to…
                </DropdownMenuSubTrigger>
                <DropdownMenuSubContent className="w-64">
                  <MoveItems conn={conn} repo={repo} group={group} canReach={canReach} actions={actions} />
                </DropdownMenuSubContent>
              </DropdownMenuSub>
              <DropdownMenuSeparator />
              <DropdownMenuItem variant="destructive" onSelect={() => actions.removeRepo(conn, repo)}>
                <CircleSlash className="size-4" /> Remove from reviews…
              </DropdownMenuItem>
            </RowMenu>
          ) : undefined
        }
      />
    );
  };

  const branches = tree.connections
    .map((conn) => {
      const named = connectionName(conn).toLowerCase().includes(q);
      const groups = conn.groups
        .map((g) => ({ group: g, repos: !searching || named || g.name.toLowerCase().includes(q) ? g.repos : g.repos.filter(hit) }))
        .filter((g) => !searching || named || g.group.name.toLowerCase().includes(q) || g.repos.length > 0);
      const repos = !searching || named ? conn.repos : conn.repos.filter(hit);
      const removed = !searching || named ? conn.removed : conn.removed.filter(hit);
      if (searching && !named && groups.length === 0 && repos.length === 0 && removed.length === 0) return null;
      return { conn, groups, repos, removed };
    })
    .filter((b) => b !== null);

  // The top hit, as the rail shows them: a connection or a group whose own name matched, else the
  // first matching repository under it.
  const firstHit = (): Resolved | null => {
    for (const { conn, groups, repos } of branches) {
      if (connectionName(conn).toLowerCase().includes(q)) return { kind: "connection", conn };
      for (const { group, repos: inGroup } of groups) {
        if (group.name.toLowerCase().includes(q)) return { kind: "group", conn, group };
        if (inGroup[0]) return { kind: "repo", conn, repo: inGroup[0], group };
      }
      if (repos[0]) return { kind: "repo", conn, repo: repos[0] };
    }
    return null;
  };

  return (
    <div className="self-start overflow-hidden rounded-xl border bg-card">
      {total > 8 && (
        <div className="border-b p-2">
          <SearchField
            value={query}
            onChange={setQuery}
            clearable
            shortcut="/"
            placeholder="Search repositories"
            onKeyDown={(e) => {
              // As on the Workspaces rail: Enter takes the top hit and Escape puts the whole tree
              // back, so a search is started and finished without the mouse.
              const top = e.key === "Enter" && searching ? firstHit() : null;
              if (top) {
                e.preventDefault();
                onSelect(top);
              } else if (e.key === "Escape" && query !== "") {
                e.preventDefault();
                setQuery("");
              }
            }}
          />
        </div>
      )}
      <ul className="divide-y">
        {branches.map(({ conn, groups, repos, removed }) => {
          const trouble = connectionTrouble(conn);
          const stopped = stoppedBecause(conn);
          const count = reposOf(conn).length;
          const addHeld = addReposHeld(conn, canReach);
          // A search shows the removed repositories it matched without anybody opening the list.
          const removedShown = searching ? removed.length > 0 : removedOpen.has(conn.id);
          const folded = !searching && collapsed.has(conn.id);
          const r: Resolved = { kind: "connection", conn };
          return (
            <Fragment key={conn.id}>
              <Row
                depth={0}
                icon={<ServiceTile preset="github" className={cn(isActive(r) && "bg-primary text-primary-foreground")} />}
                title={connectionName(conn)}
                sub={`GitHub App · ${plural(count, "repository", "repositories")}`}
                chip={
                  trouble ? <StatusChip variant={trouble.variant}>{trouble.label}</StatusChip> : <ModeChip node={conn} />
                }
                active={isActive(r)}
                onSelect={() => onSelect(r)}
                toggle={!searching && count + conn.groups.length + conn.removed.length > 0 ? () => toggle(conn.id) : undefined}
                collapsed={folded}
                toggleLabel={`${folded ? "Show" : "Hide"} what is under ${connectionName(conn)}`}
                menu={
                  canManage ? (
                    <RowMenu label={`Actions for ${connectionName(conn)}`}>
                      <DropdownMenuItem disabled={addHeld !== null} onSelect={() => actions.addRepos(conn)}>
                        <BookPlus className="size-4" />
                        <span className="min-w-0 flex-1">
                          Add repositories…
                          {addHeld && <HeldReason>{addHeld}</HeldReason>}
                        </span>
                      </DropdownMenuItem>
                      <DropdownMenuItem onSelect={() => actions.newGroup(conn)} disabled={!!conn.removed_at}>
                        <FolderPlus className="size-4" /> New group…
                      </DropdownMenuItem>
                      <DropdownMenuSeparator />
                      {conn.removed_at ? (
                        <DropdownMenuItem
                          disabled={!canReach && restoreNeedsReach(conn)}
                          onSelect={() => actions.restore(conn)}
                        >
                          <RotateCcw className="size-4" />
                          <span className="min-w-0 flex-1">
                            Restore reviews
                            {!canReach && restoreNeedsReach(conn) && (
                              <HeldReason>Posts live, on every push, to a channel or on a rule&apos;s model: needs Manage connections</HeldReason>
                            )}
                          </span>
                        </DropdownMenuItem>
                      ) : (
                        <DropdownMenuItem variant="destructive" onSelect={() => actions.stop(conn)}>
                          <Square className="size-4" /> Stop reviewing…
                        </DropdownMenuItem>
                      )}
                    </RowMenu>
                  ) : undefined
                }
              />
              {!folded && (
                <>
                  {groups.map(({ group, repos: inGroup }) => {
                    const gr: Resolved = { kind: "group", conn, group };
                    const gFolded = !searching && collapsed.has(group.id);
                    const deleteHeld = !canReach && deleteNeedsReach(conn, group);
                    return (
                      <Fragment key={group.id}>
                        <Row
                          depth={1}
                          icon={<Folder className="size-4 shrink-0 text-muted-foreground" />}
                          title={group.name}
                          sub={`Group · ${stopped ? stopped.short : plural(group.repos.length, "repository", "repositories")}`}
                          chip={<ModeChip node={group} stopped={!!stopped} />}
                          active={isActive(gr)}
                          muted={!!stopped}
                          onSelect={() => onSelect(gr)}
                          toggle={!searching && group.repos.length > 0 ? () => toggle(group.id) : undefined}
                          collapsed={gFolded}
                          toggleLabel={`${gFolded ? "Show" : "Hide"} the repositories in ${group.name}`}
                          menu={
                            canManage ? (
                              <RowMenu label={`Actions for ${group.name}`}>
                                <DropdownMenuItem onSelect={() => actions.renameGroup(group)}>
                                  <Pencil className="size-4" /> Rename…
                                </DropdownMenuItem>
                                <DropdownMenuItem
                                  variant="destructive"
                                  disabled={deleteHeld}
                                  onSelect={() => actions.deleteGroup(conn, group)}
                                >
                                  <Trash2 className="size-4" />
                                  <span className="min-w-0 flex-1">
                                    Delete group…
                                    {deleteHeld && <HeldReason>Changes how its repositories post or are announced: needs Manage connections</HeldReason>}
                                  </span>
                                </DropdownMenuItem>
                              </RowMenu>
                            ) : undefined
                          }
                        />
                        {!gFolded && inGroup.map((repo) => repoRow(conn, repo, 2, group))}
                      </Fragment>
                    );
                  })}
                  {repos.map((repo) => repoRow(conn, repo, 1))}
                  {count === 0 && conn.groups.length === 0 && !searching && (
                    <li className="px-3 py-3 pl-9 text-xs text-muted-foreground">
                      No repository yet. Add repositories… in its menu lists the ones this installation
                      reaches at GitHub, and any saved under Access bundles › Repositories appear here.
                    </li>
                  )}
                  {removed.length > 0 && (
                    <RemovedList
                      conn={conn}
                      repos={removed}
                      open={removedShown}
                      onToggle={searching ? undefined : () => toggleRemoved(conn.id)}
                      canManage={canManage}
                      canReach={canReach}
                      actions={actions}
                    />
                  )}
                </>
              )}
            </Fragment>
          );
        })}
        {searching && branches.length === 0 && (
          <li className="px-3 py-6 text-center text-sm text-muted-foreground">{`No repository matches “${query.trim()}”`}</li>
        )}
      </ul>
      <div className="flex justify-center border-t p-2">
        <AddConnectionMenu
          available={tree.available}
          install={install}
          canManage={canManage}
          canReach={canReach}
          onPick={actions.add}
        />
      </div>
    </div>
  );
}

/**
 * A connection's repositories removed from code review, under a fold of their own at the foot of
 * it: not in the tree, since nothing on them is reviewed, and not gone either — each keeps its
 * settings and its group, and Restore puts it back as it was. Restoring switches on whatever those
 * settings post or tell, so it is held for connections.manage where that is live, every push, a
 * channel or a branch rule's own model (repoRestoreNeedsReach).
 */
function RemovedList({
  conn,
  repos,
  open,
  onToggle,
  canManage,
  canReach,
  actions,
}: {
  conn: ReviewConnectionNode;
  repos: ReviewNode[];
  open: boolean;
  /** Absent while searching, when the matches are simply shown. */
  onToggle?: () => void;
  canManage: boolean;
  canReach: boolean;
  actions: TreeActions;
}) {
  return (
    <li>
      <button
        type="button"
        onClick={onToggle}
        disabled={!onToggle}
        aria-expanded={open}
        className="flex h-9 w-full items-center text-left text-xs text-muted-foreground hover:bg-secondary/60 hover:text-foreground disabled:hover:bg-transparent"
      >
        <span className="ml-3 flex w-6 shrink-0 items-center justify-center">
          <ChevronRight className={cn("size-3.5 transition-transform", open && "rotate-90")} />
        </span>
        <span className="pl-1">Removed ({repos.length})</span>
      </button>
      {open && (
        <ul className="divide-y border-t">
          {repos.map((repo) => {
            const group = conn.groups.find((g) => g.id === repo.parent_id);
            const held = !canReach && repoRestoreNeedsReach(conn, repo);
            return (
              <li key={repo.id} className="flex items-center gap-3 py-2 pr-2 pl-10">
                <BookMarked className="size-4 shrink-0 text-muted-foreground/60" />
                <span className="min-w-0 flex-1">
                  <span className="block truncate text-sm text-muted-foreground">{repoShort(repo.repo ?? "")}</span>
                  <span className="block truncate text-xs text-muted-foreground">
                    {repo.removed_at ? (
                      <>
                        Removed <RelativeTime value={repo.removed_at} />
                      </>
                    ) : (
                      "Removed"
                    )}
                    {group ? ` · back into ${group.name}` : ""}
                  </span>
                  {canManage && held && <HeldReason>Restoring needs Manage connections</HeldReason>}
                </span>
                {canManage && (
                  <Button
                    type="button"
                    size="xs"
                    variant="ghost"
                    disabled={held}
                    aria-label={`Restore ${repo.repo} to code review`}
                    onClick={() => actions.restoreRepo(conn, repo)}
                  >
                    <RotateCcw /> Restore
                  </Button>
                )}
              </li>
            );
          })}
        </ul>
      )}
    </li>
  );
}

/**
 * Add connection, like Add workspace: the installations the organisation already holds that are
 * not reviewed yet, then installing the App on another account, then what is coming. A token-only
 * repository is never offered — without the App, GitHub sends nothing to review.
 */
export function AddConnectionMenu({
  available,
  install,
  canManage,
  canReach,
  onPick,
}: {
  available: ReviewAvailableInstall[];
  install: { configured: boolean; url: string };
  canManage: boolean;
  canReach: boolean;
  onPick: (install: ReviewAvailableInstall) => void;
}) {
  const installable = install.configured && canReach;
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button disabled={!canManage}>
          <Plus className="size-4" />
          Add connection
        </Button>
      </DropdownMenuTrigger>
      {/* 22rem, not w-80: the console is on a 3px spacing grid, where w-80 is 240px and cut "Install
          the GitHub App on another account" short. On a phone it keeps the page's 16px gutter. */}
      <DropdownMenuContent align="start" collisionPadding={16} className="w-[22rem] max-w-[calc(100vw-2rem)]">
        <DropdownMenuLabel className="text-xs font-normal text-muted-foreground">
          Already connected, not reviewed yet
        </DropdownMenuLabel>
        {available.length === 0 ? (
          <DropdownMenuItem disabled>
            <span className="text-xs">Every installation here is in the tree already.</span>
          </DropdownMenuItem>
        ) : (
          available.map((a) => (
            <DropdownMenuItem key={a.installation_id} onSelect={() => onPick(a)}>
              <ServiceMark preset="github" />
              <span className="min-w-0 flex-1 truncate">{connectionName(a)}</span>
              <span className="whitespace-nowrap text-xs text-muted-foreground">
                {a.repo_selection === "all" && a.repos.length === 0 ? "all repositories" : plural(a.repos.length, "repository", "repositories")}
              </span>
            </DropdownMenuItem>
          ))
        )}
        <DropdownMenuSeparator />
        <DropdownMenuItem asChild disabled={!installable}>
          {/* A real navigation: the browser leaves for GitHub to choose the account. */}
          <a href={install.url} onClick={markInstallFromReviews}>
            <ServiceMark preset="github" />
            <span className="min-w-0 whitespace-normal">Install the GitHub App on another account</span>
          </a>
        </DropdownMenuItem>
        <DropdownMenuItem disabled>
          <ServiceMark preset="gitlab" />
          <span className="whitespace-nowrap">GitLab</span>
          <span className="ml-auto whitespace-nowrap text-xs text-muted-foreground">later</span>
        </DropdownMenuItem>
        {!installable && (
          <p className="px-2 py-1.5 text-xs text-muted-foreground">
            {!install.configured
              ? "Installing needs the GitHub App's OAuth client on the server."
              : "Installing the App on an account needs Manage connections."}
          </p>
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
