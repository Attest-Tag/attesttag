// Small display helpers shared across the console. Keep this module free of
// React so both plain functions and components can import it.

/**
 * A scope's name as it should be read: a channel drops the leading hash, because the icon
 * beside it already says what it is — and says it more accurately, since a private channel
 * is drawn with a lock rather than a hash.
 */
export function scopeName(scope: { kind: string; name: string }): string {
  return scope.kind === "channel" ? scope.name.replace(/^#+/, "") : scope.name;
}

/**
 * The IANA zone this browser is in ("Asia/Kathmandu"), or "" when it will not say.
 *
 * Read it in an event handler or an effect, never while rendering: the console is a static
 * export, so a value read during render is the build machine's zone in the served HTML and the
 * reader's zone after hydration.
 */
export function browserZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "";
  } catch {
    return "";
  }
}

/** "just now", "4m ago", "3h ago", "6d ago", then a short date. */
export function formatRelative(value: Date | string): string {
  const date = toDate(value);
  if (!date) return "";
  const minutes = Math.floor((Date.now() - date.getTime()) / 60000);
  if (minutes < 1) return "just now";
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  if (days < 30) return `${days}d ago`;
  return formatDayMonth(date);
}

/** Long form for sentences: "14 minutes ago", "2 hours ago", "3 days ago". */
export function formatRelativeLong(value: Date | string): string {
  const date = toDate(value);
  if (!date) return "";
  const minutes = Math.floor((Date.now() - date.getTime()) / 60000);
  if (minutes < 1) return "just now";
  if (minutes < 60) return plural(minutes, "minute");
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return plural(hours, "hour");
  const days = Math.floor(hours / 24);
  if (days < 30) return plural(days, "day");
  return `on ${formatDate(date)}`;
}

function plural(n: number, unit: string): string {
  return `${n} ${unit}${n === 1 ? "" : "s"} ago`;
}

/** "12 Mar" — for recent rows where the year is noise. */
export function formatDayMonth(value: Date | string): string {
  const date = toDate(value);
  if (!date) return "";
  return date.toLocaleDateString("en-US", { month: "short", day: "numeric" });
}

/** "12 Mar 2026". */
export function formatDate(value: Date | string): string {
  const date = toDate(value);
  if (!date) return "";
  return date.toLocaleDateString("en-US", {
    day: "numeric",
    month: "short",
    year: "numeric",
  });
}

/** "12 Mar, 14:05" — activity rows, where the minute matters. */
export function formatDateTime(value: Date | string): string {
  const date = toDate(value);
  if (!date) return "";
  return date.toLocaleString("en-US", {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });
}

/** `1536` -> "2 KB". */
export function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

/**
 * "$12.34" — four decimals under a cent so tiny per-turn costs stay visible.
 *
 * The sign goes outside the symbol: "-$60.00", not "$-60.00". A negative figure used to be
 * impossible here, because spend only goes up — a prepaid credit balance can go below zero while
 * the turns already in flight finish, and that is a number somebody reads while deciding whether
 * to top up.
 */
export function formatUSD(value: number): string {
  const sign = value < 0 ? "-" : "";
  const n = Math.abs(value);
  if (n !== 0 && n < 0.01) return `${sign}$${n.toFixed(4)}`;
  // Grouped above a thousand, like formatNumber: a top-up can be $1,000 and "$1000.00" is a
  // figure the eye has to count digits in.
  return `${sign}$${n.toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
}

/** "12,345". */
export function formatNumber(value: number): string {
  return value.toLocaleString("en-US");
}

/**
 * "940", "12.4K", "3.1M" — for an axis tick or a bar label, where the exact figure is a hover
 * away and the digits are there to give the eye a scale. Never for a figure somebody is meant to
 * read precisely: a month's token count rounded to "3.1M" is not a number you can check.
 */
export function formatCompact(value: number): string {
  const n = Math.abs(value);
  const sign = value < 0 ? "-" : "";
  if (n >= 1_000_000) return `${sign}${trimZero(n / 1_000_000)}M`;
  if (n >= 10_000) return `${sign}${Math.round(n / 1000)}K`;
  if (n >= 1_000) return `${sign}${trimZero(n / 1000)}K`;
  return `${sign}${Math.round(n)}`;
}

function trimZero(n: number): string {
  return n.toFixed(1).replace(/\.0$/, "");
}

/**
 * "Sep 2" from a "2026-09-02" day key, without ever making a UTC instant of it.
 *
 * The server groups turns by their UTC day and sends the day as a string for this reason:
 * `new Date("2026-09-02")` is midnight UTC, which in New York is the evening of the 1st, so a
 * chart formatted that way labels every bar with the day before. Splitting the key and building
 * a local date keeps the label the day the server counted.
 */
export function formatDayKey(
  day: string,
  options: Intl.DateTimeFormatOptions = { month: "short", day: "numeric" },
): string {
  const parts = /^(\d{4})-(\d{2})-(\d{2})$/.exec(day);
  if (!parts) return day;
  const local = new Date(Number(parts[1]), Number(parts[2]) - 1, Number(parts[3]));
  return local.toLocaleDateString("en-US", options);
}

/** Elapsed time from milliseconds: "420 ms", "1.2 s", "1m 12s". */
export function formatMillis(ms: number): string {
  if (ms < 1000) return `${ms} ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(1)} s`;
  const minutes = Math.floor(ms / 60000);
  return `${minutes}m ${Math.round((ms % 60000) / 1000)}s`;
}

/** Go's time.Duration arrives as nanoseconds. */
export function formatNanos(ns: number): string {
  return formatMillis(Math.round(ns / 1e6));
}

// SQLite timestamps come back as "2026-09-02 01:14:00" with no zone, which
// Date() parses as local time in some engines and rejects in others. Treat
// them as UTC, which is what the Go side writes.
function toDate(value: Date | string): Date | null {
  if (value instanceof Date) return Number.isNaN(value.getTime()) ? null : value;
  if (!value) return null;
  let s = value.trim();
  if (/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}/.test(s) && !/[zZ]|[+-]\d{2}:?\d{2}$/.test(s)) {
    s = s.replace(" ", "T") + "Z";
  }
  const date = new Date(s);
  return Number.isNaN(date.getTime()) ? null : date;
}

/** Exposed for components that need the parsed instant (tooltips, sorting). */
export function parseTime(value: string): Date | null {
  return toDate(value);
}
