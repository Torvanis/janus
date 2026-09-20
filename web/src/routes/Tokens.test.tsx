/**
 * Route-level behavior tests for the Tokens page, driven through the real
 * component tree (React Query + router + toasts) with only the HTTP layer
 * mocked. These exist because the page carries the highest-consequence user
 * flows in the product: token creation with a reveal-once value, and
 * (bulk) revocation.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../lib/api';
import type { ApiToken } from '../lib/types';
import { ToastProvider } from '../components/ui';
import { TokensPage } from './Tokens';

vi.mock('../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/api')>();
  return {
    ...actual,
    api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() },
  };
});

const mocked = vi.mocked(api);

function token(overrides: Partial<ApiToken> & Pick<ApiToken, 'id' | 'description'>): ApiToken {
  return {
    user_id: 'user-1',
    prefix: 'jns_' + overrides.id,
    created_at: '2025-01-02T10:00:00Z',
    last_used_at: '',
    revoked_at: '', // '' = active, matching the gateway's Token.MarshalJSON output
    ...overrides,
  };
}

const cursor = token({ id: 'tok-a', description: 'Cursor' });
const zed = token({ id: 'tok-b', description: 'Zed' });

function renderTokensPage(initialEntry = '/tokens') {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter initialEntries={[initialEntry]}>
          <Routes>
            <Route path="/tokens" element={<TokensPage />} />
            <Route path="/tokens/:id" element={<TokensPage />} />
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  cleanup();
  vi.clearAllMocks();
  mocked.get.mockResolvedValue({ tokens: [cursor, zed] });
});

describe('create and reveal-once', () => {
  it('creates a token, reveals the value exactly once, then removes it from the DOM', async () => {
    mocked.post.mockResolvedValue({
      token: token({ id: 'tok-new', description: 'New laptop' }),
      value: 'janus_secret_value_123',
    });
    const user = userEvent.setup();
    renderTokensPage();

    await user.click(await screen.findByRole('button', { name: 'Create token' }));
    const dialog = screen.getByRole('dialog');
    await user.type(within(dialog).getByPlaceholderText('Cursor on my laptop'), 'New laptop');
    await user.click(within(dialog).getByRole('button', { name: 'Create token' }));

    expect(mocked.post).toHaveBeenCalledWith('/api/v1/tokens', { description: 'New laptop' });

    // The reveal modal warns this is the only time the value is shown…
    expect(await screen.findByText('This is the only time the value is shown.')).toBeTruthy();
    expect(screen.getByText('janus_secret_value_123')).toBeTruthy();

    // …and closing it removes the secret from the DOM for good.
    await user.click(screen.getByRole('button', { name: 'I’ve saved it' }));
    expect(screen.queryByText('janus_secret_value_123')).toBeNull();
    expect(screen.queryByText('This is the only time the value is shown.')).toBeNull();
  });

  it('rejects an empty description client-side without calling the API', async () => {
    const user = userEvent.setup();
    renderTokensPage();

    await user.click(await screen.findByRole('button', { name: 'Create token' }));
    const dialog = screen.getByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: 'Create token' }));

    expect(await within(dialog).findByRole('alert')).toBeTruthy();
    expect(within(dialog).getByText('Give the token a description so you can recognise it later.')).toBeTruthy();
    expect(mocked.post).not.toHaveBeenCalled();
  });
});

describe('single revoke', () => {
  it('revokes after explicit confirmation and toasts the consequence', async () => {
    mocked.del.mockResolvedValue(undefined);
    const user = userEvent.setup();
    renderTokensPage();

    const row = (await screen.findByText('Cursor')).closest('tr')!;
    await user.click(within(row).getByRole('button', { name: 'Revoke' }));

    // Nothing happens until the dialog is confirmed.
    expect(screen.getByText('Revoke “Cursor”?')).toBeTruthy();
    expect(mocked.del).not.toHaveBeenCalled();

    await user.click(screen.getByRole('button', { name: 'Revoke token' }));
    await waitFor(() => expect(mocked.del).toHaveBeenCalledWith('/api/v1/tokens/tok-a'));
    expect(await screen.findByText('Token revoked. Any tool using it will now be refused.')).toBeTruthy();
  });
});

describe('bulk revoke', () => {
  it('revokes every selected token and clears the selection on full success', async () => {
    mocked.del.mockResolvedValue(undefined);
    const user = userEvent.setup();
    renderTokensPage();

    await user.click(await screen.findByRole('checkbox', { name: 'Select Cursor' }));
    await user.click(screen.getByRole('checkbox', { name: 'Select Zed' }));
    await user.click(screen.getByRole('button', { name: 'Revoke 2 selected' }));
    await user.click(screen.getByRole('button', { name: 'Revoke all selected' }));

    await waitFor(() => expect(mocked.del).toHaveBeenCalledTimes(2));
    expect(mocked.del).toHaveBeenCalledWith('/api/v1/tokens/tok-a');
    expect(mocked.del).toHaveBeenCalledWith('/api/v1/tokens/tok-b');

    // Selection is fully cleared, so the bulk action bar disappears.
    await waitFor(() => expect(screen.queryByRole('button', { name: /selected/ })).toBeNull());
    const boxes = screen.getAllByRole('checkbox') as HTMLInputElement[];
    expect(boxes.every((box) => !box.checked)).toBe(true);
    // No failure summary on the happy path.
    expect(screen.queryByText(/failed/)).toBeNull();
  });

  it('keeps failed tokens selected and shows a summary toast on partial failure', async () => {
    mocked.del.mockImplementation((path: string) =>
      path.endsWith(cursor.id) ? Promise.resolve(undefined) : Promise.reject(new Error('boom: gateway refused')),
    );
    const user = userEvent.setup();
    renderTokensPage();

    await user.click(await screen.findByRole('checkbox', { name: 'Select Cursor' }));
    await user.click(screen.getByRole('checkbox', { name: 'Select Zed' }));
    await user.click(screen.getByRole('button', { name: 'Revoke 2 selected' }));
    await user.click(screen.getByRole('button', { name: 'Revoke all selected' }));

    // Aggregate summary names both counts.
    expect(await screen.findByText('1 revoked, 1 failed. The failed tokens stay selected — try again.')).toBeTruthy();

    // The failed token stays selected so a retry is one click away…
    const zedBox = screen.getByRole('checkbox', { name: 'Select Zed' }) as HTMLInputElement;
    expect(zedBox.checked).toBe(true);
    // …while the successfully revoked one leaves the selection.
    const cursorBox = screen.getByRole('checkbox', { name: 'Select Cursor' }) as HTMLInputElement;
    expect(cursorBox.checked).toBe(false);
    // The bulk action bar now offers exactly the failed remainder.
    expect(screen.getByRole('button', { name: 'Revoke 1 selected' })).toBeTruthy();
  });
});

describe('filtering resets pagination', () => {
  it('applying a filter from page 2 returns to page 1 instead of a stranded empty table', async () => {
    const many = Array.from({ length: 30 }, (_, i) =>
      token({
        id: `tok-${String(i).padStart(2, '0')}`,
        description: `Token ${String(i).padStart(2, '0')}`,
        created_at: `2025-01-${String((i % 27) + 1).padStart(2, '0')}T10:00:00Z`,
        revoked_at: i < 3 ? '2025-02-01T10:00:00Z' : '',
      }),
    );
    mocked.get.mockResolvedValue({ tokens: many });
    const user = userEvent.setup();
    renderTokensPage('/tokens?page=1');

    // Page 2 of the unfiltered list is on screen.
    await screen.findAllByText(/Token \d\d/);

    // Narrow to the 3 revoked tokens while standing on page 2. Regression:
    // without a page reset the stale offset (25) exceeded the filtered total
    // (3), the table body rendered zero rows, and the pagination control hid
    // itself (total <= page size) — a header-only table with no way back.
    await user.selectOptions(screen.getByLabelText('Status'), 'revoked');

    expect(await screen.findByText('Token 00')).toBeTruthy();
    expect(screen.getByText('Token 01')).toBeTruthy();
    expect(screen.getByText('Token 02')).toBeTruthy();
  });
});

describe('revocation status badges and filtering', () => {
  const laptop = token({ id: 'tok-act', description: 'Laptop' });
  const oldCi = token({ id: 'tok-rev', description: 'Old CI', revoked_at: '2025-02-01T10:00:00Z' });

  it('badges active tokens as Active and revoked tokens as Revoked', async () => {
    mocked.get.mockResolvedValue({ tokens: [laptop, oldCi] });
    renderTokensPage();

    const activeRow = (await screen.findByText('Laptop')).closest('tr')!;
    expect(within(activeRow).getByText('Active')).toBeTruthy();
    expect(within(activeRow).queryByText('Revoked')).toBeNull();
    // Active tokens stay actionable: revocable and bulk-selectable.
    expect(within(activeRow).getByRole('button', { name: 'Revoke' })).toBeTruthy();
    expect((within(activeRow).getByRole('checkbox') as HTMLInputElement).disabled).toBe(false);

    const revokedRow = screen.getByText('Old CI').closest('tr')!;
    expect(within(revokedRow).getByText('Revoked')).toBeTruthy();
    expect(within(revokedRow).queryByText('Active')).toBeNull();
    expect(within(revokedRow).queryByRole('button', { name: 'Revoke' })).toBeNull();
    expect((within(revokedRow).getByRole('checkbox') as HTMLInputElement).disabled).toBe(true);
  });

  it('treats the Go zero time as never revoked (regression: every active token badged as revoked)', async () => {
    // Older gateway builds serialized time.Time's zero value instead of "" —
    // a truthy string that flipped every active token to the Revoked badge.
    const legacy = token({ id: 'tok-legacy', description: 'Legacy', revoked_at: '0001-01-01T00:00:00Z' });
    mocked.get.mockResolvedValue({ tokens: [legacy] });
    const user = userEvent.setup();
    renderTokensPage();

    const row = (await screen.findByText('Legacy')).closest('tr')!;
    expect(within(row).getByText('Active')).toBeTruthy();
    expect(within(row).queryByText('Revoked')).toBeNull();

    // …and the status filter groups it with the active tokens, not the revoked ones.
    await user.selectOptions(screen.getByLabelText('Status'), 'revoked');
    await waitFor(() => expect(screen.queryByText('Legacy')).toBeNull());
    await user.selectOptions(screen.getByLabelText('Status'), 'active');
    expect(await screen.findByText('Legacy')).toBeTruthy();
  });

  it('status filter separates active tokens from revoked ones', async () => {
    mocked.get.mockResolvedValue({ tokens: [laptop, oldCi] });
    const user = userEvent.setup();
    renderTokensPage();

    await screen.findByText('Laptop');
    await user.selectOptions(screen.getByLabelText('Status'), 'active');
    expect(screen.getByText('Laptop')).toBeTruthy();
    await waitFor(() => expect(screen.queryByText('Old CI')).toBeNull());

    await user.selectOptions(screen.getByLabelText('Status'), 'revoked');
    expect(await screen.findByText('Old CI')).toBeTruthy();
    expect(screen.queryByText('Laptop')).toBeNull();
  });

  it('shows the Active badge in the detail drawer for an unrevoked token', async () => {
    const totals = { tokens_in: 0, tokens_out: 0, tokens_cached: 0, cost_nanousd: 0, request_count: 0, error_count: 0 };
    mocked.get.mockImplementation((path: string) =>
      path === '/api/v1/tokens'
        ? Promise.resolve({ tokens: [laptop] })
        : Promise.resolve({ token: laptop, totals, per_model: [], recent_requests: [] }),
    );
    renderTokensPage('/tokens/tok-act');

    const dialog = await screen.findByRole('dialog');
    expect(await within(dialog).findByText('Active')).toBeTruthy();
    expect(within(dialog).queryByText('Revoked')).toBeNull();
    expect(within(dialog).getByRole('link', { name: "Open this token's request log" }).getAttribute('href')).toBe(
      '/requests?token_id=tok-act&range=day',
    );
    expect(within(dialog).getByText(/up to 10/)).toBeTruthy();
  });
});
