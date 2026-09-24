"use client";

import { useMemo, useRef, useState } from "react";
import {
  BookOpen,
  ChevronRight,
  Folder,
  FolderPlus,
  FolderSync,
  FolderUp,
  MoreHorizontal,
  Pencil,
  RefreshCw,
  Trash2,
  Upload,
} from "lucide-react";
import { toast } from "sonner";
import { useConfirm } from "@/components/core/confirm-dialog";
import { EmptyState } from "@/components/core/empty-state";
import { ErrorBanner } from "@/components/core/error-banner";
import { PageHeader } from "@/components/core/page-header";
import { RelativeTime } from "@/components/core/relative-time";
import {
  StatusChip,
  type StatusChipVariant,
} from "@/components/core/status-chip";
import { TableSkeleton } from "@/components/core/table-skeleton";
import { DocumentEditorDialog } from "@/components/documents/document-editor-dialog";
import { DriveSyncPanel } from "@/components/documents/drive-sync-panel";
import { DocumentScopeSelect } from "@/components/documents/document-scope-select";
import { MoveDialog, type MoveTarget } from "@/components/documents/move-dialog";
import { NewFolderDialog } from "@/components/documents/new-folder-dialog";
import {
  ACCEPT,
  DropOverlay,
  scanFiles,
  scanNote,
  useFileDrop,
  type Scan,
  type Upload as PendingUpload,
} from "@/components/documents/upload-dropzone";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  api,
  encodePath,
  errorMessage,
  isEditableDocument,
  useApi,
  type Document,
  type IngestReport,
  type Scope,
} from "@/lib/api";
import { formatBytes, formatNanos } from "@/lib/format";
import { cn } from "@/lib/utils";

const STATUS_TONE: Record<string, StatusChipVariant> = {
  indexed: "success",
  pending: "warning",
  error: "danger",
};

/** A folder as the table draws it: the totals underneath, and where those documents apply. */
type FolderRow = {
  path: string;
  name: string;
  docs: number;
  size: number;
  chunks: number;
  scope: string | null;
  fromDrive: boolean;
};

/** True when path sits directly in folder, rather than deeper down or elsewhere. */
function isChildOf(path: string, folder: string): boolean {
  if (folder === "") return !path.includes("/");
  return path.startsWith(folder + "/") && !path.slice(folder.length + 1).includes("/");
}

function baseName(path: string): string {
  const i = path.lastIndexOf("/");
  return i < 0 ? path : path.slice(i + 1);
}

export function DocumentsPage() {
  const docs = useApi<Document[]>("/api/documents");
  const folders = useApi<string[]>("/api/document-folders");
  const scopes = useApi<Scope[]>("/api/scopes?sync=0");
  const { confirm, confirmDialog } = useConfirm();
  const [cwd, setCwd] = useState("");
  const [uploadScope, setUploadScope] = useState("");
  const [uploading, setUploading] = useState(false);
  const [reindexing, setReindexing] = useState(false);
  const [editing, setEditing] = useState<Document | null>(null);
  const [moving, setMoving] = useState<MoveTarget | null>(null);
  const [newFolder, setNewFolder] = useState(false);
  const fileInput = useRef<HTMLInputElement>(null);
  const folderInput = useRef<HTMLInputElement>(null);

  const reload = () => {
    docs.reload();
    folders.reload();
  };

  // What is in the folder that is open: its subfolders first, then its documents. Folders
  // carry the totals of everything underneath so a collapsed tree still says something.
  const { childFolders, files } = useMemo(() => {
    const all = docs.data ?? [];
    const childFolders = (folders.data ?? [])
      .filter((f) => isChildOf(f, cwd))
      .map((path): FolderRow => {
        const inside = all.filter((d) => d.path.startsWith(path + "/"));
        return {
          path,
          name: baseName(path),
          docs: inside.length,
          size: inside.reduce((n, d) => n + d.size, 0),
          chunks: inside.reduce((n, d) => n + d.chunks, 0),
          // One scope only when everything underneath already agrees; null is "mixed", and
          // the folder's picker shows that rather than claiming one of the answers.
          scope: inside.every((d) => d.scope === inside[0]?.scope) ? (inside[0]?.scope ?? "") : null,
          fromDrive: inside.some((d) => d.drive_sync_id),
        };
      });
    return { childFolders, files: all.filter((d) => isChildOf(d.path, cwd)) };
  }, [docs.data, folders.data, cwd]);

  const upload = async (uploads: PendingUpload[]) => {
    if (uploads.length === 0) return;
    setUploading(true);
    const form = new FormData();
    // A part's filename cannot carry a folder: the server is required to ignore the directory
    // in it. The path each file should keep travels beside it instead, in the same order.
    for (const u of uploads) {
      form.append("files", u.file);
      form.append("paths", u.path);
    }
    if (uploadScope) form.append("scope", uploadScope);
    if (cwd) form.append("folder", cwd);
    try {
      const res = await api.upload<{ saved: string[] | null }>("/api/documents", form);
      const n = res.saved?.length ?? 0;
      toast.success(
        `Uploaded ${n} file${n === 1 ? "" : "s"} — indexing in the background`,
      );
      reload();
      // The index runs after the reply; a second look picks up the chunk counts.
      setTimeout(reload, 4000);
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setUploading(false);
    }
  };

  // A scan from the picker or from a drop. What it refused is said once, here, rather than
  // parked in a line of helper text nobody is looking at.
  const take = (scan: Scan) => {
    const note = scanNote(scan);
    if (note) toast.warning(note);
    upload(scan.uploads);
  };

  const { over, dropProps } = useFileDrop(take);

  const reindex = async () => {
    setReindexing(true);
    try {
      const res = await api.post<{
        ok: boolean;
        report?: IngestReport;
        error?: string;
      }>("/api/documents/reindex");
      if (!res.ok || !res.report) {
        toast.error(res.error || "Re-index failed");
      } else {
        const r = res.report;
        const errors = r.Errors?.length ?? 0;
        const line = `${r.Docs} document${r.Docs === 1 ? "" : "s"}, ${r.Chunks} chunks — ${r.Embedded} embedded, ${r.Unchanged} unchanged, ${r.Deleted} removed in ${formatNanos(r.Took)}`;
        if (errors > 0)
          toast.warning(
            `Re-indexed with ${errors} error${errors === 1 ? "" : "s"}`,
            { description: line },
          );
        else toast.success("Re-indexed", { description: line });
      }
      reload();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setReindexing(false);
    }
  };

  const setScope = async (doc: Document, scope: string) => {
    docs.mutate((list) =>
      list?.map((d) => (d.path === doc.path ? { ...d, scope } : d)),
    );
    try {
      await api.put(`/api/documents/${encodePath(doc.path)}`, { scope });
      toast.success("Scope updated");
    } catch (err) {
      toast.error(errorMessage(err));
      docs.reload();
    }
  };

  // A folder's scope is a bulk apply rather than something the folder keeps: it writes onto
  // the documents that are in it now, and any one of them can be put on its own scope
  // afterwards, inside the folder. Confirmed only when the change would bury something —
  // per-document scopes that differ, or a Drive sync that will have its own say later.
  const setFolderScope = async (folder: FolderRow, scope: string) => {
    if (folder.scope === scope) return;
    if (folder.scope === null || folder.fromDrive) {
      const ok = await confirm({
        title: `Scope all ${folder.docs} document${folder.docs === 1 ? "" : "s"} in ${folder.name}?`,
        description: [
          folder.scope === null &&
            "The documents in this folder are not all on the same scope today, and each one's own setting is replaced.",
          folder.fromDrive &&
            "Some of them are mirrored from Google Drive: where that sync carries a scope of its own, a later pass puts those documents back on it.",
        ]
          .filter(Boolean)
          .join(" "),
        confirmLabel: "Apply to folder",
      });
      if (!ok) return;
    }
    const prefix = folder.path + "/";
    docs.mutate((list) =>
      list?.map((d) => (d.path.startsWith(prefix) ? { ...d, scope } : d)),
    );
    try {
      const res = await api.put<{ updated: number }>(
        `/api/document-folders/${encodePath(folder.path)}`,
        { scope },
      );
      toast.success(
        `Scope set on ${res.updated} document${res.updated === 1 ? "" : "s"}`,
      );
    } catch (err) {
      toast.error(errorMessage(err));
      docs.reload();
    }
  };

  const remove = async (doc: Document) => {
    const ok = await confirm({
      title: `Delete ${doc.name}?`,
      description:
        "The file and its index chunks are removed and the bot can no longer search it. This can't be undone.",
      confirmLabel: "Delete",
      destructive: true,
    });
    if (!ok) return;
    try {
      await api.del(`/api/documents/${encodePath(doc.path)}`);
      toast.success("Document deleted");
      reload();
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  const removeFolder = async (folder: { path: string; name: string; docs: number }) => {
    const ok = await confirm({
      title: `Delete ${folder.name}?`,
      description:
        folder.docs === 0
          ? "The folder is empty, so nothing else goes with it."
          : `The folder and the ${folder.docs} document${folder.docs === 1 ? "" : "s"} in it are removed, along with their index chunks. This can't be undone.`,
      confirmLabel: "Delete",
      destructive: true,
    });
    if (!ok) return;
    try {
      await api.del(`/api/document-folders/${encodePath(folder.path)}`);
      toast.success(`Deleted ${folder.name}`);
      reload();
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  const uploadMenu = (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button disabled={uploading}>
          <Upload className="size-4" />
          Upload
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuItem onClick={() => fileInput.current?.click()}>
          <Upload className="size-4" /> Files…
        </DropdownMenuItem>
        <DropdownMenuItem onClick={() => folderInput.current?.click()}>
          <FolderUp className="size-4" /> Folder…
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );

  const crumbs = cwd === "" ? [] : cwd.split("/");

  return (
    <div className="space-y-5" {...dropProps}>
      {over && <DropOverlay folder={cwd} />}
      <PageHeader
        title="Documents"
        description="Files the bot searches when it answers. Markdown, text, HTML, PDF, CSV, JSON and Word."
        actions={
          <>
            <Button variant="outline" onClick={reindex} disabled={reindexing}>
              <RefreshCw
                className={reindexing ? "size-4 animate-spin" : "size-4"}
              />
              Re-index all
            </Button>
            <Button variant="outline" onClick={() => setNewFolder(true)}>
              <FolderPlus className="size-4" />
              New folder
            </Button>
            {/* The scope an upload lands with. Beside the button that uses it, because it is
                read at upload time and afterwards each row carries its own. */}
            <DocumentScopeSelect
              value={uploadScope}
              onChange={setUploadScope}
              scopes={scopes.data}
              compact
              aria-label="Scope for new uploads"
            />
            {uploadMenu}
          </>
        }
      />
      <input
        ref={fileInput}
        type="file"
        multiple
        accept={ACCEPT}
        className="hidden"
        onChange={(e) => {
          take(scanFiles(Array.from(e.target.files ?? [])));
          e.target.value = "";
        }}
      />
      {/* webkitdirectory is what turns the picker into a folder picker; every browser the
          console supports takes it, and React passes it through untouched. */}
      <input
        ref={folderInput}
        type="file"
        multiple
        className="hidden"
        {...{ webkitdirectory: "", directory: "" }}
        onChange={(e) => {
          take(scanFiles(Array.from(e.target.files ?? [])));
          e.target.value = "";
        }}
      />

      <DriveSyncPanel folders={folders.data} scopes={scopes.data} onChanged={reload} />

      {crumbs.length > 0 && (
        <nav aria-label="Folder" className="flex flex-wrap items-center gap-1 text-sm">
          <button
            type="button"
            onClick={() => setCwd("")}
            className="rounded px-1.5 py-0.5 text-muted-foreground hover:bg-secondary hover:text-foreground"
          >
            Documents
          </button>
          {crumbs.map((seg, i) => {
            const path = crumbs.slice(0, i + 1).join("/");
            const last = i === crumbs.length - 1;
            return (
              <span key={path} className="flex items-center gap-1">
                <ChevronRight className="size-3.5 text-muted-foreground" />
                <button
                  type="button"
                  onClick={() => setCwd(path)}
                  aria-current={last ? "page" : undefined}
                  className={cn(
                    "rounded px-1.5 py-0.5 hover:bg-secondary",
                    last ? "font-medium" : "text-muted-foreground hover:text-foreground",
                  )}
                >
                  {seg}
                </button>
              </span>
            );
          })}
        </nav>
      )}

      {docs.error && !docs.data && (
        <ErrorBanner message={docs.error} onRetry={reload} />
      )}

      {docs.loading ? (
        <TableSkeleton rows={5} columns={6} />
      ) : docs.data && childFolders.length === 0 && files.length === 0 ? (
        <EmptyState
          icon={BookOpen}
          title={cwd === "" ? "No documents" : `${baseName(cwd)} is empty`}
          description={
            cwd === ""
              ? "Drop runbooks, policies or product docs here and the bot will cite them in its answers. Start with the file people ask about most."
              : "Drop files or a folder here, or move documents in from elsewhere."
          }
          action={uploadMenu}
        />
      ) : docs.data ? (
        <div className="overflow-hidden rounded-xl border bg-card">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead className="text-right">Size</TableHead>
                <TableHead className="text-right">Chunks</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Scope</TableHead>
                <TableHead>Updated</TableHead>
                <TableHead className="w-20"></TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {childFolders.map((f) => (
                <TableRow key={f.path} className="bg-muted/20">
                  <TableCell>
                    <button
                      type="button"
                      onClick={() => setCwd(f.path)}
                      className="flex items-center gap-2 font-medium hover:underline"
                    >
                      <Folder className="size-4 text-muted-foreground" />
                      {f.name}
                    </button>
                    <span className="block pl-6 text-[10px] text-muted-foreground">
                      {f.docs} document{f.docs === 1 ? "" : "s"}
                    </span>
                  </TableCell>
                  <TableCell className="text-right text-xs tabular-nums text-muted-foreground">
                    {f.docs === 0 ? "—" : formatBytes(f.size)}
                  </TableCell>
                  <TableCell className="text-right text-xs tabular-nums text-muted-foreground">
                    {f.docs === 0 ? "—" : f.chunks}
                  </TableCell>
                  <TableCell></TableCell>
                  <TableCell>
                    {f.docs > 0 && (
                      <DocumentScopeSelect
                        value={f.scope}
                        onChange={(v) => setFolderScope(f, v)}
                        scopes={scopes.data}
                        compact
                        placeholder="Mixed"
                        aria-label={`Scope for everything in ${f.name}`}
                      />
                    )}
                  </TableCell>
                  <TableCell></TableCell>
                  <TableCell>
                    <div className="flex justify-end">
                      <DropdownMenu>
                        <DropdownMenuTrigger asChild>
                          <Button
                            variant="ghost"
                            size="icon-sm"
                            aria-label={`Actions for ${f.name}`}
                            className="text-muted-foreground hover:text-foreground"
                          >
                            <MoreHorizontal />
                          </Button>
                        </DropdownMenuTrigger>
                        <DropdownMenuContent align="end">
                          <DropdownMenuItem
                            onClick={() => setMoving({ kind: "folder", path: f.path, name: f.name })}
                          >
                            <FolderUp className="size-4" /> Move to…
                          </DropdownMenuItem>
                          <DropdownMenuSeparator />
                          <DropdownMenuItem variant="destructive" onClick={() => removeFolder(f)}>
                            <Trash2 className="size-4" /> Delete
                          </DropdownMenuItem>
                        </DropdownMenuContent>
                      </DropdownMenu>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
              {files.map((d) => (
                <TableRow key={d.path}>
                  <TableCell>
                    <span className="flex items-center gap-1.5 font-medium">
                      {d.name}
                      {/* A Drive-owned document is rewritten on every pass, so the row says so
                          before somebody edits it and loses the edit six hours later. */}
                      {d.drive_sync_id ? (
                        <span title="Synced from Google Drive — edits here are overwritten by the next sync">
                          <FolderSync className="size-3.5 shrink-0 text-muted-foreground" aria-hidden />
                          <span className="sr-only">Synced from Google Drive</span>
                        </span>
                      ) : null}
                    </span>
                    {d.path !== d.name && (
                      <span className="block truncate font-mono text-[10px] text-muted-foreground">
                        {d.path}
                      </span>
                    )}
                  </TableCell>
                  <TableCell className="text-right text-xs tabular-nums">
                    {formatBytes(d.size)}
                  </TableCell>
                  <TableCell className="text-right text-xs tabular-nums">
                    {d.chunks}
                  </TableCell>
                  <TableCell>
                    <div className="flex items-center gap-2">
                      <StatusChip variant={STATUS_TONE[d.status] ?? "neutral"}>
                        {d.status || "pending"}
                      </StatusChip>
                      {d.last_error && (
                        <span
                          className="max-w-xs truncate text-xs text-danger"
                          title={d.last_error}
                        >
                          {d.last_error}
                        </span>
                      )}
                    </div>
                  </TableCell>
                  <TableCell>
                    <DocumentScopeSelect
                      value={d.scope}
                      onChange={(v) => setScope(d, v)}
                      scopes={scopes.data}
                      compact
                      aria-label={`Scope for ${d.name}`}
                    />
                  </TableCell>
                  <TableCell className="text-xs">
                    <RelativeTime value={d.updated_at} />
                  </TableCell>
                  <TableCell>
                    <div className="flex justify-end gap-1">
                      <Button
                        variant="ghost"
                        size="icon-sm"
                        aria-label={`Edit ${d.name}`}
                        title={
                          !isEditableDocument(d.path)
                            ? "Only text documents can be edited"
                            : d.drive_sync_id
                              ? "Edit — but this document comes from Drive, and the next sync overwrites whatever is typed here"
                              : "Edit"
                        }
                        className="text-muted-foreground hover:text-foreground"
                        disabled={!isEditableDocument(d.path)}
                        onClick={() => setEditing(d)}
                      >
                        <Pencil />
                      </Button>
                      <DropdownMenu>
                        <DropdownMenuTrigger asChild>
                          <Button
                            variant="ghost"
                            size="icon-sm"
                            aria-label={`Actions for ${d.name}`}
                            className="text-muted-foreground hover:text-foreground"
                          >
                            <MoreHorizontal />
                          </Button>
                        </DropdownMenuTrigger>
                        <DropdownMenuContent align="end">
                          <DropdownMenuItem
                            onClick={() => setMoving({ kind: "document", path: d.path, name: d.name })}
                          >
                            <FolderUp className="size-4" /> Move to…
                          </DropdownMenuItem>
                          <DropdownMenuSeparator />
                          <DropdownMenuItem variant="destructive" onClick={() => remove(d)}>
                            <Trash2 className="size-4" /> Delete
                          </DropdownMenuItem>
                        </DropdownMenuContent>
                      </DropdownMenu>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      ) : null}
      <DocumentEditorDialog
        doc={editing}
        onClose={() => setEditing(null)}
        onSaved={() => {
          docs.reload();
          setTimeout(docs.reload, 4000);
        }}
      />
      <NewFolderDialog
        open={newFolder}
        parent={cwd}
        onOpenChange={setNewFolder}
        onCreated={() => folders.reload()}
      />
      <MoveDialog
        target={moving}
        folders={folders.data ?? []}
        onOpenChange={(open) => !open && setMoving(null)}
        onMoved={() => {
          setMoving(null);
          reload();
          setTimeout(reload, 4000);
        }}
      />
      {confirmDialog}
    </div>
  );
}
