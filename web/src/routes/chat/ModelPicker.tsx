/**
 * The chat model picker: a button that opens a list grouped by the admin's
 * categories (Administration → Chat → Models), with the chat name and
 * description a grant gives each model, a Recommended badge, and the
 * caller's own plans and image models in their own groups.
 *
 * A native, visually hidden <select aria-label="Model"> mirrors the choice so
 * keyboard/AT users and automation keep the plain control.
 */
import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '../../lib/api';

export interface ChatCatalogModel {
  name: string;
  chat_name?: string;
  description?: string;
  category?: string;
  order?: number;
  recommended?: boolean;
  /** Chat tools this model's provider runs (after the admin's switches). */
  tools?: string[];
  /** Janus-run tools it may call: create_file, create_video, create_music, memory. */
  assist?: string[];
}

export interface ChatCatalog {
  models: ChatCatalogModel[];
  hidden: string[];
  default_model: string;
  starter_prompts: string[];
  categories: string[];
  features: { shares: boolean; export: boolean; personal_plans: boolean };
  /** How long an idle temporary chat is kept, in hours. */
  temporary_ttl_hours?: number;
  /** Every Chat tool, for labels. */
  tools?: { id: string; label: string; description: string }[];
  /** Image models Create images can use for this person. */
  image_models?: string[];
  default_image_model?: string;
  video_models?: string[];
  music_models?: string[];
  /** Media kinds Child Safety keeps from this person: image, video, music, transcription. */
  child_media_off?: string[];
}

export const CATEGORY_TITLES: Record<string, string> = {
  general: 'General',
  fast: 'Fast',
  reasoning: 'Reasoning',
  coding: 'Coding',
  vision: 'Vision',
  local: 'Local & private',
  creative: 'Creative',
};

export function useChatCatalog(teamId: string) {
  return useQuery({
    queryKey: ['chat', 'catalog', teamId],
    queryFn: () => api.get<ChatCatalog>('/api/v1/chat/catalog'),
    staleTime: 60_000,
    // An older gateway without the endpoint: the picker works without it.
    retry: false,
  });
}

export interface PickerOption {
  value: string;
  label: string;
  personal?: boolean;
  image?: boolean;
  owner?: string;
}

interface Group {
  key: string;
  title: string;
  options: (PickerOption & { meta?: ChatCatalogModel })[];
}

/** Applies the admin curation: drops hidden models, groups, orders. */
export function arrangeOptions(options: PickerOption[], catalog: ChatCatalog | undefined, orgTitle: string): Group[] {
  const meta = new Map((Array.isArray(catalog?.models) ? catalog.models : []).map((m) => [m.name, m]));
  const hidden = new Set(Array.isArray(catalog?.hidden) ? catalog.hidden : []);
  const org = options.filter((o) => !o.personal && !o.image && !hidden.has(o.value));
  const byCat = new Map<string, Group['options']>();
  for (const o of org) {
    const m = meta.get(o.value);
    const cat = m?.category ?? '';
    byCat.set(cat, [...(byCat.get(cat) ?? []), { ...o, meta: m }]);
  }
  const sortOpts = (list: Group['options']) =>
    list.sort((a, b) => {
      const ao = a.meta?.order ?? 500;
      const bo = b.meta?.order ?? 500;
      if (ao !== bo) return ao - bo;
      if (!!a.meta?.recommended !== !!b.meta?.recommended) return a.meta?.recommended ? -1 : 1;
      return (a.meta?.chat_name || a.label).localeCompare(b.meta?.chat_name || b.label);
    });
  const groups: Group[] = [];
  const categorized = [...byCat.keys()].filter((k) => k !== '');
  const order = Array.isArray(catalog?.categories) ? catalog.categories : Object.keys(CATEGORY_TITLES);
  categorized.sort((a, b) => order.indexOf(a) - order.indexOf(b));
  // Uncategorized models: one group under the "Working under" title, or
  // "Other" once the admin has started categorizing.
  if (categorized.length === 0) {
    if (org.length) groups.push({ key: 'org', title: orgTitle, options: sortOpts(byCat.get('') ?? []) });
  } else {
    for (const c of categorized)
      groups.push({ key: `cat-${c}`, title: CATEGORY_TITLES[c] ?? c, options: sortOpts(byCat.get(c) ?? []) });
    const rest = byCat.get('') ?? [];
    if (rest.length) groups.push({ key: 'cat-other', title: 'Other', options: sortOpts(rest) });
  }
  const images = options.filter((o) => o.image && !hidden.has(o.value));
  if (images.length)
    groups.push({ key: 'images', title: 'Make images', options: images.map((o) => ({ ...o, meta: meta.get(o.value) })) });
  const plans = options.filter((o) => o.personal);
  if (plans.length && catalog?.features?.personal_plans !== false) {
    groups.push({ key: 'plans', title: 'Your subscriptions', options: plans });
  }
  return groups;
}

export function ModelPicker({
  value,
  onChange,
  options,
  catalog,
  orgTitle,
}: {
  value: string;
  onChange: (v: string) => void;
  options: PickerOption[];
  catalog: ChatCatalog | undefined;
  orgTitle: string;
}): ReactNode {
  const groups = useMemo(() => arrangeOptions(options, catalog, orgTitle), [options, catalog, orgTitle]);
  const flat = groups.flatMap((g) => g.options);
  const current = flat.find((o) => o.value === value);
  const [open, setOpen] = useState(false);
  const [filter, setFilter] = useState('');
  const [active, setActive] = useState(0);
  const ref = useRef<HTMLDivElement>(null);
  const searchRef = useRef<HTMLInputElement>(null);
  const showSearch = flat.length > 8;

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', onDown);
    return () => document.removeEventListener('mousedown', onDown);
  }, [open]);
  const openMenu = () => {
    setFilter('');
    setActive(
      Math.max(
        0,
        flat.findIndex((o) => o.value === value),
      ),
    );
    setOpen(true);
    setTimeout(() => searchRef.current?.focus(), 0);
  };

  const q = filter.trim().toLowerCase();
  const visible = groups
    .map((g) => ({
      ...g,
      options: g.options.filter(
        (o) =>
          !q ||
          o.label.toLowerCase().includes(q) ||
          (o.meta?.chat_name ?? '').toLowerCase().includes(q) ||
          (o.meta?.description ?? '').toLowerCase().includes(q),
      ),
    }))
    .filter((g) => g.options.length > 0);
  const visibleFlat = visible.flatMap((g) => g.options);

  const choose = (v: string) => {
    onChange(v);
    setOpen(false);
  };
  const title = current?.meta?.chat_name || current?.label || 'Pick a model';

  return (
    <div className="chat-picker" ref={ref}>
      <select className="sr-only" aria-label="Model" tabIndex={-1} value={value} onChange={(e) => onChange(e.target.value)}>
        {groups.map((g) => (
          <optgroup key={g.key} label={g.title}>
            {g.options.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </optgroup>
        ))}
      </select>
      <button
        type="button"
        className="chat-picker-button"
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-label={`Model: ${title}`}
        title={current?.meta?.chat_name ? current.label : undefined}
        onClick={() => (open ? setOpen(false) : openMenu())}
      >
        <span className="chat-picker-name">{title}</span>
        <span className="chat-picker-caret" aria-hidden="true">
          ▾
        </span>
      </button>
      {open ? (
        <div
          className="chat-picker-menu"
          onKeyDown={(e) => {
            if (e.key === 'Escape') {
              e.preventDefault();
              setOpen(false);
            } else if (e.key === 'ArrowDown') {
              e.preventDefault();
              setActive((i) => Math.min(visibleFlat.length - 1, i + 1));
            } else if (e.key === 'ArrowUp') {
              e.preventDefault();
              setActive((i) => Math.max(0, i - 1));
            } else if (e.key === 'Enter') {
              e.preventDefault();
              const o = visibleFlat[active];
              if (o) choose(o.value);
            }
          }}
        >
          {showSearch ? (
            <input
              ref={searchRef}
              className="input chat-picker-search"
              aria-label="Find a model"
              placeholder="Find a model"
              value={filter}
              onChange={(e) => {
                setFilter(e.target.value);
                setActive(0);
              }}
            />
          ) : null}
          <div
            className="chat-picker-list"
            role="listbox"
            aria-label="Models"
            tabIndex={showSearch ? -1 : 0}
            ref={showSearch ? undefined : (el) => el?.focus()}
          >
            {visible.length === 0 ? <p className="small muted chat-picker-none">No model matches.</p> : null}
            {visible.map((g) => (
              <div key={g.key} role="group" aria-label={g.title} className="chat-picker-group">
                <div className="chat-picker-group-title" aria-hidden="true">
                  {g.title}
                </div>
                {g.options.map((o) => {
                  const idx = visibleFlat.indexOf(o);
                  const named = o.meta?.chat_name;
                  return (
                    <div
                      key={o.value}
                      role="option"
                      aria-selected={o.value === value}
                      data-active={idx === active}
                      className="chat-picker-option"
                      onMouseEnter={() => setActive(idx)}
                      onClick={() => choose(o.value)}
                    >
                      <div className="chat-picker-option-main">
                        <span className="chat-picker-option-name">{named || o.label}</span>
                        {o.meta?.recommended ? <span className="chat-picker-badge">Recommended</span> : null}
                        {o.value === value ? (
                          <span className="chat-picker-check" aria-hidden="true">
                            ✓
                          </span>
                        ) : null}
                      </div>
                      {o.meta?.description ? <div className="chat-picker-option-desc">{o.meta.description}</div> : null}
                      {named ? <div className="chat-picker-option-model">{o.label}</div> : null}
                    </div>
                  );
                })}
              </div>
            ))}
          </div>
        </div>
      ) : null}
    </div>
  );
}
