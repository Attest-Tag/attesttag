"use client";

import { Lock, RefreshCw } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import type { CredType, Preset } from "@/lib/api";
import {
  SAVED_PLACEHOLDER,
  savedState,
  type SavedState,
  type SecretForm,
} from "@/components/bundles/connection-form";

// The credential inputs for one `credType`. Shared by the Connect dialog's
// Advanced tab and the rotate-secret dialog so a credential shape is described
// in exactly one place.
export function SecretFields({
  credType,
  value,
  onChange,
  preset,
  headerValues,
  onHeaderValuesChange,
  idPrefix = "secret",
  placeholder,
  saved,
}: {
  credType: CredType;
  value: SecretForm;
  onChange: (next: SecretForm) => void;
  preset?: Preset;
  headerValues?: Record<string, string>;
  onHeaderValuesChange?: (next: Record<string, string>) => void;
  idPrefix?: string;
  /** Placeholder for the main token field, usually the preset's. */
  placeholder?: string;
  /**
   * Fields this connection already holds a stored value for, from `savedSecretFields`. Empty when
   * connecting something new; empty too in the rotate dialog, where replacing is the whole point.
   */
  saved?: readonly (keyof SecretForm)[];
}) {
  const set = (patch: Partial<SecretForm>) => onChange({ ...value, ...patch });
  const id = (s: string) => `${idPrefix}-${s}`;
  const mark = (field: keyof SecretForm) => savedState(saved, value, field);
  // A stored field offers no placeholder of its own: what belongs in the box is the fact that
  // something is already in it, not an example of what one looks like.
  const ph = (field: keyof SecretForm, fallback?: string) =>
    mark(field) === "saved" ? SAVED_PLACEHOLDER : fallback;

  const extraHeaders = (preset?.extra_headers ?? []).filter((h) => h.name);

  return (
    <div className="space-y-3">
      {(saved?.length ?? 0) > 0 && (
        <p className="text-xs text-muted-foreground">
          The credential below is stored and cannot be shown again. Leave a saved field empty to keep
          it; type into one to replace it.
        </p>
      )}
      {credType === "bearer" && (
        <Field id={id("token")} label="Token" state={mark("token")}>
          <Input
            id={id("token")}
            type="password"
            autoComplete="off"
            value={value.token}
            onChange={(e) => set({ token: e.target.value })}
            placeholder={ph("token", placeholder)}
            className="font-mono"
          />
        </Field>
      )}

      {credType === "basic" && (
        <div className="grid gap-3 sm:grid-cols-2">
          <Field id={id("user")} label="User" state={mark("user")}>
            <Input
              id={id("user")}
              autoComplete="off"
              value={value.user}
              onChange={(e) => set({ user: e.target.value })}
              placeholder={ph("user")}
            />
          </Field>
          <Field id={id("password")} label="Password or token" state={mark("password")}>
            <Input
              id={id("password")}
              type="password"
              autoComplete="off"
              value={value.password}
              onChange={(e) => set({ password: e.target.value })}
              placeholder={ph("password")}
              className="font-mono"
            />
          </Field>
        </div>
      )}

      {credType === "header" && (
        <>
          <div className="grid gap-3 sm:grid-cols-2">
            <Field id={id("header-name")} label="Header name">
              <Input
                id={id("header-name")}
                value={value.headerName}
                onChange={(e) => set({ headerName: e.target.value })}
                placeholder="Authorization"
                className="font-mono"
              />
            </Field>
            <Field id={id("header-prefix")} label="Prefix" hint='Text before the token, e.g. "Bearer " or "Token token=".'>
              <Input
                id={id("header-prefix")}
                value={value.headerPrefix}
                onChange={(e) => set({ headerPrefix: e.target.value })}
                className="font-mono"
              />
            </Field>
          </div>
          <Field id={id("token")} label="Token" state={mark("token")}>
            <Input
              id={id("token")}
              type="password"
              autoComplete="off"
              value={value.token}
              onChange={(e) => set({ token: e.target.value })}
              placeholder={ph("token", placeholder)}
              className="font-mono"
            />
          </Field>
        </>
      )}

      {credType === "query" && (
        <div className="grid gap-3 sm:grid-cols-[1fr_2fr]">
          <Field id={id("param")} label="Parameter name" state={mark("headerName")}>
            <Input
              id={id("param")}
              value={value.headerName}
              onChange={(e) => set({ headerName: e.target.value })}
              placeholder={ph("headerName", "api_key")}
              className="font-mono"
            />
          </Field>
          <Field id={id("token")} label="Token" state={mark("token")}>
            <Input
              id={id("token")}
              type="password"
              autoComplete="off"
              value={value.token}
              onChange={(e) => set({ token: e.target.value })}
              placeholder={ph("token", placeholder)}
              className="font-mono"
            />
          </Field>
        </div>
      )}

      {credType === "oauth2_cc" && (
        <>
          <div className="grid gap-3 sm:grid-cols-2">
            <Field id={id("client-id")} label="Client id" state={mark("clientId")}>
              <Input
                id={id("client-id")}
                autoComplete="off"
                value={value.clientId}
                onChange={(e) => set({ clientId: e.target.value })}
                placeholder={ph("clientId")}
                className="font-mono"
              />
            </Field>
            <Field id={id("client-secret")} label="Client secret" state={mark("clientSecret")}>
              <Input
                id={id("client-secret")}
                type="password"
                autoComplete="off"
                value={value.clientSecret}
                onChange={(e) => set({ clientSecret: e.target.value })}
                placeholder={ph("clientSecret")}
                className="font-mono"
              />
            </Field>
          </div>
          <Field id={id("token-url")} label="Token URL" state={mark("tokenUrl")}>
            <Input
              id={id("token-url")}
              value={value.tokenUrl}
              onChange={(e) => set({ tokenUrl: e.target.value })}
              placeholder={ph("tokenUrl", "https://auth.example.com/oauth/token")}
              className="font-mono"
            />
          </Field>
          <Field id={id("scopes")} label="Scopes" hint="Space-separated. Leave empty if the server grants a default set.">
            <Input
              id={id("scopes")}
              value={value.scopes}
              onChange={(e) => set({ scopes: e.target.value })}
              className="font-mono"
            />
          </Field>
        </>
      )}

      {credType === "oauth_user" && (
        <>
          <p className="text-xs text-muted-foreground">
            This pair reaches nobody&rsquo;s account on its own. Everyone signs in for themselves, and the bot acts
            only as the person who asked it something.
          </p>
          <div className="grid gap-3 sm:grid-cols-2">
            <Field id={id("ou-client-id")} label="Client id" state={mark("clientId")}>
              <Input
                id={id("ou-client-id")}
                autoComplete="off"
                value={value.clientId}
                onChange={(e) => set({ clientId: e.target.value })}
                placeholder={ph("clientId", placeholder)}
                className="font-mono"
              />
            </Field>
            <Field id={id("ou-client-secret")} label="Client secret" state={mark("clientSecret")}>
              <Input
                id={id("ou-client-secret")}
                type="password"
                autoComplete="off"
                value={value.clientSecret}
                onChange={(e) => set({ clientSecret: e.target.value })}
                placeholder={ph("clientSecret")}
                className="font-mono"
              />
            </Field>
          </div>
          <Field
            id={id("ou-scopes")}
            label="Scopes"
            hint="Space-separated. Leave empty to ask for what this service normally needs."
          >
            <Input
              id={id("ou-scopes")}
              value={value.scopes}
              onChange={(e) => set({ scopes: e.target.value })}
              placeholder={preset?.scopes}
              className="font-mono"
            />
          </Field>
        </>
      )}

      {credType === "gcp_sa" && (
        <>
          <Field id={id("sa-json")} label="Service-account key (JSON)" state={mark("saJson")} hint="The whole file, as downloaded from Google Cloud. It never leaves this server.">
            <Textarea
              id={id("sa-json")}
              rows={6}
              value={value.saJson}
              onChange={(e) => set({ saJson: e.target.value })}
              placeholder={ph("saJson", placeholder || '{"type": "service_account", …}')}
              className="max-h-48 font-mono text-xs"
              spellCheck={false}
            />
          </Field>
          <Field id={id("scopes")} label="OAuth scopes" hint="Space-separated Google API scopes the tokens are minted for.">
            <Input
              id={id("scopes")}
              value={value.scopes}
              onChange={(e) => set({ scopes: e.target.value })}
              className="font-mono"
            />
          </Field>
        </>
      )}

      {credType === "aws_sigv4" && (
        <>
          <div className="grid gap-3 sm:grid-cols-2">
            <Field id={id("aws-key-id")} label="Access key id" state={mark("awsKeyId")}>
              <Input
                id={id("aws-key-id")}
                autoComplete="off"
                value={value.awsKeyId}
                onChange={(e) => set({ awsKeyId: e.target.value })}
                placeholder={ph("awsKeyId", placeholder || "AKIA…")}
                className="font-mono"
              />
            </Field>
            <Field id={id("aws-secret")} label="Secret access key" state={mark("awsSecret")}>
              <Input
                id={id("aws-secret")}
                type="password"
                autoComplete="off"
                value={value.awsSecret}
                onChange={(e) => set({ awsSecret: e.target.value })}
                placeholder={ph("awsSecret")}
                className="font-mono"
              />
            </Field>
          </div>
          <div className="grid gap-3 sm:grid-cols-2">
            <Field
              id={id("aws-region")}
              label="Default region"
              hint="Used when the host does not name one. A regional host wins over this."
            >
              <Input
                id={id("aws-region")}
                value={value.awsRegion}
                onChange={(e) => set({ awsRegion: e.target.value })}
                placeholder="us-east-1"
                className="font-mono"
              />
            </Field>
            <Field
              id={id("aws-service")}
              label="Service"
              hint="Optional. Only for an endpoint whose hostname does not name its service."
            >
              <Input
                id={id("aws-service")}
                value={value.awsService}
                onChange={(e) => set({ awsService: e.target.value })}
                placeholder="from the host"
                className="font-mono"
              />
            </Field>
          </div>
          <Field
            id={id("aws-session-token")}
            label="Session token"
            hint="Optional, and only for temporary credentials — they expire, and the connection stops working when they do."
          >
            <Input
              id={id("aws-session-token")}
              type="password"
              autoComplete="off"
              value={value.awsSessionToken}
              onChange={(e) => set({ awsSessionToken: e.target.value })}
              className="font-mono"
            />
          </Field>
        </>
      )}

      {credType === "mcp" && (
        <>
          <Field id={id("mcp-url")} label="Server URL" state={mark("mcpUrl")}>
            <Input
              id={id("mcp-url")}
              value={value.mcpUrl}
              onChange={(e) => set({ mcpUrl: e.target.value })}
              placeholder={ph("mcpUrl", "https://mcp.example.com/mcp")}
              className="font-mono"
            />
          </Field>
          <Field id={id("token")} label="Bearer token" hint="Optional. Sent as Authorization: Bearer on every request.">
            <Input
              id={id("token")}
              type="password"
              autoComplete="off"
              value={value.token}
              onChange={(e) => set({ token: e.target.value })}
              className="font-mono"
            />
          </Field>
        </>
      )}

      {extraHeaders.length > 0 && headerValues && onHeaderValuesChange && (
        <div className="grid gap-3 sm:grid-cols-2">
          {extraHeaders.map((h) => (
            <Field key={h.name} id={id(`extra-${h.name}`)} label={h.name}>
              <Input
                id={id(`extra-${h.name}`)}
                type="password"
                autoComplete="off"
                value={headerValues[h.name] ?? ""}
                onChange={(e) => onHeaderValuesChange({ ...headerValues, [h.name]: e.target.value })}
                className="font-mono"
              />
            </Field>
          ))}
        </div>
      )}
    </div>
  );
}

function Field({
  id,
  label,
  hint,
  state,
  children,
}: {
  id: string;
  label: string;
  hint?: React.ReactNode;
  state?: SavedState;
  children: React.ReactNode;
}) {
  return (
    <div className="space-y-1">
      <div className="flex items-center gap-2">
        <Label htmlFor={id}>{label}</Label>
        <SavedMark state={state} />
      </div>
      {children}
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
    </div>
  );
}

/**
 * The pill beside a label whose value is stored and never shown: "Saved" while the box is empty,
 * "Replacing" once something has been typed into it, so the difference between keeping a
 * credential and rotating one is visible before the Save button is pressed rather than after.
 */
export function SavedMark({ state }: { state?: SavedState }) {
  if (!state) return null;
  const replacing = state === "replacing";
  return (
    <Badge
      variant="secondary"
      className={`gap-1 px-1.5 py-0 font-normal ${replacing ? "text-foreground" : "text-muted-foreground"}`}
    >
      {replacing ? <RefreshCw className="size-3" /> : <Lock className="size-3" />}
      {replacing ? "Replacing" : "Saved"}
    </Badge>
  );
}
