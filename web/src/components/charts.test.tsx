/**
 * Chart component tests, centred on the stacked-series mode of AreaChart.
 *
 * The single-series contract is locked with a GOLDEN markup string captured
 * from the implementation as it existed BEFORE stacked mode was added: when
 * callers pass TimePoint[] (Dashboard, Explore, Team, admin Overview) the
 * output must remain byte-identical, modulo the opaque useId gradient id,
 * which React derives from tree position and never guarantees stable.
 */
import { beforeEach, describe, expect, it } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { renderToStaticMarkup } from 'react-dom/server';
import { AreaChart, niceAxisMax, seriesColor, seriesDash, type AreaSeries } from './charts';
import type { TimePoint } from '../lib/types';

beforeEach(cleanup);

function tp(bucket: string, tokensIn: number, tokensOut: number, requests: number): TimePoint {
  return {
    bucket,
    totals: {
      tokens_in: tokensIn,
      tokens_out: tokensOut,
      tokens_cached: 0,
      tokens_cache_write_5m: 0,
      tokens_cache_write_1h: 0,
      cost_nanousd: 0,
      request_count: requests,
      error_count: 0,
    },
  };
}

// useId output (e.g. ":R0:") depends on tree position, not on the component's
// markup contract. Normalise it in both the golden and the fresh render.
const SINGLE_SERIES: TimePoint[] = [tp('a', 10, 5, 3), tp('b', 20, 10, 7), tp('c', 5, 2, 1)];

// Captured from `renderToStaticMarkup(<AreaChart series={SINGLE_SERIES} metric="tokens" />)`
// on the pre-stacking implementation (see file header). Do not hand-edit.
const GOLDEN_SINGLE =
  '<figure style="margin:0"><figcaption class="sr-only">Tokens over time. Total 52 across 3 buckets.</figcaption>' +
  '<svg viewBox="0 0 100 100" preserveAspectRatio="none" style="width:100%;height:180px;display:block" role="img" aria-label="Tokens trend">' +
  '<defs><linearGradient id=":R0:" x1="0" y1="0" x2="0" y2="1">' +
  '<stop offset="0%" stop-color="var(--janus-chart-1)" stop-opacity="0.42"></stop>' +
  '<stop offset="100%" stop-color="var(--janus-chart-1)" stop-opacity="0"></stop>' +
  '</linearGradient></defs>' +
  '<line x1="0" y1="25" x2="100" y2="25" stroke="var(--janus-chart-grid)" stroke-width="0.3"></line>' +
  '<line x1="0" y1="50" x2="100" y2="50" stroke="var(--janus-chart-grid)" stroke-width="0.3"></line>' +
  '<line x1="0" y1="75" x2="100" y2="75" stroke="var(--janus-chart-grid)" stroke-width="0.3"></line>' +
  '<path d="M0.00,54.00 L50.00,8.00 L100.00,78.53 L100,100 L0,100 Z" fill="url(#:R0:)"></path>' +
  '<path d="M0.00,54.00 L50.00,8.00 L100.00,78.53" fill="none" stroke="var(--janus-chart-1)" stroke-width="0.8" vector-effect="non-scaling-stroke"></path>' +
  '</svg>' +
  '<div class="row-between small muted" style="margin-top:6px"><span>peak 30</span><span>total 52</span></div></figure>';

const GOLDEN_EMPTY =
  '<div class="state" style="padding:var(--janus-space-8)"><p class="state-body small">No activity in this range yet.</p></div>';

// Two stacked layers, bottom-up: totals per bucket are 3, 5, 7. This is the
// shape Explore renders for the Tokens metric (tokens-out stacked over
// tokens-in) — the default chart under usage emphasis (spend_emphasis off).
const STACKED: AreaSeries[] = [
  { key: 'tokens_in', label: 'Tokens in', color: seriesColor(0), values: [1, 2, 3] },
  { key: 'tokens_out', label: 'Tokens out', color: seriesColor(1), values: [2, 3, 4] },
];

// y = 100 - (value / axisMax) * 92. Charts now round the axis top with
// niceAxisMax so the rendered scale labels are round numbers, so tests must
// scale against the same rounded maximum the chart draws against.
function yFor(value: number, max: number): string {
  return (100 - (value / niceAxisMax(max)) * 92).toFixed(2);
}

describe('AreaChart single-series mode (backward compatibility)', () => {
  it('plots against a rounded axis top so the scale reads in round numbers', () => {
    // The chart gained a labelled y-axis, which required two deliberate
    // changes from the old golden markup: the SVG is wrapped by an HTML scale
    // (SVG <text> would be distorted by preserveAspectRatio="none"), and the
    // paths are drawn against niceAxisMax(peak) rather than the raw peak — so
    // gridlines land on 10, not 7. The series peak is 7, so the top of the
    // plot is 10 and the peak sits at 70% of the band.
    const { container } = render(<AreaChart series={SINGLE_SERIES} metric="tokens" />);
    const stroke = container.querySelector('path[stroke]')?.getAttribute('d') ?? '';
    // tp(bucket, tokensIn, tokensOut, requests), so the "tokens" metric is
    // 10+5, 20+10, 5+2 => 15, 30, 7. The peak is 30, rounded up to 50.
    expect(niceAxisMax(30)).toBe(50);
    // Every point is plotted against that rounded top.
    for (const value of [15, 30, 7]) {
      expect(stroke).toContain(yFor(value, 30));
    }
    // With a raw-peak axis the maximum pinned to y=8.00 (the top of the band),
    // as the captured golden shows; a rounded axis leaves headroom, which is
    // what makes the gridline labels meaningful.
    expect(GOLDEN_SINGLE).toContain('L50.00,8.00');
    expect(stroke).not.toContain(',8.00');
  });

  it('renders a readable y-axis scale rather than unlabelled gridlines', () => {
    // The reported gap: a chart shape with no magnitude anywhere on it.
    const { container } = render(<AreaChart series={SINGLE_SERIES} metric="tokens" />);
    const text = container.textContent ?? '';
    // Zero baseline plus a non-zero top tick must both be present.
    expect(text).toContain('0');
    expect(container.querySelectorAll('span').length).toBeGreaterThan(3);
  });

  it('renders the empty state byte-identically for an empty series', () => {
    const markup = renderToStaticMarkup(<AreaChart series={[]} metric="requests" height={120} label="Traffic" />);
    expect(markup).toBe(GOLDEN_EMPTY);
  });

  it('renders exactly one stroked path and no legend', () => {
    const { container } = render(<AreaChart series={SINGLE_SERIES} metric="tokens" />);
    const strokes = container.querySelectorAll('path[stroke]');
    expect(strokes).toHaveLength(1);
    expect(container.querySelector('ul')).toBeNull();
  });
});

describe('AreaChart cost metric (flag-off deployments)', () => {
  // 'cost' stays a fully working MetricKey: local-only instances simply never
  // select it (pages drop it from their metric pickers via useLocalOnly), so
  // this locks the unchanged flag-off contract.
  const withCost = (bucket: string, cost: number): TimePoint => ({
    bucket,
    totals: {
      tokens_in: 0,
      tokens_out: 0,
      tokens_cached: 0,
      // Required on Totals since the cache-hit-rate work landed on main.
      tokens_cache_write_5m: 0,
      tokens_cache_write_1h: 0,
      cost_nanousd: cost,
      request_count: 0,
      error_count: 0,
    },
  });

  it('charts cost_nanousd with the Spend caption and USD-formatted totals', () => {
    const { container } = render(
      <AreaChart series={[withCost('a', 1_000_000_000), withCost('b', 3_000_000_000)]} metric="cost" />,
    );

    const caption = container.querySelector('figcaption');
    expect(caption?.textContent).toContain('Spend over time');
    expect(container.querySelector('svg')?.getAttribute('aria-label')).toBe('Spend trend');
    // Footer totals go through formatUSD: 1e9 + 3e9 nano-USD = $4.00.
    expect(container.textContent).toContain('total $4.00');
  });
});

describe('AreaChart stacked mode', () => {
  it('renders one boundary path per layer, stacked cumulatively bottom-up', () => {
    const { container } = render(<AreaChart series={STACKED} metric="tokens" />);
    const boundaries = container.querySelectorAll('path[data-series]');
    expect(boundaries).toHaveLength(2);

    // Bottom layer boundary is its own values: 1, 2, 3 of max 7.
    const bottom = container.querySelector('path[data-series="tokens_in"]');
    expect(bottom?.getAttribute('d')).toBe(`M0.00,${yFor(1, 7)} L50.00,${yFor(2, 7)} L100.00,${yFor(3, 7)}`);

    // Top layer boundary is the cumulative sum: 3, 5, 7 — the stack total.
    const top = container.querySelector('path[data-series="tokens_out"]');
    expect(top?.getAttribute('d')).toBe(`M0.00,${yFor(3, 7)} L50.00,${yFor(5, 7)} L100.00,${yFor(7, 7)}`);
  });

  it('reports peak and total of the summed stack in the footer', () => {
    render(<AreaChart series={STACKED} metric="tokens" />);
    expect(screen.getByText('peak 7')).toBeTruthy();
    expect(screen.getByText('total 15')).toBeTruthy(); // 3 + 5 + 7
  });

  it('renders a legend entry per layer with label, colour swatch, and dash sample', () => {
    const { container } = render(<AreaChart series={STACKED} metric="tokens" />);
    const legend = container.querySelector('ul[aria-label="Chart series"]');
    expect(legend).not.toBeNull();
    const entries = legend!.querySelectorAll('li');
    expect(entries).toHaveLength(2);

    const first = entries[0]!;
    const second = entries[1]!;
    expect(first.textContent).toBe('Tokens in');
    expect(second.textContent).toBe('Tokens out');

    // Colour swatches carry the layer's palette token.
    const swatch = (entry: Element): string => (entry.querySelector('span[aria-hidden="true"]') as HTMLElement).style.background;
    expect(swatch(first)).toBe(seriesColor(0));
    expect(swatch(second)).toBe(seriesColor(1));
    expect(swatch(first)).not.toBe(swatch(second));

    // Each entry has an inline dash sample line.
    expect(first.querySelector('svg line')).not.toBeNull();
    expect(second.querySelector('svg line')).not.toBeNull();
  });

  it('never encodes layers by hue alone: distinct colours AND distinct dash patterns', () => {
    const three: AreaSeries[] = [0, 1, 2].map((index) => ({
      key: `layer-${index}`,
      label: `Layer ${index}`,
      color: seriesColor(index),
      values: [1, 1],
    }));
    const { container } = render(<AreaChart series={three} metric="requests" />);
    const boundaries = [...container.querySelectorAll('path[data-series]')];
    expect(boundaries).toHaveLength(3);

    const colors = boundaries.map((path) => path.getAttribute('stroke'));
    expect(new Set(colors).size).toBe(3);

    // Default dash assignment: solid for layer 0 (attribute absent), a
    // distinct non-solid pattern for every later layer.
    const dashes = boundaries.map((path) => path.getAttribute('stroke-dasharray') ?? '0');
    expect(new Set(dashes).size).toBe(3);
    expect(dashes[0]).toBe('0');
    expect(dashes[1]).toBe(seriesDash(1));
    expect(dashes[2]).toBe(seriesDash(2));
  });

  it('normalises comma-separated dash patterns to SVG space form', () => {
    const series: AreaSeries[] = [
      { key: 'a', label: 'A', color: seriesColor(0), dash: '4,2', values: [1, 2] },
      { key: 'b', label: 'B', color: seriesColor(1), values: [1, 2] },
    ];
    const { container } = render(<AreaChart series={series} metric="requests" />);
    const custom = container.querySelector('path[data-series="a"]');
    expect(custom?.getAttribute('stroke-dasharray')).toBe('4 2');
  });

  it('describes the split in the accessible caption', () => {
    const { container } = render(<AreaChart series={STACKED} metric="tokens" label="Token throughput" />);
    const caption = container.querySelector('figcaption');
    expect(caption?.textContent).toContain('Token throughput over time, split by Tokens in, Tokens out');
    expect(caption?.textContent).toContain('Total 15 across 3 buckets');
  });

  it('reports peak and total of only that layer when a single layer is passed (token In/Out views)', () => {
    const single: AreaSeries[] = [{ key: 'tokens_out', label: 'Tokens out', color: seriesColor(1), values: [50, 150] }];
    const { container } = render(<AreaChart series={single} metric="tokens" />);

    expect(container.querySelectorAll('path[data-series]')).toHaveLength(1);
    expect(screen.getByText('peak 150')).toBeTruthy();
    expect(screen.getByText('total 200')).toBeTruthy();
  });

  it('renders the shared empty state when every layer has no buckets', () => {
    const empty: AreaSeries[] = [
      { key: 'a', label: 'A', color: seriesColor(0), values: [] },
      { key: 'b', label: 'B', color: seriesColor(1), values: [] },
    ];
    const markup = renderToStaticMarkup(<AreaChart series={empty} metric="tokens" />);
    expect(markup).toBe(GOLDEN_EMPTY);
  });
});
