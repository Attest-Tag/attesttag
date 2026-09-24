"use client";

import { useEffect, useRef, useState } from "react";
import { usePathname } from "next/navigation";
import { FileText, Image as ImageIcon, Paperclip, Plus, Sparkles, Trash2, X } from "lucide-react";
import { toast } from "sonner";
import { Disclosure } from "@/components/core/disclosure";
import { Markdown } from "@/components/core/markdown";
import { ModelCombobox } from "@/components/core/model-combobox";
import { StatusChip } from "@/components/core/status-chip";
import { WorkingIndicator } from "@/components/core/working-indicator";
import { ProposalCard } from "@/components/assistant/proposal-card";
import { useAssistant } from "@/components/assistant/assistant-provider";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { assistantHintFor } from "@/lib/assistant-hints";
import { api, errorMessage, type AssistantMessage, type AssistantReply, type PlaygroundTool } from "@/lib/api";
import { navItemFor } from "@/lib/nav";

// The console assistant. One panel, mounted once beside the whole inset, so it survives every
// navigation — which is also why it cannot be handed page context as a prop. The address bar is
// the only thing the shell and the routed page both see, so that is where the context comes
// from: usePathname for the page, and the ?scope= the channel pages already write.
//
// The scope is read imperatively, at the moment Enter is pressed, rather than through
// useSearchParams. This is a static export and this component is on every route, so a
// useSearchParams here would bail the whole console out of prerendering — and there is no
// Suspense boundary anywhere in the tree to catch it. It is also the more correct answer: what
// should be attached is the channel that was on screen when the question was asked, not whatever
// the router has settled on by the time the reply lands.
//
// The conversation lives here and rides back with each question, so closing the tab ends it in
// the panel. It is still recorded — every turn lands in Activity with what was asked and what
// came back — but as history rather than as state: nothing is read back from there into a turn.

type Turn =
  | { role: "you"; text: string; files: string[] }
  | { role: "bot"; text: string; meta: AssistantReply };

function money(usd: number): string {
  if (!usd) return "$0";
  return usd < 0.01 ? `$${usd.toFixed(4)}` : `$${usd.toFixed(2)}`;
}

function scopeOnScreen(): number {
  if (typeof window === "undefined") return 0;
  return Number(new URLSearchParams(window.location.search).get("scope")) || 0;
}

function ToolRow({ tool }: { tool: PlaygroundTool }) {
  return (
    <Disclosure
      label={
        <span className="flex items-center gap-1.5">
          <span className="font-mono text-[11px]">{tool.name}</span>
          {!tool.ok && <StatusChip variant="danger">failed</StatusChip>}
        </span>
      }
      summary={`${tool.ms} ms`}
    >
      <pre className="mt-1 max-h-48 overflow-auto rounded-md bg-muted/60 p-2 text-[11px] leading-relaxed break-words whitespace-pre-wrap">
        {tool.result || "(empty)"}
      </pre>
    </Disclosure>
  );
}

function BotTurn({ turn }: { turn: Extract<Turn, { role: "bot" }> }) {
  const m = turn.meta;
  return (
    <div className="space-y-2">
      {turn.text && (
        <div className="rounded-lg rounded-tl-sm border bg-card px-3 py-2">
          <Markdown text={turn.text} className="text-sm" />
        </div>
      )}
      {m.proposals?.map((p) => <ProposalCard key={p.id} proposal={p} />)}
      {m.files?.filter((f) => f.kind === "skipped").map((f) => (
        <p key={f.name} className="rounded-lg border border-warning/30 bg-warning-soft px-3 py-2 text-xs text-warning">
          {f.name} — {f.note}
        </p>
      ))}
      {m.error && (
        <p className="rounded-lg border border-danger/30 bg-danger-soft px-3 py-2 text-xs text-danger">{m.error}</p>
      )}
      {m.tools.length > 0 && (
        <div className="space-y-1 rounded-lg border bg-card px-3 py-1.5">
          {m.tools.map((t, i) => <ToolRow key={i} tool={t} />)}
        </div>
      )}
      <p className="flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[11px] text-muted-foreground">
        <span className="font-mono">{m.model}</span>
        <span>·</span>
        <span>{m.tokens_in} in · {m.tokens_out} out</span>
        <span>·</span>
        <span>{money(m.cost_usd)}</span>
      </p>
    </div>
  );
}

export function AssistantPanel() {
  const { open, setOpen } = useAssistant();
  const pathname = usePathname();
  const [turns, setTurns] = useState<Turn[]>([]);
  const [text, setText] = useState("");
  const [files, setFiles] = useState<File[]>([]);
  const [model, setModel] = useState("");
  const [busy, setBusy] = useState(false);
  const tail = useRef<HTMLDivElement>(null);
  const picker = useRef<HTMLInputElement>(null);
  // Groups this panel's questions together in Activity. A ref rather than state: it must not
  // change when the component re-renders, and nothing renders from it.
  const conversation = useRef(
    typeof crypto !== "undefined" && crypto.randomUUID ? crypto.randomUUID() : String(Date.now()),
  );

  useEffect(() => {
    tail.current?.scrollIntoView({ behavior: "smooth", block: "end" });
  }, [turns, busy]);

  // Every hook above this line, so a conversation survives the panel being closed and reopened.
  if (!open) return null;

  const hint = assistantHintFor(pathname);
  const page = navItemFor(pathname);

  const addFiles = (chosen: FileList | null) => {
    if (!chosen) return;
    setFiles((prev) => [...prev, ...Array.from(chosen)].slice(0, 4));
  };

  const send = async () => {
    const ask = text.trim();
    if ((!ask && files.length === 0) || busy) return;
    const history: AssistantMessage[] = turns.map((t) =>
      t.role === "you" ? { role: "you", text: t.text } : { role: "assistant", text: t.text },
    );
    const sent = files;
    setText("");
    setFiles([]);
    setTurns((t) => [...t, { role: "you", text: ask, files: sent.map((f) => f.name) }]);
    setBusy(true);
    try {
      // Multipart only when there is something to carry: the JSON path is the ordinary turn and
      // there is no reason to make every question pay for a form encoder.
      const common = {
        question: ask,
        path: pathname,
        page: page?.title ?? "",
        scope_id: scopeOnScreen(),
        model,
        conversation: conversation.current,
      };
      let res: AssistantReply;
      if (sent.length > 0) {
        const form = new FormData();
        form.set("question", common.question);
        form.set("path", common.path);
        form.set("page", common.page);
        form.set("scope_id", String(common.scope_id));
        form.set("model", common.model);
        form.set("conversation", common.conversation);
        form.set("history", JSON.stringify(history));
        for (const f of sent) form.append("file", f);
        res = await api.upload<AssistantReply>("/api/assistant", form);
      } else {
        res = await api.post<AssistantReply>("/api/assistant", { ...common, history });
      }
      setTurns((t) => [...t, { role: "bot", text: res.reply, meta: res }]);
    } catch (err) {
      // Nothing reached the model, so the question goes back in the box rather than sitting in
      // the transcript as something that was asked and never answered.
      setTurns((t) => t.slice(0, -1));
      setText(ask);
      setFiles(sent);
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    // One element, two layouts. A flex sibling of the inset from lg up, so the page is pushed
    // rather than covered — you are asking about what is on screen, and covering it is the one
    // thing this panel must not do. Below that there is no room to push, so it takes the
    // screen. z-40 keeps it under the mobile rail's own sheet.
    <aside
      className="fixed inset-0 z-40 flex w-full flex-col border-l bg-background lg:static lg:z-auto lg:w-[24rem] lg:shrink-0"
      aria-label="Assistant"
    >
      <header className="flex h-12 shrink-0 items-center gap-2 border-b px-3">
        <Sparkles className="size-4 text-ai" />
        <p className="min-w-0 flex-1 truncate text-sm font-medium">Assistant</p>
        {turns.length > 0 && (
          <Button
            variant="ghost"
            size="icon"
            className="size-8 text-muted-foreground"
            onClick={() => {
              setTurns([]);
              conversation.current =
                typeof crypto !== "undefined" && crypto.randomUUID ? crypto.randomUUID() : String(Date.now());
            }}
            aria-label="Clear the conversation"
          >
            <Trash2 className="size-4" />
          </Button>
        )}
        <Button
          variant="ghost"
          size="icon"
          className="size-8 text-muted-foreground"
          onClick={() => setOpen(false)}
          aria-label="Close the assistant"
        >
          <X className="size-4" />
        </Button>
      </header>

      <div className="flex-1 space-y-3 overflow-y-auto px-3 py-3">
        {turns.length === 0 && (
          // Centred and unboxed: the opening line is an invitation, and a dashed box around it
          // reads as a slot with something missing from it.
          <div className="pt-10 text-center">
            <Sparkles className="mx-auto size-6 text-ai" strokeWidth={1.5} />
            <p className="mt-2 text-sm font-medium text-foreground">{hint.title}</p>
            <p className="mx-auto mt-1 max-w-[260px] text-xs leading-relaxed text-muted-foreground">{hint.body}</p>
          </div>
        )}
        {turns.map((turn, i) =>
          turn.role === "you" ? (
            <div key={i} className="ml-auto max-w-[85%] space-y-1">
              {turn.text && (
                <p className="rounded-lg rounded-br-sm bg-secondary px-3 py-2 text-sm break-words whitespace-pre-wrap">
                  {turn.text}
                </p>
              )}
              {turn.files.map((name) => (
                <p key={name} className="flex items-center justify-end gap-1 text-[11px] text-muted-foreground">
                  <Paperclip className="size-3" />
                  {name}
                </p>
              ))}
            </div>
          ) : (
            <BotTurn key={i} turn={turn} />
          ),
        )}
        {busy && <WorkingIndicator />}
        <div ref={tail} />
      </div>

      <div className="shrink-0 space-y-1.5 border-t px-3 py-2.5">
        {/* What the question will carry. The page tag is why the assistant can answer "this
            channel" without being told which — showing it is what makes that predictable
            rather than uncanny. */}
        <div className="flex flex-wrap items-center gap-1">
          <span className="inline-flex items-center gap-1 rounded bg-secondary px-1.5 py-0.5 text-[11px] text-muted-foreground">
            <Sparkles className="size-3 text-ai" />
            {page?.title ?? "Console"}
          </span>
          {files.map((f, i) => (
            <span key={i} className="inline-flex items-center gap-1 rounded bg-secondary px-1.5 py-0.5 text-[11px]">
              {f.type.startsWith("image/") ? <ImageIcon className="size-3" /> : <FileText className="size-3" />}
              <span className="max-w-[9rem] truncate">{f.name}</span>
              <button
                type="button"
                onClick={() => setFiles((prev) => prev.filter((_, j) => j !== i))}
                aria-label={`Remove ${f.name}`}
                className="text-muted-foreground hover:text-foreground"
              >
                <X className="size-3" />
              </button>
            </span>
          ))}
        </div>

        <Textarea
          value={text}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.shiftKey) {
              e.preventDefault();
              send();
            }
          }}
          rows={2}
          disabled={busy}
          placeholder="Ask about this page, or attach a file…"
          aria-label="Ask the assistant"
        />

        <div className="flex items-center gap-1.5">
          <input
            ref={picker}
            type="file"
            multiple
            accept="image/*,text/*,.md,.csv,.json,.yaml,.yml,.log,.txt,.conf"
            className="hidden"
            onChange={(e) => {
              addFiles(e.target.files);
              e.target.value = "";
            }}
          />
          <Button
            variant="ghost"
            size="icon"
            className="size-8 shrink-0 text-muted-foreground"
            onClick={() => picker.current?.click()}
            disabled={busy || files.length >= 4}
            aria-label="Attach a file"
          >
            <Plus className="size-4" />
          </Button>
          <div className="flex-1" />
          {/* Quiet, and not a field. Which model answers is a setting somebody changes rarely,
              so it reads as a label you can click rather than as a box waiting to be filled in
              — which is what a full-height bordered control next to the composer looked like.
              Beside Send because both are about the question you are about to ask. */}
          <ModelCombobox
            value={model}
            onChange={setModel}
            emptyLabel="Default"
            options={[{ value: "heavy", label: "Advanced" }]}
            disabled={busy}
            className="h-7 w-auto max-w-[10rem] border-0 px-1.5 text-xs text-muted-foreground shadow-none hover:bg-secondary"
            aria-label="Model"
          />
          <Button size="sm" className="shrink-0" onClick={send} disabled={busy || (!text.trim() && files.length === 0)}>
            Send
          </Button>
        </div>
      </div>
    </aside>
  );
}
