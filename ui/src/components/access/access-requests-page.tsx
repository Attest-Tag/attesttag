"use client";

import { Fragment, useState } from "react";
import { ChevronRight, ShieldCheck } from "lucide-react";
import { toast } from "sonner";
import { EmptyState } from "@/components/core/empty-state";
import { ErrorBanner } from "@/components/core/error-banner";
import { PageHeader } from "@/components/core/page-header";
import { StatusChip, type StatusChipVariant } from "@/components/core/status-chip";
import { TableSkeleton } from "@/components/core/table-skeleton";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { api, errorMessage, useApi, type AccessRequest } from "@/lib/api";
import { cn } from "@/lib/utils";
import { formatDateTime } from "@/lib/format";

const chip: Record<string, StatusChipVariant> = {
  pending: "warning",
  approved: "info",
  executed: "success",
  failed: "danger",
  denied: "danger",
  expired: "neutral",
  cancelled: "neutral",
};

export function AccessRequestsPage() {
  const requests = useApi<AccessRequest[]>("/api/access-requests");
  const [open, setOpen] = useState<number | null>(null);

  return (
    <div className="space-y-5">
      <PageHeader
        title="Access requests"
        description="Who asked for what, who approved it, and what ran. Approving happens in Slack, by a named approver — this page is the record, and a way to close out something nobody answered."
      />

      {requests.error && !requests.data && (
        <ErrorBanner message={requests.error} onRetry={requests.reload} />
      )}

      {requests.loading ? (
        <TableSkeleton rows={5} />
      ) : (requests.data ?? []).length === 0 ? (
        <EmptyState
          icon={ShieldCheck}
          title="No access requests yet"
          description="When someone asks the bot for access it cannot grant on its own, it records the exact calls and sends them to an approver as a direct message. Set the approval roles, and what each may grant from, on the Approvers page to switch it on."
        />
      ) : (
        <div className="overflow-hidden rounded-xl border bg-card">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-8" />
                <TableHead>Asked</TableHead>
                <TableHead>What</TableHead>
                <TableHead>By</TableHead>
                <TableHead>Channel</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Answered by</TableHead>
                <TableHead className="text-right">Steps</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {(requests.data ?? []).map((r) => {
                const expanded = open === r.id;
                return (
                  <Fragment key={r.id}>
                    <TableRow
                      onClick={() => setOpen(expanded ? null : r.id)}
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
                      <TableCell className="whitespace-nowrap text-xs">
                        {formatDateTime(r.created_at)}
                      </TableCell>
                      <TableCell className="max-w-[20rem] truncate text-sm font-medium">
                        {r.what || "—"}
                      </TableCell>
                      <TableCell className="text-xs">{r.requester_name || r.requester}</TableCell>
                      <TableCell className="text-xs">{r.channel_name || r.channel}</TableCell>
                      <TableCell>
                        <StatusChip variant={chip[r.status] ?? "neutral"}>{r.status}</StatusChip>
                      </TableCell>
                      <TableCell className="text-xs">
                        {r.decided_by_name || r.decided_by || "—"}
                      </TableCell>
                      <TableCell className="text-right text-xs tabular-nums">{r.steps}</TableCell>
                    </TableRow>
                    {expanded && (
                      <TableRow className="hover:bg-transparent">
                        <TableCell colSpan={8} className="bg-muted/30 p-4">
                          <Detail id={r.id} summary={r} onChanged={requests.reload} />
                        </TableCell>
                      </TableRow>
                    )}
                  </Fragment>
                );
              })}
            </TableBody>
          </Table>
        </div>
      )}
    </div>
  );
}

function Detail({
  id,
  summary,
  onChanged,
}: {
  id: number;
  summary: AccessRequest;
  onChanged: () => void;
}) {
  const detail = useApi<AccessRequest>(`/api/access-requests/${id}`);
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const r = detail.data ?? summary;

  const deny = async () => {
    if (!reason.trim()) return;
    setBusy(true);
    try {
      await api.post(`/api/access-requests/${id}/deny`, { reason });
      toast.success("Closed, and the requester has been told");
      onChanged();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-4 text-sm">
      {r.why && (
        <Field label="Why">
          <p className="text-muted-foreground">{r.why}</p>
        </Field>
      )}
      {r.ask && (
        <Field label="Their words (unverified)">
          <p className="whitespace-pre-wrap text-muted-foreground">{r.ask}</p>
        </Field>
      )}
      <Field label="What runs on approval">
        <pre className="overflow-x-auto whitespace-pre-wrap rounded-lg border bg-background p-3 text-xs">
          {r.plan ?? "…"}
        </pre>
      </Field>
      <div className="grid gap-3 sm:grid-cols-3">
        <Field label="Approvers">
          <p className="font-mono text-xs">{(r.approvers ?? []).join(", ") || "—"}</p>
        </Field>
        <Field label="Expires">
          <p className="text-xs">{formatDateTime(r.expires_at)}</p>
        </Field>
        {r.reason && (
          <Field label="Reason">
            <p className="text-xs text-muted-foreground">{r.reason}</p>
          </Field>
        )}
      </div>
      {r.result && (
        <Field label="Result">
          <pre className="overflow-x-auto whitespace-pre-wrap rounded-lg border bg-background p-3 text-xs">
            {r.result}
          </pre>
        </Field>
      )}
      {r.status === "pending" && (
        <Field label="Close this out">
          <p className="mb-2 text-xs text-muted-foreground">
            Approving is a Slack act by a named approver, so it is not offered here. Closing a stale
            request is — the requester is told, and every approver&apos;s card is cleared.
          </p>
          <form
            onSubmit={(e) => {
              e.preventDefault();
              deny();
            }}
            className="flex flex-wrap items-center gap-2"
          >
            <Input
              className="h-8 w-full max-w-md text-xs"
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              placeholder="Why this is being closed — the person waiting sees it"
            />
            <Button type="submit" size="sm" variant="destructive" disabled={busy || !reason.trim()}>
              Close request
            </Button>
          </form>
        </Field>
      )}
    </div>
  );
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <div className="mb-1 text-xs font-medium text-muted-foreground">{label}</div>
      {children}
    </div>
  );
}
