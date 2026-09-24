"use client";

import { CopyButton } from "@/components/core/copy-button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import { useApi, type Connection } from "@/lib/api";

// What the proxy sends for a connection, secret masked, from
// GET /api/connections/{id}/curl.
export function CurlDialog({
  connection,
  onOpenChange,
}: {
  connection: Connection | null;
  onOpenChange: (open: boolean) => void;
}) {
  const curl = useApi<{ curl: string }>(connection ? `/api/connections/${connection.id}/curl` : null);
  return (
    <Dialog open={connection !== null} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Resolved request{connection ? ` for ${connection.name}` : ""}</DialogTitle>
          <DialogDescription>
            The call the proxy makes on the bot&apos;s behalf, with the secret masked.
          </DialogDescription>
        </DialogHeader>
        {curl.loading || !curl.data ? (
          <Skeleton className="h-16 w-full" />
        ) : (
          <div className="relative">
            <pre className="overflow-x-auto rounded-lg border bg-muted/40 px-3 py-2 pr-10 font-mono text-[11px] leading-snug">
              {curl.data.curl}
            </pre>
            <CopyButton text={curl.data.curl} className="absolute right-1.5 top-1.5" />
          </div>
        )}
      </DialogContent>
    </Dialog>
  );
}
