"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { AboutPanel } from "@/components/auth/about-panel";
import { BrandMark } from "@/components/shell/brand-mark";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { api, errorMessage } from "@/lib/api";
import { browserZone } from "@/lib/format";
import { cn } from "@/lib/utils";

// The frame every signed-out screen sits in. One place, so sign-in, sign-up and the recovery
// flows cannot drift apart in spacing, heading level or where the brand appears.
//
// The card comes first in the DOM and `row-reverse` moves it right once there is room: on a
// phone the form is the only thing above the fold, and a keyboard or screen reader reaches it
// before the marketing copy rather than after it.
export function AuthLayout({
  title,
  subtitle,
  children,
  footer,
  wide,
}: {
  title: string;
  subtitle?: string;
  children: React.ReactNode;
  footer?: React.ReactNode;
  /** Show the product overview beside the form. Off for the short recovery screens. */
  wide?: boolean;
}) {
  return (
    <div className="flex min-h-dvh items-center justify-center bg-background px-4 py-10 lg:px-10">
      <div
        className={
          wide
            ? "flex w-full max-w-5xl flex-col items-center gap-8 lg:flex-row-reverse lg:justify-center lg:gap-12"
            : "flex w-full max-w-sm flex-col items-center gap-8"
        }
      >
        <Card className="w-full max-w-sm shrink-0">
          <CardHeader>
            <div className="flex items-center gap-3 lg:hidden">
              <BrandMark size="lg" />
              <div>
                <CardTitle>attest_tag</CardTitle>
                {subtitle && <p className="text-xs text-muted-foreground">{subtitle}</p>}
              </div>
            </div>
            <div className="hidden lg:block">
              <CardTitle>{title}</CardTitle>
              {subtitle && <p className="text-xs text-muted-foreground">{subtitle}</p>}
            </div>
          </CardHeader>
          <CardContent className="space-y-4">{children}</CardContent>
        </Card>
        {footer && <div className="text-center text-xs text-muted-foreground">{footer}</div>}
        {wide && <AboutPanel />}
      </div>
    </div>
  );
}

/** A message the auth screens show in place of a field-level error. */
// Three readings, not two shades of the same one: "error" is something that went wrong, "info"
// is background the reader may ignore, and "action" is the thing they have to do before the
// screen will go on. Red for the last one says a mistake was made when none was — nothing is
// broken, there is simply a step left — so it takes the blue the console already uses to mean
// "this is worth your attention".
const noticeStyles = {
  error: "border-danger/30 bg-danger-soft text-danger",
  action: "border-info/30 bg-info-soft text-info",
  info: "border-border bg-muted/40 text-muted-foreground",
};

export function AuthNotice({
  kind,
  children,
}: {
  kind: keyof typeof noticeStyles;
  children: React.ReactNode;
}) {
  return (
    <div
      role={kind === "error" ? "alert" : undefined}
      className={cn("rounded-lg border px-3 py-2 text-xs", noticeStyles[kind])}
    >
      {children}
    </div>
  );
}

/** The link between sign-in and sign-up, so neither screen is a dead end. */
export function AuthSwitch({ href, prompt, action }: { href: string; prompt: string; action: string }) {
  return (
    <p className="text-center text-xs text-muted-foreground">
      {prompt}{" "}
      <Link href={href} className="font-medium text-foreground underline underline-offset-2">
        {action}
      </Link>
    </p>
  );
}

// Slack's four-dot mark, drawn in the button's current colour so it sits quietly in an outline
// button rather than shouting in brand colours.
export function SlackGlyph() {
  return (
    <svg viewBox="0 0 24 24" aria-hidden className="size-4" fill="currentColor">
      <path d="M5.04 15.16a2.52 2.52 0 1 1-2.52-2.52h2.52v2.52zm1.27 0a2.52 2.52 0 0 1 5.04 0v6.32a2.52 2.52 0 1 1-5.04 0v-6.32zM8.83 5.04a2.52 2.52 0 1 1 2.52-2.52v2.52H8.83zm0 1.27a2.52 2.52 0 0 1 0 5.04H2.52a2.52 2.52 0 1 1 0-5.04h6.31zM18.96 8.83a2.52 2.52 0 1 1 2.52 2.52h-2.52V8.83zm-1.27 0a2.52 2.52 0 0 1-5.04 0V2.52a2.52 2.52 0 1 1 5.04 0v6.31zM15.17 18.96a2.52 2.52 0 1 1-2.52 2.52v-2.52h2.52zm0-1.27a2.52 2.52 0 0 1 0-5.04h6.31a2.52 2.52 0 1 1 0 5.04h-6.31z" />
    </svg>
  );
}

/**
 * The Slack sign-in link. It carries the browser's time zone, which the callback keeps as the
 * organisation's zone when this sign-in is the one that founds it — the zone is read after
 * mount, so until then the link is the plain one and Slack sign-in still works without it.
 */
export function useSlackLoginHref(): string {
  const [zone, setZone] = useState("");
  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect -- prerendered page: see browserZone
    setZone(browserZone());
  }, []);
  return zone ? `/api/auth/login?tz=${encodeURIComponent(zone)}` : "/api/auth/login";
}

// Microsoft's four squares, in the button's current colour like SlackGlyph: the two sign-in
// buttons sit one above the other, and one of them in brand colours read as the favoured one.
export function MicrosoftGlyph() {
  return (
    <svg viewBox="0 0 24 24" aria-hidden className="size-4" fill="currentColor">
      <path d="M2 2h9.5v9.5H2zM12.5 2H22v9.5h-9.5zM2 12.5h9.5V22H2zM12.5 12.5H22V22h-9.5z" />
    </svg>
  );
}

/** The Microsoft sign-in link, carrying the browser's zone for the same reason Slack's does. */
export function useMicrosoftLoginHref(): string {
  const [zone, setZone] = useState("");
  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect -- prerendered page: see browserZone
    setZone(browserZone());
  }, []);
  return zone ? `/api/auth/microsoft/login?tz=${encodeURIComponent(zone)}` : "/api/auth/microsoft/login";
}

/**
 * Starting a single sign-on, shared by the sign-in and sign-up forms because it is the same
 * walk from both: an address goes to the server, and what comes back is the authorize URL of
 * whichever identity provider has proved it owns that domain.
 *
 * Deliberately offered on every deployment rather than behind a "does anybody here use SSO"
 * flag. That flag was true as soon as any *other* organisation had verified a domain, which is
 * the wrong question — and answering it to a signed-out visitor gives away the same fact that
 * /api/auth/sso/start is written not to give away, which domains have single sign-on. An
 * address with no provider gets a plain refusal from the server, which is a better answer than
 * a button that appears for reasons the person cannot see.
 *
 * `start` resolves to a message to show, or to null when it has navigated away.
 */
export function useSSOStart(): { busy: boolean; start: (email: string) => Promise<string | null> } {
  const [busy, setBusy] = useState(false);

  const start = async (email: string): Promise<string | null> => {
    if (!email.trim()) {
      document.getElementById("email")?.focus();
      return "Type your work email address first — it is what says which provider to use.";
    }
    setBusy(true);
    try {
      const out = await api.post<{ url: string }>("/api/auth/sso/start", { email });
      // Leaving our origin for the identity provider, so a full navigation rather than a push.
      window.location.href = out.url;
      return null;
    } catch (err) {
      setBusy(false);
      return errorMessage(err);
    }
  };

  return { busy, start };
}

/** Reads ?auth_error= once, shows it, and takes it out of the address bar. */
export function useAuthError(): string | null {
  return useOneShotParam("auth_error");
}

export function useOneShotParam(name: string): string | null {
  const [value, setValue] = useState<string | null>(null);
  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    const v = params.get(name);
    if (!v) return;
    // Read after mount, not during render: this page is prerendered at build time, where there
    // is no location to read.
    // eslint-disable-next-line react-hooks/set-state-in-effect -- one-shot URL read
    setValue(v);
    params.delete(name);
    const qs = params.toString();
    window.history.replaceState(null, "", window.location.pathname + (qs ? `?${qs}` : ""));
  }, [name]);
  return value;
}
