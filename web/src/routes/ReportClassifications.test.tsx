import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
const mount = () =>
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <MemoryRouter>
        <ReportClassifications />
      </MemoryRouter>
    </QueryClientProvider>,
  );
import { ReportClassifications } from './ReportClassifications';
const mocks = vi.hoisted(() => ({ get: vi.fn(), put: vi.fn(), role: 'admin' }));
vi.mock('../lib/api', () => ({ api: { get: mocks.get, put: mocks.put } }));
vi.mock('../app/session', () => ({ useSession: () => ({ me: { role: mocks.role } }) }));
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  mocks.role = 'admin';
});
it('saves explicit labels without inferring provider or hosting', async () => {
  mocks.get.mockResolvedValue({
    classifications: [{ model_id: 'm1', model_name: 'Model One', family: '', provider: '', hosting: '' }],
  });
  mocks.put.mockResolvedValue({
    classification: { model_id: 'm1', model_name: 'Model One', family: 'Qwen', provider: 'Alibaba', hosting: 'self_hosted' },
  });
  mount();
  await screen.findByText('Model One');
  fireEvent.click(screen.getByRole('button', { name: 'Edit Model One' }));
  fireEvent.change(screen.getByLabelText('family'), { target: { value: 'Qwen' } });
  fireEvent.change(screen.getByLabelText('provider'), { target: { value: 'Alibaba' } });
  fireEvent.change(screen.getByLabelText('Hosting'), { target: { value: 'self_hosted' } });
  fireEvent.click(screen.getByRole('button', { name: 'Save labels' }));
  await waitFor(() =>
    expect(mocks.put).toHaveBeenCalledWith('/api/v1/reports/classifications/m1', {
      family: 'Qwen',
      provider: 'Alibaba',
      hosting: 'self_hosted',
    }),
  );
  expect(await screen.findByText('Labels saved for future requests.')).toBeTruthy();
});
it('does not fetch privileged model inventory for ordinary users', () => {
  mocks.role = 'user';
  mount();
  expect(screen.getByText('Administrator access required.')).toBeTruthy();
  expect(mocks.get).not.toHaveBeenCalled();
});
