"use client";

import Link from "next/link";
import { useEffect, useState } from "react";
import { CheckCircle2, CreditCard, ExternalLink, Loader2, Receipt, Wallet } from "lucide-react";
import { toast } from "sonner";
import { EmptyState } from "@/components/core/empty-state";
import { ErrorBanner } from "@/components/core/error-banner";
import {
  SettingsGroup,
  SettingsRow,
  SettingsRows,
  SettingsSection,
} from "@/components/core/settings-section";
import { StatCard } from "@/components/core/stat-card";
import { StatusChip, type StatusChipVariant } from "@/components/core/status-chip";
import { TableSkeleton } from "@/components/core/table-skeleton";
import { useAuth } from "@/components/shell/auth-provider";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  api,
  errorMessage,
  useApi,
  type ActiveUserCount,
  type BillingSize,
  type BillingView,
  type EnterpriseDeal,
  type SizeChangeResult,
  type CreditEntry,
} from "@/lib/api";
import { formatDate, formatDateTime, formatNumber, formatUSD } from "@/lib/format";

/**
 * Settings → Billing: what the plan costs, what credit is left, and the two buttons that buy
 * more of either.
 *
 * Its own endpoint rather than the batched settings form, like Security and Users — nothing here
 * is a key/value setting except the monthly budget, which saves through a one-field PUT.
 *
 * Two things this file deliberately does not do. It never works out which limit stopped the bot;
 * the server sends `paused_by`, because the rule belongs to the code that refuses the turn and a
 * second copy of it here would drift. And it never treats coming back from Stripe as proof of
 * payment: `?checkout=success` only starts the settling state, and the balance moves when the
 * server says it has.
 */
export function BillingPanel({ onSaved }: { onSaved: () => void }) {
  const { me, reload: reloadMe } = useAuth();
  const [settleParams, setSettleParams] = useState("");
  const { data, error, loading, reload } = useApi<BillingView>(`/api/billing${settleParams}`);
  const [returned, setReturned] = useState<"" | "success" | "cancelled">("");
  // gaveUp rather than a `settling` flag, so nothing has to set state in response to state.
  // Whether we are still settling is derived below: it is true while the server says a payment is
  // outstanding and we have not run out of patience.
  const [gaveUp, setGaveUp] = useState(false);

  // The permission check is the stricter of the two idioms in this console: deny when the map
  // exists and the key is absent, rather than allow unless explicitly denied. For a button that
  // charges a card, that is the right way round.
  const perms = me?.user?.permissions;
  const canManage = !perms || perms["billing.manage"] === true;
  const canSettings = !perms || perms["settings.manage"] === true;

  // Read the Stripe round-trip off the URL once, then take it out of the address bar so a reload
  // does not replay the notice. Same replaceState idiom as the tab parameter, and for the same
  // reason: this is a static export with no location at build time.
  useEffect(() => {
    const q = new URLSearchParams(window.location.search);
    const outcome = q.get("checkout");
    if (outcome !== "success" && outcome !== "cancelled") return;
    const session = q.get("session") ?? "";
    // eslint-disable-next-line react-hooks/set-state-in-effect -- one-shot URL read, as useTabParam does
    setReturned(outcome);
    if (outcome === "success") {
      // Ask the server to read the session back from Stripe once. That removes the race with the
      // webhook rather than papering over it, and it is idempotent with the webhook because both
      // key the ledger on the payment itself.
      setSettleParams(session ? `?settle=1&session=${encodeURIComponent(session)}` : "?settle=1");
    }
    const url = new URL(window.location.href);
    url.searchParams.delete("checkout");
    url.searchParams.delete("session");
    window.history.replaceState(null, "", url);
  }, []);

  // A cancelled checkout is not a failure — nothing was charged and nothing changed — so it is a
  // plain toast rather than an error, and nothing on the page.
  useEffect(() => {
    if (returned === "cancelled") toast("Payment cancelled — nothing was charged.");
  }, [returned]);

  // Three states after a payment, and they are different facts rather than degrees of the same
  // one. The server settles the session on the first load, so the ordinary answer is that the
  // money is already in the figures below — that is `landed`, and it gets a confirmation rather
  // than a spinner. Only a payment the server could not confirm is still pending, and only one
  // that stays pending past the backoff gets the apology.
  const settling = returned === "success" && !gaveUp && data?.pending_checkout === true;
  const landed = returned === "success" && !!data && !data.pending_checkout;

  // While a payment is settling, poll on a bounded backoff. Five tries over about thirty seconds,
  // then stop and say so: a spinner with no end is worse than a sentence admitting it is late.
  useEffect(() => {
    if (!settling) return;
    const timers = [1500, 3000, 5000, 8000, 13000].map((d) =>
      window.setTimeout(() => reload(), d),
    );
    timers.push(window.setTimeout(() => setGaveUp(true), 34000));
    return () => timers.forEach((t) => window.clearTimeout(t));
  }, [settling, reload]);

  // Once it has landed, refresh the session the shell draws its banner from. Without this,
  // somebody who has just paid watches the balance go green while a red strip above it still says
  // the bot has stopped.
  useEffect(() => {
    if (landed) reloadMe?.();
  }, [landed, reloadMe]);

  if (error) return <ErrorBanner message={error} onRetry={reload} />;
  if (!data) return <TableSkeleton rows={4} columns={3} />;

  const credit = data.credit;
  // Against everything spendable, not the prepaid half: an account whose plan allowance covers
  // the month is not low on credit, and telling it so would send somebody to buy what they have.
  const lowOnCredit =
    credit.metered && credit.spendable_usd > 0 && credit.spendable_usd <= data.budget.month_spend_usd;

  return (
    <div className="space-y-5">
      {returned === "success" && (
        <SettlingNotice settling={settling} landed={landed} onRefresh={reload} />
      )}

      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <StatCard
          label="Credit"
          value={formatUSD(credit.spendable_usd)}
          hint={
            !credit.metered
              ? "Not metered on this plan"
              : data.paused_by === "credit"
                ? "Spent — the bot has stopped"
                : credit.allowance_usd > 0
                  ? // The split, on the one screen where it matters: what expires and what does
                    // not. Said here rather than left to the plan section, because this is the
                    // number somebody reads before deciding whether to top up.
                    `${formatUSD(credit.allowance_usd)} included this month, ${formatUSD(credit.balance_usd)} prepaid`
                  : lowOnCredit
                    ? "Less than this month's spend so far"
                    : "Prepaid, at the provider's price"
          }
          hintTone={data.paused_by === "credit" || lowOnCredit ? "danger" : "muted"}
        />
        <StatCard
          label="Spend this month"
          value={formatUSD(data.budget.month_spend_usd)}
          hint={
            data.budget.effective_budget_usd > 0
              ? `of ${formatUSD(data.budget.effective_budget_usd)} budget`
              : "No monthly budget set"
          }
          hintTone={data.paused_by === "budget" ? "danger" : "muted"}
          href="/activity?range=month"
        />
        <StatCard
          label={`Users, last ${data.users.window_days} days`}
          value={formatNumber(data.users.users)}
          hint={
            data.users.limit > 0
              ? data.users.over
                ? `more than the ${formatNumber(data.users.limit)} allowed`
                : `of ${formatNumber(data.users.limit)} allowed`
              : "People who used the bot"
          }
          hintTone={data.users.over ? "danger" : "muted"}
          href="/activity?tab=people&range=30d"
        />
        <StatCard
          label="Fix jobs this month"
          value={formatNumber(data.jobs.used)}
          hint={
            data.jobs.limit > 0
              ? data.jobs.used > data.jobs.limit
                ? `more than the ${formatNumber(data.jobs.limit)} on this plan`
                : `of ${formatNumber(data.jobs.limit)} on this plan`
              : "Container runs that raised a pull request"
          }
          hintTone={data.jobs.limit > 0 && data.jobs.used > data.jobs.limit ? "danger" : "muted"}
          href="/jobs"
        />
      </div>

      {data.users.over && (
        <OverLimitNotice
          users={data.users}
          canManage={canManage}
          enterprise={!!data.enterprise}
          supportEmail={data.support_email}
        />
      )}

      {data.paused_by !== "" && <PausedNotice data={data} />}

      {data.enterprise ? (
        <EnterprisePlanGroup
          data={data}
          deal={data.enterprise}
          canManage={canManage}
          loading={loading}
        />
      ) : (
        <PlanGroup data={data} canManage={canManage} loading={loading} reload={reload} />
      )}
      <CreditGroup
        data={data}
        canManage={canManage}
        canSettings={canSettings}
        onSaved={onSaved}
        reload={reload}
      />
      <LedgerGroup entries={data.ledger} />
      <PaymentsGroup data={data} canManage={canManage} />

      {!canManage && (
        <p className="px-1 text-[13px] text-muted-foreground">
          Only someone with the billing permission can change the plan or buy credit. Everything
          above is what this account is on and what it has left.
        </p>
      )}
    </div>
  );
}

/**
 * The state that exists because webhooks are asynchronous: Stripe has the money and this console
 * does not know it yet.
 *
 * Driven by the URL parameter as well as the server flag, deliberately — otherwise the one screen
 * nobody can reach without a real card is the one most likely to be wrong. Typing
 * `?tab=billing&checkout=success` gets you here.
 */
function SettlingNotice({
  settling,
  landed,
  onRefresh,
}: {
  settling: boolean;
  landed: boolean;
  onRefresh: () => void;
}) {
  if (landed) {
    return (
      <Card className="border-primary/40 bg-accent/40 p-4">
        {/* The tick sits where the spinner sat a moment ago, so the one card reads as the same
            thing finishing rather than as a second card replacing the first. */}
        <p className="flex items-center gap-2 text-sm font-medium text-foreground">
          <CheckCircle2 className="size-4 text-success" aria-hidden />
          Payment received — thank you.
        </p>
      </Card>
    );
  }
  if (settling) {
    return (
      <Card className="border-primary/40 bg-accent/40 p-4">
        <p className="flex items-center gap-2 text-sm font-medium text-foreground">
          <Loader2 className="size-4 animate-spin" aria-hidden />
          Payment received. Updating your account…
        </p>
        <p className="mt-1 text-[13px] text-muted-foreground">
          Stripe has taken the payment. The balance below catches up as soon as their confirmation
          reaches us — usually a few seconds.
        </p>
      </Card>
    );
  }
  return (
    <Card className="border-warning/40 bg-warning-soft p-4">
      <p className="text-sm font-medium text-foreground">
        Payment received. The account has not caught up yet.
      </p>
      <p className="mt-1 text-[13px] text-muted-foreground">
        Stripe has the payment and it will land on its own — nothing is lost. If the balance below
        is still wrong in a few minutes, refresh.
      </p>
      <Button type="button" variant="outline" size="sm" className="mt-3" onClick={onRefresh}>
        Refresh
      </Button>
    </Card>
  );
}

/**
 * What a size throws in, as one line: credit, jobs, or both. Nothing for a size that includes
 * neither, so the row reads as what it is rather than promising "$0.00".
 */
/**
 * The rest of a "Credit included" line: what is left of this month's allowance and when it resets.
 * When this month was granted something other than the monthly figure beside it — the size changed
 * part-way through, or credit was added — that amount is named too: "$10.00 a month — $59.98 left"
 * read as arithmetic that could not be right.
 */
function AllowanceLeft({ credit, monthly }: { credit: BillingView["credit"]; monthly: number }) {
  const granted = credit.allowance_granted_usd;
  return (
    <>
      {" — "}
      {granted > 0 && Math.abs(granted - monthly) >= 0.005 && <>{formatUSD(granted)} this month, </>}
      {formatUSD(credit.allowance_usd)} left, resets {formatDate(credit.allowance_expires)}
    </>
  );
}

function sizeIncludes(b: BillingSize): string {
  const parts = [
    b.included_usd > 0 ? `${formatUSD(b.included_usd)} of model credit` : "",
    b.jobs_per_month > 0 ? `${formatNumber(b.jobs_per_month)} fix jobs` : "",
  ].filter(Boolean);
  return parts.length ? `Includes ${parts.join(" and ")} a month` : "";
}

/** Which limit stopped the bot, in a sentence, with the other one named so nobody chases it. */
/**
 * Shown when more people used the bot than the size is sold for.
 *
 * Warning-coloured and not danger-coloured, because nothing has happened: no turn was refused and
 * none will be. Refusing the twenty-sixth person would mean telling somebody mid-conversation in
 * Slack that they are surplus, for a reason only an admin can fix — so the product tells the
 * admin instead, which is the only person who can act on it.
 */
function OverLimitNotice({
  users,
  canManage,
  enterprise,
  supportEmail,
}: {
  users: ActiveUserCount;
  canManage: boolean;
  /** An enterprise agreement has no larger size on this screen to move to: it is changed by
   *  talking to whoever wrote it, so the sentence says that instead. */
  enterprise: boolean;
  supportEmail: string;
}) {
  return (
    <div className="rounded-xl border border-warning/30 bg-warning-soft px-4 py-3">
      <p className="text-sm text-warning">
        <span className="font-medium">
          {formatNumber(users.users)} people used the bot in the last {users.window_days} days.
          Your {enterprise ? "agreement is for" : "plan size allows"} {formatNumber(users.limit)}.
        </span>{" "}
        Nothing has been stopped and nobody has been turned away.{" "}
        {enterprise
          ? `To change the agreement, write to ${supportEmail || "whoever runs this deployment"}.`
          : canManage
            ? "Move to a larger plan size on the line below whenever it suits you."
            : "Someone who can manage billing can move the account to a larger plan size."}
      </p>
    </div>
  );
}

function PausedNotice({ data }: { data: BillingView }) {
  const credit = data.paused_by === "credit";
  return (
    <div className="rounded-xl border border-danger/30 bg-danger-soft px-4 py-3">
      <p className="text-sm text-danger">
        <span className="font-medium">The bot has stopped replying.</span>{" "}
        {credit ? (
          <>
            This account is out of credit. Your own monthly budget is not what stopped it —{" "}
            {formatUSD(Math.max(0, data.budget.effective_budget_usd - data.budget.month_spend_usd))}{" "}
            of it is unspent.
          </>
        ) : (
          <>
            It reached the {formatUSD(data.budget.effective_budget_usd)} monthly budget this account
            set. Credit is unaffected: {formatUSD(data.credit.balance_usd)} is still there.
          </>
        )}
      </p>
    </div>
  );
}

/**
 * Hand the browser to Stripe's billing portal: the card, the invoices and cancellation.
 *
 * Changing size used to be here too and no longer is — see ChangeSize for why a portal nobody
 * had configured for it was the wrong place to send somebody who wanted a bigger plan.
 */
function useStripePortal() {
  const [busy, setBusy] = useState(false);
  const open = async () => {
    setBusy(true);
    try {
      const res = await api.post<{ url: string }>("/api/billing/portal", {});
      // assign, not replace, so Stripe's Back button comes home.
      window.location.assign(res.url);
    } catch (err) {
      setBusy(false);
      toast.error(errorMessage(err));
    }
  };
  return { busy, open };
}

/**
 * A button with the sentence that says what pressing it will do, stacked under it.
 *
 * Not SettingsActions: that is a grid row of buttons for the panel level, so a paragraph inside
 * it is laid out beside the button with no width to wrap against and runs off the edge. These sit
 * inside a section's control column, where the column is the width.
 */
function ActionNote({ children, note }: { children: React.ReactNode; note?: React.ReactNode }) {
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-2">{children}</div>
      {note && <p className="text-[13px] leading-relaxed text-muted-foreground">{note}</p>}
    </div>
  );
}

const STATUS_TONE: Record<string, StatusChipVariant> = {
  active: "success",
  trialing: "success",
  comped: "success",
  past_due: "danger",
  unpaid: "danger",
  canceled: "neutral",
};

// The statuses that mean there is no plan any more, only a record that there was one. They are
// the ones the webhook moves an account back to free on, and the screen has to agree: a summary
// reading "canceled" with no picker under it is a dead end, and the person looking at it is the
// one most likely to want to buy again. past_due and incomplete are deliberately not here —
// Stripe is still retrying the card and the plan is still theirs.
const ENDED = new Set(["canceled", "unpaid", "incomplete_expired"]);

function PlanGroup({
  data,
  canManage,
  loading,
  reload,
}: {
  data: BillingView;
  canManage: boolean;
  loading: boolean;
  reload: () => void;
}) {
  const sub = data.subscription && !ENDED.has(data.subscription.status) ? data.subscription : null;
  const ended = data.subscription && ENDED.has(data.subscription.status) ? data.subscription : null;
  const [size, setSize] = useState(() => data.sizes.find((b) => b.available)?.key ?? "");
  const [busy, setBusy] = useState(false);
  const chosen = data.sizes.find((b) => b.key === size);

  const buy = async () => {
    setBusy(true);
    try {
      const res = await api.post<{ url: string }>("/api/billing/checkout", {
        kind: "subscription",
        size,
      });
      // assign, not replace, so Stripe's Back button comes home.
      window.location.assign(res.url);
    } catch (err) {
      setBusy(false);
      toast.error(errorMessage(err));
    }
  };

  // The size a scheduled downgrade moves to, when it is one this deployment still sells. The
  // server fills pending_size_label even for a retired price, but only a size we still sell has
  // a price to quote.
  const pendingSize =
    sub?.pending_size && sub.pending_at
      ? data.sizes.find((b) => b.key === sub.pending_size)
      : undefined;

  if (sub) {
    // Changing size is one button, so it goes on the line it changes rather than in a section of
    // its own — a heading, a paragraph and a Show more for a single link out was more furniture
    // than the act deserves. The sentence under the readout carries what the paragraph said.
    return (
      <SettingsGroup title="Plan">
        <SettingsSection
          title="Your plan"
          description={
            sub.comped
              ? "Granted rather than bought, so there is no subscription behind it and nothing is being charged. The size is a record of what it would cost."
              : "Change plan size on the line below whenever it suits you. Moving up takes effect and is charged straight away; moving down waits until the end of the month you have already paid for. Cancellation is in the card portal."
          }
        >
          <SettingsRows>
            <SettingsRow label="Plan">
              <StatusChip variant={STATUS_TONE[sub.status] ?? "neutral"}>
                {sub.comped ? "Pro — comped" : sub.status === "active" ? "Pro" : sub.status}
              </StatusChip>
            </SettingsRow>
            <SettingsRow label="Plan size">
              <span className="flex flex-wrap items-center gap-x-3 gap-y-1">
                <span>
                  {sub.size_label || "—"}
                  {/* A scheduled move has to outlive the toast that announced it. Somebody who
                      downgrades, reloads and finds no trace of it will simply ask again — which
                      was the whole of the gap this fills.

                      The label above is still what they are ON and billed for until the date;
                      the line below is what they move to. Rendering pending_size_label rather
                      than looking the key up is deliberate: the server fills it even for a price
                      this deployment no longer sells, where it reads "another plan size" and the
                      key is empty. */}
                  {sub.pending_at && (
                    <span className="mt-0.5 block text-xs font-normal text-muted-foreground">
                      Moves to {sub.pending_size_label || "another plan size"} on{" "}
                      {formatDate(sub.pending_at)}. You keep this one, and everything it includes,
                      until then.
                    </span>
                  )}
                </span>
                <ChangeSize
                  comped={sub.comped}
                  canManage={canManage}
                  hasCustomer={!!data.stripe?.customer_id}
                  sizes={data.sizes}
                  current={sub.size}
                  periodEnd={sub.period_end}
                  pendingAt={sub.pending_at}
                  supportEmail={data.support_email}
                  askedAt={data.size_request?.at}
                  reload={reload}
                />
              </span>
            </SettingsRow>
            <SettingsRow label="Monthly fee">
              {sub.comped ? (
                "Nothing — this plan was granted"
              ) : (
                <>
                  {formatUSD(sub.amount_usd)}
                  {/* A scheduled move is always a downgrade, so the fee above is what is charged
                      until the date and this is what replaces it. Only when the waiting size is
                      one this deployment still sells: for a retired price the server has a date
                      and a label but no key, and there is no honest figure to put here. The plan
                      size line still says the move is coming, so nothing is hidden by leaving
                      this off — it just does not invent a price. */}
                  {pendingSize && sub.pending_at && (
                    <span className="mt-0.5 block text-xs font-normal text-muted-foreground">
                      {formatUSD(pendingSize.price_usd)} from {formatDate(sub.pending_at)}
                    </span>
                  )}
                </>
              )}
            </SettingsRow>
            {/* Only when the size includes something. A row reading "$0.00" would read as a
                promise that was not kept, where no row at all reads as what it is: this size
                includes no credit and all of it is bought. */}
            <SettingsRow label="Users in plan">
              {data.users.limit > 0
                ? `${formatNumber(data.users.users)} of ${formatNumber(data.users.limit)} allowed, last ${data.users.window_days} days`
                : `${formatNumber(data.users.users)} in the last ${data.users.window_days} days — no limit on this plan size`}
            </SettingsRow>
            {sub.included_usd > 0 && (
              <SettingsRow label="Credit included">
                {formatUSD(sub.included_usd)} a month
                {data.credit.allowance_expires && (
                  <AllowanceLeft credit={data.credit} monthly={sub.included_usd} />
                )}
                <span className="mt-0.5 block text-xs text-muted-foreground">
                  Included credit is for the month it comes with and does not carry over. Credit
                  you buy is spent only once this is gone, and never expires.
                </span>
              </SettingsRow>
            )}
            {/* A comped plan is not renewing, because nothing is charging it. A date there would
                be wrong and a dash would be noise, so the row is simply not one of its facts. */}
            {!sub.comped && (
              <SettingsRow label={sub.cancel_at_period_end ? "Ends" : "Renews"}>
                {sub.period_end ? formatDate(sub.period_end) : "—"}
              </SettingsRow>
            )}
          </SettingsRows>
        </SettingsSection>
      </SettingsGroup>
    );
  }

  return (
    <SettingsGroup title="Plan">
      <SettingsSection
        title="Choose a plan"
        description={
          <>
            {ended && (
              <>
                Your {ended.size_label || "previous"} plan has ended, and any credit you had
                bought is still on the balance. Picking a plan size below starts a new subscription.{" "}
              </>
            )}
            Your plan size is how many people may use the bot. We count the ones who actually did —
            anybody in a connected workspace who asked it something in the last 30 days, which is
            the figure on the tile above — and show you the number rather than asking you to
            declare it. Going over does not stop anything and does not change the fee on its own:
            you change size on this screen whenever you like, and Stripe prorates the difference
            on the next invoice. Each size includes model credit every month and a number of fix jobs:
            what you spend beyond the credit is bought as a top-up below, unspent credit stays on the
            balance rather than expiring, and the jobs are counted above and never refused.
          </>
        }
      >
        <Card className="divide-y py-0">
          {data.sizes.map((b) => (
            <label
              key={b.key}
              className="flex cursor-pointer items-start gap-3 px-4 py-3 has-[:disabled]:cursor-default has-[:disabled]:opacity-60"
            >
              <input
                type="radio"
                name="size"
                value={b.key}
                checked={size === b.key}
                disabled={!canManage || (!b.available && !data.support_email)}
                onChange={() => setSize(b.key)}
                className="mt-0.5"
              />
              <span className="min-w-0 flex-1 text-sm font-medium text-foreground">
                {b.label}
                {sizeIncludes(b) !== "" && (
                  <span className="mt-0.5 block text-xs font-normal text-muted-foreground">
                    {sizeIncludes(b)}
                  </span>
                )}
              </span>
              <span className="font-mono text-sm tabular-nums text-muted-foreground">
                {b.available ? `${formatUSD(b.price_usd)} a month` : "Talk to us"}
              </span>
            </label>
          ))}
        </Card>
        {canManage && chosen && !chosen.available && (
          <div className="mt-4">
            <TalkToUs
              size={size}
              supportEmail={data.support_email}
              askedAt={data.size_request?.at}
              disabled={loading}
              onSent={reload}
            />
          </div>
        )}
        {canManage && (!chosen || chosen.available) && (
          <div className="mt-4">
            <ActionNote
              note={
                chosen && (
                  <>
                    You are subscribing to {chosen.label} at {formatUSD(chosen.price_usd)} a month.
                    {chosen.included_usd > 0 &&
                      ` ${formatUSD(chosen.included_usd)} of model credit is added to your balance when the first
                        payment goes through, and again at every renewal; spending past it is bought as a top-up.`}
                    {chosen.jobs_per_month > 0 &&
                      ` It is sold with ${formatNumber(chosen.jobs_per_month)} fix jobs a month, counted on this screen.`}{" "}
                    It renews monthly until you cancel, and you cancel on this screen. Stripe takes
                    the payment — we never see the card.
                  </>
                )
              }
            >
              <Button type="button" disabled={!size || busy || loading} onClick={buy}>
                {busy && <Loader2 className="size-4 animate-spin" aria-hidden />}
                Continue to payment
              </Button>
            </ActionNote>
          </div>
        )}
      </SettingsSection>
    </SettingsGroup>
  );
}

/**
 * The plan, for an account on an enterprise agreement.
 *
 * There is nothing on the ladder for it to buy: its figures are its own, written by whoever runs
 * this deployment. So this is a readout of the deal and the one thing the account can do about
 * paying for it — subscribe to its own price, open the page it was given to pay at, or nothing at
 * all when it is invoiced. Changing the deal is a conversation, and the section says with whom.
 */
function EnterprisePlanGroup({
  data,
  deal,
  canManage,
  loading,
}: {
  data: BillingView;
  deal: EnterpriseDeal;
  canManage: boolean;
  loading: boolean;
}) {
  const [busy, setBusy] = useState(false);
  const sub = data.subscription;
  const per = deal.interval === "year" ? "a year" : "a month";
  const contact = data.support_email || "whoever runs this deployment";

  const subscribe = async () => {
    setBusy(true);
    try {
      const res = await api.post<{ url: string }>("/api/billing/checkout", { kind: "enterprise" });
      // assign, not replace, so Stripe's Back button comes home.
      window.location.assign(res.url);
    } catch (err) {
      setBusy(false);
      toast.error(errorMessage(err));
    }
  };

  // What the plan chip says. A deal paid by subscription is not live until it is subscribed to,
  // and saying "Enterprise" in green before then would tell somebody it is paid for.
  const awaiting = deal.paid_by === "subscription" && !deal.subscribed;
  const lapsed = !!sub && ENDED.has(sub.status);
  const chip: { variant: StatusChipVariant; label: string } = lapsed
    ? { variant: "danger", label: `Enterprise — ${sub?.status.replace("_", " ")}` }
    : awaiting
      ? { variant: "neutral", label: "Enterprise — not yet subscribed" }
      : sub?.status === "past_due"
        ? { variant: "danger", label: "Enterprise — payment failed" }
        : { variant: "success", label: "Enterprise" };

  return (
    <SettingsGroup title="Plan">
      <SettingsSection
        title="Your plan"
        description={
          <>
            An enterprise agreement: the figures below were written for this account rather than
            picked from a list, and the plan does not change when a payment does. To change any of
            them, write to {contact}.
          </>
        }
      >
        <SettingsRows>
          <SettingsRow label="Plan">
            <StatusChip variant={chip.variant}>{chip.label}</StatusChip>
          </SettingsRow>
          <SettingsRow label="Users in plan">
            {deal.user_limit > 0
              ? `${formatNumber(data.users.users)} of ${formatNumber(deal.user_limit)} in the agreement, last ${data.users.window_days} days`
              : `${formatNumber(data.users.users)} in the last ${data.users.window_days} days — no limit in the agreement`}
          </SettingsRow>
          {deal.job_limit > 0 && (
            <SettingsRow label="Fix jobs">
              {formatNumber(deal.job_limit)} a month — {formatNumber(data.jobs.used)} so far this
              month
            </SettingsRow>
          )}
          {deal.included_usd > 0 && (
            <SettingsRow label="Credit included">
              {formatUSD(deal.included_usd)} a month
              {data.credit.allowance_expires ? (
                <AllowanceLeft credit={data.credit} monthly={deal.included_usd} />
              ) : (
                awaiting && " — from the day the subscription starts"
              )}
              <span className="mt-0.5 block text-xs text-muted-foreground">
                Included credit is for the month it comes with and does not carry over. Credit
                bought or granted is spent only once this is gone, and never expires.
              </span>
            </SettingsRow>
          )}
          <SettingsRow label="Fee">
            {deal.fee_usd > 0 ? `${formatUSD(deal.fee_usd)} ${per}` : "As agreed"}
          </SettingsRow>
          {deal.subscribed && sub?.period_end && (
            <SettingsRow label={sub.cancel_at_period_end ? "Ends" : "Renews"}>
              {formatDate(sub.period_end)}
            </SettingsRow>
          )}
          <SettingsRow label="Payment">
            {deal.paid_by === "subscription"
              ? deal.subscribed
                ? "By subscription — the card and invoices are under Payments below"
                : "By subscription, started from this screen"
              : deal.paid_by === "link"
                ? "At the payment page you were given"
                : `By invoice from ${contact} — there is nothing to pay on this screen`}
          </SettingsRow>
        </SettingsRows>

        {canManage && deal.paid_by === "subscription" && !deal.subscribed && (
          <div className="mt-4">
            <ActionNote
              note={
                <>
                  Subscribes this account to its agreement
                  {deal.fee_usd > 0 && ` at ${formatUSD(deal.fee_usd)} ${per}`}. It renews until
                  you cancel, and you cancel under Payments below. Stripe takes the payment — we
                  never see the card.
                </>
              }
            >
              <Button type="button" disabled={busy || loading} onClick={subscribe}>
                {busy && <Loader2 className="size-4 animate-spin" aria-hidden />}
                Subscribe
              </Button>
            </ActionNote>
          </div>
        )}
        {canManage && deal.paid_by === "link" && deal.pay_url && (
          <div className="mt-4">
            <ActionNote
              note={
                <>
                  Opens the payment page set up for this account in a new tab. A payment made there
                  is confirmed by hand, so it can take a working day to show here.
                </>
              }
            >
              <Button asChild>
                <a href={deal.pay_url} target="_blank" rel="noopener noreferrer">
                  <ExternalLink className="size-4" aria-hidden />
                  Pay
                </a>
              </Button>
            </ActionNote>
          </div>
        )}
      </SettingsSection>
    </SettingsGroup>
  );
}

/**
 * Changing size, as a picker that opens on the Size line.
 *
 * This used to be a link into Stripe's billing portal, on the reasoning that the size IS the
 * price Stripe charges. The reasoning was right and the destination was wrong: the portal offers
 * a plan switch only when its configuration has been given one, which is a setting in a Stripe
 * dashboard rather than anything this deployment controls — so the button led, for real accounts,
 * to a page whose only option was "Cancel subscription". The server does the swap against
 * Stripe's API instead (POST /api/billing/size), which keeps Stripe as the thing that decides
 * what is charged while making the change reachable from the screen that names it.
 *
 * A comped plan has no subscription to change and nobody to change it but the operator, so it
 * renders nothing at all — the section's own description says why.
 */
function ChangeSize({
  comped,
  canManage,
  hasCustomer,
  sizes,
  current,
  periodEnd,
  pendingAt,
  supportEmail,
  askedAt,
  reload,
}: {
  comped: boolean;
  canManage: boolean;
  hasCustomer: boolean;
  sizes: BillingSize[];
  current: string;
  /** The date the current period ends — the date a downgrade takes effect on. */
  periodEnd: string;
  /** Set when a move is already waiting at the boundary; picking the current size calls it off. */
  pendingAt?: string;
  /** Where Talk to us goes; "" means nowhere, and the sizes with no price stay unpickable. */
  supportEmail: string;
  /** When this organisation last used Talk to us. */
  askedAt?: string;
  reload: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [picked, setPicked] = useState(current);
  if (comped || !canManage || !hasCustomer) return null;

  // Sizes this deployment has no price for cannot be moved to, and the one already in force is
  // not a change. Both are still listed — a ladder with rungs missing is harder to read than one
  // where some rungs say why they are not available — but neither can be submitted. A size with
  // no price can still be picked, the same as in the first picker: it asks support instead, and
  // an account already on a plan is the one most likely to have outgrown the ladder.
  const sellable = sizes.filter((b) => b.available);
  const chosen = sellable.find((b) => b.key === picked);
  const talk = picked !== current ? sizes.find((b) => b.key === picked && !b.available) : undefined;
  const now = sizes.find((b) => b.key === current);
  const up = !!chosen && !!now && chosen.price_usd > now.price_usd;

  const save = async () => {
    setBusy(true);
    try {
      // The server says which of the three things happened; the console must not guess. A
      // downgrade is scheduled and has NOT moved yet, so announcing "Moved to Up to 5 users"
      // would tell somebody they had been downgraded three weeks before they are.
      const res = await api.post<SizeChangeResult>("/api/billing/size", { size: picked });
      const label = chosen?.label ?? picked;
      if (res.cancelled) {
        toast.success(`Staying on ${label}. The scheduled change has been called off.`);
      } else if (res.scheduled) {
        toast.success(
          `You keep your current plan size until ${formatDate(res.pending_at ?? periodEnd)}, then move to ${label}.`,
        );
      } else if (res.charged) {
        toast.success(`Moved to ${label}. ${formatUSD((res.amount_minor ?? 0) / 100)} charged now.`);
      } else {
        toast.success(`Moved to ${label}.`);
      }
      setOpen(false);
      reload();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  if (!open) {
    return (
      <Button
        type="button"
        variant="outline"
        size="sm"
        onClick={() => {
          setPicked(current);
          setOpen(true);
        }}
      >
        <CreditCard className="size-3.5" aria-hidden />
        Change
      </Button>
    );
  }

  return (
    <div className="w-full max-w-md space-y-3">
      <Card className="divide-y py-0">
        {sizes.map((b) => (
          <label
            key={b.key}
            className="flex cursor-pointer items-start gap-3 px-3 py-2.5 has-[:disabled]:cursor-default has-[:disabled]:opacity-60"
          >
            <input
              type="radio"
              name="change-size"
              value={b.key}
              checked={picked === b.key}
              disabled={!b.available && !supportEmail}
              onChange={() => setPicked(b.key)}
              className="mt-0.5"
            />
            <span className="min-w-0 flex-1 text-sm font-medium text-foreground">
              {b.label}
              {b.key === current && (
                <span className="ml-2 text-xs font-normal text-muted-foreground">current</span>
              )}
            </span>
            <span className="font-mono text-sm tabular-nums text-muted-foreground">
              {b.available ? `${formatUSD(b.price_usd)} a month` : "Talk to us"}
            </span>
          </label>
        ))}
      </Card>
      {talk ? (
        <TalkToUs
          size={talk.key}
          supportEmail={supportEmail}
          askedAt={askedAt}
          compact
          onSent={() => {
            setOpen(false);
            reload();
          }}
        >
          <Button type="button" size="sm" variant="ghost" onClick={() => setOpen(false)}>
            Cancel
          </Button>
        </TalkToUs>
      ) : (
        <ActionNote
          note={
            chosen && chosen.key !== current ? (
              up ? (
                <>
                  Moving up takes effect now. Your card is charged the difference for the rest of
                  this month straight away, and {formatUSD(chosen.price_usd)} a month from{" "}
                  {periodEnd ? formatDate(periodEnd) : "the next renewal"}.
                  {chosen.included_usd > 0 &&
                    ` The included credit goes up to ${formatUSD(chosen.included_usd)} immediately, less whatever you have already spent this month.`}
                </>
              ) : (
                <>
                  {/* The sentence a downgrade has to say, and the one it is easiest to get wrong:
                      nothing happens today. Saying "moved to Up to 5 users" here would be telling
                      somebody they had lost capacity they have in fact paid for until the boundary. */}
                  You keep {now?.label ?? "your current plan size"} until{" "}
                  {periodEnd ? formatDate(periodEnd) : "the end of this billing period"}, and
                  everything it includes. From then you move to {chosen.label} at{" "}
                  {formatUSD(chosen.price_usd)} a month. Nothing is charged now and nothing comes
                  back — this month is already paid for.
                </>
              )
            ) : pendingAt && picked === current ? (
              <>
                Keeping {now?.label ?? "your current plan size"} calls off the change waiting for{" "}
                {formatDate(pendingAt)}. Nothing else about the plan moves.
              </>
            ) : (
              "Pick a different plan size to move to."
            )
          }
        >
          <Button
            type="button"
            size="sm"
            disabled={busy || !chosen || (picked === current && !pendingAt)}
            onClick={save}
          >
            {busy && <Loader2 className="size-4 animate-spin" aria-hidden />}
            {pendingAt && picked === current ? "Keep this size" : "Change size"}
          </Button>
          <Button
            type="button"
            size="sm"
            variant="ghost"
            disabled={busy}
            onClick={() => setOpen(false)}
          >
            Cancel
          </Button>
        </ActionNote>
      )}
    </div>
  );
}

/**
 * Talk to us. The size with no price is a conversation, and this is how it starts: the server
 * mails support with the account's figures and the note, Reply-To whoever pressed the button.
 * Never a mailto: — the figures belong in the message, not in a draft the person has to send.
 *
 * Shared by the first picker and Change size, so an account asks the same way whether it is on
 * a plan yet or not.
 */
function TalkToUs({
  size,
  supportEmail,
  askedAt,
  disabled,
  compact,
  onSent,
  children,
}: {
  size: string;
  supportEmail: string;
  /** When this organisation last asked, so the sentence can say so after a reload. */
  askedAt?: string;
  disabled?: boolean;
  /** The smaller buttons Change size uses. */
  compact?: boolean;
  onSent: () => void;
  /** Beside the button — Change size's Cancel. */
  children?: React.ReactNode;
}) {
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState("");

  const request = async () => {
    setBusy(true);
    try {
      const res = await api.post<{ delivered: boolean; support_email: string }>(
        "/api/billing/size-request",
        { size, note },
      );
      toast.success(
        res.delivered
          ? "Sent. The reply comes to your own email address."
          : `Mail is not configured on this deployment — write to ${res.support_email}.`,
      );
      setNote("");
      onSent();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-3">
      <Textarea
        value={note}
        onChange={(e) => setNote(e.target.value)}
        maxLength={1000}
        rows={3}
        aria-label="Note to support"
        placeholder="Anything we should know? How many people, how many workspaces, what you need. Optional."
      />
      <ActionNote
        note={
          <>
            Talk to us sends {supportEmail} an upgrade request with your account&rsquo;s figures —
            who used the bot, this month&rsquo;s spend and fix jobs, the credit left — and the note
            above. The reply comes to your own address, and nothing is charged.
            {askedAt && ` You last asked on ${formatDate(askedAt)}.`}
          </>
        }
      >
        <Button
          type="button"
          size={compact ? "sm" : "default"}
          disabled={busy || disabled}
          onClick={request}
        >
          {busy && <Loader2 className="size-4 animate-spin" aria-hidden />}
          Talk to us
        </Button>
        {children}
      </ActionNote>
    </div>
  );
}

function CreditGroup({
  data,
  canManage,
  canSettings,
  onSaved,
  reload,
}: {
  data: BillingView;
  canManage: boolean;
  canSettings: boolean;
  onSaved: () => void;
  reload: () => void;
}) {
  const [amount, setAmount] = useState(String(data.topup.presets[0] ?? data.topup.min_usd));
  const [custom, setCustom] = useState(false);
  const [busy, setBusy] = useState(false);
  const [budget, setBudget] = useState(String(data.budget.monthly_budget_usd));
  const [savingBudget, setSavingBudget] = useState(false);

  const value = Number(amount);
  const outOfRange =
    !Number.isFinite(value) || value < data.topup.min_usd || value > data.topup.max_usd;

  const topUp = async (e: React.FormEvent) => {
    e.preventDefault();
    if (outOfRange || busy) return;
    setBusy(true);
    try {
      const res = await api.post<{ url: string }>("/api/billing/checkout", {
        kind: "topup",
        amount_usd: value,
      });
      window.location.assign(res.url);
    } catch (err) {
      setBusy(false);
      toast.error(errorMessage(err));
    }
  };

  const saveBudget = async (e: React.FormEvent) => {
    e.preventDefault();
    setSavingBudget(true);
    try {
      await api.put("/api/settings", { monthly_budget_usd: budget });
      toast.success("Monthly budget saved.");
      onSaved();
      reload();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setSavingBudget(false);
    }
  };

  return (
    <SettingsGroup title="Credit">
      {/* On its own key the organisation's provider bills it, so a balance here would sit still
          and look like a fault. Say so where the balance is, rather than leave it to be asked. */}
      {data.own_model_key && (
        <SettingsSection
          title="Your own model key"
          description="Every model call runs on your organisation's own key, billed by your provider."
        >
          <p className="text-sm text-muted-foreground">
            AI credit is not used while your own key is in use. Your monthly budget below still
            applies, measured at your provider&rsquo;s list prices.{" "}
            <Link href="/settings?tab=models" className="underline underline-offset-2">
              Your model key
            </Link>
          </p>
        </SettingsSection>
      )}
      <SettingsSection
        title="Top up"
        description={
          <>
            Prepaid credit for model spend, drawn at the provider&rsquo;s price with no margin on
            it. It does not expire, and it is not a subscription: you buy an amount and it goes
            down as the bot answers. It is spent only after the credit included with your plan for
            the month has gone &mdash; that one does expire, so it goes first.
          </>
        }
      >
        <form onSubmit={topUp} className="space-y-3">
          <div className="flex flex-wrap gap-2">
            {data.topup.presets.map((p) => (
              <Button
                key={p}
                type="button"
                size="sm"
                variant={!custom && value === p ? "default" : "outline"}
                disabled={!canManage}
                onClick={() => {
                  setCustom(false);
                  setAmount(String(p));
                }}
              >
                {formatUSD(p)}
              </Button>
            ))}
            <Button
              type="button"
              size="sm"
              variant={custom ? "default" : "outline"}
              disabled={!canManage}
              onClick={() => setCustom(true)}
            >
              Custom
            </Button>
          </div>
          {custom && (
            <div className="relative w-full max-w-40">
              <span className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-sm text-muted-foreground">
                $
              </span>
              <Input
                aria-label="Top-up amount in USD"
                type="number"
                min={data.topup.min_usd}
                max={data.topup.max_usd}
                step="1"
                className="pl-7 tabular-nums"
                value={amount}
                onChange={(e) => setAmount(e.target.value)}
                disabled={!canManage}
              />
            </div>
          )}
          {custom && outOfRange && (
            <p className="text-xs text-danger">
              Between {formatUSD(data.topup.min_usd)} and {formatUSD(data.topup.max_usd)}. For more
              than that, write to {data.support_email || "support"}.
            </p>
          )}
          {canManage && (
            <ActionNote
              note={
                !outOfRange && (
                  <>You are adding {formatUSD(value)} of credit. One charge, not a subscription.</>
                )
              }
            >
              <Button type="submit" disabled={outOfRange || busy}>
                {busy && <Loader2 className="size-4 animate-spin" aria-hidden />}
                Continue to payment
              </Button>
            </ActionNote>
          )}
        </form>
      </SettingsSection>

      <SettingsSection
        title="Your monthly budget"
        description={
          <>
            Your own guard rail, under the credit balance. The bot stops at whichever it reaches
            first: credit is what you have paid for and cannot be lifted from here, the budget is
            yours and can. 0 means no guard rail of your own.
            {(data.subscription?.included_usd ?? 0) > 0 && (
              <>
                {" "}
                It starts at the {formatUSD(data.subscription?.included_usd ?? 0)} your plan
                includes each month, and moves with it if you change plan size. Set it yourself and
                your figure stands until then.
              </>
            )}
            {data.credit.enforced && (
              <>
                {" "}
                Spending already running when the credit goes can take the balance up to{" "}
                {formatUSD(data.credit.overdraft_usd)} below zero — a turn is checked before it
                runs and priced after it finishes.
              </>
            )}
          </>
        }
      >
        {canSettings ? (
          <form onSubmit={saveBudget} className="space-y-3">
            <div className="relative w-full max-w-40">
              <span className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-sm text-muted-foreground">
                $
              </span>
              <Input
                id="monthly_budget_usd"
                aria-label="Monthly budget in USD"
                type="number"
                min={0}
                step="0.01"
                className="pl-7 tabular-nums"
                value={budget}
                onChange={(e) => setBudget(e.target.value)}
              />
            </div>
            <Button
              type="submit"
              variant="outline"
              disabled={savingBudget || budget === String(data.budget.monthly_budget_usd)}
            >
              {savingBudget && <Loader2 className="size-4 animate-spin" aria-hidden />}
              Save budget
            </Button>
          </form>
        ) : (
          <SettingsRows>
            <SettingsRow label="Now">
              {data.budget.effective_budget_usd > 0
                ? `${formatUSD(data.budget.effective_budget_usd)} a month`
                : "No monthly budget"}
            </SettingsRow>
          </SettingsRows>
        )}
        {!canSettings && (
          <p className="mt-2 text-[13px] leading-relaxed text-muted-foreground">
            Changing this needs the settings permission, which is separate from billing — one buys
            credit, the other sets the guard rail under it.
          </p>
        )}
      </SettingsSection>
    </SettingsGroup>
  );
}

const KIND_LABELS: Record<CreditEntry["kind"], string> = {
  topup: "Top-up",
  debit: "Model spend",
  refund: "Refund",
  adjustment: "Adjustment",
  grant: "Granted",
  included: "Included with plan",
};

function LedgerGroup({ entries }: { entries: CreditEntry[] | null }) {
  return (
    <SettingsGroup title="Credit history">
      {!entries || entries.length === 0 ? (
        <div className="px-6 py-8">
          <EmptyState
            icon={Receipt}
            title="No credit yet"
            description="Top-ups, refunds and adjustments show up here, newest first, with the balance after each one. Model spend is written up once an hour rather than once a turn. Credit included with a plan is not listed: it is part of the fee rather than a payment, and it expires with the month it came with."
          />
        </div>
      ) : (
        <div className="overflow-hidden">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Date</TableHead>
                <TableHead>What</TableHead>
                <TableHead className="text-right">Amount</TableHead>
                <TableHead className="text-right">Balance</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {entries.map((e, i) => (
                <TableRow key={`${e.at}-${i}`}>
                  <TableCell className="whitespace-nowrap text-muted-foreground">
                    {formatDateTime(e.at)}
                  </TableCell>
                  <TableCell>
                    {KIND_LABELS[e.kind] ?? e.kind}
                    {e.note && <span className="ml-2 text-xs text-muted-foreground">{e.note}</span>}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {e.amount_usd > 0 ? "+" : ""}
                    {formatUSD(e.amount_usd)}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {formatUSD(e.balance_after_usd)}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          <p className="px-6 py-3 text-xs text-muted-foreground">
            Every charge has an invoice in the card portal below. Credit included with a plan is
            not listed here: it is part of the fee rather than a payment, and it expires with the
            month it came with.
          </p>
        </div>
      )}
    </SettingsGroup>
  );
}

function PaymentsGroup({ data, canManage }: { data: BillingView; canManage: boolean }) {
  const { busy, open } = useStripePortal();
  const hasCustomer = !!data.stripe?.customer_id;

  return (
    <SettingsGroup title="Payments">
      <SettingsSection
        title="Card and invoices"
        description={
          <>
            Stripe keeps the card, the invoices and the plan itself — we never see the number.
            This opens their portal signed in as this account; changes there show up here within a
            minute.
          </>
        }
      >
        {canManage && hasCustomer ? (
          <Button type="button" variant="outline" disabled={busy} onClick={open}>
            {busy ? (
              <Loader2 className="size-4 animate-spin" aria-hidden />
            ) : (
              <CreditCard className="size-4" aria-hidden />
            )}
            Manage in Stripe
          </Button>
        ) : (
          <p className="text-[13px] leading-relaxed text-muted-foreground">
            <Wallet className="mr-1 inline size-3.5 align-[-2px]" aria-hidden />
            Nothing to manage yet. A card is added at the first payment.
          </p>
        )}
      </SettingsSection>
    </SettingsGroup>
  );
}
