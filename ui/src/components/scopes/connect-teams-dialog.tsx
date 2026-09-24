"use client";

import { useState } from "react";
import Link from "next/link";
import { BookOpen, Download, ExternalLink, KeyRound, MessageSquare } from "lucide-react";
import { CopyButton } from "@/components/core/copy-button";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { api, errorMessage } from "@/lib/api";
import { TEAMS_APP_NAME, TEAMS_GUIDE_PATH, TEAMS_MANAGE_APPS_URL, TEAMS_URL } from "@/lib/msteams";

type LinkCode = { code: string; command: string; expires_at: string; bot_name?: string; chat_url?: string };

// Connecting a Microsoft Teams organisation, in the three steps it takes. A Slack workspace is
// connected by an OAuth round trip that starts on this page, so the account it belongs to is known
// when Slack sends the person back. A Teams app is installed in the Teams admin centre instead,
// where nothing says which attest_tag account it is for — so the account is named by a code, and
// the code is sent to the bot from inside the organisation being connected. The code proves the
// account; Microsoft's signature on the message that carries it proves the organisation.
//
// Every step links to the place in Microsoft's UI where it is done, and the guide shows each one
// with a picture: Teams puts these things in places nobody finds on their own.
export function ConnectTeamsDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const [code, setCode] = useState<LinkCode | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const mint = async () => {
    setBusy(true);
    setError("");
    try {
      setCode(await api.post<LinkCode>("/api/workspaces/msteams/code", {}));
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  // A code is shown once. Closing the dialog forgets it, and opening it again makes a new one.
  const change = (next: boolean) => {
    if (!next) {
      setCode(null);
      setError("");
    }
    onOpenChange(next);
  };

  const bot = code?.bot_name ?? TEAMS_APP_NAME;

  return (
    <Dialog open={open} onOpenChange={change}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Connect Microsoft Teams</DialogTitle>
          <DialogDescription>
            Three steps. The first needs someone who administers Teams for your organisation; the
            guide shows every click, and you can send it to them.
          </DialogDescription>
          <Link
            href={TEAMS_GUIDE_PATH}
            target="_blank"
            rel="noreferrer"
            className="inline-flex w-fit items-center gap-1.5 text-sm font-medium text-primary hover:underline"
          >
            <BookOpen className="size-4" /> Step-by-step guide, with pictures
          </Link>
        </DialogHeader>
        <ol className="space-y-5 py-2 text-sm">
          <li className="space-y-2">
            <p className="font-medium">1. Upload the app to your organisation</p>
            <p className="text-muted-foreground">
              Download the app file. Then, in the Teams admin centre&apos;s <em>Manage apps</em>, choose{" "}
              <em>Actions → Upload new app → Upload</em> and pick the file. It is made for this
              deployment: it names this deployment&apos;s bot.
            </p>
            <div className="flex flex-wrap gap-2">
              <Button asChild variant="outline" size="sm">
                <a href="/api/workspaces/msteams/package" download>
                  <Download className="size-4" /> Download the Teams app
                </a>
              </Button>
              <Button asChild variant="outline" size="sm">
                <a href={TEAMS_MANAGE_APPS_URL} target="_blank" rel="noreferrer">
                  <ExternalLink className="size-4" /> Open Manage apps
                </a>
              </Button>
            </div>
          </li>
          <li className="space-y-2">
            <p className="font-medium">2. Add it in Teams</p>
            <p className="text-muted-foreground">
              In Teams, open <em>Apps</em>, search for <span className="font-mono">{bot}</span> and
              choose <em>Add</em>; a new upload takes a few minutes to show up there. To use it in a
              team&apos;s channels, add it to that team as well; when Teams asks whether it may read the
              team&apos;s channel messages, say yes, or it hears only the messages that mention it.
            </p>
            <Button asChild variant="outline" size="sm">
              <a href={TEAMS_URL} target="_blank" rel="noreferrer">
                <ExternalLink className="size-4" /> Open Teams
              </a>
            </Button>
          </li>
          <li className="space-y-2">
            <p className="font-medium">3. Send it a code</p>
            {code ? (
              <div className="space-y-2">
                <div className="flex items-center gap-1">
                  <code className="min-w-0 flex-1 truncate rounded bg-card px-2 py-1.5 font-mono text-xs">
                    {code.command}
                  </code>
                  <CopyButton text={code.command} label="Copy" />
                </div>
                {code.chat_url && (
                  <div className="flex flex-wrap items-center gap-2">
                    <Button asChild size="sm">
                      <a href={code.chat_url} target="_blank" rel="noreferrer">
                        <MessageSquare className="size-4" /> Open the chat with {bot}
                      </a>
                    </Button>
                    <span className="text-xs text-muted-foreground">
                      The code is already typed in: press Send.
                    </span>
                  </div>
                )}
                <p className="text-xs text-muted-foreground">
                  Or send it in a channel as{" "}
                  <span className="font-mono">
                    @{bot} {code.command}
                  </span>
                  . It works once, for 30 minutes, and connects the organisation it is sent from to
                  this account.
                </p>
              </div>
            ) : (
              <div className="space-y-2">
                <p className="text-muted-foreground">
                  The bot answers nobody until it knows which account it belongs to. Send it this
                  code, from inside the organisation you are connecting.
                </p>
                <Button size="sm" onClick={mint} disabled={busy}>
                  <KeyRound className="size-4" /> {busy ? "Making a code…" : "Get a code"}
                </Button>
              </div>
            )}
          </li>
        </ol>
        {error && <p className="text-sm text-danger">{error}</p>}
        <DialogFooter>
          <Button variant="outline" onClick={() => change(false)}>
            Done
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
