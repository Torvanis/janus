import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { HeaderLabel, TimeCell, TopModel, Truncate, UsedBy, formatTableTime } from './TableText';
import { IconButton, RowActions } from './IconButton';

describe('Truncate', () => {
  it('always carries the full text as its tooltip', () => {
    render(<Truncate lines={2}>A very long description that gets clamped</Truncate>);
    const el = screen.getByText('A very long description that gets clamped');
    expect(el.getAttribute('title')).toBe('A very long description that gets clamped');
    expect(el.className).toBe('clamp clamp-2');
  });

  it('takes an explicit title when the children are not plain text', () => {
    render(
      <Truncate title="gpt-5 on OpenAI">
        gpt-5 <span>on OpenAI</span>
      </Truncate>,
    );
    expect(screen.getByTitle('gpt-5 on OpenAI').classList.contains('truncate')).toBe(true);
  });
});

describe('TopModel', () => {
  it('keeps "+N more" outside the ellipsis and names both in the tooltip', () => {
    render(<TopModel name="qwen3.8-flash-next-uncensored" count={13} />);
    const name = screen.getByText('qwen3.8-flash-next-uncensored');
    const more = screen.getByText('+12 more');
    expect(name.classList.contains('truncate')).toBe(true);
    expect(more.classList.contains('truncate')).toBe(false);
    expect(name.contains(more)).toBe(false);
    expect(name.parentElement?.getAttribute('title')).toBe('qwen3.8-flash-next-uncensored and 12 more');
  });

  it('shows a dash with no model and no count for a single model', () => {
    const { rerender } = render(<TopModel name={undefined} count={0} />);
    expect(screen.getByText('—')).toBeTruthy();
    rerender(<TopModel name="gpt-5" count={1} />);
    expect(screen.queryByText(/more/)).toBeNull();
  });
});

describe('HeaderLabel', () => {
  it('wraps between words only and keeps the sort arrow on the last word', () => {
    const { container } = render(
      <button type="button">
        <HeaderLabel label="Last sign-in" after={<span aria-hidden="true">↑</span>} />
      </button>,
    );
    const words = [...container.querySelectorAll('.th-word')].map((w) => w.textContent);
    expect(words).toEqual(['Last ', 'sign-in↑']);
    expect(screen.getByRole('button', { name: 'Last sign-in' })).toBeTruthy();
  });
});

describe('UsedBy', () => {
  it('keeps each name whole', () => {
    const { container } = render(
      <p>
        <UsedBy names={['auto/demo-assist', 'auto/legal']} />
      </p>,
    );
    expect(container.textContent).toBe('auto/demo-assist, auto/legal');
    expect([...container.querySelectorAll('.nowrap')].map((n) => n.textContent)).toEqual(['auto/demo-assist,', 'auto/legal']);
  });
});

describe('formatTableTime', () => {
  const now = new Date(2026, 10, 17, 15, 0);
  it('is short and one line', () => {
    expect(formatTableTime(new Date(2026, 10, 17, 9, 5).toISOString(), now)).toMatch(/^9:05\sAM$/);
    expect(formatTableTime(new Date(2026, 10, 15, 22, 23).toISOString(), now)).toMatch(/^Nov 15, 10:23\sPM$/);
    expect(formatTableTime(new Date(2025, 10, 15, 22, 23).toISOString(), now)).toBe('Nov 15, 2025');
    expect(formatTableTime(undefined, now)).toBe('—');
  });

  it('puts the full date and time in the cell tooltip', () => {
    render(
      <table>
        <tbody>
          <tr>
            <TimeCell value="2026-11-15T22:23:00Z" />
          </tr>
        </tbody>
      </table>,
    );
    const cell = screen.getByRole('cell');
    expect(cell.classList.contains('cell-time')).toBe(true);
    expect(cell.getAttribute('title')).toMatch(/2026/);
  });
});

describe('RowActions', () => {
  it('folds two or more actions into a More actions menu that lists them by name', () => {
    const onDelete = vi.fn();
    render(
      <RowActions>
        <IconButton icon="edit" label="Edit" onClick={() => {}} />
        <IconButton icon="delete" label="Delete" danger onClick={onDelete} />
      </RowActions>,
    );
    const more = screen.getByRole('button', { name: 'More actions' });
    expect(more.getAttribute('aria-expanded')).toBe('false');
    fireEvent.click(more);
    const menu = screen.getByRole('group', { name: 'Actions' });
    expect(menu.classList.contains('row-actions-menu')).toBe(true);
    expect(menu.parentElement).toBe(document.body);
    expect(document.activeElement?.getAttribute('aria-label')).toBe('Edit');
    fireEvent.click(menu.querySelector('[aria-label="Delete"]')!);
    expect(onDelete).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole('group', { name: 'Actions' })).toBeNull();
  });

  it('closes on Escape and returns focus to the button', () => {
    render(
      <RowActions>
        <IconButton icon="edit" label="Edit" onClick={() => {}} />
        <IconButton icon="delete" label="Delete" onClick={() => {}} />
      </RowActions>,
    );
    const more = screen.getByRole('button', { name: 'More actions' });
    fireEvent.click(more);
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(screen.queryByRole('group', { name: 'Actions' })).toBeNull();
    expect(document.activeElement).toBe(more);
  });

  it('has no menu for a single action', () => {
    render(
      <RowActions>
        <IconButton icon="edit" label="Edit" onClick={() => {}} />
        {null}
      </RowActions>,
    );
    expect(screen.queryByRole('button', { name: 'More actions' })).toBeNull();
  });
});

// Layout contracts the components rely on (jsdom does not lay out, so pin the CSS).
describe('table layout CSS', () => {
  const css = readFileSync(join(__dirname, '../styles/global.css'), 'utf8');
  const rule = (selector: string): string => {
    const at = css.indexOf(`${selector} {`);
    expect(at, `${selector} rule`).toBeGreaterThanOrEqual(0);
    return css.slice(at, css.indexOf('}', at));
  };

  it('scrolls a table inside a wrap no taller than the window', () => {
    const wrap = rule('.table-wrap');
    expect(wrap).toMatch(/overflow: auto;/);
    expect(wrap).toMatch(/max-height: calc\(100dvh - var\(--janus-topbar-height\)/);
  });

  it('shows the row menu button on phones only', () => {
    expect(rule('.row-actions-more')).toMatch(/display: none;/);
    const phone = css.slice(css.indexOf('.row-actions-inline {\n    display: none;'));
    expect(phone).toMatch(
      /^\.row-actions-inline \{\n {4}display: none;\n {2}\}\n {2}\.row-actions-more \{\n {4}display: inline-grid;/,
    );
    const media = css.lastIndexOf('@media (max-width: 767px)', css.indexOf('.row-actions-inline {\n    display: none;'));
    expect(css.slice(media, css.indexOf('.row-actions-inline {\n    display: none;'))).not.toMatch(/\}\n\}/);
  });

  it('keeps header words, request times and +N more on one line', () => {
    expect(rule('.th-word')).toMatch(/white-space: nowrap;/);
    expect(rule('table.data td.cell-time')).toMatch(/white-space: nowrap;/);
    expect(rule('.cell-with-more > .truncate')).toMatch(/min-width: 0;/);
  });
});
