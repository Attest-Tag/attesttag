"use client";

import { useEffect } from "react";
import { usePathname, useRouter } from "next/navigation";
import { AssistantPanel } from "@/components/assistant/assistant-panel";
import { AssistantProvider } from "@/components/assistant/assistant-provider";
import { AppSidebar } from "@/components/shell/app-sidebar";
import { useAuth } from "@/components/shell/auth-provider";
import { BudgetBanner } from "@/components/shell/budget-banner";
import { ModelKeyBanner } from "@/components/shell/model-key-banner";
import { Topbar } from "@/components/shell/topbar";
import { TwoFactorGate } from "@/components/shell/two-factor-gate";
import { BrandMark } from "@/components/shell/brand-mark";
import { SidebarInset, SidebarProvider } from "@/components/ui/sidebar";
import { Skeleton } from "@/components/ui/skeleton";
import { useApi, type Onboarding } from "@/lib/api";
import { takeReturn } from "@/lib/return-to";

// The routes that are their own page rather than something inside the console: signing in,
// signing up, and the three links that arrive by email. They render bare — no rail, no topbar,
// and no redirect to sign in, because they ARE the signing in. The help pages join them: they are
// sent to people with no account here, like the Teams admin who has to upload the app. So does the
// MCP consent screen, which an app opens in a browser tab and which signs the person in itself,
// so that it can bring them back afterwards (lib/return-to.ts).
const SIGNED_OUT_ROUTES = ["/login", "/signup", "/forgot", "/reset", "/verify", "/join", "/help", "/oauth"];

// The setup walk. Signed in, but drawn without the rail and the topbar, because until a Slack
// workspace is connected there is nothing for them to navigate to.
const ONBOARDING = "/onboarding";

function isRoute(pathname: string, route: string) {
  return pathname === route || pathname.startsWith(route + "/");
}

// Signed-in frame: rail + topbar + scrolling content. While /api/me is in
// flight a quiet skeleton holds the page; signed out, the browser goes to the
// sign-in page rather than showing a card in place of whatever was asked for,
// so the address bar says where you actually are.
export function AppShell({ children }: { children: React.ReactNode }) {
  const { me, loading } = useAuth();
  const pathname = usePathname();
  const router = useRouter();
  const standalone = SIGNED_OUT_ROUTES.some((p) => isRoute(pathname, p));
  const onboarding = isRoute(pathname, ONBOARDING);

  // Where this organisation stands on the setup walk. Asked once per page load, and only of a
  // session that could act on the answer: it is two counts on the server, and it decides whether
  // any of the console is worth drawing.
  const signedIn = !standalone && !loading && !!me?.signed_in && !me.two_factor?.owed;
  const walk = useApi<Onboarding>(signedIn && !onboarding ? "/api/onboarding" : null);

  // Nothing connected means every page behind this is an empty listing and every number a zero,
  // so the console holds people on the walk rather than letting them wander it. Only the first
  // step gates: once a workspace is in, the rest of the setup is worth finishing but not worth
  // blocking on, and the walk offers its own way out.
  const held = walk.data?.step === "install";
  useEffect(() => {
    if (held) router.replace(ONBOARDING);
  }, [held, router]);

  // Signed in with somewhere to go back to: the MCP consent screen sent them to sign in first.
  useEffect(() => {
    if (!signedIn) return;
    const back = takeReturn();
    if (back) window.location.replace(back);
  }, [signedIn]);

  if (standalone) return <>{children}</>;

  if (loading) {
    return (
      <div className="flex h-dvh items-center justify-center">
        <div className="flex items-center gap-3">
          <BrandMark />
          <Skeleton className="h-4 w-32" />
        </div>
      </div>
    );
  }

  if (!me?.signed_in) {
    // A full navigation, not a router push: the session cookie is set by the server, so the
    // next page has to be fetched rather than rendered from the state of this one.
    if (typeof window !== "undefined") window.location.replace("/admin/login/");
    return null;
  }

  // A second factor the organisation requires and this account has not enrolled: the console is
  // held here until it has one. The same check runs on every API route, so there is nothing
  // behind this screen to show anyway.
  if (me.two_factor?.owed) return <TwoFactorGate />;

  // Its own frame, like the two-factor gate: the walk is what stands in front of the console,
  // not a page inside it.
  if (onboarding) return <>{children}</>;

  // Held on the walk: the redirect above is in flight, and drawing an empty console for the
  // frame or two until it lands would be a worse answer than drawing nothing.
  if (held) return null;

  return (
    <AssistantProvider>
      <SidebarProvider
        className="h-dvh overflow-clip"
        style={{ "--sidebar-width": "15rem" } as React.CSSProperties}
      >
        <AppSidebar />
        <SidebarInset className="flex min-w-0 flex-col overflow-clip">
          <Topbar />
          {/* Above the scrolling content, so "the bot has stopped" does not scroll away from
              somebody halfway down a page wondering why Slack went quiet. */}
          <ModelKeyBanner />
          <BudgetBanner />
          <div className="flex-1 overflow-y-auto [scrollbar-gutter:stable]">
            <div className="mx-auto w-full max-w-6xl px-5 py-4">{children}</div>
          </div>
        </SidebarInset>
        {/* A sibling of the inset rather than something inside the routed page: it is mounted
            once and outlives every navigation, which is what lets a conversation carry across
            pages — and why it reads its page context from the address bar. */}
        <AssistantPanel />
      </SidebarProvider>
    </AssistantProvider>
  );
}
