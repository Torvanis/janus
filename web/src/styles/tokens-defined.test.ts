import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';
import { expect, it } from 'vitest';

// A var(--janus-…) with no definition and no fallback renders as nothing: the
// usage bar vanished at 70–89% because its color token did not exist. Every
// token the app uses must be defined (or carry a fallback).
const SRC = join(__dirname, '..');

function files(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) return files(path);
    return /\.(css|tsx?)$/.test(name) && !/\.test\./.test(name) ? [path] : [];
  });
}

it('uses only design tokens that exist', () => {
  const all = files(SRC);
  const defined = new Set<string>();
  for (const f of all.filter((p) => p.endsWith('.css')))
    for (const m of readFileSync(f, 'utf8').matchAll(/(--janus-[\w-]+)\s*:/g)) defined.add(m[1]!);
  const missing: string[] = [];
  for (const f of all)
    for (const m of readFileSync(f, 'utf8').matchAll(/var\((--janus-[\w-]+)\s*(,)?/g)) {
      // A name built at runtime (`--janus-color-${tone}-fg`) is checked where it is built.
      const name = m[1]!;
      if (name.endsWith('-') || m[2]) continue;
      if (!defined.has(name)) missing.push(`${relative(SRC, f)}: ${name}`);
    }
  expect(missing).toEqual([]);
});
