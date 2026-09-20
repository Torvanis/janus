import { render, screen } from '@testing-library/react';
import { it, expect, vi } from 'vitest';
import { MemoryRouter } from 'react-router-dom';
import { RequestDetail } from './shared';
import type { UsageEvent } from '../lib/types';
vi.mock('../app/session', () => ({ useLocalOnly: () => false }));
it('request detail identifies recorded charged team', () => {
  render(
    <MemoryRouter>
      <RequestDetail
        event={
          {
            team_ids: 'old',
            team_names: ['Original team'],
            created_at: '2026-09-15T12:00:00Z',
            modality: 'chat',
            model: 'Astra',
            http_status: 200,
          } as UsageEvent
        }
      />
    </MemoryRouter>,
  );
  expect(screen.getByText('Charged to')).toBeTruthy();
  expect(screen.getByText('Original team')).toBeTruthy();
});
