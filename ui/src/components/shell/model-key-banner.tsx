"use client";

import Link from "next/link";
import { useAuth } from "@/components/shell/auth-provider";

/**
 * The strip under the topbar when the organisation's own model key cannot answer.
 *
 * Nothing falls back to the included models — an organisation that brought a key chose where its
 * conversations go — so a key that is refused, out of quota, or no longer in the plan is a bot
 * that has stopped replying in every channel. That is worth interrupting every page for, the way
 * a spent budget is, and the person who notices is whoever happens to be in the console.
 *
 * Its own strip rather than a third state of BudgetBanner: the two have different causes and
 * different fixes, and a key that has failed says nothing about money. Only somebody who can
 * change the key is offered the way to it; everybody else is told the admins have been told.
 */
export function ModelKeyBanner() {
  const { me } = useAuth();
  const mk = me?.model_key;
  if (!mk || !(mk.blocked || mk.failing)) return null;
  const perms = me?.user?.permissions;
  const canManage = !perms || perms["settings.manage"] === true;

  return (
    <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 border-b bg-danger-soft px-5 py-2.5">
      <p className="min-w-0 text-sm text-danger">
        <span className="font-medium">The bot has stopped replying.</span>{" "}
        {mk.blocked && mk.refusal
          ? mk.refusal.charAt(0).toUpperCase() + mk.refusal.slice(1) + "."
          : "Your organisation's own model key is failing, and nothing is sent to any other model instead."}
        {!canManage && " Your admins have been told."}
      </p>
      {canManage && (
        <Link
          href="/settings?tab=models"
          className="text-sm font-medium text-danger underline underline-offset-2"
        >
          Check the model key
        </Link>
      )}
    </div>
  );
}
