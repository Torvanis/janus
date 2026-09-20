import { useState, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../lib/api';
import type {
  DiscoveryInterval,
  DiscoveryIntervalPatch,
  MeteringHealth,
  SystemStatus,
  UpstreamTimeoutValues,
  UpstreamTimeouts,
  UpstreamTimeoutsPatch,
} from '../../lib/types';
import { formatDateTime, formatDuration, formatNumber, formatRelative, titleCase } from '../../lib/format';
import { AsyncSection, Badge, ConfirmDialog, Field, useToast } from '../../components/ui';
import { DetailRow } from '../shared';
import { t } from '../../lib/i18n';
import { TroubleshootingCard } from './TroubleshootingCard';
import { LicenseCard } from './LicenseCard';
import { RetainedPanel } from '../../components/RouteTopNav';

/**
 * Label/description/consequence copy for each feature flag on the
 * /admin/system panel. Keep this switch in sync with
 * store.DefaultFeatureFlags: a flag without a case here still renders and
 * toggles, but falls back to a titleCase label with no description or
 * consequence text in the confirm dialog.
 */
function flagCopy(name: string): { label: string; description: string; consequence: string } | undefined {
  switch (name) {
    case 'leaderboards_enabled':
      return {
        label: t('adminSystem.flags.leaderboardsLabel'),
        description: t('adminSystem.flags.leaderboardsDescription'),
        consequence: t('adminSystem.flags.leaderboardsConsequence'),
      };
    case 'per_user_rate_limits_enabled':
      return {
        label: t('adminSystem.flags.rateLimitsLabel'),
        description: t('adminSystem.flags.rateLimitsDescription'),
        consequence: t('adminSystem.flags.rateLimitsConsequence'),
      };
    case 'reduce_motion_preferred':
      return {
        label: t('adminSystem.flags.reduceMotionLabel'),
        description: t('adminSystem.flags.reduceMotionDescription'),
        consequence: t('adminSystem.flags.reduceMotionConsequence'),
      };
    case 'otel_enabled':
      return {
        label: t('adminSystem.flags.otelLabel'),
        description: t('adminSystem.flags.otelDescription'),
        consequence: t('adminSystem.flags.otelConsequence'),
      };
    case 'spend_emphasis':
      // Spend vs usage emphasis: copy lives under adminSystem.flags.spendEmphasis*
      // in i18n.ts; the toggle goes through the same confirm-dialog + audit flow
      // as every other flag rendered by this panel.
      return {
        label: t('adminSystem.flags.spendEmphasisLabel'),
        description: t('adminSystem.flags.spendEmphasisDescription'),
        consequence: t('adminSystem.flags.spendEmphasisConsequence'),
      };
    default:
      return undefined;
  }
}

export function SystemPage({
  section = 'all',
}: {
  section?: 'all' | 'general' | 'status' | 'troubleshooting' | 'hidden';
}): ReactNode {
  const visible = (name: string) => section === 'all' || section === name;
  const queryClient = useQueryClient();
  const toast = useToast();
  const [pendingFlag, setPendingFlag] = useState<{ name: string; value: boolean } | null>(null);

  const status = useQuery({
    queryKey: ['admin', 'system'],
    queryFn: () => api.get<SystemStatus>('/api/v1/admin/system/status'),
    refetchInterval: 20_000,
  });

  const setFlag = useMutation({
    mutationFn: ({ name, value }: { name: string; value: boolean }) => api.patch('/api/v1/admin/features', { [name]: value }),
    onSuccess: () => {
      toast(t('adminSystem.flagUpdatedToast'));
      void queryClient.invalidateQueries({ queryKey: ['admin', 'system'] });
      void queryClient.invalidateQueries({ queryKey: ['me'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const resolve = useMutation({
    mutationFn: (id: string) => api.post(`/api/v1/admin/docs-feedback/${id}/resolve`),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ['admin', 'system'] }),
  });

  return (
    <div className={section === 'all' ? 'page' : 'stack'}>
      {section === 'all' && (
        <header className="page-header">
          <div>
            <h1 className="page-title">{t('adminSystem.title')}</h1>
            <p className="page-subtitle">{t('adminSystem.subtitle')}</p>
          </div>
          <button type="button" className="btn" onClick={() => void status.refetch()}>
            {t('adminSystem.refresh')}
          </button>
        </header>
      )}

      {section !== 'all' && (
        <div className="row-between">
          <h2>{section === 'general' ? 'General' : section === 'status' ? 'System status' : 'Troubleshooting'}</h2>
          <button className="btn" onClick={() => void status.refetch()}>
            {t('adminSystem.refresh')}
          </button>
        </div>
      )}
      <AsyncSection query={status}>
        {(data) => (
          <>
            <RetainedPanel active={visible('status')}>
              <div className="grid grid-tiles">
                <DatabaseTile database={data.database} />
                <HealthTile
                  label={t('adminSystem.identityProvider')}
                  ok={data.identity_provider.ok}
                  detail={data.identity_provider.dev_auth ? t('adminSystem.devAuthDetail') : data.identity_provider.provider}
                  warn={data.identity_provider.dev_auth}
                />
                <HealthTile label={t('adminSystem.quotaLedger')} ok={data.quota_ledger.ok} detail={data.quota_ledger.mode} />
                <HealthTile
                  label={t('adminSystem.emailDelivery')}
                  ok={data.email.ok}
                  detail={data.email.detail}
                  warn={!data.email.ok}
                />
                <MeteringTile metering={data.metering} />
              </div>

              <section className="card card-flush">
                <div className="card-header" style={{ padding: 'var(--janus-card-padding)', marginBottom: 0 }}>
                  <h2>{t('adminSystem.upstreams')}</h2>
                  <span className="small muted">
                    {t('adminSystem.discoveryMeta', {
                      count: data.discovery.interval_minutes,
                      time: data.discovery.last_run_at ? formatRelative(data.discovery.last_run_at) : t('adminSystem.notYet'),
                    })}
                  </span>
                </div>
                {data.upstreams.length === 0 ? (
                  <p className="small muted" style={{ padding: 'var(--janus-card-padding)' }}>
                    {t('adminSystem.noUpstreams')}
                  </p>
                ) : (
                  <div className="table-wrap">
                    <table className="data">
                      <thead>
                        <tr>
                          <th scope="col">{t('adminModels.colUpstream')}</th>
                          <th scope="col">{t('adminUpstreams.colProvider')}</th>
                          <th scope="col">{t('adminSystem.colState')}</th>
                          <th scope="col">{t('tables.latency')}</th>
                          <th scope="col">{t('adminUpstreams.colModels')}</th>
                          <th scope="col">{t('adminSystem.colLastCheck')}</th>
                        </tr>
                      </thead>
                      <tbody>
                        {data.upstreams.map((upstream) => (
                          <tr key={upstream.id}>
                            <td>{upstream.name}</td>
                            <td className="small muted">{upstream.adapter_type}</td>
                            <td>
                              {!upstream.enabled ? (
                                <Badge tone="neutral">{t('tables.disabled')}</Badge>
                              ) : upstream.reachable ? (
                                <Badge tone="success" dot>
                                  {t('adminSystem.reachable')}
                                </Badge>
                              ) : (
                                <Badge tone="danger" dot>
                                  {upstream.last_error ? t('adminSystem.error') : t('adminSystem.notChecked')}
                                </Badge>
                              )}
                            </td>
                            <td className="num small">{upstream.latency_ms ? formatDuration(upstream.latency_ms) : '—'}</td>
                            <td className="num small">{upstream.model_count}</td>
                            <td className="small muted" title={upstream.last_error}>
                              {upstream.last_check_at ? formatRelative(upstream.last_check_at) : t('adminSystem.never')}
                              {upstream.last_error ? (
                                <div className="small" style={{ color: 'var(--janus-color-danger-fg)' }}>
                                  {upstream.last_error}
                                </div>
                              ) : null}
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}
              </section>
            </RetainedPanel>
            <RetainedPanel active={visible('general')}>
              <section className="card" id="feature-flags">
                <div className="card-header">
                  <h2>{t('adminSystem.featureFlags')}</h2>
                </div>
                <div className="stack">
                  {Object.entries(data.feature_flags).map(([name, enabled]) => {
                    const copy = flagCopy(name);
                    return (
                      <div key={name} className="row-between" style={{ alignItems: 'flex-start' }}>
                        <div style={{ flex: 1 }}>
                          <div style={{ fontWeight: 500 }}>{copy?.label ?? titleCase(name)}</div>
                          <div className="small muted">{copy?.description ?? ''}</div>
                        </div>
                        <label className="switch">
                          <input
                            type="checkbox"
                            checked={enabled}
                            onChange={(event) => setPendingFlag({ name, value: event.target.checked })}
                            aria-label={copy?.label ?? name}
                          />
                        </label>
                      </div>
                    );
                  })}
                </div>
              </section>
            </RetainedPanel>
            <RetainedPanel active={visible('status')}>
              <section className="card" id="deployment">
                <div className="card-header">
                  <h2>{t('adminSystem.deployment')}</h2>
                </div>
                <div className="stack">
                  <DetailRow label={t('adminSystem.version')}>
                    <span className="mono">
                      {data.build.version} · {data.build.sha.slice(0, 12)}
                    </span>
                  </DetailRow>
                  <DetailRow label={t('adminSystem.started')}>{formatDateTime(data.build.started_at)}</DetailRow>
                  <DetailRow label={t('adminSystem.uptime')}>{formatDuration(data.build.uptime_seconds * 1000)}</DetailRow>
                  <DetailRow label={t('adminSystem.usageRetention')}>
                    {t('adminSystem.retentionDays', { count: data.retention.usage_days })}
                  </DetailRow>
                  <DetailRow label={t('adminSystem.auditRetention')}>
                    {t('adminSystem.retentionDays', { count: data.retention.audit_days })}
                  </DetailRow>
                  <DetailRow label={t('adminSystem.purgeRunsAt')}>{data.retention.purge_at_utc} UTC</DetailRow>
                  <DetailRow label={t('adminSystem.prometheus')}>
                    <a href={data.telemetry.prometheus}>{data.telemetry.prometheus}</a>
                  </DetailRow>
                  <DetailRow label={t('adminSystem.adapters')}>{data.adapters.join(', ')}</DetailRow>
                </div>
              </section>
            </RetainedPanel>

            <RetainedPanel active={visible('general')}>
              <div id="upstream-timeouts">
                <UpstreamTimeoutsCard timeouts={data.upstream_timeouts} />
              </div>
              <div id="discovery">
                <DiscoveryIntervalCard interval={data.discovery.interval} />
              </div>
            </RetainedPanel>

            {section === 'all' && <LicenseCard />}

            <RetainedPanel active={visible('troubleshooting')}>
              <div id="troubleshooting">
                <TroubleshootingCard />
              </div>

              <section className="card" id="docs-feedback">
                <div className="card-header">
                  <h2>{t('adminSystem.docsFeedback')}</h2>
                  <span className="small muted">{t('adminSystem.unresolvedCount', { count: data.docs_feedback.length })}</span>
                </div>
                {data.docs_feedback.length === 0 ? (
                  <p className="small muted">{t('adminSystem.noFeedback')}</p>
                ) : (
                  <ul className="stack" style={{ listStyle: 'none', margin: 0, padding: 0 }}>
                    {data.docs_feedback.map((item) => (
                      <li key={item.id} className="row-between" style={{ alignItems: 'flex-start' }}>
                        <div>
                          <div className="row" style={{ gap: 8 }}>
                            <Badge tone={item.helpful ? 'success' : 'warning'}>
                              {item.helpful ? t('adminSystem.helpful') : t('adminSystem.notHelpful')}
                            </Badge>
                            <span className="mono small">{item.page}</span>
                          </div>
                          {item.note ? (
                            <p className="small secondary" style={{ margin: '6px 0 0' }}>
                              {item.note}
                            </p>
                          ) : null}
                        </div>
                        <button type="button" className="btn btn-ghost btn-sm" onClick={() => resolve.mutate(item.id)}>
                          {t('adminSystem.resolve')}
                        </button>
                      </li>
                    ))}
                  </ul>
                )}
              </section>
            </RetainedPanel>
          </>
        )}
      </AsyncSection>

      <ConfirmDialog
        open={Boolean(pendingFlag)}
        onClose={() => setPendingFlag(null)}
        onConfirm={() => {
          if (pendingFlag) setFlag.mutate(pendingFlag);
          setPendingFlag(null);
        }}
        danger={false}
        title={t('adminSystem.flagConfirmTitle', {
          action: pendingFlag?.value ? t('adminSystem.turnOn') : t('adminSystem.turnOff'),
          name: flagCopy(pendingFlag?.name ?? '')?.label ?? pendingFlag?.name ?? '',
        })}
        consequence={
          pendingFlag?.value
            ? (flagCopy(pendingFlag.name)?.consequence ?? t('adminSystem.flagOnConsequence'))
            : t('adminSystem.flagOffConsequence')
        }
        confirmLabel={pendingFlag?.value ? t('adminSystem.turnOn') : t('adminSystem.turnOff')}
        busy={setFlag.isPending}
      />
    </div>
  );
}

/**
 * The database tile answers "what is this instance actually running on?" at a
 * glance: backend name, credential-free location, and — for embedded SQLite —
 * an explicit single-node caveat linking to the deployment guide. It renders
 * only the sanitized `backend`/`location` fields; a raw DSN never reaches the
 * DOM because the component never touches one.
 */
function DatabaseTile({ database }: { database: SystemStatus['database'] }): ReactNode {
  const sqlite = database.backend === 'sqlite';
  const backendLabel = sqlite ? t('adminSystem.backendSqlite') : t('adminSystem.backendPostgres');
  return (
    <article className="tile" aria-labelledby="database-tile-title">
      <div className="row-between">
        <span className="overline" id="database-tile-title">
          {t('adminSystem.database')}
        </span>
        <Badge tone={database.ok ? 'success' : 'danger'} dot>
          {database.ok ? t('adminSystem.ok') : t('adminSystem.down')}
        </Badge>
      </div>
      <div className="tile-meta" style={{ marginTop: 'var(--janus-space-3)' }}>
        <div style={{ fontWeight: 500, color: 'var(--janus-color-text-primary)' }}>{backendLabel}</div>
        {database.location ? (
          <div className="mono small muted" style={{ overflowWrap: 'anywhere' }} title={database.location}>
            {database.location}
          </div>
        ) : null}
        <div className="small muted">{t('adminSystem.migrationsApplied', { count: database.migrations.length })}</div>
      </div>
      {sqlite ? (
        <div className="stack" style={{ marginTop: 'var(--janus-space-3)', gap: 'var(--janus-space-2)' }}>
          <Link
            to="/docs/admin/deployment"
            className="badge badge-warning"
            style={{ whiteSpace: 'normal', textDecoration: 'none', alignSelf: 'flex-start' }}
            aria-label={t('adminSystem.sqliteCaveatAria')}
          >
            <span aria-hidden="true">⚠</span> {t('adminSystem.sqliteCaveat')}
          </Link>
          {database.defaulted ? <span className="small muted">{t('adminSystem.databaseDefaultedNote')}</span> : null}
        </div>
      ) : null}
    </article>
  );
}

function HealthTile({ label, ok, detail, warn }: { label: string; ok: boolean; detail: string; warn?: boolean }): ReactNode {
  const tone = !ok ? 'danger' : warn ? 'warning' : 'success';
  return (
    <article className="tile">
      <div className="row-between">
        <span className="overline">{label}</span>
        <Badge tone={tone} dot>
          {!ok ? t('adminSystem.down') : warn ? t('adminSystem.attention') : t('adminSystem.ok')}
        </Badge>
      </div>
      <div className="tile-meta" style={{ marginTop: 'var(--janus-space-3)' }}>
        {detail}
      </div>
    </article>
  );
}

/**
 * MeteringTile reports whether every successful request was actually metered.
 * A media response (audio, image, video) whose upstream returned no usage is
 * recorded at zero tokens and zero cost rather than guessed from its byte
 * count; that is a configuration gap — the provider's real usage or cost
 * signal is not being extracted — and this tile is where it stops being
 * silent, naming the affected models and linking to the requests.
 */
function MeteringTile({ metering }: { metering: MeteringHealth }): ReactNode {
  const { summary } = metering;
  const gaps = summary.gaps.slice(0, 3);
  return (
    <article className="tile tile-wide" data-testid="metering-tile">
      <div className="row-between">
        <span className="overline">{t('adminSystem.metering')}</span>
        <Badge tone={metering.ok ? 'success' : 'warning'} dot>
          {metering.ok ? t('adminSystem.ok') : t('adminSystem.attention')}
        </Badge>
      </div>
      <div className="tile-meta stack" style={{ marginTop: 'var(--janus-space-2)', gap: 'var(--janus-space-2)' }}>
        {metering.ok ? (
          <>
            <span>{t('adminSystem.meteringOk', { days: metering.window_days })}</span>
            {summary.byte_estimated > 0 ? (
              <span className="small muted">
                {t('adminSystem.meteringOkEstimated', { count: formatNumber(summary.byte_estimated) })}
              </span>
            ) : null}
          </>
        ) : (
          <>
            <span>{t('adminSystem.meteringGap', { count: formatNumber(summary.unmetered), days: metering.window_days })}</span>
            <ul
              className="row wrap small"
              style={{ margin: 0, paddingLeft: 0, listStyle: 'none', gap: 'var(--janus-space-2) var(--janus-space-5)' }}
            >
              {gaps.map((gap) => (
                <li key={`${gap.model_name}-${gap.upstream_id}-${gap.modality}`}>
                  <span className="mono">{gap.model_name}</span>
                  <span className="muted">
                    {' '}
                    · {gap.upstream_name || gap.upstream_id} · {gap.modality} · {formatNumber(gap.requests)} req
                  </span>
                </li>
              ))}
              {summary.gaps.length > gaps.length ? (
                <li className="muted">{t('adminSystem.meteringMore', { count: summary.gaps.length - gaps.length })}</li>
              ) : null}
            </ul>
            <Link className="small" to="/admin/requests?accounting=unmetered_modality">
              {t('adminSystem.meteringViewRequests')}
            </Link>
          </>
        )}
      </div>
    </article>
  );
}

type TimeoutHop = 'connect' | 'ttfb' | 'total';

const TIMEOUT_HOPS: ReadonlyArray<{ hop: TimeoutHop; field: keyof UpstreamTimeoutValues }> = [
  { hop: 'connect', field: 'connect_seconds' },
  { hop: 'ttfb', field: 'ttfb_seconds' },
  { hop: 'total', field: 'total_seconds' },
];

/**
 * Runtime upstream timeouts (connect / time-to-first-byte / total). The form
 * shows the EFFECTIVE value per hop with its provenance (environment default
 * vs admin override); saving sends only the hops the admin changed as a PATCH
 * to /api/v1/admin/system/upstream-timeouts, and "Reset" DELETEs every
 * override. Client-side validation mirrors the server: whole seconds within
 * 1..max_seconds, and connect/TTFB no longer than total, so the admin sees
 * the problem before a round trip (the server still enforces it).
 */
function UpstreamTimeoutsCard({ timeouts }: { timeouts: UpstreamTimeouts }): ReactNode {
  const queryClient = useQueryClient();
  const toast = useToast();
  // Drafts are keyed by the effective values the server last reported, so a
  // background refetch that changes nothing does not clobber typing, while a
  // successful save (which changes them) resets the form to the new truth.
  const serverKey = `${timeouts.effective.connect_seconds}/${timeouts.effective.ttfb_seconds}/${timeouts.effective.total_seconds}`;
  const [draft, setDraft] = useState<{ key: string; values: Record<TimeoutHop, string> }>(() => ({
    key: serverKey,
    values: draftFromEffective(timeouts.effective),
  }));
  const values = draft.key === serverKey ? draft.values : draftFromEffective(timeouts.effective);
  const update = (hop: TimeoutHop, value: string): void => setDraft({ key: serverKey, values: { ...values, [hop]: value } });

  const parsed: Record<TimeoutHop, number | null> = {
    connect: parseSeconds(values.connect),
    ttfb: parseSeconds(values.ttfb),
    total: parseSeconds(values.total),
  };
  const errors: Partial<Record<TimeoutHop, string>> = {};
  for (const { hop } of TIMEOUT_HOPS) {
    const n = parsed[hop];
    if (n === null || n < 1 || n > timeouts.max_seconds) {
      errors[hop] = t('adminSystem.timeouts.invalidNumber', { count: timeouts.max_seconds });
    }
  }
  if (!errors.total && parsed.total !== null) {
    for (const hop of ['connect', 'ttfb'] as const) {
      if (!errors[hop] && parsed[hop] !== null && parsed[hop] > parsed.total) {
        errors[hop] = t('adminSystem.timeouts.exceedsTotal');
      }
    }
  }
  // Only hops whose draft differs from what is in force go in the PATCH. A
  // hop typed back to its environment default is sent as 0 so the override
  // is removed rather than pinned to a value that merely equals the default.
  const patch: UpstreamTimeoutsPatch = {};
  for (const { hop, field } of TIMEOUT_HOPS) {
    const n = parsed[hop];
    if (n !== null && n !== timeouts.effective[field]) {
      patch[field] = n === timeouts.defaults[field] ? 0 : n;
    }
  }
  const dirty = Object.keys(patch).length > 0;
  const hasErrors = Object.keys(errors).length > 0;
  const overridden = TIMEOUT_HOPS.some(({ hop }) => timeouts.source[hop] === 'admin');

  const invalidate = (): void => {
    void queryClient.invalidateQueries({ queryKey: ['admin', 'system'] });
  };
  const save = useMutation({
    mutationFn: (body: UpstreamTimeoutsPatch) => api.patch<UpstreamTimeouts>('/api/v1/admin/system/upstream-timeouts', body),
    onSuccess: () => {
      toast(t('adminSystem.timeouts.savedToast'));
      invalidate();
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });
  const reset = useMutation({
    mutationFn: () => api.del<UpstreamTimeouts>('/api/v1/admin/system/upstream-timeouts'),
    onSuccess: () => {
      toast(t('adminSystem.timeouts.resetToast'));
      invalidate();
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });
  const busy = save.isPending || reset.isPending;

  return (
    <section className="card" aria-labelledby="upstream-timeouts-heading">
      <div className="card-header" style={{ alignItems: 'flex-start' }}>
        <div>
          <h2 id="upstream-timeouts-heading">{t('adminSystem.timeouts.title')}</h2>
          <p className="small muted" style={{ margin: '4px 0 0' }}>
            {t('adminSystem.timeouts.subtitle')}
          </p>
        </div>
        {timeouts.updated_at ? (
          <span className="small muted">
            {t('adminSystem.timeouts.lastChanged', { time: formatRelative(timeouts.updated_at) })}
          </span>
        ) : null}
      </div>
      <form
        className="stack"
        onSubmit={(event) => {
          event.preventDefault();
          if (!dirty) {
            toast(t('adminSystem.timeouts.noChanges'));
            return;
          }
          if (hasErrors) return;
          save.mutate(patch);
        }}
      >
        <div className="grid grid-tiles">
          {TIMEOUT_HOPS.map(({ hop, field }) => {
            const label = t(`adminSystem.timeouts.${hop}Label`);
            const source = timeouts.source[hop];
            return (
              <div key={hop} className="stack" style={{ gap: 6 }}>
                <Field label={label} hint={t(`adminSystem.timeouts.${hop}Hint`)} error={errors[hop]}>
                  <div className="row" style={{ gap: 8, alignItems: 'center' }}>
                    <input
                      className="input"
                      type="number"
                      inputMode="numeric"
                      min={1}
                      max={timeouts.max_seconds}
                      step={1}
                      value={values[hop]}
                      onChange={(event) => update(hop, event.target.value)}
                      aria-label={label}
                      disabled={busy}
                      style={{ maxWidth: 140 }}
                    />
                    <span className="small muted">{t('adminSystem.timeouts.seconds')}</span>
                  </div>
                </Field>
                <div className="row" style={{ gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                  <Badge tone={source === 'admin' ? 'warning' : 'neutral'}>
                    {source === 'admin' ? t('adminSystem.timeouts.sourceAdmin') : t('adminSystem.timeouts.sourceEnv')}
                  </Badge>
                  <span className="small muted">{t('adminSystem.timeouts.effective', { count: timeouts.effective[field] })}</span>
                  {source === 'admin' ? (
                    <span className="small muted">
                      {t('adminSystem.timeouts.envDefault', { count: timeouts.defaults[field] })}
                    </span>
                  ) : null}
                </div>
              </div>
            );
          })}
        </div>
        <div className="row-between" style={{ flexWrap: 'wrap', gap: 8 }}>
          <span className="small muted">{t('adminSystem.timeouts.envVarsNote')}</span>
          <div className="row" style={{ gap: 8 }}>
            {overridden ? (
              <button type="button" className="btn btn-ghost" onClick={() => reset.mutate()} disabled={busy}>
                {reset.isPending ? t('adminSystem.timeouts.resetting') : t('adminSystem.timeouts.reset')}
              </button>
            ) : null}
            <button type="submit" className="btn btn-primary" disabled={busy || !dirty || hasErrors}>
              {save.isPending ? t('adminSystem.timeouts.saving') : t('adminSystem.timeouts.save')}
            </button>
          </div>
        </div>
      </form>
    </section>
  );
}

/**
 * DiscoveryIntervalCard is the runtime control for the upstream polling
 * cadence. Same contract as the timeouts card: the draft is keyed by the
 * server's effective value so background refetches never clobber typing, a
 * value typed back to the environment default is sent as 0 (remove the
 * override) rather than pinned, and the reset button only appears while an
 * override exists.
 */
function DiscoveryIntervalCard({ interval }: { interval: DiscoveryInterval }): ReactNode {
  const queryClient = useQueryClient();
  const toast = useToast();
  const serverKey = String(interval.effective_minutes);
  const [draft, setDraft] = useState<{ key: string; value: string }>(() => ({ key: serverKey, value: serverKey }));
  const value = draft.key === serverKey ? draft.value : serverKey;

  const parsed = parseSeconds(value);
  const error =
    parsed === null || parsed < interval.min_minutes || parsed > interval.max_minutes
      ? t('adminSystem.discovery.invalidNumber', { min: interval.min_minutes, max: interval.max_minutes })
      : undefined;
  const dirty = parsed !== null && parsed !== interval.effective_minutes;
  const overridden = interval.source === 'override';

  const invalidate = (): void => {
    void queryClient.invalidateQueries({ queryKey: ['admin', 'system'] });
  };
  const save = useMutation({
    mutationFn: (body: DiscoveryIntervalPatch) => api.patch<DiscoveryInterval>('/api/v1/admin/system/discovery-interval', body),
    onSuccess: () => {
      toast(t('adminSystem.discovery.savedToast'));
      invalidate();
    },
    onError: (err: Error) => toast(err.message, 'danger'),
  });
  const reset = useMutation({
    mutationFn: () => api.del<DiscoveryInterval>('/api/v1/admin/system/discovery-interval'),
    onSuccess: () => {
      toast(t('adminSystem.discovery.resetToast'));
      invalidate();
    },
    onError: (err: Error) => toast(err.message, 'danger'),
  });
  const busy = save.isPending || reset.isPending;
  const label = t('adminSystem.discovery.intervalLabel');

  return (
    <section className="card" aria-labelledby="discovery-interval-heading">
      <div className="card-header" style={{ alignItems: 'flex-start' }}>
        <div>
          <h2 id="discovery-interval-heading">{t('adminSystem.discovery.title')}</h2>
          <p className="small muted" style={{ margin: '4px 0 0' }}>
            {t('adminSystem.discovery.subtitle')}
          </p>
        </div>
        {interval.updated_at ? (
          <span className="small muted">
            {t('adminSystem.discovery.lastChanged', { time: formatRelative(interval.updated_at) })}
          </span>
        ) : null}
      </div>
      <form
        className="stack"
        onSubmit={(event) => {
          event.preventDefault();
          if (!dirty) {
            toast(t('adminSystem.discovery.noChanges'));
            return;
          }
          if (error || parsed === null) return;
          save.mutate({ minutes: parsed === interval.default_minutes ? 0 : parsed });
        }}
      >
        <div className="grid grid-tiles">
          <div className="stack" style={{ gap: 6 }}>
            <Field
              label={label}
              hint={t('adminSystem.discovery.intervalHint', { min: interval.min_minutes, max: interval.max_minutes })}
              error={error}
            >
              <div className="row" style={{ gap: 8, alignItems: 'center' }}>
                <input
                  className="input"
                  type="number"
                  inputMode="numeric"
                  min={interval.min_minutes}
                  max={interval.max_minutes}
                  step={1}
                  value={value}
                  onChange={(event) => setDraft({ key: serverKey, value: event.target.value })}
                  aria-label={label}
                  disabled={busy}
                  style={{ maxWidth: 140 }}
                  data-testid="discovery-interval-input"
                />
                <span className="small muted">{t('adminSystem.discovery.minutes')}</span>
              </div>
            </Field>
            <div className="row" style={{ gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
              <Badge tone={overridden ? 'warning' : 'neutral'}>
                {overridden ? t('adminSystem.discovery.sourceAdmin') : t('adminSystem.discovery.sourceEnv')}
              </Badge>
              <span className="small muted">{t('adminSystem.discovery.effective', { count: interval.effective_minutes })}</span>
              {overridden ? (
                <span className="small muted">{t('adminSystem.discovery.envDefault', { count: interval.default_minutes })}</span>
              ) : null}
            </div>
          </div>
          <div className="stack small muted" style={{ gap: 4, justifyContent: 'center' }}>
            <span>
              {t('adminSystem.discovery.lastRun', {
                time: interval.last_run_at ? formatRelative(interval.last_run_at) : t('adminSystem.discovery.notYet'),
              })}
            </span>
            {interval.next_run_at ? (
              <span>{t('adminSystem.discovery.nextRun', { time: formatRelative(interval.next_run_at) })}</span>
            ) : null}
          </div>
        </div>
        <div className="row-between" style={{ flexWrap: 'wrap', gap: 8 }}>
          <span className="small muted">{t('adminSystem.discovery.envVarNote')}</span>
          <div className="row" style={{ gap: 8 }}>
            {overridden ? (
              <button type="button" className="btn btn-ghost" onClick={() => reset.mutate()} disabled={busy}>
                {reset.isPending ? t('adminSystem.discovery.resetting') : t('adminSystem.discovery.reset')}
              </button>
            ) : null}
            <button type="submit" className="btn btn-primary" disabled={busy || !dirty || Boolean(error)}>
              {save.isPending ? t('adminSystem.discovery.saving') : t('adminSystem.discovery.save')}
            </button>
          </div>
        </div>
      </form>
    </section>
  );
}

function draftFromEffective(effective: UpstreamTimeoutValues): Record<TimeoutHop, string> {
  return {
    connect: String(effective.connect_seconds),
    ttfb: String(effective.ttfb_seconds),
    total: String(effective.total_seconds),
  };
}

/** Whole non-negative seconds, or null when the field is empty or not an integer. */
function parseSeconds(raw: string): number | null {
  const trimmed = raw.trim();
  if (trimmed === '' || !/^\d+$/.test(trimmed)) return null;
  return Number(trimmed);
}
