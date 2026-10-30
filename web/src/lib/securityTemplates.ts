/**
 * Security policy templates, applied client side: a template is a policy
 * body the normal create-policy API accepts, so the server has no template
 * engine. Pure, so it is unit-tested on its own.
 */
import type { SecgwCheck } from './types';

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

/** How many classifier calls a categories check makes per text it judges. */
export function categoryCalls(count: number, ask: string | undefined): number {
  if (count === 0) return 0;
  if (ask === 'separately') return count;
  return Math.ceil(count / 25);
}
