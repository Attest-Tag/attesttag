"use client";

import { useState } from "react";
import { ChevronRight, Loader2, ShieldCheck, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { useConfirm } from "@/components/core/confirm-dialog";
import { CopyButton } from "@/components/core/copy-button";
import { ErrorBanner } from "@/components/core/error-banner";
import {
  SettingsActions,
  SettingsEditRow,
  SettingsRow,
  SettingsRows,
  SettingsSection,
} from "@/components/core/settings-section";
import { StatusChip } from "@/components/core/status-chip";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { api, errorMessage, useApi, type SSOView } from "@/lib/api";
import { cn } from "@/lib/utils";

// Settings → Security → Single sign-on.
//
// Registering and verifying are two steps on purpose, and the screen leads with which one you
// are on rather than burying it. A provider claims an email domain and sign-in routes on that
// claim, so until the organisation has published a DNS TXT record proving it controls the
// domain, the server refuses every sign-in through it — and a registered-but-unverified
// provider otherwise looks finished when it is inert.
export function SSOPanel() {
  const sso = useApi<SSOView>("/api/settings/sso");

  return (
    <SettingsSection
      title="Single sign-on"
      description="Let your organisation's identity provider decide who gets in."
    >
      {sso.error && !sso.data ? (
        <ErrorBanner message={sso.error} onRetry={sso.reload} />
      ) : sso.loading && !sso.data ? (
        <Skeleton className="h-8 w-full max-w-sm" />
      ) : sso.data?.provider ? (
        <Registered view={sso.data} onChanged={sso.reload} />
      ) : (
        <Setup onRegistered={sso.reload} />
      )}
    </SettingsSection>
  );
}

// The same resting shape as the password row above it: what is set, and the one control that
// changes it. An organisation registers a provider once and then only ever reads this screen,
// so five inputs — one of them a secret — is the wrong thing to meet them with on every visit.
function Setup({ onRegistered }: { onRegistered: () => void }) {
  const [open, setOpen] = useState(false);

  if (!open) {
    return (
      <SettingsRows
        action={
          <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
            <ShieldCheck className="size-4" />
            Set up
          </Button>
        }
      >
        <SettingsRow label="Provider" empty="Not set up" />
      </SettingsRows>
    );
  }
  // Mounted only while open, so abandoning the form does not leave a client secret sitting in
  // component state behind a collapsed panel.
  return <RegisterForm onClose={() => setOpen(false)} onRegistered={onRegistered} />;
}

function RegisterForm({
  onClose,
  onRegistered,
}: {
  onClose: () => void;
  onRegistered: () => void;
}) {
  const [domain, setDomain] = useState("");
  const [issuer, setIssuer] = useState("");
  const [clientId, setClientId] = useState("");
  const [clientSecret, setClientSecret] = useState("");
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setFailure(null);
    try {
      await api.post("/api/settings/sso", {
        kind: "oidc",
        domain,
        issuer,
        client_id: clientId,
        client_secret: clientSecret,
      });
      toast.success("Provider registered. Verify your domain to switch it on.");
      onRegistered();
      onClose();
    } catch (err) {
      setFailure(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <form onSubmit={submit} className="space-y-2">
      <p className="pb-1 text-xs leading-relaxed text-muted-foreground">
        Members whose email is at your domain sign in through your identity provider. People who
        are not here yet are added automatically the first time they sign in, as a viewer —
        promote them from the Users tab.
      </p>

      <SettingsEditRow label="Protocol" htmlFor="sso-kind">
        {/* One option, and it stays a select: SAML is the other half of this row on every
            competitor's screen, and an admin who runs SAML should be able to see that it is
            not on offer rather than wonder whether they missed it. */}
        <Select value="oidc">
          <SelectTrigger id="sso-kind" size="sm" className="w-56 bg-card">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="oidc">OIDC</SelectItem>
            <SelectItem value="saml" disabled>
              SAML 2.0 — not supported
            </SelectItem>
          </SelectContent>
        </Select>
      </SettingsEditRow>

      <SettingsEditRow label="Email domain" htmlFor="sso-domain">
        <Input
          id="sso-domain"
          required
          autoFocus
          placeholder="example.com"
          value={domain}
          onChange={(e) => setDomain(e.target.value)}
        />
      </SettingsEditRow>

      <SettingsEditRow label="Issuer URL" htmlFor="sso-issuer">
        <Input
          id="sso-issuer"
          required
          placeholder="https://your-org.okta.com"
          value={issuer}
          onChange={(e) => setIssuer(e.target.value)}
        />
      </SettingsEditRow>

      <SettingsEditRow label="Client ID" htmlFor="sso-client-id">
        <Input
          id="sso-client-id"
          required
          value={clientId}
          onChange={(e) => setClientId(e.target.value)}
        />
      </SettingsEditRow>

      <SettingsEditRow
        label="Client secret"
        htmlFor="sso-client-secret"
        hint="Stored encrypted. It's never shown again after this."
      >
        <Input
          id="sso-client-secret"
          type="password"
          required
          autoComplete="off"
          value={clientSecret}
          onChange={(e) => setClientSecret(e.target.value)}
        />
      </SettingsEditRow>

      {failure && <p className="text-xs text-danger">{failure}</p>}

      <SettingsActions>
        <Button type="submit" loading={busy}>
          <ShieldCheck className="size-4" />
          Register provider
        </Button>
        <Button type="button" variant="ghost" onClick={onClose}>
          Cancel
        </Button>
      </SettingsActions>
    </form>
  );
}

function Registered({ view, onChanged }: { view: SSOView; onChanged: () => void }) {
  const p = view.provider!;
  const { confirm, confirmDialog } = useConfirm();
  const [busy, setBusy] = useState(false);
  // Open while the domain is unverified, because that is exactly when somebody is sitting in
  // their IdP's console needing this; collapsed once it is live and the row is only being read.
  const [showUrls, setShowUrls] = useState(!p.domain_verified);

  const verify = async () => {
    setBusy(true);
    try {
      const out = await api.post<SSOView & { verified?: boolean; error?: string }>(
        "/api/settings/sso/verify",
      );
      if (out?.verified === false) toast.error(out.error ?? "That record is not there yet.");
      else toast.success("Domain verified — single sign-on is live.");
      onChanged();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    const ok = await confirm({
      title: "Remove single sign-on?",
      description:
        "New sign-ins through your identity provider stop working. Everyone who already " +
        "joined keeps their account and their access — this does not remove anybody from the " +
        "organisation.",
      confirmLabel: "Remove",
      destructive: true,
    });
    if (!ok) return;
    setBusy(true);
    try {
      await api.del("/api/settings/sso");
      toast.success("Single sign-on removed.");
      onChanged();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-3">
        <StatusChip variant={p.domain_verified ? "success" : "warning"}>
          {p.domain_verified ? "Active" : "Domain not verified"}
        </StatusChip>
        <span className="text-sm text-muted-foreground">OIDC · {p.domain}</span>
      </div>

      {!p.domain_verified && (
        <div className="space-y-3 rounded-md border border-border bg-muted/40 p-3">
          <p className="text-xs leading-relaxed text-muted-foreground">
            Nobody can sign in through this provider yet. Add the TXT record below to{" "}
            <strong className="font-medium text-foreground">{p.domain}</strong>&apos;s DNS to
            prove your organisation controls it, then check again — this is what stops somebody
            else claiming your domain and collecting your colleagues&apos; sign-ins.
          </p>
          <CopyRow label="Record name" value={view.record_name ?? ""} />
          <CopyRow label="Record value" value={view.record_value ?? ""} />
          <div>
            <Button type="button" size="sm" loading={busy} onClick={verify}>
              <ShieldCheck className="size-4" />
              Check DNS now
            </Button>
          </div>
        </div>
      )}

      <div className="space-y-2">
        <Button
          type="button"
          variant="ghost"
          size="sm"
          className="-ml-2 text-xs text-muted-foreground"
          aria-expanded={showUrls}
          onClick={() => setShowUrls((v) => !v)}
        >
          <ChevronRight className={cn("size-3.5 transition-transform", showUrls && "rotate-90")} />
          What your identity provider needs from us
        </Button>
        {showUrls && <CopyRow label="Redirect URI" value={view.redirect_uri ?? ""} />}
      </div>

      <SettingsRows>
        <SettingsRow label="Issuer">
          <span className="break-all">{p.issuer}</span>
        </SettingsRow>
        <SettingsRow label="Client ID">
          <span className="break-all">{p.client_id}</span>
        </SettingsRow>
      </SettingsRows>

      <div>
        <Button
          type="button"
          variant="ghost"
          loading={busy}
          className="text-muted-foreground hover:text-danger"
          onClick={remove}
        >
          {busy ? <Loader2 className="animate-spin" /> : <Trash2 className="size-4" />}
          Remove
        </Button>
      </div>
      {confirmDialog}
    </div>
  );
}

/** A read-only value with a copy button — every one of these gets pasted somewhere else. */
function CopyRow({ label, value }: { label: string; value: string }) {
  return (
    <div className="space-y-1">
      <p className="text-xs text-muted-foreground">{label}</p>
      <div className="flex items-center gap-2">
        <code className="min-w-0 flex-1 truncate rounded-sm border border-border bg-card px-2 py-1 font-mono text-xs">
          {value}
        </code>
        <CopyButton text={value} label={`Copy ${label}`} />
      </div>
    </div>
  );
}
