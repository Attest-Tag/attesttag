"use client";

import { useState } from "react";
import { CircleHelp, Loader2 } from "lucide-react";
import { toast } from "sonner";
import { useAuth } from "@/components/shell/auth-provider";
import { Button } from "@/components/ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Textarea } from "@/components/ui/textarea";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { api, errorMessage } from "@/lib/api";

/**
 * Help, in the header beside the assistant. Type a question and the server mails it to the
 * deployment's support address with you as Reply-To — the same road as the Upgrade and Talk to us
 * requests, never a `mailto:` — so the answer comes to the address you sign in with and the
 * question arrives saying which organisation and page it came from.
 *
 * Not offered where the deployment names no support address (`plan.support_email`), because
 * there would be nobody to send it to; the server refuses on the same condition.
 *
 * The draft lives in this component rather than the popover, so closing it by accident — a click
 * outside, Escape — does not throw away what was typed.
 */
export function HelpButton() {
  const { me } = useAuth();
  const support = me?.plan?.support_email ?? "";
  const email = me?.user?.email ?? "";
  const [open, setOpen] = useState(false);
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);

  if (!support) return null;

  const send = async () => {
    if (!text.trim() || busy) return;
    setBusy(true);
    try {
      const res = await api.post<{ delivered?: boolean; support_email?: string }>(
        "/api/support/message",
        // The path only: a query string can carry a token, and support needs the screen, not it.
        { message: text, page: window.location.pathname },
      );
      toast.success(
        res.delivered === false
          ? `Mail is not configured on this deployment — write to ${res.support_email || support}.`
          : `Sent. The reply comes to ${email || "your own email address"}.`,
      );
      setText("");
      setOpen(false);
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <Tooltip>
        <TooltipTrigger asChild>
          <PopoverTrigger asChild>
            <Button
              variant="ghost"
              size="icon"
              className={open ? "size-8 text-foreground" : "size-8 text-muted-foreground"}
              aria-label="Help"
            >
              <CircleHelp className="size-4" />
            </Button>
          </PopoverTrigger>
        </TooltipTrigger>
        <TooltipContent>Help</TooltipContent>
      </Tooltip>
      <PopoverContent align="end" className="w-80 p-0">
        <form
          className="space-y-3 p-4"
          onSubmit={(e) => {
            e.preventDefault();
            send();
          }}
        >
          <p className="text-sm font-medium">Ask us anything</p>
          <Textarea
            value={text}
            onChange={(e) => setText(e.target.value)}
            // Enter sends and Shift+Enter starts a new line, as in the assistant beside it.
            onKeyDown={(e) => {
              if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
                e.preventDefault();
                e.currentTarget.form?.requestSubmit();
              }
            }}
            rows={4}
            maxLength={4000}
            disabled={busy}
            autoFocus
            className="min-h-24 max-h-60 resize-none"
            placeholder="What were you trying to do, and what happened?"
            aria-label="Your question"
          />
          <div className="flex items-center justify-end">
            <Button type="submit" size="sm" disabled={busy || !text.trim()}>
              {busy && <Loader2 className="animate-spin" />}
              Send
            </Button>
          </div>
        </form>
      </PopoverContent>
    </Popover>
  );
}
