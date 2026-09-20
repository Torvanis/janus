/**
 * A render-time throw inside a page must not blank the shell. The boundary
 * shows the failure in place, offers retry, and resets on navigation.
 */
import { describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes, Link } from 'react-router-dom';
import type { ReactNode } from 'react';
import { PageErrorBoundary } from './PageErrorBoundary';

function Bomb({ when }: { when: boolean }): ReactNode {
  if (when) throw new Error('trace is null');
  return <div>fine</div>;
}

describe('PageErrorBoundary', () => {
  it('contains a page crash, keeps the surrounding chrome, and recovers on retry', async () => {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {});
    let bad = true;
    function Page(): ReactNode {
      return <Bomb when={bad} />;
    }
    render(
      <MemoryRouter initialEntries={['/a']}>
        <div>sidebar still here</div>
        <PageErrorBoundary>
          <Routes>
            <Route path="/a" element={<Page />} />
            <Route path="/b" element={<div>page b</div>} />
          </Routes>
        </PageErrorBoundary>
        <Link to="/b">go b</Link>
      </MemoryRouter>,
    );
    expect(screen.getByText('sidebar still here')).toBeTruthy();
    expect(screen.getByTestId('page-error-boundary')).toBeTruthy();
    expect(screen.getByText('trace is null')).toBeTruthy();

    bad = false;
    await userEvent.setup().click(screen.getByRole('button', { name: 'Try again' }));
    expect(screen.getByText('fine')).toBeTruthy();
    spy.mockRestore();
  });

  it('resets when the route changes', async () => {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {});
    render(
      <MemoryRouter initialEntries={['/a']}>
        <PageErrorBoundary>
          <Routes>
            <Route path="/a" element={<Bomb when />} />
            <Route path="/b" element={<div>page b</div>} />
          </Routes>
        </PageErrorBoundary>
        <Link to="/b">go b</Link>
      </MemoryRouter>,
    );
    expect(screen.getByTestId('page-error-boundary')).toBeTruthy();
    await userEvent.setup().click(screen.getByText('go b'));
    expect(screen.getByText('page b')).toBeTruthy();
    expect(screen.queryByTestId('page-error-boundary')).toBeNull();
    spy.mockRestore();
  });
});
