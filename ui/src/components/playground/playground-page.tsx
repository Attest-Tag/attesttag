"use client";

import { useEffect, useRef, useState } from "react";
import Link from "next/link";
import { CornerDownLeft, FlaskConical, Loader2, ShieldAlert, Sparkles, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { ChannelCombobox } from "@/components/core/channel-combobox";
import { Disclosure } from "@/components/core/disclosure";
import { EmptyState } from "@/components/core/empty-state";
import { ErrorBanner } from "@/components/core/error-banner";
import { Markdown } from "@/components/core/markdown";
import { PageHeader } from "@/components/core/page-header";
import { StatusChip } from "@/components/core/status-chip";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { useStickyFlags } from "@/hooks/use-sticky";
import { api, errorMessage, useApi, type PlaygroundReply, type Scope } from "@/lib/api";

// A turn as the page holds it: what was asked, and what came back with everything the run spent
// getting there. The reply is stored as the model wrote it, and drawn as Slack would draw it —
// the answer is the thing being judged here, and a page of `[1c9ef3b](https://…)` is not the
// answer anyone read in the channel. What the model actually typed is a click away, because the
// other half of a preview is the formatting itself: this is where you see it choose the wrong
// dialect before a channel does.
type Turn =
  | { role: "you"; text: string }
  | { role: "bot"; text: string; meta: PlaygroundReply };

function money(usd: number): string {
  if (!usd) return "$0";
  return usd < 0.01 ? `$${usd.toFixed(4)}` : `$${usd.toFixed(2)}`;
}

function ToolRow({ tool }: { tool: PlaygroundReply["tools"][number] }) {
  return (
    <div className="rounded-lg border bg-card px-3 py-2">
      <Disclosure
        label={
          <span className="flex items-center gap-2">
            <span className="font-mono text-xs">{tool.name}</span>
            <StatusChip variant={tool.ok ? "success" : "danger"}>{tool.ok ? "ok" : "failed"}</StatusChip>
          </span>
        }
        summary={`${tool.ms} ms`}
      >
        <div className="space-y-2">
          <div>
            <p className="eyebrow text-muted-foreground">Arguments</p>
            <pre className="mt-1 max-h-40 overflow-auto rounded-md bg-muted/60 p-2 text-[11px] leading-relaxed whitespace-pre-wrap break-words">
              {tool.args || "{}"}
            </pre>
          </div>
          <div>
            <p className="eyebrow text-muted-foreground">Result</p>
            <pre className="mt-1 max-h-64 overflow-auto rounded-md bg-muted/60 p-2 text-[11px] leading-relaxed whitespace-pre-wrap break-words">
              {tool.result || "(empty)"}
            </pre>
          </div>
        </div>
      </Disclosure>
    </div>
  );
}

function BotTurn({
  turn,
  raw,
  onRaw,
}: {
  turn: Extract<Turn, { role: "bot" }>;
  raw: boolean;
  onRaw: () => void;
}) {
  const m = turn.meta;
  return (
    <div className="space-y-3">
      <div className="rounded-2xl rounded-tl-sm border bg-card px-4 py-3">
        {turn.text ? (
          raw ? (
            <p className="font-mono text-xs leading-relaxed whitespace-pre-wrap break-words">{turn.text}</p>
          ) : (
            <Markdown text={turn.text} />
          )
        ) : (
          <p className="text-sm text-muted-foreground italic">
            It answered with nothing. In the channel this would have posted no message at all.
          </p>
        )}
      </div>

      {m.error && (
        <div className="flex items-start gap-2 rounded-xl border border-danger/30 bg-danger-soft px-3 py-2 text-xs">
          <ShieldAlert className="mt-0.5 size-3.5 shrink-0 text-danger" />
          <p className="min-w-0 flex-1">The turn ended early: {m.error}</p>
        </div>
      )}

      {/* What the run would have asked a person to approve. It is the one part of a preview that
          did not happen, so it is said in full rather than folded away behind a count. */}
      {m.held.length > 0 && (
        <div className="rounded-xl border border-warning/30 bg-warning-soft px-3 py-2">
          <p className="eyebrow text-warning">Held — not sent</p>
          <ul className="mt-1 space-y-1">
            {m.held.map((h, i) => (
              <li key={i} className="text-xs leading-relaxed break-words">
                {h}
              </li>
            ))}
          </ul>
        </div>
      )}

      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-muted-foreground">
        {m.model && <span className="font-mono">{m.model}</span>}
        <span>
          {m.rounds} {m.rounds === 1 ? "round" : "rounds"}
        </span>
        <span>
          {m.tokens_in.toLocaleString()} in · {m.tokens_out.toLocaleString()} out
        </span>
        <span>{money(m.cost_usd)}</span>
        {turn.text && (
          <button
            type="button"
            onClick={onRaw}
            className="underline-offset-2 hover:text-foreground hover:underline"
            aria-pressed={raw}
          >
            {raw ? "Formatted" : "Raw"}
          </button>
        )}
      </div>

      {m.tools.length > 0 && (
        <div className="space-y-1.5">
          <p className="eyebrow text-muted-foreground">
            {m.tools.length} tool {m.tools.length === 1 ? "call" : "calls"}
          </p>
          {m.tools.map((t, i) => (
            <ToolRow key={i} tool={t} />
          ))}
        </div>
      )}
    </div>
  );
}

export function PlaygroundPage() {
  const scopes = useApi<Scope[]>("/api/scopes?sync=0");
  const [channel, setChannel] = useState("");
  // The conversation on the server. Empty starts a new one; the reply hands back the key to
  // carry on in, so a follow-up question sees the same thread the first one did.
  const [thread, setThread] = useState("");
  const [turns, setTurns] = useState<Turn[]>([]);
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  // Formatted or as typed, remembered per browser: whichever one you work in, you work in it for
  // every answer on the page, so the toggle on any turn moves all of them.
  const view = useStickyFlags("playground.view");
  const raw = view.get("raw", false);
  const tail = useRef<HTMLDivElement>(null);

  const chosen = (scopes.data ?? []).find((s) => s.slack_id === channel);

  useEffect(() => {
    tail.current?.scrollIntoView({ behavior: "smooth", block: "end" });
  }, [turns, busy]);

  // A conversation belongs to one channel: the point of the page is that the answer depends on
  // where it was asked, so carrying a transcript across would make two channels look like one.
  const pick = (next: string) => {
    if (next === channel) return;
    setChannel(next);
    setThread("");
    setTurns([]);
  };

  const send = async () => {
    const ask = text.trim();
    if (!ask || !channel || busy) return;
    setText("");
    setTurns((t) => [...t, { role: "you", text: ask }]);
    setBusy(true);
    try {
      const res = await api.post<PlaygroundReply>("/api/playground", { channel, thread, text: ask });
      setThread(res.thread);
      setTurns((t) => [...t, { role: "bot", text: res.reply, meta: res }]);
    } catch (err) {
      // Nothing reached the model, so the question goes back in the box rather than sitting in
      // the transcript as something that was asked and never answered.
      setTurns((t) => t.slice(0, -1));
      setText(ask);
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const clear = async () => {
    const had = thread;
    setThread("");
    setTurns([]);
    if (!had) return;
    try {
      await api.post("/api/playground/reset", { channel, thread: had });
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  return (
    <div className="space-y-5">
      <PageHeader
        title="Playground"
        description="Ask a channel something from here. It answers with that channel's connections, instructions, memory and model — without anyone in Slack seeing it."
        actions={
          turns.length > 0 && (
            <Button variant="outline" onClick={clear} disabled={busy}>
              <Trash2 className="size-4" />
              Clear
            </Button>
          )
        }
      />

      {scopes.error && !scopes.data && <ErrorBanner message={scopes.error} onRetry={scopes.reload} />}

      <div className="flex flex-wrap items-center gap-3">
        <div className="min-w-0 flex-1 sm:max-w-sm">
          <ChannelCombobox value={channel} onChange={pick} aria-label="Channel to try" />
        </div>
        {chosen && (
          <Link
            href={`/workspaces?scope=${chosen.id}`}
            className="text-sm text-muted-foreground underline-offset-4 hover:text-foreground hover:underline"
          >
            Settings for {chosen.name}
          </Link>
        )}
      </div>

      {!channel ? (
        <EmptyState
          icon={FlaskConical}
          title="Pick a channel"
          description="Every channel answers differently — its own credentials, its own instructions, its own model. Choose one and ask it something."
        />
      ) : (
        <>
          <div className="space-y-5">
            {turns.length === 0 && !busy && (
              <div className="rounded-xl border border-dashed bg-muted/40 px-4 py-5 text-sm text-muted-foreground">
                <p className="flex items-center gap-2 font-medium text-foreground">
                  <Sparkles className="size-4" />
                  This is the real channel, with two exceptions
                </p>
                <p className="mt-1.5 leading-relaxed">
                  Reads run for real, against the same credentials the channel uses. Anything that would
                  change data stops at the gate and is listed as held instead of being sent, and nothing is
                  written to the channel&rsquo;s memory. Tool calls still appear in Activity, because they
                  really ran.
                </p>
              </div>
            )}

            {turns.map((t, i) =>
              t.role === "you" ? (
                <div key={i} className="flex justify-end">
                  <p className="max-w-[36rem] rounded-2xl rounded-br-sm bg-secondary px-4 py-2.5 text-sm leading-relaxed whitespace-pre-wrap break-words">
                    {t.text}
                  </p>
                </div>
              ) : (
                <BotTurn key={i} turn={t} raw={raw} onRaw={() => view.set("raw", !raw)} />
              ),
            )}

            {busy && (
              <p className="flex items-center gap-2 text-sm text-muted-foreground">
                <Loader2 className="size-4 animate-spin" />
                Working in {chosen?.name ?? channel}…
              </p>
            )}
            <div ref={tail} />
          </div>

          <div className="sticky bottom-0 space-y-1.5 bg-background pt-2 pb-4">
            <Textarea
              value={text}
              onChange={(e) => setText(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && !e.shiftKey) {
                  e.preventDefault();
                  send();
                }
              }}
              rows={3}
              disabled={busy}
              placeholder={`Ask ${chosen?.name ?? "this channel"} something…`}
              aria-label="Message"
            />
            <div className="flex items-center justify-between gap-3">
              <p className="text-xs text-muted-foreground">
                <CornerDownLeft className="mr-1 inline size-3" />
                Enter to send, Shift+Enter for a new line. It runs as you, so connections that belong to
                one person use yours.
              </p>
              <Button onClick={send} disabled={busy || !text.trim()}>
                {busy && <Loader2 className="size-4 animate-spin" />}
                Send
              </Button>
            </div>
          </div>
        </>
      )}
    </div>
  );
}
