import { beforeEach, afterEach, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor, fireEvent } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api, ApiError } from '../lib/api';
import Reports from './Reports';
vi.mock('../lib/api', async (original) => ({
  ...(await original<typeof import('../lib/api')>()),
  api: { get: vi.fn(), post: vi.fn(), put: vi.fn(), del: vi.fn() },
}));
let localOnly = false;
let role = 'user';
vi.mock('../app/session', () => ({
  useLocalOnly: () => localOnly,
  useMe: () => ({
    id: 'me',
    role,
    timezone: 'UTC',
    teams: [
      { id: 't1', name: 'Engineering', my_role: 'leader' },
      { id: 't2', name: 'Other', my_role: 'member' },
    ],
  }),
}));
vi.mock('./ReportResultView', () => ({
  ReportResultView: ({ onDrillDown }: { onDrillDown?: (dimension: string, value: string) => void }) => (
    <div>
      Completed result<button onClick={() => onDrillDown?.('model', 'm1')}>Explore model</button>
    </div>
  ),
}));
const definition = {
  version: 1,
  name: 'Usage overview',
  template: 'usage',
  scope: 'self',
  team_id: '',
  period: 'last_30_days',
  start: '',
  end: '',
  timezone: 'UTC',
  compare: false,
  group_mode: 'historical',
  dimensions: ['model'],
  metrics: ['requests'],
  filters: {},
  sections: [],
  scenario_discount_percent: 0,
};
const catalog = {
  version: 1,
  templates: [definition],
  dimensions: [{ id: 'model', label: 'Model' }],
  metrics: [
    { id: 'requests', label: 'Requests' },
    { id: 'cost_nanousd', label: 'Spend', unit: 'nanousd' },
  ],
};
const mock = vi.mocked(api);
function mount(entry = '/reports') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[entry]}>
        <Reports />
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return client;
}
beforeEach(() => {
  localOnly = false;
  role = 'user';
  vi.resetAllMocks();
  mock.get.mockImplementation(async (path) => {
    if (path.endsWith('/catalog')) return catalog as never;
    if (path.split('?')[0]!.endsWith('/options')) return { dimensions: { model: [{ id: 'm1', label: 'Model One' }] } } as never;
    return { reports: [], runs: [], schedules: [], budgets: [] } as never;
  });
});
afterEach(cleanup);
it('opens a durable report in a dedicated reader and returns to the library', async () => {
  const get = mock.get.getMockImplementation()!;
  mock.get.mockImplementation(async (path) =>
    path.endsWith('/runs/reader')
      ? ({ run: { id: 'reader', definition, status: 'complete', created_at: '2026-09-01T10:00:00Z', result: {} } } as never)
      : get(path),
  );
  const user = userEvent.setup();
  mount('/reports?run=reader');
  await screen.findByText('Completed result');
  expect(screen.queryByRole('navigation', { name: 'Report workspace' })).toBeNull();
  expect(screen.queryByRole('heading', { name: 'Built-in templates' })).toBeNull();
  await user.click(screen.getByRole('button', { name: 'Back to reports' }));
  expect(await screen.findByRole('heading', { name: 'Saved reports' })).toBeTruthy();
  expect(screen.queryByText('Completed result')).toBeNull();
});
it('opens readable run actions with compact metadata and no duplicate result title', async () => {
  const get = mock.get.getMockImplementation()!;
  const reportRun = {
    id: 'opaque-id',
    definition,
    status: 'complete',
    created_at: '2026-09-01T10:00:00Z',
    expires_at: '2026-10-01T10:00:00Z',
    result: {},
  };
  mock.get.mockImplementation(async (path) =>
    path.endsWith('/runs')
      ? ({ runs: [reportRun] } as never)
      : path.endsWith('/runs/opaque-id')
        ? ({ run: reportRun } as never)
        : get(path),
  );
  const user = userEvent.setup();
  mount();
  await user.click(screen.getByRole('button', { name: 'Runs' }));
  const open = await screen.findByRole('button', { name: /^Open report: Usage overview/ });
  expect(open.textContent).toBe('Open report');
  expect(screen.queryByText(/opaque-id/)).toBeNull();
  await user.click(open);
  await screen.findByText('Completed result');
  expect(screen.getByRole('status').textContent).toContain('Ready');
  expect(screen.queryByRole('heading', { name: 'Usage overview' })).toBeNull();
  const metadata = screen.getByText('Run information').closest('details');
  expect(metadata?.open).toBe(false);
  await user.click(screen.getByText('Run information'));
  expect(screen.getByText(/Result expires/)).toBeTruthy();
  await user.click(screen.getByRole('button', { name: 'Open run definition' }));
  expect(screen.getByRole('button', { name: 'Generate report' })).toBeTruthy();
  expect(screen.queryByText('Completed result')).toBeNull();
});
it('starts scheduling from an owned saved report with a readable team scope', async () => {
  const get = mock.get.getMockImplementation()!;
  const owned = {
    id: 'saved',
    owner_user_id: 'me',
    definition: { ...definition, scope: 'team', team_id: 't1' },
    revision: 1,
    shared: false,
  };
  mock.get.mockImplementation(async (path) =>
    path === '/api/v1/reports'
      ? ({
          reports: [
            owned,
            {
              ...owned,
              id: 'shared',
              owner_user_id: 'another',
              definition: { ...definition, name: 'Shared overview' },
              shared: true,
            },
          ],
        } as never)
      : get(path),
  );
  mock.post.mockResolvedValue({ schedule: { id: 'new' } });
  const user = userEvent.setup();
  mount();
  expect(await screen.findByText(/Engineering · last 30 days/)).toBeTruthy();
  expect(screen.queryByRole('button', { name: 'Schedule Shared overview' })).toBeNull();
  await user.click(screen.getByRole('button', { name: 'Schedule Usage overview' }));
  expect(((await screen.findByRole('combobox', { name: 'Saved report' })) as HTMLSelectElement).value).toBe('saved');
  await user.click(screen.getByRole('button', { name: 'Create schedule' }));
  await waitFor(() =>
    expect(mock.post).toHaveBeenCalledWith(
      '/api/v1/reports/schedules',
      expect.objectContaining({ report_id: 'saved', enabled: true }),
    ),
  );
});
it('validates setup live and enables Generate only when the draft is valid', async () => {
  const user = userEvent.setup();
  mount();
  await user.click(await screen.findByRole('button', { name: 'Use Usage overview' }));
  await user.clear(screen.getByRole('textbox', { name: 'Report name' }));
  expect(screen.getByRole('alert').textContent).toContain('Enter a report name.');
  expect((screen.getByRole('button', { name: 'Generate report' }) as HTMLButtonElement).disabled).toBe(true);
  await user.type(screen.getByRole('textbox', { name: 'Report name' }), 'My report');
  expect(screen.queryByRole('alert')).toBeNull();
  await user.selectOptions(screen.getByRole('combobox', { name: 'Scope' }), 'team');
  expect(screen.getByRole('alert').textContent).toContain('Choose a team you lead.');
  await user.selectOptions(screen.getByRole('combobox', { name: 'Team' }), 't1');
  await user.selectOptions(screen.getByRole('combobox', { name: 'Period' }), 'custom');
  expect(screen.getByRole('alert').textContent).toContain('Start must be before end.');
  fireEvent.change(screen.getByLabelText('Start'), { target: { value: '2026-09-01T00:00' } });
  fireEvent.change(screen.getByLabelText('End'), { target: { value: '2026-09-02T00:00' } });
  expect((screen.getByRole('button', { name: 'Generate report' }) as HTMLButtonElement).disabled).toBe(false);
  expect(mock.post).not.toHaveBeenCalled();
});
it('explains executive and reliability templates as decisions rather than raw dimensions', async () => {
  const get = mock.get.getMockImplementation()!;
  mock.get.mockImplementation(async (path) =>
    path.endsWith('/catalog')
      ? ({
          ...catalog,
          templates: [
            { ...definition, name: 'Executive overview', template: 'executive' },
            { ...definition, name: 'Reliability', template: 'reliability' },
          ],
        } as never)
      : get(path),
  );
  mount();
  expect(await screen.findByText('What changed in activity, adoption, and service health?')).toBeTruthy();
  expect(screen.getByText('Where are errors and slow responses affecting the experience?')).toBeTruthy();
});
it('loads a saved reports latest outcome on a fresh Library visit', async () => {
  const get = mock.get.getMockImplementation()!;
  mock.get.mockImplementation(async (path) =>
    path === '/api/v1/reports'
      ? ({ reports: [{ id: 'saved', owner_user_id: 'me', definition, revision: 1, shared: false }] } as never)
      : path.endsWith('/runs')
        ? ({
            runs: [
              { id: 'old', report_id: 'saved', definition, status: 'failed', created_at: '2026-09-01T00:00:00Z' },
              { id: 'new', report_id: 'saved', definition, status: 'complete', created_at: '2026-09-02T00:00:00Z' },
            ],
          } as never)
        : get(path),
  );
  const user = userEvent.setup();
  mount();
  await screen.findByRole('button', { name: 'Edit Usage overview' });
  expect(await screen.findByText(/Last report:/)).toBeTruthy();
  await user.click(screen.getByRole('button', { name: 'Runs' }));
  await screen.findAllByRole('button', { name: /^Open report:/ });
  await user.click(screen.getByRole('button', { name: 'Library' }));
  expect(screen.getByText(/Last report: Ready/)).toBeTruthy();
});
it('guides template setup with a title preview and optional customization below Generate', async () => {
  const user = userEvent.setup();
  mount();
  await user.click(await screen.findByRole('button', { name: 'Use Usage overview' }));
  expect(screen.getByRole('heading', { name: 'Usage overview' })).toBeTruthy();
  const generate = screen.getByRole('button', { name: 'Generate report' });
  const customize = screen.getByText('Customize report');
  expect(customize.closest('details')?.open).toBe(false);
  expect(generate.compareDocumentPosition(customize) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  await user.click(customize);
  expect(screen.getByRole('listbox', { name: 'Metrics' })).toBeTruthy();
  expect(screen.getByRole('combobox', { name: 'Grouping 1' })).toBeTruthy();
});
it('prioritizes saved reports and describes the question a template answers', async () => {
  const user = userEvent.setup();
  mount();
  expect(await screen.findByText('How is usage changing, and which models are driving it?')).toBeTruthy();
  expect(
    screen
      .getByRole('heading', { name: 'Saved reports' })
      .compareDocumentPosition(screen.getByRole('heading', { name: 'Built-in templates' })) & Node.DOCUMENT_POSITION_FOLLOWING,
  ).toBeTruthy();
  await user.click(screen.getByRole('button', { name: 'Use Usage overview' }));
  await user.click(screen.getByText('Customize report'));
  expect(((await screen.findByRole('listbox', { name: 'Filter Model' })) as HTMLSelectElement).size).toBe(4);
});

it('omits cleared filters including the final deleted value from save and run payloads', async () => {
  const user = userEvent.setup();
  const get = mock.get.getMockImplementation()!;
  mock.get.mockImplementation(async (path) =>
    path.endsWith('/catalog')
      ? ({
          ...catalog,
          templates: [{ ...definition, filters: { model: ['deleted'], retained: ['keep'], empty: [] } }],
        } as never)
      : path.endsWith('/runs/clear-run')
        ? ({ run: { id: 'clear-run', definition, status: 'complete' } } as never)
        : get(path),
  );
  mock.post.mockImplementation(async (path) =>
    path.endsWith('/runs')
      ? ({ run: { id: 'clear-run', definition, status: 'complete' } } as never)
      : ({ report: { id: 'saved', owner_user_id: 'me', definition, revision: 1, shared: false } } as never),
  );
  mount();
  await user.click(await screen.findByRole('button', { name: 'Use Usage overview' }));
  await user.click(screen.getByText('Customize report'));
  const filter = await screen.findByRole('listbox', { name: 'Filter Model' });
  await user.selectOptions(filter, 'm1');
  await user.deselectOptions(filter, ['m1', 'deleted']);
  expect((filter as HTMLSelectElement).selectedOptions).toHaveLength(0);
  await user.click(screen.getByRole('button', { name: 'Save report' }));
  await waitFor(() =>
    expect(mock.post).toHaveBeenCalledWith(
      '/api/v1/reports',
      expect.objectContaining({
        definition: expect.objectContaining({ filters: { retained: ['keep'] } }),
      }),
    ),
  );
  await user.click(screen.getByRole('button', { name: 'Generate report' }));
  await waitFor(() =>
    expect(mock.post).toHaveBeenCalledWith(
      '/api/v1/reports/runs',
      expect.objectContaining({
        definition: expect.objectContaining({ filters: { retained: ['keep'] } }),
      }),
    ),
  );
});

it('shows only grouped, active or explicitly added filters without changing the draft on refetch', async () => {
  const user = userEvent.setup();
  const get = mock.get.getMockImplementation()!;
  const dimensions = [
    ...catalog.dimensions,
    ...Array.from({ length: 19 }, (_, i) => ({ id: `extra_${i}`, label: `Extra ${i}` })),
  ];
  const filtered = { ...definition, filters: { retired: ['deleted'] } };
  mock.get.mockImplementation(async (path) =>
    path.endsWith('/catalog') ? ({ ...catalog, dimensions, templates: [filtered] } as never) : get(path),
  );
  mock.post.mockResolvedValue({ report: { id: 'saved', owner_user_id: 'me', definition: filtered, revision: 1, shared: false } });
  const client = mount();
  await user.click(await screen.findByRole('button', { name: 'Use Usage overview' }));
  await user.click(screen.getByText('Customize report'));
  await screen.findByRole('listbox', { name: 'Filter Model' });
  expect(screen.getAllByRole('listbox', { name: /^Filter / })).toHaveLength(2);
  await user.selectOptions(screen.getByRole('combobox', { name: 'Add filter' }), 'extra_18');
  expect(screen.getByRole('listbox', { name: 'Filter Extra 18' })).toBeTruthy();
  await user.selectOptions(screen.getByRole('listbox', { name: 'Filter Model' }), 'm1');
  await user.selectOptions(screen.getByRole('combobox', { name: 'Grouping 1' }), 'extra_0');
  await client.invalidateQueries({ queryKey: ['reports', 'options'] });
  expect((screen.getByRole('listbox', { name: 'Filter Model' }) as HTMLSelectElement).value).toBe('m1');
  expect((screen.getByRole('listbox', { name: 'Filter retired' }) as HTMLSelectElement).value).toBe('deleted');
  await user.click(screen.getByRole('button', { name: 'Remove filter Extra 18' }));
  expect(screen.queryByRole('listbox', { name: 'Filter Extra 18' })).toBeNull();
  await user.click(screen.getByRole('button', { name: 'Save report' }));
  await waitFor(() =>
    expect(mock.post).toHaveBeenCalledWith(
      '/api/v1/reports',
      expect.objectContaining({
        definition: { ...filtered, dimensions: ['extra_0'], filters: { retired: ['deleted'], model: ['m1'] } },
      }),
    ),
  );
});

it('gives builder controls exact accessible names without option text or hints', async () => {
  const user = userEvent.setup();
  mount();
  await user.click(await screen.findByRole('button', { name: 'Use Usage overview' }));
  await user.click(screen.getByText('Customize report'));
  expect(screen.getByRole('combobox', { name: 'Scope' })).toBeTruthy();
  expect(screen.getByRole('combobox', { name: 'Period' })).toBeTruthy();
  expect(screen.getByRole('listbox', { name: 'Metrics' })).toBeTruthy();
  expect(screen.getByRole('spinbutton', { name: 'Hypothetical discount (%)' })).toBeTruthy();
  expect(screen.getByRole('listbox', { name: 'Filter Model' })).toBeTruthy();
});

it('drills into a result without widening its scope or changing the period', async () => {
  const get = mock.get.getMockImplementation()!;
  const frozen = { ...definition, period: 'custom', start: '2026-01-01T00:00:00Z', end: '2026-02-01T00:00:00Z' };
  mock.get.mockImplementation(async (path) =>
    path === '/api/v1/reports/runs/drill'
      ? ({ run: { id: 'drill', status: 'complete', definition: frozen, result: { definition: frozen } } } as never)
      : get(path),
  );
  const user = userEvent.setup();
  mount('/reports?run=drill');
  await user.click(await screen.findByRole('button', { name: 'Explore model' }));
  await user.click(screen.getByText('Customize report'));
  await screen.findByLabelText('Filter Model');
  mock.post.mockResolvedValue({ report: { id: 'new', owner_user_id: 'me', definition: frozen, revision: 1, shared: false } });
  await user.click(screen.getByRole('button', { name: 'Save report' }));
  await waitFor(() =>
    expect(mock.post).toHaveBeenCalledWith(
      '/api/v1/reports',
      expect.objectContaining({
        shared: false,
        definition: expect.objectContaining({
          scope: 'self',
          period: 'custom',
          start: frozen.start,
          end: frozen.end,
          filters: { model: ['m1'] },
        }),
      }),
    ),
  );
});
it('removes previously selected cost metrics if the instance becomes local-only', async () => {
  const user = userEvent.setup();
  mount();
  await user.click(await screen.findByRole('button', { name: 'Use Usage overview' }));
  await user.click(screen.getByText('Customize report'));
  await user.selectOptions(screen.getByLabelText('Metrics'), 'cost_nanousd');
  localOnly = true;
  await user.click(screen.getByLabelText('Compare with previous period'));
  expect(screen.getByLabelText('Metrics').querySelector('option[value="cost_nanousd"]')).toBeNull();
});
it('shows the saved-report load error in the schedules workspace', async () => {
  const get = mock.get.getMockImplementation()!;
  mock.get.mockImplementation(async (path) => {
    if (path === '/api/v1/reports')
      throw new ApiError(403, {
        message: 'Cannot read saved reports',
        code: 'forbidden',
        type: 'invalid_request',
        reason: 'Scope unavailable',
      });
    return get(path);
  });
  const user = userEvent.setup();
  mount();
  await user.click(screen.getByRole('button', { name: 'Schedules' }));
  await screen.findByText('Scope unavailable');
});

it('loads named filter choices for the selected team and preserves deleted filter values', async () => {
  const user = userEvent.setup();
  const get = mock.get.getMockImplementation()!;
  mock.get.mockImplementation(async (path) =>
    path.endsWith('/catalog')
      ? ({ ...catalog, templates: [{ ...definition, filters: { model: ['deleted'] } }] } as never)
      : get(path),
  );
  mock.post.mockResolvedValue({ report: { id: 'saved', owner_user_id: 'me', definition, revision: 1, shared: false } });
  mount();
  await user.click(await screen.findByRole('button', { name: 'Use Usage overview' }));
  await user.click(screen.getByText('Customize report'));
  await user.selectOptions(screen.getByLabelText('Scope'), 'team');
  await user.selectOptions(screen.getByLabelText('Team'), 't1');
  expect(screen.queryByRole('option', { name: 'Other' })).toBeNull();
  await waitFor(() => expect(mock.get).toHaveBeenCalledWith('/api/v1/reports/options?scope=team&team_id=t1'));
  await user.selectOptions(await screen.findByLabelText('Filter Model'), 'm1');
  await user.click(screen.getByLabelText('Compare with previous period'));
  await user.selectOptions(screen.getByLabelText('Group membership'), 'current');
  await user.click(screen.getByRole('button', { name: 'Save report' }));
  await waitFor(() =>
    expect(mock.post).toHaveBeenCalledWith(
      '/api/v1/reports',
      expect.objectContaining({
        definition: expect.objectContaining({
          scope: 'team',
          team_id: 't1',
          compare: true,
          group_mode: 'current',
          filters: { model: expect.arrayContaining(['deleted', 'm1']) },
        }),
      }),
    ),
  );
});

it('keeps exact nanodollars when editing a large existing budget', async () => {
  const user = userEvent.setup();
  const get = mock.get.getMockImplementation()!;
  const budget = {
    id: 'b1',
    owner_user_id: 'me',
    name: 'Exact target',
    scope: 'user',
    subject_id: 'me',
    amount_nanousd: 9007199254740001,
    start: '2026-09-01T00:00:00Z',
    end: '2026-10-01T00:00:00Z',
  };
  mock.get.mockImplementation(async (path) => (path.endsWith('/budgets') ? ({ budgets: [budget] } as never) : get(path)));
  mock.put.mockResolvedValue({ budget });
  mount();
  await user.click(screen.getByRole('button', { name: 'Budgets' }));
  await user.click(await screen.findByRole('button', { name: 'Edit budget' }));
  await user.click(screen.getByRole('button', { name: 'Update budget' }));
  await waitFor(() =>
    expect(mock.put).toHaveBeenCalledWith(
      '/api/v1/reports/budgets/b1',
      expect.objectContaining({ amount_nanousd: 9007199254740001 }),
    ),
  );
});

it('lets admins choose named teams outside their memberships', async () => {
  role = 'admin';
  const get = mock.get.getMockImplementation()!;
  mock.get.mockImplementation(async (path) =>
    path === '/api/v1/admin/teams' ? ({ teams: [{ id: 'elsewhere', name: 'Research' }] } as never) : get(path),
  );
  const user = userEvent.setup();
  mount();
  await user.click(await screen.findByRole('button', { name: 'Use Usage overview' }));
  await user.click(screen.getByText('Customize report'));
  await user.selectOptions(screen.getByLabelText('Scope'), 'team');
  await user.selectOptions(screen.getByLabelText('Team'), 'elsewhere');
  expect(screen.getByRole('option', { name: 'Research' })).toBeTruthy();
  expect(screen.getByRole('link', { name: 'Model labels' }).getAttribute('href')).toBe('/reports/classifications');
});
it('surfaces a failed poll even with queued data cached', async () => {
  const get = mock.get.getMockImplementation()!;
  mock.get.mockImplementation(async (path) => {
    if (path.endsWith('/runs/r1'))
      throw new ApiError(403, {
        message: 'Access denied',
        code: 'forbidden',
        type: 'invalid_request',
        reason: 'Team access was removed',
      });
    return get(path);
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(['reports', 'run', 'r1'], {
    run: { id: 'r1', status: 'queued', definition, created_at: new Date().toISOString() },
  });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={['/reports?run=r1']}>
        <Reports />
      </MemoryRouter>
    </QueryClientProvider>,
  );
  await screen.findByText(/Team access was removed/);
  expect(screen.getByRole('button', { name: 'Refresh run details' })).toBeTruthy();
});
it('hides budgets, cost metrics and scenarios in local-only mode', async () => {
  localOnly = true;
  const user = userEvent.setup();
  mount();
  expect(screen.queryByRole('button', { name: 'Budgets' })).toBeNull();
  await user.click(await screen.findByRole('button', { name: 'Use Usage overview' }));
  await user.click(screen.getByText('Customize report'));
  expect(screen.queryByRole('option', { name: 'Spend' })).toBeNull();
  expect(screen.queryByLabelText(/Hypothetical discount/)).toBeNull();
  expect(mock.get).not.toHaveBeenCalledWith('/api/v1/reports/budgets');
});
it('shows conflict reason, preserves dirty draft on refetch, and never silently overwrites', async () => {
  const user = userEvent.setup();
  const report = { id: 'saved', owner_user_id: 'me', definition, revision: 2, shared: false };
  const get = mock.get.getMockImplementation()!;
  mock.get.mockImplementation(async (path) => (path === '/api/v1/reports' ? ({ reports: [report] } as never) : get(path)));
  mock.put.mockRejectedValue(
    new ApiError(409, { message: 'Conflict', code: 'conflict', type: 'invalid_request', reason: 'A newer revision exists' }),
  );
  const client = mount();
  await user.click(await screen.findByRole('button', { name: 'Edit Usage overview' }));
  await user.clear(screen.getByLabelText('Report name'));
  await user.type(screen.getByLabelText('Report name'), 'My unsaved draft');
  await client.invalidateQueries({ queryKey: ['reports', 'library'] });
  expect((screen.getByLabelText('Report name') as HTMLInputElement).value).toBe('My unsaved draft');
  await user.click(screen.getByRole('button', { name: 'Update report' }));
  await screen.findByText(/A newer revision exists/);
  expect((screen.getByRole('button', { name: 'Update report' }) as HTMLButtonElement).disabled).toBe(true);
  expect(screen.getByRole('button', { name: 'Reload latest revision' })).toBeTruthy();
});
it('validates reversed dates and unsafe budget amounts without a request', async () => {
  const user = userEvent.setup();
  mount();
  await user.click(await screen.findByRole('button', { name: 'Use Usage overview' }));
  await user.click(screen.getByText('Customize report'));
  await user.selectOptions(screen.getByLabelText('Period'), 'custom');
  await user.click(screen.getByRole('button', { name: 'Generate report' }));
  await screen.findByText('Start must be before end.');
  expect(mock.post).not.toHaveBeenCalled();
  await user.click(screen.getByRole('button', { name: 'Budgets' }));
  await user.type(screen.getByLabelText('Budget name'), 'Target');
  await user.type(screen.getByLabelText('Amount (USD)'), '9007199.254740992');
  await user.click(screen.getByRole('button', { name: 'Create budget' }));
  await screen.findByText(/safely supported budget limit/);
  expect(mock.post).not.toHaveBeenCalled();
});

it('confirms saved report deletion and refreshes the library', async () => {
  const user = userEvent.setup();
  let reports = [{ id: 'saved', owner_user_id: 'me', definition, revision: 1, shared: false }];
  const get = mock.get.getMockImplementation()!;
  mock.get.mockImplementation(async (path) => (path === '/api/v1/reports' ? ({ reports } as never) : get(path)));
  mock.del.mockImplementation(async () => {
    reports = [];
  });
  // Destructive actions now use the shared accessible confirmation dialog.
  mount();
  await screen.findByRole('button', { name: 'Edit Usage overview' });
  await user.click(screen.getByRole('button', { name: 'Delete Usage overview' }));
  expect(mock.del).not.toHaveBeenCalled();
  await user.click(screen.getByRole('button', { name: 'Cancel' }));
  expect(mock.del).not.toHaveBeenCalled();
  await user.click(screen.getByRole('button', { name: 'Delete Usage overview' }));
  await user.click(screen.getByRole('button', { name: 'Confirm' }));
  await waitFor(() => expect(mock.del).toHaveBeenCalledWith('/api/v1/reports/saved'));
  await screen.findByText(/No saved reports yet/);
});

it('creates a non-enforcing personal budget using exact nanodollars and ISO dates', async () => {
  const user = userEvent.setup();
  mock.post.mockResolvedValue({ budget: { id: 'b1' } });
  mount();
  await user.click(screen.getByRole('button', { name: 'Budgets' }));
  await user.type(screen.getByLabelText('Budget name'), 'Monthly target');
  await user.type(screen.getByLabelText('Amount (USD)'), '12.34');
  fireEvent.change(screen.getByLabelText('Budget start'), { target: { value: '2026-09-01T00:00' } });
  fireEvent.change(screen.getByLabelText('Budget end'), { target: { value: '2026-10-01T00:00' } });
  await user.click(screen.getByRole('button', { name: 'Create budget' }));
  await waitFor(() =>
    expect(mock.post).toHaveBeenCalledWith('/api/v1/reports/budgets', {
      name: 'Monthly target',
      scope: 'user',
      subject_id: 'me',
      amount_nanousd: 12340000000,
      start: new Date('2026-09-01T00:00').toISOString(),
      end: new Date('2026-10-01T00:00').toISOString(),
    }),
  );
  expect(screen.getByText(/Non-enforcing/)).toBeTruthy();
  expect(screen.queryByRole('option', { name: 'Organization' })).toBeNull();
});

it('creates an owned report schedule and pauses it using explicit schedule contract', async () => {
  const user = userEvent.setup();
  let schedule = {
    id: 's1',
    report_id: 'saved',
    frequency: 'weekly',
    timezone: 'UTC',
    at: '09:00',
    weekday: 1,
    monthday: 1,
    enabled: true,
    next_run_at: '2026-09-14T09:00:00Z',
  };
  const get = mock.get.getMockImplementation()!;
  mock.get.mockImplementation(async (path) =>
    path.endsWith('/schedules')
      ? ({ schedules: [schedule] } as never)
      : path === '/api/v1/reports'
        ? ({ reports: [{ id: 'saved', owner_user_id: 'me', definition, revision: 1, shared: false }] } as never)
        : get(path),
  );
  mock.post.mockResolvedValue({ schedule });
  mock.put.mockImplementation(async (_path, body) => {
    schedule = { ...schedule, ...(body as object) };
    return { schedule };
  });
  mount();
  await user.click(screen.getByRole('button', { name: 'Schedules' }));
  await user.selectOptions(await screen.findByLabelText('Saved report'), 'saved');
  await user.selectOptions(screen.getByLabelText('Frequency'), 'weekly');
  await user.click(screen.getByRole('button', { name: 'Create schedule' }));
  await waitFor(() =>
    expect(mock.post).toHaveBeenCalledWith('/api/v1/reports/schedules', {
      report_id: 'saved',
      frequency: 'weekly',
      timezone: 'UTC',
      at: '09:00',
      weekday: 1,
      monthday: 1,
      enabled: true,
    }),
  );
  await user.click(screen.getByRole('button', { name: 'Pause schedule' }));
  await waitFor(() =>
    expect(mock.put).toHaveBeenCalledWith('/api/v1/reports/schedules/s1', {
      report_id: 'saved',
      frequency: 'weekly',
      timezone: 'UTC',
      at: '09:00',
      weekday: 1,
      monthday: 1,
      enabled: false,
    }),
  );
  await screen.findByRole('button', { name: 'Resume schedule' });
});

it('runs an unsaved definition, polls queued to complete, and closes durable run', async () => {
  const user = userEvent.setup();
  let polls = 0;
  const get = mock.get.getMockImplementation()!;
  mock.get.mockImplementation(async (path) =>
    path.endsWith('/runs/r1')
      ? ({
          run: {
            id: 'r1',
            definition,
            status: ++polls > 1 ? 'complete' : 'queued',
            created_at: new Date().toISOString(),
            result: polls > 1 ? {} : undefined,
          },
        } as never)
      : get(path),
  );
  mock.post.mockResolvedValue({ run: { id: 'r1', definition, status: 'queued', created_at: new Date().toISOString() } });
  mount();
  await user.click(await screen.findByRole('button', { name: 'Use Usage overview' }));
  await user.click(screen.getByText('Customize report'));
  await user.click(screen.getByRole('button', { name: 'Generate report' }));
  await screen.findByText('Queued');
  expect(screen.queryByRole('progressbar')).toBeNull();
  await screen.findByRole('button', { name: 'Cancel run' });
  await waitFor(() => expect(screen.getByText('Completed result')).toBeTruthy(), { timeout: 4500 });
  expect(mock.post).toHaveBeenCalledWith('/api/v1/reports/runs', { definition });
  await user.click(screen.getByRole('button', { name: 'Back to reports' }));
  expect(screen.queryByText('Completed result')).toBeNull();
});

it('saves ISO custom dates and explicit sharing, then updates using returned revision', async () => {
  const user = userEvent.setup();
  mock.post.mockResolvedValue({ report: { id: 'saved', owner_user_id: 'me', definition, revision: 7, shared: false } });
  mock.put.mockResolvedValue({ report: { id: 'saved', owner_user_id: 'me', definition, revision: 8, shared: false } });
  mount();
  await user.click(await screen.findByRole('button', { name: 'Use Usage overview' }));
  await user.click(screen.getByText('Customize report'));
  await user.selectOptions(screen.getByLabelText('Period'), 'custom');
  fireEvent.change(screen.getByLabelText('Start'), { target: { value: '2026-09-01T00:00' } });
  fireEvent.change(screen.getByLabelText('End'), { target: { value: '2026-09-02T00:00' } });
  await user.click(screen.getByRole('button', { name: 'Save report' }));
  await waitFor(() =>
    expect(mock.post).toHaveBeenCalledWith('/api/v1/reports', {
      shared: false,
      definition: expect.objectContaining({
        start: new Date('2026-09-01T00:00').toISOString(),
        end: new Date('2026-09-02T00:00').toISOString(),
        period: 'custom',
      }),
    }),
  );
  await user.click(await screen.findByRole('button', { name: 'Update report' }));
  await waitFor(() =>
    expect(mock.put).toHaveBeenCalledWith('/api/v1/reports/saved', expect.objectContaining({ revision: 7, shared: false })),
  );
});

it('starts with built-in and saved library, opens template without widening scope', async () => {
  const user = userEvent.setup();
  mount();
  await user.click(await screen.findByRole('button', { name: 'Use Usage overview' }));
  await user.click(screen.getByText('Customize report'));
  expect((screen.getByLabelText('Report name') as HTMLInputElement).value).toBe('Usage overview');
  expect(screen.queryByRole('option', { name: 'Organization' })).toBeNull();
  expect((screen.getByLabelText('Scope') as HTMLSelectElement).value).toBe('self');
});
