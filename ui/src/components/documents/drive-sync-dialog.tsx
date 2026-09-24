"use client";

import { useRef, useState } from "react";
import { ExternalLink, Loader2, Plus, Zap } from "lucide-react";
import { toast } from "sonner";
import {
  createBundle,
  defaultBundleName,
} from "@/components/bundles/bundle-picker";
import { ConnectDialog, type ConnectTarget } from "@/components/bundles/connect-dialog";
import { NameDialog } from "@/components/bundles/name-dialog";
import { DrivePreviewPanel } from "@/components/documents/drive-preview";
import { DocumentScopeSelect } from "@/components/documents/document-scope-select";
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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectSeparator,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  api,
  errorMessage,
  useApi,
  type Bundle,
  type DriveConnection,
  type DrivePreview,
  type DriveSync,
  type Preset,
  type Scope,
} from "@/lib/api";

const ROOT = "__root__";
/** The "Add a credential…" row's value. No connection id can collide with it. */
const NEW_CRED = "__new_credential__";

/** "Engineering tools / Company Drive" — the bundle first, because that is what decides which
 *  channels the credential is reachable from, and two bundles can each hold a "Drive". */
function credentialLabel(c: { name: string; bundle_name: string }): string {
  return c.bundle_name ? `${c.bundle_name} / ${c.name}` : c.name;
}

type Props = {
  /** The sync being edited, or null when one is being added. */
  editing: DriveSync | null;
  folders: string[] | undefined;
  scopes: Scope[] | undefined;
  onClose: () => void;
  onSaved: () => void;
};

// Adding a Drive folder to follow, or changing where an existing one lands. The credential and
// the folder are fixed once saved: changing either would orphan every document the sync already
// owns, so that is a new sync and a deletion rather than an edit.
export function DriveSyncDialog({
  open,
  onOpenChange,
  editing,
  folders,
  scopes,
  onSaved,
}: Omit<Props, "onClose"> & { open: boolean; onOpenChange: (open: boolean) => void }) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      {/* Scrolls as a whole: a tested folder adds its tree underneath the fields. */}
      <DialogContent className="max-h-[calc(100vh-2rem)] overflow-y-auto sm:max-w-lg">
        {/* Keyed on the row, so opening the dialog on a different folder starts from that
            folder's values rather than resetting the last one's in an effect. */}
        {open && (
          <DriveSyncForm
            key={editing ? `edit-${editing.id}` : "new"}
            editing={editing}
            folders={folders}
            scopes={scopes}
            onClose={() => onOpenChange(false)}
            onSaved={onSaved}
          />
        )}
      </DialogContent>
    </Dialog>
  );
}

function DriveSyncForm({ editing, folders, scopes, onClose, onSaved }: Props) {
  const conns = useApi<DriveConnection[]>(editing ? null : "/api/drive/connections");
  // Bundles and presets are fetched only once somebody actually asks to add a credential: the
  // picker does not need every bundle and preset in the deployment just to list what exists.
  const [bundles, setBundles] = useState<Bundle[]>([]);
  const [connectTarget, setConnectTarget] = useState<ConnectTarget | null>(null);
  // A credential is filed under a bundle, so a deployment with none needs one before the
  // credential dialog can open. Rather than a dead end that sends somebody to another page,
  // the first bundle is named here and the flow carries on.
  const [namingBundle, setNamingBundle] = useState<Preset | null>(null);
  const [picked, setPicked] = useState("");
  // See BundlePicker: set while the dropdown closes into a dialog, so focus is left alone.
  const toDialog = useRef(false);
  const [folder, setFolder] = useState("");
  const [dest, setDest] = useState(editing?.dest ?? "");
  const [scope, setScope] = useState(editing?.scope ?? "");
  const [recurse, setRecurse] = useState(editing ? editing.recurse : true);
  const [saving, setSaving] = useState(false);
  // "Test": what a pass would bring in, worked out from Drive's listing without copying a
  // thing. Remembered against the inputs it was run on, so changing the folder, the credential
  // or the subfolders switch makes it stale and it goes, rather than staying up to describe a
  // folder nobody is looking at any more.
  const [testing, setTesting] = useState(false);
  const [preview, setPreview] = useState<{
    on: string;
    data: DrivePreview | null;
    error: string | null;
  } | null>(null);

  const list = conns.data ?? [];
  // One connection and nothing to choose between: treat it as chosen, rather than making
  // somebody open a dropdown to agree with the only answer.
  const connID = picked || (list.length === 1 ? String(list[0].id) : "");
  const noCredential = !editing && !conns.loading && list.length === 0;
  const ready = editing ? true : connID !== "" && folder.trim() !== "";
  const previewOn = editing ? `${editing.id}|${recurse}` : `${connID}|${folder.trim()}|${recurse}`;
  const shown = preview && preview.on === previewOn ? preview : null;

  const test = async () => {
    const on = previewOn;
    setTesting(true);
    try {
      const data = await api.post<DrivePreview>(
        "/api/drive/syncs/preview",
        editing
          ? { sync_id: editing.id, dest, recurse }
          : { connection_id: Number(connID), folder, dest, recurse },
      );
      setPreview({ on, data, error: null });
    } catch (err) {
      setPreview({ on, data: null, error: errorMessage(err) });
    } finally {
      setTesting(false);
    }
  };

  // "+" opens the same credential dialog the Bundles page uses, on the Google Drive preset, so
  // a first Drive folder can be set up without leaving Documents. The bundle it is filed under
  // is chosen inside that dialog; this only has to hand it a starting one.
  const addCredential = async () => {
    try {
      const [bs, ps] = await Promise.all([
        api.get<Bundle[]>("/api/bundles"),
        api.get<Preset[]>("/api/presets"),
      ]);
      const gdrive = ps.find((p) => p.id === "gdrive");
      if (!gdrive) {
        toast.error("This build has no Google Drive preset");
        return;
      }
      setBundles(bs);
      if (bs.length === 0) {
        setNamingBundle(gdrive);
        return;
      }
      setConnectTarget({ preset: gdrive, bundle: bs[0] });
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  const save = async (e: React.FormEvent) => {
    e.preventDefault();
    setSaving(true);
    try {
      if (editing) {
        await api.put(`/api/drive/syncs/${editing.id}`, {
          dest,
          scope,
          recurse,
          enabled: editing.enabled,
        });
        toast.success(`Updated ${editing.folder_name || "the folder"}`);
      } else {
        const res = await api.post<{ id: number; folder_name: string }>("/api/drive/syncs", {
          connection_id: Number(connID),
          folder,
          dest,
          scope,
          recurse,
        });
        toast.success(`Following ${res.folder_name}`, {
          description: "Sync now to bring it in, or wait for the next scheduled pass.",
        });
      }
      onSaved();
      onClose();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setSaving(false);
    }
  };

  return (
    <form onSubmit={save} className="grid gap-4">
      <DialogHeader>
        <DialogTitle>{editing ? "Edit Drive folder" : "Sync a Drive folder"}</DialogTitle>
        <DialogDescription>
          {editing
            ? `${editing.folder_name || editing.folder_id} on ${editing.connection_name}. The folder and the credential can't be changed — remove this and add it again to point somewhere else.`
            : "Files in the folder are copied into Documents and kept in step with it. Google Docs come across as Markdown, Sheets as CSV, Slides as PDF."}
        </DialogDescription>
      </DialogHeader>

      {noCredential && (
        <div className="flex flex-wrap items-center justify-between gap-3 rounded-md border border-dashed bg-muted/40 px-3 py-3">
          <p className="max-w-sm text-sm text-muted-foreground">
            No credential here reaches Drive yet. Add one, then share the folder with its service
            account&rsquo;s email.
          </p>
          <Button type="button" variant="outline" size="sm" onClick={addCredential}>
            <Plus className="size-4" />
            Add credential
          </Button>
        </div>
      )}

      {!editing && (
        <>
          <div className="grid gap-2">
            <Label htmlFor="drive-conn">Credential</Label>
            <Select
              value={connID}
              onValueChange={(v) => {
                if (v !== NEW_CRED) {
                  setPicked(v);
                  return;
                }
                toDialog.current = true;
                addCredential();
              }}
            >
              <SelectTrigger id="drive-conn" className="w-full">
                <SelectValue placeholder={conns.loading ? "Loading…" : "Pick a Drive credential"} />
              </SelectTrigger>
              <SelectContent
                // The dropdown is opening a dialog, so the trigger must not take focus back —
                // otherwise the dialog's first field never gets it. See BundlePicker.
                onCloseAutoFocus={(e) => {
                  if (!toDialog.current) return;
                  toDialog.current = false;
                  e.preventDefault();
                }}
              >
                {list.map((c) => (
                  <SelectItem key={c.id} value={String(c.id)}>
                    {credentialLabel(c)}
                  </SelectItem>
                ))}
                {list.length > 0 && <SelectSeparator />}
                {/* A credential can be made here rather than sending somebody away
                    mid-thought to come back and start the folder over. */}
                <SelectItem value={NEW_CRED}>
                  <Plus className="size-4 text-muted-foreground" />
                  Add a credential…
                </SelectItem>
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground">
              Bundle / credential — the bundle decides which channels can reach it.
            </p>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="drive-folder">Drive folder</Label>
            <Input
              id="drive-folder"
              value={folder}
              onChange={(e) => setFolder(e.target.value)}
              placeholder="https://drive.google.com/drive/folders/…"
              autoComplete="off"
              spellCheck={false}
            />
            <p className="text-xs text-muted-foreground">
              Paste the link from Drive&rsquo;s address bar. The folder has to be shared with the
              connection&rsquo;s service account, or it will come back empty.{" "}
              <a
                className="inline-flex items-center gap-1 underline underline-offset-2"
                href="https://drive.google.com/drive/my-drive"
                target="_blank"
                rel="noreferrer"
              >
                Open Drive <ExternalLink className="size-3" />
              </a>
            </p>
          </div>
        </>
      )}

      <div className="grid gap-2">
        <Label htmlFor="drive-dest">Lands in</Label>
        <Select value={dest === "" ? ROOT : dest} onValueChange={(v) => setDest(v === ROOT ? "" : v)}>
          <SelectTrigger id="drive-dest" className="w-full">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={ROOT}>Top level</SelectItem>
            {(folders ?? []).map((f) => (
              <SelectItem key={f} value={f}>
                {f}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <p className="text-xs text-muted-foreground">
          Subfolders in Drive keep their shape underneath this one.
        </p>
      </div>

      <div className="grid gap-2">
        <Label htmlFor="drive-scope">Scope</Label>
        <DocumentScopeSelect
          value={scope}
          onChange={setScope}
          scopes={scopes}
          aria-label="Scope for everything this folder brings in"
        />
        <p className="text-xs text-muted-foreground">
          Every file this folder brings in gets this scope. Change it here rather than on the
          documents, which are rewritten each pass.
        </p>
      </div>

      <label className="flex items-start gap-2 text-sm">
        <Checkbox
          checked={recurse}
          onCheckedChange={(v) => setRecurse(v === true)}
          className="mt-0.5"
        />
        <span>
          Include subfolders
          <span className="block text-xs text-muted-foreground">
            Off means only the files sitting directly in the folder.
          </span>
        </span>
      </label>

      {shown && <DrivePreviewPanel preview={shown.data} error={shown.error} dest={dest} />}

      <DialogFooter className="sm:justify-between">
        {/* Reads the folder on the credential and shows what a pass would take, before anything
            is saved. Same readiness as saving: it needs the credential and the folder. */}
        <Button type="button" variant="outline" onClick={test} disabled={testing || saving || !ready}>
          {testing ? <Loader2 className="size-4 animate-spin" /> : <Zap className="size-4" />}
          {shown?.data ? "Test again" : "Test"}
        </Button>
        <div className="flex flex-col-reverse gap-2 sm:flex-row">
          <Button type="button" variant="outline" onClick={onClose} disabled={saving}>
            Cancel
          </Button>
          <Button type="submit" disabled={saving || !ready}>
            {saving && <Loader2 className="size-4 animate-spin" />}
            {editing ? "Save" : "Add folder"}
          </Button>
        </div>
      </DialogFooter>

      <NameDialog
        open={namingBundle !== null}
        onOpenChange={(open) => !open && setNamingBundle(null)}
        title="New bundle"
        description="A credential is filed under a bundle, and the bot reaches it wherever that bundle is attached."
        label="Name"
        suggestion={defaultBundleName([])}
        submitLabel="Create"
        onSubmit={async (name) => {
          const preset = namingBundle;
          if (!preset) return;
          const made = await createBundle(name);
          setBundles([made]);
          setNamingBundle(null);
          setConnectTarget({ preset, bundle: made });
        }}
      />
      <ConnectDialog
        target={connectTarget}
        bundles={bundles}
        onBundlesChanged={async () => setBundles(await api.get<Bundle[]>("/api/bundles"))}
        onOpenChange={(open) => !open && setConnectTarget(null)}
        onSaved={() => {
          setConnectTarget(null);
          // The new credential is what they came for, so select it as soon as it exists.
          conns.reload();
        }}
      />
    </form>
  );
}
