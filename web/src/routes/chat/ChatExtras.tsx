/**
 * Janus Chat round 10 in the thread and the composer:
 *
 * - MadeFiles: files, videos and music a reply made with Janus's tools
 *   (create_file / create_video / create_music, deep-research reports),
 *   loaded from the stored attachment: file card (Preview / Download /
 *   Copy / Code), video player, audio player.
 * - MemoryChip: "Memory updated" under a reply that saved or forgot facts.
 * - MemorySettings: Settings → Memory (on/off, list, add, edit, delete).
 * - CompareRow: sibling replies made side by side, in columns.
 */
import { useEffect, useState, type ReactNode } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { api, ApiError } from '../../lib/api';
import type { ChatAttachment } from './Attachments';
import type { ChatActivity } from './ChatPage';
import { FileCard } from './Markdown';
import { wholeDocument } from './chat-files';

const content = (id: string) => `/api/v1/chat/attachments/${id}/content`;

/** A stored text file a reply made, shown as a file card once loaded. */
function StoredFile({ a }: { a: ChatAttachment }): ReactNode {
  const [text, setText] = useState<string | null>(null);
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    let live = true;
    fetch(content(a.id), { credentials: 'same-origin' })
      .then((r) => (r.ok ? r.text() : Promise.reject(new Error(String(r.status)))))
      .then((t) => live && setText(t))
      .catch(() => live && setFailed(true));
    return () => {
      live = false;
    };
  }, [a.id]);
  if (failed) {
    return (
      <div className="chat-file chat-file-missing">
        <div className="chat-file-head">
          <span className="chat-file-meta">
            <span className="chat-file-name">{a.filename}</span>
            <span className="chat-file-sub">The file could not be loaded.</span>
          </span>
        </div>
      </div>
    );
  }
  if (text === null) {
    return (
      <div className="chat-file" data-in-progress>
        <div className="chat-file-head">
          <span className="chat-file-icon" aria-hidden="true">
            FILE
          </span>
          <span className="chat-file-meta">
            <span className="chat-file-name">{a.filename}</span>
            <span className="chat-file-sub">Loading…</span>
          </span>
        </div>
      </div>
    );
  }
  const ext = a.filename.includes('.') ? a.filename.slice(a.filename.lastIndexOf('.') + 1).toLowerCase() : '';
  return <FileCard code={text} lang={ext} kind={wholeDocument(text, ext)} name={a.filename} stored />;
}

/** Files, videos and music a reply made (not images: those have their own view). */
export function MadeFiles({ attachments }: { attachments?: ChatAttachment[] }): ReactNode {
  const made = (attachments ?? []).filter((a) => a.kind === 'file' || a.kind === 'video' || a.kind === 'audio');
  if (made.length === 0) return null;
  return (
    <div className="chat-made">
      {made.map((a) =>
        a.kind === 'video' ? (
          <figure key={a.id} className="chat-media chat-media-video">
            <video controls preload="metadata" src={content(a.id)} aria-label={a.filename} />
            <figcaption>
              <a className="chat-gen-download" href={content(a.id)} download={a.filename}>
                Download video
              </a>
            </figcaption>
          </figure>
        ) : a.kind === 'audio' ? (
          <figure key={a.id} className="chat-media chat-media-audio">
            <span className="chat-media-icon" aria-hidden="true">
              ♪
            </span>
            <audio controls preload="metadata" src={content(a.id)} aria-label={a.filename} />
            <a className="chat-gen-download" href={content(a.id)} download={a.filename}>
              Download
            </a>
          </figure>
        ) : (
          <StoredFile key={a.id} a={a} />
        ),
      )}
    </div>
  );
}

/** "Memory updated" under a reply that saved or forgot something. */
export function MemoryChip({ activity, onOpen }: { activity?: ChatActivity[]; onOpen: () => void }): ReactNode {
  const saved = (activity ?? []).filter((a) => a.type === 'remember');
  const forgot = (activity ?? []).filter((a) => a.type === 'forget');
  if (saved.length + forgot.length === 0) return null;
  const parts = [
    ...saved.map((a) => ({ k: 'Saved', t: a.query ?? '' })),
    ...forgot.map((a) => ({ k: 'Forgot', t: a.query ?? '' })),
  ];
  return (
    <div className="chat-memory-chip" role="status">
      <span className="chat-memory-chip-title">Memory updated</span>
      <ul>
        {parts.map((p, i) => (
          <li key={i}>
            <span className="chat-memory-chip-kind">{p.k}</span> {p.t}
          </li>
        ))}
      </ul>
      <button type="button" className="chat-act chat-memory-manage" onClick={onOpen}>
        Manage memory
      </button>
    </div>
  );
}

export interface PickOption {
  value: string;
  label: string;
}

// ---- Memory settings ------------------------------------------------------------

interface MemoryDoc {
  enabled: boolean;
  available: boolean;
  limit: number;
  memories: { id: string; content: string; from_conversation_title?: string; created_at: string }[];
}

const MEMORY_KEY = ['chat', 'memory'];

export function useChatMemory(enabled = true) {
  return useQuery({ queryKey: MEMORY_KEY, queryFn: () => api.get<MemoryDoc>('/api/v1/chat/memory'), enabled });
}

/** Settings → Memory: what the assistant remembers about you. */
export function MemorySettings(): ReactNode {
  const qc = useQueryClient();
  const mem = useChatMemory();
  const [adding, setAdding] = useState('');
  const [editing, setEditing] = useState<{ id: string; text: string } | null>(null);
  const [error, setError] = useState<string | null>(null);
  const run = async (p: Promise<MemoryDoc>) => {
    setError(null);
    try {
      qc.setQueryData(MEMORY_KEY, await p);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not save that.');
    }
  };
  const doc = mem.data;
  if (!doc) return <p className="chat-settings-hint">{mem.isError ? 'Memory could not be loaded.' : 'Loading…'}</p>;
  if (!doc.available) {
    return <p className="chat-settings-hint">Memory is turned off for everyone on this instance.</p>;
  }
  return (
    <div className="chat-memory">
      <label className="chat-memory-switch">
        <span>
          <span className="chat-settings-label">Remember things across chats</span>
          <span className="chat-settings-hint">
            The assistant saves what you ask it to keep, and reads it in new chats. Temporary chats never use memory.
          </span>
        </span>
        <input
          type="checkbox"
          role="switch"
          className="chat-tools-switch"
          aria-label="Memory"
          checked={doc.enabled}
          onChange={(e) => void run(api.put<MemoryDoc>('/api/v1/chat/memory', { enabled: e.target.checked }))}
        />
      </label>
      {doc.memories.length === 0 ? (
        <p className="chat-memory-empty">Nothing saved yet. Tell the assistant “remember that …”, or add one below.</p>
      ) : (
        <ul className="chat-memory-list" aria-label="Memories">
          {doc.memories.map((m) => (
            <li key={m.id} className="chat-memory-item">
              {editing?.id === m.id ? (
                <form
                  className="chat-memory-edit"
                  onSubmit={(e) => {
                    e.preventDefault();
                    void run(api.patch<MemoryDoc>(`/api/v1/chat/memory/${m.id}`, { content: editing.text })).then(() =>
                      setEditing(null),
                    );
                  }}
                >
                  <input
                    className="input"
                    aria-label="Edit memory"
                    value={editing.text}
                    maxLength={500}
                    autoFocus
                    onChange={(e) => setEditing({ id: m.id, text: e.target.value })}
                  />
                  <button type="submit" className="btn btn-sm btn-primary">
                    Save
                  </button>
                  <button type="button" className="btn btn-sm" onClick={() => setEditing(null)}>
                    Cancel
                  </button>
                </form>
              ) : (
                <>
                  <span className="chat-memory-text">
                    {m.content}
                    {m.from_conversation_title ? (
                      <span className="chat-memory-from">from “{m.from_conversation_title}”</span>
                    ) : null}
                  </span>
                  <span className="chat-memory-actions">
                    <button
                      type="button"
                      className="chat-act"
                      aria-label={`Edit “${m.content}”`}
                      onClick={() => setEditing({ id: m.id, text: m.content })}
                    >
                      Edit
                    </button>
                    <button
                      type="button"
                      className="chat-act"
                      aria-label={`Delete “${m.content}”`}
                      onClick={() => void run(api.del<MemoryDoc>(`/api/v1/chat/memory/${m.id}`))}
                    >
                      Delete
                    </button>
                  </span>
                </>
              )}
            </li>
          ))}
        </ul>
      )}
      <form
        className="chat-memory-add"
        onSubmit={(e) => {
          e.preventDefault();
          if (!adding.trim()) return;
          void run(api.post<MemoryDoc>('/api/v1/chat/memory', { content: adding })).then(() => setAdding(''));
        }}
      >
        <input
          className="input"
          aria-label="Add a memory"
          placeholder="e.g. I live in Denver and prefer metric units"
          value={adding}
          maxLength={500}
          onChange={(e) => setAdding(e.target.value)}
        />
        <button type="submit" className="btn btn-sm" disabled={!adding.trim()}>
          Add
        </button>
      </form>
      <div className="chat-memory-foot">
        <span className="chat-settings-hint">
          {doc.memories.length} of {doc.limit}
        </span>
        {doc.memories.length > 0 ? (
          <button
            type="button"
            className="btn btn-sm btn-ghost"
            onClick={() => {
              if (window.confirm('Delete everything the assistant remembers about you?')) {
                void run(api.del<MemoryDoc>('/api/v1/chat/memory'));
              }
            }}
          >
            Delete all
          </button>
        ) : null}
      </div>
      {error ? (
        <p className="chat-error" role="alert">
          {error}
        </p>
      ) : null}
    </div>
  );
}
