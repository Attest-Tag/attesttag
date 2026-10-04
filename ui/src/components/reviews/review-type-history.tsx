"use client";

import { useState } from "react";
import { toast } from "sonner";
import { ChevronRight, RotateCcw } from "lucide-react";
import { useConfirm } from "@/components/core/confirm-dialog";
import { RelativeTime } from "@/components/core/relative-time";
import { StatusChip } from "@/components/core/status-chip";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import { api, errorMessage, useApi, type ReviewType, type ReviewTypeVersion } from "@/lib/api";
import { cn } from "@/lib/utils";

// A type's history: every save is a version, and every run names the version it ran with, so the
// list only ever grows. Revert saves an old version's text as the newest one — the run that used
// the version being undone still names what it ran with — and leaves the type's switch where it is.

export function ReviewTypeHistoryDialog({
  open,
  onOpenChange,
  type,
  canManage,
  dirty,
  onReverted,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  type: ReviewType;
  canManage: boolean;
  /** Unsaved edits in the editor, which a revert discards. */
  dirty: boolean;
  onReverted: (t: ReviewType) => void;
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>History of {type.name}</DialogTitle>
          <DialogDescription>
            Every save is a version, and every review records the version it ran with, so an old review still shows what it
            was asked to check.
          </DialogDescription>
        </DialogHeader>
        {open && (
          <Versions
            key={`${type.key}:${type.version}`}
            type={type}
            canManage={canManage}
            dirty={dirty}
            onReverted={(t) => {
              onReverted(t);
              onOpenChange(false);
            }}
          />
        )}
      </DialogContent>
    </Dialog>
  );
}

function Versions({
  type,
  canManage,
  dirty,
  onReverted,
}: {
  type: ReviewType;
  canManage: boolean;
  dirty: boolean;
  onReverted: (t: ReviewType) => void;
}) {
  const versions = useApi<{ versions: ReviewTypeVersion[] }>(`/api/review-types/${encodeURIComponent(type.key)}/versions`);
  const [shown, setShown] = useState<number | null>(null);
  const [busy, setBusy] = useState<number | null>(null);
  const { confirm, confirmDialog } = useConfirm();

  const revert = async (v: number) => {
    const ok = await confirm({
      title: `Go back to v${v}?`,
      description: `Its text is saved as v${type.version + 1}, the newest version; v${type.version} stays in the history.${dirty ? " Unsaved edits in the editor are discarded." : ""}`,
      confirmLabel: `Revert to v${v}`,
    });
    if (!ok) return;
    setBusy(v);
    try {
      const out = await api.post<{ type: ReviewType }>(`/api/review-types/${encodeURIComponent(type.key)}/revert`, { version: v });
      toast.success(`${type.name} is back to v${v}, saved as v${out.type.version}`);
      onReverted(out.type);
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };

  if (versions.error && !versions.data) return <p className="text-sm text-danger">{versions.error}</p>;
  if (!versions.data) {
    return (
      <div className="space-y-2">
        <Skeleton className="h-10 w-full" />
        <Skeleton className="h-10 w-full" />
      </div>
    );
  }
  if (versions.data.versions.length === 0) {
    return (
      <p className="rounded-lg border border-dashed px-3 py-4 text-sm text-muted-foreground">
        No saved versions: this is the built-in as attest_tag ships it. The first change made here is saved as v1 (the built-in)
        and v2 (the change).
      </p>
    );
  }
  return (
    <>
      <ol className="divide-y rounded-lg border">
        {versions.data.versions.map((v) => {
          const current = v.version === type.version;
          const isOpen = shown === v.version;
          return (
            <li key={v.version} className="px-3 py-2">
              <div className="flex flex-wrap items-center gap-2 text-sm">
                <button
                  type="button"
                  onClick={() => setShown(isOpen ? null : v.version)}
                  aria-expanded={isOpen}
                  className="flex min-w-0 flex-1 items-center gap-2 text-left"
                >
                  <ChevronRight className={cn("size-4 shrink-0 text-muted-foreground transition-transform", isOpen && "rotate-90")} />
                  <span className="font-medium tabular-nums">v{v.version}</span>
                  {current && <StatusChip variant="success">Current</StatusChip>}
                  <span className="min-w-0 truncate text-xs text-muted-foreground">
                    {v.created_by || "unknown"} · <RelativeTime value={v.created_at} />
                  </span>
                </button>
                {canManage && !current && (
                  <Button variant="outline" size="xs" onClick={() => void revert(v.version)} loading={busy === v.version}>
                    <RotateCcw /> Revert
                  </Button>
                )}
              </div>
              {isOpen && <VersionBody typeKey={type.key} version={v.version} />}
            </li>
          );
        })}
      </ol>
      {confirmDialog}
    </>
  );
}

/** One version exactly as it read. */
function VersionBody({ typeKey, version }: { typeKey: string; version: number }) {
  const snap = useApi<{ type: ReviewType }>(`/api/review-types/${encodeURIComponent(typeKey)}/versions/${version}`);
  if (snap.error && !snap.data) return <p className="mt-2 text-xs text-danger">{snap.error}</p>;
  if (!snap.data) return <Skeleton className="mt-2 h-16 w-full" />;
  const t = snap.data.type;
  return (
    <div className="mt-2 space-y-2 rounded-md bg-muted/40 p-2.5 text-xs">
      <p>
        <span className="font-medium">{t.name}</span>
        {t.strictness ? ` · ${t.strictness} strictness` : ""}
        {t.inline_min_severity ? ` · inline ${t.inline_min_severity} and worse` : ""}
        {t.model ? ` · model ${t.model}` : ""}
        {t.max_usd ? ` · up to $${t.max_usd.toFixed(2)}` : ""}
        {t.path_globs?.length ? ` · only ${t.path_globs.join(", ")}` : ""}
      </p>
      {t.purpose && <p className="whitespace-pre-wrap text-muted-foreground">{t.purpose}</p>}
      {(t.skills?.length ?? 0) > 0 && (
        <ol className="space-y-0.5">
          {t.skills!.map((l, i) => (
            <li key={i} className="flex gap-2">
              <span className="w-7 shrink-0 tabular-nums text-muted-foreground">S{i + 1}</span>
              <span className="min-w-0 flex-1 break-all font-mono">
                {l.repo ? `${l.repo}${l.ref ? `@${l.ref}` : ""}:` : ""}
                {l.path}
                {l.repo ? "" : <span className="font-sans text-muted-foreground"> · in the repository under review</span>}
              </span>
            </li>
          ))}
        </ol>
      )}
      <ol className="space-y-0.5">
        {(t.rules ?? []).map((r, i) => (
          <li key={i} className={cn("flex gap-2", (!r.enabled || r.status === "rejected") && "text-muted-foreground line-through")}>
            <span className="w-7 shrink-0 tabular-nums text-muted-foreground">R{i + 1}</span>
            <span className="min-w-0 flex-1">
              {r.text}
              {r.severity_cap ? <span className="text-muted-foreground"> · ≤ {r.severity_cap}</span> : null}
              {r.status === "proposed" ? <span className="text-warning"> · proposed</span> : null}
            </span>
          </li>
        ))}
      </ol>
    </div>
  );
}
