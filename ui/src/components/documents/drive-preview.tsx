"use client";

import { useState } from "react";
import { CheckCircle2, ChevronRight, FileText, Folder, XCircle } from "lucide-react";
import type { DriveNode, DrivePreview } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import { cn } from "@/lib/utils";

/** What a pass would do, as a sentence. Zeroes are dropped, the way the run report reads. */
function describePreview(p: DrivePreview): string {
  const parts = [
    p.files === 0 ? "nothing would come in" : `${p.files} file${p.files === 1 ? "" : "s"} would come in`,
  ];
  if (p.skipped) parts.push(`${p.skipped} skipped`);
  if (p.folders) parts.push(`${p.folders} subfolder${p.folders === 1 ? "" : "s"}`);
  return parts.join(", ") + (p.bytes ? ` · ${formatBytes(p.bytes)}` : "");
}

// The result of "Test" in the Drive folder dialog: the folder as Drive has it, each file with
// the name it would have as a document or the reason it would not become one — so what the
// sync will do is seen before it is saved, rather than found in a run log six hours later.
export function DrivePreviewPanel({
  preview,
  error,
  dest,
}: {
  preview: DrivePreview | null;
  error: string | null;
  /** Where in Documents it would land; "" is the top level. */
  dest: string;
}) {
  if (error) {
    return (
      <div
        role="alert"
        className="flex items-start gap-2 rounded-lg border border-danger/40 bg-danger-soft px-3 py-2 text-sm text-danger"
      >
        <XCircle className="mt-0.5 size-4 shrink-0" />
        <span className="[overflow-wrap:anywhere]">{error}</span>
      </div>
    );
  }
  if (!preview) return null;
  const empty = (preview.tree.children?.length ?? 0) === 0;
  return (
    <div className="rounded-lg border bg-muted/40">
      <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5 border-b px-3 py-2 text-sm">
        <CheckCircle2 className="size-4 shrink-0 text-success-text" />
        <span className="font-medium">{preview.folder_name}</span>
        <span className="text-xs text-muted-foreground">
          {describePreview(preview)} · lands in {dest || "top level"}
        </span>
      </div>
      {empty ? (
        <p className="px-3 py-2 text-xs text-muted-foreground">
          The folder is empty, or nothing in it is visible to this connection.
        </p>
      ) : (
        <ul className="max-h-64 overflow-auto py-1 text-sm">
          <PreviewNode node={preview.tree} depth={0} />
        </ul>
      )}
      {(preview.capped || preview.more > 0) && (
        <p className="border-t px-3 py-2 text-xs text-warning">
          {preview.capped &&
            "The folder is over what one pass walks, so a pass takes this first slice and stops. "}
          {preview.more > 0 &&
            `${preview.more} more entr${preview.more === 1 ? "y is" : "ies are"} counted above but not listed.`}
        </p>
      )}
    </div>
  );
}

function PreviewNode({ node, depth }: { node: DriveNode; depth: number }) {
  // The top two levels open, deeper ones closed: the shape of the folder is what somebody came
  // to see, and a big tree's leaves are a click away rather than a scroll.
  const [open, setOpen] = useState(depth < 2);
  const kids = node.children ?? [];
  const indent = { paddingLeft: `${depth + 0.5}rem` };

  if (node.folder) {
    const skipped = Boolean(node.skip);
    const leaf = skipped || kids.length === 0;
    return (
      <li>
        <button
          type="button"
          onClick={() => setOpen((o) => !o)}
          disabled={leaf}
          aria-expanded={leaf ? undefined : open}
          className="flex w-full items-center gap-1.5 py-0.5 pr-3 text-left hover:bg-muted/60 disabled:hover:bg-transparent"
          style={indent}
        >
          <ChevronRight
            className={cn(
              "size-3.5 shrink-0 text-muted-foreground transition-transform",
              open && !leaf && "rotate-90",
              leaf && "invisible",
            )}
          />
          <Folder className="size-4 shrink-0 text-muted-foreground" />
          <span className={cn("truncate", skipped && "text-muted-foreground")}>{node.name}</span>
          {skipped ? (
            <span className="min-w-0 truncate text-xs text-muted-foreground">— {node.skip}</span>
          ) : kids.length === 0 ? (
            <span className="text-xs text-muted-foreground">empty</span>
          ) : null}
        </button>
        {open && kids.length > 0 && (
          <ul>
            {kids.map((c, i) => (
              <PreviewNode key={`${i}-${c.name}`} node={c} depth={depth + 1} />
            ))}
          </ul>
        )}
      </li>
    );
  }

  return (
    <li className="flex items-center gap-1.5 py-0.5 pr-3" style={indent}>
      <span className="size-3.5 shrink-0" aria-hidden />
      <FileText className="size-4 shrink-0 text-muted-foreground" />
      <span className={cn("truncate", node.skip && "text-muted-foreground")}>{node.name}</span>
      {node.skip ? (
        <span className="min-w-0 truncate text-xs text-muted-foreground">— {node.skip}</span>
      ) : node.doc && node.doc !== node.name ? (
        // A native Doc has no extension of its own; this is the name it takes as a document.
        <span className="min-w-0 truncate text-xs text-muted-foreground">→ {node.doc}</span>
      ) : null}
      {node.size ? (
        <span className="ml-auto shrink-0 pl-2 text-xs tabular-nums text-muted-foreground">
          {formatBytes(node.size)}
        </span>
      ) : null}
    </li>
  );
}
