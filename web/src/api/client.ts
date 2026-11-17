/**
 * Typed bindings for the admin model & rate-card endpoints.
 *
 * The field names below are the canonical snake_case wire contract implemented
 * by internal/httpapi/admin_handlers.go (handlePatchModel) and asserted by its
 * Go tests. This repository has no OpenAPI client generator — the published
 * spec (web/public/openapi.json) documents routes only — so these bindings are
 * hand-maintained and MUST be kept in lock-step with the Go handler when the
 * PATCH body changes.
 */

import { api, qs } from '../lib/api';
import type { MetadataSource, MetadataSourceDetail, Model } from '../lib/types';

/**
 * PATCH /api/v1/admin/models/{id} request body. All rates are USD per million
 * tokens. Every dimension accepts 0 (an explicit $0 card is how self-hosted
 * models are priced), within the signed 64-bit nano-USD limit. Omitted
 * dimensions are unchanged; null removes only that field's override.
 */
export interface ModelRatePatch {
  display_name?: string;
  context_window?: number | null;
  rate_in_usd_per_mtok?: number | null;
  rate_out_usd_per_mtok?: number | null;
  rate_cached_usd_per_mtok?: number | null;
  rate_cache_write_5m_usd_per_mtok?: number | null;
  rate_cache_write_1h_usd_per_mtok?: number | null;
  /**
   * Price lines beyond the five token rates, in USD (per 1M tokens for a
   * conditional token rate, per unit otherwise); null returns a line to its
   * automatic value.
   */
  price_lines?: Record<string, number | null>;
  /**
   * true resets the prices to the Janus price catalog and keeps them
   * aligned; false stops following the catalog (no price changes).
   */
  catalog_aligned?: boolean;
  status?: 'enabled' | 'disabled' | 'pending_approval';
  /** RFC 3339; defaults to "now" server-side when omitted. */
  effective_from?: string;
}

export interface AdminModelsResponse {
  models: Model[];
  counts: Record<string, number>;
  modalities: Array<{ value: string; label: string }>;
}

export interface AdminModelsFilter {
  status?: string;
  upstream_id?: string;
  search?: string;
  sort?: string;
}

/** Lists discovered models across upstreams, optionally filtered. */
export function listAdminModels(filter: AdminModelsFilter = {}): Promise<AdminModelsResponse> {
  return api.get<AdminModelsResponse>(`/api/v1/admin/models${qs({ ...filter })}`);
}

/** Updates a model's status and/or appends a new rate-card version. */
export function patchModel(id: string, body: ModelRatePatch): Promise<{ model: Model }> {
  return api.patch<{ model: Model }>(`/api/v1/admin/models/${id}`, body);
}

/** One price line before and after a reset to the Janus price catalog (nano-USD; null = not priced). */
export interface CatalogPriceChange {
  line: string;
  before: number | null;
  before_source: MetadataSource;
  after: number | null;
  after_source: MetadataSource;
  after_detail?: MetadataSourceDetail;
  changed: boolean;
}

/** What a reset to the Janus price catalog does to one model (server: catalogResetPlan). */
export interface CatalogResetPlan {
  model_id: string;
  model: string;
  available: boolean;
  reason?: 'no_catalog' | 'no_provider' | 'no_entry';
  message?: string;
  catalog?: string;
  catalog_aligned: boolean;
  has_admin_prices: boolean;
  changes: CatalogPriceChange[];
  changed_lines: number;
  provider_lines: number;
}

/** A preview that has not answered by then shows an error and Try again. */
export const CATALOG_PREVIEW_TIMEOUT_MS = 20_000;

/**
 * What resetting a model's prices to the Janus price catalog would do, every
 * line before → after. Read-only (a GET): the model drawer asks for it when it
 * opens.
 */
export function previewModelCatalogReset(id: string): Promise<{ plan: CatalogResetPlan }> {
  return api.get<{ plan: CatalogResetPlan }>(`/api/v1/admin/models/${id}/pricing/catalog-preview`, {
    timeoutMs: CATALOG_PREVIEW_TIMEOUT_MS,
  });
}

/** Resets a model's prices to the Janus price catalog and keeps it aligned. */
export function resetModelToCatalog(id: string): Promise<{ plan: CatalogResetPlan; applied: boolean; model?: Model }> {
  return api.post<{ plan: CatalogResetPlan; applied: boolean; model?: Model }>(
    `/api/v1/admin/models/${id}/pricing/reset-to-catalog`,
  );
}

export interface BulkCatalogResult {
  model_id: string;
  model?: string;
  /** would_reset only in a preview. */
  outcome: 'reset' | 'would_reset' | 'skipped' | 'not_found';
  reason?: 'no_catalog' | 'no_provider' | 'no_entry' | 'prices_would_change';
  message?: string;
  changed_lines: number;
}

type BulkCatalogReply = { results: BulkCatalogResult[]; counts: Record<string, number>; applied: boolean };

/**
 * What resetting many models to the Janus price catalog would do. Read-only;
 * a POST only because the id list can be too long for a URL.
 */
export function previewBulkCatalogReset(ids: string[], onlyIfUnchanged: boolean): Promise<BulkCatalogReply> {
  return api.post<BulkCatalogReply>('/api/v1/admin/models/pricing/catalog-preview', {
    ids,
    only_if_unchanged: onlyIfUnchanged,
  });
}

/**
 * Resets many models to the Janus price catalog. onlyIfUnchanged keeps
 * aligned only the models whose prices stay the same (the bulk "Keep
 * aligned").
 */
export function bulkResetModelsToCatalog(ids: string[], onlyIfUnchanged: boolean): Promise<BulkCatalogReply> {
  return api.post<BulkCatalogReply>('/api/v1/admin/models/pricing/reset-to-catalog', {
    ids,
    only_if_unchanged: onlyIfUnchanged,
  });
}

/** True when an administrator set any of the model's prices (the context window is not a price). */
export function hasAdminPrices(model: Model): boolean {
  return Object.entries(model.metadata ?? {}).some(
    ([key, field]) => key !== 'context_window' && field.override !== null && field.override !== undefined,
  );
}

/** One saved price of a model (server: store.RateCard), nano-USD. */
export interface RateCardVersion {
  effective_from: string;
  rate_in_nanousd: number;
  rate_out_nanousd: number;
  rate_cached_nanousd: number;
  rate_cache_write_5m_nanousd: number;
  rate_cache_write_1h_nanousd: number;
  lines?: Record<string, number>;
}

/** A model's saved prices, newest first. */
export function getModelPriceHistory(id: string): Promise<{ versions: RateCardVersion[] }> {
  return api.get<{ versions: RateCardVersion[] }>(`/api/v1/admin/models/${id}/price-history`);
}

/**
 * True when a rate card has been explicitly saved for the model at some point
 * — including an all-$0 card. The gateway writes rate_effective_from on every
 * save, so the zero time (0001-01-01…) means "never priced". Mirrors the
 * enable-guard in handlePatchModel: an explicit $0/$0 card may be enabled, a
 * never-priced model may not.
 */
export function hasRateCard(model: Model): boolean {
  return Boolean(model.rate_effective_from) && !model.rate_effective_from.startsWith('0001-01-01');
}
