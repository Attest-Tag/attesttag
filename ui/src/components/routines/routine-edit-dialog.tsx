"use client";

import { useEffect, useRef, useState } from "react";
import { Loader2, Maximize2, Minimize2 } from "lucide-react";
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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import { ChannelCombobox } from "@/components/core/channel-combobox";
import { SegmentedControl } from "@/components/core/segmented-control";
import { TimezoneCombobox } from "@/components/core/timezone-combobox";
import {
  api,
  errorMessage,
  useApi,
  type AdminUser,
  type Routine,
  type SettingsResponse,
} from "@/lib/api";

/**
 * The stored value for "follow the channel, then Settings" is "", which a Select cannot hold
 * as an item value, so the form carries this in its place and translates on the way out.
 */
const DEFAULT_MODEL = "__default__";

/** How a routine's model reads in the list: nothing for the default, a name otherwise. */
export function routineModelLabel(model: string): string {
  if (!model) return "";
  return model === "heavy" ? "Advanced model" : model;
}

// Cron, timezone, prompt, model and when it is allowed to speak, for one routine — an
// existing one, or "new" for one being set up here rather than by asking in Slack. The two are
// the same form because they are the same decisions; only where it is sent differs. Keyed on
// the routine so the form state resets when the dialog is pointed at another one.
export function RoutineEditDialog({
  routine,
  onOpenChange,
  onSaved,
}: {
  routine: Routine | "new" | null;
  onOpenChange: (open: boolean) => void;
  onSaved: () => void;
}) {
  const [container, setContainer] = useState<HTMLDivElement | null>(null);
  // A prompt can be a page long, and a page reads badly through a five-line window. Full
  // screen gives the prompt the whole viewport and hides the rest of the form until it is
  // done; the form state stays where it was, so nothing typed elsewhere is lost.
  const [fullScreen, setFullScreen] = useState(false);
  const close = (open: boolean) => {
    if (!open) setFullScreen(false);
    onOpenChange(open);
  };
  return (
    <Dialog open={routine !== null} onOpenChange={close}>
      <DialogContent
        className={
          fullScreen
            ? "flex h-dvh w-dvw max-w-none flex-col overflow-hidden rounded-none sm:max-w-none"
            : "flex h-[min(88dvh,780px)] flex-col overflow-hidden sm:max-w-2xl"
        }
        ref={setContainer}
        onEscapeKeyDown={(e) => {
          // Escape leaves full screen first; a second one closes the dialog as before.
          if (fullScreen) {
            e.preventDefault();
            setFullScreen(false);
          }
        }}
      >
        {routine && (
          <RoutineForm
            key={routine === "new" ? "new" : routine.ID}
            routine={routine === "new" ? null : routine}
            container={container}
            fullScreen={fullScreen}
            onFullScreen={setFullScreen}
            onOpenChange={close}
            onSaved={onSaved}
          />
        )}
      </DialogContent>
    </Dialog>
  );
}

function RoutineForm({
  routine,
  onOpenChange,
  onSaved,
  container,
  fullScreen,
  onFullScreen,
}: {
  /** The routine being edited, or null when this is a new one. */
  routine: Routine | null;
  onOpenChange: (open: boolean) => void;
  onSaved: () => void;
  container: HTMLDivElement | null;
  fullScreen: boolean;
  onFullScreen: (on: boolean) => void;
}) {
  const [channel, setChannel] = useState(routine?.Channel ?? "");
  const [cron, setCron] = useState(routine?.Cron ?? "");
  const [tz, setTz] = useState(routine?.TZ ?? "");
  const [prompt, setPrompt] = useState(routine?.Prompt ?? "");
  const [model, setModel] = useState(routine?.Model || "");
  const [notify, setNotify] = useState(routine?.Notify === "when_needed" ? "when_needed" : "always");
  const [notifyWhen, setNotifyWhen] = useState(routine?.NotifyWhen ?? "");
  // A new routine's writes run without asking, which is what a schedule needs: nobody is there to
  // press Confirm at six in the morning. One asked for in Slack starts on Ask first instead, since
  // what the model was told must not be what lets a routine write unattended.
  const [autoConfirm, setAutoConfirm] = useState(routine ? routine.AutoConfirm === true : true);
  const [busy, setBusy] = useState(false);
  const promptRef = useRef<HTMLTextAreaElement>(null);

  const opened = useRef(false);
  useEffect(() => {
    // Whichever way the prompt just moved, the cursor should still be in it. The exception is
    // the first render of a new routine: there the first thing to fill in is the channel, and
    // a cursor already three fields down hides that.
    if (!opened.current) {
      opened.current = true;
      if (!routine) return;
    }
    promptRef.current?.focus();
  }, [fullScreen, routine]);

  // The models a routine may pick from are the ones Settings offers to channels, plus the
  // default and the advanced model. The list is short by design — an admin named each entry —
  // so it is a plain dropdown rather than the searchable picker the console uses elsewhere.
  const settings = useApi<SettingsResponse>("/api/settings");
  const effective = settings.data?.effective;
  const offered = effective?.ChannelModels ?? [];
  const heavy = effective?.HeavyModel ?? "";
  // A model that was offered when this routine was set and has since been withdrawn stays
  // selectable, and says so, so the person editing sees what it runs on and can change it.
  const gone = model !== "" && model !== "heavy" && !offered.includes(model);

  // A routine acts as whoever last said what it does, spending the accounts that person
  // connected. So editing somebody else's moves it to you, and what stops is their reach
  // rather than what starts is yours — which is worth reading before saving, not after.
  // A console session signed in with a password has no Slack id at all (user_id is ""), and
  // that is the case where the notice matters most: the routine ends up running as nobody.
  const me = useApi<AdminUser>("/api/me");
  const myID = me.data?.user_id;
  const belongsToSomebodyElse = !!routine?.CreatedBy && myID !== undefined && routine.CreatedBy !== myID;

  // A new routine starts on the organisation's timezone rather than on nothing: "0 9 * * 1-5"
  // means a different morning in each, and Settings already holds the answer. Derived rather
  // than written into the field, so the form never has to chase its own state; an empty one
  // means the same thing to the API anyway.
  const timezone = routine ? tz : tz || (effective?.Timezone ?? "");

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!channel.trim()) {
      // Cron and the prompt are required fields the browser stops on; the channel picker is a
      // button, so it is checked here rather than making the round trip to be told the same.
      toast.error("Pick the channel it posts to");
      return;
    }
    if (!routine) {
      setBusy(true);
      try {
        await api.post("/api/routines", {
          channel,
          cron,
          tz: timezone,
          prompt,
          model,
          notify,
          notifyWhen,
          autoConfirm,
        });
        toast.success("Routine created");
        onOpenChange(false);
        onSaved();
      } catch (err) {
        toast.error(errorMessage(err));
      } finally {
        setBusy(false);
      }
      return;
    }
    const body: Record<string, unknown> = {};
    if (channel !== routine.Channel) body.channel = channel;
    if (cron !== routine.Cron) body.cron = cron;
    if (tz !== routine.TZ) body.tz = tz;
    if (prompt !== routine.Prompt) body.prompt = prompt;
    if (model !== (routine.Model || "")) body.model = model;
    if (notify !== routine.Notify) body.notify = notify;
    if (notifyWhen !== routine.NotifyWhen) body.notifyWhen = notifyWhen;
    if (autoConfirm !== (routine.AutoConfirm === true)) body.autoConfirm = autoConfirm;
    if (Object.keys(body).length === 0) {
      onOpenChange(false);
      return;
    }
    setBusy(true);
    try {
      // The server is what decides whether this edit took the routine over, so it is what
      // says so: the form cannot tell a prompt it rewrote from one it left alone.
      const saved = await api.put<{ runsAs?: string; wasRunningAs?: string }>(
        `/api/routines/${routine.ID}`,
        body,
      );
      if (saved?.wasRunningAs) {
        toast.success(saved.runsAs ? "Saved — this routine now runs as you" : "Saved — this routine now runs as nobody");
      } else {
        toast.success("Routine saved");
      }
      onOpenChange(false);
      onSaved();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const promptField = (
    <div className={fullScreen ? "flex min-h-0 flex-1 flex-col gap-1" : "space-y-1"}>
      <div className="flex items-center justify-between">
        <Label htmlFor="routine-prompt">Prompt</Label>
        <Button
          type="button"
          variant="ghost"
          size="xs"
          onClick={() => onFullScreen(!fullScreen)}
          aria-pressed={fullScreen}
        >
          {fullScreen ? <Minimize2 /> : <Maximize2 />}
          {fullScreen ? "Back to the form" : "Full screen"}
        </Button>
      </div>
      <Textarea
        ref={promptRef}
        id="routine-prompt"
        rows={5}
        className={fullScreen ? "min-h-0 flex-1 resize-none text-sm leading-6" : "max-h-96"}
        value={prompt}
        onChange={(e) => setPrompt(e.target.value)}
        required
      />
    </div>
  );

  return (
    <form onSubmit={submit} className="flex min-h-0 flex-1 flex-col gap-4">
      <DialogHeader>
        <DialogTitle>{routine ? `Edit routine #${routine.ID}` : "New routine"}</DialogTitle>
        <DialogDescription>
          {routine
            ? "Where it posts, when it runs, and what it does."
            : "A prompt on a schedule. It starts running as soon as it is created — pause it with the switch in the list."}
        </DialogDescription>
      </DialogHeader>
      {fullScreen ? (
        // Only the prompt, filling everything between the title and the buttons.
        <div className="flex min-h-0 flex-1 flex-col">{promptField}</div>
      ) : (
        /* A routine's prompt can be a page long, so the fields scroll inside a dialog of
           fixed height: the title stays put and Save stays where it was left. */
        <div className="min-h-0 flex-1 space-y-4 overflow-y-auto pr-1">
          {belongsToSomebodyElse && (
            <p className="text-xs text-amber-600 dark:text-amber-500">
              This routine runs as the person who set it up, using the accounts they connected.
              {myID
                ? " Changing what it does — its prompt, channel, or when it speaks — makes it run as you instead, and it stops reaching theirs."
                : " This session is not signed in through Slack, so changing what it does leaves it running as nobody: it stops reaching their accounts and reaches no others."}
            </p>
          )}
          <div className="space-y-1">
            <Label htmlFor="routine-channel">Channel</Label>
            <ChannelCombobox
              id="routine-channel"
              aria-label="Channel"
              value={channel}
              onChange={setChannel}
              container={container}
            />
            <p className="text-xs text-muted-foreground">
              Where each run posts. Any channel the bot is in; moving a routine moves its runs
              from the next one on.
            </p>
          </div>
          <div className="grid gap-3 sm:grid-cols-[1fr_12rem]">
            <div className="space-y-1">
              <Label htmlFor="routine-cron">Cron</Label>
              <Input
                id="routine-cron"
                value={cron}
                onChange={(e) => setCron(e.target.value)}
                className="font-mono"
                placeholder="0 9 * * 1-5"
                required
              />
              <p className="text-xs text-muted-foreground">Five fields: minute hour day month weekday.</p>
            </div>
            <div className="space-y-1">
              <Label htmlFor="routine-tz">Timezone</Label>
              <TimezoneCombobox
                id="routine-tz"
                value={timezone}
                onChange={setTz}
                emptyLabel="UTC (not set)"
                container={container}
              />
            </div>
          </div>
          {promptField}
          <div className="space-y-1">
            <Label htmlFor="routine-model">Model</Label>
            <Select
              value={model || DEFAULT_MODEL}
              onValueChange={(v) => setModel(v === DEFAULT_MODEL ? "" : v)}
              disabled={settings.loading && !settings.data}
            >
              <SelectTrigger id="routine-model" aria-label="Model" className="w-full max-w-sm">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={DEFAULT_MODEL}>
                  Default{effective?.Model ? ` (${effective.Model})` : ""}
                </SelectItem>
                {heavy && <SelectItem value="heavy">Advanced ({heavy})</SelectItem>}
                {offered.map((m) => (
                  <SelectItem key={m} value={m}>
                    {m}
                  </SelectItem>
                ))}
                {gone && <SelectItem value={model}>{model} (no longer offered)</SelectItem>}
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground">
              Which model answers each run. Default follows the channel&apos;s model, then
              Settings. The rest are the models offered to channels under Settings → Models.
            </p>
          </div>
          <div className="space-y-2">
            <Label>Reply to channel</Label>
            <SegmentedControl
              className="w-fit"
              value={notify}
              onValueChange={setNotify}
              options={[
                { value: "always", label: "Always" },
                { value: "when_needed", label: "Only when it matters" },
              ]}
            />
            {notify === "when_needed" ? (
              <div className="space-y-1 pt-1">
                <Label htmlFor="routine-notify-when">Post only when…</Label>
                <Textarea
                  id="routine-notify-when"
                  rows={2}
                  value={notifyWhen}
                  onChange={(e) => setNotifyWhen(e.target.value)}
                  placeholder="any VM is over 80% CPU, or has been up more than 30 days"
                />
                <p className="text-xs text-muted-foreground">
                  The bar it checks its own answer against. Runs that do not meet it stay out of
                  the channel — they are still recorded here, with what it found. Leave this
                  empty and it decides from the prompt alone.
                </p>
              </div>
            ) : (
              <p className="text-xs text-muted-foreground">Every run posts its answer to the channel.</p>
            )}
          </div>
          <div className="space-y-2">
            <Label>Writes</Label>
            <SegmentedControl
              className="w-fit"
              value={autoConfirm ? "auto" : "confirm"}
              onValueChange={(v) => setAutoConfirm(v === "auto")}
              options={[
                { value: "auto", label: "Run without asking" },
                { value: "confirm", label: "Ask first" },
              ]}
            />
            <p className="text-xs text-muted-foreground">
              {autoConfirm
                ? "The default for a routine: it changes things on its own, because nobody is there to press Confirm when it runs. Every write is still recorded, and the run log says how many went through. Connections that hand out access are the exception and still need a named approver."
                : "Writes post a Confirm card and wait for a person. On a schedule that usually means they expire unpressed, so pick this only for a routine that runs while someone is watching."}
            </p>
          </div>
        </div>
      )}
      <DialogFooter>
        <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
          Cancel
        </Button>
        <Button type="submit" disabled={busy}>
          {busy && <Loader2 className="animate-spin" />}
          {routine ? "Save" : "Create routine"}
        </Button>
      </DialogFooter>
    </form>
  );
}
