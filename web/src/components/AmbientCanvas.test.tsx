/**
 * Ambient canvas + request attribution contracts.
 *
 * These lock the behaviours a screenshot would catch: that a density change
 * never sweeps a band of stars across the sky, and that shooting-star size
 * actually tracks a job's output tokens.
 */
import { describe, expect, it } from 'vitest';
import { arrivingStarPosition, headSizeForTokens, trailLengthForTokens } from './AmbientCanvas';

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

  it('keeps the smallest job the same size and grows the largest to 1.5x', () => {
    // Previous bounds: trail 18..190px, head 0.9..3.2px.
    expect(trailLengthForTokens(0)).toBe(18);
    expect(headSizeForTokens(0)).toBe(0.9);
    expect(trailLengthForTokens(2_000_000)).toBeCloseTo(190 * 1.5, 5);
    expect(headSizeForTokens(2_000_000)).toBeCloseTo(3.2 * 1.5, 5);
  });

  it('clamps a huge job so it cannot streak across the viewport', () => {
    // 200k and 2M tokens must both land on the same bounded maximum.
    expect(trailLengthForTokens(200_000)).toBeLessThanOrEqual(285);
    expect(trailLengthForTokens(200_000)).toBe(trailLengthForTokens(2_000_000));
    expect(headSizeForTokens(2_000_000)).toBeLessThanOrEqual(4.8);
  });

  it('treats a missing or negative count as the minimum, not NaN', () => {
    expect(Number.isFinite(trailLengthForTokens(0))).toBe(true);
    expect(Number.isFinite(trailLengthForTokens(-5))).toBe(true);
    expect(trailLengthForTokens(-5)).toBe(trailLengthForTokens(0));
  });
});

describe('background density changes', () => {
  it('fades new stars in across the whole field, not at the left edge', () => {
    // A burst of arrivals (the jump from the quiet first paint to real
    // traffic on page load) must spread over the sky; entering them all at
    // x<0 is what drew a band of stars sweeping left to right.
    let seed = 1;
    const random = () => {
      seed = (seed * 16807) % 2147483647;
      return (seed - 1) / 2147483646;
    };
    const width = 1600;
    const xs = Array.from({ length: 400 }, () => arrivingStarPosition(width, 900, random).x);
    expect(Math.min(...xs)).toBeGreaterThanOrEqual(0);
    expect(Math.max(...xs)).toBeLessThanOrEqual(width);
    const inRightHalf = xs.filter((x) => x > width / 2).length;
    expect(inRightHalf).toBeGreaterThan(150);
    expect(inRightHalf).toBeLessThan(250);
  });
});
