"use client";

import { useCallback, useRef, useState } from "react";
import { UploadCloud } from "lucide-react";

export const EXTENSIONS = ["md", "txt", "html", "pdf", "csv", "json", "docx"] as const;
export const ACCEPT = EXTENSIONS.map((e) => `.${e}`).join(",");

/** A file and the path it should keep, relative to the folder it is being uploaded into. */
export type Upload = { file: File; path: string };

/** Dropping a folder can reach a whole checkout, so one batch has a ceiling. */
export const MAX_FILES = 300;

function accepted(path: string): boolean {
  const ext = path.split(".").pop()?.toLowerCase() ?? "";
  return (EXTENSIONS as readonly string[]).includes(ext);
}

/** Dot files and dot folders are never indexed, so they are dropped before the upload. */
function hidden(path: string): boolean {
  return path.split("/").some((seg) => seg.startsWith("."));
}

export type Scan = { uploads: Upload[]; skipped: string[]; capped: boolean };

/** What the file picker hands back: a folder chosen there carries its shape in webkitRelativePath. */
export function scanFiles(files: File[]): Scan {
  const scan: Scan = { uploads: [], skipped: [], capped: false };
  for (const file of files) {
    const path = file.webkitRelativePath || file.name;
    if (hidden(path)) continue;
    if (!accepted(path)) {
      scan.skipped.push(path);
      continue;
    }
    if (scan.uploads.length >= MAX_FILES) {
      scan.capped = true;
      break;
    }
    scan.uploads.push({ file, path });
  }
  return scan;
}

function readAll(reader: FileSystemDirectoryReader): Promise<FileSystemEntry[]> {
  // readEntries hands back a page at a time and an empty page means the end.
  const out: FileSystemEntry[] = [];
  const next = (): Promise<FileSystemEntry[]> =>
    new Promise((resolve, reject) => reader.readEntries(resolve, reject)).then((batch) => {
      const page = batch as FileSystemEntry[];
      if (page.length === 0) return out;
      out.push(...page);
      return next();
    });
  return next();
}

async function walk(entry: FileSystemEntry, prefix: string, scan: Scan): Promise<void> {
  if (scan.capped) return;
  if (hidden(entry.name)) return;
  const path = prefix + entry.name;
  if (entry.isFile) {
    if (!accepted(path)) {
      scan.skipped.push(path);
      return;
    }
    if (scan.uploads.length >= MAX_FILES) {
      scan.capped = true;
      return;
    }
    const file = await new Promise<File>((resolve, reject) =>
      (entry as FileSystemFileEntry).file(resolve, reject),
    );
    scan.uploads.push({ file, path });
    return;
  }
  if (!entry.isDirectory) return;
  for (const child of await readAll((entry as FileSystemDirectoryEntry).createReader())) {
    await walk(child, path + "/", scan);
  }
}

/** A drop carrying folders. The entries have to be taken before the first await: the
 *  DataTransfer is emptied as soon as the drop handler returns. */
async function scanDrop(items: DataTransferItem[], files: File[]): Promise<Scan> {
  const entries = items.map((i) => i.webkitGetAsEntry?.() ?? null).filter((e): e is FileSystemEntry => e !== null);
  if (entries.length === 0) return scanFiles(files);
  const scan: Scan = { uploads: [], skipped: [], capped: false };
  for (const entry of entries) await walk(entry, "", scan);
  return scan;
}

/** Describes what a scan refused, for the caller to show. Empty when it took everything. */
export function scanNote(scan: Scan): string {
  const notes: string[] = [];
  if (scan.capped) notes.push(`Only the first ${MAX_FILES} files were taken.`);
  if (scan.skipped.length > 0) {
    const names = scan.skipped.slice(0, 3).join(", ");
    const more = scan.skipped.length > 3 ? ` and ${scan.skipped.length - 3} more` : "";
    notes.push(`Skipped ${names}${more} — not a supported type.`);
  }
  return notes.join(" ");
}

// Drag-and-drop over the whole page. There is no resting UI: the Upload button is the
// affordance, and this shows nothing at all until something is actually being dragged in.
export function useFileDrop(onScan: (scan: Scan) => void) {
  const [over, setOver] = useState(false);
  // dragenter and dragleave fire for every child the cursor crosses, so a boolean flickers as
  // the pointer moves over the table. A depth counter is what survives that.
  const depth = useRef(0);

  const onDragEnter = useCallback((e: React.DragEvent) => {
    // Text selections and internal drags also raise these events; only a drag carrying files
    // should light the page up.
    if (!Array.from(e.dataTransfer.types).includes("Files")) return;
    depth.current += 1;
    setOver(true);
  }, []);

  const onDragLeave = useCallback(() => {
    depth.current = Math.max(0, depth.current - 1);
    if (depth.current === 0) setOver(false);
  }, []);

  const onDragOver = useCallback((e: React.DragEvent) => {
    if (Array.from(e.dataTransfer.types).includes("Files")) e.preventDefault();
  }, []);

  const onDrop = useCallback(
    (e: React.DragEvent) => {
      e.preventDefault();
      depth.current = 0;
      setOver(false);
      scanDrop(Array.from(e.dataTransfer.items), Array.from(e.dataTransfer.files)).then(onScan);
    },
    [onScan],
  );

  return { over, dropProps: { onDragEnter, onDragLeave, onDragOver, onDrop } };
}

/** What a drag sees. Only rendered while one is in progress, so it costs nothing at rest. */
export function DropOverlay({ folder }: { folder: string }) {
  return (
    <div className="pointer-events-none fixed inset-0 z-50 flex items-center justify-center bg-background/70 backdrop-blur-[1px]">
      <div className="flex flex-col items-center gap-3 rounded-xl border-2 border-dashed border-primary bg-card px-10 py-8 shadow-lg">
        <UploadCloud className="size-7 text-primary" />
        <p className="text-sm font-medium">
          {folder ? `Drop into ${folder}` : "Drop to upload"}
        </p>
        <p className="text-xs text-muted-foreground">
          {EXTENSIONS.map((e) => `.${e}`).join(" ")} · up to 64 MB per batch
        </p>
      </div>
    </div>
  );
}
