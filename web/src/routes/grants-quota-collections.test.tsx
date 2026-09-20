import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, within, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, useLocation } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { GrantsPage } from './admin/Grants';
import { QuotaPage } from './Quota';
import { ToastProvider } from '../components/ui';
import { api } from '../lib/api';

function Probe() { return <output data-testid="url">{useLocation().search}</output>; }
function mount(page: 'grants' | 'quota', query = '') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={client}><MemoryRouter initialEntries={[`/${page}?${query}`]}><ToastProvider>
    {page === 'grants' ? <GrantsPage /> : <QuotaPage />}<Probe />
  </ToastProvider></MemoryRouter></QueryClientProvider>);
  return client;
}
const grants = Array.from({ length: 31 }, (_, i) => ({ id: `g${i}`, model_id: 'm', model_kind: 'model', model_name: 'Model', grantee_type: 'user', grantee_id: `u${i}`, grantee_name: `Person ${String(i).padStart(2, '0')}`, created_at: '' }));
const quotas = Array.from({ length: 31 }, (_, i) => ({ id: `q${i}`, metric: 'requests', metric_label: `Metric ${String(i).padStart(2, '0')}`, window: 'day', window_label: 'Daily', subject_type: 'user', subject_name: 'Me', model_name: `model-${i}`, breach_behavior: 'hard_kill', limit_value: 100, current_value: i, unit: 'count', percent: i, at_risk: i === 30, breached: false, reset_at: '2026-09-20T00:00:00Z' }));
beforeEach(() => {
  vi.restoreAllMocks();
  vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/v1/admin/grants') return { grants } as never;
    if (url === '/api/v1/admin/models') return { models: [] } as never;
    if (url === '/api/v1/dashboard/quota') return { quotas } as never;
    throw new Error(`Unexpected ${url}`);
  });
});

describe('complete grant and personal quota collections', () => {
  it('pages/searches the full expanded group and revokes the selected off-page match', async () => {
    const user = userEvent.setup();
    const remove = vi.spyOn(api, 'del').mockResolvedValue({});
    mount('grants', 'grant-rows-model-Model.size=10&unrelated=kept');
    const group = await screen.findByTestId('grants-group');
    expect(within(group).getAllByRole('button', { name: 'Revoke' })).toHaveLength(10);
    await user.click(within(group).getByRole('button', { name: 'Next' }));
    expect(within(group).getByRole('button', { name: 'Person 10' })).toBeTruthy();
    expect(screen.getByTestId('url').textContent).toContain('grant-rows-model-Model.page=10');
    await user.type(within(group).getByRole('searchbox'), 'Person 30');
    expect(within(group).getAllByRole('button', { name: 'Revoke' })).toHaveLength(1);
    expect(within(group).getByText('1 of 31 rows')).toBeTruthy();
    await user.click(within(group).getByRole('button', { name: 'Revoke' }));
    const dialog = screen.getByRole('dialog');
    expect(dialog.textContent).toContain('Person 30');
    await user.click(within(dialog).getByRole('button', { name: /Revoke/ }));
    await waitFor(() => expect(remove).toHaveBeenCalledWith('/api/v1/admin/grants/g30'));
    expect(screen.getByTestId('url').textContent).toContain('unrelated=kept');
  });

  it('restores child paging, clamps after deletion and resets hidden child pages on outer filters', async () => {
    const user = userEvent.setup();
    const client = mount('grants', 'grant-rows-model-Model.size=10&grant-rows-model-Model.page=30');
    const group = await screen.findByTestId('grants-group');
    expect(within(group).getByRole('button', { name: 'Person 30' })).toBeTruthy();
    client.setQueryData(['admin', 'grants'], { grants: grants.slice(0, 30) });
    await waitFor(() => expect(within(group).getAllByRole('button', { name: 'Revoke' })).toHaveLength(10));
    await waitFor(() => expect(screen.getByTestId('url').textContent).toContain('grant-rows-model-Model.page=20'));
    await user.click(within(group).getByRole('button', { name: 'Hide Model grants' }));
    await user.selectOptions(screen.getByRole('combobox', { name: 'Grantee' }), 'user:u0');
    expect(screen.getByTestId('url').textContent).not.toContain('.page=20');
    expect(screen.getByRole('button', { name: 'Person 00' })).toBeTruthy();
  });

  it('restores group size/page and clamps after the last group disappears', async () => {
    const user = userEvent.setup();
    const fleet = grants.map((g, i) => ({ ...g, model_name: `Model ${String(i).padStart(2, '0')}` }));
    vi.mocked(api.get).mockImplementation(async (url) => (url === '/api/v1/admin/grants' ? { grants: fleet } : { models: [] }) as never);
    const client = mount('grants', 'size=25&page=25');
    expect((await screen.findByTestId('grants-group-count')).textContent).toBe('6 of 31 models');
    client.setQueryData(['admin', 'grants'], { grants: fleet.slice(0, 25) });
    await waitFor(() => expect(screen.getAllByTestId('grants-group')).toHaveLength(25));
    await waitFor(() => expect(screen.getByTestId('url').textContent).not.toContain('page=25'));
    await user.selectOptions(screen.getByLabelText('Groups per page'), '12');
    expect(screen.getAllByTestId('grants-group')).toHaveLength(12);
  });

  it('sorts the complete quota inventory before paging, searches off-page cards and filters risk', async () => {
    const user = userEvent.setup();
    mount('quota', 'personal-quotas.size=10&personal-quotas.page=10');
    await screen.findByText('31 quotas in your complete authorized inventory');
    expect(screen.getAllByRole('article')).toHaveLength(10);
    expect(screen.getAllByRole('heading', { level: 2 })[0]?.textContent).toContain('Metric 20');
    await user.type(screen.getByRole('searchbox'), 'Metric 00');
    expect(screen.getAllByRole('article')).toHaveLength(1);
    expect(screen.getByText('1 of 31 rows')).toBeTruthy();
    await user.clear(screen.getByRole('searchbox'));
    await user.selectOptions(screen.getByLabelText('Quota status'), 'at-risk');
    expect(screen.getAllByRole('article')).toHaveLength(1);
    expect(screen.getByRole('heading', { level: 2 }).textContent).toContain('Metric 30');
    await user.click(screen.getByRole('button', { name: 'Clear status' }));
    await user.selectOptions(screen.getByLabelText('Sort quotas'), 'metric:asc');
    expect(screen.getAllByRole('heading', { level: 2 })[0]?.textContent).toContain('Metric 00');
    expect(screen.getByTestId('url').textContent).toContain('quota.order=metric%3Aasc');
  });
});
