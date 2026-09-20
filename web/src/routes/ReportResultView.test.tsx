import { cleanup, fireEvent, render as rtlRender, screen, within } from '@testing-library/react';
import { MemoryRouter, useLocation, useNavigate } from 'react-router-dom';
import { afterEach, expect, it, vi } from 'vitest';
import type { ReportResult } from '../lib/reports';
import { readFileSync } from 'node:fs';
import { ReportResultView } from './ReportResultView';

const resultCSS = readFileSync('src/routes/report-result.css', 'utf8');

function HistoryControls() {
  const navigate = useNavigate();
  return (
    <>
      <button type="button" onClick={() => navigate('/reports?run=other')}>
        Other route
      </button>
      <button type="button" onClick={() => navigate(-1)}>
        Back
      </button>
    </>
  );
}

function LocationProbe() {
  return <output data-testid="location">{useLocation().search}</output>;
}
function baseRender(ui: Parameters<typeof rtlRender>[0], entry = '/reports?run=r&tab=runs') {
  return rtlRender(ui, {
    wrapper: ({ children }) => (
      <MemoryRouter initialEntries={[entry]}>
        {children}
        <LocationProbe />
      </MemoryRouter>
    ),
  });
}

function render(ui: Parameters<typeof baseRender>[0]) {
  const view = baseRender(ui);
  view.container.querySelectorAll('details').forEach((detail) => {
    detail.open = true;
  });
  return view;
}
const session = vi.hoisted(() => ({ localOnly: false }));
vi.mock('../app/session', () => ({ useLocalOnly: () => session.localOnly }));
afterEach(() => {
  cleanup();
  session.localOnly = false;
});
it('does not present an incomplete comparison as growth', () => {
  const result = fixture();
  result.definition.compare = true;
  result.comparison_reliable = false;
  result.comparison_warnings = ['Previous period was partly removed by retention.'];
  render(<ReportResultView result={result} runID="coverage" />);
  expect(screen.getAllByText('Comparison unavailable: incomplete coverage').length).toBeGreaterThan(0);
  expect(screen.getByText('Previous period was partly removed by retention.')).toBeTruthy();
});
it('offers working drill-down on supported section rows', () => {
  const result = fixture();
  result.sections = [
    {
      id: 'portfolio_model',
      title: 'Model portfolio',
      columns: [
        { key: 'model', label: 'Model', unit: '' },
        { key: 'requests', label: 'Requests', unit: 'count' },
      ],
      rows: [{ dimensions: { model: 'model-section' }, values: { requests: 1 } }],
      notes: [],
    },
  ];
  const drill = vi.fn();
  render(<ReportResultView result={result} runID="section" onDrillDown={drill} drillableDimensions={['model']} />);
  fireEvent.click(screen.getByRole('button', { name: 'Explore model-section' }));
  expect(drill).toHaveBeenCalledWith('model', 'model-section');
});
it('suppresses stale monetary columns, sections and prose in local-only mode', () => {
  session.localOnly = true;
  const result = fixture();
  result.definition.name = 'Cost summary $99';
  result.definition.template = 'budget';
  result.columns.push({ key: 'cost_nanousd', label: 'Amount', unit: 'nanousd' });
  result.totals.cost_nanousd = 99000000000;
  result.warnings.push('Budget remaining $99');
  result.sections = [
    { id: 'scenario', title: 'Potential savings', columns: result.columns, rows: result.rows, notes: ['You could save $10'] },
    {
      id: 'coverage',
      title: 'Coverage',
      columns: result.columns,
      rows: result.rows,
      notes: ['Estimated amount: 42 EUR', 'Source complete.'],
    },
  ];
  render(<ReportResultView result={result} runID="r" />);
  expect(document.body.textContent).not.toMatch(/\$99|\$10|42 EUR|Amount|Potential savings|budget|Cost summary/i);
  expect(screen.getByText('Source complete.')).toBeTruthy();
  expect(screen.getByText('Groups overlap; totals are deduplicated.')).toBeTruthy();
  expect(screen.getByRole('heading', { name: 'Requests' })).toBeTruthy();
});
it('preserves null ratios and supports drill-down only when wired', () => {
  const result = fixture();
  result.columns.push({ key: 'ratio', label: 'Success ratio', unit: 'ratio' });
  result.totals.ratio = null;
  result.rows[0]!.values.ratio = null;
  const drill = vi.fn();
  const { rerender } = render(<ReportResultView result={result} runID="r" />);
  expect(screen.queryByRole('button', { name: 'Explore alpha' })).toBeNull();
  const card = screen.getByRole('heading', { name: 'Success ratio' }).parentElement!;
  expect(within(card).getByText('Unknown')).toBeTruthy();
  rerender(<ReportResultView result={result} runID="r" onDrillDown={drill} />);
  fireEvent.click(screen.getByRole('button', { name: 'Explore alpha' }));
  expect(drill).toHaveBeenCalledWith('model', 'alpha');
});
it('keeps small nonzero ratios visible and totals independent of overlapping rows', () => {
  const result = fixture();
  result.columns.push({ key: 'ratio', label: 'Success ratio', unit: 'ratio' });
  result.totals.ratio = 0.00001234;
  result.rows.push({ dimensions: { model: 'overlap' }, values: { requests: 3 } });
  render(<ReportResultView result={result} runID="r" />);
  expect(screen.getByText('0.001234%')).toBeTruthy();
  const requests = screen.getByRole('heading', { name: 'Requests' }).parentElement!;
  expect(within(requests).getByText('3')).toBeTruthy();
});
it('leads with scope and KPIs, keeping provenance in complete data', () => {
  const { container } = baseRender(<ReportResultView result={fixture()} runID="reader" />);
  expect(screen.getByText(/Self scope/)).toBeTruthy();
  const detail = screen.getByText(/Complete data and provenance/).closest('details')!;
  expect(detail.open).toBe(false);
  expect(detail.contains(screen.getByText('2026-09-10T13:00:00Z'))).toBe(true);
  expect(
    container.querySelector('.report-result-metrics')!.compareDocumentPosition(detail) & Node.DOCUMENT_POSITION_FOLLOWING,
  ).toBeTruthy();
});
it('formats money semantically without losing exact values or nonmoney units', () => {
  const result = fixture();
  result.columns.push(
    { key: 'cost', label: 'Recorded cost', unit: 'USD' },
    { key: 'tiny', label: 'Tiny cost', unit: 'USD' },
    { key: 'latency', label: 'Latency', unit: 'ms' },
    { key: 'derived', label: 'Efficiency', unit: 'tokens/request' },
  );
  Object.assign(result.totals, { cost: 17.612345, tiny: 0.000002, latency: 12.5, derived: 3 });
  render(<ReportResultView result={result} runID="money" />);
  expect(screen.getByText('$17.61').getAttribute('title')).toContain('17.612345');
  expect(screen.getByText('<$0.01')).toBeTruthy();
  expect(screen.getByText('12.5 ms')).toBeTruthy();
  expect(screen.getByText('3 tokens/request')).toBeTruthy();
});
it('shows a ranked full-data chart before details, independent of paging with working table toggle', () => {
  const result = fixture();
  result.rows = Array.from({ length: 27 }, (_, i) => ({
    dimensions: { model: `id-${i}`, model_label: `Named ${i}` },
    values: { requests: i },
  }));
  const { container } = baseRender(<ReportResultView result={result} runID="rank" />);
  const chart = screen.getByRole('img', { name: /Requests by Model/ });
  expect(chart.textContent).toContain('Named 26');
  expect(chart.textContent).not.toContain('id-26');
  expect(screen.getByText(/Top 5 of 27/)).toBeTruthy();
  expect(chart.closest('details')).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: 'Table' }));
  expect(screen.getByRole('table', { name: 'Report data summary' })).toBeTruthy();
  fireEvent.click(screen.getByRole('button', { name: 'Chart' }));
  const details = container.querySelector('details.report-result-complete')! as HTMLDetailsElement;
  details.open = true;
  fireEvent.click(screen.getByRole('button', { name: 'Next Report data page' }));
  expect(screen.getByRole('img', { name: /Requests by Model/ }).textContent).toContain('Named 26');
});
it.each(['day', 'week', 'month'])('uses a complete date axis for %s, leaving missing periods unfilled', (dimension) => {
  const result = fixture();
  result.definition.compare = false;
  result.columns = [
    { key: dimension, label: dimension, unit: '' },
    { key: 'requests', label: 'Requests', unit: 'count' },
    { key: 'cost', label: 'Cost', unit: 'USD' },
  ];
  result.start = '2026-01-01T00:00:00Z';
  result.end = '2026-04-01T00:00:00Z';
  result.rows = [
    { dimensions: { [dimension]: '2026-01-01' }, values: { requests: 0, cost: 0.001 } },
    { dimensions: { [dimension]: '2026-03-01' }, values: { requests: 10, cost: null } },
  ];
  render(<ReportResultView result={result} runID="time" />);
  const chart = screen.getByRole('img', { name: /Requests by .*entire period/ });
  expect(chart.getAttribute('data-start')).toBe(result.start);
  expect(chart.getAttribute('data-end')).toBe(result.end);
  const points = chart.querySelectorAll('[data-observation]');
  expect(points.length).toBe(2);
  expect(parseFloat(points[1]!.getAttribute('cx')!)).toBeGreaterThan(50);
  expect(screen.getByText(/Gaps mean no returned row, not zero/)).toBeTruthy();
  fireEvent.change(screen.getByLabelText('Chart metric'), { target: { value: 'cost' } });
  expect(screen.getByRole('img', { name: /Cost by/ }).textContent).toContain('0.001 USD');
});
it('surfaces model and user mix with compact warnings while retaining every detail', () => {
  const result = fixture();
  result.sections = ['model', 'user'].map((kind) => ({
    id: `executive_${kind}s`,
    title: `${kind} mix`,
    columns: [
      { key: kind, label: kind, unit: '' },
      { key: 'requests', label: 'Requests', unit: 'count' },
    ],
    rows: [{ dimensions: { [kind]: 'opaque', [`${kind}_label`]: 'Friendly' }, values: { requests: 2 } }],
    notes: [],
  }));
  baseRender(<ReportResultView result={result} runID="mix" />);
  expect(screen.getAllByRole('img').length).toBe(3);
  expect(screen.getByText(/2 coverage notices/).closest('details')!.open).toBe(false);
  expect(screen.getByText('Partial source coverage.')).toBeTruthy();
});
it('includes supplied executive KPIs and exact table details without erasing recorded money for unknown confidence', () => {
  const result = fixture();
  result.definition.compare = false;
  result.columns.push({ key: 'cost_usd', label: 'Recorded cost', unit: 'USD' });
  result.totals.cost_usd = 17.612345;
  result.rows[0]!.values.cost_usd = 17.612345;
  result.sections = [
    {
      id: 'executive',
      title: 'Executive',
      columns: [{ key: 'success_rate', label: 'Success rate', unit: 'ratio' }],
      rows: [{ dimensions: {}, values: { success_rate: 0.95 } }],
      notes: [],
    },
    {
      id: 'cost_coverage',
      title: 'Cost coverage',
      columns: [{ key: 'unknown_cost_requests', label: 'Unknown pricing', unit: 'count' }],
      rows: [{ dimensions: {}, values: { unknown_cost_requests: 3 } }],
      notes: [],
    },
  ];
  render(<ReportResultView result={result} runID="confidence" />);
  expect(within(screen.getByRole('region', { name: 'Overview' })).getByText('95%')).toBeTruthy();
  expect(screen.getByText(/Recorded amounts do not establish complete pricing coverage/)).toBeTruthy();
  expect(within(screen.getByRole('table', { name: 'Report data' })).getByText('17.612345 USD')).toBeTruthy();
  expect(screen.getByText('$17.61', { selector: 'strong' })).toBeTruthy();
});
it('keeps executive KPIs to four primary measures with a readable success rate and exact details', () => {
  const result = fixture();
  result.columns.push(
    { key: 'cost_usd', label: 'Recorded cost', unit: 'USD' },
    { key: 'active_users', label: 'Active users', unit: 'count' },
    { key: 'errors', label: 'Errors', unit: 'count' },
  );
  Object.assign(result.totals, { cost_usd: 17.61, active_users: 1, errors: 163 });
  result.sections = [
    {
      id: 'executive',
      title: 'Executive',
      columns: [{ key: 'success_rate', label: 'Success rate', unit: 'ratio' }],
      rows: [{ dimensions: {}, values: { success_rate: 0.901866345575 } }],
      notes: [],
    },
  ];
  render(<ReportResultView result={result} runID="compact" />);
  const overview = screen.getByRole('region', { name: 'Overview' });
  expect(
    within(overview)
      .getAllByRole('heading')
      .map((h) => h.textContent),
  ).toEqual(['Requests', 'Recorded cost', 'Success rate', 'Active users']);
  expect(within(overview).getByText('90.2%').getAttribute('title')).toBe('0.901866345575 ratio');
  expect(within(screen.getByRole('table', { name: 'Executive' })).getByText('0.901866345575 ratio')).toBeTruthy();
  expect(screen.getByText('163', { selector: '.report-result-complete *' })).toBeTruthy();
});
it('keeps chart text unscaled and controls touch-sized in the scoped responsive layout', () => {
  expect(resultCSS).toMatch(/min-height:\s*44px/);
  expect(resultCSS).toContain('.report-result-visuals');
  expect(resultCSS).toContain('.report-result-bars');
  expect(resultCSS).toContain('.report-result-trend');
  expect(resultCSS).toContain('var(--janus-chart-1)');
});
it.each([
  { values: [0, 10], baseline: 185, top: '10', bottom: '0', left: '0%', width: '100%' },
  { values: [-10, 0], baseline: 15, top: '0', bottom: '-10', left: '0%', width: '100%' },
  { values: [-10, 30], baseline: 142.5, top: '30', bottom: '-10', left: '25%', width: '75%' },
  { values: [0, 0], baseline: 185, top: '1', bottom: '0', left: '0%', width: '0%' },
])('fits chart axes to actual signs: $values', ({ values, baseline, top, bottom, left, width }) => {
  const result = fixture();
  result.columns[0] = { key: 'day', label: 'Day', unit: '' };
  result.rows = values.map((requests, i) => ({ dimensions: { day: `2026-09-0${i + 1}` }, values: { requests } }));
  const { rerender, container } = render(<ReportResultView result={result} runID="signed" />);
  const chart = screen.getByRole('img', { name: /entire period/ });
  expect(chart.querySelector('line')!.getAttribute('y1')).toBe(String(baseline));
  const axis = container.querySelector('.report-result-axis')!;
  expect(axis.firstElementChild!.textContent).toBe(top);
  expect(axis.lastElementChild!.textContent).toBe(bottom);
  result.columns[0] = { key: 'model', label: 'Model', unit: '' };
  result.rows = values.map((requests, i) => ({ dimensions: { model: `model-${i}` }, values: { requests } }));
  rerender(<ReportResultView result={result} runID="signed-bars" />);
  const bars = Array.from(container.querySelectorAll<HTMLElement>('.report-result-track > span'));
  const bar = bars.find((b) => b.style.width !== '0%') ?? bars[0]!;
  expect(bar.style.marginLeft).toBe(left);
  expect(bar.style.width).toBe(width);
});
it('gives the primary chart two thirds, ranks models beside it and users below with compact responsive KPIs', () => {
  const result = fixture();
  result.sections = ['user', 'model'].map((kind) => ({
    id: `executive_${kind}s`,
    title: `${kind} mix`,
    columns: [
      { key: kind, label: kind, unit: '' },
      { key: 'requests', label: 'Requests', unit: 'count' },
    ],
    rows: [{ dimensions: { [kind]: 'Friendly' }, values: { requests: 2 } }],
    notes: [],
  }));
  const { container } = baseRender(<ReportResultView result={result} runID="hierarchy" />);
  const panels = Array.from(container.querySelectorAll('.report-result-visuals > section'));
  expect(panels.map((p) => p.querySelector('h3')!.textContent)).toEqual(['Report data', 'model mix', 'user mix']);
  expect(panels[0]!.classList.contains('report-result-primary')).toBe(true);
  expect(panels[2]!.classList.contains('report-result-users')).toBe(true);
  expect(resultCSS).toMatch(/\.report-result-primary\s*\{[^}]*grid-column:\s*span 2/s);
  expect(resultCSS).toMatch(/\.report-result-users\s*\{[^}]*grid-column:\s*1\s*\/\s*-1/s);
  expect(resultCSS).toMatch(/\.report-result-metrics\s*\{[^}]*grid-template-columns:\s*repeat\(4,/s);
  expect(resultCSS).toMatch(/@media\s*\(max-width:\s*600px\)[\s\S]*\.report-result-metrics\s*\{[^}]*repeat\(2,/);
  expect(resultCSS).toMatch(/\.report-result-card > strong\s*\{[^}]*white-space:\s*nowrap/s);
});
it('only discloses unknown or signed measurement caveats when present', () => {
  const result = fixture();
  const { container, rerender } = baseRender(<ReportResultView result={result} runID="notes" />);
  const panel = () => container.querySelector('.report-result-primary')!;
  expect(panel().textContent).not.toMatch(/Unknown measurements|Negative values/);
  result.rows.push({ dimensions: { model: 'Unknown measurement' }, values: { requests: null } });
  rerender(<ReportResultView result={result} runID="notes" />);
  let notes = within(panel() as HTMLElement)
    .getByText('Chart notes')
    .closest('details')!;
  expect(notes.open).toBe(false);
  expect(notes.textContent).toContain('Unknown measurements are unplotted');
  expect(notes.textContent).not.toContain('Negative values');
  result.rows.push({ dimensions: { model: 'Correction' }, values: { requests: -1 } });
  rerender(<ReportResultView result={result} runID="notes" />);
  notes = within(panel() as HTMLElement)
    .getByText('Chart notes')
    .closest('details')!;
  expect(notes.textContent).toContain('Negative values extend left of zero');
  expect(panel().querySelectorAll('.report-result-track > span').length).toBe(2);
});
it.each([
  [
    'day',
    'America/New_York',
    '2026-09-01T02:00:00Z',
    '2026-09-02T02:00:00Z',
    '2026-08-31',
    '2026-09-01',
    '2026-08-30',
    '2026-09-02',
  ],
  [
    'week',
    'America/New_York',
    '2026-09-07T02:00:00Z',
    '2026-09-14T02:00:00Z',
    '2026-08-31',
    '2026-09-07',
    '2026-08-24',
    '2026-09-14',
  ],
  [
    'month',
    'America/New_York',
    '2026-09-01T02:00:00Z',
    '2026-10-01T02:00:00Z',
    '2026-08-01',
    '2026-09-01',
    '2026-07-01',
    '2026-10-01',
  ],
  ['day', 'Asia/Tokyo', '2026-08-31T16:00:00Z', '2026-09-01T16:00:00Z', '2026-09-01', '2026-09-02', '2026-08-31', '2026-09-03'],
  [
    'day',
    'America/New_York',
    '2026-03-08T05:00:00Z',
    '2026-03-10T04:00:00Z',
    '2026-03-08',
    '2026-03-09',
    '2026-03-07',
    '2026-03-10',
  ],
])('clips %s buckets in %s calendar time for %s', (dimension, timezone, start, end, first, last, before, after) => {
  const result = fixture();
  result.definition.timezone = timezone;
  result.definition.dimensions = [dimension];
  result.start = start;
  result.end = end;
  result.columns = [
    { key: dimension, label: dimension, unit: '' },
    { key: 'requests', label: 'Requests', unit: 'count' },
  ];
  result.rows = [before, first, last, after].map((date) => ({
    dimensions: { [dimension]: date },
    values: { requests: 3 },
  }));
  const { container } = render(<ReportResultView result={result} runID="local-boundaries" />);
  const chart = screen.getByRole('img', { name: /entire period/ });
  expect(Array.from(chart.querySelectorAll('title'), (node) => node.textContent)).toEqual([
    `${first}: 3 count`,
    `${last}: 3 count`,
  ]);
  expect(chart.querySelectorAll('[data-observation]')).toHaveLength(2);
  expect(screen.getByText(/2 rows cannot be placed on the date axis/)).toBeTruthy();
  const labels = container.querySelector('.report-result-date-axis')!;
  expect(labels.firstElementChild!.textContent).toBe(
    new Date(first).toLocaleDateString('en-US', {
      month: 'short',
      day: 'numeric',
      year: 'numeric',
      timeZone: 'UTC',
    }),
  );
  expect(labels.lastElementChild!.textContent).toBe(
    new Date(end).toLocaleDateString('en-US', {
      month: 'short',
      day: 'numeric',
      year: 'numeric',
      timeZone: timezone,
    }),
  );
});
it('keeps a partial first week on the time axis and warns about unplottable dates', () => {
  const result = fixture();
  result.start = '2026-09-03T12:00:00Z';
  result.columns = [
    { key: 'week', label: 'Week', unit: '' },
    { key: 'requests', label: 'Requests', unit: 'count' },
  ];
  result.rows = [
    { dimensions: { week: '2026-08-31' }, values: { requests: 3 } },
    { dimensions: { week: 'not-a-date' }, values: { requests: 7 } },
  ];
  render(<ReportResultView result={result} runID="boundary" />);
  expect(screen.getByRole('img', { name: /entire period/ }).querySelectorAll('[data-observation]').length).toBe(1);
  expect(screen.getByText(/1 row cannot be placed on the date axis/)).toBeTruthy();
});
it.each([
  ['model', 'day'],
  ['model', 'group'],
  ['model', 'day', 'group'],
])('preserves every grouping key and unsummed metric in the primary table: %j', (...dimensions) => {
  const result = fixture();
  result.definition.dimensions = dimensions;
  result.columns = [
    ...dimensions.map((key) => ({ key, label: key, unit: '' })),
    { key: 'requests', label: 'Requests', unit: 'count' },
    { key: 'ratio', label: 'Success ratio', unit: 'ratio' },
  ];
  result.rows = ['alpha', 'beta', 'gamma'].map((model, i) => ({
    dimensions: Object.fromEntries(
      dimensions.map((key) => [key, key === 'model' ? model : key === 'day' ? '2026-09-03' : 'overlapping-group']),
    ),
    values: { requests: 3, ratio: i === 2 ? null : 0.5 },
  }));
  const { container } = baseRender(<ReportResultView result={result} runID="mixed" />);
  const primary = container.querySelector('.report-result-primary')! as HTMLElement;
  expect(within(primary).queryByRole('img')).toBeNull();
  expect(primary.closest('details')).toBeNull();
  expect(within(primary).getByText(/Multiple grouping dimensions/)).toBeTruthy();
  const table = within(primary).getByRole('table', { name: 'Report data summary' });
  expect(within(table).getAllByRole('columnheader')).toHaveLength(dimensions.length + 2);
  const rows = within(table).getAllByRole('row').slice(1);
  expect(rows).toHaveLength(3);
  rows.forEach((row, i) => {
    const cells = within(row).getAllByRole('cell');
    expect(cells.slice(0, dimensions.length).map((cell) => cell.textContent)).toEqual(
      dimensions.map((key) => result.rows[i]!.dimensions[key]),
    );
    expect(cells[dimensions.length]!.textContent).toBe('3');
    expect(cells[dimensions.length + 1]!.textContent).toContain(i === 2 ? 'Unknown' : '50%');
  });
  expect(within(screen.getByRole('region', { name: 'Overview' })).getByText('3')).toBeTruthy();
});
it('prefers the actual time dimension when unused categorical columns precede it', () => {
  const result = fixture();
  result.columns.unshift({ key: 'day', label: 'Day', unit: '' });
  result.columns.reverse();
  result.rows = [{ dimensions: { day: '2026-09-03', day_label: 'Sep 3' }, values: { requests: 3 } }];
  render(<ReportResultView result={result} runID="single-time" />);
  expect(screen.getByRole('img', { name: /Requests by Day — entire period/ })).toBeTruthy();
});
it('uses whole count ticks without fractional requests', () => {
  const result = fixture();
  result.columns[0] = { key: 'day', label: 'Day', unit: '' };
  result.rows = [{ dimensions: { day: '2026-09-02' }, values: { requests: 439 } }];
  const view = render(<ReportResultView result={result} runID="count-axis" />);
  const ticks = Array.from(view.container.querySelectorAll('.report-result-axis span'), (e) => Number(e.textContent));
  expect(ticks).toHaveLength(3);
  expect(ticks.every(Number.isInteger)).toBe(true);
  expect(ticks[0]).toBeGreaterThanOrEqual(439);
});

it('searches all frozen rows before paging and resets only its own offset', () => {
  const result = fixture();
  result.rows = Array.from({ length: 61 }, (_, i) => ({ dimensions: { model: `model-${i}` }, values: { requests: i } }));
  const drill = vi.fn();
  render(<ReportResultView result={result} runID="r" onDrillDown={drill} />);
  fireEvent.click(screen.getByRole('button', { name: 'Next Report data page' }));
  fireEvent.change(screen.getByRole('searchbox', { name: 'Search Report data' }), { target: { value: ' MODEL-60 ' } });
  const table = screen.getByRole('table', { name: 'Report data' });
  expect(within(table).getAllByRole('row')).toHaveLength(2);
  expect(within(table).getByText('model-60')).toBeTruthy();
  expect(screen.getByText('1 of 61 snapshot rows')).toBeTruthy();
  const params = new URLSearchParams(screen.getByTestId('location').textContent!);
  expect(params.get('report.r.data.q')).toBe(' MODEL-60 ');
  expect(params.has('report.r.data.page')).toBe(false);
  expect(params.get('run')).toBe('r');
  expect(params.get('tab')).toBe('runs');
  fireEvent.click(within(table).getByRole('button', { name: 'Explore model-60' }));
  expect(drill).toHaveBeenCalledWith('model', 'model-60');
  expect(screen.getByRole('link', { name: 'CSV' }).getAttribute('href')).toBe('/api/v1/reports/runs/r/download?format=csv');
  expect(within(screen.getByRole('region', { name: 'Overview' })).getByText('3')).toBeTruthy();
});

it('restores page size and sort from the URL, resets size changes and repairs stale offsets after shrink', () => {
  const result = fixture();
  result.rows = Array.from({ length: 61 }, (_, i) => ({ dimensions: { model: `model-${i}` }, values: { requests: i } }));
  const view = baseRender(
    <ReportResultView result={result} runID="r" />,
    '/reports?run=r&tab=runs&report.r.data.size=10&report.r.data.page=50&report.r.data.sort=requests:desc',
  );
  view.container.querySelectorAll('details').forEach((d) => {
    d.open = true;
  });
  const table = screen.getByRole('table', { name: 'Report data' });
  expect(within(table).getAllByRole('row')).toHaveLength(11);
  expect(within(table).getAllByRole('cell')[0]!.textContent).toBe('model-10');
  fireEvent.change(screen.getByRole('combobox', { name: 'Report data rows per page' }), { target: { value: '25' } });
  expect(within(table).getAllByRole('row')).toHaveLength(26);
  expect(within(table).getAllByRole('cell')[0]!.textContent).toBe('model-60');
  fireEvent.click(screen.getByRole('button', { name: 'Next Report data page' }));
  fireEvent.click(screen.getByRole('button', { name: 'Next Report data page' }));
  view.rerender(<ReportResultView result={{ ...result, rows: result.rows.slice(0, 30) }} runID="r" />);
  expect(screen.getByText('Showing 5 of 30 · Rows 26–30')).toBeTruthy();
  expect(new URLSearchParams(screen.getByTestId('location').textContent!).get('report.r.data.page')).toBe('25');
});

it('keeps blank dimensions last in both sort directions and preserves ties and source rows', () => {
  const result = fixture();
  result.rows = [
    { dimensions: { model: '' }, values: { requests: 1 } },
    { dimensions: { model: 'zebra' }, values: { requests: 2 } },
    { dimensions: { model: 'alpha' }, values: { requests: 3 } },
    { dimensions: { model: 'zebra' }, values: { requests: 4 } },
    { dimensions: {}, values: { requests: 5 } },
  ];
  const original = JSON.stringify(result);
  render(<ReportResultView result={result} runID="r" />);
  const table = screen.getByRole('table', { name: 'Report data' });
  const order = () =>
    within(table)
      .getAllByRole('row')
      .slice(1)
      .map((row) => within(row).getAllByRole('cell')[1]!.textContent);
  fireEvent.click(within(table).getByRole('button', { name: 'Sort by Model' }));
  expect(order()).toEqual(['3', '2', '4', '1', '5']);
  fireEvent.click(within(table).getByRole('button', { name: 'Sort by Model' }));
  expect(order()).toEqual(['2', '4', '3', '1', '5']);
  expect(JSON.stringify(result)).toBe(original);
});

it('distinguishes no matches from empty snapshots and clears search and sort without submitting', () => {
  const submit = vi.fn((e: React.FormEvent) => e.preventDefault());
  render(
    <form onSubmit={submit}>
      <ReportResultView result={fixture()} runID="r" />
    </form>,
  );
  fireEvent.change(screen.getByRole('searchbox', { name: 'Search Report data' }), { target: { value: 'absent' } });
  expect(screen.getByText('No matching snapshot rows. Clear the search to see all frozen rows.')).toBeTruthy();
  expect(screen.queryByText('No rows matched this frozen report.')).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: 'Clear Report data filters and sort' }));
  expect(within(screen.getByRole('table', { name: 'Report data' })).getByText('alpha')).toBeTruthy();
  expect(submit).not.toHaveBeenCalled();
});

it('isolates run, section and multidimension-summary state and restores links after back and remount', () => {
  const result = fixture();
  result.columns.push({ key: 'team', label: 'Team', unit: '' });
  result.rows = Array.from({ length: 61 }, (_, i) => ({
    dimensions: { model: `model-${i}`, team: 'team' },
    values: { requests: i },
  }));
  result.sections = ['a.b', 'a%2Eb'].map((id) => ({ id, title: id, columns: result.columns, rows: result.rows, notes: [] }));
  const view = render(
    <>
      <ReportResultView result={result} runID="r" />
      <HistoryControls />
    </>,
  );
  fireEvent.change(screen.getByRole('searchbox', { name: 'Search a.b' }), { target: { value: 'model-60' } });
  fireEvent.change(screen.getByRole('combobox', { name: 'a%2Eb rows per page' }), { target: { value: '10' } });
  fireEvent.click(screen.getByRole('button', { name: 'Next a%2Eb page' }));
  fireEvent.change(screen.getByRole('searchbox', { name: 'Search Report data summary' }), { target: { value: 'model-59' } });
  fireEvent.click(screen.getByRole('button', { name: 'Next Report data page' }));
  const saved = screen.getByTestId('location').textContent!;
  const params = new URLSearchParams(saved);
  expect(params.get('report.r.section.a%2Eb.q')).toBe('model-60');
  expect(params.get('report.r.section.a%252Eb.page')).toBe('10');
  expect(params.get('report.r.data.summary.q')).toBe('model-59');
  expect(params.get('report.r.data.page')).toBe('25');
  expect(within(screen.getByRole('table', { name: 'Report data' })).getAllByRole('row')).toHaveLength(26);
  fireEvent.click(screen.getByRole('button', { name: 'Other route' }));
  expect((screen.getByRole('searchbox', { name: 'Search a.b' }) as HTMLInputElement).value).toBe('');
  fireEvent.click(screen.getByRole('button', { name: 'Back' }));
  expect(screen.getByTestId('location').textContent).toBe(saved);
  view.rerender(<ReportResultView result={result} runID="other" />);
  expect((screen.getByRole('searchbox', { name: 'Search a.b' }) as HTMLInputElement).value).toBe('');
  view.rerender(<ReportResultView result={result} runID="r" />);
  expect((screen.getByRole('searchbox', { name: 'Search a.b' }) as HTMLInputElement).value).toBe('model-60');
  view.unmount();
  const restored = baseRender(<ReportResultView result={result} runID="r" />, `/reports${saved}`);
  restored.container.querySelectorAll('details').forEach((d) => {
    d.open = true;
  });
  expect((screen.getByRole('searchbox', { name: 'Search a.b' }) as HTMLInputElement).value).toBe('model-60');
  expect((screen.getByRole('combobox', { name: 'a%2Eb rows per page' }) as HTMLSelectElement).value).toBe('10');
  expect(within(screen.getByRole('table', { name: 'a%2Eb' })).getAllByRole('cell')[0]!.textContent).toBe('model-10');
});

it.each(['NaN', 'Infinity', '-50', '9999', '26.7'])('repairs malformed or stale URL offset %s in every panel', (page) => {
  const result = fixture();
  result.sections = [{ id: 'coverage', title: 'Coverage', rows: result.rows, columns: result.columns, notes: [] }];
  const view = baseRender(
    <ReportResultView result={result} runID="r" />,
    `/reports?run=r&report.r.data.page=${page}&report.r.section.coverage.page=${page}&report.r.data.size=999&report.r.data.sort=bogus:desc`,
  );
  view.container.querySelectorAll('details').forEach((d) => {
    d.open = true;
  });
  const params = new URLSearchParams(screen.getByTestId('location').textContent!);
  expect(params.has('report.r.data.page')).toBe(false);
  expect(params.has('report.r.section.coverage.page')).toBe(false);
  expect(params.get('run')).toBe('r');
  expect((screen.getByRole('combobox', { name: 'Report data rows per page' }) as HTMLSelectElement).value).toBe('25');
  expect(within(screen.getByRole('table', { name: 'Report data' })).getByText('alpha')).toBeTruthy();
});

it('sorts numbers globally with unknowns last and stable ties in either direction', () => {
  const result = fixture();
  result.rows = [null, 2, 0, 2, undefined, NaN, Infinity, -1].map((value, i) => {
    const values: Record<string, number | null> = {};
    if (value !== undefined) values.requests = value;
    return { dimensions: { model: `row-${i}` }, values };
  });
  render(<ReportResultView result={result} runID="r" />);
  const table = screen.getByRole('table', { name: 'Report data' });
  const order = () =>
    within(table)
      .getAllByRole('row')
      .slice(1)
      .map((row) => within(row).getAllByRole('cell')[0]!.textContent);
  fireEvent.click(within(table).getByRole('button', { name: 'Sort by Requests' }));
  expect(order()).toEqual(['row-7', 'row-2', 'row-1', 'row-3', 'row-0', 'row-4', 'row-5', 'row-6']);
  fireEvent.click(within(table).getByRole('button', { name: 'Sort by Requests' }));
  expect(order()).toEqual(['row-1', 'row-3', 'row-2', 'row-7', 'row-0', 'row-4', 'row-5', 'row-6']);
});

it('does not search withheld columns or hidden snapshot properties', () => {
  session.localOnly = true;
  const result = fixture();
  result.columns.push({ key: 'cost', label: 'Cost', unit: 'USD' }, { key: 'prompt', label: 'Prompt', unit: '' });
  result.rows[0]!.values.cost = 987654;
  result.rows[0]!.dimensions.prompt = 'secrettext';
  result.rows[0]!.dimensions.unlisted = 'privateproperty';
  render(<ReportResultView result={result} runID="r" />);
  for (const value of ['987654', 'secrettext', 'privateproperty']) {
    fireEvent.change(screen.getByRole('searchbox', { name: 'Search Report data' }), { target: { value } });
    expect(within(screen.getByRole('table', { name: 'Report data' })).getAllByRole('row')).toHaveLength(1);
    expect(screen.getByText('0 of 1 snapshot rows')).toBeTruthy();
  }
});

function fixture(): ReportResult {
  return {
    version: 1,
    definition: {
      version: 1,
      sections: [],
      name: 'Frozen usage',
      template: 'usage',
      timezone: 'UTC',
      group_mode: 'historical',
      dimensions: ['model'],
      metrics: ['requests'],
      filters: {},
      compare: true,
      scenario_discount_percent: 0,
      scope: 'self',
      team_id: '',
      period: 'custom',
    },
    generated_at: '2026-09-10T13:00:00Z',
    data_cutoff: '2026-09-10T12:00:00Z',
    start: '2026-09-01T00:00:00Z',
    end: '2026-09-10T00:00:00Z',
    columns: [
      { key: 'model', label: 'Model', unit: '' },
      { key: 'requests', label: 'Requests', unit: 'count' },
    ],
    rows: [{ dimensions: { model: 'alpha' }, values: { requests: 3 } }],
    totals: { requests: 3 },
    previous_totals: { requests: 0 },
    warnings: ['Groups overlap; totals are deduplicated.', 'Partial source coverage.'],
    sections: [],
    source_rows: 4,
  };
}
it('shows frozen provenance, every warning, supplied totals and all full exports', () => {
  render(<ReportResultView result={fixture()} runID="run/a b" />);
  expect(screen.getByRole('heading', { name: 'Frozen usage' })).toBeTruthy();
  expect(screen.getByText('Groups overlap; totals are deduplicated.')).toBeTruthy();
  expect(screen.getByText('Partial source coverage.')).toBeTruthy();
  expect(screen.getByText('2026-09-10T13:00:00Z')).toBeTruthy();
  expect(screen.getByText('2026-09-10T12:00:00Z')).toBeTruthy();
  expect(screen.getByText('UTC')).toBeTruthy();
  expect(screen.getByText('4 source rows')).toBeTruthy();
  expect(screen.getByText('Version 1')).toBeTruthy();
  for (const format of ['csv', 'xlsx', 'pdf', 'json']) {
    expect(screen.getByRole('link', { name: format.toUpperCase() }).getAttribute('href')).toBe(
      `/api/v1/reports/runs/run%2Fa%20b/download?format=${format}`,
    );
  }
  expect(screen.getByText('Export all rows')).toBeTruthy();
  expect(screen.getByText(/percentage change unavailable/i)).toBeTruthy();
  expect(document.body.textContent).not.toMatch(/Infinity|NaN/);
});
it('pages and sorts the measured rows without changing frozen totals or export scope', () => {
  const result = fixture();
  result.rows = Array.from({ length: 27 }, (_, i) => ({
    dimensions: { model: i === 0 ? '' : `model-${String(i).padStart(2, '0')}` },
    values: { requests: i === 0 ? null : i },
  }));
  render(<ReportResultView result={result} runID="r" />);
  const table = screen.getByRole('table', { name: 'Report data' });
  expect(within(table).getAllByText('Unknown').length).toBe(2);
  expect(screen.getByText('Showing 25 of 27 · Rows 1–25')).toBeTruthy();
  expect(screen.getByRole('img', { name: /Requests by Model/ }).textContent).toContain('model-26');
  fireEvent.click(screen.getByRole('button', { name: 'Next Report data page' }));
  expect(screen.getByText('Showing 2 of 27 · Rows 26–27')).toBeTruthy();
  expect(within(table).getByText('model-26')).toBeTruthy();
  fireEvent.click(within(table).getByRole('button', { name: 'Sort by Requests' }));
  expect(screen.getByText('Showing 25 of 27 · Rows 1–25')).toBeTruthy();
  expect(screen.getByRole('link', { name: 'CSV' }).getAttribute('href')).toBe('/api/v1/reports/runs/r/download?format=csv');
});
it('renders every section with independent pagination and methodology, safe text and zero states', () => {
  const result = fixture();
  result.rows = [];
  result.sections = [
    {
      id: 'coverage',
      title: 'Coverage',
      columns: result.columns,
      rows: Array.from({ length: 26 }, (_, i) => ({ dimensions: { model: `entry-${i}` }, values: { requests: 0 } })),
      notes: ['<img src=x onerror=alert(1)>'],
    },
  ];
  const { container } = render(<ReportResultView result={result} runID="r" />);
  expect(screen.getByText('No rows matched this frozen report.')).toBeTruthy();
  expect(screen.getByRole('heading', { name: 'Methodology' })).toBeTruthy();
  expect(screen.getByText('<img src=x onerror=alert(1)>')).toBeTruthy();
  expect(container.querySelector('img')).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: 'Next Coverage page' }));
  expect(screen.getByText('entry-25')).toBeTruthy();
  expect(screen.getByText('Showing 1 of 26 · Rows 26–26')).toBeTruthy();
});
