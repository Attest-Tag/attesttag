"use client";

import { useState } from "react";
import { QRCodeSVG } from "qrcode.react";
import { Loader2 } from "lucide-react";
import { toast } from "sonner";
import { CopyButton } from "@/components/core/copy-button";
import { SettingsActions, SettingsEditRow } from "@/components/core/settings-section";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { api, errorMessage } from "@/lib/api";

// Turning two-factor on, and the codes that come with it. Shared by Settings → Security and by
// the screen that holds somebody at the door when their organisation requires a factor they do
// not have yet: the same three steps either way — scan, type a code back, write the recovery
// codes down — so there is one flow to get right rather than two that drift.

/** What POST /api/account/totp/start hands back: the secret, and the URI a QR code carries. */
export type Enrolment = { secret: string; uri: string };

export function EnrolmentFlow({
  enrolment,
  email,
  onCancel,
  cancelLabel = "Cancel",
  onDone,
}: {
  enrolment: Enrolment;
  /** Shown so the person can tell which entry in their app this is; omitted on the sign-in gate. */
  email?: string;
  onCancel: () => void;
  cancelLabel?: string;
  onDone: (recovery: string[]) => void;
}) {
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const out = await api.post<{ recovery_codes: string[] }>("/api/account/totp/confirm", {
        code,
      });
      toast.success("Two-factor is on");
      onDone(out.recovery_codes);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form onSubmit={submit} className="space-y-3">
      <div className="flex flex-wrap items-start gap-4">
        {/* White behind the code regardless of theme: a dark-mode QR with an inverted
            quiet zone is one many phone cameras will not read. */}
        <div className="rounded-lg border bg-white p-2">
          <QRCodeSVG value={enrolment.uri} size={132} />
        </div>
        {/* A minimum width rather than min-w-0: below it the text wraps under the QR instead of
            squeezing beside it, which is what the narrow card on the sign-in gate needs. */}
        <div className="min-w-[13rem] flex-1 space-y-2 text-sm">
          <p className="text-muted-foreground">
            Scan this with an authenticator app — 1Password, Google Authenticator, whichever you
            use — then type the code it shows{email ? ` for ${email}` : ""}.
          </p>
          <div className="flex min-w-0 items-center gap-1">
            <code className="min-w-0 truncate rounded bg-muted px-2 py-1 font-mono text-xs">
              {enrolment.secret}
            </code>
            <CopyButton text={enrolment.secret} label="Copy the setup key" />
          </div>
          <p className="text-xs text-muted-foreground">
            Or type that key in by hand if the camera will not cooperate.
          </p>
        </div>
      </div>
      <SettingsEditRow label="Code" htmlFor="totp-code">
        <Input
          id="totp-code"
          autoFocus
          inputMode="numeric"
          autoComplete="one-time-code"
          placeholder="123456"
          maxLength={7}
          value={code}
          onChange={(e) => setCode(e.target.value)}
          className="max-w-32 font-mono tabular-nums"
        />
      </SettingsEditRow>
      {error && <p className="px-1 text-xs text-danger">{error}</p>}
      <SettingsActions>
        <Button type="submit" size="sm" disabled={busy || code.trim().length < 6}>
          {busy && <Loader2 className="animate-spin" />}
          Confirm
        </Button>
        <Button type="button" variant="ghost" size="sm" disabled={busy} onClick={onCancel}>
          {cancelLabel}
        </Button>
      </SettingsActions>
    </form>
  );
}

export function RecoveryCodes({ codes, onDone }: { codes: string[]; onDone: () => void }) {
  return (
    <div className="space-y-3">
      <p className="text-sm text-muted-foreground">
        Keep these somewhere that is not your phone. Each one signs you in once, and this is the
        only time they are shown — asking for a new set replaces every one of them.
      </p>
      <ul className="grid grid-cols-2 gap-x-6 gap-y-1 rounded-lg border bg-muted/40 p-3 font-mono text-sm sm:max-w-sm">
        {codes.map((c) => (
          <li key={c} className="tabular-nums">
            {c}
          </li>
        ))}
      </ul>
      <div className="flex flex-wrap gap-2">
        <CopyButton text={codes.join("\n")} label="Copy all" />
        <Button size="sm" onClick={onDone}>
          I have saved them
        </Button>
      </div>
    </div>
  );
}
