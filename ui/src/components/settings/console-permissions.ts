// The permission vocabulary, named for people instead of for the API.
//
// The wire format is a flat list of dotted keys — `access_requests.close`,
// `connections.view` — which is the right shape for a matrix check and the
// wrong one to put in front of somebody choosing what a role may do. Sixteen
// mono strings in a checkbox grid say nothing about which of them are
// dangerous, which pair up, or what "scopes" even is.
//
// So the catalogue lives here: a human label, a one-line hint, and an order
// that groups a permission with the ones you'd think about at the same time.
// It is keyed by the same strings the server sends, and anything the server
// reports that this table has never heard of still renders — under "Other",
// with its raw key — so a console built before a new permission existed shows
// it rather than silently dropping it from a role's list.

export type PermissionMeta = {
  key: string;
  /** Short enough to sit in a chip beside a dozen others. */
  label: string;
  hint: string;
};

export type PermissionGroup = { title: string; items: PermissionMeta[] };

const CATALOGUE: PermissionGroup[] = [
  {
    title: "Console",
    items: [
      { key: "settings.manage", label: "Settings", hint: "Models, budgets, limits and behaviour" },
      {
        key: "billing.manage",
        label: "Billing",
        hint: "Buy a plan, top up credit, and open the card on file. Reading the balance needs nothing",
      },
      { key: "users.manage", label: "People", hint: "Invite, remove and change someone's role" },
      { key: "roles.manage", label: "Roles", hint: "Define the custom roles this console offers" },
    ],
  },
  {
    title: "Connections",
    items: [
      { key: "connections.view", label: "See connections", hint: "That one exists — never its secret" },
      {
        key: "connections.manage",
        label: "Manage connections",
        hint: "Create, rotate and delete credentials, and their setup links",
      },
      { key: "bundles.manage", label: "Bundles", hint: "Bundles, domains and skills" },
    ],
  },
  {
    title: "Channels and content",
    items: [
      { key: "scopes.manage", label: "Channels", hint: "Per-channel instructions, model, budget and grants" },
      { key: "documents.manage", label: "Documents", hint: "Upload, replace and remove what the bot reads" },
      { key: "memory.manage", label: "Memory", hint: "What the bot remembers in each scope" },
      { key: "routines.manage", label: "Routines", hint: "Scheduled prompts and where they post" },
    ],
  },
  {
    title: "Access requests",
    items: [
      { key: "access_requests.view", label: "See requests", hint: "The queue and what each one asked for" },
      { key: "access_requests.close", label: "Close requests", hint: "Mark a request granted, denied or done" },
      { key: "approvers.manage", label: "Approvers", hint: "The tiers that can grant access, and who holds them" },
    ],
  },
  {
    title: "Developer",
    items: [
      {
        key: "api_keys.manage",
        label: "API keys",
        hint: "Mint and revoke keys for the /v1 API — each one carries its maker's own access",
      },
    ],
  },
  {
    title: "Activity",
    items: [
      { key: "activity.view", label: "Activity", hint: "Turns, tool calls and what they cost" },
      {
        key: "audit.view",
        label: "Audit log",
        hint: "Who signed in and from where, what they changed, what they approved",
      },
      { key: "artifacts.view", label: "See artifacts", hint: "Files a turn produced" },
      { key: "artifacts.manage", label: "Manage artifacts", hint: "Delete artifacts and set retention" },
    ],
  },
];

const BY_KEY: Record<string, PermissionMeta> = {};
for (const g of CATALOGUE) for (const p of g.items) BY_KEY[p.key] = p;

/** The human name for a permission, falling back to its raw key. */
export function permissionLabel(key: string): string {
  return BY_KEY[key]?.label ?? key;
}

export function permissionHint(key: string): string {
  return BY_KEY[key]?.hint ?? "";
}

/**
 * The catalogue narrowed to the permissions this build actually reports, in
 * catalogue order, with anything unrecognised collected at the end.
 */
export function groupPermissions(all: string[]): PermissionGroup[] {
  const known = new Set(all);
  const out: PermissionGroup[] = [];
  for (const g of CATALOGUE) {
    const items = g.items.filter((p) => known.has(p.key));
    if (items.length) out.push({ title: g.title, items });
  }
  const extra = all.filter((k) => !BY_KEY[k]);
  if (extra.length) {
    out.push({
      title: "Other",
      items: extra.map((k) => ({ key: k, label: k, hint: "" })),
    });
  }
  return out;
}

/** Sorts a role's permissions into the order the catalogue lists them. */
export function orderPermissions(perms: string[]): string[] {
  const order = new Map<string, number>();
  let i = 0;
  for (const g of CATALOGUE) for (const p of g.items) order.set(p.key, i++);
  return [...perms].sort(
    (a, b) => (order.get(a) ?? 1e9) - (order.get(b) ?? 1e9) || a.localeCompare(b),
  );
}

/** What each built-in role is for. The server sends no description, and "admin" alone doesn't say. */
export const BUILTIN_ROLE_BLURB: Record<string, string> = {
  admin: "Everything, including credentials, people and roles.",
  editor: "Looks after content and channels. No credentials, no people.",
  viewer: "Reads what exists and changes nothing.",
};

/** Mirrors the server's `slug`: lowercase, anything else becomes an underscore, trimmed. */
export function roleKey(name: string): string {
  return name
    .toLowerCase()
    .replace(/[^a-z0-9]/g, "_")
    .replace(/^_+|_+$/g, "");
}
