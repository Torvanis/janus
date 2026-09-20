// Ensures web/node_modules is populated before build/test lifecycle scripts.
//
// The repository's required check runs `cd web && npm run build && npm test`
// in a fresh checkout, with no explicit install step. Without this guard the
// very first command dies with `sh: tsc: not found`. When dependencies are
// already installed (dev machines, CI jobs that ran `npm ci`), this is a
// near-instant no-op.
import { existsSync } from 'node:fs';
import { execSync } from 'node:child_process';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const webDir = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const sentinel = join(webDir, 'node_modules', '.bin', 'tsc');

if (!existsSync(sentinel)) {
  console.log('web dependencies missing — running `npm ci` once before continuing…');
  execSync('npm ci --no-audit --no-fund', { cwd: webDir, stdio: 'inherit' });
}
