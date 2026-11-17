/**
 * Layout contracts from the November 2026 UI audit. Each rule below fixed a
 * page that scrolled sideways or hid content at some window size; jsdom has
 * no layout engine, so the contract is pinned on the stylesheet itself (the
 * live before/after measurements are in the commit message).
 */
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

const src = resolve(__dirname, '..');
const globalCss = readFileSync(join(src, 'styles/global.css'), 'utf8');
const loginCss = readFileSync(join(src, 'routes/login.css'), 'utf8');

/** The declarations of the first rule whose selector list is exactly `selector`. */
function rule(css: string, selector: string): string {
  const escaped = selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  const m = new RegExp(`(?:^|\\n)${escaped}\\s*\\{([^}]*)\\}`).exec(css);
  if (!m) throw new Error(`no rule for ${selector}`);
  return m[1]!;
}

function tsxFiles(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) return name === 'node_modules' ? [] : tsxFiles(path);
    return path.endsWith('.tsx') && !path.includes('.test.') ? [path] : [];
  });
}

describe('layout contracts', () => {
  it('segmented tabs and filters wrap onto more rows instead of scrolling labels out of sight', () => {
    const segmented = rule(globalCss, '.segmented');
    expect(segmented).toMatch(/flex-wrap:\s*wrap/);
    expect(segmented).not.toMatch(/overflow-x:\s*auto/);
    expect(rule(globalCss, '.segmented button,\n.segmented a')).toMatch(/max-width:\s*100%/);
    // The opt-in modifier is gone: every segmented control wraps.
    expect(globalCss).not.toContain('segmented-wrap');
  });

  it('grid items may shrink below their content, so one wide card cannot widen the page', () => {
    expect(rule(globalCss, '.grid > *')).toMatch(/min-width:\s*0/);
  });

  it('a drawer body scrolls instead of growing past the panel', () => {
    const body = rule(globalCss, '.drawer-body');
    expect(body).toMatch(/overflow-y:\s*auto/);
    expect(body).toMatch(/min-height:\s*0/);
  });

  it('lookup suggestions are wide enough for an email but never wider than the window', () => {
    const menu = rule(globalCss, '.lookup-menu');
    expect(menu).toMatch(/width:\s*max\(100%,\s*min\(22rem,\s*calc\(100vw - 24px\)\)\)/);
    expect(menu).not.toMatch(/inset-inline:\s*0/);
  });

  it('touch screens get 40px tap targets and 18px checkboxes', () => {
    const coarse = [...globalCss.matchAll(/@media \(pointer: coarse\) \{([\s\S]*?)\n\}/g)].map((m) => m[1]).join('\n');
    expect(coarse).toMatch(/\.btn-sm \{\s*min-height: 40px;/);
    expect(coarse).toMatch(/\.icon-btn \{\s*width: 40px;\s*height: 40px;/);
    expect(coarse).toMatch(/input\[type='checkbox'\],\s*input\[type='radio'\] \{\s*width: 18px;\s*height: 18px;/);
  });

  it('the sign-in notice fits the window: its text scrolls and the button stays on screen', () => {
    expect(rule(loginCss, '.auth-card-consent')).toMatch(/max-height:\s*calc\(100dvh/);
    const text = rule(loginCss, '.consent-text');
    expect(text).toMatch(/overflow-y:\s*auto/);
    expect(text).not.toMatch(/max-height/);
  });

  it('no component uses the undefined table-scroll class (its tables widened the page)', () => {
    const offenders = tsxFiles(src).filter((f) => /className="table-scroll"/.test(readFileSync(f, 'utf8')));
    expect(offenders).toEqual([]);
  });
});
