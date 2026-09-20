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
import type { Model } from '../lib/types';

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
