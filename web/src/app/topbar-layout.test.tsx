import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { Breadcrumb } from './Breadcrumb';

vi.mock('./session', () => ({ useSession: () => ({ me: { id: 'viewer', role: 'admin' } }) }));

afterEach(cleanup);

/** The declarations of the FIRST top-level rule for `selector` in a CSS file. */
function rule(file: string, selector: string): string {
  const css = readFileSync(join(__dirname, file), 'utf8');
  const at = css.indexOf(`\n${selector} {\n`);
  expect(at, `${selector} in ${file}`).toBeGreaterThanOrEqual(0);
  return css.slice(at, css.indexOf('}', at));
}

function renderCrumb(path: string) {
  const client = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, retry: false, enabled: false } } });
  render(
    <QueryClientProvider client={client}>
      <Breadcrumb path={path} />
    </QueryClientProvider>,
  );
  return screen.getByRole('navigation');
}

describe('top bar breadcrumb', () => {
  it('names every crumb in a tooltip and marks the current page', () => {
    const nav = renderCrumb('/admin/settings/sign-in');
    const names = [...nav.querySelectorAll('.breadcrumb-name')];
    expect(names.map((n) => n.getAttribute('title'))).toEqual(['Administration', 'Settings', 'Sign-in & provisioning']);
    expect(names.at(-1)?.getAttribute('aria-current')).toBe('page');
    expect(names.slice(0, -1).every((n) => !n.hasAttribute('aria-current'))).toBe(true);
  });

  it('turns an unknown route segment into sentence case words', () => {
    const nav = renderCrumb('/admin/managed-models');
    expect(nav.querySelector('.breadcrumb-last .breadcrumb-name')?.textContent).toBe('Managed models');
  });
});

describe('top bar layout contract', () => {
  it('grows with its content instead of clipping it at a fixed height', () => {
    const topbar = rule('shell.css', '.topbar');
    expect(topbar).toMatch(/min-height: var\(--janus-topbar-height\)/);
    expect(topbar).not.toMatch(/(^|\s)height:/);
  });

  it('keeps the breadcrumb on one shrinking line, middle crumbs first', () => {
    const crumb = rule('shell.css', '.breadcrumb');
    expect(crumb).toMatch(/white-space: nowrap/);
    expect(crumb).toMatch(/overflow: hidden/);
    expect(crumb).toMatch(/min-width: 0/);
    expect(rule('shell.css', '.breadcrumb-crumb')).toMatch(/flex: 1 1 0;/);
    expect(rule('shell.css', '.breadcrumb-last')).toMatch(/flex: 0 1 auto/);
    expect(rule('shell.css', '.breadcrumb-name')).toMatch(/text-overflow: ellipsis/);
  });
});

describe('chat layout contract', () => {
  it('sizes the history list to the rail and keeps the row menus in view', () => {
    expect(rule('../routes/chat/chat.css', '.chat-list')).toMatch(/grid-template-columns: minmax\(0, 1fr\)/);
    expect(rule('../routes/chat/chat.css', '.chat-list-scroll')).toMatch(/overflow-x: hidden/);
    expect(rule('../routes/chat/chat.css', '.chat-list-more')).not.toMatch(/opacity: 0/);
  });

  it('lets the usage meter shrink before the chat title does', () => {
    expect(rule('../routes/chat/chat.css', '.chat-meter')).toMatch(/flex: 0 1 auto/);
    expect(rule('../routes/chat/chat.css', '.chat-title')).toMatch(/min-width: min\(7rem, 40%\)/);
  });
});
