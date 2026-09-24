import { useApi, type Scope } from "@/lib/api";

/**
 * The Slack workspace a row came from.
 *
 * Rows across the console are account-wide, not filtered to one workspace, so once a second
 * workspace is connected "#general" is ambiguous on its own. This names the workspace beside
 * it — and renders nothing at all while only one is connected, so the column does not become
 * a repeated word for the common case.
 */
export function useTeamNames(): { name: (teamId: string) => string; several: boolean } {
  const scopes = useApi<Scope[]>("/api/scopes?sync=0");
  const teams = (scopes.data ?? []).filter((s) => s.kind === "team");
  return {
    several: teams.length > 1,
    name: (teamId: string) =>
      teamId ? (teams.find((t) => t.team_id === teamId)?.name ?? teamId) : "",
  };
}

export function TeamName({ teamId, names }: { teamId: string; names: ReturnType<typeof useTeamNames> }) {
  if (!names.several) return null;
  return <span className="text-xs text-muted-foreground">{names.name(teamId) || "—"}</span>;
}
