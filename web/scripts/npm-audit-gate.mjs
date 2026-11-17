#!/usr/bin/env node
// CI gate: no high or critical npm advisories, except ones listed in
// audit-exceptions.json. An exception names one advisory, says why it does
// not reach the shipped bundle, and expires; after its date the gate fails
// again so the exception is looked at, not forgotten.
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const exceptions = JSON.parse(readFileSync(join(here, '..', 'audit-exceptions.json'), 'utf8'));
const today = new Date().toISOString().slice(0, 10);

let raw;
try {
  raw = execFileSync('npm', ['audit', '--json'], { cwd: join(here, '..'), encoding: 'utf8' });
} catch (err) {
  // npm audit exits non-zero when it finds anything; the JSON is still on stdout.
  raw = err.stdout;
}
const report = JSON.parse(raw);

const seen = new Map();
for (const vuln of Object.values(report.vulnerabilities ?? {})) {
  for (const via of vuln.via) {
    if (typeof via !== 'object' || !['high', 'critical'].includes(via.severity)) continue;
    const id = via.url.split('/').pop();
    seen.set(id, { pkg: via.name, title: via.title, severity: via.severity });
  }
}

let failed = false;
for (const [id, adv] of seen) {
  const ex = exceptions.find((e) => e.id === id);
  if (!ex) {
    console.error(`FAIL ${adv.severity} ${id} ${adv.pkg}: ${adv.title}`);
    failed = true;
  } else if (ex.expires < today) {
    console.error(`FAIL ${id} ${adv.pkg}: exception expired ${ex.expires} (${ex.reason})`);
    failed = true;
  } else {
    console.log(`allowed until ${ex.expires}: ${adv.severity} ${id} ${adv.pkg} (${ex.reason})`);
  }
}
for (const ex of exceptions) {
  if (!seen.has(ex.id)) console.log(`note: exception ${ex.id} no longer needed; remove it`);
}
if (failed) process.exit(1);
console.log(`npm audit gate: ${seen.size} high/critical advisories, all excepted`);
