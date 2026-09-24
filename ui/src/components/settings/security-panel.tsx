"use client";

import { useState } from "react";
import { KeyRound, Loader2, ShieldCheck, Unlink } from "lucide-react";
import { toast } from "sonner";
import { ErrorBanner } from "@/components/core/error-banner";
import {
  SettingsActions,
  SettingsEditRow,
  SettingsGroup,
  SettingsRow,
  SettingsRows,
  SettingsSection,
} from "@/components/core/settings-section";
import { StatusChip } from "@/components/core/status-chip";
import { useAuth } from "@/components/shell/auth-provider";
import { SSOPanel } from "@/components/settings/sso-panel";
import {
  EnrolmentFlow,
  RecoveryCodes,
  type Enrolment,
} from "@/components/settings/two-factor-enrolment";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
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
  type Account,
  type AuthPolicy,
  type EffectiveSettings,
  type ProofKind,
} from "@/lib/api";

// Everything about getting into the console, on one screen: your own credentials at the top,
// then the policies an admin sets for everybody — including themselves, which is the sentence
// the copy keeps repeating on purpose.
//
// This tab saves as you go rather than collecting changes behind a Save button like the rest of
// Settings. A password field, a QR code and a list of recovery codes are each a finished act
// with its own confirmation; batching them into one submit would mean a screen that says
// "2 changes" and cannot tell you which half of your second factor is live.

const MIN_PASSWORD = 10;

const POLICY_LABELS: Record<AuthPolicy, string> = {
  any: "Password, Slack or single sign-on",
  password: "Email and password only",
  slack: "Slack only",
  microsoft: "Microsoft only",
  sso: "Single sign-on only",
};

const POLICY_HINTS: Record<AuthPolicy, string> = {
  any: "Members sign in with whichever they hold. Somebody who has more than one can use either.",
  password:
    "Continue with Slack stops working for everyone here, including you. Accounts that have only ever signed in with Slack will need a password before they can get back in.",
  slack:
    "Passwords stop working for everyone here, including you — sign-in and password resets both. Everybody goes through Slack, which is also where their workspace's own rules apply.",
  microsoft:
    "Passwords, Slack and single sign-on stop working for everyone here, including you. Everybody signs in with their Microsoft work account, which is also where their organisation's own rules apply.",
  sso:
    "Passwords and Slack both stop working for everyone here, including you. Everybody goes through your identity provider, which becomes the roster as well as the door: who it lets in is who gets in. Needs a verified provider below.",
};

/** A policy's name, which for "any" lists what this deployment actually offers. */
function policyLabel(p: AuthPolicy, microsoft: boolean): string {
  return p === "any" && microsoft ? "Password, Slack, Microsoft or single sign-on" : POLICY_LABELS[p];
}

/** The policies a deployment can offer: Microsoft only where it has Sign in with Microsoft. */
function policiesFor(microsoft: boolean, current: AuthPolicy): AuthPolicy[] {
  return (Object.keys(POLICY_LABELS) as AuthPolicy[]).filter(
    (p) => p !== "microsoft" || microsoft || current === "microsoft",
  );
}

export function SecurityPanel({
  settings,
  defaultEmailDomains,
  canManageOrg,
  onSaved,
}: {
  settings: EffectiveSettings;
  /** The deployment's ALLOWED_EMAIL_DOMAINS, which an empty list of email domains falls back to. */
  defaultEmailDomains: string[];
  /** Whether this person may set policy for everybody, not just for themselves. */
  canManageOrg: boolean;
  onSaved: () => void;
}) {
  const account = useApi<Account>("/api/account");
  const me = useAuth().me;
  const microsoftLogin = me?.microsoft_login ?? false;
  // A deployment with a Teams app says so here, since the same list gates both.
  const teams = me?.msteams ?? false;
  // An empty list means every member only where the deployment names no domains of its own; where
  // it does, an organisation without a list is held to those.
  const emptyMeans =
    defaultEmailDomains.length > 0
      ? `empty falls back to this deployment's default, ${defaultEmailDomains.join(", ")}.`
      : "empty means every member of the connected workspaces.";

  if (account.error && !account.data) {
    return <ErrorBanner message={account.error} onRetry={account.reload} />;
  }
  if (account.loading || !account.data) {
    return (
      <Card className="divide-y py-0">
        {Array.from({ length: 3 }).map((_, i) => (
          <div key={i} className="grid gap-3 px-6 py-5 md:grid-cols-[13rem_1fr] md:gap-8">
            <div className="space-y-2">
              <Skeleton className="h-3.5 w-24" />
              <Skeleton className="h-3 w-40" />
            </div>
            <Skeleton className="h-8 w-full max-w-sm" />
          </div>
        ))}
      </Card>
    );
  }

  const a = account.data;
  return (
    <div className="space-y-5">
      <SettingsGroup title="Your sign-in">
        <SettingsSection
          title="Password"
          description={
            a.has_password
              ? "Changing it does not sign your other devices out; end those under Users if you need to."
              : "You signed in with Slack and have never set one. A password is a second way in, for the day Slack is not."
          }
        >
          <PasswordField hasPassword={a.has_password} onSaved={account.reload} />
        </SettingsSection>

        <SettingsSection
          title="Two-factor"
          description="A six-digit code from an authenticator app, asked for after you sign in, whichever way you do — a password, Slack, Microsoft or single sign-on."
        >
          <TwoFactorField account={a} onChanged={account.reload} />
        </SettingsSection>

        <SettingsSection
          title="Slack"
          description="Connecting your Slack account lets you use Continue with Slack, and is what an organisation on the Slack-only policy signs in with."
        >
          <SlackField account={a} onChanged={account.reload} />
        </SettingsSection>

        {(microsoftLogin || (a.identities ?? []).some((i) => i.provider === "microsoft")) && (
          <SettingsSection
            title="Microsoft"
            description="Connecting your Microsoft work account lets you use Sign in with Microsoft, and makes you the same person here as the one the bot talks to in Teams."
          >
            <MicrosoftField account={a} onChanged={account.reload} />
          </SettingsSection>
        )}
      </SettingsGroup>

      {canManageOrg && (
        <SettingsGroup title="Everyone in this organisation">
          <SettingsSection
            title="Sign-in methods"
            description="Which credentials open this console — for every member, and for you."
          >
            <PolicyField policy={settings.AuthPolicy} microsoft={microsoftLogin} onSaved={onSaved} />
          </SettingsSection>
          <SSOPanel />
          <SettingsSection
            title="Require two-factor"
            description="Every sign-in has to carry a code, however it was made; Slack is no exemption. Members who have not enrolled are held at an enrolment screen until they do."
          >
            <RequireTwoFactorField
              required={settings.RequireTwoFactor}
              onSaved={() => {
                onSaved();
                account.reload();
              }}
            />
          </SettingsSection>
        </SettingsGroup>
      )}

      {canManageOrg && (
        <SettingsGroup title={teams ? "Who the bot answers" : "Who the bot answers in Slack"}>
          <SettingsSection
            title="Email domains"
            description={
              teams
                ? `Members whose email is on one of these domains: their Slack email, or their Microsoft account's address in Teams. It starts as the domain whoever founded this organisation signed up with; ${emptyMeans}`
                : `Members whose Slack email is on one of these domains. It starts as the domain whoever founded this organisation signed up with; ${emptyMeans}`
            }
          >
            <EmailDomainsField
              domains={settings.AllowedEmailDomains ?? []}
              fallback={defaultEmailDomains}
              onSaved={onSaved}
            />
          </SettingsSection>
          <SettingsSection
            title={teams ? "Guests and other organisations" : "Guests and Slack Connect"}
            description={
              teams
                ? "Accounts that are in a channel but not in the workspace: guests, people on the other side of a Slack Connect channel, and in Teams people from other Microsoft 365 organisations. Off means the bot refuses them and says who can change that. On means the domains above are all that gates them."
                : "Accounts that are in a channel but not in the workspace: single-channel guests, and people on the other side of a shared channel. Off means the bot refuses them and says who can change that. On means the domains above are all that gates them."
            }
          >
            <ExternalUsersField allowed={settings.AllowExternalUsers} onSaved={onSaved} />
          </SettingsSection>
        </SettingsGroup>
      )}
    </div>
  );
}

// ---- your password ----

// Nothing to read — a password is never displayed — so the resting state is the one control
// that matters, and the fields appear on request.
function PasswordField({ hasPassword, onSaved }: { hasPassword: boolean; onSaved: () => void }) {
  const [open, setOpen] = useState(false);
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const close = () => {
    setCurrent("");
    setNext("");
    setConfirm("");
    setError(null);
    setOpen(false);
  };

  if (!open) {
    return (
      <SettingsRows
        action={
          <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
            <KeyRound className="size-4" />
            {hasPassword ? "Change" : "Set a password"}
          </Button>
        }
      >
        <SettingsRow label="Password" empty="Not set">
          {hasPassword ? "••••••••••" : null}
        </SettingsRow>
      </SettingsRows>
    );
  }

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    if (next.length < MIN_PASSWORD) {
      setError(`Use at least ${MIN_PASSWORD} characters.`);
      return;
    }
    if (next !== confirm) {
      setError("The two new passwords do not match.");
      return;
    }
    setBusy(true);
    try {
      await api.post("/api/auth/password", { current, password: next });
      toast.success(hasPassword ? "Password changed" : "Password set");
      close();
      onSaved();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form onSubmit={submit} className="space-y-2">
      {hasPassword && (
        <SettingsEditRow label="Current" htmlFor="current-password">
          <Input
            id="current-password"
            type="password"
            autoComplete="current-password"
            autoFocus
            value={current}
            onChange={(e) => setCurrent(e.target.value)}
            className="max-w-sm"
          />
        </SettingsEditRow>
      )}
      <SettingsEditRow
        label="New"
        htmlFor="new-password"
        hint={`At least ${MIN_PASSWORD} characters.`}
      >
        <Input
          id="new-password"
          type="password"
          autoComplete="new-password"
          autoFocus={!hasPassword}
          value={next}
          onChange={(e) => setNext(e.target.value)}
          className="max-w-sm"
        />
      </SettingsEditRow>
      <SettingsEditRow label="Repeat" htmlFor="repeat-password">
        <Input
          id="repeat-password"
          type="password"
          autoComplete="new-password"
          value={confirm}
          onChange={(e) => setConfirm(e.target.value)}
          className="max-w-sm"
        />
      </SettingsEditRow>
      {error && <p className="px-1 text-xs text-danger">{error}</p>}
      <SettingsActions>
        <Button type="submit" size="sm" disabled={busy}>
          {busy && <Loader2 className="animate-spin" />}
          Save
        </Button>
        <Button type="button" variant="ghost" size="sm" disabled={busy} onClick={close}>
          Cancel
        </Button>
      </SettingsActions>
    </form>
  );
}

// ---- your second factor ----

function TwoFactorField({ account, onChanged }: { account: Account; onChanged: () => void }) {
  const [enrolment, setEnrolment] = useState<Enrolment | null>(null);
  const [codes, setCodes] = useState<string[] | null>(null);
  const [removing, setRemoving] = useState(false);
  const [starting, setStarting] = useState(false);
  const [busy, setBusy] = useState(false);

  const tf = account.two_factor;

  // The recovery codes, whether they arrived from enrolling or from asking for a new set. This
  // is the only time they exist anywhere but the person's own notes, so the panel stays open
  // until they say they have them.
  if (codes) {
    return <RecoveryCodes codes={codes} onDone={() => setCodes(null)} />;
  }
  if (enrolment) {
    return (
      <EnrolmentFlow
        enrolment={enrolment}
        email={account.user.email}
        onCancel={() => setEnrolment(null)}
        onDone={(recovery) => {
          setEnrolment(null);
          setCodes(recovery);
          onChanged();
        }}
      />
    );
  }
  // Turning it on asks for the same proof as turning it off. An authenticator is a way into the
  // account that outlives the session that added it, so a console somebody left signed in must
  // not be enough to add one.
  if (starting) {
    return (
      <StartFlow
        proof={tf.proof}
        onCancel={() => setStarting(false)}
        onStarted={(e) => {
          setStarting(false);
          setEnrolment(e);
        }}
      />
    );
  }
  if (removing) {
    return (
      <DisableFlow
        hasPassword={account.has_password}
        onCancel={() => setRemoving(false)}
        onDone={() => {
          setRemoving(false);
          onChanged();
        }}
      />
    );
  }

  // An account with nothing to type — a Slack sign-in that has never set a password — proves
  // itself with the age of its session, so there is no field to put up and the server answers
  // either with a secret or with "sign in again".
  const start = async () => {
    if (tf.proof !== "recent") {
      setStarting(true);
      return;
    }
    setBusy(true);
    try {
      setEnrolment(await api.post<Enrolment>("/api/account/totp/start"));
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const reissue = async () => {
    setBusy(true);
    try {
      const out = await api.post<{ recovery_codes: string[] }>("/api/account/totp/recovery");
      setCodes(out.recovery_codes);
      onChanged();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  if (!tf.enabled) {
    return (
      <SettingsRows
        action={
          <Button variant="outline" size="sm" onClick={start} disabled={busy}>
            {busy ? <Loader2 className="animate-spin" /> : <ShieldCheck className="size-4" />}
            Turn on
          </Button>
        }
      >
        <SettingsRow label="Status">
          <span className="flex flex-wrap items-center gap-2">
            <StatusChip variant={tf.required ? "warning" : "neutral"}>Off</StatusChip>
            {tf.required && (
              <span className="text-xs text-muted-foreground">
                Required here — the console holds you at enrolment until it is on.
              </span>
            )}
          </span>
        </SettingsRow>
      </SettingsRows>
    );
  }

  return (
    <SettingsRows
      action={
        <div className="flex flex-wrap justify-end gap-2">
          <Button variant="outline" size="sm" onClick={reissue} disabled={busy}>
            {busy && <Loader2 className="animate-spin" />}
            New recovery codes
          </Button>
          {!tf.required && (
            <Button variant="ghost" size="sm" onClick={() => setRemoving(true)}>
              Turn off
            </Button>
          )}
        </div>
      }
    >
      <SettingsRow label="Status">
        <StatusChip variant="success">On</StatusChip>
      </SettingsRow>
      <SettingsRow label="Recovery codes">
        <span className={tf.recovery_left === 0 ? "text-danger" : undefined}>
          {tf.recovery_left} unused
        </span>
      </SettingsRow>
    </SettingsRows>
  );
}

function DisableFlow({
  hasPassword,
  onCancel,
  onDone,
}: {
  hasPassword: boolean;
  onCancel: () => void;
  onDone: () => void;
}) {
  const [value, setValue] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await api.post("/api/account/totp/disable", hasPassword ? { password: value } : { code: value });
      toast.success("Two-factor is off");
      onDone();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form onSubmit={submit} className="space-y-2">
      <p className="px-1 text-sm text-muted-foreground">
        {hasPassword
          ? "Enter your password to take the second factor off this account."
          : "Enter a current code from your authenticator app to take it off this account."}
      </p>
      <SettingsEditRow label={hasPassword ? "Password" : "Code"} htmlFor="disable-proof">
        <Input
          id="disable-proof"
          autoFocus
          type={hasPassword ? "password" : "text"}
          inputMode={hasPassword ? undefined : "numeric"}
          autoComplete={hasPassword ? "current-password" : "one-time-code"}
          value={value}
          onChange={(e) => setValue(e.target.value)}
          className={hasPassword ? "max-w-sm" : "max-w-32 font-mono tabular-nums"}
        />
      </SettingsEditRow>
      {error && <p className="px-1 text-xs text-danger">{error}</p>}
      <SettingsActions>
        <Button type="submit" size="sm" variant="destructive" disabled={busy || value === ""}>
          {busy && <Loader2 className="animate-spin" />}
          Turn off
        </Button>
        <Button type="button" variant="ghost" size="sm" disabled={busy} onClick={onCancel}>
          Cancel
        </Button>
      </SettingsActions>
    </form>
  );
}

/**
 * The proof in front of enrolment. It is the same shape as DisableFlow and for the same reason:
 * adding a second factor and removing one are both changes to how this account is got into, and
 * neither should rest on nothing more than holding the session.
 */
function StartFlow({
  proof,
  onStarted,
  onCancel,
}: {
  proof: ProofKind;
  onStarted: (e: Enrolment) => void;
  onCancel: () => void;
}) {
  const [value, setValue] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const isPassword = proof === "password";

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      onStarted(
        await api.post<Enrolment>("/api/account/totp/start", isPassword ? { password: value } : { code: value }),
      );
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form onSubmit={submit} className="space-y-2">
      <p className="px-1 text-sm text-muted-foreground">
        {isPassword
          ? "Enter your password to add a second factor to this account."
          : "Enter a current code from your authenticator app to add another."}
      </p>
      <SettingsEditRow label={isPassword ? "Password" : "Code"} htmlFor="enrol-proof">
        <Input
          id="enrol-proof"
          autoFocus
          type={isPassword ? "password" : "text"}
          inputMode={isPassword ? undefined : "numeric"}
          autoComplete={isPassword ? "current-password" : "one-time-code"}
          value={value}
          onChange={(e) => setValue(e.target.value)}
          className={isPassword ? "max-w-sm" : "max-w-32 font-mono tabular-nums"}
        />
      </SettingsEditRow>
      {error && <p className="px-1 text-xs text-danger">{error}</p>}
      <SettingsActions>
        <Button type="submit" size="sm" disabled={busy || value === ""}>
          {busy && <Loader2 className="animate-spin" />}
          Continue
        </Button>
        <Button type="button" variant="ghost" size="sm" disabled={busy} onClick={onCancel}>
          Cancel
        </Button>
      </SettingsActions>
    </form>
  );
}

// ---- your Slack account ----

function SlackField({ account, onChanged }: { account: Account; onChanged: () => void }) {
  const [busy, setBusy] = useState(false);
  const slack = (account.identities ?? []).find((i) => i.provider === "slack");

  const disconnect = async () => {
    if (!slack) return;
    setBusy(true);
    try {
      await api.del(`/api/auth/identities/slack/${encodeURIComponent(slack.subject)}`);
      toast.success("Slack disconnected");
      onChanged();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <SettingsRows
      action={
        slack ? (
          <Button variant="ghost" size="sm" onClick={disconnect} disabled={busy}>
            {busy ? <Loader2 className="animate-spin" /> : <Unlink className="size-4" />}
            Disconnect
          </Button>
        ) : (
          <Button variant="outline" size="sm" asChild>
            <a href="/api/auth/login">Connect Slack</a>
          </Button>
        )
      }
    >
      <SettingsRow label="Status">
        {slack ? (
          <StatusChip variant="success">Connected</StatusChip>
        ) : (
          <StatusChip variant="neutral">Not connected</StatusChip>
        )}
      </SettingsRow>
      {slack && (
        <SettingsRow label="Workspace">
          <span className="font-mono text-xs">{slack.subject.split(":")[0]}</span>
        </SettingsRow>
      )}
    </SettingsRows>
  );
}

// ---- your Microsoft account ----

function MicrosoftField({ account, onChanged }: { account: Account; onChanged: () => void }) {
  const [busy, setBusy] = useState(false);
  const ms = (account.identities ?? []).find((i) => i.provider === "microsoft");

  const disconnect = async () => {
    if (!ms) return;
    setBusy(true);
    try {
      await api.del(`/api/auth/identities/microsoft/${encodeURIComponent(ms.subject)}`);
      toast.success("Microsoft disconnected");
      onChanged();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <SettingsRows
      action={
        ms ? (
          <Button variant="ghost" size="sm" onClick={disconnect} disabled={busy}>
            {busy ? <Loader2 className="animate-spin" /> : <Unlink className="size-4" />}
            Disconnect
          </Button>
        ) : (
          <Button variant="outline" size="sm" asChild>
            <a href="/api/auth/microsoft/login">Connect Microsoft</a>
          </Button>
        )
      }
    >
      <SettingsRow label="Status">
        {ms ? (
          <StatusChip variant="success">Connected</StatusChip>
        ) : (
          <StatusChip variant="neutral">Not connected</StatusChip>
        )}
      </SettingsRow>
      {ms && (
        <SettingsRow label="Organisation">
          <span className="font-mono text-xs">{ms.subject.split(":")[0]}</span>
        </SettingsRow>
      )}
    </SettingsRows>
  );
}

// ---- policy for everybody ----

function PolicyField({
  policy,
  microsoft,
  onSaved,
}: {
  policy: AuthPolicy;
  /** Whether this deployment offers Sign in with Microsoft, and so the Microsoft-only policy. */
  microsoft: boolean;
  onSaved: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [value, setValue] = useState<AuthPolicy>(policy);
  const [seen, setSeen] = useState<AuthPolicy>(policy);
  const [error, setError] = useState<string | null>(null);

  // The control moves first and puts itself back if the save is refused, so it already agrees
  // with what this browser asked for. This is the other direction: the settings were read
  // again — any panel's save asks for that — and somebody else had moved the policy meanwhile.
  // The settings page used to remount this whole tab to get it, which also threw away every
  // unsaved edit on every other tab; taking the one value costs less.
  if (policy !== seen) {
    setSeen(policy);
    setValue(policy);
  }

  const change = async (next: AuthPolicy) => {
    const previous = value;
    setValue(next);
    setBusy(true);
    setError(null);
    try {
      await api.put("/api/settings", { auth_policy: next });
      toast.success(`Sign-in methods: ${policyLabel(next, microsoft).toLowerCase()}`);
      onSaved();
    } catch (err) {
      setValue(previous); // refused: the control goes back to what is actually stored
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-2">
      <Label htmlFor="auth-policy" className="sr-only">
        Allowed sign-in methods
      </Label>
      <Select value={value} disabled={busy} onValueChange={(v) => change(v as AuthPolicy)}>
        <SelectTrigger id="auth-policy" aria-label="Allowed sign-in methods" className="w-full max-w-sm">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {policiesFor(microsoft, value).map((p) => (
            <SelectItem key={p} value={p}>
              {policyLabel(p, microsoft)}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      <p className="text-xs leading-relaxed text-muted-foreground">{POLICY_HINTS[value]}</p>
      {error && <p className="text-xs text-danger">{error}</p>}
    </div>
  );
}

function RequireTwoFactorField({
  required,
  onSaved,
}: {
  required: boolean;
  onSaved: () => void;
}) {
  const [on, setOn] = useState(required);
  const [seen, setSeen] = useState(required);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Same as the policy above: optimistic here, re-read from the server when it changed there.
  if (required !== seen) {
    setSeen(required);
    setOn(required);
  }

  const change = async (next: boolean) => {
    setOn(next);
    setBusy(true);
    setError(null);
    try {
      await api.put("/api/settings", { require_two_factor: next ? "1" : "0" });
      toast.success(next ? "Two-factor is now required" : "Two-factor is no longer required");
      onSaved();
    } catch (err) {
      setOn(!next);
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-2">
      <label className="flex items-center gap-2.5">
        <Switch
          id="require_two_factor"
          // The switch carries its own name because the text beside it is only there when it is
          // on: off, the label would otherwise wrap nothing and leave the control unnamed.
          aria-label="Require two-factor"
          checked={on}
          disabled={busy}
          onCheckedChange={change}
        />
        {on && <span className="text-sm">Required</span>}
      </label>
      {error && <p className="text-xs text-danger">{error}</p>}
    </div>
  );
}

// ---- who the bot answers ----

// A short list typed as text: the server splits on commas and spaces, lowercases, drops a
// leading @ and refuses anything that is not a domain, so the field can stay one input.
function EmailDomainsField({
  domains,
  fallback,
  onSaved,
}: {
  domains: string[];
  /** What an empty list stands for: the deployment's own domains, or nobody at all. */
  fallback: string[];
  onSaved: () => void;
}) {
  const joined = domains.join(", ");
  const [value, setValue] = useState(joined);
  const [seen, setSeen] = useState(joined);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Re-read from the server when it changed there, as the policy control above does.
  if (joined !== seen) {
    setSeen(joined);
    setValue(joined);
  }

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await api.put("/api/settings", { allowed_email_domains: value.trim() });
      toast.success(
        value.trim() !== ""
          ? "Email domains saved"
          : fallback.length > 0
            ? "Cleared: this deployment's default applies"
            : "The bot answers every member",
      );
      onSaved();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form onSubmit={submit} className="space-y-2">
      <SettingsEditRow
        label="Domains"
        htmlFor="allowed-email-domains"
        hint="Separate several with commas: acme.example, acme.co"
      >
        <Input
          id="allowed-email-domains"
          value={value}
          placeholder={
            fallback.length > 0
              ? `This deployment's default: ${fallback.join(", ")}`
              : "Everyone in the workspace"
          }
          autoComplete="off"
          spellCheck={false}
          onChange={(e) => setValue(e.target.value)}
          className="max-w-sm"
        />
      </SettingsEditRow>
      {error && <p className="px-1 text-xs text-danger">{error}</p>}
      <SettingsActions>
        <Button type="submit" size="sm" disabled={busy || value.trim() === joined}>
          {busy && <Loader2 className="animate-spin" />}
          Save
        </Button>
      </SettingsActions>
    </form>
  );
}

function ExternalUsersField({ allowed, onSaved }: { allowed: boolean; onSaved: () => void }) {
  const [on, setOn] = useState(allowed);
  const [seen, setSeen] = useState(allowed);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  if (allowed !== seen) {
    setSeen(allowed);
    setOn(allowed);
  }

  const change = async (next: boolean) => {
    setOn(next);
    setBusy(true);
    setError(null);
    try {
      await api.put("/api/settings", { allow_external_users: next ? "1" : "0" });
      toast.success(
        next
          ? "Guests and Slack Connect members can use the bot"
          : "Only workspace members can use the bot",
      );
      onSaved();
    } catch (err) {
      setOn(!next);
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-2">
      <label className="flex items-center gap-2.5">
        <Switch id="allow_external_users" checked={on} disabled={busy} onCheckedChange={change} />
        <span className="text-sm">{on ? "Allowed" : "Members only"}</span>
      </label>
      {error && <p className="text-xs text-danger">{error}</p>}
    </div>
  );
}
