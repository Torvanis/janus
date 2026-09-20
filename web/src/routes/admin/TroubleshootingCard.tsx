import { useEffect, useRef, useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../lib/api';
import type { TroubleshootingConfig, TroubleshootingCriteria, TroubleshootingPut, TroubleshootingStatus } from '../../lib/types';
import { formatBytes, formatDateTime, formatNumber, formatRelative } from '../../lib/format';
import { AsyncSection, Badge, Chevron, ConfirmDialog, Field, useToast } from '../../components/ui';
import { DetailRow } from '../shared';
import { t } from '../../lib/i18n';
import { useLicensed } from '../../app/session';
import { LookupInput } from '../../components/LookupInput';
import {
  errorCodeLookup,
  groupLookup,
  httpStatusLookup,
  modelNameLookup,
  principalLookup,
  upstreamLookup,
} from '../../lib/lookups';

const QUERY_KEY = ['admin', 'troubleshooting'] as const;

/** One gate's criteria as strings the inputs can hold. Lists are comma-joined. */
interface CriteriaForm {
  models: string;
  userIds: string;
  groupIds: string;
  upstreamIds: string;
  errorCodes: string;
  httpStatuses: string;
  outcome: '' | 'success' | 'failure';
  tokensInGt: string;
  tokensInLt: string;
  tokensOutGt: string;
  tokensOutLt: string;
}

type CriteriaKey = keyof CriteriaForm;

/** The three logical gates of the rules builder, in display order. */
type Gate = 'include' | 'require' | 'exclude';
const GATES: Gate[] = ['include', 'require', 'exclude'];

const EMPTY_CRITERIA: CriteriaForm = {
  models: '',
  userIds: '',
  groupIds: '',
  upstreamIds: '',
  errorCodes: '',
  httpStatuses: '',
  outcome: '',
  tokensInGt: '',
  tokensInLt: '',
  tokensOutGt: '',
  tokensOutLt: '',
};

/** Form state: the config as strings the inputs can hold, plus the window. */
interface FormState {
  gates: Record<Gate, CriteriaForm>;
  captureRequestBody: boolean;
  captureResponseBody: boolean;
  encrypt: boolean;
  maxBodyKb: string;
  storage: 'database' | 'disk';
  maxAgeHours: string;
  maxCount: string;
  maxMb: string;
  expiresInHours: string;
}

const DEFAULT_FORM: FormState = {
  gates: {
    include: EMPTY_CRITERIA,
    // "Failures only" is the sensible starting point; as the only rule it
    // is a requirement, so it lives in the AND gate.
    require: { ...EMPTY_CRITERIA, outcome: 'failure' },
    exclude: EMPTY_CRITERIA,
  },
  captureRequestBody: true,
  captureResponseBody: true,
  encrypt: true,
  maxBodyKb: '1024',
  storage: 'database',
  maxAgeHours: '24',
  maxCount: '500',
  maxMb: '256',
  expiresInHours: '24',
};

const splitList = (value: string): string[] =>
  value
    .split(',')
    .map((item) => item.trim())
    .filter(Boolean);

const optionalInt = (value: string): number | undefined => {
  const n = Number.parseInt(value, 10);
  return Number.isFinite(n) && value.trim() !== '' ? n : undefined;
};

const criteriaFromWire = (c: TroubleshootingCriteria | undefined): CriteriaForm => ({
  models: (c?.models ?? []).join(', '),
  userIds: (c?.user_ids ?? []).join(', '),
  groupIds: (c?.group_ids ?? []).join(', '),
  upstreamIds: (c?.upstream_ids ?? []).join(', '),
  errorCodes: (c?.error_codes ?? []).join(', '),
  httpStatuses: (c?.http_statuses ?? []).join(', '),
  outcome: c?.outcome ?? '',
  tokensInGt: c?.tokens_in_gt?.toString() ?? '',
  tokensInLt: c?.tokens_in_lt?.toString() ?? '',
  tokensOutGt: c?.tokens_out_gt?.toString() ?? '',
  tokensOutLt: c?.tokens_out_lt?.toString() ?? '',
});

const criteriaToWire = (c: CriteriaForm): TroubleshootingCriteria => ({
  models: splitList(c.models),
  user_ids: splitList(c.userIds),
  group_ids: splitList(c.groupIds),
  upstream_ids: splitList(c.upstreamIds),
  error_codes: splitList(c.errorCodes),
  http_statuses: splitList(c.httpStatuses)
    .map((s) => Number.parseInt(s, 10))
    .filter((n) => Number.isFinite(n)),
  outcome: c.outcome,
  tokens_in_gt: optionalInt(c.tokensInGt),
  tokens_in_lt: optionalInt(c.tokensInLt),
  tokens_out_gt: optionalInt(c.tokensOutGt),
  tokens_out_lt: optionalInt(c.tokensOutLt),
});

const criteriaIsEmpty = (c: CriteriaForm): boolean => (Object.keys(c) as CriteriaKey[]).every((k) => !c[k].trim());

/**
 * Merge two gates' criteria (only needed for a legacy `match: "all"` session
 * that somehow also carries a `require` gate): lists are unioned, scalars
 * prefer the first when set.
 */
function mergeCriteria(a: CriteriaForm, b: CriteriaForm): CriteriaForm {
  const list = (x: string, y: string) => [...new Set([...splitList(x), ...splitList(y)])].join(', ');
  return {
    models: list(a.models, b.models),
    userIds: list(a.userIds, b.userIds),
    groupIds: list(a.groupIds, b.groupIds),
    upstreamIds: list(a.upstreamIds, b.upstreamIds),
    errorCodes: list(a.errorCodes, b.errorCodes),
    httpStatuses: list(a.httpStatuses, b.httpStatuses),
    outcome: a.outcome || b.outcome,
    tokensInGt: a.tokensInGt || b.tokensInGt,
    tokensInLt: a.tokensInLt || b.tokensInLt,
    tokensOutGt: a.tokensOutGt || b.tokensOutGt,
    tokensOutLt: a.tokensOutLt || b.tokensOutLt,
  };
}

function fromConfig(config: TroubleshootingConfig, status: TroubleshootingStatus): FormState {
  const f = config.filter;
  const hoursLeft = status.session?.enabled
    ? Math.max(1, Math.round((new Date(status.session.expires_at).getTime() - Date.now()) / 3_600_000))
    : 24;
  const noExpiry = status.session ? new Date(status.session.expires_at).getFullYear() < 1900 : false;
  // The top-level criteria are the include gate when combined with OR. A
  // session saved by the old builder with AND is shown — and, once saved
  // again, stored — as Require rules, which is exactly what it meant.
  const top = criteriaFromWire(f);
  const legacyAnd = (f.match ?? 'all') === 'all' && !criteriaIsEmpty(top);
  return {
    gates: {
      include: legacyAnd ? EMPTY_CRITERIA : top,
      require: legacyAnd ? mergeCriteria(criteriaFromWire(f.require), top) : criteriaFromWire(f.require),
      exclude: criteriaFromWire(f.exclude),
    },
    captureRequestBody: config.capture_request_body,
    captureResponseBody: config.capture_response_body,
    encrypt: config.encrypt,
    maxBodyKb: String(Math.max(1, Math.round(config.max_body_bytes / 1024))),
    storage: config.storage,
    maxAgeHours: String(config.retention.max_age_hours || ''),
    maxCount: String(config.retention.max_count || ''),
    maxMb: config.retention.max_bytes ? String(Math.round(config.retention.max_bytes / 1_048_576)) : '',
    expiresInHours: noExpiry ? '-1' : String(Math.min(hoursLeft, status.max_session_hours)),
  };
}

function toPut(form: FormState, enabled: boolean): TroubleshootingPut {
  const { include, require, exclude } = form.gates;
  const config: TroubleshootingConfig = {
    filter: {
      match: 'any',
      ...criteriaToWire(include),
      require: criteriaIsEmpty(require) ? undefined : criteriaToWire(require),
      exclude: criteriaIsEmpty(exclude) ? undefined : criteriaToWire(exclude),
    },
    retention: {
      max_age_hours: optionalInt(form.maxAgeHours) ?? 0,
      max_count: optionalInt(form.maxCount) ?? 0,
      max_bytes: (optionalInt(form.maxMb) ?? 0) * 1_048_576,
    },
    storage: form.storage,
    capture_request_body: form.captureRequestBody,
    capture_response_body: form.captureResponseBody,
    encrypt: form.encrypt,
    max_body_bytes: (optionalInt(form.maxBodyKb) ?? 1024) * 1024,
  };
  return { enabled, config, expires_in_hours: optionalInt(form.expiresInHours) ?? 0 };
}

function isZeroTime(value: string | undefined): boolean {
  return !value || new Date(value).getFullYear() < 1900;
}

/**
 * Troubleshooting mode: an explicit, time-boxed opt-in to capture the
 * headers and bodies of proxied requests matching a filter, kept under a
 * retention policy and downloadable per request (admin Requests page) or in
 * bulk. Off by default — Janus never stores payloads otherwise — and every
 * enabled state carries the warnings the backend computes (unencrypted
 * bodies, empty filter, no expiry).
 */
export function TroubleshootingCard(): ReactNode {
  const capturesLicensed = useLicensed('captures');
  const toast = useToast();
  const queryClient = useQueryClient();
  const status = useQuery({
    queryKey: QUERY_KEY,
    queryFn: () => api.get<TroubleshootingStatus>('/api/v1/admin/troubleshooting'),
  });
  const [form, setForm] = useState<FormState>(DEFAULT_FORM);
  const [dirty, setDirty] = useState(false);
  const [purging, setPurging] = useState(false);

  // Seed the form from the server's current session whenever fresh data
  // arrives and the admin has not started editing.
  useEffect(() => {
    if (status.data && !dirty) {
      setForm(status.data.session ? fromConfig(status.data.session.config, status.data) : DEFAULT_FORM);
    }
  }, [status.data, dirty]);

  const update = (patch: Partial<FormState>) => {
    setDirty(true);
    setForm((current) => ({ ...current, ...patch }));
  };
  const updateGate = (gate: Gate, patch: Partial<CriteriaForm>) => {
    setDirty(true);
    setForm((current) => ({
      ...current,
      gates: { ...current.gates, [gate]: { ...current.gates[gate], ...patch } },
    }));
  };

  const settle = (message: string) => (data: TroubleshootingStatus) => {
    queryClient.setQueryData(QUERY_KEY, data);
    setDirty(false);
    toast(message);
    void queryClient.invalidateQueries({ queryKey: ['admin', 'requests'] });
  };
  const fail = (error: Error) => toast(error.message, 'danger');

  const save = useMutation({
    mutationFn: (body: TroubleshootingPut) => api.put<TroubleshootingStatus>('/api/v1/admin/troubleshooting', body),
    onSuccess: (data) =>
      settle(data.active ? t('adminSystem.troubleshoot.savedToast') : t('adminSystem.troubleshoot.enabledToast'))(data),
    onError: fail,
  });
  const disable = useMutation({
    mutationFn: () => api.del<TroubleshootingStatus>('/api/v1/admin/troubleshooting'),
    onSuccess: settle(t('adminSystem.troubleshoot.disabledToast')),
    onError: fail,
  });
  const cleanup = useMutation({
    mutationFn: () => api.post<{ deleted: number; session_expired: boolean }>('/api/v1/admin/troubleshooting/cleanup', {}),
    onSuccess: (result) => {
      toast(t('adminSystem.troubleshoot.cleanupToast', { count: result.deleted }));
      void queryClient.invalidateQueries({ queryKey: QUERY_KEY });
    },
    onError: fail,
  });
  const purge = useMutation({
    mutationFn: () => api.del<{ deleted: number; bytes_freed: number }>('/api/v1/admin/troubleshooting/data'),
    onSuccess: (result) => {
      setPurging(false);
      toast(t('adminSystem.troubleshoot.purgedToast', { count: result.deleted, size: formatBytes(result.bytes_freed) }));
      void queryClient.invalidateQueries({ queryKey: QUERY_KEY });
      void queryClient.invalidateQueries({ queryKey: ['admin', 'requests'] });
    },
    onError: fail,
  });

  const data = status.data;
  const active = data?.active ?? false;
  const busy = save.isPending || disable.isPending;
  const bodiesCaptured = form.captureRequestBody || form.captureResponseBody;
  const filterEmpty = GATES.every((gate) => criteriaIsEmpty(form.gates[gate]));
  const retentionUnbounded = !optionalInt(form.maxAgeHours) && !optionalInt(form.maxCount) && !optionalInt(form.maxMb);

  return (
    <section className="card" aria-labelledby="troubleshooting-title">
      <div className="card-header">
        <h2 id="troubleshooting-title">{t('adminSystem.troubleshoot.title')}</h2>
        {data ? (
          active ? (
            <Badge tone="warning" dot>
              {t('adminSystem.troubleshoot.stateActive')}
            </Badge>
          ) : data.session?.enabled ? (
            <Badge tone="neutral">{t('adminSystem.troubleshoot.stateExpired')}</Badge>
          ) : (
            <Badge tone="neutral">{t('adminSystem.troubleshoot.stateOff')}</Badge>
          )
        ) : null}
      </div>
      <AsyncSection query={status}>
        {(current) => (
          <div className="stack">
            <p className="small secondary" style={{ margin: 0 }}>
              {t('adminSystem.troubleshoot.intro')}
            </p>

            {current.warnings.length > 0 ? (
              <div className="banner banner-warning" role="alert">
                <ul style={{ margin: 0, paddingLeft: 'var(--janus-space-5)' }}>
                  {current.warnings.map((warning) => (
                    <li key={warning} className="small">
                      {warning}
                    </li>
                  ))}
                </ul>
              </div>
            ) : null}

            <div className="grid grid-halves">
              <div className="stack">
                <DetailRow label={t('adminSystem.troubleshoot.captured')}>
                  {t('adminSystem.troubleshoot.capturedValue', {
                    count: formatNumber(current.stats.count),
                    size: formatBytes(current.stats.total_bytes),
                  })}
                </DetailRow>
                <DetailRow label={t('adminSystem.troubleshoot.oldest')}>
                  {isZeroTime(current.stats.oldest_at) ? '—' : formatRelative(current.stats.oldest_at)}
                </DetailRow>
                <DetailRow label={t('adminSystem.troubleshoot.lastCleanup')}>
                  {isZeroTime(current.retention_last_run_at)
                    ? t('adminSystem.notYet')
                    : formatRelative(current.retention_last_run_at)}
                </DetailRow>
                {current.session ? (
                  <DetailRow label={t('adminSystem.troubleshoot.window')}>
                    {current.session.enabled
                      ? isZeroTime(current.session.expires_at)
                        ? t('adminSystem.troubleshoot.noExpiry')
                        : t('adminSystem.troubleshoot.expires', { time: formatDateTime(current.session.expires_at) })
                      : t('adminSystem.troubleshoot.disabledAt', { time: formatDateTime(current.session.disabled_at) })}
                  </DetailRow>
                ) : null}
              </div>
              <div className="row wrap" style={{ alignItems: 'flex-start', justifyContent: 'flex-end' }}>
                <button
                  type="button"
                  className="btn btn-ghost btn-sm"
                  onClick={() => cleanup.mutate()}
                  disabled={cleanup.isPending || current.stats.count === 0}
                >
                  {t('adminSystem.troubleshoot.runCleanup')}
                </button>
                <a
                  className={`btn btn-ghost btn-sm${current.stats.count === 0 ? ' disabled' : ''}`}
                  href="/api/v1/admin/troubleshooting/export"
                  aria-disabled={current.stats.count === 0}
                  onClick={(event) => {
                    if (current.stats.count === 0) event.preventDefault();
                  }}
                >
                  {t('adminSystem.troubleshoot.exportAll')}
                </a>
                <button
                  type="button"
                  className="btn btn-danger btn-sm"
                  onClick={() => setPurging(true)}
                  disabled={current.stats.count === 0}
                >
                  {t('adminSystem.troubleshoot.purgeAll')}
                </button>
              </div>
            </div>

            <form
              className="stack"
              onSubmit={(event) => {
                event.preventDefault();
                save.mutate(toPut(form, true));
              }}
            >
              <FilterBuilder
                gates={form.gates}
                updateGate={updateGate}
                clearAll={() => update({ gates: { include: EMPTY_CRITERIA, require: EMPTY_CRITERIA, exclude: EMPTY_CRITERIA } })}
                filterEmpty={filterEmpty}
              />

              <h3 className="small" style={{ margin: 0 }}>
                {t('adminSystem.troubleshoot.captureHeading')}
              </h3>
              <div className="grid grid-halves">
                <div className="stack">
                  <label className="row small" style={{ gap: 'var(--janus-space-2)' }}>
                    <input
                      type="checkbox"
                      checked={form.captureRequestBody}
                      onChange={(e) => update({ captureRequestBody: e.target.checked })}
                    />
                    <span>{t('adminSystem.troubleshoot.captureRequestBody')}</span>
                  </label>
                  <label className="row small" style={{ gap: 'var(--janus-space-2)' }}>
                    <input
                      type="checkbox"
                      checked={form.captureResponseBody}
                      onChange={(e) => update({ captureResponseBody: e.target.checked })}
                    />
                    <span>{t('adminSystem.troubleshoot.captureResponseBody')}</span>
                  </label>
                  <label className="row small" style={{ gap: 'var(--janus-space-2)' }}>
                    <input
                      type="checkbox"
                      checked={form.encrypt}
                      disabled={!current.encryption_available}
                      onChange={(e) => update({ encrypt: e.target.checked })}
                    />
                    <span>
                      {t('adminSystem.troubleshoot.encrypt')}
                      {!current.encryption_available ? ` (${t('adminSystem.troubleshoot.encryptUnavailable')})` : ''}
                    </span>
                  </label>
                  {bodiesCaptured && !form.encrypt ? (
                    <div className="banner banner-danger small" role="alert">
                      {t('adminSystem.troubleshoot.unencryptedWarning')}
                    </div>
                  ) : null}
                </div>
                <div className="stack">
                  <Field label={t('adminSystem.troubleshoot.storage')} hint={t('adminSystem.troubleshoot.storageHint')}>
                    <select
                      className="input"
                      value={form.storage}
                      onChange={(e) => update({ storage: e.target.value as FormState['storage'] })}
                    >
                      {current.backends.map((backend) => (
                        <option key={backend} value={backend}>
                          {backend === 'disk'
                            ? t('adminSystem.troubleshoot.storageDisk')
                            : t('adminSystem.troubleshoot.storageDatabase')}
                        </option>
                      ))}
                    </select>
                  </Field>
                  <Field
                    label={t('adminSystem.troubleshoot.maxBodyKb')}
                    hint={t('adminSystem.troubleshoot.maxBodyHint', { max: formatBytes(current.max_body_bytes_ceiling) })}
                  >
                    <input
                      className="input"
                      type="number"
                      min={1}
                      max={Math.floor(current.max_body_bytes_ceiling / 1024)}
                      value={form.maxBodyKb}
                      onChange={(e) => update({ maxBodyKb: e.target.value })}
                    />
                  </Field>
                </div>
              </div>

              <h3 className="small" style={{ margin: 0 }}>
                {t('adminSystem.troubleshoot.retentionHeading')}
              </h3>
              <div className="grid grid-halves">
                <Field label={t('adminSystem.troubleshoot.maxAgeHours')}>
                  <input
                    className="input"
                    type="number"
                    min={0}
                    value={form.maxAgeHours}
                    onChange={(e) => update({ maxAgeHours: e.target.value })}
                  />
                </Field>
                <Field label={t('adminSystem.troubleshoot.maxCount')}>
                  <input
                    className="input"
                    type="number"
                    min={0}
                    value={form.maxCount}
                    onChange={(e) => update({ maxCount: e.target.value })}
                  />
                </Field>
                <Field label={t('adminSystem.troubleshoot.maxMb')}>
                  <input
                    className="input"
                    type="number"
                    min={0}
                    value={form.maxMb}
                    onChange={(e) => update({ maxMb: e.target.value })}
                  />
                </Field>
                <Field
                  label={t('adminSystem.troubleshoot.expiresIn')}
                  hint={t('adminSystem.troubleshoot.expiresHint', { max: current.max_session_hours })}
                >
                  <select
                    className="input"
                    value={form.expiresInHours}
                    onChange={(e) => update({ expiresInHours: e.target.value })}
                  >
                    {['1', '6', '24', '72', '168'].map((hours) => (
                      <option key={hours} value={hours}>
                        {t('adminSystem.troubleshoot.hours', { count: hours })}
                      </option>
                    ))}
                    {!['1', '6', '24', '72', '168', '-1'].includes(form.expiresInHours) ? (
                      <option value={form.expiresInHours}>
                        {t('adminSystem.troubleshoot.hours', { count: form.expiresInHours })}
                      </option>
                    ) : null}
                    <option value="-1">{t('adminSystem.troubleshoot.noExpiry')}</option>
                  </select>
                </Field>
              </div>
              {retentionUnbounded ? (
                <p className="small" style={{ margin: 0, color: 'var(--janus-color-danger-fg)' }}>
                  {t('adminSystem.troubleshoot.retentionRequired')}
                </p>
              ) : null}
              <p className="small muted" style={{ margin: 0 }}>
                {t('adminSystem.troubleshoot.performanceNote')}
              </p>

              <div className="row wrap" style={{ justifyContent: 'flex-end' }}>
                {active ? (
                  <button type="button" className="btn btn-ghost" onClick={() => disable.mutate()} disabled={busy}>
                    {t('adminSystem.troubleshoot.disable')}
                  </button>
                ) : null}
                {!active && !capturesLicensed ? (
                  <span className="small muted">{t('adminSystem.troubleshoot.upsell')}</span>
                ) : null}
                <button
                  type="submit"
                  className="btn btn-primary"
                  disabled={busy || retentionUnbounded || (active && !dirty) || (!active && !capturesLicensed)}
                >
                  {active ? t('adminSystem.troubleshoot.save') : t('adminSystem.troubleshoot.enable')}
                </button>
              </div>
            </form>
          </div>
        )}
      </AsyncSection>

      <ConfirmDialog
        open={purging}
        onClose={() => setPurging(false)}
        onConfirm={() => purge.mutate()}
        title={t('adminSystem.troubleshoot.purgeTitle')}
        consequence={t('adminSystem.troubleshoot.purgeConsequence', {
          count: formatNumber(data?.stats.count ?? 0),
          size: formatBytes(data?.stats.total_bytes ?? 0),
        })}
        confirmLabel={t('adminSystem.troubleshoot.purgeConfirm')}
        busy={purge.isPending}
      />
    </section>
  );
}

/* --- Filter builder -------------------------------------------------------- */

/**
 * A rule is one criterion inside a gate. Token bounds pair up (greater/smaller
 * than) so an admin sees one "Tokens in" rule with two inputs rather than two
 * rules.
 */
type RuleKind =
  'models' | 'userIds' | 'groupIds' | 'upstreamIds' | 'outcome' | 'errorCodes' | 'httpStatuses' | 'tokensIn' | 'tokensOut';

const RULE_KINDS: RuleKind[] = [
  'models',
  'userIds',
  'groupIds',
  'upstreamIds',
  'outcome',
  'errorCodes',
  'httpStatuses',
  'tokensIn',
  'tokensOut',
];

const KIND_KEYS: Record<RuleKind, CriteriaKey[]> = {
  models: ['models'],
  userIds: ['userIds'],
  groupIds: ['groupIds'],
  upstreamIds: ['upstreamIds'],
  outcome: ['outcome'],
  errorCodes: ['errorCodes'],
  httpStatuses: ['httpStatuses'],
  tokensIn: ['tokensInGt', 'tokensInLt'],
  tokensOut: ['tokensOutGt', 'tokensOutLt'],
};

const LIST_KINDS = new Set<RuleKind>(['models', 'userIds', 'groupIds', 'upstreamIds', 'errorCodes', 'httpStatuses']);

const kindLabel = (kind: RuleKind): string =>
  ({
    models: t('adminSystem.troubleshoot.models'),
    userIds: t('adminSystem.troubleshoot.users'),
    groupIds: t('adminSystem.troubleshoot.groups'),
    upstreamIds: t('adminSystem.troubleshoot.upstreams'),
    outcome: t('adminSystem.troubleshoot.outcome'),
    errorCodes: t('adminSystem.troubleshoot.errorCodes'),
    httpStatuses: t('adminSystem.troubleshoot.httpStatuses'),
    tokensIn: t('adminSystem.troubleshoot.tokensIn'),
    tokensOut: t('adminSystem.troubleshoot.tokensOut'),
  })[kind];

const kindHint = (kind: RuleKind): string =>
  ({
    models: t('adminSystem.troubleshoot.modelsHint'),
    userIds: t('adminSystem.troubleshoot.usersHint'),
    groupIds: t('adminSystem.troubleshoot.groupsHint'),
    upstreamIds: t('adminSystem.troubleshoot.upstreamsHint'),
    outcome: '',
    errorCodes: t('adminSystem.troubleshoot.errorCodesHint'),
    httpStatuses: t('adminSystem.troubleshoot.httpStatusesHint'),
    tokensIn: t('adminSystem.troubleshoot.tokensHint'),
    tokensOut: t('adminSystem.troubleshoot.tokensHint'),
  })[kind];

const kindPlaceholder: Partial<Record<RuleKind, string>> = {
  models: 'Search models…',
  userIds: 'Search people by name or email…',
  groupIds: 'Search groups by name…',
  upstreamIds: 'Search providers…',
  errorCodes: 'Search error codes…',
  httpStatuses: 'Search or type a status…',
};

/** Which lookup backs each list rule. `free` = accept unknown typed values. */
const LOOKUPS: Partial<
  Record<
    RuleKind,
    {
      search: (q: string) => Promise<import('../../components/LookupInput').LookupOption[]>;
      resolve: (ids: string[]) => Promise<import('../../components/LookupInput').LookupOption[]>;
      free: boolean;
    }
  >
> = {
  userIds: { ...principalLookup, free: true },
  groupIds: { ...groupLookup, free: false },
  models: { ...modelNameLookup, free: true },
  upstreamIds: { ...upstreamLookup, free: true },
  errorCodes: { ...errorCodeLookup, free: true },
  httpStatuses: { ...httpStatusLookup, free: true },
};

/** Patch that blanks every key behind a rule kind. */
const clearedKind = (kind: RuleKind): Partial<CriteriaForm> => {
  const patch: Partial<CriteriaForm> = {};
  for (const key of KIND_KEYS[kind]) (patch as Record<CriteriaKey, string>)[key] = '';
  return patch;
};

const kindIsSet = (c: CriteriaForm, kind: RuleKind): boolean => KIND_KEYS[kind].some((key) => c[key].trim() !== '');

interface GateMeta {
  badge: string;
  tone: 'primary' | 'warning' | 'neutral';
  title: string;
  hint: string;
  add: string;
}

/** Per-gate look: badge text/tone and the copy under the title. */
function gateMeta(gate: Gate): GateMeta {
  switch (gate) {
    case 'include':
      return {
        badge: t('adminSystem.troubleshoot.gateIncludeBadge'),
        tone: 'primary',
        title: t('adminSystem.troubleshoot.gateInclude'),
        hint: t('adminSystem.troubleshoot.gateIncludeHint'),
        add: t('adminSystem.troubleshoot.addInclude'),
      };
    case 'require':
      return {
        badge: t('adminSystem.troubleshoot.gateRequireBadge'),
        tone: 'warning',
        title: t('adminSystem.troubleshoot.gateRequire'),
        hint: t('adminSystem.troubleshoot.gateRequireHint'),
        add: t('adminSystem.troubleshoot.addRequire'),
      };
    case 'exclude':
      return {
        badge: t('adminSystem.troubleshoot.gateExcludeBadge'),
        tone: 'neutral',
        title: t('adminSystem.troubleshoot.gateExclude'),
        hint: t('adminSystem.troubleshoot.gateExcludeHint'),
        add: t('adminSystem.troubleshoot.addExclude'),
      };
  }
}

/** Human summary of one range: "> 10,000", "< 500" or "> 10,000 and < 500". */
function rangeSummary(gt: string, lt: string): string {
  const parts: string[] = [];
  if (gt.trim()) parts.push(`> ${formatNumber(Number(gt))}`);
  if (lt.trim()) parts.push(`< ${formatNumber(Number(lt))}`);
  return parts.join(' and ');
}

/** One rule's value, as it reads in the summary chip. */
/** Names for ids the lookups have seen, so summaries read "Casey" not a uuid. */
const labelStore: Record<string, string> = {};
let labelListeners: Array<() => void> = [];
function rememberLabels(next: Record<string, string>): void {
  let changed = false;
  for (const [id, label] of Object.entries(next)) {
    if (labelStore[id] !== label) {
      labelStore[id] = label;
      changed = true;
    }
  }
  if (changed) for (const l of labelListeners) l();
}
function useLabelVersion(): number {
  const [v, setV] = useState(0);
  useEffect(() => {
    const bump = () => setV((x) => x + 1);
    labelListeners.push(bump);
    return () => {
      labelListeners = labelListeners.filter((l) => l !== bump);
    };
  }, []);
  return v;
}
const named = (ids: string[]): string => ids.map((id) => labelStore[id] ?? id).join(', ');

function ruleSummary(c: CriteriaForm, kind: RuleKind): string {
  switch (kind) {
    case 'models':
      return t('adminSystem.troubleshoot.summaryModels', { value: named(splitList(c.models)) });
    case 'userIds':
      return t('adminSystem.troubleshoot.summaryUsers', { value: named(splitList(c.userIds)) });
    case 'groupIds':
      return t('adminSystem.troubleshoot.summaryGroups', { value: named(splitList(c.groupIds)) });
    case 'upstreamIds':
      return t('adminSystem.troubleshoot.summaryUpstreams', { value: named(splitList(c.upstreamIds)) });
    case 'errorCodes':
      return t('adminSystem.troubleshoot.summaryErrorCodes', { value: splitList(c.errorCodes).join(', ') });
    case 'httpStatuses':
      return t('adminSystem.troubleshoot.summaryHttpStatuses', { value: splitList(c.httpStatuses).join(', ') });
    case 'outcome':
      return t('adminSystem.troubleshoot.summaryOutcome', {
        value:
          c.outcome === 'failure' ? t('adminSystem.troubleshoot.outcomeFailure') : t('adminSystem.troubleshoot.outcomeSuccess'),
      });
    case 'tokensIn':
      return t('adminSystem.troubleshoot.summaryTokensIn', { value: rangeSummary(c.tokensInGt, c.tokensInLt) });
    case 'tokensOut':
      return t('adminSystem.troubleshoot.summaryTokensOut', { value: rangeSummary(c.tokensOutGt, c.tokensOutLt) });
  }
}

/**
 * ListInput: a comma-joined string shown as removable chips plus a text box.
 * Enter, comma or blur commit what was typed; Backspace on an empty box
 * removes the last chip. Pasting "a, b" adds two chips.
 */
function ListInput({
  label,
  value,
  onChange,
  placeholder,
}: {
  label: string;
  value: string;
  onChange: (next: string) => void;
  placeholder?: string;
}): ReactNode {
  const [draft, setDraft] = useState('');
  const items = splitList(value);
  const commit = (raw: string) => {
    const additions = splitList(raw).filter((item) => !items.includes(item));
    if (additions.length > 0) onChange([...items, ...additions].join(', '));
    setDraft('');
  };
  const remove = (item: string) => onChange(items.filter((other) => other !== item).join(', '));
  const inputRef = useRef<HTMLInputElement>(null);
  return (
    // The box is a visual frame around the chips; its input is the focusable
    // control, so a click anywhere on the frame just moves focus there.
    <div className="rule-list" onClick={() => inputRef.current?.focus()}>
      {items.map((item) => (
        <span key={item} className="rule-chip">
          <span className="mono">{item}</span>
          <button
            type="button"
            className="rule-chip-remove"
            onClick={() => remove(item)}
            aria-label={t('adminSystem.troubleshoot.removeValue', { value: item })}
          >
            ×
          </button>
        </span>
      ))}
      <input
        ref={inputRef}
        className="rule-list-input"
        aria-label={label}
        value={draft}
        placeholder={
          items.length === 0
            ? (placeholder ?? t('adminSystem.troubleshoot.listPlaceholder'))
            : t('adminSystem.troubleshoot.listAddMore')
        }
        onChange={(e) => {
          if (e.target.value.includes(',')) commit(e.target.value);
          else setDraft(e.target.value);
        }}
        onKeyDown={(e) => {
          if (e.key === 'Enter') {
            e.preventDefault();
            commit(draft);
          } else if (e.key === 'Backspace' && draft === '' && items.length > 0) {
            remove(items[items.length - 1]!);
          }
        }}
        onBlur={() => commit(draft)}
      />
    </div>
  );
}

/** The value editor for one rule; every criterion of the old grid is here under its old label. */
function RuleEditor({
  kind,
  criteria,
  update,
}: {
  kind: RuleKind;
  criteria: CriteriaForm;
  update: (patch: Partial<CriteriaForm>) => void;
}): ReactNode {
  if (LIST_KINDS.has(kind)) {
    const key = KIND_KEYS[kind][0]!;
    // Every list rule is a lookup: type a fragment, pick from what exists.
    // Ids travel on the wire; names show in the chips. Codes and statuses
    // also accept free text so a value the list does not know still works.
    const source = LOOKUPS[kind];
    if (source) {
      return (
        <LookupInput
          label={kindLabel(kind)}
          value={splitList(criteria[key])}
          onChange={(ids) => update({ [key]: ids.join(', ') } as Partial<CriteriaForm>)}
          search={source.search}
          resolve={source.resolve}
          allowFreeText={source.free}
          placeholder={kindPlaceholder[kind]}
          onLabels={rememberLabels}
        />
      );
    }
    return (
      <ListInput
        label={kindLabel(kind)}
        value={criteria[key]}
        onChange={(next) => update({ [key]: next } as Partial<CriteriaForm>)}
        placeholder={kindPlaceholder[kind]}
      />
    );
  }
  if (kind === 'outcome') {
    return (
      <select
        className="select"
        aria-label={t('adminSystem.troubleshoot.outcome')}
        value={criteria.outcome}
        onChange={(e) => update({ outcome: e.target.value as CriteriaForm['outcome'] })}
      >
        <option value="">{t('adminSystem.troubleshoot.outcomeChoose')}</option>
        <option value="failure">{t('adminSystem.troubleshoot.outcomeFailure')}</option>
        <option value="success">{t('adminSystem.troubleshoot.outcomeSuccess')}</option>
      </select>
    );
  }
  const [gtKey, ltKey] = KIND_KEYS[kind] as [CriteriaKey, CriteriaKey];
  const gtLabel = kind === 'tokensIn' ? t('adminSystem.troubleshoot.tokensInGt') : t('adminSystem.troubleshoot.tokensOutGt');
  const ltLabel = kind === 'tokensIn' ? t('adminSystem.troubleshoot.tokensInLt') : t('adminSystem.troubleshoot.tokensOutLt');
  const numberField = (key: CriteriaKey, label: string) => (
    <Field label={label}>
      <input
        className="input"
        type="number"
        min={0}
        inputMode="numeric"
        value={criteria[key]}
        onChange={(e) => update({ [key]: e.target.value } as Partial<CriteriaForm>)}
        placeholder="—"
      />
    </Field>
  );
  return (
    <div className="rule-range">
      {numberField(gtKey, gtLabel)}
      {numberField(ltKey, ltLabel)}
    </div>
  );
}

/**
 * FilterBuilder arranges the capture criteria as a rules builder with three
 * logical gates — Include (OR), Require (AND), Exclude (NOT) — the way a
 * policy-rules editor does. Each gate is a collapsible card holding one rule
 * per criterion (selector on top, value below, remove at the side); gates
 * with no rules stay out of the way, and the three add buttons at the bottom
 * open a new rule in the chosen gate. A live summary lists every set rule
 * with its gate badge, each chip clearing exactly that rule.
 */
function FilterBuilder({
  gates,
  updateGate,
  clearAll,
  filterEmpty,
}: {
  gates: Record<Gate, CriteriaForm>;
  updateGate: (gate: Gate, patch: Partial<CriteriaForm>) => void;
  clearAll: () => void;
  filterEmpty: boolean;
}): ReactNode {
  useLabelVersion(); // re-render summaries when a lookup learns a name
  // Rules the admin opened but has not filled in yet ('' = kind not chosen).
  const [open, setOpen] = useState<Record<Gate, Array<RuleKind | ''>>>({ include: [], require: [], exclude: [] });
  const [collapsed, setCollapsed] = useState<Record<Gate, boolean>>({ include: false, require: false, exclude: false });

  const rows = (gate: Gate): Array<RuleKind | ''> => [
    ...RULE_KINDS.filter((kind) => kindIsSet(gates[gate], kind) || open[gate].includes(kind)),
    ...(open[gate].includes('') ? ([''] as const) : []),
  ];
  const setRows = (gate: Gate, next: Array<RuleKind | ''>) => setOpen((current) => ({ ...current, [gate]: next }));

  const addRule = (gate: Gate) => {
    setRows(gate, [...open[gate], '']);
    setCollapsed((current) => ({ ...current, [gate]: false }));
  };
  const removeRule = (gate: Gate, kind: RuleKind | '') => {
    setRows(
      gate,
      open[gate].filter((k) => k !== kind),
    );
    if (kind) updateGate(gate, clearedKind(kind));
  };
  const changeKind = (gate: Gate, from: RuleKind | '', to: RuleKind) => {
    setRows(gate, [...open[gate].filter((k) => k !== from && k !== to), to]);
    if (from) updateGate(gate, clearedKind(from));
  };

  const setRules = GATES.flatMap((gate) =>
    RULE_KINDS.filter((kind) => kindIsSet(gates[gate], kind)).map((kind) => ({ gate, kind })),
  );
  const setCount = setRules.length;
  const anyRows = GATES.some((gate) => rows(gate).length > 0);

  return (
    <div className="stack rule-builder" data-testid="troubleshoot-filter-builder">
      <div>
        <h3 className="small rule-heading">{t('adminSystem.troubleshoot.filterHeading')}</h3>
        <p className="small muted rule-intro">{t('adminSystem.troubleshoot.filterIntro')}</p>
      </div>

      {!anyRows ? <p className="small muted rule-empty">{t('adminSystem.troubleshoot.noRules')}</p> : null}

      {GATES.map((gate) => {
        const gateRows = rows(gate);
        if (gateRows.length === 0) return null;
        const meta = gateMeta(gate);
        const count = gateRows.filter((kind) => kind && kindIsSet(gates[gate], kind)).length;
        const isCollapsed = collapsed[gate];
        const bodyId = `troubleshoot-gate-${gate}-rules`;
        return (
          <section key={gate} className={`rule-gate rule-gate-${gate}`} data-testid={`troubleshoot-gate-${gate}`}>
            <header className="rule-gate-head">
              <button
                type="button"
                className="rule-gate-toggle"
                aria-expanded={!isCollapsed}
                aria-controls={bodyId}
                aria-label={
                  isCollapsed
                    ? t('adminSystem.troubleshoot.expandGate', { gate: meta.title })
                    : t('adminSystem.troubleshoot.collapseGate', { gate: meta.title })
                }
                onClick={() => setCollapsed((current) => ({ ...current, [gate]: !current[gate] }))}
              >
                <Badge tone={meta.tone}>{meta.badge}</Badge>
                <span className="rule-gate-titles">
                  <span className="rule-gate-title">{meta.title}</span>
                  <span className="small muted">{meta.hint}</span>
                </span>
                <span className="rule-gate-meta">
                  {count > 0 ? (
                    <span className="small muted">
                      {count === 1
                        ? t('adminSystem.troubleshoot.ruleCountOne')
                        : t('adminSystem.troubleshoot.ruleCount', { count })}
                    </span>
                  ) : null}
                  <Chevron open={!isCollapsed} className="rule-gate-chevron" />
                </span>
              </button>
            </header>
            {!isCollapsed ? (
              <div className="rule-gate-body" id={bodyId}>
                {gateRows.map((kind) => {
                  const used = new Set(gateRows.filter((k) => k && k !== kind));
                  return (
                    <div key={kind || '__new'} className="rule-row">
                      <div className="rule-row-head">
                        <select
                          className="select rule-kind"
                          aria-label={t('adminSystem.troubleshoot.criterion')}
                          value={kind}
                          onChange={(e) => changeKind(gate, kind, e.target.value as RuleKind)}
                        >
                          <option value="" disabled>
                            {t('adminSystem.troubleshoot.chooseCriterion')}
                          </option>
                          {RULE_KINDS.map((option) => (
                            <option key={option} value={option} disabled={used.has(option)}>
                              {kindLabel(option)}
                            </option>
                          ))}
                        </select>
                        <button
                          type="button"
                          className="btn btn-ghost btn-sm rule-remove"
                          onClick={() => removeRule(gate, kind)}
                          aria-label={t('adminSystem.troubleshoot.removeRule')}
                          title={t('adminSystem.troubleshoot.removeRule')}
                        >
                          <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true" className="rule-remove-icon">
                            <path
                              d="M6 2.5h4M2.5 4.5h11M4 4.5l.7 8.2a1 1 0 0 0 1 .8h4.6a1 1 0 0 0 1-.8l.7-8.2M6.5 7v4M9.5 7v4"
                              fill="none"
                              stroke="currentColor"
                              strokeWidth="1.4"
                              strokeLinecap="round"
                              strokeLinejoin="round"
                            />
                          </svg>
                        </button>
                      </div>
                      {kind ? (
                        <div className="rule-row-body">
                          <RuleEditor kind={kind} criteria={gates[gate]} update={(patch) => updateGate(gate, patch)} />
                          {kindHint(kind) ? <span className="field-hint">{kindHint(kind)}</span> : null}
                        </div>
                      ) : null}
                    </div>
                  );
                })}
              </div>
            ) : null}
          </section>
        );
      })}

      <div className="row wrap rule-add-row">
        {GATES.map((gate) => (
          <button
            key={gate}
            type="button"
            className="btn btn-ghost btn-sm"
            onClick={() => addRule(gate)}
            disabled={open[gate].includes('') || rows(gate).length >= RULE_KINDS.length}
          >
            {gateMeta(gate).add}
          </button>
        ))}
      </div>

      <div className="row-between rule-summary" data-testid="troubleshoot-filter-summary">
        <div className="row wrap rule-summary-chips">
          <span className="small muted">
            {setCount === 0
              ? t('adminSystem.troubleshoot.criteriaNone')
              : setCount === 1
                ? t('adminSystem.troubleshoot.criteriaCountOne')
                : t('adminSystem.troubleshoot.criteriaCount', { count: setCount })}
          </span>
          {setRules.map(({ gate, kind }) => (
            <button
              key={`${gate}:${kind}`}
              type="button"
              className="btn btn-ghost btn-sm rule-summary-chip"
              onClick={() => removeRule(gate, kind)}
              title={t('adminSystem.troubleshoot.clearGroup')}
            >
              <Badge tone={gateMeta(gate).tone}>{gateMeta(gate).badge}</Badge>
              {ruleSummary(gates[gate], kind)} ×
            </button>
          ))}
        </div>
        {setCount > 0 ? (
          <button
            type="button"
            className="btn btn-ghost btn-sm"
            onClick={() => {
              setOpen({ include: [], require: [], exclude: [] });
              clearAll();
            }}
          >
            {t('adminSystem.troubleshoot.clearAll')}
          </button>
        ) : null}
      </div>
      {filterEmpty ? (
        <p className="small rule-warning" role="note">
          {t('adminSystem.troubleshoot.emptyFilterWarning')}
        </p>
      ) : null}
    </div>
  );
}
