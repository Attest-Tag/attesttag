"use client";

import { useState } from "react";
import { KeyRound, Plus, Terminal, TriangleAlert } from "lucide-react";
import { toast } from "sonner";
import Link from "next/link";
import { useConfirm } from "@/components/core/confirm-dialog";
import { CopyButton } from "@/components/core/copy-button";
import { EmptyState } from "@/components/core/empty-state";
import { ErrorBanner } from "@/components/core/error-banner";
import { PageHeader } from "@/components/core/page-header";
import { RelativeTime } from "@/components/core/relative-time";
import { StatusChip, type StatusChipVariant } from "@/components/core/status-chip";
import { TableSkeleton } from "@/components/core/table-skeleton";
import { useAuth } from "@/components/shell/auth-provider";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
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
  errorMessage,
  useApi,
  type ApiKey,
  type ApiKeyCreated,
  type ApiKeysResponse,
} from "@/lib/api";
import { formatDate, parseTime } from "@/lib/format";

const NEVER = "never";
const CUSTOM = "custom";
const PRESETS = [
  { value: "30", label: "In 30 days" },
  { value: "60", label: "In 60 days" },
  { value: "90", label: "In 90 days" },
];

/** Today plus n days, as the "YYYY-MM-DD" the API takes. */
function inDays(days: number): string {
  const d = new Date();
  d.setDate(d.getDate() + days);
  return isoDay(d);
}

function isoDay(d: Date): string {
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
}

/**
 * A bare "YYYY-MM-DD" as a readable date. Not `formatDate` — that reads the string through the
 * Date constructor, which takes a date with no time as UTC midnight and so renders the day
 * before anywhere west of Greenwich. The picker's value is a local day and has to stay one.
 */
function formatDay(iso: string): string {
  const [y, m, d] = iso.split("-").map(Number);
  if (!y || !m || !d) return iso;
  return formatDate(new Date(y, m - 1, d));
}

/**
 * What a key is right now, in one word. Expiry is decided here against the browser's clock
 * rather than by the server: a key that lapses while this page sits open reads as active until
 * the next load, which is cosmetic — the API refuses it either way.
 */
function keyState(k: ApiKey): { label: string; variant: StatusChipVariant } {
  if (k.revoked_at) return { label: "Revoked", variant: "neutral" };
  const at = k.expires_at ? parseTime(k.expires_at) : null;
  if (at && at.getTime() <= Date.now()) return { label: "Expired", variant: "danger" };
  return { label: "Active", variant: "success" };
}

/**
 * The keys an organisation has minted, and the one moment a new one is legible.
 *
 * The reveal is a banner rather than a modal on purpose: a modal that holds the only copy of a
 * credential is one stray Escape away from a key nobody has. This one stays until it is
 * dismissed, and says plainly that it will not come back.
 */
export function ApiKeysPanel() {
  const keys = useApi<ApiKeysResponse>("/api/api-keys");
  const { me } = useAuth();
  const { confirm, confirmDialog } = useConfirm();

  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [expiry, setExpiry] = useState(NEVER);
  const [customDay, setCustomDay] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [revealed, setRevealed] = useState<ApiKeyCreated | null>(null);

  const rows = keys.data?.keys ?? [];
  const perms = me?.user?.permissions;
  const canManage = !perms || perms["api_keys.manage"] === true;

  const expiresOn = expiry === NEVER ? "" : expiry === CUSTOM ? customDay : inDays(Number(expiry));
  const canSubmit = !busy && name.trim() !== "" && !(expiry === CUSTOM && !customDay);

  const openDialog = () => {
    setName("");
    setExpiry(NEVER);
    setCustomDay("");
    setError("");
    setOpen(true);
  };

  const create = async () => {
    setBusy(true);
    setError("");
    try {
      const made = await api.post<ApiKeyCreated>("/api/api-keys", {
        name: name.trim(),
        expires_on: expiresOn,
      });
      setRevealed(made);
      setOpen(false);
      keys.reload();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const revoke = async (k: ApiKey) => {
    const ok = await confirm({
      title: `Revoke ${k.name}?`,
      description:
        "Anything still using this key stops working immediately, and it cannot be turned back on. Issue a new key to replace it.",
      confirmLabel: "Revoke",
      destructive: true,
    });
    if (!ok) return;
    try {
      await api.del(`/api/api-keys/${k.id}`);
      toast.success("Key revoked");
      keys.reload();
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  const createButton = canManage ? (
    <Button onClick={openDialog}>
      <Plus className="size-4" /> Create key
    </Button>
  ) : null;

  return (
    <div className="space-y-5">
      <PageHeader
        title="API keys"
        description={
          <>
            Credentials for the <code className="rounded bg-secondary px-1">/v1</code> API and the
            MCP server. A key carries the access of the person who made it — never more — and is
            shown once, when it is made. See the{" "}
            <Link href="/developer/api-reference" className="text-primary hover:underline">
              API reference
            </Link>{" "}
            for what it can reach, and{" "}
            <Link href="/developer/mcp" className="text-primary hover:underline">
              MCP
            </Link>{" "}
            to use it from Claude or Cursor.
          </>
        }
        actions={createButton}
      />

      {keys.error && !keys.data && <ErrorBanner message={keys.error} onRetry={keys.reload} />}

      {revealed && (
        <Card className="gap-3 border-primary/40 bg-accent/40 p-4">
          <div className="flex items-start gap-2">
            <TriangleAlert className="mt-0.5 size-4 shrink-0 text-warning" />
            <div className="min-w-0 space-y-2">
              <p className="text-sm font-medium">
                Copy {revealed.key.name} now — this is the only time it is shown.
              </p>
              <div className="flex items-center gap-1">
                <code className="min-w-0 flex-1 truncate rounded bg-card px-2 py-1.5 font-mono text-xs">
                  {revealed.raw}
                </code>
                <CopyButton text={revealed.raw} label="Copy key" />
              </div>
              <p className="text-xs text-muted-foreground">
                Only a hash is stored, so it cannot be shown again. If it is lost, revoke it and
                make another.
              </p>
            </div>
          </div>
          <div>
            <Button variant="outline" size="sm" onClick={() => setRevealed(null)}>
              I&apos;ve saved it
            </Button>
          </div>
        </Card>
      )}

      {!keys.data && !keys.error && <TableSkeleton rows={3} />}

      {keys.data && rows.length === 0 && (
        <EmptyState
          icon={Terminal}
          title="No API keys yet"
          description="A key lets a script or an MCP client read what the bot knows, keep its documents in step with somewhere else, make routines, and see what it spent — with exactly the access you have yourself."
          action={createButton ?? undefined}
        />
      )}

      {rows.length > 0 && (
        <Card className="overflow-hidden p-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Key</TableHead>
                <TableHead>Acts as</TableHead>
                <TableHead>Last used</TableHead>
                <TableHead>Expires</TableHead>
                <TableHead>Status</TableHead>
                <TableHead className="w-0" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((k) => {
                const state = keyState(k);
                return (
                  <TableRow key={k.id}>
                    <TableCell className="font-medium">{k.name}</TableCell>
                    <TableCell>
                      <code className="rounded bg-secondary px-1.5 py-0.5 font-mono text-xs text-muted-foreground">
                        {k.prefix}…
                      </code>
                    </TableCell>
                    <TableCell className="text-muted-foreground">{k.owner || "—"}</TableCell>
                    <TableCell className="text-muted-foreground">
                      {k.last_used_at ? <RelativeTime value={k.last_used_at} /> : "Never"}
                    </TableCell>
                    <TableCell className="text-muted-foreground">
                      {k.expires_at ? formatDate(k.expires_at) : "Never"}
                    </TableCell>
                    <TableCell>
                      <StatusChip variant={state.variant}>{state.label}</StatusChip>
                    </TableCell>
                    <TableCell className="text-right">
                      {canManage && !k.revoked_at && (
                        <Button variant="ghost" size="sm" onClick={() => revoke(k)}>
                          Revoke
                        </Button>
                      )}
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        </Card>
      )}

      {keys.data && (
        <p className="text-xs text-muted-foreground">
          Every key is limited to {keys.data.rate_limit_per_minute} requests a minute. Keys stop
          working when the person who made them leaves the organisation.
        </p>
      )}

      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent>
          <form
            onSubmit={(e) => {
              e.preventDefault();
              if (canSubmit) create();
            }}
          >
            <DialogHeader>
              <DialogTitle>Create an API key</DialogTitle>
              <DialogDescription>
                It will act as you, with your permissions, until you revoke it.
              </DialogDescription>
            </DialogHeader>
            <div className="space-y-4 py-4">
              <div className="space-y-2">
                <Label htmlFor="key-name">Name</Label>
                <Input
                  id="key-name"
                  autoFocus
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  placeholder="Nightly document sync"
                />
                <p className="text-xs text-muted-foreground">
                  What it is for. This is what you will read when deciding whether to revoke it.
                </p>
              </div>
              <div className="space-y-2">
                <Label htmlFor="key-expiry">Expires</Label>
                <Select value={expiry} onValueChange={setExpiry}>
                  <SelectTrigger id="key-expiry" className="w-full">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value={NEVER}>Never</SelectItem>
                    {PRESETS.map((p) => (
                      <SelectItem key={p.value} value={p.value}>
                        {p.label}
                      </SelectItem>
                    ))}
                    <SelectItem value={CUSTOM}>On a date…</SelectItem>
                  </SelectContent>
                </Select>
                {expiry === CUSTOM && (
                  <Input
                    type="date"
                    value={customDay}
                    min={inDays(1)}
                    onChange={(e) => setCustomDay(e.target.value)}
                  />
                )}
                {expiry !== NEVER && expiresOn && (
                  <p className="text-xs text-muted-foreground">
                    Stops working at the end of {formatDay(expiresOn)}.
                  </p>
                )}
              </div>
              {error && <p className="text-sm text-danger">{error}</p>}
            </div>
            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => setOpen(false)}>
                Cancel
              </Button>
              <Button type="submit" disabled={!canSubmit}>
                <KeyRound className="size-4" /> Create key
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      {confirmDialog}
    </div>
  );
}
