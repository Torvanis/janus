/**
 * Janus Chat sidebar search and the context-trimming divider.
 *
 * Search is whole-word only (the server keeps a keyed blind index over the
 * encrypted chats, so substring search is not possible by design).
 */
import { useState, type ReactNode } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Link } from 'react-router-dom';
import { api } from '../../lib/api';
import { useDebounced } from '../../lib/hooks';

export interface ChatSearchResult {
  conversation_id: string;
  title: string;
  updated_at: string;
  snippet: string;
}

/** Minimal message shape the divider needs (a subset of ChatMessage). */
export interface ContextMessage {
  id: string;
  role: 'user' | 'assistant';
  content: string;
  status: string;
  context_dropped?: number;
}

/**
 * The message before which "N earlier messages were not sent" goes: on the
 * latest assistant reply that trimmed its context, skip the N oldest
 * messages that were context (failed or empty replies never are) and return
 * the first one that was sent. null when nothing was trimmed.
 */
export function contextDivider(branch: ContextMessage[]): { beforeId: string; count: number } | null {
  const latest = [...branch].reverse().find((m) => m.role === 'assistant' && m.status !== 'streaming');
  const dropped = latest?.context_dropped ?? 0;
  if (!latest || dropped <= 0) return null;
  let skipped = 0;
  for (const m of branch) {
    if (m.id === latest.id) break;
    const isContext = m.role === 'user' || ((m.status === 'complete' || m.status === 'stopped') && m.content !== '');
    if (!isContext) continue;
    if (skipped === dropped) return { beforeId: m.id, count: dropped };
    skipped++;
  }
  return null;
}

export function ContextDivider({ count }: { count: number }): ReactNode {
  return (
    <div className="chat-context-divider small muted" role="separator">
      {count === 1 ? '1 earlier message was not sent' : `${count} earlier messages were not sent`} (context window)
    </div>
  );
}

/** Sidebar search: debounced 250 ms, at least 2 characters. */
export function ChatSearch({ activeId, onOpen }: { activeId?: string; onOpen: () => void }): ReactNode {
  const [q, setQ] = useState('');
  const query = useDebounced(q.trim(), 250);
  const enabled = query.length >= 2;
  const results = useQuery({
    queryKey: ['chat', 'search', query],
    queryFn: () => api.get<{ results: ChatSearchResult[] }>(`/api/v1/chat/search?q=${encodeURIComponent(query)}`),
    enabled,
    staleTime: 10_000,
  });
  return (
    <div className="chat-search">
      <div className="chat-search-box">
        <input
          className="input"
          type="search"
          aria-label="Search chats"
          placeholder="Search chats"
          value={q}
          onChange={(e) => setQ(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Escape') setQ('');
          }}
        />
        {q ? (
          <button type="button" className="chat-search-clear" aria-label="Clear search" onClick={() => setQ('')}>
            ✕
          </button>
        ) : null}
      </div>
      {enabled ? (
        <ul className="chat-list chat-search-results" aria-label="Search results">
          {results.isError ? <li className="small muted chat-search-note">Search failed.</li> : null}
          {results.data && results.data.results.length === 0 ? (
            <li className="small muted chat-search-note">No chats contain all of those words.</li>
          ) : null}
          {(results.data?.results ?? []).map((r) => (
            <li
              key={r.conversation_id}
              className={
                r.conversation_id === activeId ? 'chat-list-item chat-search-hit active' : 'chat-list-item chat-search-hit'
              }
            >
              <Link to={`/chat/${r.conversation_id}`} onClick={onOpen}>
                <span className="chat-search-title">{r.title || 'New chat'}</span>
                {r.snippet ? <span className="chat-search-snippet small muted">{r.snippet}</span> : null}
              </Link>
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}
