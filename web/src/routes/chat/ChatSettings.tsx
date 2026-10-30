/**
 * Janus Chat settings: the per-conversation panel (instructions, thinking
 * effort) with the user's own instructions for every chat, and the row
 * menu in the history list (rename, pin, delete).
 */
import { useEffect, useRef, useState, type ReactNode } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { api, ApiError } from '../../lib/api';
import { Drawer } from '../../components/ui';
import { MemorySettings } from './ChatExtras';

export interface ChatUserSettings {
  instructions: string;
  default_model: string;
  /** The person's own models for images / video / music ('' = automatic). */
  image_model?: string;
  video_model?: string;
  music_model?: string;
}

export const CHAT_SETTINGS_KEY = ['chat', 'settings'] as const;

export function useChatSettings() {
  return useQuery({
    queryKey: CHAT_SETTINGS_KEY,
    queryFn: () => api.get<ChatUserSettings>('/api/v1/chat/settings'),
    staleTime: 60_000,
  });
}

export const EFFORTS: { value: string; label: string; hint: string }[] = [
  { value: '', label: 'Model default', hint: 'Let the model decide how long to think.' },
  { value: 'low', label: 'Low', hint: 'Quick answers, least thinking.' },
  { value: 'medium', label: 'Medium', hint: 'A balance of speed and care.' },
  { value: 'high', label: 'High', hint: 'Thinks longest; slower and uses more tokens.' },
];

const MAX = 8000;

/**
 * The settings panel. `conversation` is null for a chat that has not been
 * sent yet: its values are held by the page and applied when it is created.
 */
export function ChatSettingsDrawer({
  open,
  onClose,
  instructions,
  effort,
  onSave,
}: {
  open: boolean;
  onClose: () => void;
  instructions: string;
  effort: string;
  onSave: (v: { instructions: string; effort: string }) => Promise<void>;
}): ReactNode {
  const qc = useQueryClient();
  const settings = useChatSettings();
  const [mine, setMine] = useState('');
  const [here, setHere] = useState(instructions);
  const [eff, setEff] = useState(effort);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!open) return;
    setHere(instructions);
    setEff(effort);
    setError(null);
  }, [open, instructions, effort]);
  useEffect(() => {
    if (open && settings.data) setMine(settings.data.instructions);
  }, [open, settings.data]);

  async function save(): Promise<void> {
    setSaving(true);
    setError(null);
    try {
      if (settings.data && mine !== settings.data.instructions) {
        const next = await api.put<ChatUserSettings>('/api/v1/chat/settings', { instructions: mine });
        qc.setQueryData(CHAT_SETTINGS_KEY, next);
      }
      await onSave({ instructions: here, effort: eff });
      onClose();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not save.');
    } finally {
      setSaving(false);
    }
  }

  return (
    <Drawer
      open={open}
      onClose={onClose}
      title="Chat settings"
      description="Instructions go first in every request; the model sees them, they are not shown in the thread."
      footer={
        <div className="row" style={{ justifyContent: 'flex-end', gap: 8 }}>
          <button type="button" className="btn" onClick={onClose}>
            Cancel
          </button>
          <button
            type="button"
            className="btn btn-primary"
            disabled={saving || mine.length > MAX || here.length > MAX}
            onClick={() => void save()}
          >
            {saving ? 'Saving…' : 'Save'}
          </button>
        </div>
      }
    >
      <div className="chat-settings">
        <section>
          <label className="chat-settings-label" htmlFor="chat-here">
            This chat
          </label>
          <p className="chat-settings-hint">Only for this conversation, after your own instructions.</p>
          <textarea
            id="chat-here"
            className="textarea"
            aria-label="Instructions for this chat"
            rows={5}
            value={here}
            placeholder="e.g. You are a patient maths tutor. Explain step by step and check my working."
            onChange={(e) => setHere(e.target.value)}
          />
          <Counter n={here.length} />
        </section>

        <section>
          <span className="chat-settings-label" id="chat-effort-label">
            Thinking
          </span>
          <p className="chat-settings-hint">
            For models that can reason before answering. Janus fits it to what the model takes, and models without the setting
            ignore it.
          </p>
          <div className="chat-effort" role="radiogroup" aria-labelledby="chat-effort-label">
            {EFFORTS.map((e) => (
              <label key={e.value || 'default'} className={eff === e.value ? 'chat-effort-opt active' : 'chat-effort-opt'}>
                <input
                  type="radio"
                  name="chat-effort"
                  value={e.value}
                  checked={eff === e.value}
                  onChange={() => setEff(e.value)}
                />
                <span className="chat-effort-name">{e.label}</span>
                <span className="chat-effort-hint">{e.hint}</span>
              </label>
            ))}
          </div>
        </section>

        <section>
          <label className="chat-settings-label" htmlFor="chat-mine">
            Your instructions for every chat
          </label>
          <p className="chat-settings-hint">How you would like replies in general: your name, tone, language, format.</p>
          <textarea
            id="chat-mine"
            className="textarea"
            aria-label="Your instructions for every chat"
            rows={5}
            value={mine}
            disabled={!settings.data}
            placeholder="e.g. Call me Sam. Keep answers short unless I ask for detail. Use metric units."
            onChange={(e) => setMine(e.target.value)}
          />
          <Counter n={mine.length} />
        </section>

        <section aria-labelledby="chat-memory-title">
          <span className="chat-settings-label" id="chat-memory-title">
            Memory
          </span>
          <MemorySettings />
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

function Counter({ n }: { n: number }): ReactNode {
  return (
    <p className={n > MAX ? 'chat-settings-count chat-error' : 'chat-settings-count'}>
      {n.toLocaleString()} / {MAX.toLocaleString()}
    </p>
  );
}

/** The ⋯ menu on a history row: rename, pin / unpin, delete. */
export function ConversationMenu({
  title,
  pinned,
  onRename,
  onPin,
  onDelete,
}: {
  title: string;
  pinned: boolean;
  onRename: () => void;
  onPin: () => void;
  onDelete: () => void;
}): ReactNode {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const close = (e: MouseEvent | KeyboardEvent) => {
      if (e instanceof KeyboardEvent ? e.key === 'Escape' : !ref.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', close);
    document.addEventListener('keydown', close);
    return () => {
      document.removeEventListener('mousedown', close);
      document.removeEventListener('keydown', close);
    };
  }, [open]);
  const item = (label: string, fn: () => void, danger = false) => (
    <button
      type="button"
      role="menuitem"
      className={danger ? 'chat-menu-item danger' : 'chat-menu-item'}
      onClick={() => {
        setOpen(false);
        fn();
      }}
    >
      {label}
    </button>
  );
  return (
    <div className={open ? 'chat-menu open' : 'chat-menu'} ref={ref}>
      <button
        type="button"
        className="chat-list-more"
        aria-label={`Options for ${title || 'chat'}`}
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <svg width="16" height="16" viewBox="0 0 24 24" aria-hidden="true" fill="currentColor">
          <circle cx="5.5" cy="12" r="1.6" />
          <circle cx="12" cy="12" r="1.6" />
          <circle cx="18.5" cy="12" r="1.6" />
        </svg>
      </button>
      {open ? (
        <div className="chat-menu-pop" role="menu">
          {item('Rename', onRename)}
          {item(pinned ? 'Unpin' : 'Pin', onPin)}
          {item('Delete', onDelete, true)}
        </div>
      ) : null}
    </div>
  );
}

/** Inline rename field for a history row. */
export function RenameField({ initial, onDone }: { initial: string; onDone: (title: string | null) => void }): ReactNode {
  const [v, setV] = useState(initial);
  return (
    <input
      className="chat-rename"
      aria-label="Chat title"
      value={v}
      autoFocus
      maxLength={200}
      onFocus={(e) => e.currentTarget.select()}
      onChange={(e) => setV(e.target.value)}
      onBlur={() => onDone(v.trim() && v.trim() !== initial ? v.trim() : null)}
      onKeyDown={(e) => {
        if (e.key === 'Enter') onDone(v.trim() && v.trim() !== initial ? v.trim() : null);
        if (e.key === 'Escape') onDone(null);
      }}
    />
  );
}
