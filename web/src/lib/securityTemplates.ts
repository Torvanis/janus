/**
 * Security policy templates, applied client side: a template is a policy
 * body the normal create-policy API accepts, so the server has no template
 * engine. Pure, so it is unit-tested on its own.
 */
import type { SecgwCheck, SecurityCategory } from './types';

export interface PolicyDraftTemplate {
  name: string;
  description: string;
  checks: SecgwCheck[];
}

/** The critical safety categories every Categories check starts with. */
export const CRITICAL_SAFETY_SLUGS = [
  'violent_crimes',
  'sex_crimes',
  'child_sexual_exploitation',
  'indiscriminate_weapons',
  'self_harm',
];

/** The personal data classes the Parental controls template redacts. */
export const PARENTAL_PII_CLASSES = ['email', 'phone', 'pan'];

/**
 * Parental controls: content safety with the Categories method over every
 * Family category plus the critical safety ones, on requests and replies
 * (replies are always observed); secrets with the default rules; personal
 * data (email, phone, card numbers). Blocks when enforcement is licensed,
 * otherwise observes. Bound to nothing: the admin binds it.
 */
export function parentalControlsTemplate(opts: {
  name: string;
  description: string;
  categories: SecurityCategory[];
  classifierId?: string;
  enforce: boolean;
}): PolicyDraftTemplate {
  const enabled = new Set(opts.categories.filter((c) => c.enabled).map((c) => c.slug));
  const family = opts.categories.filter((c) => c.group === 'family' && c.enabled).map((c) => c.slug);
  const critical = CRITICAL_SAFETY_SLUGS.filter((s) => enabled.has(s) || opts.categories.length === 0);
  const safety = Array.from(new Set([...family, ...critical]));
  const mode = opts.enforce ? 'block' : 'observe';
  return {
    name: opts.name,
    description: opts.description,
    checks: [
      {
        kind: 'content_safety',
        enabled: true,
        mode,
        fail: 'open',
        direction: 'both',
        classifier_model_id: opts.classifierId || undefined,
        options: { method: 'categories', safety, timeout_ms: 2000 },
      },
      { kind: 'secrets', enabled: true, mode: opts.enforce ? 'block' : 'observe', direction: 'both', options: {} },
      {
        kind: 'pii',
        enabled: true,
        mode: opts.enforce ? 'redact' : 'observe',
        direction: 'both',
        options: { classes: PARENTAL_PII_CLASSES },
      },
    ],
  };
}

/** How many classifier calls a categories check makes per text it judges. */
export function categoryCalls(count: number, ask: string | undefined): number {
  if (count === 0) return 0;
  if (ask === 'separately') return count;
  return Math.ceil(count / 25);
}
