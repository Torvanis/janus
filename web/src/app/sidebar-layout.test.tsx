import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { Shell } from './Shell';
import { SessionContext } from './session';

vi.mock('./ActiveTeamSelector', () => ({ ActiveTeamSelector: () => null }));
vi.mock('./LicenseBanner', () => ({ LicenseBanner: () => null }));
vi.mock('../components/AmbientCanvas', () => ({ AmbientCanvas: () => null }));
vi.mock('../lib/api', () => ({ api: { get: vi.fn(async () => ({})) } }));

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  localStorage.clear();
});

function renderAt(path: string, role = 'admin') {
  vi.stubGlobal(
    'matchMedia',
    vi.fn(() => ({ matches: false, addEventListener: vi.fn(), removeEventListener: vi.fn() })),
  );
  return render(
    <QueryClientProvider client={new QueryClient()}>
      <SessionContext.Provider
        value={
          {
            me: { id: 'me', name: 'Admin', email: 'a@example.com', role, groups: [], teams: [], chat_preview: true },
            config: null,
            refresh: vi.fn(),
            loading: false,
            error: null,
          } as any
        }
      >
        <MemoryRouter initialEntries={[path]}>
          <Shell>
            <p>page</p>
          </Shell>
        </MemoryRouter>
      </SessionContext.Provider>
    </QueryClientProvider>,
  );
}

it('keeps the live readout outside the scrolling menu, so it never scrolls away', () => {
  renderAt('/dashboard');
  const sidebar = screen.getByRole('navigation', { name: /primary/i });
  const scroll = sidebar.querySelector('.sidebar-scroll');
  const footer = sidebar.querySelector('.sidebar-footer');
  expect(scroll).toBeTruthy();
  expect(footer).toBeTruthy();
  expect(scroll!.contains(footer)).toBe(false);
  expect(footer!.textContent).toMatch(/req\/min/i);
});

it('groups the personal menu and folds administration, opening the section that holds the page', () => {
  renderAt('/admin/security');
  const sidebar = screen.getByRole('navigation', { name: /primary/i });
  // Personal headings are plain labels.
  expect(within(sidebar).getByText('Usage')).toBeTruthy();
  expect(within(sidebar).getByText('Access')).toBeTruthy();
  // The Security section holds the current page: open, and cannot be folded away.
  const security = within(sidebar).getByRole('button', { name: /Security/ });
  expect(security.getAttribute('aria-expanded')).toBe('true');
  expect((security as HTMLButtonElement).disabled).toBe(true);
  expect(within(sidebar).getByRole('link', { name: 'Policy rules' })).toBeTruthy();
  // Others start folded; their links are hidden until opened.
  const gateway = within(sidebar).getByRole('button', { name: /Gateway/ });
  expect(gateway.getAttribute('aria-expanded')).toBe('false');
  expect(within(sidebar).queryByRole('link', { name: 'Upstreams' })).toBeNull();
  fireEvent.click(gateway);
  expect(within(sidebar).getByRole('link', { name: 'Upstreams' })).toBeTruthy();
  // The choice is remembered.
  expect(JSON.parse(localStorage.getItem('janus.nav.open') ?? '{}')['admin-gateway']).toBe(true);
});

it('shows no administration to non-admins', () => {
  renderAt('/dashboard', 'user');
  expect(screen.queryByText('Administration')).toBeNull();
  expect(screen.queryByRole('button', { name: /Gateway/ })).toBeNull();
});

it('puts Chat at the top of administration, grants and service tokens under Gateway, and usage insights under People & Usage', () => {
  renderAt('/admin/chat');
  const sidebar = screen.getByRole('navigation', { name: /primary/i });
  const section = (id: string) => sidebar.querySelector(`[data-section="${id}"]`) as HTMLElement;
  const links = (id: string) =>
    within(section(id))
      .queryAllByRole('link', { hidden: true })
      .map((a) => a.getAttribute('href'));
  // Chat is its own entry, not inside a folded group.
  expect(links('admin-chat')).toEqual(['/admin/chat', '/admin/settings']);
  expect(within(section('admin-chat')).queryByRole('button')).toBeNull();
  expect(links('admin-gateway')).toEqual(expect.arrayContaining(['/admin/grants', '/admin/service-tokens']));
  expect(links('admin-gateway')).not.toContain('/admin/chat');
  expect(within(sidebar).getByRole('button', { name: /People & Usage/ })).toBeTruthy();
  expect(within(sidebar).queryByRole('button', { name: /People & access/ })).toBeNull();
  expect(links('admin-people')).toEqual(['/admin/users', '/admin/quotas', '/admin/insights']);
  expect(links('admin-activity')).toEqual(['/admin/requests', '/admin/audit']);
});

it('puts Overview at the very top of administration, on its own', () => {
  renderAt('/admin');
  const sidebar = screen.getByRole('navigation', { name: /primary/i });
  const sections = [...sidebar.querySelectorAll<HTMLElement>('[data-section^="admin-"]')];
  const first = sections[0]!;
  expect(first.dataset.section).toBe('admin-overview');
  expect(
    within(first)
      .getAllByRole('link')
      .map((a) => a.getAttribute('href')),
  ).toEqual(['/admin']);
  expect(within(first).queryByRole('button')).toBeNull();
  const gateway = sidebar.querySelector('[data-section="admin-gateway"]') as HTMLElement;
  expect(
    within(gateway)
      .queryAllByRole('link', { hidden: true })
      .map((a) => a.getAttribute('href')),
  ).not.toContain('/admin');
});

it('keeps header policies out of the menu: they are a tab of Policy rules', () => {
  renderAt('/admin/rules/headers');
  const sidebar = screen.getByRole('navigation', { name: /primary/i });
  const hrefs = Array.from(sidebar.querySelectorAll('a')).map((a) => a.getAttribute('href'));
  expect(hrefs).not.toContain('/admin/header-policies');
  const security = sidebar.querySelector('[data-section="admin-security"]')!;
  const rules = within(security as HTMLElement).getByRole('link', { name: /Policy rules/ });
  expect(rules.getAttribute('aria-current')).toBe('page');
});

it('makes section titles a heading with a chevron, and sets their pages apart from them', () => {
  renderAt('/admin/requests');
  const sidebar = screen.getByRole('navigation', { name: /primary/i });
  const gateway = within(sidebar).getByRole('button', { name: /Gateway/ });
  expect(gateway.getAttribute('aria-expanded')).toBe('false');
  expect(gateway.querySelector('svg.chevron.is-collapsed')).toBeTruthy();
  expect(gateway.textContent).not.toContain('▸');
  fireEvent.click(gateway);
  expect(gateway.getAttribute('aria-expanded')).toBe('true');
  expect(gateway.querySelector('svg.chevron.is-collapsed')).toBeNull();
  const group = sidebar.querySelector('#nav-admin-gateway')!;
  expect(group.classList.contains('nav-group-folded')).toBe(true);
  // Unlabeled, standalone entries (Overview, Chat, Settings) are not indented.
  expect(sidebar.querySelector('#nav-admin-overview')!.classList.contains('nav-group-folded')).toBe(false);
});
