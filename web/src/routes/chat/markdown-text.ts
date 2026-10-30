/**
 * Pure text helpers for the chat markdown renderer. Kept free of React so
 * they are cheap to unit test and run on every streamed token.
 */

const FENCE_OPEN = /^\s*(`{3,}|~{3,})/;

interface Fence {
  char: string;
  len: number;
}

function openerOf(line: string): Fence | null {
  const m = FENCE_OPEN.exec(line);
  const marker = m?.[1];
  if (!marker) return null;
  // A backtick fence's info string may not contain backticks (CommonMark).
  if (marker[0] === '`' && line.slice(line.indexOf(marker) + marker.length).includes('`')) return null;
  return { char: marker[0] ?? '`', len: marker.length };
}

function closes(line: string, f: Fence): boolean {
  const t = line.trim();
  if (t.length < f.len) return false;
  for (const c of t) if (c !== f.char) return false;
  return true;
}

/**
 * Walk the lines of a markdown text, reporting which ones sit inside a
 * fenced code block (the fence lines themselves count as inside).
 */
function fenceMap(lines: string[]): { inside: boolean[]; open: Fence | null } {
  const inside: boolean[] = [];
  let open: Fence | null = null;
  for (const line of lines) {
    if (open) {
      inside.push(true);
      if (closes(line, open)) open = null;
      continue;
    }
    const f = openerOf(line);
    if (f) {
      open = f;
      inside.push(true);
      continue;
    }
    inside.push(false);
  }
  return { inside, open };
}

/** True when the text ends inside a fenced code block (an odd fence count). */
export function hasOpenFence(text: string): boolean {
  return fenceMap(text.split('\n')).open !== null;
}

/**
 * While a reply streams, a code fence is often open. Append the matching
 * closing fence (for rendering only) so the partial block renders as a code
 * block in progress rather than whatever the parser makes of half a fence.
 */
export function closeOpenFences(text: string): string {
  const { open } = fenceMap(text.split('\n'));
  if (!open) return text;
  return `${text}${text.endsWith('\n') ? '' : '\n'}${open.char.repeat(open.len)}`;
}

/** Replace \( \) and \[ \] math delimiters in prose (no code spans inside). */
function normalizeProse(s: string): string {
  return s
    .replace(/\\\[([\s\S]+?)\\\]/g, (_m, inner: string, offset: number, all: string) => {
      const before = all.slice(0, offset);
      const atLineStart = /(^|\n)[ \t]*$/.test(before);
      return atLineStart ? `\n$$\n${inner.trim()}\n$$\n` : `$$${inner.trim()}$$`;
    })
    .replace(/\\\(([\s\S]+?)\\\)/g, (_m, inner: string) => `$${inner.trim()}$`);
}

/** Apply normalizeProse to everything outside inline code spans. */
function normalizeOutsideInlineCode(s: string): string {
  const re = /(`+)[\s\S]*?\1/g;
  let out = '';
  let last = 0;
  for (let m = re.exec(s); m; m = re.exec(s)) {
    out += normalizeProse(s.slice(last, m.index)) + m[0];
    last = m.index + m[0].length;
  }
  return out + normalizeProse(s.slice(last));
}

/**
 * Models write LaTeX as \( … \) and \[ … \] as often as $ … $; remark-math
 * only understands dollars. Rewrite the backslash delimiters outside fenced
 * code and inline code so `\(x\)` in a code sample stays literal.
 */
export function normalizeMathDelimiters(text: string): string {
  if (!text.includes('\\(') && !text.includes('\\[')) return text;
  const lines = text.split('\n');
  const { inside } = fenceMap(lines);
  const out: string[] = [];
  let prose: string[] = [];
  const flush = () => {
    if (prose.length) out.push(normalizeOutsideInlineCode(prose.join('\n')));
    prose = [];
  };
  lines.forEach((line, i) => {
    if (inside[i]) {
      flush();
      out.push(line);
    } else {
      prose.push(line);
    }
  });
  flush();
  return out.join('\n');
}

const LIST_ITEM = /^\s*([-*+]|\d{1,9}[.)])\s/;
const DEFINITION = /^ {0,3}\[[^\]]+\]:/m;

function dollarPairsOpen(block: string): boolean {
  const n = block.match(/\$\$/g)?.length ?? 0;
  return n % 2 === 1;
}

/**
 * Split markdown into top-level blocks at blank lines outside code fences so
 * each block can be memoized: while a reply streams, only the last block's
 * text changes. Blocks that only make sense together are kept together:
 * indented continuations, consecutive list blocks (loose lists), and display
 * math with blank lines inside. Texts with reference-style link or footnote
 * definitions are not split at all, since a definition must share a parse
 * with its uses.
 */
export function splitBlocks(text: string): string[] {
  if (DEFINITION.test(text)) return [text];
  const lines = text.split('\n');
  const { inside } = fenceMap(lines);
  const raw: string[] = [];
  let cur: string[] = [];
  lines.forEach((line, i) => {
    if (!inside[i] && line.trim() === '') {
      if (cur.length) raw.push(cur.join('\n'));
      cur = [];
      return;
    }
    cur.push(line);
  });
  if (cur.length) raw.push(cur.join('\n'));

  const blocks: string[] = [];
  for (const b of raw) {
    const prev = blocks.length ? blocks[blocks.length - 1] : undefined;
    const merge = prev !== undefined && (/^\s/.test(b) || (LIST_ITEM.test(prev) && LIST_ITEM.test(b)) || dollarPairsOpen(prev));
    if (merge && prev !== undefined) blocks[blocks.length - 1] = `${prev}\n\n${b}`;
    else blocks.push(b);
  }
  return blocks;
}

const DATA_IMAGE = /^data:image\/(png|jpeg|gif|webp)[;,]/i;

/** data: URIs we render directly: raster images only (SVG can carry script). */
export function isSafeDataImage(src: string): boolean {
  return DATA_IMAGE.test(src.trim());
}

/**
 * URL allow-list for model output. Links: http, https, mailto and in-page
 * fragments (footnotes). Images: http(s) (loaded only on click) and raster
 * data: URIs. Anything else becomes '' and the renderer shows plain text.
 */
export function safeUrl(url: string, key: string, tagName: string): string {
  const u = url.trim();
  if (tagName === 'img' && key === 'src') {
    return /^https?:\/\//i.test(u) || isSafeDataImage(u) ? u : '';
  }
  if (/^(https?:\/\/|mailto:)/i.test(u)) return u;
  if (u.startsWith('#')) return u;
  return '';
}
