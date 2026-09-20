import { describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import DocsRoutes from './DocsRoutes';

vi.mock('../../app/session', () => ({
  useSession: () => ({ me: null, config: null }),
}));

describe('DocsSearchBox', () => {
  // Regression: submitting the search form used window.location.assign, which
  // reloads the whole document and drops SPA state. In jsdom (as in the SPA)
  // that navigation never renders the search route; a client-side
  // useNavigate() push does.
  it('navigates to the search route client-side on submit', async () => {
    render(
      <MemoryRouter initialEntries={['/docs']}>
        <Routes>
          <Route path="/docs/*" element={<DocsRoutes />} />
        </Routes>
      </MemoryRouter>,
    );

    const input = screen.getByLabelText('Search documentation');
    await userEvent.type(input, 'token{Enter}');

    expect(await screen.findByRole('heading', { name: 'Search' })).toBeTruthy();
  });
});

describe('Pricing by modality', () => {
  it('renders each modality table in its own shape, with tiers switchable by tab', async () => {
    render(
      <MemoryRouter initialEntries={['/docs/admin/pricing']}>
        <Routes>
          <Route path="/docs/*" element={<DocsRoutes />} />
        </Routes>
      </MemoryRouter>,
    );
    expect(await screen.findByRole('heading', { name: 'Pricing by modality' })).toBeTruthy();

    // One table per billing model, each carrying its variant class so the
    // stylesheet can lay it out differently.
    const figures = document.querySelectorAll('figure.docs-pricing');
    const variants = [...figures].map((el) => el.getAttribute('data-variant'));
    expect(variants).toEqual(['tokens', 'voice', 'image', 'transcription', 'realtime']);

    // Grouped variants show one block per model with modality pills on sub-rows.
    const realtime = document.querySelector('figure.docs-pricing-realtime')!;
    expect(realtime.querySelectorAll('tr.docs-pricing-group-start')).toHaveLength(2);
    expect(realtime.querySelectorAll('tr.docs-pricing-sub')).toHaveLength(3);
    expect([...realtime.querySelectorAll('.badge')].map((b) => b.textContent)).toEqual([
      'Text',
      'Audio',
      'Image',
      'Text',
      'Audio',
    ]);

    // Transcription leads with a per-minute column; voice carries the provider's own unit.
    const transcription = document.querySelector('figure.docs-pricing-transcription')!;
    expect([...transcription.querySelectorAll('th[scope="col"]')].map((th) => th.textContent)).toContain('Per minute');
    const voice = document.querySelector('figure.docs-pricing-voice')!;
    expect(voice.textContent).toContain('per 1M characters');
    expect(voice.textContent).toContain('Janus rate card');

    // Image pricing has Standard / Batch tiers; switching swaps the rows.
    const image = document.querySelector('figure.docs-pricing-image')!;
    expect(image.textContent).toContain('40.00');
    await userEvent.click(screen.getByRole('tab', { name: 'Batch' }));
    expect(image.textContent).toContain('20.00');
    expect(image.textContent).not.toContain('40.00');
  });
});
