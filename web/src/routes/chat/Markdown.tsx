/**
 * Chat message renderer: GitHub-flavoured markdown, highlighted code, KaTeX
 * math and mermaid diagrams, safe for untrusted model output.
 *
 * Safety: no rehype-raw, so raw HTML in a reply renders as text; links and
 * images go through an allow-list (safeUrl); remote images are never fetched
 * until the reader clicks; KaTeX runs with trust:false; mermaid runs with
 * securityLevel 'strict' and its SVG is shown through an <img>, where it
 * cannot run script or touch the page.
 *
 * Streaming: the text is split into top-level blocks and each block is a
 * memoized component, so as tokens arrive only the last block re-parses. An
 * unclosed code fence renders as a code block in progress.
 *
 * Heavy pieces load on demand: KaTeX only when the text contains math, shiki
 * per grammar when a fence names one, mermaid when a mermaid fence closes.
 */
import { createContext, memo, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import ReactMarkdown, { type Components, type Options } from 'react-markdown';
import remarkGfm from 'remark-gfm';
import remarkMath from 'remark-math';
import { Icon } from '../../components/IconButton';
import { highlight, type HighlightToken } from './highlight';
import { closeOpenFences, hasOpenFence, isSafeDataImage, normalizeMathDelimiters, safeUrl, splitBlocks } from './markdown-text';
import { type DocKind, downloadText, fileName, formatSize, previewDocument, wholeDocument } from './chat-files';

type PluggableList = NonNullable<Options['rehypePlugins']>;

const REMARK_PLUGINS: PluggableList = [remarkGfm, remarkMath];
const KATEX_OPTIONS = { trust: false, strict: 'ignore', maxExpand: 1000, maxSize: 50 };

// ---- KaTeX, loaded once on first use ---------------------------------------

let rehypeKatexPlugins: PluggableList | null = null;
let katexLoading: Promise<PluggableList> | null = null;

function loadKatex(): Promise<PluggableList> {
  katexLoading ??= Promise.all([import('rehype-katex'), import('katex/dist/katex.min.css')]).then(([m]) => {
    rehypeKatexPlugins = [[m.default, KATEX_OPTIONS]];
    return rehypeKatexPlugins;
  });
  return katexLoading;
}

/** Cheap check for "this text may contain math": $ or a normalized delimiter. */
function mayHaveMath(text: string): boolean {
  return text.includes('$');
}

function useRehypePlugins(needed: boolean): PluggableList {
  const [plugins, setPlugins] = useState<PluggableList>(() => rehypeKatexPlugins ?? []);
  useEffect(() => {
    if (!needed || plugins.length) return;
    let live = true;
    void loadKatex().then((p) => {
      if (live) setPlugins(p);
    });
    return () => {
      live = false;
    };
  }, [needed, plugins.length]);
  return plugins;
}

// ---- Theme ------------------------------------------------------------------

function currentTheme(): 'light' | 'dark' {
  // tokens.css: dark is the default; light only when explicitly set.
  return document.documentElement.getAttribute('data-theme') === 'light' ? 'light' : 'dark';
}

/** The app theme (html[data-theme]), following live changes. */
function useTheme(): 'light' | 'dark' {
  const [theme, setTheme] = useState(currentTheme);
  useEffect(() => {
    const obs = new MutationObserver(() => setTheme(currentTheme()));
    obs.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
    return () => obs.disconnect();
  }, []);
  return theme;
}

// ---- Code blocks ------------------------------------------------------------

/** True inside the block whose closing fence has not arrived yet. */
const InProgress = createContext(false);

interface HastLike {
  type: string;
  value?: string;
  tagName?: string;
  properties?: Record<string, unknown>;
  children?: HastLike[];
}

function hastText(n: HastLike | undefined): string {
  if (!n) return '';
  if (n.type === 'text') return n.value ?? '';
  return (n.children ?? []).map(hastText).join('');
}

function fenceLang(code: HastLike | undefined): string {
  const cls = code?.properties?.className;
  const list = Array.isArray(cls) ? cls.map(String) : typeof cls === 'string' ? cls.split(' ') : [];
  const lang = list.find((c) => c.startsWith('language-'));
  return lang ? lang.slice('language-'.length) : '';
}

function useCopied(): [boolean, () => void] {
  const [copied, setCopied] = useState(false);
  const timer = useRef<number | undefined>(undefined);
  useEffect(() => () => window.clearTimeout(timer.current), []);
  return [
    copied,
    () => {
      setCopied(true);
      window.clearTimeout(timer.current);
      timer.current = window.setTimeout(() => setCopied(false), 1500);
    },
  ];
}

async function copyText(text: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    const area = document.createElement('textarea');
    area.value = text;
    document.body.appendChild(area);
    area.select();
    document.execCommand('copy');
    document.body.removeChild(area);
  }
}

const LINE_NUMBERS_OVER = 15;

function tokenStyle(t: HighlightToken): Record<string, string> {
  const s: Record<string, string> = {};
  if (t.light) s['--shiki-light'] = t.light;
  if (t.dark) s['--shiki-dark'] = t.dark;
  return s;
}

function tokenClass(t: HighlightToken): string | undefined {
  const f = t.fontStyle ?? 0;
  if (f <= 0) return undefined;
  const out: string[] = [];
  if (f & 1) out.push('i');
  if (f & 2) out.push('b');
  if (f & 4) out.push('u');
  if (f & 8) out.push('s');
  return out.length ? out.join(' ') : undefined;
}

function DownloadButton({ code, lang }: { code: string; lang: string }): ReactNode {
  const name = fileName(code, lang);
  return (
    <button
      type="button"
      className="chat-code-btn"
      aria-label={`Download ${name}`}
      data-tooltip={`Download ${name}`}
      onClick={() => downloadText(name, code)}
    >
      Download
    </button>
  );
}

/**
 * A whole HTML or SVG document, offered as a file: name, size, Preview
 * (sandboxed, opaque origin — no access to Janus), Download, Copy, and the
 * code on request. While it is still streaming it shows its progress.
 */
export function FileCard({
  code,
  lang,
  kind,
  name: given,
  stored = false,
}: {
  code: string;
  lang: string;
  /** html / svg get a sandboxed preview; null = any other text file. */
  kind: DocKind | null;
  /** The file's own name (a stored file); else derived from the code. */
  name?: string;
  /** A file the model handed over (create_file), stored on the reply. */
  stored?: boolean;
}): ReactNode {
  const inProgress = useContext(InProgress);
  const [view, setView] = useState<'none' | 'preview' | 'code'>('none');
  const [copied, markCopied] = useCopied();
  const frame = useRef<HTMLIFrameElement>(null);
  const text = code.endsWith('\n') ? code.slice(0, -1) : code;
  const name = given ?? fileName(text, lang);
  const lines = text.split('\n').length;
  const size = formatSize(new Blob([text]).size);
  const toggle = (v: 'preview' | 'code') => setView((cur) => (cur === v ? 'none' : v));
  const ext = name.includes('.')
    ? name
        .slice(name.lastIndexOf('.') + 1)
        .toUpperCase()
        .slice(0, 4)
    : 'FILE';
  return (
    <div
      className="chat-file"
      data-kind={kind ?? 'file'}
      data-stored={stored || undefined}
      data-in-progress={inProgress || undefined}
    >
      <div className="chat-file-head">
        <span className="chat-file-icon" aria-hidden="true">
          {kind === 'svg' ? 'SVG' : kind === 'html' ? 'HTML' : ext}
        </span>
        <span className="chat-file-meta">
          <span className="chat-file-name">{name}</span>
          <span className="chat-file-sub">
            {inProgress ? `Writing… ${lines.toLocaleString()} lines` : `${size} · ${lines.toLocaleString()} lines`}
          </span>
        </span>
        <span className="chat-file-actions">
          {kind ? (
            <button
              type="button"
              className={`btn btn-sm${view === 'preview' ? ' btn-primary' : ''}`}
              aria-pressed={view === 'preview'}
              disabled={inProgress}
              onClick={() => toggle('preview')}
            >
              {view === 'preview' ? 'Close preview' : kind === 'html' ? 'Preview' : 'View'}
            </button>
          ) : null}
          <button type="button" className="btn btn-sm" disabled={inProgress} onClick={() => downloadText(name, text)}>
            Download
          </button>
          <button
            type="button"
            className="btn btn-sm btn-ghost"
            aria-label="Copy file"
            disabled={inProgress}
            onClick={() => void copyText(text).then(markCopied)}
          >
            {copied ? 'Copied' : 'Copy'}
          </button>
          <button type="button" className="btn btn-sm btn-ghost" aria-pressed={view === 'code'} onClick={() => toggle('code')}>
            {view === 'code' ? 'Hide code' : 'Code'}
          </button>
        </span>
      </div>
      {view === 'preview' && kind && !inProgress ? (
        <div className="chat-file-preview">
          <button
            type="button"
            className="btn btn-sm chat-file-full"
            onClick={() => void frame.current?.requestFullscreen?.().catch(() => undefined)}
          >
            Full screen
          </button>
          <iframe
            ref={frame}
            title={`Preview of ${name}`}
            sandbox="allow-scripts allow-pointer-lock allow-modals"
            allow="autoplay; fullscreen; gamepad"
            referrerPolicy="no-referrer"
            srcDoc={previewDocument(text, kind)}
          />
        </div>
      ) : null}
      {view === 'code' ? <CodeBlock code={code} lang={lang || kind || ''} /> : null}
    </div>
  );
}

function CodeBlock({ code, lang }: { code: string; lang: string }): ReactNode {
  const inProgress = useContext(InProgress);
  const [wrap, setWrap] = useState(false);
  const [copied, markCopied] = useCopied();
  const [tokens, setTokens] = useState<{ code: string; lines: HighlightToken[][] } | null>(null);
  const text = code.endsWith('\n') ? code.slice(0, -1) : code;
  const lines = useMemo(() => text.split('\n'), [text]);
  const numbered = lines.length > LINE_NUMBERS_OVER;

  useEffect(() => {
    if (!lang) return;
    let live = true;
    // While a fence streams, re-highlight at most every 150 ms.
    const t = window.setTimeout(
      () => {
        highlight(text, lang)
          .then((res) => {
            if (live && res) setTokens({ code: text, lines: res });
          })
          .catch(() => undefined);
      },
      inProgress ? 150 : 0,
    );
    return () => {
      live = false;
      window.clearTimeout(t);
    };
  }, [text, lang, inProgress]);

  // Plain lines first; highlighted tokens once they arrive. Stale tokens from
  // a shorter streamed prefix are reused for the lines they cover.
  const hl = tokens?.lines;
  const fresh = tokens?.code === text;
  const body = lines.map((line, i) => {
    const toks = hl && (fresh || i < hl.length - 1) ? hl[i] : undefined;
    return (
      <span className="chat-code-line" key={i}>
        {toks
          ? toks.map((t, j) => (
              <span key={j} style={tokenStyle(t)} className={tokenClass(t)}>
                {t.content}
              </span>
            ))
          : line}
        {'\n'}
      </span>
    );
  });

  return (
    <div className="chat-code" data-lang={lang || undefined} data-in-progress={inProgress || undefined}>
      <div className="chat-code-header">
        <span className="chat-code-lang">{lang || 'text'}</span>
        <span className="chat-code-actions">
          {inProgress ? null : <DownloadButton code={text} lang={lang} />}
          <button
            type="button"
            className={wrap ? 'chat-code-btn active' : 'chat-code-btn'}
            aria-pressed={wrap}
            onClick={() => setWrap((w) => !w)}
          >
            Wrap
          </button>
          <button
            type="button"
            className="icon-btn icon-btn-sm chat-code-copy"
            aria-label="Copy code"
            data-tooltip={copied ? 'Copied' : 'Copy code'}
            onClick={() => void copyText(text).then(markCopied)}
          >
            <Icon name={copied ? 'check' : 'copy'} size={14} />
            {copied ? <span className="chat-code-copied">Copied</span> : null}
          </button>
        </span>
      </div>
      <pre className={`chat-code-pre${wrap ? ' wrap' : ''}${numbered ? ' numbered' : ''}`}>
        <code className={lang ? `language-${lang}` : undefined}>{body}</code>
      </pre>
    </div>
  );
}

// ---- Mermaid ----------------------------------------------------------------

type MermaidApi = typeof import('mermaid').default;
let mermaidLoading: Promise<MermaidApi> | null = null;
let mermaidTheme = '';
let mermaidSeq = 0;

async function renderMermaid(source: string, theme: 'light' | 'dark'): Promise<string> {
  mermaidLoading ??= import('mermaid').then((m) => m.default);
  const mermaid = await mermaidLoading;
  if (mermaidTheme !== theme) {
    mermaid.initialize({
      startOnLoad: false,
      securityLevel: 'strict',
      theme: theme === 'dark' ? 'dark' : 'default',
      // Plain SVG text labels: the diagram is shown through <img>, where
      // HTML-in-foreignObject labels are not reliable.
      flowchart: { htmlLabels: false },
      htmlLabels: false,
    });
    mermaidTheme = theme;
  }
  const { svg } = await mermaid.render(`chat-mermaid-${++mermaidSeq}`, source);
  return sizedSvg(svg);
}

/**
 * Mermaid emits width="100%" with a max-width style, which gives an <img>
 * no intrinsic size. Pin width/height to the viewBox so the image renders at
 * its natural size (and CSS max-width scales it down on narrow screens).
 */
function sizedSvg(svg: string): string {
  const vb = /viewBox="[\d.-]+ [\d.-]+ ([\d.]+) ([\d.]+)"/.exec(svg);
  const w = vb?.[1];
  const h = vb?.[2];
  if (!w || !h) return svg;
  return svg.replace(/<svg\b([^>]*)>/, (_m, attrs: string) => {
    const rest = attrs.replace(/\s(width|height)="[^"]*"/g, '');
    return `<svg width="${Math.ceil(Number(w))}" height="${Math.ceil(Number(h))}"${rest}>`;
  });
}

function MermaidBlock({ code }: { code: string }): ReactNode {
  const inProgress = useContext(InProgress);
  const theme = useTheme();
  const [state, setState] = useState<{ src: string } | { error: string } | null>(null);
  const [showSource, setShowSource] = useState(false);

  useEffect(() => {
    if (inProgress) return;
    let live = true;
    renderMermaid(code, theme)
      .then((svg) => {
        if (live) setState({ src: `data:image/svg+xml;charset=utf-8,${encodeURIComponent(svg)}` });
      })
      .catch((err: unknown) => {
        if (live) setState({ error: err instanceof Error ? err.message : 'The diagram could not be drawn.' });
      });
    return () => {
      live = false;
    };
  }, [code, theme, inProgress]);

  if (inProgress || !state || 'error' in state || showSource) {
    return (
      <div className="chat-mermaid">
        {state && 'error' in state ? <p className="chat-mermaid-error small muted">Diagram error: {state.error}</p> : null}
        {showSource ? (
          <button type="button" className="chat-code-btn" onClick={() => setShowSource(false)}>
            Show diagram
          </button>
        ) : null}
        <CodeBlock code={code} lang="mermaid" />
      </div>
    );
  }
  return (
    <figure className="chat-mermaid">
      <img src={state.src} alt="Diagram" className="chat-mermaid-img" />
      <button type="button" className="chat-code-btn" onClick={() => setShowSource(true)}>
        Show source
      </button>
    </figure>
  );
}

// ---- Links and images -------------------------------------------------------

function RemoteImage({ src, alt }: { src: string; alt: string }): ReactNode {
  const [load, setLoad] = useState(false);
  let host = src;
  try {
    host = new URL(src).host;
  } catch {
    /* keep the raw text */
  }
  if (!load) {
    return (
      <button type="button" className="chat-image-load" onClick={() => setLoad(true)} title={src}>
        Load image from {host}
        {alt ? <span className="muted"> ({alt})</span> : null}
      </button>
    );
  }
  return <img src={src} alt={alt} referrerPolicy="no-referrer" className="chat-image" />;
}

const COMPONENTS: Components = {
  a({ href, children, node: _node, ...rest }) {
    if (!href) return <span className="chat-link-blocked">{children}</span>;
    if (href.startsWith('#')) {
      return (
        <a href={href} {...rest}>
          {children}
        </a>
      );
    }
    return (
      <a href={href} target="_blank" rel="noopener noreferrer nofollow" {...rest}>
        {children}
      </a>
    );
  },
  img({ src, alt }) {
    const s = typeof src === 'string' ? src : '';
    const a = alt ?? '';
    if (!s) return <span className="chat-image-blocked">{a ? `[image: ${a}]` : '[image]'}</span>;
    if (isSafeDataImage(s)) return <img src={s} alt={a} className="chat-image" />;
    return <RemoteImage src={s} alt={a} />;
  },
  table({ children, node: _node, ...rest }) {
    return (
      <div className="chat-table-scroll">
        <table {...rest}>{children}</table>
      </div>
    );
  },
  pre({ node }) {
    const code = (node?.children as HastLike[] | undefined)?.find((c) => c.type === 'element' && c.tagName === 'code');
    const lang = fenceLang(code);
    const text = hastText(code);
    if (lang.toLowerCase() === 'mermaid') return <MermaidBlock code={text.replace(/\n$/, '')} />;
    const kind = wholeDocument(text, lang);
    if (kind) return <FileCard code={text} lang={lang} kind={kind} />;
    return <CodeBlock code={text} lang={lang} />;
  },
};

// ---- Blocks -----------------------------------------------------------------

const Block = memo(function Block({ text }: { text: string }): ReactNode {
  const plugins = useRehypePlugins(mayHaveMath(text));
  // Only the last block can end inside a fence (the splitter never cuts
  // inside one). Close it for rendering and mark it in progress, so it shows
  // as a code block that is still arriving and a mermaid fence waits.
  const inProgress = hasOpenFence(text);
  const source = inProgress ? closeOpenFences(text) : text;
  return (
    <InProgress.Provider value={inProgress}>
      <ReactMarkdown
        remarkPlugins={REMARK_PLUGINS}
        rehypePlugins={plugins}
        components={COMPONENTS}
        urlTransform={(url, key, node) => safeUrl(url, key, node.tagName)}
      >
        {source}
      </ReactMarkdown>
    </InProgress.Provider>
  );
});

/**
 * Render a chat message (assistant reply or reasoning) as markdown. Blocks
 * are keyed by index and memoized on their text, so while `streaming` only
 * the block that is still growing re-renders.
 */
export function ChatMarkdown({ text, streaming = false }: { text: string; streaming?: boolean }): ReactNode {
  const blocks = useMemo(() => splitBlocks(normalizeMathDelimiters(text)), [text]);
  return (
    <div className="chat-md" data-streaming={streaming || undefined}>
      {blocks.map((b, i) => (
        <Block key={i} text={b} />
      ))}
    </div>
  );
}
