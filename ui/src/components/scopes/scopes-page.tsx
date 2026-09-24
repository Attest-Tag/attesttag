"use client";

import { useEffect, useState } from "react";
import { MessagesSquare } from "lucide-react";
import { toast } from "sonner";
import { EmptyState } from "@/components/core/empty-state";
import { ErrorBanner } from "@/components/core/error-banner";
import { PageHeader } from "@/components/core/page-header";
import { ScopeDetail } from "@/components/scopes/scope-detail";
import { AddWorkspaceMenu, ScopeList } from "@/components/scopes/scope-list";
import { useAuth } from "@/components/shell/auth-provider";
import { Skeleton } from "@/components/ui/skeleton";
import { api, errorMessage, useApi, type Bundle, type Scope, type TeamsResponse } from "@/lib/api";

// Master-detail over the whole hierarchy: the Workspace (this account), every connected Slack
// workspace, and the channels the bot is in inside each. The selection rides on ?scope= so a
// link to one channel survives a reload, and ?team= (what the install callback redirects to)
// selects a freshly connected workspace.
export function ScopesPage() {
  const scopes = useApi<Scope[]>("/api/scopes");
  const bundles = useApi<Bundle[]>("/api/bundles");
  const teams = useApi<TeamsResponse>("/api/teams");
  // Whether a Teams organisation can be connected here at all: a deployment fact, so it rides on
  // /api/me like every other one.
  const msteams = useAuth().me?.msteams ?? false;
  const [selectedId, setSelectedId] = useState<number | null>(null);

  // The install flow comes back through a redirect, so its outcome arrives in the URL.
  useEffect(() => {
    const url = new URL(window.location.href);
    const err = url.searchParams.get("install_error");
    if (err) {
      toast.error(err);
      url.searchParams.delete("install_error");
      window.history.replaceState(null, "", url);
    }
  }, []);

  // First paint picks the scope named in the URL, then the workspace named in it, else the account.
  useEffect(() => {
    if (!scopes.data || scopes.data.length === 0 || selectedId !== null) return;
    const params = new URLSearchParams(window.location.search);
    const fromScope = scopes.data.find((s) => s.id === Number(params.get("scope")));
    const fromTeam = params.get("team")
      ? scopes.data.find((s) => s.kind === "team" && s.team_id === params.get("team"))
      : undefined;
    const first = scopes.data.find((s) => s.kind === "workspace") ?? scopes.data[0];
    // eslint-disable-next-line react-hooks/set-state-in-effect -- one-shot URL read after data lands
    setSelectedId((fromScope ?? fromTeam ?? first).id);
  }, [scopes.data, selectedId]);

  const select = (id: number) => {
    setSelectedId(id);
    const url = new URL(window.location.href);
    url.searchParams.set("scope", String(id));
    url.searchParams.delete("team");
    window.history.replaceState(null, "", url);
  };

  const selected = scopes.data?.find((s) => s.id === selectedId) ?? null;
  const teamList = teams.data?.teams ?? [];
  const canInstall = teams.data?.install_configured ?? false;
  const installUrl = teams.data?.install_url ?? "/slack/install";
  // Only the account scope exists: nothing is connected yet.
  const nothingConnected = scopes.data && teamList.length === 0;

  // Ask Slack for one workspace's channels again. The answer is a count rather than a list: what
  // the rail draws still comes from /api/scopes, reloaded right after, so there is one place a
  // scope row is ever built from.
  const refreshTeam = async (teamID: string) => {
    const name = teamList.find((t) => t.team_id === teamID)?.name ?? "the workspace";
    try {
      const res = await api.post<{ channels: number; added: number }>(`/api/teams/${teamID}/sync`, {});
      scopes.reload();
      toast.success(
        res.added > 0
          ? `${name}: ${res.added} new channel${res.added === 1 ? "" : "s"}`
          : `${name} is up to date — ${res.channels} channel${res.channels === 1 ? "" : "s"}`,
      );
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  const reloadAll = () => {
    scopes.reload();
    bundles.reload();
    teams.reload();
  };

  return (
    <div className="space-y-5">
      <PageHeader
        title="Workspaces"
        description="The workspaces this account is connected to, and what the bot may reach in each — from the whole account down to a single channel."
      />

      {scopes.error && !scopes.data && (
        <ErrorBanner message={scopes.error} onRetry={scopes.reload} />
      )}

      {scopes.loading ? (
        <div className="grid gap-5 lg:grid-cols-[18rem_1fr]">
          <div className="space-y-2">
            {Array.from({ length: 5 }).map((_, i) => (
              <Skeleton key={i} className="h-12 w-full rounded-xl" />
            ))}
          </div>
          <Skeleton className="h-96 w-full rounded-xl" />
        </div>
      ) : nothingConnected ? (
        <EmptyState
          icon={MessagesSquare}
          title="No workspace connected"
          description={
            canInstall || msteams
              ? "Connect one and the bot appears in it. You can connect several — each keeps its own channels, instructions and budget, under the settings you put on the Workspace above them."
              : "Connecting a workspace needs SLACK_CLIENT_ID and SLACK_CLIENT_SECRET on the server, and the callback URL registered on the Slack app."
          }
          action={<AddWorkspaceMenu installUrl={installUrl} canInstall={canInstall} msteams={msteams} />}
        />
      ) : scopes.data ? (
        <div className="grid gap-5 lg:grid-cols-[18rem_1fr]">
          <ScopeList
            scopes={scopes.data}
            teams={teamList}
            selectedId={selectedId}
            onSelect={select}
            installUrl={installUrl}
            canInstall={canInstall}
            msteams={msteams}
            onRefreshTeam={refreshTeam}
          />
          {selected ? (
            <ScopeDetail
              key={selected.id}
              scope={selected}
              team={teamList.find((t) => t.team_id === selected.team_id)}
              bundles={bundles.data ?? []}
              onChanged={reloadAll}
              onRemoved={(teamID) => {
                // The channel is about to disappear from the rail, and a selection pointing at a
                // row that is no longer there leaves this pane on a skeleton. Land on the
                // workspace it was in — the place where it can be invited back.
                const parent = scopes.data?.find((s) => s.kind === "team" && s.team_id === teamID);
                select(parent ? parent.id : (scopes.data?.find((s) => s.kind === "workspace")?.id ?? 0));
                reloadAll();
              }}
              onRemovedWorkspace={() => {
                // A whole workspace has gone, its channels with it, so there is no parent to
                // fall back to: land on the account above them all.
                select(scopes.data?.find((s) => s.kind === "workspace")?.id ?? 0);
                reloadAll();
              }}
            />
          ) : (
            <Skeleton className="h-96 w-full rounded-xl" />
          )}
        </div>
      ) : null}
    </div>
  );
}
