"use client";

import { Fragment, useState } from "react";
import { BookOpen, Check, Loader2, Pencil, Plug, Plus, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { SkillDialog, type SkillTarget } from "@/components/bundles/skill-dialog";
import { useConfirm } from "@/components/core/confirm-dialog";
import { RelativeTime } from "@/components/core/relative-time";
import { SearchField } from "@/components/core/search-field";
import { ServiceTile } from "@/components/core/service-mark";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { api, errorMessage, type Bundle, type Preset, type Skill } from "@/lib/api";

export type EditTab = "credentials" | "domains" | "instructions" | "tools" | "skills";

// Everything inside one bundle, on five tabs. Connecting a service is handed
// back to the page, which owns the Connect dialog so it can sit above this one.
export function BundleEditDialog({
  bundle,
  presets,
  initialTab,
  onOpenChange,
  onConnect,
  onChanged,
}: {
  bundle: Bundle | null;
  presets: Preset[];
  initialTab?: EditTab;
  onOpenChange: (open: boolean) => void;
  onConnect: (bundle: Bundle, preset: Preset) => void;
  onChanged: () => void;
}) {
  return (
    <Dialog open={bundle !== null} onOpenChange={onOpenChange}>
      <DialogContent className="flex h-[min(88dvh,700px)] flex-col overflow-hidden sm:max-w-2xl">
        {bundle && (
          <BundleEditor
            key={bundle.id}
            bundle={bundle}
            presets={presets}
            initialTab={initialTab ?? "credentials"}
            onConnect={onConnect}
            onChanged={onChanged}
          />
        )}
      </DialogContent>
    </Dialog>
  );
}

function BundleEditor({
  bundle,
  presets,
  initialTab,
  onConnect,
  onChanged,
}: {
  bundle: Bundle;
  presets: Preset[];
  initialTab: EditTab;
  onConnect: (bundle: Bundle, preset: Preset) => void;
  onChanged: () => void;
}) {
  const [tab, setTab] = useState<string>(initialTab);
  const [query, setQuery] = useState("");

  const connectedPresets = new Set((bundle.connections ?? []).map((c) => c.preset));

  const q = query.trim().toLowerCase();
  const hit = (...fields: string[]) => !q || fields.some((f) => f.toLowerCase().includes(q));

  // Two dozen services is past the length anyone scans, so the list is grouped
  // and filterable. The groups come out of the order the server sends — the
  // catalogue in internal/app/presets.go is already sorted by category — rather
  // than out of a second ordering kept here.
  const groups: [string, Preset[]][] = [];
  for (const p of presets) {
    if (p.id === "custom" || p.id === "mcp") continue;
    if (!hit(p.name, p.category, ...(p.hosts ?? []))) continue;
    const last = groups.at(-1);
    if (last && last[0] === p.category) last[1].push(p);
    else groups.push([p.category, [p]]);
  }

  // The two that name no vendor. They read as categories rather than brands, so
  // they get their own copy and an Add rather than a Connect, and they always
  // sit last: they are the answer to "my service isn't here".
  const house = [
    {
      preset: presets.find((p) => p.id === "custom"),
      label: "Custom tool",
      hint: "Any HTTP API — pick the credential type it uses.",
    },
    {
      preset: presets.find((p) => p.id === "mcp"),
      label: "Custom MCP server",
      hint: "A remote MCP server; its tools are offered to the model.",
    },
  ].filter((r): r is { preset: Preset; label: string; hint: string } => !!r.preset && hit(r.label, r.hint));

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-4">
      <DialogHeader>
        <DialogTitle>{bundle.name}</DialogTitle>
        <DialogDescription>
          {bundle.used_in === 0
            ? "Not attached to any scope yet — attach it under Workspaces once it has something in it."
            : `Attached in ${bundle.used_in} place${bundle.used_in === 1 ? "" : "s"}. Changes apply there straight away.`}
        </DialogDescription>
      </DialogHeader>

      <Tabs value={tab} onValueChange={setTab} className="flex min-h-0 flex-1 flex-col">
        <TabsList className="shrink-0 self-start">
          <TabsTrigger value="credentials">Credentials</TabsTrigger>
          <TabsTrigger value="domains">Domains</TabsTrigger>
          <TabsTrigger value="instructions">Instructions</TabsTrigger>
          <TabsTrigger value="tools">Tools</TabsTrigger>
          <TabsTrigger value="skills">Skills</TabsTrigger>
        </TabsList>

        <TabsContent value="credentials" className="min-h-0 flex-1 space-y-3 overflow-y-auto pr-1">
          <div className="flex items-center gap-3">
            <p className="min-w-0 flex-1 text-xs text-muted-foreground">
              Connect a service and the bot can call it from every scope this bundle is attached to.
            </p>
            <SearchField
              value={query}
              onChange={setQuery}
              placeholder="Find a service"
              className="w-48 shrink-0"
              clearable
            />
          </div>
          <ul className="divide-y overflow-hidden rounded-lg border">
            {groups.map(([category, items]) => (
              <Fragment key={category}>
                <li className="bg-muted/50 px-3 py-1.5 text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
                  {category}
                </li>
                {items.map((p) => {
                  const on = connectedPresets.has(p.id);
                  return (
                    <li key={p.id} className="flex h-12 items-center gap-3 px-3">
                      <ServiceTile preset={p.id} name={p.name} />
                      <span className="min-w-0 flex-1">
                        <span className="flex items-center gap-1.5 text-sm font-medium">
                          {p.name}
                          {on && <Check className="size-3.5 text-success" />}
                        </span>
                        <span className="block truncate font-mono text-xs text-muted-foreground">
                          {(p.hosts ?? []).join(", ")}
                        </span>
                      </span>
                      <Button variant={on ? "ghost" : "outline"} size="sm" onClick={() => onConnect(bundle, p)}>
                        <Plug className="size-4" />
                        {on ? "Connect another" : "Connect"}
                      </Button>
                    </li>
                  );
                })}
              </Fragment>
            ))}
            {house.length > 0 && (
              <li className="bg-muted/50 px-3 py-1.5 text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
                Anything else
              </li>
            )}
            {house.map((row) => (
              <li key={row.preset.id} className="flex h-12 items-center gap-3 px-3">
                <ServiceTile preset={row.preset.id} className="bg-secondary text-muted-foreground" />
                <span className="min-w-0 flex-1">
                  <span className="block text-sm font-medium">{row.label}</span>
                  <span className="block truncate text-xs text-muted-foreground">{row.hint}</span>
                </span>
                <Button variant="outline" size="sm" onClick={() => onConnect(bundle, row.preset)}>
                  <Plus className="size-4" />
                  Add
                </Button>
              </li>
            ))}
            {groups.length === 0 && house.length === 0 && (
              <li className="px-3 py-8 text-center text-sm text-muted-foreground">
                Nothing matches “{query.trim()}”. Anything with an HTTP API can still be connected as a
                custom tool.
              </li>
            )}
          </ul>
        </TabsContent>

        <TabsContent value="domains" className="min-h-0 flex-1 overflow-y-auto pr-1">
          <DomainsTab bundle={bundle} onChanged={onChanged} />
        </TabsContent>

        <TabsContent value="instructions" className="min-h-0 flex-1 overflow-y-auto pr-1">
          <InstructionsTab bundle={bundle} onChanged={onChanged} />
        </TabsContent>

        <TabsContent value="tools" className="min-h-0 flex-1 overflow-y-auto pr-1">
          <ToolsTab bundle={bundle} presets={presets} onChanged={onChanged} />
        </TabsContent>

        <TabsContent value="skills" className="min-h-0 flex-1 overflow-y-auto pr-1">
          <SkillsTab bundle={bundle} onChanged={onChanged} />
        </TabsContent>
      </Tabs>
    </div>
  );
}

// Hosts the bot may fetch without a credential — public docs, status pages.
function DomainsTab({ bundle, onChanged }: { bundle: Bundle; onChanged: () => void }) {
  const [host, setHost] = useState("");
  const [ports, setPorts] = useState("");
  const [busy, setBusy] = useState(false);
  const domains = bundle.domains ?? [];

  const add = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    try {
      await api.post(`/api/bundles/${bundle.id}/domains`, { host: host.trim(), ports: ports.trim() });
      toast.success("Domain added");
      setHost("");
      setPorts("");
      onChanged();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const remove = async (id: number, name: string) => {
    try {
      await api.del(`/api/domains/${id}`);
      toast.success(`Removed ${name}`);
      onChanged();
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  return (
    <div className="space-y-3">
      <p className="text-xs text-muted-foreground">
        Hosts the bot may reach with no credential attached — documentation sites, public APIs, status pages.
        A write to one still waits for someone in the thread to press Confirm.
      </p>
      {domains.length === 0 ? (
        <p className="rounded-lg border border-dashed bg-muted/40 px-3 py-4 text-center text-sm text-muted-foreground">
          No domains yet.
        </p>
      ) : (
        <ul className="divide-y overflow-hidden rounded-lg border">
          {domains.map((d) => (
            <li key={d.id} className="flex h-10 items-center gap-3 px-3">
              <span className="min-w-0 flex-1 truncate font-mono text-sm">{d.host}</span>
              <span className="text-xs text-muted-foreground">{d.ports ? `ports ${d.ports}` : "https"}</span>
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label={`Remove ${d.host}`}
                className="text-muted-foreground hover:text-destructive"
                onClick={() => remove(d.id, d.host)}
              >
                <Trash2 />
              </Button>
            </li>
          ))}
        </ul>
      )}
      <form onSubmit={add} className="grid gap-2 sm:grid-cols-[1fr_8rem_auto] sm:items-end">
        <div className="space-y-1">
          <Label htmlFor="domain-host">Host</Label>
          <Input
            id="domain-host"
            value={host}
            onChange={(e) => setHost(e.target.value)}
            placeholder="docs.example.com or *.example.com"
            className="font-mono"
            required
          />
        </div>
        <div className="space-y-1">
          <Label htmlFor="domain-ports">Ports</Label>
          <Input
            id="domain-ports"
            value={ports}
            onChange={(e) => setPorts(e.target.value)}
            placeholder="443"
            className="font-mono"
          />
        </div>
        <Button type="submit" disabled={busy || !host.trim()}>
          {busy ? <Loader2 className="animate-spin" /> : <Plus className="size-4" />}
          Add
        </Button>
      </form>
      <p className="text-xs text-muted-foreground">Wildcard only as the leftmost label. Ports default to 443.</p>
    </div>
  );
}

function InstructionsTab({ bundle, onChanged }: { bundle: Bundle; onChanged: () => void }) {
  const [text, setText] = useState(bundle.instructions);
  const [busy, setBusy] = useState(false);

  const save = async () => {
    setBusy(true);
    try {
      await api.put(`/api/bundles/${bundle.id}`, { instructions: text });
      toast.success("Instructions saved");
      onChanged();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-3">
      <div className="space-y-1">
        <Label htmlFor="bundle-instructions">Instructions</Label>
        <Textarea
          id="bundle-instructions"
          rows={8}
          value={text}
          onChange={(e) => setText(e.target.value)}
          placeholder="How to use these services well: which endpoints to prefer, what to double-check before a write, house conventions for ids."
        />
        <p className="text-xs text-muted-foreground">
          Added to the model&apos;s context wherever this bundle is attached.
        </p>
      </div>
      <div className="flex items-center gap-2">
        <Button onClick={save} disabled={busy || text === bundle.instructions}>
          {busy && <Loader2 className="animate-spin" />}
          Save instructions
        </Button>
        {text !== bundle.instructions && (
          <Button variant="ghost" onClick={() => setText(bundle.instructions)}>
            Discard
          </Button>
        )}
      </div>
    </div>
  );
}

// Tool packs: a switch per preset that ships one. Saved on each toggle.
function ToolsTab({
  bundle,
  presets,
  onChanged,
}: {
  bundle: Bundle;
  presets: Preset[];
  onChanged: () => void;
}) {
  const packs = presets.filter((p) => p.has_pack);
  const [saving, setSaving] = useState<string | null>(null);
  const enabled = new Set(bundle.tool_packs ?? []);

  const toggle = async (id: string, on: boolean) => {
    const next = new Set(enabled);
    if (on) next.add(id);
    else next.delete(id);
    setSaving(id);
    try {
      await api.put(`/api/bundles/${bundle.id}`, { tool_packs: [...next] });
      toast.success(on ? "Tool pack enabled" : "Tool pack disabled");
      onChanged();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setSaving(null);
    }
  };

  return (
    <div className="space-y-3">
      <p className="text-xs text-muted-foreground">
        A tool pack gives the model ready-made tools for a service instead of raw HTTP calls. It still needs a matching connection in this bundle.
      </p>
      <ul className="divide-y overflow-hidden rounded-lg border">
        {packs.map((p) => {
          const connected = (bundle.connections ?? []).some((c) => c.preset === p.id);
          return (
            <li key={p.id} className="flex h-12 items-center gap-3 px-3">
              <span className="min-w-0 flex-1">
                <Label htmlFor={`pack-${p.id}`} className="block text-sm font-medium">
                  {p.name} tools
                </Label>
                <span className="block text-xs text-muted-foreground">
                  {connected ? "Connection present" : "No connection in this bundle yet"}
                </span>
              </span>
              <Switch
                id={`pack-${p.id}`}
                checked={enabled.has(p.id)}
                disabled={saving === p.id}
                onCheckedChange={(v) => toggle(p.id, v)}
              />
            </li>
          );
        })}
      </ul>
    </div>
  );
}

// Skills: Markdown documents that ride along with the bundle. Toggled in
// place; the text is edited in its own dialog.
function SkillsTab({ bundle, onChanged }: { bundle: Bundle; onChanged: () => void }) {
  const skills = bundle.skills ?? [];
  const { confirm, confirmDialog } = useConfirm();
  const [target, setTarget] = useState<SkillTarget | null>(null);
  const [saving, setSaving] = useState<number | null>(null);

  const toggle = async (skill: Skill, enabled: boolean) => {
    setSaving(skill.id);
    try {
      await api.put(`/api/skills/${skill.id}`, { name: skill.name, content: skill.content, enabled });
      toast.success(enabled ? `${skill.name} enabled` : `${skill.name} disabled`);
      onChanged();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setSaving(null);
    }
  };

  const remove = async (skill: Skill) => {
    const ok = await confirm({
      title: `Delete ${skill.name}?`,
      description: "The assistant stops reading it everywhere this bundle is attached. This can't be undone.",
      confirmLabel: "Delete",
      destructive: true,
    });
    if (!ok) return;
    try {
      await api.del(`/api/skills/${skill.id}`);
      toast.success(`Deleted ${skill.name}`);
      onChanged();
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  return (
    <div className="space-y-3">
      <p className="text-xs text-muted-foreground">
        Skills are Markdown instructions that travel with this bundle: how to use a tool well, house
        conventions, runbooks. They&apos;re added to the assistant&apos;s instructions in every channel
        this bundle covers.
      </p>
      {skills.length === 0 ? (
        <p className="rounded-lg border border-dashed bg-muted/40 px-3 py-4 text-center text-sm text-muted-foreground">
          No skills yet.
        </p>
      ) : (
        <ul className="divide-y overflow-hidden rounded-lg border">
          {skills.map((s) => (
            <li key={s.id} className="flex h-12 items-center gap-3 px-3">
              <span className="flex size-8 shrink-0 items-center justify-center rounded-md bg-secondary text-muted-foreground">
                <BookOpen className="size-4" />
              </span>
              <span className="min-w-0 flex-1">
                <Label htmlFor={`skill-${s.id}`} className="block truncate text-sm font-medium">
                  {s.name}
                </Label>
                <span className="block text-xs text-muted-foreground">
                  {s.updated_at ? (
                    <>
                      Updated <RelativeTime value={s.updated_at} />
                    </>
                  ) : (
                    "Never updated"
                  )}
                </span>
              </span>
              <Switch
                id={`skill-${s.id}`}
                checked={s.enabled}
                disabled={saving === s.id}
                onCheckedChange={(v) => toggle(s, v)}
              />
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label={`Edit ${s.name}`}
                onClick={() => setTarget({ bundleId: bundle.id, skill: s })}
              >
                <Pencil />
              </Button>
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label={`Delete ${s.name}`}
                className="text-muted-foreground hover:text-destructive"
                onClick={() => remove(s)}
              >
                <Trash2 />
              </Button>
            </li>
          ))}
        </ul>
      )}
      <Button variant="outline" size="sm" onClick={() => setTarget({ bundleId: bundle.id })}>
        <Plus className="size-4" />
        Add skill
      </Button>
      <SkillDialog
        target={target}
        onOpenChange={(open) => !open && setTarget(null)}
        onSaved={onChanged}
      />
      {confirmDialog}
    </div>
  );
}
