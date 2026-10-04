import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { InlineText } from './DocsRoutes';

describe('docs inline text', () => {
  it('renders backticks as code and links documented error codes to the catalog', () => {
    const { container } = render(
      <MemoryRouter>
        <p>
          <InlineText text="`policy.quota_exceeded` means a quota ran out; send `Authorization: Bearer <token>`." />
        </p>
      </MemoryRouter>,
    );
    const link = container.querySelector('a.docs-code-link')!;
    expect(link.getAttribute('href')).toBe('/docs/api/errors#policy.quota_exceeded');
    expect(link.querySelector('code')!.textContent).toBe('policy.quota_exceeded');
    const codes = [...container.querySelectorAll('code')].map((c) => c.textContent);
    expect(codes).toEqual(['policy.quota_exceeded', 'Authorization: Bearer <token>']);
    expect(container.textContent).not.toContain('`');
  });
});
