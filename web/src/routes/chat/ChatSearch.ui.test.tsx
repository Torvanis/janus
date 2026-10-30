import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { ChatSearch } from './ChatSearch';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return { ...actual, api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() } };
});

const mocked = vi.mocked(api);

function renderSearch() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <ChatSearch onOpen={() => undefined} />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe('ChatSearch', () => {
  it('searches after 2 characters and shows title, snippet and link', async () => {
    mocked.get.mockResolvedValue({
      results: [{ conversation_id: 'c1', title: 'Orchid care', updated_at: '', snippet: '…repot an orchid without…' }],
    });
    renderSearch();
    const box = screen.getByLabelText('Search chats');
    await userEvent.type(box, 'o');
    await new Promise((r) => setTimeout(r, 300));
    expect(mocked.get).not.toHaveBeenCalled();
    await userEvent.type(box, 'rchid');
    const link = await screen.findByRole('link', { name: /Orchid care/ });
    expect(link.getAttribute('href')).toBe('/chat/c1');
    expect(screen.getByText('…repot an orchid without…')).toBeTruthy();
    expect(mocked.get).toHaveBeenLastCalledWith('/api/v1/chat/search?q=orchid');

    await userEvent.click(screen.getByRole('button', { name: 'Clear search' }));
    expect((box as HTMLInputElement).value).toBe('');
    await waitFor(() => expect(screen.queryByRole('link', { name: /Orchid care/ })).toBeNull());
  });

  it('says so when nothing matches', async () => {
    mocked.get.mockResolvedValue({ results: [] });
    renderSearch();
    await userEvent.type(screen.getByLabelText('Search chats'), 'volcano');
    expect(await screen.findByText('No chats contain all of those words.')).toBeTruthy();
  });
});
