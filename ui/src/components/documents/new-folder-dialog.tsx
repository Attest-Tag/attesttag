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
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api, errorMessage } from "@/lib/api";

// A new folder inside the one that is open. Empty folders are remembered by the server, so
// this can be done before there is anything to put in them.
export function NewFolderDialog({
  open,
  parent,
  onOpenChange,
  onCreated,
}: {
  open: boolean;
  /** The folder it goes in. Empty is the top level. */
  parent: string;
  onOpenChange: (open: boolean) => void;
  onCreated: (path: string) => void;
}) {
  const [name, setName] = useState("");
  const [saving, setSaving] = useState(false);

  const create = async (e: React.FormEvent) => {
    e.preventDefault();
    const clean = name.trim().replace(/^\/+|\/+$/g, "");
    if (!clean) return;
    const path = parent ? `${parent}/${clean}` : clean;
    setSaving(true);
    try {
      await api.post("/api/document-folders", { path });
      toast.success(`Created ${path}`);
      setName("");
      onCreated(path);
      onOpenChange(false);
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) setName("");
        onOpenChange(next);
      }}
    >
      <DialogContent className="sm:max-w-md">
        <form onSubmit={create} className="grid gap-4">
          <DialogHeader>
            <DialogTitle>New folder</DialogTitle>
            <DialogDescription>
              {parent ? `Inside ${parent}.` : "At the top level of Documents."} Folders are for
              your own ordering — the bot searches every document either way.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-2">
            <Label htmlFor="folder-name">Name</Label>
            <Input
              id="folder-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="policies"
              autoFocus
              autoComplete="off"
              spellCheck={false}
            />
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)} disabled={saving}>
              Cancel
            </Button>
            <Button type="submit" disabled={saving || name.trim() === ""}>
              {saving && <Loader2 className="size-4 animate-spin" />}
              Create folder
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
