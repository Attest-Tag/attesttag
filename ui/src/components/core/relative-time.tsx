import { formatRelative, parseTime } from "@/lib/format";

// "4h ago" for list rows and feeds — the one place relative timestamps are
// rendered, so they behave the same everywhere.
//
// It exists because `formatRelative` reads the clock, and a client component
// renders twice: once on the server, once again when React hydrates. A row
// written seconds earlier is "just now" in the server HTML and "1m ago" a
// moment later in the browser, and React treats that as a failed hydration
// (error #418) — it discards the server HTML for that subtree and re-renders
// it. The mismatch is real, not cosmetic, and it is time-dependent, so it
// surfaced on a different list on every run.
//
// Suppression is the right answer rather than a dodge: the text is *supposed*
// to differ between the two renders, since time passed between them. React
// documents this exact case. It is scoped to this element's own text, so a
// genuine mismatch anywhere else still warns — the same posture as the
// suppression on <html>/<body> in app/layout.tsx.
//
// Deliberately NOT a "use client" module. It holds no state and no hooks, so
// it compiles into whichever graph imports it: part of the bundle inside a
// client list, plain server rendering inside a server page. Marking it would
// push an island into pages that don't need one.
//
// The <time> wrapper is the other half of the fix. "4h ago" alone is unusable
// to a screen reader arriving out of context, and unreadable once it has
// scrolled past a month; `dateTime` carries the machine-readable instant and
// `title` the full one for anyone who hovers.
export function RelativeTime({
  value,
  className,
}: {
  value: Date | string;
  className?: string;
}) {
  // Through `parseTime`, not `new Date`: the API writes SQLite's naive
  // "2026-09-02 19:35:56", which the Date constructor reads as *local* time.
  // Every timestamp then landed one UTC offset in the future, and anything
  // newer than that offset — four hours, here — rendered as "just now".
  const date = typeof value === "string" ? parseTime(value) : value;
  if (!date || Number.isNaN(date.getTime())) return null;

  return (
    <time
      dateTime={date.toISOString()}
      title={date.toLocaleString()}
      suppressHydrationWarning
      className={className}
    >
      {formatRelative(date)}
    </time>
  );
}
