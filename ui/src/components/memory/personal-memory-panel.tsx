"use client";

import { useState } from "react";
import { Loader2, Lock, Plus, Trash2, Pencil } from "lucide-react";
import { toast } from "sonner";
import { useConfirm } from "@/components/core/confirm-dialog";
import { EmptyState } from "@/components/core/empty-state";
import { ErrorBanner } from "@/components/core/error-banner";
import { RelativeTime } from "@/components/core/relative-time";
import { StatusChip } from "@/components/core/status-chip";
import { MemoryEditor } from "@/components/memory/memory-page";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Textarea } from "@/components/ui/textarea";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { api, errorMessage, useApi, type PersonalMemories } from "@/lib/api";

/**
 * Your own notes. Everything on this page is scoped to you by the server, through the Slack
 * account you signed in with — there is no route here that takes somebody else's id, and no
 * permission that would unlock one.
 */
export function PersonalMemoryPanel() {
  const notes = useApi<PersonalMemories>("/api/personal-memories");
  const { confirm, confirmDialog } = useConfirm();
  const [editing, setEditing] = useState<number | null>(null);
  const [draft, setDraft] = useState("");
  const [team, setTeam] = useState("");
  const [busy, setBusy] = useState(false);

  const identities = notes.data?.identities ?? [];
  const items = notes.data?.memories ?? [];
  const several = identities.length > 1;
  const effectiveTeam = team || identities[0]?.team_id || "";

  const add = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!draft.trim() || busy) return;
    setBusy(true);
    try {
      await api.post("/api/personal-memories", { team_id: effectiveTeam, text: draft.trim() });
      setDraft("");
      notes.reload();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const remove = async (id: number, text: string) => {
    const excerpt = text.length > 60 ? text.slice(0, 60) + "…" : text;
    const ok = await confirm({
      title: "Delete this note?",
      description: `"${excerpt}" is removed and the bot stops using it. Anything it already said in a conversation stays in that conversation.`,
      confirmLabel: "Delete",
      destructive: true,
    });
    if (!ok) return;
    try {
      await api.del(`/api/personal-memories/${id}`);
      notes.reload();
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  if (notes.loading) {
    return (
      <div className="space-y-2">
        <Skeleton className="h-4 w-40" />
        <Skeleton className="h-24 w-full rounded-xl" />
      </div>
    );
  }

  if (notes.error && !notes.data) {
    return <ErrorBanner message={notes.error} onRetry={notes.reload} />;
  }

  // Nothing to key notes to. Not an error, and the ordinary case for anyone who signed up with a
  // password and never connected Slack or Microsoft: a note belongs to the chat account the bot
  // talks to — Slack, or Microsoft for Teams — so there has to be one.
  if (identities.length === 0) {
    return (
      <EmptyState
        icon={Lock}
        title="Connect your chat account to keep your own notes"
        description="Your notes belong to the account the bot talks to — your Slack account, or for Teams your Microsoft one — so it can read them when you ask it something. Connect it to this console account in Settings and they will appear here."
        action={
          <Button asChild>
            <a href="/settings">Go to Settings</a>
          </Button>
        }
      />
    );
  }

  return (
    <div className="space-y-4">
      <p className="text-sm text-muted-foreground">
        Only you can see these. Nobody else in your organisation — including admins — can read or
        edit them, and the bot only uses them on your own turns.
      </p>

      <form onSubmit={add} className="flex items-start gap-2">
        <Textarea
          aria-label="New note"
          rows={2}
          value={draft}
          disabled={busy}
          placeholder="Something to keep for yourself…"
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.shiftKey) {
              e.preventDefault();
              void add(e as unknown as React.FormEvent);
            }
          }}
        />
        {several && (
          <Select value={effectiveTeam} onValueChange={setTeam}>
            <SelectTrigger className="w-44" aria-label="Workspace">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {identities.map((i) => (
                <SelectItem key={i.team_id} value={i.team_id}>
                  {i.team_name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        )}
        <Button type="submit" disabled={busy || !draft.trim()}>
          {busy ? <Loader2 className="animate-spin" /> : <Plus className="size-4" />}
          Add
        </Button>
      </form>

      {items.length === 0 ? (
        <EmptyState
          icon={Lock}
          title="No notes yet"
          description="Anything you want the bot to hold for you — what you owe someone, what to pick up tomorrow. Add one here, or say 'remember for me: …' to the bot in Slack."
        />
      ) : (
        <ul className="divide-y overflow-hidden rounded-xl border bg-card">
          {items.map((n) =>
            editing === n.id ? (
              <MemoryEditor
                key={n.id}
                value={n.text}
                onSave={async (text) => {
                  await api.put(`/api/personal-memories/${n.id}`, { text });
                  setEditing(null);
                  notes.reload();
                }}
                onCancel={() => setEditing(null)}
              />
            ) : (
              <li key={n.id} className="flex items-start gap-3 px-4 py-3">
                <p className="min-w-0 flex-1 whitespace-pre-wrap text-sm">{n.text}</p>
                <div className="flex shrink-0 items-center gap-2 text-xs text-muted-foreground">
                  {several && <StatusChip variant="neutral">{n.team_name}</StatusChip>}
                  <RelativeTime value={n.created_at} />
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    aria-label="Edit note"
                    className="text-muted-foreground"
                    onClick={() => setEditing(n.id)}
                  >
                    <Pencil />
                  </Button>
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    aria-label="Delete note"
                    className="text-muted-foreground hover:text-destructive"
                    onClick={() => remove(n.id, n.text)}
                  >
                    <Trash2 />
                  </Button>
                </div>
              </li>
            ),
          )}
        </ul>
      )}
      {confirmDialog}
    </div>
  );
}
