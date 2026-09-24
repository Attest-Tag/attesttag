"use client";

import { useState } from "react";
import { KeyRound, Loader2 } from "lucide-react";
import {
  AuthLayout,
  AuthNotice,
  AuthSwitch,
  MicrosoftGlyph,
  SlackGlyph,
  useMicrosoftLoginHref,
  useOneShotParam,
  useSlackLoginHref,
  useSSOStart,
} from "@/components/auth/auth-layout";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api, errorMessage, useApi, type Me } from "@/lib/api";
import { browserZone } from "@/lib/format";

// Creating an account, and with it an organisation.
//
// An invited person arrives here from /invite/{token}, which parks the token in a cookie and
// puts the address it was sent to in the query string — so the form knows to ask for a name and
// a password rather than an organisation, and the address is fixed rather than typed.
//
// A share link names nobody, so it arrives as `invite=open` with no address to fix: the same
// form, joining the same organisation, but they type the address their account will use. When
// the link only accepts one email domain that arrives too, so it can be said before the form is
// filled in rather than after it is submitted.
export function SignUpForm() {
  const me = useApi<Me>("/api/me");
  const invited = useOneShotParam("invited");
  const openInvite = useOneShotParam("invite") === "open";
  const inviteDomain = useOneShotParam("domain");
  const [email, setEmail] = useState("");
  const [name, setName] = useState("");
  const [org, setOrg] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);
  const slackLoginHref = useSlackLoginHref();
  const microsoftLoginHref = useMicrosoftLoginHref();
  const sso = useSSOStart();

  const slackLogin = me.data?.slack_login ?? false;
  const microsoftLogin = me.data?.microsoft_login ?? false;
  const signupOpen = me.data?.signup_open ?? false;
  // See SignInForm: an install parked behind this page by a signed-out Add to Slack.
  const installPending = me.data?.install_pending ?? false;
  // Both kinds join an organisation rather than found one, which is what the form branches on.
  // They differ only in whether the address is fixed.
  const joining = invited !== null || openInvite;

  // Single sign-on does not create anything here, which is why it is not one of the buttons at
  // the top beside Slack. An organisation's identity provider is registered by that
  // organisation, and signing in through it joins THAT organisation — so for somebody whose
  // employer is already on attest_tag, this page is the wrong one and the answer is to send
  // them through their provider rather than to have them found a second organisation next to
  // their colleagues' and wonder later why it is empty.
  const startSSO = async () => {
    setFailure(null);
    const problem = await sso.start(invited ?? email);
    if (problem) setFailure(problem);
  };

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setFailure(null);
    try {
      const out = await api.post<{ next?: string }>("/api/auth/signup", {
        email: invited ?? email,
        password,
        name,
        org: joining ? "" : org,
        // Where they are signing up from becomes the organisation's zone, so "today" and new
        // routines mean what they mean by them. Ignored when joining somebody else's.
        tz: browserZone(),
      });
      // A full navigation, not a router push. The session cookie is set by the response we
      // just got, and the console's first render reads it — a client-side transition would
      // render the shell from state that predates the cookie.
      //
      // `next` is the server saying this sign-up was on its way to connecting a workspace: the
      // install is where it ends, not the Overview. A path the server chose from a fixed set,
      // never a redirect target from the address bar.
      // eslint-disable-next-line @next/next/no-location-assign-relative-destination -- see above
      window.location.href = out?.next || "/admin/";
    } catch (err) {
      setFailure(errorMessage(err));
      setBusy(false);
    }
  };

  if (!signupOpen && !joining) {
    return (
      <AuthLayout title="By invitation" subtitle="attest_tag is not open for sign-ups yet" wide>
        <AuthNotice kind="info">
          Somebody already using attest_tag can invite you from Settings → Users. The link they
          send brings you back here with your address filled in.
        </AuthNotice>
        <AuthSwitch href="/login" prompt="Already have an account?" action="Sign in" />
      </AuthLayout>
    );
  }

  return (
    <AuthLayout
      title={joining ? "Accept your invitation" : "Create your organisation"}
      subtitle={
        invited
          ? `Invited as ${invited}`
          : joining
            ? inviteDomain
              ? `Join with your ${inviteDomain} address`
              : "Join with the link you were sent"
            : "It takes a minute"
      }
      wide
    >
      {installPending && !joining && (
        <AuthNotice kind="info">
          Create your organisation and you go straight on to Slack’s consent screen to connect
          your workspace.
        </AuthNotice>
      )}
      {(slackLogin || microsoftLogin) && !joining && (
        <>
          {slackLogin && (
            <Button variant="outline" className="w-full" asChild>
              <a href={slackLoginHref}>
                <SlackGlyph />
                Continue with Slack
              </a>
            </Button>
          )}
          {microsoftLogin && (
            <Button variant="outline" className="w-full" asChild>
              <a href={microsoftLoginHref}>
                <MicrosoftGlyph />
                Continue with Microsoft
              </a>
            </Button>
          )}
          <div className="flex items-center gap-3 text-xs text-muted-foreground">
            <span className="h-px flex-1 bg-border" />
            or
            <span className="h-px flex-1 bg-border" />
          </div>
        </>
      )}
      <form onSubmit={submit} className="space-y-3">
        {!joining && (
          <div className="space-y-1">
            <Label htmlFor="org">Organisation name</Label>
            <Input
              id="org"
              value={org}
              onChange={(e) => setOrg(e.target.value)}
              placeholder="Acme"
              required
              autoFocus
            />
            <p className="text-xs text-muted-foreground">
              What your team is called. You can change it later.
            </p>
          </div>
        )}
        <div className="space-y-1">
          <Label htmlFor="name">Your name</Label>
          <Input id="name" autoComplete="name" value={name} onChange={(e) => setName(e.target.value)} />
        </div>
        {invited === null && (
          <div className="space-y-1">
            <Label htmlFor="email">Email</Label>
            <Input
              id="email"
              type="email"
              autoComplete="username"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              required
            />
            {inviteDomain && (
              <p className="text-xs text-muted-foreground">
                This link only accepts {inviteDomain} addresses.
              </p>
            )}
          </div>
        )}
        <div className="space-y-1">
          <Label htmlFor="password">Password</Label>
          <Input
            id="password"
            type="password"
            autoComplete="new-password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
            minLength={10}
          />
          <p className="text-xs text-muted-foreground">At least 10 characters, and not a common one.</p>
        </div>
        {failure && <p className="text-xs text-danger">{failure}</p>}
        <Button type="submit" className="w-full" disabled={busy}>
          {busy && <Loader2 className="animate-spin" />}
          {joining ? "Join" : "Create organisation"}
        </Button>
      </form>
      <div className="space-y-2">
        <div className="flex items-center gap-3 text-xs text-muted-foreground">
          <span className="h-px flex-1 bg-border" />
          or
          <span className="h-px flex-1 bg-border" />
        </div>
        <Button
          type="button"
          variant="outline"
          className="w-full"
          disabled={sso.busy}
          onClick={startSSO}
        >
          {sso.busy ? <Loader2 className="animate-spin" /> : <KeyRound className="size-4" />}
          Use your organisation&apos;s single sign-on
        </Button>
        <p className="text-center text-xs text-muted-foreground">
          {joining
            ? "Signs you in through your provider instead, with no password to set here."
            : "Joins the organisation that set it up, with the address above \u2014 no invitation needed."}
        </p>
      </div>
      <AuthSwitch href="/login" prompt="Already have an account?" action="Sign in" />
    </AuthLayout>
  );
}
