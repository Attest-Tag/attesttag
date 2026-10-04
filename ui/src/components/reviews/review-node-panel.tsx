"use client";

import { useEffect, useRef } from "react";
import { AlertTriangle, BookMarked, ExternalLink, Folder, FolderInput, GitPullRequest, Pencil, RotateCcw, Square, Trash2 } from "lucide-react";
import { ErrorBanner } from "@/components/core/error-banner";
import { RelativeTime } from "@/components/core/relative-time";
import { ServiceTile } from "@/components/core/service-mark";
import { StatusChip } from "@/components/core/status-chip";
import { RepoLink } from "@/components/jobs/repo-link";
import { ReviewSettingsForm } from "@/components/reviews/review-settings-form";
import { MoveItems, type TreeActions } from "@/components/reviews/review-tree";
import {
  connectionName,
  deleteNeedsReach,
  installationURL,
  reposOf,
  restoreNeedsReach,
  settingsPath,
  stoppedBecause,
  type Resolved,
} from "@/components/reviews/review-format";
import { PausedChip, ResumeButton, planRefusalLine, usePlanRefusal } from "@/components/reviews/review-access";
import { PullReviewChip } from "@/components/reviews/review-pull-picker";
import type { StartReviewTarget } from "@/components/reviews/start-review-dialog";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Skeleton } from "@/components/ui/skeleton";
import {
  useApi,
  type ReviewConnectionNode,
  type ReviewNodeDetail,
  type ReviewPullsResponse,
  type ReviewSettingsTree,
  type ReviewTypeSummary,
} from "@/lib/api";
import { formatDate, parseTime } from "@/lib/format";

// The right-hand panel of Reviews › Settings for one node: what it is and how it stands, then
// every setting it can hold, then the one way to end it. A connection opens on its installation's
// status card, as a Slack workspace opens on its install card; a repository on the pull requests
// still waiting for a review, which is what most people come to a repository for.

export function ReviewNodePanel({
  resolved,
  tree,
  types,
  orgRepos,
  offeredModels,
  heavy,
  githubReview,
  canManage,
  canReach,
  actions,
  onTreeChanged,
  onStart,
  started,
}: {
  resolved: Resolved;
  tree: ReviewSettingsTree;
  types: ReviewTypeSummary[];
  orgRepos: string[];
  offeredModels: string[];
  heavy: string;
  /** Whether GitHub's deliveries can be heard at all here (/api/me). */
  githubReview: boolean;
  canManage: boolean;
  canReach: boolean;
  actions: TreeActions;
  onTreeChanged: () => void;
  onStart: (target: StartReviewTarget) => void;
  /** How many reviews were started from this page: each one sends the open pull requests to GitHub again. */
  started: number;
}) {
  const path = settingsPath(resolved);
  const detail = useApi<ReviewNodeDetail>(path);
  const { conn } = resolved;
  const stopped = !!conn.removed_at;
  // Stopped here, or uninstalled or suspended at GitHub: nothing under it is reviewed, or listed.
  const halted = stoppedBecause(conn);
  const restoreHeld = !canReach && restoreNeedsReach(conn);

  return (
    <div className="min-w-0 space-y-6">
      <PanelHeader resolved={resolved} canManage={canManage} canReach={canReach} actions={actions} onStart={onStart} />

      {resolved.kind === "connection" && (
        <ConnectionCard conn={conn} githubReview={githubReview} canManage={canManage} restoreHeld={restoreHeld} actions={actions} />
      )}
      {resolved.kind !== "connection" && halted && (
        <p className="rounded-lg border bg-muted/40 px-3 py-2 text-xs text-muted-foreground">
          Nothing here is reviewed: {halted.why}
          {stopped ? ", until it is restored" : ""}. The settings below are kept as they are.
        </p>
      )}
      {resolved.kind === "repo" && !halted && (
        <RepoPulls repo={resolved.repo.repo ?? ""} canManage={canManage} onStart={onStart} started={started} />
      )}

      {detail.error && !detail.data ? (
        <ErrorBanner message={detail.error} onRetry={detail.reload} />
      ) : !detail.data ? (
        <div className="space-y-3">
          <Skeleton className="h-40 w-full rounded-xl" />
          <Skeleton className="h-64 w-full rounded-xl" />
        </div>
      ) : (
        <ReviewSettingsForm
          path={path}
          level={resolved.kind}
          detail={detail.data}
          tree={tree}
          types={types}
          orgRepos={orgRepos}
          offeredModels={offeredModels}
          heavy={heavy}
          canManage={canManage}
          canReach={canReach}
          stopped={halted?.why}
          onSaved={(out) => {
            detail.mutate(out);
            onTreeChanged();
          }}
          // The tree carries every level's own settings too, which the inherited lines credit.
          onStale={() => {
            detail.reload();
            onTreeChanged();
          }}
        />
      )}

      {canManage && resolved.kind === "connection" && (
        <section className="flex flex-wrap items-center justify-between gap-3 rounded-xl border px-4 py-3">
          <div className="min-w-0 space-y-0.5">
            <h3 className="text-sm font-semibold">{stopped ? "Reviews are stopped" : "Stop reviewing this connection"}</h3>
            <p className="text-xs text-muted-foreground">
              {stopped
                ? restoreHeld
                  ? RESTORE_HELD
                  : "Restoring brings its settings, groups and repositories back as they were."
                : "Nothing under it is reviewed, not even when asked. Settings, groups and history are kept."}
            </p>
          </div>
          {stopped ? (
            <Button variant="outline" size="sm" disabled={restoreHeld} onClick={() => actions.restore(conn)}>
              <RotateCcw /> Restore reviews
            </Button>
          ) : (
            <Button
              variant="outline"
              size="sm"
              className="border-destructive/40 text-destructive hover:bg-destructive/10 hover:text-destructive"
              onClick={() => actions.stop(conn)}
            >
              <Square /> Stop reviewing
            </Button>
          )}
        </section>
      )}
    </div>
  );
}

/** Said wherever Restore is held back: the restore switches back on what an admin set going. */
const RESTORE_HELD =
  "Restoring would post live, review every push, announce in a channel or run a branch rule's own model again somewhere under it, which needs Manage connections as well.";

function PanelHeader({
  resolved,
  canManage,
  canReach,
  actions,
  onStart,
}: {
  resolved: Resolved;
  canManage: boolean;
  canReach: boolean;
  actions: TreeActions;
  onStart: (target: StartReviewTarget) => void;
}) {
  const { conn } = resolved;
  if (resolved.kind === "connection") {
    const n = reposOf(conn).length;
    return (
      <div className="flex items-center gap-3">
        <ServiceTile preset="github" className="size-9" />
        <div className="min-w-0">
          <h2 className="truncate text-base font-semibold leading-tight">{connectionName(conn)}</h2>
          <p className="text-xs text-muted-foreground">
            GitHub App · installation {conn.installation_id} · {n === 1 ? "1 repository inherits" : `${n} repositories inherit`} this
          </p>
        </div>
      </div>
    );
  }
  if (resolved.kind === "group") {
    const { group } = resolved;
    // Its repositories move up to the connection: one that inherits from the group would start
    // posting live, spending differently or being announced elsewhere under the connection, and what
    // the group sets itself — a model, a channel — goes with it.
    const deleteHeld = !canReach && deleteNeedsReach(conn, group);
    return (
      <div className="space-y-1.5">
      <div className="flex flex-wrap items-center gap-3">
        <span className="flex size-9 shrink-0 items-center justify-center rounded-md bg-accent text-accent-foreground">
          <Folder className="size-5" />
        </span>
        <div className="min-w-0 flex-1">
          <h2 className="truncate text-base font-semibold leading-tight">{group.name}</h2>
          <p className="text-xs text-muted-foreground">
            Group in {connectionName(conn)} ·{" "}
            {group.repos.length === 1 ? "1 repository inherits" : `${group.repos.length} repositories inherit`} this before the
            connection
          </p>
        </div>
        {canManage && (
          <div className="flex items-center gap-1">
            <Button variant="ghost" size="sm" onClick={() => actions.renameGroup(group)}>
              <Pencil /> Rename
            </Button>
            <Button
              variant="ghost"
              size="sm"
              disabled={deleteHeld}
              className="text-destructive hover:bg-destructive/10 hover:text-destructive"
              onClick={() => actions.deleteGroup(conn, group)}
            >
              <Trash2 /> Delete
            </Button>
          </div>
        )}
      </div>
      {canManage && deleteHeld && (
        <p className="text-right text-xs text-muted-foreground">
          Deleting it would change how its repositories post, spend or are announced under {connectionName(conn)}, which
          needs Manage connections as well.
        </p>
      )}
      </div>
    );
  }
  const { repo, group } = resolved;
  const name = repo.repo ?? "";
  return (
    <div className="flex flex-wrap items-center gap-3">
      <span className="flex size-9 shrink-0 items-center justify-center rounded-md bg-accent text-accent-foreground">
        <BookMarked className="size-5" />
      </span>
      <div className="min-w-0 flex-1">
        <h2 className="truncate font-mono text-base font-semibold leading-tight">
          <RepoLink repo={name} className="hover:underline underline-offset-2" />
        </h2>
        <p className="text-xs text-muted-foreground">
          {group ? `In ${group.name} · ` : "In no group · "}
          {connectionName(conn)}
        </p>
      </div>
      <div className="flex flex-wrap items-center gap-2">
        {canManage && (
          // The row menu's Move to…, the same items: with no group yet, New group… makes one and
          // moves this repository into it in one go.
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="outline" size="sm">
                <FolderInput /> Move to…
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end" className="w-64">
              <DropdownMenuLabel className="text-xs font-normal text-muted-foreground">Inherit from</DropdownMenuLabel>
              <DropdownMenuSeparator />
              <MoveItems conn={conn} repo={repo} group={group} canReach={canReach} actions={actions} />
            </DropdownMenuContent>
          </DropdownMenu>
        )}
        {canManage && !stoppedBecause(conn) && (
          <Button size="sm" onClick={() => onStart({ repo: name })}>
            <GitPullRequest /> Review a PR…
          </Button>
        )}
      </div>
    </div>
  );
}

/**
 * How the installation stands at GitHub — the Workspaces install card's job, for an App: who put
 * it there, whether GitHub still lets it in, whether GitHub's deliveries are arriving, and what its
 * owner still has to accept before a review can post.
 */
function ConnectionCard({
  conn,
  githubReview,
  canManage,
  restoreHeld,
  actions,
}: {
  conn: ReviewConnectionNode;
  githubReview: boolean;
  canManage: boolean;
  /** Restoring needs connections.manage, which this member does not hold. */
  restoreHeld: boolean;
  actions: TreeActions;
}) {
  const installed = parseTime(conn.installed_at);
  const status =
    conn.status === "uninstalled"
      ? { variant: "danger" as const, label: "Uninstalled" }
      : conn.status === "suspended"
        ? { variant: "warning" as const, label: "Suspended" }
        : { variant: "success" as const, label: "Installed" };
  return (
    <div className="space-y-2.5 rounded-xl border bg-card p-3">
      <div className="flex flex-wrap items-center gap-2">
        <StatusChip variant={status.variant}>{status.label}</StatusChip>
        {conn.removed_at && <StatusChip variant="neutral">Reviews stopped</StatusChip>}
        <span className="text-xs text-muted-foreground">
          {conn.installed_by ? `Installed by ${conn.installed_by}` : "Installed"}
          {installed ? ` · ${formatDate(installed)}` : ""}
          {conn.repo_selection === "all" ? " · every repository of the account" : conn.repo_selection ? " · the repositories chosen at GitHub" : ""}
        </span>
        <span className="flex-1" />
        <Button asChild variant="ghost" size="sm">
          <a href={installationURL(conn)} target="_blank" rel="noreferrer">
            At GitHub <ExternalLink />
          </a>
        </Button>
      </div>
      <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
        {!githubReview ? (
          <>
            <StatusChip variant="danger">Webhook not set up</StatusChip>
            <span>GitHub&apos;s deliveries cannot be verified on this deployment, so no pull request is heard about.</span>
          </>
        ) : conn.last_delivery_at ? (
          <>
            <StatusChip variant="success">Webhook receiving</StatusChip>
            <span>
              Last delivery <RelativeTime value={conn.last_delivery_at} />
            </span>
          </>
        ) : (
          <>
            <StatusChip variant="neutral">No delivery yet</StatusChip>
            <span>GitHub sends one when a pull request opens or is commented on.</span>
          </>
        )}
      </div>
      {conn.status === "suspended" && (
        <p className="text-xs text-warning">
          Suspended at GitHub: nothing under it can be read or posted to until it is unsuspended there.
        </p>
      )}
      {conn.status === "uninstalled" && (
        <p className="text-xs text-danger">
          The App is no longer installed on this account, so reviews stopped by themselves. Nothing here was
          deleted. Installing the App again makes a new installation, which you add with Add connection —
          copying this one&apos;s settings if you like.
        </p>
      )}
      {conn.missing_permissions.length > 0 && conn.status !== "uninstalled" && (
        <div className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-soft px-3 py-2 text-xs">
          <AlertTriangle className="mt-0.5 size-3.5 shrink-0 text-warning" />
          <p className="min-w-0 flex-1 text-foreground">
            The account&apos;s owner has to accept new permissions ({conn.missing_permissions.join(", ")}) before a
            review can post.{" "}
            <a href={installationURL(conn)} target="_blank" rel="noreferrer" className="font-medium text-primary underline-offset-2 hover:underline">
              Review the request at GitHub
            </a>
          </p>
        </div>
      )}
      {/* Quieter than the permissions above: the answers in the threads still go out, and only the
          threads stay open. GitHub does not document what resolving one needs of an App, so this
          names the token's permission rather than one to go and accept. */}
      {conn.threads_refused_at && conn.status !== "uninstalled" && (
        <div className="flex items-start gap-2 rounded-lg border bg-muted/40 px-3 py-2 text-xs">
          <AlertTriangle className="mt-0.5 size-3.5 shrink-0 text-muted-foreground" />
          <p className="min-w-0 flex-1 text-muted-foreground">
            GitHub refused to resolve a finding&apos;s thread with this installation&apos;s token (Pull requests: Read
            and write), last <RelativeTime value={conn.threads_refused_at} />. Fixed and withdrawn findings are still
            answered in their threads, which stay open for a person to resolve. This clears once a thread resolves.
          </p>
        </div>
      )}
      {conn.removed_at && canManage && (
        <div className="flex flex-wrap items-center gap-2">
          <Button size="sm" disabled={restoreHeld} onClick={() => actions.restore(conn)}>
            <RotateCcw /> Restore reviews
          </Button>
          <span className="min-w-0 flex-1 text-xs text-muted-foreground">
            {restoreHeld ? RESTORE_HELD : "Its settings, groups and repositories come back as they were."}
          </span>
        </div>
      )}
    </div>
  );
}

/**
 * A repository's open pull requests that its last review did not read — never reviewed, or pushed
 * to since — each one press from a review. Read from GitHub as the panel opens, so it is the list
 * as it is now and not as the webhook last heard it. A pull request whose automatic reviews are
 * paused is listed too, reviewed at its head or not, with Resume: the next push to it is not
 * reviewed, and this is where a team looks for why.
 */
function RepoPulls({
  repo,
  canManage,
  onStart,
  started,
}: {
  repo: string;
  canManage: boolean;
  onStart: (target: StartReviewTarget) => void;
  started: number;
}) {
  const pulls = useApi<ReviewPullsResponse>(`/api/review-pulls?repo=${encodeURIComponent(repo)}`);
  const all = pulls.data?.pulls ?? [];
  const waiting = all.filter((p) => !p.review?.reviewed_head || p.review?.paused);
  const reviewed = all.filter((p) => p.review?.reviewed_head).length;
  const anyPaused = waiting.some((p) => p.review?.paused);
  // A start the plan would refuse is not offered as one that would go.
  const refused = usePlanRefusal();
  // A review started from here is queued at once: read the list again, so the pull request it was
  // started on says Queued rather than looking untouched.
  // Only a start made while it is open: a panel opened after one already reads the list as it is.
  const { reload } = pulls;
  const seen = useRef(started);
  useEffect(() => {
    if (started === seen.current) return;
    seen.current = started;
    reload();
  }, [started, reload]);

  return (
    <section className="space-y-2">
      <div className="flex flex-wrap items-baseline justify-between gap-2 px-1">
        <h2 className="text-sm font-semibold">
          {anyPaused ? "Open pull requests not reviewed yet, or paused" : "Open pull requests not reviewed yet"}
        </h2>
        {pulls.data && (
          <span className="text-xs text-muted-foreground">
            <a
              href={`https://github.com/${repo}/pulls`}
              target="_blank"
              rel="noreferrer"
              className="underline-offset-2 hover:text-foreground hover:underline"
            >
              {all.length}
              {pulls.data.more ? "+" : ""} open
            </a>
            {/* Not a link: History lists every run on the repository, closed pull requests and
                tries included, and no filter there lands on exactly these. */}
            {reviewed > 0 && ` · ${reviewed} already reviewed at their head`}
          </span>
        )}
      </div>
      <div className="overflow-hidden rounded-xl border bg-card">
        {pulls.error && !pulls.data ? (
          <p className="px-4 py-3 text-sm text-muted-foreground">Could not list them: {pulls.error}</p>
        ) : !pulls.data ? (
          <div className="space-y-2 p-3">
            <Skeleton className="h-8 w-full" />
            <Skeleton className="h-8 w-2/3" />
          </div>
        ) : waiting.length === 0 ? (
          <p className="px-4 py-3 text-sm text-muted-foreground">
            {all.length === 0 ? "No pull request is open." : "Every open pull request was reviewed at the commit it is at."}
          </p>
        ) : (
          <ul className="divide-y">
            {waiting.slice(0, 8).map((p) => (
              <li key={p.number} className="flex flex-wrap items-center gap-x-3 gap-y-1.5 px-4 py-2.5">
                {/* A floor on the title's width, so on a phone the chips and the button wrap under
                    it rather than squeezing it to three words. */}
                <div className="min-w-[14rem] flex-1">
                  <p className="truncate text-sm">
                    <a href={p.url} target="_blank" rel="noreferrer" className="font-medium tabular-nums text-primary underline-offset-2 hover:underline">
                      #{p.number}
                    </a>{" "}
                    {p.title}
                  </p>
                  <p className="truncate text-xs text-muted-foreground">
                    <span className="font-mono">
                      {p.head} → {p.base}
                    </span>
                    {p.author ? ` · ${p.author}` : ""}
                    {p.updated_at ? (
                      <>
                        {" · updated "}
                        <RelativeTime value={p.updated_at} />
                      </>
                    ) : null}
                  </p>
                </div>
                {/* Wraps: with Paused and Resume beside the chips, a row on a phone is wider than the card. */}
                <div className="flex min-w-0 flex-wrap items-center gap-2">
                  {p.draft && <StatusChip variant="neutral">Draft</StatusChip>}
                  <PullReviewChip pull={p} link />
                  {p.review && <PausedChip paused={p.review} />}
                  {canManage && p.review?.paused && <ResumeButton repo={repo} pr={p.number} onResumed={reload} />}
                  {canManage && !p.review?.reviewed_head && (
                    // The reason on a wrapper: a disabled button takes no hover, so a title on it
                    // never shows. The page's plan strip says it in words.
                    <span title={refused ? planRefusalLine(refused) : undefined}>
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={!!refused}
                        onClick={() => onStart({ repo, pr: p.number, title: p.title })}
                      >
                        Review
                      </Button>
                    </span>
                  )}
                </div>
              </li>
            ))}
            {waiting.length > 8 && (
              <li className="px-4 py-2 text-xs text-muted-foreground">
                {canManage ? (
                  <button
                    type="button"
                    onClick={() => onStart({ repo })}
                    className="font-medium text-primary underline-offset-2 hover:underline"
                  >
                    {waiting.length - 8} more, in Review a PR…
                  </button>
                ) : (
                  `${waiting.length - 8} more not shown.`
                )}
              </li>
            )}
          </ul>
        )}
      </div>
    </section>
  );
}
