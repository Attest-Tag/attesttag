"use client";

import { useState } from "react";
import { Loader2, PencilLine } from "lucide-react";
import { toast } from "sonner";
import { ErrorBanner } from "@/components/core/error-banner";
import { RelativeTime } from "@/components/core/relative-time";
import {
  SettingsActions,
  SettingsEditRow,
  SettingsGroup,
  SettingsRow,
  SettingsRows,
  SettingsSection,
} from "@/components/core/settings-section";
import { StatusChip } from "@/components/core/status-chip";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { api, errorMessage, useApi, type Account } from "@/lib/api";

// The General tab: you, rather than the bot. Everything here is about the account making the
// request — the console shows it read-only until you ask to change something, because most
// visits are to check what an address or a name is rather than to edit it.
//
// The organisation appears at the bottom as a readout only. Renaming it, and everything else
// that applies to everybody, lives under Workspace where it is somebody's permission.

export function AccountPanel() {
  const account = useApi<Account>("/api/account");

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
      <SettingsGroup title="You">
        <SettingsSection
          title="Name"
          description="How the console and the bot refer to you — on access requests, approvals and anything you create."
        >
          <NameField name={a.user.name} onSaved={account.reload} />
        </SettingsSection>

        <SettingsSection
          title="Email"
          description="Your sign-in address and where invitations and password resets are sent. It is the key an invitation is addressed to, so changing it is not a text field."
        >
          <EmailField account={a} onChanged={account.reload} />
        </SettingsSection>

        <SettingsSection
          title="Account"
          description="When this account was made, and the last time it was seen in the console."
        >
          <SettingsRows>
            <SettingsRow label="Member since">
              {a.user.created_at ? <RelativeTime value={a.user.created_at} /> : null}
            </SettingsRow>
            <SettingsRow label="Last seen" empty="Not since this sign-in">
              {a.user.last_seen ? <RelativeTime value={a.user.last_seen} /> : null}
            </SettingsRow>
            <SettingsRow label="Status">
              <StatusChip variant={a.user.status === "active" ? "success" : "warning"}>
                {a.user.status}
              </StatusChip>
            </SettingsRow>
          </SettingsRows>
        </SettingsSection>
      </SettingsGroup>

      <SettingsGroup title="Organisation">
        <SettingsSection
          title={a.org.name || "Organisation"}
          description="The organisation this session is acting in. Its name, members and everything the bot does are under Workspace, Users and Roles."
        >
          <SettingsRows>
            <SettingsRow label="Your role">{a.role || "member"}</SettingsRow>
            <SettingsRow label="Identifier">
              <span className="font-mono text-xs">{a.org.slug}</span>
            </SettingsRow>
            <SettingsRow label="Signed in with">
              {a.signed_in_with === "slack" ? "Slack" : "Email and password"}
            </SettingsRow>
          </SettingsRows>
        </SettingsSection>
      </SettingsGroup>
    </div>
  );
}

function NameField({ name, onSaved }: { name: string; onSaved: () => void }) {
  const [open, setOpen] = useState(false);
  const [value, setValue] = useState(name);
  const [busy, setBusy] = useState(false);

  if (!open) {
    return (
      <SettingsRows
        action={
          <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
            <PencilLine className="size-4" />
            Edit
          </Button>
        }
      >
        <SettingsRow label="Name" empty="No name set">
          {name}
        </SettingsRow>
      </SettingsRows>
    );
  }

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    try {
      await api.put("/api/account", { name: value.trim() });
      toast.success("Name saved");
      setOpen(false);
      onSaved();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form onSubmit={submit} className="space-y-2">
      <SettingsEditRow label="Name" htmlFor="account-name">
        <Input
          id="account-name"
          autoFocus
          value={value}
          maxLength={120}
          onChange={(e) => setValue(e.target.value)}
          className="max-w-sm"
        />
      </SettingsEditRow>
      <SettingsActions>
        <Button type="submit" size="sm" disabled={busy}>
          {busy && <Loader2 className="animate-spin" />}
          Save
        </Button>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          disabled={busy}
          onClick={() => {
            setValue(name);
            setOpen(false);
          }}
        >
          Cancel
        </Button>
      </SettingsActions>
    </form>
  );
}

function EmailField({ account, onChanged }: { account: Account; onChanged: () => void }) {
  const [busy, setBusy] = useState(false);
  const verified = account.user.email_verified;

  const resend = async () => {
    setBusy(true);
    try {
      await api.post("/api/auth/resend-verification");
      toast.success("Confirmation sent. Check your inbox.");
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
        verified ? undefined : (
          <Button variant="outline" size="sm" onClick={resend} disabled={busy}>
            {busy && <Loader2 className="animate-spin" />}
            Resend
          </Button>
        )
      }
    >
      <SettingsRow label="Address">
        <span className="flex min-w-0 flex-wrap items-center gap-2">
          <span className="truncate">{account.user.email}</span>
          <StatusChip variant={verified ? "success" : "warning"}>
            {verified ? "confirmed" : "unconfirmed"}
          </StatusChip>
        </span>
      </SettingsRow>
    </SettingsRows>
  );
}
