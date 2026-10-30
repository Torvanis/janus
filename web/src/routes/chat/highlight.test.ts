import { describe, expect, it } from 'vitest';
import { highlight, resolveLang } from './highlight';

describe('highlight (real shiki, JavaScript regex engine)', () => {
  it('resolves fence aliases', () => {
    expect(resolveLang('ts')).toBe('typescript');
    expect(resolveLang('Bash')).toBe('shellscript');
    expect(resolveLang('c++')).toBe('cpp');
    expect(resolveLang('brainfuck')).toBeNull();
    expect(resolveLang('')).toBeNull();
  });

  it('tokenizes with both theme colours', async () => {
    const lines = await highlight('const x = 1;\nexport {};', 'ts');
    expect(lines).toHaveLength(2);
    const first = lines?.find(() => true) ?? [];
    expect(first.map((t) => t.content).join('')).toBe('const x = 1;');
    const kw = first.find((t) => t.content === 'const');
    expect(kw?.light).toMatch(/^#/);
    expect(kw?.dark).toMatch(/^#/);
    expect(kw?.light).not.toBe(kw?.dark);
  });

  it('returns null for unknown languages', async () => {
    expect(await highlight('x', 'nope')).toBeNull();
  });
});
