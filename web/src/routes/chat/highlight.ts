/**
 * Lazy syntax highlighting for chat code blocks.
 *
 * Fine-grained shiki: the core, the JavaScript regex engine (no WASM, so the
 * CSP needs no 'wasm-unsafe-eval'), two themes and only the grammars a reply
 * actually uses, each a separate dynamic-import chunk. Output is tokens, not
 * HTML: the component renders them as React spans, so nothing here is ever
 * injected as markup.
 */
import type { HighlighterCore, LanguageRegistration, ThemedToken } from 'shiki/core';

type LangModule = { default: LanguageRegistration[] };

// Literal import() calls so Vite emits one chunk per grammar.
const LANGS: Record<string, () => Promise<LangModule>> = {
  typescript: () => import('shiki/langs/typescript.mjs'),
  javascript: () => import('shiki/langs/javascript.mjs'),
  tsx: () => import('shiki/langs/tsx.mjs'),
  jsx: () => import('shiki/langs/jsx.mjs'),
  json: () => import('shiki/langs/json.mjs'),
  python: () => import('shiki/langs/python.mjs'),
  go: () => import('shiki/langs/go.mjs'),
  rust: () => import('shiki/langs/rust.mjs'),
  java: () => import('shiki/langs/java.mjs'),
  c: () => import('shiki/langs/c.mjs'),
  cpp: () => import('shiki/langs/cpp.mjs'),
  csharp: () => import('shiki/langs/csharp.mjs'),
  shellscript: () => import('shiki/langs/shellscript.mjs'),
  sql: () => import('shiki/langs/sql.mjs'),
  yaml: () => import('shiki/langs/yaml.mjs'),
  toml: () => import('shiki/langs/toml.mjs'),
  html: () => import('shiki/langs/html.mjs'),
  css: () => import('shiki/langs/css.mjs'),
  markdown: () => import('shiki/langs/markdown.mjs'),
  diff: () => import('shiki/langs/diff.mjs'),
  dockerfile: () => import('shiki/langs/dockerfile.mjs'),
  php: () => import('shiki/langs/php.mjs'),
  ruby: () => import('shiki/langs/ruby.mjs'),
  kotlin: () => import('shiki/langs/kotlin.mjs'),
  swift: () => import('shiki/langs/swift.mjs'),
};

const ALIASES: Record<string, string> = {
  ts: 'typescript',
  mts: 'typescript',
  cts: 'typescript',
  js: 'javascript',
  mjs: 'javascript',
  cjs: 'javascript',
  node: 'javascript',
  jsonc: 'json',
  json5: 'json',
  py: 'python',
  python3: 'python',
  golang: 'go',
  rs: 'rust',
  h: 'c',
  'c++': 'cpp',
  cc: 'cpp',
  hpp: 'cpp',
  cxx: 'cpp',
  cs: 'csharp',
  'c#': 'csharp',
  bash: 'shellscript',
  sh: 'shellscript',
  shell: 'shellscript',
  zsh: 'shellscript',
  console: 'shellscript',
  yml: 'yaml',
  htm: 'html',
  xml: 'html',
  md: 'markdown',
  patch: 'diff',
  docker: 'dockerfile',
  rb: 'ruby',
  kt: 'kotlin',
  kts: 'kotlin',
};

/** Canonical grammar id for a fence's info string, or null if unsupported. */
export function resolveLang(info: string): string | null {
  const l = info.trim().toLowerCase();
  if (!l) return null;
  if (LANGS[l]) return l;
  return ALIASES[l] ?? null;
}

let core: Promise<HighlighterCore> | null = null;
const loaded = new Map<string, Promise<void>>();

function highlighter(): Promise<HighlighterCore> {
  core ??= (async () => {
    const [{ createHighlighterCore }, { createJavaScriptRegexEngine }] = await Promise.all([
      import('shiki/core'),
      import('shiki/engine/javascript'),
    ]);
    return createHighlighterCore({
      themes: [import('shiki/themes/github-light.mjs'), import('shiki/themes/github-dark.mjs')],
      langs: [],
      engine: createJavaScriptRegexEngine({ forgiving: true }),
    });
  })();
  return core;
}

export interface HighlightToken {
  content: string;
  light?: string;
  dark?: string;
  fontStyle?: number;
}

function toToken(t: ThemedToken): HighlightToken {
  const style = (t.htmlStyle ?? {}) as Record<string, string>;
  return { content: t.content, light: style['--shiki-light'], dark: style['--shiki-dark'], fontStyle: t.fontStyle };
}

/** Tokenize code into lines of dual-theme tokens; null when the language is unknown. */
export async function highlight(code: string, info: string): Promise<HighlightToken[][] | null> {
  const lang = resolveLang(info);
  const load = lang ? LANGS[lang] : undefined;
  if (!lang || !load) return null;
  const h = await highlighter();
  let ready = loaded.get(lang);
  if (!ready) {
    ready = load().then((m) => h.loadLanguage(...m.default));
    loaded.set(lang, ready);
  }
  await ready;
  const res = h.codeToTokens(code, {
    lang,
    themes: { light: 'github-light', dark: 'github-dark' },
    defaultColor: false,
  });
  return res.tokens.map((line) => line.map(toToken));
}
