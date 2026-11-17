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

// A preview runs a page the model wrote, and the model writes what it was
// led to write: a web page it read, an uploaded document or a shared chat
// can tell it to build a page that sends the conversation somewhere when it
// is opened. The sandbox keeps the page away from Janus; it does not keep it
// off the network. This policy does: no request of any kind leaves the
// preview. Scripts, styles, images, fonts and media written into the page
// itself (inline, data: and blob:) still work.
//
// One thing a page's own policy cannot stop is the page navigating its
// frame to another address. The console's own policy does that (frame-src
// 'self', internal/httpapi/security_headers.go). The preview page is also
// served with this policy as a header; writing it into the document as well
// costs nothing and holds if the document is ever shown another way.
export const PREVIEW_POLICY =
  "default-src 'none'; script-src 'unsafe-inline' 'unsafe-eval' blob: data:; style-src 'unsafe-inline'; " +
  'img-src data: blob:; media-src data: blob:; font-src data:; worker-src blob:; connect-src data: blob:; ' +
  "form-action 'none'; base-uri 'none'; webrtc 'block'";

/**
 * The page a preview is loaded from (internal/httpapi/security_headers.go).
 * It carries the policy above as a response header and a sandbox with no
 * origin, asks the chat page for the document (PREVIEW_READY), and writes
 * what it is sent (PREVIEW_DOCUMENT). A page of its own, not a srcdoc frame,
 * because a srcdoc frame takes on the console's policy, which allows no
 * inline script.
 */
export const PREVIEW_FRAME = '/chat/preview-frame';
export const PREVIEW_READY = 'janusPreviewReady';
export const PREVIEW_DOCUMENT = 'janusPreviewDocument';

/** The message a preview posts to the page that holds it when a request was blocked. */
export const PREVIEW_BLOCKED = 'janusPreviewBlocked';

// Tells the chat page what was blocked, so the person is told (see FileCard).
const BLOCKED_REPORT =
  "<script>document.addEventListener('securitypolicyviolation',function(e){try{parent.postMessage({" +
  PREVIEW_BLOCKED +
  ":String(e.blockedURI||'')},'*')}catch(_){}});</script>";

/**
 * The document the preview page is sent. The policy, then the report and the
 * storage stand-in, go before everything the model wrote: a policy applies
 * only to what follows it, so nothing may come first. (Looking for the
 * page's own <head> to put them in would let a script written above it run
 * without the policy.)
 */
export function previewDocument(text: string, kind: DocKind): string {
  const guard = `<!DOCTYPE html><meta http-equiv="Content-Security-Policy" content="${PREVIEW_POLICY}">${BLOCKED_REPORT}`;
  return kind === 'html' ? guard + STORAGE_SHIM + text : guard + text;
}

/**
 * The host a blocked request was for, or '' when it was not a network
 * request (the policy also reports things like a blocked inline handler).
 */
export function blockedHost(uri: unknown): string {
  if (typeof uri !== 'string' || !/^(https?|wss?|ftp):/i.test(uri)) return '';
  try {
    return new URL(uri).host.slice(0, 255);
  } catch {
    return '';
  }
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
