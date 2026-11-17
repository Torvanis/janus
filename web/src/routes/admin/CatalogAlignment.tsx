import { useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import {
  patchModel,
  previewModelCatalogReset,
  resetModelToCatalog,
  type CatalogPriceChange,
  type CatalogResetPlan,
} from '../../api/client';
import { ApiError } from '../../lib/api';
import type { Model } from '../../lib/types';
import { compareLines, isBaseTokenRate, priceLineLabel } from '../../lib/priceLines';
import { ConfirmDialog, useToast } from '../../components/ui';
import { t } from '../../lib/i18n';
import { priceText } from './PriceBuilder';

// "Keep aligned to Janus price catalog" and "Reset to Janus price catalog"
// in the model drawer. An aligned model has no prices set by an
// administrator: its prices follow the catalog (the provider's own API
// prices still apply where it publishes them). Turning the switch on is a
// reset, so both go through the same confirmation with the exact
// before → after list.

/** True when a price line in force comes from the provider's API. */
function providerPriced(model: Model): boolean {
  return Object.entries(model.metadata ?? {}).some(
    ([key, field]) => key !== 'context_window' && field.source === 'upstream' && field.value !== null,
  );
}

function capitalized(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

function afterText(change: CatalogPriceChange): string {
  if (change.after === null) return isBaseTokenRate(change.line) ? priceText(null) : t('adminModels.catalogAlign.removed');
  return priceText(change.after);
}

/** The before → after list of a reset, changed lines only. */
export function CatalogChanges({ plan }: { plan: CatalogResetPlan }): ReactNode {
  const changed = plan.changes.filter((c) => c.changed).sort((a, b) => compareLines(a.line, b.line));
  const same = plan.changes.length - changed.length;
  return (
    <div className="stack" style={{ gap: 'var(--janus-space-2)' }}>
      {changed.length === 0 ? (
        <p className="small" style={{ margin: 0 }}>
          {t('adminModels.catalogAlign.noChanges')}
        </p>
      ) : (
        <ul className="catalog-changes" aria-label={t('adminModels.catalogAlign.changesLabel')}>
          {changed.map((c) => (
            <li key={c.line} data-line={c.line}>
              <span className="catalog-change-line">{capitalized(priceLineLabel(c.line))}</span>
              <span className="catalog-change-values mono">
                {priceText(c.before)} → {afterText(c)}
              </span>
            </li>
          ))}
        </ul>
      )}
      {changed.length > 0 && same > 0 ? (
        <p className="small muted" style={{ margin: 0 }}>
          {t('adminModels.catalogAlign.unchanged', { count: same })}
        </p>
      ) : null}
    </div>
  );
}

/**
 * Where a catalog preview stands: "Working out the prices…" while it loads,
 * else the reason it failed with Try again. Never both, and never a
 * spinner that outlives the request.
 */
export function CatalogPreviewStatus({
  query,
}: {
  query: { isFetching: boolean; error: Error | null; refetch: () => unknown };
}): ReactNode {
  if (query.error && !query.isFetching) {
    // The generic network message promises an automatic retry; previews
    // are not retried, so say what to do instead.
    const reason =
      query.error instanceof ApiError && query.error.code === 'network_error'
        ? t('adminModels.catalogAlign.previewUnreachable')
        : query.error.message;
    return (
      <div className="catalog-preview-error" role="alert" data-testid="catalog-preview-error">
        <span className="small">{t('adminModels.catalogAlign.previewFailed', { value: reason })}</span>
        <button type="button" className="btn btn-sm" onClick={() => void query.refetch()}>
          {t('common.tryAgain')}
        </button>
      </div>
    );
  }
  if (query.isFetching) {
    return (
      <span className="small muted" role="status">
        {t('adminModels.catalogAlign.loading')}
      </span>
    );
  }
  return null;
}

export function CatalogAlignment({
  model,
  pricesDirty,
  disabled,
  onChanged,
}: {
  model: Model;
  /** The price builder holds unsaved edits. */
  pricesDirty: boolean;
  disabled: boolean;
  /** The model after a reset or a switch change. */
  onChanged: (model: Model) => void;
}): ReactNode {
  const toast = useToast();
  const queryClient = useQueryClient();
  const aligned = Boolean(model.catalog_aligned);
  const [confirm, setConfirm] = useState<'reset' | 'align' | null>(null);
  // A read-only GET, asked for once per open: a failure shows its reason
  // and Try again rather than retrying behind "Working out the prices…".
  // The key changes when the model's prices or switch change, which is
  // exactly when the plan does.
  const plan = useQuery({
    queryKey: ['admin', 'model-catalog-plan', model.id, model.rate_effective_from, aligned],
    queryFn: () => previewModelCatalogReset(model.id).then((r) => r.plan),
    retry: false,
    refetchOnWindowFocus: false,
  });
  const refresh = (next?: Model) => {
    if (next) onChanged(next);
    void queryClient.invalidateQueries({ queryKey: ['admin', 'models'] });
    void queryClient.invalidateQueries({ queryKey: ['models'] });
  };
  const reset = useMutation({
    mutationFn: () => resetModelToCatalog(model.id),
    onSuccess: (res) => {
      setConfirm(null);
      toast(t('adminModels.catalogAlign.done'));
      refresh(res.model);
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });
  const turnOff = useMutation({
    mutationFn: () => patchModel(model.id, { catalog_aligned: false }),
    onSuccess: (res) => {
      toast(t('adminModels.catalogAlign.turnedOff'));
      refresh(res.model);
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });
  const busy = disabled || reset.isPending || turnOff.isPending;
  const available = plan.data?.available ?? false;
  const unavailable = plan.data && !plan.data.available ? plan.data.message : undefined;
  const blockedByDraft = pricesDirty && !aligned;

  return (
    <section className="catalog-align" aria-label={t('adminModels.catalogAlign.sectionLabel')}>
      <label className="switch">
        <input
          type="checkbox"
          role="switch"
          aria-label={t('adminModels.catalogAlign.switchLabel')}
          checked={aligned}
          disabled={busy || (!aligned && (!available || blockedByDraft))}
          onChange={(event) => {
            if (event.target.checked) setConfirm('align');
            else turnOff.mutate();
          }}
        />
        <span>{t('adminModels.catalogAlign.switchLabel')}</span>
      </label>
      <p className="small muted" style={{ margin: 0 }}>
        {t(aligned ? 'adminModels.catalogAlign.onHint' : 'adminModels.catalogAlign.offHint')}
      </p>
      {aligned && providerPriced(model) ? (
        <p className="small" style={{ margin: 0 }} data-testid="provider-price-note">
          {t('adminModels.catalogAlign.providerNote', { provider: model.upstream_name })}
        </p>
      ) : null}
      {aligned && pricesDirty ? (
        <p className="small catalog-align-warning" role="status" style={{ margin: 0 }}>
          {t('adminModels.catalogAlign.editWarning')}
        </p>
      ) : null}
      <div className="row wrap" style={{ gap: 'var(--janus-space-2)' }}>
        <button
          className="btn btn-sm"
          type="button"
          disabled={busy || !available || pricesDirty}
          aria-describedby={unavailable || pricesDirty ? 'catalog-reset-reason' : undefined}
          onClick={() => setConfirm('reset')}
        >
          {t('adminModels.catalogAlign.reset')}
        </button>
        {plan.data ? null : <CatalogPreviewStatus query={plan} />}
      </div>
      {unavailable ? (
        <p id="catalog-reset-reason" className="small muted" style={{ margin: 0 }} data-testid="catalog-reset-reason">
          {t('adminModels.catalogAlign.unavailable', { value: unavailable })}
        </p>
      ) : pricesDirty ? (
        <p id="catalog-reset-reason" className="small muted" style={{ margin: 0 }}>
          {t('adminModels.catalogAlign.saveFirst')}
        </p>
      ) : null}
      <ConfirmDialog
        open={confirm !== null && !!plan.data}
        onClose={() => setConfirm(null)}
        onConfirm={() => reset.mutate()}
        danger={false}
        busy={reset.isPending}
        title={t(confirm === 'align' ? 'adminModels.catalogAlign.confirmAlignTitle' : 'adminModels.catalogAlign.confirmTitle')}
        consequence={t('adminModels.catalogAlign.confirmBody', { name: plan.data?.catalog ?? '' })}
        confirmLabel={t(confirm === 'align' ? 'adminModels.catalogAlign.confirmAlign' : 'adminModels.catalogAlign.confirm')}
      >
        {plan.data ? (
          <div className="stack" style={{ gap: 'var(--janus-space-2)' }}>
            <CatalogChanges plan={plan.data} />
            {plan.data.provider_lines > 0 ? (
              <p className="small" style={{ margin: 0 }}>
                {t('adminModels.catalogAlign.providerNote', { provider: model.upstream_name })}
              </p>
            ) : null}
          </div>
        ) : null}
      </ConfirmDialog>
    </section>
  );
}
