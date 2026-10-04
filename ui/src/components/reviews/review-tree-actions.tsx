"use client";

import { useRef, useState } from "react";
import { ExternalLink } from "lucide-react";
import { toast } from "sonner";
import { useConfirm } from "@/components/core/confirm-dialog";
import { RelativeTime } from "@/components/core/relative-time";
import { SearchField } from "@/components/core/search-field";
import { StatusChip } from "@/components/core/status-chip";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import {
  api,
  errorMessage,
  useApi,
  type ReviewAddReposResult,
  type ReviewAvailableInstall,
  type ReviewAvailableRepos,
  type ReviewConnectionNode,
  type ReviewGroupNode,
  type ReviewNode,
  type ReviewSettingsTree,
} from "@/lib/api";
import {
  MODES,
  choiceLabel,
  connectionName,
  installationURL,
  reposOf,
  settingsNeedReach,
  stoppedBecause,
  type Selection,
} from "@/components/reviews/review-format";
import type { TreeActions } from "@/components/reviews/review-tree";

// What the rail's menus and the panels' buttons do to the tree, and the dialogs they ask through.
// One hook for both, because Stop reviewing is on a connection's row menu and at the foot of its
// panel, and the two must ask the same question in the same words.

const GROUP_NAME_MAX = 80;
const DEFAULTS = "__defaults";
/** The most repositories one Add saves: the server's cap on a request (autoConnectMax). */
const ADD_REPOS_MAX = 50;

type NameDialog =
  | { mode: "new"; conn: ReviewConnectionNode; repo?: ReviewNode }
  | { mode: "rename"; group: ReviewGroupNode };

export function useReviewTreeActions({
  tree,
  canReach,
  onChanged,
  onSelectNode,
}: {
  tree: ReviewSettingsTree | undefined;
  canReach: boolean;
  /** Reload the tree, and the open panel with it. */
  onChanged: () => void;
  /** Select a node once the reloaded tree has it. */
  onSelectNode: (sel: Selection) => void;
}) {
  const { confirm, confirmDialog } = useConfirm();
  const [adding, setAdding] = useState<ReviewAvailableInstall | null>(null);
  const [naming, setNaming] = useState<NameDialog | null>(null);
  const [addingRepos, setAddingRepos] = useState<ReviewConnectionNode | null>(null);

  const run = async (call: () => Promise<unknown>, done: string) => {
    try {
      await call();
      toast.success(done);
      onChanged();
      return true;
    } catch (err) {
      toast.error(errorMessage(err));
      return false;
    }
  };

  const actions: TreeActions = {
    add: (install) => setAdding(install),
    newGroup: (conn, repo) => setNaming({ mode: "new", conn, repo }),
    renameGroup: (group) => setNaming({ mode: "rename", group }),
    deleteGroup: async (_conn, group) => {
      const ok = await confirm({
        title: `Delete the group ${group.name}?`,
        description:
          group.repos.length > 0
            ? `Its ${group.repos.length === 1 ? "repository moves" : `${group.repos.length} repositories move`} back under the connection, keeping ${group.repos.length === 1 ? "its" : "their"} own settings, and stop inheriting what the group set.`
            : "It holds no repository; only its settings go.",
        confirmLabel: "Delete group",
        destructive: true,
      });
      if (ok) await run(() => api.del(`/api/review-settings/${group.id}`), `${group.name} deleted`);
    },
    move: (conn, repo, to) =>
      void run(
        () => api.post(`/api/review-settings/${conn.id}/move?repo=${encodeURIComponent(repo.repo ?? "")}`, { parent: to.id }),
        to.id === conn.id ? `${repo.repo} is in no group now` : `${repo.repo} moved to ${to.name}`,
      ),
    stop: async (conn) => {
      const ok = await confirm({
        title: `Stop reviewing ${connectionName(conn)}?`,
        description:
          "No pull request on its repositories is reviewed from now on, not even when somebody asks. Its settings, groups and history are kept, and Restore brings them back as they were.",
        confirmLabel: "Stop reviewing",
        destructive: true,
      });
      if (ok) await run(() => api.del(`/api/review-settings/${conn.id}`), `${connectionName(conn)} is no longer reviewed`);
    },
    restore: (conn) =>
      void run(() => api.post(`/api/review-settings/${conn.id}/restore`), `${connectionName(conn)} is reviewed again`),
    addRepos: (conn) => setAddingRepos(conn),
    removeRepo: async (conn, repo) => {
      const name = repo.repo ?? "";
      const ok = await confirm({
        title: `Remove ${name} from code review?`,
        description: (
          <>
            Nothing on it is reviewed from now on — not when a pull request opens, not when somebody asks — and it
            moves to {connectionName(conn)}&apos;s Removed list. Its settings are kept for a restore, and so is its
            saved connection under Access bundles › Repositories: Slack tools and fix jobs still use it.
          </>
        ),
        confirmLabel: "Remove from reviews",
        destructive: true,
      });
      // By name through its connection, which works whether or not anything was set on it.
      if (ok)
        await run(
          () => api.post(`/api/review-settings/${conn.id}/remove?repo=${encodeURIComponent(name)}`),
          `${name} is no longer reviewed`,
        );
    },
    restoreRepo: (_conn, repo) =>
      void run(() => api.post(`/api/review-settings/${repo.id}/restore`), `${repo.repo} is reviewed again`),
  };

  const dialogs = (
    <>
      <AddConnectionDialog
        install={adding}
        connections={tree?.connections ?? []}
        canReach={canReach}
        onOpenChange={(open) => !open && setAdding(null)}
        onAdded={(id) => {
          setAdding(null);
          onChanged();
          onSelectNode({ node: id });
        }}
      />
      <AddReposDialog
        conn={addingRepos}
        onOpenChange={(open) => !open && setAddingRepos(null)}
        onAdded={() => {
          setAddingRepos(null);
          onChanged();
        }}
      />
      <GroupNameDialog
        target={naming}
        onOpenChange={(open) => !open && setNaming(null)}
        onSaved={(sel) => {
          setNaming(null);
          onChanged();
          if (sel) onSelectNode(sel);
        }}
      />
      {confirmDialog}
    </>
  );

  return { actions, dialogs };
}

/**
 * Adding an installation to the tree: always in Shadow, whatever it starts from — going Live is a
 * decision about these repositories, made once they are here, and copying a connection that is
 * Live must not make it for somebody.
 */
function AddConnectionDialog({
  install,
  connections,
  canReach,
  onOpenChange,
  onAdded,
}: {
  install: ReviewAvailableInstall | null;
  connections: ReviewConnectionNode[];
  canReach: boolean;
  onOpenChange: (open: boolean) => void;
  onAdded: (id: string) => void;
}) {
  const [from, setFrom] = useState(DEFAULTS);
  const [busy, setBusy] = useState(false);
  const submitRef = useRef<HTMLButtonElement>(null);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!install || busy) return;
    setBusy(true);
    try {
      const out = await api.post<{ node: ReviewNode; restored: boolean }>("/api/review-settings", {
        kind: "connection",
        installation_id: install.installation_id,
        copy_from: from === DEFAULTS ? "" : from,
      });
      toast.success(
        out.restored
          ? `${connectionName(install)} is reviewed again, as it was`
          : `${connectionName(install)} added in Shadow`,
      );
      setFrom(DEFAULTS);
      onAdded(out.node.id);
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={install !== null} onOpenChange={onOpenChange}>
      <DialogContent
        className="sm:max-w-md"
        // The focus starts on Add connection, not on the Select: Enter on a Select opens its list,
        // and the built-in defaults are what most adds want, so Enter should add.
        onOpenAutoFocus={(e) => {
          e.preventDefault();
          submitRef.current?.focus();
        }}
      >
        <form onSubmit={submit} className="space-y-4">
          <DialogHeader>
            <DialogTitle>Review {install ? connectionName(install) : ""}</DialogTitle>
            <DialogDescription>
              Its pull requests are reviewed in Shadow: recorded here, with nothing posted on GitHub
              until somebody makes it Live.
              {install && install.repos.length > 0
                ? ` ${install.repos.length} ${install.repos.length === 1 ? "repository inherits" : "repositories inherit"} it at once, and any the App is given later.`
                : ""}
            </DialogDescription>
          </DialogHeader>
          {install && install.missing_permissions.length > 0 && (
            <p className="rounded-lg border border-warning/30 bg-warning-soft px-3 py-2 text-xs text-foreground">
              GitHub still has to be told to grant {install.missing_permissions.join(", ")}. Its owner accepts the new
              permissions on the installation&apos;s page at GitHub; until then reviews cannot post.
            </p>
          )}
          <div className="space-y-1.5">
            <Label htmlFor="review-add-from">Start from</Label>
            <Select value={from} onValueChange={setFrom}>
              <SelectTrigger id="review-add-from" className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={DEFAULTS}>The built-in defaults</SelectItem>
                {connections.map((c) => {
                  // Copying a model, money, forks, context or every push sets them here, which is
                  // connections.manage's to do, as it would be field by field.
                  const held = !canReach && settingsNeedReach(c.settings);
                  return (
                    <SelectItem key={c.id} value={c.id} disabled={held}>
                      A copy of {connectionName(c)}&apos;s settings{held ? " — needs Manage connections" : ""}
                    </SelectItem>
                  );
                })}
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground">
              A copy takes the connection&apos;s own settings, not its groups or repositories. A copied rule
              that posted live records in Shadow here.
            </p>
          </div>
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button ref={submitRef} type="submit" loading={busy}>
              Add connection
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

/**
 * Add repositories: what the connection's installation reaches at GitHub that code review does not
 * cover — never saved for it, or removed from reviews — read live as the dialog opens. Each one
 * ticked is saved as one of the organisation's App connections, attached to no channel, and is in
 * the tree under the connection's settings at once; one that was removed comes back as it was.
 */
function AddReposDialog({
  conn,
  onOpenChange,
  onAdded,
}: {
  conn: ReviewConnectionNode | null;
  onOpenChange: (open: boolean) => void;
  onAdded: () => void;
}) {
  // Keyed on the connection, so each opening reads GitHub afresh and starts with nothing ticked.
  return (
    <Dialog open={conn !== null} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        {conn && <AddReposForm key={conn.id} conn={conn} onCancel={() => onOpenChange(false)} onAdded={onAdded} />}
      </DialogContent>
    </Dialog>
  );
}

function AddReposForm({ conn, onCancel, onAdded }: { conn: ReviewConnectionNode; onCancel: () => void; onAdded: () => void }) {
  const list = useApi<ReviewAvailableRepos>(`/api/review-settings/${conn.id}/available`);
  const [query, setQuery] = useState("");
  const [picked, setPicked] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const repos = list.data?.repos ?? [];
  const q = query.trim().toLowerCase();
  // Past what GitHub's listing read, a repository is added by its owner/name, typed in the search:
  // the server asks GitHub about it by name. Offered only when the listing was cut short, and for one
  // neither listed nor in the tree already.
  const typed =
    list.data?.truncated &&
    /^[\w.-]+\/[\w.-]+$/.test(query.trim()) &&
    !repos.some((r) => r.repo.toLowerCase() === q) &&
    !reposOf(conn).some((r) => r.repo?.toLowerCase() === q)
      ? query.trim()
      : "";
  const listed = q ? repos.filter((r) => r.repo.toLowerCase().includes(q)) : repos;
  const shown: (ReviewAvailableRepos["repos"][number] & { unlisted?: boolean })[] = typed
    ? [{ repo: typed, private: false, removed: false, saved: false, unlisted: true }, ...listed]
    : listed;
  const allShown = shown.length > 0 && shown.every((r) => picked.includes(r.repo));
  const someShown = shown.some((r) => picked.includes(r.repo));
  const over = picked.length > ADD_REPOS_MAX;
  const stopped = stoppedBecause(conn);

  const toggle = (repo: string) => setPicked((p) => (p.includes(repo) ? p.filter((x) => x !== repo) : [...p, repo]));
  const toggleShown = () =>
    setPicked((p) =>
      allShown ? p.filter((x) => !shown.some((r) => r.repo === x)) : [...new Set([...p, ...shown.map((r) => r.repo)])],
    );

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (busy || picked.length === 0 || over) return;
    setBusy(true);
    try {
      const out = await api.post<ReviewAddReposResult>(`/api/review-settings/${conn.id}/repos`, { repos: picked });
      // One repository, however the server lists it: saved again and restored is still one.
      const done = [...new Set([...out.added, ...out.restored].map((r) => r.toLowerCase()))];
      if (done.length > 0) {
        toast.success(
          done.length === 1
            ? `${done[0]} ${out.restored.length === 1 ? "is reviewed again" : "added to code review"}`
            : `${done.length} repositories added to code review${out.restored.length > 0 ? `, ${out.restored.length} of them restored` : ""}`,
        );
      } else if (out.already.length > 0) {
        toast.success(out.already.length === 1 ? `${out.already[0]} was in code review already` : "They were in code review already");
      }
      for (const f of out.failed) toast.error(`${f.repo}: ${f.error}`);
      onAdded();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    // The search box is the first thing focused, and Enter in it adds what is ticked: one form, one
    // submit button, which nothing ticked holds disabled.
    <form onSubmit={submit} className="space-y-4">
      <DialogHeader>
        <DialogTitle>Add repositories to {connectionName(conn)}</DialogTitle>
        <DialogDescription>
          The repositories the GitHub App reaches on {conn.account_login || "this account"} that code review does not
          cover yet, read from GitHub now. Each one added is saved under Access bundles › Repositories, attached to no
          channel, and reviewed under this connection&apos;s settings
          {stopped ? ` once it is reviewed again — ${stopped.why}.` : ` at once: ${choiceLabel(MODES, conn.mode ?? "off")}.`}
        </DialogDescription>
      </DialogHeader>
      <div className="space-y-2">
        <SearchField value={query} onChange={setQuery} placeholder="Search repositories" clearable />
        <div className="overflow-hidden rounded-lg border">
          {list.error && !list.data ? (
            <div className="space-y-2 px-3 py-4 text-sm">
              <p className="text-danger">Could not read what the installation reaches: {list.error}</p>
              <Button type="button" size="sm" variant="outline" onClick={list.reload}>
                Try again
              </Button>
            </div>
          ) : !list.data ? (
            <div className="space-y-2 p-3">
              <Skeleton className="h-7 w-full" />
              <Skeleton className="h-7 w-2/3" />
              <Skeleton className="h-7 w-3/4" />
            </div>
          ) : repos.length === 0 && !typed ? (
            <p className="px-3 py-4 text-sm text-muted-foreground">
              {list.data.truncated
                ? "Every repository listed is in code review already. Type another's owner/name in the search to add it."
                : "Every repository this installation reaches is in code review already. To review another, give the App access to it at GitHub first."}
            </p>
          ) : (
            <>
              <label className="flex items-center gap-3 border-b bg-muted/40 px-3 py-2 text-xs text-muted-foreground">
                <Checkbox
                  checked={allShown ? true : someShown ? "indeterminate" : false}
                  onCheckedChange={toggleShown}
                  disabled={shown.length === 0}
                  aria-label={q ? "Tick every repository shown" : "Tick every repository"}
                />
                <span className="flex-1">
                  {q ? `${listed.length} of ${repos.length} shown` : `${repos.length} ${repos.length === 1 ? "repository" : "repositories"}`}
                </span>
                {picked.length > 0 && <span>{picked.length} ticked</span>}
              </label>
              {shown.length === 0 ? (
                <p className="px-3 py-4 text-sm text-muted-foreground">{`No repository matches “${query.trim()}”.`}</p>
              ) : (
                <ul className="max-h-72 divide-y overflow-y-auto">
                  {shown.map((r) => (
                    <li key={r.repo}>
                      <label className="flex cursor-pointer items-center gap-3 px-3 py-2 hover:bg-secondary/60">
                        <Checkbox checked={picked.includes(r.repo)} onCheckedChange={() => toggle(r.repo)} />
                        <span className="min-w-0 flex-1">
                          <span className="block truncate font-mono text-xs">{r.repo}</span>
                          {r.unlisted ? (
                            <span className="block truncate text-xs text-muted-foreground">
                              Not in the list read — GitHub is asked about it when it is added
                            </span>
                          ) : r.removed ? (
                            <span className="block truncate text-xs text-muted-foreground">
                              Removed from reviews — adding it restores its settings
                            </span>
                          ) : r.pushed_at ? (
                            <span className="block truncate text-xs text-muted-foreground">
                              Pushed <RelativeTime value={r.pushed_at} />
                            </span>
                          ) : null}
                        </span>
                        {!r.unlisted && (
                          <StatusChip variant={r.private ? "neutral" : "info"}>{r.private ? "Private" : "Public"}</StatusChip>
                        )}
                      </label>
                    </li>
                  ))}
                </ul>
              )}
            </>
          )}
        </div>
        {list.data?.truncated && (
          <p className="text-xs text-muted-foreground">
            The installation reaches more repositories than one listing reads, so some are not shown here: type one&apos;s
            owner/name in the search to add it.
          </p>
        )}
        {over && <p className="text-xs text-danger">{`At most ${ADD_REPOS_MAX} at a time: untick ${picked.length - ADD_REPOS_MAX}.`}</p>}
        {/* After the list in the tab order: the way to reach a repository the App was not given. */}
        <a
          href={installationURL(conn)}
          target="_blank"
          rel="noreferrer"
          className="inline-flex items-center gap-1 text-xs font-medium text-primary underline-offset-2 hover:underline"
        >
          Choose which repositories the App reaches, at GitHub <ExternalLink className="size-3" />
        </a>
      </div>
      <DialogFooter>
        <Button type="button" variant="ghost" onClick={onCancel}>
          Cancel
        </Button>
        <Button type="submit" disabled={picked.length === 0 || over} loading={busy}>
          {picked.length > 1 ? `Add ${picked.length} repositories` : "Add repository"}
        </Button>
      </DialogFooter>
    </form>
  );
}

function GroupNameDialog({
  target,
  onOpenChange,
  onSaved,
}: {
  target: NameDialog | null;
  onOpenChange: (open: boolean) => void;
  onSaved: (sel: Selection | null) => void;
}) {
  const current = target?.mode === "rename" ? target.group.name : "";
  // Keyed on the target so each opening starts from the name it is about, not the last one typed.
  return (
    <Dialog open={target !== null} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-sm">
        {target && <GroupNameForm key={target.mode === "new" ? `new:${target.conn.id}` : target.group.id} target={target} current={current} onCancel={() => onOpenChange(false)} onSaved={onSaved} />}
      </DialogContent>
    </Dialog>
  );
}

function GroupNameForm({
  target,
  current,
  onCancel,
  onSaved,
}: {
  target: NameDialog;
  current: string;
  onCancel: () => void;
  onSaved: (sel: Selection | null) => void;
}) {
  const [name, setName] = useState(current);
  const [busy, setBusy] = useState(false);
  const trimmed = name.trim();
  const ready = trimmed !== "" && trimmed.length <= GROUP_NAME_MAX && trimmed !== current;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!ready || busy) return;
    setBusy(true);
    try {
      if (target.mode === "new") {
        const { conn, repo } = target;
        const out = await api.post<{ node: ReviewNode }>("/api/review-settings", {
          kind: "group",
          connection_id: conn.id,
          name: trimmed,
        });
        if (!repo?.repo) {
          toast.success(`${trimmed} created`);
          onSaved({ node: out.node.id });
          return;
        }
        // Started from a repository's Move to…: the group was made to hold it, so it goes in now
        // rather than on a second trip through the same menu.
        try {
          await api.post(`/api/review-settings/${conn.id}/move?repo=${encodeURIComponent(repo.repo)}`, { parent: out.node.id });
          toast.success(`${trimmed} created, with ${repo.repo} in it`);
          onSaved({ node: repo.repo, conn: conn.id });
        } catch (err) {
          toast.error(`${trimmed} created, but ${repo.repo} did not move: ${errorMessage(err)}`);
          onSaved({ node: out.node.id });
        }
      } else {
        await api.put(`/api/review-settings/${target.group.id}`, { name: trimmed });
        toast.success("Group renamed");
        onSaved(null);
      }
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form onSubmit={submit} className="space-y-4">
      <DialogHeader>
        <DialogTitle>{target.mode === "new" ? "New group" : `Rename ${current}`}</DialogTitle>
        <DialogDescription>
          {target.mode === "new"
            ? target.repo?.repo
              ? `Repositories of ${connectionName(target.conn)} that share settings — Frontend, Backend. ${target.repo.repo} moves into it, and inherits the group before the connection.`
              : `Repositories of ${connectionName(target.conn)} that share settings — Frontend, Backend. Move one in from its row menu; it then inherits the group before the connection.`
            : "Only the name changes; its settings and repositories stay."}
        </DialogDescription>
      </DialogHeader>
      <div className="space-y-1.5">
        <Label htmlFor="review-group-name">Name</Label>
        <Input
          id="review-group-name"
          autoFocus
          autoComplete="off"
          value={name}
          maxLength={GROUP_NAME_MAX}
          onChange={(e) => setName(e.target.value)}
          placeholder="Frontend"
        />
      </div>
      <DialogFooter>
        <Button type="button" variant="ghost" onClick={onCancel}>
          Cancel
        </Button>
        <Button type="submit" disabled={!ready} loading={busy}>
          {target.mode === "new" ? "Create group" : "Rename"}
        </Button>
      </DialogFooter>
    </form>
  );
}
