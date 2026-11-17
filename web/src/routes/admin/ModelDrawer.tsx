import { useState, type ReactNode } from 'react';
import { useMutation } from '@tanstack/react-query';
import { patchModel, type ModelRatePatch } from '../../api/client';
import {
  MAX_MODEL_DISPLAY_NAME,
  publicModelName,
  type MetadataSource,
  type MetadataSourceDetail,
  type Model,
  type ModelMetadataField,
} from '../../lib/types';
import { ConfirmDialog, Drawer, Field, Badge, useToast } from '../../components/ui';
import { useUnsavedGuard } from '../../lib/hooks';
import { useLocalOnly } from '../../app/session';
import { t } from '../../lib/i18n';
import { ModelCapabilities } from './ModelCapabilities';
import { PriceBuilder, draftChangeCount, draftErrors, draftPatch, emptyDraft, type PriceDraft } from './PriceBuilder';
import { CatalogAlignment } from './CatalogAlignment';

// Older servers lack provenance for the context window. Preserve a saved
// value conservatively; never silently promote an absent value on save.
function contextField(model: Model): ModelMetadataField {
  if (model.metadata?.context_window) return model.metadata.context_window;
  const known = model.context_window > 0;
  return {
    value: known ? model.context_window : null,
    source: known ? 'admin' : 'unknown',
    override: known ? model.context_window : null,
    automatic_value: null,
    automatic_source: 'unknown',
  };
}

// The text for the context window's source: the server's label (catalog
// version, provider API) when it sends one, else the layer's name.
function contextSource(source: MetadataSource, detail?: MetadataSourceDetail): string {
  if (source !== 'admin' && detail?.label) return detail.label;
  return t(`adminModels.metadataSources.${source}`);
}

export function ModelDrawer({
  model: initial,
  onClose,
  onSaved,
}: {
  model: Model;
  onClose: () => void;
  onSaved: () => void;
}): ReactNode {
  const localOnly = useLocalOnly();
  const toast = useToast();
  // A reset to the price catalog or the alignment switch saves at once and
  // keeps the drawer open on the updated model.
  const [model, setModel] = useState(initial);
  // A save that would stop the model following the price catalog, waiting
  // for the administrator to confirm it.
  const [stopping, setStopping] = useState<ModelRatePatch | null>(null);
  const [alias, setAlias] = useState(model.display_name);
  // Absent = untouched, null = reset, string = explicitly entered override.
  const [context, setContext] = useState<string | null | undefined>(undefined);
  const [prices, setPrices] = useState<PriceDraft>(emptyDraft);
  const [showErrors, setShowErrors] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const changes =
    (alias.trim() !== model.display_name ? 1 : 0) + (context !== undefined ? 1 : 0) + (localOnly ? 0 : draftChangeCount(prices));
  const dirty = changes > 0;
  useUnsavedGuard(dirty);
  const save = useMutation({
    mutationFn: (body: ModelRatePatch) => patchModel(model.id, body),
    onSuccess: () => {
      toast(t('adminModels.metadataSaved'));
      onSaved();
    },
    onError: (failure: Error) => setError(failure.message),
  });
  const discard = () => {
    setAlias(model.display_name);
    setContext(undefined);
    setPrices(emptyDraft);
    setShowErrors(false);
    setError(null);
  };
  const submit = () => {
    if (!dirty || save.isPending) return;
    const body: ModelRatePatch = {};
    if (alias.trim() !== model.display_name) body.display_name = alias.trim();
    if (context !== undefined) {
      if (context === null) body.context_window = null;
      else {
        const n = Number(context);
        if (context.trim() === '' || !Number.isSafeInteger(n) || n < 0) {
          setError(t('adminModels.contextNumberError'));
          return;
        }
        body.context_window = n;
      }
    }
    if (!localOnly) {
      if (Object.keys(draftErrors(prices)).length > 0) {
        setShowErrors(true);
        setError(t('adminModels.priceBuilder.fixFirst'));
        return;
      }
      Object.assign(body, draftPatch(prices));
    }
    setError(null);
    // Editing a price stops following the Janus price catalog: ask first.
    if (!localOnly && model.catalog_aligned && draftChangeCount(prices) > 0) {
      setStopping(body);
      return;
    }
    save.mutate(body);
  };
  const ctx = contextField(model);
  const ctxValue = context === undefined ? ctx.value : context === null ? ctx.automatic_value : null;
  const ctxShown = typeof context === 'string' ? context : ctxValue === null ? '' : String(ctxValue);
  const ctxSource = context === undefined ? ctx.source : context === null ? ctx.automatic_source : 'admin';
  const ctxDetail = context === undefined ? ctx.source_detail : context === null ? ctx.automatic_detail : undefined;
  const contextLabel = t('adminModels.contextLabel');
  return (
    <Drawer
      open
      title={t('adminModels.editTitle', { name: publicModelName(model) })}
      onClose={save.isPending ? () => undefined : onClose}
      wide
      footer={
        dirty ? (
          <div className="model-save-bar">
            <span className="small" role="status">
              {t('adminModels.priceBuilder.unsaved', { count: changes })}
            </span>
            <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
              <button className="btn" type="button" disabled={save.isPending} onClick={discard}>
                {t('adminModels.priceBuilder.discard')}
              </button>
              <button className="btn btn-primary" type="submit" form="model-drawer-form" disabled={save.isPending}>
                {save.isPending ? t('tables.saving') : t('common.save')}
              </button>
            </div>
          </div>
        ) : undefined
      }
    >
      <form
        id="model-drawer-form"
        className="stack"
        noValidate
        onSubmit={(event) => {
          event.preventDefault();
          submit();
        }}
        // Enter in a field saves, as in any form. The Save button lives in
        // the drawer's pinned footer (outside this element), so implicit
        // submission is wired here; Enter in the new-line form adds the line.
        onKeyDown={(event) => {
          const target = event.target as HTMLElement;
          if (event.key !== 'Enter' || target.tagName !== 'INPUT' || target.closest('.price-add')) return;
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
        <div className="model-meta-grid">
          <div className="model-meta-field">
            <Field label={contextLabel}>
              <input
                className="input"
                type="number"
                min="0"
                step="1"
                placeholder={t('adminModels.metadataSources.unknown')}
                value={ctxShown}
                disabled={save.isPending}
                onChange={(event) => {
                  setContext(event.target.value);
                  setError(null);
                }}
              />
            </Field>
            <div className="row-between">
              <Badge tone={ctxSource === 'admin' ? 'info' : 'neutral'}>{contextSource(ctxSource, ctxDetail)}</Badge>
              <button
                className="btn btn-ghost btn-sm"
                type="button"
                aria-label={t('adminModels.automaticFor', { name: contextLabel })}
                disabled={ctxSource !== 'admin' || save.isPending}
                onClick={() => {
                  setContext(ctx.override === null ? undefined : null);
                  setError(null);
                }}
              >
                {t('adminModels.automatic')}
              </button>
            </div>
          </div>
        </div>
        {!localOnly ? (
          <CatalogAlignment
            model={model}
            pricesDirty={draftChangeCount(prices) > 0}
            disabled={save.isPending}
            onChanged={(next) => {
              setModel(next);
              setPrices(emptyDraft);
              setShowErrors(false);
              setError(null);
            }}
          />
        ) : null}
        {!localOnly ? (
          <PriceBuilder
            model={model}
            draft={prices}
            onChange={(next) => {
              setPrices(next);
              setError(null);
            }}
            showErrors={showErrors}
            disabled={save.isPending}
          />
        ) : null}
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
      </form>
      <hr style={{ margin: 'var(--janus-space-5) 0', border: 0, borderTop: '1px solid var(--janus-color-border-subtle)' }} />
      <ModelCapabilities model={model} disabled={save.isPending} />
      <ConfirmDialog
        open={stopping !== null}
        onClose={() => setStopping(null)}
        onConfirm={() => {
          if (stopping) save.mutate(stopping);
          setStopping(null);
        }}
        danger={false}
        title={t('adminModels.catalogAlign.stopTitle')}
        consequence={t('adminModels.catalogAlign.stopBody')}
        confirmLabel={t('adminModels.catalogAlign.stopConfirm')}
      />
    </Drawer>
  );
}
