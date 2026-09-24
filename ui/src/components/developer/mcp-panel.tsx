"use client";

import Link from "next/link";
import { Check, Minus, Plug } from "lucide-react";
import { toast } from "sonner";
import { useConfirm } from "@/components/core/confirm-dialog";
import { CopyButton } from "@/components/core/copy-button";
import { EmptyState } from "@/components/core/empty-state";
import { ErrorBanner } from "@/components/core/error-banner";
import { PageHeader } from "@/components/core/page-header";
import { RelativeTime } from "@/components/core/relative-time";
import { StatusChip, type StatusChipVariant } from "@/components/core/status-chip";
import { TableSkeleton } from "@/components/core/table-skeleton";
import { useAuth } from "@/components/shell/auth-provider";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { api, errorMessage, useApi, type McpGrant, type McpInfo } from "@/lib/api";
import { parseTime } from "@/lib/format";

function Snippet({ label, code }: { label: string; code: string }) {
  return (
    <div className="space-y-1.5">
      <div className="flex items-center justify-between gap-2">
        <span className="text-xs font-medium text-muted-foreground">{label}</span>
        <CopyButton text={code} label={`Copy ${label.toLowerCase()}`} />
      </div>
      <pre className="overflow-x-auto rounded-lg border bg-muted/50 p-3 font-mono text-xs leading-relaxed">
        {code}
      </pre>
    </div>
  );
}

/** What a connection is now, in one word. Lapsing is judged by the browser's clock, like keys. */
function grantState(g: McpGrant): { label: string; variant: StatusChipVariant } {
  if (g.revoked_at) return { label: "Disconnected", variant: "neutral" };
  const at = g.expires_at ? parseTime(g.expires_at) : null;
  if (at && at.getTime() <= Date.now()) return { label: "Lapsed", variant: "danger" };
  return { label: "Connected", variant: "success" };
}

/**
 * attest_tag as an MCP server: the address to give a client, the two ways in, the apps people
 * have connected, and the tools — each one a /v1 route, offered to a caller only when their role
 * holds what the route asks for.
 */
export function McpPanel() {
  const info = useApi<McpInfo>("/api/mcp");
  const { me } = useAuth();
  const { confirm, confirmDialog } = useConfirm();

  const perms = me?.user?.permissions;
  const canManage = !perms || perms["api_keys.manage"] === true;
  const url = info.data?.url ?? "";
  const grants = info.data?.grants ?? [];
  const tools = info.data?.tools ?? [];

  const disconnect = async (g: McpGrant) => {
    const ok = await confirm({
      title: `Disconnect ${g.client_name}?`,
      description:
        "It stops working at once. Using it again means connecting it again: signing in and pressing Allow.",
      confirmLabel: "Disconnect",
      destructive: true,
    });
    if (!ok) return;
    try {
      await api.del(`/api/mcp/grants/${g.id}`);
      toast.success("Disconnected");
      info.reload();
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  const withKey = `claude mcp add --transport http attest-tag ${url} \\\n  --header "Authorization: Bearer $ATTESTTAG_KEY"`;
  const withOAuth = `claude mcp add --transport http attest-tag ${url}\n# then, in Claude Code: /mcp → attest-tag → Authenticate`;
  const json = JSON.stringify(
    { mcpServers: { "attest-tag": { type: "http", url, headers: { Authorization: "Bearer <your API key>" } } } },
    null,
    2,
  );

  return (
    <div className="space-y-5">
      <PageHeader
        title="MCP"
        description={
          <>
            Claude, Cursor, VS Code and any other MCP client can read and change what the bot knows,
            as whoever connects it. Every tool is one route of the{" "}
            <Link href="/developer/api-reference" className="text-primary hover:underline">
              API
            </Link>
            , with the same permission, so a client can do exactly what its person can and nothing
            more.
          </>
        }
      />

      {info.error && !info.data && <ErrorBanner message={info.error} onRetry={info.reload} />}
      {!info.data && !info.error && <TableSkeleton rows={4} />}

      {info.data && (
        <>
          <Card className="gap-2 p-4">
            <span className="text-xs font-medium text-muted-foreground">Server URL</span>
            <div className="flex items-center gap-1">
              <code className="min-w-0 flex-1 truncate rounded bg-muted/50 px-2 py-1.5 font-mono text-sm">
                {url}
              </code>
              <CopyButton text={url} label="Copy the server URL" />
            </div>
            <p className="text-xs text-muted-foreground">
              Streamable HTTP. It takes an OAuth token from a client you connected below, or an API
              key sent as <code className="rounded bg-secondary px-1">Authorization: Bearer</code>.
            </p>
          </Card>

          <div className="grid gap-4 lg:grid-cols-2">
            <Card className="gap-3 p-4">
              <div>
                <h2 className="text-sm font-semibold">Connect with OAuth</h2>
                <p className="mt-1 text-sm text-muted-foreground">
                  For Claude, Claude Desktop, Cursor and VS Code. Give the app the server URL; it
                  opens this console, you sign in and press Allow, and it acts as you.
                </p>
              </div>
              <ul className="list-disc space-y-1 pl-5 text-sm text-muted-foreground">
                <li>
                  <span className="text-foreground">Claude and Claude Desktop:</span> Settings →
                  Connectors → Add custom connector, and paste the URL.
                </li>
                <li>
                  <span className="text-foreground">Cursor and VS Code:</span> add it as an HTTP MCP
                  server; each opens a browser to sign in.
                </li>
              </ul>
              <Snippet label="Claude Code" code={withOAuth} />
              {info.data.refusal && (
                <p className="rounded-md border border-warning/30 bg-warning-soft px-3 py-2 text-xs text-warning">
                  {info.data.refusal}
                </p>
              )}
            </Card>

            <Card className="gap-3 p-4">
              <div>
                <h2 className="text-sm font-semibold">Connect with an API key</h2>
                <p className="mt-1 text-sm text-muted-foreground">
                  For scripts, CI, and clients configured with a header. The key is a{" "}
                  <Link href="/developer/api-keys" className="text-primary hover:underline">
                    developer API key
                  </Link>
                  ; it carries its maker&apos;s access, and revoking it cuts the client off.
                </p>
              </div>
              <Snippet label="Claude Code" code={withKey} />
              <Snippet label="JSON config (.mcp.json, Cursor's mcp.json)" code={json} />
              <p className="text-xs text-muted-foreground">
                Keep the key out of anything you commit.
              </p>
            </Card>
          </div>

          <section className="space-y-2">
            <h2 className="text-sm font-semibold">Connected apps</h2>
            {grants.length === 0 ? (
              <EmptyState
                icon={Plug}
                title="Nothing connected with OAuth yet"
                description="When somebody connects an app with the server URL and presses Allow, it appears here, and they — or anyone who may manage API keys — can disconnect it."
              />
            ) : (
              <Card className="overflow-hidden p-0">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>App</TableHead>
                      <TableHead>Acts as</TableHead>
                      <TableHead>Connected</TableHead>
                      <TableHead>Last used</TableHead>
                      <TableHead>Status</TableHead>
                      <TableHead className="w-0" />
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {grants.map((g) => {
                      const state = grantState(g);
                      const mine = !!me?.user?.email && g.owner === me.user.email;
                      return (
                        <TableRow key={g.id}>
                          <TableCell>
                            <div className="font-medium">{g.client_name}</div>
                            <div className="font-mono text-xs text-muted-foreground">{g.returns_to}</div>
                          </TableCell>
                          <TableCell className="text-muted-foreground">{g.owner || "—"}</TableCell>
                          <TableCell className="text-muted-foreground">
                            <RelativeTime value={g.created_at} />
                          </TableCell>
                          <TableCell className="text-muted-foreground">
                            {g.last_used_at ? <RelativeTime value={g.last_used_at} /> : "Never"}
                          </TableCell>
                          <TableCell>
                            <StatusChip variant={state.variant}>{state.label}</StatusChip>
                          </TableCell>
                          <TableCell className="text-right">
                            {(mine || canManage) && !g.revoked_at && (
                              <Button variant="ghost" size="sm" onClick={() => disconnect(g)}>
                                Disconnect
                              </Button>
                            )}
                          </TableCell>
                        </TableRow>
                      );
                    })}
                  </TableBody>
                </Table>
              </Card>
            )}
          </section>

          <section className="space-y-2">
            <h2 className="text-sm font-semibold">Tools</h2>
            <Card className="overflow-hidden p-0">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Tool</TableHead>
                    <TableHead>What it does</TableHead>
                    <TableHead>Needs</TableHead>
                    <TableHead className="text-center">You</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {tools.map((t) => (
                    <TableRow key={t.name}>
                      <TableCell className="align-top">
                        <code className="font-mono text-xs">{t.name}</code>
                        {!t.read_only && (
                          <StatusChip variant={t.destructive ? "danger" : "warning"} className="ml-2">
                            {t.destructive ? "deletes" : "writes"}
                          </StatusChip>
                        )}
                      </TableCell>
                      <TableCell className="whitespace-normal text-sm text-muted-foreground">
                        {t.description}
                      </TableCell>
                      <TableCell className="align-top">
                        {t.permission ? (
                          <code className="rounded bg-secondary px-1 font-mono text-xs">{t.permission}</code>
                        ) : (
                          <span className="text-xs text-muted-foreground">Any member</span>
                        )}
                      </TableCell>
                      <TableCell className="text-center align-top">
                        {t.allowed ? (
                          <Check className="mx-auto size-4 text-success" aria-label="Your role may use it" />
                        ) : (
                          <Minus className="mx-auto size-4 text-muted-foreground" aria-label="Your role may not" />
                        )}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </Card>
            <p className="text-xs text-muted-foreground">
              A client is only offered the tools its person&apos;s role may use, and every call is
              rate limited and recorded like an API call — writes appear in the audit log as MCP.
            </p>
          </section>
        </>
      )}

      {confirmDialog}
    </div>
  );
}
