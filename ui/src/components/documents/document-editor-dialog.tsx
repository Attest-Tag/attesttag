"use client";

import { useEffect, useState } from "react";
import { Loader2, Save } from "lucide-react";
import { toast } from "sonner";
import { ErrorBanner } from "@/components/core/error-banner";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Textarea } from "@/components/ui/textarea";
import { api, encodePath, errorMessage, type Document } from "@/lib/api";
import { formatBytes } from "@/lib/format";

type Props = {
  doc: Document | null;
  onClose: () => void;
  /** Called after a successful save so the list can refresh. */
  onSaved: () => void;
};

// Plain-text editor for a document. The body is keyed on the document path so
// opening another file (or retrying a failed load) starts from a clean slate.
export function DocumentEditorDialog({ doc, onClose, onSaved }: Props) {
  const [loadKey, setLoadKey] = useState(0);
  const [dirty, setDirty] = useState(false);

  const close = () => {
    if (dirty && !window.confirm("Discard unsaved changes?")) return;
    setDirty(false);
    onClose();
  };

  return (
    <Dialog open={doc !== null} onOpenChange={(open) => !open && close()}>
      <DialogContent className="flex h-[85vh] max-w-[calc(100%-2rem)] flex-col sm:max-w-4xl">
        {doc && (
          <EditorBody
            key={`${doc.path}#${loadKey}`}
            doc={doc}
            onDirty={setDirty}
            onRetry={() => setLoadKey((k) => k + 1)}
            onCancel={close}
            onSaved={() => {
              setDirty(false);
              onSaved();
              onClose();
            }}
          />
        )}
      </DialogContent>
    </Dialog>
  );
}

type BodyProps = {
  doc: Document;
  onDirty: (dirty: boolean) => void;
  onRetry: () => void;
  onCancel: () => void;
  onSaved: () => void;
};

function EditorBody({ doc, onDirty, onRetry, onCancel, onSaved }: BodyProps) {
  const [original, setOriginal] = useState<string | null>(null);
  const [text, setText] = useState("");
  const [loadError, setLoadError] = useState<string | undefined>();
  const [saving, setSaving] = useState(false);

  const dirty = original !== null && text !== original;

  useEffect(() => {
    let cancelled = false;
    api
      .text(`/api/documents/${encodePath(doc.path)}`)
      .then((body) => {
        if (cancelled) return;
        setOriginal(body);
        setText(body);
      })
      .catch((err) => {
        if (!cancelled) setLoadError(errorMessage(err));
      });
    return () => {
      cancelled = true;
    };
  }, [doc.path]);

  useEffect(() => {
    onDirty(dirty);
  }, [dirty, onDirty]);

  const save = async () => {
    setSaving(true);
    try {
      await api.put(`/api/documents/${encodePath(doc.path)}`, { content: text });
      setOriginal(text);
      toast.success(`Saved ${doc.name} — re-indexing in the background`);
      onSaved();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setSaving(false);
    }
  };

  const onKeyDown = (e: React.KeyboardEvent) => {
    if ((e.metaKey || e.ctrlKey) && e.key === "s") {
      e.preventDefault();
      if (dirty && !saving) save();
    }
  };

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-4" onKeyDown={onKeyDown}>
      <DialogHeader>
        <DialogTitle className="truncate">{doc.name}</DialogTitle>
        <DialogDescription className="truncate font-mono text-xs">
          {doc.path} · {formatBytes(new Blob([text]).size)}
        </DialogDescription>
      </DialogHeader>

      {loadError ? (
        <ErrorBanner message={loadError} onRetry={onRetry} />
      ) : original === null ? (
        <div className="flex flex-1 items-center justify-center text-sm text-muted-foreground">
          <Loader2 className="mr-2 size-4 animate-spin" />
          Loading…
        </div>
      ) : (
        <Textarea
          value={text}
          onChange={(e) => setText(e.target.value)}
          spellCheck={false}
          aria-label={`Contents of ${doc.name}`}
          className="min-h-0 flex-1 resize-none font-mono text-xs leading-relaxed [field-sizing:fixed]"
        />
      )}

      <DialogFooter className="items-center sm:justify-between">
        <span className="text-xs text-muted-foreground">
          {dirty ? "Unsaved changes" : original !== null ? "No changes" : ""}
        </span>
        <div className="flex gap-2">
          <Button variant="outline" onClick={onCancel} disabled={saving}>
            Cancel
          </Button>
          <Button onClick={save} disabled={!dirty || saving}>
            {saving ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}
            Save
          </Button>
        </div>
      </DialogFooter>
    </div>
  );
}
