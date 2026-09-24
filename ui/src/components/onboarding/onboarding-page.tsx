"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { ArrowRight, Check, Loader2, Send, Sparkles, UserPlus } from "lucide-react";
import { toast } from "sonner";
import { AuthNotice, MicrosoftGlyph, SlackGlyph, useOneShotParam } from "@/components/auth/auth-layout";
import { CopyButton } from "@/components/core/copy-button";
import { ConnectTeamsDialog } from "@/components/scopes/connect-teams-dialog";
import { BrandMark } from "@/components/shell/brand-mark";
import { useAuth } from "@/components/shell/auth-provider";
import { UserMenu } from "@/components/shell/user-menu";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import {
  api,
  errorMessage,
  useApi,
  type InviteResult,
  type Onboarding,
  type OnboardingStep,
  type TakenWorkspace,
} from "@/lib/api";
import { cn } from "@/lib/utils";

// The one path from a fresh account to a bot that answers: connect a workspace, invite it to a
// channel, say something to it. Three steps and no branches — every choice this screen could
// offer is a choice about something that does not exist yet.
//
// It reads its state back from the server rather than remembering what it has shown, so it is
// the same screen whether you are seeing it for the first time or coming back to it a week
// later with two steps already done. While a step is somebody else's move — they are in Slack,
// typing the invite — it polls, so finishing in the other window finishes it here too.

const POLL_MS = 4000;

const STEPS: { id: OnboardingStep; title: string }[] = [
  { id: "install", title: "Connect your Slack workspace" },
  { id: "channel", title: "Invite the bot to a channel" },
  { id: "mention", title: "Say something to it" },
  { id: "invite", title: "Bring your team in" },
];

/** Where a step stands relative to the one the organisation is on. */
type StepState = "done" | "now" | "later";

function stateOf(step: OnboardingStep, current: OnboardingStep): StepState {
  if (current === "done") return "done";
  const at = STEPS.findIndex((s) => s.id === current);
  const mine = STEPS.findIndex((s) => s.id === step);
  return mine < at ? "done" : mine === at ? "now" : "later";
}

function Step({
  index,
  title,
  state,
  summary,
  children,
}: {
  index: number;
  title: string;
  state: StepState;
  /** The one line a finished step collapses to: what it ended up connecting to. */
  summary?: React.ReactNode;
  children?: React.ReactNode;
}) {
  return (
    <li
      className={cn(
        "rounded-2xl border bg-card p-5 shadow-sm transition-opacity",
        state === "later" && "opacity-55",
        state === "now" && "border-foreground/20",
      )}
    >
      <div className="flex items-start gap-3">
        <span
          aria-hidden
          className={cn(
            "mt-0.5 flex size-6 shrink-0 items-center justify-center rounded-full text-xs font-medium",
            state === "done"
              ? "bg-success-soft text-success"
              : state === "now"
                ? "bg-foreground text-background"
                : "border bg-muted text-muted-foreground",
          )}
        >
          {state === "done" ? <Check className="size-3.5" strokeWidth={3} /> : index}
        </span>
        <div className="min-w-0 flex-1">
          <h2 className="text-sm font-semibold leading-6 text-foreground">
            {title}
            <span className="sr-only">
              {state === "done" ? " — done" : state === "now" ? " — current step" : ""}
            </span>
          </h2>
          {state === "done" ? (
            summary && <p className="mt-0.5 truncate text-sm text-muted-foreground">{summary}</p>
          ) : state === "now" ? (
            <div className="mt-3 space-y-3">{children}</div>
          ) : null}
        </div>
      </div>
    </li>
  );
}

/** A line to type in Slack, with the button that puts it on the clipboard. */
function SlackCommand({ text }: { text: string }) {
  return (
    <div className="flex items-center gap-2 rounded-lg border bg-muted/40 py-1.5 pl-3 pr-1.5">
      <code className="min-w-0 flex-1 truncate font-mono text-sm text-foreground">{text}</code>
      <CopyButton text={text} label="Copy" />
    </div>
  );
}

/** What a step says while it is waiting on something happening in Slack. */
function Waiting({ children }: { children: React.ReactNode }) {
  return (
    <p className="flex items-center gap-2 text-sm text-muted-foreground" aria-live="polite">
      <Loader2 className="size-3.5 shrink-0 animate-spin" />
      {children}
    </p>
  );
}

/**
 * The dead end step one reaches when the workspace behind somebody's Slack sign-in already
 * belongs to another organisation. Pressing Add to Slack would be refused, so this replaces it:
 * what has happened, and the one thing that fixes it — an invitation from the person who
 * connected it. The button asks them, once.
 */
function WorkspaceTaken({ taken, onChanged }: { taken: TakenWorkspace; onChanged: () => void }) {
  const [asking, setAsking] = useState(false);
  const who = taken.installer || "whoever set it up";

  const ask = async () => {
    setAsking(true);
    try {
      await api.post("/api/onboarding/ask-invite");
      toast.success(`Asked ${who} to invite you.`);
      onChanged();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setAsking(false);
    }
  };

  return (
    <>
      <AuthNotice kind="info">
        <strong className="font-medium text-foreground">{taken.name}</strong> is already connected
        to another attest_tag organisation
        {taken.installer ? <> — {taken.installer} connected it</>: null}. A workspace belongs to
        one organisation, so connecting it again is refused. What you want is an invitation to
        theirs: you would land in the same console, with the same sign-in you have now.
      </AuthNotice>
      {taken.asked ? (
        <p className="flex items-center gap-2 text-sm text-muted-foreground">
          <Check className="size-3.5 shrink-0 text-success" />
          {who} has been messaged in Slack. When they invite you, the link will come to your email.
        </p>
      ) : taken.can_ask ? (
        <Button onClick={ask} disabled={asking}>
          {asking ? <Loader2 className="size-4 animate-spin" /> : <Send className="size-4" />}
          Ask {who} to invite me
        </Button>
      ) : (
        <p className="text-sm text-muted-foreground">
          Ask {who} in Slack to invite you from their console.
        </p>
      )}
    </>
  );
}

/**
 * The address behind the account has not been confirmed, and unconfirmed is where this walk
 * stops: connecting a workspace and inviting a teammate are both refused until it is. Settings
 * has the same button, but somebody held on step one has not been to Settings and may not be
 * allowed in — so the one thing that unblocks them sits on the screen that is blocking them.
 */
function VerifyEmail({ email, onSent }: { email?: string; onSent: () => void }) {
  const [sending, setSending] = useState(false);
  const [sent, setSent] = useState(false);

  const resend = async () => {
    setSending(true);
    try {
      await api.post("/api/auth/resend-verification");
      setSent(true);
      toast.success("Confirmation sent. Check your inbox.");
      onSent();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setSending(false);
    }
  };

  return (
    <AuthNotice kind="action">
      <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-2">
        <span className="min-w-0">
          Confirm your email address — the link is in the inbox of{" "}
          <strong className="font-medium">{email || "your account"}</strong>. Nothing here waits
          for it, but a password reset and your first invitation do.
        </span>
        {sent ? (
          <span className="flex shrink-0 items-center gap-1.5">
            <Check className="size-3.5 shrink-0" />
            Sent again
          </span>
        ) : (
          // The brand button the rest of the walk uses, because this is the same kind of thing
          // as the buttons below it: the next step, not a way of dismissing a warning.
          <Button size="sm" onClick={resend} disabled={sending}>
            {sending && <Loader2 className="animate-spin" />}
            Resend
          </Button>
        )}
      </div>
    </AuthNotice>
  );
}

/** The last step: one address, one invitation. Anything more belongs in Settings. */
function InviteTeammate({ onInvited }: { onInvited: () => void }) {
  const [email, setEmail] = useState("");
  const [sending, setSending] = useState(false);

  const invite = async (e: React.FormEvent) => {
    e.preventDefault();
    setSending(true);
    try {
      const out = await api.post<InviteResult>("/api/console/invites", { email });
      toast.success(
        out?.delivered
          ? `Invitation sent to ${email}.`
          : `Invitation created for ${email} — Settings has the link to send them.`,
      );
      setEmail("");
      onInvited();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setSending(false);
    }
  };

  return (
    <form onSubmit={invite} className="flex flex-wrap items-center gap-2">
      <Input
        type="email"
        required
        value={email}
        onChange={(e) => setEmail(e.target.value)}
        placeholder="colleague@company.com"
        className="min-w-0 flex-1"
        aria-label="Their email address"
      />
      <Button type="submit" disabled={sending || !email}>
        {sending ? <Loader2 className="size-4 animate-spin" /> : <UserPlus className="size-4" />}
        Invite
      </Button>
    </form>
  );
}

export function OnboardingPage() {
  const { me } = useAuth();
  const walk = useApi<Onboarding>("/api/onboarding?sync=1");
  const data = walk.data;
  const reload = walk.reload;
  const [teamsOpen, setTeamsOpen] = useState(false);

  // The install comes back through a redirect, so its outcome arrives in the URL rather than in
  // a reply. Shown in place on step one, not as a toast: it is the reason the step is still open.
  // The ?team= a successful callback leaves is read for nothing but its removal — the step list
  // already knows what is connected, and a stale id in the address bar outlives the truth.
  const installError = useOneShotParam("install_error");
  useOneShotParam("team");

  // Steps two and three are finished in Slack, in another window. Poll while one of them is the
  // move, and again the moment this tab is looked at, so coming back is enough to catch up.
  // Confirming the address is the same shape of thing — it is done on a link in the mail, in
  // another tab — but nothing arrives to poll for, so that one only listens for the way back.
  // Connecting Teams is finished in Teams too — the code is sent to the bot there — so step one
  // polls while that dialog is open.
  const live = !!data && (data.step === "channel" || data.step === "mention" || (data.step === "install" && teamsOpen));
  const watch = live || !!data?.email_unverified;
  useEffect(() => {
    if (!watch) return;
    const timer = live ? setInterval(reload, POLL_MS) : undefined;
    const onFocus = () => reload();
    window.addEventListener("focus", onFocus);
    return () => {
      if (timer) clearInterval(timer);
      window.removeEventListener("focus", onFocus);
    };
  }, [watch, live, reload]);

  if (walk.loading || !data) {
    return (
      <Frame>
        <div className="space-y-3">
          {STEPS.map((s) => (
            <Skeleton key={s.id} className="h-24 w-full rounded-2xl" />
          ))}
        </div>
      </Frame>
    );
  }

  const handle = data.bot_handle || "attest_tag";
  const teamNames = data.teams.map((t) => t.name).join(", ");
  const channelNames = data.channels.map((c) => c.name).join(", ");
  const invite = `/invite @${handle}`;
  const hello = `@${handle} what can you do?`;
  // Where the bot lives once it is connected, and before that where it could: the steps after the
  // first are said in the words of the platform that was connected.
  const onTeams = data.platform === "msteams";
  const where = onTeams ? "Teams" : data.platform === "slack" || !data.msteams ? "Slack" : "Slack or Teams";
  const slackReady = data.install_configured;
  const teamsReady = !!data.msteams;
  // Somebody who signed in with Microsoft is most likely on Teams, so Teams is the first and main
  // button for them; everyone else sees Slack first, as the walk always has.
  const teamsFirst = teamsReady && (me?.user?.via === "microsoft" || !slackReady);
  const slackButton = slackReady && (
    <Button key="slack" asChild variant={teamsFirst ? "outline" : "default"}>
      {/* A real navigation, not a fetch: the browser leaves for Slack's consent screen. */}
      <a href={data.install_url}>
        <SlackGlyph />
        Add to Slack
      </a>
    </Button>
  );
  const teamsButton = teamsReady && (
    // Not a navigation: a Teams app is installed in the Teams admin centre, so what this opens is
    // the steps, and the code that names this account.
    <Button key="teams" variant={teamsFirst ? "default" : "outline"} onClick={() => setTeamsOpen(true)}>
      <MicrosoftGlyph />
      Connect Microsoft Teams
    </Button>
  );

  return (
    <Frame email={me?.user?.email}>
      <div className="mb-6">
        <h1 className="text-xl font-semibold tracking-tight text-foreground">
          {data.done ? "You're set up" : "Set up attest_tag"}
        </h1>
        <p className="mt-1 text-sm text-muted-foreground">
          {data.done
            ? `The bot is in ${where} and answering. Everything else is tuning.`
            : `Three steps to a bot that answers in ${where}, and a fourth to bring your team. There is nothing to configure first.`}
        </p>
      </div>

      {data.email_unverified ? (
        // Not a gate any more — Add to Slack works before the address is confirmed — but the
        // address is what a password reset and the invite step rest on, and this page is where
        // the founder is, so the resend button lives here rather than under Settings.
        <div className="mb-3">
          <VerifyEmail email={me?.user?.email} onSent={reload} />
        </div>
      ) : installError ? (
        <div className="mb-3">
          <AuthNotice kind="error">{installError}</AuthNotice>
        </div>
      ) : null}

      <ol className="space-y-3">
        <Step
          index={1}
          title={teamsReady ? "Connect Slack or Microsoft Teams" : STEPS[0].title}
          state={stateOf("install", data.step)}
          summary={teamNames && `Connected to ${teamNames}`}
        >
          {data.taken ? (
            <WorkspaceTaken taken={data.taken} onChanged={reload} />
          ) : (
            <>
              <p className="text-sm text-muted-foreground">
                {teamsReady
                  ? "Wherever your team talks. Slack asks a workspace admin or owner to approve the bot once; Teams needs a Teams admin to upload the app once."
                  : "Slack asks you to approve the bot once, for the whole workspace. Only a Slack workspace admin or owner can do it."}
              </p>
              {!slackReady && !teamsReady ? (
                <AuthNotice kind="info">
                  Connecting a workspace needs SLACK_CLIENT_ID and SLACK_CLIENT_SECRET on the server,
                  and this console&apos;s /slack/oauth/callback registered as a redirect URL on the
                  Slack app.
                </AuthNotice>
              ) : !data.can_install ? (
                <AuthNotice kind="info">
                  Your role cannot connect a workspace. Ask an owner or admin of this organisation to
                  do it — the console has nothing in it until one is connected.
                </AuthNotice>
              ) : (
                <div className="flex flex-wrap gap-2">
                  {teamsFirst ? [teamsButton, slackButton] : [slackButton, teamsButton]}
                </div>
              )}
            </>
          )}
        </Step>

        <Step
          index={2}
          title={onTeams ? "Add it to a team" : STEPS[1].title}
          state={stateOf("channel", data.step)}
          summary={channelNames && `It is in ${channelNames}`}
        >
          {onTeams ? (
            <p className="text-sm text-muted-foreground">
              In Teams, open <em>Apps</em>, find {handle} and choose <em>Add to a team</em>. When
              Teams asks whether it may read the team&apos;s channels, say yes, or it hears only
              the messages that mention it. It can also be talked to in a chat of its own.
            </p>
          ) : (
            <>
              <p className="text-sm text-muted-foreground">
                In Slack, open the channel you want it in and send this. It only ever sees the
                channels it has been let into.
              </p>
              <SlackCommand text={invite} />
            </>
          )}
          <Waiting>Watching for it to turn up in a channel…</Waiting>
        </Step>

        <Step
          index={3}
          title={STEPS[2].title}
          state={stateOf("mention", data.step)}
          summary={data.turns > 0 ? "It answered" : undefined}
        >
          <p className="text-sm text-muted-foreground">
            {onTeams
              ? "Mention it in one of that team's channels and it will answer in the reply chain."
              : "Mention it in that channel and it will answer in the thread."}
          </p>
          <SlackCommand text={hello} />
          <Waiting>Waiting for its first reply…</Waiting>
        </Step>

        <Step
          index={4}
          title={STEPS[3].title}
          state={stateOf("invite", data.step)}
          summary={
            data.members > 1
              ? `${data.members} people`
              : data.invited > 0
                ? `${data.invited} invitation${data.invited === 1 ? "" : "s"} out`
                : undefined
          }
        >
          <p className="text-sm text-muted-foreground">
            Anyone in the channel can talk to the bot without an account. This is for the console
            — spend, access and instructions. They join your organisation rather than starting
            one of their own.
          </p>
          <InviteTeammate onInvited={reload} />
        </Step>
      </ol>
      {teamsReady && (
        <ConnectTeamsDialog
          open={teamsOpen}
          onOpenChange={(open) => {
            setTeamsOpen(open);
            if (!open) reload();
          }}
        />
      )}

      {data.done ? (
        <div className="mt-6 flex items-center gap-3">
          <Button asChild>
            <Link href="/">
              <Sparkles className="size-4" />
              Open the console
            </Link>
          </Button>
          <p className="text-sm text-muted-foreground">
            Next: give it something to reach in Access bundles.
          </p>
        </div>
      ) : data.step !== "install" ? (
        // Once a workspace is connected the console has something to show, so the walk stops
        // being a gate. The remaining steps are still worth finishing, so the way out is a quiet
        // link rather than a second button competing with the step in front of you.
        <div className="mt-6">
          <Link
            href="/"
            className="inline-flex items-center gap-1 text-sm text-muted-foreground underline-offset-4 hover:text-foreground hover:underline"
          >
            Skip for now, open the console
            <ArrowRight className="size-3.5" />
          </Link>
        </div>
      ) : null}
    </Frame>
  );
}

// The frame. Its own page rather than something inside the console, for the same reason the
// two-factor gate is: while step one is unfinished there is nothing behind it but empty
// listings. The account menu stays in the corner — being unable to sign out, switch
// organisation or change theme is not a way to make somebody finish a setup.
function Frame({ email, children }: { email?: string; children: React.ReactNode }) {
  return (
    <div className="flex min-h-dvh flex-col bg-background">
      <header className="flex items-center justify-between px-5 py-4">
        <div className="flex items-center gap-2.5">
          <BrandMark />
          <span className="text-sm font-semibold">attest_tag</span>
        </div>
        <UserMenu />
      </header>
      <main className="mx-auto w-full max-w-xl flex-1 px-5 pb-16 pt-6">{children}</main>
      {email && (
        <footer className="px-5 pb-6 text-center text-xs text-muted-foreground">
          Signed in as {email}
        </footer>
      )}
    </div>
  );
}
