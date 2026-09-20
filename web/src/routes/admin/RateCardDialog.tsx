import { useState, type ReactNode } from 'react';
import { useMutation } from '@tanstack/react-query';
import { patchModel, type ModelRatePatch } from '../../api/client';
import type { Model } from '../../lib/types';
import { usdFromNano } from '../../lib/format';
import { Badge, Drawer, Field, useToast } from '../../components/ui';
import { t } from '../../lib/i18n';
import { useLocalOnly } from '../../app/session';

interface RateCardDialogProps {
  model: Model | null;
  onClose: () => void;
  onSaved: () => void;
}

type RateDimension = 'in' | 'out' | 'cached' | 'write5m' | 'write1h';

/**
 * Which optional dimensions each provider family actually bills, so the
 * dialog can say "typically $0 for X" next to the fields an admin should skip
 * and "billed by X" next to the ones they must look up. Input and Output are
 * universal and never listed here. Unknown adapter types fall through to a
 * neutral guide with no per-field verdict.
 */
const PROVIDER_BILLED: Record<string, RateDimension[]> = {
  anthropic: ['cached', 'write5m', 'write1h'],
  bedrock: ['cached', 'write5m'],
  vertex: ['cached'],
  openai_compatible: ['cached'],
  ollama: [],
};

const PROVIDER_LABEL: Record<string, string> = {
  anthropic: 'Anthropic',
  bedrock: 'Bedrock',
  vertex: 'Vertex AI',
  openai_compatible: 'OpenAI-compatible providers',
  ollama: 'Ollama',
};

type GuideKey = keyof typeof import('../../lib/i18n').en.adminModels.rateProviderGuide;

function guideKeyFor(adapterType: string): GuideKey {
  return adapterType in PROVIDER_BILLED ? (adapterType as GuideKey) : 'other';
}

/**
 * Client-side rate validation: numeric and ≥ 0, no upper bound. Number() (not
 * parseFloat) so trailing garbage like "50x" is rejected rather than truncated.
 */
function rateError(value: string): string | undefined {
  const trimmed = value.trim();
  if (trimmed === '' || Number.isNaN(Number(trimmed))) return t('adminModels.rateNumberError');
  if (Number(trimmed) < 0) return t('adminModels.rateNegativeError');
  return undefined;
}

/**
 * The rate-card editor: all five billing dimensions, versioned server-side.
 * Used from the models-admin table and the upstream-detail drawer. $0 is a
 * valid explicit price (self-hosted models) and there is no client-side
 * ceiling — $50/MTok-class rates are legitimate.
 */
export function RateCardDialog({ model, onClose, onSaved }: RateCardDialogProps): ReactNode {
  // Local-only mode (JANUS_LOCAL_ONLY): rate-card editing is a pricing
  // surface, so the dialog never renders — defense in depth on top of the
  // callers hiding their Rates buttons.
  const localOnly = useLocalOnly();
  const toast = useToast();
  const [initialised, setInitialised] = useState<string | null>(null);
  const [rateIn, setRateIn] = useState('0');
  const [rateOut, setRateOut] = useState('0');
  const [rateCached, setRateCached] = useState('0');
  const [rateWrite5m, setRateWrite5m] = useState('0');
  const [rateWrite1h, setRateWrite1h] = useState('0');
  const [touched, setTouched] = useState(false);

  // Reset the form whenever a different model is opened.
  if (model && initialised !== model.id) {
    setInitialised(model.id);
    setRateIn(String(usdFromNano(model.rate_in_nanousd)));
    setRateOut(String(usdFromNano(model.rate_out_nanousd)));
    setRateCached(String(usdFromNano(model.rate_cached_nanousd)));
    setRateWrite5m(String(usdFromNano(model.rate_cache_write_5m_nanousd)));
    setRateWrite1h(String(usdFromNano(model.rate_cache_write_1h_nanousd)));
    setTouched(false);
  }

  const save = useMutation({
    mutationFn: () => {
      const body: ModelRatePatch = {
        rate_in_usd_per_mtok: Number(rateIn.trim()),
        rate_out_usd_per_mtok: Number(rateOut.trim()),
        rate_cached_usd_per_mtok: Number(rateCached.trim()),
        rate_cache_write_5m_usd_per_mtok: Number(rateWrite5m.trim()),
        rate_cache_write_1h_usd_per_mtok: Number(rateWrite1h.trim()),
      };
      return patchModel(model?.id ?? '', body);
    },
    onSuccess: () => {
      toast(t('adminModels.rateSavedToast'));
      onSaved();
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  if (!model || localOnly) return null;

  const errors = {
    in: rateError(rateIn),
    out: rateError(rateOut),
    cached: rateError(rateCached),
    write5m: rateError(rateWrite5m),
    write1h: rateError(rateWrite1h),
  };
  const invalid = Object.values(errors).some(Boolean);
  const show = (error: string | undefined) => (touched ? error : undefined);

  const numberInput = (value: string, onChange: (next: string) => void, error: string | undefined): ReactNode => (
    <input
      className="input"
      type="number"
      min="0"
      step="0.01"
      inputMode="decimal"
      value={value}
      onChange={(event) => onChange(event.target.value)}
      onBlur={() => setTouched(true)}
      aria-invalid={Boolean(show(error))}
      placeholder="0.00"
    />
  );

  // Provider context: the model row carries its upstream's adapter type, so
  // both callers (models table, upstream drawer) get tailored hints for free.
  const adapterType = model.adapter_type;
  const billed = PROVIDER_BILLED[adapterType];
  const providerLabel = PROVIDER_LABEL[adapterType] ?? model.upstream_name;
  const verdict = (dimension: RateDimension): ReactNode => {
    if (!billed) return null;
    return billed.includes(dimension) ? (
      <Badge tone="info">{t('adminModels.rateTypicallyBilled', { provider: providerLabel })}</Badge>
    ) : (
      <Badge tone="neutral">{t('adminModels.rateTypicallyZero', { provider: providerLabel })}</Badge>
    );
  };
  const optionalField = (
    dimension: RateDimension,
    label: string,
    hint: string,
    value: string,
    onChange: (next: string) => void,
    error: string | undefined,
  ): ReactNode => (
    <Field
      label={
        <span className="row" style={{ gap: 'var(--janus-space-2)', alignItems: 'center', flexWrap: 'wrap' }}>
          {label}
          {verdict(dimension)}
        </span>
      }
      hint={hint}
      error={show(error)}
    >
      {numberInput(value, onChange, error)}
    </Field>
  );

  return (
    <Drawer open onClose={onClose} title={t('adminModels.rateDrawerTitle', { name: model.name })}>
      <div className="banner banner-info">
        <div>
          {t('adminModels.ratesVersionedPrefix')} <strong>{t('adminModels.ratesVersionedEmphasis')}</strong>
          {t('adminModels.ratesVersionedSuffix')}
        </div>
      </div>

      <section className="stack" aria-labelledby="rate-group-universal" style={{ gap: 'var(--janus-space-3)' }}>
        <div>
          <h3 id="rate-group-universal" className="small" style={{ margin: 0, fontWeight: 600 }}>
            {t('adminModels.rateGroupUniversal')}
          </h3>
          <p className="small muted" style={{ margin: 0 }}>
            {t('adminModels.rateGroupUniversalHint')}
          </p>
        </div>
        <Field label={t('adminModels.rateInLabel')} hint={t('adminModels.rateInHint')} required error={show(errors.in)}>
          {numberInput(rateIn, setRateIn, errors.in)}
        </Field>
        <Field label={t('adminModels.rateOutLabel')} hint={t('adminModels.rateOutHint')} required error={show(errors.out)}>
          {numberInput(rateOut, setRateOut, errors.out)}
        </Field>
      </section>

      <section className="stack" aria-labelledby="rate-group-optional" style={{ gap: 'var(--janus-space-3)' }}>
        <div>
          <h3 id="rate-group-optional" className="small" style={{ margin: 0, fontWeight: 600 }}>
            {t('adminModels.rateGroupOptional')}
          </h3>
          <p className="small muted" style={{ margin: 0 }}>
            {t('adminModels.rateGroupOptionalHint')}
          </p>
        </div>
        {optionalField(
          'cached',
          t('adminModels.rateCachedLabel'),
          t('adminModels.rateCachedHint'),
          rateCached,
          setRateCached,
          errors.cached,
        )}
        {optionalField(
          'write5m',
          t('adminModels.rateCacheWrite5mLabel'),
          t('adminModels.rateCacheWrite5mHint'),
          rateWrite5m,
          setRateWrite5m,
          errors.write5m,
        )}
        {optionalField(
          'write1h',
          t('adminModels.rateCacheWrite1hLabel'),
          t('adminModels.rateCacheWrite1hHint'),
          rateWrite1h,
          setRateWrite1h,
          errors.write1h,
        )}
      </section>

      <div className="card stack" style={{ gap: 'var(--janus-space-1)' }} data-testid="rate-provider-guide">
        <span className="small" style={{ fontWeight: 600 }}>
          {t('adminModels.rateProviderGuideTitle')}
        </span>
        <p className="small muted" style={{ margin: 0 }}>
          {t(`adminModels.rateProviderGuide.${guideKeyFor(adapterType)}`)}
        </p>
      </div>

      <div className="row">
        <button
          type="button"
          className="btn btn-primary"
          onClick={() => {
            setTouched(true);
            if (!invalid) save.mutate();
          }}
          disabled={save.isPending}
        >
          {save.isPending ? t('tables.saving') : t('adminModels.saveRateCard')}
        </button>
        <button type="button" className="btn" onClick={onClose}>
          {t('common.cancel')}
        </button>
      </div>
    </Drawer>
  );
}
