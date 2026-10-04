"use client";

import { useEffect, useRef, useState } from "react";
import { useSearchParams } from "next/navigation";
import { ArrowDown, ArrowUp, CircleAlert, CircleCheck, ExternalLink, Loader2, Pin, Plus, RefreshCw, Trash2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { api, errorMessage, useApi, type ReviewSettingsTree, type ReviewSkillCheck, type ReviewSkillLink } from "@/lib/api";
import { cn } from "@/lib/utils";

// A review type's skills: folders in GitHub repositories — a SKILL.md and the Markdown beside it — the
// reviewer follows besides the type's rules, cited as S1, S2… One repository may hold many skills; a
// link names one folder, and only that folder is read. The bounds and the reading are the server's
// (internal/review/skills.go, internal/app/review_skills.go); this says what it would refuse before it
// is asked, and asks it what a link reads now with Check.

export const MAX_SKILLS = 5;

export type SkillDraft = { uid: string; repo: string; path: string; ref: string };

let suids = 0;
const skillUid = () => `s${++suids}`;

export function skillDraft(l: ReviewSkillLink): SkillDraft {
  return { uid: skillUid(), repo: l.repo ?? "", path: l.path, ref: l.ref ?? "" };
}

/** The link as the server stores it: trimmed, the path without "./" or slashes at either end. */
export function skillLink(d: Pick<SkillDraft, "repo" | "path" | "ref">): ReviewSkillLink {
  const out: ReviewSkillLink = { path: d.path.trim().replace(/^(\.\/)+/, "").replace(/^\/+|\/+$/g, "") };
  if (d.repo.trim()) out.repo = d.repo.trim();
  if (d.ref.trim()) out.ref = d.ref.trim();
  return out;
}

/** A link's identity, for comparing drafts and for knowing which link a check answered. */
const linkKey = (l: ReviewSkillLink) => `${l.repo ?? ""}|${l.ref ?? ""}|${l.path}`;

// A folder or file on github.com, as review.ParseSkillURL reads one: /owner/name/tree/<ref>/<path>.
const GITHUB_URL = /^https?:\/\/(?:www\.)?github\.com\/([A-Za-z0-9-]+\/[A-Za-z0-9._-]+?)(?:\.git)?\/(?:tree|blob)\/([^/?#]+)\/([^?#]+?)\/?(?:[?#].*)?$/;

export function parseSkillURL(raw: string): ReviewSkillLink | null {
  const m = GITHUB_URL.exec(raw.trim());
  if (!m) return null;
  try {
    return skillLink({ repo: m[1], ref: m[2], path: decodeURIComponent(m[3]) });
  } catch {
    return null;
  }
}

const REPO = /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})\/[A-Za-z0-9._-]{1,100}$/;
const SHA = /^[0-9a-f]{40}$/;

/** What the server would refuse in the list, one line per problem. */
export function skillProblems(skills: SkillDraft[]): string[] {
  const out: string[] = [];
  if (skills.length > MAX_SKILLS) out.push(`At most ${MAX_SKILLS} skills.`);
  const seen = new Set<string>();
  skills.forEach((d, i) => {
    const l = skillLink(d);
    const id = `S${i + 1}`;
    if (!l.path) out.push(`${id} needs a folder or a file.`);
    else if (l.path.split("/").some((s) => s === "." || s === ".." || s === "") || /[\\*?[\]{}]/.test(l.path))
      out.push(`${id}'s path is a plain path from the repository's root, such as skills/review.`);
    if (l.repo && !REPO.test(l.repo)) out.push(`${id}'s repository is owner/name.`);
    if (l.ref && !l.repo) out.push(`${id} is in the repository under review, read at each pull request's base: it takes no branch or commit.`);
    if (l.path && seen.has(linkKey(l))) out.push(`${id} is linked twice.`);
    seen.add(linkKey(l));
  });
  return out;
}

type CheckState = { busy: boolean; result?: ReviewSkillCheck; error?: string };

export function SkillsList({
  skills,
  saved,
  ro,
  canCheck,
  onChange,
}: {
  skills: SkillDraft[];
  /** The links as the type's version on screen saved them: checked as it opens, and as another is taken. */
  saved: ReviewSkillLink[];
  ro: boolean;
  /** Check reads GitHub on the server, which needs Manage reviews. */
  canCheck: boolean;
  onChange: (next: SkillDraft[]) => void;
}) {
  const params = useSearchParams();
  const tree = useApi<ReviewSettingsTree>(canCheck && skills.some((d) => !d.repo.trim()) ? "/api/review-settings" : null);
  const repos = reviewedRepos(tree.data);
  const node = params.get("node") ?? "";
  // A link to the repository under review is checked in one of the organisation's repositories:
  // the one open in Settings when there is one, else the first.
  const [pickedAgainst, setAgainst] = useState("");
  const against = pickedAgainst || (repos.includes(node) ? node : (repos[0] ?? ""));

  // Checks are kept by the link they read, not by its row: the editor redraws its rows from each
  // version it takes — its own save, a card confirmed in the assistant, the newest loaded over a refused
  // save — and a link those leave as it was still has its answer.
  const [checks, setChecks] = useState<Record<string, CheckState>>({});
  // What has been checked, for check() to read without waiting on a render: kept in step by every
  // write below rather than copied from state during one.
  const checksNow = useRef<Record<string, CheckState>>({});
  const putCheck = (key: string, v: CheckState) =>
    setChecks((c) => {
      const out = { ...c, [key]: v };
      checksNow.current = out;
      return out;
    });
  const checkKey = (d: SkillDraft) => {
    const l = skillLink(d);
    return linkKey(l) + (l.repo ? "" : `@${against}`);
  };

  /**
   * Checks a link, unless it was checked as it now reads — leaving a field nobody changed must not
   * redraw the status under the pointer, whose Pin button a click is on its way to. force is the
   * person asking again. A check of the same link keeps the last answer on screen while it runs.
   */
  const check = async (d: SkillDraft, force = false) => {
    const l = skillLink(d);
    if (!canCheck || !l.path || skillProblems([d]).length > 0 || (!l.repo && !against)) return;
    const key = checkKey(d);
    const prev = checksNow.current[key];
    if (!force && (prev?.busy || prev?.result)) return;
    checksNow.current = { ...checksNow.current, [key]: { busy: true, result: prev?.result } };
    putCheck(key, { busy: true, result: prev?.result });
    try {
      const result = await api.post<ReviewSkillCheck>("/api/review-types/skill-check", { ...l, against: l.repo ? undefined : against });
      putCheck(key, { busy: false, result });
    } catch (err) {
      putCheck(key, { busy: false, error: errorMessage(err) });
    }
  };

  // The saved links are checked as the editor opens, and again when a version taken in its place links
  // others: those in another repository at once, those in the repository under review once there is a
  // repository to check them in. One already checked as it reads keeps its answer and is not asked again.
  const savedKeys = saved.map(linkKey).join("\n");
  useEffect(() => {
    if (!canCheck) return;
    saved.forEach((l) => void check(skillDraft(l)));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [canCheck, savedKeys]);
  // And every link to the repository under review, saved or not, again when that repository changes.
  useEffect(() => {
    if (!canCheck || !against) return;
    skills.filter((d) => !d.repo.trim() && skillLink(d).path).forEach((d) => void check(d));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [canCheck, against]);

  const set = (uid: string, patch: Partial<SkillDraft>) => onChange(skills.map((d) => (d.uid === uid ? { ...d, ...patch } : d)));
  const move = (i: number, by: -1 | 1) => {
    const j = i + by;
    if (j < 0 || j >= skills.length) return;
    const next = [...skills];
    [next[i], next[j]] = [next[j], next[i]];
    onChange(next);
  };
  const drop = (uid: string) => onChange(skills.filter((d) => d.uid !== uid));
  const added = useRef<string | null>(null);
  const add = () => {
    const d = skillDraft({ path: "" });
    added.current = d.uid;
    onChange([...skills, d]);
  };

  /**
   * A github.com address pasted into either field fills all three, and is checked; one typed is read
   * when the field is left, since halfway through typing it already reads as an address.
   */
  const fromURL = (d: SkillDraft, text: string): boolean => {
    const l = parseSkillURL(text);
    if (!l) return false;
    const next = { ...d, repo: l.repo ?? "", path: l.path, ref: l.ref ?? "" };
    set(d.uid, next);
    void check(next);
    return true;
  };

  const full = skills.length >= MAX_SKILLS;
  const anyHere = skills.some((d) => !d.repo.trim());
  return (
    <section className="space-y-2">
      <div className="flex flex-wrap items-baseline justify-between gap-2 px-1">
        <h2 className="text-sm font-semibold">
          Skills{" "}
          <span className={cn("font-normal tabular-nums text-muted-foreground", skills.length > MAX_SKILLS && "text-danger")}>
            {skills.length} of {MAX_SKILLS}
          </span>
        </h2>
        <span className="text-xs text-muted-foreground">Findings cite them as S1, S2… by place.</span>
      </div>
      <div className="overflow-hidden rounded-xl border bg-card">
        <p className="border-b px-4 py-3 text-xs text-muted-foreground">
          A folder in a GitHub repository that the reviewer follows as well as the rules: its <span className="font-mono">SKILL.md</span>{" "}
          and the Markdown files beside it. One repository can hold many skills; only the folder you name is read. Paste its
          github.com address, or give the repository and the folder. Leave the repository empty for a folder in the repository
          being reviewed, which is read at each pull request&apos;s base, so a pull request cannot rewrite its own review. A
          public repository that isn&apos;t yours is read without credentials; pin it to a commit to keep someone else&apos;s
          push out of your reviews.
        </p>
        <ol className="divide-y">
          {skills.map((d, i) => {
            const id = `S${i + 1}`;
            const current = checks[checkKey(d)];
            return (
              <li key={d.uid} className="px-3 py-3">
                <div className="flex items-start gap-2.5">
                  <span className="mt-2 w-7 shrink-0 text-xs font-medium tabular-nums text-muted-foreground">{id}</span>
                  <div className="min-w-0 flex-1 space-y-1.5">
                    {/* Repository and ref side by side, the folder under them at full width: it is the
                        longest of the three and the one that says which skill. Tab order stays
                        repository, folder, ref. */}
                    <div className="grid grid-cols-[minmax(0,1fr)_minmax(0,9rem)] gap-2">
                      <Input
                        ref={(el) => {
                          if (el && added.current === d.uid) {
                            added.current = null;
                            el.focus();
                          }
                        }}
                        aria-label={`${id} repository`}
                        value={d.repo}
                        disabled={ro}
                        placeholder="This repository"
                        onPaste={(e) => fromURL(d, e.clipboardData.getData("text")) && e.preventDefault()}
                        onChange={(e) => set(d.uid, { repo: e.target.value, ...(e.target.value.trim() ? {} : { ref: "" }) })}
                        onBlur={() => fromURL(d, d.repo) || void check(d)}
                        className="h-8 font-mono text-xs md:text-xs"
                        aria-invalid={!!d.repo.trim() && !REPO.test(d.repo.trim())}
                      />
                      <Input
                        aria-label={`${id} folder or file`}
                        value={d.path}
                        disabled={ro}
                        placeholder="skills/code-review, or paste a github.com link"
                        onPaste={(e) => fromURL(d, e.clipboardData.getData("text")) && e.preventDefault()}
                        onChange={(e) => set(d.uid, { path: e.target.value })}
                        onBlur={() => fromURL(d, d.path) || void check(d)}
                        className="col-span-2 row-start-2 h-8 font-mono text-xs md:text-xs"
                        aria-invalid={!skillLink(d).path}
                      />
                      <Input
                        aria-label={`${id} branch, tag or commit`}
                        value={d.ref}
                        disabled={ro || !d.repo.trim()}
                        placeholder={d.repo.trim() ? "Default branch" : "The PR's base"}
                        onChange={(e) => set(d.uid, { ref: e.target.value })}
                        onBlur={() => void check(d)}
                        className="col-start-2 row-start-1 h-8 font-mono text-xs md:text-xs"
                      />
                    </div>
                    {canCheck && <SkillStatus d={d} state={current} onCheck={() => void check(d, true)} onPin={(sha) => {
                      const next = { ...d, ref: sha };
                      set(d.uid, { ref: sha });
                      void check(next);
                    }} ro={ro} />}
                  </div>
                  {!ro && (
                    <span className="mt-0.5 flex items-center">
                      <Button variant="ghost" size="icon-xs" disabled={i === 0} onClick={() => move(i, -1)} aria-label={`Move ${id} up`}>
                        <ArrowUp />
                      </Button>
                      <Button variant="ghost" size="icon-xs" disabled={i === skills.length - 1} onClick={() => move(i, 1)} aria-label={`Move ${id} down`}>
                        <ArrowDown />
                      </Button>
                      <Button variant="ghost" size="icon-xs" onClick={() => drop(d.uid)} aria-label={`Remove ${id}`}>
                        <Trash2 />
                      </Button>
                    </span>
                  )}
                </div>
              </li>
            );
          })}
          {skills.length === 0 && (
            <li className="px-4 py-5 text-center text-sm text-muted-foreground">No skills: the reviewer goes by what the type is for and its rules.</li>
          )}
        </ol>
        {canCheck && anyHere && repos.length > 0 && (
          <div className="flex flex-wrap items-center gap-2 border-t px-4 py-2.5">
            <Label htmlFor="skills-against" className="text-xs font-normal text-muted-foreground">
              Check links to the repository under review in
            </Label>
            <Select value={against} onValueChange={setAgainst}>
              <SelectTrigger id="skills-against" size="sm" className="h-7 w-auto max-w-72 gap-1 px-2 font-mono text-xs">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {repos.map((r) => (
                  <SelectItem key={r} value={r} className="font-mono text-xs">
                    {r}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <span className="text-xs text-muted-foreground">at its default branch</span>
          </div>
        )}
      </div>
      {!ro && (
        <div className="flex items-center gap-3 px-1">
          <Button variant="outline" size="sm" onClick={add} disabled={full}>
            <Plus /> Add skill
          </Button>
          {full && <span className="text-xs text-muted-foreground">Five is the most one type follows.</span>}
        </div>
      )}
    </section>
  );
}

function SkillStatus({
  d,
  state,
  onCheck,
  onPin,
  ro,
}: {
  d: SkillDraft;
  state?: CheckState;
  onCheck: () => void;
  onPin: (sha: string) => void;
  ro: boolean;
}) {
  const l = skillLink(d);
  if (!l.path) return null;
  if (!state) {
    return (
      <div className="flex items-center gap-2 text-xs text-muted-foreground">
        Not checked yet.
        <Button variant="ghost" size="xs" onClick={onCheck}>
          <RefreshCw /> Check
        </Button>
      </div>
    );
  }
  if (state.busy && !state.result) {
    return (
      <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
        <Loader2 className="size-3.5 animate-spin" /> Reading it from GitHub…
      </p>
    );
  }
  const r = state.result;
  const failed = state.error ?? r?.error;
  if (failed || !r) {
    return (
      <div className="flex flex-wrap items-center gap-2 text-xs text-danger">
        <CircleAlert className="size-3.5 shrink-0" />
        <span className="min-w-0">{failed ?? "Could not be checked."}</span>
        <Button variant="ghost" size="xs" onClick={onCheck} className="text-muted-foreground">
          <RefreshCw /> Check again
        </Button>
      </div>
    );
  }
  const files = r.files ?? [];
  const shown = files.slice(0, 4).map((f) => f.path);
  const url = r.repo && r.sha ? `https://github.com/${r.repo}/tree/${r.sha}/${l.path}` : "";
  const pinnable = !ro && !!r.sha && !!l.repo && !SHA.test(l.ref ?? "") && r.public;
  return (
    <div className="space-y-1 rounded-lg border bg-muted/30 px-2.5 py-2 text-xs">
      <p className="flex flex-wrap items-center gap-x-1.5 gap-y-0.5">
        <CircleCheck className="size-3.5 shrink-0 text-success" />
        <span className="font-medium text-foreground">{r.name}</span>
        <span className="text-muted-foreground">
          · {files.length} {files.length === 1 ? "file" : "files"} · {(r.chars ?? 0).toLocaleString("en-US")} characters ·{" "}
          <span className="font-mono">
            {r.repo}@{r.sha?.slice(0, 7)}
          </span>
          {r.here ? " (as checked; a review reads each pull request's base)" : ""}
        </span>
        {url && (
          <a href={url} target="_blank" rel="noreferrer" className="inline-flex items-center gap-0.5 text-primary underline-offset-2 hover:underline">
            Open <ExternalLink className="size-3" />
          </a>
        )}
        <Button variant="ghost" size="xs" onClick={onCheck} disabled={state.busy} className="ml-auto text-muted-foreground" aria-label="Check again">
          {state.busy ? <Loader2 className="animate-spin" /> : <RefreshCw />}
        </Button>
      </p>
      {r.description && <p className="line-clamp-2 text-muted-foreground">{r.description}</p>}
      <p className="font-mono text-[11px] text-muted-foreground">
        {shown.join(", ")}
        {files.length > shown.length ? `, +${files.length - shown.length}` : ""}
      </p>
      {(r.omitted?.length ?? 0) > 0 && (
        <p className="text-muted-foreground">Not read: {r.omitted!.slice(0, 3).join("; ")}{r.omitted!.length > 3 ? ` (+${r.omitted!.length - 3})` : ""}</p>
      )}
      {(r.warnings ?? []).map((w) => (
        <p key={w} className="flex items-start gap-1.5 text-warning">
          <CircleAlert className="mt-0.5 size-3.5 shrink-0" />
          <span className="min-w-0">{w}</span>
        </p>
      ))}
      {pinnable && (
        <Button variant="outline" size="xs" onClick={() => onPin(r.sha!)}>
          <Pin /> Pin to {r.sha!.slice(0, 7)}
        </Button>
      )}
    </div>
  );
}

/** The repositories in Reviews › Settings, for checking a link to the repository under review. */
function reviewedRepos(tree?: ReviewSettingsTree): string[] {
  if (!tree) return [];
  const out: string[] = [];
  for (const c of tree.connections) {
    for (const r of c.repos) if (r.repo && !out.includes(r.repo)) out.push(r.repo);
    for (const g of c.groups) for (const r of g.repos) if (r.repo && !out.includes(r.repo)) out.push(r.repo);
  }
  return out.sort();
}
