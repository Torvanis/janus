import { afterEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { SessionContext } from '../../app/session';
import type { Me } from '../../lib/types';
import ChatPage from './ChatPage';
import { ATTACH_ACCEPT, MessageAttachments } from './Attachments';
import { uploadChatAttachment, type ChatAttachment } from './upload';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return { ...actual, api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() } };
});
vi.mock('./upload', () => ({ uploadChatAttachment: vi.fn() }));

const mocked = vi.mocked(api);
const upload = vi.mocked(uploadChatAttachment);

const docAttachment: ChatAttachment = {
  id: 'a1',
  kind: 'document',
  filename: 'permit.pdf',
  mime: 'application/pdf',
  size_bytes: 2048,
  est_tokens: 1234,
};

function mockGets(conversation?: unknown) {
  mocked.get.mockImplementation(async (path: string) => {
    if (path === '/api/v1/models') {
      return {
        models: [{ id: 'm1', name: 'gpt-4o', display_name: '', status: 'enabled', modalities: ['chat'], context_window: 1500 }],
        managed_models: [],
      } as never;
    }
    if (path === '/api/v1/chat/conversations/c1' && conversation) return conversation as never;
    if (path.startsWith('/api/v1/chat/conversations')) return { conversations: [] } as never;
    if (path.startsWith('/api/v1/chat/search')) return { results: [] } as never;
    throw new Error(`unexpected GET ${path}`);
  });
}

function renderPage(path = '/chat') {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const me = { id: 'u1', active_team_id: '', teams: [], feature_flags: {}, chat_preview: true } as unknown as Me;
  return render(
    <QueryClientProvider client={qc}>
      <SessionContext.Provider value={{ me, config: null, loading: false, error: null, refresh: () => {} }}>
        <MemoryRouter initialEntries={[path]}>
          <Routes>
            <Route path="/chat" element={<ChatPage />} />
            <Route path="/chat/:id" element={<ChatPage />} />
          </Routes>
        </MemoryRouter>
      </SessionContext.Provider>
    </QueryClientProvider>,
  );
}

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

function pickFiles(files: File[]) {
  const input = screen.getByLabelText('Choose files') as HTMLInputElement;
  fireEvent.change(input, { target: { files } });
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe('ChatPage attachments', () => {
  it('shows a chip with size and token estimate, disables Send while uploading, and removes it', async () => {
    mockGets();
    const pending = deferred<ChatAttachment>();
    upload.mockReturnValueOnce(pending.promise);
    mocked.del.mockResolvedValue(undefined as never);
    renderPage();
    await waitFor(() => expect(screen.getByRole('option', { name: 'gpt-4o' })).toBeTruthy());
    expect(screen.getByRole('button', { name: 'Attach files' })).toBeTruthy();

    pickFiles([new File(['%PDF-1.4'], 'permit.pdf', { type: 'application/pdf' })]);
    const files = screen.getByLabelText('Attached files');
    expect(files.textContent).toContain('permit.pdf');
    expect(files.textContent).toContain('uploading');
    fireEvent.change(screen.getByLabelText('Message'), { target: { value: 'Summarize' } });
    expect((screen.getByRole('button', { name: 'Send' }) as HTMLButtonElement).disabled).toBe(true);

    await act(async () => pending.resolve(docAttachment));
    await waitFor(() => expect(screen.getByLabelText('Attached files').textContent).toContain('~1,234 tokens'));
    expect((screen.getByRole('button', { name: 'Send' }) as HTMLButtonElement).disabled).toBe(false);
    // 1,234 tokens of a 1,500-token window is over 80%: warn.
    expect(screen.getByLabelText('Attached files').textContent).toMatch(/About 1,234 tokens of files.*over 80%/);

    fireEvent.click(screen.getByRole('button', { name: 'Remove permit.pdf' }));
    expect(screen.queryByLabelText('Attached files')).toBeNull();
    expect(mocked.del).toHaveBeenCalledWith('/api/v1/chat/attachments/a1');
  });

  it('shows the reason when an upload is refused and blocks sending until removed', async () => {
    mockGets();
    const { ApiError } = await import('../../lib/api');
    upload.mockRejectedValueOnce(
      new ApiError(400, { message: 'Programs and archives cannot be attached.', code: 'invalid_request', type: 'x' }),
    );
    renderPage();
    await waitFor(() => expect(screen.getByRole('option', { name: 'gpt-4o' })).toBeTruthy());
    pickFiles([new File(['MZ'], 'notes.txt', { type: 'text/plain' })]);
    await waitFor(() => expect(screen.getByRole('alert').textContent).toContain('Programs and archives cannot be attached.'));
    fireEvent.change(screen.getByLabelText('Message'), { target: { value: 'hi' } });
    expect((screen.getByRole('button', { name: 'Send' }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(screen.getByRole('button', { name: 'Remove notes.txt' }));
    expect((screen.getByRole('button', { name: 'Send' }) as HTMLButtonElement).disabled).toBe(false);
  });

  it('turns a pasted image into a chip with a thumbnail and sends its id', async () => {
    mockGets();
    const created = vi.fn(() => 'blob:thumb');
    Object.assign(URL, { createObjectURL: created, revokeObjectURL: vi.fn() });
    upload.mockResolvedValueOnce({
      id: 'img1',
      kind: 'image',
      filename: 'pasted-image.png',
      mime: 'image/png',
      size_bytes: 10,
      est_tokens: 1000,
    });
    mocked.post.mockImplementation(async (path: string) => {
      if (path === '/api/v1/chat/conversations') return { id: 'c1', title: '', model: 'gpt-4o', current_leaf_id: '' } as never;
      return {
        run_id: 'r1',
        user_message: { id: 'u1', parent_id: '', role: 'user', content: '', status: 'complete' },
        assistant_message: {
          id: 'a1',
          parent_id: 'u1',
          role: 'assistant',
          content: '',
          status: 'complete',
          tokens_in: 0,
          tokens_out: 0,
          tokens_cached: 0,
          cost_usd: 0,
          tokens_out_per_second: 0,
          latency_ms: 0,
          ttfb_ms: 0,
        },
        conversation: { id: 'c1', title: 'x', model: 'gpt-4o', current_leaf_id: 'a1' },
        warnings: [],
      } as never;
    });
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => new Response('', { status: 200 })),
    );
    renderPage();
    await waitFor(() => expect(screen.getByRole('option', { name: 'gpt-4o' })).toBeTruthy());

    const image = new File([new Uint8Array([137, 80, 78, 71])], '', { type: 'image/png' });
    fireEvent.paste(screen.getByLabelText('Message'), { clipboardData: { files: [image] } });
    await waitFor(() => expect(screen.getByLabelText('Attached files').textContent).toContain('pasted-image.png'));
    const thumb = screen.getByLabelText('Attached files').querySelector('img.chat-chip-thumb');
    expect(thumb?.getAttribute('src')).toBe('blob:thumb');
    expect(upload.mock.calls[0]![0].name).toBe('pasted-image.png');

    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() =>
      expect(mocked.post).toHaveBeenCalledWith(
        '/api/v1/chat/conversations/c1/messages',
        expect.objectContaining({ attachment_ids: ['img1'] }),
      ),
    );
    await waitFor(() => expect(screen.queryByLabelText('Attached files')).toBeNull());
    vi.unstubAllGlobals();
  });

  it('shows a sent message’s image thumbnail and document chip in the thread', async () => {
    mockGets({
      conversation: { id: 'c1', title: 'Trip', model: 'gpt-4o', current_leaf_id: 'u1' },
      messages: [
        {
          id: 'u1',
          parent_id: '',
          role: 'user',
          content: 'Look',
          status: 'complete',
          attachments: [
            { id: 'img1', kind: 'image', filename: 'lake.png', mime: 'image/png', size_bytes: 10, est_tokens: 1000 },
            docAttachment,
          ],
        },
      ],
      runs: {},
    });
    renderPage('/chat/c1');
    const files = await screen.findByLabelText('Files');
    const img = files.querySelector('img');
    expect(img?.getAttribute('src')).toBe('/api/v1/chat/attachments/img1/content');
    expect(img?.getAttribute('alt')).toBe('lake.png');
    expect(img?.closest('a')?.getAttribute('target')).toBe('_blank');
    expect(files.textContent).toContain('permit.pdf');
    expect(files.textContent).toContain('2 KB');
  });
});

describe('sound and video on a sent message', () => {
  it('shows players for attached sound and video', () => {
    render(
      <MessageAttachments
        attachments={[
          { id: 's', kind: 'audio', filename: 'memo.mp3', mime: 'audio/mpeg', size_bytes: 9, est_tokens: 200 },
          { id: 'v', kind: 'video', filename: 'clip.mp4', mime: 'video/mp4', size_bytes: 9, est_tokens: 500 },
        ]}
      />,
    );
    expect(screen.getByLabelText('memo.mp3').tagName).toBe('AUDIO');
    expect(screen.getByLabelText('clip.mp4').tagName).toBe('VIDEO');
    expect(screen.getByLabelText('clip.mp4').getAttribute('src')).toBe('/api/v1/chat/attachments/v/content');
  });
  it('the picker offers sound and video', () => {
    expect(ATTACH_ACCEPT).toContain('.mp3');
    expect(ATTACH_ACCEPT).toContain('.mp4');
    expect(ATTACH_ACCEPT).toContain('.webm');
  });
});
