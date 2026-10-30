import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../lib/api';
import { Breadcrumb } from './Breadcrumb';

vi.mock('../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/api')>();
  return { ...actual, api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() } };
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe('chat breadcrumb', () => {
  it('names the conversation instead of showing its id', async () => {
    vi.mocked(api.get).mockResolvedValue({
      conversations: [{ id: '62123fb3-4d42-4e3d-af6c-c793415c96b4', title: 'Casual Greeting Exchange' }],
    } as never);
    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <Breadcrumb path="/chat/62123fb3-4d42-4e3d-af6c-c793415c96b4" />
      </QueryClientProvider>,
    );
    expect(await screen.findByText('Casual Greeting Exchange')).toBeTruthy();
    const crumb = screen.getByRole('navigation');
    expect(crumb.textContent).toContain('Chat');
    expect(crumb.textContent).not.toMatch(/62123fb3/);
  });
});
