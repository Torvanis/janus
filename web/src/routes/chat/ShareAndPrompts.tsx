/**
 * Janus Chat: the Share & export panel (links, Markdown/JSON/PDF) and the
 * saved-prompt library ("/" commands with {{variables}}).
 */
import { useEffect, useMemo, useState, type ReactNode } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { api, ApiError } from '../../lib/api';
import { Drawer } from '../../components/ui';

/* --- Share & export --------------------------------------------------------- */

interface ShareLink {
  id: string;
  url: string;
  title: string;
  message_count: number;
  views: number;
  created_at: string;
}

function absolute(url: string): string {
  return new URL(url, window.location.origin).toString();
}

export function ShareDrawer({
  open,
  onClose,
  conversationId,
  temporary,
  onPrint,
  sharesEnabled = true,
  exportEnabled = true,
}: {
  open: boolean;
  onClose: () => void;
  conversationId: string;
  temporary: boolean;
  onPrint: () => void;
  /** Administration → Chat switches. Off hides the section. */
  sharesEnabled?: boolean;
  exportEnabled?: boolean;
}): ReactNode {
  const qc = useQueryClient();
  const key = ['chat', 'shares', conversationId];
  const shares = useQuery({
    queryKey: key,
    queryFn: () => api.get<{ shares: ShareLink[] }>(`/api/v1/chat/conversations/${conversationId}/shares`),
    enabled: open && !temporary && sharesEnabled,
  });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [copied, setCopied] = useState<string | null>(null);

  useEffect(() => {
    if (open) {
      setError(null);
      setCopied(null);
    }
  }, [open]);

  async function create(): Promise<void> {
    setBusy(true);
    setError(null);
    try {
      const link = await api.post<ShareLink>(`/api/v1/chat/conversations/${conversationId}/shares`, {});
      await qc.invalidateQueries({ queryKey: key });
      void copy(link);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not make a link.');
    } finally {
      setBusy(false);
    }
  }

  async function copy(link: ShareLink): Promise<void> {
    try {
      await navigator.clipboard.writeText(absolute(link.url));
      setCopied(link.id);
    } catch {
      setCopied(null);
    }
  }

  async function revoke(link: ShareLink): Promise<void> {
    try {
      await api.del(`/api/v1/chat/conversations/${conversationId}/shares/${link.id}`);
      await qc.invalidateQueries({ queryKey: key });
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not stop sharing.');
    }
  }

  const exportHref = (format: string) => `/api/v1/chat/conversations/${conversationId}/export?format=${format}`;

  return (
    <Drawer open={open} onClose={onClose} title="Share & export">
      <div className="chat-settings">
        {sharesEnabled ? (
          <section>
            <span className="chat-settings-label">Share a link</span>
            {temporary ? (
              <p className="chat-settings-hint">Temporary chats cannot be shared.</p>
            ) : (
              <>
                <p className="chat-settings-hint">
                  A read-only copy of the chat as it is now, for anyone signed in to this Janus. Later messages are not shared,
                  and your instructions and costs are left out. Stop sharing at any time.
                </p>
                <div>
                  <button type="button" className="btn btn-primary" disabled={busy} onClick={() => void create()}>
                    {busy ? 'Making a link…' : 'Copy a new link'}
                  </button>
                </div>
                {shares.data && shares.data.shares.length > 0 ? (
                  <ul className="chat-share-list" aria-label="Links">
                    {shares.data.shares.map((l) => (
                      <li key={l.id} className="chat-share-row">
                        <div className="chat-share-meta">
                          <a href={l.url} target="_blank" rel="noopener noreferrer">
                            {new Date(l.created_at).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })}
                          </a>
                          <span className="small muted">
                            {l.message_count} messages · {l.views} {l.views === 1 ? 'view' : 'views'}
                          </span>
                        </div>
                        <button type="button" className="btn btn-sm" onClick={() => void copy(l)}>
                          {copied === l.id ? 'Copied' : 'Copy'}
                        </button>
                        <button type="button" className="btn btn-sm btn-ghost" onClick={() => void revoke(l)}>
                          Stop sharing
                        </button>
                      </li>
                    ))}
                  </ul>
                ) : null}
              </>
            )}
          </section>
        ) : null}

        <section>
          <span className="chat-settings-label">{exportEnabled ? 'Export' : 'Print'}</span>
          <p className="chat-settings-hint">
            {exportEnabled
              ? 'The messages you see, with the model and cost of each reply. Files are listed by name.'
              : 'Print the messages you see, or save them as a PDF.'}
          </p>
          <div className="chat-export-row">
            {exportEnabled ? (
              <>
                <a className="btn" href={exportHref('markdown')} download>
                  Markdown
                </a>
                <a className="btn" href={exportHref('json')} download>
                  JSON
                </a>
              </>
            ) : null}
            <button
              type="button"
              className="btn"
              onClick={() => {
                onClose();
                onPrint();
              }}
            >
              PDF (print)
            </button>
          </div>
        </section>
        {error ? (
          <p className="chat-error" role="alert">
            {error}
          </p>
        ) : null}
      </div>
    </Drawer>
  );
}

/* --- Saved prompts ---------------------------------------------------------- */

export interface ChatPrompt {
  id: string;
  name: string;
  title: string;
  body: string;
  updated_at: string;
}

export const PROMPTS_KEY = ['chat', 'prompts'] as const;

export function usePrompts() {
  return useQuery({
    queryKey: PROMPTS_KEY,
    queryFn: () => api.get<{ prompts: ChatPrompt[] }>('/api/v1/chat/prompts'),
    staleTime: 60_000,
  });
}

/** {{name}} placeholders in order of first appearance. */
export function promptVariables(body: string): string[] {
  const out: string[] = [];
  for (const m of body.matchAll(/\{\{\s*([a-zA-Z0-9_ -]{1,40}?)\s*\}\}/g)) {
    const v = m[1]!.trim();
    if (!out.includes(v)) out.push(v);
  }
  return out;
}

export function fillPrompt(body: string, values: Record<string, string>): string {
  return body.replace(/\{\{\s*([a-zA-Z0-9_ -]{1,40}?)\s*\}\}/g, (all, name: string) => values[name.trim()] ?? all);
}

/** The "/" query the user is typing, when the draft is a slash command so far. */
export function slashQuery(draft: string): string | null {
  const m = /^\/([a-z0-9-]*)$/i.exec(draft);
  return m ? m[1]!.toLowerCase() : null;
}

/** The suggestion list above the composer while typing "/…". */
export function SlashMenu({
  prompts,
  query,
  active,
  onPick,
  onManage,
}: {
  prompts: ChatPrompt[];
  query: string;
  active: number;
  onPick: (p: ChatPrompt) => void;
  onManage: () => void;
}): ReactNode {
  const list = matchPrompts(prompts, query);
  return (
    <div className="chat-slash" role="listbox" aria-label="Saved prompts">
      {list.length === 0 ? (
        <p className="chat-slash-empty">
          {prompts.length === 0 ? 'No saved prompts yet.' : `No prompt called /${query}.`}{' '}
          <button type="button" className="chat-link" onMouseDown={(e) => e.preventDefault()} onClick={onManage}>
            {prompts.length === 0 ? 'Save your first prompt' : 'Manage prompts'}
          </button>
        </p>
      ) : (
        list.slice(0, 8).map((p, i) => (
          <button
            key={p.id}
            type="button"
            role="option"
            aria-selected={i === active}
            className={i === active ? 'chat-slash-item active' : 'chat-slash-item'}
            onMouseDown={(e) => e.preventDefault()}
            onClick={() => onPick(p)}
          >
            <span className="chat-slash-name">/{p.name}</span>
            <span className="chat-slash-title">{p.title || p.body.slice(0, 80)}</span>
          </button>
        ))
      )}
    </div>
  );
}

export function matchPrompts(prompts: ChatPrompt[], query: string): ChatPrompt[] {
  const q = query.toLowerCase();
  return prompts
    .filter((p) => p.name.startsWith(q) || (q.length > 1 && (p.name.includes(q) || p.title.toLowerCase().includes(q))))
    .sort((a, b) => Number(!a.name.startsWith(q)) - Number(!b.name.startsWith(q)) || a.name.localeCompare(b.name));
}

/** Ask for a prompt's {{variables}} before inserting it. */
export function PromptFillDialog({
  prompt,
  onCancel,
  onInsert,
}: {
  prompt: ChatPrompt | null;
  onCancel: () => void;
  onInsert: (text: string) => void;
}): ReactNode {
  const vars = useMemo(() => (prompt ? promptVariables(prompt.body) : []), [prompt]);
  const [values, setValues] = useState<Record<string, string>>({});
  useEffect(() => setValues({}), [prompt]);
  if (!prompt) return null;
  return (
    <Drawer
      open
      onClose={onCancel}
      title={`/${prompt.name}`}
      description={prompt.title || undefined}
      footer={
        <div className="row" style={{ justifyContent: 'flex-end', gap: 8 }}>
          <button type="button" className="btn" onClick={onCancel}>
            Cancel
          </button>
          <button type="button" className="btn btn-primary" onClick={() => onInsert(fillPrompt(prompt.body, values))}>
            Insert
          </button>
        </div>
      }
    >
      <form
        className="chat-settings"
        onSubmit={(e) => {
          e.preventDefault();
          onInsert(fillPrompt(prompt.body, values));
        }}
      >
        {vars.map((v, i) => (
          <label key={v} className="chat-fill-field">
            <span className="chat-settings-label">{v}</span>
            <textarea
              className="textarea"
              rows={v.toLowerCase().includes('text') ? 5 : 2}
              autoFocus={i === 0}
              value={values[v] ?? ''}
              onChange={(e) => setValues((x) => ({ ...x, [v]: e.target.value }))}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
                  e.preventDefault();
                  onInsert(fillPrompt(prompt.body, values));
                }
              }}
            />
          </label>
        ))}
        <p className="chat-settings-hint">Blank fields stay as {'{{name}}'} so you can fill them in the message box.</p>
      </form>
    </Drawer>
  );
}

/** The prompt library: list, add, edit, delete. */
export function PromptsDrawer({
  open,
  onClose,
  seed,
}: {
  open: boolean;
  onClose: () => void;
  /** Text to start a new prompt from (e.g. "Save as prompt" on a message). */
  seed?: string;
}): ReactNode {
  const qc = useQueryClient();
  const prompts = usePrompts();
  const [editing, setEditing] = useState<ChatPrompt | 'new' | null>(null);
  const [name, setName] = useState('');
  const [title, setTitle] = useState('');
  const [body, setBody] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    if (!open) return;
    setError(null);
    if (seed) start('new', seed);
    else setEditing(null);
  }, [open, seed]);

  function start(p: ChatPrompt | 'new', seedBody = ''): void {
    setEditing(p);
    setError(null);
    setName(p === 'new' ? '' : p.name);
    setTitle(p === 'new' ? '' : p.title);
    setBody(p === 'new' ? seedBody : p.body);
  }

  async function save(): Promise<void> {
    setSaving(true);
    setError(null);
    try {
      if (editing === 'new') await api.post('/api/v1/chat/prompts', { name, title, body });
      else if (editing) await api.patch(`/api/v1/chat/prompts/${editing.id}`, { name, title, body });
      await qc.invalidateQueries({ queryKey: PROMPTS_KEY });
      setEditing(null);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not save the prompt.');
    } finally {
      setSaving(false);
    }
  }

  async function remove(p: ChatPrompt): Promise<void> {
    if (!window.confirm(`Delete /${p.name}?`)) return;
    await api.del(`/api/v1/chat/prompts/${p.id}`);
    await qc.invalidateQueries({ queryKey: PROMPTS_KEY });
  }

  const vars = promptVariables(body);
  const list = prompts.data?.prompts ?? [];
  return (
    <Drawer
      open={open}
      onClose={onClose}
      title="Saved prompts"
      description="Type / in the message box to use one. {{name}} asks for a value when you insert it."
      footer={
        editing ? (
          <div className="row" style={{ justifyContent: 'flex-end', gap: 8 }}>
            <button type="button" className="btn" onClick={() => setEditing(null)}>
              Back
            </button>
            <button
              type="button"
              className="btn btn-primary"
              disabled={saving || !name.trim() || !body.trim()}
              onClick={() => void save()}
            >
              {saving ? 'Saving…' : 'Save prompt'}
            </button>
          </div>
        ) : (
          <div className="row" style={{ justifyContent: 'flex-end' }}>
            <button type="button" className="btn btn-primary" onClick={() => start('new')}>
              New prompt
            </button>
          </div>
        )
      }
    >
      {editing ? (
        <div className="chat-settings">
          <label className="chat-fill-field">
            <span className="chat-settings-label">Command</span>
            <span className="chat-prompt-name">
              <span aria-hidden="true">/</span>
              <input
                className="input"
                aria-label="Command"
                value={name}
                placeholder="summarise"
                maxLength={40}
                onChange={(e) => setName(e.target.value.toLowerCase().replace(/[^a-z0-9-]/g, '-'))}
              />
            </span>
          </label>
          <label className="chat-fill-field">
            <span className="chat-settings-label">Title (optional)</span>
            <input
              className="input"
              aria-label="Title"
              value={title}
              maxLength={120}
              placeholder="Summarize for a reader"
              onChange={(e) => setTitle(e.target.value)}
            />
          </label>
          <label className="chat-fill-field">
            <span className="chat-settings-label">Prompt</span>
            <textarea
              className="textarea"
              aria-label="Prompt"
              rows={8}
              value={body}
              placeholder={'Summarize the text below for {{audience}} in five bullet points.\n\n{{text}}'}
              onChange={(e) => setBody(e.target.value)}
            />
          </label>
          {vars.length > 0 ? <p className="chat-settings-hint">Asks for: {vars.join(', ')}</p> : null}
          {error ? (
            <p className="chat-error" role="alert">
              {error}
            </p>
          ) : null}
        </div>
      ) : list.length === 0 ? (
        <div className="chat-prompts-empty">
          <p>No saved prompts yet.</p>
          <p className="chat-settings-hint">
            Save the instructions you type again and again, like “Rewrite this email to sound friendlier: {'{{text}}'}”, and use
            them with /name.
          </p>
        </div>
      ) : (
        <ul className="chat-prompt-list" aria-label="Prompts">
          {list.map((p) => (
            <li key={p.id} className="chat-prompt-row">
              <button type="button" className="chat-prompt-open" onClick={() => start(p)}>
                <span className="chat-slash-name">/{p.name}</span>
                <span className="chat-slash-title">{p.title || p.body.slice(0, 90)}</span>
              </button>
              <button
                type="button"
                className="btn btn-sm btn-ghost"
                aria-label={`Delete /${p.name}`}
                onClick={() => void remove(p)}
              >
                Delete
              </button>
            </li>
          ))}
        </ul>
      )}
    </Drawer>
  );
}
