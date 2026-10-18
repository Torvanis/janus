import { describe, expect, it } from 'vitest';
import {
  bandOf,
  categorySlug,
  difficultyLabel,
  FAMILY_CATEGORIES,
  instructionsLine,
  reasonLabel,
  ROUTER_TEMPLATES,
  routingHealth,
  splitRouteReason,
  type Router,
  type RoutingDocument,
} from './routing';

const doc = (over: Partial<RoutingDocument> = {}): RoutingDocument => ({
  settings: {
    classifier_model_id: 'c',
    max_concurrent_decisions: 32,
    difficulty_style: 'general',
    max_chars: 6000,
    eval_judge_model_id: '',
    eval_max_concurrent: 2,
    eval_pause_above_in_flight: 50,
    eval_max_output_tokens: 2048,
    retention_days: 90,
  },
  classifiers: [],
  judge_models: [],
  running: true,
  warnings: [],
  eval_queue: 0,
  in_flight_decisions: 0,
  max_categories: 25,
  thinking_levels: [],
  ...over,
});

const router = (over: Partial<Router> = {}): Router => ({
  id: 'r',
  name: 'auto/test',
  description: '',
  status: 'live',
  mode: 'live',
  enabled: true,
  config: {
    routes: [],
    default: { model_id: 'm', thinking: 'passthrough' },
    budget_ms: 800,
    sticky: true,
    sticky_minutes: 30,
    disclose: false,
  },
  grant_count: 1,
  created_at: '',
  updated_at: '',
  problems: [],
  ...over,
});

describe('routingHealth', () => {
  it('is off with no routers or only drafts', () => {
    expect(routingHealth(doc(), []).level).toBe('off');
    expect(routingHealth(doc(), [router({ status: 'draft' })]).level).toBe('off');
  });
  it('is green when live and the classifier answers', () => {
    expect(routingHealth(doc(), [router()]).level).toBe('ok');
  });
  it('is amber while only shadowing', () => {
    const h = routingHealth(doc(), [router({ status: 'shadow' })]);
    expect(h.level).toBe('attention');
    expect(h.reasons[0]).toContain('shadow');
  });
  it('is red when live routers have no classifier', () => {
    const h = routingHealth(doc({ running: false, problem: 'No routing classifier is chosen.' }), [router()]);
    expect(h.level).toBe('down');
    expect(h.reasons).toContain('No routing classifier is chosen.');
  });
  it('flags many fallbacks and losing routes', () => {
    const r = router({
      stats: {
        router_id: 'r',
        requests: 100,
        routed: 50,
        defaulted: 50,
        timeouts: 15,
        errors: 0,
        shadow: 0,
        p50_ms: 1,
        p95_ms: 1,
        mix: [],
        reasons: [],
        categories: [],
      },
      config: {
        ...router().config,
        routes: [
          {
            id: 'x',
            name: 'Cheap',
            model_id: 'a',
            categories: [],
            difficulty_min: 0,
            difficulty_max: 0,
            thinking: 'passthrough',
            restrict_to_granted: false,
            skip_when_over_quota: false,
            eval: { enabled: true, sample_pct: 5, max_per_day: 1, daily_budget_usd: 1, until: '', judge_model_id: '' },
          },
        ],
      },
      evaluations: [
        {
          router_id: 'r',
          route_id: 'x',
          evaluated: 40,
          failed: 0,
          better: 5,
          ties: 5,
          worse: 30,
          consistency: 1,
          routed_cost_nanousd: 0,
          default_cost_nanousd: 0,
          eval_cost_nanousd: 0,
          routed_latency_ms: 0,
          default_latency_ms: 0,
          at_least_as_good_low: 0.1,
          at_least_as_good_high: 0.3,
          verdict: 'losing',
        },
      ],
    });
    const h = routingHealth(doc(), [r]);
    expect(h.level).toBe('attention');
    expect(h.reasons.join(' ')).toContain('15%');
    expect(h.reasons.join(' ')).toContain('“Cheap” is losing');
  });
});

describe('labels', () => {
  it('names reasons and bands', () => {
    expect(reasonLabel('matched:legal')).toBe('Matched a route');
    expect(reasonLabel('route_unavailable')).toBe('Route could not serve');
    expect(bandOf(3.5, 0)).toBe('hard');
    expect(bandOf(1, 2)).toBe('custom');
    expect(difficultyLabel(1, 2)).toBe('1–2');
  });
});

describe('router instructions in the request log', () => {
  it('splits the recorded route reason and words what was added', () => {
    expect(splitRouteReason('matched:legal;instructions=router+route')).toEqual({
      reason: 'matched:legal',
      instructions: 'router+route',
    });
    expect(reasonLabel('matched:legal;instructions=router')).toBe('Matched a route');
    expect(instructionsLine('matched:legal;instructions=router+route')).toBe('Router instructions added: router + route');
    expect(instructionsLine('sticky;instructions=route')).toBe('Router instructions added: route');
    expect(instructionsLine('no_match;instructions=not_applied')).toMatch(/not applied/);
    expect(instructionsLine('matched:legal')).toBe('');
  });
});

describe('Family template', () => {
  it('uses slugs the server will give its categories', () => {
    for (const c of FAMILY_CATEGORIES) expect(categorySlug(c.name)).toBe(c.slug);
    expect(categorySlug('C++ & C#')).toBe('cplusplus_csharp');
  });

  it('routes the five Family categories, then homework learning, with sticky off', () => {
    const t = ROUTER_TEMPLATES.find((x) => x.id === 'family')!;
    expect(t.sticky).toBe(false);
    expect(t.instructions).toMatch(/child/);
    // Backstop for homework the classifier files elsewhere (dev test C01a).
    expect(t.instructions).toMatch(/do not hand over finished homework, final answers to copy/);
    expect(t.instructions).toMatch(/Practice questions, explanations and everyday facts are fine/);
    expect(t.routes.find((r) => r.name === 'Sharing personal information')!.instructions).toMatch(
      /Never quote the information back/,
    );
    expect(t.routes.find((r) => r.name === 'Sharing personal information')!.instructions).toMatch(
      /do not call them by a name taken from/,
    );
    expect(t.instructions).toMatch(/tell them the correct word or answer and why/);
    // Router-wide: personal details are often not routed to the privacy coach.
    expect(t.instructions).toMatch(/never call them by a name taken from an email address, username/);
    expect(t.instructions!.length).toBeLessThanOrEqual(8000);
    expect(t.routes.map((r) => r.name)).toEqual([
      'Feeling low',
      'Not suitable for children',
      'Getting around parental controls',
      'Sharing personal information',
      'Homework done for them',
      'Homework learning',
    ]);
    const known = new Set([...FAMILY_CATEGORIES.map((c) => c.slug), 'homework_learning']);
    for (const r of t.routes) for (const c of r.categories ?? []) expect(known.has(c)).toBe(true);
    for (const r of t.routes) expect((r.instructions ?? '').length).toBeLessThanOrEqual(8000);
    expect(FAMILY_CATEGORIES.find((c) => c.slug === 'feeling_low_or_unsafe')!.min_confidence).toBe(0.35);
    expect(FAMILY_CATEGORIES.find((c) => c.slug === 'homework_done_for_them')!.min_confidence).toBe(0.6);
  });
});
