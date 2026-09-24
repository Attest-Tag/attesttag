"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { Brain, Loader2, Pencil, Plus, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { useTeamNames } from "@/components/core/team-cell";
import { useConfirm } from "@/components/core/confirm-dialog";
import { EmptyState } from "@/components/core/empty-state";
import { ErrorBanner } from "@/components/core/error-banner";
import { PageHeader } from "@/components/core/page-header";
import { RelativeTime } from "@/components/core/relative-time";
import { describeMemoryScope } from "@/components/core/scope-label";
import { StatusChip } from "@/components/core/status-chip";
import { AddMemoryDialog } from "@/components/memory/add-memory-dialog";
import { PersonalMemoryPanel } from "@/components/memory/personal-memory-panel";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Textarea } from "@/components/ui/textarea";
import { api, errorMessage, useApi, type Memory, type Scope } from "@/lib/api";

export function MemoryPage() {
  const memories = useApi<Memory[]>("/api/memories");
  const scopes = useApi<Scope[]>("/api/scopes?sync=0");
  const teams = useTeamNames();
  const { confirm, confirmDialog } = useConfirm();
  const [adding, setAdding] = useState(false);
  // One memory is edited at a time; opening another closes this one.
  const [editing, setEditing] = useState<number | null>(null);
  const [tab, setTab] = useState("shared");

  const groups = useMemo(() => {
    const map = new Map<string, Memory[]>();
    for (const m of memories.data ?? []) {
      const list = map.get(m.Scope) ?? [];
      list.push(m);
      map.set(m.Scope, list);
    }
    // Workspace first, then channels by name. The store keys a whole workspace as "team:T…".
    const isWorkspace = (scope: string) => scope.startsWith("team:") || scope.startsWith("workspace:");
    return [...map.entries()].sort(([a], [b]) => {
      const wa = isWorkspace(a) ? 0 : 1;
      const wb = isWorkspace(b) ? 0 : 1;
      if (wa !== wb) return wa - wb;
      return describeMemoryScope(scopes.data, a).name.localeCompare(
        describeMemoryScope(scopes.data, b).name,
      );
    });
  }, [memories.data, scopes.data]);

  const remove = async (m: Memory) => {
    const excerpt = m.Text.length > 60 ? m.Text.slice(0, 60) + "…" : m.Text;
    const ok = await confirm({
      title: "Forget this memory?",
      description: `"${excerpt}" is removed and the bot stops using it. This can't be undone.`,
      confirmLabel: "Forget",
      destructive: true,
    });
    if (!ok) return;
    try {
      await api.del(`/api/memories/${m.ID}`);
      toast.success("Memory forgotten");
      memories.reload();
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  const update = async (m: Memory, text: string) => {
    await api.put(`/api/memories/${m.ID}`, { text });
    toast.success("Memory updated");
    setEditing(null);
    memories.reload();
  };

  const addButton = (
    <Button onClick={() => setAdding(true)}>
      <Plus className="size-4" />
      Add memory
    </Button>
  );

  // Two ranges of the same idea, side by side, which is what teaches the difference: what this
  // organisation's rooms know, and what you asked to be kept for you. A second rail entry a word
  // apart from "Memory" would have been the trap nav.ts already warns about.
  return (
    <div className="space-y-5">
      <PageHeader
        title="Memory"
        description="Facts the bot keeps per channel and workspace, and the private notes it keeps for you alone."
        actions={tab === "shared" ? addButton : undefined}
      />

      <Tabs value={tab} onValueChange={setTab}>
        <TabsList>
          <TabsTrigger value="shared">Shared</TabsTrigger>
          <TabsTrigger value="mine">Yours</TabsTrigger>
        </TabsList>
        <TabsContent value="mine" className="pt-4">
          <PersonalMemoryPanel />
        </TabsContent>
        <TabsContent value="shared" className="space-y-5 pt-4">

      {memories.error && !memories.data && (
        <ErrorBanner message={memories.error} onRetry={memories.reload} />
      )}

      {memories.loading ? (
        <div className="space-y-4">
          {Array.from({ length: 2 }).map((_, i) => (
            <div key={i} className="space-y-2">
              <Skeleton className="h-4 w-40" />
              <Skeleton className="h-24 w-full rounded-xl" />
            </div>
          ))}
        </div>
      ) : groups.length === 0 ? (
        <EmptyState
          icon={Brain}
          title="Nothing remembered yet"
          description="Memories are short facts the bot should keep in mind — a team's on-call rota, a product's codename. Add one here, or say 'remember for this channel: …' to the bot in Slack."
          action={addButton}
        />
      ) : (
        <div className="space-y-6">
          {groups.map(([scope, items]) => {
            const desc = describeMemoryScope(scopes.data, scope);
            return (
              <section key={scope} className="space-y-2">
                <div className="flex items-center gap-2">
                  <h2 className="text-sm font-semibold">{desc.name}</h2>
                  <StatusChip variant={desc.kind === "team" ? "info" : "neutral"}>
                    {desc.kind === "team" ? "all channels" : desc.kind}
                  </StatusChip>
                  <span className="font-mono text-xs text-muted-foreground">{desc.id}</span>
                  {teams.several && desc.kind === "channel" && (
                    <span className="text-xs text-muted-foreground">
                      in {teams.name(desc.teamId)}
                    </span>
                  )}
                </div>
                <ul className="divide-y overflow-hidden rounded-xl border bg-card">
                  {items.map((m) =>
                    editing === m.ID ? (
                      <MemoryEditor
                        key={m.ID}
                        value={m.Text}
                        onSave={(text) => update(m, text)}
                        onCancel={() => setEditing(null)}
                      />
                    ) : (
                      <li key={m.ID} className="flex items-start gap-3 px-4 py-3">
                        <p className="min-w-0 flex-1 whitespace-pre-wrap text-sm">{m.Text}</p>
                        <div className="flex shrink-0 items-center gap-2 text-xs text-muted-foreground">
                          <RelativeTime value={m.At} />
                          <Button
                            variant="ghost"
                            size="icon-sm"
                            aria-label="Edit memory"
                            className="text-muted-foreground"
                            onClick={() => setEditing(m.ID)}
                          >
                            <Pencil />
                          </Button>
                          <Button
                            variant="ghost"
                            size="icon-sm"
                            aria-label="Forget memory"
                            className="text-muted-foreground hover:text-destructive"
                            onClick={() => remove(m)}
                          >
                            <Trash2 />
                          </Button>
                        </div>
                      </li>
                    ),
                  )}
                </ul>
              </section>
            );
          })}
        </div>
      )}

      <AddMemoryDialog
        open={adding}
        onOpenChange={setAdding}
        scopes={scopes.data ?? []}
        onAdded={memories.reload}
      />
        </TabsContent>
      </Tabs>
      {confirmDialog}
    </div>
  );
}

/**
 * One memory turned into a form, in the same row it occupied. Enter saves and Shift+Enter
 * breaks a line, like every other console form; Escape puts the text back.
 */
export function MemoryEditor({
  value,
  onSave,
  onCancel,
}: {
  value: string;
  onSave: (text: string) => Promise<void>;
  onCancel: () => void;
}) {
  const [text, setText] = useState(value);
  const [busy, setBusy] = useState(false);
  const ref = useRef<HTMLTextAreaElement>(null);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    el.focus();
    el.setSelectionRange(el.value.length, el.value.length);
  }, []);

  const trimmed = text.trim();
  const unchanged = trimmed === value.trim();

  const submit = async (e?: React.FormEvent) => {
    e?.preventDefault();
    if (!trimmed || unchanged || busy) return;
    setBusy(true);
    try {
      await onSave(trimmed);
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <li className="px-4 py-3">
      <form onSubmit={submit} className="space-y-2">
        <Textarea
          ref={ref}
          aria-label="Memory text"
          rows={3}
          value={text}
          disabled={busy}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Escape") {
              e.preventDefault();
              onCancel();
            } else if (e.key === "Enter" && !e.shiftKey) {
              e.preventDefault();
              void submit();
            }
          }}
        />
        <div className="flex items-center justify-end gap-2">
          <Button type="button" variant="outline" size="sm" onClick={onCancel} disabled={busy}>
            Cancel
          </Button>
          <Button type="submit" size="sm" disabled={busy || !trimmed || unchanged}>
            {busy && <Loader2 className="animate-spin" />}
            Save
          </Button>
        </div>
      </form>
    </li>
  );
}
