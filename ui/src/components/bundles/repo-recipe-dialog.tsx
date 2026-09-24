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
import { api, errorMessage, type Connection } from "@/lib/api";

// How the fix worker builds and tests several repositories at once. Repositories that arrive
// together usually build the same way — the same monorepo split into services, the same
// language, the same one-line test command — and setting that twenty times through twenty row
// disclosures is the sort of thing nobody does, so the worker keeps guessing.
//
// Empty boxes mean "work it out from the clone", which is also how a repository is put back to
// detection. Each command is a program and its arguments, never a shell line; the API splits
// and refuses pipes rather than making this box a shell in the worker container.
export function RepoRecipeDialog({
  connections,
  onOpenChange,
  onDone,
}: {
  /** The ticked repositories; null when the dialog is closed. */
  connections: Connection[] | null;
  onOpenChange: (open: boolean) => void;
  onDone: () => void;
}) {
  const [value, setValue] = useState({ test: "", build: "", workdir: "" });
  const [busy, setBusy] = useState(false);
  const n = connections?.length ?? 0;

  const apply = async () => {
    if (!connections || busy) return;
    setBusy(true);
    const empty = !value.test.trim() && !value.build.trim() && !value.workdir.trim();
    const body = empty
      ? { clear_recipe: true, test_cmd: "" }
      : {
          test_cmd: "",
          recipe: {
            source: "connection",
            workdir: value.workdir.trim(),
            build: value.build.trim() ? { run: value.build.trim(), argv: [] } : null,
            test: value.test.trim() ? { run: value.test.trim(), argv: [] } : null,
          },
        };
    const failed: string[] = [];
    try {
      for (const c of connections) {
        try {
          await api.put(`/api/connections/${c.id}`, body);
        } catch (err) {
          failed.push(`${c.repo}: ${errorMessage(err)}`);
        }
      }
      const done = connections.length - failed.length;
      if (done > 0) {
        toast.success(
          empty
            ? `${done} back to working it out from the repository`
            : done === 1
              ? `Recipe saved for ${connections[0].repo}`
              : `Recipe saved for ${done} repositories`,
        );
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
              Set the recipe on {n} repositor{n === 1 ? "y" : "ies"}
            </DialogTitle>
            <DialogDescription>
              What the fix worker runs before and after its change. This replaces whatever each
              of them has now; leave every box empty to put them back to working it out from the
              clone.
            </DialogDescription>
          </DialogHeader>
          {(
            [
              ["test", "Test", "go test ./..."],
              ["build", "Build", "go build ./..."],
              ["workdir", "Directory", "the repository root"],
            ] as const
          ).map(([field, label, placeholder]) => (
            <div key={field} className="space-y-1">
              <Label htmlFor={`bulk-recipe-${field}`}>{label}</Label>
              <Input
                id={`bulk-recipe-${field}`}
                className="font-mono text-xs"
                placeholder={placeholder}
                value={value[field]}
                onChange={(e) => setValue((v) => ({ ...v, [field]: e.target.value }))}
              />
            </div>
          ))}
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
