/**
 * Janus Chat settings: the per-conversation panel (instructions, thinking
 * effort) with the user's own instructions for every chat, and the row
 * menu in the history list (rename, pin, delete).
 */
import type { CapabilityResolved } from '../../lib/capabilities';
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

export interface EffortOption {
  value: string;
  label: string;
  hint: string;
}

/** Every thinking setting a chat can hold, least to most. */
export const EFFORTS: EffortOption[] = [
  { value: '', label: 'Model default', hint: 'Let the model decide how long to think.' },
  { value: 'none', label: 'Off', hint: 'Answers straight away, without thinking first.' },
  { value: 'minimal', label: 'Minimal', hint: 'Barely any thinking.' },
  { value: 'low', label: 'Low', hint: 'Quick answers, least thinking.' },
  { value: 'medium', label: 'Medium', hint: 'A balance of speed and care.' },
  { value: 'high', label: 'High', hint: 'Thinks longer; slower and uses more tokens.' },
  { value: 'xhigh', label: 'Extra high', hint: 'Thinks longer still.' },
  { value: 'max', label: 'Maximum', hint: 'Thinks as long as the model allows; slowest.' },
];

const WHEN_UNKNOWN = ['', 'low', 'medium', 'high'];

/**
 * The thinking settings to offer for one model, from what is known about
 * it (lib/capabilities.ts) and how Janus turns its thinking off
 * (`thinkingOff`, the model's `thinking_off`). Off is offered only where
 * the model can turn thinking off; a model that always thinks says so and
 * names its lowest setting. Nothing known: the three every client knows.
 * `current` stays listed even when the model does not take it, so a chat
 * moved to another model still shows what it holds (Janus sends the
 * nearest value the model takes).
 */
export function effortOptions(
  caps: Record<string, CapabilityResolved> | undefined | null,
  current = '',
  thinkingOff?: string | null,
): { options: EffortOption[]; note: string } {
  const pick = (values: string[]) => EFFORTS.filter((e) => values.includes(e.value));
  const levels = caps?.['thinking.levels']?.value;
  const canTurnOff = !!thinkingOff && thinkingOff !== 'never';
  const always = thinkingOff === 'never';
  let values = WHEN_UNKNOWN;
  let note = '';
  let lowest: EffortOption | undefined;
  if (caps?.['thinking.supported']?.value === false || (Array.isArray(levels) && levels.length === 0)) {
    values = [''];
    note = 'This model does not have a thinking setting.';
  } else if (Array.isArray(levels)) {
    values = ['', ...levels.filter((l) => l !== 'none')];
    if (canTurnOff) values.push('none');
    lowest = EFFORTS.find((e) => e.value !== '' && e.value !== 'none' && levels.includes(e.value));
  } else if (canTurnOff) {
    values = ['', 'none', 'low', 'medium', 'high'];
  }
  if (always) {
    note = lowest
      ? `This model always thinks and cannot turn it off. Its lowest setting is ${lowest.label}.`
      : 'This model always thinks and cannot turn it off.';
  }
  const options = pick(values);
  if (current && !values.includes(current)) {
    const held = EFFORTS.find((e) => e.value === current);
    const hint =
      current === 'none' && always && lowest
        ? `This model always thinks; ${lowest.label} is used instead.`
        : 'This model does not take it; the nearest setting it takes is used.';
    if (held) options.push({ ...held, hint });
  }
  return { options, note };
}

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
  capabilities,
  thinkingOff,
  onSave,
}: {
  open: boolean;
  onClose: () => void;
  instructions: string;
  effort: string;
  /** What is known about the chosen model; absent for managed models and personal plans. */
  capabilities?: Record<string, CapabilityResolved> | null;
  /** How Janus turns the chosen model's thinking off (the model's `thinking_off`). */
  thinkingOff?: string | null;
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
            For models that can reason before answering. The settings shown are the ones this model takes; if you switch models,
            Janus uses the nearest one the new model takes. {effortOptions(capabilities, eff, thinkingOff).note}
          </p>
          <div className="chat-effort" role="radiogroup" aria-labelledby="chat-effort-label">
            {effortOptions(capabilities, eff, thinkingOff).options.map((e) => (
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
