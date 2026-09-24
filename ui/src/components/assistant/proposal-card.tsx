"use client";

import { useState } from "react";
import Link from "next/link";
import { ArrowRight, Check, Loader2, Sparkles } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { api, errorMessage, type Proposal } from "@/lib/api";

// The card IS the confirmation. There is deliberately no second dialog over it: two decisions
// for one change is how somebody learns to click through both.
//
// Confirm replays the proposal's steps, in order, against the console's own endpoints — from
// this browser, under this session. So everything those endpoints do on a hand-typed save
// (validate, bump the config version, write the audit row) happens here unchanged, and the
// button offers nobody any reach they did not already have. The proposal id rides along in each
// body, where the endpoint ignores it for writing and records it, which is what ties this card
// to the row in the audit log.

type State = "proposed" | "saving" | "done" | "dismissed";

// A value as a person should read it in a narrow column. Instructions are prose and arrive
// whole; showing four lines of them in a diff is not showing a diff.
function short(v: string): string {
  const s = v.trim();
  if (!s) return "—";
  return s.length <= 120 ? s : s.slice(0, 120) + "…";
}

export function ProposalCard({ proposal }: { proposal: Proposal }) {
  const [state, setState] = useState<State>("proposed");
  const [error, setError] = useState("");
  const [applied, setApplied] = useState(0);

  const confirm = async () => {
    setError("");
    setState("saving");
    let done = 0;
    try {
      // In order, stopping at the first failure. A later step often depends on an earlier one
      // having landed, and carrying on past a failure would leave a half-applied change nobody
      // asked for — so the count of what did apply is kept and reported.
      for (const step of proposal.steps) {
        await api.send(step.method, step.path, step.body);
        done += 1;
        setApplied(done);
      }
      setState("done");
      toast.success(`${proposal.target} updated`);
    } catch (err) {
      setApplied(done);
      setState("proposed");
      setError(
        done === 0
          ? errorMessage(err)
          : `${errorMessage(err)} — ${done} of ${proposal.steps.length} step(s) had already been applied.`,
      );
    }
  };

  if (state === "dismissed") return null;

  const href = proposal.kind === "approval" ? "/approvers/" : "/workspaces/";

  return (
    <div className="rounded-lg border border-ai-border bg-ai-soft/40 px-3 py-2.5">
      <div className="flex items-center gap-2">
        <Sparkles className="size-3.5 shrink-0 text-ai" />
        <p className="min-w-0 flex-1 truncate text-xs font-medium">
          {state === "done" ? "Applied to" : "Proposed for"} {proposal.target}
        </p>
      </div>

      <dl className="mt-2 space-y-1.5">
        {proposal.changes.map((c) => {
          // Short values read as one line — "inherit → on" is the whole story. Prose does not:
          // an arrow in the middle of a wrapped paragraph leaves a strikethrough column two
          // words wide beside it, and neither half is legible. Those stack instead.
          const long = c.from.trim().length > 32 || c.to.trim().length > 32;
          return (
            <div key={c.key}>
              <dt className="eyebrow text-muted-foreground">{c.label}</dt>
              {long ? (
                <dd className="space-y-0.5 text-xs break-words">
                  <p className="text-muted-foreground line-through">{short(c.from)}</p>
                  <p className="flex items-start gap-1.5 font-medium">
                    <ArrowRight className="mt-0.5 size-3 shrink-0 text-muted-foreground" />
                    <span>{short(c.to)}</span>
                  </p>
                </dd>
              ) : (
                <dd className="flex items-start gap-1.5 text-xs break-words">
                  <span className="text-muted-foreground line-through">{short(c.from)}</span>
                  <ArrowRight className="mt-0.5 size-3 shrink-0 text-muted-foreground" />
                  <span className="font-medium">{short(c.to)}</span>
                </dd>
              )}
            </div>
          );
        })}
      </dl>

      {/* What Confirm will actually do, when it is more than the one save the diff implies.
          A card that changes two things in two places should say so before it is pressed. */}
      {proposal.steps.length > 1 && (
        <ul className="mt-2 space-y-0.5">
          {proposal.steps.map((s, i) => (
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
        <p className="mt-2 flex items-center gap-1.5 text-xs text-success-text">
          <Check className="size-3.5" />
          Saved.{" "}
          <Link href={href} className="underline underline-offset-2">
            Open the page
          </Link>
        </p>
      ) : (
        <>
          <p className="mt-2 text-xs text-muted-foreground">Nothing has changed yet.</p>
          <div className="mt-2 flex items-center gap-2">
            <Button size="sm" onClick={confirm} disabled={state === "saving"}>
              {state === "saving" && <Loader2 className="size-3.5 animate-spin" />}
              Confirm
            </Button>
            <Button size="sm" variant="ghost" onClick={() => setState("dismissed")} disabled={state === "saving"}>
              Dismiss
            </Button>
          </div>
        </>
      )}
    </div>
  );
}
