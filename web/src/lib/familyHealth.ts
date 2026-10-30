/**
 * Family safety verdict, ranked once so the card, the Overview summary and
 * the tests agree: green Protecting, red Not protecting with the reasons,
 * grey Off.
 */
import type { FamilyReason, FamilyState } from './types';
import type { MessageKey } from './i18n';

export interface FamilyVerdict {
  tone: 'success' | 'danger' | 'neutral';
  title: MessageKey;
  reasons: MessageKey[];
}

const REASON_KEYS: Record<FamilyReason, MessageKey> = {
  no_model: 'adminSecurity.family.reasonNoModel',
  model_unavailable: 'adminSecurity.family.reasonModelUnavailable',
  model_down: 'adminSecurity.family.reasonModelDown',
  no_one_assigned: 'adminSecurity.family.reasonNoOneAssigned',
};

export function familyVerdict(state: FamilyState, reasons: FamilyReason[]): FamilyVerdict {
  if (state === 'protecting') return { tone: 'success', title: 'adminSecurity.family.stateProtecting', reasons: [] };
  if (state === 'not_protecting') {
    return {
      tone: 'danger',
      title: 'adminSecurity.family.stateNotProtecting',
      reasons: reasons.map((r) => REASON_KEYS[r]).filter(Boolean),
    };
  }
  return { tone: 'neutral', title: 'adminSecurity.family.stateOff', reasons: [] };
}
