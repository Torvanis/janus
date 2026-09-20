import { useEffect, useMemo, useState, type ReactNode } from 'react';
import { Link, NavLink, Route, Routes, useLocation, useNavigate, useParams, useSearchParams } from 'react-router-dom';
import { api } from '../../lib/api';
import { useSession } from '../../app/session';
import { Badge, CodeBlock, EmptyState, useToast } from '../../components/ui';
import { ERROR_CATALOG, GLOSSARY, PAGES, pageBySlug, type DocPage, type PricingRow, type PricingTable } from './content';
import './docs.css';

/**
 * The documentation site.
 *
 * It is reachable without signing in so integration steps can be linked from a
 * wiki or a chat channel and read before anyone has an account. Content ships
 * inside the binary, so what a reader sees always matches the running build.
 */
export default function DocsRoutes(): ReactNode {
  return (
    <DocsShell>
      <Routes>
        <Route index element={<DocsHome />} />
        <Route path="search" element={<DocsSearch />} />
        <Route path="api/errors" element={<ErrorCatalog />} />
        <Route path="glossary" element={<Glossary />} />
        <Route path="*" element={<DocsPageRoute />} />
      </Routes>
    </DocsShell>
  );
}

function DocsShell({ children }: { children: ReactNode }): ReactNode {
  const { me, config } = useSession();
  const location = useLocation();
  const [navOpen, setNavOpen] = useState(false);

  useEffect(() => setNavOpen(false), [location.pathname]);

  const groups = useMemo(() => {
    const byGroup = new Map<string, DocPage[]>();
    for (const page of PAGES) {
      byGroup.set(page.group, [...(byGroup.get(page.group) ?? []), page]);
    }
    return [...byGroup.entries()];
  }, []);

  return (
    <div className="docs">
      <a className="skip-link" href="#docs-main">
        Skip to content
      </a>
      <header className="docs-topbar">
        <Link to="/docs" className="docs-brand">
          Janus <span className="muted">docs</span>
        </Link>
        <button type="button" className="btn btn-ghost btn-sm docs-nav-toggle" onClick={() => setNavOpen((open) => !open)}>
          Contents
        </button>
        <div className="row" style={{ marginLeft: 'auto', gap: 'var(--janus-space-2)' }}>
          {config ? (
            <span className="small muted mono hide-xs">
              {config.version} · {config.build.slice(0, 7)}
            </span>
          ) : null}
          <Link className="btn btn-sm" to={me ? '/dashboard' : '/auth/login'}>
            {me ? 'Back to app' : 'Sign in'}
          </Link>
        </div>
      </header>

      <div className="docs-body">
        <nav className={`docs-nav${navOpen ? ' docs-nav-open' : ''}`} aria-label="Documentation">
          <DocsSearchBox />
          <NavLink to="/docs" end className={({ isActive }) => `docs-link${isActive ? ' docs-link-active' : ''}`}>
            Home
          </NavLink>
          {groups.map(([group, pages]) => (
            <section key={group}>
              <div className="nav-heading">{group}</div>
              {pages.map((page) => (
                <NavLink
                  key={page.slug}
                  to={`/docs/${page.slug}`}
                  className={({ isActive }) => `docs-link${isActive ? ' docs-link-active' : ''}`}
                >
                  {page.title}
                </NavLink>
              ))}
            </section>
          ))}
          <section>
            <div className="nav-heading">Reference</div>
            <NavLink to="/docs/api/errors" className={({ isActive }) => `docs-link${isActive ? ' docs-link-active' : ''}`}>
              Error catalog
            </NavLink>
            <NavLink to="/docs/glossary" className={({ isActive }) => `docs-link${isActive ? ' docs-link-active' : ''}`}>
              Glossary
            </NavLink>
          </section>
        </nav>

        <main id="docs-main" className="docs-main">
          {children}
        </main>
      </div>
    </div>
  );
}

function DocsSearchBox(): ReactNode {
  const [query, setQuery] = useState('');
  const navigate = useNavigate();
  return (
    <form
      className="row"
      style={{ marginBottom: 'var(--janus-space-3)' }}
      onSubmit={(event) => {
        event.preventDefault();
        // Client-side navigation: a window.location.assign here reloads the
        // whole document and drops SPA state (nav, scroll position).
        navigate(`/docs/search?q=${encodeURIComponent(query)}`);
      }}
    >
      <input
        className="input"
        type="search"
        value={query}
        placeholder="Search docs"
        aria-label="Search documentation"
        onChange={(event) => setQuery(event.target.value)}
      />
    </form>
  );
}

function DocsHome(): ReactNode {
  const { me } = useSession();
  return (
    <article className="docs-article">
      <h1>Janus documentation</h1>
      <p className="secondary">
        Janus is an authenticated, metered gateway between your people and the AI providers your organisation approves. It speaks
        the OpenAI API, so any compatible tool works unchanged.
      </p>

      {!me ? (
        <div className="banner banner-info">
          <div>
            You are reading this signed out. <Link to="/auth/login">Sign in</Link> to get snippets pre-filled with your own
            endpoint and token.
          </div>
        </div>
      ) : null}

      <div className="grid grid-tiles" style={{ marginTop: 'var(--janus-space-6)' }}>
        {[
          { to: '/docs/getting-started', title: 'Get started', body: 'Sign in, create a token, make a metered call.' },
          { to: '/docs/guides/quotas', title: 'User guide', body: 'Tokens, models, quotas, and streaming.' },
          { to: '/docs/admin/upstreams', title: 'Admin guide', body: 'Upstreams, grants, quotas, policy, operations.' },
          { to: '/docs/reference/api', title: 'API reference', body: 'All three surfaces, plus the OpenAPI 3.1 download.' },
          { to: '/docs/api/errors', title: 'Error catalog', body: 'Every error code with its cause and fix.' },
          { to: '/docs/troubleshooting', title: 'Troubleshooting', body: 'Symptom-first diagnosis.' },
          { to: '/docs/faq', title: 'FAQ', body: 'Short answers to the questions asked first.' },
          { to: '/docs/glossary', title: 'Glossary', body: 'The vocabulary used across the product.' },
        ].map((tile) => (
          <Link key={tile.to} to={tile.to} className="tile" style={{ textDecoration: 'none', display: 'block' }}>
            <h2 style={{ fontSize: 'var(--janus-text-lg)' }}>{tile.title}</h2>
            <p className="small muted" style={{ margin: '6px 0 0' }}>
              {tile.body}
            </p>
          </Link>
        ))}
      </div>
    </article>
  );
}

interface SearchDoc {
  to: string;
  title: string;
  excerpt: string;
  kind: string;
  /** Tokenized fields with their ranking weight. */
  fields: Array<{ tokens: Set<string>; weight: number }>;
}

function tokenize(text: string): Set<string> {
  return new Set(
    text
      .toLowerCase()
      .split(/[^a-z0-9_./-]+/)
      .filter((token) => token.length > 1),
  );
}

/** Build the search index once per session — every doc surface, tokenized with field weights. */
function buildSearchIndex(): SearchDoc[] {
  const docs: SearchDoc[] = [];
  for (const page of PAGES) {
    docs.push({
      to: `/docs/${page.slug}`,
      title: page.title,
      excerpt: page.summary,
      kind: page.group,
      fields: [
        { tokens: tokenize(page.title), weight: 5 },
        { tokens: tokenize(page.summary), weight: 3 },
        { tokens: tokenize(page.sections.map((s) => s.heading).join(' ')), weight: 2 },
        {
          tokens: tokenize(page.sections.map((s) => `${s.body.join(' ')} ${s.code?.code ?? ''}`).join(' ')),
          weight: 1,
        },
      ],
    });
  }
  for (const entry of ERROR_CATALOG) {
    docs.push({
      to: `/docs/api/errors#${entry.code}`,
      title: entry.code,
      excerpt: entry.when,
      kind: 'Error',
      fields: [
        { tokens: tokenize(entry.code), weight: 5 },
        { tokens: tokenize(`${entry.when} ${entry.action}`), weight: 1 },
      ],
    });
  }
  for (const entry of GLOSSARY) {
    docs.push({
      to: `/docs/glossary#${slugify(entry.term)}`,
      title: entry.term,
      excerpt: entry.definition,
      kind: 'Glossary',
      fields: [
        { tokens: tokenize(entry.term), weight: 5 },
        { tokens: tokenize(entry.definition), weight: 1 },
      ],
    });
  }
  return docs;
}

/**
 * Score one document against the query terms. Every term must match somewhere
 * (AND semantics); a term matches a field when any token starts with it, so
 * partial words like "quot" still find "quotas".
 */
function scoreDoc(doc: SearchDoc, terms: string[]): number {
  let total = 0;
  for (const term of terms) {
    let best = 0;
    for (const field of doc.fields) {
      if (field.weight <= best) continue;
      for (const token of field.tokens) {
        if (token === term) {
          best = Math.max(best, field.weight * 2);
          break;
        }
        if (token.startsWith(term)) {
          best = Math.max(best, field.weight);
        }
      }
    }
    if (best === 0) return 0;
    total += best;
  }
  return total;
}

function DocsSearch(): ReactNode {
  const [params] = useSearchParams();
  const query = (params.get('q') ?? '').trim().toLowerCase();
  const index = useMemo(buildSearchIndex, []);

  const results = useMemo(() => {
    const terms = [...tokenize(query)];
    if (terms.length === 0) return [];
    return index
      .map((doc) => ({ doc, score: scoreDoc(doc, terms) }))
      .filter((hit) => hit.score > 0)
      .sort((a, b) => b.score - a.score)
      .map(({ doc }) => ({ to: doc.to, title: doc.title, excerpt: doc.excerpt, kind: doc.kind }));
  }, [query, index]);

  return (
    <article className="docs-article">
      <h1>Search</h1>
      <p className="secondary">
        {query ? `${results.length} result${results.length === 1 ? '' : 's'} for “${query}”.` : 'Type a term to search.'}
      </p>
      {query && results.length === 0 ? (
        <EmptyState
          title="Nothing matched"
          body="Try a shorter term, or browse the quick start and error catalog."
          action={
            <Link className="btn" to="/docs/getting-started">
              Quick start
            </Link>
          }
        />
      ) : (
        <ul className="stack" style={{ listStyle: 'none', padding: 0 }}>
          {results.map((hit) => (
            <li key={`${hit.to}-${hit.title}`} className="card">
              <div className="row-between">
                <Link to={hit.to} style={{ fontWeight: 600 }}>
                  {hit.title}
                </Link>
                <Badge tone="neutral">{hit.kind}</Badge>
              </div>
              <p className="small muted" style={{ margin: '6px 0 0' }}>
                {hit.excerpt}
              </p>
            </li>
          ))}
        </ul>
      )}
    </article>
  );
}

function DocsPageRoute(): ReactNode {
  const params = useParams();
  const slug = params['*'] ?? '';
  const page = pageBySlug(slug);

  if (!page) {
    return (
      <article className="docs-article">
        <EmptyState
          title="That documentation page doesn’t exist"
          body="It may have been renamed. Browse the contents, or search for what you need."
          action={
            <Link className="btn btn-primary" to="/docs">
              Documentation home
            </Link>
          }
        />
      </article>
    );
  }

  return (
    <article className="docs-article">
      <div className="overline">{page.group}</div>
      <h1>{page.title}</h1>
      <p className="secondary">{page.summary}</p>

      {page.sections.map((section) => (
        <section key={section.heading} id={slugify(section.heading)}>
          <h2>{section.heading}</h2>
          {section.body.map((paragraph, index) => (
            <p key={index}>{paragraph}</p>
          ))}
          {section.code ? <CodeBlock code={section.code.code} language={section.code.language} /> : null}
          {section.pricing ? <PricingTableView table={section.pricing} /> : null}
        </section>
      ))}

      <FeedbackWidget page={page.slug} />
    </article>
  );
}

/** Modality pills share a tone per kind so Audio reads the same in every table. */
function pricingTagTone(tag: string): 'primary' | 'info' | 'warning' | 'neutral' | 'success' {
  switch (tag.toLowerCase()) {
    case 'audio':
      return 'warning';
    case 'image':
      return 'primary';
    case 'text':
      return 'info';
    case 'chars':
      return 'success';
    default:
      return 'neutral';
  }
}

/** True for cells that read as a figure (so they right-align in a tabular font). */
const isFigure = (cell: string): boolean => /^[\d.,]+(\s|$)/.test(cell) || cell === '—';

/**
 * A pricing table whose layout follows the modality's billing model — see
 * PricingVariant in content.ts. Grouped rows (image, realtime, voice) render
 * as one block per model with a modality pill on each sub-row; tabs switch
 * between price tiers (Standard / Batch); the highlighted cell is the figure
 * an administrator copies into the Janus rate card.
 */
function PricingTableView({ table }: { table: PricingTable }): ReactNode {
  const [tab, setTab] = useState(0);
  const rows: PricingRow[] = table.tabs ? (table.tabs[tab]?.rows ?? []) : (table.rows ?? []);
  const grouped = rows.some((row) => row.group);
  return (
    <figure className={`docs-pricing docs-pricing-${table.variant}`} data-variant={table.variant}>
      <figcaption className="docs-pricing-caption">
        <div className="docs-pricing-title">
          <strong>{table.caption}</strong>
          <span className="docs-pricing-unit">{table.unit}</span>
        </div>
        {table.tabs ? (
          <div className="segmented docs-pricing-tabs" role="tablist" aria-label={`${table.caption} price tier`}>
            {table.tabs.map((entry, index) => (
              <button
                key={entry.label}
                type="button"
                role="tab"
                aria-selected={index === tab}
                aria-pressed={index === tab}
                onClick={() => setTab(index)}
              >
                {entry.label}
              </button>
            ))}
          </div>
        ) : null}
      </figcaption>
      <div className="table-wrap docs-pricing-wrap">
        <table className="data docs-pricing-table">
          <thead>
            <tr>
              {table.columns.map((column, index) => (
                <th key={column} scope="col" className={index > 0 ? 'docs-pricing-figure' : undefined}>
                  {column}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {rows.map((row, rowIndex) => {
              const previous = rows[rowIndex - 1];
              const startsGroup = Boolean(row.group) && row.group !== previous?.group;
              const isSub = Boolean(row.group) && !startsGroup;
              return (
                <tr
                  key={`${row.group ?? ''}:${row.tag ?? ''}:${rowIndex}`}
                  className={[
                    grouped ? 'docs-pricing-row' : '',
                    startsGroup ? 'docs-pricing-group-start' : '',
                    isSub ? 'docs-pricing-sub' : '',
                  ]
                    .filter(Boolean)
                    .join(' ')}
                >
                  {row.cells.map((cell, cellIndex) => {
                    const highlight = row.highlight === cellIndex;
                    if (cellIndex === 0) {
                      return (
                        <th key={cellIndex} scope="row" className="docs-pricing-label">
                          <span className="docs-pricing-label-inner">
                            {cell ? <span className="mono docs-pricing-model">{cell}</span> : null}
                            {row.tag ? <Badge tone={pricingTagTone(row.tag)}>{row.tag}</Badge> : null}
                          </span>
                        </th>
                      );
                    }
                    return (
                      <td
                        key={cellIndex}
                        className={[isFigure(cell) ? 'docs-pricing-figure' : '', highlight ? 'docs-pricing-highlight' : '']
                          .filter(Boolean)
                          .join(' ')}
                      >
                        {cell}
                      </td>
                    );
                  })}
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      {table.footnote ? <p className="small secondary docs-pricing-footnote">{table.footnote}</p> : null}
    </figure>
  );
}

function ErrorCatalog(): ReactNode {
  const location = useLocation();

  useEffect(() => {
    // Deep links from every error surface in the app land on a specific code.
    if (location.hash) {
      document.getElementById(location.hash.slice(1))?.scrollIntoView({ block: 'center' });
    }
  }, [location.hash]);

  return (
    <article className="docs-article">
      <div className="overline">Reference</div>
      <h1>Error catalog</h1>
      <p className="secondary">
        Every failure Janus originates carries a stable <code>code</code>. Branch on the code, not on the message text. Errors
        from a provider are passed through unchanged.
      </p>

      <div className="stack">
        {ERROR_CATALOG.map((entry) => (
          <section key={entry.code} id={entry.code} className="card">
            <div className="row-between">
              <h2 className="mono" style={{ fontSize: 'var(--janus-text-md)' }}>
                {entry.code}
              </h2>
              <div className="row">
                <Badge tone={entry.status >= 500 ? 'danger' : entry.status >= 400 ? 'warning' : 'neutral'}>
                  HTTP {entry.status}
                </Badge>
                <Badge tone={entry.retryable ? 'success' : 'neutral'}>{entry.retryable ? 'Retryable' : 'Not retryable'}</Badge>
              </div>
            </div>
            <p className="small secondary" style={{ marginTop: 8 }}>
              <strong>When:</strong> {entry.when}
            </p>
            <p className="small secondary" style={{ marginBottom: 0 }}>
              <strong>What to do:</strong> {entry.action}
            </p>
          </section>
        ))}
      </div>

      <FeedbackWidget page="api/errors" />
    </article>
  );
}

function Glossary(): ReactNode {
  return (
    <article className="docs-article">
      <div className="overline">Reference</div>
      <h1>Glossary</h1>
      <dl className="stack">
        {GLOSSARY.map((entry) => (
          <div key={entry.term} id={slugify(entry.term)} className="card">
            <dt style={{ fontWeight: 600 }}>{entry.term}</dt>
            <dd className="small secondary" style={{ margin: '6px 0 0' }}>
              {entry.definition}
            </dd>
          </div>
        ))}
      </dl>
      <FeedbackWidget page="glossary" />
    </article>
  );
}

/** Anonymous page-level feedback. It never blocks reading and stores no identity. */
function FeedbackWidget({ page }: { page: string }): ReactNode {
  const [sent, setSent] = useState(false);
  const [note, setNote] = useState('');
  const [showNote, setShowNote] = useState(false);
  const toast = useToast();

  const send = async (helpful: boolean, text = '') => {
    try {
      await api.post('/api/v1/docs/feedback', { page, helpful, note: text });
      setSent(true);
    } catch {
      // Feedback is best-effort; never surface a failure as a blocking error.
      toast('Thanks — we couldn’t record that just now.', 'warning');
      setSent(true);
    }
  };

  if (sent) {
    return <p className="small muted docs-feedback">Thanks for the feedback.</p>;
  }

  return (
    <div className="docs-feedback">
      <span className="small muted">Was this page helpful?</span>
      <div className="row">
        <button type="button" className="btn btn-sm" onClick={() => void send(true)}>
          Yes
        </button>
        <button type="button" className="btn btn-sm" onClick={() => setShowNote(true)}>
          No
        </button>
      </div>
      {showNote ? (
        <form
          className="stack"
          style={{ marginTop: 'var(--janus-space-3)', width: '100%' }}
          onSubmit={(event) => {
            event.preventDefault();
            void send(false, note);
          }}
        >
          <textarea
            className="textarea"
            value={note}
            onChange={(event) => setNote(event.target.value)}
            placeholder="What was missing or unclear? (optional)"
            aria-label="Feedback note"
            maxLength={1000}
          />
          <button type="submit" className="btn btn-sm">
            Send feedback
          </button>
        </form>
      ) : null}
    </div>
  );
}

function slugify(value: string): string {
  return value
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-|-$/g, '');
}
