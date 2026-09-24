import {
  Activity,
  BookOpen,
  FileText,
  FlaskConical,
  Brain,
  CalendarClock,
  Hammer,
  Hash,
  KeyRound,
  LayoutGrid,
  Plug,
  ScrollText,
  Settings,
  ShieldCheck,
  SquareTerminal,
  Terminal,
  UserCheck, PlayCircle } from "lucide-react";

export type NavItem = {
  href: string;
  label: string;
  /** Shown in the topbar; the sidebar uses `label`. */
  title: string;
  description: string;
  icon: React.ComponentType<{ className?: string }>;
  /**
   * The permission the page needs, when it is one most members do not hold. The rail hides
   * the entry rather than offering a page that only ever says no; a page anybody can open at
   * least partly leaves this unset.
   */
  permission?: string;
};

export type NavGroup = { label: string; items: NavItem[] };

// One list drives the sidebar, the topbar title, and the page headers, so a
// renamed section changes everywhere at once.
export const NAV_GROUPS: NavGroup[] = [
  {
    label: "Overview",
    items: [
      {
        href: "/",
        label: "Overview",
        title: "Overview",
        description: "Spend, usage and what the bot has to work with.",
        icon: LayoutGrid,
      },
    ],
  },
  {
    label: "Access",
    items: [
      {
        href: "/workspaces",
        label: "Workspaces",
        title: "Workspaces",
        description: "What the bot may reach from the workspace and each channel.",
        icon: Hash,
      },
      {
        href: "/bundles",
        label: "Access bundles",
        title: "Access bundles",
        description: "Credentials, domains and instructions, grouped to attach to scopes.",
        icon: KeyRound,
      },
      {
        href: "/approvers",
        label: "Approvers",
        title: "Approvers",
        description: "The tiers that can approve access, and what each may grant.",
        icon: ShieldCheck,
      },
      {
        href: "/access-requests",
        label: "Access requests",
        title: "Access requests",
        description: "Who asked for what, who approved it, and what ran.",
        icon: UserCheck,
      },
    ],
  },
  {
    label: "Knowledge",
    items: [
      {
        href: "/documents",
        label: "Documents",
        title: "Documents",
        description: "Files the bot can search when it answers.",
        icon: BookOpen,
      },
      {
        href: "/memory",
        label: "Memory",
        title: "Memory",
        description:
          "Facts the bot keeps per channel and workspace, and the private notes you keep for yourself.",
        icon: Brain,
      },
    ],
  },
  {
    label: "Automation",
    items: [
      {
        href: "/routines",
        label: "Routines",
        title: "Routines",
        description: "Prompts that run on a schedule and post to a channel.",
        icon: CalendarClock,
      },
      {
        href: "/jobs",
        label: "Jobs",
        title: "Jobs",
        description: "Fix jobs the bot handed to a worker: status, pull request, cost, log.",
        icon: Hammer,
      },
    ],
  },
  {
    label: "Insight",
    items: [
      {
        href: "/artifacts",
        label: "Artifacts",
        title: "Artifacts",
        description: "Files the bot made and posted in Slack.",
        icon: FileText,
      },
      {
        href: "/activity",
        label: "Activity",
        title: "Activity",
        description: "Recent turns, tool calls and proxied requests.",
        icon: Activity,
      },
      {
        // Activity is what the bot did; this is what the people did. Kept beside it because a
        // reader asking "what happened on Tuesday" wants both, and apart from it because the
        // two answer different people: one the person running the bot, the other the person
        // auditing them.
        href: "/audit",
        label: "Audit log",
        title: "Audit log",
        description: "Who signed in, what they changed, and what they approved — with where it came from.",
        icon: ScrollText,
        permission: "audit.view",
      },
    ],
  },
  {
    label: "Developer",
    items: [
      {
        href: "/developer/api-keys",
        label: "API keys",
        title: "API keys",
        description: "Credentials for the /v1 API and the MCP server, each carrying the access of whoever made it.",
        icon: Terminal,
      },
      {
        // "API reference" rather than "Documentation": the rail already has Documents, and two
        // entries a word apart that mean entirely different things is a trap, not a section.
        href: "/developer/api-reference",
        label: "API reference",
        title: "API reference",
        description: "Every endpoint, with the request to copy and the response to expect.",
        icon: SquareTerminal,
      },
      {
        // Below the API because it is the API: each tool is one /v1 route, spoken over MCP.
        href: "/developer/mcp",
        label: "MCP",
        title: "MCP",
        description: "Connect Claude, Cursor or any MCP client, with OAuth or an API key.",
        icon: Plug,
      },
    ],
  },
];

// Pinned to the bottom of the rail rather than scrolling with the sections. Neither of these is a
// destination among the rest: one is where you go to change how the whole thing behaves, and the
// other is where you go to find out what that changed. Both are reached from wherever you already
// are, so they are worth a fixed place more than a place in the running order.
export const FOOTER_ITEMS: NavItem[] = [
  // First of the pinned three, above the two that change and inspect things:
  // it is onboarding for people who already have a login, and the one place
  // in the console that explains the rest of it. Same walkthroughs as the
  // marketing site's /walkthroughs, fetched from there.
  {
    href: "/get-started",
    label: "Get started",
    title: "Get started",
    description: "Recorded walkthroughs of the whole product, chapter by chapter.",
    icon: PlayCircle,
  },
  {
    href: "/playground",
    label: "Playground",
    title: "Playground",
    description: "Ask a channel something from here and watch what its settings actually do.",
    icon: FlaskConical,
  },
  {
    href: "/settings",
    label: "Settings",
    title: "Settings",
    description: "Models, budget, behaviour, and who can open the console.",
    icon: Settings,
  },
];

export const NAV_ITEMS = [...NAV_GROUPS.flatMap((g) => g.items), ...FOOTER_ITEMS];

/** The nav item for a pathname — the longest href that prefixes it. */
export function navItemFor(pathname: string): NavItem | undefined {
  const clean = pathname.replace(/\/+$/, "") || "/";
  return NAV_ITEMS.filter(
    (item) => clean === item.href || (item.href !== "/" && clean.startsWith(item.href + "/")),
  ).sort((a, b) => b.href.length - a.href.length)[0];
}
