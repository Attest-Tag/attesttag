"use client";

import { useEffect, useState } from "react";
import { Loader2 } from "lucide-react";
import { AuthLayout, AuthNotice } from "@/components/auth/auth-layout";
import { useAuth } from "@/components/shell/auth-provider";
import {
  EnrolmentFlow,
  RecoveryCodes,
  type Enrolment,
} from "@/components/settings/two-factor-enrolment";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api, errorMessage, type Account, type ProofKind } from "@/lib/api";

// What somebody sees when their organisation requires a second factor and their account has
// none: the enrolment, and nothing else.
//
// It stands in front of the whole console rather than nagging from a banner, because that is
// what the server is already doing — every route but this one's own endpoints answers 403 while
// the factor is owed, so a console rendered behind a banner would be a screen of failed
// requests. Signing out stays available: being unable to leave is not a security posture.
//
// Enrolling proves the account first, the way taking the factor off always has. For most people
// held here that proof is the password they signed in with a moment ago; for a Slack sign-in
// that has never set one it is the age of the session, so there is nothing to type and the
// secret is asked for straight away. /api/account is readable while the factor is owed — it has
// to be, or this screen could not know which of the two it is looking at.

export function TwoFactorGate() {
  const { signOut } = useAuth();
  const [proof, setProof] = useState<ProofKind | null>(null);
  const [enrolment, setEnrolment] = useState<Enrolment | null>(null);
  const [codes, setCodes] = useState<string[] | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  const [value, setValue] = useState("");
  const [busy, setBusy] = useState(false);

  const start = (body?: unknown) =>
    api
      .post<Enrolment>("/api/account/totp/start", body)
      .then(setEnrolment)
      .catch((err) => setFailure(errorMessage(err)));

  useEffect(() => {
    let live = true;
    api
      .get<Account>("/api/account")
      .then((acct) => {
        if (!live) return;
        setProof(acct.two_factor.proof);
        // Nothing to ask for: the session's own age is the proof, so go straight for the secret.
        if (acct.two_factor.proof === "recent") return start();
      })
      .catch((err) => {
        if (live) setFailure(errorMessage(err));
      });
    return () => {
      live = false;
    };
  }, []);

  // The codes are the last step, and reloading is what lets the console in: /api/me is what
  // decides this screen is over, so it has to be asked again rather than assumed.
  if (codes) {
    return (
      <AuthLayout title="Save these" subtitle="Recovery codes, shown once">
        <RecoveryCodes codes={codes} onDone={() => window.location.reload()} />
      </AuthLayout>
    );
  }

  const submitProof = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setFailure(null);
    await start(proof === "password" ? { password: value } : { code: value });
    setBusy(false);
  };

  const asking = !enrolment && !failure && proof && proof !== "recent";

  return (
    <AuthLayout
      title="Two-factor required"
      subtitle="Your organisation asks for a code after the password"
    >
      {failure ? (
        <AuthNotice kind="error">{failure}</AuthNotice>
      ) : enrolment ? (
        <EnrolmentFlow
          enrolment={enrolment}
          email=""
          onCancel={signOut}
          cancelLabel="Sign out"
          onDone={setCodes}
        />
      ) : asking ? (
        <form onSubmit={submitProof} className="space-y-3">
          <p className="text-sm text-muted-foreground">
            {proof === "password"
              ? "Confirm your password to set up your authenticator."
              : "Enter a current code from your authenticator app."}
          </p>
          <div className="space-y-1.5">
            <Label htmlFor="gate-proof">{proof === "password" ? "Password" : "Code"}</Label>
            <Input
              id="gate-proof"
              autoFocus
              type={proof === "password" ? "password" : "text"}
              inputMode={proof === "password" ? undefined : "numeric"}
              autoComplete={proof === "password" ? "current-password" : "one-time-code"}
              value={value}
              onChange={(e) => setValue(e.target.value)}
            />
          </div>
          <Button type="submit" className="w-full" disabled={busy || value === ""}>
            {busy && <Loader2 className="animate-spin" />}
            Continue
          </Button>
        </form>
      ) : (
        <div className="flex items-center gap-2 text-sm text-muted-foreground">
          <Loader2 className="size-4 animate-spin" />
          Preparing your authenticator setup…
        </div>
      )}
      {(failure || asking) && (
        <Button variant="outline" className="w-full" onClick={signOut}>
          Sign out
        </Button>
      )}
    </AuthLayout>
  );
}
