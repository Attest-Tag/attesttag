"use client";

import { useEffect, useRef, useState } from "react";
import {
  Cable,
  ChevronDown,
  GitBranch,
  KeyRound,
  Package,
  Plus,
} from "lucide-react";
import { toast } from "sonner";
import {
  BundleCard,
  type BundleAction,
} from "@/components/bundles/bundle-card";
import { BundleEditDialog } from "@/components/bundles/bundle-edit-dialog";
import { defaultBundleName } from "@/components/bundles/bundle-picker";
import {
  ConnectDialog,
  type ConnectTarget,
} from "@/components/bundles/connect-dialog";
import type { ConnectionAction } from "@/components/bundles/connection-table";
import { CurlDialog } from "@/components/bundles/curl-dialog";
import { CopyToBundleDialog } from "@/components/bundles/copy-dialog";
import { NameDialog } from "@/components/bundles/name-dialog";
import { NewConnectionDialog } from "@/components/bundles/new-connection-dialog";
import { AttachReposDialog } from "@/components/bundles/attach-repos-dialog";
import { AttachScopesDialog } from "@/components/bundles/attach-scopes-dialog";
import { RepoManagerDialog } from "@/components/bundles/repo-manager-dialog";
import { RepoRecipeDialog } from "@/components/bundles/repo-recipe-dialog";
import { RepoWritesDialog } from "@/components/bundles/repo-writes-dialog";
import { RepoGroupList, type RepoBulkAction } from "@/components/bundles/repo-group-list";
import { countRepoSources, isRepoBundle, useInstallations } from "@/components/scopes/repo-sources";
import { CONNECTED_PARAM, startOAuthSignIn } from "@/components/bundles/oauth";
import { RotateSecretDialog } from "@/components/bundles/rotate-secret-dialog";
import { useConfirm } from "@/components/core/confirm-dialog";
import { useAuth } from "@/components/shell/auth-provider";
import { EmptyState } from "@/components/core/empty-state";
import { ErrorBanner } from "@/components/core/error-banner";
import { PageHeader } from "@/components/core/page-header";
import {
  ConnectRepoPopover,
  type TokenSource,
} from "@/components/scopes/connect-repo-popover";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Skeleton } from "@/components/ui/skeleton";
import { useStickyFlags } from "@/hooks/use-sticky";
import {
  api,
  errorMessage,
  useApi,
  type Bundle,
  type Connection,
  type Preset,
} from "@/lib/api";

export function BundlesPage() {
  // Which bundles are open, kept in this browser: a page of cards you have to reopen every visit
  // is a page you stop scrolling to the bottom of.
  const cardOpen = useStickyFlags("bundles:open");
  const bundles = useApi<Bundle[]>("/api/bundles");
  const presets = useApi<Preset[]>("/api/presets");
  const { confirm, confirmDialog } = useConfirm();
  // Granting a bundle at a channel is scopes.manage, which somebody who may edit bundles need
  // not hold — the permissions map carries only what was granted, so an absent key is a no. It
  // shows while /api/me has not answered, the way the rail treats a gated entry, rather than
  // appearing a moment after the page does.
  const { me } = useAuth();
  const perms = me?.user?.permissions;
  const canAttach = !perms || perms["scopes.manage"] === true;

  const [creating, setCreating] = useState(false);
  const [renaming, setRenaming] = useState<Bundle | null>(null);
  const [managingId, setManagingId] = useState<number | null>(null);
  // Configure on the repository bundle opens the repository manager instead of the five-tab
  // editor; the editor is still reachable from a link in its footer.
  const [repoManagingId, setRepoManagingId] = useState<number | null>(null);
  const [addRepoOpen, setAddRepoOpen] = useState(false);
  // The bundle whose scopes are being chosen, by id so the dialog sees each reload.
  const [attachingId, setAttachingId] = useState<number | null>(null);
  const [connect, setConnect] = useState<ConnectTarget | null>(null);
  const [rotating, setRotating] = useState<Connection | null>(null);
  const [curlFor, setCurlFor] = useState<Connection | null>(null);
  const [copying, setCopying] = useState<Connection | null>(null);
  const [repoOpen, setRepoOpen] = useState(false);
  // The two bulk dialogs the grouped repository list opens; null while closed.
  const [attachRepos, setAttachRepos] = useState<Connection[] | null>(null);
  const [writesRepos, setWritesRepos] = useState<Connection[] | null>(null);
  const [recipeRepos, setRecipeRepos] = useState<Connection[] | null>(null);
  // Radix hands focus back to the Create button as the menu unmounts, and focus landing
  // outside the popover is what dismisses it — so the picker is opened from the menu's own
  // close handler with that focus restore suppressed. Opening it on a timer instead let the
  // returning focus close it again in the same breath, which read as the item doing nothing.
  const openRepoOnClose = useRef(false);
  const [newConnection, setNewConnection] = useState(false);

  // The OAuth callback lands on ?connected=<id>; acknowledge it once and
  // drop it from the URL so a reload doesn't repeat the toast.
  useEffect(() => {
    const url = new URL(window.location.href);
    if (!url.searchParams.has(CONNECTED_PARAM)) return;
    url.searchParams.delete(CONNECTED_PARAM);
    window.history.replaceState(null, "", url);
    toast.success("Signed in");
  }, []);

  // The GitHub App install lands back here on ?github_install=<id>&connected_repos=<n>, and its
  // failures land on ?github_error / ?github_notice. Acknowledged once and dropped from the URL,
  // the same way the OAuth callback above is, so a reload does not repeat any of it.
  useEffect(() => {
    const url = new URL(window.location.href);
    const installed = url.searchParams.get("github_install");
    const url_connected = url.searchParams.get("connected_repos") ?? "0";
    const err = url.searchParams.get("github_error");
    const notice = url.searchParams.get("github_notice");
    if (installed === null && err === null && notice === null) return;
    for (const p of [
      "github_install",
      "connected_repos",
      "github_error",
      "github_notice",
    ]) {
      url.searchParams.delete(p);
    }
    window.history.replaceState(null, "", url);
    if (err) toast.error(err);
    else if (notice) toast.info(notice);
    else {
      const n = Number(url_connected);
      toast.success(
        n > 0
          ? `GitHub App installed — ${n === 1 ? "1 repository" : `${n} repositories`} saved under Repositories`
          : "GitHub App installed. Add its repositories from Create → Repository.",
      );
      bundles.reload();
    }
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  const list = bundles.data ?? [];
  // Repositories first, then the rest by name. It is the bundle that grows on its own — every
  // repository connected from a channel lands in it — so it is both the longest card and the
  // one people come back to, and alphabetical order buried it wherever the alphabet decided.
  const ordered = [...list].sort(
    (a, b) => Number(isRepoBundle(b)) - Number(isRepoBundle(a)) || a.name.localeCompare(b.name),
  );
  const presetList = presets.data ?? [];
  // Only asked for where a repository actually names an installation: a console with no GitHub
  // App in it should not call an endpoint to be told there is none.
  const installs = useInstallations(
    list.some((b) => (b.connections ?? []).some((c) => c.github_installation_id > 0)),
  );
  // Looked up by id so the editor sees fresh data after every reload.
  const managing = list.find((b) => b.id === managingId) ?? null;
  const attaching = list.find((b) => b.id === attachingId) ?? null;
  const repoManaging = list.find((b) => b.id === repoManagingId) ?? null;
  const repoRows = repoManaging?.connections ?? [];
  const bundleOf = (c: Connection) => list.find((b) => b.id === c.bundle_id);
  const presetOf = (c: Connection): Preset =>
    presetList.find((p) => p.id === c.preset) ??
    presetList.find((p) => p.id === "custom") ?? {
      id: "custom",
      name: "Custom HTTP API",
      category: "Custom",
      cred_type: "custom",
      hosts: [],
      secret_label: "Secret",
      secret_hint: "",
      placeholder: "",
      docs_url: "",
      test: { method: "GET", path: "/" },
      notes: "",
      has_pack: false,
    };

  // A fresh bundle is an empty shell, so creating one lands straight in its editor — the
  // next thing to do is always to put something in it. The list is seeded with the bundle
  // the POST hands back rather than waiting for the refetch: the editor looks its subject
  // up in the list, so without this the dialog would stay shut for the length of a round
  // trip after the name dialog closed, and read as the Create button doing nothing.
  const create = async (name: string) => {
    try {
      const created = await api.post<Bundle>("/api/bundles", { name });
      toast.success("Bundle created");
      bundles.mutate((current) =>
        [...(current ?? []), created].sort((a, b) =>
          a.name.localeCompare(b.name),
        ),
      );
      bundles.reload();
      setManagingId(created.id);
    } catch (err) {
      toast.error(errorMessage(err));
      throw err;
    }
  };

  const rename = async (name: string) => {
    if (!renaming) return;
    try {
      await api.put(`/api/bundles/${renaming.id}`, { name });
      toast.success("Bundle renamed");
      bundles.reload();
    } catch (err) {
      toast.error(errorMessage(err));
      throw err;
    }
  };

  const onBundleAction = async (action: BundleAction, bundle: Bundle) => {
    if (action === "rename") setRenaming(bundle);
    if (action === "manage") {
      if (isRepoBundle(bundle)) setRepoManagingId(bundle.id);
      else setManagingId(bundle.id);
    }
    if (action === "tabs") setManagingId(bundle.id);
    if (action === "attach") setAttachingId(bundle.id);
    if (action === "delete") {
      const creds = bundle.connections?.length ?? 0;
      const ok = await confirm({
        title: `Delete ${bundle.name}?`,
        description: `${creds} credential${creds === 1 ? "" : "s"} and ${bundle.domains?.length ?? 0} domain${(bundle.domains?.length ?? 0) === 1 ? "" : "s"} go with it, and ${bundle.used_in === 0 ? "no scope loses access" : `${bundle.used_in} scope${bundle.used_in === 1 ? "" : "s"} lose${bundle.used_in === 1 ? "s" : ""} that access`}. This can't be undone.`,
        confirmLabel: "Delete",
        destructive: true,
      });
      if (!ok) return;
      try {
        await api.del(`/api/bundles/${bundle.id}`);
        toast.success("Bundle deleted");
        bundles.reload();
      } catch (err) {
        toast.error(errorMessage(err));
      }
    }
  };

  const onConnectionAction = async (
    action: ConnectionAction,
    c: Connection,
  ) => {
    const bundle = bundleOf(c);
    if (!bundle) return;
    if (action === "edit")
      setConnect({ preset: presetOf(c), bundle, connection: c });
    if (action === "rotate") setRotating(c);
    if (action === "curl") setCurlFor(c);
    if (action === "copy") setCopying(c);
    if (action === "oauth") {
      const id = toast.loading(`Starting sign-in for ${c.name}…`);
      const left = await startOAuthSignIn(c.id);
      if (left) toast.loading("Redirecting…", { id });
      else toast.dismiss(id);
    }
    if (action === "delete") {
      const ok = await confirm({
        title: `Delete ${c.name}?`,
        description:
          "The credential is destroyed and the bot loses this access everywhere it is granted, through the bundle or on its own. This can't be undone.",
        confirmLabel: "Delete",
        destructive: true,
      });
      if (!ok) return;
      try {
        await api.del(`/api/connections/${c.id}`);
        toast.success("Connection deleted");
        bundles.reload();
      } catch (err) {
        toast.error(errorMessage(err));
      }
    }
  };

  // Several repositories at once. Attaching and setting writes each have their own dialog;
  // removing asks here, because what it destroys is the same thing the single-row Delete
  // destroys and the sentence explaining that is worth keeping in one place.
  const onRepoBulk = async (action: RepoBulkAction, chosen: Connection[]) => {
    // Repositories the manager found at GitHub and saved for us: nothing to ask, just refresh.
    if (action === "added") {
      bundles.reload();
      return;
    }
    if (chosen.length === 0) return;
    if (action === "attach") setAttachRepos(chosen);
    if (action === "writes") setWritesRepos(chosen);
    if (action === "recipe") setRecipeRepos(chosen);
    if (action === "delete") {
      const ok = await confirm({
        title:
          chosen.length === 1
            ? `Remove ${chosen[0].repo}?`
            : `Remove ${chosen.length} repositories?`,
        description:
          "Their credentials are destroyed and the bot loses this access everywhere it is granted, through the bundle or on its own. This can't be undone.",
        confirmLabel: "Remove",
        destructive: true,
      });
      if (!ok) return;
      const failed: string[] = [];
      for (const c of chosen) {
        try {
          await api.del(`/api/connections/${c.id}`);
        } catch (err) {
          failed.push(`${c.repo}: ${errorMessage(err)}`);
        }
      }
      const done = chosen.length - failed.length;
      if (done > 0) toast.success(done === 1 ? `${chosen[0].repo} removed` : `${done} repositories removed`);
      for (const f of failed) toast.error(f);
      bundles.reload();
    }
  };

  // Tokens already sealed on repository connections, so a second repository needs no paste.
  const savedTokens: TokenSource[] = list.flatMap((b) =>
    (b.connections ?? [])
      .filter((c) => c.repo)
      .map((c) => ({
        connection_id: c.id,
        repo: c.repo,
        name: c.name,
        installation_id: c.github_installation_id,
        fp: c.secret_fp,
      })),
  );

  // One Create button, three things it can make: a bundle (a name), a repository (the same
  // token-first flow the Workspaces page has, saved under Repositories and attached nowhere until a
  // channel adds it), or a connection in a bundle.
  const createMenu = (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button>
          <Plus className="size-4" />
          Create
          <ChevronDown className="size-4 opacity-70" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent
        align="end"
        onCloseAutoFocus={(e) => {
          if (!openRepoOnClose.current) return;
          openRepoOnClose.current = false;
          e.preventDefault();
          setRepoOpen(true);
        }}
      >
        <DropdownMenuItem onSelect={() => setCreating(true)}>
          <Package className="size-4" /> Bundle
        </DropdownMenuItem>
        <DropdownMenuItem
          // Only flagged here; the popover opens once the menu has finished closing.
          onSelect={() => {
            openRepoOnClose.current = true;
          }}
        >
          <GitBranch className="size-4" /> Repository
        </DropdownMenuItem>
        <DropdownMenuItem onSelect={() => setNewConnection(true)}>
          <Cable className="size-4" /> Connection
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
  const createButton = (
    <ConnectRepoPopover
      busy={false}
      savedTokens={savedTokens}
      open={repoOpen}
      onOpenChange={setRepoOpen}
      // The anchor must be a DOM node; the menu root renders none, so it gets a wrapper.
      anchor={<span className="inline-flex">{createMenu}</span>}
      onConnected={bundles.reload}
    />
  );

  return (
    <div className="space-y-5">
      <PageHeader
        title="Access bundles"
        description="Credentials, domains and instructions grouped so they can be attached as one unit. Bundles belong to this account, so a bundle attached at the Workspace level is reachable from every connected Slack workspace — attach it to one workspace or channel when it should not be."
        actions={createButton}
      />

      {bundles.error && !bundles.data && (
        <ErrorBanner message={bundles.error} onRetry={bundles.reload} />
      )}

      {bundles.loading ? (
        <div className="space-y-3">
          {Array.from({ length: 3 }).map((_, i) => (
            <Skeleton key={i} className="h-14 w-full rounded-xl" />
          ))}
        </div>
      ) : list.length === 0 ? (
        <EmptyState
          icon={KeyRound}
          title="No bundles yet"
          description="A bundle holds what the bot may reach — an API key, a few allowed domains, some usage notes. Create one, connect a service, then attach the bundle, or a single connection from it, to a channel under Workspaces."
          action={
            <Button onClick={() => setCreating(true)}>
              <Plus className="size-4" />
              Create bundle
            </Button>
          }
        />
      ) : (
        <div className="space-y-3">
          {ordered.map((b) => (
            <BundleCard
              key={b.id}
              bundle={b}
              // Whatever was left open last time, and for a bundle nobody has touched, the old
              // rule: a short list opens itself, a long one would be a wall.
              open={cardOpen.get(b.id, list.length <= 3)}
              onOpenChange={(next) => cardOpen.set(b.id, next)}
              onAction={onBundleAction}
              onConnectionAction={onConnectionAction}
              onRepoBulk={onRepoBulk}
              installs={installs}
              canAttach={canAttach}
            />
          ))}
        </div>
      )}

      <NameDialog
        open={creating}
        onOpenChange={setCreating}
        title="Create bundle"
        description="Name it after what it grants — 'Engineering tools', 'Billing read-only'."
        label="Name"
        suggestion={defaultBundleName(list)}
        submitLabel="Create"
        onSubmit={create}
      />
      <NameDialog
        open={renaming !== null}
        onOpenChange={(open) => !open && setRenaming(null)}
        title="Rename bundle"
        label="Name"
        initial={renaming?.name ?? ""}
        submitLabel="Rename"
        onSubmit={rename}
      />
      <AttachScopesDialog
        bundle={attaching}
        onOpenChange={(open) => !open && setAttachingId(null)}
        onChanged={bundles.reload}
      />
      <BundleEditDialog
        bundle={managing}
        presets={presetList}
        onOpenChange={(open) => !open && setManagingId(null)}
        onConnect={(bundle, preset) => setConnect({ bundle, preset })}
        onChanged={bundles.reload}
      />
      <NewConnectionDialog
        open={newConnection}
        bundles={list}
        presets={presetList}
        onOpenChange={setNewConnection}
        onPick={(bundle, preset) => setConnect({ bundle, preset })}
        onCreateBundle={() => setCreating(true)}
        onBundlesChanged={bundles.reload}
      />
      <ConnectDialog
        target={connect}
        bundles={list}
        onOpenChange={(open) => !open && setConnect(null)}
        onSaved={bundles.reload}
      />
      <RotateSecretDialog
        connection={rotating}
        presets={presetList}
        onOpenChange={(open) => !open && setRotating(null)}
        onSaved={bundles.reload}
      />
      <CurlDialog
        connection={curlFor}
        onOpenChange={(open) => !open && setCurlFor(null)}
      />
      <RepoManagerDialog
        open={repoManaging !== null}
        onOpenChange={(open) => !open && setRepoManagingId(null)}
        title={repoManaging?.name ?? "Repositories"}
        description={
          repoRows.length === 0
            ? "No repositories yet. Add one and a channel can be given it from its own page."
            : `${repoRows.length} repositor${repoRows.length === 1 ? "y" : "ies"} from ${countRepoSources(repoRows)} source${countRepoSources(repoRows) === 1 ? "" : "s"}. Each is saved here and attached nowhere until a channel adds it.`
        }
        rows={repoRows}
        facts={(c) => c}
        add={(container) => (
          <ConnectRepoPopover
            busy={false}
            savedTokens={repoRows.map((c) => ({
              connection_id: c.id,
              repo: c.repo,
              name: c.name,
              installation_id: c.github_installation_id,
              fp: c.secret_fp,
            }))}
            open={addRepoOpen}
            onOpenChange={setAddRepoOpen}
            container={container}
            anchor={
              <Button size="sm" onClick={() => setAddRepoOpen(true)}>
                <GitBranch className="size-4" />
                Add repositories
              </Button>
            }
            onConnected={bundles.reload}
          />
        )}
        empty={
          <EmptyState
            icon={GitBranch}
            title="No repositories yet"
            description="Connect a GitHub App account or paste an access token, then pick what to save. Each repository is saved on its own, and a channel adds the ones it needs."
            action={
              <Button onClick={() => setAddRepoOpen(true)}>
                <GitBranch className="size-4" />
                Add repositories
              </Button>
            }
          />
        }
      >
        {(shown) => (
          <RepoGroupList
            key={shown.length}
            connections={shown}
            installs={installs}
            onAction={onConnectionAction}
            onBulk={onRepoBulk}
            checkGitHub
          />
        )}
      </RepoManagerDialog>
      <AttachReposDialog
        connections={attachRepos}
        onOpenChange={(open) => !open && setAttachRepos(null)}
        onDone={bundles.reload}
      />
      <RepoWritesDialog
        connections={writesRepos}
        onOpenChange={(open) => !open && setWritesRepos(null)}
        onDone={bundles.reload}
      />
      <RepoRecipeDialog
        connections={recipeRepos}
        onOpenChange={(open) => !open && setRecipeRepos(null)}
        onDone={bundles.reload}
      />
      <CopyToBundleDialog
        connection={copying}
        bundles={bundles.data ?? []}
        onOpenChange={(open) => !open && setCopying(null)}
        onDone={() => bundles.reload()}
      />
      {confirmDialog}
    </div>
  );
}
