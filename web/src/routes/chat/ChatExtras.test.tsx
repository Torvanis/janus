import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import type { ReactNode } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MadeFiles, MemoryChip, MemorySettings } from './ChatExtras';

function wrap(ui: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

afterEach(() => vi.restoreAllMocks());

describe('MadeFiles', () => {
  it('loads a file the model handed over and shows it as a file card', async () => {
    const game = '<!DOCTYPE html><html><head><title>Snake</title></head><body><canvas></canvas></body></html>';
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response(game, { status: 200 }));
    const { container } = render(
      <MadeFiles
        attachments={[
          { id: 'a1', kind: 'file', filename: 'snake.html', mime: 'text/html', size_bytes: game.length, est_tokens: 0 },
        ]}
      />,
    );
    await waitFor(() => expect(screen.getByText('snake.html')).toBeTruthy());
    expect(fetch).toHaveBeenCalledWith('/api/v1/chat/attachments/a1/content', { credentials: 'same-origin' });
    fireEvent.click(await screen.findByRole('button', { name: 'Preview' }));
    const frame = container.querySelector('iframe');
    expect(frame?.getAttribute('sandbox')).not.toContain('allow-same-origin');
    expect(frame?.getAttribute('srcdoc')).toContain('<canvas>');
  });

  it('shows a non-HTML file without a preview, and plays video and music', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response('a,b\n1,2\n', { status: 200 }));
    const { container } = render(
      <MadeFiles
        attachments={[
          { id: 'c', kind: 'file', filename: 'data.csv', mime: 'text/csv', size_bytes: 8, est_tokens: 0 },
          { id: 'v', kind: 'video', filename: 'video-1.mp4', mime: 'video/mp4', size_bytes: 9, est_tokens: 0 },
          { id: 'm', kind: 'audio', filename: 'music-1.mp3', mime: 'audio/mpeg', size_bytes: 9, est_tokens: 0 },
          { id: 'i', kind: 'image', filename: 'image-1.png', mime: 'image/png', size_bytes: 9, est_tokens: 0 },
        ]}
      />,
    );
    await waitFor(() => expect(screen.getByText('data.csv')).toBeTruthy());
    expect(screen.queryByRole('button', { name: 'Preview' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Download' })).toBeTruthy();
    expect(container.querySelector('video')?.getAttribute('src')).toBe('/api/v1/chat/attachments/v/content');
    expect(container.querySelector('audio')?.getAttribute('src')).toBe('/api/v1/chat/attachments/m/content');
    expect(container.querySelectorAll('img').length).toBe(0); // images have their own view
  });
});

describe('MemoryChip', () => {
  it('says what was saved or forgotten', () => {
    const open = vi.fn();
    render(
      <MemoryChip
        activity={[
          { type: 'remember', status: 'done', query: 'Prefers metric units' },
          { type: 'forget', status: 'done', query: 'Lives in Ohio' },
        ]}
        onOpen={open}
      />,
    );
    expect(screen.getByText('Memory updated')).toBeTruthy();
    expect(screen.getByText(/Prefers metric units/)).toBeTruthy();
    expect(screen.getByText(/Lives in Ohio/)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Manage memory' }));
    expect(open).toHaveBeenCalled();
  });
  it('renders nothing for a reply that did not touch memory', () => {
    const { container } = render(<MemoryChip activity={[{ type: 'web_search', status: 'done' }]} onOpen={() => undefined} />);
    expect(container.innerHTML).toBe('');
  });
});

describe('MemorySettings', () => {
  it('lists, adds, deletes and switches memory off', async () => {
    let doc: {
      enabled: boolean;
      available: boolean;
      limit: number;
      memories: { id: string; content: string; from_conversation_title?: string; created_at: string }[];
    } = {
      enabled: true,
      available: true,
      limit: 200,
      memories: [{ id: 'm1', content: 'Prefers metric units', from_conversation_title: 'Trip plan', created_at: '' }],
    };
    const calls: string[] = [];
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (input, init) => {
      const url = String(input);
      const method = init?.method ?? 'GET';
      calls.push(`${method} ${url}`);
      if (method === 'POST') doc = { ...doc, memories: [...doc.memories, { id: 'm2', content: 'Has a dog', created_at: '' }] };
      if (method === 'DELETE') doc = { ...doc, memories: doc.memories.filter((m) => !url.endsWith(m.id)) };
      if (method === 'PUT') doc = { ...doc, enabled: false };
      return new Response(JSON.stringify(doc), { status: 200, headers: { 'Content-Type': 'application/json' } });
    });
    wrap(<MemorySettings />);
    expect(await screen.findByText('Prefers metric units')).toBeTruthy();
    expect(screen.getByText('from “Trip plan”')).toBeTruthy();
    fireEvent.change(screen.getByLabelText('Add a memory'), { target: { value: 'Has a dog' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add' }));
    expect(await screen.findByText('Has a dog')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Delete “Prefers metric units”' }));
    await waitFor(() => expect(screen.queryByText('Prefers metric units')).toBeNull());
    fireEvent.click(screen.getByRole('switch', { name: 'Memory' }));
    await waitFor(() => expect((screen.getByRole('switch', { name: 'Memory' }) as HTMLInputElement).checked).toBe(false));
    expect(calls).toContain('DELETE /api/v1/chat/memory/m1');
    expect(calls).toContain('PUT /api/v1/chat/memory');
  });
});
