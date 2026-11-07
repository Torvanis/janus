import type { PasswordPolicy } from './types';

export const DEFAULT_PASSWORD_POLICY: PasswordPolicy = {
  min_length: 12,
  require_upper: false,
  require_lower: false,
  require_digit: false,
  require_special: false,
  min_changed_chars: 0,
  history: 0,
  min_age_hours: 0,
  max_age_days: 0,
};

/** One-line description of the rules, for form hints. */
export function describePolicy(p: PasswordPolicy = DEFAULT_PASSWORD_POLICY): string {
  const parts: string[] = [];
  if (p.require_upper) parts.push('an upper-case letter');
  if (p.require_lower) parts.push('a lower-case letter');
  if (p.require_digit) parts.push('a digit');
  if (p.require_special) parts.push('a special character');
  let s = `At least ${p.min_length} characters`;
  if (parts.length === 1) s += `, with ${parts[0]}`;
  else if (parts.length > 1) s += `, with ${parts.slice(0, -1).join(', ')} and ${parts[parts.length - 1]}`;
  s += '.';
  if (p.history > 0) s += ` Not one of your last ${p.history}.`;
  if (parts.length === 0) s += ' A sentence works well.';
  return s;
}

/** The first rule pw breaks, or null (the server checks history and age). */
export function passwordProblem(pw: string, p: PasswordPolicy = DEFAULT_PASSWORD_POLICY): string | null {
  if ([...pw].length < p.min_length) return `Use at least ${p.min_length} characters.`;
  if (p.require_upper && !/\p{Lu}/u.test(pw)) return 'Add an upper-case letter.';
  if (p.require_lower && !/\p{Ll}/u.test(pw)) return 'Add a lower-case letter.';
  if (p.require_digit && !/\p{Nd}/u.test(pw)) return 'Add a digit.';
  if (p.require_special && !/[\p{P}\p{S}]/u.test(pw)) return 'Add a special character.';
  return null;
}
