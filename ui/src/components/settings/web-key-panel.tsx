"use client";

import { useState } from "react";
import { CheckCircle2, Loader2, XCircle } from "lucide-react";
import { toast } from "sonner";
import { StatusChip } from "@/components/core/status-chip";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { api, errorMessage, type WebProviderInfo } from "@/lib/api";

/**
 * The provider key, which is not a setting.
 *
 * Everything else on this tab round-trips through the console: it is read back on load and
 * saved with the rest of the form. A key cannot, because a key the console reads back is a key
 * in a browser cache, a screenshot and whatever the next bug report attaches. So it goes in
 * through its own endpoint, comes back only as "Saved", and has its own Remove.
 *
 * It also has its own Test, and the Test runs against what is typed rather than what is saved,
 * so somebody can find out a pasted key works before it becomes the organisation's.
 *
 * One panel belongs to one provider. The caller remounts it when the choice changes, so a key
 * half-typed for Firecrawl is never sitting in the box when Cloudflare is selected — which is
 * also why the keys are stored per provider rather than one to an organisation.
 */
export function WebKeyPanel({
  provider,
  accountID,
  keySet,
  onChanged,
}: {
  provider: WebProviderInfo;
  accountID: string;
  keySet: boolean;
  onChanged: () => void;
}) {
  const [key, setKey] = useState("");
  const [busy, setBusy] = useState<"save" | "test" | "remove" | null>(null);
  const [result, setResult] = useState<{ ok: boolean; detail: string } | null>(null);

  const save = async () => {
    setBusy("save");
    try {
      await api.put("/api/settings/web-key", { provider: provider.id, key: key.trim() });
      setKey("");
      setResult(null);
      toast.success(`${provider.name} key saved`);
      onChanged();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };

  const remove = async () => {
    setBusy("remove");
    try {
      await api.del(`/api/settings/web-key?provider=${encodeURIComponent(provider.id)}`);
      setResult(null);
      toast.success(`${provider.name} key removed`);
      onChanged();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };

  const test = async () => {
    setBusy("test");
    setResult(null);
    try {
      const res = await api.post<{ ok: boolean; detail: string }>("/api/settings/web-key/test", {
        provider: provider.id,
        account_id: accountID,
        key: key.trim(),
      });
      setResult(res);
    } catch (err) {
      setResult({ ok: false, detail: errorMessage(err) });
    } finally {
      setBusy(null);
    }
  };

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        <Input
          id="web_key"
          aria-label={`${provider.name} ${provider.key_label ?? "API key"}`}
          type="password"
          autoComplete="off"
          spellCheck={false}
          className="w-full max-w-sm"
          placeholder={keySet ? "•••••••• saved — paste a new key to replace it" : provider.placeholder}
          value={key}
          onChange={(e) => setKey(e.target.value)}
          onKeyDown={(e) => {
            // Enter saves, like every other field on this page. It is its own endpoint, so the
            // form's submit would otherwise save the rest of the tab and leave the key behind.
            if (e.key === "Enter") {
              e.preventDefault();
              if (key.trim() && !busy) save();
            }
          }}
        />
        {keySet && !key && <StatusChip variant="success">Saved</StatusChip>}
      </div>

      <div className="flex flex-wrap items-center gap-2">
        <Button type="button" size="sm" onClick={save} disabled={!key.trim() || busy !== null}>
          {busy === "save" && <Loader2 className="animate-spin" />}
          {keySet ? "Replace key" : "Save key"}
        </Button>
        <Button
          type="button"
          size="sm"
          variant="outline"
          onClick={test}
          disabled={busy !== null || (!keySet && !key.trim())}
        >
          {busy === "test" && <Loader2 className="animate-spin" />}
          Test
        </Button>
        {keySet && (
          <Button
            type="button"
            size="sm"
            variant="ghost"
            onClick={remove}
            disabled={busy !== null}
            className="text-danger hover:text-danger"
          >
            {busy === "remove" && <Loader2 className="animate-spin" />}
            Remove
          </Button>
        )}
      </div>

      {result && (
        <p
          className={`flex items-start gap-1.5 text-xs ${result.ok ? "text-success-text" : "text-danger"}`}
          role="status"
        >
          {result.ok ? (
            <CheckCircle2 className="mt-px size-3.5 shrink-0" />
          ) : (
            <XCircle className="mt-px size-3.5 shrink-0" />
          )}
          {result.detail}
        </p>
      )}

      {provider.key_hint && <p className="text-xs text-muted-foreground">{provider.key_hint}</p>}
    </div>
  );
}
