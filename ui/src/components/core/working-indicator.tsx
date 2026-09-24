"use client";

import { useEffect, useState } from "react";
import { Sparkles } from "lucide-react";

/**
 * The in-flight indicator for an answer the console is waiting on.
 *
 * Everything it shows is observed, never guessed. The console assistant answers in one request
 * rather than streaming, so there is no step to name and no draft to preview: what can honestly
 * be reported is that the request is running and for how long. It says that and stops. A label
 * is accepted for callers that do know their step, but nothing invents one to fill the line.
 *
 * The placeholder bars are not decoration — they reserve the shape the answer will take, so the
 * panel does not jump when it lands.
 */
export function WorkingIndicator({ label }: { label?: string | null } = {}) {
  // Mounts when the caller starts working and unmounts when it stops, so the counter resets per
  // turn with no external plumbing.
  const [seconds, setSeconds] = useState(0);

  useEffect(() => {
    const id = setInterval(() => setSeconds((s) => s + 1), 1000);
    return () => clearInterval(id);
  }, []);

  return (
    <div className="space-y-2">
      <div className="flex items-center gap-2">
        <span className="relative flex size-6 shrink-0 items-center justify-center rounded-full bg-ai-soft">
          <span className="absolute inset-0 animate-ping rounded-full bg-ai/15" />
          <Sparkles className="relative size-3.5 text-ai" strokeWidth={1.75} />
        </span>
        <span className="text-shimmer min-w-0 flex-1 truncate text-xs font-medium">
          {label ?? "Working…"}
        </span>
        {/* Held back a few seconds so a quick answer never flashes a timer. */}
        {seconds >= 3 && (
          <span className="text-[11px] text-muted-foreground tabular-nums">{seconds}s</span>
        )}
      </div>
      <div className="space-y-1.5 pl-8" aria-hidden>
        <span className="block h-2 w-4/5 animate-pulse rounded-full bg-secondary" />
        <span
          className="block h-2 w-3/5 animate-pulse rounded-full bg-secondary"
          style={{ animationDelay: "200ms" }}
        />
      </div>
      <span className="sr-only" role="status">
        {label ?? "The assistant is working on your request."}
      </span>
    </div>
  );
}
