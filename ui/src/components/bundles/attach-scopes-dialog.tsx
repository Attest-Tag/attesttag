"use client";

import { useState } from "react";
import { Building2, Check, Hash, Lock, MessagesSquare } from "lucide-react";
import { toast } from "sonner";
import { useConfirm } from "@/components/core/confirm-dialog";
import { Button } from "@/components/ui/button";
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { api, errorMessage, useApi, type Bundle, type Scope } from "@/lib/api";
import { cn } from "@/lib/utils";

const rank = (s: Scope) => (s.kind === "workspace" ? 0 : s.kind === "team" ? 1 : 2);

// Where one bundle is attached, asked from the bundle's side.
//
// It posts to the same two endpoints the channel's Add popover does, with the list turned round:
// every scope at once, ticked where this bundle is already granted. A bundle is made for a job
// and then handed to the two or three channels doing that job, which from the Access page is a
// visit to each of them; the card that says "Used in 2 places" is where somebody is standing
// when they want a third.
//
// Attaching is scopes.manage, which is not the permission that opens the bundles page — the card
// only offers this when the session holds it, and the server refuses it either way.
export function AttachScopesDialog({
  bundle,
  onOpenChange,
  onChanged,
}: {
  /** The bundle being placed; null when the dialog is closed. */
  bundle: Bundle | null;
  onOpenChange: (open: boolean) => void;
  /** Refetch the bundles, so the card's usage line and the ticks below agree. */
  onChanged: () => void;
}) {
  const open = bundle !== null;
  // The scopes carry what is attached to each of them, so the ticks come from the same fetch as
  // the names — no second list to keep in step, and no Slack sync for a picker.
  const scopes = useApi<Scope[]>(open ? "/api/scopes?sync=0" : null);
  const { confirm, confirmDialog } = useConfirm();
  // In flight, by scope. A second press on the same row is ignored; every other row stays live,
  // because one request in the air is no reason for the whole list to go grey and come back.
  const [busy, setBusy] = useState<ReadonlySet<number>>(new Set());

  const list = [...(scopes.data ?? [])].sort(
    (a, b) => rank(a) - rank(b) || a.team_name.localeCompare(b.team_name) || a.name.localeCompare(b.name),
  );
  const attached = (s: Scope) => (s.bundle_ids ?? []).includes(bundle?.id ?? 0);
  const count = list.filter(attached).length;

  // The tick this row shows, changed here rather than waited for. Called again with the old
  // value when the request fails, which is the only thing that can put it back.
  const setTick = (scopeID: number, on: boolean, bundleID: number) =>
    scopes.mutate((current) =>
      (current ?? []).map((s) =>
        s.id === scopeID
          ? {
              ...s,
              bundle_ids: on
                ? [...(s.bundle_ids ?? []), bundleID]
                : (s.bundle_ids ?? []).filter((id) => id !== bundleID),
            }
          : s,
      ),
    );

  const toggle = async (scope: Scope) => {
    if (!bundle || busy.has(scope.id)) return;
    const on = attached(scope);
    if (on) {
      const ok = await confirm({
        title: `Detach ${bundle.name} from ${scope.name}?`,
        description:
          scope.kind === "channel"
            ? "The bot stops using this bundle's connections and domains in this channel. The bundle itself is kept."
            : `${scope.kind === "workspace" ? "Every connected workspace" : "Every channel here"} loses this bundle's connections and domains. The bundle itself is kept.`,
        confirmLabel: "Detach",
        destructive: true,
      });
      if (!ok) return;
    }
    setBusy((current) => new Set(current).add(scope.id));
    // The tick moves on the press, not on the reply: a row that sits unchanged for the length of
    // a round trip — or worse, greys out and comes back — reads as a press that did not land.
    setTick(scope.id, !on, bundle.id);
    const path = `/api/scopes/${scope.id}/bundles/${bundle.id}`;
    try {
      if (on) await api.del(path);
      else await api.post(path);
      toast.success(
        on ? `${bundle.name} detached from ${scope.name}` : `${bundle.name} attached to ${scope.name}`,
      );
      onChanged();
    } catch (err) {
      setTick(scope.id, on, bundle.id);
      toast.error(errorMessage(err));
    } finally {
      setBusy((current) => {
        const next = new Set(current);
        next.delete(scope.id);
        return next;
      });
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Where {bundle?.name} is attached</DialogTitle>
          <DialogDescription>
            Wherever it is ticked, the bot may use its connections, domains and instructions. A
            workspace hands it to every channel below it, and the account level to every connected
            workspace.
          </DialogDescription>
        </DialogHeader>
        <Command className="rounded-lg border">
          <CommandInput placeholder="Find a channel or workspace…" />
          <CommandList className="max-h-64">
            {scopes.loading && !scopes.data ? (
              <p className="px-3 py-6 text-center text-sm text-muted-foreground">Loading…</p>
            ) : list.length === 0 ? (
              <p className="px-3 py-6 text-center text-sm text-muted-foreground">
                No channels yet. Add a workspace under Workspaces first.
              </p>
            ) : (
              <CommandEmpty>Nothing matches.</CommandEmpty>
            )}
            <CommandGroup>
              {list.map((s) => {
                const Icon =
                  s.kind === "workspace"
                    ? Building2
                    : s.kind === "team"
                      ? MessagesSquare
                      : s.is_private
                        ? Lock
                        : Hash;
                const on = attached(s);
                return (
                  <CommandItem
                    key={s.id}
                    value={`${s.team_name} ${s.name} ${s.kind}`}
                    onSelect={() => toggle(s)}
                  >
                    <Icon className="size-4 text-muted-foreground" />
                    <span className="min-w-0 flex-1 truncate">
                      {s.kind === "channel" && s.team_name && (
                        <span className="text-muted-foreground">{s.team_name} / </span>
                      )}
                      {s.name}
                    </span>
                    <Check className={cn("size-4", on ? "opacity-100" : "opacity-0")} />
                  </CommandItem>
                );
              })}
            </CommandGroup>
          </CommandList>
        </Command>
        <DialogFooter>
          <span className="mr-auto self-center text-xs text-muted-foreground">
            {count === 0 ? "Attached nowhere" : `Attached in ${count} place${count === 1 ? "" : "s"}`}
          </span>
          <Button type="button" onClick={() => onOpenChange(false)}>
            Done
          </Button>
        </DialogFooter>
        {confirmDialog}
      </DialogContent>
    </Dialog>
  );
}
