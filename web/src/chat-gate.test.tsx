import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { App } from './App';
import type { ReactNode } from 'react';
import { api } from './lib/api';

// Chat pages key on Administration → Chat's switch (me.chat_enabled) and on
// whether this person may use Chat (me.chat_available); there is no startup
// flag any more.
const session = vi.hoisted(() => ({ role: 'user', enabled: true, available: true, reason: '' }));
vi.mock('./app/session', () => ({
  SessionProvider: ({ children }: { children: ReactNode }) => children,
  useSession: () => ({
    me: {
      role: session.role,
      groups: [],
      teams: [],
      email: 'viewer@example.com',
      chat_enabled: session.enabled,
      chat_available: session.available,
      chat_unavailable_reason: session.reason,
    },
    loading: false,
  }),
}));
vi.mock('./app/Shell', () => ({ Shell: ({ children }: { children: ReactNode }) => children }));
vi.mock('./lib/api', async (original) => ({
  ...(await original<typeof import('./lib/api')>()),
  api: { get: vi.fn(), post: vi.fn() },
}));
vi.mock('./routes/chat/ChatPage', () => ({ default: () => <h1>Chat page</h1> }));
vi.mock('./routes/chat/SharedChatPage', () => ({ default: () => <h1>Shared chat page</h1> }));
afterEach(cleanup);
beforeEach(() => {
  session.role = 'user';
  session.enabled = true;
  session.available = true;
  session.reason = '';
  vi.clearAllMocks();
});
function mount(path: string) {
  render(
    <QueryClientProvider client={new QueryClient()}>
      <MemoryRouter initialEntries={[path]}>
        <App />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

it.each(['/chat', '/chat/c1', '/chat/shared/s1'])(
  'does not render %s or call the chat API when an admin turned Chat off',
  (path) => {
    session.enabled = false;
    session.available = false;
    session.reason = 'Janus Chat is turned off for this instance.';
    mount(path);
    expect(screen.getByRole('heading', { name: 'Chat is turned off' })).toBeTruthy();
    expect(screen.queryByRole('heading', { name: /chat page/i })).toBeNull();
    expect(screen.queryByRole('link', { name: /Administration → Chat/ })).toBeNull();
    expect(api.get).not.toHaveBeenCalled();
  },
);

it('links an administrator to Administration → Chat', () => {
  session.role = 'admin';
  session.enabled = false;
  session.available = false;
  mount('/chat');
  expect(screen.getByRole('heading', { name: 'Chat is turned off' })).toBeTruthy();
  expect(screen.getByRole('link', { name: 'Open Administration → Chat' }).getAttribute('href')).toBe('/admin/chat');
  expect(screen.queryByText(/JANUS_CHAT_PREVIEW/)).toBeNull();
  expect(api.get).not.toHaveBeenCalled();
});

it('explains an access restriction without calling the chat API, but still opens shared chats', async () => {
  session.available = false;
  session.reason = 'Ask IT for Chat access.';
  mount('/chat');
  expect(screen.getByRole('heading', { name: 'Chat is not available to you' })).toBeTruthy();
  expect(screen.getByText('Ask IT for Chat access.')).toBeTruthy();
  expect(api.get).not.toHaveBeenCalled();
  cleanup();
  mount('/chat/shared/s1');
  expect(await screen.findByRole('heading', { name: 'Shared chat page' })).toBeTruthy();
});

it.each(['/chat', '/chat/c1'])('renders %s when Chat is on, with no startup flag', async (path) => {
  mount(path);
  expect(await screen.findByRole('heading', { name: 'Chat page' })).toBeTruthy();
});
