import { describe, expect, it } from 'vitest';
import { newMember, type PoolDraft } from './PoolEditor';

const draft = (policy: PoolDraft['policy'], priorities: number[]): PoolDraft => ({
  policy,
  affinity: 'bounded',
  spill_pct: 25,
  members: priorities.map((p, i) => ({ model_id: `m${i}`, weight: 1, priority: p, enabled: true, context_capacity: 0 })),
});

describe('adding a server to a pool', () => {
  it('under Failover becomes the next standby, not a sharer', () => {
    expect(newMember(draft('failover', [0]), 'x').priority).toBe(1);
    expect(newMember(draft('failover', [0, 1, 1]), 'x').priority).toBe(2);
    expect(newMember(draft('failover', []), 'x').priority).toBe(0);
  });
  it('under a sharing policy joins the serving tier', () => {
    expect(newMember(draft('round_robin', [0, 3]), 'x').priority).toBe(0);
    expect(newMember(draft('least_loaded', [2]), 'x').priority).toBe(2);
  });
});
