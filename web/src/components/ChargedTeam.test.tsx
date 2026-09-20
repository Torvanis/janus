import { render, screen } from '@testing-library/react';
import { describe, it, expect } from 'vitest';
import { ChargedTeam } from './ChargedTeam';

describe('request charged team', () => {
  it('uses recorded team names, not current membership', () => {
    render(<ChargedTeam event={{ team_ids: 'old', team_names: ['Former team'] }} />);
    expect(screen.getByText('Former team')).toBeTruthy();
  });
  it('distinguishes Personal from service-token work', () => {
    const view = render(<ChargedTeam event={{ team_ids: '' }} />);
    expect(screen.getByText('Personal')).toBeTruthy();
    view.rerender(<ChargedTeam event={{ team_ids: '', service_token_id: 'service' }} />);
    expect(screen.getByText('Service token')).toBeTruthy();
  });
  it('does not describe unresolved historic teams as personal', () => {
    render(<ChargedTeam event={{ team_ids: 'gone' }} />);
    expect(screen.getByText('Unavailable team')).toBeTruthy();
  });
});
