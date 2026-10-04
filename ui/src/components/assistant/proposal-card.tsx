"use client";

import { useEffect, useRef, useState } from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { ArrowRight, Check, Info, Loader2, Lock, Sparkles, TriangleAlert } from "lucide-react";
import { toast } from "sonner";
import { Disclosure } from "@/components/core/disclosure";
import { Button } from "@/components/ui/button";
import {
  ApiError,
  announceConsoleChange,
  api,
  errorMessage,
  type Me,
  type Proposal,
  type ProposalChange,
  type ProposalItem,
} from "@/lib/api";
import { mayMoveTo, selectInPlace } from "@/lib/assistant-screen";
import { cn } from "@/lib/utils";

// The card IS the confirmation. There is deliberately no second dialog over it: two decisions
// for one change is how somebody learns to click through both.
//
// Confirm replays the proposal's steps, in order, against the console's own endpoints — from
// this browser, under this session. So everything those endpoints do on a hand-typed save
// (validate, bump the config version, write the audit row) happens here unchanged, and the
// button offers nobody any reach they did not already have. The proposal id rides along in each
// body, where the endpoint ignores it for writing and records it, which is what ties this card
// to the row in the audit log.
//
// Every step carries what it was read from — a type's version, a digest of a level's branch rules —
// so one made over something that changed since answers 409 and writes nothing. The card then says
// so and offers no second try: the same steps would be refused again, and the way on is a new card
// built from what is there now.

/** What the person did with a card, as the panel tells the model on the next question. */
export type ProposalOutcome = "confirmed" | "not confirmed" | "changed before Confirm";

/**
 * What became of a card, held by the panel rather than the card: the panel is mounted once beside
 * every route but its cards are not — closing it unmounts them — and a card that came back from that
 * offering Confirm again for a change it already made would be offering to make it twice.
 */
export type ProposalRecord = { outcome?: ProposalOutcome; applied?: number; dismissed?: boolean };

// A value as a person should read it in a narrow column. Instructions are prose and arrive
// whole; showing four lines of them in a diff is not showing a diff. What is cut is never cut
// silently: the row offers the whole value (ValueChange), since the end of a long value — the last
// pattern of a list, the last skill a copy brings — is as much the change as its start.
const SHORT = 120;

function short(v: string): string {
  const s = v.trim();
  if (!s) return "—";
  return s.length <= SHORT ? s : s.slice(0, SHORT) + "…";
}

function isCut(v: string): boolean {
  return v.trim().length > SHORT;
}

// Prose longer than this folds behind a disclosure, so one purpose does not push Confirm off screen.
const LONG_TEXT = 400;

const MARK: Record<ProposalItem["mark"], { word: string; className: string }> = {
  "+": { word: "added", className: "text-success-text" },
  "-": { word: "removed", className: "text-danger" },
  "~": { word: "changed", className: "text-warning" },
  "↕": { word: "moved", className: "text-info" },
  "=": { word: "unchanged", className: "text-muted-foreground" },
};

/** A create is the one change whose result is not on screen yet, so the page may be moved to it. */
function creates(p: Proposal): boolean {
  return p.steps.some((s) => s.method.toUpperCase() === "POST" && s.path === "/api/review-types");
}

/** A card that makes something new — a review type, an approval tier — says so once it is confirmed. */
function makesNew(p: Proposal): boolean {
  return p.steps.some(
    (s) => s.method.toUpperCase() === "POST" && (s.path === "/api/review-types" || s.path === "/api/approval-roles"),
  );
}

function ListChange({ change }: { change: ProposalChange }) {
  return (
    <dd className="space-y-1 text-xs">
      {change.to && <p className="text-muted-foreground">{change.to}</p>}
      <ol className="space-y-0.5 rounded-md border bg-background/60 px-2 py-1.5">
        {(change.items ?? []).map((it, i) => {
          const m = MARK[it.mark] ?? MARK["="];
          return (
            <li key={i} className="flex items-start gap-1.5">
              <span aria-hidden className={cn("w-3 shrink-0 text-center font-mono font-semibold", m.className)}>
                {it.mark}
              </span>
              <span className="sr-only">{m.word}: </span>
              <span
                className={cn(
                  "min-w-0 break-words",
                  it.mark === "-" && "text-muted-foreground line-through",
                  it.mark === "=" && "text-muted-foreground",
                )}
              >
                {it.text}
              </span>
            </li>
          );
        })}
      </ol>
    </dd>
  );
}

function TextChange({ change }: { change: ProposalChange }) {
  const to = change.to.trim();
  const from = change.from.trim();
  const body = <p className="text-xs break-words whitespace-pre-wrap">{to || "—"}</p>;
  return (
    <dd className="space-y-1">
      {to.length > LONG_TEXT ? (
        <Disclosure
          label={<span className="text-xs font-medium">The new text</span>}
          summary={`${to.length.toLocaleString("en-US")} characters`}
          contentClassName="pt-1"
        >
          {body}
        </Disclosure>
      ) : (
        body
      )}
      {from && (
        <Disclosure label={<span className="text-xs font-medium text-muted-foreground">Before</span>} contentClassName="pt-1">
          <p className="text-xs break-words whitespace-pre-wrap text-muted-foreground">{from}</p>
        </Disclosure>
      )}
    </dd>
  );
}

function ValueChange({ change }: { change: ProposalChange }) {
  // Short values read as one line — "inherit → on" is the whole story. Prose does not: an arrow
  // in the middle of a wrapped paragraph leaves a strikethrough column two words wide beside it,
  // and neither half is legible. Those stack instead.
  const long = change.from.trim().length > 32 || change.to.trim().length > 32;
  const fromCut = isCut(change.from);
  const toCut = isCut(change.to);
  return (
    <dd className="space-y-1">
      {long ? (
        <div className="space-y-0.5 text-xs break-words">
          <p className="text-muted-foreground line-through">{short(change.from)}</p>
          <p className="flex items-start gap-1.5 font-medium">
            <ArrowRight className="mt-0.5 size-3 shrink-0 text-muted-foreground" />
            <span className="min-w-0">{short(change.to)}</span>
          </p>
        </div>
      ) : (
        <div className="flex items-start gap-1.5 text-xs break-words">
          <span className="text-muted-foreground line-through">{short(change.from)}</span>
          <ArrowRight className="mt-0.5 size-3 shrink-0 text-muted-foreground" />
          <span className="font-medium">{short(change.to)}</span>
        </div>
      )}
      {(fromCut || toCut) && (
        <Disclosure
          label={<span className="text-xs font-medium text-muted-foreground">The whole value</span>}
          summary={`${Math.max(change.from.trim().length, change.to.trim().length).toLocaleString("en-US")} characters`}
          contentClassName="pt-1"
        >
          <div className="space-y-1 text-xs break-words whitespace-pre-wrap">
            {fromCut && <p className="text-muted-foreground line-through">{change.from.trim()}</p>}
            {toCut && <p className="font-medium">{change.to.trim()}</p>}
          </div>
        </Disclosure>
      )}
    </dd>
  );
}

/**
 * Where the change is seen. On the page it belongs to, the page is told what to open rather than
 * linked to: a link to the route already on screen would change the address and show nothing new.
 */
function OpenLink({ proposal, children }: { proposal: Proposal; children?: React.ReactNode }) {
  const pathname = usePathname();
  const href = proposal.open?.href ?? (proposal.kind === "approval" ? "/approvers/" : "/workspaces/");
  const label = children ?? proposal.open?.label ?? "Open the page";
  const select = selectInPlace(href, pathname);
  if (select) {
    return (
      <button
        type="button"
        onClick={() => announceConsoleChange({ refresh: [], select })}
        className="underline underline-offset-2"
      >
        {label}
      </button>
    );
  }
  return (
    <Link href={href} className="underline underline-offset-2">
      {label}
    </Link>
  );
}

/**
 * Why this card cannot be confirmed where the console is signed in now, or "" when it can. The session's
 * organisation is whichever one it last switched to — from any tab, which this one is not told of — so it
 * is read fresh on every press rather than taken from what the page loaded with. The endpoints refuse a
 * step naming another organisation as well; this says so before the first step is sent, by name.
 */
async function elsewhere(p: Proposal): Promise<string> {
  if (!p.org) return "";
  const me = await api.get<Me>("/api/me");
  const now = me.user;
  if (!now || now.org_id === p.org) return "";
  const from = me.orgs?.find((o) => o.org_id === p.org)?.org_name;
  return (
    `This was proposed in ${from ? `“${from}”` : "another organisation"}, and the console is now signed in to ` +
    `“${now.org_name}”. Switch back to ${from ? `“${from}”` : "it"} to confirm it, or ask again here. Nothing was saved.`
  );
}

/**
 * The card another one needs confirmed first — the type a branch rule names, made only by that card —
 * and why it is not: still waiting for its Confirm, or gone for good, dismissed or refused as changed,
 * when this card can never be confirmed either and the way on is to ask again.
 */
export type ProposalBlock = { target: string; why: "waiting" | "dismissed" | "changed" };

function blockedText(b: ProposalBlock): string {
  switch (b.why) {
    case "dismissed":
      return `Needs “${b.target}”, which was dismissed — ask again.`;
    case "changed":
      return `Needs “${b.target}”, which changed since it was proposed — ask again.`;
    default:
      return `Confirm “${b.target}” first.`;
  }
}

export function ProposalCard({
  proposal,
  record,
  onRecord,
  blocked,
}: {
  proposal: Proposal;
  record: ProposalRecord;
  onRecord: (patch: ProposalRecord) => void;
  blocked?: ProposalBlock;
}) {
  const pathname = usePathname();
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const root = useRef<HTMLDivElement>(null);
  // Set by a press of Confirm, so only the card somebody acted on takes the focus back below — not
  // every finished card the panel draws again when it opens.
  const pressed = useRef(false);
  const applied = record.applied ?? 0;
  const steps = proposal.steps;
  const state = record.dismissed
    ? "dismissed"
    : record.outcome === "confirmed"
      ? "done"
      : record.outcome === "changed before Confirm"
        ? "stale"
        : saving
          ? "saving"
          : "proposed";

  // Confirm is gone once the card is done or refused, and the focus of whoever pressed it from the
  // keyboard would fall to the page with it. The card takes it, so a screen reader reads where it went.
  useEffect(() => {
    if (pressed.current && (state === "done" || state === "stale")) {
      pressed.current = false;
      root.current?.focus();
    }
  }, [state]);

  const confirm = async () => {
    if (blocked) return;
    pressed.current = true;
    setError("");
    setSaving(true);
    const from = applied;
    let done = applied;
    try {
      const away = await elsewhere(proposal);
      if (away) {
        setError(away);
        return;
      }
      // In order, stopping at the first failure, and picking up after the last one that landed:
      // a later step often depends on an earlier one, and carrying on past a failure — or making
      // a step that already landed again — would leave a change nobody asked for.
      for (let i = from; i < steps.length; i++) {
        await api.send(steps[i].method, steps[i].path, steps[i].body);
        done = i + 1;
        onRecord({ applied: done });
      }
      // The page behind the panel reads again. A type just created is also opened, since nothing on
      // screen shows it yet — but only where that moves nothing out from under the person (mayMoveTo):
      // from another tab, or over unsaved edits, the card's Open link takes them there instead.
      const select = creates(proposal) ? selectInPlace(proposal.open?.href ?? "", pathname) : null;
      announceConsoleChange({
        refresh: proposal.refresh ?? [],
        select: mayMoveTo(select) && select ? select : undefined,
      });
      onRecord({ applied: done, outcome: "confirmed" });
      toast.success(`${proposal.target} ${makesNew(proposal) ? "created" : "updated"}`);
    } catch (err) {
      // Told by the status, not the words: the server's sentence for a lost race may change.
      const lost = err instanceof ApiError && err.status === 409;
      // What did land is on screen, whatever the failure after it — and after a lost race, so is the
      // save that won it: the page reads again and shows what a new card would be built from. An
      // editor with unsaved edits keeps them; it only learns a newer version is there.
      if (done > from || lost) announceConsoleChange({ refresh: proposal.refresh ?? [] });
      if (lost) {
        onRecord({ applied: done, outcome: "changed before Confirm" });
        toast.error(
          `${proposal.target} changed since this was proposed — ${done > 0 ? "the rest was not saved" : "nothing was saved"}`,
        );
      } else {
        onRecord({ applied: done });
        setError(
          done === 0
            ? errorMessage(err)
            : `${errorMessage(err)} — ${done} of ${steps.length} step(s) had already been applied.`,
        );
      }
    } finally {
      setSaving(false);
    }
  };

  if (state === "dismissed") return null;

  const changes = proposal.changes ?? [];

  return (
    <div
      ref={root}
      tabIndex={-1}
      role="group"
      aria-label={`Proposal for ${proposal.target}`}
      className="rounded-lg border border-ai-border bg-ai-soft/40 px-3 py-2.5 outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
    >
      <div className="flex items-center gap-2">
        <Sparkles className="size-3.5 shrink-0 text-ai" />
        <p className="min-w-0 flex-1 truncate text-xs font-medium">
          {state === "done" ? (makesNew(proposal) ? "Created" : "Applied to") : "Proposed for"} {proposal.target}
        </p>
      </div>
      {proposal.based && <p className="mt-0.5 pl-5.5 text-[11px] text-muted-foreground">{proposal.based}</p>}

      <dl className="mt-2 space-y-1.5">
        {changes.map((c) => (
          <div key={c.key}>
            <dt className="eyebrow text-muted-foreground">{c.label}</dt>
            {c.format === "list" ? (
              <ListChange change={c} />
            ) : c.format === "text" ? (
              <TextChange change={c} />
            ) : (
              <ValueChange change={c} />
            )}
          </div>
        ))}
      </dl>

      {/* What the change alone does not show — a level that stops inheriting, a built-in copied on
          its first save — said before Confirm rather than discovered after it. */}
      {proposal.note && (
        <p className="mt-2 flex items-start gap-1.5 rounded-md border border-info/30 bg-info-soft px-2 py-1.5 text-xs text-foreground">
          <Info className="mt-0.5 size-3 shrink-0 text-info" />
          <span className="min-w-0 break-words whitespace-pre-line">{proposal.note}</span>
        </p>
      )}

      {/* What Confirm will actually do, when it is more than the one save the diff implies.
          A card that changes two things in two places should say so before it is pressed. */}
      {steps.length > 1 && (
        <ul className="mt-2 space-y-0.5">
          {steps.map((s, i) => (
            <li key={i} className="flex items-center gap-1.5 text-[11px] text-muted-foreground">
              {i < applied ? (
                <Check className="size-3 shrink-0 text-success" />
              ) : (
                <span className="size-3 shrink-0" />
              )}
              {s.label}
            </li>
          ))}
        </ul>
      )}

      {error && <p className="mt-2 text-xs text-danger">{error}</p>}

      {state === "done" ? (
        <p role="status" className="mt-2 flex items-center gap-1.5 text-xs text-success-text">
          <Check className="size-3.5" />
          Saved. <OpenLink proposal={proposal} />
        </p>
      ) : state === "stale" ? (
        <p role="status" className="mt-2 flex items-start gap-1.5 text-xs text-warning">
          <TriangleAlert className="mt-0.5 size-3.5 shrink-0" />
          <span>
            Changed since this was proposed — ask again.
            {applied > 0 && ` ${applied} of ${steps.length} step(s) had already been applied.`}
          </span>
        </p>
      ) : (
        <>
          <p className="mt-2 flex items-center gap-1.5 text-xs text-muted-foreground">
            {blocked ? (
              <>
                <Lock className="size-3 shrink-0" />
                {blockedText(blocked)}
              </>
            ) : applied > 0 ? (
              `${applied} of ${steps.length} step(s) applied.`
            ) : (
              "Nothing has changed yet."
            )}
          </p>
          <div className="mt-2 flex items-center gap-2">
            <Button size="sm" onClick={confirm} disabled={state === "saving" || !!blocked}>
              {state === "saving" && <Loader2 className="size-3.5 animate-spin" />}
              {applied > 0 ? "Confirm the rest" : "Confirm"}
            </Button>
            <Button
              size="sm"
              variant="ghost"
              onClick={() => onRecord({ dismissed: true })}
              disabled={state === "saving"}
            >
              Dismiss
            </Button>
            {proposal.open && (
              <span className="ml-auto text-xs text-muted-foreground">
                <OpenLink proposal={proposal}>{proposal.open.label || "Open"}</OpenLink>
              </span>
            )}
          </div>
        </>
      )}
    </div>
  );
}
