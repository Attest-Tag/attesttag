import { navItemFor } from "@/lib/nav";

/**
 * What the panel says before anybody has asked it anything, chosen by the page behind it.
 *
 * The route decides what the assistant ADVERTISES; the selected channel and the attached files
 * decide what it acts on. That split is the point of this file — and the reason the copy is
 * per-page rather than one sentence is that "ask me anything" is the least useful thing an empty
 * box can say. Somebody standing on the audit log wants to know it can read the audit log.
 *
 * One or two short sentences, naming something worth asking rather than describing a capability.
 * Every claim maps to a resource the assistant actually has, so keep the two honest with each
 * other. What it can read also depends on the reader's own permissions, so these say what the
 * page is for rather than promising this particular person will get it.
 */
export type AssistantHint = { title: string; body: string };

// Keyed by the nav item's href, which navItemFor already resolves by longest prefix.
const PAGES: Record<string, AssistantHint> = {
  "/": {
    title: "Ask about this deployment",
    body: "What is running, what it cost this month, and which channel spent it.",
  },
  "/workspaces": {
    title: "Ask about this channel",
    body: "What it can reach, what it inherits, what a field does. Ask for a change and confirm it here.",
  },
  "/bundles": {
    title: "Ask about access",
    body: "Which credentials exist and which channels can reach them. Setting a new one up is a question too.",
  },
  "/approvers": {
    title: "Ask about approvals",
    body: "Who can approve what, and what each tier grants. Ask to add an approver and confirm it here.",
  },
  "/access-requests": {
    title: "Ask about requests",
    body: "Who asked for what, who answered, and what ran. Approving is a Slack act, so not here.",
  },
  "/documents": {
    title: "Ask about the corpus",
    body: "Which files the bot can search when it answers.",
  },
  "/memory": {
    title: "Ask about this console",
    body: "Not what the bot remembers — that stays on this page. Channels, access and spend it can do.",
  },
  "/routines": {
    title: "Ask about routines",
    body: "What runs on a schedule, where it posts, and when it last ran.",
  },
  "/jobs": {
    title: "Ask about fix jobs",
    body: "What the worker was handed, how it went, and what it cost.",
  },
  "/artifacts": {
    title: "Ask about artifacts",
    body: "What the bot has made and posted, and when.",
  },
  "/activity": {
    title: "Ask about activity",
    body: "Turns and tool calls as a log to analyse — what failed, what is slow, which channel is busiest.",
  },
  "/audit": {
    title: "Ask about the audit log",
    body: "Everything one person did, or every change to one thing.",
  },
  "/settings": {
    title: "Ask about settings",
    body: "Models, budget and limits as they stand. It can change a channel's, not the organisation's.",
  },
  "/playground": {
    title: "Ask about channels",
    body: "The playground runs a real turn in a channel. This reads the console and proposes changes.",
  },
  "/developer/api-keys": {
    title: "Ask about the API",
    body: "What a key carries and how /v1 is used. Minting one stays on this page.",
  },
  "/developer/api-reference": {
    title: "Ask about the API",
    body: "Which endpoint does what, and what a key needs to call it.",
  },
  "/get-started": {
    title: "Ask how to set it up",
    body: "It searches the product documentation, so “how do I…” has an answer here.",
  },
};

const FALLBACK: AssistantHint = {
  title: "Ask about this console",
  body: "Channels, access, spend and the audit log — and how to set any of it up.",
};

export function assistantHintFor(pathname: string): AssistantHint {
  return PAGES[navItemFor(pathname)?.href ?? ""] ?? FALLBACK;
}
