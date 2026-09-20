/**
 * Model grants — list layout contracts.
 *
 * Many grants across many models must stay navigable: cards collapse to a
 * preview, page twelve at a time, can be regrouped by grantee, and can be
 * narrowed to one person so "what can X call?" has a direct answer. Filters
 * and grouping live in the URL.
 */
import { describe, expect, it, vi, beforeEach } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter, useLocation } from 'react-router-dom';
import { GrantsPage } from './Grants';
import { ToastProvider } from '../../components/ui';
import type { Grant } from '../../lib/types';

function grant(over: Partial<Grant> & Pick<Grant, 'id' | 'model_name'>): Grant {
  return {
    model_id: `id-${over.model_name}`,
    model_kind: 'model',
    grantee_type: 'user',
    grantee_id: 'u-1',
    grantee_name: 'Ada Lovelace',
    created_at: '2026-01-01T00:00:00Z',
    ...over,
  };
}

/** 15 models × several grantees: enough to page and to make cards collapse. */
function bigFleet(): Grant[] {
  const out: Grant[] = [];
  for (let m = 1; m <= 15; m++) {
    const model = `model-${String(m).padStart(2, '0')}`;
    out.push(grant({ id: `${model}-ada`, model_name: model }));
    out.push(
      grant({ id: `${model}-grp`, model_name: model, grantee_type: 'group', grantee_id: 'g-1', grantee_name: 'Research' }),
    );
    out.push(
      grant({
        id: `${model}-svc`,
        model_name: model,
        grantee_type: 'service_token',
        grantee_id: 'st-1',
        grantee_name: 'nightly-job',
      }),
    );
    if (m % 2 === 0) out.push(grant({ id: `${model}-bob`, model_name: model, grantee_id: 'u-2', grantee_name: 'Bob Byte' }));
    if (m === 1) {
      out.push(
        grant({ id: `${model}-all`, model_name: model, grantee_type: 'all_users', grantee_id: '', grantee_name: 'All users' }),
      );
      out.push(grant({ id: `${model}-eve`, model_name: model, grantee_id: 'u-3', grantee_name: 'Eve Ember' }));
    }
  }
  return out;
}

function mockFetch(grants: Grant[]) {
  return vi.fn(async (input: RequestInfo | URL) => {
    const url = typeof input === 'string' ? input : input.toString();
    const body = url.includes('/admin/grants') ? { grants } : { models: [] };
    return {
      ok: true,
      status: 200,
      headers: new Headers({ 'content-type': 'application/json' }),
      json: async () => body,
      text: async () => JSON.stringify(body),
    } as Response;
  });
}

function LocationProbe() {
  const location = useLocation();
  return <output data-testid="location">{location.search}</output>;
}

function renderPage(url = '/admin/grants') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter initialEntries={[url]}>
          <GrantsPage />
          <LocationProbe />
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.restoreAllMocks();
});

describe('GrantsPage — scalable list', () => {
  it('pages model cards twelve at a time and collapses them to a preview', async () => {
    vi.stubGlobal('fetch', mockFetch(bigFleet()));
    renderPage();
    const user = userEvent.setup();

    expect((await screen.findByTestId('grants-group-count')).textContent).toBe('12 of 15 models');
    expect(screen.getAllByTestId('grants-group')).toHaveLength(12);

    // With many cards each one is collapsed: a preview of who has access, a
    // "+N more" tail, and no Revoke buttons until it is opened.
    const first = screen.getAllByTestId('grants-group')[0]!;
    expect(within(first).getByRole('heading', { name: /model-01/ })).toBeTruthy();
    expect(within(first).getByText('5 grants')).toBeTruthy();
    expect(within(first).getByText('+1 more')).toBeTruthy();
    expect(within(first).queryByRole('button', { name: 'Revoke' })).toBeNull();

    await user.click(within(first).getByRole('button', { name: 'Show model-01 grants' }));
    expect(within(first).getAllByRole('button', { name: 'Revoke' })).toHaveLength(5);
    // Every grantee kind is labelled for what it is.
    expect(within(first).getByText('Service token')).toBeTruthy();
    expect(within(first).getAllByText('All users').length).toBeGreaterThanOrEqual(1);
    expect(within(first).getByText('Group')).toBeTruthy();
    await user.click(within(first).getByRole('button', { name: 'Hide model-01 grants' }));
    expect(within(first).queryByRole('button', { name: 'Revoke' })).toBeNull();

    // Expand all / collapse all.
    await user.click(screen.getByRole('button', { name: 'Expand all' }));
    expect(screen.getAllByRole('button', { name: 'Revoke' }).length).toBeGreaterThan(30);
    await user.click(screen.getByRole('button', { name: 'Collapse all' }));
    expect(screen.queryByRole('button', { name: 'Revoke' })).toBeNull();

    // Page two holds the remaining three models and is in the URL.
    await user.click(screen.getByRole('button', { name: 'Next' }));
    expect(screen.getByTestId('grants-group-count').textContent).toBe('3 of 15 models');
    expect(screen.getByRole('heading', { name: /model-15/ })).toBeTruthy();
    expect(screen.getByTestId('location').textContent).toContain('page=12');
  });

  it('answers "what can this person call?" via the grantee filter and grantee view', async () => {
    vi.stubGlobal('fetch', mockFetch(bigFleet()));
    renderPage();
    const user = userEvent.setup();
    await screen.findByTestId('grants-group-count');

    // The grantee dropdown lists every distinct grantee, grouped by kind.
    const who = screen.getByRole('combobox', { name: 'Grantee' });
    expect(within(who).getByRole('group', { name: 'People' })).toBeTruthy();
    expect(within(who).getByRole('group', { name: 'Groups' })).toBeTruthy();
    expect(within(who).getByRole('group', { name: 'Service tokens' })).toBeTruthy();
    expect(within(who).getByRole('group', { name: 'Blanket grants' })).toBeTruthy();
    expect(within(who).getAllByRole('option', { name: 'Bob Byte' })).toHaveLength(1);

    await user.selectOptions(who, 'user:u-2');
    // Only Bob's models remain, cards open because the view is focused, and
    // the banner names him. The choice is deep-linkable.
    expect(screen.getByText('Everything Bob Byte can call')).toBeTruthy();
    expect(screen.getByTestId('grants-group-count').textContent).toBe('7 of 7 models');
    expect(screen.getAllByRole('button', { name: 'Revoke' })).toHaveLength(7);
    expect(screen.queryByRole('heading', { name: /model-01/ })).toBeNull();
    expect(screen.getByTestId('location').textContent).toContain('grantee=user%3Au-2');

    // Regroup by grantee: one card for Bob listing his models.
    await user.click(screen.getByRole('button', { name: 'By grantee' }));
    expect(screen.getByTestId('grants-group-count').textContent).toBe('1 of 1 grantees');
    const card = screen.getByTestId('grants-group');
    expect(within(card).getByRole('heading', { name: /Bob Byte/ })).toBeTruthy();
    expect(within(card).getByText('7 models')).toBeTruthy();
    expect(within(card).getByText('model-02')).toBeTruthy();
    expect(screen.getByTestId('location').textContent).toContain('view=grantee');

    // Clearing filters returns to everyone, still grouped by grantee (6 grantees).
    await user.click(screen.getByRole('button', { name: 'Clear filters' }));
    expect(screen.getByTestId('grants-group-count').textContent).toBe('6 of 6 grantees');
    expect(screen.getByTestId('location').textContent).not.toContain('grantee=');

    // The grantee-type filter narrows to one kind; the URL carries it.
    await user.selectOptions(screen.getByRole('combobox', { name: 'Grantee type' }), 'service_token');
    expect(screen.getByTestId('grants-group-count').textContent).toBe('1 of 1 grantees');
    expect(screen.getByRole('heading', { name: /nightly-job/ })).toBeTruthy();
    expect(screen.getByTestId('location').textContent).toContain('type=service_token');
  });

  it('restores view, grantee and page from the URL and lets a grantee name focus that grantee', async () => {
    vi.stubGlobal('fetch', mockFetch(bigFleet()));
    renderPage('/admin/grants?view=grantee&grantee=group%3Ag-1');
    const user = userEvent.setup();

    expect((await screen.findByTestId('grants-group-count')).textContent).toBe('1 of 1 grantees');
    expect(screen.getByRole('heading', { name: /Research/ })).toBeTruthy();
    expect((screen.getByRole('button', { name: 'By grantee' }) as HTMLButtonElement).getAttribute('aria-pressed')).toBe('true');
    expect((screen.getByRole('combobox', { name: 'Grantee' }) as HTMLSelectElement).value).toBe('group:g-1');

    // Back to the model view, open a card, click a grantee name → focused.
    await user.click(screen.getByRole('button', { name: 'Clear filters' }));
    await user.click(screen.getByRole('button', { name: 'By model' }));
    const first = screen.getAllByTestId('grants-group')[0]!;
    await user.click(within(first).getByRole('button', { name: 'Show model-01 grants' }));
    await user.click(within(first).getByRole('button', { name: 'Eve Ember' }));
    expect(screen.getByText('Everything Eve Ember can call')).toBeTruthy();
    expect(screen.getByTestId('grants-group-count').textContent).toBe('1 of 1 models');
  });

  it('opens cards by default when there are only a few', async () => {
    vi.stubGlobal(
      'fetch',
      mockFetch([
        grant({ id: 'a', model_name: 'gpt-4o' }),
        grant({ id: 'b', model_name: 'claude', grantee_id: 'u-2', grantee_name: 'Bob Byte' }),
      ]),
    );
    renderPage();
    expect((await screen.findByTestId('grants-group-count')).textContent).toBe('2 of 2 models');
    expect(screen.getAllByRole('button', { name: 'Revoke' })).toHaveLength(2);
    expect(screen.queryByRole('navigation', { name: 'Pagination' })).toBeNull();
  });
});
