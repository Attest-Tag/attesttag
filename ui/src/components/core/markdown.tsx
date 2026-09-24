import { Fragment } from "react";
import { cn } from "@/lib/utils";

/**
 * The Markdown the model is told to write, drawn as what it means.
 *
 * The system prompt asks for standard Markdown — bold, lists, code fences, links — and for people
 * to be named as <@USERID>. Slack draws that. A console that prints the source instead shows
 * `[1c9ef3b](https://…)` where the answer said "1c9ef3b", so the same words are worth less here
 * than in the channel they came from.
 *
 * Written out rather than installed: the text is Markdown and Slack's own dialect at once —
 * mentions, channel references, <url|label> links — and a Markdown library reads the first and
 * leaves the second lying there as punctuation. It knows a subset on purpose; anything outside it
 * falls through as the characters it already was, which is a failure a reader can still work with.
 *
 * No string here reaches the DOM as HTML. Every node is built by React, so a reply carrying a tag
 * — from a web page a tool read, an issue somebody else wrote — is drawn, not run, and a link is
 * an href only when its scheme is http, https or mailto.
 */
export function Markdown({ text, className }: { text: string; className?: string }) {
  return (
    <div className={cn("space-y-2 text-sm leading-relaxed break-words", className)}>{blocks(text)}</div>
  );
}

const LINK_CLASS = "text-primary underline-offset-2 hover:underline";
const MENTION_CLASS = "rounded bg-info-soft px-1 font-medium text-info";

/** An href only for schemes that open a page or a mail client. Everything else stays as text. */
function safeHref(url: string): string | null {
  const u = url.trim();
  if (/^https?:\/\//i.test(u) || /^mailto:/i.test(u)) return u;
  if (/^www\./i.test(u)) return `https://${u}`;
  return null;
}

function Link({ href, children }: { href: string; children: React.ReactNode }) {
  const safe = safeHref(href);
  if (!safe) return <>{children}</>;
  return (
    <a href={safe} target="_blank" rel="noreferrer noopener" className={LINK_CLASS}>
      {children}
    </a>
  );
}

type Rule = {
  re: RegExp;
  node: (m: RegExpExecArray, key: number) => React.ReactNode;
};

// Read in this order where two could start at the same character: code before everything, because
// what is inside a span is characters rather than markup, and ** before * so bold is not read as
// an empty italic. `_italic_` is deliberately absent — snake_case names and URLs are full of
// underscores, and mangling an identifier costs more than an unrendered emphasis.
const RULES: Rule[] = [
  {
    re: /`([^`\n]+)`/,
    node: (m, k) => (
      <code key={k} className="rounded bg-muted px-1 py-0.5 font-mono text-[0.9em]">
        {m[1]}
      </code>
    ),
  },
  {
    // A destination may carry a balanced pair of brackets of its own — Wikipedia titles do — so
    // the closing one is not simply the first ")" after the "(".
    re: /\[([^\]]*)\]\(\s*<?((?:[^\s()<>]|\([^\s()<>]*\))*)>?(?:\s+"[^"]*")?\s*\)/,
    node: (m, k) => (
      <Link key={k} href={m[2]}>
        {m[1] ? inline(m[1]) : m[2]}
      </Link>
    ),
  },
  // Slack's own: a person, a channel, and a link that carries its label after a pipe.
  {
    re: /<@([A-Z0-9]{2,})(?:\|([^>]*))?>/,
    node: (m, k) => (
      <span key={k} className={MENTION_CLASS}>
        @{m[2] || m[1]}
      </span>
    ),
  },
  {
    re: /<#([A-Z0-9]{2,})(?:\|([^>]*))?>/,
    node: (m, k) => (
      <span key={k} className={MENTION_CLASS}>
        #{m[2] || m[1]}
      </span>
    ),
  },
  {
    re: /<((?:https?:\/\/|mailto:)[^\s|>]+)(?:\|([^>]*))?>/,
    node: (m, k) => (
      <Link key={k} href={m[1]}>
        {m[2] || m[1]}
      </Link>
    ),
  },
  {
    re: /\*\*([^\s](?:[\s\S]*?[^\s])?)\*\*/,
    node: (m, k) => (
      <strong key={k} className="font-semibold text-foreground">
        {inline(m[1])}
      </strong>
    ),
  },
  {
    re: /__([^\s](?:[\s\S]*?[^\s])?)__/,
    node: (m, k) => (
      <strong key={k} className="font-semibold text-foreground">
        {inline(m[1])}
      </strong>
    ),
  },
  {
    re: /~~([^\s](?:[\s\S]*?[^\s])?)~~/,
    node: (m, k) => <s key={k}>{inline(m[1])}</s>,
  },
  {
    re: /\*([^\s*](?:[^*]*?[^\s*])?)\*/,
    node: (m, k) => <em key={k}>{inline(m[1])}</em>,
  },
  // A link the model wrote as bare text. It ends on the last character that could belong to a
  // URL, so the full stop closing the sentence stays in the sentence.
  {
    re: /(?:https?:\/\/|www\.)[^\s<>()[\]"'`]*[^\s<>()[\]"'`.,;:!?]/,
    node: (m, k) => (
      <Link key={k} href={m[0]}>
        {m[0]}
      </Link>
    ),
  },
];

/** Everything inside one line: the earliest rule that matches wins, then the rest is read again. */
function inline(text: string): React.ReactNode[] {
  const out: React.ReactNode[] = [];
  let rest = text;
  let key = 0;
  while (rest) {
    let at = -1;
    let hit: RegExpExecArray | null = null;
    let rule: Rule | null = null;
    for (const r of RULES) {
      const m = r.re.exec(rest);
      if (m && (at < 0 || m.index < at)) {
        at = m.index;
        hit = m;
        rule = r;
      }
      if (at === 0) break;
    }
    if (!hit || !rule) {
      out.push(rest);
      break;
    }
    if (at > 0) out.push(rest.slice(0, at));
    out.push(rule.node(hit, key++));
    rest = rest.slice(at + hit[0].length);
  }
  return out;
}

const FENCE = /^ {0,3}(?:```|~~~)\s*[\w+-]*\s*$/;
const HEADING = /^ {0,3}(#{1,6})\s+(.*)$/;
const RULE_LINE = /^ {0,3}(?:-{3,}|\*{3,}|_{3,})\s*$/;
const QUOTE = /^ {0,3}> ?(.*)$/;
const ITEM = /^([ \t]*)(?:([-*+])|(\d{1,9})[.)])\s+(.*)$/;

const isTableSep = (s: string) => /^[\s|:-]+$/.test(s) && s.includes("|") && s.includes("-");
const startsBlock = (s: string) =>
  FENCE.test(s) || HEADING.test(s) || RULE_LINE.test(s) || QUOTE.test(s) || ITEM.test(s);

/** The lines of a reply, grouped into the shapes they describe. */
function blocks(src: string, depth = 0): React.ReactNode[] {
  // An answer is not a document: nesting this deep is a runaway rather than a structure worth
  // drawing, and the text itself is still the honest thing to show.
  if (depth > 5) {
    return [
      <p key={0} className="whitespace-pre-wrap">
        {src}
      </p>,
    ];
  }
  const lines = src.replace(/\r\n?/g, "\n").split("\n");
  const out: React.ReactNode[] = [];
  let i = 0;
  let key = 0;

  while (i < lines.length) {
    const line = lines[i];
    if (!line.trim()) {
      i++;
      continue;
    }

    if (FENCE.test(line)) {
      const body: string[] = [];
      i++;
      while (i < lines.length && !FENCE.test(lines[i])) body.push(lines[i++]);
      i++; // The closing fence, or the end of a reply the model never closed.
      out.push(
        <pre
          key={key++}
          className="max-h-80 overflow-auto rounded-md bg-muted/60 p-2 font-mono text-[11px] leading-relaxed"
        >
          <code>{body.join("\n")}</code>
        </pre>,
      );
      continue;
    }

    // The prompt asks for nothing louder than bold, so a heading is drawn as the emphasis it is.
    const h = line.match(HEADING);
    if (h) {
      out.push(
        <p key={key++} className="pt-1 font-semibold text-foreground">
          {inline(h[2])}
        </p>,
      );
      i++;
      continue;
    }

    if (RULE_LINE.test(line)) {
      out.push(<hr key={key++} className="my-1" />);
      i++;
      continue;
    }

    if (QUOTE.test(line)) {
      const quoted: string[] = [];
      while (i < lines.length && QUOTE.test(lines[i])) quoted.push(lines[i++].replace(QUOTE, "$1"));
      out.push(
        <blockquote key={key++} className="space-y-2 border-l-2 pl-3 text-muted-foreground">
          {blocks(quoted.join("\n"), depth + 1)}
        </blockquote>,
      );
      continue;
    }

    if (line.includes("|") && i + 1 < lines.length && isTableSep(lines[i + 1])) {
      const [node, next] = takeTable(lines, i, key++);
      out.push(node);
      i = next;
      continue;
    }

    if (ITEM.test(line)) {
      const [node, next] = takeList(lines, i, depth, key++);
      out.push(node);
      i = next;
      continue;
    }

    // Anything else is a paragraph, and its line breaks are kept: people write answers in a chat
    // box, where a new line is a new line and not the space Markdown would turn it into.
    const para: string[] = [lines[i++]];
    while (
      i < lines.length &&
      lines[i].trim() &&
      !startsBlock(lines[i]) &&
      !(lines[i].includes("|") && isTableSep(lines[i + 1] ?? ""))
    ) {
      para.push(lines[i++]);
    }
    out.push(
      <p key={key++}>
        {para.map((l, n) => (
          <Fragment key={n}>
            {n > 0 && <br />}
            {inline(l)}
          </Fragment>
        ))}
      </p>,
    );
  }

  return out;
}

/** One list and everything indented under it, items read back as blocks of their own. */
function takeList(lines: string[], start: number, depth: number, key: number): [React.ReactNode, number] {
  const first = lines[start].match(ITEM)!;
  const indent = first[1].length;
  const ordered = !!first[3];
  const pad = first[0].length - first[4].length; // the column this list's content starts in
  const items: string[][] = [];
  let i = start;

  const sibling = (line: string) => {
    const m = line.match(ITEM);
    return m && m[1].length <= indent + 1 && !!m[3] === ordered ? m : null;
  };
  const belongs = (line: string) =>
    !!sibling(line) || line.length - line.trimStart().length >= indent + 2;

  while (i < lines.length) {
    const line = lines[i];
    const s = sibling(line);
    if (s) {
      items.push([s[4]]);
      i++;
      continue;
    }
    if (!items.length) break;
    if (!line.trim()) {
      // A blank line only ends the list if nothing below carries it on.
      let j = i + 1;
      while (j < lines.length && !lines[j].trim()) j++;
      if (j >= lines.length || !belongs(lines[j])) break;
      items[items.length - 1].push("");
      i++;
      continue;
    }
    const lead = line.length - line.trimStart().length;
    if (ITEM.test(line) && lead <= indent + 1) break; // a list of the other kind starts here
    items[items.length - 1].push(line.slice(Math.min(lead, pad)));
    i++;
  }

  const body = items.map((it, n) => (
    <li key={n} className="space-y-2">
      {blocks(it.join("\n"), depth + 1)}
    </li>
  ));
  const node = ordered ? (
    <ol key={key} start={Number(first[3]) || 1} className="ml-5 list-decimal space-y-1 marker:text-muted-foreground">
      {body}
    </ol>
  ) : (
    <ul key={key} className="ml-5 list-disc space-y-1 marker:text-muted-foreground">
      {body}
    </ul>
  );
  return [node, i];
}

/** A pipe table, scrolling sideways rather than pushing the page wider than the screen. */
function takeTable(lines: string[], start: number, key: number): [React.ReactNode, number] {
  const cells = (row: string) =>
    row
      .trim()
      .replace(/^\|/, "")
      .replace(/\|$/, "")
      .split("|")
      .map((c) => c.trim());
  const head = cells(lines[start]);
  const rows: string[][] = [];
  let i = start + 2;
  while (i < lines.length && lines[i].trim() && lines[i].includes("|")) rows.push(cells(lines[i++]));
  return [
    <div key={key} className="overflow-x-auto">
      <table className="w-full border-collapse text-left text-xs">
        <thead>
          <tr className="border-b">
            {head.map((c, n) => (
              <th key={n} className="px-2 py-1 font-semibold">
                {inline(c)}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((r, n) => (
            <tr key={n} className="border-b last:border-0">
              {r.map((c, j) => (
                <td key={j} className="px-2 py-1 align-top">
                  {inline(c)}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>,
    i,
  ];
}
