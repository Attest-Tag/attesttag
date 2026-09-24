import { branchURL, repoURL } from "@/components/jobs/job-format";

// GitHub links for the repository and branch a job worked on. Both fall back to
// plain text when the repo is not the "owner/name" a GitHub connection stores,
// so a job from another provider still reads the same, just without the link.
//
// Not a "use client" module: no state, no hooks, so it compiles into whichever
// graph imports it. `stopPropagation` is here rather than at the call site
// because every table row that shows a repo also opens the job on click.

const linkClass = "text-primary underline-offset-2 hover:underline";

export function RepoLink({ repo, className }: { repo: string; className?: string }) {
  const href = repoURL(repo);
  if (!href) return <>{repo || "—"}</>;
  return (
    <a
      href={href}
      target="_blank"
      rel="noreferrer"
      onClick={(e) => e.stopPropagation()}
      className={className ?? linkClass}
    >
      {repo}
    </a>
  );
}

export function BranchLink({
  repo,
  branch,
  className,
}: {
  repo: string;
  branch: string;
  className?: string;
}) {
  const href = branchURL(repo, branch);
  if (!href) return <>{branch || "—"}</>;
  return (
    <a
      href={href}
      target="_blank"
      rel="noreferrer"
      onClick={(e) => e.stopPropagation()}
      className={className ?? linkClass}
    >
      {branch}
    </a>
  );
}
