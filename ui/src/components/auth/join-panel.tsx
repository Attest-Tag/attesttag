"use client";

import { useEffect, useState } from "react";
import { CheckCircle2, Loader2 } from "lucide-react";
import { AuthLayout, AuthNotice } from "@/components/auth/auth-layout";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { api, errorMessage, useApi, type InvitePreview } from "@/lib/api";

// A domain-limited link whose only objection is how this person's address was confirmed is
// answered without an organisation: the reason, and where a confirmation mail would go. Proving
// the address by our mail is a step they can take from here, so it is offered, not just refused.
type Preview = InvitePreview & { confirm_email?: boolean; blocked?: string };

// Where an invitation lands when the person opening it is already signed in. They are joining a
// second organisation rather than making an account, so this asks before doing it: the same link
// in a forwarded email should not silently attach somebody's existing account to a stranger's org.
//
// It also asks the question that comes up every time somebody signs up on their own and is then
// invited to the organisation they meant to join all along: what about the empty one they
// founded on the way here? Leaving it is offered only when it is theirs alone and nothing has
// been put in it, it is opt-in, and it detaches them and nothing more — the organisation and its
// rows stay where they are, and their account is untouched.
export function JoinPanel() {
  const preview = useApi<Preview>("/api/auth/invite");
  const [state, setState] = useState<"asking" | "working" | "done" | "failed">("asking");
  const [org, setOrg] = useState("");
  const [left, setLeft] = useState("");
  const [failure, setFailure] = useState("");
  const [leaveOwn, setLeaveOwn] = useState(true);
  const [mail, setMail] = useState<"idle" | "sending" | "sent" | "unsent" | "failed">("idle");
  const [mailFailure, setMailFailure] = useState("");

  const sendConfirmation = () => {
    setMail("sending");
    api
      .post<{ email_configured?: boolean }>("/api/auth/resend-verification")
      .then((r) => setMail(r?.email_configured === false ? "unsent" : "sent"))
      .catch((err) => {
        setMailFailure(errorMessage(err));
        setMail("failed");
      });
  };

  const own = preview.data?.own;
  const canLeave = !!own?.leavable;

  useEffect(() => {
    if (state !== "working") return;
    api
      .post<{ org?: { name?: string }; left?: string }>("/api/auth/accept-invite", {
        leave_own: canLeave && leaveOwn,
      })
      .then((r) => {
        setOrg(r?.org?.name ?? "");
        setLeft(r?.left ?? "");
        setState("done");
      })
      .catch((err) => {
        setFailure(errorMessage(err));
        setState("failed");
      });
  }, [state, canLeave, leaveOwn]);

  const inviting = preview.data?.org?.name;

  return (
    <AuthLayout
      title={state === "done" ? "You are in" : "Accept the invitation"}
      subtitle={state === "done" ? org : inviting || "You are already signed in"}
    >
      {preview.error && state === "asking" && <AuthNotice kind="error">{preview.error}</AuthNotice>}

      {state === "asking" && !preview.error && preview.data?.confirm_email && (
        <>
          <AuthNotice kind="info">{preview.data.blocked}</AuthNotice>
          {mail === "sent" ? (
            <AuthNotice kind="info">
              Sent to <strong className="font-medium">{preview.data.email}</strong>. Open the link in it, then open
              the invitation link again to accept.
            </AuthNotice>
          ) : mail === "unsent" ? (
            <AuthNotice kind="error">
              This deployment has no email provider configured, so nothing was sent. Ask for an invitation
              addressed to you instead.
            </AuthNotice>
          ) : (
            <Button className="w-full" disabled={mail === "sending"} onClick={sendConfirmation}>
              {mail === "sending" && <Loader2 className="size-4 animate-spin" />}
              Email me a confirmation link
            </Button>
          )}
          {mail === "failed" && <AuthNotice kind="error">{mailFailure}</AuthNotice>}
          <Button variant="ghost" className="w-full" asChild>
            <a href="/admin/">Not now</a>
          </Button>
        </>
      )}

      {state === "asking" && !preview.error && !preview.data?.confirm_email && (
        <>
          {preview.loading ? (
            <p className="flex items-center gap-2 text-sm text-muted-foreground">
              <Loader2 className="size-4 animate-spin" /> Reading the invitation…
            </p>
          ) : (
            <>
              <AuthNotice kind="info">
                Accepting adds{inviting ? <> <strong className="font-medium">{inviting}</strong></> : " this organisation"} to
                the account you are signed in with. Same email, same password — it opens there
                afterwards.
              </AuthNotice>

              {/* A shared link was not addressed to anybody, so nothing about holding it says it
                  was meant for this account. Worth saying plainly before somebody attaches their
                  account to an organisation on the strength of a link they were forwarded. */}
              {preview.data?.shared && (
                <AuthNotice kind="info">
                  This is a shared invitation link rather than one sent to you. If you were not
                  expecting it, close this page — nothing happens until you accept.
                </AuthNotice>
              )}

              {canLeave && (
                <div className="flex items-start gap-2.5 rounded-lg border bg-muted/40 px-3 py-2.5">
                  <Checkbox
                    id="leave-own"
                    checked={leaveOwn}
                    onCheckedChange={(v) => setLeaveOwn(v === true)}
                    className="mt-0.5"
                  />
                  {/* A plain label, not the shared one: that is a flex row, and a sentence with a
                      name in bold laid out as flex items becomes a column of one word each. */}
                  <label
                    htmlFor="leave-own"
                    className="block text-xs font-normal leading-relaxed text-muted-foreground"
                  >
                    Also leave{" "}
                    <strong className="font-medium text-foreground">{own?.name}</strong>, the
                    organisation you made when you signed up. You are its only member and nothing
                    has been set up in it, so it would only sit in your switcher. It is not
                    deleted — you simply come out of it.
                  </label>
                </div>
              )}

              <Button className="w-full" onClick={() => setState("working")}>
                Accept
              </Button>
              <Button variant="ghost" className="w-full" asChild>
                <a href="/admin/">Not now</a>
              </Button>
            </>
          )}
        </>
      )}

      {state === "working" && (
        <p className="flex items-center gap-2 text-sm text-muted-foreground">
          <Loader2 className="size-4 animate-spin" /> Joining…
        </p>
      )}
      {state === "failed" && <AuthNotice kind="error">{failure}</AuthNotice>}
      {state === "done" && (
        <>
          <p className="flex items-center gap-2 text-sm">
            <CheckCircle2 className="size-4 text-success" /> Joined.
          </p>
          {left && <p className="text-xs text-muted-foreground">You have left {left}.</p>}
          {/* A full navigation: the session now names a different organisation, so every page
              behind this has to be fetched rather than rendered from the state of this one. */}
          <Button className="w-full" asChild>
            <a href="/admin/">Go to the console</a>
          </Button>
        </>
      )}
    </AuthLayout>
  );
}
