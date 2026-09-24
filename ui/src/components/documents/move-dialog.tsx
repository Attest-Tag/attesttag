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
import { api, errorMessage } from "@/lib/api";

/** What is being moved: one document, or a folder and everything under it. */
export type MoveTarget = { kind: "document" | "folder"; path: string; name: string };

const ROOT = "__root__";

/** The folder a path currently sits in ("" for the top level). */
function parentOf(path: string): string {
  const i = path.lastIndexOf("/");
  return i < 0 ? "" : path.slice(0, i);
}

export function MoveDialog({
  target,
  folders,
  onOpenChange,
  onMoved,
}: {
  target: MoveTarget | null;
  folders: string[];
  onOpenChange: (open: boolean) => void;
  onMoved: () => void;
}) {
  return (
    <Dialog open={target !== null} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        {target && (
          // Keyed on the path so opening another row starts from that row's own folder.
          <MoveForm
            key={target.path}
            target={target}
            folders={folders}
            onCancel={() => onOpenChange(false)}
            onMoved={onMoved}
          />
        )}
      </DialogContent>
    </Dialog>
  );
}

function MoveForm({
  target,
  folders,
  onCancel,
  onMoved,
}: {
  target: MoveTarget;
  folders: string[];
  onCancel: () => void;
  onMoved: () => void;
}) {
  const here = parentOf(target.path);
  const [dest, setDest] = useState(here || ROOT);
  const [saving, setSaving] = useState(false);

  // A folder cannot be moved into itself or into anything it contains.
  const choices = folders.filter(
    (f) => !(target.kind === "folder" && (f === target.path || f.startsWith(target.path + "/"))),
  );

  const to = dest === ROOT ? target.name : `${dest}/${target.name}`;
  const unchanged = (dest === ROOT ? "" : dest) === here;

  const move = async (e: React.FormEvent) => {
    e.preventDefault();
    if (unchanged) return;
    setSaving(true);
    try {
      const url = target.kind === "folder" ? "/api/document-folders/move" : "/api/documents/move";
      await api.post(url, { from: target.path, to });
      toast.success(`Moved to ${dest === ROOT ? "Documents" : dest}`);
      onMoved();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setSaving(false);
    }
  };

  return (
    <form onSubmit={move} className="grid gap-4">
      <DialogHeader>
        <DialogTitle className="truncate">Move {target.name}</DialogTitle>
        <DialogDescription>
          {target.kind === "folder"
            ? "The folder and everything in it moves. Documents are re-indexed afterwards."
            : "The document keeps its scope and is re-indexed under its new path."}
        </DialogDescription>
      </DialogHeader>
      <div className="grid gap-2">
        <Label htmlFor="move-dest">Destination folder</Label>
        <Select value={dest} onValueChange={setDest}>
          <SelectTrigger id="move-dest" className="w-full">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={ROOT}>Documents (top level)</SelectItem>
            {choices.map((f) => (
              <SelectItem key={f} value={f}>
                {f}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <p className="truncate font-mono text-xs text-muted-foreground">{to}</p>
      </div>
      <DialogFooter>
        <Button type="button" variant="outline" onClick={onCancel} disabled={saving}>
          Cancel
        </Button>
        <Button type="submit" disabled={saving || unchanged}>
          {saving && <Loader2 className="size-4 animate-spin" />}
          Move
        </Button>
      </DialogFooter>
    </form>
  );
}
