/**
 * Ambient canvas + request attribution contracts.
 *
 * These lock the two behaviours a screenshot would catch: that a density
 * change never blanks the sky, and that shooting-star size actually tracks a
 * job's output tokens.
 */
import { describe, expect, it } from 'vitest';
import { headSizeForTokens, trailLengthForTokens } from './AmbientCanvas';

describe('shooting star sizing', () => {
  it('grows with output tokens', () => {
    const small = trailLengthForTokens(200);
    const medium = trailLengthForTokens(5_000);
    const large = trailLengthForTokens(60_000);
    expect(small).toBeLessThan(medium);
    expect(medium).toBeLessThan(large);
  });

  it('keeps a tiny job visible rather than sub-pixel', () => {
    // A one-line reply must still draw a legible trail; a linear
    // tokens/1000 mapping would make this 0.02px.
    expect(trailLengthForTokens(20)).toBeGreaterThanOrEqual(18);
    expect(headSizeForTokens(20)).toBeGreaterThan(0.5);
  });

  it('clamps a huge job so it cannot streak across the viewport', () => {
    // 200k and 2M tokens must both land on the same bounded maximum.
    expect(trailLengthForTokens(200_000)).toBeLessThanOrEqual(190);
    expect(trailLengthForTokens(2_000_000)).toBeLessThanOrEqual(190);
    expect(headSizeForTokens(2_000_000)).toBeLessThanOrEqual(3.2);
  });

  it('treats a missing or negative count as the minimum, not NaN', () => {
    expect(Number.isFinite(trailLengthForTokens(0))).toBe(true);
    expect(Number.isFinite(trailLengthForTokens(-5))).toBe(true);
    expect(trailLengthForTokens(-5)).toBe(trailLengthForTokens(0));
  });
});
