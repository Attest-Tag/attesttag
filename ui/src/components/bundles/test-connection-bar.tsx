"use client";

import { useState } from "react";
import { CheckCircle2, Loader2, XCircle, Zap } from "lucide-react";
import { Button } from "@/components/ui/button";
import { api, errorMessage, type ConnectionInput, type TestResult } from "@/lib/api";
import { cn } from "@/lib/utils";

const BODY_MAX = 400;

// "Test connection" with its result inline: the status and an excerpt of the
// body, green when the call got through and red when it didn't. Tests the
// unsaved form against POST /api/connections/test; with `savedId` and nothing
// new typed, it exercises the stored credential instead.
export function TestConnectionBar({
  input,
  savedId,
  ready,
  hint,
}: {
  input: () => ConnectionInput;
  savedId?: number;
  ready: boolean;
  hint?: string;
}) {
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<TestResult | null>(null);

  const run = async () => {
    setBusy(true);
    setResult(null);
    try {
      const body = input();
      const res =
        body.secret || !savedId
          ? await api.post<TestResult>("/api/connections/test", body)
          : await api.post<TestResult>(`/api/connections/${savedId}/test`);
      setResult(res);
    } catch (err) {
      setResult({ ok: false, error: errorMessage(err) });
    } finally {
      setBusy(false);
    }
  };

  const excerpt = result?.body
    ? result.body.length > BODY_MAX
      ? result.body.slice(0, BODY_MAX) + "…"
      : result.body
    : "";

  return (
    <div className="rounded-lg border bg-muted/40">
      <div className="flex flex-wrap items-center gap-3 px-3 py-2">
        <Button type="button" variant="outline" size="sm" onClick={run} disabled={busy || !ready}>
          {busy ? <Loader2 className="animate-spin" /> : <Zap className="size-4" />}
          Test connection
        </Button>
        {result ? (
          <span
            className={cn(
              "flex min-w-0 flex-wrap items-center gap-1.5 text-sm",
              result.ok ? "text-success-text" : "text-danger",
            )}
          >
            {result.ok ? <CheckCircle2 className="size-4" /> : <XCircle className="size-4" />}
            {result.status ? `HTTP ${result.status}` : result.ok ? "OK" : "Failed"}
            {result.error && <span className="text-xs [overflow-wrap:anywhere]">· {result.error}</span>}
          </span>
        ) : (
          <span className="text-xs text-muted-foreground">
            {hint ?? "Makes the preset's check call through the proxy with these settings."}
          </span>
        )}
      </div>
      {excerpt && (
        <pre
          className={cn(
            "max-h-32 overflow-auto border-t px-3 py-2 font-mono text-[11px] leading-snug whitespace-pre-wrap break-all",
            result?.ok ? "text-foreground" : "text-danger",
          )}
        >
          {excerpt}
        </pre>
      )}
    </div>
  );
}
