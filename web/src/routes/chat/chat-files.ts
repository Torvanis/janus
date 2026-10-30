// Code that is a whole file is offered as one: a complete HTML or SVG
// document becomes a file card (name, size, Preview, Download, Copy, Show
// code) instead of hundreds of lines of code in the reply, and every code
// block can be downloaded as a file.
//
// Preview runs the document in a sandboxed iframe WITHOUT allow-same-origin:
// it gets an opaque origin, so its scripts cannot read Janus cookies,
// storage or call the Janus API. It is never opened as a blob: URL in a tab
// (that would run it with the Janus origin).

const EXT: Record<string, string> = {
  html: 'html',
  htm: 'html',
  xhtml: 'html',
  svg: 'svg',
  xml: 'xml',
  js: 'js',
  javascript: 'js',
  jsx: 'jsx',
  mjs: 'js',
  ts: 'ts',
  typescript: 'ts',
  tsx: 'tsx',
  py: 'py',
  python: 'py',
  go: 'go',
  golang: 'go',
  rs: 'rs',
  rust: 'rs',
  java: 'java',
  kotlin: 'kt',
  kt: 'kt',
  c: 'c',
  h: 'h',
  cpp: 'cpp',
  'c++': 'cpp',
  cs: 'cs',
  csharp: 'cs',
  rb: 'rb',
  ruby: 'rb',
  php: 'php',
  swift: 'swift',
  sh: 'sh',
  bash: 'sh',
  shell: 'sh',
  zsh: 'sh',
  ps1: 'ps1',
  powershell: 'ps1',
  css: 'css',
  scss: 'scss',
  json: 'json',
  yaml: 'yaml',
  yml: 'yaml',
  toml: 'toml',
  ini: 'ini',
  sql: 'sql',
  md: 'md',
  markdown: 'md',
  csv: 'csv',
  dockerfile: 'Dockerfile',
  makefile: 'Makefile',
  lua: 'lua',
};

const MIME: Record<string, string> = {
  html: 'text/html',
  svg: 'image/svg+xml',
  json: 'application/json',
  css: 'text/css',
  js: 'text/javascript',
  csv: 'text/csv',
  md: 'text/markdown',
  xml: 'application/xml',
};

export type DocKind = 'html' | 'svg';

/** The kind of whole document a fence holds, or null for ordinary code. */
export function wholeDocument(code: string, lang: string): DocKind | null {
  const head = code.trimStart().slice(0, 400).toLowerCase();
  const l = lang.toLowerCase();
  if (
    (l === 'html' || l === 'htm' || l === 'xhtml' || l === '') &&
    (head.startsWith('<!doctype html') || head.startsWith('<html'))
  ) {
    return 'html';
  }
  if (
    (l === 'svg' || l === 'xml' || l === 'html' || l === '') &&
    /^(<\?xml[^>]*>\s*)?<svg[\s>]/.test(head) &&
    /<\/svg>\s*$/i.test(code)
  ) {
    return 'svg';
  }
  return null;
}

function slug(s: string): string {
  return s
    .normalize('NFKD')
    .replace(/[\u0300-\u036f]/g, '')
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 48);
}

/** A file name for a fence: the document's <title> when it has one. */
export function fileName(code: string, lang: string): string {
  const kind = wholeDocument(code, lang);
  const l = lang.toLowerCase();
  const ext = kind ?? EXT[l] ?? (l && /^[a-z0-9]{1,8}$/.test(l) ? l : 'txt');
  let base = '';
  if (kind === 'html') {
    const m = /<title[^>]*>([^<]*)<\/title>/i.exec(code);
    if (m?.[1]) base = slug(m[1].replace(/&[a-z]+;|&#\d+;/gi, ' '));
  }
  if (ext === 'Dockerfile' || ext === 'Makefile') return ext;
  return `${base || (kind ?? 'code')}.${ext}`;
}

// The preview's sandbox has no origin, so localStorage and sessionStorage
// throw — and generated games and apps keep scores and settings there, and
// stop on the first read. Give them an in-memory store that lasts as long as
// the preview. It holds nothing of Janus's and is never saved.
const STORAGE_SHIM =
  '<script>(function(){function S(){var m=new Map();return{get length(){return m.size},' +
  'key:function(i){return Array.from(m.keys())[i]??null},getItem:function(k){k=String(k);return m.has(k)?m.get(k):null},' +
  'setItem:function(k,v){m.set(String(k),String(v))},removeItem:function(k){m.delete(String(k))},clear:function(){m.clear()}}}' +
  "['localStorage','sessionStorage'].forEach(function(n){try{window[n].length}catch(e){" +
  'try{Object.defineProperty(window,n,{value:S(),configurable:true})}catch(_){}}})})();</script>';

/** The document as the preview runs it: storage shim first, before any script. */
export function previewDocument(text: string, kind: DocKind): string {
  if (kind !== 'html') return text;
  const head = /<head(\s[^>]*)?>/i.exec(text);
  if (head) return text.slice(0, head.index + head[0].length) + STORAGE_SHIM + text.slice(head.index + head[0].length);
  const html = /<html(\s[^>]*)?>/i.exec(text);
  if (html) return text.slice(0, html.index + html[0].length) + STORAGE_SHIM + text.slice(html.index + html[0].length);
  return STORAGE_SHIM + text;
}

export function mimeFor(name: string): string {
  const ext = name.includes('.') ? name.slice(name.lastIndexOf('.') + 1) : '';
  return `${MIME[ext] ?? 'text/plain'};charset=utf-8`;
}

export function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(bytes < 10240 ? 1 : 0)} KB`;
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
}

export function downloadText(name: string, text: string): void {
  const url = URL.createObjectURL(new Blob([text], { type: mimeFor(name) }));
  const a = document.createElement('a');
  a.href = url;
  a.download = name;
  a.rel = 'noopener';
  document.body.appendChild(a);
  a.click();
  a.remove();
  window.setTimeout(() => URL.revokeObjectURL(url), 10_000);
}
