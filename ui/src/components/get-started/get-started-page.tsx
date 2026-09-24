"use client";

import * as React from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { ListVideo, Play } from "lucide-react";
import { PageHeader } from "@/components/core/page-header";
import { SearchField } from "@/components/core/search-field";
import { useAuth } from "@/components/shell/auth-provider";
import { cn } from "@/lib/utils";

// The console's video library: the same walkthroughs as the marketing site's
// /walkthroughs, fetched from it, so there is one set of recordings and one
// set of measured chapter offsets.
//
// One page, not two. The console is a static export served by the Go binary,
// and a static export cannot have a /get-started/[id] route whose ids are only
// known at runtime — so `?id=` selects a walkthrough and the player renders in
// place of the library. Everything else about the screen is the site's: rows
// grouped by section, a search that also matches chapter names, a contents
// rail.

// The walkthrough recordings are hosted with the marketing site, not in this binary — they are
// several hundred megabytes of video. SITE_URL names the site that serves them and the server
// hands it to us on /api/me, rather than the console baking it in at build time: the same value
// is what secureHeaders puts in connect-src, so the origin this page asks and the origin the
// browser permits are one setting instead of two that have to be kept in agreement.
//
// Empty is the default: a self-hosted console must not reach out to somebody else's domain to
// draw one of its own pages, which would both break air-gapped deployments and tell that domain
// who is using the console. Unset, the page says where the guides live instead of fetching them.

type Chapter = { title: string; startSeconds: number; seconds: number };
type Walkthrough = {
  id: string;
  section: string;
  title: string;
  kicker: string;
  summary: string;
  durationSeconds: number;
  renderedAt: string;
  videoSrc: string;
  posterSrc: string;
  pageUrl: string;
  chapters: Chapter[];
};

function timecode(total: number): string {
  const s = Math.max(0, Math.round(total));
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60;
  const pad = (n: number) => String(n).padStart(2, "0");
  return h ? `${h}:${pad(m)}:${pad(sec)}` : `${m}:${pad(sec)}`;
}

const anchorId = (section: string) => `section-${section.toLowerCase().replace(/[^a-z0-9]+/g, "-")}`;

export function GetStartedPage() {
  const [fetched, setFetched] = React.useState<Walkthrough[] | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  const params = useSearchParams();
  const router = useRouter();
  const selectedId = params.get("id");
  const { me } = useAuth();
  const site = me?.site_url ?? "";

  React.useEffect(() => {
    if (!site) return; // nothing to ask: either /api/me has not landed, or there is no site
    let cancelled = false;
    fetch(`${site}/api/walkthroughs`, { cache: "no-store" })
      .then(async (r) => {
        if (!r.ok) throw new Error(`${r.status} from ${site}`);
        const body = (await r.json()) as { walkthroughs: Walkthrough[] };
        if (!cancelled) setFetched(body.walkthroughs);
      })
      .catch((e) => !cancelled && setError(String(e.message ?? e)));
    return () => {
      cancelled = true;
    };
  }, [site]);

  // Three states, derived rather than stored: null while /api/me is still in flight — not
  // knowing whether there is a site is not the same as knowing there is none — then the empty
  // library once it says there is none, and otherwise whatever the site sent. Memoised because
  // the empty case is a fresh array, and the grouping below takes this as a dependency.
  const all = React.useMemo(() => (me ? (site ? fetched : []) : null), [me, site, fetched]);

  const selected = all?.find((w) => w.id === selectedId) ?? null;
  const startAt = Number(params.get("t")) || 0;

  // Every hook above the first return: the same instance renders the library
  // and, after `?id=` lands, the player, and a hook that only runs in one of
  // the two is the "rendered fewer hooks than expected" crash on the switch.
  const sections = React.useMemo(() => {
    const order = ["Deep dives", "In Slack", "The console"];
    const groups = new Map<string, Walkthrough[]>();
    for (const w of all ?? []) groups.set(w.section, [...(groups.get(w.section) ?? []), w]);
    return [...groups.entries()]
      .sort((a, b) => order.indexOf(a[0]) - order.indexOf(b[0]))
      .map(([section, walkthroughs]) => ({ section, walkthroughs }));
  }, [all]);

  if (selected) {
    return (
      <div className="space-y-4">
        <PageHeader
          backHref="/get-started"
          backLabel="Get started"
          title={selected.title}
          description={`${timecode(selected.durationSeconds)} · ${selected.chapters.length} chapters · recorded ${selected.renderedAt}`}
        />
        <Player w={selected} startAt={startAt} />
      </div>
    );
  }

  const total = all?.reduce((n, w) => n + w.durationSeconds, 0) ?? 0;

  return (
    <div className="mx-auto max-w-6xl space-y-4">
      <PageHeader
        title="Get started"
        description={
          all && all.length === 0
            ? "The recorded walkthroughs are served with the project's website, which this deployment is not configured to reach. Set SITE_URL to the site that serves them to show them here; the README covers the same ground in writing."
            : all
              ? `${all.length} walkthrough${all.length === 1 ? "" : "s"} of how the whole thing works — ${timecode(total)} in total. Start with a deep dive for the whole picture, or search for the one thing you came for.`
              : error
                ? `Could not load the walkthroughs (${error}).`
                : "Loading the walkthroughs…"
        }
      />
      {all && all.length > 0 && (
        <Library
          groups={sections}
          onOpen={(id, t) => router.push(`/get-started?id=${id}${t ? `&t=${t}` : ""}`)}
        />
      )}
    </div>
  );
}

function matchOne(w: Walkthrough, q: string) {
  const chapters = w.chapters.filter((c) => c.title.toLowerCase().includes(q));
  const matches =
    chapters.length > 0 ||
    w.title.toLowerCase().includes(q) ||
    w.summary.toLowerCase().includes(q) ||
    w.section.toLowerCase().includes(q);
  return { matches, chapters };
}

function Library({
  groups,
  onOpen,
}: {
  groups: { section: string; walkthroughs: Walkthrough[] }[];
  onOpen: (id: string, t?: number) => void;
}) {
  const [query, setQuery] = React.useState("");
  const [active, setActive] = React.useState(groups[0]?.section ?? null);
  const q = query.trim().toLowerCase();

  const visible = React.useMemo(() => {
    if (!q) return groups.map((g) => ({ section: g.section, rows: g.walkthroughs.map((w) => ({ w, chapters: [] as Chapter[] })) }));
    return groups
      .map((g) => ({ section: g.section, rows: g.walkthroughs.map((w) => ({ w, ...matchOne(w, q) })).filter((r) => r.matches) }))
      .filter((g) => g.rows.length > 0);
  }, [groups, q]);

  const hits = visible.reduce((n, g) => n + g.rows.length, 0);
  const total = groups.reduce((n, g) => n + g.walkthroughs.length, 0);

  React.useEffect(() => {
    const headings = visible.map((g) => document.getElementById(anchorId(g.section))).filter((el): el is HTMLElement => el !== null);
    if (!headings.length) return;
    const observer = new IntersectionObserver(
      (entries) => {
        const on = entries.filter((e) => e.isIntersecting).sort((a, b) => a.boundingClientRect.top - b.boundingClientRect.top);
        const id = on[0]?.target.id;
        const found = id && visible.find((g) => anchorId(g.section) === id);
        if (found) setActive(found.section);
      },
      { rootMargin: "-8px 0px -70% 0px" },
    );
    headings.forEach((el) => observer.observe(el));
    return () => observer.disconnect();
  }, [visible]);

  return (
    <div className="flex flex-col gap-5 lg:flex-row-reverse lg:items-start">
      <div className="w-full shrink-0 space-y-4 lg:sticky lg:top-1 lg:w-52">
        <div className="space-y-1.5">
          <SearchField value={query} onChange={setQuery} placeholder="Search walkthroughs…" shortcut="/" />
          <p className="px-0.5 text-[11px] text-muted-foreground">{q ? `${hits} of ${total}` : "Searches titles and chapter names."}</p>
        </div>
        {visible.length > 0 && (
          <nav aria-label="Sections" className="hidden space-y-1 lg:block">
            <p className="eyebrow px-2 text-muted-foreground">Contents</p>
            <ul className="space-y-px">
              {visible.map(({ section, rows }) => (
                <li key={section}>
                  <a
                    href={`#${anchorId(section)}`}
                    aria-current={section === active ? "true" : undefined}
                    className={cn(
                      "flex items-center justify-between gap-2 rounded-md px-2 py-1 text-[13px] transition hover:bg-secondary",
                      section === active ? "bg-accent/60 font-medium text-foreground" : "text-muted-foreground",
                    )}
                  >
                    <span className="min-w-0 truncate">{section}</span>
                    <span className="shrink-0 text-[11px] tabular-nums text-muted-foreground">{rows.length}</span>
                  </a>
                </li>
              ))}
            </ul>
          </nav>
        )}
      </div>

      <div className="min-w-0 flex-1 space-y-5">
        {visible.length === 0 ? (
          <p className="rounded-xl border border-dashed bg-muted/40 px-3 py-8 text-center text-sm text-muted-foreground">Nothing matches “{query.trim()}”.</p>
        ) : (
          visible.map(({ section, rows }) => (
            <section key={section} id={anchorId(section)} className="scroll-mt-2 space-y-1.5">
              <h2 className="eyebrow px-0.5 text-muted-foreground">{section}</h2>
              <ul className="divide-y overflow-hidden rounded-xl border bg-card shadow-sm">
                {rows.map(({ w, chapters }) => (
                  <li key={w.id}>
                    <button type="button" onClick={() => onOpen(w.id)} className="group flex w-full gap-3 p-2 text-left transition hover:bg-secondary/60">
                      <div className="relative w-36 shrink-0 overflow-hidden rounded-md border sm:w-44">
                        {/* eslint-disable-next-line @next/next/no-img-element */}
                        <img src={w.posterSrc} alt="" className="aspect-[16/10] w-full bg-muted object-cover" />
                        <span className="absolute right-1 bottom-1 rounded-sm bg-foreground/85 px-1 py-px text-[10px] font-medium tabular-nums text-background">{timecode(w.durationSeconds)}</span>
                      </div>
                      <div className="flex min-w-0 flex-1 flex-col gap-1 py-0.5">
                        <h3 className="text-sm font-medium leading-snug text-foreground">{w.title}</h3>
                        <p className="line-clamp-2 text-xs leading-relaxed text-muted-foreground">{w.summary}</p>
                        <span className="mt-auto inline-flex items-center gap-1 text-[11px] text-muted-foreground">
                          <ListVideo className="size-3" />
                          {w.chapters.length} chapters
                        </span>
                      </div>
                    </button>
                    {chapters.length > 0 && (
                      <ul className="border-t bg-muted/30 py-0.5 pl-[calc(0.5rem+9rem)] sm:pl-[calc(0.5rem+11rem)]">
                        {chapters.slice(0, 4).map((c) => (
                          <li key={c.title}>
                            <button type="button" onClick={() => onOpen(w.id, Math.floor(c.startSeconds))} className="flex w-full items-center gap-2 rounded-md px-3 py-1 text-left text-xs text-muted-foreground transition hover:bg-secondary hover:text-foreground">
                              <Play className="size-3 shrink-0" />
                              <span className="min-w-0 flex-1 truncate">{c.title}</span>
                              <span className="shrink-0 tabular-nums">{timecode(c.startSeconds)}</span>
                            </button>
                          </li>
                        ))}
                      </ul>
                    )}
                  </li>
                ))}
              </ul>
            </section>
          ))
        )}
      </div>
    </div>
  );
}

function Player({ w, startAt }: { w: Walkthrough; startAt: number }) {
  const ref = React.useRef<HTMLVideoElement>(null);
  const [active, setActive] = React.useState(() => {
    let i = 0;
    w.chapters.forEach((c, n) => { if (c.startSeconds <= startAt + 0.25) i = n; });
    return i;
  });
  const sync = React.useCallback(() => {
    const t = ref.current?.currentTime ?? 0;
    let next = 0;
    w.chapters.forEach((c, i) => { if (c.startSeconds <= t + 0.25) next = i; });
    setActive(next);
  }, [w.chapters]);
  const seek = (i: number) => {
    const v = ref.current; if (!v) return;
    v.currentTime = w.chapters[i].startSeconds; setActive(i); void v.play();
  };
  return (
    <div className="grid gap-4 rounded-xl border bg-card p-4 lg:grid-cols-[minmax(0,1fr)_16rem]">
      <div className="space-y-2">
        <video
          ref={ref}
          src={w.videoSrc}
          poster={w.posterSrc}
          controls
          preload="metadata"
          playsInline
          onTimeUpdate={sync}
          onSeeked={sync}
          onLoadedMetadata={(e) => { if (startAt > 0) e.currentTarget.currentTime = Math.min(startAt, Math.max(e.currentTarget.duration - 0.1, 0)); }}
          className="w-full rounded-lg border bg-black"
        />
        <p className="text-sm leading-relaxed text-muted-foreground">{w.summary}</p>
      </div>
      <div className="space-y-1.5">
        <p className="eyebrow text-muted-foreground">Chapters</p>
        <ol className="divide-y overflow-hidden rounded-lg border bg-card">
          {w.chapters.map((c, i) => (
            <li key={c.title}>
              <button type="button" onClick={() => seek(i)} aria-current={i === active ? "true" : undefined} className={cn("flex w-full items-center gap-2 px-3 py-2 text-left transition hover:bg-secondary/60", i === active && "bg-accent/60")}>
                <span className={cn("flex size-5 shrink-0 items-center justify-center rounded-sm", i === active ? "bg-primary text-primary-foreground" : "bg-accent text-muted-foreground")}>
                  {i === active ? <Play className="size-3" /> : <span className="text-[11px] tabular-nums">{i + 1}</span>}
                </span>
                <span className="min-w-0 flex-1 truncate text-sm text-foreground">{c.title}</span>
                <span className="shrink-0 text-xs tabular-nums text-muted-foreground">{timecode(c.startSeconds)}</span>
              </button>
            </li>
          ))}
        </ol>
      </div>
    </div>
  );
}
