"use client";

import { useEffect, useRef, useState } from "react";
import { ChevronRight, ExternalLink, Loader2, LogIn, ShieldCheck } from "lucide-react";
import { toast } from "sonner";
import {
  CRED_TYPES,
  formForConnection,
  formForPreset,
  maskedCurl,
  normalizeHost,
  normalizePathPrefix,
  reachForOptions,
  SAVED_PLACEHOLDER,
  savedGroupState,
  savedSecretFields,
  savedState,
  secretComplete,
  toInput,
  type ConnectionForm,
  type SecretForm,
  type WritesMode,
} from "@/components/bundles/connection-form";
import { BundlePicker } from "@/components/bundles/bundle-picker";
import { startOAuthSignIn } from "@/components/bundles/oauth";
import { SavedMark, SecretFields } from "@/components/bundles/secret-fields";
import { TestConnectionBar } from "@/components/bundles/test-connection-bar";
import { ChipsInput, type ChipsHandle } from "@/components/core/chips-input";
import { ServiceTile } from "@/components/core/service-mark";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import {
  api,
  errorMessage,
  useApi,
  type Bundle,
  type Connection,
  type ConnectionMembers,
  type CredType,
  type Preset,
} from "@/lib/api";

export type ConnectTarget = {
  preset: Preset;
  bundle: Bundle;
  /** Present when editing an existing connection rather than creating one. */
  connection?: Connection;
};

// "Connect {Name}": the preset's recommended path (one secret, the default
// hosts) on the first tab, and every knob on the second. Custom presets open
// on Advanced because there is nothing to recommend.
export function ConnectDialog({
  target,
  bundles,
  onOpenChange,
  onSaved,
  onBundlesChanged,
}: {
  target: ConnectTarget | null;
  bundles: Bundle[];
  onOpenChange: (open: boolean) => void;
  onSaved: () => void;
  /** A bundle was made from the "+" beside the bundle picker; refetch the list. */
  onBundlesChanged?: () => void | Promise<void>;
}) {
  return (
    <Dialog open={target !== null} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-2xl">
        {target && (
          <ConnectForm
            key={`${target.preset.id}-${target.bundle.id}-${target.connection?.id ?? "new"}`}
            target={target}
            bundles={bundles}
            onOpenChange={onOpenChange}
            onSaved={onSaved}
            onBundlesChanged={onBundlesChanged}
          />
        )}
      </DialogContent>
    </Dialog>
  );
}

function ConnectForm({
  target,
  bundles,
  onOpenChange,
  onSaved,
  onBundlesChanged,
}: {
  target: ConnectTarget;
  bundles: Bundle[];
  onOpenChange: (open: boolean) => void;
  onSaved: () => void;
  onBundlesChanged?: () => void | Promise<void>;
}) {
  const { preset, bundle, connection } = target;
  const editing = !!connection;
  const isCustom = preset.id === "custom" || preset.id === "mcp";
  const packOn = (bundle.tool_packs ?? []).includes(preset.id);

  const [form, setForm] = useState<ConnectionForm>(() =>
    connection
      ? formForConnection(connection, preset, packOn)
      : formForPreset(preset, bundle.id, true),
  );
  // What this organisation actually configured. The selection is not stored as such — the hosts
  // and prefixes carry which parts are on, and the scopes carry which may write — so an existing
  // connection has to be asked. Without it the form fell back to the preset's defaults and
  // re-opening a connection to add one part quietly cleared the write box on another.
  const configured = useApi<ConnectionMembers>(
    connection && preset.cred_type === "oauth_user" && (preset.options ?? []).length > 0
      ? `/api/connections/${connection.id}/members`
      : null,
  );
  // Applied once. The admin may already be ticking boxes while this is in flight, and an answer
  // that arrives late must not undo what they just did.
  const applied = useRef(false);
  useEffect(() => {
    const opts = configured.data?.options;
    if (applied.current || !opts) return;
    applied.current = true;
    setForm((f) => ({
      ...f,
      options: Object.fromEntries(
        Object.keys(f.options).map((id) => {
          const got = opts.find((o) => o.id === id);
          return [id, { on: !!got, write: !!got?.write }];
        }),
      ),
    }));
  }, [configured.data]);

  const [tab, setTab] = useState<string>(isCustom || editing ? "advanced" : "recommended");
  const [showCurl, setShowCurl] = useState(false);
  const [busy, setBusy] = useState(false);

  const patch = (p: Partial<ConnectionForm>) => setForm((f) => ({ ...f, ...p }));
  const setSecret = (secret: ConnectionForm["secret"]) => patch({ secret });

  // Ticking a part on or off changes what the connection may reach, so the two lists on the
  // Advanced tab move with it rather than sitting at the preset's full set.
  const setOption = (id: string, next: { on?: boolean; write?: boolean }) =>
    setForm((f) => {
      const options = { ...f.options, [id]: { ...f.options[id], ...next } };
      return { ...f, options, ...reachForOptions(preset, options) };
    });

  const presetOptions = preset.options ?? [];
  const anyOptionOn = presetOptions.length === 0 || presetOptions.some((o) => form.options[o.id]?.on);

  // The three chip boxes, asked directly rather than read off the form. Clicking Connect blurs
  // whichever one has focus, and a host or a path somebody had finished typing but not pressed
  // Enter on is still a host or a path they asked for — dropping it saves a connection narrower
  // than the one on the screen, which is the version of this bug that is hard to see.
  const hostsBox = useRef<ChipsHandle>(null);
  const pathsBox = useRef<ChipsHandle>(null);
  const methodsBox = useRef<ChipsHandle>(null);
  const input = () =>
    toInput(
      {
        ...form,
        hosts: hostsBox.current?.flush() ?? form.hosts,
        pathPrefixes: pathsBox.current?.flush() ?? form.pathPrefixes,
        methods: methodsBox.current?.flush() ?? form.methods,
      },
      preset.id,
    );
  const hasSecret = secretComplete(form);
  // What is already in the vault. Editing opens every credential box blank — nothing sealed can be
  // read back — and an empty box that means "unchanged" looks exactly like one that means "gone",
  // so the boxes that stand for a stored value say so.
  const savedFields = editing && connection.has_secret ? savedSecretFields(form.credType) : [];
  const canSave =
    anyOptionOn && (editing ? form.hosts.length > 0 || form.credType === "mcp" : hasSecret);
  const testHint =
    form.credType === "mcp"
      ? "Connects to the MCP server and lists the tools it offers."
      : undefined;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    try {
      const body = input();
      if (editing) {
        await api.put(`/api/connections/${connection.id}`, body);
      } else {
        await api.post(`/api/bundles/${form.bundleId}/connections`, body);
      }
      if (preset.has_pack) {
        const packs = new Set(
          (bundles.find((b) => b.id === form.bundleId) ?? bundle).tool_packs ?? [],
        );
        const before = packs.has(preset.id);
        if (form.includePack) packs.add(preset.id);
        else packs.delete(preset.id);
        if (before !== form.includePack) {
          await api.put(`/api/bundles/${form.bundleId}`, { tool_packs: [...packs] });
        }
      }
      toast.success(editing ? "Connection saved" : "Connected");
      onOpenChange(false);
      onSaved();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const packCheckbox = preset.has_pack && (
    <label className="flex items-start gap-2.5 text-sm">
      <Checkbox
        checked={form.includePack}
        onCheckedChange={(v) => patch({ includePack: v === true })}
        className="mt-0.5"
      />
      <span>
        Include {preset.name} tool pack
        <span className="block text-xs text-muted-foreground">
          Ready-made tools for the common calls, so the model doesn&apos;t have to compose raw requests.
        </span>
      </span>
    </label>
  );

  const hostsHint = (
    <p className="text-xs text-muted-foreground">
      Wildcard only as the leftmost label, e.g. <span className="font-mono">*.example.com</span>.
    </p>
  );

  // What of the service to connect. Everyone signs in once for the whole connection, so the
  // boxes are the moment to decide what that sign-in asks them for — a part left out is a scope
  // nobody is ever asked to grant, not a rule the model is trusted to keep.
  const optionChecklist = presetOptions.length > 0 && (
    <div className="space-y-1">
      <Label>What to connect</Label>
      <div className="space-y-2.5 rounded-lg border px-3 py-2.5">
        {presetOptions.map((o) => {
          const on = !!form.options[o.id]?.on;
          return (
            <div key={o.id} className="space-y-1.5">
              <label className="flex items-start gap-2.5 text-sm">
                <Checkbox
                  checked={on}
                  onCheckedChange={(v) => setOption(o.id, { on: v === true })}
                  className="mt-0.5"
                />
                <span>
                  {o.label}
                  {o.hint && <span className="block text-xs text-muted-foreground">{o.hint}</span>}
                </span>
              </label>
              {on && o.write_scopes && (
                <label className="ml-6 flex items-start gap-2.5 text-sm">
                  <Checkbox
                    checked={!!form.options[o.id]?.write}
                    onCheckedChange={(v) => setOption(o.id, { write: v === true })}
                    className="mt-0.5"
                  />
                  <span className="text-muted-foreground">
                    May also {o.write_label || "write"}
                  </span>
                </label>
              )}
            </div>
          );
        })}
      </div>
      <p className="text-xs text-muted-foreground">
        {anyOptionOn
          ? "Only the ticked parts are asked for when people sign in, so anything left out is refused by the service itself. Changing this later asks everyone to sign in again."
          : "Tick at least one part to connect."}
      </p>
    </div>
  );

  return (
    <form onSubmit={submit} className="space-y-4">
      <DialogHeader>
        <DialogTitle>{editing ? `Edit ${connection.name}` : `Connect ${preset.name}`}</DialogTitle>
        <DialogDescription>
          {editing
            ? "Change what the bot may call and how. Leave the credential empty to keep the current one."
            : form.credType === "oauth_user"
              ? `Register an OAuth client for ${preset.name}. There is no shared account here: everyone signs in for themselves, and the bot only ever acts as whoever asked it something.`
              : `Get credentials from the ${preset.name} account the bot should use. Recommended: create a dedicated account just for it, or use a service key.`}
        </DialogDescription>
      </DialogHeader>

      <Tabs value={tab} onValueChange={setTab}>
        <TabsList>
          <TabsTrigger value="recommended" disabled={isCustom}>
            Recommended
          </TabsTrigger>
          <TabsTrigger value="advanced">Advanced</TabsTrigger>
        </TabsList>

        <TabsContent value="recommended" className="space-y-4">
          <div className="flex items-center gap-3 rounded-lg border bg-muted/40 px-3 py-2.5">
            <ServiceTile preset={preset.id} name={preset.name} className="bg-card text-foreground" />
            <div className="min-w-0 flex-1">
              <p className="text-sm font-medium">{preset.name}</p>
              <p className="truncate font-mono text-xs text-muted-foreground">
                {(preset.hosts ?? []).join(", ") || "No default host"}
              </p>
            </div>
            <span className="text-xs text-muted-foreground">{preset.category}</span>
          </div>

          <div className="space-y-1">
            <Label htmlFor="rec-name">Name</Label>
            <Input
              id="rec-name"
              value={form.name}
              onChange={(e) => patch({ name: e.target.value })}
              placeholder={preset.name}
            />
          </div>

          {optionChecklist}

          <div className="space-y-1">
            <div className="flex items-center gap-2">
              <Label htmlFor="rec-secret">{preset.secret_label}</Label>
              <SavedMark state={savedGroupState(savedFields, form.secret)} />
            </div>
            <RecommendedSecret
              credType={form.credType}
              form={form}
              onSecret={setSecret}
              onHeaderValues={(headerValues) => patch({ headerValues })}
              preset={preset}
              saved={savedFields}
            />
            <p className="text-xs text-muted-foreground">
              {preset.secret_hint}{" "}
              {preset.docs_url && (
                <a
                  href={preset.docs_url}
                  target="_blank"
                  rel="noreferrer"
                  className="inline-flex items-center gap-0.5 text-primary hover:underline"
                >
                  Where do I find this? <ExternalLink className="size-3" />
                </a>
              )}
            </p>
          </div>

          <div className="space-y-1">
            <Label htmlFor="rec-hosts">Allowed websites</Label>
            <ChipsInput
              id="rec-hosts"
              value={form.hosts}
              onChange={(hosts) => patch({ hosts })}
              normalize={normalizeHost}
              placeholder="api.example.com"
            />
            {hostsHint}
          </div>

          <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
            <ShieldCheck className="size-3.5" />
            {savedFields.length > 0
              ? "A credential is saved and cannot be shown again. Leave the boxes empty to keep it."
              : "Stored securely and never shown again after saving."}
          </p>

          <TestConnectionBar input={input} savedId={connection?.id} ready={hasSecret || editing} hint={testHint} />

          {packCheckbox}
        </TabsContent>

        <TabsContent value="advanced" className="space-y-4">
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-1">
              <Label htmlFor="adv-bundle">Add to bundle</Label>
              <BundlePicker
                id="adv-bundle"
                bundles={bundles}
                value={String(form.bundleId)}
                onChange={(v) => patch({ bundleId: Number(v) })}
                onCreated={() => onBundlesChanged?.()}
                disabled={editing}
              />
            </div>
            <div className="space-y-1">
              <Label htmlFor="adv-name">Name</Label>
              <Input
                id="adv-name"
                value={form.name}
                onChange={(e) => patch({ name: e.target.value })}
                placeholder={preset.name}
              />
            </div>
          </div>

          <div className="space-y-1">
            <Label htmlFor="adv-cred">Credential type</Label>
            <Select
              value={form.credType}
              onValueChange={(v) => patch({ credType: v as CredType })}
              disabled={editing}
            >
              <SelectTrigger id="adv-cred" className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {CRED_TYPES.map((c) => (
                  <SelectItem key={c.value} value={c.value}>
                    {c.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground">
              {CRED_TYPES.find((c) => c.value === form.credType)?.hint}
              {editing && " The type is fixed once a connection exists; create a new one to change it."}
            </p>
          </div>

          {optionChecklist}

          <SecretFields
            idPrefix="adv"
            credType={form.credType}
            value={form.secret}
            onChange={setSecret}
            preset={preset}
            headerValues={form.headerValues}
            onHeaderValuesChange={(headerValues) => patch({ headerValues })}
            placeholder={preset.placeholder}
            saved={savedFields}
          />

          {form.credType === "mcp" && (
            <McpOAuthSection
              form={form}
              onChange={(oauth) => patch({ oauth })}
              savedId={connection?.id}
            />
          )}

          <div className="space-y-1">
            <Label htmlFor="adv-hosts">Allowed websites</Label>
            <ChipsInput
              id="adv-hosts"
              ref={hostsBox}
              value={form.hosts}
              onChange={(hosts) => patch({ hosts })}
              normalize={normalizeHost}
              placeholder="api.example.com"
            />
            {hostsHint}
          </div>

          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-1">
              <Label htmlFor="adv-paths">Path prefixes</Label>
              <ChipsInput
                id="adv-paths"
                ref={pathsBox}
                value={form.pathPrefixes}
                onChange={(pathPrefixes) => patch({ pathPrefixes })}
                normalize={normalizePathPrefix}
                placeholder="/api/v2"
              />
              <p className="text-xs text-muted-foreground">Empty allows every path.</p>
            </div>
            <div className="space-y-1">
              <Label htmlFor="adv-methods">Methods</Label>
              <ChipsInput
                id="adv-methods"
                ref={methodsBox}
                value={form.methods}
                onChange={(methods) => patch({ methods })}
                normalize={(m) => m.toUpperCase()}
                placeholder="GET POST PUT DELETE"
              />
              <p className="text-xs text-muted-foreground">Empty allows every method.</p>
            </div>
          </div>

          <div className="space-y-1">
            <Label htmlFor="adv-writes">Writes</Label>
            <Select value={form.writes} onValueChange={(v) => patch({ writes: v as WritesMode })}>
              <SelectTrigger id="adv-writes" className="w-full sm:w-64">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="confirm">Confirm in Slack</SelectItem>
                <SelectItem value="auto">Automatic</SelectItem>
                <SelectItem value="all">Confirm everything, reads included</SelectItem>
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground">
              A write waits for someone in the thread to press Confirm. Reads go straight through,
              including a POST that only looks, such as a search or a query.
              {form.writes === "auto" && " Automatic skips that wait — only for services where a write is safe to repeat."}
              {form.writes === "all" && " Every call is held, including reads — for a service where even looking is sensitive."}
            </p>
          </div>

          <div className="space-y-2">
            <div className="flex items-start gap-2">
              <Checkbox
                id="adv-grants"
                checked={!!form.allow_grants}
                onCheckedChange={(v) => patch({ allow_grants: v === true })}
              />
              <Label htmlFor="adv-grants" className="font-normal leading-snug">
                Allow access grants
              </Label>
            </div>
            <p className="text-xs text-muted-foreground">
              Writes through this connection stop being the requester&apos;s to confirm: they go to a
              named approver as a direct message, and no allow rule can pre-approve them. Use it for
              credentials that hand out access. Needs explicit methods and path prefixes above, and
              is refused together with Writes = Automatic. An approval role on the Approvers page must
              list this connection too — in one of its bundles, or picked on its own — before
              anything can run.
            </p>
          </div>

          <div className="space-y-1">
            <Label htmlFor="adv-notes">Notes</Label>
            <Textarea
              id="adv-notes"
              rows={3}
              value={form.notes}
              onChange={(e) => patch({ notes: e.target.value })}
              placeholder={preset.notes || "Which endpoints matter, how ids look, anything the model should know."}
            />
            <p className="text-xs text-muted-foreground">Usage notes the model reads.</p>
          </div>

          <div className="rounded-lg border">
            <button
              type="button"
              onClick={() => setShowCurl((v) => !v)}
              className="flex w-full items-center gap-2 px-3 py-2 text-left text-sm font-medium hover:bg-secondary/60"
            >
              <ChevronRight className={`size-4 text-muted-foreground transition-transform ${showCurl ? "rotate-90" : ""}`} />
              See resolved curl example
            </button>
            {showCurl && (
              <pre className="overflow-x-auto border-t bg-muted/40 px-3 py-2 font-mono text-[11px] leading-snug">
                {maskedCurl(form, preset)}
              </pre>
            )}
          </div>

          <TestConnectionBar input={input} savedId={connection?.id} ready={hasSecret || editing} hint={testHint} />

          {packCheckbox}
        </TabsContent>
      </Tabs>

      <DialogFooter>
        <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
          Cancel
        </Button>
        <Button type="submit" disabled={busy || !canSave}>
          {busy && <Loader2 className="animate-spin" />}
          {editing ? "Save" : "Connect"}
        </Button>
      </DialogFooter>
    </form>
  );
}

// The Recommended tab's single credential control. Most presets take one
// token; basic auth takes two fields; a service account takes the JSON key;
// Datadog-style presets add one input per extra header.
function RecommendedSecret({
  credType,
  form,
  onSecret,
  onHeaderValues,
  preset,
  saved,
}: {
  credType: CredType;
  form: ConnectionForm;
  onSecret: (s: ConnectionForm["secret"]) => void;
  onHeaderValues: (v: Record<string, string>) => void;
  preset: Preset;
  /** Fields this connection already holds a stored value for; empty when connecting something new. */
  saved: readonly (keyof SecretForm)[];
}) {
  const s = form.secret;
  const set = (patch: Partial<ConnectionForm["secret"]>) => onSecret({ ...s, ...patch });
  const extra = (preset.extra_headers ?? []).filter((h) => h.name);
  // A stored field says so where its example would have gone; the pill beside the label carries
  // the rest of the story.
  const ph = (field: keyof SecretForm, fallback?: string) =>
    savedState(saved, s, field) === "saved" ? SAVED_PLACEHOLDER : fallback;

  let main: React.ReactNode;
  if (credType === "basic") {
    main = (
      <div className="grid gap-2 sm:grid-cols-2">
        <Input
          id="rec-secret"
          autoComplete="off"
          value={s.user}
          onChange={(e) => set({ user: e.target.value })}
          placeholder={ph("user", "Email or user")}
        />
        <Input
          type="password"
          autoComplete="off"
          value={s.password}
          onChange={(e) => set({ password: e.target.value })}
          placeholder={ph("password", "API token")}
          className="font-mono"
          aria-label="Password or API token"
        />
      </div>
    );
  } else if (credType === "gcp_sa") {
    main = (
      <Textarea
        id="rec-secret"
        rows={5}
        value={s.saJson}
        onChange={(e) => set({ saJson: e.target.value })}
        placeholder={ph("saJson", preset.placeholder)}
        className="max-h-48 font-mono text-xs"
        spellCheck={false}
      />
    );
  } else if (credType === "oauth2_cc") {
    main = (
      <div className="grid gap-2 sm:grid-cols-2">
        <Input id="rec-secret" autoComplete="off" value={s.clientId} onChange={(e) => set({ clientId: e.target.value })} placeholder={ph("clientId", "Client id")} className="font-mono" />
        <Input type="password" autoComplete="off" value={s.clientSecret} onChange={(e) => set({ clientSecret: e.target.value })} placeholder={ph("clientSecret", "Client secret")} className="font-mono" aria-label="Client secret" />
        <Input value={s.tokenUrl} onChange={(e) => set({ tokenUrl: e.target.value })} placeholder={ph("tokenUrl", "Token URL")} className="font-mono sm:col-span-2" aria-label="Token URL" />
      </div>
    );
  } else if (credType === "aws_sigv4") {
    main = (
      <div className="grid gap-2 sm:grid-cols-2">
        <Input id="rec-secret" autoComplete="off" value={s.awsKeyId} onChange={(e) => set({ awsKeyId: e.target.value })} placeholder={ph("awsKeyId", preset.placeholder || "Access key id")} className="font-mono" aria-label="Access key id" />
        <Input type="password" autoComplete="off" value={s.awsSecret} onChange={(e) => set({ awsSecret: e.target.value })} placeholder={ph("awsSecret", "Secret access key")} className="font-mono" aria-label="Secret access key" />
        <Input value={s.awsRegion} onChange={(e) => set({ awsRegion: e.target.value })} placeholder="Default region (us-east-1)" className="font-mono sm:col-span-2" aria-label="Default region" />
      </div>
    );
  } else if (credType === "oauth_user") {
    main = (
      <div className="space-y-2">
        <div className="grid gap-2 sm:grid-cols-2">
          <Input id="rec-secret" autoComplete="off" value={s.clientId} onChange={(e) => set({ clientId: e.target.value })} placeholder={ph("clientId", preset.placeholder || "Client id")} className="font-mono" aria-label="Client id" />
          <Input type="password" autoComplete="off" value={s.clientSecret} onChange={(e) => set({ clientSecret: e.target.value })} placeholder={ph("clientSecret", "Client secret")} className="font-mono" aria-label="Client secret" />
        </div>
        <RedirectURI />
      </div>
    );
  } else if (credType === "mcp") {
    main = (
      <div className="space-y-2">
        <div className="grid gap-2 sm:grid-cols-2">
          <Input id="rec-secret" value={s.mcpUrl} onChange={(e) => set({ mcpUrl: e.target.value })} placeholder={ph("mcpUrl", "https://mcp.example.com/mcp")} className="font-mono" />
          <Input type="password" autoComplete="off" value={s.token} onChange={(e) => set({ token: e.target.value })} placeholder="Bearer token (optional)" className="font-mono" aria-label="Bearer token" />
        </div>
        <p className="text-xs text-muted-foreground">{MCP_OAUTH_NOTE}</p>
      </div>
    );
  } else {
    main = (
      <Input
        id="rec-secret"
        type="password"
        autoComplete="off"
        value={s.token}
        onChange={(e) => set({ token: e.target.value })}
        placeholder={ph("token", preset.placeholder || undefined)}
        className="font-mono"
      />
    );
  }

  return (
    <div className="space-y-2">
      {main}
      {extra.map((h) => (
        <div key={h.name} className="space-y-1">
          <Label htmlFor={`rec-extra-${h.name}`}>{h.name}</Label>
          <Input
            id={`rec-extra-${h.name}`}
            type="password"
            autoComplete="off"
            value={form.headerValues[h.name] ?? ""}
            onChange={(e) => onHeaderValues({ ...form.headerValues, [h.name]: e.target.value })}
            className="font-mono"
          />
        </div>
      ))}
    </div>
  );
}

// The redirect URI the admin has to register with the provider. It is the console's own origin,
// which is the origin the server learns and later builds the callback from — so what is shown
// here is what the callback will actually be, not a guess at it.
function RedirectURI() {
  const [origin, setOrigin] = useState("");
  useEffect(() => setOrigin(window.location.origin), []);
  const uri = origin ? `${origin}/connect/callback` : "";
  return (
    <div className="space-y-1 rounded-md border bg-muted/40 p-2">
      <Label htmlFor="redirect-uri" className="text-xs">
        Authorised redirect URI &mdash; add this to the OAuth client, exactly
      </Label>
      <div className="flex gap-2">
        <Input id="redirect-uri" readOnly value={uri} className="font-mono text-xs" onFocus={(e) => e.currentTarget.select()} />
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={!uri}
          onClick={() => {
            navigator.clipboard?.writeText(uri);
            toast.success("Redirect URI copied");
          }}
        >
          Copy
        </Button>
      </div>
    </div>
  );
}

const MCP_OAUTH_NOTE =
  "Servers that use OAuth (for example mcp.mongodb.com): save first, then use Sign in from the row menu. A bearer token is optional.";

// Under the MCP server URL: how OAuth servers are handled, plus the client
// details a server without dynamic registration needs. Those ride with the
// sign-in call only, so they are never stored on the connection itself.
function McpOAuthSection({
  form,
  onChange,
  savedId,
}: {
  form: ConnectionForm;
  onChange: (next: ConnectionForm["oauth"]) => void;
  savedId?: number;
}) {
  const o = form.oauth;
  const set = (patch: Partial<ConnectionForm["oauth"]>) => onChange({ ...o, ...patch });
  const [busy, setBusy] = useState(false);

  const signIn = async () => {
    if (!savedId) return;
    setBusy(true);
    const left = await startOAuthSignIn(savedId, {
      client_id: o.clientId,
      client_secret: o.clientSecret,
      scopes: o.scopes,
    });
    if (!left) setBusy(false);
  };

  return (
    <div className="space-y-3 rounded-lg border bg-muted/40 px-3 py-3">
      <p className="text-xs text-muted-foreground">{MCP_OAUTH_NOTE}</p>
      <div className="grid gap-3 sm:grid-cols-2">
        <div className="space-y-1">
          <Label htmlFor="adv-oauth-client-id">Client ID</Label>
          <Input
            id="adv-oauth-client-id"
            autoComplete="off"
            value={o.clientId}
            onChange={(e) => set({ clientId: e.target.value })}
            className="font-mono"
            placeholder="Optional"
          />
        </div>
        <div className="space-y-1">
          <Label htmlFor="adv-oauth-client-secret">Client secret</Label>
          <Input
            id="adv-oauth-client-secret"
            type="password"
            autoComplete="off"
            value={o.clientSecret}
            onChange={(e) => set({ clientSecret: e.target.value })}
            className="font-mono"
            placeholder="Optional"
          />
        </div>
      </div>
      <div className="space-y-1">
        <Label htmlFor="adv-oauth-scopes">Scopes</Label>
        <Input
          id="adv-oauth-scopes"
          value={o.scopes}
          onChange={(e) => set({ scopes: e.target.value })}
          className="font-mono"
          placeholder="Optional, space-separated"
        />
        <p className="text-xs text-muted-foreground">
          Only needed when the server has no dynamic client registration. Sent with the sign-in, not saved
          with the connection.
        </p>
      </div>
      <div className="flex items-center gap-2">
        <Button type="button" variant="outline" size="sm" disabled={!savedId || busy} onClick={signIn}>
          {busy ? <Loader2 className="animate-spin" /> : <LogIn className="size-4" />}
          Sign in (OAuth)
        </Button>
        {!savedId && (
          <span className="text-xs text-muted-foreground">Available once the connection is saved.</span>
        )}
      </div>
    </div>
  );
}
