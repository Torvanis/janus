import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { fileName, formatSize, mimeFor, previewDocument, wholeDocument } from './chat-files';
import { ChatMarkdown } from './Markdown';

const GAME = `<!DOCTYPE html>
<html lang="en">
<head><title>TETRIS — Neon</title></head>
<body><canvas id="c"></canvas><script>document.title = 'x';</script></body>
</html>
`;

describe('wholeDocument', () => {
  it('recognizes complete HTML and SVG documents, not snippets', () => {
    expect(wholeDocument(GAME, 'html')).toBe('html');
    expect(wholeDocument(GAME, '')).toBe('html');
    expect(wholeDocument('<div class="a">hi</div>', 'html')).toBeNull();
    expect(wholeDocument('<svg viewBox="0 0 10 10"><circle r="4"/></svg>', 'svg')).toBe('svg');
    expect(wholeDocument('<?xml version="1.0"?>\n<svg xmlns="x"></svg>\n', 'xml')).toBe('svg');
    expect(wholeDocument('<svg><g>', 'svg')).toBeNull();
    expect(wholeDocument(GAME, 'python')).toBeNull();
  });
});

describe('fileName', () => {
  it('names a document after its title and code after its language', () => {
    expect(fileName(GAME, 'html')).toBe('tetris-neon.html');
    expect(fileName('<!doctype html><html></html>', 'html')).toBe('html.html');
    expect(fileName('print(1)', 'python')).toBe('code.py');
    expect(fileName('FROM alpine', 'dockerfile')).toBe('Dockerfile');
    expect(fileName('x', '')).toBe('code.txt');
    expect(fileName('x', 'some lang!')).toBe('code.txt');
  });
  it('gives types and sizes people read', () => {
    expect(mimeFor('a.html')).toBe('text/html;charset=utf-8');
    expect(mimeFor('a.py')).toBe('text/plain;charset=utf-8');
    expect(formatSize(512)).toBe('512 B');
    expect(formatSize(45_561)).toBe('44 KB');
  });
});

describe('previewDocument', () => {
  it("puts the storage stand-in before any of the page's scripts", () => {
    const out = previewDocument(GAME, 'html');
    expect(out.indexOf('localStorage')).toBeLessThan(out.indexOf('<canvas'));
    expect(out.indexOf('localStorage')).toBeGreaterThan(out.indexOf('<head>'));
    expect(out.startsWith('<!DOCTYPE html>')).toBe(true); // stays in standards mode
    expect(previewDocument('<svg></svg>', 'svg')).toBe('<svg></svg>');
  });
});

describe('file card', () => {
  it('shows a whole HTML document as a file, with a sandboxed preview', () => {
    const { container } = render(<ChatMarkdown text={'Here it is:\n\n```html\n' + GAME + '```\n\nEnjoy.'} />);
    expect(screen.getByText('tetris-neon.html')).toBeTruthy();
    expect(container.querySelector('.chat-code')).toBeNull(); // no wall of code
    fireEvent.click(screen.getByRole('button', { name: 'Preview' }));
    const frame = container.querySelector('iframe');
    expect(frame?.getAttribute('sandbox')).toBe('allow-scripts allow-pointer-lock allow-modals');
    expect(frame?.getAttribute('sandbox')).not.toContain('allow-same-origin');
    expect(frame?.getAttribute('srcdoc')).toContain('<canvas id="c">');
    fireEvent.click(screen.getByRole('button', { name: 'Code' }));
    expect(container.querySelector('.chat-file .chat-code')).not.toBeNull();
  });

  it('downloads the file under its name', () => {
    const create = vi.fn(() => 'blob:x');
    const revoke = vi.fn();
    Object.assign(URL, { createObjectURL: create, revokeObjectURL: revoke });
    const clicked: string[] = [];
    const orig = HTMLAnchorElement.prototype.click;
    HTMLAnchorElement.prototype.click = function (this: HTMLAnchorElement) {
      clicked.push(this.download);
    };
    try {
      render(<ChatMarkdown text={'```html\n' + GAME + '```'} />);
      fireEvent.click(screen.getByRole('button', { name: 'Download' }));
      expect(clicked).toEqual(['tetris-neon.html']);
      expect(create).toHaveBeenCalledOnce();
    } finally {
      HTMLAnchorElement.prototype.click = orig;
    }
  });

  it('shows progress while the file is still arriving', () => {
    render(<ChatMarkdown text={'```html\n<!DOCTYPE html>\n<html>\n<head>'} streaming />);
    expect(screen.getByText(/Writing… 3 lines/)).toBeTruthy();
    expect((screen.getByRole('button', { name: 'Preview' }) as HTMLButtonElement).disabled).toBe(true);
  });

  it('leaves ordinary code as code, with a Download button', () => {
    const { container } = render(<ChatMarkdown text={'```python\nprint(1)\n```'} />);
    expect(container.querySelector('.chat-file')).toBeNull();
    expect(screen.getByRole('button', { name: 'Download code.py' })).toBeTruthy();
  });
});
