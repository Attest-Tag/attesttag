"use client";

import { useState } from "react";
import { Loader2 } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { api, errorMessage } from "@/lib/api";

/**
 * Upgrade. One press and the server writes the support request that asks for the Pro plan,
 * names the organisation in it and sends it; the operator's link goes into that message and
 * nowhere near this screen.
 *
 * It was a `mailto:` that opened a draft carrying that link — useless without the operator
 * secret, but still the URL that moves an account between plans, in front of the account
 * asking to be moved, at the cost of three steps in a mail client.
 *
 * `askedAt` is the organisation's own record that a request went out (`plan_request_at`), not
 * this browser's: the banner is on every page, and without it the second page would offer the
 * button again and the operator would get the same message twice.
 *
 * `type="button"`: this sits inside the settings form, and a bare button in a form submits it.
 */
export function UpgradeButton({
  support,
  askedAt,
  note = true,
}: {
  support: string;
  /** When the account last asked, from the server. Empty or absent means it has not. */
  askedAt?: string | null;
  /** Whether to say where the request went once it has gone. */
  note?: boolean;
}) {
  const [busy, setBusy] = useState(false);
  const [justAsked, setJustAsked] = useState(false);
  const asked = justAsked || !!askedAt;

  const ask = async () => {
    setBusy(true);
    try {
      const res = await api.post<{ delivered?: boolean }>("/api/plan/raise-budget");
      setJustAsked(true);
      toast.success(
        res.delivered === false
          ? `Email is not configured on this deployment — write to ${support}.`
          : "Upgrade requested. Support replies to the address you sign in with.",
      );
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
      {/* The primary variant: on the banner it is the way out of a stopped bot, and in Settings
          it is the only thing on the page that can move the plan. */}
      <Button type="button" size="sm" onClick={ask} disabled={busy || asked}>
        {busy && <Loader2 className="animate-spin" />}
        {asked ? "Requested" : "Upgrade"}
      </Button>
      {asked && note && (
        <span className="text-xs text-muted-foreground">Sent to {support}.</span>
      )}
    </div>
  );
}
