"use client";

import { useState } from "react";
import { Loader2, ShieldCheck } from "lucide-react";
import { toast } from "sonner";
import {
  EMPTY_SECRET,
  headerValuesForWire,
  secretComplete,
  secretForWire,
  type ConnectionForm,
} from "@/components/bundles/connection-form";
import { SecretFields } from "@/components/bundles/secret-fields";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { api, errorMessage, type Connection, type CredType, type Preset } from "@/lib/api";

// Swap the credential on an existing connection without touching anything
// else: PUT /api/connections/{id} with only `secret` (and extra header values).
export function RotateSecretDialog({
  connection,
  presets,
  onOpenChange,
  onSaved,
}: {
  connection: Connection | null;
  presets: Preset[];
  onOpenChange: (open: boolean) => void;
  onSaved: () => void;
}) {
  return (
    <Dialog open={connection !== null} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        {connection && (
          <RotateForm
            key={connection.id}
            connection={connection}
            preset={presets.find((p) => p.id === connection.preset)}
            onOpenChange={onOpenChange}
            onSaved={onSaved}
          />
        )}
      </DialogContent>
    </Dialog>
  );
}

function RotateForm({
  connection,
  preset,
  onOpenChange,
  onSaved,
}: {
  connection: Connection;
  preset: Preset | undefined;
  onOpenChange: (open: boolean) => void;
  onSaved: () => void;
}) {
  const credType = (connection.cred_type as CredType) || "bearer";
  const header = connection.headers?.[0];
  const [secret, setSecret] = useState<ConnectionForm["secret"]>({
    ...EMPTY_SECRET,
    headerName: header?.name ?? preset?.header_name ?? "",
    headerPrefix: header?.prefix ?? preset?.header_prefix ?? "",
    scopes: preset?.scopes ?? "",
  });
  const [headerValues, setHeaderValues] = useState<Record<string, string>>(
    Object.fromEntries((preset?.extra_headers ?? []).map((h) => [h.name, ""])),
  );
  const [busy, setBusy] = useState(false);

  // Only the credential fields are read from this partial form.
  const form = { credType, secret, headerValues } as ConnectionForm;
  const ready = secretComplete(form);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    try {
      const body: Record<string, unknown> = { secret: secretForWire(form) };
      const hv = headerValuesForWire(form);
      if (hv) body.header_values = hv;
      await api.put(`/api/connections/${connection.id}`, body);
      toast.success("Secret rotated");
      onOpenChange(false);
      onSaved();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form onSubmit={submit} className="space-y-4">
      <DialogHeader>
        <DialogTitle>Rotate secret for {connection.name}</DialogTitle>
        <DialogDescription>
          The new credential replaces the old one immediately. Hosts, paths and notes stay as they are.
        </DialogDescription>
      </DialogHeader>
      <SecretFields
        idPrefix="rotate"
        credType={credType}
        value={secret}
        onChange={setSecret}
        preset={preset}
        headerValues={headerValues}
        onHeaderValuesChange={setHeaderValues}
        placeholder={preset?.placeholder}
      />
      <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
        <ShieldCheck className="size-3.5" />
        Stored securely and never shown again after saving.
      </p>
      <DialogFooter>
        <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
          Cancel
        </Button>
        <Button type="submit" disabled={busy || !ready}>
          {busy && <Loader2 className="animate-spin" />}
          Rotate
        </Button>
      </DialogFooter>
    </form>
  );
}
