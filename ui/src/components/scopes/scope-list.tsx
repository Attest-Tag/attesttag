"use client";

import { Fragment, useEffect, useRef, useState } from "react";
import { Building2, ChevronRight, Hash, Lock, MessagesSquare, Plus, RefreshCw, Star } from "lucide-react";
import { SearchField } from "@/components/core/search-field";
import { StatusChip } from "@/components/core/status-chip";
import { scopeName } from "@/lib/format";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { ConnectTeamsDialog } from "@/components/scopes/connect-teams-dialog";
import { useStickySet } from "@/hooks/use-sticky";
import type { Scope, Team } from "@/lib/api";
import { cn } from "@/lib/utils";

// What a scope has attached, for the rail: "2 bundles · 1 connection".
function attachedLabel(scope: Scope): string {
  const bundles = scope.bundle_ids?.length ?? 0;
  const connections = scope.connection_ids?.length ?? 0;
  const parts = [
    bundles > 0 ? `${bundles} bundle${bundles === 1 ? "" : "s"}` : "",
    connections > 0 ? `${connections} connection${connections === 1 ? "" : "s"}` : "",
  ].filter(Boolean);
  return parts.length === 0 ? "—" : parts.join(" · ");
}

function ScopeRow({
  scope,
  team,
  active,
  depth,
  onSelect,
  toggle,
  collapsed,
  channelCount,
  starred,
  onStar,
  subLine,
  onRefresh,
  refreshing,
}: {
  scope: Scope;
  team?: Team;
  active: boolean;
  depth: 0 | 1 | 2;
  onSelect: (id: number) => void;
  /** Present on a row that has children; renders the collapse control. */
  toggle?: () => void;
  collapsed?: boolean;
  channelCount?: number;
  /** Present on a channel row; renders the star and says which way it points. */
  onStar?: () => void;
  starred?: boolean;
  /** Replaces the Slack id under the name — the starred list says which workspace instead. */
  subLine?: string;
  /** Present on a connected workspace; asks Slack for its channels again. */
  onRefresh?: () => void;
  refreshing?: boolean;
}) {
  const Icon =
    scope.kind === "workspace"
      ? Building2
      : scope.kind === "team"
        ? MessagesSquare
        : scope.is_private
          ? Lock
          : Hash;
  // The account's own row has no Slack id to show; a team shows its domain if Slack gave us one.
  // A Teams tenant has neither, and its ids are long enough to be noise, so it says what it is.
  const teams = scope.team_id?.startsWith("msteams:");
  const sub =
    scope.kind === "workspace"
      ? "all workspaces"
      : teams
        ? scope.kind === "team"
          ? "Microsoft Teams"
          : "Teams channel"
        : scope.kind === "team"
          ? team?.domain
            ? `${team.domain}.slack.com`
            : scope.slack_id
          : scope.slack_id;
  return (
    <li
      className={cn(
        "group flex items-center transition-colors hover:bg-secondary/60",
        active && "bg-accent/60 hover:bg-accent/60",
      )}
    >
      {toggle && (
        <button
          type="button"
          onClick={toggle}
          aria-expanded={!collapsed}
          aria-label={`${collapsed ? "Show" : "Hide"} channels in ${scope.name}`}
          className="flex h-12 w-6 shrink-0 items-center justify-center text-muted-foreground hover:text-foreground"
        >
          <ChevronRight className={cn("size-3.5 transition-transform", !collapsed && "rotate-90")} />
        </button>
      )}
      <button
        type="button"
        onClick={() => onSelect(scope.id)}
        aria-current={active ? "true" : undefined}
        className={cn(
          "flex h-12 min-w-0 flex-1 items-center gap-3 px-3 text-left",
          depth === 1 && !toggle && "pl-5",
          depth === 2 && "pl-8",
        )}
      >
        <span
          className={cn(
            "flex size-8 shrink-0 items-center justify-center rounded-md bg-accent text-accent-foreground",
            active && "bg-primary text-primary-foreground",
          )}
        >
          {team?.icon ? (
            // eslint-disable-next-line @next/next/no-img-element -- a Slack-hosted workspace icon
            <img src={team.icon} alt="" className="size-8 rounded-md object-cover" />
          ) : (
            <Icon className="size-4" />
          )}
        </span>
        <span className="min-w-0 flex-1">
          <span className="block truncate text-sm font-medium">{scopeName(scope)}</span>
          <span className="block truncate font-mono text-xs text-muted-foreground">{subLine ?? sub}</span>
        </span>
        {team && team.status !== "active" ? (
          <StatusChip variant="danger">Disconnected</StatusChip>
        ) : team?.needs_reinstall ? (
          <StatusChip variant="warning">Reinstall</StatusChip>
        ) : collapsed && channelCount ? (
          // What is hidden, so a collapsed workspace is not mistaken for an empty one.
          <span className="shrink-0 text-xs text-muted-foreground">
            {channelCount} channel{channelCount === 1 ? "" : "s"}
          </span>
        ) : (
          <span className="shrink-0 text-xs text-muted-foreground">{attachedLabel(scope)}</span>
        )}
      </button>
      {onRefresh && (
        // Where a channel carries its star. A workspace's channels arrive from Slack on a
        // timer, so the one thing wanted here is "ask again, now" — after inviting the bot to a
        // channel, which is exactly when a minute of waiting feels like something is broken.
        <button
          type="button"
          onClick={onRefresh}
          disabled={refreshing}
          aria-label={`Refresh the channels in ${scope.name}`}
          className="flex h-12 w-8 shrink-0 items-center justify-center text-muted-foreground opacity-35 transition-opacity hover:opacity-100 group-hover:opacity-70 disabled:opacity-100"
        >
          <RefreshCw className={cn("size-3.5", refreshing && "animate-spin")} />
        </button>
      )}
      {onStar && (
        // Its own button beside the row's, not inside it: a button cannot nest in a button, and
        // starring must not also select. Quiet until it is pointed at or starred — a rail of
        // grey stars is noise — but never hidden outright, since a control that only exists on
        // hover cannot be found by touch.
        <button
          type="button"
          onClick={onStar}
          aria-label={`${starred ? "Unstar" : "Star"} ${scopeName(scope)}`}
          className={cn(
            "flex h-12 w-8 shrink-0 items-center justify-center transition-opacity hover:opacity-100",
            starred ? "text-primary opacity-100" : "text-muted-foreground opacity-35 group-hover:opacity-70",
          )}
        >
          <Star className={cn("size-3.5", starred && "fill-current")} />
        </button>
      )}
    </li>
  );
}

// What a row can be found by: the name as it reads in Slack, the Slack id people paste out of a
// link, and a workspace's slack.com domain. Someone hunting for a channel has one of the three to
// hand, and not always the one the row happens to show.
function matches(scope: Scope, team: Team | undefined, query: string): boolean {
  return [scopeName(scope), scope.slack_id, team?.domain ?? ""]
    .filter(Boolean)
    .some((key) => key.toLowerCase().includes(query));
}

// Best hit first, so typing "eng" does not put #product-engineering above #eng. Ties keep the
// order the rail already had — sort is stable — and an id-only hit sinks below every name hit.
function byRelevance(channels: Scope[], query: string): Scope[] {
  const score = (s: Scope) => {
    const name = scopeName(s).toLowerCase();
    return name.startsWith(query) ? 0 : name.includes(query) ? 1 : 2;
  };
  return [...channels].sort((a, b) => score(a) - score(b));
}

// The left rail of the Workspaces page. Three levels, because access resolves through three:
// the Workspace (this account) sits above every connected Slack workspace, and each of those
// above its own channels. A channel id is only unique inside its workspace, so channels are
// grouped under the workspace they belong to rather than listed flat.
export function ScopeList({
  scopes,
  teams,
  selectedId,
  onSelect,
  installUrl,
  canInstall,
  msteams = false,
  onRefreshTeam,
}: {
  scopes: Scope[];
  teams: Team[];
  selectedId: number | null;
  onSelect: (id: number) => void;
  installUrl: string;
  canInstall: boolean;
  /** Whether a Microsoft Teams organisation can be connected on this deployment. */
  msteams?: boolean;
  /** Ask Slack for one workspace's channels again, then reload what the rail draws. */
  onRefreshTeam: (teamID: string) => Promise<void>;
}) {
  const teamByID = new Map(teams.map((t) => [t.team_id, t]));
  const account = scopes.find((s) => s.kind === "workspace");
  const teamScopes = scopes.filter((s) => s.kind === "team");

  // Starred channels. Whoever lives in three of a hundred channels should not have to hunt for
  // them, so a star lifts one out of its workspace and onto the top of the rail. It is a
  // personal shortcut rather than a setting anyone else sees, and it rides in this browser
  // beside the fold state for the same reason: nothing about it is worth a round trip.
  const { value: starred, toggle: toggleStar } = useStickySet<number>("scopes:starred");

  const channelsOf = (teamID: string) =>
    scopes.filter((s) => s.kind === "channel" && s.team_id === teamID);

  // Starred first, wherever a list of channels is drawn. A starred channel keeps its place in
  // the workspace it belongs to — pulling it out would leave a hole where somebody expects to
  // find it — it just rises to the top of that workspace, as well as appearing in the block at
  // the very top of the rail. Sort is stable, so everything else keeps the order it had.
  const starFirst = (list: Scope[]) =>
    [...list].sort((a, b) => Number(starred.has(b.id)) - Number(starred.has(a.id)));

  // Which workspaces are folded away. A workspace can be in hundreds of channels, so the rail
  // has to be foldable to stay usable — and the choice is remembered, because refolding the
  // same three workspaces on every visit is the kind of small tax that makes a page tiring.
  const { value: collapsed, toggle, dropForNow } = useStickySet<string>("scopes:collapsed");
  // A folded workspace opens itself when a link lands on something inside it — arriving at a
  // channel must never leave you looking at a list that does not contain it. Only on arrival,
  // though: while the page is open, folding is a deliberate act and has to stick, even when the
  // thing on the right is a channel of the workspace being folded away. The reveal is not
  // written back either, so the fold you chose is still there on your next visit.
  // Which workspace is mid-refresh, so its icon spins and cannot be pressed again.
  const [refreshing, setRefreshing] = useState("");
  const refresh = async (teamID: string) => {
    if (refreshing !== "") return;
    setRefreshing(teamID);
    try {
      await onRefreshTeam(teamID);
    } finally {
      setRefreshing("");
    }
  };

  // Searching the rail. A workspace can be in hundreds of channels, and scrolling a tree to find
  // one you can already name is the slow way round — so what matches rises to the top and
  // everything else steps out of the way until the field is cleared.
  const [query, setQuery] = useState("");
  const q = query.trim().toLowerCase().replace(/^#+/, "");
  const searching = q !== "";
  const hasChannels = scopes.some((s) => s.kind === "channel");

  // The tree as it will be drawn. A workspace that matches keeps all of its channels — you asked
  // for the workspace — and every other workspace is kept only for the channels that match, open
  // whatever its stored fold says, because a hit you cannot see is not a hit.
  const branches = teamScopes
    .map((team) => {
      const info = teamByID.get(team.team_id);
      const all = channelsOf(team.team_id);
      if (!searching)
        return { team, info, channels: starFirst(all), hit: false, folded: collapsed.has(team.team_id) };
      const hit = matches(team, info, q);
      const channels = starFirst(
        hit ? all : byRelevance(all.filter((c) => matches(c, undefined, q)), q),
      );
      if (!hit && channels.length === 0) return null;
      return { team, info, channels, hit, folded: false };
    })
    .filter((b) => b !== null);
  // The starred rows themselves, in their own order: alphabetical while browsing, best-first
  // while searching, like every other list on the page.
  const starredRows = scopes.filter(
    (sc) => sc.kind === "channel" && starred.has(sc.id) && (!searching || matches(sc, undefined, q)),
  );
  const starredList = searching
    ? byRelevance(starredRows, q)
    : [...starredRows].sort((a, b) => scopeName(a).localeCompare(scopeName(b)));
  // A starred channel has left its workspace group, so with more than one workspace connected
  // the row has to say which one it came from; its Slack id says less here than the name does.
  const starredSub = (sc: Scope) =>
    teamScopes.length > 1 ? (teamByID.get(sc.team_id)?.name ?? sc.team_name) : undefined;

  // Where Enter goes: the workspace itself when that is what was typed, its best channel otherwise.
  const firstHit = searching
    ? (starredList[0] ?? branches.flatMap((b) => (b.hit ? [b.team] : b.channels.slice(0, 1)))[0] ?? null)
    : null;

  const selectedTeam = scopes.find((s) => s.id === selectedId)?.team_id ?? "";
  const revealed = useRef(false);
  useEffect(() => {
    if (revealed.current || selectedTeam === "") return;
    revealed.current = true;
    dropForNow(selectedTeam);
    // eslint-disable-next-line react-hooks/exhaustive-deps -- once on arrival, not on every render
  }, [selectedTeam]);

  return (
    <div className="self-start overflow-hidden rounded-xl border bg-card">
      {hasChannels && (
        <div className="border-b p-2">
          <SearchField
            value={query}
            onChange={setQuery}
            clearable
            shortcut="/"
            placeholder="Search channels"
            onKeyDown={(e) => {
              // Enter takes the top hit and Escape puts the whole tree back, so a search can be
              // started and finished without the mouse ever leaving the keyboard.
              if (e.key === "Enter" && firstHit) {
                e.preventDefault();
                onSelect(firstHit.id);
              } else if (e.key === "Escape" && query !== "") {
                e.preventDefault();
                setQuery("");
              }
            }}
          />
        </div>
      )}
      <ul className="divide-y">
        {starredList.length > 0 && (
          <li className="bg-muted/50 px-3 py-1.5 text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
            Starred
          </li>
        )}
        {starredList.map((sc) => (
          <ScopeRow
            key={`starred-${sc.id}`}
            scope={sc}
            active={sc.id === selectedId}
            depth={1}
            onSelect={onSelect}
            starred
            onStar={() => toggleStar(sc.id)}
            subLine={starredSub(sc)}
          />
        ))}
        {account && (!searching || matches(account, undefined, q)) && (
          <ScopeRow
            key={account.id}
            scope={account}
            active={account.id === selectedId}
            depth={0}
            onSelect={onSelect}
          />
        )}
        {branches.map(({ team, info, channels, folded }) => (
          <Fragment key={team.id}>
            <ScopeRow
              scope={team}
              team={info}
              active={team.id === selectedId}
              depth={1}
              onSelect={onSelect}
              toggle={
                // Folding is for the whole tree; while a search is narrowing it, the fold
                // control would only be a way to hide the results you just asked for.
                !searching && channels.length > 0 ? () => toggle(team.team_id) : undefined
              }
              collapsed={folded}
              channelCount={channels.length}
              // A disconnected workspace has no token to ask Slack with, so it is offered no
              // refresh; its row already carries the chip that says what to do instead. A Teams
              // tenant has no list to ask for: its channels arrive as the bot is talked to in them.
              onRefresh={
                (info?.status ?? "active") === "active" && info?.platform !== "msteams"
                  ? () => void refresh(team.team_id)
                  : undefined
              }
              refreshing={refreshing === team.team_id}
            />
            {!folded &&
              channels.map((ch) => (
                <ScopeRow
                  key={ch.id}
                  scope={ch}
                  active={ch.id === selectedId}
                  depth={2}
                  onSelect={onSelect}
                  starred={starred.has(ch.id)}
                  onStar={() => toggleStar(ch.id)}
                />
              ))}
          </Fragment>
        ))}
        {searching && branches.length === 0 && starredList.length === 0 && (
          <li className="px-3 py-6 text-center text-sm text-muted-foreground">
            {`No channel matches “${query.trim()}”`}
          </li>
        )}
      </ul>
      <div className="flex justify-center border-t p-2">
        <AddWorkspaceMenu installUrl={installUrl} canInstall={canInstall} msteams={msteams} />
      </div>
    </div>
  );
}

// The platforms a workspace can be connected from. Slack and Microsoft Teams are built, Teams only
// where the deployment has a bot registered for it; Google Chat is listed because the hierarchy
// above them is deliberately platform-neutral — a "Team" is one connected workspace, whichever
// product it lives in — and a menu that says nothing about what is coming reads as a missing
// feature rather than a planned one.
const PLATFORMS: { id: string; label: string; note: string }[] = [
  { id: "slack", label: "Slack", note: "" },
  { id: "teams", label: "Microsoft Teams", note: "not configured" },
  { id: "gchat", label: "Google Chat", note: "not yet" },
];

export function AddWorkspaceMenu({
  installUrl,
  canInstall,
  msteams = false,
}: {
  installUrl: string;
  canInstall: boolean;
  /** Whether this deployment has a Teams app registration (/api/me). */
  msteams?: boolean;
}) {
  const [teamsOpen, setTeamsOpen] = useState(false);
  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          {/* The one thing this page can add, so it is the console's primary button — the same
              one the Memory page adds with. It keeps its own width rather than filling the rail's
              foot: a filled bar as wide as the tree would outweigh the workspaces above it. */}
          <Button>
            <Plus className="size-4" />
            Add workspace
          </Button>
        </DropdownMenuTrigger>
        {/* The console is on a 3px spacing grid, so w-80 is 240px, not Tailwind's usual 320. */}
        <DropdownMenuContent align="start" className="w-80">
          <DropdownMenuLabel className="text-xs font-normal text-muted-foreground">
            Connect a workspace from
          </DropdownMenuLabel>
          <DropdownMenuSeparator />
          {PLATFORMS.map((p) =>
            p.id === "slack" ? (
              <DropdownMenuItem key={p.id} asChild disabled={!canInstall}>
                {/* A real navigation, not a fetch: the browser leaves for Slack's consent screen. */}
                <a href={installUrl}>
                  <MessagesSquare className="size-4 shrink-0" />
                  <span className="whitespace-nowrap">{p.label}</span>
                </a>
              </DropdownMenuItem>
            ) : p.id === "teams" && msteams ? (
              // Not a navigation: a Teams app is installed in the Teams admin centre, not from
              // here, so what this opens is the steps and the code that names this account.
              <DropdownMenuItem key={p.id} onSelect={() => setTeamsOpen(true)}>
                <MessagesSquare className="size-4 shrink-0" />
                <span className="whitespace-nowrap">{p.label}</span>
              </DropdownMenuItem>
            ) : (
              <DropdownMenuItem key={p.id} disabled>
                <MessagesSquare className="size-4 shrink-0" />
                <span className="whitespace-nowrap">{p.label}</span>
                <span className="ml-auto whitespace-nowrap text-xs text-muted-foreground">{p.note}</span>
              </DropdownMenuItem>
            ),
          )}
          {!canInstall && (
            <p className="px-2 py-1.5 text-xs text-muted-foreground">
              Connecting Slack needs SLACK_CLIENT_ID and SLACK_CLIENT_SECRET on the server.
            </p>
          )}
        </DropdownMenuContent>
      </DropdownMenu>
      <ConnectTeamsDialog open={teamsOpen} onOpenChange={setTeamsOpen} />
    </>
  );
}
