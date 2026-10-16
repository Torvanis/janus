/** Auto-routing types and pure helpers (Administration → Auto-routing). */

export type RouterStatus = 'draft' | 'shadow' | 'live';

export interface RouteEval {
  enabled: boolean;
  sample_pct: number;
  max_per_day: number;
  daily_budget_usd: number;
  until: string;
  judge_model_id: string;
}

export interface RouterRoute {
  id: string;
  name: string;
  model_id: string;
  categories: string[];
  difficulty_min: number;
  difficulty_max: number;
  thinking: string;
  restrict_to_granted: boolean;
  skip_when_over_quota: boolean;
  eval: RouteEval;
  model_kind?: string;
  model_name?: string;
  upstream_name?: string;
  model_hosting?: string;
  context_window?: number;
  broken?: boolean;
  broken_reason?: string;
}

export interface RouterDefault {
  model_id: string;
  thinking: string;
  model_kind?: string;
  model_name?: string;
  upstream_name?: string;
  model_hosting?: string;
  broken?: boolean;
  broken_reason?: string;
}

export interface RouterConfig {
  routes: RouterRoute[];
  default: RouterDefault;
  budget_ms: number;
  sticky: boolean;
  sticky_minutes: number;
  disclose: boolean;
}

export interface RouteMixRow {
  route_id: string;
  route_name: string;
  model_name: string;
  requests: number;
}

export interface CountRow {
  reason: string;
  requests: number;
}

export interface RouterStats {
  router_id: string;
  requests: number;
  routed: number;
  defaulted: number;
  timeouts: number;
  errors: number;
  shadow: number;
  p50_ms: number;
  p95_ms: number;
  mix: RouteMixRow[];
  reasons: CountRow[];
  categories: CountRow[];
}

export interface RouteEvalSummary {
  router_id: string;
  route_id: string;
  /** The pairing these comparisons used: the route's model, the default and the judge. */
  routed_model_id?: string;
  default_model_id?: string;
  judge_model_id?: string;
  /** True when the pairing is the one the route is configured with now. */
  current?: boolean;
  first_at?: string;
  last_at?: string;
  evaluated: number;
  failed: number;
  /** Samples a Security Gateway policy kept from being sent to the default or the judge. */
  skipped?: number;
  better: number;
  ties: number;
  worse: number;
  consistency: number;
  routed_cost_nanousd: number;
  default_cost_nanousd: number;
  eval_cost_nanousd: number;
  routed_latency_ms: number;
  default_latency_ms: number;
  at_least_as_good_low: number;
  at_least_as_good_high: number;
  verdict: 'keep' | 'losing' | 'needs_data';
}

export interface Router {
  id: string;
  name: string;
  description: string;
  status: RouterStatus;
  mode: string;
  enabled: boolean;
  config: RouterConfig;
  grant_count: number;
  created_at: string;
  updated_at: string;
  problems: string[];
  stats?: RouterStats;
  evaluations?: RouteEvalSummary[];
}

export interface RoutingCategory {
  id: string;
  slug: string;
  name: string;
  includes: string;
  excludes: string;
  linked_slug: string;
  linked_name?: string;
  min_confidence: number;
  examples: string[];
  builtin: boolean;
  enabled: boolean;
  sort_order: number;
  used_by: string[];
}

export interface RoutingSettings {
  classifier_model_id: string;
  max_concurrent_decisions: number;
  difficulty_style: 'general' | 'category';
  max_chars: number;
  eval_judge_model_id: string;
  eval_max_concurrent: number;
  eval_pause_above_in_flight: number;
  eval_max_output_tokens: number;
  retention_days: number;
}

export interface RoutingClassifierOption {
  id: string;
  name: string;
  upstream_name: string;
  hosting: string;
  status: string;
  style: string;
  used_by_security: boolean;
}

export interface RoutingTarget {
  id: string;
  kind: 'model' | 'managed';
  name: string;
  upstream_name?: string;
  hosting?: string;
  context_window?: number;
  pool?: boolean;
  description?: string;
}

export interface RoutingDocument {
  settings: RoutingSettings;
  classifiers: RoutingClassifierOption[];
  judge_models: RoutingTarget[];
  running: boolean;
  problem?: string;
  warnings: string[];
  /** Evaluation set-ups to check (also in warnings): judge is a contestant, on-prem work evaluated off site. */
  eval_warnings?: string[];
  eval_queue: number;
  eval_running?: number;
  in_flight_decisions: number;
  max_categories: number;
  thinking_levels: string[];
}

export interface RoutingStep {
  route_id: string;
  index: number;
  outcome: 'matched' | 'skipped';
  detail: string;
}

export interface RoutingTestRouter {
  router_id: string;
  router_name: string;
  route_id: string;
  route_name: string;
  model_id: string;
  model_name: string;
  thinking: string;
  reason: string;
  trace: RoutingStep[];
}

export interface RoutingTestResult {
  status: string;
  error?: string;
  category: string;
  confidence: number;
  probabilities: Record<string, number>;
  difficulty: number;
  latency_ms: number;
  calls: number;
  classifier: string;
  routers: RoutingTestRouter[];
}

export interface BatchRow {
  expected: string;
  got: string;
  confidence: number;
  difficulty: number;
  text: string;
  ok: boolean;
  error?: string;
}

export interface BatchCategory {
  slug: string;
  name: string;
  total: number;
  correct: number;
  accuracy: number;
  misses: BatchRow[];
}

export interface BatchResult {
  classifier: string;
  total: number;
  correct: number;
  failed: number;
  accuracy: number;
  elapsed_ms: number;
  categories: BatchCategory[];
}

export interface RoutingDecisionRow {
  id: string;
  created_at: string;
  request_id: string;
  router_id: string;
  mode: string;
  route_id: string;
  route_name: string;
  model_name: string;
  category: string;
  confidence: number;
  difficulty: number;
  thinking: string;
  reason: string;
  status: string;
  error?: string;
  decision_ms: number;
  calls: number;
}

export interface RoutingEvalRow {
  id: string;
  created_at: string;
  request_id: string;
  route_id: string;
  routed_model_id?: string;
  default_model_id?: string;
  judge_model_id?: string;
  verdict: string;
  routed_score: number;
  default_score: number;
  consistent: boolean;
  rationale: string;
  status: string;
  error?: string;
  eval_cost_nanousd: number;
}

/**
 * Evaluation tallies are kept per pairing (route model, default, judge). A
 * tally is current when it compares the models the route uses now; older
 * pairings are history and never count towards the route's verdict.
 */
export function isCurrentPairing(e: RouteEvalSummary): boolean {
  return e.current !== false;
}

/** The current pairing's tally for a route, and its older pairings (newest first). */
export function routeEvalPairings(
  evals: RouteEvalSummary[] | undefined,
  routeId: string,
): { current?: RouteEvalSummary; older: RouteEvalSummary[] } {
  const mine = (evals ?? []).filter((e) => e.route_id === routeId);
  return { current: mine.find(isCurrentPairing), older: mine.filter((e) => !isCurrentPairing(e)) };
}

/** Plain-language names for decision reasons. */
export const REASON_LABELS: Record<string, string> = {
  matched: 'Matched a route',
  sticky: 'Same conversation',
  no_match: 'No route fit',
  low_confidence: 'Classifier unsure',
  timeout: 'Classifier too slow',
  classifier_error: 'Classifier failed',
  busy: 'Too many decisions at once',
  no_classifier: 'No classifier',
  no_text: 'Nothing to classify',
  shadow: 'Shadow (default served)',
  default_only: 'Default only',
  route_unavailable: 'Route could not serve',
  over_quota: 'Caller over quota',
};

export function reasonLabel(reason: string): string {
  const base = reason.split(':')[0] ?? reason;
  return REASON_LABELS[base] ?? reason;
}

/** Reasons that mean the router could not decide (not a decision to use the default). */
export const FAILURE_REASONS = new Set(['timeout', 'classifier_error', 'busy', 'no_classifier']);

export const DIFFICULTY_BANDS: Array<{ id: string; label: string; min: number; max: number }> = [
  { id: 'any', label: 'Any', min: 0, max: 0 },
  { id: 'easy', label: 'Easy (1–2)', min: 0, max: 2.5 },
  { id: 'medium', label: 'Medium (3)', min: 2.5, max: 3.5 },
  { id: 'hard', label: 'Hard (4–5)', min: 3.5, max: 0 },
  { id: 'expert', label: 'Expert (5)', min: 4.5, max: 0 },
];

export function bandOf(min: number, max: number): string {
  const b = DIFFICULTY_BANDS.find((d) => d.min === min && d.max === max);
  return b ? b.id : 'custom';
}

export function difficultyLabel(min: number, max: number): string {
  const b = DIFFICULTY_BANDS.find((d) => d.min === min && d.max === max);
  if (b) return b.label;
  if (min > 0 && max > 0) return `${min}–${max}`;
  if (min > 0) return `≥ ${min}`;
  return `≤ ${max}`;
}

export const THINKING_LABELS: Record<string, string> = {
  passthrough: 'As requested',
  scale: 'Scale with difficulty',
  none: 'None',
  minimal: 'Minimal',
  low: 'Low',
  medium: 'Medium',
  high: 'High',
  xhigh: 'Extra high',
  max: 'Max',
};

export function thinkingLabel(v: string): string {
  return THINKING_LABELS[v] ?? v;
}

export type HealthLevel = 'ok' | 'attention' | 'off' | 'down';

export interface RoutingHealth {
  level: HealthLevel;
  title: string;
  reasons: string[];
}

/**
 * One verdict for the Auto-routing overview: green when routers are live and
 * deciding, amber when an admin should look (shadow only, slow or failing
 * classifier, a losing route, a configuration problem), red when live routers
 * cannot decide at all, grey when nothing is set up.
 */
export function routingHealth(doc: RoutingDocument, routers: Router[]): RoutingHealth {
  const live = routers.filter((r) => r.status === 'live');
  const shadow = routers.filter((r) => r.status === 'shadow');
  const reasons: string[] = [];
  if (routers.length === 0 || (live.length === 0 && shadow.length === 0)) {
    return {
      level: 'off',
      title: 'Auto-routing is not in use',
      reasons: [routers.length === 0 ? 'No router has been created yet.' : 'Every router is a draft.'],
    };
  }
  if (!doc.running) {
    const msg = doc.problem ?? 'The routing classifier is not available.';
    if (live.length > 0) {
      return {
        level: 'down',
        title: 'Live routers cannot decide',
        reasons: [msg, 'Every request is being served by its router’s default.'],
      };
    }
    reasons.push(msg);
  }
  for (const r of routers) {
    if (r.status === 'draft') continue;
    for (const p of r.problems) reasons.push(`${r.name}: ${p}`);
    const st = r.stats;
    if (st && st.requests >= 20) {
      const failed = st.timeouts + st.errors;
      if (failed / st.requests > 0.1) {
        reasons.push(
          `${r.name}: ${Math.round((failed / st.requests) * 100)}% of decisions in the last 24 hours fell back because the classifier was slow or failed.`,
        );
      }
    }
    for (const e of r.evaluations ?? []) {
      if (e.verdict === 'losing' && isCurrentPairing(e)) {
        const route = r.config.routes.find((x) => x.id === e.route_id);
        reasons.push(
          `${r.name}: route “${route?.name || route?.model_name || e.route_id}” is losing to the default in evaluations.`,
        );
      }
    }
  }
  for (const w of doc.warnings) reasons.push(w);
  if (live.length === 0) {
    reasons.unshift(
      `${shadow.length === 1 ? 'One router is' : `${shadow.length} routers are`} in shadow: requests are served by the default while Janus records what it would have done.`,
    );
    return { level: 'attention', title: 'Watching, not routing yet', reasons };
  }
  if (reasons.length > 0) return { level: 'attention', title: 'Routing, with things to check', reasons };
  return {
    level: 'ok',
    title: 'Routing is working',
    reasons: [`${live.length === 1 ? 'One router is' : `${live.length} routers are`} live and the classifier is answering.`],
  };
}

/** A blank route for the builder. */
export function newRoute(): RouterRoute {
  return {
    id: '',
    name: '',
    model_id: '',
    categories: [],
    difficulty_min: 0,
    difficulty_max: 0,
    thinking: 'passthrough',
    restrict_to_granted: false,
    skip_when_over_quota: false,
    eval: { enabled: false, sample_pct: 5, max_per_day: 200, daily_budget_usd: 5, until: '', judge_model_id: '' },
  };
}

export interface RouterTemplate {
  id: string;
  name: string;
  modelName: string;
  summary: string;
  routes: Array<Partial<RouterRoute> & { hint: string }>;
  defaultThinking: string;
  defaultHint: string;
}

/**
 * Starting points. Templates fill categories, difficulty and thinking; the
 * admin picks the models (Janus cannot know which of theirs is "local" or
 * "frontier").
 */
export const ROUTER_TEMPLATES: RouterTemplate[] = [
  {
    id: 'cost',
    name: 'Cost saver',
    modelName: 'auto/cost-saver',
    summary: 'Easy everyday work goes to a cheaper or local model; everything else stays on your best model.',
    routes: [
      {
        name: 'Everyday, easy',
        categories: ['quick_facts', 'summarising', 'translation', 'admin_docs', 'writing'],
        difficulty_min: 0,
        difficulty_max: 2.5,
        hint: 'a cheaper or local model',
      },
    ],
    defaultThinking: 'passthrough',
    defaultHint: 'your best general model',
  },
  {
    id: 'onprem',
    name: 'Keep specialist work on-prem',
    modelName: 'auto/legal',
    summary: 'Legal and other sensitive specialist requests stay on a model you host; everything else goes to a frontier model.',
    routes: [
      { name: 'Legal on-prem', categories: ['legal'], hint: 'your self-hosted or fine-tuned model' },
      { name: 'Specialist on-prem', categories: ['specialist'], hint: 'your self-hosted model' },
    ],
    defaultThinking: 'passthrough',
    defaultHint: 'a frontier model',
  },
  {
    id: 'effort',
    name: 'Effort scaler',
    modelName: 'auto/smart',
    summary: 'One frontier model for everything; Janus sets how hard it thinks from the request.',
    routes: [
      {
        name: 'Hard reasoning',
        categories: ['coding', 'maths_science', 'research'],
        difficulty_min: 3.5,
        difficulty_max: 0,
        thinking: 'high',
        hint: 'the same frontier model',
      },
      {
        name: 'Quick answers',
        categories: ['quick_facts', 'translation', 'personal_wellbeing'],
        thinking: 'low',
        hint: 'the same frontier model',
      },
    ],
    defaultThinking: 'medium',
    defaultHint: 'your frontier model',
  },
  {
    id: 'coding',
    name: 'Coding by difficulty',
    modelName: 'auto/code',
    summary: 'Routine coding goes to a fast coding model, hard coding to your strongest model with more thinking.',
    routes: [
      {
        name: 'Hard coding',
        categories: ['coding'],
        difficulty_min: 3.5,
        difficulty_max: 0,
        thinking: 'high',
        hint: 'your strongest model',
      },
      { name: 'Routine coding', categories: ['coding'], hint: 'a fast coding model' },
    ],
    defaultThinking: 'passthrough',
    defaultHint: 'a general model for non-coding requests',
  },
];

/** Demo prompts for the Try box, one per behaviour worth showing. */
export const DEMO_PROMPTS: Array<{ label: string; text: string }> = [
  {
    label: 'Contract clause',
    text: 'Review this indemnification clause and tell me what risk it puts on us: "Supplier shall indemnify Customer against all losses arising from any breach, without limit."',
  },
  {
    label: 'Hard coding',
    text: 'Design a lock-free multi-producer single-consumer queue in Rust and explain the memory ordering on each atomic.',
  },
  { label: 'Easy coding', text: 'How do I check if a list is empty in Python?' },
  { label: 'Quick fact', text: 'Convert 100 kilometers to miles.' },
  { label: 'Translation', text: 'Translate "the meeting is moved to Thursday at 3pm" into formal German.' },
  { label: 'Medical', text: "I've had a dull headache behind one eye for three days, should I worry?" },
  { label: 'Planning', text: 'Should we launch the beta in March or wait for the payments feature? We have 4 engineers.' },
];

export function pct(v: number): string {
  return `${Math.round(v * 100)}%`;
}
