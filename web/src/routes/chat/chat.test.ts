import { describe, expect, it } from 'vitest';
import { activeBranch, chatTotalCost, newestLeaf, siblingsOf, type ChatMessage } from './ChatPage';

function msg(id: string, parent: string, role: 'user' | 'assistant'): ChatMessage {
  return {
    id,
    parent_id: parent,
    role,
    content: id,
    team_id: '',
    status: 'complete',
    tokens_in: 0,
    tokens_out: 0,
    tokens_cached: 0,
    cost_usd: 0,
    tokens_out_per_second: 0,
    latency_ms: 0,
    ttfb_ms: 0,
    created_at: '',
  };
}

// u1 → a1 ; edit of u1 → u1b → a1b ; regenerate of u1b → a1c
const tree = [
  msg('u1', '', 'user'),
  msg('a1', 'u1', 'assistant'),
  msg('u1b', '', 'user'),
  msg('a1b', 'u1b', 'assistant'),
  msg('a1c', 'u1b', 'assistant'),
];

describe('chat branch helpers', () => {
  it('walks the active branch from the leaf', () => {
    expect(activeBranch(tree, 'a1b').map((m) => m.id)).toEqual(['u1b', 'a1b']);
    expect(activeBranch(tree, 'a1').map((m) => m.id)).toEqual(['u1', 'a1']);
    expect(activeBranch(tree, '')).toEqual([]);
  });

  it('finds edit and regenerate siblings', () => {
    expect(siblingsOf(tree, tree[0]!).map((m) => m.id)).toEqual(['u1', 'u1b']);
    expect(siblingsOf(tree, tree[3]!).map((m) => m.id)).toEqual(['a1b', 'a1c']);
  });

  it('switching to a branch lands on its newest leaf', () => {
    expect(newestLeaf(tree, 'u1')).toBe('a1');
    expect(newestLeaf(tree, 'u1b')).toBe('a1c');
  });
});

describe('chat cost', () => {
  it('sums every reply on every branch, Compare answers included', () => {
    const priced = tree.map((m, i) => ({ ...m, cost_usd: m.role === 'assistant' ? i : 0 }));
    // a1 (1) is on another branch, a1c (4) a sibling of the active a1b (3).
    expect(chatTotalCost(priced)).toBe(1 + 3 + 4);
  });
});
