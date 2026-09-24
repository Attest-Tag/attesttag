"use client";

import { useEffect, useState } from "react";
import { KeyRound, Loader2 } from "lucide-react";
import Link from "next/link";
import {
  AuthLayout,
  AuthNotice,
  AuthSwitch,
  MicrosoftGlyph,
  SlackGlyph,
  useAuthError,
  useMicrosoftLoginHref,
  useSlackLoginHref,
  useSSOStart,
} from "@/components/auth/auth-layout";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api, errorMessage, useApi, type Me } from "@/lib/api";

/** What POST /api/auth/login answers with: a session, or a second factor still owed. */
type LoginResult = {
  ok?: boolean;
  two_factor?: boolean;
  challenge?: string;
  recovery_left?: number;
  /** Where to go instead of the console — set when this sign-in carried an invitation. */
  next?: string;
};

// Signing in. Up to four ways, and they are separate credentials rather than doors to the same
// one: a Slack or Microsoft sign-in is only ever matched to the identity it proves, never to an
// account that happens to share an email address, and single sign-on is matched to the identity
// its provider vouches for at a domain that provider's organisation has proved it owns.
//
// Single sign-on has no button of its own to press blind: which provider somebody belongs to is
// decided by the domain of the address they type, so the email field above serves both it and
// the password below.
export function SignInForm() {
  const me = useApi<Me>("/api/me");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);
  const [challenge, setChallenge] = useState<string | null>(null);
  useEffect(() => {
    const token = new URLSearchParams(window.location.hash.slice(1)).get("challenge");
    if (token) {
      setChallenge(token);
      window.history.replaceState(null, "", window.location.pathname);
    }
  }, []);
  const authError = useAuthError();
  const slackLoginHref = useSlackLoginHref();
  const microsoftLoginHref = useMicrosoftLoginHref();

  const sso = useSSOStart();

  const slackLogin = me.data?.slack_login ?? false;
  const microsoftLogin = me.data?.microsoft_login ?? false;
  const oneClick = slackLogin || microsoftLogin;
  const signupOpen = me.data?.signup_open ?? false;
  // Add to Slack was pressed signed out — on the site or on the Marketplace — and the server
  // parked the install behind this sign-in. Say so: otherwise a sign-in page is the last thing
  // somebody who pressed an install button expects to see.
  const installPending = me.data?.install_pending ?? false;

  const startSSO = async () => {
    setFailure(null);
    const problem = await sso.start(email);
    if (problem) setFailure(problem);
  };

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setFailure(null);
    try {
      const out = await api.post<LoginResult>("/api/auth/login", { email, password });
      // A correct password is not always a session: an account with a second factor gets a
      // challenge instead, and the code screen below finishes the sign-in.
      if (out?.two_factor && out.challenge) {
        setChallenge(out.challenge);
        setBusy(false);
        return;
      }
      // A full navigation, not a router push. The session cookie is set by the response we
      // just got, and the console's first render reads it — a client-side transition would
      // render the shell from state that predates the cookie.
      //
      // `next` is the server saying this sign-in arrived holding an invitation: following the
      // link is what they were doing, so the join screen comes before the console. It is a path
      // the server chose from a fixed set, never a redirect target from the address bar.
      // eslint-disable-next-line @next/next/no-location-assign-relative-destination -- see above
      window.location.href = out?.next || "/admin/";
    } catch (err) {
      setFailure(errorMessage(err));
      setBusy(false);
    }
  };

  if (challenge) {
    return <TwoFactorStep challenge={challenge} onCancel={() => setChallenge(null)} />;
  }

  return (
    <AuthLayout title="Sign in" subtitle="To the admin console" wide>
      {authError && <AuthNotice kind="error">{authError}</AuthNotice>}
      {installPending && (
        <AuthNotice kind="info">
          Sign in and you go straight on to Slack’s consent screen to connect your workspace.
          New here? Create an account below and it does the same.
        </AuthNotice>
      )}
      {slackLogin && (
        <Button variant="outline" className="w-full" asChild>
          <a href={slackLoginHref}>
            <SlackGlyph />
            Sign in with Slack
          </a>
        </Button>
      )}
      {microsoftLogin && (
        <Button variant="outline" className="w-full" asChild>
          <a href={microsoftLoginHref}>
            <MicrosoftGlyph />
            Sign in with Microsoft
          </a>
        </Button>
      )}
      {oneClick && (
        <div className="flex items-center gap-3 text-xs text-muted-foreground">
          <span className="h-px flex-1 bg-border" />
          or
          <span className="h-px flex-1 bg-border" />
        </div>
      )}
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
            autoFocus={!oneClick}
          />
        </div>
        {/* A grid rather than a label row above the box, so that "Forgot it?" can sit
            top-right while coming after the password in the DOM — tabbing out of the email
            should land on the password, not on the way out of the form. */}
        <div className="grid grid-cols-[1fr_auto] items-baseline gap-x-3 gap-y-1">
          <Label htmlFor="password" className="col-start-1 row-start-1">
            Password
          </Label>
          <Input
            id="password"
            type="password"
            autoComplete="current-password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
            className="col-span-2 row-start-2"
          />
          <Link
            href="/forgot"
            className="col-start-2 row-start-1 justify-self-end text-xs text-muted-foreground underline underline-offset-2"
          >
            Forgot it?
          </Link>
        </div>
        {failure && <p className="text-xs text-danger">{failure}</p>}
        <Button type="submit" className="w-full" disabled={busy}>
          {busy && <Loader2 className="animate-spin" />}
          Sign in
        </Button>
        {/* Under the form rather than beside the Slack button, because it reads the address
            typed above it: which provider to send somebody to is decided by their domain. */}
        <Button
          type="button"
          variant="outline"
          className="w-full"
          disabled={sso.busy}
          onClick={startSSO}
        >
          {sso.busy ? <Loader2 className="animate-spin" /> : <KeyRound className="size-4" />}
          Use single sign-on instead
        </Button>
      </form>
      {signupOpen ? (
        <AuthSwitch href="/signup" prompt="No account?" action="Create one" />
      ) : (
        <p className="text-center text-xs text-muted-foreground">
          New accounts are by invitation. Ask somebody in your organisation for a link.
        </p>
      )}
    </AuthLayout>
  );
}

/**
 * The second half of a password sign-in. It is a separate screen rather than a field that
 * appears under the password, because the two are answered at different moments — the password
 * from memory, the code from a phone that has to be picked up — and a form that keeps both on
 * screen invites a manager to fill the code box with the password it just saved.
 *
 * The recovery code is behind a link, not a second box: offering both at once is how somebody
 * spends one of ten single-use codes on a night when their phone was simply in the next room.
 */
function TwoFactorStep({ challenge, onCancel }: { challenge: string; onCancel: () => void }) {
  const [code, setCode] = useState("");
  const [recovery, setRecovery] = useState(false);
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setFailure(null);
    try {
      const out = await api.post<{ next?: string }>("/api/auth/two-factor", {
        challenge,
        ...(recovery ? { recovery: code } : { code }),
      });
      // eslint-disable-next-line @next/next/no-location-assign-relative-destination -- see submit above
      window.location.href = out?.next || "/admin/";
    } catch (err) {
      setFailure(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <AuthLayout
      title="One more step"
      subtitle={recovery ? "Use a recovery code" : "Enter the code from your authenticator app"}
      wide
    >
      <form onSubmit={submit} className="space-y-3">
        <div className="space-y-1">
          <Label htmlFor="totp">{recovery ? "Recovery code" : "Six-digit code"}</Label>
          <Input
            id="totp"
            autoFocus
            autoComplete="one-time-code"
            inputMode={recovery ? "text" : "numeric"}
            placeholder={recovery ? "ABCDE-FGHJK" : "123456"}
            value={code}
            onChange={(e) => setCode(e.target.value)}
            required
            className="font-mono tabular-nums"
          />
          <p className="text-xs text-muted-foreground">
            {recovery
              ? "One of the ten codes you saved when you turned two-factor on. Each works once."
              : "The code changes every 30 seconds. If it keeps failing, check your phone's clock."}
          </p>
        </div>
        {failure && <p className="text-xs text-danger">{failure}</p>}
        <Button type="submit" className="w-full" disabled={busy}>
          {busy && <Loader2 className="animate-spin" />}
          Continue
        </Button>
      </form>
      <p className="text-center text-xs text-muted-foreground">
        <button
          type="button"
          className="font-medium text-foreground underline underline-offset-2"
          onClick={() => {
            setRecovery(!recovery);
            setCode("");
            setFailure(null);
          }}
        >
          {recovery ? "Use your authenticator app" : "Lost your phone? Use a recovery code"}
        </button>
        {" · "}
        <button type="button" className="underline underline-offset-2" onClick={onCancel}>
          Start again
        </button>
      </p>
    </AuthLayout>
  );
}
