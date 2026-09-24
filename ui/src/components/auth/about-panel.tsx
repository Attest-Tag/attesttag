import {
  BookOpen,
  CalendarClock,
  CircleDollarSign,
  FileText,
  KeyRound,
  MessageSquareText,
  ShieldCheck,
} from "lucide-react";
import { BrandMark } from "@/components/shell/brand-mark";

// What the bot is, next to the sign-in card. The console is public at the edge
// (the /setup and /configure pages have to be reachable by non-admins), so this
// page is the one thing a curious colleague can see without an account — it
// should tell them what the thing does. Everything here is static copy: the
// signed-out page must never leak a workspace's own configuration.

type Feature = { icon: React.ComponentType<{ className?: string }>; title: string; body: string };

const FEATURES: Feature[] = [
  {
    icon: MessageSquareText,
    title: "Answers in the thread",
    body: "@mention it in a channel or DM it. It reads the whole thread and streams the answer back where the question was asked.",
  },
  {
    icon: BookOpen,
    title: "Knows your workspace",
    body: "Searches Slack history, the documents you upload, and the web — and answers policy questions only from what it found.",
  },
  {
    icon: KeyRound,
    title: "Reaches your other tools",
    body: "GitHub, ClickUp, Sentry, Linear, Jira, Notion, Drive, Datadog and any remote MCP server, granted per channel.",
  },
  {
    icon: CalendarClock,
    title: "Remembers and schedules",
    body: "Keeps facts per channel, and turns “every weekday at 9am, post a digest” into a routine that runs on its own.",
  },
  {
    icon: FileText,
    title: "Hands back real files",
    body: "Reports and exports are written as Markdown, CSV, JSON or YAML and posted straight into the thread.",
  },
  {
    icon: ShieldCheck,
    title: "Never holds a credential",
    body: "Secrets are injected at the network edge, never shown to the model, and any write waits for someone to press Confirm.",
  },
];

export function AboutPanel() {
  return (
    // Definite width beside the card: left to `w-full`, the flex row hands it a slot wider
    // than its max-width and `mx-auto` floats it off-centre inside that slack.
    <div className="mx-auto flex w-full max-w-xl flex-col gap-6 lg:mx-0 lg:w-[32rem] lg:max-w-none">
      <div className="space-y-3">
        <div className="hidden items-center gap-3 lg:flex">
          <BrandMark size="lg" />
          <span className="font-semibold tracking-tight">attest_tag</span>
        </div>
        <h1 className="text-balance text-2xl font-semibold leading-tight tracking-tight">
          An AI teammate that lives in your Slack.
        </h1>
        <p className="text-pretty text-sm text-muted-foreground">
          It reads the thread, searches your documents and the web, calls the services you
          connect, and answers where the question was asked — on open-weight models, through
          whichever endpoint you point it at.
        </p>
      </div>

      <ul className="grid gap-2 sm:grid-cols-2">
        {FEATURES.map(({ icon: Icon, title, body }) => (
          <li key={title} className="rounded-xl border bg-card p-4 shadow-sm">
            <div className="flex items-center gap-2">
              <span className="flex size-7 shrink-0 items-center justify-center rounded-md border border-ai-border bg-ai-soft text-ai">
                <Icon className="size-3.5" />
              </span>
              <span className="text-sm font-medium">{title}</span>
            </div>
            <p className="mt-2 text-pretty text-xs leading-relaxed text-muted-foreground">
              {body}
            </p>
          </li>
        ))}
      </ul>

      <div className="flex items-start gap-2 border-t pt-4 text-xs text-muted-foreground">
        <CircleDollarSign className="mt-px size-3.5 shrink-0" />
        <p className="text-pretty">
          Every turn is costed and audited. Admins set a monthly budget for the workspace and
          for a single channel, cap requests per person, and see the tokens and spend behind
          each reply.
        </p>
      </div>
    </div>
  );
}
