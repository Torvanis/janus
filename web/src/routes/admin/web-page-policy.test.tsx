import { afterEach, describe, expect, it } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { WebPagePolicyCard } from './WebPagePolicyCard';
import type { WebPagePolicy } from './ChatAdmin';

const base: WebPagePolicy = {
  default: true,
  allow_private: false,
  allow_networks: [],
  deny_networks: [],
  allow_domains: [],
  deny_domains: [],
  ports: ['80', '443', '8080', '8443'],
  https_only: false,
  proxy: '',
  tls_trust: 'system',
};

afterEach(cleanup);

describe('WebPagePolicyCard', () => {
  it('shows the default as public internet only', () => {
    render(<WebPagePolicyCard policy={base} />);
    expect(screen.getByText('Public internet only')).toBeTruthy();
    expect(screen.getByText('Blocked')).toBeTruthy();
    expect(screen.getByText(/direct/)).toBeTruthy();
  });

  it('shows an offline-network policy read-only', () => {
    render(
      <WebPagePolicyCard
        policy={{
          ...base,
          default: false,
          allow_private: true,
          allow_domains: ['corp.example'],
          proxy: 'http://proxy.corp:3128',
          tls_trust: 'custom-only',
        }}
      />,
    );
    expect(screen.getByText('Custom')).toBeTruthy();
    expect(screen.getByText('Allowed (JANUS_WEB_ALLOW_PRIVATE)')).toBeTruthy();
    expect(screen.getByText('corp.example')).toBeTruthy();
    expect(screen.getByText('http://proxy.corp:3128')).toBeTruthy();
    expect(screen.getByText(/Only Settings → Certificates/)).toBeTruthy();
    expect(screen.queryByRole('textbox')).toBeNull();
  });
});
