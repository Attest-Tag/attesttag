"use client";

import { useState } from "react";
import { Loader2, TriangleAlert } from "lucide-react";
import { toast } from "sonner";
import { RelativeTime } from "@/components/core/relative-time";
import {
  SettingsActions,
  SettingsEditRow,
  SettingsGroup,
  SettingsRow,
  SettingsRows,
  SettingsSection,
} from "@/components/core/settings-section";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { api, errorMessage, useApi, type OrgAccount } from "@/lib/api";

// Settings → Workspace, last: deleting the account.
//
// It sits under the rename because that is where somebody looks for it, and it is the only thing
// on the page that is not a setting — the rest of the tab changes what the account does, this
// ends it. Three things are on the screen before the button: what the account holds, who owns
// it, and the fact that it does not come back.
//
// The panel asks the server who may press it rather than deciding from a role. The rule is
// ownership — the person who signed the account up, or the administrators once that person has
// left — and a console that worked it out for itself would be a second copy of it to keep in
// step with the one that is enforced.

export function DeleteAccountPanel() {
  const org = useApi<OrgAccount>("/api/org");
  const [open, setOpen] = useState(false);

  if (org.loading && !org.data) {
    return (
      <SettingsGroup title="Danger zone">
        <SettingsSection title="Delete this account" description="Everything in it goes with it.">
          <Skeleton className="h-8 w-full max-w-sm" />
        </SettingsSection>
      </SettingsGroup>
    );
  }
  // A failed read is not a reason to show a delete button the server would refuse anyway.
  if (org.error || !org.data) return null;

  const a = org.data;
  const holds = [
    `${a.members} ${a.members === 1 ? "person" : "people"}`,
    `${a.workspaces} connected ${a.workspaces === 1 ? "workspace" : "workspaces"}`,
  ].join(", ");

  return (
    <SettingsGroup title="Danger zone">
      <SettingsSection
        title="Delete this account"
        description={
          <>
            Everything goes at once: every connected workspace and the app&apos;s place in it,
            every credential, document, routine and thread the bot answered in, and everybody&apos;s
            console. Anyone whose only organisation this was loses their sign-in with it. It cannot
            be undone, and there is no copy to restore from.
          </>
        }
      >
        {open ? (
          <DeleteForm account={a} onCancel={() => setOpen(false)} />
        ) : (
          <SettingsRows
            action={
              a.can_delete ? (
                <Button
                  variant="outline"
                  size="sm"
                  className="border-destructive/40 bg-card text-destructive hover:bg-destructive/10 hover:text-destructive"
                  onClick={() => setOpen(true)}
                >
                  <TriangleAlert className="size-4" />
                  Delete account
                </Button>
              ) : undefined
            }
          >
            <SettingsRow label="Holds">{holds}</SettingsRow>
            <SettingsRow label="Created" empty="Unknown">
              {a.created_at ? <RelativeTime value={a.created_at} /> : null}
            </SettingsRow>
            <SettingsRow label="Made by" empty="Nobody on record">
              {a.owner
                ? `${a.owner.name || a.owner.email}${a.owner.is_you ? " (you)" : ""}${
                    a.owner.is_member ? "" : " — no longer a member"
                  }`
                : null}
            </SettingsRow>
            {!a.can_delete && (
              <SettingsRow label="Who can">{a.why_not}</SettingsRow>
            )}
          </SettingsRows>
        )}
      </SettingsSection>
    </SettingsGroup>
  );
}

// The typed name is the confirmation — a dialog on top of it would be a second Cancel to click
// past, not a second thought. What it buys is the pause to read the name and notice it is the
// wrong account.
function DeleteForm({ account, onCancel }: { account: OrgAccount; onCancel: () => void }) {
  const [typed, setTyped] = useState("");
  const [proof, setProof] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const named = typed.trim().toLowerCase() === account.name.trim().toLowerCase();
  const needsProof = account.proof !== "recent";
  const ready = named && (!needsProof || proof !== "");

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!ready) return;
    setBusy(true);
    setError(null);
    try {
      await api.post("/api/org/delete", {
        confirm: typed,
        password: account.proof === "password" ? proof : "",
        code: account.proof === "code" ? proof : "",
      });
      toast.success(`${account.name} has been deleted`);
      // The session went with the account. Reloading is what turns the console back into the
      // sign-in card rather than a page of requests that now fail.
      window.location.reload();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <form onSubmit={submit} className="space-y-2">
      <SettingsEditRow
        label="Account name"
        htmlFor="delete-confirm"
        hint={
          <>
            Type <span className="font-medium text-foreground">{account.name}</span> to confirm.
          </>
        }
      >
        <Input
          id="delete-confirm"
          autoFocus
          autoComplete="off"
          value={typed}
          onChange={(e) => setTyped(e.target.value)}
          className="max-w-sm"
        />
      </SettingsEditRow>
      {needsProof && (
        <SettingsEditRow
          label={account.proof === "password" ? "Your password" : "Your code"}
          htmlFor="delete-proof"
        >
          <Input
            id="delete-proof"
            type={account.proof === "password" ? "password" : "text"}
            inputMode={account.proof === "password" ? undefined : "numeric"}
            autoComplete={account.proof === "password" ? "current-password" : "one-time-code"}
            value={proof}
            onChange={(e) => setProof(e.target.value)}
            className={account.proof === "password" ? "max-w-sm" : "max-w-32 font-mono tabular-nums"}
          />
        </SettingsEditRow>
      )}
      {error && <p className="px-1 text-xs text-danger">{error}</p>}
      <SettingsActions>
        <Button type="submit" size="sm" variant="destructive" disabled={busy || !ready}>
          {busy && <Loader2 className="animate-spin" />}
          Delete {account.name}
        </Button>
        <Button type="button" variant="ghost" size="sm" disabled={busy} onClick={onCancel}>
          Cancel
        </Button>
      </SettingsActions>
    </form>
  );
}
