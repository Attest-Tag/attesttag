"use client";

import { useId, useState } from "react";
import Link from "next/link";
import { CheckCircle2, ChevronRight, Loader2, Lock, XCircle } from "lucide-react";
import { toast } from "sonner";
import { loadModels } from "@/components/core/model-combobox";
import { RelativeTime } from "@/components/core/relative-time";
import {
  SettingsActions,
  SettingsEditRow,
  SettingsSection,
} from "@/components/core/settings-section";
import { StatusChip } from "@/components/core/status-chip";
import { useAuth } from "@/components/shell/auth-provider";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import {
  api,
  errorMessage,
  useApi,
  type ModelInfo,
  type ModelKeyRef,
  type ModelKeyTest,
  type ModelKeyView,
} from "@/lib/api";
import { useStickyFlags } from "@/hooks/use-sticky";
import { formatUSD } from "@/lib/format";
import { cn } from "@/lib/utils";

/**
 * Where this organisation's model calls go, and on whose account.
 *
 * Not a setting, for the reason the web key is not one: a key the console reads back is a key in
 * a browser cache and a screenshot. It goes in through its own endpoint and comes back as a host,
 * the last four characters and a status.
 *
 * Three things the server insists on shape the form. A key is tried before it is kept, so Save
 * can take a few seconds and can refuse. A new key or a new address asks for the same proof as
 * turning two-factor off, so that field appears when either is about to change. And the stored key
 * is only ever sent where it was saved for, so moving to another address — or testing one — needs
 * the key pasted again, which the form says rather than letting the server refuse it.
 */
export function ModelKeyPanel({
  billingEnabled,
  onChanged,
}: {
  billingEnabled: boolean;
  onChanged: () => void;
}) {
  const { data: view, loading, error, mutate } = useApi<ModelKeyView>("/api/settings/model-key");
  const { reload: reloadMe } = useAuth();
  // Folded or open is this browser's to remember, like every other fold in the console.
  const folds = useStickyFlags("settings:open");
  const bodyId = useId();

  if (loading) {
    return (
      <KeyHeader
        bodyId={bodyId}
        open={false}
        canOpen={false}
        accessory={<Skeleton className="h-5 w-16 rounded-full" />}
      />
    );
  }
  if (error || !view) {
    return <p className="px-1 text-xs text-danger">{error ?? "Your model key could not be read."}</p>;
  }
  // Nothing to offer and nothing stored: a deployment that does not do this says nothing about it.
  if (!view.allowed && !view.key && view.policy === "off") return null;

  // Only an organisation that may bring its own key opens this: the enterprise plan on the hosted
  // service, and anybody on a deployment of its own, where ORG_MODEL_KEYS is all — view.allowed is
  // the server's answer to both. A key already stored opens whatever the plan says now, because a
  // key the plan no longer allows has stopped the bot, and Remove is inside.
  const canOpen = view.allowed || view.key !== null;
  // Folded until somebody opens it, unless the stored key is what has stopped the bot.
  const trouble = view.key !== null && (!view.active || view.failing);
  const open = canOpen && folds.get("model-key", trouble);

  const changed = (next: ModelKeyView) => {
    mutate(next);
    // Every picker on the page reads the organisation's endpoint now, and the shell's banner
    // reads /api/me: both are out of date the moment the endpoint moves.
    void loadModels(true);
    reloadMe();
    onChanged();
  };

  return (
    <section className="space-y-2">
      <KeyHeader
        bodyId={bodyId}
        open={open}
        canOpen={canOpen}
        onToggle={() => folds.set("model-key", !open)}
        accessory={
          canOpen ? <KeyStatus view={view} /> : <StatusChip variant="neutral">Enterprise plan</StatusChip>
        }
      />
      {open ? (
        <Card id={bodyId} className="divide-y py-0">
          <KeyForm
            // Remounted whenever what is stored changes, so the form starts from the saved values
            // rather than carrying a half-typed address across a save.
            key={view.key?.updated_at ?? "none"}
            view={view}
            billingEnabled={billingEnabled}
            onChanged={changed}
          />
        </Card>
      ) : (
        <p className="px-1 text-xs text-muted-foreground">
          {view.key ? (
            <>
              <span className="font-mono">{hostOf(view.key.base_url)}</span> · key{" "}
              <span className="font-mono">{view.key.key_hint}</span>
            </>
          ) : canOpen ? (
            "Run the bot on your own OpenAI, OpenRouter or OpenAI-compatible account instead of the models included here."
          ) : (
            "Run the bot on your own OpenAI, OpenRouter or OpenAI-compatible account. Part of the Enterprise plan: your conversations and documents go only to the provider you choose."
          )}
        </p>
      )}
    </section>
  );
}

/**
 * The group's title as the button that folds it — a heading holding a button, the accordion
 * pattern — or, where the plan does not include a key of one's own, the same title behind a lock.
 */
function KeyHeader({
  bodyId,
  open,
  canOpen,
  onToggle,
  accessory,
}: {
  bodyId: string;
  open: boolean;
  canOpen: boolean;
  onToggle?: () => void;
  accessory: React.ReactNode;
}) {
  return (
    <div className="flex items-center justify-between gap-3 px-1">
      <h2 className="text-sm font-semibold text-foreground">
        <button
          type="button"
          onClick={onToggle}
          disabled={!canOpen}
          aria-expanded={open}
          aria-controls={open ? bodyId : undefined}
          className="flex items-center gap-1.5 rounded-md text-left disabled:cursor-default"
        >
          {canOpen ? (
            <ChevronRight
              className={cn("size-4 shrink-0 text-muted-foreground transition-transform", open && "rotate-90")}
            />
          ) : (
            <Lock className="size-3.5 shrink-0 text-muted-foreground" />
          )}
          Your model key
        </button>
      </h2>
      {accessory}
    </div>
  );
}

function KeyStatus({ view }: { view: ModelKeyView }) {
  if (!view.key) return <StatusChip variant="neutral">Not set</StatusChip>;
  if (!view.active) return <StatusChip variant="warning">Not in use</StatusChip>;
  if (view.failing) return <StatusChip variant="danger">Failing</StatusChip>;
  return <StatusChip variant="success">In use</StatusChip>;
}

function hostOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

function KeyForm({
  view,
  billingEnabled,
  onChanged,
}: {
  view: ModelKeyView;
  billingEnabled: boolean;
  onChanged: (next: ModelKeyView) => void;
}) {
  const stored: ModelKeyRef | null = view.key;
  const presets = view.presets;
  const [preset, setPreset] = useState(stored?.preset || "openai");
  const [baseURL, setBaseURL] = useState(
    stored?.base_url ?? presets.find((p) => p.id === "openai")?.base_url ?? "",
  );
  const [key, setKey] = useState("");
  const [model, setModel] = useState(stored?.default_model ?? "");
  const [embed, setEmbed] = useState(stored?.embed_model ?? "");
  const [fixJobs, setFixJobs] = useState(stored?.fix_jobs ?? false);
  const [proof, setProof] = useState("");
  const [models, setModels] = useState<ModelInfo[]>([]);
  const [busy, setBusy] = useState<"save" | "test" | "remove" | null>(null);
  const [result, setResult] = useState<{ ok: boolean; detail: string } | null>(null);
  // Removing moves every conversation too — to the included models — so it takes the same proof a
  // new key does, asked for inline because a confirm dialog has nowhere to type it.
  const [removing, setRemoving] = useState(false);
  const [removeProof, setRemoveProof] = useState("");

  const current = presets.find((p) => p.id === preset);
  const address = baseURL.trim().replace(/\/+$/, "");
  const moved = !stored || key.trim() !== "" || address !== stored.base_url;
  // The server sends the stored key only where it was saved for, so a new address needs it again.
  const needsKey = !stored || address !== stored.base_url;
  const needsProof = moved && view.proof !== "recent";
  const dirty =
    moved ||
    model.trim() !== (stored?.default_model ?? "") ||
    embed.trim() !== (stored?.embed_model ?? "") ||
    fixJobs !== (stored?.fix_jobs ?? false);
  const ready =
    dirty &&
    address !== "" &&
    model.trim() !== "" &&
    (!needsKey || key.trim() !== "") &&
    (!needsProof || proof !== "");
  const chat = models.filter((m) => m.kind !== "embedding");
  const embedding = models.filter((m) => m.kind === "embedding");

  const pickPreset = (id: string) => {
    setPreset(id);
    const next = presets.find((p) => p.id === id);
    // A preset with an address fills it in; "another endpoint" leaves whatever is there to edit.
    if (next?.base_url) setBaseURL(next.base_url);
    else if (presets.some((p) => p.base_url && p.base_url === address)) setBaseURL("");
    setModels([]);
    setResult(null);
  };

  const test = async () => {
    setBusy("test");
    setResult(null);
    try {
      const res = await api.post<ModelKeyTest>("/api/settings/model-key/test", {
        preset,
        base_url: address,
        key: key.trim(),
      });
      setResult({ ok: res.ok, detail: res.detail });
      setModels(res.models ?? []);
    } catch (err) {
      setResult({ ok: false, detail: errorMessage(err) });
    } finally {
      setBusy(null);
    }
  };

  const save = async (e?: React.FormEvent) => {
    e?.preventDefault();
    if (!ready || busy) return;
    setBusy("save");
    setResult(null);
    try {
      const next = await api.put<ModelKeyView>("/api/settings/model-key", {
        preset,
        base_url: address,
        key: key.trim(),
        default_model: model.trim(),
        embed_model: embed.trim(),
        fix_jobs: fixJobs,
        password: view.proof === "password" ? proof : "",
        code: view.proof === "code" ? proof : "",
      });
      toast.success(`The bot now answers on ${hostOf(address)}`);
      onChanged(next);
    } catch (err) {
      // A refusal from the endpoint is the useful answer here — the key, the address or the
      // model — so it is shown where the form is rather than in a toast that goes away.
      setResult({ ok: false, detail: errorMessage(err) });
    } finally {
      setBusy(null);
    }
  };

  const remove = async () => {
    setBusy("remove");
    try {
      const next = await api.send<ModelKeyView>("DELETE", "/api/settings/model-key", {
        password: view.proof === "password" ? removeProof : "",
        code: view.proof === "code" ? removeProof : "",
      });
      toast.success("Your model key was removed");
      onChanged(next);
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };

  const removeNeedsProof = view.proof !== "recent";
  const removeButton = stored && !removing && (
    <Button
      type="button"
      size="sm"
      variant="ghost"
      onClick={() => setRemoving(true)}
      disabled={busy !== null}
      className="text-danger hover:text-danger"
    >
      Remove
    </Button>
  );
  const removeConfirm = removing && (
    <div className="space-y-2 border-t px-6 py-5">
      <p className="text-sm">
        Remove your model key? The bot goes back to the models included here, its spend is drawn
        from AI credit again, and documents are indexed again for the included embedding model.
      </p>
      {removeNeedsProof && (
        <SettingsEditRow
          label={view.proof === "password" ? "Your password" : "Your code"}
          htmlFor="model_key_remove_proof"
        >
          <Input
            id="model_key_remove_proof"
            type={view.proof === "password" ? "password" : "text"}
            inputMode={view.proof === "password" ? undefined : "numeric"}
            autoComplete={view.proof === "password" ? "current-password" : "one-time-code"}
            value={removeProof}
            onChange={(e) => setRemoveProof(e.target.value)}
            className={view.proof === "password" ? "max-w-sm" : "max-w-32 font-mono tabular-nums"}
          />
        </SettingsEditRow>
      )}
      <SettingsActions>
        <Button
          type="button"
          size="sm"
          variant="destructive"
          onClick={remove}
          disabled={busy !== null || (removeNeedsProof && removeProof === "")}
        >
          {busy === "remove" && <Loader2 className="animate-spin" />}
          Remove key
        </Button>
        <Button
          type="button"
          size="sm"
          variant="ghost"
          disabled={busy !== null}
          onClick={() => {
            setRemoving(false);
            setRemoveProof("");
          }}
        >
          Cancel
        </Button>
      </SettingsActions>
    </div>
  );

  return (
    <form onSubmit={save}>
      <SettingsSection title="Where the models run" description="Your own provider account, instead of the models included here.">
        <div className="space-y-2">
          <p className="text-sm text-muted-foreground">
            Every model call this organisation makes — replies, document search and fix jobs — goes
            to this endpoint, on this key. Nothing falls back to the included models: if the key
            stops working, the bot says so.
          </p>
          {stored && (
            <p className="text-xs text-muted-foreground">
              <span className="font-mono">{hostOf(stored.base_url)}</span> · key{" "}
              <span className="font-mono">{stored.key_hint}</span>
              {stored.updated_by && <> · saved by {stored.updated_by}</>}{" "}
              <RelativeTime value={stored.updated_at} />
            </p>
          )}
          {view.refusal && <p className="text-xs text-danger">{view.refusal}</p>}
          {view.active && view.failing && stored?.last_error && (
            <p className="text-xs text-danger">
              Failing since <RelativeTime value={stored.last_error_at} />: {stored.last_error}
            </p>
          )}
          {!view.allowed && stored && (
            <div className="flex flex-wrap items-center gap-2">
              <p className="text-xs text-muted-foreground">
                Your plan no longer includes your own key. Remove it to use the included models.
              </p>
              {removeButton}
            </div>
          )}
        </div>
      </SettingsSection>

      {view.allowed && (
        <div className="space-y-2 border-t px-6 py-5">
          <SettingsEditRow label="Provider" htmlFor="model_key_preset" hint={current?.hint}>
            <Select value={preset} onValueChange={pickPreset}>
              <SelectTrigger id="model_key_preset" className="w-full max-w-sm">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {presets.map((p) => (
                  <SelectItem key={p.id} value={p.id}>
                    {p.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </SettingsEditRow>
          <SettingsEditRow label="Base URL" htmlFor="model_key_base_url">
            <Input
              id="model_key_base_url"
              className="w-full max-w-sm font-mono text-xs"
              spellCheck={false}
              autoComplete="off"
              placeholder="https://…/v1"
              value={baseURL}
              onChange={(e) => {
                setBaseURL(e.target.value);
                setModels([]);
              }}
            />
          </SettingsEditRow>
          <SettingsEditRow
            label="API key"
            htmlFor="model_key_key"
            hint={
              stored && needsKey
                ? "Paste the key again to use it at this address — a saved key is only ever sent where it was saved for."
                : undefined
            }
          >
            <Input
              id="model_key_key"
              type="password"
              autoComplete="off"
              spellCheck={false}
              className="w-full max-w-sm"
              placeholder={stored && !needsKey ? "•••••••• saved — paste a new key to replace it" : "Paste the key"}
              value={key}
              onChange={(e) => setKey(e.target.value)}
            />
          </SettingsEditRow>
          <SettingsActions>
            <Button
              type="button"
              size="sm"
              variant="outline"
              onClick={test}
              disabled={busy !== null || address === "" || (needsKey && key.trim() === "")}
            >
              {busy === "test" && <Loader2 className="animate-spin" />}
              Test
            </Button>
            {result && (
              <p
                role="status"
                className={`flex items-start gap-1.5 self-center text-xs ${result.ok ? "text-success-text" : "text-danger"}`}
              >
                {result.ok ? (
                  <CheckCircle2 className="mt-px size-3.5 shrink-0" />
                ) : (
                  <XCircle className="mt-px size-3.5 shrink-0" />
                )}
                {result.detail}
              </p>
            )}
          </SettingsActions>

          <SettingsEditRow
            label="Default model"
            htmlFor="model_key_model"
            hint={chat.length > 0 ? undefined : "Test the key to list its models, or type the model id (an Azure deployment name, for Azure)."}
          >
            <Input
              id="model_key_model"
              list="model_key_chat_models"
              className="w-full max-w-sm font-mono text-xs"
              spellCheck={false}
              autoComplete="off"
              placeholder="gpt-5-mini"
              value={model}
              onChange={(e) => setModel(e.target.value)}
            />
            <datalist id="model_key_chat_models">
              {chat.map((m) => (
                <option key={m.id} value={m.id}>
                  {m.name}
                </option>
              ))}
            </datalist>
          </SettingsEditRow>
          <SettingsEditRow
            label="Embedding model"
            htmlFor="model_key_embed"
            hint="For document search. Empty turns document search off while this key is in use; changing it indexes your documents again."
          >
            <Input
              id="model_key_embed"
              list="model_key_embed_models"
              className="w-full max-w-sm font-mono text-xs"
              spellCheck={false}
              autoComplete="off"
              placeholder="text-embedding-3-small"
              value={embed}
              onChange={(e) => setEmbed(e.target.value)}
            />
            <datalist id="model_key_embed_models">
              {embedding.map((m) => (
                <option key={m.id} value={m.id}>
                  {m.name}
                </option>
              ))}
            </datalist>
          </SettingsEditRow>
          <SettingsEditRow
            label="Fix jobs"
            htmlFor="model_key_fix_jobs"
            hint="A fix job runs the repository's own code with this key in its environment. Off means fix jobs are refused while this key is in use."
          >
            <label className="flex items-center gap-2.5">
              <Switch id="model_key_fix_jobs" checked={fixJobs} onCheckedChange={setFixJobs} />
              <span className="text-sm">{fixJobs ? "May use this key" : "Off"}</span>
            </label>
          </SettingsEditRow>
          {needsProof && dirty && (
            <SettingsEditRow
              label={view.proof === "password" ? "Your password" : "Your code"}
              htmlFor="model_key_proof"
              hint="A new key or address decides where every conversation is sent, so it asks who you are."
            >
              <Input
                id="model_key_proof"
                type={view.proof === "password" ? "password" : "text"}
                inputMode={view.proof === "password" ? undefined : "numeric"}
                autoComplete={view.proof === "password" ? "current-password" : "one-time-code"}
                value={proof}
                onChange={(e) => setProof(e.target.value)}
                className={view.proof === "password" ? "max-w-sm" : "max-w-32 font-mono tabular-nums"}
              />
            </SettingsEditRow>
          )}
          <SettingsActions>
            <Button type="submit" size="sm" disabled={!ready || busy !== null}>
              {busy === "save" && <Loader2 className="animate-spin" />}
              {busy === "save" ? "Checking the key…" : stored ? "Save" : "Save key"}
            </Button>
            {removeButton}
          </SettingsActions>
        </div>
      )}
      {removeConfirm}

      {view.active && (
        <SettingsSection
          title="Spend on your key"
          description="Billed by your provider, not drawn from AI credit here. Your own monthly budget is the one limit left on it."
          className="border-t"
        >
          <p className="text-sm">
            {view.monthly_budget_usd > 0 ? (
              <>Monthly budget: {formatUSD(view.monthly_budget_usd)}</>
            ) : (
              <>No monthly budget</>
            )}{" "}
            <span className="text-muted-foreground">
              —{" "}
              <Link
                href={billingEnabled ? "/settings?tab=billing" : "/settings?tab=models"}
                className="underline underline-offset-2"
              >
                change it
              </Link>
            </span>
          </p>
          <p className="mt-1 text-xs text-muted-foreground">
            {view.priced
              ? "Measured at your provider's list prices, which is an estimate: the invoice that counts is theirs."
              : "Calls on this endpoint carry no price we can read, so a monthly budget cannot stop them. Set limits with your provider."}
          </p>
        </SettingsSection>
      )}
    </form>
  );
}
