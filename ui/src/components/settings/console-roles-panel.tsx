"use client";

import { useMemo, useState } from "react";
import { Copy, Pencil, Plus, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { useConfirm } from "@/components/core/confirm-dialog";
import { ErrorBanner } from "@/components/core/error-banner";
import { StatusChip } from "@/components/core/status-chip";
import { useAuth } from "@/components/shell/auth-provider";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import {
  api,
  errorMessage,
  useApi,
  type ConsoleRole,
  type ConsoleRolesResponse,
  type ConsoleUser,
} from "@/lib/api";
import {
  BUILTIN_ROLE_BLURB,
  groupPermissions,
  orderPermissions,
  permissionHint,
  permissionLabel,
  roleKey,
} from "@/components/settings/console-permissions";

/**
 * Roles are their own tab: what a role may do is a different question from who holds one.
 *
 * The list reads as an answer to "what does each of these mean" — a blurb, who holds it, and the
 * permissions spelled out in words. Choosing permissions is a separate act, so it happens in a
 * dialog rather than as a permanently open sixteen-checkbox form at the bottom of the page.
 */
export function ConsoleRolesPanel() {
  const roles = useApi<ConsoleRolesResponse>("/api/console/roles");
  // Only to say how many people hold each role — the endpoint needs no extra permission.
  const users = useApi<ConsoleUser[]>("/api/console/users");
  const { me } = useAuth();
  const { confirm, confirmDialog } = useConfirm();
  const [editing, setEditing] = useState<{ role: ConsoleRole | null; from?: ConsoleRole } | null>(
    null,
  );

  const all = roles.data?.all_permissions ?? [];
  const list = roles.data?.roles ?? [];
  const builtin = list.filter((r) => r.builtin);
  const custom = list.filter((r) => !r.builtin);

  const holders = useMemo(() => {
    const n = new Map<string, number>();
    for (const u of users.data ?? []) n.set(u.role, (n.get(u.role) ?? 0) + 1);
    return n;
  }, [users.data]);

  // The server refuses a role holding more than you do, so the editor greys those out rather than
  // letting you tick them and fail on save. A session with no map yet is allowed everything — the
  // server is still the one enforcing it.
  const mine = me?.user?.permissions;
  const holds = (p: string) => !mine || mine[p] === true;
  const canManage = holds("roles.manage");

  const remove = async (r: ConsoleRole) => {
    const n = holders.get(r.key) ?? 0;
    const ok = await confirm({
      title: `Delete the ${r.label} role?`,
      description: n
        ? `${n} ${n === 1 ? "person holds" : "people hold"} it. They keep their sign-in but lose every permission until you give them another role.`
        : "Nobody holds it, so nothing changes for anyone.",
      confirmLabel: "Delete",
      destructive: true,
    });
    if (!ok) return;
    try {
      await api.del(`/api/console/roles/${r.key}`);
      toast.success("Role deleted");
      roles.reload();
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  return (
    <div className="space-y-5">
      {roles.error && !roles.data && <ErrorBanner message={roles.error} onRetry={roles.reload} />}

      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="max-w-2xl">
          <h3 className="text-sm font-semibold">Roles</h3>
          <p className="mt-0.5 text-xs leading-relaxed text-muted-foreground">
            A role is a set of permissions, and everyone who can open the console holds exactly one.
            The three built-in roles are defined in the code, so a permission added in a later
            release reaches them on its own. Roles you define here are yours — and you can only give
            one permissions you hold yourself.
          </p>
        </div>
        {canManage && (
          <Button size="sm" onClick={() => setEditing({ role: null })}>
            <Plus /> New role
          </Button>
        )}
      </div>

      {roles.loading && !roles.data ? (
        <RolesSkeleton />
      ) : (
        <>
          <Section title="Built-in">
            {builtin.map((r) => (
              <RoleRow
                key={r.key}
                role={r}
                holders={holders.get(r.key) ?? 0}
                total={all.length}
                onDuplicate={canManage ? () => setEditing({ role: null, from: r }) : undefined}
              />
            ))}
          </Section>

          <Section
            title="Custom"
            empty={
              custom.length === 0
                ? canManage
                  ? "None yet. Add one when the built-in three don't fit — say a role that looks after documents and nothing else."
                  : "None yet."
                : undefined
            }
          >
            {custom.map((r) => (
              <RoleRow
                key={r.key}
                role={r}
                holders={holders.get(r.key) ?? 0}
                total={all.length}
                onEdit={canManage ? () => setEditing({ role: r }) : undefined}
                onDelete={canManage ? () => remove(r) : undefined}
                onDuplicate={canManage ? () => setEditing({ role: null, from: r }) : undefined}
              />
            ))}
          </Section>
        </>
      )}

      {editing && (
        <RoleEditor
          open
          onOpenChange={(o) => !o && setEditing(null)}
          all={all}
          role={editing.role}
          from={editing.from}
          holds={holds}
          onSaved={() => {
            setEditing(null);
            roles.reload();
          }}
        />
      )}
      {confirmDialog}
    </div>
  );
}

function Section({
  title,
  empty,
  children,
}: {
  title: string;
  empty?: string;
  children: React.ReactNode;
}) {
  return (
    <section className="space-y-2">
      <h4 className="px-1 text-xs font-medium tracking-wide text-muted-foreground uppercase">
        {title}
      </h4>
      {empty ? (
        <Card className="border-dashed bg-muted/30 px-4 py-6 text-center shadow-none">
          <p className="text-xs text-muted-foreground">{empty}</p>
        </Card>
      ) : (
        <Card className="divide-y gap-0 py-0">{children}</Card>
      )}
    </section>
  );
}

function RoleRow({
  role,
  holders,
  total,
  onEdit,
  onDelete,
  onDuplicate,
}: {
  role: ConsoleRole;
  holders: number;
  /** How many permissions exist at all, so "holds them all" can be said as such. */
  total: number;
  onEdit?: () => void;
  onDelete?: () => void;
  onDuplicate?: () => void;
}) {
  const perms = orderPermissions(role.permissions ?? []);
  const everything = total > 0 && perms.length >= total;

  return (
    <div className="relative grid gap-3 p-4 sm:grid-cols-[14rem_1fr_auto] sm:gap-5">
      <div className="min-w-0 space-y-1">
        <div className="flex items-center gap-2">
          <span className="text-sm font-medium">{role.label}</span>
          {role.builtin && <StatusChip variant="neutral">built-in</StatusChip>}
        </div>
        <code className="block truncate font-mono text-[11px] text-muted-foreground">
          {role.key}
        </code>
        <p className="text-xs leading-relaxed text-muted-foreground">
          {BUILTIN_ROLE_BLURB[role.key] ??
            `${perms.length} permission${perms.length === 1 ? "" : "s"}.`}{" "}
          {holders === 0 ? "Nobody holds it." : `${holders} ${holders === 1 ? "person" : "people"}.`}
        </p>
      </div>

      <div className="min-w-0">
        {everything ? (
          <StatusChip variant="success">Full access — every permission</StatusChip>
        ) : perms.length === 0 ? (
          <p className="text-xs text-muted-foreground">
            No permissions. Someone holding this signs in and sees nothing.
          </p>
        ) : (
          <div className="flex flex-wrap gap-1">
            {perms.map((p) => (
              <span
                key={p}
                title={`${p}${permissionHint(p) ? ` — ${permissionHint(p)}` : ""}`}
                className="rounded-md bg-secondary px-2 py-0.5 text-[11px] text-secondary-foreground"
              >
                {permissionLabel(p)}
              </span>
            ))}
          </div>
        )}
      </div>

      {/* Stacked, the third column would leave the buttons stranded under the chips, so on a
          narrow screen they sit in the row's own top-right corner instead. */}
      <div className="absolute top-3 right-3 flex items-start gap-1 sm:static sm:justify-end">
        {onDuplicate && (
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label={`Duplicate ${role.label}`}
            title="Start a new role from this one"
            onClick={onDuplicate}
          >
            <Copy />
          </Button>
        )}
        {onEdit && (
          <Button variant="ghost" size="icon-sm" aria-label={`Edit ${role.label}`} onClick={onEdit}>
            <Pencil />
          </Button>
        )}
        {onDelete && (
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label={`Delete ${role.label}`}
            onClick={onDelete}
          >
            <Trash2 />
          </Button>
        )}
      </div>
    </div>
  );
}

function RolesSkeleton() {
  return (
    <Card className="divide-y gap-0 py-0">
      {Array.from({ length: 3 }).map((_, i) => (
        <div key={i} className="grid gap-3 p-4 sm:grid-cols-[14rem_1fr] sm:gap-5">
          <div className="space-y-2">
            <Skeleton className="h-3.5 w-24" />
            <Skeleton className="h-3 w-16" />
          </div>
          <div className="flex flex-wrap gap-1">
            {Array.from({ length: 5 }).map((_, j) => (
              <Skeleton key={j} className="h-5 w-20 rounded-md" />
            ))}
          </div>
        </div>
      ))}
    </Card>
  );
}

/** Create or edit a custom role. Editing re-posts the same key, which the API upserts. */
function RoleEditor({
  open,
  onOpenChange,
  all,
  role,
  from,
  holds,
  onSaved,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  all: string[];
  /** The role being edited, or null when creating. */
  role: ConsoleRole | null;
  /** A role to copy permissions from when creating. */
  from?: ConsoleRole;
  holds: (permission: string) => boolean;
  onSaved: () => void;
}) {
  const [name, setName] = useState(role?.label ?? "");
  const [picked, setPicked] = useState<string[]>(() =>
    (role?.permissions ?? from?.permissions ?? []).filter(holds),
  );
  const [busy, setBusy] = useState(false);
  const groups = useMemo(() => groupPermissions(all), [all]);

  const key = role?.key ?? roleKey(name);
  const has = (p: string) => picked.includes(p);
  const toggle = (p: string, on: boolean) =>
    setPicked((cur) => (on ? [...new Set([...cur, p])] : cur.filter((x) => x !== p)));

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!key || picked.length === 0) return;
    setBusy(true);
    try {
      await api.post("/api/console/roles", { key, label: name.trim() || key, permissions: picked });
      toast.success(role ? "Role updated" : "Role created");
      onSaved();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-2xl">
        <form onSubmit={submit} className="space-y-4">
          <DialogHeader>
            <DialogTitle>{role ? `Edit ${role.label}` : "New role"}</DialogTitle>
            <DialogDescription>
              {from
                ? `Starting from ${from.label}. Give it a name and adjust what it may do.`
                : "Name it after the job it does, then tick what it may do. Permissions you don't hold yourself are greyed out."}
            </DialogDescription>
          </DialogHeader>

          <div className="grid gap-1.5">
            <Label htmlFor="role-name">Name</Label>
            <Input
              id="role-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="Operations"
              autoFocus={!role}
            />
            <p className="text-xs text-muted-foreground">
              {role ? (
                <>
                  Stored as <code className="font-mono">{key}</code>. Renaming keeps the same key, so
                  nobody loses their role.
                </>
              ) : key ? (
                <>
                  Stored as <code className="font-mono">{key}</code>.
                </>
              ) : (
                "Letters and numbers; anything else becomes an underscore."
              )}
            </p>
          </div>

          <div className="max-h-[46vh] space-y-4 overflow-y-auto rounded-lg border p-3">
            {groups.map((g) => {
              const usable = g.items.filter((p) => holds(p.key)).map((p) => p.key);
              const allOn = usable.length > 0 && usable.every(has);
              return (
                <div key={g.title} className="space-y-2">
                  <div className="flex items-center justify-between gap-3">
                    <h4 className="text-xs font-medium tracking-wide text-muted-foreground uppercase">
                      {g.title}
                    </h4>
                    {usable.length > 0 && (
                      <Button
                        type="button"
                        variant="ghost"
                        size="xs"
                        onClick={() =>
                          setPicked((cur) =>
                            allOn
                              ? cur.filter((x) => !usable.includes(x))
                              : [...new Set([...cur, ...usable])],
                          )
                        }
                      >
                        {allOn ? "Clear" : "All"}
                      </Button>
                    )}
                  </div>
                  <div className="grid gap-1 sm:grid-cols-2">
                    {g.items.map((p) => {
                      const allowed = holds(p.key);
                      return (
                        <label
                          key={p.key}
                          title={allowed ? p.key : "You don't hold this permission yourself"}
                          className={
                            "flex items-start gap-2 rounded-md p-2 " +
                            (allowed ? "hover:bg-muted/60" : "opacity-50")
                          }
                        >
                          <Checkbox
                            className="mt-0.5"
                            disabled={!allowed}
                            checked={has(p.key)}
                            onCheckedChange={(v) => toggle(p.key, v === true)}
                          />
                          <span className="min-w-0">
                            <span className="block text-xs font-medium">{p.label}</span>
                            {p.hint && (
                              <span className="block text-[11px] leading-snug text-muted-foreground">
                                {p.hint}
                              </span>
                            )}
                          </span>
                        </label>
                      );
                    })}
                  </div>
                </div>
              );
            })}
          </div>

          <DialogFooter className="sm:items-center sm:justify-between">
            <p className="text-xs text-muted-foreground">
              {picked.length} of {all.length} selected
            </p>
            <div className="flex gap-2">
              <Button type="button" variant="ghost" onClick={() => onOpenChange(false)}>
                Cancel
              </Button>
              <Button type="submit" loading={busy} disabled={!key || picked.length === 0}>
                {role ? "Save changes" : "Create role"}
              </Button>
            </div>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
