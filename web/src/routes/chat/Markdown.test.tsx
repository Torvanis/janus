import { describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { ChatMarkdown } from './Markdown';

// shiki's grammars and mermaid's layout engine are heavy and need a real
// browser; their wiring is proven by the production build. Here a fake
// highlighter returns one token per line so the swap-in path is exercised.
vi.mock('./highlight', () => ({
  highlight: vi.fn(async (code: string, info: string) =>
    info === 'nope' ? null : code.split('\n').map((l) => [{ content: l, light: '#111111', dark: '#eeeeee' }]),
  ),
}));
const mermaidRender = vi.fn(async () => ({ svg: '<svg xmlns="http://www.w3.org/2000/svg"></svg>' }));
vi.mock('mermaid', () => ({ default: { initialize: vi.fn(), render: mermaidRender } }));

describe('ChatMarkdown', () => {
  it('renders raw HTML from the model as text', () => {
    const { container } = render(
      <ChatMarkdown text={'hi <script>alert(1)</script> and <img src=x onerror="alert(2)"> <b>bold</b>'} />,
    );
    expect(container.querySelector('script')).toBeNull();
    expect(container.querySelector('img')).toBeNull();
    expect(container.querySelector('b')).toBeNull();
    expect(container.textContent).toContain('<script>alert(1)</script>');
    expect(container.textContent).toContain('onerror');
  });

  it('does not turn javascript: links into anchors', () => {
    const { container } = render(<ChatMarkdown text={'[click](javascript:alert(1)) and [ok](https://example.test/x)'} />);
    const anchors = [...container.querySelectorAll('a')];
    expect(anchors.map((a) => a.getAttribute('href'))).toEqual(['https://example.test/x']);
    expect(screen.getByText('click').closest('a')).toBeNull();
    const ok = anchors.find((a) => a.textContent === 'ok');
    expect(ok?.getAttribute('target')).toBe('_blank');
    expect(ok?.getAttribute('rel')).toBe('noopener noreferrer nofollow');
  });

  it('does not load remote images until asked', () => {
    const { container } = render(<ChatMarkdown text={'![chart](https://tracker.example.test/p.png?u=1)'} />);
    expect(container.querySelector('img')).toBeNull();
    const btn = screen.getByRole('button', { name: /Load image from tracker\.example\.test/ });
    fireEvent.click(btn);
    const img = container.querySelector('img');
    expect(img?.getAttribute('src')).toBe('https://tracker.example.test/p.png?u=1');
    expect(img?.getAttribute('referrerpolicy')).toBe('no-referrer');
  });

  it('shows raster data images directly and refuses svg data images', () => {
    const { container } = render(
      <ChatMarkdown text={'![a](data:image/png;base64,iVBORw0KGgo=) ![b](data:image/svg+xml;base64,PHN2Zz4=)'} />,
    );
    const imgs = [...container.querySelectorAll('img')];
    expect(imgs).toHaveLength(1);
    expect(imgs.find(() => true)?.getAttribute('src')).toMatch(/^data:image\/png/);
    expect(screen.queryByRole('button', { name: /Load image/ })).toBeNull();
  });

  it('wraps tables in a scroll container and supports GFM', () => {
    const { container } = render(<ChatMarkdown text={'| a | b |\n| - | - |\n| 1 | 2 |\n\n- [x] done\n- [ ] todo\n\n~~old~~'} />);
    const table = container.querySelector('table');
    expect(table?.parentElement?.classList.contains('chat-table-scroll')).toBe(true);
    expect(container.querySelectorAll('input[type="checkbox"]')).toHaveLength(2);
    expect(container.querySelector('del')?.textContent).toBe('old');
  });

  it('renders footnotes', () => {
    const { container } = render(<ChatMarkdown text={'Claim[^1].\n\n[^1]: Source.'} />);
    expect(container.querySelector('.footnotes, section[data-footnotes]')).not.toBeNull();
  });

  it('renders code blocks with a header, copy and wrap', async () => {
    const { container } = render(<ChatMarkdown text={'```ts\nconst x = 1;\n```'} />);
    expect(screen.getByRole('button', { name: 'Copy code' })).toBeTruthy();
    expect(container.querySelector('.chat-code-lang')?.textContent).toBe('ts');
    const wrap = screen.getByRole('button', { name: 'Wrap' });
    fireEvent.click(wrap);
    expect(wrap.getAttribute('aria-pressed')).toBe('true');
    expect(container.querySelector('pre.wrap')).not.toBeNull();
    // Plain first, then highlighted tokens swap in.
    await waitFor(() => expect(container.querySelector('.chat-code-pre span[style]')).not.toBeNull());
    expect(container.querySelector('code')?.textContent).toBe('const x = 1;\n');
  });

  it('copies code and says so', async () => {
    const writeText = vi.fn(async () => undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    render(<ChatMarkdown text={'```\nls -la\n```'} />);
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Copy code' }));
    });
    expect(writeText).toHaveBeenCalledWith('ls -la');
    expect(screen.getByText('Copied')).toBeTruthy();
  });

  it('numbers lines only for long blocks', () => {
    const long = Array.from({ length: 20 }, (_, i) => `line ${i}`).join('\n');
    const { container, rerender } = render(<ChatMarkdown text={'```\na\nb\n```'} />);
    expect(container.querySelector('pre.numbered')).toBeNull();
    rerender(<ChatMarkdown text={'```\n' + long + '\n```'} />);
    expect(container.querySelector('pre.numbered')).not.toBeNull();
  });

  it('renders an unclosed fence as a code block in progress', () => {
    const { container } = render(<ChatMarkdown streaming text={'Here:\n\n```python\ndef f():\n\n    return 1'} />);
    const code = container.querySelector('.chat-code');
    expect(code?.getAttribute('data-in-progress')).toBe('true');
    expect(code?.textContent).toContain('return 1');
    expect(container.querySelectorAll('p')).toHaveLength(1);
  });

  it('renders KaTeX for $x^2$ and \\(x\\) delimiters', async () => {
    const { container } = render(<ChatMarkdown text={'Inline $x^2$ and \\(y_1\\).\n\n\\[\\frac{a}{b}\\]'} />);
    await waitFor(() => expect(container.querySelectorAll('.katex').length).toBeGreaterThanOrEqual(3));
    expect(container.querySelector('.katex-display')).not.toBeNull();
  });

  it('does not run KaTeX on code', async () => {
    const { container } = render(<ChatMarkdown text={'`$x$` and $y$'} />);
    await waitFor(() => expect(container.querySelector('.katex')).not.toBeNull());
    expect(container.querySelector('code')?.textContent).toBe('$x$');
  });

  it('renders mermaid only after the fence closes', async () => {
    mermaidRender.mockClear();
    const { container, rerender } = render(<ChatMarkdown streaming text={'```mermaid\ngraph TD\nA-->B'} />);
    await new Promise((r) => setTimeout(r, 20));
    expect(mermaidRender).not.toHaveBeenCalled();
    expect(container.querySelector('.chat-code')).not.toBeNull();
    rerender(<ChatMarkdown text={'```mermaid\ngraph TD\nA-->B\n```'} />);
    await waitFor(() => expect(container.querySelector('img.chat-mermaid-img')).not.toBeNull());
    expect(mermaidRender).toHaveBeenCalledTimes(1);
    expect(container.querySelector('img.chat-mermaid-img')?.getAttribute('src')).toMatch(/^data:image\/svg\+xml/);
    expect(container.querySelector('svg')).toBeNull();
  });

  it('renders a 2000-line message without throwing', () => {
    const parts: string[] = [];
    for (let i = 0; i < 400; i++) {
      parts.push(`## Section ${i}`, '', `Paragraph ${i} with **bold** and \`code\`.`, '', '- item', '');
    }
    parts.push('```js', ...Array.from({ length: 200 }, (_, i) => `console.log(${i});`), '```');
    const text = parts.join('\n');
    expect(text.split('\n').length).toBeGreaterThan(2000);
    const { container } = render(<ChatMarkdown text={text} />);
    expect(container.querySelectorAll('h2')).toHaveLength(400);
    expect(container.querySelector('pre.numbered')).not.toBeNull();
  });
});
