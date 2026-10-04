"use client";

import { useState } from "react";
import Link from "next/link";
import { toast } from "sonner";
import { SettingsGroup, SettingsSection } from "@/components/core/settings-section";
import { useAuth } from "@/components/shell/auth-provider";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { api, errorMessage, type SettingsResponse } from "@/lib/api";
import { formatUSD } from "@/lib/format";

// Code review's own two caps: the only review settings that are the organisation's rather than a
// connection's. A review is started by a pull request opening, not by anybody pressing a button, so
// a busy repository is a standing bill nobody approves turn by turn; these keep it inside a share of
// the account's money (settings.go, review_lane.go's reviewMoney).
//
// Each row saves on its own, as a field of Reviews › Settings does, with the same "Inherit: … ·
// from …" line under it: an empty box follows the default, and Reset is how a figure set here goes
// back to it. Changing either needs connections.manage on top of the route's settings.manage — they
// decide what reviews may spend — so the rows read for everybody and edit for an admin.

/** settings.go's defaultReviewDailyUSD: the cap before anybody has chosen one. */
const DAILY_DEFAULT_USD = 10;
/** validateReviewSetting's upper bound. */
const MAX_USD = 100_000;

type Field = "review_monthly_budget_usd" | "review_daily_usd";

export function ReviewBudgetGroup({
  settings,
  onSaved,
}: {
  settings: SettingsResponse;
  onSaved: () => void;
}) {
  const { me } = useAuth();
  // /api/me sends only the keys a role holds, so a missing one is a no; no map yet shows the form.
  const perms = me?.user?.permissions;
  const canEdit = !perms || (perms["settings.manage"] === true && perms["connections.manage"] === true);
  const stored = settings.stored ?? {};
  const account = settings.effective.EffectiveBudgetUSD;
  // A deployment without code review has no reviews to budget for.
  if (me?.code_review?.reason === "off") return null;

  return (
    <SettingsGroup
      title="Code review budget"
      accessory={
        <Link href="/reviews" className="text-xs text-muted-foreground underline-offset-2 hover:text-foreground hover:underline">
          Reviews
        </Link>
      }
    >
      <BudgetRow
        field="review_monthly_budget_usd"
        title="Monthly review budget"
        description={
          <>
            What code reviews may spend in a calendar month, inside the account&rsquo;s monthly budget
            rather than on top of it. Reviews start from pull requests, not from anybody asking, so this
            keeps a busy repository from spending the money the bot&rsquo;s conversations run on. 0 is no
            cap of its own; the account&rsquo;s budget and credit still apply.
          </>
        }
        stored={stored.review_monthly_budget_usd ?? ""}
        inherited={account / 2}
        from={account > 0 ? `half the account's ${formatUSD(account)} monthly budget` : "the account has no monthly budget"}
        per="a month"
        canEdit={canEdit}
        onSaved={onSaved}
      />
      <BudgetRow
        field="review_daily_usd"
        title="Daily review cap"
        description={
          <>
            What code reviews may spend in a day, counted in UTC. $10 is ten reviews at the default $1
            each; a review loop gone wrong stops here by lunchtime instead of at the end of the month. 0
            is no cap of its own.
          </>
        }
        stored={stored.review_daily_usd ?? ""}
        inherited={DAILY_DEFAULT_USD}
        from="built-in default"
        per="a day"
        canEdit={canEdit}
        onSaved={onSaved}
      />
      {!canEdit && (
        <p className="px-6 py-3 text-[13px] leading-relaxed text-muted-foreground">
          Changing these needs the settings and connections permissions together: they decide what
          reviews may spend, as the model and the most one review may spend do under Reviews › Settings.
        </p>
      )}
    </SettingsGroup>
  );
}

function BudgetRow({
  field,
  title,
  description,
  stored,
  inherited,
  from,
  per,
  canEdit,
  onSaved,
}: {
  field: Field;
  title: string;
  description: React.ReactNode;
  /** What the organisation saved; "" follows the default. */
  stored: string;
  /** What an empty box comes to, and where that comes from. */
  inherited: number;
  from: string;
  per: string;
  canEdit: boolean;
  onSaved: () => void;
}) {
  // The box follows what is stored after a save or a Reset, in place rather than by remounting the
  // row, so focus stays in the box Enter was pressed in.
  const [draft, setDraft] = useState(stored);
  const [seen, setSeen] = useState(stored);
  if (seen !== stored) {
    setSeen(stored);
    setDraft(stored);
  }
  const [busy, setBusy] = useState(false);
  const trimmed = draft.trim();
  const value = Number(trimmed);
  const invalid = trimmed !== "" && !(Number.isFinite(value) && value >= 0 && value <= MAX_USD);
  const unchanged = trimmed === stored.trim();
  const id = `${field}-input`;
  const shown = (n: number) => (n > 0 ? `${formatUSD(n)} ${per}` : "no cap of its own");

  const put = async (next: string) => {
    setBusy(true);
    try {
      await api.put("/api/settings", { [field]: next });
      toast.success(next === "" ? `${title} follows the default again` : `${title} saved`);
      onSaved();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!canEdit || busy || invalid || unchanged) return;
    void put(trimmed);
  };

  return (
    <SettingsSection title={title} description={description}>
      <div className="space-y-1.5">
        <form onSubmit={submit} className="flex gap-2">
          <div className="relative w-full max-w-40">
            <span className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-sm text-muted-foreground">
              $
            </span>
            <Input
              id={id}
              aria-label={`${title} in USD`}
              aria-invalid={invalid}
              type="number"
              inputMode="decimal"
              min={0}
              max={MAX_USD}
              step="0.01"
              className="pl-7 tabular-nums"
              // The default as the placeholder, so an empty box reads as the figure it comes to.
              placeholder={inherited.toFixed(2)}
              value={draft}
              // Read-only while it saves rather than disabled, which would drop the focus Enter left here.
              disabled={!canEdit}
              readOnly={busy}
              onChange={(e) => setDraft(e.target.value)}
            />
          </div>
          <Button type="submit" variant="outline" disabled={!canEdit || invalid || unchanged} loading={busy}>
            Save
          </Button>
        </form>
        {invalid && <p className="text-xs text-danger">From $0 to {formatUSD(MAX_USD)}.</p>}
        {/* After the input in the DOM, as Reviews › Settings puts Reset: tabbing out of the box lands
            on it rather than past it. */}
        {stored === "" ? (
          <p className="text-xs text-muted-foreground">
            Inherit: <span className="font-medium text-foreground">{shown(inherited)}</span> · {from}
          </p>
        ) : (
          <p className="flex flex-wrap items-center gap-x-1.5 text-xs text-muted-foreground">
            <span className="font-medium text-foreground">Set here</span>
            {canEdit ? (
              <>
                <span aria-hidden>·</span>
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => void put("")}
                  className="font-medium text-primary underline-offset-2 hover:underline disabled:opacity-50"
                >
                  Reset
                </button>
                <span>
                  to {shown(inherited)} · {from}
                </span>
              </>
            ) : (
              <span>
                · {shown(Number(stored))} · the default is {shown(inherited)}
              </span>
            )}
          </p>
        )}
      </div>
    </SettingsSection>
  );
}
