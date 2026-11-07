/**
 * Header policies admin page: which policy is in effect for each target,
 * assigning a policy, editing rules (secret values kept, never shown), the
 * header-logging switch, and the request-log Headers section.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import type { ReactNode } from 'react';
import { api } from '../../lib/api';
import { ToastProvider } from '../../components/ui';
import { HeaderPoliciesPanel, summarizePolicy, type HeaderPoliciesResponse } from './HeaderPolicies';
import { RequestHeadersSection } from './RequestHeaders';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return { ...actual, api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() } };
});
const mocked = vi.mocked(api);

const fixture: HeaderPoliciesResponse = {
  policies: [
    {
      id: 'hp_builtin_edge',
      name: 'Strip edge & proxy headers',
      description: '',
      allow_only: [],
      rules: [
        { action: 'remove', name: 'cf-*' },
        { action: 'remove', name: 'x-forwarded-*' },
      ],
      assignments: [{ target_type: 'default', target_id: '', policy_id: 'hp_builtin_edge' }],
      updated_at: '2026-10-31T00:00:00Z',
    },
    {
      id: 'hp_acme',
      name: 'Acme partner',
      description: '',
      allow_only: [],
      rules: [{ action: 'set', name: 'X-Partner-Key', secret: true, has_value: true }],
      assignments: [{ target_type: 'upstream', target_id: 'up-1', policy_id: 'hp_acme' }],
      updated_at: '2026-10-31T00:00:00Z',
    },
  ],
  targets: [
    { target_type: 'default', target_id: '', name: 'Default (all upstreams)', policy_id: 'hp_builtin_edge', effective_policy_id: 'hp_builtin_edge' },
    { target_type: 'upstream', target_id: 'up-1', name: 'OpenAI', detail: 'https://api.openai.com/v1', policy_id: 'hp_acme', effective_policy_id: 'hp_acme' },
    { target_type: 'upstream', target_id: 'up-2', name: 'Local vLLM', policy_id: '', effective_policy_id: 'hp_builtin_edge' },
    { target_type: 'personal_subscription', target_id: 'openai', name: 'OpenAI ChatGPT (Plus / Pro)', policy_id: '', effective_policy_id: 'hp_builtin_edge' },
  ],
  settings: { mode: 'values' },
  edge_headers: ['cf-*', 'x-forwarded-*', 'via'],
};

function wrap(node: ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter>{node}</MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  cleanup();
  vi.resetAllMocks();
  mocked.get.mockImplementation((path: string) => {
    if (path === '/api/v1/admin/header-policies') return Promise.resolve(fixture);
    return Promise.reject(new Error(`unexpected GET ${path}`));
  });
  mocked.put.mockResolvedValue({ ok: true });
  mocked.post.mockResolvedValue({ policy: {} });
});

describe('summarizePolicy', () => {
  it('describes rules and the allow-list', () => {
    expect(summarizePolicy({ allow_only: [], rules: [] })).toBe('no rules (passes headers through)');
    expect(summarizePolicy({ allow_only: ['content-type'], rules: [{ action: 'remove', name: 'a' }, { action: 'set', name: 'b' }] })).toBe(
      'allows only 1 header, removes 1, sets 1',
    );
  });
});

describe('HeaderPoliciesPanel', () => {
  it('shows the policy in effect for every target, including inherited ones', async () => {
    wrap(<HeaderPoliciesPanel />);
    const vllm = await screen.findByRole('row', { name: /Local vLLM/ });
    const cells = within(vllm).getAllByRole('cell');
    expect(cells[cells.length - 1]?.textContent).toContain('Strip edge & proxy headers');
    expect((within(vllm).getByRole('combobox') as HTMLSelectElement).value).toBe('');
    const personal = screen.getByRole('row', { name: /OpenAI ChatGPT/ });
    expect(within(personal).getByText('Personal subscription')).toBeTruthy();
    const openai = screen.getByRole('row', { name: /^OpenAI https/ });
    expect((within(openai).getByRole('combobox') as HTMLSelectElement).value).toBe('hp_acme');
  });

  it('assigns a policy to a target', async () => {
    const user = userEvent.setup();
    wrap(<HeaderPoliciesPanel />);
    await user.selectOptions(await screen.findByLabelText('Policy for Local vLLM'), 'hp_acme');
    await waitFor(() =>
      expect(mocked.put).toHaveBeenCalledWith('/api/v1/admin/header-policies/assignments', {
        target_type: 'upstream',
        target_id: 'up-2',
        policy_id: 'hp_acme',
      }),
    );
  });

  it('keeps a stored secret value when saving without retyping it', async () => {
    const user = userEvent.setup();
    wrap(<HeaderPoliciesPanel />);
    await user.click(await screen.findByRole('button', { name: 'Edit Acme partner' }));
    const value = screen.getByLabelText('Rule 1 value') as HTMLInputElement;
    expect(value.type).toBe('password');
    expect(value.value).toBe('');
    await user.click(screen.getByRole('button', { name: 'Save policy' }));
    await waitFor(() => expect(mocked.put).toHaveBeenCalled());
    const [path, body] = mocked.put.mock.calls[0] as [string, { rules: { keep_value?: boolean; secret?: boolean; value?: string }[] }];
    expect(path).toBe('/api/v1/admin/header-policies/hp_acme');
    expect(body.rules[0]).toMatchObject({ secret: true, keep_value: true, value: '' });
  });

  it('creates a policy with edge headers and a set rule', async () => {
    const user = userEvent.setup();
    wrap(<HeaderPoliciesPanel />);
    await user.click(await screen.findByRole('button', { name: 'New policy' }));
    await user.type(screen.getByLabelText('Name', { selector: 'input' }), 'Strict');
    await user.click(screen.getByRole('button', { name: /Add edge/ }));
    await user.click(screen.getByRole('button', { name: 'Add rule' }));
    await user.type(screen.getByLabelText('Rule 4 header'), 'X-Org');
    await user.type(screen.getByLabelText('Rule 4 value'), 'acme');
    await user.click(screen.getByRole('button', { name: 'Save policy' }));
    await waitFor(() => expect(mocked.post).toHaveBeenCalled());
    const [path, body] = mocked.post.mock.calls[0] as [string, { name: string; rules: { action: string; name: string; value?: string }[] }];
    expect(path).toBe('/api/v1/admin/header-policies');
    expect(body.name).toBe('Strict');
    expect(body.rules.map((r) => `${r.action}:${r.name}`)).toEqual(['remove:cf-*', 'remove:x-forwarded-*', 'remove:via', 'set:X-Org']);
    expect(body.rules[3]?.value).toBe('acme');
  });

  it('switches header logging', async () => {
    const user = userEvent.setup();
    wrap(<HeaderPoliciesPanel />);
    expect((await screen.findByRole('radio', { name: 'Names and values' })).getAttribute('aria-checked')).toBe('true');
    await user.click(screen.getByRole('radio', { name: 'Names only' }));
    await waitFor(() => expect(mocked.put).toHaveBeenCalledWith('/api/v1/admin/header-policies/settings', { mode: 'names' }));
  });
});

describe('RequestHeadersSection', () => {
  it('shows changes, caller and provider headers with masking', async () => {
    mocked.get.mockImplementation((path: string) =>
      path === '/api/v1/admin/requests/evt-1/headers'
        ? Promise.resolve({
            headers: {
              usage_id: 'evt-1',
              policy_id: 'hp_builtin_edge',
              policy_name: 'Strip edge & proxy headers',
              target: 'default',
              inbound: [
                { name: 'Cf-Connecting-Ip', values: ['203.0.113.9'] },
                { name: 'Cf-Access-Jwt-Assertion', values: ['•••• (220 chars)'], masked: true },
              ],
              outbound: [{ name: 'Authorization', values: ['•••• (60 chars)'], masked: true }],
              changes: [{ action: 'removed', name: 'Cf-Connecting-Ip', rule: 1 }],
            },
          })
        : Promise.reject(new Error(`unexpected GET ${path}`)),
    );
    const user = userEvent.setup();
    wrap(<RequestHeadersSection eventId="evt-1" />);
    expect(await screen.findByRole('link', { name: 'Strip edge & proxy headers' })).toBeTruthy();
    expect(screen.getByRole('cell', { name: 'Cf-Connecting-Ip' })).toBeTruthy();
    expect(screen.getByText('Removed')).toBeTruthy();
    await user.click(screen.getByRole('tab', { name: /From caller/ }));
    expect(screen.getByText('203.0.113.9')).toBeTruthy();
    expect(screen.getByText(/220 chars/)).toBeTruthy();
    await user.click(screen.getByRole('tab', { name: /To provider/ }));
    expect(screen.getByText('masked')).toBeTruthy();
  });

  it('explains a missing record', async () => {
    mocked.get.mockResolvedValue({ headers: null });
    wrap(<RequestHeadersSection eventId="evt-2" />);
    expect(await screen.findByText(/No header record/)).toBeTruthy();
  });
});
