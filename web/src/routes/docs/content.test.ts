import { describe, expect, it } from 'vitest';
import { ERROR_CATALOG, GLOSSARY, PAGES, pageBySlug } from './content';
import openapi from '../../../public/openapi.json';

/**
 * Every error the gateway can originate must be documented, because each error
 * surface in the app deep-links to `/docs/api/errors#<code>`. A code with no
 * entry produces a dead link at the exact moment a user is confused.
 */
const CODES_EMITTED_BY_GATEWAY = [
  'policy.quota_exceeded',
  'policy.user_disabled',
  'policy.model_not_granted',
  'policy.endpoint_blocked',
  'policy.token_invalid',
  'policy.rate_limit',
  'upstream.unavailable',
  'upstream.rate_limit',
  'invalid_request_error',
  'authentication_error',
  'permission_error',
  'server_error',
];

describe('error catalog', () => {
  it('documents every code the gateway emits', () => {
    const documented = new Set(ERROR_CATALOG.map((entry) => entry.code));
    for (const code of CODES_EMITTED_BY_GATEWAY) {
      expect(documented.has(code), `missing documentation for ${code}`).toBe(true);
    }
  });

  it('gives every entry an actionable next step and a plausible status', () => {
    for (const entry of ERROR_CATALOG) {
      expect(entry.action.length, `${entry.code} has no action`).toBeGreaterThan(20);
      expect(entry.when.length, `${entry.code} has no cause`).toBeGreaterThan(20);
      expect(entry.status).toBeGreaterThanOrEqual(400);
      expect(entry.status).toBeLessThan(600);
    }
  });

  it('uses unique anchors so deep links resolve to one section', () => {
    const codes = ERROR_CATALOG.map((entry) => entry.code);
    expect(new Set(codes).size).toBe(codes.length);
  });
});

describe('pricing tables', () => {
  it('ships one table per billing model, each row matching its columns', () => {
    const page = pageBySlug('admin/pricing');
    expect(page?.group).toBe('Admin guide');
    const tables = (page?.sections ?? []).flatMap((section) => (section.pricing ? [section.pricing] : []));
    expect(tables.map((table) => table.variant)).toEqual(['tokens', 'voice', 'image', 'transcription', 'realtime']);
    for (const table of tables) {
      const bodies = table.tabs ? table.tabs.map((tab) => tab.rows) : [table.rows ?? []];
      expect(bodies.length, `${table.caption} has no rows`).toBeGreaterThan(0);
      for (const rows of bodies) {
        expect(rows.length).toBeGreaterThan(0);
        for (const row of rows) {
          expect(row.cells.length, `${table.caption}: "${row.cells[0]}" has ${row.cells.length} cells`).toBe(
            table.columns.length,
          );
          if (row.highlight !== undefined) expect(row.highlight).toBeLessThan(row.cells.length);
        }
        // A sub-row (blank model cell) must follow a row of the same group.
        rows.forEach((row, index) => {
          if (row.group && !row.cells[0]) expect(rows[index - 1]?.group).toBe(row.group);
        });
      }
    }
  });
});

describe('documentation pages', () => {
  it('resolves every page by its slug', () => {
    for (const page of PAGES) {
      expect(pageBySlug(page.slug)?.title).toBe(page.title);
    }
    expect(pageBySlug('does-not-exist')).toBeUndefined();
  });

  it('gives every page a summary and at least one section with prose', () => {
    for (const page of PAGES) {
      expect(page.summary.length, `${page.slug} has no summary`).toBeGreaterThan(10);
      expect(page.sections.length, `${page.slug} has no sections`).toBeGreaterThan(0);
      for (const section of page.sections) {
        expect(section.body.length, `${page.slug}/${section.heading} is empty`).toBeGreaterThan(0);
      }
    }
  });

  it('covers the quick start and both admin-critical runbooks', () => {
    const slugs = PAGES.map((page) => page.slug);
    expect(slugs).toContain('getting-started');
    expect(slugs).toContain('admin/upstreams');
    expect(slugs).toContain('admin/operations');
    expect(slugs).toContain('troubleshooting');
  });

  it('ships the reference set: API reference, endpoints, environment, FAQ, changelog, and legal notices', () => {
    const slugs = PAGES.map((page) => page.slug);
    for (const required of [
      'reference/api',
      'reference/proxy-endpoints',
      'reference/environment',
      'faq',
      'changelog',
      'legal/notices',
    ]) {
      expect(slugs, `missing documentation page ${required}`).toContain(required);
    }
    // The API reference must point readers at the downloadable spec.
    expect(JSON.stringify(pageBySlug('reference/api'))).toContain('/openapi.json');
  });

  it('documents every divergence from the locked decision record', () => {
    const page = pageBySlug('reference/divergences');
    expect(page, 'reference/divergences page is missing').toBeDefined();
    const text = JSON.stringify(page);
    for (const topic of ['TimescaleDB', 'Redis', 'OTLP', 'SQLite', 'JANUS_DEV_AUTH']) {
      expect(text, `divergence page does not cover ${topic}`).toContain(topic);
    }
  });
});

describe('openapi document', () => {
  it('is OpenAPI 3.1 with the three surfaces represented', () => {
    expect(openapi.openapi.startsWith('3.1')).toBe(true);
    expect(openapi.info.title.length).toBeGreaterThan(5);
    const paths = Object.keys(openapi.paths);
    expect(paths.length).toBeGreaterThan(20);
    expect(paths.some((p) => p.startsWith('/v1/'))).toBe(true);
    expect(paths.some((p) => p.startsWith('/api/v1/') && !p.startsWith('/api/v1/admin'))).toBe(true);
    expect(paths.some((p) => p.startsWith('/api/v1/admin/'))).toBe(true);
    expect(paths).toContain('/v1/chat/completions');
    expect(paths).toContain('/v1/models');
  });

  it('declares both authentication schemes', () => {
    const schemes = openapi.components.securitySchemes;
    expect(schemes.bearerToken.type).toBe('http');
    expect(schemes.sessionCookie.name).toBe('janus_session');
  });

  it('keeps quota enums in step with the documented values', () => {
    const quota = openapi.components.schemas.QuotaInput.properties;
    expect(quota.metric.enum).toEqual(['tokens_in', 'tokens_out', 'cost_usd', 'requests']);
    expect(quota.window.enum).toEqual(['daily', 'weekly', 'monthly', 'rolling_24h', 'rolling_7d', 'rolling_30d']);
  });
});

describe('glossary', () => {
  it('defines the vocabulary the UI uses without explanation', () => {
    const terms = GLOSSARY.map((entry) => entry.term.toLowerCase());
    for (const required of ['adapter', 'upstream', 'grant', 'rate card', 'modality', 'cached tokens']) {
      expect(terms, `glossary is missing "${required}"`).toContain(required);
    }
  });
});
