"use client";

import { useEffect, useState } from "react";
import { AlertCircle, Download } from "lucide-react";
import { ErrorBanner } from "@/components/core/error-banner";
import { PageHeader } from "@/components/core/page-header";
import { SegmentedControl } from "@/components/core/segmented-control";
import { TableSkeleton } from "@/components/core/table-skeleton";
import { AssistantTurnsTable } from "@/components/activity/assistant-turns-table";
import { PeopleTable } from "@/components/activity/people-table";
import { ProxyTable } from "@/components/activity/proxy-table";
import { ToolCallsTable } from "@/components/activity/tool-calls-table";
import { TurnsTable } from "@/components/activity/turns-table";
import { Button } from "@/components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  useApi,
  type ActiveUsersResponse,
  type Activity,
  type AssistantTurnRow,
  type Scope,
} from "@/lib/api";

const ALL = "__all__";

type Tab = "turns" | "tools" | "proxy" | "assistant" | "people";

/** The windows the overview counts in: a tile links here with the one it counted. */
type Range = "all" | "today" | "7d" | "30d" | "month";

const RANGES: { value: Range; label: string }[] = [
  { value: "all", label: "All time" },
  { value: "today", label: "Today" },
  { value: "7d", label: "Last 7 days" },
  { value: "30d", label: "Last 30 days" },
  { value: "month", label: "This month" },
];

// Every tab, and it has to BE every tab: pickTab refuses anything this does not recognise, so a
// tab missing from here is one that cannot be clicked. "assistant" was missing, which is exactly
// what that looked like from the outside — the tab took focus and nothing happened.
function isTab(value: string | null): value is Tab {
  return (
    value === "turns" ||
    value === "tools" ||
    value === "proxy" ||
    value === "assistant" ||
    value === "people"
  );
}

function isRange(value: string | null): value is Range {
  return RANGES.some((r) => r.value === value);
}

/** What the header promises, which is the window and the filter the reader arrived with. */
function describe(range: Range, errorsOnly: boolean): string {
  const what = errorsOnly
    ? "failed tool calls, and requests that were blocked or answered with an error"
    : "turns, tool calls and proxied requests";
  switch (range) {
    case "today":
      return `Today's ${what} \u2014 the most recent 200.`;
    case "7d":
      return `The last 7 days of ${what} \u2014 the most recent 200.`;
    case "month":
      return `This month's ${what} \u2014 the most recent 200.`;
    default:
      return `The last 200 ${what}.`;
  }
}

export function ActivityPage() {
  const scopes = useApi<Scope[]>("/api/scopes?sync=0");
  const [channel, setChannel] = useState<string>(ALL);
  const [tab, setTab] = useState<Tab>("turns");
  const [errorsOnly, setErrorsOnly] = useState(false);
  const [range, setRange] = useState<Range>("all");
  // The tool call the reader was sent here to look at; 0 when they came on their own.
  const [focusCall, setFocusCall] = useState(0);

  // How the overview links land here: ?channel= from a channel row, ?range= from a tile so the
  // rows are the window that tile counted, and ?tab=tools&errors=1&call= from one of its recent
  // errors, which arrives filtered to failures with that call open.
  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    const fromUrl = params.get("channel");
    const failures = params.get("errors") === "1";
    const wanted = params.get("tab");
    const window_ = params.get("range");
    /* eslint-disable react-hooks/set-state-in-effect -- one-shot URL read on mount */
    if (fromUrl) setChannel(fromUrl);
    if (failures) setErrorsOnly(true);
    if (isRange(window_)) setRange(window_);
    setFocusCall(Number(params.get("call")) || 0);
    if (isTab(wanted)) setTab(failures && wanted === "turns" ? "tools" : wanted);
    else if (failures) setTab("tools");
    /* eslint-enable react-hooks/set-state-in-effect */
  }, []);

  const query = new URLSearchParams({ limit: "200" });
  if (channel !== ALL) query.set("channel", channel);
  if (errorsOnly) query.set("errors", "1");
  if (range !== "all") query.set("range", range);
  const activity = useApi<Activity>(`/api/activity?${query.toString()}`);
  // Its own endpoint, fetched only when the tab is open: the transcript is prose and there is no
  // reason for every visit to Activity to carry it.
  // These two have their own endpoints, and they are fetched whichever tab is open rather than
  // only when theirs is: the count in a tab's label is the reason to click it, and a label that
  // only learns its number after you have clicked is no use for deciding to. Both are cheap —
  // one indexed read each, and the name lookups behind the second are cached and time-boxed.
  const assistant = useApi<AssistantTurnRow[]>("/api/assistant/turns?limit=100");
  // The rows behind the billing tile. Its own fixed window — the size is judged over 30 days
  // whatever the range control says, so letting that control narrow this list would show a
  // number that disagrees with the one that sent the reader here.
  const people = useApi<ActiveUsersResponse>("/api/active-users?limit=500");

  // The export is the view, not the whole table: same channel, same window.
  const csv = new URLSearchParams();
  if (channel !== ALL) csv.set("channel", channel);
  if (range !== "all") csv.set("range", range);
  const csvQuery = csv.toString();
  const csvHref = csvQuery ? `/api/activity.csv?${csvQuery}` : "/api/activity.csv";

  // The address bar keeps the view, so a reload or a shared link comes back to it.
  const syncUrl = (next: {
    channel: string;
    tab: Tab;
    errors: boolean;
    call: number;
    range: Range;
  }) => {
    const url = new URL(window.location.href);
    const set = (key: string, value: string) =>
      value ? url.searchParams.set(key, value) : url.searchParams.delete(key);
    set("channel", next.channel === ALL ? "" : next.channel);
    set("tab", next.tab === "turns" ? "" : next.tab);
    set("errors", next.errors ? "1" : "");
    set("call", next.call ? String(next.call) : "");
    set("range", next.range === "all" ? "" : next.range);
    window.history.replaceState(null, "", url);
  };

  const pickChannel = (value: string) => {
    setChannel(value);
    setFocusCall(0);
    syncUrl({ channel: value, tab, errors: errorsOnly, call: 0, range });
  };

  const pickRange = (value: string) => {
    if (!isRange(value)) return;
    setRange(value);
    // The call the reader was sent to may be outside the new window; nothing to focus then.
    setFocusCall(0);
    syncUrl({ channel, tab, errors: errorsOnly, call: 0, range: value });
  };

  const pickTab = (value: string) => {
    if (!isTab(value)) return;
    setTab(value);
    syncUrl({ channel, tab: value, errors: errorsOnly, call: focusCall, range });
  };

  // A turn has no failed state, so the filter moves off that tab rather than emptying it.
  const pickFilter = (value: string) => {
    const failures = value === "errors";
    const next: Tab = failures && tab === "turns" ? "tools" : tab;
    setErrorsOnly(failures);
    setTab(next);
    setFocusCall(0);
    syncUrl({ channel, tab: next, errors: failures, call: 0, range });
  };

  const channels = (scopes.data ?? []).filter((s) => s.kind === "channel");
  const counts = {
    turns: activity.data?.turns?.length ?? 0,
    tools: activity.data?.tool_calls?.length ?? 0,
    proxy: activity.data?.proxy?.length ?? 0,
    assistant: assistant.data?.length ?? 0,
    // The deduped figure rather than the row count: somebody in two workspaces is two lines and
    // one person, and this badge has to agree with the tile on the Billing screen that sent
    // them here.
    people: people.data?.count?.users ?? 0,
  };

  return (
    <div className="space-y-5">
      <PageHeader
        title="Activity"
        description={describe(range, errorsOnly)}
        actions={
          // Not on the People tab: that list comes from its own endpoint, and a button offering
          // "Export CSV" while handing back turns would be the wrong file under the right name.
          tab === "people" ? null : (
            <Button variant="outline" asChild>
              <a href={csvHref}>
                <Download className="size-4" />
                Export CSV
              </a>
            </Button>
          )
        }
      />

      {activity.error && !activity.data && (
        <ErrorBanner message={activity.error} onRetry={activity.reload} />
      )}

      <Tabs value={tab} onValueChange={pickTab}>
        <div className="flex flex-wrap items-center justify-between gap-3">
          <TabsList>
            <TabsTrigger
              value="turns"
              disabled={errorsOnly}
              title={errorsOnly ? "A turn has no failed state" : undefined}
            >
              Turns <Count n={counts.turns} />
            </TabsTrigger>
            <TabsTrigger value="tools">
              Tool calls <Count n={counts.tools} />
            </TabsTrigger>
            <TabsTrigger value="proxy">
              Proxy <Count n={counts.proxy} />
            </TabsTrigger>
            <TabsTrigger
              value="assistant"
              disabled={errorsOnly}
              title={errorsOnly ? "A console question has no failed filter here" : undefined}
            >
              Assistant <Count n={counts.assistant} />
            </TabsTrigger>
            <TabsTrigger
              value="people"
              disabled={errorsOnly}
              title={errorsOnly ? "A person has no failed state" : undefined}
            >
              People <Count n={counts.people} />
            </TabsTrigger>
          </TabsList>
          <div className="flex items-center gap-2">
            <SegmentedControl
              value={errorsOnly ? "errors" : "all"}
              onValueChange={pickFilter}
              options={[
                { value: "all", label: "All" },
                {
                  value: "errors",
                  label: "Errors",
                  icon: <AlertCircle className="size-3.5" />,
                },
              ]}
            />
            <Select value={range} onValueChange={pickRange}>
              <SelectTrigger className="w-40" aria-label="Filter by time">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {RANGES.map((r) => (
                  <SelectItem key={r.value} value={r.value}>
                    {r.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Select value={channel} onValueChange={pickChannel}>
              <SelectTrigger className="w-56" aria-label="Filter by channel">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={ALL}>All channels</SelectItem>
                {channels.map((s) => (
                  <SelectItem key={s.id} value={s.slack_id}>
                    {s.name}
                  </SelectItem>
                ))}
                {channel !== ALL && !channels.some((s) => s.slack_id === channel) && (
                  <SelectItem value={channel}>{channel}</SelectItem>
                )}
              </SelectContent>
            </Select>
          </div>
        </div>

        <TabsContent value="turns">
          {activity.loading || !activity.data ? (
            <TableSkeleton rows={8} columns={6} />
          ) : (
            <TurnsTable rows={activity.data.turns ?? []} />
          )}
        </TabsContent>
        <TabsContent value="tools">
          {activity.loading || !activity.data ? (
            <TableSkeleton rows={8} columns={5} />
          ) : (
            <ToolCallsTable
              rows={activity.data.tool_calls ?? []}
              failedOnly={errorsOnly}
              focus={focusCall}
            />
          )}
        </TabsContent>
        <TabsContent value="people">
          {people.loading || !people.data ? (
            <TableSkeleton rows={8} columns={5} />
          ) : (
            <PeopleTable data={people.data} />
          )}
        </TabsContent>
        <TabsContent value="assistant">
          {assistant.loading || !assistant.data ? (
            <TableSkeleton rows={6} columns={3} />
          ) : (
            <AssistantTurnsTable rows={assistant.data} />
          )}
        </TabsContent>
        <TabsContent value="proxy">
          {activity.loading || !activity.data ? (
            <TableSkeleton rows={8} columns={7} />
          ) : (
            <ProxyTable rows={activity.data.proxy ?? []} failedOnly={errorsOnly} />
          )}
        </TabsContent>
      </Tabs>
    </div>
  );
}

function Count({ n }: { n: number }) {
  return (
    <span className="ml-1 rounded-sm bg-foreground/5 px-1 text-[10px] tabular-nums text-muted-foreground">
      {n}
    </span>
  );
}
