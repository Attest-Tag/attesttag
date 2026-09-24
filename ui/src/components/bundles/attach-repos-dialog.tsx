"use client";

import { useState } from "react";
import { Building2, Check, Hash, Loader2, Lock, MessagesSquare } from "lucide-react";
import { toast } from "sonner";
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
import { api, errorMessage, useApi, type Connection, type Scope } from "@/lib/api";
import { cn } from "@/lib/utils";

const rank = (s: Scope) => (s.kind === "workspace" ? 0 : s.kind === "team" ? 1 : 2);

// Give a channel several saved repositories at once. The same endpoint the repository picker
// posts one connection to takes a list, so this is one request however many are ticked, and the
// server attaches each on its own — the bundle is not attached and nothing else in it follows.
export function AttachReposDialog({
  connections,
  onOpenChange,
  onDone,
}: {
  /** The ticked repositories; null when the dialog is closed. */
  connections: Connection[] | null;
  onOpenChange: (open: boolean) => void;
  onDone: () => void;
}) {
  const open = connections !== null;
  const scopes = useApi<Scope[]>(open ? "/api/scopes" : null);
  const [target, setTarget] = useState<number | null>(null);
  const [busy, setBusy] = useState(false);
  const n = connections?.length ?? 0;

  const list = [...(scopes.data ?? [])].sort(
    (a, b) => rank(a) - rank(b) || a.team_name.localeCompare(b.team_name) || a.name.localeCompare(b.name),
  );
  const picked = list.find((s) => s.id === target);

  const attach = async () => {
    if (!connections || !picked || busy) return;
    setBusy(true);
    try {
      const res = await api.post<{ repos: string[]; failed: { repo: string; error: string }[] }>(
        `/api/scopes/${picked.id}/repos`,
        { connection_ids: connections.map((c) => c.id) },
      );
      toast.success(
        res.repos.length === 1
          ? `${res.repos[0]} added to ${picked.name}`
          : `${res.repos.length} repositories added to ${picked.name}`,
      );
      for (const f of res.failed ?? []) toast.error(`${f.repo}: ${f.error}`);
      onDone();
      onOpenChange(false);
      setTarget(null);
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            attach();
          }}
        >
          <DialogHeader>
            <DialogTitle>
              Add {n} repositor{n === 1 ? "y" : "ies"} to a channel
            </DialogTitle>
            <DialogDescription>
              Each is attached on its own, not through the bundle, so nothing else under
              Repositories comes with it. A workspace hands them to every channel below it.
            </DialogDescription>
          </DialogHeader>
          <Command className="rounded-lg border">
            <CommandInput placeholder="Find a channel or workspace…" />
            <CommandList className="max-h-64">
              {scopes.loading && !scopes.data ? (
                <p className="px-3 py-6 text-center text-sm text-muted-foreground">Loading…</p>
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
                  return (
                    <CommandItem
                      key={s.id}
                      value={`${s.team_name} ${s.name} ${s.kind}`}
                      onSelect={() => setTarget(s.id)}
                    >
                      <Icon className="size-4 text-muted-foreground" />
                      <span className="min-w-0 flex-1 truncate">
                        {s.kind === "channel" && s.team_name && (
                          <span className="text-muted-foreground">{s.team_name} / </span>
                        )}
                        {s.name}
                      </span>
                      <Check className={cn("size-4", target === s.id ? "opacity-100" : "opacity-0")} />
                    </CommandItem>
                  );
                })}
              </CommandGroup>
            </CommandList>
          </Command>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={!picked || busy}>
              {busy && <Loader2 className="animate-spin" />}
              {picked ? `Add to ${picked.name}` : "Add"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
