"use client";

import { useState } from "react";
import { Loader2 } from "lucide-react";
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
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { api, errorMessage, type Connection } from "@/lib/api";

// Whether the bot waits for someone to press Confirm before it writes, set on several
// repositories at once. The same three settings the connection editor offers, since this only
// writes the field that editor writes — one PUT per repository, because that is the shape the
// API has; the failures are named rather than swallowed.
export function RepoWritesDialog({
  connections,
  onOpenChange,
  onDone,
}: {
  /** The ticked repositories; null when the dialog is closed. */
  connections: Connection[] | null;
  onOpenChange: (open: boolean) => void;
  onDone: () => void;
}) {
  const [writes, setWrites] = useState("confirm");
  const [busy, setBusy] = useState(false);
  const n = connections?.length ?? 0;

  const apply = async () => {
    if (!connections || busy) return;
    setBusy(true);
    const failed: string[] = [];
    try {
      for (const c of connections) {
        try {
          await api.put(`/api/connections/${c.id}`, { name: c.name, preset: c.preset, writes });
        } catch (err) {
          failed.push(`${c.repo}: ${errorMessage(err)}`);
        }
      }
      const done = connections.length - failed.length;
      if (done > 0) {
        toast.success(done === 1 ? `Writes set on ${connections[0].repo}` : `Writes set on ${done} repositories`);
      }
      for (const f of failed) toast.error(f);
      onDone();
      if (failed.length === 0) onOpenChange(false);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={connections !== null} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault();
            apply();
          }}
        >
          <DialogHeader>
            <DialogTitle>
              Set writes on {n} repositor{n === 1 ? "y" : "ies"}
            </DialogTitle>
            <DialogDescription>
              This is a property of the repository itself, so it applies in every channel that
              has it, not only the one you came from.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-1">
            <Label htmlFor="bulk-writes">Writes</Label>
            <Select value={writes} onValueChange={setWrites}>
              <SelectTrigger id="bulk-writes" className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="confirm">Confirm in Slack</SelectItem>
                <SelectItem value="auto">Automatic</SelectItem>
                <SelectItem value="all">Confirm everything, reads included</SelectItem>
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground">
              Commenting on an issue or opening a pull request waits for someone in the thread to
              press Confirm. Reads — searching issues, listing commits — go straight through.
              {writes === "auto" && " Automatic skips that wait for every write."}
              {writes === "all" && " Everything is held, including reads."}
            </p>
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={busy}>
              {busy && <Loader2 className="animate-spin" />}
              Apply
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
