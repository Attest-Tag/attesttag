"use client";

import { Fragment, useState } from "react";
import { ChevronRight, ScrollText } from "lucide-react";
import { EmptyState } from "@/components/core/empty-state";
import { StatusChip, type StatusChipVariant } from "@/components/core/status-chip";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import type { AuditEvent } from "@/lib/api";
import { cn } from "@/lib/utils";
import { formatDateTime } from "@/lib/format";

// What each action is called on the page. The wire form is a dotted verb the server writes and
// the filter menu shows; this is the sentence a person reads in the row. Anything the server
// sends that is not here still renders — humanised from its key — so a console built before an
// event existed shows it rather than dropping it.
const ACTION_LABELS: Record<string, string> = {
  "auth.sign_in": "Signed in",
  "auth.sign_in_failed": "Sign-in refused",
  "auth.sign_out": "Signed out",
  "auth.org_switched": "Switched organisation",
  "auth.password_changed": "Password changed",
  "auth.password_reset": "Password reset",
  "auth.password_reset_requested": "Password reset requested",
  "auth.email_verified": "Email verified",
  "auth.two_factor_enrolled": "Two-factor enrolled",
  "auth.two_factor_disabled": "Two-factor turned off",
  "auth.recovery_codes_reissued": "Recovery codes reissued",
  "org.created": "Organisation created",
  "org.renamed": "Organisation renamed",
  "member.invited": "Member invited",
  "member.invite_link_created": "Invite link created",
  "member.invite_revoked": "Invitation revoked",
  "member.joined": "Member joined",
  "member.role_changed": "Role changed",
  "member.removed": "Member removed",
  "role.saved": "Role saved",
  "role.deleted": "Role deleted",
  "api_key.created": "API key created",
  "api_key.revoked": "API key revoked",
  "mcp.approved": "MCP app approved",
  "mcp.revoked": "MCP app disconnected",
  "mcp.replay_ended": "MCP app cut off: token reused",
  "connection.created": "Connection created",
  "connection.updated": "Connection updated",
  "connection.deleted": "Connection deleted",
  "connection.copied": "Connection copied",
  "bundle.created": "Bundle created",
  "bundle.updated": "Bundle updated",
  "bundle.deleted": "Bundle deleted",
  "domain.added": "Domain added",
  "domain.removed": "Domain removed",
  "scope.updated": "Channel settings changed",
  "scope.bundle_attached": "Bundle attached to channel",
  "scope.bundle_detached": "Bundle detached from channel",
  "scope.connection_attached": "Connection attached to channel",
  "scope.connection_detached": "Connection detached from channel",
  "settings.updated": "Settings changed",
  "document.uploaded": "Documents uploaded",
  "document.updated": "Document updated",
  "document.deleted": "Document deleted",
  "approver.added": "Approver added",
  "approver.removed": "Approver removed",
  "access_request.approved": "Access request approved",
  "access_request.denied": "Access request denied",
  "write.confirmed": "Write confirmed",
  "write.cancelled": "Write cancelled",
  "workspace.connected": "Workspace connected",
  "workspace.disconnected": "Workspace disconnected",
  "workspace.removed": "Workspace removed",
  "export.activity": "Activity exported",
  "export.audit": "Audit log exported",
  "plan.changed": "Plan changed",
  "retention.swept": "Retention sweep",
  "console.request": "Console request",
};

/** "connection.secret_rotated" → "Connection secret rotated", for an action the table has never heard of. */
export function actionLabel(action: string): string {
  const known = ACTION_LABELS[action];
  if (known) return known;
  const words = action.replace(/[._]+/g, " ").trim();
  return words.charAt(0).toUpperCase() + words.slice(1);
}

function outcomeTone(outcome: string): StatusChipVariant {
  switch (outcome) {
    case "ok":
      return "success";
    case "denied":
      return "danger";
    case "failed":
      return "warning";
    default:
      return "neutral";
  }
}

function outcomeLabel(outcome: string): string {
  switch (outcome) {
    case "ok":
      return "OK";
    case "denied":
      return "Denied";
    case "failed":
      return "Failed";
    default:
      return outcome || "—";
  }
}

/** The person, as the row names them: name over address, or the role when there is no person. */
function actorOf(e: AuditEvent): { primary: string; secondary: string } {
  switch (e.via) {
    case "slack":
      return { primary: e.actor_name || e.actor_slack || "Someone in Slack", secondary: e.actor_slack };
    case "operator":
      return { primary: "Operator", secondary: "the deployment's operator" };
    case "system":
      return { primary: "attest_tag", secondary: "on its own schedule" };
  }
  if (e.actor_name && e.actor_email) return { primary: e.actor_name, secondary: e.actor_email };
  return { primary: e.actor_email || e.actor_name || "—", secondary: "" };
}

const VIA_LABELS: Record<string, string> = {
  console: "Console",
  api_key: "API key",
  mcp: "MCP",
  slack: "Slack",
  operator: "Operator",
  system: "System",
};

// A refused sign-in says why, in the words the details column uses.
const REFUSALS: Record<string, string> = {
  password: "wrong password",
  two_factor: "wrong code",
  locked_out: "locked out",
  policy: "not accepted by the sign-in policy",
};

/**
 * What the row was about: the name when it has one, the id when that is all there is. A
 * sign-in has no target beyond the session, so the column says how it was made instead —
 * "via password", "wrong code" — which is what a reader scanning the column wants from it.
 */
function targetOf(e: AuditEvent): { primary: string; secondary: string } {
  const d = e.details ?? {};
  if (e.target_kind === "session") {
    if (e.action === "auth.sign_in_failed") {
      const reason = typeof d.reason === "string" ? REFUSALS[d.reason] ?? d.reason : "";
      return { primary: reason || "refused", secondary: "session" };
    }
    const via = typeof d.via === "string" ? d.via : "";
    const second = d.two_factor === true ? " + second factor" : "";
    return { primary: via ? `via ${via}${second}` : "—", secondary: "session" };
  }
  if (e.target_name && e.target_id && e.target_name !== e.target_id) {
    return { primary: e.target_name, secondary: `${e.target_kind} ${e.target_id}`.trim() };
  }
  return { primary: e.target_name || e.target_id || "—", secondary: e.target_kind };
}

export function AuditTable({
  rows,
  filtered,
}: {
  rows: AuditEvent[];
  /** The list is narrowed, which the empty state should say. */
  filtered?: boolean;
}) {
  const [open, setOpen] = useState(0);
  if (rows.length === 0) {
    return filtered ? (
      <EmptyState
        icon={ScrollText}
        title="Nothing matches"
        description="No event matches these filters. Widen the window, clear the search, or pick All actions."
      />
    ) : (
      <EmptyState
        icon={ScrollText}
        title="Nothing recorded yet"
        description="Every sign-in, every change made in the console or through the API, and every approval pressed in Slack is recorded here — who did it, from where, and what came of it."
      />
    );
  }
  return (
    // overflow-x-auto rather than the usual overflow-hidden: seven columns, two of them a
    // person and a target, do not fit a narrow window, and the address column is the one that
    // gets clipped — which is the column an investigation reads.
    <div className="overflow-x-auto rounded-xl border bg-card">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead className="w-8" />
            <TableHead>Time</TableHead>
            <TableHead>Who</TableHead>
            <TableHead>Action</TableHead>
            <TableHead>What</TableHead>
            <TableHead>Outcome</TableHead>
            <TableHead>From</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((e) => {
            const expanded = open === e.id;
            const actor = actorOf(e);
            const target = targetOf(e);
            return (
              <Fragment key={e.id}>
                <TableRow
                  onClick={() => setOpen(expanded ? 0 : e.id)}
                  aria-expanded={expanded}
                  className="cursor-pointer"
                >
                  <TableCell className="pr-0">
                    <ChevronRight
                      className={cn(
                        "size-4 text-muted-foreground transition-transform",
                        expanded && "rotate-90",
                      )}
                    />
                  </TableCell>
                  <TableCell className="whitespace-nowrap text-xs">{formatDateTime(e.at)}</TableCell>
                  <TableCell className="max-w-[14rem]">
                    <span className="block truncate text-sm">{actor.primary}</span>
                    {actor.secondary && (
                      <span className="block truncate text-xs text-muted-foreground">{actor.secondary}</span>
                    )}
                  </TableCell>
                  <TableCell>
                    <span className="block text-sm">{actionLabel(e.action)}</span>
                    {e.via !== "console" && (
                      <span className="block text-xs text-muted-foreground">via {VIA_LABELS[e.via] ?? e.via}</span>
                    )}
                  </TableCell>
                  <TableCell className="max-w-[16rem]">
                    <span className="block truncate text-sm" title={target.primary}>
                      {target.primary}
                    </span>
                    {target.secondary && (
                      <span className="block truncate text-xs text-muted-foreground">{target.secondary}</span>
                    )}
                  </TableCell>
                  <TableCell>
                    <StatusChip variant={outcomeTone(e.outcome)}>{outcomeLabel(e.outcome)}</StatusChip>
                  </TableCell>
                  <TableCell className="whitespace-nowrap font-mono text-xs">{e.ip || "—"}</TableCell>
                </TableRow>
                {expanded && (
                  <TableRow className="hover:bg-transparent">
                    <TableCell colSpan={7} className="bg-muted/30 p-4">
                      <EventDetail event={e} />
                    </TableCell>
                  </TableRow>
                )}
              </Fragment>
            );
          })}
        </TableBody>
      </Table>
    </div>
  );
}

/** The whole row: every field the table had no column for, and the details the event wrote. */
function EventDetail({ event: e }: { event: AuditEvent }) {
  const facts: [string, string][] = [
    ["Action", e.action],
    ["Via", VIA_LABELS[e.via] ?? e.via],
    ["Actor id", e.actor_id],
    ["Actor email", e.actor_email],
    ["Slack user", e.actor_slack],
    ["Workspace", e.team_id],
    ["Target", [e.target_kind, e.target_id].filter(Boolean).join(" ")],
    ["Address", e.ip],
    ["Browser", e.user_agent],
    ["Event id", String(e.id)],
  ];
  const details = e.details && Object.keys(e.details).length > 0 ? JSON.stringify(e.details, null, 2) : "";
  return (
    <div className="grid gap-4 md:grid-cols-[minmax(0,1fr)_minmax(0,1fr)]">
      {/* minmax(0,1fr), not 1fr: a user-agent string is one long token, and a bare 1fr track
          grows to its min-content width, which pushed the browser line out past the panel. */}
      <dl className="grid grid-cols-[7rem_minmax(0,1fr)] gap-x-3 gap-y-1 text-xs">
        {facts
          .filter(([, v]) => v)
          .map(([k, v]) => (
            <Fragment key={k}>
              <dt className="text-muted-foreground">{k}</dt>
              <dd className="min-w-0 break-words font-mono">{v}</dd>
            </Fragment>
          ))}
      </dl>
      <div className="space-y-1.5">
        <p className="text-xs font-semibold text-foreground">Details</p>
        {details ? (
          <pre className="max-h-80 overflow-auto rounded-lg border bg-background p-3 font-mono text-xs leading-relaxed whitespace-pre-wrap break-words">
            {details}
          </pre>
        ) : (
          <p className="text-xs text-muted-foreground">This event recorded nothing beyond the row itself.</p>
        )}
      </div>
    </div>
  );
}
