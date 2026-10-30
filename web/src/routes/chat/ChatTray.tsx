/**
 * The chat tray: one panel from the right that holds everything the
 * composer used to crowd onto its bar.
 *
 * - This message: Deep research, Compare (up to two more models).
 * - Tools: what this chat's model may use (switches are per chat).
 * - Your models: the person's own model for chat, images, video and music.
 *   These are personal defaults, used in every chat while they are still
 *   available to them (PUT /api/v1/chat/settings); "Automatic" = the plan's
 *   own model when chatting on a plan, else the organization's.
 * - Who pays, and a link to the other chat settings.
 *
 * The composer keeps only: attach · chat model · Tools (with a count) ·
 * send, plus removable chips for the modes that are on.
 */
import { useState, type ReactNode } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { api, ApiError } from '../../lib/api';
import { Drawer } from '../../components/ui';
import { CHAT_SETTINGS_KEY, useChatSettings, type ChatUserSettings } from './ChatSettings';
import type { ToolInfo } from './ChatTools';

export interface TrayOption {
  value: string;
  label: string;
}

/** "my/xai/grok-imagine-video" → "grok-imagine-video · your plan". */
export function mediaLabel(m: string): string {
  return m.startsWith('my/') ? `${m.split('/').slice(2).join('/')} · your plan` : m;
}

/** The composer button that opens the tray. */
export function TrayButton({
  onOpen,
  toolsOn,
  modes,
}: {
  onOpen: () => void;
  /** The tools on for this chat (labels). */
  toolsOn: string[];
  /** How many modes (research / compare) are on for the next message. */
  modes: number;
}): ReactNode {
  const n = toolsOn.length + modes;
  return (
    <button
      type="button"
      className={`chat-tray-button${modes > 0 ? ' chat-tray-button-on' : ''}`}
      aria-haspopup="dialog"
      aria-label={toolsOn.length > 0 ? `Tools: ${toolsOn.join(', ')}` : 'Tools and models'}
      title="Tools, modes and your models"
      onClick={onOpen}
    >
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" aria-hidden="true">
        <path d="M4 6h10M18 6h2M4 12h4M12 12h8M4 18h12M20 18h0" strokeLinecap="round" />
        <circle cx="16" cy="6" r="2" />
        <circle cx="10" cy="12" r="2" />
        <circle cx="18" cy="18" r="2" />
      </svg>
      <span className="chat-tray-button-label">Tools</span>
      {n > 0 ? (
        <span className="chat-tray-count" aria-hidden="true">
          {n}
        </span>
      ) : null}
    </button>
  );
}

/** Removable chips above the input for what is on for the next message. */
export function ModeChips({
  research,
  compare,
  effortLabel,
  labelFor,
  onResearchOff,
  onCompareOff,
  onEffort,
}: {
  research: boolean;
  compare: string[];
  effortLabel: string;
  labelFor: (v: string) => string;
  onResearchOff: () => void;
  onCompareOff: () => void;
  onEffort: () => void;
}): ReactNode {
  if (!research && compare.length === 0 && !effortLabel) return null;
  return (
    <div className="chat-mode-chips" aria-label="On for this message">
      {research ? (
        <span className="chat-mode-chip">
          Deep research
          <button type="button" aria-label="Turn off deep research" onClick={onResearchOff}>
            ×
          </button>
        </span>
      ) : null}
      {compare.length > 0 ? (
        <span className="chat-mode-chip" title={compare.map(labelFor).join(', ')}>
          {compare.length === 1 ? `Compare: pick one more besides ${labelFor(compare[0]!)}` : `Compare ${compare.length} models`}
          <button type="button" aria-label="Stop comparing" onClick={onCompareOff}>
            ×
          </button>
        </span>
      ) : null}
      {effortLabel ? (
        <button
          type="button"
          className="chat-mode-chip chat-effort-chip"
          title="Thinking effort (chat settings)"
          onClick={onEffort}
        >
          Thinking: {effortLabel}
        </button>
      ) : null}
    </div>
  );
}

function Section({ title, hint, children }: { title: string; hint?: string; children: ReactNode }): ReactNode {
  return (
    <section className="chat-tray-section" aria-label={title}>
      <h3 className="chat-tray-title">{title}</h3>
      {hint ? <p className="chat-tray-hint">{hint}</p> : null}
      {children}
    </section>
  );
}

const CHILD_MEDIA_WORDS: Record<string, string> = {
  image: 'making pictures',
  video: 'making videos',
  music: 'making music',
  transcription: 'sound and video files',
};

/** Why Child Safety has removed media tools from this person's Chat. */
export function childMediaNote(off: string[]): string {
  const words = off.map((k) => CHILD_MEDIA_WORDS[k]).filter(Boolean);
  if (words.length === 0) return '';
  const list = words.length === 1 ? words[0] : `${words.slice(0, -1).join(', ')} and ${words[words.length - 1]}`;
  return `Child Safety is on for your account, so ${list} ${words.length === 1 ? 'is' : 'are'} not available here.`;
}

function Switch({
  label,
  hint,
  checked,
  disabled,
  onChange,
}: {
  label: string;
  hint?: string;
  checked: boolean;
  disabled?: boolean;
  onChange: (v: boolean) => void;
}): ReactNode {
  return (
    <label className="chat-tools-row">
      <span className="chat-tools-text">
        <span className="chat-tools-name">{label}</span>
        {hint ? <span className="chat-tools-desc">{hint}</span> : null}
      </span>
      <input
        type="checkbox"
        role="switch"
        className="chat-tools-switch"
        aria-label={label}
        checked={checked}
        disabled={disabled}
        onChange={(e) => onChange(e.target.checked)}
      />
    </label>
  );
}

type ModelKey = 'default_model' | 'image_model' | 'video_model' | 'music_model';

export function ChatTray({
  open,
  onClose,
  busy,
  model,
  chatOptions,
  research,
  onResearch,
  compare,
  onCompare,
  tools,
  available,
  off,
  onToolsOff,
  imageModels,
  videoModels,
  musicModels,
  childMediaOff = [],
  onImageDefault,
  billedTo,
  onOpenSettings,
}: {
  open: boolean;
  onClose: () => void;
  /** A reply is running: modes cannot change until it ends. */
  busy: boolean;
  model: string;
  chatOptions: TrayOption[];
  research: boolean;
  onResearch: (v: boolean) => void;
  compare: string[];
  onCompare: (v: string[]) => void;
  tools: ToolInfo[];
  available: string[];
  off: string[];
  onToolsOff: (off: string[]) => void;
  imageModels: string[];
  videoModels: string[];
  musicModels: string[];
  /** Media kinds Child Safety switches off for this person (image, video, music, transcription). */
  childMediaOff?: string[];
  /** The image default changed: the open chat follows it. */
  onImageDefault?: (m: string) => void;
  billedTo: string;
  onOpenSettings: () => void;
}): ReactNode {
  const qc = useQueryClient();
  const settings = useChatSettings();
  const [saving, setSaving] = useState<ModelKey | null>(null);
  const [saved, setSaved] = useState<ModelKey | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [filter, setFilter] = useState('');
  const here = tools.filter((t) => available.includes(t.id));
  // Compare picks every model that answers (the bar model is not added on
  // top); the first one picked is preferred: the chat goes on from it.
  const others = chatOptions;
  const label = (v: string) => chatOptions.find((o) => o.value === v)?.label ?? v;

  async function saveModel(key: ModelKey, value: string): Promise<void> {
    setSaving(key);
    setSaved(null);
    setError(null);
    try {
      const next = await api.put<ChatUserSettings>('/api/v1/chat/settings', { [key]: value });
      qc.setQueryData(CHAT_SETTINGS_KEY, next);
      if (key === 'image_model') onImageDefault?.(value);
      setSaved(key);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not save that change.');
    } finally {
      setSaving(null);
    }
  }

  const s = settings.data;
  const picker = (key: ModelKey, title: string, list: TrayOption[], auto: string) => {
    const value = (s?.[key] as string | undefined) ?? '';
    return (
      <label className="chat-tray-model" key={key}>
        <span className="chat-tray-model-name">{title}</span>
        <select
          className="select"
          aria-label={`Your ${title.toLowerCase()} model`}
          value={list.some((o) => o.value === value) ? value : ''}
          disabled={!s || saving === key}
          onChange={(e) => void saveModel(key, e.target.value)}
        >
          <option value="">{auto}</option>
          {list.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>
        {saved === key ? (
          <span className="chat-tray-saved" role="status">
            Saved
          </span>
        ) : null}
      </label>
    );
  };
  const asOptions = (l: string[]) => l.map((m) => ({ value: m, label: mediaLabel(m) }));

  return (
    <Drawer
      open={open}
      onClose={onClose}
      title="Tools & models"
      description="What the assistant can use, and the models you want for each job."
    >
      <div className="chat-tray">
        <Section title="This message" hint={busy ? 'Wait for the reply to finish to change these.' : undefined}>
          <Switch
            label="Deep research"
            hint="Many searches and page reads, then a cited report. Takes a few minutes."
            checked={research}
            disabled={busy}
            onChange={(v) => {
              onResearch(v);
              if (v) onCompare([]);
            }}
          />
          <div className="chat-tray-compare" role="group" aria-label="Compare with">
            <div className="chat-tools-text">
              <span className="chat-tools-name">Compare</span>
              <span className="chat-tools-desc">
                Send the message to two or three models and see the answers side by side. Only the models you pick answer; the
                first one is preferred and the chat goes on from it. Each is billed for its own reply.
              </span>
            </div>
            {others.length < 2 ? (
              <p className="chat-tray-hint">You need two chat models to compare.</p>
            ) : (
              <>
                {others.length > 6 ? (
                  <input
                    type="search"
                    className="input chat-tray-filter"
                    placeholder={`Find a model (${others.length})`}
                    aria-label="Find a model to compare with"
                    value={filter}
                    onChange={(e) => setFilter(e.target.value)}
                  />
                ) : null}
                <ul className="chat-tray-compare-list">
                  {[...others]
                    // Chosen first in the order picked, then the rest by name.
                    .sort((a, b) => {
                      const ia = compare.indexOf(a.value);
                      const ib = compare.indexOf(b.value);
                      if (ia >= 0 || ib >= 0) return (ia < 0 ? 99 : ia) - (ib < 0 ? 99 : ib);
                      return a.label.localeCompare(b.label);
                    })
                    .filter((o) => compare.includes(o.value) || o.label.toLowerCase().includes(filter.trim().toLowerCase()))
                    .map((o) => {
                      const on = compare.includes(o.value);
                      const preferred = compare[0] === o.value;
                      return (
                        <li key={o.value}>
                          <label
                            className={`chat-tray-option${on ? ' chat-tray-option-on' : ''}${preferred ? ' chat-tray-option-preferred' : ''}`}
                          >
                            <input
                              type="checkbox"
                              aria-label={`Compare with ${o.label}`}
                              checked={on}
                              disabled={busy || (!on && compare.length >= 3)}
                              onChange={(e) => {
                                // The first pick also picks the bar model (preferred), so
                                // "compare with X" is one click; uncheck it to leave it out.
                                const start =
                                  compare.length === 0 && o.value !== model && others.some((x) => x.value === model)
                                    ? [model]
                                    : compare;
                                onCompare(e.target.checked ? [...start, o.value] : compare.filter((x) => x !== o.value));
                                if (e.target.checked) onResearch(false);
                              }}
                            />
                            <span title={o.label}>{o.label}</span>
                            {preferred ? <span className="chat-tray-preferred">Preferred</span> : null}
                          </label>
                        </li>
                      );
                    })}
                </ul>
                <p className="chat-tray-hint chat-tray-count-line">
                  {compare.length === 0
                    ? 'Pick two or three.'
                    : compare.length === 1
                      ? 'Pick at least one more.'
                      : `${compare.length} of 3 picked.`}
                </p>
              </>
            )}
          </div>
        </Section>

        <Section
          title="Tools for this chat"
          hint={here.length > 0 ? `What ${label(model) || 'this model'} may use. The model decides when.` : undefined}
        >
          {here.length === 0 ? (
            <p className="chat-tray-hint">This model cannot use tools.</p>
          ) : (
            <div role="group" aria-label="Tools">
              {here.map((t) => (
                <Switch
                  key={t.id}
                  label={t.label}
                  hint={t.description}
                  checked={!off.includes(t.id)}
                  onChange={(v) => onToolsOff(v ? off.filter((x) => x !== t.id) : [...off, t.id])}
                />
              ))}
            </div>
          )}
          {childMediaOff.length > 0 ? (
            <p className="chat-tray-hint" role="note">
              {childMediaNote(childMediaOff)}
            </p>
          ) : null}
        </Section>

        <Section
          title="Your models"
          hint="Your defaults for every chat. Automatic uses your plan’s model when you chat on a plan, otherwise the organization’s."
        >
          {picker('default_model', 'Chat', chatOptions, 'Organization default')}
          {imageModels.length > 0 ? picker('image_model', 'Images', asOptions(imageModels), 'Automatic') : null}
          {videoModels.length > 0 ? picker('video_model', 'Video', asOptions(videoModels), 'Automatic') : null}
          {musicModels.length > 0 ? picker('music_model', 'Music', asOptions(musicModels), 'Automatic') : null}
          {error ? (
            <p className="chat-error" role="alert">
              {error}
            </p>
          ) : null}
        </Section>

        <Section title="Billing">
          <p className="chat-tray-billing" aria-label="Billed to">
            Billed to <strong>{billedTo}</strong>
          </p>
        </Section>

        <button type="button" className="btn btn-sm chat-tray-more" onClick={onOpenSettings}>
          Instructions, thinking and memory…
        </button>
      </div>
    </Drawer>
  );
}
