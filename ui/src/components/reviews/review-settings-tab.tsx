"use client";

import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import { AlertTriangle, GitPullRequest, Webhook } from "lucide-react";
import { CopyButton } from "@/components/core/copy-button";
import { EmptyState } from "@/components/core/empty-state";
import { ErrorBanner } from "@/components/core/error-banner";
import { ReviewNodePanel } from "@/components/reviews/review-node-panel";
import { AddConnectionMenu, ReviewTree } from "@/components/reviews/review-tree";
import { useReviewTreeActions } from "@/components/reviews/review-tree-actions";
import {
  connectionName,
  reposOf,
  resolveSelection,
  revealPanel,
  selectionOf,
  type Resolved,
  type Selection,
} from "@/components/reviews/review-format";
import { markInstallFromReviews } from "@/components/reviews/install-return";
import { StartReviewDialog, type StartReviewTarget } from "@/components/reviews/start-review-dialog";
import { useAuth } from "@/components/shell/auth-provider";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { useStickyValue } from "@/hooks/use-sticky";
import {
  onConsoleChange,
  useApi,
  type GithubInstalls,
  type ReviewSettingsTree,
  type ReviewTypeSummary,
  type SettingsResponse,
} from "@/lib/api";
import { announceReviewsSelection } from "@/lib/assistant-screen";

// Reviews › Settings, laid out like Workspaces: the tree of GitHub connections, their groups and
// repositories on the left, one node's settings on the right. The selection rides on ?node= — a
// node's public id, or a repository's owner/name, which is all a link from elsewhere knows and the
// only name a repository with nothing set on it has — so a link to one repository survives a
// reload. ?github_install= (where an install comes back to) and ?add= (Access bundles' link for a
// repository whose installation nobody added yet) pick the installation they name: the connection
// if it is in the tree, else Add connection for it. With none of them, the node this browser had
// open last. A card in the assistant whose Open is pressed here hands its node over instead
// (CONSOLE_CHANGED_EVENT's select), as ?node= and ?conn= would on arrival.


export function ReviewSettingsTab() {
  const { me } = useAuth();
  // /api/me sends only the permissions the role holds, so a missing key is "no", and an absent
  // map — /api/me not answered yet — shows everything rather than flashing it away.
  const perms = me?.user?.permissions;
  const canManage = !perms || perms["reviews.manage"] === true;
  const canReach = !perms || perms["connections.manage"] === true;
  const canSeeInstalls = !perms || perms["connections.view"] === true;

  const tree = useApi<ReviewSettingsTree>("/api/review-settings");
  const installs = useApi<GithubInstalls>(canSeeInstalls ? "/api/github/installations" : null);
  const types = useApi<{ types: ReviewTypeSummary[] }>("/api/review-types");
  const settings = useApi<SettingsResponse>("/api/settings");

  const [sel, setSel] = useState<Selection | null>(null);
  const [start, setStart] = useState<StartReviewTarget | null>(null);
  // Reviews started from this tab, so a repository's list of pull requests not reviewed yet reads
  // GitHub again and the one just started says Queued.
  const [started, setStarted] = useState(0);
  const lastConn = useRef<string>("");
  const panel = useRef<HTMLDivElement>(null);
  // The node this browser had open last, for an arrival that names none.
  const kept = useStickyValue<Selection>("reviews:node");

  const reload = tree.reload;
  const { actions, dialogs } = useReviewTreeActions({
    tree: tree.data,
    canReach,
    onChanged: reload,
    onSelectNode: setSel,
  });

  const resolved = tree.data ? resolveSelection(tree.data, sel) : null;
  const resolvedConn = resolved?.conn.id ?? "";
  useEffect(() => {
    if (resolvedConn) lastConn.current = resolvedConn;
  }, [resolvedConn]);

  // First paint: the node the URL names, else the installation an install came back with, else
  // the first connection.
  const arrived = useRef(false);
  useEffect(() => {
    const data = tree.data;
    if (!data || !kept.ready || arrived.current) return;
    arrived.current = true;
    const params = new URLSearchParams(window.location.search);
    const node = params.get("node");
    const conn = params.get("conn") ?? undefined;
    const back = params.get("github_install");
    const installed = Number(back) || Number(params.get("add")) || 0;
    let first: Selection | null = node && resolveSelection(data, { node, conn }) ? { node, conn } : null;
    if (back !== null) {
      // Back from installing the App (install-return.ts): said once, and dropped from the address.
      const n = Number(params.get("connected_repos")) || 0;
      toast.success(
        n > 0 ? `GitHub App installed — ${n === 1 ? "1 repository" : `${n} repositories`} connected` : "GitHub App installed",
      );
      const url = new URL(window.location.href);
      url.searchParams.delete("github_install");
      url.searchParams.delete("connected_repos");
      window.history.replaceState(null, "", url);
    }
    if (params.has("add")) {
      // Spent on arrival like the install's mark: a reload must not open the dialog again.
      const url = new URL(window.location.href);
      url.searchParams.delete("add");
      window.history.replaceState(null, "", url);
    }
    if (!first && installed > 0) {
      // In the tree already (installed again over a connection that was reviewed): open it. Not
      // yet: Add connection for it, since installing reviews nothing by itself.
      const c = data.connections.find((x) => x.installation_id === installed);
      const a = data.available.find((x) => x.installation_id === installed);
      if (c) first = { node: c.id };
      else if (a && canManage) actions.add(a);
    }
    // A link that named a node — Access bundles' "Code review settings…" — came for that panel, so
    // on a phone it is brought up past the tree, as a tap there does.
    if (first) revealPanel(panel.current);
    if (!first && installed === 0) {
      // Nothing named: the node this browser had open last, as Types remembers its type — unless
      // it is gone, or a value somebody edited by hand.
      const k = kept.value;
      if (typeof k?.node === "string" && resolveSelection(data, k)) first = k;
    }
    if (!first && data.connections.length > 0) first = { node: data.connections[0].id };
    if (first) setSel(first);
  }, [tree.data, kept.ready, kept.value, actions, canManage]);

  // A selection that went away — its group deleted, a reload that moved a repository — falls back
  // to the connection it was under rather than leaving the panel on a skeleton. Not while a reload
  // is in flight: a group just created is selected before the tree that holds it has arrived.
  useEffect(() => {
    const data = tree.data;
    if (!data || !sel || resolved || tree.refreshing) return;
    const conn = data.connections.find((c) => c.id === lastConn.current) ?? data.connections[0];
    setSel(conn ? { node: conn.id } : null);
  }, [tree.data, sel, resolved, tree.refreshing]);

  useEffect(
    () =>
      onConsoleChange(({ select }) => {
        if (select?.tab !== "settings" || !select.node) return;
        setSel({ node: select.node, conn: select.conn || undefined });
        revealPanel(panel.current);
      }),
    [],
  );

  // The address bar follows the selection, so a reload or a shared link comes back to it — and the
  // assistant's chip with it, under the name a person knows the node by, since a connection's or a
  // group's ?node= is an id.
  const selKey = resolved && tree.data ? JSON.stringify(selectionOf(resolved, tree.data)) : "";
  const selName = !resolved
    ? ""
    : resolved.kind === "connection"
      ? connectionName(resolved.conn)
      : resolved.kind === "group"
        ? `${connectionName(resolved.conn)} / ${resolved.group.name}`
        : (resolved.repo.repo ?? "");
  const keep = kept.set;
  useEffect(() => {
    if (!selKey) return;
    const s = JSON.parse(selKey) as Selection;
    const url = new URL(window.location.href);
    url.searchParams.set("node", s.node);
    if (s.conn) url.searchParams.set("conn", s.conn);
    else url.searchParams.delete("conn");
    window.history.replaceState(null, "", url);
    keep(s);
    announceReviewsSelection(s.node, selName);
  }, [selKey, selName, keep]);

  const select = (r: Resolved) => {
    if (!tree.data) return;
    setSel(selectionOf(r, tree.data));
    revealPanel(panel.current);
  };

  // What the deployment is missing, from the installations endpoint when this member may read it,
  // else from /api/me's one bit — which is enough to say the webhook is not heard, if not why.
  const missing = installs.data?.review_missing ?? [];
  const appMissing = missing.includes("GITHUB_APP_ID");
  const githubReview = me?.github_review ?? true;
  const hookMissing = !appMissing && (missing.includes("GITHUB_APP_WEBHOOK_SECRET") || !githubReview);
  const install = { configured: installs.data?.install_configured ?? false, url: installs.data?.install_url ?? "/github/install" };

  const data = tree.data;
  // A repository removed from code review is still one of the organisation's App connections, which
  // another repository's review may read.
  const orgRepos = data
    ? [
        ...new Set([
          ...data.connections.flatMap((c) => [...reposOf(c), ...c.removed].map((r) => r.repo ?? "")),
          ...data.available.flatMap((a) => a.repos),
        ]),
      ]
        .filter(Boolean)
        .sort()
    : [];

  if (tree.loading) {
    return (
      <div className="grid gap-5 lg:grid-cols-[18rem_1fr]">
        <div className="space-y-2">
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} className="h-12 w-full rounded-xl" />
          ))}
        </div>
        <Skeleton className="h-96 w-full rounded-xl" />
      </div>
    );
  }
  if (tree.error && !data) return <ErrorBanner message={tree.error} onRetry={tree.reload} />;
  if (!data) return null;

  const nothing = data.connections.length === 0;

  return (
    <div className="space-y-5">
      {appMissing && !nothing && (
        <Banner tone="danger" icon={AlertTriangle} title="The GitHub App is not set up on this deployment">
          Nothing here is reviewed until it is: set {missing.filter((m) => m !== "GITHUB_APP_WEBHOOK_SECRET").join(", ")} on the
          server.
        </Banner>
      )}
      {hookMissing && (
        <Banner tone="warning" icon={Webhook} title="GitHub's deliveries are not set up">
          Pull requests are not reviewed as they open, and commands and replies on GitHub go unheard, until the
          App&apos;s webhook is on and GITHUB_APP_WEBHOOK_SECRET is set on the server. A review started from this
          console still runs.
          {installs.data?.webhook_url && (
            <span className="mt-1.5 flex flex-wrap items-center gap-1.5">
              Webhook URL: <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs">{installs.data.webhook_url}</code>
              <CopyButton text={installs.data.webhook_url} label="Copy the webhook URL" />
            </span>
          )}
        </Banner>
      )}

      {nothing && appMissing ? (
        <EmptyState
          icon={GitPullRequest}
          title="Code review needs the GitHub App"
          description={`Reviews read pull requests and post to them through the deployment's GitHub App. Set ${missing.join(", ")} on the server, then install the App on the account that owns your repositories.`}
        />
      ) : nothing && data.available.length === 0 ? (
        <EmptyState
          icon={GitPullRequest}
          title="No GitHub account connected"
          description="Install the GitHub App on the account that owns your repositories. Its installation then appears under Add connection, ready to review in Shadow; a repository connected with a pasted token cannot be reviewed, since GitHub tells nobody about its pull requests."
          action={
            install.configured && canReach ? (
              <Button asChild>
                <a href={install.url} onClick={markInstallFromReviews}>
                  Install the GitHub App
                </a>
              </Button>
            ) : (
              <p className="text-xs text-muted-foreground">
                {install.configured
                  ? "Installing it needs Manage connections."
                  : "Installing it needs the App's OAuth client on the server."}
              </p>
            )
          }
        />
      ) : nothing ? (
        <EmptyState
          icon={GitPullRequest}
          title="Nothing is reviewed yet"
          description={`The organisation has ${data.available.length === 1 ? "a GitHub App installation" : `${data.available.length} GitHub App installations`}, and installing the App reviews nothing by itself. Add one here and its pull requests are reviewed in Shadow — recorded on this page, with nothing posted on GitHub until you make it Live.`}
          action={
            <AddConnectionMenu
              available={data.available}
              install={install}
              canManage={canManage}
              canReach={canReach}
              onPick={actions.add}
            />
          }
        />
      ) : (
        <div className="grid gap-5 lg:grid-cols-[18rem_1fr]">
          <ReviewTree
            tree={data}
            selected={resolved}
            onSelect={select}
            canManage={canManage}
            canReach={canReach}
            install={install}
            actions={actions}
          />
          <div ref={panel} className="min-w-0 scroll-mt-4">
            {resolved ? (
              <ReviewNodePanel
                // The group is in the key as well as the node: a repository moved into a group, or
                // out of one when it is deleted, keeps its name and its settings route, and only a
                // fresh panel reads what it inherits now.
                key={`${selKey}:${resolved.kind === "repo" ? (resolved.group?.id ?? "") : ""}`}
                resolved={resolved}
                tree={data}
                types={types.data?.types ?? []}
                orgRepos={orgRepos}
                offeredModels={settings.data?.effective.ChannelModels ?? []}
                heavy={settings.data?.effective.HeavyModel ?? ""}
                githubReview={githubReview}
                canManage={canManage}
                canReach={canReach}
                actions={actions}
                onTreeChanged={reload}
                onStart={setStart}
                started={started}
              />
            ) : (
              <Skeleton className="h-96 w-full rounded-xl" />
            )}
          </div>
        </div>
      )}

      {dialogs}
      <StartReviewDialog
        target={start}
        onOpenChange={(open) => !open && setStart(null)}
        onQueued={() => setStarted((n) => n + 1)}
      />
    </div>
  );
}

function Banner({
  tone,
  icon: Icon,
  title,
  children,
}: {
  tone: "warning" | "danger";
  icon: React.ComponentType<{ className?: string }>;
  title: string;
  children: React.ReactNode;
}) {
  return (
    <div
      className={
        tone === "danger"
          ? "flex gap-3 rounded-xl border border-danger/30 bg-danger-soft px-4 py-3 text-sm"
          : "flex gap-3 rounded-xl border border-warning/30 bg-warning-soft px-4 py-3 text-sm"
      }
    >
      <Icon className={tone === "danger" ? "mt-0.5 size-4 shrink-0 text-danger" : "mt-0.5 size-4 shrink-0 text-warning"} />
      <div className="min-w-0 space-y-0.5">
        <p className="font-medium text-foreground">{title}</p>
        <div className="text-xs leading-relaxed text-muted-foreground">{children}</div>
      </div>
    </div>
  );
}
