"use client";

import { useMemo, useState } from "react";
import { Link2, Mail, Trash2, UserPlus, Users, X } from "lucide-react";
import { toast } from "sonner";
import { SlackGlyph } from "@/components/auth/auth-layout";
import { useConfirm } from "@/components/core/confirm-dialog";
import { CopyButton } from "@/components/core/copy-button";
import { EmptyState } from "@/components/core/empty-state";
import { ErrorBanner } from "@/components/core/error-banner";
import { RelativeTime } from "@/components/core/relative-time";
import { SearchField } from "@/components/core/search-field";
import { SegmentedControl } from "@/components/core/segmented-control";
import { StatusChip } from "@/components/core/status-chip";
import { TableSkeleton } from "@/components/core/table-skeleton";
import { useAuth } from "@/components/shell/auth-provider";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
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
  type ConsoleInvite,
  type ConsoleRole,
  type ConsoleRolesResponse,
  type ConsoleUser,
  type InviteResult,
} from "@/lib/api";
import { parseTime } from "@/lib/format";

/**
 * "in 6 days" for a moment still ahead. `RelativeTime` counts backwards from now and renders an
 * expiry that has not happened yet as "just now", which reads as the opposite of what it means.
 */
function expiresIn(value: string): string {
  const at = parseTime(value);
  if (!at) return "";
  const ms = at.getTime() - Date.now();
  if (ms <= 0) return "expired";
  const hours = Math.round(ms / 3_600_000);
  if (hours < 1) return "expires within the hour";
  if (hours < 48) return `expires in ${hours} hour${hours === 1 ? "" : "s"}`;
  const days = Math.round(hours / 24);
  return `expires in ${days} days`;
}

function initials(name: string): string {
  const letters = name
    .split(/[\s._-]+/)
    .map((p) => p[0])
    .filter((c) => /[a-z0-9]/i.test(c ?? ""))
    .slice(0, 2)
    .join("");
  return (letters || name.slice(0, 2)).toUpperCase();
}

/**
 * Who can open the console, and as what.
 *
 * The role cell is the whole point of the screen, so it says what a role *is* — its label and how
 * much it carries — rather than repeating the key the API stores. Roles that hold more than the
 * signed-in person does are shown but not selectable: the server refuses those, and a select that
 * offers a choice it will then reject is worse than one that explains why it can't.
 */
export function ConsoleUsersPanel() {
  const users = useApi<ConsoleUser[]>("/api/console/users");
  const roles = useApi<ConsoleRolesResponse>("/api/console/roles");
  const invites = useApi<ConsoleInvite[]>("/api/console/invites");
  const { me } = useAuth();
  const [q, setQ] = useState("");

  const roleList = useMemo(() => roles.data?.roles ?? [], [roles.data]);
  const total = (roles.data?.all_permissions ?? []).length;
  const byKey = useMemo(() => {
    const m = new Map<string, ConsoleRole>();
    for (const r of roleList) m.set(r.key, r);
    return m;
  }, [roleList]);

  // A role you can hand out is one whose every permission you hold. A session that reports no
  // permission map at all is left alone — the server still decides.
  const mine = me?.user?.permissions;
  // Inviting, like removing, needs users.manage — and the invite list itself 403s without it, so
  // the card would sit there offering an action that always fails.
  const canInvite = !mine || mine["users.manage"] === true;
  const assignable = (key: string) => {
    if (!mine) return true;
    const r = byKey.get(key);
    if (!r) return false;
    return (r.permissions ?? []).every((p) => mine[p] === true);
  };

  const all = users.data ?? [];
  const needle = q.trim().toLowerCase();
  const shown = needle
    ? all.filter((u) =>
        [u.name, u.email, u.role].some((f) =>
          (f ?? "").toLowerCase().includes(needle),
        ),
      )
    : all;

  return (
    <div className="space-y-5">
      {users.error && !users.data && <ErrorBanner message={users.error} onRetry={users.reload} />}

      <Card className="gap-0 py-0">
        <div className="flex flex-wrap items-start justify-between gap-3 p-4">
          <div className="max-w-2xl">
            <h3 className="text-sm font-semibold">
              Who can open the console
              {all.length > 0 && (
                <span className="ml-2 font-normal text-muted-foreground">{all.length}</span>
              )}
            </h3>
            <p className="mt-0.5 text-xs leading-relaxed text-muted-foreground">
              Everyone here was invited. Signing up — with a password or with Slack — founds a new
              organisation rather than joining this one, so an invitation is the only way in, and
              the role it carries decides what they see. Anyone you add gets the console for
              <span className="font-medium"> every</span> workspace this organisation has connected,
              not only their own.
            </p>
          </div>
          {all.length > 5 && (
            <SearchField
              value={q}
              onChange={setQ}
              clearable
              placeholder="Search people"
              className="w-full sm:w-56"
            />
          )}
        </div>

        {users.loading && !users.data ? (
          <div className="border-t p-4">
            <TableSkeleton rows={3} />
          </div>
        ) : all.length === 0 ? (
          <div className="border-t p-4">
            <EmptyState
              icon={Users}
              title="Nobody has signed in yet"
              description="You're the first. Anyone you invite below shows up here once they accept, along with the role they hold."
              className="py-12"
            />
          </div>
        ) : shown.length === 0 ? (
          <p className="border-t p-8 text-center text-sm text-muted-foreground">
            Nobody matches “{q}”.
          </p>
        ) : (
          <div className="border-t">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Person</TableHead>
                  <TableHead className="w-44">Role</TableHead>
                  <TableHead className="w-40">Can do</TableHead>
                  <TableHead className="w-28">Last seen</TableHead>
                  <TableHead className="w-10" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {shown.map((u) => (
                  <UserRow
                    key={u.id}
                    user={u}
                    roles={roleList}
                    total={total}
                    assignable={assignable}
                    onChanged={users.reload}
                  />
                ))}
              </TableBody>
            </Table>
          </div>
        )}
      </Card>

      {canInvite && <InviteCard roles={roleList} assignable={assignable} invites={invites} />}
    </div>
  );
}

function UserRow({
  user,
  roles,
  total,
  assignable,
  onChanged,
}: {
  user: ConsoleUser;
  roles: ConsoleRole[];
  total: number;
  assignable: (key: string) => boolean;
  onChanged: () => void;
}) {
  const { confirm, confirmDialog } = useConfirm();
  const who = user.name || user.email;
  // You can only change — or remove — someone whose access you already hold yourself.
  const locked = !assignable(user.role);
  const perms = (roles.find((r) => r.key === user.role)?.permissions ?? user.permissions ?? [])
    .length;

  const setRole = async (role: string) => {
    try {
      await api.put(`/api/console/users/${user.id}/role`, { role });
      toast.success(`${who} is now ${roles.find((r) => r.key === role)?.label ?? role}`);
      onChanged();
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  const remove = async () => {
    const ok = await confirm({
      title: `Remove ${who}?`,
      description: "They lose access to the console. Nothing they set up is changed.",
      confirmLabel: "Remove",
      destructive: true,
    });
    if (!ok) return;
    try {
      await api.del(`/api/console/users/${user.id}`);
      toast.success(`${who} removed`);
      onChanged();
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  return (
    <TableRow>
      <TableCell>
        <div className="flex items-center gap-3">
          <span
            aria-hidden
            className="flex size-8 shrink-0 items-center justify-center rounded-full bg-accent text-[11px] font-medium text-accent-foreground"
          >
            {initials(who)}
          </span>
          <div className="min-w-0">
            <div className="flex items-center gap-1.5 text-sm font-medium">
              <span className="truncate">{who}</span>
              {user.is_you && (
                <span className="rounded bg-secondary px-1.5 text-[10px] text-muted-foreground">
                  you
                </span>
              )}
            </div>
            <div className="truncate text-xs text-muted-foreground">
              {user.email}
            </div>
          </div>
        </div>
      </TableCell>

      <TableCell>
        <Select value={user.role} onValueChange={setRole} disabled={locked}>
          <SelectTrigger
            className="w-full"
            aria-label={`Role for ${who}`}
            title={locked ? "Their role holds access you don't have yourself" : undefined}
          >
            <SelectValue placeholder={user.role} />
          </SelectTrigger>
          <SelectContent>
            {roles.map((r) => (
              <SelectItem key={r.key} value={r.key} disabled={!assignable(r.key)}>
                {r.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </TableCell>

      <TableCell>
        {total > 0 && perms >= total ? (
          <StatusChip variant="success">everything</StatusChip>
        ) : perms === 0 ? (
          <span className="text-xs text-muted-foreground">nothing</span>
        ) : (
          <span className="text-xs text-muted-foreground">
            {perms} of {total} permissions
          </span>
        )}
      </TableCell>

      <TableCell className="text-xs text-muted-foreground">
        {user.last_seen ? (
          <RelativeTime value={user.last_seen} />
        ) : (
          <span className="text-muted-foreground/70">never</span>
        )}
      </TableCell>

      <TableCell>
        {!user.is_you && !locked && (
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label={`Remove ${who}`}
            onClick={remove}
          >
            <Trash2 />
          </Button>
        )}
      </TableCell>
      {confirmDialog}
    </TableRow>
  );
}

function InviteCard({
  roles,
  assignable,
  invites,
}: {
  roles: ConsoleRole[];
  assignable: (key: string) => boolean;
  invites: ReturnType<typeof useApi<ConsoleInvite[]>>;
}) {
  const [mode, setMode] = useState<"person" | "link">("person");
  const pending = invites.data ?? [];

  return (
    <Card className="gap-0 py-0">
      <div className="space-y-4 p-4">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="max-w-2xl">
            <h3 className="text-sm font-semibold">Invite someone</h3>
            <p className="mt-0.5 text-xs leading-relaxed text-muted-foreground">
              {mode === "person"
                ? "Give a Slack user id and the bot sends them the link directly; give an email and we send it there. Either way it is theirs alone, single-use, and expires in seven days."
                : "A link with nobody’s name on it: anyone who opens it joins at the role you pick. Bound by how long it lives, how many people it lets in, and — if you want one — the email domain it accepts."}
            </p>
          </div>
          <SegmentedControl
            value={mode}
            onValueChange={setMode}
            options={[
              { value: "person", label: "One person", icon: <UserPlus className="size-3.5" /> },
              { value: "link", label: "Shareable link", icon: <Link2 className="size-3.5" /> },
            ]}
          />
        </div>

        {mode === "person" ? (
          <InvitePersonForm roles={roles} assignable={assignable} onCreated={invites.reload} />
        ) : (
          <InviteLinkForm roles={roles} assignable={assignable} onCreated={invites.reload} />
        )}
      </div>

      {pending.length > 0 && (
        <div className="border-t">
          <Label className="block px-4 pt-3 text-xs text-muted-foreground">
            Waiting to be accepted
          </Label>
          <div className="divide-y">
            {pending.map((i) => (
              <PendingRow
                key={i.id}
                invite={i}
                roles={roles}
                onChanged={invites.reload}
              />
            ))}
          </div>
        </div>
      )}
    </Card>
  );
}

/**
 * The link an invitation produced, offered to copy.
 *
 * It appears whenever there is a link worth passing on by hand: always for a share link, and for
 * a personal invitation whenever delivery failed — because the invitation is already written, and
 * losing the only copy of it because an email bounced would mean starting again.
 */
function CreatedLink({ result }: { result: InviteResult }) {
  const failed = !result.delivered && result.via !== "link";
  return (
    <div className="space-y-2 rounded-lg border bg-muted/40 p-3">
      <div className="flex items-center gap-2">
        <code className="flex-1 truncate text-xs">{result.link}</code>
        <CopyButton text={result.link} label="Copy the invitation link" />
      </div>
      {failed && (
        <p className="text-[11px] leading-relaxed text-muted-foreground">
          {result.delivery_error ||
            "We could not deliver it, so pass this link on yourself — it still works."}
        </p>
      )}
    </div>
  );
}

/** Inviting one person, by email address or by Slack user id. */
function InvitePersonForm({
  roles,
  assignable,
  onCreated,
}: {
  roles: ConsoleRole[];
  assignable: (key: string) => boolean;
  onCreated: () => void;
}) {
  const [who, setWho] = useState("");
  const [role, setRole] = useState("viewer");
  const [result, setResult] = useState<InviteResult | null>(null);
  const [busy, setBusy] = useState(false);
  // The same shape the server tests for, so the hint under the field and the route the request
  // takes cannot disagree about what was typed.
  const isID = /^[UW][A-Z0-9]{4,}$/i.test(who.trim());

  const send = async () => {
    if (!who.trim() || busy) return;
    setBusy(true);
    setResult(null);
    try {
      const res = await api.post<InviteResult>("/api/console/invites", {
        slack_user_id: isID ? who.trim().toUpperCase() : "",
        email: isID ? "" : who.trim(),
        role,
      });
      setWho("");
      if (res.delivered) {
        toast.success(
          res.via === "dm"
            ? `Invite sent to ${res.to} as a direct message`
            : `Invite emailed to ${res.to}`,
        );
      } else {
        // The link comes back either way: a failed send must not lose an invitation already
        // written, so it is offered to copy rather than swallowed.
        setResult(res);
        toast.message("Invite created — copy the link and pass it on");
      }
      onCreated();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-3">
      <form
        onSubmit={(e) => {
          e.preventDefault();
          send();
        }}
        className="grid gap-2 sm:grid-cols-[1fr_11rem_auto]"
      >
        <div className="space-y-1">
          <Input
            className="font-mono"
            value={who}
            onChange={(e) => setWho(e.target.value)}
            placeholder="U0123ABCDEF or someone@example.com"
            aria-label="Who to invite"
          />
          {who.trim() !== "" && (
            <p className="text-[11px] text-muted-foreground">
              {isID
                ? "Looks like a Slack id — the bot will DM them the link."
                : "Treated as an email — we’ll send the link there."}
            </p>
          )}
        </div>
        <Select value={role} onValueChange={setRole}>
          <SelectTrigger aria-label="Role for the invite">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {roles.map((r) => (
              <SelectItem key={r.key} value={r.key} disabled={!assignable(r.key)}>
                {r.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Button type="submit" loading={busy} disabled={!who.trim()}>
          <UserPlus /> Invite
        </Button>
      </form>
      {result && <CreatedLink result={result} />}
    </div>
  );
}

/**
 * Making a link that anybody can use.
 *
 * Every field here is a bound rather than a decoration: a link with no name on it is only as
 * safe as its shortest leash, so the form picks conservative answers and makes the looser ones
 * something you choose on purpose.
 */
function InviteLinkForm({
  roles,
  assignable,
  onCreated,
}: {
  roles: ConsoleRole[];
  assignable: (key: string) => boolean;
  onCreated: () => void;
}) {
  const [label, setLabel] = useState("");
  const [role, setRole] = useState("viewer");
  const [days, setDays] = useState("7");
  const [uses, setUses] = useState("10");
  const [domain, setDomain] = useState("");
  const [result, setResult] = useState<InviteResult | null>(null);
  const [busy, setBusy] = useState(false);

  const create = async () => {
    if (busy) return;
    setBusy(true);
    setResult(null);
    try {
      const res = await api.post<InviteResult>("/api/console/invites", {
        share: true,
        label: label.trim(),
        role,
        days: Number(days),
        max_uses: Number(uses),
        domain: domain.trim(),
      });
      setResult(res);
      toast.success("Link created — anyone you send it to can join");
      onCreated();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-3">
      <form
        onSubmit={(e) => {
          e.preventDefault();
          create();
        }}
        className="space-y-2"
      >
        <div className="grid gap-2 sm:grid-cols-[1fr_11rem]">
          <Input
            value={label}
            onChange={(e) => setLabel(e.target.value)}
            placeholder="What is this link for? e.g. Design team"
            aria-label="What the link is for"
            maxLength={60}
          />
          <Select value={role} onValueChange={setRole}>
            <SelectTrigger aria-label="Role the link grants">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {roles.map((r) => (
                <SelectItem key={r.key} value={r.key} disabled={!assignable(r.key)}>
                  {r.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>

        <div className="grid gap-2 sm:grid-cols-[10rem_10rem_1fr_auto]">
          <Select value={days} onValueChange={setDays}>
            <SelectTrigger aria-label="How long the link lasts">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="1">Expires in a day</SelectItem>
              <SelectItem value="7">Expires in 7 days</SelectItem>
              <SelectItem value="30">Expires in 30 days</SelectItem>
            </SelectContent>
          </Select>
          <Select value={uses} onValueChange={setUses}>
            <SelectTrigger aria-label="How many people the link lets in">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="1">1 person</SelectItem>
              <SelectItem value="10">Up to 10 people</SelectItem>
              <SelectItem value="50">Up to 50 people</SelectItem>
              <SelectItem value="0">No limit</SelectItem>
            </SelectContent>
          </Select>
          <Input
            value={domain}
            onChange={(e) => setDomain(e.target.value)}
            placeholder="Only @example.com addresses (optional)"
            aria-label="Email domain the link accepts"
          />
          <Button type="submit" loading={busy}>
            <Link2 /> Create link
          </Button>
        </div>
      </form>

      <p className="text-[11px] leading-relaxed text-muted-foreground">
        {domain.trim()
          ? `Only ${domain.trim().replace(/^@/, "")} addresses can use it. `
          : "Anyone who opens it can join, so send it somewhere only the right people can see. "}
        You can revoke it below at any time.
      </p>

      {result && <CreatedLink result={result} />}
    </div>
  );
}

/** How many more people a share link will let in. */
function usesLeft(i: ConsoleInvite): string {
  if (i.max_uses < 0) return `no limit${i.uses > 0 ? ` · ${i.uses} joined` : ""}`;
  return `${i.uses} of ${i.max_uses} used`;
}

/**
 * One pending invitation. It says who it is for, who sent it and when it dies, because a list of
 * five identical rows reading "expires in 7 days" is a list nobody can act on — the question
 * being asked of it is always "is this one still wanted?".
 */
function PendingRow({
  invite: i,
  roles,
  onChanged,
}: {
  invite: ConsoleInvite;
  roles: ConsoleRole[];
  onChanged: () => void;
}) {
  const { confirm, confirmDialog } = useConfirm();

  const revoke = async () => {
    if (
      i.kind === "link" &&
      !(await confirm({
        title: `Revoke “${i.who}”?`,
        description:
          "The link stops working immediately. Anyone who already joined with it keeps their access.",
        confirmLabel: "Revoke",
        destructive: true,
      }))
    ) {
      return;
    }
    try {
      await api.del(`/api/console/invites/${i.id}`);
      toast.success(i.kind === "link" ? "Link revoked" : "Invite revoked");
      onChanged();
    } catch (err) {
      toast.error(errorMessage(err));
    }
  };

  return (
    <div className="flex items-center gap-3 px-4 py-2.5">
      <span className="shrink-0 text-muted-foreground">
        {i.kind === "slack" ? (
          <SlackGlyph />
        ) : i.kind === "link" ? (
          <Link2 className="size-4" strokeWidth={1.75} />
        ) : (
          <Mail className="size-4" strokeWidth={1.75} />
        )}
      </span>
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-1.5">
          <span className="truncate text-sm">{i.who}</span>
          {i.kind === "slack" && i.email && (
            <span className="truncate text-xs text-muted-foreground">{i.email}</span>
          )}
          {i.kind === "link" && i.domain && (
            <StatusChip variant="neutral">@{i.domain} only</StatusChip>
          )}
        </div>
        <div
          className="truncate text-xs text-muted-foreground"
          title={parseTime(i.expires_at)?.toLocaleString()}
          suppressHydrationWarning
        >
          {[
            i.kind === "link" ? usesLeft(i) : null,
            expiresIn(i.expires_at),
            i.invited_by ? `invited by ${i.invited_by}` : null,
          ]
            .filter(Boolean)
            .join(" · ")}
        </div>
      </div>
      <StatusChip variant="neutral">
        {roles.find((r) => r.key === i.role)?.label ?? i.role}
      </StatusChip>
      <Button
        variant="ghost"
        size="icon-sm"
        aria-label={`Revoke the invite for ${i.who}`}
        onClick={revoke}
      >
        <X />
      </Button>
      {confirmDialog}
    </div>
  );
}
