"use client";

import { useRef, useState } from "react";
import { Loader2 } from "lucide-react";
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

// One text field and a verb — creating a bundle and renaming one are the same
// dialog with different words.
export function NameDialog({
  open,
  onOpenChange,
  title,
  description,
  label,
  initial = "",
  suggestion,
  submitLabel,
  onSubmit,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description?: string;
  label: string;
  /** The thing's CURRENT name, when renaming. Submitting it unchanged is a no-op, so it is
   *  refused. */
  initial?: string;
  /** A name offered for something that does not exist yet. The opposite of `initial`:
   *  accepting it untouched is the point, so it must not disable the button. */
  suggestion?: string;
  submitLabel: string;
  onSubmit: (name: string) => Promise<void>;
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-sm">
        {open && (
          <NameForm
            key={initial || suggestion || ""}
            title={title}
            description={description}
            label={label}
            initial={initial}
            suggestion={suggestion}
            submitLabel={submitLabel}
            onSubmit={onSubmit}
            onOpenChange={onOpenChange}
          />
        )}
      </DialogContent>
    </Dialog>
  );
}

function NameForm({
  title,
  description,
  label,
  initial,
  suggestion,
  submitLabel,
  onSubmit,
  onOpenChange,
}: {
  title: string;
  description?: string;
  label: string;
  initial: string;
  suggestion?: string;
  submitLabel: string;
  onSubmit: (name: string) => Promise<void>;
  onOpenChange: (open: boolean) => void;
}) {
  const [name, setName] = useState(initial || suggestion || "");
  const [busy, setBusy] = useState(false);
  const input = useRef<HTMLInputElement>(null);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    try {
      await onSubmit(name.trim());
      onOpenChange(false);
    } finally {
      setBusy(false);
    }
  };

  return (
    <form onSubmit={submit} className="space-y-4">
      <DialogHeader>
        <DialogTitle>{title}</DialogTitle>
        {description && <DialogDescription>{description}</DialogDescription>}
      </DialogHeader>
      <div className="space-y-1">
        <Label htmlFor="name-dialog-input">{label}</Label>
        <Input
          id="name-dialog-input"
          ref={input}
          value={name}
          onChange={(e) => setName(e.target.value)}
          autoFocus
          // A suggested name is meant to be accepted or typed over, so it starts selected.
          onFocus={(e) => suggestion && e.target.select()}
          required
        />
      </div>
      <DialogFooter>
        <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
          Cancel
        </Button>
        <Button type="submit" disabled={busy || !name.trim() || (!!initial && name.trim() === initial)}>
          {busy && <Loader2 className="animate-spin" />}
          {submitLabel}
        </Button>
      </DialogFooter>
    </form>
  );
}
