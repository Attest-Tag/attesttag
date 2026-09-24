"use client";

import { useMemo, useState } from "react";
import { BundlePicker } from "@/components/bundles/bundle-picker";
import { ServiceMark } from "@/components/core/service-mark";
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
  SelectGroup,
  SelectItem,
  SelectLabel,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import type { Bundle, Preset } from "@/lib/api";

// "Create → Connection" from the bundles page: a connection needs a bundle to live in and a
// preset to shape it, so this asks for both and hands off to the same Connect dialog a bundle's
// own Connect buttons open. Without a bundle there is nowhere to put it, so the shortcut is to
// create one first.
export function NewConnectionDialog({
  open,
  bundles,
  presets,
  onOpenChange,
  onPick,
  onCreateBundle,
  onBundlesChanged,
}: {
  open: boolean;
  bundles: Bundle[];
  presets: Preset[];
  onOpenChange: (open: boolean) => void;
  onPick: (bundle: Bundle, preset: Preset) => void;
  onCreateBundle: () => void;
  /** A bundle was made from the "+" in here, so the caller should refetch its list. */
  onBundlesChanged: () => void | Promise<void>;
}) {
  const [bundleId, setBundleId] = useState("");
  const [presetId, setPresetId] = useState("");
  const chosenBundle = bundles.find((b) => String(b.id) === (bundleId || String(bundles[0]?.id ?? "")));
  const chosenPreset = presets.find((p) => p.id === presetId);

  const groups = useMemo(() => {
    const byCategory = new Map<string, Preset[]>();
    for (const p of presets) {
      const list = byCategory.get(p.category) ?? [];
      list.push(p);
      byCategory.set(p.category, list);
    }
    return Array.from(byCategory.entries());
  }, [presets]);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>New connection</DialogTitle>
          <DialogDescription>
            A credential for one service. It lives in a bundle, and the bot reaches it wherever
            that bundle, or the connection on its own, is attached.
          </DialogDescription>
        </DialogHeader>
        {bundles.length === 0 ? (
          <div className="space-y-3">
            <p className="text-sm text-muted-foreground">
              There is no bundle to put it in yet. Create one first; it takes a name and nothing else.
            </p>
            <Button
              onClick={() => {
                onOpenChange(false);
                onCreateBundle();
              }}
            >
              Create a bundle
            </Button>
          </div>
        ) : (
          <div className="space-y-4">
            <div className="space-y-1.5">
              <Label htmlFor="new-conn-bundle">Bundle</Label>
              <BundlePicker
                id="new-conn-bundle"
                bundles={bundles}
                value={chosenBundle ? String(chosenBundle.id) : ""}
                onChange={setBundleId}
                onCreated={onBundlesChanged}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="new-conn-preset">Service</Label>
              <Select value={presetId} onValueChange={setPresetId}>
                <SelectTrigger id="new-conn-preset" className="w-full">
                  <SelectValue placeholder="Choose a service" />
                </SelectTrigger>
                <SelectContent>
                  {groups.map(([category, list]) => (
                    <SelectGroup key={category}>
                      <SelectLabel>{category}</SelectLabel>
                      {list.map((p) => (
                        <SelectItem key={p.id} value={p.id}>
                          <ServiceMark preset={p.id} name={p.name} className="text-muted-foreground" />
                          {p.name}
                        </SelectItem>
                      ))}
                    </SelectGroup>
                  ))}
                </SelectContent>
              </Select>
              {chosenPreset?.docs_url && (
                <p className="text-xs text-muted-foreground">
                  Where the credential comes from:{" "}
                  <a href={chosenPreset.docs_url} target="_blank" rel="noreferrer" className="underline">
                    {chosenPreset.docs_url}
                  </a>
                </p>
              )}
            </div>
          </div>
        )}
        {bundles.length > 0 && (
          <DialogFooter>
            <Button variant="ghost" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button
              disabled={!chosenBundle || !chosenPreset}
              onClick={() => {
                if (!chosenBundle || !chosenPreset) return;
                onOpenChange(false);
                onPick(chosenBundle, chosenPreset);
              }}
            >
              Continue
            </Button>
          </DialogFooter>
        )}
      </DialogContent>
    </Dialog>
  );
}
