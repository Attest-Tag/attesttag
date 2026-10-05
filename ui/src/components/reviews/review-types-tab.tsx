"use client";

import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import { ListChecks, Plus } from "lucide-react";
import { ErrorBanner } from "@/components/core/error-banner";
import { SegmentedControl } from "@/components/core/segmented-control";
import { StatusChip } from "@/components/core/status-chip";
import { revealPanel } from "@/components/reviews/review-format";
import { ReviewTypeEditor, typeKind } from "@/components/reviews/review-type-editor";
import { useAuth } from "@/components/shell/auth-provider";
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
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { useStickyValue } from "@/hooks/use-sticky";
import { api, errorMessage, onConsoleChange, useApi, type ReviewType, type SettingsResponse } from "@/lib/api";
import { announceReviewsSelection } from "@/lib/assistant-screen";
import { cn } from "@/lib/utils";

// Reviews › Types, laid out like Settings beside it: every type on the left — built-in, edited,
// custom or off, with how many branch rules name it — and one type's editor on the right. The type
// rides on ?type=<key>, which is how a try's "Open the type" lands here; without one, the type last
// opened in this browser, else General. A card in the assistant that creates a type, or whose Open
// is pressed here, hands the key over instead (CONSOLE_CHANGED_EVENT's select).

export function ReviewTypesTab() {
  const { me } = useAuth();
  const perms = me?.user?.permissions;
  const canManage = !perms || perms["reviews.manage"] === true;
  const canReach = !perms || perms["connections.manage"] === true;

  const list = useApi<{ types: ReviewType[] }>("/api/review-types");
  const settings = useApi<SettingsResponse>("/api/settings");
  const [sel, setSel] = useState<string>("");
  const [creating, setCreating] = useState(false);
  const panel = useRef<HTMLDivElement>(null);
  // The type this browser had open last, for an arrival that names none.
  const kept = useStickyValue<string>("reviews:type");

  useEffect(() => {
    const wanted = new URLSearchParams(window.location.search).get("type");
    // eslint-disable-next-line react-hooks/set-state-in-effect -- one-shot URL read on mount
    if (wanted) setSel(wanted);
  }, []);

  // A type handed over by a card. One the card has just created is not in the list until the list
  // reloads, so it waits here until it is: opened at once, the editor would fall back to the first
  // type in between and show it — and write it into the address — for a moment.
  const [handed, setHanded] = useState("");
  useEffect(
    () =>
      onConsoleChange(({ select }) => {
        if (select?.tab !== "types" || !select.type) return;
        setHanded(select.type);
        revealPanel(panel.current);
      }),
    [],
  );

  const types = list.data?.types ?? [];
  if (handed && types.some((t) => t.key === handed)) {
    setSel(handed);
    setHanded("");
  }
  const current = types.find((t) => t.key === (sel || kept.value)) ?? types[0];
  // Nothing is shown until the kept type has been read, so General never flashes up before it.
  const key = kept.ready ? (current?.key ?? "") : "";

  // The address and this browser follow the type on screen, and the assistant's chip with them.
  const keep = kept.set;
  useEffect(() => {
    if (!key) return;
    const url = new URL(window.location.href);
    url.searchParams.set("type", key);
    window.history.replaceState(null, "", url);
    keep(key);
    announceReviewsSelection(key);
  }, [key, keep]);

  if (list.error && !list.data) return <ErrorBanner message={list.error} onRetry={list.reload} />;
  if (!list.data) {
    return (
      <div className="grid gap-5 lg:grid-cols-[18rem_1fr]">
        <div className="space-y-2">
          {Array.from({ length: 3 }).map((_, i) => (
            <Skeleton key={i} className="h-12 w-full rounded-xl" />
          ))}
        </div>
        <Skeleton className="h-96 w-full rounded-xl" />
      </div>
    );
  }

  return (
    <div className="grid gap-5 lg:grid-cols-[18rem_1fr]">
      <div className="self-start overflow-hidden rounded-xl border bg-card">
        <ul className="divide-y">
          {types.map((t) => {
            const k = typeKind(t);
            const active = t.key === key;
            const used = t.used_by === 0 ? "no branch rule" : `${t.used_by} branch rule${t.used_by === 1 ? "" : "s"}`;
            return (
              <li key={t.key}>
                <button
                  type="button"
                  onClick={() => {
                    setSel(t.key);
                    revealPanel(panel.current);
                  }}
                  aria-current={active ? "true" : undefined}
                  title={`${t.name}: ${t.rules.length} rule${t.rules.length === 1 ? "" : "s"}, named by ${used}`}
                  className={cn(
                    "flex h-14 w-full items-center gap-3 px-3 text-left transition-colors hover:bg-secondary/60",
                    active && "bg-accent/60 hover:bg-accent/60",
                  )}
                >
                  <ListChecks className={cn("size-4 shrink-0", t.enabled ? "text-muted-foreground" : "text-muted-foreground/50")} />
                  <span className={cn("min-w-0 flex-1", !t.enabled && "opacity-60")}>
                    <span className="block truncate text-sm font-medium">{t.name}</span>
                    <span className="block truncate text-xs text-muted-foreground">
                      <span className="font-mono">{t.key}</span> · {used}
                      {(t.skills?.length ?? 0) > 0 && ` · ${t.skills!.length} skill${t.skills!.length === 1 ? "" : "s"}`}
                    </span>
                  </span>
                  {t.enabled ? (
                    <StatusChip variant={k.variant}>{k.label}</StatusChip>
                  ) : (
                    <StatusChip variant="warning">Off</StatusChip>
                  )}
                </button>
              </li>
            );
          })}
        </ul>
        {canManage && (
          <div className="flex justify-center border-t p-2">
            <Button variant="ghost" size="sm" onClick={() => setCreating(true)}>
              <Plus /> New type
            </Button>
          </div>
        )}
      </div>

      <div ref={panel} className="min-w-0 scroll-mt-4">
        {key ? (
          <ReviewTypeEditor
            key={key}
            typeKey={key}
            canManage={canManage}
            canReach={canReach}
            offeredModels={settings.data?.effective.ChannelModels ?? []}
            heavy={settings.data?.effective.HeavyModel ?? ""}
            onChanged={list.reload}
            onDeleted={(gone) => {
              setSel(types.find((t) => t.key !== gone)?.key ?? "");
              list.reload();
            }}
          />
        ) : (
          <Skeleton className="h-96 w-full rounded-xl" />
        )}
      </div>

      <NewTypeDialog
        open={creating}
        onOpenChange={setCreating}
        types={types}
        canReach={canReach}
        onCreated={(t) => {
          list.reload();
          setSel(t.key);
        }}
      />
    </div>
  );
}

/** "API contract" → "api-contract": the key a new type's name suggests, which the person may change. */
function keyOf(name: string): string {
  return name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^[^a-z]+/, "")
    .replace(/-+$/, "")
    .slice(0, 30)
    .replace(/-+$/, "");
}

const KEY_RE = /^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$/;

function NewTypeDialog({
  open,
  onOpenChange,
  types,
  canReach,
  onCreated,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  types: ReviewType[];
  canReach: boolean;
  onCreated: (t: ReviewType) => void;
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        {open && <NewTypeForm types={types} canReach={canReach} onClose={() => onOpenChange(false)} onCreated={onCreated} />}
      </DialogContent>
    </Dialog>
  );
}

/** A copy of a type with its own model or max $ sets them on the new one: connections.manage's to do. */
const copyHeld = (t: ReviewType, canReach: boolean) => !canReach && (!!t.model || t.max_usd > 0);

function NewTypeForm({
  types,
  canReach,
  onClose,
  onCreated,
}: {
  types: ReviewType[];
  canReach: boolean;
  onClose: () => void;
  onCreated: (t: ReviewType) => void;
}) {
  const [name, setName] = useState("");
  const [key, setKey] = useState("");
  const [keyTouched, setKeyTouched] = useState(false);
  const [from, setFrom] = useState<"blank" | "copy">("blank");
  const [copyOf, setCopyOf] = useState(types.find((t) => !copyHeld(t, canReach))?.key ?? "general");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const k = keyTouched ? key : keyOf(name);
  const taken = types.some((t) => t.key === k);
  const keyBad = k !== "" && (!KEY_RE.test(k) || k.length < 2 || k.length > 30);
  const nameBad = name.trim().length > 0 && (name.trim().length < 2 || name.trim().length > 40);
  const ready = name.trim().length >= 2 && !nameBad && k !== "" && !keyBad && !taken;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!ready || busy) return;
    setBusy(true);
    setError("");
    try {
      const out = await api.post<{ type: ReviewType }>("/api/review-types", {
        key: k,
        name: name.trim(),
        ...(from === "copy" ? { copy_from: copyOf } : {}),
      });
      toast.success(`${out.type.name} created`);
      onCreated(out.type);
      onClose();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form onSubmit={submit} className="space-y-4">
      <DialogHeader>
        <DialogTitle>New review type</DialogTitle>
        <DialogDescription>
          A rubric of your team&apos;s: what it is for, the files it reads and the rules it checks. Branch rules, Start review
          and @-commands can name it once it exists.
        </DialogDescription>
      </DialogHeader>
      <div className="space-y-1.5">
        <Label htmlFor="new-type-name">Name</Label>
        <Input id="new-type-name" value={name} onChange={(e) => setName(e.target.value)} placeholder="API contract" autoFocus aria-invalid={nameBad} />
      </div>
      <div className="space-y-1.5">
        <Label htmlFor="new-type-key">Key</Label>
        <Input
          id="new-type-key"
          value={k}
          onChange={(e) => {
            setKeyTouched(true);
            setKey(e.target.value.toLowerCase());
          }}
          className="font-mono"
          placeholder="api-contract"
          aria-invalid={keyBad || taken}
        />
        <p className={cn("text-xs", keyBad || taken ? "text-danger" : "text-muted-foreground")}>
          {taken
            ? "A type with this key exists already."
            : keyBad
              ? "2 to 30 lowercase letters, digits and hyphens, starting with a letter."
              : "Fixed once made: it is how branch rules and @… review name the type."}
        </p>
      </div>
      <div className="space-y-1.5">
        <p className="text-sm font-medium">Start from</p>
        <SegmentedControl<"blank" | "copy">
          value={from}
          onValueChange={setFrom}
          options={[
            { value: "blank", label: "Blank" },
            { value: "copy", label: "A copy of…" },
          ]}
          className="w-fit"
        />
        {from === "copy" && (
          <Select value={copyOf} onValueChange={setCopyOf}>
            <SelectTrigger className="mt-1.5 w-full" aria-label="Type to copy">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {types.map((t) => (
                <SelectItem key={t.key} value={t.key} disabled={copyHeld(t, canReach)}>
                  {t.name} <span className="font-mono text-xs text-muted-foreground">{t.key}</span>
                  {copyHeld(t, canReach) && <span className="text-xs text-muted-foreground"> — needs Manage connections</span>}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        )}
        <p className="text-xs text-muted-foreground">
          {from === "copy"
            ? "Its purpose, files, strictness and rules, as a type of your own. The original is left as it is."
            : "An empty rubric: you write what it is for and its rules."}
        </p>
      </div>
      {error && <p className="text-sm text-danger">{error}</p>}
      <DialogFooter>
        <Button variant="outline" onClick={onClose}>
          Cancel
        </Button>
        <Button type="submit" disabled={!ready} loading={busy}>
          Create type
        </Button>
      </DialogFooter>
    </form>
  );
}
