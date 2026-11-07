/**
 * Administration → Chat. Instance-wide Janus Chat settings:
 *
 * - General: on/off, default model, organization instructions, starter
 *   suggestions, temporary-chat lifetime, share links, export, personal plans.
 * - Models: what the picker shows — hide, category, order, recommended —
 *   for every model granted to anyone; chat names/descriptions live on
 *   grants (Administration → Grants) and are shown here read-only.
 * - Access (Business): everyone, or only chosen groups, teams and people.
 *
 * Admins can configure Chat but never read anyone's chats.
 */
import { useEffect, useMemo, useState, type ReactNode } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { AsyncSection, Badge, Field, useToast } from '../../components/ui';
import { LookupInput } from '../../components/LookupInput';
import { groupLookup, teamLookup, userLookup } from '../../lib/lookups';
import { publicModelName, type Grant, type ManagedModel, type Model } from '../../lib/types';
import { formatDateTime } from '../../lib/format';
import { useSession } from '../../app/session';
import { ToolsTab } from './ChatToolsTab';

export interface ChatModelSetting {
  hidden?: boolean;
  category?: string;
  order?: number;
  recommended?: boolean;
  /** Whether Chat sends this model attached images ('' = automatic). */
  images?: '' | 'yes' | 'no';
  /** …sound and video files. */
  audio?: '' | 'yes' | 'no';
  video?: '' | 'yes' | 'no';
}

export interface WebPagePolicy {
  default: boolean;
  allow_private: boolean;
  allow_networks: string[];
  deny_networks: string[];
  allow_domains: string[];
  deny_domains: string[];
  ports: string[];
  https_only: boolean;
  proxy: string;
  tls_trust: 'system' | 'custom' | 'custom-only';
}

export interface ChatAdminDoc {
  enabled: boolean;
  access: { mode: 'everyone' | 'restricted'; groups: string[]; teams: string[]; users: string[]; message: string };
  default_model: string;
  temporary_ttl_hours: number;
  shares_enabled: boolean;
  export_enabled: boolean;
  personal_plans: boolean;
  org_instructions: string;
  starter_prompts: string[];
  /** Tools turned off for everyone (tools are on by default). */
  tools_off?: string[];
  /** The model create_image uses by default ('' = the first one granted). */
  image_model?: string;
  video_model?: string;
  music_model?: string;
  /** Turn attached sound into text / describe attached videos for models
   * that cannot take them ('' = the first one a person may use). */
  transcribe_model?: string;
  describe_model?: string;
  /** How chats get titles: the chat's own model (default), naming_model for
   * every chat, or off. '' follows JANUS_CHAT_AUTO_TITLE. */
  naming?: '' | 'same_model' | 'model' | 'off';
  naming_model?: string;
  /** Janus-run web search for models whose provider has none. */
  search?: ChatSearchSettings;
  /** Read-only: which search services have a stored key. */
  search_keys_set?: Record<string, boolean>;
  /** What read_page may open (JANUS_WEB_*, startup only). Read-only. */
  web_page_policy?: WebPagePolicy;
  /** Write-only: a new key for search.provider, or clear it. */
  search_key?: string;
  search_key_clear?: boolean;
  models: Record<string, ChatModelSetting>;
  updated_at?: string;
  access_licensed: boolean;
  preview_on: boolean;
}

export interface ChatSearchSettings {
  enabled: boolean;
  provider: '' | 'searxng' | 'brave' | 'exa' | 'tavily';
  url: string;
  max_results: number;
  /** SearXNG address installed with this deployment (Helm), read-only. */
  deployed?: string;
}

export const CHAT_ADMIN_KEY = ['admin', 'chat'] as const;
const URL = '/api/v1/admin/chat';

export const CATEGORY_LABELS: Record<string, string> = {
  general: 'General',
  fast: 'Fast',
  reasoning: 'Reasoning',
  coding: 'Coding',
  vision: 'Vision',
  local: 'Local & private',
  creative: 'Creative',
};

const TABS = ['general', 'models', 'tools', 'access'] as const;
type Tab = (typeof TABS)[number];
const TAB_LABELS: Record<Tab, string> = { general: 'General', models: 'Models', tools: 'Tools', access: 'Who can use Chat' };

export function ChatAdminPage(): ReactNode {
  const params = useParams<{ tab?: string }>();
  const navigate = useNavigate();
  const tab: Tab = (TABS as readonly string[]).includes(params.tab ?? '') ? (params.tab as Tab) : 'general';
  const doc = useQuery({ queryKey: CHAT_ADMIN_KEY, queryFn: () => api.get<ChatAdminDoc>(URL) });
  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">Chat</h1>
          <p className="page-subtitle">
            How Janus Chat works for everyone on this instance. Administrators configure Chat but can never read anyone's chats.
          </p>
        </div>
      </header>
      <div className="segmented" role="tablist" aria-label="Chat settings" style={{ marginBottom: 'var(--janus-space-4)' }}>
        {TABS.map((item) => (
          <button
            key={item}
            type="button"
            role="tab"
            aria-selected={tab === item}
            aria-pressed={tab === item}
            onClick={() => navigate(item === 'general' ? '/admin/chat' : `/admin/chat/${item}`)}
          >
            {TAB_LABELS[item]}
          </button>
        ))}
      </div>
      <AsyncSection query={doc}>
        {(d) => (
          <>
            {!d.preview_on ? (
              <div className="banner banner-warning" role="status" style={{ marginBottom: 'var(--janus-space-4)' }}>
                Janus Chat is switched off for this gateway by its startup configuration (JANUS_CHAT_PREVIEW). These settings
                apply once it is on.
              </div>
            ) : null}
            {tab === 'general' ? <GeneralTab doc={d} /> : null}
            {tab === 'models' ? <ModelsTab doc={d} /> : null}
            {tab === 'tools' ? <ToolsTab doc={d} /> : null}
            {tab === 'access' ? <AccessTab doc={d} /> : null}
          </>
        )}
      </AsyncSection>
    </div>
  );
}

export function useSaveChat() {
  const queryClient = useQueryClient();
  const toast = useToast();
  return useMutation({
    mutationFn: (next: ChatAdminDoc) => api.put<ChatAdminDoc>(URL, next),
    onSuccess: (saved) => {
      queryClient.setQueryData(CHAT_ADMIN_KEY, saved);
      void queryClient.invalidateQueries({ queryKey: ['chat'] });
      void queryClient.invalidateQueries({ queryKey: ['me'] });
      toast('Chat settings saved.');
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });
}

/** Every model granted to anyone: what the default and curation lists offer. */
export function useChatModelNames() {
  const models = useQuery({
    queryKey: ['admin', 'models', 'for-grant-names'],
    queryFn: () => api.get<{ models: Model[] }>('/api/v1/admin/models'),
    staleTime: 60_000,
  });
  const managed = useQuery({
    queryKey: ['admin', 'managed-models', 'for-grants'],
    queryFn: () => api.get<{ managed_models: ManagedModel[] }>('/api/v1/admin/managed-models'),
  });
  const grants = useQuery({
    queryKey: ['admin', 'grants'],
    queryFn: () => api.get<{ grants: Grant[] }>('/api/v1/admin/grants'),
  });
  const rows = useMemo(() => {
    const granted = new Set((grants.data?.grants ?? []).map((g) => g.model_id));
    const labels = new Map<string, Grant[]>();
    for (const g of grants.data?.grants ?? []) {
      if (!g.chat_name && !g.chat_description) continue;
      labels.set(g.model_id, [...(labels.get(g.model_id) ?? []), g]);
    }
    const out: {
      id: string;
      name: string;
      kind: 'model' | 'managed';
      upstream?: string;
      chat: boolean;
      /** video / music: models create_video / create_music can use;
       * stt: speech-to-text (transcribes attached sound). */
      media?: 'video' | 'music' | 'stt';
      labels: Grant[];
    }[] = [];
    for (const m of models.data?.models ?? []) {
      if (!granted.has(m.id) || m.classifier_role || m.status !== 'enabled') continue;
      const mods = m.modalities ?? [];
      out.push({
        id: m.id,
        name: publicModelName(m),
        kind: 'model',
        upstream: m.upstream_name,
        chat: mods.includes('chat'),
        media: mods.includes('video')
          ? 'video'
          : mods.includes('stt') && !/realtime|live|diarize/i.test(m.name)
            ? 'stt'
            : /^(.*\/)?lyria/i.test(m.name) && !/realtime/i.test(m.name)
              ? 'music'
              : undefined,
        labels: labels.get(m.id) ?? [],
      });
    }
    for (const mm of managed.data?.managed_models ?? []) {
      if (!granted.has(mm.id)) continue;
      out.push({ id: mm.id, name: mm.name, kind: 'managed', chat: true, labels: labels.get(mm.id) ?? [] });
    }
    return out.sort((a, b) => a.name.localeCompare(b.name));
  }, [models.data, managed.data, grants.data]);
  return { rows, loading: models.isLoading || grants.isLoading || managed.isLoading };
}

function GeneralTab({ doc }: { doc: ChatAdminDoc }): ReactNode {
  const save = useSaveChat();
  const [draft, setDraft] = useState(doc);
  useEffect(() => setDraft(doc), [doc]);
  const { rows } = useChatModelNames();
  const chatModels = rows.filter((r) => r.chat && !doc.models[r.name]?.hidden);
  const set = <K extends keyof ChatAdminDoc>(key: K, value: ChatAdminDoc[K]) => setDraft((d) => ({ ...d, [key]: value }));
  const prompts = [...draft.starter_prompts, ''];
  return (
    <form
      className="stack"
      style={{ gap: 'var(--janus-space-5)' }}
      onSubmit={(e) => {
        e.preventDefault();
        save.mutate(draft);
      }}
    >
      <section className="card stack" aria-labelledby="chat-general-title">
        <h2 id="chat-general-title" style={{ margin: 0 }}>
          Chat for this instance
        </h2>
        <label className="switch">
          <input
            type="checkbox"
            checked={draft.enabled}
            onChange={(e) => set('enabled', e.target.checked)}
            aria-label="Janus Chat is on"
          />
          <span>
            <strong>Janus Chat is {draft.enabled ? 'on' : 'off'}</strong>
            <span className="small muted" style={{ display: 'block' }}>
              Off hides Chat from everyone and refuses new messages. Nothing is deleted; turning it back on restores every
              conversation.
            </span>
          </span>
        </label>
        <Field label="Default model" hint="What a person starts with until they pick their own.">
          <select
            className="select"
            aria-label="Default model"
            value={draft.default_model}
            onChange={(e) => set('default_model', e.target.value)}
          >
            <option value="">First available model</option>
            {chatModels.map((m) => (
              <option key={m.id} value={m.name}>
                {m.name}
                {m.upstream ? ` · ${m.upstream}` : ''}
              </option>
            ))}
          </select>
        </Field>
        <Field
          label="Instructions for every chat"
          hint="Sent first in every conversation, before each person's own instructions. Up to 8,000 characters."
        >
          <textarea
            className="textarea"
            aria-label="Instructions for every chat"
            rows={4}
            maxLength={8000}
            value={draft.org_instructions}
            onChange={(e) => set('org_instructions', e.target.value)}
            placeholder="For example: You are the assistant for Acme Corp. Answer in American English."
          />
        </Field>
        <fieldset className="stack" style={{ border: 0, padding: 0, margin: 0, gap: 'var(--janus-space-2)' }}>
          <legend className="field-label">Suggestions on the empty chat screen</legend>
          {prompts.slice(0, 8).map((p, i) => (
            <input
              key={i}
              className="input"
              aria-label={`Suggestion ${i + 1}`}
              value={p}
              maxLength={200}
              placeholder={i === 0 ? 'For example: Summarize this document' : 'Add another (optional)'}
              onChange={(e) => {
                const next = [...draft.starter_prompts];
                next[i] = e.target.value;
                set(
                  'starter_prompts',
                  next.filter((x, j) => x.trim() !== '' || j < next.length - 1),
                );
              }}
            />
          ))}
        </fieldset>
      </section>

      <section className="card stack" aria-labelledby="chat-features-title">
        <h2 id="chat-features-title" style={{ margin: 0 }}>
          Features
        </h2>
        <ToggleRow
          label="Share links"
          hint="People can share a read-only copy of a chat with others signed in to this instance."
          checked={draft.shares_enabled}
          onChange={(v) => set('shares_enabled', v)}
        />
        <ToggleRow
          label="Export"
          hint="People can download their chats as Markdown or JSON."
          checked={draft.export_enabled}
          onChange={(v) => set('export_enabled', v)}
        />
        <ToggleRow
          label="Personal plans in Chat"
          hint="People can chat with their own connected vendor plans (ChatGPT, Grok…). Those are paid by their owner, show $0 here, and still count in usage and reports."
          checked={draft.personal_plans}
          onChange={(v) => set('personal_plans', v)}
        />
        <Field label="Keep temporary chats for" hint="Temporary chats are deleted once idle this long.">
          <select
            className="select"
            aria-label="Keep temporary chats for"
            value={draft.temporary_ttl_hours}
            onChange={(e) => set('temporary_ttl_hours', Number(e.target.value))}
          >
            {[1, 6, 24, 72, 168, 720].map((h) => (
              <option key={h} value={h}>
                {h < 24 ? `${h} hour${h === 1 ? '' : 's'}` : `${h / 24} day${h === 24 ? '' : 's'}`}
              </option>
            ))}
          </select>
        </Field>
      </section>
      <SaveBar
        doc={doc}
        busy={save.isPending}
        dirty={JSON.stringify(draft) !== JSON.stringify(doc)}
        onReset={() => setDraft(doc)}
      />
    </form>
  );
}

export function toggleOff(list: string[] | undefined, id: string, on: boolean): string[] {
  const cur = list ?? [];
  return on ? cur.filter((x) => x !== id) : [...new Set([...cur, id])];
}

export function ToggleRow({
  label,
  hint,
  checked,
  onChange,
  disabled = false,
}: {
  label: string;
  hint: string;
  checked: boolean;
  onChange: (v: boolean) => void;
  disabled?: boolean;
}): ReactNode {
  return (
    <label
      className="switch"
      aria-disabled={disabled || undefined}
      style={disabled ? { opacity: 0.6, cursor: 'not-allowed' } : undefined}
    >
      <input
        type="checkbox"
        checked={checked}
        disabled={disabled}
        onChange={(e) => onChange(e.target.checked)}
        aria-label={label}
      />
      <span>
        <strong>{label}</strong>
        <span className="small muted" style={{ display: 'block' }}>
          {hint}
        </span>
      </span>
    </label>
  );
}

export function SaveBar({
  doc,
  busy,
  dirty,
  onReset,
}: {
  doc: ChatAdminDoc;
  busy: boolean;
  dirty: boolean;
  onReset: () => void;
}): ReactNode {
  return (
    <div className="row" style={{ gap: 'var(--janus-space-2)', alignItems: 'center' }}>
      <button type="submit" className="btn btn-primary" disabled={busy || !dirty}>
        {busy ? 'Saving…' : 'Save'}
      </button>
      <button type="button" className="btn btn-ghost" disabled={busy || !dirty} onClick={onReset}>
        Discard changes
      </button>
      {doc.updated_at && !doc.updated_at.startsWith('0001') ? (
        <span className="small muted">Last saved {formatDateTime(doc.updated_at)}</span>
      ) : null}
    </div>
  );
}

function ModelsTab({ doc }: { doc: ChatAdminDoc }): ReactNode {
  const save = useSaveChat();
  const [draft, setDraft] = useState(doc.models);
  useEffect(() => setDraft(doc.models), [doc.models]);
  const { rows, loading } = useChatModelNames();
  const [filter, setFilter] = useState('');
  const shown = rows.filter((r) => !filter || r.name.toLowerCase().includes(filter.toLowerCase()));
  const update = (name: string, patch: Partial<ChatModelSetting>) =>
    setDraft((d) => {
      const next = { ...d, [name]: { ...d[name], ...patch } };
      return next;
    });
  const dirty = JSON.stringify(draft) !== JSON.stringify(doc.models);
  return (
    <form
      className="stack"
      style={{ gap: 'var(--janus-space-4)' }}
      onSubmit={(e) => {
        e.preventDefault();
        save.mutate({ ...doc, models: draft });
      }}
    >
      <p className="small secondary" style={{ margin: 0 }}>
        Every model granted to someone, and how it appears in the Chat picker. People only ever see the models they are granted.
        The friendly <strong>chat name</strong> and <strong>description</strong> are set per grant on{' '}
        <Link to="/admin/grants">Grants</Link>, so a model can read differently for different teams.
      </p>
      <input
        className="input"
        aria-label="Filter models"
        placeholder="Filter models"
        value={filter}
        onChange={(e) => setFilter(e.target.value)}
        style={{ maxWidth: 320 }}
      />
      <div className="table-scroll">
        <table className="data data-compact" aria-label="Chat models">
          <thead>
            <tr>
              <th scope="col">Model</th>
              <th scope="col">Chat name (from grants)</th>
              <th scope="col">In Chat</th>
              <th scope="col">Category</th>
              <th scope="col">Order</th>
              <th scope="col">Recommended</th>
              <th scope="col">Sees images</th>
              <th scope="col">Hears sound</th>
              <th scope="col">Sees video</th>
            </tr>
          </thead>
          <tbody>
            {loading ? (
              <tr>
                <td colSpan={9} className="muted">
                  Loading…
                </td>
              </tr>
            ) : shown.length === 0 ? (
              <tr>
                <td colSpan={9} className="muted">
                  No granted models{filter ? ' match' : ' yet'}.
                </td>
              </tr>
            ) : (
              shown.map((r) => {
                const set = draft[r.name] ?? {};
                return (
                  <tr key={r.id}>
                    <td>
                      <div style={{ fontWeight: 600 }}>{r.name}</div>
                      <div className="small muted">
                        {r.kind === 'managed' ? 'Managed model' : r.upstream}
                        {!r.chat ? ' · not a chat model' : ''}
                      </div>
                    </td>
                    <td className="small">
                      {r.labels.length === 0 ? (
                        <span className="muted">—</span>
                      ) : (
                        r.labels.map((g) => (
                          <div key={g.id}>
                            {g.chat_name || <span className="muted">(no name)</span>}
                            <span className="muted"> · {g.grantee_name || g.grantee_type.replace('_', ' ')}</span>
                          </div>
                        ))
                      )}
                    </td>
                    <td>
                      <label className="switch" style={{ margin: 0 }}>
                        <input
                          type="checkbox"
                          checked={!set.hidden}
                          aria-label={`Show ${r.name} in Chat`}
                          onChange={(e) => update(r.name, { hidden: !e.target.checked })}
                        />
                      </label>
                    </td>
                    <td>
                      <select
                        className="select"
                        style={{ height: 32, minWidth: 150 }}
                        aria-label={`Category for ${r.name}`}
                        value={set.category ?? ''}
                        disabled={set.hidden}
                        onChange={(e) => update(r.name, { category: e.target.value || undefined })}
                      >
                        <option value="">—</option>
                        {Object.entries(CATEGORY_LABELS).map(([id, label]) => (
                          <option key={id} value={id}>
                            {label}
                          </option>
                        ))}
                      </select>
                    </td>
                    <td>
                      <input
                        className="input"
                        type="number"
                        min={0}
                        max={999}
                        style={{ width: 72, height: 32 }}
                        aria-label={`Order for ${r.name}`}
                        value={set.order ?? ''}
                        disabled={set.hidden}
                        onChange={(e) =>
                          update(r.name, { order: e.target.value === '' ? undefined : Math.max(0, Number(e.target.value)) })
                        }
                      />
                    </td>
                    <td>
                      <input
                        type="checkbox"
                        aria-label={`Recommend ${r.name}`}
                        checked={!!set.recommended}
                        disabled={set.hidden}
                        onChange={(e) => update(r.name, { recommended: e.target.checked || undefined })}
                      />
                    </td>
                    <td>
                      <select
                        className="select"
                        style={{ height: 32, minWidth: 110 }}
                        aria-label={`Images for ${r.name}`}
                        value={set.images ?? ''}
                        disabled={set.hidden || !r.chat}
                        onChange={(e) => update(r.name, { images: (e.target.value as 'yes' | 'no') || undefined })}
                      >
                        <option value="">Automatic</option>
                        <option value="yes">Yes</option>
                        <option value="no">No</option>
                      </select>
                    </td>
                    <td>
                      <select
                        className="select"
                        style={{ height: 32, minWidth: 110 }}
                        aria-label={`Sound for ${r.name}`}
                        value={set.audio ?? ''}
                        disabled={set.hidden || !r.chat}
                        onChange={(e) => update(r.name, { audio: (e.target.value as 'yes' | 'no') || undefined })}
                      >
                        <option value="">Automatic</option>
                        <option value="yes">Yes</option>
                        <option value="no">No</option>
                      </select>
                    </td>
                    <td>
                      <select
                        className="select"
                        style={{ height: 32, minWidth: 110 }}
                        aria-label={`Video for ${r.name}`}
                        value={set.video ?? ''}
                        disabled={set.hidden || !r.chat}
                        onChange={(e) => update(r.name, { video: (e.target.value as 'yes' | 'no') || undefined })}
                      >
                        <option value="">Automatic</option>
                        <option value="yes">Yes</option>
                        <option value="no">No</option>
                      </select>
                    </td>
                  </tr>
                );
              })
            )}
          </tbody>
        </table>
      </div>
      <p className="small muted" style={{ margin: 0 }}>
        Categories become the groups in the Chat picker; lower order comes first. Hidden models stay usable through the API — this
        only curates Chat. <strong>Sees images</strong>: Automatic guesses from the model&apos;s name — set Yes for a self-hosted
        vision model (for example one served by vLLM under your own name) so attached images reach it.
      </p>
      <SaveBar doc={doc} busy={save.isPending} dirty={dirty} onReset={() => setDraft(doc.models)} />
    </form>
  );
}

function AccessTab({ doc }: { doc: ChatAdminDoc }): ReactNode {
  const save = useSaveChat();
  const { me } = useSession();
  const [draft, setDraft] = useState(doc.access);
  useEffect(() => setDraft(doc.access), [doc.access]);
  const licensed = doc.access_licensed;
  const dirty = JSON.stringify(draft) !== JSON.stringify(doc.access);
  const empty = draft.groups.length + draft.teams.length + draft.users.length === 0;
  return (
    <form
      className="stack"
      style={{ gap: 'var(--janus-space-4)' }}
      onSubmit={(e) => {
        e.preventDefault();
        save.mutate({ ...doc, access: draft });
      }}
    >
      <section className="card stack" aria-labelledby="chat-access-title">
        <div className="row-between">
          <h2 id="chat-access-title" style={{ margin: 0 }}>
            Who can use Chat
          </h2>
          <Badge tone={licensed ? 'primary' : 'warning'}>Business</Badge>
        </div>
        {!licensed ? (
          <div className="banner banner-info" role="note">
            Limiting Chat to chosen groups, teams and people is part of Janus Business
            {me?.license?.edition ? ` (this instance runs ${me.license.edition})` : ''}. On Community, everyone with an account
            can use Chat; you can still turn Chat off for the whole instance on the General tab.{' '}
            <a href="https://janusedge.com/pricing" target="_blank" rel="noreferrer">
              See plans
            </a>
          </div>
        ) : null}
        {!licensed && doc.access.mode === 'restricted' ? (
          <div className="banner banner-warning row-between" role="note">
            <span>
              A restriction saved under the earlier license still applies: only the groups, teams and people chosen below can use
              Chat. You can lift it; changing it needs Janus Business.
            </span>
            <button
              type="button"
              className="btn btn-sm"
              disabled={save.isPending}
              onClick={() => save.mutate({ ...doc, access: { ...doc.access, mode: 'everyone' } })}
            >
              Let everyone use Chat
            </button>
          </div>
        ) : null}
        <fieldset className="stack" disabled={!licensed} style={{ border: 0, padding: 0, margin: 0 }}>
          <label className="radio">
            <input
              type="radio"
              name="chat-access"
              checked={draft.mode === 'everyone'}
              onChange={() => setDraft({ ...draft, mode: 'everyone' })}
            />
            <span>
              <strong>Everyone</strong> <span className="small muted">— every signed-in person</span>
            </span>
          </label>
          <label className="radio">
            <input
              type="radio"
              name="chat-access"
              checked={draft.mode === 'restricted'}
              onChange={() => setDraft({ ...draft, mode: 'restricted' })}
            />
            <span>
              <strong>Only these groups, teams and people</strong>{' '}
              <span className="small muted">— administrators always keep access</span>
            </span>
          </label>
          {draft.mode === 'restricted' ? (
            <div className="stack" style={{ paddingLeft: 'var(--janus-space-5)' }}>
              <LookupInput
                label="Groups"
                value={draft.groups}
                onChange={(groups) => setDraft({ ...draft, groups })}
                search={groupLookup.search}
                resolve={groupLookup.resolve}
                placeholder="Search groups"
              />
              <LookupInput
                label="Teams"
                value={draft.teams}
                onChange={(teams) => setDraft({ ...draft, teams })}
                search={teamLookup.search}
                resolve={teamLookup.resolve}
                placeholder="Search teams"
              />
              <LookupInput
                label="People"
                value={draft.users}
                onChange={(users) => setDraft({ ...draft, users })}
                search={userLookup.search}
                resolve={userLookup.resolve}
                placeholder="Search people"
              />
              {empty ? (
                <p className="small" style={{ color: 'var(--janus-color-warning-fg)', margin: 0 }}>
                  Choose at least one group, team or person.
                </p>
              ) : null}
              <Field label="Message for people without access" hint="Shown instead of Chat.">
                <input
                  className="input"
                  aria-label="Message for people without access"
                  maxLength={300}
                  value={draft.message}
                  placeholder="Janus Chat isn't available to your account. Ask an administrator for access."
                  onChange={(e) => setDraft({ ...draft, message: e.target.value })}
                />
              </Field>
            </div>
          ) : null}
        </fieldset>
      </section>
      {licensed ? (
        <SaveBar
          doc={doc}
          busy={save.isPending}
          dirty={dirty && !(draft.mode === 'restricted' && empty)}
          onReset={() => setDraft(doc.access)}
        />
      ) : null}
    </form>
  );
}
