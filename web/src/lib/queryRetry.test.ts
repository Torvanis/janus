import { describe, expect, it } from 'vitest';
import { ApiError } from './api';
import { shouldRetryQuery } from './queryRetry';

const err = (status: number) => new ApiError(status, { message: 'x', code: 'x', type: 'x' });

describe('shouldRetryQuery', () => {
  it('never retries a client error', () => {
    for (const status of [400, 401, 403, 404, 409, 422]) expect(shouldRetryQuery(0, err(status))).toBe(false);
  });
  it('retries network, server, timeout and rate-limit errors twice', () => {
    for (const status of [0, 408, 429, 500, 502, 503]) {
      expect(shouldRetryQuery(0, err(status))).toBe(true);
      expect(shouldRetryQuery(1, err(status))).toBe(true);
      expect(shouldRetryQuery(2, err(status))).toBe(false);
    }
    expect(shouldRetryQuery(0, new Error('boom'))).toBe(true);
  });
});
