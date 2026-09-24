"use client";

import { useState } from "react";
import { KeyRound, Package, Plus, ShieldCheck, Trash2, X } from "lucide-react";
import { toast } from "sonner";
import { AttachedChip } from "@/components/core/attached-chip";
import { useConfirm } from "@/components/core/confirm-dialog";
import { EmptyState } from "@/components/core/empty-state";
import { ErrorBanner } from "@/components/core/error-banner";
import { PageHeader } from "@/components/core/page-header";
import { StatusChip } from "@/components/core/status-chip";
import { TableSkeleton } from "@/components/core/table-skeleton";
import { AddAccessPopover } from "@/components/scopes/add-access-popover";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api, errorMessage, useApi, type ApprovalRole, type Bundle, type Connection } from "@/lib/api";

export function ApproversPage() {
  const roles = useApi<ApprovalRole[]>("/api/approval-roles");
  const bundles = useApi<Bundle[]>("/api/bundles");
  const { confirm, confirmDialog } = useConfirm();
  const [adding, setAdding] = useState(false);
  const [name, setName] = useState("");
  const [rank, setRank] = useState("1");

  // A tier's header count comes from the server; the chips under it read the bundle list. Refresh
  // both after a change so the count and the chips describe the same moment.
  const changed = () => {
    roles.reload();
    bundles.reload();
  };

  const create = async () => {
    if (!name.trim()) return;
    try {
      await api.post("/api/approval-roles", { name, rank: Number(rank) || 1 });
      toast.success("Tier added — now give it something to grant from, and some members");
      setName("");
      setRank("1");
      setAdding(false);
      roles.reload();
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  const removeRole = async (r: ApprovalRole) => {
    const ok = await confirm({
      title: `Delete the ${r.name} tier?`,
      description:
        "Its members stop being approvers. Requests already routed to this tier can no longer be answered and will expire.",
      confirmLabel: "Delete",
      destructive: true,
    });
    if (!ok) return;
    try {
      await api.del(`/api/approval-roles/${r.id}`);
      roles.reload();
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  return (
    <div className="space-y-5">
      <PageHeader
        title="Approvers"
        description="The tiers that can approve an access request, and what each may grant. What a tier grants from — whole bundles, or single connections — is its reach; rank orders them, so a higher tier covers everything below it and a request no lower tier can grant escalates to one that can."
      />

      {roles.error && !roles.data && <ErrorBanner message={roles.error} onRetry={roles.reload} />}

      {roles.loading ? (
        <TableSkeleton rows={3} />
      ) : (roles.data ?? []).length === 0 && !adding ? (
        <EmptyState
          icon={ShieldCheck}
          title="No approval tiers yet"
          description="Add one — say 'Approver' at rank 1 and 'Super admin' at rank 9 — and give each a bundle or a few connections to grant from. Until then the bot cannot raise an access request at all."
          action={<Button onClick={() => setAdding(true)}>Add a tier</Button>}
        />
      ) : (
        <div className="space-y-4">
          {(roles.data ?? []).map((r) => (
            <RoleCard
              key={r.id}
              role={r}
              bundles={bundles.data ?? []}
              onChanged={changed}
              onDelete={() => removeRole(r)}
            />
          ))}
        </div>
      )}

      {adding ? (
        <Card className="space-y-3 p-4">
          <form
            onSubmit={(e) => {
              e.preventDefault();
              create();
            }}
            className="grid gap-3 sm:grid-cols-[1fr_8rem_auto] sm:items-end"
          >
            <div className="space-y-1">
              <Label htmlFor="role-name">Name</Label>
              <Input
                id="role-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="Super admin"
              />
            </div>
            <div className="space-y-1">
              <Label htmlFor="role-rank">Rank</Label>
              <Input
                id="role-rank"
                inputMode="numeric"
                value={rank}
                onChange={(e) => setRank(e.target.value)}
              />
            </div>
            <div className="flex gap-2">
              <Button type="submit" disabled={!name.trim()}>
                Add
              </Button>
              <Button variant="ghost" onClick={() => setAdding(false)}>
                Cancel
              </Button>
            </div>
          </form>
          <p className="text-xs text-muted-foreground">
            Rank is just an order: higher can approve anything a lower tier can.
          </p>
        </Card>
      ) : (
        (roles.data ?? []).length > 0 && (
          <Button variant="outline" onClick={() => setAdding(true)}>
            <Plus /> Add a tier
          </Button>
        )
      )}
      {confirmDialog}
    </div>
  );
}

function RoleCard({
  role,
  bundles,
  onChanged,
  onDelete,
}: {
  role: ApprovalRole;
  bundles: Bundle[];
  onChanged: () => void;
  onDelete: () => void;
}) {
  const [ref, setRef] = useState("");
  const [busy, setBusy] = useState(false);

  const bundleIDs = role.bundle_ids ?? [];
  const connectionIDs = role.connection_ids ?? [];
  const nothing = bundleIDs.length === 0 && connectionIDs.length === 0;

  // A one-off connection is shown as bundle/name, so find it across every bundle.
  const connectionOf = (id: number): { bundle: Bundle; connection: Connection } | null => {
    for (const bundle of bundles) {
      const connection = (bundle.connections ?? []).find((c) => c.id === id);
      if (connection) return { bundle, connection };
    }
    return null;
  };

  // Add or remove one thing the tier grants from, then refresh the list so the count in the
  // header says what routing now sees.
  const change = async (call: () => Promise<unknown>, done: string) => {
    setBusy(true);
    try {
      await call();
      toast.success(done);
      onChanged();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const addMember = async () => {
    if (!ref.trim()) return;
    try {
      await api.post(`/api/approval-roles/${role.id}/members`, { ref });
      setRef("");
      onChanged();
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  const removeMember = async (m: string) => {
    try {
      await api.del(`/api/approval-roles/${role.id}/members?ref=${encodeURIComponent(m)}`);
      onChanged();
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  const members = role.members ?? [];

  return (
    <Card className="space-y-4 p-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <h3 className="text-sm font-semibold">{role.name}</h3>
          <StatusChip variant="neutral">rank {role.rank}</StatusChip>
          {role.grantable === 0 ? (
            <StatusChip variant="warning">grants nothing yet</StatusChip>
          ) : (
            <StatusChip variant="success">
              {role.grantable} connection{role.grantable === 1 ? "" : "s"}
            </StatusChip>
          )}
        </div>
        <Button variant="ghost" size="icon-sm" aria-label="Delete tier" onClick={onDelete}>
          <Trash2 />
        </Button>
      </div>

      <div className="grid gap-4 sm:grid-cols-2">
        <div className="space-y-1">
          <div className="flex items-center justify-between gap-2">
            <Label>Grants from</Label>
            <AddAccessPopover
              bundles={bundles}
              attachedBundles={bundleIDs}
              attachedConnections={connectionIDs}
              onAttachBundle={(id) =>
                change(() => api.post(`/api/approval-roles/${role.id}/bundles/${id}`), "Bundle added")
              }
              onAttachConnection={(id) =>
                change(
                  () => api.post(`/api/approval-roles/${role.id}/connections/${id}`),
                  "Connection added",
                )
              }
              busy={busy}
            />
          </div>
          {nothing ? (
            <p className="pt-1 text-sm text-muted-foreground">
              Nothing yet, so this tier can grant nothing. Add a bundle, or a single connection.
            </p>
          ) : (
            <div className="flex flex-wrap gap-1.5 pt-1">
              {bundleIDs.map((id) => {
                const bundle = bundles.find((b) => b.id === id);
                if (!bundle) return null;
                return (
                  <AttachedChip
                    key={`bundle-${id}`}
                    icon={Package}
                    label={bundle.name}
                    disabled={busy}
                    onDetach={() =>
                      change(
                        () => api.del(`/api/approval-roles/${role.id}/bundles/${id}`),
                        "Bundle removed",
                      )
                    }
                  />
                );
              })}
              {connectionIDs.map((id) => {
                const hit = connectionOf(id);
                if (!hit) return null;
                return (
                  <AttachedChip
                    key={`connection-${id}`}
                    icon={KeyRound}
                    prefix={`${hit.bundle.name}/`}
                    label={hit.connection.name}
                    disabled={busy}
                    onDetach={() =>
                      change(
                        () => api.del(`/api/approval-roles/${role.id}/connections/${id}`),
                        "Connection removed",
                      )
                    }
                  />
                );
              })}
            </div>
          )}
          <p className="text-xs text-muted-foreground">
            A bundle brings every connection in it; a connection added on its own (bundle/name)
            brings just that one. Being listed here is what lets this tier grant it, and an
            approved request runs under what is listed here — not the channel&apos;s own
            connections.
          </p>
        </div>

        <div className="space-y-1">
          <Label htmlFor={`m-${role.id}`}>Members</Label>
          <form
            onSubmit={(e) => {
              e.preventDefault();
              addMember();
            }}
            className="flex gap-2"
          >
            <Input
              id={`m-${role.id}`}
              className="font-mono"
              value={ref}
              onChange={(e) => setRef(e.target.value)}
              placeholder="U0123ABCDEF or someone@example.com"
            />
            <Button type="submit" variant="outline" disabled={!ref.trim()}>
              Add
            </Button>
          </form>
          <div className="flex flex-wrap gap-1.5 pt-1">
            {members.length === 0 ? (
              <p className="text-xs text-muted-foreground">
                Nobody holds this tier yet, so nothing routed to it can be answered.
              </p>
            ) : (
              members.map((m) => (
                <span
                  key={m.ref}
                  className="inline-flex items-center gap-1 rounded-md bg-secondary px-2 py-0.5 text-xs"
                >
                  {m.name || m.ref}
                  <button
                    type="button"
                    aria-label={`Remove ${m.name || m.ref}`}
                    onClick={() => removeMember(m.ref)}
                    className="text-muted-foreground hover:text-foreground"
                  >
                    <X className="size-3" />
                  </button>
                </span>
              ))
            )}
          </div>
        </div>
      </div>
    </Card>
  );
}
