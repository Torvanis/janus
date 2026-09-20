#!/usr/bin/env node
/**
 * gen-docs — build-time documentation generator.
 *
 * Reads the repository-root CHANGELOG.md (the authoritative release history)
 * and emits `src/routes/docs/changelog.generated.ts`, the typed data module
 * behind the in-product `/docs/changelog` page. The generated file is
 * committed; `src/docs/gen.test.ts` regenerates it in CI and fails on any
 * drift, so hand edits to the generated module (or a malformed changelog)
 * break the build instead of shipping silently.
 *
 * Usage:
 *   node scripts/gen-docs.mjs            # (re)write the generated module
 *   node scripts/gen-docs.mjs --stdout   # print the module to stdout only
 *   node scripts/gen-docs.mjs --check    # exit 1 if the committed file drifts
 */
import { readFileSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const WEB_ROOT = new URL('../', import.meta.url);
const CHANGELOG_PATH = fileURLToPath(new URL('../CHANGELOG.md', WEB_ROOT));
const OUTPUT_PATH = fileURLToPath(new URL('src/routes/docs/changelog.generated.ts', WEB_ROOT));

/**
 * Parse a Keep-a-Changelog document into releases.
 *
 * Recognised structure:
 *   `## [version] — date`  release heading (date optional; em/en dash or hyphen)
 *   `### Category`         entry category (Added, Changed, Fixed, ...)
 *   `- entry text`         one entry; indented continuation lines are folded in
 *
 * Duplicate category headings inside one release are merged in first-seen
 * order. Releases with no entries (for example an empty `[Unreleased]`
 * placeholder) are dropped from the output — the docs page shows shipped
 * history, not scaffolding.
 */
export function parseChangelog(markdown) {
  const releases = [];
  let release = null;
  let category = null;
  let item = null;

  const flushItem = () => {
    if (release && category && item) {
      // The docs page renders plain paragraphs, so fold whitespace and strip
      // the inline markdown markers (code spans, bold, italics) that only
      // make sense in the raw markdown file.
      const text = item
        .replace(/\s+/g, ' ')
        .replace(/`([^`]*)`/g, '$1')
        .replace(/\*\*([^*]+)\*\*/g, '$1')
        .replace(/\*([^*]+)\*/g, '$1')
        .trim();
      category.items.push(text);
    }
    item = null;
  };

  for (const line of markdown.split(/\r?\n/)) {
    const releaseMatch = /^##\s+\[([^\]]+)\]\s*(?:[—–-]\s*(.+?)\s*)?$/.exec(line);
    if (releaseMatch) {
      flushItem();
      release = { version: releaseMatch[1].trim(), date: (releaseMatch[2] ?? '').trim(), categories: [] };
      releases.push(release);
      category = null;
      continue;
    }
    const categoryMatch = /^###\s+(.+?)\s*$/.exec(line);
    if (categoryMatch && release) {
      flushItem();
      const name = categoryMatch[1].trim();
      category = release.categories.find((existing) => existing.name === name);
      if (!category) {
        category = { name, items: [] };
        release.categories.push(category);
      }
      continue;
    }
    const itemMatch = /^-\s+(.*)$/.exec(line);
    if (itemMatch && release && category) {
      flushItem();
      item = itemMatch[1];
      continue;
    }
    if (item && /^\s+\S/.test(line)) {
      item += ` ${line.trim()}`;
      continue;
    }
    flushItem();
  }
  flushItem();

  return releases
    .map((entry) => ({ ...entry, categories: entry.categories.filter((cat) => cat.items.length > 0) }))
    .filter((entry) => entry.categories.length > 0);
}

/** Render the parsed releases as a deterministic TypeScript module. */
export function renderChangelogModule(releases) {
  const lines = [];
  lines.push('// GENERATED FILE — DO NOT EDIT BY HAND.');
  lines.push('// Source of truth: CHANGELOG.md at the repository root.');
  lines.push('// Regenerate with: cd web && node scripts/gen-docs.mjs');
  lines.push('// Drift-checked by src/docs/gen.test.ts.');
  lines.push('');
  lines.push('export interface ChangelogCategory {');
  lines.push('  name: string;');
  lines.push('  items: string[];');
  lines.push('}');
  lines.push('');
  lines.push('export interface ChangelogRelease {');
  lines.push('  version: string;');
  lines.push('  /** Release date as written in CHANGELOG.md; empty when undated. */');
  lines.push('  date: string;');
  lines.push('  categories: ChangelogCategory[];');
  lines.push('}');
  lines.push('');
  lines.push('export const CHANGELOG: ChangelogRelease[] = [');
  for (const release of releases) {
    lines.push('  {');
    lines.push(`    version: ${quote(release.version)},`);
    lines.push(`    date: ${quote(release.date)},`);
    lines.push('    categories: [');
    for (const category of release.categories) {
      lines.push('      {');
      lines.push(`        name: ${quote(category.name)},`);
      lines.push('        items: [');
      for (const entry of category.items) {
        lines.push(`          ${quote(entry)},`);
      }
      lines.push('        ],');
      lines.push('      },');
    }
    lines.push('    ],');
    lines.push('  },');
  }
  lines.push('];');
  lines.push('');
  return lines.join('\n');
}

/** Single-quoted TypeScript string literal (matches the codebase style). */
function quote(value) {
  return `'${value.replace(/\\/g, '\\\\').replace(/'/g, "\\'")}'`;
}

/** Generate the module content from the checked-in CHANGELOG.md. */
export function generate() {
  const releases = parseChangelog(readFileSync(CHANGELOG_PATH, 'utf8'));
  if (releases.length === 0) {
    throw new Error(`gen-docs: no release headings with entries found in ${CHANGELOG_PATH}`);
  }
  return renderChangelogModule(releases);
}

const invokedDirectly = process.argv[1] !== undefined && import.meta.url === new URL(`file://${process.argv[1]}`, 'file://').href;

if (invokedDirectly || process.argv[1]?.endsWith('gen-docs.mjs')) {
  const mode = process.argv[2] ?? '';
  const content = generate();
  if (mode === '--stdout') {
    process.stdout.write(content);
  } else if (mode === '--check') {
    let existing = '';
    try {
      existing = readFileSync(OUTPUT_PATH, 'utf8');
    } catch {
      // Missing output file: keep the empty sentinel so --check reports drift.
    }
    if (existing !== content) {
      console.error(`gen-docs: ${OUTPUT_PATH} is out of date — run: cd web && node scripts/gen-docs.mjs`);
      process.exit(1);
    }
    console.log('gen-docs: changelog.generated.ts is up to date');
  } else {
    writeFileSync(OUTPUT_PATH, content);
    console.log(`gen-docs: wrote ${OUTPUT_PATH}`);
  }
}
