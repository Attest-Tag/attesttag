"use client";

import { useState } from "react";
import { Check, KeyRound, Package, Plus } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { isRepoBundle } from "@/components/scopes/repo-sources";
import type { Bundle } from "@/lib/api";

// "Add" for a scope: every bundle, then every connection as bundle/name, in
// one searchable list that ticks what is already attached. Picking a bundle
// grants all of it; picking a connection grants just that one, on its own.
export function AddAccessPopover({
  bundles,
  attachedBundles,
  attachedConnections,
  onAttachBundle,
  onAttachConnection,
  busy,
}: {
  bundles: Bundle[];
  attachedBundles: number[];
  attachedConnections: number[];
  onAttachBundle: (bundleId: number) => Promise<void>;
  onAttachConnection: (connectionId: number) => Promise<void>;
  busy: boolean;
}) {
  const [open, setOpen] = useState(false);
  // Repositories are not offered here. The bundle they live in grants every repository the
  // account has, including the ones added to it next week, and a repository attached from this
  // list cannot be taken off again in the Repositories section below — that section only owns
  // the ones it attached. The section does the same job better: it groups by credential,
  // selects a whole source at once, and removes what it added.
  const shown = bundles.filter((b) => !isRepoBundle(b));
  const connections = bundles.flatMap((bundle) =>
    (bundle.connections ?? [])
      .filter((connection) => !connection.repo)
      .map((connection) => ({ bundle, connection })),
  );
  const hiddenRepos = bundles.length !== shown.length;

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <Button variant="outline" size="sm" disabled={busy}>
          <Plus className="size-4" />
          Add
        </Button>
      </PopoverTrigger>
      <PopoverContent className="w-80 p-0" align="end">
        <Command>
          <CommandInput placeholder="Search bundles and connections…" />
          <CommandList>
            <CommandEmpty>
              {shown.length === 0 && connections.length === 0
                ? "No bundles yet. Create one under Access bundles."
                : "Nothing matches."}
            </CommandEmpty>
            {shown.length > 0 && (
              <CommandGroup heading="Bundles">
                {shown.map((bundle) => {
                  const on = attachedBundles.includes(bundle.id);
                  return (
                    <CommandItem
                      key={`bundle-${bundle.id}`}
                      value={bundle.name}
                      disabled={on}
                      onSelect={async () => {
                        if (on) return;
                        await onAttachBundle(bundle.id);
                        setOpen(false);
                      }}
                    >
                      <Package className="size-4 text-muted-foreground" />
                      <span className="flex-1 truncate">{bundle.name}</span>
                      {on && <Check className="size-4 text-muted-foreground" />}
                    </CommandItem>
                  );
                })}
              </CommandGroup>
            )}
            {connections.length > 0 && (
              <CommandGroup heading="Connections">
                {connections.map(({ bundle, connection }) => {
                  const on = attachedConnections.includes(connection.id);
                  const viaBundle = attachedBundles.includes(bundle.id);
                  return (
                    <CommandItem
                      key={`connection-${connection.id}`}
                      value={`${bundle.name}/${connection.name}`}
                      disabled={on}
                      onSelect={async () => {
                        if (on) return;
                        await onAttachConnection(connection.id);
                        setOpen(false);
                      }}
                    >
                      <KeyRound className="size-4 text-muted-foreground" />
                      <span className="flex-1 truncate">
                        <span className="text-muted-foreground">{bundle.name}/</span>
                        {connection.name}
                      </span>
                      {on ? (
                        <Check className="size-4 text-muted-foreground" />
                      ) : viaBundle ? (
                        <span className="text-xs text-muted-foreground">via bundle</span>
                      ) : null}
                    </CommandItem>
                  );
                })}
              </CommandGroup>
            )}
          </CommandList>
        </Command>
        {hiddenRepos && (
          <p className="border-t px-3 py-2 text-xs text-muted-foreground">
            Repositories are added in the Repositories section below, one or a whole account at a
            time.
          </p>
        )}
      </PopoverContent>
    </Popover>
  );
}
