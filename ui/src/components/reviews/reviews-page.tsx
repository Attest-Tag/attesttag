"use client";

import { useCallback, useEffect, useState } from "react";
import { GitPullRequest } from "lucide-react";
import { EmptyState } from "@/components/core/empty-state";
import { PageHeader } from "@/components/core/page-header";
import { CodeReviewPlanNotice, usePlanRefusal } from "@/components/reviews/review-access";
import { ReviewHistoryTab } from "@/components/reviews/review-history-tab";
import { ReviewSettingsTab } from "@/components/reviews/review-settings-tab";
import { ReviewTypesTab } from "@/components/reviews/review-types-tab";
import { NavContext, REVIEW_TABS, isReviewTab, type ReviewTab, type ReviewsNav } from "@/components/reviews/reviews-nav";
import { useAuth } from "@/components/shell/auth-provider";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { recallTab, rememberTab } from "@/hooks/use-sticky-tab";
import { onConsoleChange } from "@/lib/api";
import { announceReviewsSelection } from "@/lib/assistant-screen";

// Automation › Reviews: History, Settings and Types. The tab rides on ?tab= so a link can name
// one — Access bundles' "Code review settings…" lands on Settings with its repository selected —
// and the last one picked is remembered in this browser for a visit that names none. A link that
// names a node of the settings tree (?node=) and no tab came for Settings all the same: the node is
// only Settings' to show, and History, remembered from last time, would hide what it pointed at.
//
// Where the deployment has no code review (CODE_REVIEW=off) the rail has no entry for it and its
// routes answer 404, so an address typed by hand gets a page that says so rather than three tabs of
// errors. Where the organisation's plan has none, the tabs still read — what was set and reviewed
// before is still the organisation's — under a strip that says which plan has it.
//
// The address is also what the console assistant is told the page has open (assistant-screen.ts):
// so the tab is always in it, a remembered one included, and every change to it is announced for
// the panel's chip. A card confirmed or opened in the panel hands the page what to open as the same
// query parameters (CONSOLE_CHANGED_EVENT's select), since a link to the route already on screen
// would change the address and leave every tab as it was.

const LABELS: Record<ReviewTab, string> = { history: "History", settings: "Settings", types: "Types" };

export function ReviewsPage() {
  const { me } = useAuth();
  const off = me?.code_review?.reason === "off";
  const refused = usePlanRefusal();
  const [tab, setTab] = useState<ReviewTab>("history");

  // The URL names the tab when it names one, or a settings node; otherwise this browser's last pick
  // does, and is written into the URL so what the assistant reads from there is the tab on screen.
  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    const wanted = params.get("tab");
    const next = isReviewTab(wanted) ? wanted : params.has("node") ? "settings" : recallTab("reviews");
    if (!isReviewTab(next)) return;
    if (next !== wanted) {
      const url = new URL(window.location.href);
      url.searchParams.set("tab", next);
      window.history.replaceState(null, "", url);
      announceReviewsSelection();
    }
    // eslint-disable-next-line react-hooks/set-state-in-effect -- one-shot URL read on mount
    setTab(next);
  }, []);

  const go = useCallback<ReviewsNav>((next, params) => {
    const url = new URL(window.location.href);
    // What one tab put in the address means nothing to another: only the tab, and what the
    // caller hands over, survive the move.
    url.search = "";
    url.searchParams.set("tab", next);
    for (const [k, v] of Object.entries(params ?? {})) url.searchParams.set(k, v);
    window.history.replaceState(null, "", url);
    rememberTab("reviews", next);
    setTab(next);
    announceReviewsSelection();
  }, []);

  // What a card in the assistant names: its tab here, and the rest to that tab — read from the
  // address by one that mounts, handed over by one already open (Types and Settings listen too).
  useEffect(
    () =>
      onConsoleChange(({ select }) => {
        const { tab: next = null, ...params } = select ?? {};
        if (isReviewTab(next)) go(next, params);
      }),
    [go],
  );

  const pick = (value: string) => {
    if (!isReviewTab(value)) return;
    const url = new URL(window.location.href);
    url.searchParams.set("tab", value);
    window.history.replaceState(null, "", url);
    rememberTab("reviews", value);
    setTab(value);
    announceReviewsSelection();
  };

  return (
    <NavContext.Provider value={go}>
      <div className="space-y-5">
        <PageHeader
          title="Reviews"
          description="Pull requests reviewed by the bot: what each review found and cost, the settings that decide which pull requests are reviewed and where the result goes, and the review types they run."
        />
        {off ? (
          <EmptyState
            icon={GitPullRequest}
            title="Code review is not available here"
            description={`${me?.code_review?.message ?? "Code review is not available on this deployment."} Whoever runs it switches it on with CODE_REVIEW on the server.`}
          />
        ) : (
          <>
            {refused && <CodeReviewPlanNotice access={refused} />}
            <Tabs value={tab} onValueChange={pick} className="space-y-5">
              {/* Underlined, as on Settings: three headings over one panel rather than a block of buttons. */}
              <TabsList
                variant="line"
                className="h-auto w-full justify-start gap-x-6 rounded-none border-b border-border p-0 pb-[5px] group-data-[orientation=horizontal]/tabs:h-auto [&>button]:after:bg-primary"
              >
                {REVIEW_TABS.map((t) => (
                  <TabsTrigger key={t} value={t} className="flex-none px-0.5 pt-1 pb-1 data-[state=active]:text-foreground">
                    {LABELS[t]}
                  </TabsTrigger>
                ))}
              </TabsList>
              <TabsContent value="history">
                <ReviewHistoryTab />
              </TabsContent>
              <TabsContent value="settings">
                <ReviewSettingsTab />
              </TabsContent>
              <TabsContent value="types">
                <ReviewTypesTab />
              </TabsContent>
            </Tabs>
          </>
        )}
      </div>
    </NavContext.Provider>
  );
}
