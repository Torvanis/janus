/**
 * Administration → Chat → Tools.
 *
 * 1. Tools from the model's provider — run on the provider's own servers
 *    (OpenAI, xAI, Anthropic, Google). On by default; each can be turned
 *    off for everyone here, or for one chat by the person using it.
 * 2. Create images — Janus makes the image with an image model.
 * 3. Web search for other models — models whose provider runs no search
 *    (vLLM, llama.cpp, Ollama…) can search through a service set up here.
 *    Off by default. Janus only ever runs searches: no code runs on Janus.
 */
import { useEffect, useState, type ReactNode } from 'react';
import { useMutation } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { Badge, Field } from '../../components/ui';
import {
  SaveBar,
  ToggleRow,
  toggleOff,
  useChatModelNames,
  useSaveChat,
  type ChatAdminDoc,
  type ChatSearchSettings,
} from './ChatAdmin';

type Provider = ChatSearchSettings['provider'];

interface SearchService {
  id: Exclude<Provider, ''>;
  name: string;
  tagline: string;
  price: string;
  needsKey: boolean;
  keyUrl?: string;
  steps: string[];
}

const SERVICES: SearchService[] = [
  {
    id: 'searxng',
    name: 'SearXNG',
    tagline: 'Self-hosted, free, no account. Combines results from many search engines.',
    price: 'Free (you run it)',
    needsKey: false,
    keyUrl: 'https://docs.searxng.org/admin/installation-docker.html',
    steps: [
      'Run the official SearXNG Docker image on your network. To set it up with the deployment, pass its address in JANUS_CHAT_SEARCH_SEARXNG_URL and Janus turns this on for you.',
      'In its settings.yml, add json under search → formats, then restart it.',
      'Enter its address below, for example http://searxng.search.svc:8080.',
    ],
  },
  {
    id: 'brave',
    name: 'Brave Search',
    tagline: 'Its own independent index. Fast, private, good general results.',
    price: 'Free tier, then about $5 per 1,000 searches',
    needsKey: true,
    keyUrl: 'https://api-dashboard.search.brave.com/app/keys',
    steps: [
      'Sign in to the Brave Search API dashboard and choose a plan (the free plan works).',
      'Open API Keys and select Add API key.',
      'Copy the key and paste it below.',
    ],
  },
  {
    id: 'exa',
    name: 'Exa',
    tagline: 'Search built for AI models: returns the most relevant passages of each page.',
    price: 'Free credits, then about $5 per 1,000 searches',
    needsKey: true,
    keyUrl: 'https://dashboard.exa.ai/api-keys',
    steps: ['Sign in to the Exa dashboard.', 'Open API Keys and create a key.', 'Copy the key and paste it below.'],
  },
  {
    id: 'tavily',
    name: 'Tavily',
    tagline: 'Search built for AI models: clean summaries of each result.',
    price: '1,000 free searches a month, then paid plans',
    needsKey: true,
    keyUrl: 'https://app.tavily.com/home',
    steps: ['Sign in to Tavily.', 'Your API key is on the Overview page; select copy.', 'Paste it below.'],
  },
];

const DEFAULT_SEARCH: ChatSearchSettings = { enabled: false, provider: '', url: '', max_results: 5 };

interface TestResult {
  ok: boolean;
  error?: string;
  ms?: number;
  results?: { title: string; url: string; snippet: string }[];
}

export function ToolsTab({ doc }: { doc: ChatAdminDoc }): ReactNode {
  const save = useSaveChat();
  const { rows } = useChatModelNames();
  const [draft, setDraft] = useState(doc);
  const [key, setKey] = useState('');
  useEffect(() => {
    setDraft(doc);
    setKey('');
  }, [doc]);
  const search = { ...DEFAULT_SEARCH, ...(draft.search ?? {}) };
  const setSearch = (patch: Partial<ChatSearchSettings>) => setDraft((d) => ({ ...d, search: { ...search, ...patch } }));
  const off = draft.tools_off ?? [];
  const setOff = (id: string, on: boolean) => setDraft((d) => ({ ...d, tools_off: toggleOff(d.tools_off, id, on) }));
  const service = SERVICES.find((s) => s.id === search.provider);
  const keySet = !!(search.provider && doc.search_keys_set?.[search.provider]);
  const dirty = JSON.stringify(draft) !== JSON.stringify(doc) || key !== '';
  const test = useMutation({
    mutationFn: () =>
      api.post<TestResult>('/api/v1/admin/chat/search-test', {
        provider: search.provider,
        url: search.url,
        key,
        query: "today's top news",
      }),
  });
  const resetTest = test.reset;
  useEffect(() => resetTest(), [search.provider, search.url, key, resetTest]);
  const missing = !service
    ? 'Choose a search service.'
    : service.needsKey
      ? !keySet && !key
        ? 'Add the API key.'
        : ''
      : !search.url
        ? 'Enter the SearXNG address.'
        : '';
  return (
    <form
      className="stack"
      style={{ gap: 'var(--janus-space-5)' }}
      onSubmit={(e) => {
        e.preventDefault();
        const next: ChatAdminDoc = { ...draft, search };
        if (key) next.search_key = key;
        save.mutate(next);
      }}
    >
      <section className="card stack" aria-labelledby="chat-tools-title">
        <div>
          <h2 id="chat-tools-title" style={{ margin: 0 }}>
            Tools from the model's provider
          </h2>
          <p className="small secondary" style={{ margin: '4px 0 0' }}>
            These run on the provider's own servers, the way they do in ChatGPT, Grok, Claude and Gemini. A model only gets the
            tools its provider runs, and people can turn each one off for a chat. On by default; extra provider fees are included
            in each reply's cost.
          </p>
        </div>
        <ToggleRow
          label="Web search"
          hint="OpenAI, xAI, Claude and Gemini models search the web and cite the pages they used."
          checked={!off.includes('web_search')}
          onChange={(v) => setOff('web_search', v)}
        />
        <ToggleRow
          label="X search"
          hint="xAI (Grok) models search posts on X for news and what people are saying."
          checked={!off.includes('x_search')}
          onChange={(v) => setOff('x_search', v)}
        />
        <ToggleRow
          label="Read web pages"
          hint="Models open a link the person gives them and read the page or PDF: Claude and Gemini with their provider's reader, every other model with Janus's (public web pages only; never your own network)."
          checked={!off.includes('web_fetch')}
          onChange={(v) => setOff('web_fetch', v)}
        />
        <ToggleRow
          label="Run code"
          hint="OpenAI, xAI, Claude and Gemini models run code in the provider's own sandbox for calculations and data. Nothing runs on Janus."
          checked={!off.includes('code_execution')}
          onChange={(v) => setOff('code_execution', v)}
        />
      </section>

      <section className="card stack" aria-labelledby="chat-images-title">
        <div>
          <h2 id="chat-images-title" style={{ margin: 0 }}>
            Create images
          </h2>
          <p className="small secondary" style={{ margin: '4px 0 0' }}>
            Any chat model can make a picture when asked. A personal-plan chat uses the plan's own image model (paid by the plan);
            otherwise the model below, billed to Working under.
          </p>
        </div>
        <ToggleRow
          label="Let chat models make images"
          hint="Turn off to stop chat models making images."
          checked={!off.includes('create_image')}
          onChange={(v) => setOff('create_image', v)}
        />
        <Field
          label="Image model for Create images"
          hint="Used when the chat is not on a personal plan with its own image model."
        >
          <select
            className="select"
            aria-label="Image model for Create images"
            value={draft.image_model ?? ''}
            disabled={off.includes('create_image')}
            onChange={(e) => setDraft((d) => ({ ...d, image_model: e.target.value }))}
          >
            <option value="">First image model a person may use</option>
            {rows
              .filter((m) => !m.chat && !m.media)
              .map((m) => (
                <option key={m.id} value={m.name}>
                  {m.name}
                  {m.upstream ? ` · ${m.upstream}` : ''}
                </option>
              ))}
          </select>
        </Field>
      </section>

      <section className="card stack" aria-labelledby="chat-assist-title">
        <div>
          <h2 id="chat-assist-title" style={{ margin: 0 }}>
            Files, video, music and memory
          </h2>
          <p className="small secondary" style={{ margin: '4px 0 0', maxWidth: '62ch' }}>
            Janus runs these for any chat model that can call tools. Video and music are their own metered requests, billed like
            images; files and memory cost nothing extra.
          </p>
        </div>
        <ToggleRow
          label="Create files"
          hint="Models hand over whole files (code, web pages, games, documents, data) as cards with Preview and Download."
          checked={!off.includes('create_file')}
          onChange={(v) => setOff('create_file', v)}
        />
        <ToggleRow
          label="Memory"
          hint="Models save what people ask them to remember and read it in later chats. Each person sees, edits and deletes their own memories; administrators cannot read them."
          checked={!off.includes('memory')}
          onChange={(v) => setOff('memory', v)}
        />
        <ToggleRow
          label="Create videos"
          hint="Short clips with the video model below (xAI Grok Imagine, Google Veo, OpenAI Sora)."
          checked={!off.includes('create_video')}
          onChange={(v) => setOff('create_video', v)}
        />
        <Field label="Video model" hint="Used when the chat is not on a personal plan with its own video model.">
          <select
            className="select"
            aria-label="Video model"
            value={draft.video_model ?? ''}
            disabled={off.includes('create_video')}
            onChange={(e) => setDraft((d) => ({ ...d, video_model: e.target.value }))}
          >
            <option value="">First video model a person may use</option>
            {rows
              .filter((m) => m.media === 'video')
              .map((m) => (
                <option key={m.id} value={m.name}>
                  {m.name}
                  {m.upstream ? ` · ${m.upstream}` : ''}
                </option>
              ))}
          </select>
        </Field>
        <ToggleRow
          label="Create music"
          hint="Music clips with the music model below (Google Lyria)."
          checked={!off.includes('create_music')}
          onChange={(v) => setOff('create_music', v)}
        />
        <Field label="Music model">
          <select
            className="select"
            aria-label="Music model"
            value={draft.music_model ?? ''}
            disabled={off.includes('create_music')}
            onChange={(e) => setDraft((d) => ({ ...d, music_model: e.target.value }))}
          >
            <option value="">First music model a person may use</option>
            {rows
              .filter((m) => m.media === 'music')
              .map((m) => (
                <option key={m.id} value={m.name}>
                  {m.name}
                  {m.upstream ? ` · ${m.upstream}` : ''}
                </option>
              ))}
          </select>
        </Field>
        <Field
          label="Transcribe sound with"
          hint="Models that do not hear sound get a transcript of attached sound (and of a video's sound track when nothing can watch it), made once per file."
        >
          <select
            className="select"
            aria-label="Transcribe sound with"
            value={draft.transcribe_model ?? ''}
            onChange={(e) => setDraft((d) => ({ ...d, transcribe_model: e.target.value }))}
          >
            <option value="">First speech-to-text model a person may use</option>
            {rows
              .filter((m) => m.media === 'stt')
              .map((m) => (
                <option key={m.id} value={m.name}>
                  {m.name}
                  {m.upstream ? ` · ${m.upstream}` : ''}
                </option>
              ))}
          </select>
        </Field>
        <Field
          label="Describe videos with"
          hint="Models that do not watch video get a description of attached videos from this model (one that watches video — set Sees video in Models), made once per file."
        >
          <select
            className="select"
            aria-label="Describe videos with"
            value={draft.describe_model ?? ''}
            onChange={(e) => setDraft((d) => ({ ...d, describe_model: e.target.value }))}
          >
            <option value="">First model that watches video a person may use</option>
            {rows
              .filter((m) => m.chat)
              .map((m) => (
                <option key={m.id} value={m.name}>
                  {m.name}
                  {m.upstream ? ` · ${m.upstream}` : ''}
                </option>
              ))}
          </select>
        </Field>
        {rows.every((m) => !m.media) ? (
          <p className="small secondary" style={{ margin: 0 }}>
            No video or music models are enabled and granted yet. Enable one in Models, then grant it, to turn these on.
          </p>
        ) : null}
      </section>

      <section className="card stack" aria-labelledby="chat-search-title">
        <div className="row-between" style={{ alignItems: 'flex-start', gap: 'var(--janus-space-3)' }}>
          <div>
            <h2 id="chat-search-title" style={{ margin: 0 }}>
              Web search for other models
            </h2>
            <p className="small secondary" style={{ margin: '4px 0 0', maxWidth: '62ch' }}>
              Self-hosted and other models (vLLM, llama.cpp, Ollama…) have no search of their own. Set up a search service and
              Janus searches for them when they ask. Providers with their own search keep using it.
            </p>
          </div>
          <span className={`search-status${doc.search?.enabled ? ' search-status-on' : ''}`} role="status">
            <span className="search-status-dot" aria-hidden="true" />
            {doc.search?.enabled ? `In use · ${SERVICES.find((x) => x.id === doc.search?.provider)?.name ?? ''}` : 'Not in use'}
          </span>
        </div>

        <fieldset className="search-services" aria-label="Search service">
          <legend className="search-services-legend">Search service</legend>
          <div className="search-service-grid">
            {SERVICES.map((s) => (
              <label key={s.id} className={`search-service${search.provider === s.id ? ' search-service-on' : ''}`}>
                <input
                  type="radio"
                  name="search-provider"
                  value={s.id}
                  checked={search.provider === s.id}
                  onChange={() => {
                    setKey('');
                    setSearch({ provider: s.id });
                  }}
                />
                <span className="search-service-head">
                  <span className="search-service-check" aria-hidden="true" />
                  <strong>{s.name}</strong>
                </span>
                <span>
                  {s.id === 'searxng' && doc.search?.deployed ? (
                    <Badge tone="success">Installed with Janus</Badge>
                  ) : doc.search_keys_set?.[s.id] ? (
                    <Badge tone="success">Key saved</Badge>
                  ) : (
                    <Badge tone="neutral">{s.needsKey ? 'Needs a key' : 'No key needed'}</Badge>
                  )}
                </span>
                <span className="small secondary">{s.tagline}</span>
                <span className="small muted search-service-price">{s.price}</span>
              </label>
            ))}
          </div>
        </fieldset>

        {service ? (
          <div className="search-setup">
            <div className="search-setup-steps">
              {service.id === 'searxng' && doc.search?.deployed ? (
                <>
                  <h3 className="search-setup-title">Installed with Janus</h3>
                  <p className="small secondary" style={{ margin: 0 }}>
                    This deployment runs its own private SearXNG at <code>{doc.search.deployed}</code>, reachable only by Janus.
                    Nothing to set up: use Test search to check it, or turn it off below.
                  </p>
                </>
              ) : (
                <>
                  <h3 className="search-setup-title">Set up {service.name}</h3>
                  <ol>
                    {service.steps.map((st) => (
                      <li key={st}>{st}</li>
                    ))}
                  </ol>
                </>
              )}
              {service.keyUrl ? (
                <a className="btn btn-sm" href={service.keyUrl} target="_blank" rel="noreferrer noopener">
                  {service.needsKey ? `Get a ${service.name} key` : 'SearXNG install guide'}
                  <span aria-hidden="true"> ↗</span>
                </a>
              ) : null}
            </div>
            <div className="stack" style={{ gap: 'var(--janus-space-3)' }}>
              <h3 className="search-setup-title">Connect</h3>
              {service.needsKey ? (
                <Field
                  label="API key"
                  hint={keySet ? 'A key is saved. Paste a new one to replace it.' : 'Stored encrypted. It is never shown again.'}
                >
                  <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
                    <input
                      className="input"
                      type="password"
                      autoComplete="off"
                      spellCheck={false}
                      aria-label="API key"
                      placeholder={keySet ? '•••••••• saved' : `Paste your ${service.name} key`}
                      value={key}
                      onChange={(e) => setKey(e.target.value)}
                    />
                    {keySet ? (
                      <button
                        type="button"
                        className="btn btn-ghost btn-sm"
                        onClick={() => save.mutate({ ...draft, search: { ...search, enabled: false }, search_key_clear: true })}
                      >
                        Remove key
                      </button>
                    ) : null}
                  </div>
                </Field>
              ) : (
                <Field label="SearXNG address" hint="The instance's base URL, reachable from Janus.">
                  <input
                    className="input"
                    type="url"
                    aria-label="SearXNG address"
                    placeholder="http://searxng.search.svc:8080"
                    value={search.url}
                    onChange={(e) => setSearch({ url: e.target.value })}
                  />
                </Field>
              )}
              <Field label="Results per search" hint="More results give the model more to work with but cost more tokens.">
                <select
                  className="select"
                  aria-label="Results per search"
                  value={search.max_results}
                  onChange={(e) => setSearch({ max_results: Number(e.target.value) })}
                  style={{ maxWidth: 160 }}
                >
                  {[3, 5, 8, 10].map((n) => (
                    <option key={n} value={n}>
                      {n}
                    </option>
                  ))}
                </select>
              </Field>
              <div className="row" style={{ gap: 'var(--janus-space-3)', alignItems: 'center', flexWrap: 'wrap' }}>
                <button type="button" className="btn btn-sm" disabled={!!missing || test.isPending} onClick={() => test.mutate()}>
                  {test.isPending ? 'Searching…' : 'Test search'}
                </button>
                {missing ? <span className="small muted">{missing}</span> : null}
                {test.data?.ok ? (
                  <span className="small" style={{ color: 'var(--janus-color-success-fg)' }} role="status">
                    Works — {test.data.results?.length ?? 0} results in {test.data.ms} ms
                  </span>
                ) : null}
                {test.data && !test.data.ok ? (
                  <span className="small" style={{ color: 'var(--janus-color-danger-fg)' }} role="alert">
                    {test.data.error}
                  </span>
                ) : null}
                {test.error ? (
                  <span className="small" style={{ color: 'var(--janus-color-danger-fg)' }} role="alert">
                    {(test.error as Error).message}
                  </span>
                ) : null}
              </div>
              <div className="search-enable">
                <ToggleRow
                  label="Use this search for models without their own"
                  hint={
                    missing
                      ? `${missing} Then turn this on.`
                      : 'Models that can call tools will search when they need current information.'
                  }
                  checked={search.enabled}
                  disabled={!!missing && !search.enabled}
                  onChange={(v) => setSearch({ enabled: v })}
                />
              </div>
              {test.data?.ok && test.data.results?.length ? (
                <ul className="search-test-results">
                  {test.data.results.slice(0, 3).map((r) => (
                    <li key={r.url}>
                      <a href={r.url} target="_blank" rel="noreferrer noopener">
                        {r.title || r.url}
                      </a>
                      <span className="small muted">{new URL(r.url).hostname.replace(/^www\./, '')}</span>
                    </li>
                  ))}
                </ul>
              ) : null}
            </div>
          </div>
        ) : (
          <p className="small muted" style={{ margin: 0 }}>
            Choose a service to see how to set it up.
          </p>
        )}
      </section>

      <SaveBar
        doc={doc}
        busy={save.isPending}
        dirty={dirty}
        onReset={() => {
          setDraft(doc);
          setKey('');
        }}
      />
    </form>
  );
}
