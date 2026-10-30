/**
 * A shared Janus Chat: the read-only snapshot behind /chat/shared/:share.
 * Anyone signed in to this Janus can open it; the owner can revoke it from
 * the chat's Share panel.
 */
import type { ReactNode } from 'react';
import { Link, useParams } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { api, ApiError } from '../../lib/api';
import { ChatMarkdown } from './Markdown';
import './chat.css';

interface SharedMessage {
  role: 'user' | 'assistant';
  content: string;
  reasoning?: string;
  model?: string;
  status?: string;
  attachments?: string[];
}

interface SharedChat {
  title: string;
  shared_by: string;
  shared_at: string;
  messages: SharedMessage[];
  own: boolean;
}

export default function SharedChatPage(): ReactNode {
  const { share } = useParams();
  const q = useQuery({
    queryKey: ['chat', 'shared', share],
    queryFn: () => api.get<SharedChat>(`/api/v1/chat/shared/${share}`),
    retry: false,
  });
  if (q.isLoading) return <div className="chat-shared muted">Loading…</div>;
  if (q.error || !q.data) {
    const gone = q.error instanceof ApiError && q.error.status === 404;
    return (
      <div className="chat-shared">
        <div className="chat-shared-missing">
          <h1>{gone ? 'This link no longer works' : 'This shared chat could not be opened'}</h1>
          <p className="muted">
            {gone ? 'Its owner may have stopped sharing it, or deleted the chat.' : 'Try again in a moment.'}
          </p>
          <Link className="btn" to="/chat">
            Go to Chat
          </Link>
        </div>
      </div>
    );
  }
  const d = q.data;
  return (
    <div className="chat-shared">
      <header className="chat-shared-head">
        <p className="chat-shared-kicker">Shared chat · read only</p>
        <h1>{d.title}</h1>
        <p className="muted small">
          {d.shared_by ? `Shared by ${d.shared_by} · ` : ''}
          {new Date(d.shared_at).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })}
          {d.own ? ' · you shared this; manage the link from the chat’s Share button' : ''}
        </p>
      </header>
      <div className="chat-column">
        {d.messages.map((m, i) => (
          <article key={i} className={`chat-msg chat-msg-${m.role}`}>
            {m.attachments && m.attachments.length > 0 ? (
              <ul className="chat-msg-files" aria-label="Files">
                {m.attachments.map((a) => (
                  <li key={a} className="chat-chip chat-chip-ready">
                    <span className="chat-chip-text">
                      <span className="chat-chip-name">{a}</span>
                    </span>
                  </li>
                ))}
              </ul>
            ) : null}
            {m.role === 'user' ? (
              m.content ? (
                <div className="chat-bubble">
                  <div className="chat-text chat-body">{m.content}</div>
                </div>
              ) : null
            ) : (
              <>
                {m.reasoning ? (
                  <details className="chat-reasoning">
                    <summary>Thinking</summary>
                    <div className="chat-reasoning-body">
                      <ChatMarkdown text={m.reasoning} streaming={false} />
                    </div>
                  </details>
                ) : null}
                <div className="chat-body">
                  <ChatMarkdown text={m.content} streaming={false} />
                </div>
                {m.model ? (
                  <div className="chat-shared-model small muted">
                    Answered by {m.model}
                    {m.status ? ` · ${m.status}` : ''}
                  </div>
                ) : null}
              </>
            )}
          </article>
        ))}
      </div>
      <footer className="chat-shared-foot">
        <p className="muted small">A read-only copy of this chat as it was when it was shared. Replies cannot be added here.</p>
        <Link className="btn" to="/chat">
          Start your own chat
        </Link>
      </footer>
    </div>
  );
}
