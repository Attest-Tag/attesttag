"use client";

import { useRef, useState } from "react";
import { Plus } from "lucide-react";
import { toast } from "sonner";
import { NameDialog } from "@/components/bundles/name-dialog";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectSeparator,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { api, errorMessage, type Bundle } from "@/lib/api";

/** The sentinel the "New bundle…" row carries. Radix needs every item to have a value, and it
 *  has to be one no bundle id can ever be. */
const NEW = "__new_bundle__";

/** A name the Create dialog opens with, so making a bundle is one key rather than one key and
 *  a decision. It never collides with one that is already there — "Bundle", then "Bundle 2" —
 *  because a list of bundles with two called the same thing is unreadable later. */
export function defaultBundleName(existing: Bundle[]): string {
  const taken = new Set(existing.map((b) => b.name.trim().toLowerCase()));
  if (!taken.has("bundle")) return "Bundle";
  for (let n = 2; ; n++) {
    const name = `Bundle ${n}`;
    if (!taken.has(name.toLowerCase())) return name;
  }
}

/** Creates a bundle and hands back the row. Shared so every "New bundle…" makes one the same way. */
export async function createBundle(name: string): Promise<Bundle> {
  return api.post<Bundle>("/api/bundles", { name });
}

// Choosing the bundle a credential is filed under, with "New bundle…" as the last row of the
// same dropdown. Without it, needing a new bundle means closing this dialog, going to Access
// bundles, and starting the connection over from the top — which is how a half-entered
// credential gets lost.
export function BundlePicker({
  bundles,
  value,
  onChange,
  onCreated,
  id,
  disabled,
}: {
  bundles: Bundle[];
  /** The chosen bundle's id, or "" for none yet. */
  value: string;
  onChange: (id: string) => void;
  /** The owner refetches its bundle list; the new one is selected once it arrives. */
  onCreated: () => void | Promise<void>;
  id?: string;
  disabled?: boolean;
}) {
  const [naming, setNaming] = useState(false);
  // Set while the dropdown is closing *because* "New bundle…" was picked, so the trigger can
  // be told not to take focus back — see onCloseAutoFocus below.
  const toDialog = useRef(false);

  const create = async (name: string) => {
    try {
      const made = await createBundle(name);
      await onCreated();
      onChange(String(made.id));
      toast.success(`Created ${name}`);
      setNaming(false);
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  return (
    <>
      <Select
        value={value}
        onValueChange={(v) => {
          if (v !== NEW) {
            onChange(v);
            return;
          }
          toDialog.current = true;
          setNaming(true);
        }}
        disabled={disabled}
      >
        <SelectTrigger id={id} className="w-full">
          <SelectValue placeholder="Choose a bundle" />
        </SelectTrigger>
        <SelectContent
          // A closing Radix dropdown puts focus back on its trigger. That normally is what you
          // want, but here the dropdown is opening a dialog, and focus landing on the trigger
          // behind it means the name field never gets it — so typing goes nowhere and Enter
          // does not submit. Leaving focus alone lets the dialog's own autoFocus win.
          onCloseAutoFocus={(e) => {
            if (!toDialog.current) return;
            toDialog.current = false;
            e.preventDefault();
          }}
        >
          {bundles.map((b) => (
            <SelectItem key={b.id} value={String(b.id)}>
              {b.name}
            </SelectItem>
          ))}
          {bundles.length > 0 && <SelectSeparator />}
          <SelectItem value={NEW}>
            <Plus className="size-4 text-muted-foreground" />
            New bundle…
          </SelectItem>
        </SelectContent>
      </Select>
      <NameDialog
        open={naming}
        onOpenChange={setNaming}
        title="New bundle"
        description="Name it after what it grants — 'Engineering tools', 'Billing read-only'."
        label="Name"
        suggestion={defaultBundleName(bundles)}
        submitLabel="Create"
        onSubmit={create}
      />
    </>
  );
}
