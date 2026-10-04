"use client";

import { useState } from "react";
import { RelativeTime } from "@/components/core/relative-time";
import { SearchField } from "@/components/core/search-field";
import { StatusChip } from "@/components/core/status-chip";
import { PausedChip } from "@/components/reviews/review-access";
import { reposOf } from "@/components/reviews/review-format";
import { isRunActive, runStatus, skipLabel } from "@/components/reviews/review-run-format";
import { ReviewsTabLink } from "@/components/reviews/reviews-nav";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import type { ReviewPull, ReviewPullsResponse, ReviewSettingsTree } from "@/lib/api";
import { cn } from "@/lib/utils";

// Picking a pull request to review: a repository from the settings tree, then one of its open pull
// requests as GitHub lists them now. Start review and a type's Try on a PR both begin here, so the
// two lists read the same and a pull request is found the same way in either.

/** Every repository a connection still reviews: a stopped one refuses a start, so it is not offered. */
export function reviewableRepos(tree: ReviewSettingsTree | undefined): string[] | null {
  if (!tree) return null;
  return [...new Set(tree.connections.filter((c) => !c.removed_at).flatMap((c) => reposOf(c).map((r) => r.repo ?? "")))]
    .filter(Boolean)
    .sort();
}

export function RepoField({
  id,
  repos,
  error,
  value,
  onChange,
}: {
  id: string;
  /** null while the tree loads. */
  repos: string[] | null;
  error?: string;
  value: string;
  onChange: (repo: string) => void;
}) {
  return (
    <div className="space-y-1.5">
      <Label htmlFor={id}>Repository</Label>
      {error && !repos ? (
        <p className="text-sm text-danger">{error}</p>
      ) : !repos ? (
        <Skeleton className="h-9 w-full" />
      ) : repos.length === 0 ? (
        <p className="text-sm text-muted-foreground">No repository is reviewed yet. Add a connection under Settings first.</p>
      ) : (
        <Select value={value} onValueChange={onChange}>
          <SelectTrigger id={id} className="w-full font-mono">
            <SelectValue placeholder="Pick a repository" />
          </SelectTrigger>
          <SelectContent>
            {repos.map((r) => (
              <SelectItem key={r} value={r} className="font-mono">
                {r}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      )}
    </div>
  );
}

export function PullField({
  id,
  repo,
  pulls,
  error,
  pr,
  onPick,
}: {
  id: string;
  repo: string;
  pulls: ReviewPullsResponse | undefined;
  error?: string;
  pr: number | null;
  onPick: (n: number) => void;
}) {
  const [query, setQuery] = useState("");
  const [typed, setTyped] = useState("");
  const q = query.trim().toLowerCase();
  const listed = (pulls?.pulls ?? []).filter(
    (p) => !q || String(p.number).includes(q) || p.title.toLowerCase().includes(q) || p.head.toLowerCase().includes(q),
  );
  return (
    // min-w-0: a fieldset is as wide as its widest line by default, which pushes a dialog off a phone.
    <fieldset className="min-w-0 space-y-1.5">
      <legend className="mb-1.5 text-sm font-medium">Pull request</legend>
      {error && !pulls ? (
        <p className="text-sm text-danger">Could not list them: {error}</p>
      ) : !pulls ? (
        <div className="space-y-1.5">
          <Skeleton className="h-11 w-full" />
          <Skeleton className="h-11 w-full" />
        </div>
      ) : pulls.pulls.length === 0 ? (
        <p className="rounded-lg border border-dashed px-3 py-3 text-sm text-muted-foreground">No pull request is open on {repo}.</p>
      ) : (
        <>
          {pulls.pulls.length > 6 && (
            <SearchField
              value={query}
              onChange={setQuery}
              clearable
              placeholder="Search by number, title or branch"
              onKeyDown={(e) => {
                // Enter here takes the first hit rather than submitting a half-filled form.
                if (e.key === "Enter") {
                  e.preventDefault();
                  if (listed[0]) onPick(listed[0].number);
                }
              }}
            />
          )}
          <div role="radiogroup" aria-label="Open pull requests" className="max-h-56 divide-y overflow-y-auto rounded-lg border">
            {listed.map((p) => (
              <PullOption key={p.number} pull={p} on={p.number === pr} onPick={() => onPick(p.number)} />
            ))}
            {listed.length === 0 && <p className="px-3 py-3 text-sm text-muted-foreground">Nothing matches “{query.trim()}”.</p>}
          </div>
        </>
      )}
      {pulls?.more && (
        // More open pull requests than one listing reads: the number reaches the rest.
        <div className="flex items-center gap-2 pt-1">
          <Label htmlFor={`${id}-number`} className="shrink-0 text-xs font-normal text-muted-foreground">
            Not listed? Its number
          </Label>
          <Input
            id={`${id}-number`}
            inputMode="numeric"
            value={typed}
            onChange={(e) => {
              setTyped(e.target.value);
              const n = Number(e.target.value);
              if (Number.isInteger(n) && n > 0) onPick(n);
            }}
            className="h-8 w-28"
          />
        </div>
      )}
    </fieldset>
  );
}

function PullOption({ pull: p, on, onPick }: { pull: ReviewPull; on: boolean; onPick: () => void }) {
  return (
    <button
      type="button"
      role="radio"
      aria-checked={on}
      onClick={onPick}
      className={cn(
        "flex w-full items-start gap-2.5 px-3 py-2 text-left transition-colors hover:bg-secondary/60",
        on && "bg-accent/60 hover:bg-accent/60",
      )}
    >
      <span className={cn("mt-1 flex size-3.5 shrink-0 items-center justify-center rounded-full border", on ? "border-primary" : "border-input")}>
        {on && <span className="size-1.5 rounded-full bg-primary" />}
      </span>
      <span className="min-w-0 flex-1">
        <span className="block truncate text-sm">
          <span className="font-medium tabular-nums">#{p.number}</span> {p.title}
        </span>
        <span className="block truncate text-xs text-muted-foreground">
          <span className="font-mono">
            {p.head} → {p.base}
          </span>
          {p.author ? ` · ${p.author}` : ""}
          {p.updated_at && (
            <>
              {" · "}
              <RelativeTime value={p.updated_at} />
            </>
          )}
        </span>
      </span>
      <span className="flex shrink-0 flex-col items-end gap-1">
        {p.draft && <StatusChip variant="neutral">Draft</StatusChip>}
        <PullReviewChip pull={p} />
        {p.review && <PausedChip paused={p.review} />}
      </span>
    </button>
  );
}

/**
 * How a pull request's last review stands, in one chip — said the same in the Start dialog's list
 * and on a repository's panel: reviewed at this commit, pushed to since, in the lane now, set aside
 * and why, or failed. `review.run` is the latest run in any state, so whether the pull request was
 * ever reviewed is `last_reviewed_sha`, not the run. With `link`, a chip about a run opens it — never
 * inside the Start dialog's option, which is a button already.
 */
export function PullReviewChip({ pull: p, link = false }: { pull: ReviewPull; link?: boolean }) {
  const rv = p.review;
  if (!rv) return null;
  const status = rv.status ?? "";
  const chip = rv.reviewed_head ? (
    <StatusChip variant="success">Reviewed</StatusChip>
  ) : rv.last_reviewed_sha ? (
    <StatusChip variant="warning">Pushed since review</StatusChip>
  ) : isRunActive(status) ? (
    <StatusChip variant={runStatus(status).variant}>{runStatus(status).label}</StatusChip>
  ) : rv.skip_reason && !(rv.paused && rv.skip_reason === "paused") ? (
    // A pause has a chip of its own beside this one (PausedChip), with Resume where it may be pressed.
    <StatusChip variant="neutral">Skipped: {skipLabel(rv.skip_reason)}</StatusChip>
  ) : status === "failed" ? (
    <StatusChip variant="danger">Failed</StatusChip>
  ) : null;
  if (!chip || !link || !rv.run) return chip;
  return (
    <ReviewsTabLink tab="history" params={{ run: rv.run }}>
      {chip}
    </ReviewsTabLink>
  );
}
