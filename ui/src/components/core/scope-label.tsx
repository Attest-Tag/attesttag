import type { Scope } from "@/lib/api";

/**
 * Resolve a Slack channel or workspace id to its scope name, falling back to the id.
 *
 * A channel id is only unique inside its workspace — a Slack Connect channel carries the same
 * C-id in every workspace it is shared into — so pass the team when the row carries one. Without
 * it the first match wins, which is fine for a single connected workspace and is why the team is
 * optional rather than required.
 */
export function scopeNameFor(scopes: Scope[] | undefined, slackId: string, teamId?: string): string {
  if (!slackId) return "";
  const match = scopes?.find(
    (s) => s.slack_id === slackId && (!teamId || s.team_id === teamId),
  );
  return match?.name ?? slackId;
}

/** The workspace a row belongs to, named, for the Team column. */
export function teamNameFor(scopes: Scope[] | undefined, teamId: string): string {
  if (!teamId) return "";
  return scopes?.find((s) => s.kind === "team" && s.team_id === teamId)?.name ?? teamId;
}

/**
 * Memory scopes are "team:T…" (everything public in one Slack workspace) or
 * "channel:T…/C…" (one conversation). Split one into a kind and the display name, using the
 * scope list to resolve the ids when we have them.
 */
export function describeMemoryScope(
  scopes: Scope[] | undefined,
  scope: string,
): { kind: string; name: string; id: string; teamId: string } {
  const [kind, ...rest] = scope.split(":");
  const id = rest.join(":");
  if (!id) return { kind: "scope", name: scope, id: scope, teamId: "" };
  if (kind === "channel") {
    // "T…/C…": the workspace, then the channel inside it.
    const slash = id.indexOf("/");
    if (slash > 0) {
      const teamId = id.slice(0, slash);
      const channel = id.slice(slash + 1);
      return { kind, name: scopeNameFor(scopes, channel, teamId), id: channel, teamId };
    }
  }
  if (kind === "team") {
    return { kind, name: teamNameFor(scopes, id), id, teamId: id };
  }
  return { kind, name: scopeNameFor(scopes, id), id, teamId: "" };
}
