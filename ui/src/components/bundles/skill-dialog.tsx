"use client";

import { useRef, useState } from "react";
import { FileUp, Loader2 } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
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
import { Textarea } from "@/components/ui/textarea";
import { api, errorMessage, type Skill } from "@/lib/api";

export type SkillTarget = {
  bundleId: number;
  /** Present when editing; absent when adding. */
  skill?: Skill;
};

// Add or edit one skill: a name and its Markdown body. The body is keyed on
// the target so switching skills starts from that skill's text.
export function SkillDialog({
  target,
  onOpenChange,
  onSaved,
}: {
  target: SkillTarget | null;
  onOpenChange: (open: boolean) => void;
  onSaved: () => void;
}) {
  return (
    <Dialog open={target !== null} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-2xl">
        {target && (
          <SkillForm
            key={target.skill?.id ?? "new"}
            target={target}
            onOpenChange={onOpenChange}
            onSaved={onSaved}
          />
        )}
      </DialogContent>
    </Dialog>
  );
}

function SkillForm({
  target,
  onOpenChange,
  onSaved,
}: {
  target: SkillTarget;
  onOpenChange: (open: boolean) => void;
  onSaved: () => void;
}) {
  const { bundleId, skill } = target;
  const editing = !!skill;
  const [name, setName] = useState(skill?.name ?? "");
  const [content, setContent] = useState(skill?.content ?? "");
  const [busy, setBusy] = useState(false);
  const fileRef = useRef<HTMLInputElement>(null);

  const canSave = name.trim() !== "" && content.trim() !== "";

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!canSave) return;
    setBusy(true);
    try {
      if (editing) {
        await api.put(`/api/skills/${skill.id}`, {
          name: name.trim(),
          content,
          enabled: skill.enabled,
        });
      } else {
        await api.post(`/api/bundles/${bundleId}/skills`, {
          name: name.trim(),
          content,
          enabled: true,
        });
      }
      toast.success(editing ? "Skill saved" : "Skill added");
      onOpenChange(false);
      onSaved();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  // Read a .md file in the browser and drop it into the textarea. The name
  // is filled from the file name only when it is still empty.
  const importFile = (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    e.target.value = "";
    if (!file) return;
    const reader = new FileReader();
    reader.onload = () => {
      setContent(String(reader.result ?? ""));
      if (!name.trim()) setName(file.name.replace(/\.(md|markdown)$/i, ""));
      toast.success(`Imported ${file.name}`);
    };
    reader.onerror = () => toast.error(`Could not read ${file.name}`);
    reader.readAsText(file);
  };

  return (
    <form onSubmit={submit} className="space-y-4">
      <DialogHeader>
        <DialogTitle>{editing ? `Edit ${skill.name}` : "Add skill"}</DialogTitle>
        <DialogDescription>
          Markdown the assistant reads wherever this bundle is attached — how to use a tool
          well, house conventions, a runbook.
        </DialogDescription>
      </DialogHeader>

      <div className="space-y-1">
        <Label htmlFor="skill-name">Name</Label>
        <Input
          id="skill-name"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="Deploy runbook"
          autoFocus={!editing}
        />
      </div>

      <div className="space-y-1">
        <div className="flex items-center justify-between gap-3">
          <Label htmlFor="skill-content">Content</Label>
          <input
            ref={fileRef}
            type="file"
            accept=".md,.markdown,text/markdown,text/plain"
            className="hidden"
            onChange={importFile}
          />
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="h-7 text-xs"
            onClick={() => fileRef.current?.click()}
          >
            <FileUp className="size-3.5" />
            Import .md
          </Button>
        </div>
        <Textarea
          id="skill-content"
          value={content}
          onChange={(e) => setContent(e.target.value)}
          spellCheck={false}
          placeholder={"# When to use\n\n- …\n\n# Steps\n\n1. …"}
          className="min-h-[280px] font-mono text-xs leading-relaxed [field-sizing:fixed]"
        />
        <p className="text-xs text-muted-foreground">
          Plain Markdown. Headings and lists help the model find the right part.
        </p>
      </div>

      <DialogFooter>
        <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
          Cancel
        </Button>
        <Button type="submit" disabled={busy || !canSave}>
          {busy && <Loader2 className="animate-spin" />}
          Save
        </Button>
      </DialogFooter>
    </form>
  );
}
