import { describe, expect, it } from 'vitest';
import { contextDivider, type ContextMessage } from './ChatSearch';

function m(id: string, role: 'user' | 'assistant', extra: Partial<ContextMessage> = {}): ContextMessage {
  return { id, role, content: id, status: 'complete', ...extra };
}

describe('contextDivider', () => {
  it('is absent when nothing was dropped', () => {
    expect(contextDivider([m('u1', 'user'), m('a1', 'assistant')])).toBeNull();
    expect(contextDivider([])).toBeNull();
  });

  it('goes before the first message that was sent', () => {
    const branch = [m('u1', 'user'), m('a1', 'assistant'), m('u2', 'user'), m('a2', 'assistant', { context_dropped: 2 })];
    expect(contextDivider(branch)).toEqual({ beforeId: 'u2', count: 2 });
  });

  it('skips failed replies, which were never context', () => {
    const branch = [
      m('u1', 'user'),
      m('a1', 'assistant', { status: 'error', content: '' }),
      m('u2', 'user'),
      m('a2', 'assistant'),
      m('u3', 'user'),
      m('a3', 'assistant', { context_dropped: 2 }),
    ];
    expect(contextDivider(branch)).toEqual({ beforeId: 'a2', count: 2 });
  });

  it('follows the latest finished reply only', () => {
    const branch = [m('u1', 'user'), m('a1', 'assistant', { context_dropped: 1 }), m('u2', 'user'), m('a2', 'assistant')];
    expect(contextDivider(branch)).toBeNull();
  });
});
