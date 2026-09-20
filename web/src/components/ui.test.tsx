/**
 * Behavior tests for the shared UI primitives every route leans on. These are
 * the components where a silent regression is most expensive: ConfirmDialog
 * gates every destructive action, and AsyncSection is the single place that
 * decides between loading, error, empty, and content.
 */
import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ApiError } from '../lib/api';
import { AsyncSection, ConfirmDialog, Drawer } from './ui';

beforeEach(cleanup);

describe('overlay z-index scale', () => {
  // jsdom does not compute the stylesheet cascade, so the stacking invariant
  // is asserted directly on the token values in tokens.css. Regression test
  // for the bug where the blurred full-viewport scrim (z 800) covered the
  // drawer panel (z 700), leaving "Add upstream" (and every other Drawer)
  // blurred out.
  const here = dirname(fileURLToPath(import.meta.url));
  const tokensCss = readFileSync(resolve(here, '../styles/tokens.css'), 'utf8');

  function token(name: string): number {
    const match = tokensCss.match(new RegExp(`--janus-z-${name}:\\s*(-?\\d+)\\s*;`));
    if (!match) throw new Error(`token --janus-z-${name} not found in tokens.css`);
    return Number(match[1]);
  }

  it('stacks the scrim below the drawer, and the drawer below the modal', () => {
    const scrim = token('scrim');
    const drawer = token('drawer');
    const modal = token('modal');
    expect(scrim).toBeLessThan(drawer);
    expect(drawer).toBeLessThan(modal);
  });

  it('keeps the scrim above ordinary page chrome (dropdowns, sticky bars)', () => {
    expect(token('dropdown')).toBeLessThan(token('scrim'));
    expect(token('sticky')).toBeLessThan(token('scrim'));
  });

  it('Drawer renders the scrim before the panel in DOM order', () => {
    const { container } = render(
      <Drawer open onClose={() => {}} title="Add upstream">
        <p>form</p>
      </Drawer>,
    );
    const scrim = container.querySelector('.scrim');
    const panel = container.querySelector('.drawer');
    expect(scrim).not.toBeNull();
    expect(panel).not.toBeNull();
    // The panel must come after the scrim so equal-context stacking can never
    // put the blur layer on top of the drawer content.
    expect(scrim!.compareDocumentPosition(panel!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });
});

describe('ConfirmDialog', () => {
  it('confirms immediately when no typed confirmation is required', async () => {
    const onConfirm = vi.fn();
    const user = userEvent.setup();
    render(
      <ConfirmDialog
        open
        onClose={() => {}}
        onConfirm={onConfirm}
        title="Revoke token?"
        consequence="Tools using it will fail."
        confirmLabel="Revoke token"
      />,
    );
    const confirm = screen.getByRole('button', { name: 'Revoke token' });
    expect((confirm as HTMLButtonElement).disabled).toBe(false);
    await user.click(confirm);
    expect(onConfirm).toHaveBeenCalledTimes(1);
  });

  it('keeps the confirm button disabled until the exact resource name is typed', async () => {
    const onConfirm = vi.fn();
    const user = userEvent.setup();
    render(
      <ConfirmDialog
        open
        onClose={() => {}}
        onConfirm={onConfirm}
        title="Delete upstream?"
        consequence="Every model behind it disappears."
        confirmLabel="Delete upstream"
        requireTyped="openai-prod"
      />,
    );
    const confirm = screen.getByRole('button', { name: 'Delete upstream' }) as HTMLButtonElement;
    expect(confirm.disabled).toBe(true);

    const input = screen.getByPlaceholderText('openai-prod');
    await user.type(input, 'openai-prd');
    expect(confirm.disabled).toBe(true);
    // Clicking a disabled button must not fire the destructive action.
    await user.click(confirm);
    expect(onConfirm).not.toHaveBeenCalled();

    await user.clear(input);
    await user.type(input, 'openai-prod');
    expect(confirm.disabled).toBe(false);
    await user.click(confirm);
    expect(onConfirm).toHaveBeenCalledTimes(1);
  });

  it('accepts the typed name with surrounding whitespace and disables everything while busy', async () => {
    const user = userEvent.setup();
    const { rerender } = render(
      <ConfirmDialog
        open
        onClose={() => {}}
        onConfirm={() => {}}
        title="Delete?"
        consequence="Gone."
        confirmLabel="Delete"
        requireTyped="thing"
      />,
    );
    await user.type(screen.getByPlaceholderText('thing'), '  thing  ');
    expect((screen.getByRole('button', { name: 'Delete' }) as HTMLButtonElement).disabled).toBe(false);

    rerender(
      <ConfirmDialog
        open
        onClose={() => {}}
        onConfirm={() => {}}
        title="Delete?"
        consequence="Gone."
        confirmLabel="Delete"
        requireTyped="thing"
        busy
      />,
    );
    expect((screen.getByRole('button', { name: 'Working…' }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole('button', { name: 'Cancel' }) as HTMLButtonElement).disabled).toBe(true);
  });
});

describe('AsyncSection', () => {
  const baseQuery = { data: undefined, isLoading: false, error: null, refetch: () => {} };

  it('shows a skeleton while loading', () => {
    const { container } = render(<AsyncSection query={{ ...baseQuery, isLoading: true }}>{() => <p>content</p>}</AsyncSection>);
    expect(container.querySelector('.skeleton')).not.toBeNull();
    expect(screen.queryByText('content')).toBeNull();
  });

  it('renders a human error state with retry wired to refetch', async () => {
    const refetch = vi.fn();
    const user = userEvent.setup();
    const error = new ApiError(500, {
      code: 'internal_error',
      type: 'api_error',
      message: 'The gateway hit an unexpected error.',
      request_id: 'req-123',
    });
    render(<AsyncSection query={{ ...baseQuery, error, refetch }}>{() => <p>content</p>}</AsyncSection>);
    expect(screen.getByRole('alert')).toBeTruthy();
    expect(screen.getByText('The gateway hit an unexpected error.')).toBeTruthy();
    expect(screen.getByText('Request ID req-123')).toBeTruthy();
    await user.click(screen.getByRole('button', { name: 'Try again' }));
    expect(refetch).toHaveBeenCalledTimes(1);
  });

  it('renders the empty state when the predicate matches', () => {
    render(
      <AsyncSection
        query={{ ...baseQuery, data: [] as string[] }}
        empty={{ when: (rows) => rows.length === 0, title: 'Nothing here', body: 'Create one to get started.' }}
      >
        {() => <p>content</p>}
      </AsyncSection>,
    );
    expect(screen.getByText('Nothing here')).toBeTruthy();
    expect(screen.queryByText('content')).toBeNull();
  });

  it('renders content when data is present', () => {
    render(
      <AsyncSection
        query={{ ...baseQuery, data: ['row'] }}
        empty={{ when: (rows) => rows.length === 0, title: 'Nothing here', body: 'Create one.' }}
      >
        {(rows) => <p>loaded {rows.length}</p>}
      </AsyncSection>,
    );
    expect(screen.getByText('loaded 1')).toBeTruthy();
  });

  it('prefers stale data over the error state once something has rendered', () => {
    render(
      <AsyncSection query={{ ...baseQuery, data: ['row'], error: new Error('refresh failed') }}>
        {(rows) => <p>loaded {rows.length}</p>}
      </AsyncSection>,
    );
    expect(screen.getByText('loaded 1')).toBeTruthy();
    expect(screen.getByRole('alert').textContent).toContain('Refresh failed. Showing previously loaded data.');
    expect(screen.getByRole('button', { name: 'Retry' })).toBeTruthy();
  });
});
