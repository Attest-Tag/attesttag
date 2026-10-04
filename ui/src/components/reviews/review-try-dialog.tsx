"use client";

import { useState } from "react";
import { AlertTriangle, FlaskConical, History } from "lucide-react";
import { StatusChip } from "@/components/core/status-chip";
import { planRefusalLine, usePlanRefusal } from "@/components/reviews/review-access";
import { PullField, RepoField, reviewableRepos } from "@/components/reviews/review-pull-picker";
import { RunFindings, useReviewRun, useTypeNames } from "@/components/reviews/review-run-detail";
import { formatDuration, isRunActive, runCost, runStatus } from "@/components/reviews/review-run-format";
import { useReviewsNav } from "@/components/reviews/reviews-nav";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  api,
  errorMessage,
  useApi,
  type ReviewPullsResponse,
  type ReviewSettingsTree,
  type ReviewStartResponse,
  type ReviewTypeInput,
} from "@/lib/api";

// Try on a PR: the type as it stands in the editor, saved or not, run on a pull request somebody
// picks (POST /api/review-types/try). Always in shadow — nothing it finds is posted, and the pull
// request's own review takes no notice of it — so a new rule is seen at work before it can say
// anything on GitHub. The dialog follows the run until it is done and shows what it kept here; the
// rest of it, dropped candidates included, is the run's detail in History.

export function TryTypeDialog({
  open,
  onOpenChange,
  typeName,
  versionNote,
  input,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  typeName: string;
  /** "v3 with your unsaved changes": what exactly is being tried. */
  versionNote: string;
  /** The type as the editor holds it now, read when Try is pressed. */
  input: () => ReviewTypeInput;
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[88vh] overflow-y-auto sm:max-w-2xl">
        {open && <TryBody typeName={typeName} versionNote={versionNote} input={input} onClose={() => onOpenChange(false)} />}
      </DialogContent>
    </Dialog>
  );
}

function TryBody({
  typeName,
  versionNote,
  input,
  onClose,
}: {
  typeName: string;
  versionNote: string;
  input: () => ReviewTypeInput;
  onClose: () => void;
}) {
  const [repo, setRepo] = useState("");
  const [pr, setPr] = useState<number | null>(null);
  const [runId, setRunId] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const go = useReviewsNav();
  const nameOf = useTypeNames();
  // A try is a run, which the plan refuses where it has no code review: said before Try it, not after.
  const refused = usePlanRefusal();

  const tree = useApi<ReviewSettingsTree>("/api/review-settings");
  const pulls = useApi<ReviewPullsResponse>(repo ? `/api/review-pulls?repo=${encodeURIComponent(repo)}` : null);
  const run = useReviewRun(runId);

  const tryIt = async () => {
    if (!repo || !pr || busy || refused) return;
    setBusy(true);
    setError("");
    try {
      const out = await api.post<ReviewStartResponse>("/api/review-types/try", { type: input(), repo, pr });
      setRunId(out.run.id);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const detail = run.data;
  const st = detail ? runStatus(detail.run.status) : null;
  const active = !!detail && isRunActive(detail.run.status);

  return (
    <form
      className="space-y-5"
      onSubmit={(e) => {
        e.preventDefault();
        void tryIt();
      }}
    >
      <DialogHeader>
        <DialogTitle>Try {typeName} on a pull request</DialogTitle>
        <DialogDescription>
          Runs this type {versionNote}, in shadow: its findings show here and nothing is posted. The pull request&apos;s own
          review, score and summary take no notice of it. It costs what a review of one type costs.
        </DialogDescription>
      </DialogHeader>

      {!runId ? (
        <>
          <RepoField
            id="try-repo"
            repos={reviewableRepos(tree.data)}
            error={tree.error}
            value={repo}
            onChange={(v) => {
              setRepo(v);
              setPr(null);
            }}
          />
          {repo && (
            // The list for the repository picked now, never the last one's while this one loads:
            // a pull request picked there would be tried under the wrong repository's number.
            <PullField
              key={repo}
              id="try-pr"
              repo={repo}
              pulls={pulls.refreshing ? undefined : pulls.data}
              error={pulls.error}
              pr={pr}
              onPick={setPr}
            />
          )}
        </>
      ) : (
        <div className="space-y-4">
          <div className="flex flex-wrap items-center gap-2 rounded-lg border bg-muted/40 px-3 py-2.5 text-sm">
            <span className="font-mono">
              {repo}#{pr}
            </span>
            {st && <StatusChip variant={st.variant}>{st.label}</StatusChip>}
            {detail && !active && (
              <span className="text-xs text-muted-foreground">
                {runCost(detail.usage.cost_usd)} · {formatDuration(detail.timings.duration_ms)} · {detail.run.candidates} candidate
                {detail.run.candidates === 1 ? "" : "s"}, {detail.run.findings} kept
              </span>
            )}
            {(!detail || active) && <span className="text-xs text-muted-foreground">This follows it until it is done.</span>}
          </div>
          {detail?.run.error && !active && (
            <p className="rounded-lg border bg-muted/40 px-3 py-2 text-sm text-muted-foreground">{detail.run.error}</p>
          )}
          {detail ? (
            <RunFindings findings={detail.findings} nameOf={nameOf} active={active} />
          ) : (
            <p className="text-sm text-muted-foreground">Queued…</p>
          )}
          {detail && !active && detail.dropped.length > 0 && (
            <p className="text-xs text-muted-foreground">
              {detail.dropped.length} candidate{detail.dropped.length === 1 ? " was" : "s were"} dropped; the review in History says
              which and why.
            </p>
          )}
        </div>
      )}

      {refused && !runId && (
        <p className="flex gap-2 rounded-lg border border-warning/30 bg-warning-soft px-3 py-2 text-sm">
          <AlertTriangle className="mt-0.5 size-4 shrink-0 text-warning" />
          <span>
            {planRefusalLine(refused)} {refused.message}
          </span>
        </p>
      )}

      {error && (
        <p className="flex gap-2 rounded-lg border border-danger/30 bg-danger-soft px-3 py-2 text-sm">
          <AlertTriangle className="mt-0.5 size-4 shrink-0 text-danger" />
          <span>{error}</span>
        </p>
      )}

      <DialogFooter>
        {runId ? (
          <>
            <Button
              variant="outline"
              onClick={() => {
                onClose();
                go("history", { run: runId });
              }}
            >
              <History /> Open in History
            </Button>
            <Button variant="outline" disabled={active} onClick={() => setRunId(null)}>
              Try on another
            </Button>
            <Button onClick={onClose}>Done</Button>
          </>
        ) : (
          <>
            <Button variant="outline" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" disabled={!repo || !pr || !!refused} loading={busy}>
              <FlaskConical /> Try it
            </Button>
          </>
        )}
      </DialogFooter>
    </form>
  );
}
