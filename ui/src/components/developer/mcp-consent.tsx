"use client";

import { useEffect, useState, useSyncExternalStore } from "react";
import { AuthLayout, AuthNotice } from "@/components/auth/auth-layout";
import { useAuth } from "@/components/shell/auth-provider";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { api, errorMessage, useApi, type OAuthConsent } from "@/lib/api";
import { rememberReturn } from "@/lib/return-to";

function subscribeToNothing() {
  return () => {};
}

function readSearch(): string | null {
  return window.location.search;
}

/**
 * The page an MCP client sends somebody to: "this app wants to use attest_tag as you". It signs
 * them in first when it has to, and either answer — Allow or Cancel — is an address the browser is
 * sent on to, back to the app.
 *
 * The app's name is whatever it called itself when it registered, so the page leans on the address
 * it will send them back to: the one part of the request an impostor cannot make up.
 */
export function McpConsent() {
  const { me, loading } = useAuth();
  // The request is the page's query string. This is a static page, so the query only exists in
  // the browser: null while the page is built, the real one once it runs.
  const search = useSyncExternalStore(subscribeToNothing, readSearch, () => null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [leaving, setLeaving] = useState(false);
  const params = search === null ? null : new URLSearchParams(search);
  const linkError = params?.get("error")
    ? params.get("error_description") || "This is not a request this console can answer."
    : "";
  const signedIn = !!me?.signed_in && !me.two_factor?.owed;
  const consent = useApi<OAuthConsent>(
    signedIn && search && !linkError ? `/api/oauth/consent${search}` : null,
  );

  // Not signed in: sign in first, and come back here afterwards (lib/return-to.ts).
  useEffect(() => {
    if (loading || search === null || linkError || me?.signed_in) return;
    rememberReturn(window.location.pathname + window.location.search);
    window.location.replace("/admin/login/");
  }, [loading, search, linkError, me?.signed_in]);

  const answer = async (allow: boolean) => {
    setBusy(true);
    setError("");
    try {
      const out = await api.post<{ redirect: string }>("/api/oauth/consent", { query: search, allow });
      setLeaving(true);
      window.location.assign(out.redirect);
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  if (linkError) {
    return (
      <AuthLayout title="This link doesn't work" subtitle="Connecting an app">
        <AuthNotice kind="error">{linkError}</AuthNotice>
        <p className="text-sm text-muted-foreground">
          Go back to the app and connect attest_tag again. If it keeps happening, remove the
          server from the app and add it afresh.
        </p>
      </AuthLayout>
    );
  }

  if (me?.signed_in && me.two_factor?.owed) {
    return (
      <AuthLayout title="Two-factor authentication first" subtitle="Connecting an app">
        <AuthNotice kind="action">
          This organisation requires two-factor authentication. Set it up in the console, then
          connect the app again.
        </AuthNotice>
        <Button asChild className="w-full">
          <a href="/admin/">Open the console</a>
        </Button>
      </AuthLayout>
    );
  }

  const c = consent.data;
  if (!signedIn || (!c && !consent.error)) {
    return (
      <AuthLayout title="Connecting an app" subtitle="One moment">
        <Skeleton className="h-4 w-3/4" />
        <Skeleton className="h-4 w-1/2" />
        <Skeleton className="h-9 w-full" />
      </AuthLayout>
    );
  }

  if (consent.error || c?.problem) {
    const back = c?.problem?.redirect;
    return (
      <AuthLayout title="This app can't be connected" subtitle="Connecting an app">
        <AuthNotice kind="error">{c?.problem?.error || consent.error}</AuthNotice>
        {back ? (
          <Button className="w-full" onClick={() => window.location.assign(back)}>
            Back to the app
          </Button>
        ) : (
          <p className="text-sm text-muted-foreground">Go back to the app and connect attest_tag again.</p>
        )}
      </AuthLayout>
    );
  }

  const name = c?.client_name ?? "An app";
  return (
    <AuthLayout title={`Connect ${name}`} subtitle="Model Context Protocol">
      <p className="text-sm">
        <span className="font-medium">{name}</span> wants to use attest_tag as you.
      </p>
      <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 text-sm">
        <dt className="text-muted-foreground">Account</dt>
        <dd className="min-w-0 truncate">{c?.email}</dd>
        <dt className="text-muted-foreground">Organisation</dt>
        <dd className="min-w-0 truncate">{c?.org_name}</dd>
        <dt className="text-muted-foreground">Returns to</dt>
        <dd className="min-w-0 break-all font-mono text-xs leading-5">{c?.returns_to}</dd>
      </dl>
      <p className="text-xs leading-relaxed text-muted-foreground">
        It can do what your role can do through the API: read documents, memories, activity and
        spend, and change documents and memories where your role may. It keeps that until you
        disconnect it under Developer → MCP, or leave the organisation.
      </p>
      <AuthNotice kind="info">
        Only allow this if you started it from {name}, and it is sending you back to{" "}
        <span className="font-mono">{c?.returns_to}</span>.
      </AuthNotice>
      {c?.refusal && <AuthNotice kind="action">{c.refusal}</AuthNotice>}
      {error && <AuthNotice kind="error">{error}</AuthNotice>}
      {leaving ? (
        <p className="text-sm text-muted-foreground">
          Sending you back to {c?.returns_to}… You can close this tab once the app says it is
          connected.
        </p>
      ) : (
        <div className="flex gap-2">
          <Button variant="outline" className="flex-1" disabled={busy} onClick={() => answer(false)}>
            Cancel
          </Button>
          <Button className="flex-1" disabled={busy || !!c?.refusal} onClick={() => answer(true)}>
            Allow
          </Button>
        </div>
      )}
    </AuthLayout>
  );
}
