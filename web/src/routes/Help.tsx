import { useState, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { useQuery } from '@tanstack/react-query';
import { api } from '../lib/api';
import { AsyncSection, Badge, CodeBlock, EmptyState } from '../components/ui';
import { useUrlState } from '../lib/hooks';
import { useMe } from '../app/session';
import { t } from '../lib/i18n';

interface HelpSnippet {
  language: string;
  label: string;
  code: string;
}

interface HelpTopic {
  id: string;
  title: string;
  body?: string;
  docs_url?: string;
  fields?: Array<{ label: string; value: string }>;
  snippets?: HelpSnippet[];
  errors?: Array<{ code: string; status: string; fix: string }>;
  terms?: Array<{ term: string; definition: string }>;
  topics: Array<{ id: string; title: string; summary: string }>;
  has_active_token: boolean;
}

export function HelpPage(): ReactNode {
  const me = useMe();
  const [topic, setTopic] = useUrlState('topic', 'integration');
  const [snippetIndex, setSnippetIndex] = useState(0);

  const help = useQuery({
    queryKey: ['help', topic],
    queryFn: () => api.get<HelpTopic>(`/api/v1/help/${topic}`),
  });

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">{t('help.title')}</h1>
          <p className="page-subtitle">
            {t('help.subtitlePrefix')} <Link to="/docs">{t('help.subtitleLink')}</Link>.
          </p>
        </div>
      </header>

      <div className="grid help-layout">
        <nav className="card" aria-label={t('help.topicsNav')} style={{ padding: 'var(--janus-space-2)' }}>
          <ul className="stack" style={{ listStyle: 'none', margin: 0, padding: 0, gap: 2 }}>
            {(help.data?.topics ?? []).map((item) => (
              <li key={item.id}>
                <button
                  type="button"
                  className={`nav-item${topic === item.id ? ' nav-item-active' : ''}`}
                  style={{ width: '100%', border: 'none', cursor: 'pointer', textAlign: 'left', font: 'inherit' }}
                  onClick={() => {
                    setTopic(item.id);
                    setSnippetIndex(0);
                  }}
                  aria-current={topic === item.id ? 'page' : undefined}
                >
                  <span className="nav-label">{item.title}</span>
                </button>
              </li>
            ))}
          </ul>
        </nav>

        <div className="stack" style={{ gap: 'var(--janus-space-4)', minWidth: 0 }}>
          <AsyncSection query={help}>
            {(data) => (
              <>
                {!data.has_active_token && data.id === 'integration' ? (
                  <div className="banner banner-warning">
                    <div>
                      <strong>{t('help.noTokenTitle')}</strong>
                      <div className="small" style={{ marginTop: 4 }}>
                        {t('help.noTokenBodyPrefix')} <Link to="/tokens">{t('help.noTokenLink')}</Link>{' '}
                        {t('help.noTokenBodySuffix')} <code>$JANUS_API_KEY</code>.
                      </div>
                    </div>
                  </div>
                ) : null}

                <section className="card">
                  <div className="card-header">
                    <h2>{data.title}</h2>
                    {data.docs_url ? (
                      <Link className="small" to={data.docs_url}>
                        {t('help.readFullDocs')}
                      </Link>
                    ) : null}
                  </div>
                  {data.body ? <p className="secondary">{data.body}</p> : null}

                  {data.fields ? (
                    <dl className="stack" style={{ gap: 8 }}>
                      {data.fields.map((field) => (
                        <div key={field.label} className="row-between">
                          <dt className="small muted">{field.label}</dt>
                          <dd className="mono small" style={{ margin: 0 }}>
                            {field.value}
                          </dd>
                        </div>
                      ))}
                    </dl>
                  ) : null}
                </section>

                {data.snippets && data.snippets.length > 0 ? (
                  <section className="card">
                    <div className="card-header">
                      <h2>{t('help.copyPasteSetup')}</h2>
                      <span className="small muted">{t('help.endpointLabel', { name: me?.endpoint ?? '' })}</span>
                    </div>
                    <div
                      className="segmented"
                      role="tablist"
                      aria-label={t('help.snippetLanguage')}
                      style={{ marginBottom: 'var(--janus-space-3)' }}
                    >
                      {data.snippets.map((snippet, index) => (
                        <button
                          key={snippet.label}
                          type="button"
                          role="tab"
                          aria-selected={snippetIndex === index}
                          aria-pressed={snippetIndex === index}
                          onClick={() => setSnippetIndex(index)}
                        >
                          {snippet.label}
                        </button>
                      ))}
                    </div>
                    <CodeBlock
                      code={data.snippets[Math.min(snippetIndex, data.snippets.length - 1)]?.code ?? ''}
                      language={data.snippets[Math.min(snippetIndex, data.snippets.length - 1)]?.language}
                    />
                  </section>
                ) : null}

                {data.errors ? (
                  <section className="card card-flush">
                    <div className="card-header" style={{ padding: 'var(--janus-card-padding)', marginBottom: 0 }}>
                      <h2>{t('help.errorCodes')}</h2>
                    </div>
                    <div className="table-wrap">
                      <table className="data">
                        <thead>
                          <tr>
                            <th scope="col">{t('help.colCode')}</th>
                            <th scope="col">{t('help.colHttp')}</th>
                            <th scope="col">{t('help.colWhatToDo')}</th>
                          </tr>
                        </thead>
                        <tbody>
                          {data.errors.map((item) => (
                            <tr key={item.code}>
                              <td className="mono small">
                                <Link to={`/docs/api/errors#${item.code}`}>{item.code}</Link>
                              </td>
                              <td>
                                <Badge tone={item.status.startsWith('4') ? 'warning' : 'danger'}>{item.status}</Badge>
                              </td>
                              <td className="small">{item.fix}</td>
                            </tr>
                          ))}
                        </tbody>
                      </table>
                    </div>
                  </section>
                ) : null}

                {data.terms ? (
                  <section className="card">
                    <div className="card-header">
                      <h2>{t('help.terms')}</h2>
                    </div>
                    <dl className="stack" style={{ gap: 'var(--janus-space-3)' }}>
                      {data.terms.map((entry) => (
                        <div key={entry.term}>
                          <dt style={{ fontWeight: 600 }}>{entry.term}</dt>
                          <dd className="small secondary" style={{ margin: 0 }}>
                            {entry.definition}
                          </dd>
                        </div>
                      ))}
                    </dl>
                  </section>
                ) : null}

                {!data.body && !data.snippets && !data.errors && !data.terms ? (
                  <EmptyState title={t('help.emptyTitle')} body={t('help.emptyBody')} />
                ) : null}
              </>
            )}
          </AsyncSection>
        </div>
      </div>
    </div>
  );
}
