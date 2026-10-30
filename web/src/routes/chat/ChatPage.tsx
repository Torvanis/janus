/**
 * Janus Chat.
 *
 * Conversations, a server-owned run per reply (survives refresh), resumable
 * SSE, Stop, billing that follows "Working under" (or the caller's own plan
 * for my/<provider>/<model>), per-message usage straight from the request
 * log. Replies and reasoning render as markdown (./Markdown.tsx); user
 * messages stay plain text.
 */
import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { api, ApiError } from '../../lib/api';
import { useSession } from '../../app/session';
import { publicModelName, type ManagedModel, type Model } from '../../lib/types';
import { Icon } from '../../components/IconButton';
import { ChatMarkdown } from './Markdown';
import { ChatSearch, ContextDivider, contextDivider } from './ChatSearch';
import {
  PromptFillDialog,
  PromptsDrawer,
  ShareDrawer,
  SlashMenu,
  matchPrompts,
  promptVariables,
  slashQuery,
  usePrompts,
  type ChatPrompt,
} from './ShareAndPrompts';
import { QuotaLine, SendEstimate, estimateInputTokens, pressingQuota, useChatQuota } from './Estimate';
import { ChatSettingsDrawer, ConversationMenu, EFFORTS, RenameField, useChatSettings } from './ChatSettings';
import { ATTACH_ACCEPT, AttachmentChips, MessageAttachments, useComposerAttachments, type ChatAttachment } from './Attachments';
import { ModelPicker, arrangeOptions, useChatCatalog } from './ModelPicker';
import { SearchActivity, SourcesList } from './ChatTools';
import { MadeFiles, MemoryChip } from './ChatExtras';
import { ChatTray, ModeChips, TrayButton } from './ChatTray';
import './chat.css';

export interface ChatConversation {
  id: string;
  team_id: string;
  title: string;
  model: string;
  current_leaf_id: string;
  created_at: string;
  updated_at: string;
  pinned?: boolean;
  /** Never listed or searchable; removed once idle for a day. */
  temporary?: boolean;
  /** This conversation's own instructions (after the user's). */
  instructions?: string;
  /** '' = model default, low, medium, high. */
  reasoning_effort?: string;
  /** Tools turned off for this chat (tools are on by default). */
  tools_off?: string[];
  /** The model Create images uses here ('' = automatic). */
  image_model?: string;
}

/** Request-log context for a reply (only when something noteworthy happened). */
export interface ChatReplyNotes {
  requested_model?: string;
  route_reason?: string;
  fallback_reason?: string;
  reasoning_adjustment?: string;
  secgw_action?: string;
  secgw_violations?: number;
}

export interface ChatMessage {
  id: string;
  parent_id: string;
  role: 'user' | 'assistant';
  content: string;
  reasoning?: string;
  model?: string;
  served_model?: string;
  team_id: string;
  request_id?: string;
  status: 'complete' | 'streaming' | 'stopped' | 'error' | 'interrupted' | 'blocked';
  error_code?: string;
  error_message?: string;
  /** Oldest history messages left out of this reply's request (context window). */
  context_dropped?: number;
  tokens_in: number;
  tokens_out: number;
  tokens_cached: number;
  cost_usd: number;
  tokens_out_per_second: number;
  latency_ms: number;
  ttfb_ms: number;
  created_at: string;
  /** Files sent with a user message. */
  attachments?: ChatAttachment[];
  notes?: ChatReplyNotes;
  /** What the provider searched while answering (search tools). */
  activity?: ChatActivity[];
  /** Pages the reply cited. */
  sources?: ChatSource[];
  /** Shared by replies made side by side (Compare). */
  compare_group?: string;
}

export interface ChatActivity {
  type: string;
  status: 'searching' | 'done';
  query?: string;
  sources?: string[];
  /** Run code: the program and its output (run by the provider). */
  code?: string;
  output?: string;
  /** A file the reply made (create_file), in bytes. */
  size?: number;
}

export interface ChatSource {
  url: string;
  title?: string;
  host?: string;
}

/** A model from the caller's own connected plan (GET /api/v1/models personal_models). */
interface PersonalModel {
  id: string;
  owned_by: string;
  /** modalities: what the plan model does (chat, image, video…). */
  janus?: { native_name?: string; modalities?: string[] };
}

interface ModelOption {
  value: string;
  label: string;
  ctx: number;
  inRate: number;
  outRate: number;
  personal?: boolean;
  owner?: string;
  /** Makes images from the message instead of a chat reply. */
  image?: boolean;
}

interface SendResponse {
  run_id: string;
  user_message: ChatMessage;
  assistant_message: ChatMessage;
  conversation: ChatConversation;
  /** e.g. images left out because the model does not accept them. */
  warnings?: string[];
  /** Compare: the other models' replies to the same message. */
  compare?: { run_id: string; assistant_message: ChatMessage }[];
}

// chatTotalCost is what the whole chat cost: every reply on every branch,
// Compare answers included (each reply's cost already sums all its calls).
export function chatTotalCost(messages: ChatMessage[] | undefined): number {
  return (messages ?? []).reduce((sum, m) => sum + (m.cost_usd || 0), 0);
}

/** Active branch: walk parent links from the current leaf back to the root. */
export function activeBranch(messages: ChatMessage[], leafId: string): ChatMessage[] {
  const byId = new Map(messages.map((m) => [m.id, m]));
  const out: ChatMessage[] = [];
  let cur = leafId ? byId.get(leafId) : undefined;
  while (cur) {
    out.unshift(cur);
    cur = cur.parent_id ? byId.get(cur.parent_id) : undefined;
  }
  return out;
}

/** Siblings share a parent and role; used for the ‹ 1/2 › branch switcher. */
export function siblingsOf(messages: ChatMessage[], m: ChatMessage): ChatMessage[] {
  return messages.filter((x) => x.parent_id === m.parent_id && x.role === m.role);
}

/** How close to the end (px) still counts as "at the bottom" for following. */
const CHAT_BOTTOM_SLACK = 48;

/** Deepest leaf under a message, following the newest child each step. */
export function newestLeaf(messages: ChatMessage[], id: string): string {
  let cur = id;
  for (;;) {
    const kids = messages.filter((x) => x.parent_id === cur);
    const last = kids[kids.length - 1];
    if (!last) return cur;
    cur = last.id;
  }
}

/**
 * Follow a run's event stream, resuming from the text already shown.
 * Returns when the run finishes or the signal aborts; reconnects on drops.
 */
async function followRun(
  runId: string,
  start: { content: number; reasoning: number },
  onDelta: (d: { content?: string; reasoning?: string; activity?: ChatActivity[]; sources?: ChatSource[] }) => void,
  onDone: (m: ChatMessage) => void,
  signal: AbortSignal,
): Promise<void> {
  const enc = new TextEncoder();
  let contentFrom = start.content;
  let reasoningFrom = start.reasoning;
  for (let attempt = 0; attempt < 50 && !signal.aborted; attempt++) {
    try {
      const res = await fetch(`/api/v1/chat/runs/${runId}/events?content_from=${contentFrom}&reasoning_from=${reasoningFrom}`, {
        credentials: 'same-origin',
        signal,
        headers: { Accept: 'text/event-stream' },
      });
      if (!res.ok || !res.body) throw new Error(`events ${res.status}`);
      const reader = res.body.getReader();
      const dec = new TextDecoder();
      let buf = '';
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let idx: number;
        while ((idx = buf.indexOf('\n\n')) >= 0) {
          const block = buf.slice(0, idx);
          buf = buf.slice(idx + 2);
          let event = 'message';
          let data = '';
          for (const line of block.split('\n')) {
            if (line.startsWith('event: ')) event = line.slice(7);
            else if (line.startsWith('data: ')) data += line.slice(6);
          }
          if (!data) continue;
          const payload = JSON.parse(data) as Record<string, unknown>;
          if (event === 'delta') {
            const d: { content?: string; reasoning?: string } = {};
            if (typeof payload.content === 'string') {
              d.content = payload.content;
              contentFrom = (payload.content_to as number) ?? contentFrom + enc.encode(payload.content).length;
            }
            if (typeof payload.reasoning === 'string') {
              d.reasoning = payload.reasoning;
              reasoningFrom = (payload.reasoning_to as number) ?? reasoningFrom + enc.encode(payload.reasoning).length;
            }
            onDelta(d);
          } else if (event === 'activity') {
            onDelta({
              activity: (payload.activity as ChatActivity[] | undefined) ?? [],
              sources: (payload.sources as ChatSource[] | undefined) ?? [],
            });
          } else if (event === 'done') {
            onDone(payload as unknown as ChatMessage);
            return;
          }
        }
      }
    } catch (err) {
      if (signal.aborted) return;
      void err;
    }
    await new Promise((r) => setTimeout(r, Math.min(500 * (attempt + 1), 3000)));
  }
}

/** Plain text of the rendered reply that owns a footer button. */
function renderedText(button: HTMLElement): string {
  const body = button.closest('.chat-msg')?.querySelector<HTMLElement>('.chat-body');
  if (!body) return '';
  // innerText follows layout, so hide code-block toolbars while reading it
  // (chat.css: [data-copying] .chat-code-header) and they are not copied.
  // jsdom has no layout; textContent is the fallback there.
  body.setAttribute('data-copying', '');
  try {
    return (body.innerText || body.textContent || '').trim();
  } finally {
    body.removeAttribute('data-copying');
  }
}

function byteLen(s: string): number {
  return new TextEncoder().encode(s).length;
}

function formatCost(usd: number): string {
  if (usd === 0) return '$0';
  if (usd < 0.01) return `$${usd.toFixed(5)}`;
  return `$${usd.toFixed(4)}`;
}

/** "1 hour", "6 hours", "a day", "3 days" for the temporary-chat lifetime. */
export function idleLabel(hours: number): string {
  if (hours === 24) return 'a day';
  if (hours % 24 === 0) return `${hours / 24} days`;
  return hours === 1 ? '1 hour' : `${hours} hours`;
}

export default function ChatPage(): ReactNode {
  const { id } = useParams();
  const navigate = useNavigate();
  const qc = useQueryClient();
  const { me } = useSession();
  const localOnly = me?.local_only === true;

  const list = useQuery({
    queryKey: ['chat', 'conversations'],
    queryFn: () => api.get<{ conversations: ChatConversation[] }>('/api/v1/chat/conversations'),
  });

  const [conv, setConv] = useState<ChatConversation | null>(null);
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [draft, setDraftState] = useState('');
  // Drafts survive switching chats and reloads (per conversation, this browser).
  const draftKey = `janus.chat.draft.${id ?? 'new'}`;
  const setDraft = useCallback(
    (v: string) => {
      setDraftState(v);
      try {
        if (v) sessionStorage.setItem(draftKey, v);
        else sessionStorage.removeItem(draftKey);
      } catch {
        /* storage unavailable: drafts just are not kept */
      }
    },
    [draftKey],
  );
  useEffect(() => {
    try {
      setDraftState(sessionStorage.getItem(draftKey) ?? '');
    } catch {
      setDraftState('');
    }
  }, [draftKey]);
  const settings = useChatSettings();
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [trayOpen, setTrayOpen] = useState(false);
  const [shareOpen, setShareOpen] = useState(false);
  const [promptsOpen, setPromptsOpen] = useState(false);
  const [promptSeed, setPromptSeed] = useState<string | undefined>(undefined);
  const [filling, setFilling] = useState<ChatPrompt | null>(null);
  const [slashActive, setSlashActive] = useState(0);
  const prompts = usePrompts();
  const [renaming, setRenaming] = useState<string | null>(null);
  // Settings chosen before the first message of a new chat.
  const [pending, setPending] = useState<{
    instructions: string;
    effort: string;
    temporary: boolean;
    toolsOff?: string[];
    imageModel?: string;
  }>({
    instructions: '',
    effort: '',
    temporary: false,
  });
  const [model, setModel] = useState('');
  // Who pays is the "Working under" selection at the top of the app; the
  // model list below is scoped to it by /api/v1/models (refetched when the
  // selector changes, which invalidates every query).
  const teamId = me?.active_team_id ?? '';
  const [runId, setRunId] = useState<string | null>(null);
  // Compare: replies of the other models still streaming (run id → message id).
  const [sideRuns, setSideRuns] = useState<Record<string, string>>({});
  const sideAborts = useRef<Map<string, AbortController>>(new Map());
  const [research, setResearch] = useState(false);
  const [compare, setCompare] = useState<string[]>([]);
  // /api/v1/models is scoped to the "Working under" selection, which is
  // also who pays for the message; the selector invalidates every query.
  const catalog = useQuery({
    queryKey: ['models', 'chat-picker', teamId],
    queryFn: () =>
      api.get<{ models: Model[]; managed_models: ManagedModel[]; personal_models?: PersonalModel[] }>('/api/v1/models'),
    staleTime: 60_000,
  });

  const modelOptions = useMemo(() => {
    const out: ModelOption[] = [];
    for (const m of catalog.data?.managed_models ?? []) {
      if (m.status === 'enabled') out.push({ value: m.name, label: `${m.name} (managed)`, ctx: 0, inRate: 0, outRate: 0 });
    }
    for (const m of catalog.data?.models ?? []) {
      const mods = m.modalities ?? [];
      // Chat models, and image models (image without chat: a vision chat
      // model lists both and stays a chat model).
      const image = mods.includes('image') && !mods.includes('chat');
      if (m.status !== 'enabled' || !(mods.includes('chat') || image)) continue;
      out.push({
        value: publicModelName(m),
        label: publicModelName(m),
        ctx: image ? 0 : m.context_window,
        inRate: m.rate_in_nanousd,
        outRate: m.rate_out_nanousd,
        image,
      });
    }
    // The caller's own connected plans: paid by their plan, never by the
    // "Working under" selection, so they are listed whatever it says.
    for (const m of catalog.data?.personal_models ?? []) {
      // A plan's image / video / music models are not chat models: they
      // are picked in the tray (Your models), never as the chat model.
      const mods = m.janus?.modalities;
      if (mods && mods.length > 0 && !mods.includes('chat')) continue;
      const native = m.janus?.native_name || m.id.split('/').slice(2).join('/');
      // "xAI Grok (SuperGrok / X Premium+)" → "xAI Grok": the plan tier is noise here.
      const owner = m.owned_by.replace(/\s*\(.*\)\s*$/, '') || m.owned_by;
      out.push({
        value: m.id,
        label: `${native} · ${owner}`,
        ctx: 0,
        inRate: 0,
        outRate: 0,
        personal: true,
        owner,
      });
    }
    return out;
  }, [catalog.data]);
  // Administration → Chat: hidden models, categories, chat names, default.
  const chatCatalog = useChatCatalog(teamId);
  const pickable = useMemo(() => {
    const hidden = new Set(Array.isArray(chatCatalog.data?.hidden) ? chatCatalog.data.hidden : []);
    const plansOff = chatCatalog.data?.features?.personal_plans === false;
    return modelOptions.filter((o) => !hidden.has(o.value) && !(plansOff && o.personal));
  }, [modelOptions, chatCatalog.data]);
  const [editing, setEditing] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [listOpen, setListOpen] = useState(false);
  const [dragging, setDragging] = useState(false);
  const files = useComposerAttachments();
  const fileInputRef = useRef<HTMLInputElement>(null);
  const abortRef = useRef<AbortController | null>(null);
  const threadRef = useRef<HTMLDivElement>(null);
  const composerRef = useRef<HTMLTextAreaElement>(null);
  const layoutRef = useRef<HTMLDivElement>(null);

  // Fit the chat to the viewport below whatever the shell put above it, so
  // only the thread scrolls and the composer is always visible.
  useEffect(() => {
    const el = layoutRef.current;
    if (!el) return;
    const fit = () => el.style.setProperty('--chat-top', `${Math.max(0, el.getBoundingClientRect().top + window.scrollY)}px`);
    fit();
    window.addEventListener('resize', fit);
    return () => window.removeEventListener('resize', fit);
  }, []);

  useEffect(() => {
    if (!catalog.data) return;
    if (pickable.some((o) => o.value === model)) return;
    // A new chat starts on the user's default model when it is offered,
    // then the organization's (Administration → Chat), then the first
    // model in the picker's order.
    const offered = (v?: string) => !!v && pickable.some((o) => o.value === v);
    const preferred = settings.data?.default_model;
    const orgDefault = chatCatalog.data?.default_model;
    const first = arrangeOptions(pickable, chatCatalog.data, '').flatMap((g) => g.options)[0]?.value;
    setModel(offered(preferred) ? preferred! : offered(orgDefault) ? orgDefault! : (first ?? ''));
  }, [catalog.data, model, pickable, settings.data, chatCatalog.data]);

  const attach = useCallback(
    (rid: string, msg: ChatMessage) => {
      abortRef.current?.abort();
      const ac = new AbortController();
      abortRef.current = ac;
      setRunId(rid);
      void followRun(
        rid,
        { content: byteLen(msg.content), reasoning: byteLen(msg.reasoning ?? '') },
        (d) =>
          setMessages((prev) =>
            prev.map((m) =>
              m.id === msg.id
                ? {
                    ...m,
                    content: m.content + (d.content ?? ''),
                    reasoning: (m.reasoning ?? '') + (d.reasoning ?? ''),
                    ...(d.activity ? { activity: d.activity, sources: d.sources } : {}),
                  }
                : m,
            ),
          ),
        (final) => {
          // Keep a compared reply in its group whatever the server sent.
          setMessages((prev) =>
            prev.map((m) => (m.id === final.id ? { ...final, compare_group: final.compare_group || m.compare_group } : m)),
          );
          setRunId(null);
          void qc.invalidateQueries({ queryKey: ['chat', 'conversations'] });
        },
        ac.signal,
      );
    },
    [qc],
  );

  // A compare reply streams alongside the main one (its own run).
  const attachSide = useCallback((rid: string, msg: ChatMessage) => {
    const ac = new AbortController();
    sideAborts.current.set(rid, ac);
    setSideRuns((r) => ({ ...r, [rid]: msg.id }));
    void followRun(
      rid,
      { content: byteLen(msg.content), reasoning: byteLen(msg.reasoning ?? '') },
      (d) =>
        setMessages((prev) =>
          prev.map((m) =>
            m.id === msg.id
              ? {
                  ...m,
                  content: m.content + (d.content ?? ''),
                  reasoning: (m.reasoning ?? '') + (d.reasoning ?? ''),
                  ...(d.activity ? { activity: d.activity, sources: d.sources } : {}),
                }
              : m,
          ),
        ),
      (final) => {
        setMessages((prev) => prev.map((m) => (m.id === final.id ? { ...final, compare_group: m.compare_group } : m)));
        setSideRuns((r) => {
          const next = { ...r };
          delete next[rid];
          return next;
        });
        sideAborts.current.delete(rid);
      },
      ac.signal,
    );
  }, []);

  // Load the selected conversation; resume any reply still streaming.
  useEffect(() => {
    abortRef.current?.abort();
    for (const ac of sideAborts.current.values()) ac.abort();
    sideAborts.current.clear();
    setSideRuns({});
    setRunId(null);
    setError(null);
    if (!id) {
      setConv(null);
      setMessages([]);
      return;
    }
    let cancelled = false;
    void api
      .get<{ conversation: ChatConversation; messages: ChatMessage[]; runs?: Record<string, string> }>(
        `/api/v1/chat/conversations/${id}`,
      )
      .then((res) => {
        if (cancelled) return;
        setConv(res.conversation);
        setMessages(res.messages);
        if (res.conversation.model) setModel(res.conversation.model);
        const streamingAll = res.messages.filter((m) => m.status === 'streaming');
        const leafStreaming = streamingAll.find((m) => m.id === res.conversation.current_leaf_id) ?? streamingAll[0];
        for (const m of streamingAll) {
          const rid = res.runs?.[m.id];
          if (!rid) continue;
          if (m === leafStreaming) attach(rid, m);
          else attachSide(rid, m);
        }
      })
      .catch((err: unknown) => setError(err instanceof ApiError ? err.message : 'Could not load this chat.'));
    return () => {
      cancelled = true;
    };
  }, [id, attach, attachSide]);

  // Follow the reply only while the reader is at the bottom: scrolling up
  // to read stops it, scrolling back down (or "Jump to latest", or sending)
  // resumes it. The ref is what the follow reads; the state shows the button.
  const followRef = useRef(true);
  const [following, setFollowing] = useState(true);
  const setFollow = useCallback((on: boolean) => {
    followRef.current = on;
    setFollowing(on);
  }, []);
  const toBottom = useCallback(() => {
    const el = threadRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, []);
  const onThreadScroll = useCallback(() => {
    const el = threadRef.current;
    if (!el) return;
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight <= CHAT_BOTTOM_SLACK;
    if (atBottom !== followRef.current) setFollow(atBottom);
  }, [setFollow]);
  // A different chat opens at its end, following.
  useEffect(() => {
    setFollow(true);
    toBottom();
  }, [id, setFollow, toBottom]);
  useEffect(() => {
    if (followRef.current) toBottom();
  }, [messages, toBottom]);
  // Content also grows without a message change (images and code blocks
  // finishing layout): keep the end in view while following.
  useEffect(() => {
    const col = threadRef.current?.firstElementChild;
    if (!col || typeof ResizeObserver === 'undefined') return;
    const ro = new ResizeObserver(() => {
      if (followRef.current) toBottom();
    });
    ro.observe(col);
    return () => ro.disconnect();
  }, [toBottom]);

  const branch = conv ? activeBranch(messages, conv.current_leaf_id) : [];
  const divider = contextDivider(branch);

  async function send(opts: {
    content: string;
    parentId?: string;
    regenerate?: boolean;
    attachmentIds?: string[];
    fromComposer?: boolean;
  }): Promise<void> {
    setError(null);
    setNotice(null);
    // Sending is a request to watch the answer.
    setFollow(true);
    toBottom();
    try {
      let c = conv;
      if (!c) {
        c = await api.post<ChatConversation>('/api/v1/chat/conversations', {
          model,
          temporary: pending.temporary,
          instructions: pending.instructions,
          reasoning_effort: pending.effort,
          ...(pending.toolsOff ? { tools_off: pending.toolsOff } : {}),
          ...(pending.imageModel ? { image_model: pending.imageModel } : {}),
        });
        if (!pending.temporary && settings.data && model !== settings.data.default_model) {
          // The last model you start a chat with becomes your default.
          // Best effort: a failure here never blocks the message.
          void Promise.resolve()
            .then(() => api.put('/api/v1/chat/settings', { default_model: model }))
            .then(() => qc.invalidateQueries({ queryKey: ['chat', 'settings'] }))
            .catch(() => undefined);
        }
      }
      // Compare: only the picked models answer; the first is preferred (the
      // chat goes on from it), so it is the one sent as the model.
      const comparing = !!opts.fromComposer && compare.length > 0;
      if (comparing && compare.length < 2) {
        setError('Pick at least two models to compare, or turn Compare off.');
        return;
      }
      const sendModel = comparing ? compare[0]! : model;
      const body: Record<string, unknown> = { content: opts.content, model: sendModel };
      if (opts.parentId !== undefined) body.parent_id = opts.parentId;
      if (opts.regenerate) body.regenerate = true;
      if (opts.attachmentIds && opts.attachmentIds.length > 0) body.attachment_ids = opts.attachmentIds;
      if (opts.fromComposer && research) body.research = true;
      if (comparing) body.compare_models = compare.slice(1);
      const res = await api.post<SendResponse>(`/api/v1/chat/conversations/${c.id}/messages`, body);
      setConv(res.conversation);
      if (comparing) setModel(sendModel);
      const sides = res.compare ?? [];
      setMessages((prev) => {
        const next = prev.filter((m) => m.id !== res.user_message.id);
        return [...next, res.user_message, res.assistant_message, ...sides.map((x) => x.assistant_message)];
      });
      for (const x of sides) attachSide(x.run_id, x.assistant_message);
      if (opts.fromComposer && research) setResearch(false);
      setDraft('');
      if (opts.fromComposer) files.clear();
      if (res.warnings && res.warnings.length > 0) setNotice(res.warnings.join(' '));
      if (!id || id !== c.id) navigate(`/chat/${c.id}`, { replace: !id });
      attach(res.run_id, res.assistant_message);
      void qc.invalidateQueries({ queryKey: ['chat', 'conversations'] });
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'The message could not be sent.');
    }
  }

  async function stop(): Promise<void> {
    const all = [...(runId ? [runId] : []), ...Object.keys(sideRuns)];
    await Promise.all(all.map((r) => api.post(`/api/v1/chat/runs/${r}/stop`, {}).catch(() => undefined)));
  }

  async function selectBranch(target: ChatMessage): Promise<void> {
    if (!conv) return;
    const leaf = newestLeaf(messages, target.id);
    const updated = await api.patch<ChatConversation>(`/api/v1/chat/conversations/${conv.id}`, { current_leaf_id: leaf });
    setConv(updated);
  }

  // "Prefer this answer": the chat goes on from that reply, with its model.
  async function preferReply(target: ChatMessage): Promise<void> {
    if (!conv) return;
    try {
      const leaf = newestLeaf(messages, target.id);
      const body: Record<string, unknown> = { current_leaf_id: leaf };
      if (target.model && pickable.some((o) => o.value === target.model)) body.model = target.model;
      const updated = await api.patch<ChatConversation>(`/api/v1/chat/conversations/${conv.id}`, body);
      setConv(updated);
      if (body.model) setModel(target.model!);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not switch to that answer.');
    }
  }

  async function remove(cid: string, title: string): Promise<void> {
    if (!window.confirm(`Delete “${title || 'New chat'}”? This cannot be undone.`)) return;
    await api.del(`/api/v1/chat/conversations/${cid}`);
    void qc.invalidateQueries({ queryKey: ['chat', 'conversations'] });
    if (cid === id) navigate('/chat');
  }

  async function patchConversation(cid: string, body: Record<string, unknown>): Promise<void> {
    try {
      const updated = await api.patch<ChatConversation>(`/api/v1/chat/conversations/${cid}`, body);
      if (conv?.id === cid) setConv(updated);
      void qc.invalidateQueries({ queryKey: ['chat', 'conversations'] });
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not save that change.');
    }
  }

  async function saveImageModel(m: string): Promise<void> {
    if (!conv) {
      setPending((p) => ({ ...p, imageModel: m }));
      return;
    }
    setConv({ ...conv, image_model: m });
    try {
      setConv(await api.patch<ChatConversation>(`/api/v1/chat/conversations/${conv.id}`, { image_model: m }));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not save that change.');
    }
  }

  async function saveToolsOff(off: string[]): Promise<void> {
    if (!conv) {
      setPending((p) => ({ ...p, toolsOff: off }));
      return;
    }
    setConv({ ...conv, tools_off: off });
    try {
      setConv(await api.patch<ChatConversation>(`/api/v1/chat/conversations/${conv.id}`, { tools_off: off }));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not save that change.');
    }
  }

  async function saveSettings(v: { instructions: string; effort: string }): Promise<void> {
    if (!conv) {
      setPending((p) => ({ ...p, instructions: v.instructions, effort: v.effort }));
      return;
    }
    const updated = await api.patch<ChatConversation>(`/api/v1/chat/conversations/${conv.id}`, {
      instructions: v.instructions,
      reasoning_effort: v.effort,
    });
    setConv(updated);
  }

  // Shortcuts: Ctrl/Cmd+Shift+O new chat, Esc stops a reply, / focuses the box.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const typing = e.target instanceof HTMLElement && /^(INPUT|TEXTAREA|SELECT)$/.test(e.target.tagName);
      if ((e.ctrlKey || e.metaKey) && e.shiftKey && e.key.toLowerCase() === 'o') {
        e.preventDefault();
        navigate('/chat');
      } else if (e.key === 'Escape' && runId && !settingsOpen && !shareOpen && !promptsOpen && !filling) {
        void api.post(`/api/v1/chat/runs/${runId}/stop`, {});
      } else if (e.key === '/' && !typing) {
        e.preventDefault();
        document.querySelector<HTMLTextAreaElement>('textarea[aria-label=Message]')?.focus();
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [navigate, runId, settingsOpen, shareOpen, promptsOpen, filling]);

  // A new chat starts with fresh pending settings.
  useEffect(() => {
    if (!id) setPending({ instructions: '', effort: '', temporary: false });
  }, [id]);

  const conversationCost = chatTotalCost(messages);
  const lastWithUsage = [...branch].reverse().find((m) => m.role === 'assistant' && m.tokens_in > 0);
  const ctxWindow = modelOptions.find((o) => o.value === model)?.ctx ?? 0;
  const ctxUsed = lastWithUsage ? lastWithUsage.tokens_in + lastWithUsage.tokens_out : 0;
  const quota = useChatQuota();
  const urgentQuota = pressingQuota(quota.data?.quotas ?? []);
  const currentOption = modelOptions.find((o) => o.value === model);
  const teamName = teamId ? (me?.teams.find((tm) => tm.id === teamId)?.name ?? 'Team') : 'Personal';

  const personalOption = modelOptions.find((o) => o.value === model && o.personal);
  const billedTo = personalOption ? `your ${personalOption.owner} plan` : teamName;
  const allConvs = list.data?.conversations ?? [];
  const pinnedConvs = allConvs.filter((c) => c.pinned);
  const groups = [
    ...(pinnedConvs.length > 0 ? [{ label: 'Pinned', items: pinnedConvs }] : []),
    ...groupConversations(allConvs.filter((c) => !c.pinned)),
  ];
  const temporary = conv ? !!conv.temporary : pending.temporary;
  const activeInstructions = conv ? (conv.instructions ?? '') : pending.instructions;
  const activeEffort = conv ? (conv.reasoning_effort ?? '') : pending.effort;
  const toolsOff = conv ? (conv.tools_off ?? []) : (pending.toolsOff ?? []);
  const catalogModel = chatCatalog.data?.models?.find((m) => m.name === model);
  const modelTools = [...(catalogModel?.tools ?? []), ...(catalogModel?.assist ?? [])];
  const effortLabel = EFFORTS.find((e) => e.value === activeEffort)?.label ?? 'Model default';
  const customised = !!activeInstructions || !!activeEffort || !!settings.data?.instructions;
  const slash = slashQuery(draft);
  const slashMatches = slash !== null ? matchPrompts(prompts.data?.prompts ?? [], slash) : [];
  function applyPrompt(p: ChatPrompt): void {
    if (promptVariables(p.body).length > 0) {
      setDraft('');
      setFilling(p);
      return;
    }
    setDraft(p.body);
  }
  const estimate =
    draft.trim() || files.tokens > 0
      ? estimateInputTokens({
          lastIn: lastWithUsage?.tokens_in ?? 0,
          lastOut: lastWithUsage?.tokens_out ?? 0,
          draft,
          fileTokens: files.tokens,
          instructionChars: activeInstructions.length + (settings.data?.instructions.length ?? 0),
          conversationStarted: !!lastWithUsage,
        })
      : 0;
  const empty = branch.length === 0;
  const lastAssistant = [...branch].reverse().find((m) => m.role === 'assistant');

  const imageMode = !!modelOptions.find((o) => o.value === model)?.image;
  const canSend = imageMode
    ? draft.trim() !== '' && files.items.length === 0 && !!model
    : (draft.trim() !== '' || files.readyIds.length > 0) && !files.uploading && !files.hasErrors && !!model;
  const submit = () => {
    if (!canSend || runId) return;
    void send({ content: draft, parentId: conv?.current_leaf_id ?? '', attachmentIds: files.readyIds, fromComposer: true });
  };

  const composer = (
    <form
      className={dragging ? 'chat-composer chat-composer-drop' : 'chat-composer'}
      onSubmit={(e) => {
        e.preventDefault();
        submit();
      }}
      onDragOver={(e) => {
        if (imageMode || !e.dataTransfer.types.includes('Files')) return;
        e.preventDefault();
        setDragging(true);
      }}
      onDragLeave={() => setDragging(false)}
      onDrop={(e) => {
        if (e.dataTransfer.files.length === 0) return;
        e.preventDefault();
        setDragging(false);
        files.addFiles(e.dataTransfer.files);
      }}
    >
      <AttachmentChips items={files.items} onRemove={files.remove} tokens={files.tokens} contextWindow={ctxWindow} />
      {imageMode ? null : (
        <ModeChips
          research={research}
          compare={compare}
          effortLabel={activeEffort ? effortLabel : ''}
          labelFor={(v) => pickable.find((o) => o.value === v)?.label ?? v}
          onResearchOff={() => setResearch(false)}
          onCompareOff={() => setCompare([])}
          onEffort={() => setSettingsOpen(true)}
        />
      )}
      <input
        ref={fileInputRef}
        type="file"
        multiple
        hidden
        accept={ATTACH_ACCEPT}
        aria-label="Choose files"
        onChange={(e) => {
          if (e.target.files) files.addFiles(e.target.files);
          e.target.value = '';
        }}
      />
      {slash !== null ? (
        <SlashMenu
          prompts={prompts.data?.prompts ?? []}
          query={slash}
          active={slashActive}
          onPick={applyPrompt}
          onManage={() => {
            setPromptSeed(undefined);
            setPromptsOpen(true);
          }}
        />
      ) : null}
      <AutoGrowTextarea
        inputRef={composerRef}
        value={draft}
        onChange={(v) => {
          setDraft(v);
          setSlashActive(0);
        }}
        onSubmit={submit}
        onKeyIntercept={(e) => {
          if (slash === null || slashMatches.length === 0) return false;
          if (e.key === 'ArrowDown') {
            setSlashActive((i) => (i + 1) % Math.min(8, slashMatches.length));
            return true;
          }
          if (e.key === 'ArrowUp') {
            setSlashActive((i) => (i - 1 + Math.min(8, slashMatches.length)) % Math.min(8, slashMatches.length));
            return true;
          }
          if ((e.key === 'Enter' && !e.shiftKey) || e.key === 'Tab') {
            applyPrompt(slashMatches[Math.min(slashActive, slashMatches.length - 1)]!);
            return true;
          }
          if (e.key === 'Escape') {
            setDraft('');
            return true;
          }
          return false;
        }}
        onPasteFiles={imageMode ? undefined : (pasted) => files.addFiles(pasted)}
        placeholder={imageMode ? 'Describe the image you want…' : empty ? 'Ask anything…' : 'Reply…'}
        autoFocus
      />
      {imageMode && files.items.length > 0 ? (
        <p className="chat-image-hint" role="status">
          Image models make a new image from your description and cannot use attached files. Remove them to send.
        </p>
      ) : null}
      <div className="chat-composer-bar">
        <button
          type="button"
          className="chat-attach"
          hidden={imageMode}
          aria-label="Attach files"
          title="Attach files (or drop / paste them)"
          onClick={() => fileInputRef.current?.click()}
        >
          <svg
            width="18"
            height="18"
            viewBox="0 0 24 24"
            aria-hidden="true"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.9"
            strokeLinecap="round"
            strokeLinejoin="round"
          >
            <path d="M20.5 11.5l-8.2 8.2a5 5 0 0 1-7.1-7.1l8.6-8.6a3.4 3.4 0 0 1 4.8 4.8l-8.6 8.6a1.7 1.7 0 0 1-2.4-2.4l7.9-7.9" />
          </svg>
        </button>
        <ModelPicker
          value={model}
          onChange={setModel}
          options={pickable}
          catalog={chatCatalog.data}
          orgTitle={`Working under ${teamName}`}
        />
        {imageMode ? null : (
          <TrayButton
            onOpen={() => setTrayOpen(true)}
            toolsOn={(chatCatalog.data?.tools ?? [])
              .filter((t) => modelTools.includes(t.id) && !toolsOff.includes(t.id))
              .map((t) => t.label)}
            modes={(research ? 1 : 0) + (compare.length > 0 ? 1 : 0)}
          />
        )}
        <span className="chat-composer-spacer" />
        {imageMode ? (
          <span className="chat-estimate" title="Image models are priced per image; the request log has the exact cost.">
            Makes an image
          </span>
        ) : null}
        <SendEstimate
          tokens={imageMode ? 0 : estimate}
          inRate={currentOption?.inRate ?? 0}
          personal={!!currentOption?.personal}
          contextWindow={ctxWindow}
        />
        {runId ? (
          <button type="button" className="chat-send chat-stop" onClick={() => void stop()} aria-label="Stop">
            <span className="chat-stop-square" aria-hidden="true" />
          </button>
        ) : (
          <button type="submit" className="chat-send" disabled={!canSend} aria-label="Send">
            <svg
              width="18"
              height="18"
              viewBox="0 0 24 24"
              aria-hidden="true"
              fill="none"
              stroke="currentColor"
              strokeWidth="2.2"
              strokeLinecap="round"
              strokeLinejoin="round"
            >
              <path d="M12 19V5M5.5 11.5L12 5l6.5 6.5" />
            </svg>
          </button>
        )}
      </div>
    </form>
  );

  return (
    <div className={listOpen ? 'chat-layout list-open' : 'chat-layout'} ref={layoutRef}>
      <aside className="chat-sidebar" aria-label="Conversations">
        <div className="chat-sidebar-top">
          <button
            type="button"
            className="chat-new"
            onClick={() => {
              setListOpen(false);
              navigate('/chat');
            }}
          >
            <svg
              width="16"
              height="16"
              viewBox="0 0 24 24"
              aria-hidden="true"
              fill="none"
              stroke="currentColor"
              strokeWidth="2"
              strokeLinecap="round"
            >
              <path d="M12 5v14M5 12h14" />
            </svg>
            New chat
          </button>
        </div>
        <ChatSearch activeId={id} onOpen={() => setListOpen(false)} />
        <nav className="chat-list-scroll" aria-label="Chat history">
          {groups.map((g) => (
            <section key={g.label} className="chat-list-group">
              <h3 className="chat-list-heading">{g.label}</h3>
              <ul className="chat-list">
                {g.items.map((c) => (
                  <li key={c.id} className={c.id === id ? 'chat-list-item active' : 'chat-list-item'}>
                    {renaming === c.id ? (
                      <RenameField
                        initial={c.title}
                        onDone={(title) => {
                          setRenaming(null);
                          if (title) void patchConversation(c.id, { title });
                        }}
                      />
                    ) : (
                      <Link
                        to={`/chat/${c.id}`}
                        onClick={() => setListOpen(false)}
                        onDoubleClick={(e) => {
                          e.preventDefault();
                          setRenaming(c.id);
                        }}
                        aria-current={c.id === id ? 'page' : undefined}
                      >
                        {c.pinned ? (
                          <svg
                            className="chat-pin"
                            width="12"
                            height="12"
                            viewBox="0 0 24 24"
                            aria-label="Pinned"
                            role="img"
                            fill="currentColor"
                          >
                            <path d="M14.5 3l6.5 6.5-2 1-3.3 3.3.8 4.7-1.5 1.5-4.2-4.2L5 21l-1-1 5.2-5.8L5 10l1.5-1.5 4.7.8L14.5 6l-1-2z" />
                          </svg>
                        ) : null}
                        {c.title || 'New chat'}
                      </Link>
                    )}
                    <ConversationMenu
                      title={c.title}
                      pinned={!!c.pinned}
                      onRename={() => setRenaming(c.id)}
                      onPin={() => void patchConversation(c.id, { pinned: !c.pinned })}
                      onDelete={() => void remove(c.id, c.title)}
                    />
                  </li>
                ))}
              </ul>
            </section>
          ))}
          {list.data && list.data.conversations.length === 0 ? (
            <p className="chat-list-empty">Your chats will appear here.</p>
          ) : null}
        </nav>
      </aside>
      {listOpen ? (
        <button type="button" className="chat-scrim" aria-label="Close chat list" onClick={() => setListOpen(false)} />
      ) : null}

      <section className={empty ? 'chat-main chat-main-empty' : 'chat-main'}>
        <header className="chat-toolbar">
          <button
            type="button"
            className="chat-sidebar-toggle"
            aria-expanded={listOpen}
            aria-label="Chats"
            onClick={() => setListOpen((v) => !v)}
          >
            <svg
              width="18"
              height="18"
              viewBox="0 0 24 24"
              aria-hidden="true"
              fill="none"
              stroke="currentColor"
              strokeWidth="2"
              strokeLinecap="round"
            >
              <path d="M4 6h16M4 12h16M4 18h10" />
            </svg>
          </button>
          <h1 className="chat-title" title={conv?.title || undefined}>
            {temporary ? <span className="chat-temp-badge">Temporary</span> : null}
            {conv ? conv.title || 'New chat' : temporary ? 'Temporary chat' : 'New chat'}
          </h1>
          {!conv ? (
            <button
              type="button"
              className={pending.temporary ? 'chat-tool active' : 'chat-tool'}
              aria-pressed={pending.temporary}
              title={`Temporary chat: not saved to your history and not searchable; deleted after ${idleLabel(chatCatalog.data?.temporary_ttl_hours ?? 24)} idle`}
              onClick={() => setPending((p) => ({ ...p, temporary: !p.temporary }))}
            >
              <svg
                width="16"
                height="16"
                viewBox="0 0 24 24"
                aria-hidden="true"
                fill="none"
                stroke="currentColor"
                strokeWidth="1.9"
                strokeLinecap="round"
                strokeDasharray={pending.temporary ? undefined : '3 3'}
              >
                <circle cx="12" cy="12" r="8.5" />
              </svg>
              <span className="chat-tool-label">Temporary</span>
            </button>
          ) : null}
          <button
            type="button"
            className={customised ? 'chat-tool active' : 'chat-tool'}
            title={`Instructions and thinking (${effortLabel})`}
            aria-label="Chat settings"
            onClick={() => setSettingsOpen(true)}
          >
            <svg
              width="16"
              height="16"
              viewBox="0 0 24 24"
              aria-hidden="true"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.9"
              strokeLinecap="round"
            >
              <path d="M4 7h10M18 7h2M4 17h4M12 17h8" />
              <circle cx="16" cy="7" r="2" />
              <circle cx="10" cy="17" r="2" />
            </svg>
          </button>
          <button
            type="button"
            className="chat-tool"
            title="Saved prompts (type / in the message box)"
            aria-label="Saved prompts"
            onClick={() => {
              setPromptSeed(undefined);
              setPromptsOpen(true);
            }}
          >
            <svg
              width="16"
              height="16"
              viewBox="0 0 24 24"
              aria-hidden="true"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.9"
              strokeLinecap="round"
              strokeLinejoin="round"
            >
              <path d="M6 4h9l3 3v13H6z" />
              <path d="M9.5 11h5M9.5 15h5" />
            </svg>
          </button>
          {conv && !empty ? (
            <button
              type="button"
              className="chat-tool"
              title={chatCatalog.data?.features?.shares === false ? 'Export or print' : 'Share a link or export'}
              aria-label="Share and export"
              onClick={() => setShareOpen(true)}
            >
              <svg
                width="16"
                height="16"
                viewBox="0 0 24 24"
                aria-hidden="true"
                fill="none"
                stroke="currentColor"
                strokeWidth="1.9"
                strokeLinecap="round"
                strokeLinejoin="round"
              >
                <path d="M12 15V4M8 8l4-4 4 4" />
                <path d="M5 13v6h14v-6" />
              </svg>
              <span className="chat-tool-label">Share</span>
            </button>
          ) : null}
          <div className="chat-meter" aria-label="Conversation usage">
            {!localOnly && !empty ? <span>{formatCost(conversationCost)} this chat</span> : null}
            {ctxWindow > 0 && ctxUsed > 0 ? (
              <span className="chat-ctx" title={`${ctxUsed.toLocaleString()} of ${ctxWindow.toLocaleString()} tokens`}>
                <span className="chat-ctx-bar" aria-hidden="true">
                  <span style={{ width: `${Math.min(100, Math.max(2, (ctxUsed / ctxWindow) * 100))}%` }} />
                </span>
                context {Math.round((ctxUsed / ctxWindow) * 100)}% of {ctxWindow.toLocaleString()}
              </span>
            ) : null}
          </div>
        </header>

        {catalog.data && pickable.length === 0 ? (
          <p className="chat-error chat-banner" role="alert">
            No chat models are available while working under {teamName}. Change “Working under” at the top, connect a plan on the
            Subscriptions page, or ask an administrator for access.
          </p>
        ) : null}

        <div className="chat-thread" ref={threadRef} aria-live="polite" onScroll={onThreadScroll}>
          <div className="chat-column">
            {empty ? (
              <div className="chat-empty">
                <h2>{pending.temporary ? 'Temporary chat' : 'What can I help with?'}</h2>
                <p className="muted">
                  {pending.temporary
                    ? 'This chat will not appear in your history or search, and is removed when it goes unused.'
                    : 'Replies keep going even if you close this tab.'}
                </p>
                {!pending.temporary && (chatCatalog.data?.starter_prompts?.length ?? 0) > 0 ? (
                  <div className="chat-starters" role="group" aria-label="Suggestions">
                    {chatCatalog.data?.starter_prompts?.map((p) => (
                      <button
                        key={p}
                        type="button"
                        className="chat-starter"
                        onClick={() => {
                          setDraft(p);
                          composerRef.current?.focus();
                        }}
                      >
                        {p}
                      </button>
                    ))}
                  </div>
                ) : null}
              </div>
            ) : null}
            {branch.map((m) => {
              // A reply made side by side with others (Compare): show the
              // group in columns, the chosen one first-class.
              const mates = m.compare_group
                ? messages.filter((x) => x.compare_group === m.compare_group && x.parent_id === m.parent_id)
                : [];
              const sibs =
                mates.length > 1
                  ? siblingsOf(messages, m).filter((x) => x.compare_group !== m.compare_group || x.id === m.id)
                  : siblingsOf(messages, m);
              const idx = sibs.findIndex((s) => s.id === m.id);
              const isLast = m.id === lastAssistant?.id;
              return [
                divider?.beforeId === m.id ? <ContextDivider key={`divider-${m.id}`} count={divider.count} /> : null,
                mates.length > 1 ? (
                  <CompareRow
                    key={`cmp-${m.compare_group}`}
                    replies={mates}
                    current={m.id}
                    busy={!!runId || Object.keys(sideRuns).length > 0}
                    onPick={(x) => void preferReply(x)}
                  />
                ) : null,
                mates.length > 1 ? null : (
                  <article
                    key={m.id}
                    className={`chat-msg chat-msg-${m.role}${isLast ? ' chat-msg-last' : ''}`}
                    data-status={m.status}
                  >
                    {m.role === 'assistant' ? (
                      <SearchActivity activity={m.activity} streaming={m.status === 'streaming'} />
                    ) : null}
                    {m.reasoning ? (
                      <details className="chat-reasoning">
                        <summary>{m.status === 'streaming' && !m.content ? 'Thinking…' : 'Thinking'}</summary>
                        <div className="chat-reasoning-body">
                          <ChatMarkdown text={m.reasoning} streaming={m.status === 'streaming'} />
                        </div>
                      </details>
                    ) : null}
                    {m.role === 'user' ? (
                      editing === m.id ? (
                        <InlineEdit
                          initial={m.content}
                          onCancel={() => setEditing(null)}
                          onSave={(text) => {
                            setEditing(null);
                            // An edit keeps the message's files.
                            void send({ content: text, parentId: m.parent_id, attachmentIds: m.attachments?.map((a) => a.id) });
                          }}
                        />
                      ) : (
                        <>
                          <MessageAttachments attachments={m.attachments} />
                          {m.content ? (
                            <div className="chat-bubble">
                              <div className="chat-text chat-body">{m.content}</div>
                            </div>
                          ) : null}
                        </>
                      )
                    ) : m.attachments?.some((a) => a.kind === 'image') && !m.activity?.some((x) => x.type === 'create_image') ? (
                      <>
                        <GeneratedImages attachments={m.attachments} />
                        {m.content ? <p className="chat-image-caption">{m.content}</p> : null}
                      </>
                    ) : m.attachments?.some((a) => a.kind === 'image') ? (
                      // A chat model made images with the create_image tool:
                      // the pictures, then its reply.
                      <>
                        <GeneratedImages attachments={m.attachments} />
                        {m.content ? (
                          <div className="chat-body">
                            <ChatMarkdown text={m.content} streaming={m.status === 'streaming'} />
                          </div>
                        ) : null}
                      </>
                    ) : m.content ? (
                      <div className="chat-body">
                        <ChatMarkdown text={m.content} streaming={m.status === 'streaming'} />
                      </div>
                    ) : m.status === 'streaming' && modelOptions.find((o) => o.value === m.model)?.image ? (
                      <div className="chat-image-pending" role="status" aria-label="Making the image">
                        <span className="chat-image-pending-box" />
                        <span className="chat-image-pending-text">Making the image… this can take up to a minute.</span>
                      </div>
                    ) : m.status === 'streaming' ? (
                      <div className="chat-text chat-typing" aria-label="Writing">
                        <span />
                        <span />
                        <span />
                      </div>
                    ) : (
                      <div className="chat-text" />
                    )}
                    {m.role === 'assistant' ? <MadeFiles attachments={m.attachments} /> : null}
                    {m.role === 'assistant' ? <SourcesList sources={m.sources} /> : null}
                    {m.role === 'assistant' ? <MemoryChip activity={m.activity} onOpen={() => setSettingsOpen(true)} /> : null}
                    {m.status === 'stopped' ? <div className="chat-stopped">Stopped</div> : null}
                    {m.role === 'assistant' && m.status !== 'streaming' ? (
                      <ReplyNotes notes={m.notes} model={m.served_model || m.model || ''} />
                    ) : null}
                    {m.status === 'blocked' ? (
                      <div className="chat-blocked" role="status">
                        {m.error_message || 'This reply is not available.'}
                      </div>
                    ) : null}
                    {m.status === 'error' || m.status === 'interrupted' ? (
                      <div className="chat-error" role="alert">
                        {m.error_message || 'The reply failed.'} {m.error_code ? <code>{m.error_code}</code> : null}
                      </div>
                    ) : null}
                    {editing === m.id || m.status === 'streaming' ? null : (
                      <footer className="chat-msg-footer">
                        {sibs.length > 1 ? (
                          <span className="chat-branch">
                            <button
                              type="button"
                              className="chat-act"
                              disabled={idx <= 0}
                              onClick={() => void selectBranch(sibs[idx - 1]!)}
                              aria-label="Previous version"
                            >
                              ‹
                            </button>
                            <span className="chat-branch-count">
                              {idx + 1}/{sibs.length}
                            </span>
                            <button
                              type="button"
                              className="chat-act"
                              disabled={idx >= sibs.length - 1}
                              onClick={() => void selectBranch(sibs[idx + 1]!)}
                              aria-label="Next version"
                            >
                              ›
                            </button>
                          </span>
                        ) : null}
                        <CopyAction label="Copy message" title="Copy (markdown)" text={() => m.content} />
                        {m.role === 'assistant' && m.content ? (
                          <CopyAction
                            label="Copy as text"
                            title="Copy as plain text"
                            icon="text"
                            text={(el) => renderedText(el)}
                          />
                        ) : null}
                        {m.role === 'user' && !runId ? (
                          <button
                            type="button"
                            className="chat-act"
                            aria-label="Edit"
                            title="Edit"
                            onClick={() => setEditing(m.id)}
                          >
                            <Icon name="edit" size={15} />
                          </button>
                        ) : null}
                        {m.role === 'user' && m.content ? (
                          <button
                            type="button"
                            className="chat-act"
                            aria-label="Save as prompt"
                            title="Save as prompt"
                            onClick={() => {
                              setPromptSeed(m.content);
                              setPromptsOpen(true);
                            }}
                          >
                            <svg
                              width="15"
                              height="15"
                              viewBox="0 0 24 24"
                              aria-hidden="true"
                              fill="none"
                              stroke="currentColor"
                              strokeWidth="1.9"
                              strokeLinecap="round"
                              strokeLinejoin="round"
                            >
                              <path d="M6 4h12v16l-6-4-6 4z" />
                            </svg>
                          </button>
                        ) : null}
                        {m.role === 'assistant' && !runId ? (
                          <button
                            type="button"
                            className="chat-act"
                            aria-label="Regenerate"
                            title="Regenerate"
                            onClick={() => void send({ content: '', parentId: m.parent_id, regenerate: true })}
                          >
                            <Icon name="rotate" size={15} />
                          </button>
                        ) : null}
                        {m.role === 'assistant' ? (
                          <span
                            className="chat-usage"
                            title={`${m.tokens_in.toLocaleString()} tokens in, ${m.tokens_out.toLocaleString()} out${m.latency_ms ? `, ${(m.latency_ms / 1000).toFixed(1)} s` : ''}`}
                          >
                            {m.served_model || m.model} · {m.tokens_in.toLocaleString()} in / {m.tokens_out.toLocaleString()} out
                            {!localOnly ? ` · ${formatCost(m.cost_usd)}` : ''}
                            {m.attachments?.some((a) => a.kind === 'image') && m.latency_ms
                              ? ` · ${(m.latency_ms / 1000).toFixed(1)} s`
                              : m.tokens_out_per_second > 0
                                ? ` · ${m.tokens_out_per_second.toFixed(0)} tok/s`
                                : ''}
                            {m.request_id ? (
                              <>
                                {' · '}
                                <Link to={`/requests?q=${m.request_id}`}>request</Link>
                              </>
                            ) : null}
                          </span>
                        ) : null}
                      </footer>
                    )}
                  </article>
                ),
              ].filter(Boolean);
            })}
          </div>
        </div>

        <div className="chat-dock">
          {!following && !empty ? (
            <button
              type="button"
              className="chat-jump"
              onClick={() => {
                setFollow(true);
                toBottom();
              }}
            >
              ↓ Jump to latest
            </button>
          ) : null}
          {error ? (
            <div className="chat-error chat-banner" role="alert">
              {error}
            </div>
          ) : null}
          <QuotaLine quota={urgentQuota} />
          {notice ? (
            <div className="chat-notice" role="status">
              {notice}
            </div>
          ) : null}
          {composer}
          <p className="chat-hint">
            <span className="chat-billing" aria-label="Billed to" title="Who pays for this message">
              Billed to <strong>{billedTo}</strong>
            </span>
            <span className="chat-hint-keys"> · Enter to send · Shift+Enter for a new line · Esc stops · / for prompts</span>
          </p>
        </div>
      </section>
      {conv ? (
        <ShareDrawer
          open={shareOpen}
          onClose={() => setShareOpen(false)}
          conversationId={conv.id}
          temporary={!!conv.temporary}
          onPrint={() => setTimeout(() => window.print(), 50)}
          sharesEnabled={chatCatalog.data?.features?.shares !== false}
          exportEnabled={chatCatalog.data?.features?.export !== false}
        />
      ) : null}
      <PromptsDrawer open={promptsOpen} onClose={() => setPromptsOpen(false)} seed={promptSeed} />
      <PromptFillDialog
        prompt={filling}
        onCancel={() => setFilling(null)}
        onInsert={(text) => {
          setFilling(null);
          setDraft(text);
          setTimeout(() => document.querySelector<HTMLTextAreaElement>('textarea[aria-label=Message]')?.focus(), 0);
        }}
      />
      <ChatTray
        open={trayOpen}
        onClose={() => setTrayOpen(false)}
        busy={!!runId}
        model={model}
        chatOptions={pickable.filter((o) => !o.image).map((o) => ({ value: o.value, label: o.label }))}
        research={research}
        onResearch={setResearch}
        compare={compare}
        onCompare={setCompare}
        tools={chatCatalog.data?.tools ?? []}
        available={modelTools}
        off={toolsOff}
        onToolsOff={(off) => void saveToolsOff(off)}
        imageModels={chatCatalog.data?.image_models ?? []}
        videoModels={chatCatalog.data?.video_models ?? []}
        musicModels={chatCatalog.data?.music_models ?? []}
        childMediaOff={chatCatalog.data?.child_media_off ?? []}
        onImageDefault={(m) => {
          // The open chat follows the new default (a per-chat pick would
          // otherwise win over it); new chats read the default.
          if (conv?.image_model || pending.imageModel) void saveImageModel(m);
        }}
        billedTo={billedTo}
        onOpenSettings={() => {
          setTrayOpen(false);
          setSettingsOpen(true);
        }}
      />
      <ChatSettingsDrawer
        open={settingsOpen}
        onClose={() => setSettingsOpen(false)}
        instructions={activeInstructions}
        effort={activeEffort}
        onSave={saveSettings}
      />
    </div>
  );
}

/**
 * Replies made side by side (Compare): one column per model (stacked on
 * narrow screens), each with its status, usage and a "Continue with this"
 * button that makes it the branch the conversation goes on from.
 */
function CompareRow({
  replies,
  current,
  busy,
  onPick,
}: {
  replies: ChatMessage[];
  current: string;
  busy: boolean;
  onPick: (m: ChatMessage) => void;
}): ReactNode {
  return (
    <section className="chat-compare" aria-label={`${replies.length} models compared`} data-count={replies.length}>
      {replies.map((m) => {
        const chosen = m.id === current;
        return (
          <article
            key={m.id}
            className={`chat-msg chat-msg-assistant chat-compare-col${chosen ? ' chat-compare-chosen' : ''}`}
            aria-label={`${m.model}${chosen ? ' (preferred)' : ''}`}
            data-status={m.status}
            aria-current={chosen ? 'true' : undefined}
          >
            <header className="chat-compare-head">
              <span className="chat-compare-model" title={m.served_model || m.model}>
                {m.model}
              </span>
              {chosen ? <span className="chat-compare-badge">Preferred</span> : null}
            </header>
            <SearchActivity activity={m.activity} streaming={m.status === 'streaming'} />
            <div className="chat-compare-body">
              {m.reasoning ? (
                <details className="chat-reasoning">
                  <summary>{m.status === 'streaming' && !m.content ? 'Thinking…' : 'Thinking'}</summary>
                  <div className="chat-reasoning-body">
                    <ChatMarkdown text={m.reasoning} streaming={m.status === 'streaming'} />
                  </div>
                </details>
              ) : null}
              {m.content ? (
                <div className="chat-body">
                  <ChatMarkdown text={m.content} streaming={m.status === 'streaming'} />
                </div>
              ) : m.status === 'streaming' ? (
                <div className="chat-text chat-typing" aria-label="Writing">
                  <span />
                  <span />
                  <span />
                </div>
              ) : null}
              <MadeFiles attachments={m.attachments} />
              <SourcesList sources={m.sources} />
            </div>
            {m.status === 'error' || m.status === 'interrupted' ? (
              <div className="chat-error" role="alert">
                {m.error_message || 'The reply failed.'}
              </div>
            ) : null}
            {m.status === 'blocked' ? (
              <div className="chat-blocked" role="status">
                {m.error_message || 'This reply is not available.'}
              </div>
            ) : null}
            {m.status !== 'streaming' ? (
              <footer className="chat-compare-foot">
                <span className="chat-usage">
                  {m.tokens_out.toLocaleString()} out
                  {m.cost_usd ? ` · ${formatCost(m.cost_usd)}` : ''}
                  {m.latency_ms ? ` · ${(m.latency_ms / 1000).toFixed(1)} s` : ''}
                </span>
                <CopyAction label="Copy message" title="Copy (markdown)" text={() => m.content} />
                {!chosen ? (
                  <button type="button" className="btn btn-sm" disabled={busy} onClick={() => onPick(m)}>
                    Prefer this answer
                  </button>
                ) : null}
              </footer>
            ) : null}
          </article>
        );
      })}
    </section>
  );
}

/** A generated image on a reply: large, opens full size, downloadable. */
function GeneratedImages({ attachments }: { attachments: ChatAttachment[] }): ReactNode {
  return (
    <div className="chat-gen-images">
      {attachments
        .filter((a) => a.kind === 'image')
        .map((a) => {
          const href = `/api/v1/chat/attachments/${a.id}/content`;
          return (
            <figure key={a.id} className="chat-gen-image">
              <a href={href} target="_blank" rel="noopener noreferrer" title="Open full size">
                <img src={href} alt={a.filename} width={a.width || undefined} height={a.height || undefined} loading="lazy" />
              </a>
              <figcaption>
                <a className="chat-gen-download" href={href} download={a.filename}>
                  Download
                </a>
                {a.width && a.height ? (
                  <span className="muted">
                    {a.width}×{a.height}
                  </span>
                ) : null}
              </figcaption>
            </figure>
          );
        })}
    </div>
  );
}

const SECGW_LABEL: Record<string, string> = {
  observed: 'Flagged by a security policy',
  redacted: 'Some of your message was redacted by a security policy',
  stream_cut: 'A security policy stopped this reply',
  blocked: 'Blocked by a security policy',
  // Child Safety's tutor: not a security event, just a note.
  withheld: 'Tutor held back the answer',
};

/** One-line request-log context under a reply: routing, fallback, security. */
function ReplyNotes({ notes, model }: { notes?: ChatReplyNotes; model: string }): ReactNode {
  if (!notes) return null;
  const items: { key: string; text: string; tone: 'info' | 'warn' }[] = [];
  if (notes.requested_model) {
    const why = notes.route_reason ? ` (${notes.route_reason.replace(/^matched:/, 'matched ')})` : '';
    items.push({ key: 'route', text: `${notes.requested_model} → ${model}${why}`, tone: 'info' });
  }
  if (notes.fallback_reason)
    items.push({ key: 'fb', text: `Fallback model answered (${notes.fallback_reason.replace(/_/g, ' ')})`, tone: 'warn' });
  if (notes.reasoning_adjustment) items.push({ key: 'think', text: `Thinking: ${notes.reasoning_adjustment}`, tone: 'info' });
  if (notes.secgw_action && SECGW_LABEL[notes.secgw_action]) {
    const n = notes.secgw_violations ?? 0;
    const tone = notes.secgw_action === 'withheld' ? 'info' : 'warn';
    items.push({ key: 'sec', text: SECGW_LABEL[notes.secgw_action] + (n > 1 ? ` (${n} findings)` : ''), tone });
  }
  if (items.length === 0) return null;
  return (
    <ul className="chat-notes" aria-label="About this reply">
      {items.map((it) => (
        <li key={it.key} className={`chat-note chat-note-${it.tone}`}>
          {it.text}
        </li>
      ))}
    </ul>
  );
}

/** Group conversations the way chat apps do: Today, Yesterday, 7 days, 30 days, older by month. */
export function groupConversations(
  list: ChatConversation[],
  now: Date = new Date(),
): { label: string; items: ChatConversation[] }[] {
  const start = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
  const day = 86_400_000;
  const order: string[] = [];
  const byLabel = new Map<string, ChatConversation[]>();
  const sorted = [...list].sort((a, b) => (b.updated_at || b.created_at).localeCompare(a.updated_at || a.created_at));
  for (const c of sorted) {
    const t = new Date(c.updated_at || c.created_at).getTime();
    let label: string;
    if (Number.isNaN(t) || t >= start) label = 'Today';
    else if (t >= start - day) label = 'Yesterday';
    else if (t >= start - 7 * day) label = 'Previous 7 days';
    else if (t >= start - 30 * day) label = 'Previous 30 days';
    else label = new Date(t).toLocaleDateString(undefined, { month: 'long', year: 'numeric' });
    if (!byLabel.has(label)) {
      byLabel.set(label, []);
      order.push(label);
    }
    byLabel.get(label)!.push(c);
  }
  return order.map((label) => ({ label, items: byLabel.get(label)! }));
}

/** A textarea that grows with its content up to ~40% of the viewport. */
function AutoGrowTextarea({
  value,
  onChange,
  onSubmit,
  placeholder,
  autoFocus,
  onPasteFiles,
  onKeyIntercept,
  inputRef,
}: {
  value: string;
  onChange: (v: string) => void;
  onSubmit: () => void;
  placeholder: string;
  autoFocus?: boolean;
  onPasteFiles?: (files: File[]) => void;
  /** Return true to swallow the key (e.g. the "/" prompt menu). */
  onKeyIntercept?: (e: React.KeyboardEvent<HTMLTextAreaElement>) => boolean;
  inputRef?: React.RefObject<HTMLTextAreaElement | null>;
}): ReactNode {
  const ownRef = useRef<HTMLTextAreaElement>(null);
  const ref = inputRef ?? ownRef;
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    el.style.height = 'auto';
    el.style.height = `${Math.min(el.scrollHeight, Math.max(160, window.innerHeight * 0.4))}px`;
  }, [value]);
  return (
    <textarea
      ref={ref}
      className="chat-input"
      aria-label="Message"
      rows={1}
      value={value}
      placeholder={placeholder}
      autoFocus={autoFocus}
      onChange={(e) => onChange(e.target.value)}
      onPaste={(e) => {
        const pasted = Array.from(e.clipboardData.files);
        if (pasted.length === 0 || !onPasteFiles) return;
        e.preventDefault();
        onPasteFiles(pasted);
      }}
      onKeyDown={(e) => {
        if (onKeyIntercept?.(e)) {
          e.preventDefault();
          return;
        }
        if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
          e.preventDefault();
          onSubmit();
        }
      }}
    />
  );
}

/** Edit a sent message in place (forks a new branch on save). */
function InlineEdit({
  initial,
  onSave,
  onCancel,
}: {
  initial: string;
  onSave: (text: string) => void;
  onCancel: () => void;
}): ReactNode {
  const [text, setText] = useState(initial);
  return (
    <div className="chat-edit">
      <textarea
        className="chat-input"
        aria-label="Edit message"
        value={text}
        rows={Math.min(10, Math.max(2, text.split('\n').length))}
        autoFocus
        onChange={(e) => setText(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Escape') onCancel();
          if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
            e.preventDefault();
            if (text.trim()) onSave(text);
          }
        }}
      />
      <div className="chat-edit-actions">
        <button type="button" className="btn btn-sm" onClick={onCancel}>
          Cancel
        </button>
        <button type="button" className="btn btn-primary btn-sm" disabled={!text.trim()} onClick={() => onSave(text)}>
          Send
        </button>
      </div>
    </div>
  );
}

/** Icon copy button that briefly shows a check once copied. */
function CopyAction({
  label,
  title,
  text,
  icon = 'copy',
}: {
  label: string;
  title: string;
  text: (el: HTMLElement) => string;
  icon?: 'copy' | 'text';
}): ReactNode {
  const [done, setDone] = useState(false);
  useEffect(() => {
    if (!done) return;
    const t = setTimeout(() => setDone(false), 1500);
    return () => clearTimeout(t);
  }, [done]);
  return (
    <button
      type="button"
      className="chat-act"
      aria-label={label}
      title={done ? 'Copied' : title}
      onClick={(e) => {
        void navigator.clipboard.writeText(text(e.currentTarget)).then(() => setDone(true));
      }}
    >
      {done ? (
        <Icon name="check" size={15} />
      ) : icon === 'text' ? (
        <svg
          width="15"
          height="15"
          viewBox="0 0 24 24"
          aria-hidden="true"
          fill="none"
          stroke="currentColor"
          strokeWidth="1.8"
          strokeLinecap="round"
        >
          <path d="M5 6h14M5 12h14M5 18h9" />
        </svg>
      ) : (
        <Icon name="copy" size={15} />
      )}
    </button>
  );
}
