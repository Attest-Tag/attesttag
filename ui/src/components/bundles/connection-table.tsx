"use client";

import { LogIn, MoreHorizontal, Pencil, RefreshCw, Terminal, Trash2, Zap, Copy } from "lucide-react";
import { toast } from "sonner";
import { ServiceTile } from "@/components/core/service-mark";
import { StatusChip } from "@/components/core/status-chip";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { api, errorMessage, useApi, type Connection, type ConnectionMembers, type OAuthStatus, type TestResult } from "@/lib/api";
import { formatDateTime, formatRelativeLong } from "@/lib/format";

export type ConnectionAction = "edit" | "rotate" | "delete" | "curl" | "oauth" | "copy";

/** Fire one call through the connection and report what came back. Shared with the grouped
 * repository list, which shows the same row menu over a different row. */
export async function testConnection(c: Connection) {
  const id = toast.loading(`Testing ${c.name}…`);
  try {
    const res = await api.post<TestResult>(`/api/connections/${c.id}/test`);
    if (res.ok) toast.success(`${c.name}: HTTP ${res.status}`, { id });
    else toast.error(`${c.name}: ${res.error ?? `HTTP ${res.status}`}`, { id, description: res.body?.slice(0, 200) });
  } catch (err) {
    toast.error(errorMessage(err), { id });
  }
}

// A bundle's connections: what each one is called (bundle/name, the name it
// goes by everywhere), what it can reach, and whether the bot has used it.
// Row actions hand off to the page.
export function ConnectionTable({
  connections,
  bundleName,
  onAction,
}: {
  connections: Connection[];
  /** Shown as a muted prefix so every connection reads as bundle/name. */
  bundleName?: string;
  onAction: (action: ConnectionAction, connection: Connection) => void;
}) {
  if (connections.length === 0) {
    return (
      <p className="px-4 py-6 text-center text-sm text-muted-foreground">
        No credentials yet. Open the bundle and connect a service.
      </p>
    );
  }

  return (
    // Fixed columns: every bundle renders its own table, and auto layout sized each one to its
    // own longest name, so Access and Status landed in a different place on every card. The
    // minimum width is what a fixed layout needs — below it the wrapper scrolls rather than
    // crushing the two chip columns into each other.
    <Table className="table-fixed min-w-[44rem]">
      <TableHeader>
        <TableRow>
          <TableHead>Name</TableHead>
          <TableHead className="w-[26%]">Access</TableHead>
          <TableHead className="w-[30%]">Status</TableHead>
          <TableHead className="w-12"></TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {connections.map((c) => {
          const active = (c.status || "active") === "active";
          return (
            <TableRow key={c.id}>
              <TableCell>
                <div className="flex items-center gap-2.5">
                  <ServiceTile preset={c.preset || "custom"} name={c.preset} />
                  <div className="min-w-0">
                    <p className="truncate font-medium">
                      {bundleName && <span className="font-normal text-muted-foreground">{bundleName}/</span>}
                      {c.name}
                    </p>
                    <p className="text-xs text-muted-foreground">
                      {c.preset && c.preset !== "custom" ? c.preset : "custom"}
                      {c.writes === "auto" ? " · writes automatic" : ""}
                      {c.writes === "all" ? " · every call confirmed" : ""}
                    </p>
                  </div>
                </div>
              </TableCell>
              <TableCell>
                <div className="flex flex-wrap items-center gap-1.5">
                  <StatusChip variant="neutral">
                    <span className="font-mono">{c.cred_type}</span>
                  </StatusChip>
                  {c.cred_type === "mcp" && <OAuthChip connectionId={c.id} />}
                  {c.cred_type === "oauth_user" && <MembersChip connectionId={c.id} />}
                  {(c.allowed_hosts ?? []).map((h) => (
                    <span key={h} className="max-w-full truncate font-mono text-xs" title={h}>
                      {h}
                    </span>
                  ))}
                </div>
              </TableCell>
              <TableCell>
                <div className="flex min-w-0 items-center gap-2 text-xs">
                  <StatusChip variant={active ? "success" : "warning"}>
                    {active ? "Active" : c.status}
                  </StatusChip>
                  <span className="truncate text-muted-foreground">
                    {c.last_used ? `Used ${formatRelativeLong(c.last_used)}` : "Never used"}
                    {(c.scope_ids?.length ?? 0) > 0 &&
                      ` · on its own in ${c.scope_ids!.length} scope${c.scope_ids!.length === 1 ? "" : "s"}`}
                  </span>
                </div>
              </TableCell>
              <TableCell>
                <DropdownMenu>
                  <DropdownMenuTrigger asChild>
                    <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${c.name}`}>
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
                    {c.cred_type === "mcp" && (
                      <DropdownMenuItem onClick={() => onAction("oauth", c)}>
                        <LogIn className="size-4" /> Sign in (OAuth)
                      </DropdownMenuItem>
                    )}
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
              </TableCell>
            </TableRow>
          );
        })}
      </TableBody>
    </Table>
  );
}

// Whether an MCP connection holds an OAuth session. Fetched per row when the
// row is rendered, so a bundle with no MCP servers makes no extra calls.
// Who has signed in for themselves. A per-person connection with nobody on it is configured but
// unusable, and that is worth seeing at a glance rather than discovering in Slack.
function MembersChip({ connectionId }: { connectionId: number }) {
  const res = useApi<ConnectionMembers>(`/api/connections/${connectionId}/members`);
  const members = res.data?.members ?? null;
  if (!members) {
    return (
      <StatusChip variant="neutral" className="opacity-60">
        {res.error ? "members unknown" : "…"}
      </StatusChip>
    );
  }
  if (members.length === 0) {
    return (
      <span title="Nobody has connected an account yet. People connect from Slack — ask the bot, or run !connect in a channel.">
        <StatusChip variant="warning">nobody connected yet</StatusChip>
      </span>
    );
  }
  const title = members
    .map((m) => `${m.account || m.slack_user_id}${m.last_used ? ` · used ${m.last_used}` : ""}`)
    .join("\n");
  return (
    <span title={title} className="whitespace-pre-line">
      <StatusChip variant="success">
        {members.length} {members.length === 1 ? "person" : "people"} connected
      </StatusChip>
    </span>
  );
}

function OAuthChip({ connectionId }: { connectionId: number }) {
  const status = useApi<OAuthStatus>(`/api/connections/${connectionId}/oauth/status`);
  const s = status.data;
  if (!s) {
    return (
      <StatusChip variant="neutral" className="opacity-60">
        OAuth · {status.error ? "unknown" : "…"}
      </StatusChip>
    );
  }
  const title = s.signed_in
    ? [
        s.expires_at > 0 ? `Expires ${formatDateTime(new Date(s.expires_at * 1000))}` : "No expiry",
        s.has_refresh ? "refreshes automatically" : "no refresh token",
        s.client_id ? `client ${s.client_id}` : "",
      ]
        .filter(Boolean)
        .join(" · ")
    : "Use Sign in (OAuth) from the row menu.";
  return (
    <span title={title}>
      <StatusChip variant={s.signed_in ? "success" : "warning"}>
        OAuth · {s.signed_in ? "signed in" : "not signed in"}
      </StatusChip>
    </span>
  );
}
