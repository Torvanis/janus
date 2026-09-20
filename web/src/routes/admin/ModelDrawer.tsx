import { useState, type ReactNode } from 'react';
import { useMutation } from '@tanstack/react-query';
import { hasRateCard, patchModel, type ModelRatePatch } from '../../api/client';
import { MAX_MODEL_DISPLAY_NAME, publicModelName, type Model, type ModelMetadataField } from '../../lib/types';
import { Drawer, Field, Badge, useToast } from '../../components/ui';
import { useUnsavedGuard } from '../../lib/hooks';
import { useLocalOnly } from '../../app/session';
import { t } from '../../lib/i18n';

const dimensions = [
  { field: 'context_window', patch: 'context_window', label: 'contextLabel', scale: 1 },
  { field: 'rate_in_nanousd', patch: 'rate_in_usd_per_mtok', label: 'rateInLabel', scale: 1e9 },
  { field: 'rate_out_nanousd', patch: 'rate_out_usd_per_mtok', label: 'rateOutLabel', scale: 1e9 },
  { field: 'rate_cached_nanousd', patch: 'rate_cached_usd_per_mtok', label: 'rateCachedLabel', scale: 1e9 },
  { field: 'rate_cache_write_5m_nanousd', patch: 'rate_cache_write_5m_usd_per_mtok', label: 'rateCacheWrite5mLabel', scale: 1e9 },
  { field: 'rate_cache_write_1h_nanousd', patch: 'rate_cache_write_1h_usd_per_mtok', label: 'rateCacheWrite1hLabel', scale: 1e9 },
] as const;
type MetadataKey = (typeof dimensions)[number]['field'];

// Older servers lack provenance. Preserve their saved values conservatively;
// never silently promote an absent optional rate to a free override on save.
function metadata(model: Model, key: MetadataKey): ModelMetadataField {
  if (model.metadata?.[key]) return model.metadata[key];
  const known = key === 'context_window' ? model[key] > 0 : hasRateCard(model);
  return {
    value: known ? model[key] : null,
    source: known ? 'admin' : 'unknown',
    override: known ? model[key] : null,
    automatic_value: null,
    automatic_source: 'unknown',
  };
}

export function ModelDrawer({ model, onClose, onSaved }: { model: Model; onClose: () => void; onSaved: () => void }): ReactNode {
  const localOnly = useLocalOnly();
  const toast = useToast();
  const [alias, setAlias] = useState(model.display_name);
  // Absent = untouched, null = reset, string = explicitly entered override.
  const [draft, setDraft] = useState<Partial<Record<MetadataKey, string | null>>>({});
  const [error, setError] = useState<string | null>(null);
  const dirty = alias.trim() !== model.display_name || Object.keys(draft).length > 0;
  useUnsavedGuard(dirty);
  const save = useMutation({
    mutationFn: (body: ModelRatePatch) => patchModel(model.id, body),
    onSuccess: () => {
      toast(t('adminModels.metadataSaved'));
      onSaved();
    },
    onError: (failure: Error) => setError(failure.message),
  });
  const submit = () => {
    if (!dirty || save.isPending) return;
    const body: ModelRatePatch = {};
    if (alias.trim() !== model.display_name) body.display_name = alias.trim();
    for (const d of dimensions) {
      const value = draft[d.field];
      if (value === undefined) continue;
      if (value === null) {
        body[d.patch] = null;
        continue;
      }
      const n = Number(value);
      const invalid =
        value.trim() === '' ||
        !Number.isFinite(n) ||
        n < 0 ||
        (d.scale === 1 ? !Number.isSafeInteger(n) : Math.round(n * d.scale) >= 2 ** 63);
      if (invalid) {
        setError(t(d.scale === 1 ? 'adminModels.contextNumberError' : 'adminModels.metadataNumberError'));
        return;
      }
      body[d.patch] = n;
    }
    setError(null);
    save.mutate(body);
  };
  return (
    <Drawer
      open
      title={t('adminModels.editTitle', { name: publicModelName(model) })}
      onClose={save.isPending ? () => undefined : onClose}
    >
      <form
        className="stack"
        noValidate
        onSubmit={(event) => {
          event.preventDefault();
          submit();
        }}
      >
        <Field label={t('adminModels.originalID')}>
          <input className="input mono" value={model.name} readOnly />
        </Field>
        <Field label={t('adminModels.displayNameLabel')} hint={t('adminModels.displayNameHint', { name: model.name })}>
          <input
            className="input"
            aria-label={t('adminModels.displayNameFor', { name: model.name })}
            value={alias}
            maxLength={MAX_MODEL_DISPLAY_NAME}
            disabled={save.isPending}
            onChange={(event) => {
              setAlias(event.target.value);
              setError(null);
            }}
          />
        </Field>
        {alias ? (
          <button
            className="btn btn-ghost"
            type="button"
            disabled={save.isPending}
            onClick={() => {
              setAlias('');
              setError(null);
            }}
          >
            {t('adminModels.clearDisplayName')}
          </button>
        ) : null}
        <p className="small muted">{t('adminModels.metadataHint')}</p>
        {dimensions
          .filter((d) => !localOnly || d.scale === 1)
          .map((d) => {
            const field = metadata(model, d.field);
            const edit = draft[d.field];
            const value = edit === undefined ? field.value : edit === null ? field.automatic_value : null;
            const displayed = typeof edit === 'string' ? edit : value === null ? '' : String(value / d.scale);
            const source = edit === undefined ? field.source : edit === null ? field.automatic_source : 'admin';
            const label = t(`adminModels.${d.label}`);
            return (
              <div key={d.field} className="stack" style={{ gap: 'var(--janus-space-2)' }}>
                <Field label={label}>
                  <input
                    className="input"
                    type="number"
                    min="0"
                    step={d.scale === 1 ? '1' : 'any'}
                    placeholder={t(d.scale === 1 ? 'adminModels.metadataSources.unknown' : 'common.notSet')}
                    value={displayed}
                    disabled={save.isPending}
                    onChange={(event) => {
                      setDraft({ ...draft, [d.field]: event.target.value });
                      setError(null);
                    }}
                  />
                </Field>
                <div className="row-between">
                  <Badge tone={source === 'admin' ? 'info' : 'neutral'}>{t(`adminModels.metadataSources.${source}`)}</Badge>
                  <button
                    className="btn btn-ghost btn-sm"
                    type="button"
                    aria-label={t('adminModels.automaticFor', { name: label })}
                    disabled={source !== 'admin' || save.isPending}
                    onClick={() => {
                      setDraft({ ...draft, [d.field]: null });
                      setError(null);
                    }}
                  >
                    {t('adminModels.automatic')}
                  </button>
                </div>
              </div>
            );
          })}
        {!localOnly && model.metadata_warnings?.length ? (
          <div className="banner banner-warning">
            <div>
              <p>{t('adminModels.metadataWarnings')}</p>
              <ul>
                {model.metadata_warnings.map((warning) => (
                  <li key={warning}>{warning}</li>
                ))}
              </ul>
            </div>
          </div>
        ) : null}
        {!localOnly ? (
          <p className="small muted">
            {t('adminModels.ratesVersionedPrefix')} <strong>{t('adminModels.ratesVersionedEmphasis')}</strong>
            {t('adminModels.ratesVersionedSuffix')}
          </p>
        ) : null}
        {error ? (
          <p role="alert" className="small" style={{ color: 'var(--janus-color-danger-fg)' }}>
            {error}
          </p>
        ) : null}
        <div className="row">
          <button className="btn btn-primary" type="submit" disabled={!dirty || save.isPending}>
            {save.isPending ? t('tables.saving') : t('common.save')}
          </button>
          <button className="btn" type="button" disabled={save.isPending} onClick={onClose}>
            {t('common.cancel')}
          </button>
        </div>
      </form>
    </Drawer>
  );
}
