/**
 * Drift check for the generated documentation data.
 *
 * `scripts/gen-docs.mjs` derives `src/routes/docs/changelog.generated.ts`
 * from the repository-root CHANGELOG.md. Both source and output are
 * committed; this suite regenerates the module and fails when either a hand
 * edit to the generated file or an unregenerated CHANGELOG change slips in —
 * so the in-product /docs/changelog page can never drift from the file
 * operators actually read.
 */
import { execFileSync } from 'node:child_process';
import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { CHANGELOG } from '../routes/docs/changelog.generated';
import { pageBySlug } from '../routes/docs/content';

// vitest runs with the web/ directory as its root; tolerate a repo-root cwd too.
const WEB_ROOT = existsSync(join(process.cwd(), 'scripts', 'gen-docs.mjs')) ? process.cwd() : join(process.cwd(), 'web');
const SCRIPT = join(WEB_ROOT, 'scripts', 'gen-docs.mjs');
const GENERATED = join(WEB_ROOT, 'src', 'routes', 'docs', 'changelog.generated.ts');

function runGenerator(): string {
  return execFileSync(process.execPath, [SCRIPT, '--stdout'], { cwd: WEB_ROOT, encoding: 'utf8' });
}

// Spawning a Node subprocess for the generator can exceed vitest's default
// 5s timeout on slow/loaded CI runners, so these tests get an explicit budget.
const GENERATOR_TEST_TIMEOUT_MS = 60_000;

describe('generated changelog (gen-docs.mjs)', () => {
  it(
    'matches the committed changelog.generated.ts exactly (no drift, no hand edits)',
    () => {
      expect(readFileSync(GENERATED, 'utf8')).toBe(runGenerator());
    },
    GENERATOR_TEST_TIMEOUT_MS,
  );

  it(
    'is deterministic: two runs produce identical output',
    () => {
      expect(runGenerator()).toBe(runGenerator());
    },
    GENERATOR_TEST_TIMEOUT_MS,
  );

  it('contains only entries under a release heading — nothing floats', () => {
    expect(CHANGELOG.length).toBeGreaterThanOrEqual(1);
    for (const release of CHANGELOG) {
      expect(release.version.length, 'release without a version').toBeGreaterThan(0);
      expect(release.categories.length, `${release.version} has no categories`).toBeGreaterThan(0);
      for (const category of release.categories) {
        expect(category.items.length, `${release.version}/${category.name} is empty`).toBeGreaterThan(0);
        for (const item of category.items) {
          expect(item.trim().length, `${release.version}/${category.name} has a blank entry`).toBeGreaterThan(20);
        }
      }
    }
  });

  it('ends the public history with the initial public release and leads with the newest', () => {
    expect(CHANGELOG.length).toBeGreaterThanOrEqual(2);
    const latest = CHANGELOG[0];
    expect(latest?.version).toBe('2026.9.3');
    expect(latest?.date).toBe('2026-09-24');
    expect(JSON.stringify(latest)).toContain('janus-ctl');
    const release = CHANGELOG[CHANGELOG.length - 1];
    expect(release?.version).toBe('2026.9.1');
    expect(release?.date).toBe('2026-09-20');
    const text = JSON.stringify(release);
    expect(text).toContain('Initial public release');
    expect(text).toContain('personal Team workspaces');
    expect(text).toContain('Administration → People → Teams');
  });
});

describe('/docs/changelog page', () => {
  it('is built from the generated data, one section per release category', () => {
    const page = pageBySlug('changelog');
    expect(page).toBeDefined();
    const expectedSections = CHANGELOG.reduce((count, release) => count + release.categories.length, 0);
    expect(page?.sections.length).toBe(expectedSections);
    const headings = page?.sections.map((section) => section.heading) ?? [];
    expect(headings.some((heading) => heading.startsWith('2026.9.1'))).toBe(true);
    expect(JSON.stringify(page)).toContain('Initial public release');
  });
});
