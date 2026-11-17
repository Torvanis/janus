// What a model can take, give and be asked, as the server reports it
// (internal/capability and internal/adapter/capabilities.go).

export type CapabilityValue = boolean | string | string[] | number;
/** 'default' is a built-in assumption for a vendor's plans (personal-plan models only). */
export type CapabilitySource = 'provider' | 'probe' | 'admin' | 'default';

export interface CapabilityResolved {
  value: CapabilityValue;
  source: CapabilitySource;
  at: string;
  note?: string;
}

export interface CapabilityFact {
  value: CapabilityValue;
  at: string;
  note?: string;
}

export interface CapabilityKey {
  key: string;
  kind: 'bool' | 'string' | 'list' | 'int';
  enum?: string[];
}

export interface CapabilityProbeRun {
  at: string;
  status: 'ok' | 'failed' | 'skipped' | 'running' | 'queued';
  detail?: string;
  requests: number;
  input_tokens: number;
  output_tokens: number;
  by?: string;
}

export interface ModelCapabilityView {
  model_id: string;
  keys: CapabilityKey[];
  resolved: Record<string, CapabilityResolved>;
  provider: Record<string, CapabilityFact>;
  probe: Record<string, CapabilityFact>;
  admin: Record<string, CapabilityFact>;
  probed?: CapabilityProbeRun | null;
}

export const CAPABILITY_GROUPS: { id: string; label: string; keys: string[] }[] = [
  { id: 'input', label: 'Takes in', keys: ['input.text', 'input.image', 'input.audio', 'input.video', 'input.file'] },
  {
    id: 'output',
    label: 'Gives back',
    keys: ['output.text', 'output.image', 'output.audio', 'output.video', 'output.embedding', 'output.rerank'],
  },
  {
    id: 'thinking',
    label: 'Thinking',
    keys: ['thinking.supported', 'thinking.default_on', 'thinking.off', 'thinking.levels', 'thinking.default_level'],
  },
  {
    id: 'request',
    label: 'Requests',
    keys: [
      'request.output_cap',
      'request.refused_params',
      'request.tools',
      'request.tools_endpoint',
      'request.structured_output',
      'limits.max_output_tokens',
    ],
  },
];

const LABELS: Record<string, string> = {
  'input.text': 'Text',
  'input.image': 'Images',
  'input.audio': 'Audio',
  'input.video': 'Video',
  'input.file': 'Files (PDF)',
  'output.text': 'Text',
  'output.image': 'Images',
  'output.audio': 'Audio',
  'output.video': 'Video',
  'output.embedding': 'Embeddings',
  'output.rerank': 'Reranking',
  'thinking.supported': 'Can think before answering',
  'thinking.default_on': 'Thinks unless told not to',
  'thinking.off': 'How thinking is turned off',
  'thinking.levels': 'Thinking levels accepted',
  'thinking.default_level': 'Level when none is sent',
  'request.output_cap': 'Output limit parameter',
  'request.refused_params': 'Parameters it refuses',
  'request.tools': 'Function tools',
  'request.tools_endpoint': 'Tools work through',
  'request.structured_output': 'JSON schema output',
  'limits.max_output_tokens': 'Most output tokens',
};

const VALUE_LABELS: Record<string, string> = {
  reasoning_effort_none: 'reasoning_effort: "none"',
  enable_thinking_false: 'enable_thinking: false',
  think_false: 'think: false',
  thinking_disabled: 'thinking: disabled',
  thinking_budget_zero: 'thinking budget 0',
  reasoning_enabled_false: 'reasoning: enabled false',
  never: 'Cannot be turned off',
  max_tokens: 'max_tokens',
  max_completion_tokens: 'max_completion_tokens',
  chat_completions: 'Chat Completions',
  responses: 'Responses API',
};

export function capabilityLabel(key: string): string {
  return LABELS[key] ?? key;
}

/** One enum value, in words. */
export function capabilityEnumLabel(value: string): string {
  return VALUE_LABELS[value] ?? value;
}

/** A value as a person reads it. */
export function capabilityValueText(value: CapabilityValue | undefined | null): string {
  if (value === undefined || value === null) return 'Not known';
  if (typeof value === 'boolean') return value ? 'Yes' : 'No';
  if (typeof value === 'number') return value.toLocaleString();
  if (Array.isArray(value)) return value.length > 0 ? value.join(', ') : 'None';
  return capabilityEnumLabel(value);
}

export const CAPABILITY_SOURCE_LABEL: Record<CapabilitySource, string> = {
  provider: 'From the provider',
  probe: 'Measured by Janus',
  admin: 'Set by an administrator',
  default: 'Assumed',
};

/** One model of a person's own plan (GET /api/v1/me/subscriptions/{id}/capabilities). */
export interface PlanCapabilityView {
  model: string;
  /** The name to call: my/<provider>/<model>. */
  name: string;
  selected: boolean;
  resolved: Record<string, CapabilityResolved>;
  defaults: Record<string, CapabilityFact>;
  reported: Record<string, CapabilityFact>;
  measured: Record<string, CapabilityFact>;
  set: Record<string, CapabilityFact>;
  probed?: CapabilityProbeRun | null;
  /** When Measure may be pressed again; absent = now. */
  measure_after?: string;
  /** Why this model is not measured; absent = it can be. */
  not_measurable?: string;
}

export interface PlanCapabilitiesResponse {
  subscription_id: string;
  provider: string;
  provider_name: string;
  keys: CapabilityKey[];
  /** Most requests one measurement sends. */
  max_requests: number;
  measure_every_seconds: number;
  models: PlanCapabilityView[];
}

/** "xAI Grok (SuperGrok / X Premium+)" → "xAI Grok": the plan tier is noise in a sentence. */
export function vendorShortName(providerName: string): string {
  return providerName.replace(/\s*\(.*\)\s*$/, '') || providerName;
}

/**
 * Where a plan model's value came from, as the owner reads it. `when` is
 * the measurement's date, already formatted.
 */
export function planSourceLabel(source: CapabilitySource, vendor: string, when: string): string {
  switch (source) {
    case 'default':
      return `Assumed for ${vendor} plans`;
    case 'provider':
      return `Reported by ${vendor}`;
    case 'probe':
      return when ? `Measured ${when}` : 'Measured';
    case 'admin':
      return 'Set by you';
  }
}

/**
 * Short badges for a model card: only what is known to be true. Text in and
 * out is what every chat model does, so it is not a badge.
 */
export function capabilityBadges(caps: Record<string, CapabilityResolved> | undefined | null): string[] {
  if (!caps) return [];
  const yes = (key: string) => caps[key]?.value === true;
  const out: string[] = [];
  if (yes('input.image')) out.push('Sees images');
  if (yes('input.audio')) out.push('Hears audio');
  if (yes('input.video')) out.push('Watches video');
  if (yes('input.file')) out.push('Reads files');
  if (yes('request.tools')) out.push('Tools');
  if (yes('request.structured_output')) out.push('JSON schema');
  if (yes('thinking.supported')) {
    const levels = caps['thinking.levels']?.value;
    const named = Array.isArray(levels) ? sortEfforts(levels.filter((l) => l !== 'none')) : [];
    out.push(named.length > 1 ? `Thinking: ${named[0]}–${named[named.length - 1]}` : 'Thinking');
  }
  return out;
}

const EFFORT_LADDER = ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'ultra'];

/** Thinking levels from least to most; providers list them in either order. */
export function sortEfforts(levels: string[]): string[] {
  const rank = (l: string) => {
    const i = EFFORT_LADDER.indexOf(l);
    return i < 0 ? EFFORT_LADDER.length : i;
  };
  return [...levels].sort((a, b) => rank(a) - rank(b));
}

/** True while a measurement is waiting or under way. */
export function probeInFlight(run: CapabilityProbeRun | null | undefined): boolean {
  return run?.status === 'running' || run?.status === 'queued';
}

const PROBE_BY: Record<string, string> = {
  enabled: 'when it was enabled',
  manual: 'on request',
  all: 'with every enabled model',
  refusal: 'after the provider refused a setting',
  selected: 'when you turned it on',
};

/** The last measurement in one sentence; `when` is the time already formatted. */
export function probeSummary(run: CapabilityProbeRun | null | undefined, when: string): string {
  if (!run) return 'Not measured yet.';
  switch (run.status) {
    case 'running':
      return 'Measuring now: a few small requests are being sent to the model.';
    case 'queued':
      return 'Waiting its turn to be measured.';
    case 'failed':
      return `Could not be measured ${when}: ${run.detail || 'the model did not answer'}.`;
    case 'skipped':
      return `Not measured: ${run.detail || 'this kind of model is not measured'}.`;
    default: {
      const tokens = (run.input_tokens + run.output_tokens).toLocaleString();
      const by = run.by && PROBE_BY[run.by] ? ` ${PROBE_BY[run.by]}` : '';
      return `Measured ${when}${by}: ${run.requests} requests, ${tokens} tokens.${run.detail ? ` ${run.detail}.` : ''}`;
    }
  }
}

/** What is known about a model, as label and value pairs, for a read-only list. */
export function knownCapabilities(
  caps: Record<string, CapabilityResolved> | undefined | null,
): { group: string; items: { key: string; label: string; value: string }[] }[] {
  if (!caps) return [];
  return CAPABILITY_GROUPS.map((group) => ({
    group: group.label,
    items: group.keys
      .filter((key) => caps[key] !== undefined)
      .map((key) => ({ key, label: capabilityLabel(key), value: capabilityValueText(caps[key]?.value) })),
  })).filter((group) => group.items.length > 0);
}
