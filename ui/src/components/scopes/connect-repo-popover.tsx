"use client";

import { useEffect, useMemo, useState } from "react";
import { GitBranch, KeyRound, Loader2, Lock, PackagePlus } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Popover,
  PopoverAnchor,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { groupRepos, useInstallations } from "@/components/scopes/repo-sources";
import { api, errorMessage } from "@/lib/api";
import { cn } from "@/lib/utils";

type GithubRepo = { repo: string; private: boolean; pushed_at?: string };
/** A saved repository connection: its sealed token can open more repositories, and on a scope
 * the repository itself can be added as it is. The two grouping facts ride along so the list
 * can be offered by credential — a whole installation at once, not one repository at a time. */
export type TokenSource = {
  connection_id: number;
  repo: string;
  name: string;
  installation_id?: number;
  fp?: string;
};
type ConnectResult = {
  repos: string[];
  failed: { repo: string; error: string }[];
};
/** A GitHub App installation: an account whose admin ticked which repositories we may see. */
type GithubInstall = {
  installation_id: number;
  account_login: string;
  repo_selection: string;
  status: string;
};
type InstallsResponse = {
  installations: GithubInstall[];
  install_configured: boolean;
  install_url?: string;
  setup_url?: string;
  // What is missing when install_configured is false, so the disabled entry can name it.
  install_missing?: string[];
};

// "Connect repo". Two ways in, and an account may use both at once: a GitHub App installation,
// whose account admin chose the repositories at GitHub and where nothing is pasted, or an access
// token. Repositories connected one way are untouched by the other, so an account that installs
// the app keeps the repositories it already connected with a token, and one the app cannot be
// installed on — a personal account, an Enterprise Server — still has the token path.
//
// Where nothing is set up yet, neither is assumed: the two are offered side by side and the person
// picks. Below that, the token path is unchanged: a token is chosen — the one already sealed on a
// saved repository, or a freshly pasted one — GitHub is asked what it can reach, and the
// repositories come back as a list to tick. A stored token is named, never sent to the browser: the
// console posts a connection id and the server opens the secret. On a scope, a saved repository can
// also be added as it is, with no GitHub round trip. Without a scope (the Bundles page)
// repositories are only saved under Repositories, attached nowhere, for a channel to add later.
// Typing a name stays available for the repository a token can act on but GitHub will not list. The
// server rechecks each pick before storing and files one connection per repository.
export function ConnectRepoPopover({
  scopeId,
  onConnected,
  busy,
  savedTokens = [],
  connectedRepos = [],
  open: openProp,
  onOpenChange,
  anchor,
  container,
}: {
  /** Where the repositories are added. Absent: saved under Repositories and attached nowhere. */
  scopeId?: number;
  onConnected: () => void;
  busy: boolean;
  savedTokens?: TokenSource[];
  /** Repositories already reachable in the scope, so adding one again is not offered. */
  connectedRepos?: string[];
  /** Controlled open state, for a caller that opens this from somewhere else (a menu item). */
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
  /** Rendered in place of the Connect repo button; the popover hangs off it. */
  anchor?: React.ReactNode;
  /** Portal target. A modal dialog blocks pointer events outside itself, so a popover opened
   * from inside one has to be portalled into the dialog rather than to the body. */
  container?: React.ComponentProps<typeof PopoverContent>["container"];
}) {
  const sources = useMemo(() => {
    const seen = new Set<number>();
    return savedTokens.filter(
      (s) => !seen.has(s.connection_id) && seen.add(s.connection_id),
    );
  }, [savedTokens]);
  const idSuffix = scopeId ?? "org";
  const toScope = scopeId !== undefined;

  const [openState, setOpenState] = useState(false);
  const open = openProp ?? openState;
  const setOpen = (v: boolean) => {
    setOpenState(v);
    onOpenChange?.(v);
  };
  // Installations are fetched when the popover opens rather than at mount: most of the time
  // nobody opens it, and the answer decides which of three ways in is offered first.
  const [installs, setInstalls] = useState<GithubInstall[] | null>(null);
  const [appConfigured, setAppConfigured] = useState(false);
  const [appMissing, setAppMissing] = useState<string[]>([]);
  useEffect(() => {
    if (!open || installs !== null) return;
    let live = true;
    api
      .get<InstallsResponse>("/api/github/installations")
      .then((res) => {
        if (!live) return;
        setInstalls(
          (res.installations ?? []).filter((i) => i.status !== "revoked"),
        );
        setAppConfigured(res.install_configured);
        setAppMissing(res.install_missing ?? []);
      })
      // A deployment with no app configured answers this fine; anything else failing should not
      // take the token path down with it, so an error just means "no installations".
      .catch(() => live && setInstalls([]));
    return () => {
      live = false;
    };
  }, [open, installs]);

  // The saved repositories and the installations both arrive after this mounts, so nothing about
  // them is fixed at mount: the mode and the chosen source are derived on every render from what
  // was chosen and what exists now. A choice that no longer exists falls back.
  //
  // "choose" is the first-time state — no installation, no saved token — and it is deliberately
  // not the token box: which of the two an account wants is a real decision, and defaulting to
  // the one that needs a secret pasted quietly makes it for them.
  type Mode = "install" | "saved" | "paste" | "choose";
  const [modeChoice, setModeChoice] = useState<Mode | null>(null);
  const installList = installs ?? [];
  const defaultMode: Mode =
    installList.length > 0
      ? "install"
      : sources.length > 0
        ? "saved"
        : appConfigured
          ? "choose"
          : "paste";
  // A mode that no longer has anything behind it falls back to the default rather than showing
  // an empty select.
  const chosen_ok =
    modeChoice === null ||
    (modeChoice === "install"
      ? installList.length > 0
      : modeChoice === "saved"
        ? sources.length > 0
        : true);
  const mode: Mode = chosen_ok ? (modeChoice ?? defaultMode) : defaultMode;

  const [installChoice, setInstallChoice] = useState(0);
  const installId = installList.some((i) => i.installation_id === installChoice)
    ? installChoice
    : (installList[0]?.installation_id ?? 0);
  const install = installList.find((i) => i.installation_id === installId);
  const [sourceChoice, setSourceChoice] = useState(0);
  const sourceId = sources.some((s) => s.connection_id === sourceChoice)
    ? sourceChoice
    : (sources[0]?.connection_id ?? 0);
  const source = sources.find((s) => s.connection_id === sourceId);
  const sourceLabel = source?.repo ?? "";
  const savedInstalls = useInstallations(sources.some((t) => (t.installation_id ?? 0) > 0));
  const savedGroups = groupRepos(
    sources,
    (t) => ({ repo: t.repo, installationID: t.installation_id, fp: t.fp }),
    savedInstalls,
  );

  const [token, setToken] = useState("");
  const [repos, setRepos] = useState<GithubRepo[] | null>(null);
  const [truncated, setTruncated] = useState(false);
  const [picked, setPicked] = useState<string[]>([]);
  const [manual, setManual] = useState("");
  const [byName, setByName] = useState(false);
  const [search, setSearch] = useState("");
  // On a scope, saved repositories are added several at a time — a whole credential's worth in
  // one tick. The Bundles page has nothing to add them to, so there it stays a single choice of
  // whose token to search with.
  const [savedPicked, setSavedPicked] = useState<number[]>([]);
  const [finding, setFinding] = useState(false);
  const [adding, setAdding] = useState(false);
  const [saving, setSaving] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);

  // What picking again would add nothing to: on a scope, what this channel already reaches; on
  // the Bundles page, what is already saved. Re-picking one of these re-keys the connection it
  // already has, which is not what a tick in a list of twenty looks like it does.
  const alreadyHave = new Set(
    (toScope ? connectedRepos : savedTokens.map((t) => t.repo)).map((r) => r.toLowerCase()),
  );
  const alreadyHere = (repo: string) => alreadyHave.has(repo.toLowerCase());
  const shownRepos = (repos ?? []).filter((r) =>
    r.repo.toLowerCase().includes(search.trim().toLowerCase()),
  );
  // Owner is the grouping a person reads: one installation is one account, and a token that
  // reaches three organisations becomes three headings rather than one list of sixty.
  const owners: [string, GithubRepo[]][] = [
    ...shownRepos
      .reduce((m, r) => {
        const owner = r.repo.slice(0, r.repo.indexOf("/")) || r.repo;
        m.set(owner, [...(m.get(owner) ?? []), r]);
        return m;
      }, new Map<string, GithubRepo[]>())
      .entries(),
  ].sort((a, b) => a[0].localeCompare(b[0]));
  const pickable = shownRepos.filter((r) => !alreadyHere(r.repo)).map((r) => r.repo);

  // Which credential the calls should use: an installation, a stored token named by id, or the
  // pasted text. A stored token is never sent to the browser — the server opens it from the id.
  const searchID = toScope && savedPicked.length > 0 ? savedPicked[0] : sourceId;
  const auth = () =>
    mode === "install"
      ? { installation_id: installId }
      : mode === "saved"
        ? { connection_id: searchID }
        : { token: token.trim() };
  const authReady =
    mode === "install"
      ? installId > 0
      : mode === "saved"
        ? searchID > 0
        : token.trim() !== "";
  const connectPath = toScope ? `/api/scopes/${scopeId}/repos` : "/api/repos";

  const reset = () => {
    setModeChoice(null);
    setSourceChoice(0);
    setInstallChoice(0);
    setToken("");
    setRepos(null);
    setTruncated(false);
    setPicked([]);
    setSavedPicked([]);
    setSearch("");
    setManual("");
    setByName(false);
    setFailure(null);
  };
  const close = () => {
    setOpen(false);
    reset();
  };

  const report = (res: ConnectResult, verb: string) => {
    toast.success(
      res.repos.length === 1
        ? `${verb} ${res.repos[0]}`
        : `${verb} ${res.repos.length} repositories`,
    );
    for (const f of res.failed ?? []) toast.error(`${f.repo}: ${f.error}`);
    close();
    onConnected();
  };

  // Ask GitHub what this token can see. A token that lists exactly one repository means it was
  // scoped to that repository, so it is picked already and Connect is one more click.
  const find = async () => {
    if (!authReady || finding) return;
    setFinding(true);
    setFailure(null);
    try {
      const res = await api.post<{ repos: GithubRepo[]; truncated: boolean }>(
        "/api/github/repos",
        auth(),
      );
      setRepos(res.repos ?? []);
      setTruncated(res.truncated);
      setPicked(
        res.repos?.length === 1 && !alreadyHere(res.repos[0].repo) ? [res.repos[0].repo] : [],
      );
      if ((res.repos ?? []).length === 0) {
        setFailure("This token does not open any repository GitHub will list.");
        setByName(true);
      }
    } catch (err) {
      // Staying put with the reason on screen: dropping straight into the by-name form hides
      // why the list never came, which is exactly when the reason matters most.
      setFailure(errorMessage(err));
    } finally {
      setFinding(false);
    }
  };

  // Add saved repositories to this scope: their connections are attached as they are, in one
  // request however many were ticked. Nothing is asked of GitHub — these are already proved.
  const addSaved = async () => {
    if (!toScope || savedPicked.length === 0 || adding) return;
    setAdding(true);
    try {
      const res = await api.post<ConnectResult>(connectPath, { connection_ids: savedPicked });
      report(res, "Added");
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setAdding(false);
    }
  };

  const chosen = byName
    ? manual.trim() === ""
      ? []
      : [manual.trim()]
    : picked;

  const connect = async () => {
    if (chosen.length === 0 || !authReady || saving) return;
    setSaving(true);
    try {
      const res = await api.post<ConnectResult>(connectPath, {
        repos: chosen,
        ...auth(),
      });
      report(res, toScope ? "Connected" : "Saved");
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setSaving(false);
    }
  };

  const toggle = (repo: string) =>
    setPicked((cur) =>
      cur.includes(repo) ? cur.filter((r) => r !== repo) : [...cur, repo],
    );

  // The ways in other than the one on screen, in the order they are worth offering.
  const alternatives: { mode: Mode; label: string }[] = [
    {
      mode: "install" as Mode,
      label: "Use a GitHub App account",
      show: installList.length > 0,
    },
    {
      mode: "saved" as Mode,
      label: "Use a saved repository",
      show: sources.length > 0,
    },
    { mode: "paste" as Mode, label: "Paste an access token", show: true },
  ]
    .filter((a) => a.show && a.mode !== mode)
    .map(({ mode: m, label }) => ({ mode: m, label }));

  const failureNote = failure && (
    <p className="rounded-lg border border-danger/30 bg-danger-soft px-3 py-2 text-xs text-danger">
      {failure}
    </p>
  );

  const savedTail = toScope
    ? "It is stored sealed and never shown again."
    : "Each is saved under Repositories, attached nowhere; a channel adds it from its own page.";
  const intro =
    mode === "choose"
      ? "Two ways in, and you can use both — repositories connected one way are untouched by the other."
      : mode === "install"
        ? `Pick from the repositories this account's admin chose at GitHub. Nothing is pasted. ${savedTail}`
        : mode === "saved"
          ? toScope
            ? "Add repositories already saved under Repositories — one, or a whole credential at a time. Or use one's token to reach more."
            : "Uses a token already sealed here, so there is nothing to paste. Pick from what it reaches."
          : `Paste a token and pick from what it can reach. ${savedTail}`;

  return (
    <Popover open={open} onOpenChange={(v) => (v ? setOpen(true) : close())}>
      {anchor ? (
        <PopoverAnchor asChild>{anchor}</PopoverAnchor>
      ) : (
        <PopoverTrigger asChild>
          <Button variant="outline" size="sm" disabled={busy}>
            <GitBranch className="size-4" />
            Connect repo
          </Button>
        </PopoverTrigger>
      )}
      <PopoverContent className="w-96 p-0" align="end" container={container}>
        {repos === null && !byName ? (
          <form
            className="space-y-3 p-4"
            onSubmit={(e) => {
              e.preventDefault();
              if (mode === "choose") return; // nothing chosen yet: the two buttons above are the choice
              if (mode === "saved" && toScope) addSaved();
              else find();
            }}
          >
            <div className="space-y-1">
              <p className="text-sm font-medium">
                {toScope
                  ? "Connect a GitHub repository"
                  : "Save a GitHub repository"}
              </p>
              <p className="text-xs text-muted-foreground">{intro}</p>
            </div>
            {mode === "choose" ? (
              // Nothing set up yet. Both routes, side by side, with the app first because it is
              // the one that needs no secret — but neither is chosen for them.
              <div className="space-y-2">
                <Button
                  asChild
                  variant="outline"
                  className="h-auto w-full justify-start gap-3 px-3 py-2.5 whitespace-normal"
                >
                  {/* A real navigation, not a fetch: the browser leaves for GitHub's own
                      account and repository picker, and comes back to /github/setup. */}
                  <a href="/github/install">
                    <PackagePlus className="size-4 shrink-0" />
                    <span className="min-w-0 flex-1 text-left">
                      <span className="block text-sm font-medium">
                        Install the GitHub App
                      </span>
                      <span className="mt-0.5 block text-xs leading-snug font-normal text-muted-foreground">
                        You pick the repositories at GitHub. Nothing to paste,
                        and access can be withdrawn there.
                      </span>
                    </span>
                  </a>
                </Button>
                <Button
                  type="button"
                  variant="outline"
                  className="h-auto w-full justify-start gap-3 px-3 py-2.5 whitespace-normal"
                  onClick={() => setModeChoice("paste")}
                >
                  <KeyRound className="size-4 shrink-0" />
                  <span className="min-w-0 flex-1 text-left">
                    <span className="block text-sm font-medium">
                      Paste an access token
                    </span>
                    <span className="mt-0.5 block text-xs leading-snug font-normal text-muted-foreground">
                      For a personal account or GitHub Enterprise Server, where
                      the app cannot be installed.
                    </span>
                  </span>
                </Button>
              </div>
            ) : mode === "install" ? (
              <div className="space-y-1.5">
                <Label htmlFor={`repo-inst-${idSuffix}`}>GitHub account</Label>
                <Select
                  value={String(installId)}
                  onValueChange={(v) => setInstallChoice(Number(v))}
                >
                  <SelectTrigger
                    id={`repo-inst-${idSuffix}`}
                    className="w-full"
                  >
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {installList.map((i) => (
                      <SelectItem
                        key={i.installation_id}
                        value={String(i.installation_id)}
                      >
                        {i.account_login}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                <p className="text-xs text-muted-foreground">
                  {install?.repo_selection === "all"
                    ? "The app can see every repository in this account."
                    : "The app can see the repositories chosen for it at GitHub."}{" "}
                  <a
                    href="/github/install"
                    className="text-primary hover:underline"
                  >
                    Add another account
                  </a>
                </p>
              </div>
            ) : mode === "saved" && toScope ? (
              // Saved repositories, grouped by the credential they came through, so a channel
              // can be given a whole installation in one tick. Before this the choice was one
              // repository from a dropdown or the whole Repositories bundle — one at a time or
              // everything, with nothing in between.
              <div className="space-y-1.5">
                <div className="flex items-baseline justify-between gap-2">
                  <Label>Saved repositories</Label>
                  <span className="text-xs tabular-nums text-muted-foreground">
                    {savedPicked.length} selected
                  </span>
                </div>
                <div className="max-h-56 space-y-2 overflow-y-auto rounded-md border p-1.5">
                  {savedGroups.map((group) => {
                    const ids = group.rows
                      .filter((t) => !connectedRepos.some((r) => r.toLowerCase() === t.repo.toLowerCase()))
                      .map((t) => t.connection_id);
                    const on = ids.filter((id) => savedPicked.includes(id)).length;
                    return (
                      <div key={group.key}>
                        <div className="flex items-center gap-2 rounded-sm bg-muted/50 px-1.5 py-1">
                          <Checkbox
                            checked={on === 0 ? false : on === ids.length ? true : "indeterminate"}
                            disabled={ids.length === 0}
                            onCheckedChange={(v) =>
                              setSavedPicked((cur) =>
                                v === true
                                  ? [...new Set([...cur, ...ids])]
                                  : cur.filter((id) => !ids.includes(id)),
                              )
                            }
                            aria-label={`Add every repository from ${group.title}`}
                          />
                          <span className="min-w-0 flex-1 truncate text-xs font-semibold">
                            {group.title}
                          </span>
                          <span className="shrink-0 text-xs text-muted-foreground">
                            {ids.length === 0 ? "all here" : `${ids.length} to add`}
                          </span>
                        </div>
                        {group.rows.map((t) => {
                          const here = connectedRepos.some(
                            (r) => r.toLowerCase() === t.repo.toLowerCase(),
                          );
                          return (
                            <label
                              key={t.connection_id}
                              className={cn(
                                "flex items-center gap-2 rounded-sm px-1.5 py-1 text-xs",
                                here ? "opacity-50" : "cursor-pointer hover:bg-accent",
                              )}
                            >
                              <Checkbox
                                checked={!here && savedPicked.includes(t.connection_id)}
                                disabled={here}
                                onCheckedChange={() =>
                                  setSavedPicked((cur) =>
                                    cur.includes(t.connection_id)
                                      ? cur.filter((id) => id !== t.connection_id)
                                      : [...cur, t.connection_id],
                                  )
                                }
                              />
                              <span className="min-w-0 flex-1 truncate">
                                <span className="text-muted-foreground">
                                  {t.repo.slice(0, t.repo.indexOf("/") + 1)}
                                </span>
                                {t.repo.slice(t.repo.indexOf("/") + 1)}
                              </span>
                              {here && <span className="text-muted-foreground">here</span>}
                            </label>
                          );
                        })}
                      </div>
                    );
                  })}
                </div>
                <p className="text-xs text-muted-foreground">
                  Attached here as they are; nothing is asked of GitHub.
                </p>
              </div>
            ) : mode === "saved" ? (
              <div className="space-y-1.5">
                <Label htmlFor={`repo-src-${idSuffix}`}>Saved repository</Label>
                <Select
                  value={String(sourceId)}
                  onValueChange={(v) => setSourceChoice(Number(v))}
                >
                  <SelectTrigger id={`repo-src-${idSuffix}`} className="w-full">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {sources.map((s) => (
                      <SelectItem
                        key={s.connection_id}
                        value={String(s.connection_id)}
                      >
                        {s.repo}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                <p className="text-xs text-muted-foreground">
                  Its sealed token is used to list what else it can reach.
                </p>
              </div>
            ) : (
              <div className="space-y-1.5">
                <Label htmlFor={`repo-key-${idSuffix}`}>Access token</Label>
                <Input
                  id={`repo-key-${idSuffix}`}
                  type="password"
                  value={token}
                  onChange={(e) => setToken(e.target.value)}
                  placeholder="github_pat_…"
                  autoComplete="off"
                  autoFocus
                />
                <p className="text-xs text-muted-foreground">
                  A fine-grained personal access token with read access to the
                  repository; add issues and pull-request write if the bot
                  should comment.
                </p>
              </div>
            )}
            {failureNote}
            {mode !== "choose" && (
              <div className="flex items-center justify-between gap-2">
                <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                  {/* Every other way in that this account actually has, so switching between an
                    installation and a token is one click either way and neither is a dead end. */}
                  {/* Offered even with no installation yet: without it, an account that already
                    has a saved token would never see the app route from here at all. A real
                    navigation, since the choosing happens at GitHub. Where the deployment has no
                    app set up it stays on the list but disabled, saying which variables are
                    missing: hidden, it is indistinguishable from a console that forgot it, and
                    the person who can fix that is often the one reading this. */}
                  {installList.length === 0 &&
                    (appConfigured ? (
                      <Button
                        asChild
                        variant="link"
                        size="sm"
                        className="h-auto p-0 text-xs"
                      >
                        <a href="/github/install">Install the GitHub App</a>
                      </Button>
                    ) : (
                      <Tooltip>
                        <TooltipTrigger asChild>
                          <span className="cursor-not-allowed text-xs text-muted-foreground underline decoration-dotted underline-offset-4">
                            Install the GitHub App
                          </span>
                        </TooltipTrigger>
                        <TooltipContent className="max-w-xs break-words">
                          {appMissing.length > 0 ? (
                            <>
                              Not set up on this deployment. Set{" "}
                              {appMissing.map((v, i) => (
                                <span key={v}>
                                  {i > 0 &&
                                    (i === appMissing.length - 1
                                      ? " and "
                                      : ", ")}
                                  <span className="font-mono">{v}</span>
                                </span>
                              ))}{" "}
                              in the environment and restart. Until then,
                              connect a repository with an access token.
                            </>
                          ) : (
                            <>
                              No GitHub App is set up on this deployment.
                              Connect a repository with an access token instead.
                            </>
                          )}
                        </TooltipContent>
                      </Tooltip>
                    ))}
                  {alternatives.map((alt) => (
                    <Button
                      key={alt.mode}
                      type="button"
                      variant="link"
                      size="sm"
                      className="h-auto p-0 text-xs"
                      onClick={() => {
                        setFailure(null);
                        setModeChoice(alt.mode);
                        if (alt.mode !== "paste") setToken("");
                      }}
                    >
                      {alt.label}
                    </Button>
                  ))}
                  <Button
                    type="button"
                    variant="link"
                    size="sm"
                    className="h-auto p-0 text-xs"
                    onClick={() => {
                      setFailure(null);
                      setByName(true);
                    }}
                  >
                    Enter a repository by name
                  </Button>
                  {mode === "saved" && toScope && (
                    <Button
                      type="button"
                      variant="link"
                      size="sm"
                      className="h-auto p-0 text-xs"
                      disabled={!authReady || finding}
                      onClick={find}
                    >
                      {finding && <Loader2 className="animate-spin" />}
                      Find more with its token
                    </Button>
                  )}
                </div>
                {mode === "saved" && toScope ? (
                  <Button type="submit" size="sm" disabled={savedPicked.length === 0 || adding}>
                    {adding && <Loader2 className="animate-spin" />}
                    {savedPicked.length > 1 ? `Add ${savedPicked.length} here` : "Add here"}
                  </Button>
                ) : (
                  <Button
                    type="submit"
                    size="sm"
                    disabled={!authReady || finding}
                  >
                    {finding && <Loader2 className="animate-spin" />}
                    Find repositories
                  </Button>
                )}
              </div>
            )}
          </form>
        ) : byName ? (
          <form
            className="space-y-3 p-4"
            onSubmit={(e) => {
              e.preventDefault();
              connect();
            }}
          >
            <div className="space-y-1">
              <p className="text-sm font-medium">Enter a repository</p>
              <p className="text-xs text-muted-foreground">
                For a repository the token can act on but GitHub does not list.
                It is checked against GitHub before anything is stored.
              </p>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor={`repo-${idSuffix}`}>Repository</Label>
              <Input
                id={`repo-${idSuffix}`}
                value={manual}
                onChange={(e) => setManual(e.target.value)}
                placeholder="owner/name or https://github.com/owner/name"
                autoComplete="off"
                autoFocus
              />
              {mode === "saved" && sourceLabel !== "" && (
                <p className="text-xs text-muted-foreground">
                  Opened with the token stored for {sourceLabel}.
                </p>
              )}
            </div>
            {mode === "paste" && token.trim() === "" && (
              <div className="space-y-1.5">
                <Label htmlFor={`repo-key-manual-${idSuffix}`}>
                  Access token
                </Label>
                <Input
                  id={`repo-key-manual-${idSuffix}`}
                  type="password"
                  value={token}
                  onChange={(e) => setToken(e.target.value)}
                  placeholder="github_pat_…"
                  autoComplete="off"
                />
              </div>
            )}
            {failureNote}
            <div className="flex items-center justify-between gap-2">
              <Button
                type="button"
                variant="link"
                size="sm"
                className="h-auto p-0 text-xs"
                onClick={() => {
                  setFailure(null);
                  setByName(false);
                  setManual("");
                  if (repos !== null && repos.length === 0) setRepos(null);
                }}
              >
                Back
              </Button>
              <Button
                type="submit"
                size="sm"
                disabled={manual.trim() === "" || !authReady || saving}
              >
                {saving && <Loader2 className="animate-spin" />}
                {toScope ? "Connect" : "Save"}
              </Button>
            </div>
          </form>
        ) : repos !== null ? (
          <div>
            <div className="space-y-1 p-3 pb-2">
              <div className="flex items-baseline gap-2">
                <p className="min-w-0 flex-1 text-sm font-medium">
                  {repos.length === 1
                    ? "This token opens one repository"
                    : `This token opens ${repos.length}${truncated ? "+" : ""} repositories`}
                </p>
                {/* What is ticked so far, held in a slot that is always there: a counter that
                    appears with the first tick moves the sentence beside it. */}
                {repos.length > 1 && (
                  <span className="flex shrink-0 items-center gap-2">
                    <span className="text-xs tabular-nums text-muted-foreground">
                      {picked.length} selected
                    </span>
                    <Button
                      type="button"
                      variant="link"
                      size="sm"
                      className="h-auto p-0 text-xs"
                      disabled={pickable.length === 0}
                      onClick={() =>
                        setPicked((cur) =>
                          pickable.every((r) => cur.includes(r))
                            ? cur.filter((r) => !pickable.includes(r))
                            : [...new Set([...cur, ...pickable])],
                        )
                      }
                    >
                      {pickable.length > 0 && pickable.every((r) => picked.includes(r))
                        ? "Select none"
                        : `Select all ${pickable.length}`}
                    </Button>
                  </span>
                )}
              </div>
              <p className="text-xs text-muted-foreground">
                {repos.length === 1
                  ? toScope
                    ? "Connect it here, or go back for a token that reaches more."
                    : "Save it, or go back for a token that reaches more."
                  : toScope
                    ? "Pick what this channel should reach. Each becomes its own connection."
                    : "Pick what to save. Each becomes its own connection under Repositories."}
              </p>
            </div>
            {/* cmdk filters for itself by default, which would leave "select all" ticking rows
                nobody can see. The search is ours instead, so an owner header takes exactly the
                rows under it that are showing. */}
            <Command shouldFilter={false}>
              {repos.length > 6 && (
                <CommandInput
                  placeholder="Filter repositories…"
                  value={search}
                  onValueChange={setSearch}
                />
              )}
              <CommandList>
                {shownRepos.length === 0 && <CommandEmpty>Nothing matches.</CommandEmpty>}
                {owners.map(([owner, rows]) => {
                  const ids = rows.filter((r) => !alreadyHere(r.repo)).map((r) => r.repo);
                  const on = ids.filter((r) => picked.includes(r)).length;
                  return (
                    <CommandGroup key={owner}>
                      {/* One tick for the whole account: twenty-one repositories behind one
                          installation used to be twenty-one clicks. */}
                      <div className="flex items-center gap-2 px-2 py-1.5">
                        <Checkbox
                          checked={on === 0 ? false : on === ids.length ? true : "indeterminate"}
                          disabled={ids.length === 0}
                          onCheckedChange={(v) =>
                            setPicked((cur) =>
                              v === true
                                ? [...new Set([...cur, ...ids])]
                                : cur.filter((r) => !ids.includes(r)),
                            )
                          }
                          aria-label={`Select every repository from ${owner}`}
                        />
                        <span className="text-xs font-semibold">{owner}</span>
                        <span className="text-xs text-muted-foreground">
                          {ids.length === 0
                            ? "all saved already"
                            : `${ids.length} to ${toScope ? "add" : "save"}`}
                          {rows.length - ids.length > 0 &&
                            ids.length > 0 &&
                            ` · ${rows.length - ids.length} already ${toScope ? "here" : "saved"}`}
                        </span>
                      </div>
                      {rows.map((r) => {
                        const have = alreadyHere(r.repo);
                        return (
                          <CommandItem
                            key={r.repo}
                            value={r.repo}
                            disabled={have}
                            onSelect={() => !have && toggle(r.repo)}
                          >
                            <Checkbox
                              checked={!have && picked.includes(r.repo)}
                              disabled={have}
                              className="pointer-events-none"
                            />
                            <span className="flex-1 truncate">
                              <span className="text-muted-foreground">
                                {r.repo.slice(0, r.repo.indexOf("/") + 1)}
                              </span>
                              {r.repo.slice(r.repo.indexOf("/") + 1)}
                            </span>
                            {r.private && <Lock className="size-3 text-muted-foreground" />}
                            {/* Re-picking one of these would quietly re-key the connection it
                                already has; saying so is cheaper than undoing it. */}
                            {have && (
                              <span className="text-xs text-muted-foreground">
                                {toScope ? "here" : "saved"}
                              </span>
                            )}
                          </CommandItem>
                        );
                      })}
                    </CommandGroup>
                  );
                })}
              </CommandList>
            </Command>
            <div className="flex items-center justify-between gap-2 border-t p-3">
              <Button
                type="button"
                variant="link"
                size="sm"
                className="h-auto p-0 text-xs"
                onClick={() => (truncated ? setByName(true) : reset())}
              >
                {truncated
                  ? "Not listed? Enter it by name"
                  : "Use a different token"}
              </Button>
              <Button
                type="button"
                size="sm"
                onClick={connect}
                disabled={picked.length === 0 || saving}
              >
                {saving && <Loader2 className="animate-spin" />}
                {picked.length > 1
                  ? `${toScope ? "Connect" : "Save"} ${picked.length}`
                  : toScope
                    ? "Connect"
                    : "Save"}
              </Button>
            </div>
          </div>
        ) : null}
      </PopoverContent>
    </Popover>
  );
}
