"use client";

import { useState } from "react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { api, errorMessage, type Bundle, type Connection } from "@/lib/api";
import { cn } from "@/lib/utils";

// Copy a connected app (with its stored credential) into another bundle, so the same
// service can be part of differently shaped bundles. Rotating the secret later must be
// done on each copy.
export function CopyToBundleDialog({
  connection,
  bundles,
  onOpenChange,
  onDone,
}: {
  connection: Connection | null;
  bundles: Bundle[];
  onOpenChange: (open: boolean) => void;
  onDone: () => void;
}) {
  const [target, setTarget] = useState<number | null>(null);
  const [busy, setBusy] = useState(false);
  const others = bundles.filter((b) => b.id !== connection?.bundle_id);

  const copy = async () => {
    if (!connection || !target) return;
    setBusy(true);
    try {
      await api.post(`/api/connections/${connection.id}/copy`, { bundle_id: target });
      toast.success(`Copied ${connection.name}`);
      onDone();
      onOpenChange(false);
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={connection !== null} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Copy {connection?.name} to another bundle</DialogTitle>
          <DialogDescription>
            The credential is copied too. Each copy is rotated on its own; to share one
            credential across channels, attach this bundle, or just this connection, to more
            scopes under Workspaces instead.
          </DialogDescription>
        </DialogHeader>
        {others.length === 0 ? (
          <p className="text-sm text-muted-foreground">There is no other bundle yet. Create one first.</p>
        ) : (
          <ul className="overflow-hidden rounded-lg border">
            {others.map((b) => (
              <li key={b.id}>
                <button
                  type="button"
                  onClick={() => setTarget(b.id)}
                  className={cn(
                    "flex h-10 w-full items-center justify-between px-3 text-left text-sm hover:bg-secondary/60",
                    target === b.id && "bg-accent",
                  )}
                >
                  <span className="font-medium">{b.name}</span>
                  <span className="text-xs text-muted-foreground">
                    {(b.connections ?? []).length} credential{(b.connections ?? []).length === 1 ? "" : "s"}
                  </span>
                </button>
              </li>
            ))}
          </ul>
        )}
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button onClick={copy} disabled={!target || busy}>
            Copy
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
