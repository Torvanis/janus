import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { Navigate, useMatch } from 'react-router-dom';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../lib/api';
import type {
  SubscriptionConnection,
  SubscriptionDeviceStart,
  SubscriptionPlanUsage,
  SubscriptionPollResult,
  SubscriptionProvider,
} from '../lib/types';
import { formatNumber, formatRelative } from '../lib/format';
import { AsyncSection, Badge, ConfirmDialog, CopyButton, Drawer, EmptyState, Modal, Spinner, useToast } from '../components/ui';
import { CopyIconButton } from '../components/IconButton';
import { RouteTopNav } from '../components/RouteTopNav';
import { t } from '../lib/i18n';

interface ListResponse {
  available: boolean;
  providers: SubscriptionProvider[];
  model_prefix: string;
}

/**
 * Personal subscriptions: connect a provider plan the viewer owns, then call
 * it through Janus as my/<provider>/<model>. Sign-in is the provider's own
 * device flow; Janus only polls for the result.
 */
export function SubscriptionsPage(): ReactNode {
  const queryClient = useQueryClient();
  const toast = useToast();
  const [device, setDevice] = useState<{ provider: SubscriptionProvider; start: SubscriptionDeviceStart } | null>(null);
  const [confirm, setConfirm] = useState<SubscriptionProvider | null>(null);
  const [keyFor, setKeyFor] = useState<SubscriptionProvider | null>(null);

  const list = useQuery({
    queryKey: ['subscriptions'],
    queryFn: () => api.get<ListResponse>('/api/v1/me/subscriptions'),
  });
  const activeId = useMatch('/subscriptions/:provider')?.params.provider;
  const prefix = list.data?.model_prefix ?? 'my/';

  const start = useMutation({
    mutationFn: (p: SubscriptionProvider) =>
      api.post<SubscriptionDeviceStart>(`/api/v1/me/subscriptions/${encodeURIComponent(p.id)}/connect`),
    onSuccess: (data, p) => {
      setDevice({ provider: p, start: data });
      window.open(data.verification_uri_complete, '_blank', 'noopener');
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const check = useMutation({
    mutationFn: (c: SubscriptionConnection) =>
      api.post<SubscriptionConnection>(`/api/v1/me/subscriptions/${encodeURIComponent(c.id)}/check`),
    onSuccess: (res) => {
      if (res.status !== 'active') toast(res.last_error || t('subscriptions.statusReauth'), 'danger');
      else if (res.check_error) toast(res.check_error, 'danger');
      else toast(t('subscriptions.checkOk'), 'success');
      void queryClient.invalidateQueries({ queryKey: ['subscriptions'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const refresh = useMutation({
    mutationFn: (c: SubscriptionConnection) =>
      api.post<SubscriptionConnection>(`/api/v1/me/subscriptions/${encodeURIComponent(c.id)}/refresh-models`),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ['subscriptions'] }),
    onError: (error: Error) => {
      toast(error.message, 'danger');
      void queryClient.invalidateQueries({ queryKey: ['subscriptions'] });
    },
  });

  const putSelection = (c: SubscriptionConnection, selected: string[]) =>
    api.put<SubscriptionConnection>(`/api/v1/me/subscriptions/${encodeURIComponent(c.id)}/models`, { selected });

  const select = useMutation({
    mutationFn: ({ c, selected }: { c: SubscriptionConnection; selected: string[] }) => putSelection(c, selected),
    onSuccess: () => {
      toast(t('subscriptions.selectionSaved'), 'success');
      void queryClient.invalidateQueries({ queryKey: ['subscriptions'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  // Removing a model takes effect at once; the toast offers Undo, which puts
  // back exactly the selection that was there before (order included).
  const removeModel = (c: SubscriptionConnection, model: string): void => {
    const before = c.selected_models ?? [];
    const after = before.filter((m) => m !== model);
    putSelection(c, after)
      .then(() => {
        void queryClient.invalidateQueries({ queryKey: ['subscriptions'] });
        toast(t('subscriptions.removedModel', { model }), 'success', {
          label: t('subscriptions.undo'),
          onClick: () => {
            putSelection(c, before)
              .then(() => void queryClient.invalidateQueries({ queryKey: ['subscriptions'] }))
              .catch((error: Error) => toast(error.message, 'danger'));
          },
        });
      })
      .catch((error: Error) => toast(error.message, 'danger'));
  };

  const disconnect = useMutation({
    mutationFn: (c: SubscriptionConnection) => api.del(`/api/v1/me/subscriptions/${encodeURIComponent(c.id)}`),
    onSuccess: (_d, c) => {
      const name = list.data?.providers.find((p) => p.id === c.provider)?.name ?? c.provider;
      toast(t('subscriptions.disconnectedToast', { provider: name }), 'success');
      setConfirm(null);
      void queryClient.invalidateQueries({ queryKey: ['subscriptions'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">{t('subscriptions.title')}</h1>
          <p className="page-subtitle">{t('subscriptions.subtitle')}</p>
        </div>
      </header>

      <AsyncSection
        query={list}
        empty={{
          when: (data) => data.available && data.providers.length === 0,
          title: t('subscriptions.emptyTitle'),
          body: t('subscriptions.emptyBody'),
        }}
      >
        {(data) =>
          !data.available ? (
            <EmptyState title={t('subscriptions.unavailableTitle')} body={t('subscriptions.unavailableBody')} />
          ) : (
            <ProviderTabs
              providers={data.providers}
              prefix={prefix}
              activeId={activeId}
              renderPanel={(p) => (
                <ProviderCard
                  key={p.id}
                  provider={p}
                  prefix={prefix}
                  busy={start.isPending || refresh.isPending}
                  checking={check.isPending && check.variables?.id === p.connection?.id}
                  saving={select.isPending}
                  onConnect={() => (p.auth === 'key' ? setKeyFor(p) : start.mutate(p))}
                  onRefresh={(c) => refresh.mutate(c)}
                  onCheck={(c) => check.mutate(c)}
                  onSelect={(c, selected) => select.mutateAsync({ c, selected })}
                  onRemove={(c, model) => removeModel(c, model)}
                  onDisconnect={() => setConfirm(p)}
                />
              )}
            />
          )
        }
      </AsyncSection>

      {device ? (
        <DeviceDialog
          provider={device.provider}
          start={device.start}
          onClose={() => setDevice(null)}
          onDone={(result) => {
            setDevice(null);
            if (result.status === 'connected') {
              toast(t('subscriptions.connectedToast', { provider: device.provider.name }), 'success');
            } else if (result.message) {
              toast(result.message, 'danger');
            }
            void queryClient.invalidateQueries({ queryKey: ['subscriptions'] });
          }}
        />
      ) : null}

      {keyFor ? (
        <KeyDialog
          provider={keyFor}
          onClose={() => setKeyFor(null)}
          onDone={() => {
            toast(t('subscriptions.connectedToast', { provider: keyFor.name }), 'success');
            setKeyFor(null);
            void queryClient.invalidateQueries({ queryKey: ['subscriptions'] });
          }}
        />
      ) : null}

      <ConfirmDialog
        open={confirm !== null && confirm.connection !== null}
        onClose={() => setConfirm(null)}
        onConfirm={() => confirm?.connection && disconnect.mutate(confirm.connection)}
        title={t('subscriptions.disconnectTitle', { provider: confirm?.name ?? '' })}
        consequence={
          confirm?.auth === 'key'
            ? t('subscriptions.disconnectKeyConsequence', { prefix, provider: confirm?.id ?? '' })
            : t('subscriptions.disconnectConsequence', { prefix, provider: confirm?.id ?? '' })
        }
        confirmLabel={t('subscriptions.disconnect')}
        busy={disconnect.isPending}
      />
    </div>
  );
}

/**
 * One tab per provider the administrator has enabled (connected or not), so
 * each plan gets the whole page. Tabs are links (/subscriptions/<provider>):
 * history, deep links and new-tab work. Without a provider in the URL the
 * first connected provider opens, or the first provider when none is.
 */
function ProviderTabs({
  providers,
  prefix,
  activeId,
  renderPanel,
}: {
  providers: SubscriptionProvider[];
  prefix: string;
  activeId: string | undefined;
  renderPanel: (p: SubscriptionProvider) => ReactNode;
}): ReactNode {
  const fallback = providers.find((p) => p.connection?.status === 'active') ?? providers[0];
  const active = providers.find((p) => p.id === activeId);
  if (!active) {
    return fallback ? <Navigate to={`/subscriptions/${encodeURIComponent(fallback.id)}`} replace /> : null;
  }
  return (
    <>
      <RouteTopNav
        label={t('subscriptions.title')}
        active={active.id}
        items={providers.map((p) => ({
          id: p.id,
          to: `/subscriptions/${encodeURIComponent(p.id)}`,
          label: (
            <span className="row" style={{ gap: 'var(--janus-space-2)', whiteSpace: 'nowrap' }} title={p.name}>
              <ProviderDot provider={p} />
              {shortName(p.name)}
            </span>
          ),
        }))}
      />
      <p className="secondary small" style={{ margin: '0 0 var(--janus-space-4)' }}>
        {t('subscriptions.availableNote', { prefix })}
      </p>
      {renderPanel(active)}
    </>
  );
}

/** Tab label: the name without its plan note ("xAI Grok (SuperGrok / X Premium+)" → "xAI Grok"). */
function shortName(name: string): string {
  return name.replace(/\s*\(.*\)\s*$/, '') || name;
}

/**
 * A model id that line-breaks only after a "/" (and inside the last segment
 * only when it alone is too wide), never mid-word on a narrow screen.
 */
function ModelId({ value }: { value: string }): ReactNode {
  const parts = value.split('/');
  return (
    <span className="mono small" style={{ display: 'block', overflowWrap: 'break-word' }}>
      {parts.map((part, i) => (
        <span key={i}>
          {part}
          {i < parts.length - 1 ? (
            <>
              /<wbr />
            </>
          ) : null}
        </span>
      ))}
    </span>
  );
}

/** Green: connected. Amber: needs signing in again. Nothing: not connected. */
function ProviderDot({ provider: p }: { provider: SubscriptionProvider }): ReactNode {
  const c = p.connection;
  if (!c) return null;
  const ok = c.status === 'active';
  return (
    <span
      role="img"
      aria-label={ok ? t('subscriptions.statusActive') : t('subscriptions.statusReauth')}
      style={{
        width: 8,
        height: 8,
        borderRadius: '50%',
        flex: 'none',
        background: ok ? 'var(--janus-color-success-fg)' : 'var(--janus-color-warning-text)',
      }}
    />
  );
}

function ProviderCard({
  provider,
  prefix,
  busy,
  checking,
  saving,
  onConnect,
  onRefresh,
  onCheck,
  onSelect,
  onRemove,
  onDisconnect,
}: {
  provider: SubscriptionProvider;
  prefix: string;
  busy: boolean;
  checking: boolean;
  saving: boolean;
  onConnect: () => void;
  onRefresh: (c: SubscriptionConnection) => void;
  onCheck: (c: SubscriptionConnection) => void;
  onSelect: (c: SubscriptionConnection, selected: string[]) => Promise<unknown>;
  onRemove: (c: SubscriptionConnection, model: string) => void;
  onDisconnect: () => void;
}): ReactNode {
  const c = provider.connection;
  const live = c && c.status === 'active' && provider.enabled ? c : null;
  const status = !c ? (
    <Badge tone="neutral">{t('subscriptions.statusNotConnected')}</Badge>
  ) : c.status === 'active' ? (
    <Badge tone="success" dot>
      {t('subscriptions.statusActive')}
    </Badge>
  ) : (
    <Badge tone="warning" dot>
      {t('subscriptions.statusReauth')}
    </Badge>
  );
  return (
    <article className="card">
      <div className="row-between" style={{ alignItems: 'flex-start', gap: 'var(--janus-space-4)', flexWrap: 'wrap' }}>
        <div style={{ minWidth: 0, flex: '1 1 260px' }}>
          <div className="row" style={{ gap: 'var(--janus-space-3)', flexWrap: 'wrap' }}>
            <h2 style={{ margin: 0, fontSize: 'var(--janus-font-size-lg)' }}>{provider.name}</h2>
            {status}
          </div>
          {!c ? (
            <p className="secondary small" style={{ margin: 'var(--janus-space-2) 0 0' }}>
              {provider.description}
            </p>
          ) : (
            <p className="small" style={{ margin: 'var(--janus-space-2) 0 0' }}>
              {t('subscriptions.connectedAs', { account: c.account_email || c.account_name || provider.id })}
              {' · '}
              <span className="secondary">
                {c.last_used_at && !c.last_used_at.startsWith('0001')
                  ? t('subscriptions.lastUsed', { when: formatRelative(c.last_used_at) })
                  : t('subscriptions.neverUsed')}
              </span>
            </p>
          )}
          {c?.status === 'active' ? <SignInStatus connection={c} auth={provider.auth} /> : null}
          {c?.status === 'reauth_required' && c.last_error ? (
            <p className="small" style={{ margin: 'var(--janus-space-2) 0 0', color: 'var(--janus-color-warning-text)' }}>
              {c.last_error}
            </p>
          ) : null}
          {!provider.enabled ? (
            <p className="small secondary" style={{ margin: 'var(--janus-space-2) 0 0' }}>
              {t('subscriptions.providerOff')}
            </p>
          ) : null}
        </div>
        <div className="row" style={{ flexWrap: 'wrap', gap: 'var(--janus-space-2)' }}>
          {provider.enabled && (!c || c.status !== 'active') ? (
            <button type="button" className="btn btn-primary" onClick={onConnect} disabled={busy}>
              {c ? t('subscriptions.reconnect') : t('subscriptions.connect')}
            </button>
          ) : null}
          {live ? (
            <>
              <button type="button" className="btn btn-sm" onClick={() => onCheck(live)} disabled={checking}>
                {checking ? t('subscriptions.checking') : t('subscriptions.checkNow')}
              </button>
              <button type="button" className="btn btn-sm" onClick={() => onRefresh(live)} disabled={busy}>
                {t('subscriptions.refreshModels')}
              </button>
            </>
          ) : null}
          {c ? (
            <button type="button" className="btn btn-sm btn-danger" onClick={onDisconnect}>
              {t('subscriptions.disconnect')}
            </button>
          ) : null}
        </div>
      </div>

      {live ? <UsagePanel connection={live} /> : null}

      {live ? (
        <SelectedModels
          connection={live}
          modelName={(m) => `${prefix}${provider.id}/${m}`}
          saving={saving}
          onSave={(selected) => onSelect(live, selected)}
          onRemove={(m) => onRemove(live, m)}
        />
      ) : null}
    </article>
  );
}

/**
 * One line on whether the stored sign-in is healthy: when it expires (or
 * that it does not), whether Janus renews it, and when it was last checked.
 */
function SignInStatus({ connection: c, auth }: { connection: SubscriptionConnection; auth: 'device' | 'key' }): ReactNode {
  const parts: string[] = [];
  if (auth === 'key') {
    parts.push(t('subscriptions.keyNoExpiry'));
  } else if (!c.access_expires_at) {
    parts.push(t('subscriptions.signInNoExpiry'));
  } else if (c.auto_renews) {
    parts.push(t('subscriptions.signInRenews'));
  } else {
    parts.push(t('subscriptions.signInExpires', { when: formatRelative(c.access_expires_at) }));
  }
  parts.push(
    c.checked_at ? t('subscriptions.checkedAgo', { when: formatRelative(c.checked_at) }) : t('subscriptions.notCheckedYet'),
  );
  return (
    <>
      <p className="small secondary" style={{ margin: 'var(--janus-space-1) 0 0' }}>
        {parts.join(' · ')}
      </p>
      {c.check_error ? (
        <p className="small" style={{ margin: 'var(--janus-space-1) 0 0', color: 'var(--janus-color-warning-text)' }}>
          {c.check_error}
        </p>
      ) : null}
    </>
  );
}

/**
 * Plan usage as the vendor reports it (when it does), plus Janus's own count
 * of what went through this connection, which every provider has.
 */
function UsagePanel({ connection: c }: { connection: SubscriptionConnection }): ReactNode {
  const plan: SubscriptionPlanUsage | undefined = c.plan_usage;
  const day = c.activity?.day;
  const week = c.activity?.week;
  const footer = plan
    ? [plan.plan, ...(plan.notes ?? []), c.usage_at ? t('subscriptions.usageAsOf', { when: formatRelative(c.usage_at) }) : '']
        .filter(Boolean)
        .join(' · ')
    : '';
  return (
    <div
      className="subscription-usage"
      style={{
        marginTop: 'var(--janus-space-4)',
        paddingTop: 'var(--janus-space-3)',
        borderTop: '1px solid var(--janus-color-border-default)',
        display: 'grid',
        gap: 'var(--janus-space-3) var(--janus-space-5)',
        gridTemplateColumns: 'repeat(auto-fit, minmax(min(100%, 220px), 1fr))',
        alignItems: 'start',
      }}
    >
      {plan?.windows.map((w) => (
        <UsageMeter key={w.label} label={w.label} window={w} />
      ))}
      <div className="small">
        <strong>{t('subscriptions.throughJanus')}</strong>
        <div className="secondary">
          {t('subscriptions.activityDay', {
            requests: formatNumber(day?.requests ?? 0),
            count: formatNumber((day?.tokens_in ?? 0) + (day?.tokens_out ?? 0), { compact: true }),
          })}
        </div>
        <div className="secondary">
          {t('subscriptions.activityWeek', {
            requests: formatNumber(week?.requests ?? 0),
            count: formatNumber((week?.tokens_in ?? 0) + (week?.tokens_out ?? 0), { compact: true }),
          })}
        </div>
      </div>
      {footer ? (
        <p className="small secondary" style={{ margin: 0, gridColumn: '1 / -1' }}>
          {footer}
        </p>
      ) : null}
    </div>
  );
}

function UsageMeter({ label, window: w }: { label: string; window: SubscriptionPlanUsage['windows'][number] }): ReactNode {
  const pct = Math.max(0, Math.min(100, w.used_percent));
  const tone =
    pct >= 90 ? 'var(--janus-color-danger-fg)' : pct >= 70 ? 'var(--janus-color-warning-text)' : 'var(--janus-color-primary-bg)';
  const amount = w.unlimited
    ? t('subscriptions.unlimited')
    : w.limit !== undefined && w.used !== undefined
      ? t('subscriptions.usedOf', { value: formatNumber(Math.round(w.used)), limit: formatNumber(Math.round(w.limit)) })
      : t('subscriptions.usedPercent', { percent: Math.round(pct) });
  return (
    <div>
      <div className="row-between small" style={{ gap: 'var(--janus-space-2)' }}>
        <strong>{label}</strong>
        <span className="secondary">{amount}</span>
      </div>
      {!w.unlimited ? (
        <div
          role="meter"
          aria-label={label}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={Math.round(pct)}
          style={{
            height: 6,
            borderRadius: 3,
            background: 'var(--janus-color-bg-subtle, var(--janus-state-hover))',
            margin: '4px 0',
          }}
        >
          <div style={{ width: `${pct}%`, height: '100%', borderRadius: 3, background: tone }} />
        </div>
      ) : null}
      {w.resets_at ? (
        <div className="small secondary">{t('subscriptions.resets', { when: formatRelative(w.resets_at) })}</div>
      ) : null}
    </div>
  );
}

/**
 * The reasoning levels a model accepts. compact: the range ("low → ultra"),
 * with every level in the tooltip; otherwise the full list. Janus fits any
 * requested level to these, so this is information, not a restriction.
 * Models that take no setting show nothing in the compact form.
 */
function ReasoningHint({ efforts, compact }: { efforts: string[] | undefined; compact?: boolean }): ReactNode {
  if (efforts === undefined || (compact && efforts.length === 0)) {
    return null;
  }
  const text =
    efforts.length === 0
      ? t('subscriptions.reasoningNone')
      : compact && efforts.length > 2
        ? t('subscriptions.reasoningRange', { from: efforts[0]!, to: efforts[efforts.length - 1]! })
        : compact
          ? t('subscriptions.reasoningList', { list: efforts.join(' · ') })
          : efforts.join(' · ');
  const hint = efforts.length === 0 ? t('subscriptions.reasoningNoneHint') : t('subscriptions.reasoningLevelsHint');
  return (
    <span
      className="small secondary"
      style={{ display: 'block', overflowWrap: 'anywhere' }}
      title={compact ? `${efforts.join(' · ')}. ${hint}` : hint}
    >
      {text}
    </span>
  );
}

/**
 * The models this user chose from the plan: one row each with the name to
 * call, a copy button and a remove button (immediate, with Undo). Adding
 * happens in a side panel so the full plan list is out of the way.
 */
export function SelectedModels({
  connection,
  modelName,
  saving,
  onSave,
  onRemove,
}: {
  connection: SubscriptionConnection;
  modelName: (m: string) => string;
  saving: boolean;
  onSave: (selected: string[]) => Promise<unknown>;
  onRemove: (model: string) => void;
}): ReactNode {
  const [adding, setAdding] = useState(false);
  const selected = (connection.selected_models ?? []).filter((m) => connection.models.includes(m));
  if (connection.models.length === 0) {
    return (
      <p className="small secondary" style={{ margin: 'var(--janus-space-4) 0 0' }}>
        {t('subscriptions.noModels')}
      </p>
    );
  }
  return (
    <section style={{ marginTop: 'var(--janus-space-5)' }} aria-labelledby={`models-${connection.id}`}>
      <div className="row-between" style={{ gap: 'var(--janus-space-3)' }}>
        <div>
          <h3 id={`models-${connection.id}`} style={{ margin: 0, fontSize: 'var(--janus-font-size-md, 1rem)' }}>
            {t('subscriptions.yourModels')}
          </h3>
          <div className="small secondary">
            {t('subscriptions.yourModelsHint', { count: selected.length, total: connection.models.length })}
          </div>
        </div>
        <button type="button" className="btn btn-sm btn-primary" onClick={() => setAdding(true)}>
          {t('subscriptions.addModels')}
        </button>
      </div>
      {selected.length === 0 ? (
        <p className="small" style={{ margin: 'var(--janus-space-3) 0 0', color: 'var(--janus-color-warning-text)' }}>
          {t('subscriptions.noneSelected')}
        </p>
      ) : (
        <ul
          className="subscription-selected"
          style={{
            listStyle: 'none',
            margin: 'var(--janus-space-3) 0 0',
            padding: 0,
            border: '1px solid var(--janus-color-border-default)',
            borderRadius: 'var(--janus-radius-md, 8px)',
          }}
        >
          {selected.map((m, i) => (
            <li
              key={m}
              className="row"
              style={{
                gap: 'var(--janus-space-3)',
                padding: 'var(--janus-space-2) var(--janus-space-3)',
                borderTop: i === 0 ? 'none' : '1px solid var(--janus-color-border-default)',
                minWidth: 0,
              }}
            >
              <div style={{ minWidth: 0, flex: 1 }}>
                <ModelId value={modelName(m)} />
                <ReasoningHint efforts={connection.reasoning_efforts?.[m]} compact />
              </div>
              <CopyIconButton
                value={modelName(m)}
                label={t('subscriptions.copyModelName', { name: modelName(m) })}
                copiedLabel={t('subscriptions.copiedModelName')}
              />
              <button
                type="button"
                className="btn btn-ghost btn-sm"
                aria-label={t('subscriptions.removeModel', { model: m })}
                title={t('subscriptions.removeModel', { model: m })}
                onClick={() => onRemove(m)}
              >
                ✕
              </button>
            </li>
          ))}
        </ul>
      )}
      {adding ? (
        <ModelPicker
          connection={connection}
          saving={saving}
          onClose={() => setAdding(false)}
          onSave={(next) => onSave(next).then(() => setAdding(false))}
        />
      ) : null}
    </section>
  );
}

/**
 * The plan's full model list in a side panel: filter, tick, save. Only
 * selected models appear in the user's model list; the choice is saved
 * explicitly so a long list can be edited without a request per click.
 */
export function ModelPicker({
  connection,
  saving,
  onSave,
  onClose,
}: {
  connection: SubscriptionConnection;
  saving: boolean;
  onSave: (selected: string[]) => unknown;
  onClose: () => void;
}): ReactNode {
  const saved = useMemo(() => connection.selected_models ?? [], [connection.selected_models]);
  const [draft, setDraft] = useState<string[]>(saved);
  const [filter, setFilter] = useState('');

  const dirty = draft.length !== saved.length || draft.some((m) => !saved.includes(m));
  const needle = filter.trim().toLowerCase();
  const shown = needle ? connection.models.filter((m) => m.toLowerCase().includes(needle)) : connection.models;
  const toggle = (m: string): void => setDraft((cur) => (cur.includes(m) ? cur.filter((x) => x !== m) : [...cur, m]));

  return (
    <Drawer
      open
      onClose={onClose}
      title={t('subscriptions.models')}
      description={t('subscriptions.modelsHint', { count: draft.length, total: connection.models.length })}
      footer={
        <div className="row-between" style={{ width: '100%' }}>
          <span className="small secondary">{dirty ? t('subscriptions.selectionUnsaved') : ''}</span>
          <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
            <button type="button" className="btn" onClick={onClose}>
              {t('common.cancel')}
            </button>
            <button
              type="button"
              className="btn btn-primary"
              disabled={!dirty || saving}
              onClick={() => onSave(connection.models.filter((m) => draft.includes(m)))}
            >
              {t('subscriptions.saveSelection')}
            </button>
          </div>
        </div>
      }
    >
      <div className="row" style={{ gap: 'var(--janus-space-2)', flexWrap: 'wrap' }}>
        <input
          className="input"
          style={{ flex: '1 1 160px', minWidth: 0 }}
          type="search"
          aria-label={t('subscriptions.filterModels')}
          placeholder={t('subscriptions.filterModels')}
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
        />
        <button type="button" className="btn btn-sm" onClick={() => setDraft([...connection.models])}>
          {t('subscriptions.selectAll')}
        </button>
        <button type="button" className="btn btn-sm" onClick={() => setDraft([])}>
          {t('subscriptions.selectNone')}
        </button>
      </div>
      <ul className="subscription-models" style={{ listStyle: 'none', margin: 0, padding: 0 }}>
        {shown.map((m) => (
          <li key={m} style={{ padding: 'var(--janus-space-1) 0' }}>
            <label className="row" style={{ gap: 'var(--janus-space-2)', alignItems: 'flex-start', cursor: 'pointer' }}>
              <input
                type="checkbox"
                checked={draft.includes(m)}
                onChange={() => toggle(m)}
                aria-label={m}
                style={{ marginTop: 3 }}
              />
              <span style={{ minWidth: 0 }}>
                <span className="mono small" style={{ display: 'block', overflowWrap: 'anywhere' }}>
                  {m}
                </span>
                <ReasoningHint efforts={connection.reasoning_efforts?.[m]} />
              </span>
            </label>
          </li>
        ))}
        {shown.length === 0 ? <li className="small secondary">{t('subscriptions.noFilterMatch')}</li> : null}
      </ul>
    </Drawer>
  );
}

/**
 * Shows the provider's user code and polls Janus on the provider's interval
 * until the sign-in resolves. Closing the dialog stops polling; the pending
 * sign-in simply expires server-side.
 */
function DeviceDialog({
  provider,
  start,
  onClose,
  onDone,
}: {
  provider: SubscriptionProvider;
  start: SubscriptionDeviceStart;
  onClose: () => void;
  onDone: (result: SubscriptionPollResult) => void;
}): ReactNode {
  const [message, setMessage] = useState<string | undefined>();
  const intervalRef = useRef(Math.max(1, start.interval_seconds) * 1000);
  const doneRef = useRef(onDone);
  doneRef.current = onDone;

  useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout>;
    const tick = async (): Promise<void> => {
      if (cancelled) return;
      try {
        const res = await api.post<SubscriptionPollResult>(
          `/api/v1/me/subscriptions/connect/${encodeURIComponent(start.pending_id)}/poll`,
        );
        if (cancelled) return;
        if (res.interval_seconds) intervalRef.current = res.interval_seconds * 1000;
        if (res.status === 'pending') {
          setMessage(res.message);
          timer = setTimeout(() => void tick(), intervalRef.current);
          return;
        }
        doneRef.current(res);
      } catch (error) {
        if (cancelled) return;
        setMessage((error as Error).message);
        timer = setTimeout(() => void tick(), intervalRef.current);
      }
    };
    timer = setTimeout(() => void tick(), intervalRef.current);
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [start.pending_id]);

  return (
    <Modal
      open
      onClose={onClose}
      title={t('subscriptions.deviceTitle', { provider: provider.name })}
      footer={
        <button type="button" className="btn" onClick={onClose}>
          {t('subscriptions.deviceCancel')}
        </button>
      }
    >
      <p className="small" style={{ margin: 0 }}>
        {t('subscriptions.deviceStep1')}
      </p>
      <div className="row" style={{ gap: 'var(--janus-space-3)', justifyContent: 'center', flexWrap: 'wrap' }}>
        <code
          className="mono"
          style={{ fontSize: '1.75rem', letterSpacing: '0.15em', padding: 'var(--janus-space-2) var(--janus-space-4)' }}
        >
          {start.user_code}
        </code>
        <CopyButton value={start.user_code} />
      </div>
      <a className="btn btn-primary" href={start.verification_uri_complete} target="_blank" rel="noopener noreferrer">
        {t('subscriptions.deviceOpen')}
      </a>
      <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
        <Spinner />
        <span className="small secondary">{message ?? t('subscriptions.deviceWaiting')}</span>
      </div>
      <p className="small secondary" style={{ margin: 0 }}>
        {t('subscriptions.deviceExpires', { when: formatRelative(start.expires_at) })}
      </p>
    </Modal>
  );
}

/**
 * Key-based providers (Mistral Vibe): the user creates a key at the vendor
 * and pastes it. Janus verifies it live before storing it encrypted; the key
 * is never shown again.
 */
function KeyDialog({
  provider,
  onClose,
  onDone,
}: {
  provider: SubscriptionProvider;
  onClose: () => void;
  onDone: () => void;
}): ReactNode {
  const [key, setKey] = useState('');
  const [error, setError] = useState<string | undefined>();
  const submit = useMutation({
    mutationFn: () =>
      api.post<SubscriptionPollResult>(`/api/v1/me/subscriptions/${encodeURIComponent(provider.id)}/key`, { key: key.trim() }),
    onSuccess: (res) => {
      if (res.status === 'connected') onDone();
      else setError(res.message ?? t('subscriptions.keyRejected'));
    },
    onError: (e: Error) => setError(e.message),
  });
  return (
    <Modal
      open
      onClose={onClose}
      title={t('subscriptions.keyTitle', { provider: provider.name })}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose}>
            {t('subscriptions.deviceCancel')}
          </button>
          <button
            type="button"
            className="btn btn-primary"
            disabled={key.trim() === '' || submit.isPending}
            onClick={() => {
              setError(undefined);
              submit.mutate();
            }}
          >
            {submit.isPending ? t('subscriptions.keyVerifying') : t('subscriptions.connect')}
          </button>
        </>
      }
    >
      <p className="small" style={{ margin: 0 }}>
        {provider.key_help}
      </p>
      {provider.key_url ? (
        <a className="btn" href={provider.key_url} target="_blank" rel="noopener noreferrer">
          {t('subscriptions.keyOpen', { provider: provider.name })}
        </a>
      ) : null}
      <label className="field">
        <span className="field-label">{t('subscriptions.keyLabel')}</span>
        <input
          className="input mono"
          type="password"
          autoComplete="off"
          spellCheck={false}
          value={key}
          onChange={(e) => setKey(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && key.trim() !== '' && !submit.isPending) submit.mutate();
          }}
        />
      </label>
      <p className="small secondary" style={{ margin: 0 }}>
        {t('subscriptions.keyStored')}
      </p>
      {error ? (
        <p className="small" role="alert" style={{ margin: 0, color: 'var(--janus-color-danger-fg)' }}>
          {error}
        </p>
      ) : null}
    </Modal>
  );
}
