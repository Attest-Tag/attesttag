"use client";

import Link from "next/link";
import { UpgradeButton } from "@/components/core/upgrade-button";
import { useAuth } from "@/components/shell/auth-provider";
import type { Me } from "@/lib/api";
import { formatUSD } from "@/lib/format";

/**
 * The strip under the topbar when the bot has stopped replying in Slack.
 *
 * On every page rather than on Overview, because the person who notices is whoever is in the
 * console when Slack goes quiet, and the number they need is not the page they are on. It is
 * the one console-wide state worth interrupting for: nothing else here is broken, but the
 * product is not answering anybody until this is dealt with.
 *
 * Two limits can stop it and they want different answers, so the banner has to say which. Credit
 * is money and only a payment moves it; a monthly budget is the account's own guard rail and an
 * admin can raise it. Telling somebody their budget is spent when what ran out was the balance
 * sends them to a field that will not help.
 *
 * `paused_by` comes from the server, which is the code that actually refuses the turn. The
 * budget comparison below is only a fallback for a console running ahead of its own backend.
 *
 * It also warns BEFORE any of that, on `warn_by`, when an account is within ten percent of
 * running out of credit or of the people its plan size allows. Same strip, deliberately: a second
 * banner stacked under this one would be two rows of chrome competing for the same glance, and
 * the two states are mutually exclusive anyway — once something has stopped, that it was about to
 * stop is no longer the news. Amber rather than red, because nothing is broken yet and a warning
 * that looks like a failure is a warning people learn to scroll past.
 */
export function BudgetBanner() {
  const { me } = useAuth();
  const plan = me?.plan;
  if (!plan) return null;

  const paused =
    plan.paused_by ??
    (plan.budget_usd > 0 && plan.spend_usd >= plan.budget_usd ? "budget" : "");

  const free = plan.plan === "free";
  // /api/me sends only the keys the role holds, so a permission somebody lacks is absent
  // rather than false. A viewer is still told the bot has stopped; they are just not offered
  // a way out of it that would refuse them.
  const perms = me?.user?.permissions;
  const canManage = !perms || perms["settings.manage"] === true;
  const canBill = !perms || perms["billing.manage"] === true;
  const sells = !!plan.billing_enabled;

  // Nothing has stopped, but something is close. Checked after `paused` so the louder state wins.
  if (!paused) {
    if (!plan.warn_by) return null;
    return <NearLimitBanner plan={plan} canBill={canBill} />;
  }

  return (
    <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 border-b bg-danger-soft px-5 py-2.5">
      <p className="min-w-0 text-sm text-danger">
        <span className="font-medium">The bot has stopped replying.</span>{" "}
        {paused === "credit" ? (
          <>
            This account is out of API credit. Its monthly budget is not what stopped it — topping
            up starts the bot again straight away.
          </>
        ) : free ? (
          `This month's ${formatUSD(plan.budget_usd)} free-plan budget is spent; it starts again when the month rolls over.`
        ) : (
          `This month's ${formatUSD(plan.budget_usd)} budget is spent; it starts again when the month rolls over.`
        )}
      </p>

      {/* Out of credit: the fix is a payment, and only somebody who can make one is offered it. */}
      {paused === "credit" &&
        (canBill ? (
          <Link
            href="/settings?tab=billing"
            className="text-sm font-medium text-danger underline underline-offset-2"
          >
            Top up credit
          </Link>
        ) : null)}

      {/* Over its own budget: the field that lifts it. It lives on the Billing tab where the
          credit balance beside it explains what the guard rail sits under — it used to link to
          the Workspace tab, which has never had a budget field on it. */}
      {paused === "budget" &&
        canManage &&
        (free ? (
          sells ? (
            canBill ? (
              <Link
                href="/settings?tab=billing"
                className="text-sm font-medium text-danger underline underline-offset-2"
              >
                Choose a plan
              </Link>
            ) : null
          ) : // A deployment that published no support address has nobody to ask, so the banner
          // says what happened and stops there rather than offering a button that refuses.
          plan.support_email ? (
            <UpgradeButton
              support={plan.support_email}
              askedAt={plan.requested_at}
              note={false}
            />
          ) : null
        ) : (
          <Link
            href={sells ? "/settings?tab=billing" : "/settings?tab=models"}
            className="text-sm font-medium text-danger underline underline-offset-2"
          >
            Raise the budget
          </Link>
        ))}
    </div>
  );
}

/**
 * The amber strip: within ten percent of a limit, and still able to do something about it.
 *
 * Which limit, and therefore which sentence, is the server's answer (`warn_by`) rather than a
 * comparison made here — the same rule that decides whether to refuse a turn should be the rule
 * that decides whether to warn about it, or the console and the bot will eventually disagree in
 * front of a customer.
 *
 * The link is offered only to somebody who can act on it. A viewer who cannot buy anything gets
 * the sentence without a button that would refuse them, which is the same choice the red strip
 * above makes.
 */
function NearLimitBanner({
  plan,
  canBill,
}: {
  plan: NonNullable<Me["plan"]>;
  canBill: boolean;
}) {
  const credit = plan.warn_by === "credit";
  const limit = plan.users_limit ?? 0;
  const active = plan.users_active ?? 0;
  const over = !credit && limit > 0 && active > limit;

  return (
    <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 border-b bg-warning-soft px-5 py-2.5">
      <p className="min-w-0 text-sm text-warning">
        {credit ? (
          <>
            <span className="font-medium">Credit is nearly spent.</span>{" "}
            {formatUSD(plan.credit_balance_usd ?? 0)} left
            {(plan.credit_month_allowance_usd ?? 0) > 0 &&
              ` of the ${formatUSD(plan.credit_month_allowance_usd ?? 0)} a month your plan includes`}
            . The bot stops replying when it runs out.
          </>
        ) : over ? (
          <>
            <span className="font-medium">
              More people are using the bot than your plan size allows.
            </span>{" "}
            {active} in the last 30 days, against {limit}. Nothing has been stopped and nobody has
            been turned away.
          </>
        ) : (
          <>
            <span className="font-medium">You are close to the people your plan size allows.</span>{" "}
            {active} of {limit} in the last 30 days.
          </>
        )}
      </p>

      {canBill && (
        <Link
          href="/settings?tab=billing"
          className="text-sm font-medium text-warning underline underline-offset-2"
        >
          {credit ? "Top up credit" : "Move to a larger plan size"}
        </Link>
      )}
    </div>
  );
}
