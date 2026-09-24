"use client";

import { useEffect, useState } from "react";
import { CheckCircle2, Loader2 } from "lucide-react";
import { AuthLayout, AuthNotice, AuthSwitch } from "@/components/auth/auth-layout";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api, errorMessage } from "@/lib/api";

// Asking for a reset link. The answer is the same whether or not the address has an account:
// telling somebody it does not would turn this form into a way of finding out who has one.
export function ForgotForm() {
  const [email, setEmail] = useState("");
  const [busy, setBusy] = useState(false);
  const [sent, setSent] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setFailure(null);
    try {
      await api.post("/api/auth/forgot", { email });
      setSent(true);
    } catch (err) {
      setFailure(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  if (sent) {
    return (
      <AuthLayout title="Check your inbox" subtitle="If that address has an account">
        <AuthNotice kind="info">
          We have sent a link to <span className="font-medium text-foreground">{email}</span>. It
          works once and expires in an hour. Nothing arrives if the address has no account.
        </AuthNotice>
        <AuthSwitch href="/login" prompt="Remembered it?" action="Sign in" />
      </AuthLayout>
    );
  }

  return (
    <AuthLayout title="Reset your password" subtitle="We will email you a link">
      <form onSubmit={submit} className="space-y-3">
        <div className="space-y-1">
          <Label htmlFor="email">Email</Label>
          <Input
            id="email"
            type="email"
            autoComplete="username"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            required
            autoFocus
          />
        </div>
        {failure && <p className="text-xs text-danger">{failure}</p>}
        <Button type="submit" className="w-full" disabled={busy}>
          {busy && <Loader2 className="animate-spin" />}
          Send the link
        </Button>
      </form>
      <AuthSwitch href="/login" prompt="Remembered it?" action="Sign in" />
    </AuthLayout>
  );
}

// Setting a new password from a link. The token is in the URL because it arrived there; it is
// consumed on submit, and every other session on the account ends with it.
export function ResetForm() {
  const [token, setToken] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);

  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect -- one-shot URL read after mount
    setToken(new URLSearchParams(window.location.search).get("token") ?? "");
  }, []);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setFailure(null);
    try {
      await api.post("/api/auth/reset", { token, password });
      setDone(true);
    } catch (err) {
      setFailure(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  if (done) {
    return (
      <AuthLayout title="Password changed" subtitle="Every other session has been signed out">
        <Button className="w-full" asChild>
          <a href="/admin/login/">Sign in</a>
        </Button>
      </AuthLayout>
    );
  }

  return (
    <AuthLayout title="Choose a new password">
      {token === "" && <AuthNotice kind="error">This link is missing its token. Ask for a new one.</AuthNotice>}
      <form onSubmit={submit} className="space-y-3">
        <div className="space-y-1">
          <Label htmlFor="password">New password</Label>
          <Input
            id="password"
            type="password"
            autoComplete="new-password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
            minLength={10}
            autoFocus
          />
          <p className="text-xs text-muted-foreground">At least 10 characters, and not a common one.</p>
        </div>
        {failure && <p className="text-xs text-danger">{failure}</p>}
        <Button type="submit" className="w-full" disabled={busy || token === ""}>
          {busy && <Loader2 className="animate-spin" />}
          Set the password
        </Button>
      </form>
      <AuthSwitch href="/forgot" prompt="Link expired?" action="Ask for another" />
    </AuthLayout>
  );
}

// Confirming an address. The link is consumed on arrival, so this page acts and then reports
// rather than asking somebody to press a button to do what they already asked for.
//
// A sign-up through a domain-limited invitation joins here, and only if the invitation still
// allows it — it may have been withdrawn, run out or expired since — so the address can be
// confirmed and the join refused in the same answer. Both are said.
export function VerifyPanel() {
  const [state, setState] = useState<"working" | "done" | "failed">("working");
  const [failure, setFailure] = useState("");
  const [joinError, setJoinError] = useState("");

  useEffect(() => {
    const token = new URLSearchParams(window.location.search).get("token") ?? "";
    if (!token) {
      // eslint-disable-next-line react-hooks/set-state-in-effect -- the URL is only readable after mount
      setState("failed");
      setFailure("This link is missing its token.");
      return;
    }
    api
      .post<{ joined?: boolean; join_error?: string }>("/api/auth/verify", { token })
      .then((r) => {
        setJoinError(r?.join_error ?? "");
        setState("done");
      })
      .catch((err) => {
        setState("failed");
        setFailure(errorMessage(err));
      });
  }, []);

  return (
    <AuthLayout
      title={state === "done" ? "Address confirmed" : "Confirming your address"}
      subtitle={state === "done" ? "You are all set" : undefined}
    >
      {state === "working" && (
        <p className="flex items-center gap-2 text-sm text-muted-foreground">
          <Loader2 className="size-4 animate-spin" /> One moment…
        </p>
      )}
      {state === "failed" && <AuthNotice kind="error">{failure}</AuthNotice>}
      {state === "done" && (
        <p className="flex items-center gap-2 text-sm">
          <CheckCircle2 className="size-4 text-success" /> Thank you — that is confirmed.
        </p>
      )}
      {state === "done" && joinError && <AuthNotice kind="error">{joinError}</AuthNotice>}
      <Button className="w-full" asChild>
        <a href="/admin/">Go to the console</a>
      </Button>
    </AuthLayout>
  );
}
