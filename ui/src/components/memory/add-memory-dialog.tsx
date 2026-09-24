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
import { Textarea } from "@/components/ui/textarea";
import { api, errorMessage, type Scope } from "@/lib/api";

/**
 * The scope string the server writes, which is not the scope's own kind and id.
 * tools_memory.go writes "team:T…" for a whole workspace and "channel:T…/C…" for one channel —
 * the workspace is in the channel key because Slack only promises channel ids are unique inside
 * one. Getting this wrong is invisible until you try: teamOfMemoryScope cannot parse a key
 * without a workspace, so POST /api/memories answers 404 and the dialog looks broken for no
 * stated reason. Returns "" for the account-level scope, which holds no memories at all.
 */
function memoryScopeKey(scope: Scope): string {
  if (scope.kind === "team") return `team:${scope.team_id || scope.slack_id}`;
  if (scope.kind === "channel") return `channel:${scope.team_id}/${scope.slack_id}`;
  return "";
}

export function AddMemoryDialog({
  open,
  onOpenChange,
  scopes,
  onAdded,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  scopes: Scope[];
  onAdded: () => void;
}) {
  const [scope, setScope] = useState<string>("");
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);

  // Only the scopes a memory can actually be written to. The account-level scope is in this list
  // for every other page, but the store has no organisation-wide memory scope, so offering it
  // here — as the default, as it happens — was offering the one choice guaranteed to fail.
  const writable = scopes.filter((s) => memoryScopeKey(s) !== "");
  const effectiveScope = scope || (writable.length > 0 ? memoryScopeKey(writable[0]) : "");

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!effectiveScope) {
      toast.error("Pick a scope first");
      return;
    }
    setBusy(true);
    try {
      await api.post("/api/memories", { scope: effectiveScope, text });
      toast.success("Memory added");
      setText("");
      onOpenChange(false);
      onAdded();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <form onSubmit={submit} className="space-y-4">
          <DialogHeader>
            <DialogTitle>Add memory</DialogTitle>
            <DialogDescription>
              One fact, in a sentence. The bot reads it on every turn in that scope.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-1">
            <Label htmlFor="memory-scope">Scope</Label>
            <Select value={effectiveScope} onValueChange={setScope}>
              <SelectTrigger id="memory-scope" className="w-full">
                <SelectValue placeholder="Choose where this applies" />
              </SelectTrigger>
              <SelectContent>
                {writable.map((s) => (
                  <SelectItem key={s.id} value={memoryScopeKey(s)}>
                    {s.name}
                    <span className="ml-1 text-muted-foreground">
                      {s.kind === "team" ? "· whole workspace" : ""}
                    </span>
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-1">
            <Label htmlFor="memory-text">Memory</Label>
            <Textarea
              id="memory-text"
              rows={4}
              value={text}
              onChange={(e) => setText(e.target.value)}
              placeholder="The on-call rota lives in #ops-oncall; escalate to Priya after 18:00."
              required
              autoFocus
            />
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={busy || !text.trim()}>
              {busy && <Loader2 className="animate-spin" />}
              Add
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
