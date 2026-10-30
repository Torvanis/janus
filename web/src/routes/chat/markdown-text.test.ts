import { describe, expect, it } from 'vitest';
import { closeOpenFences, hasOpenFence, isSafeDataImage, normalizeMathDelimiters, safeUrl, splitBlocks } from './markdown-text';

describe('closeOpenFences', () => {
  it('leaves balanced text alone', () => {
    const t = 'a\n```js\nx\n```\nb';
    expect(closeOpenFences(t)).toBe(t);
    expect(hasOpenFence(t)).toBe(false);
    expect(closeOpenFences('no fences')).toBe('no fences');
  });

  it('closes an open fence while streaming', () => {
    expect(closeOpenFences('intro\n```python\nprint(1)')).toBe('intro\n```python\nprint(1)\n```');
    expect(closeOpenFences('```\n')).toBe('```\n```');
    expect(hasOpenFence('```go\nfunc')).toBe(true);
  });

  it('matches the opener length and character', () => {
    expect(closeOpenFences('````md\n```js\nx\n```')).toBe('````md\n```js\nx\n```\n````');
    expect(closeOpenFences('~~~\ncode')).toBe('~~~\ncode\n~~~');
  });

  it('counts a second opener after a closed block', () => {
    expect(closeOpenFences('```\na\n```\n\n```ts\nb')).toBe('```\na\n```\n\n```ts\nb\n```');
  });

  it('ignores triple backticks inline', () => {
    const t = 'use ```inline``` here';
    expect(closeOpenFences(t)).toBe(t);
  });
});

describe('normalizeMathDelimiters', () => {
  it('rewrites \\( \\) and \\[ \\] in prose', () => {
    expect(normalizeMathDelimiters('area \\(\\pi r^2\\) ok')).toBe('area $\\pi r^2$ ok');
    expect(normalizeMathDelimiters('\\[x^2\\]')).toBe('\n$$\nx^2\n$$\n');
    expect(normalizeMathDelimiters('so \\[a+b\\] inline')).toBe('so $$a+b$$ inline');
  });

  it('skips fenced code and inline code', () => {
    const fenced = '```latex\n\\(x\\)\n```\nthen \\(y\\)';
    expect(normalizeMathDelimiters(fenced)).toBe('```latex\n\\(x\\)\n```\nthen $y$');
    expect(normalizeMathDelimiters('`\\(x\\)` and \\(z\\)')).toBe('`\\(x\\)` and $z$');
  });
});

describe('splitBlocks', () => {
  it('splits at blank lines outside fences only', () => {
    const t = 'para one\n\n```\na\n\nb\n```\n\npara two';
    expect(splitBlocks(t)).toEqual(['para one', '```\na\n\nb\n```', 'para two']);
  });

  it('keeps loose lists, indented continuations and display math together', () => {
    expect(splitBlocks('- a\n\n- b')).toEqual(['- a\n\n- b']);
    expect(splitBlocks('1. a\n\n    more')).toEqual(['1. a\n\n    more']);
    expect(splitBlocks('$$\na\n\nb\n$$\n\nafter')).toEqual(['$$\na\n\nb\n$$', 'after']);
  });

  it('does not split text with footnote or reference definitions', () => {
    const t = 'see [^1]\n\n[^1]: note';
    expect(splitBlocks(t)).toEqual([t]);
  });
});

describe('url allow-list', () => {
  it('allows web and mail links only', () => {
    expect(safeUrl('https://x.test/a', 'href', 'a')).toBe('https://x.test/a');
    expect(safeUrl('mailto:a@b.test', 'href', 'a')).toBe('mailto:a@b.test');
    expect(safeUrl('#fn-1', 'href', 'a')).toBe('#fn-1');
    expect(safeUrl('javascript:alert(1)', 'href', 'a')).toBe('');
    expect(safeUrl(' JavaScript:alert(1)', 'href', 'a')).toBe('');
    expect(safeUrl('data:text/html,<b>', 'href', 'a')).toBe('');
    expect(safeUrl('vbscript:x', 'href', 'a')).toBe('');
    expect(safeUrl('/relative', 'href', 'a')).toBe('');
  });

  it('allows raster data images but not svg', () => {
    expect(isSafeDataImage('data:image/png;base64,AAAA')).toBe(true);
    expect(isSafeDataImage('data:image/webp;base64,AAAA')).toBe(true);
    expect(isSafeDataImage('data:image/svg+xml,<svg/>')).toBe(false);
    expect(safeUrl('data:image/svg+xml,<svg/>', 'src', 'img')).toBe('');
    expect(safeUrl('https://cdn.test/a.png', 'src', 'img')).toBe('https://cdn.test/a.png');
  });
});
