import { describe, expect, it } from 'vitest';
import { formatNanoUSD, lineKey, modelPriceType, nanoToInput, parseUSD, priceLineLabel, relevantBases } from './priceLines';

describe('price lines in words', () => {
  it('names the unit and the conditions', () => {
    expect(priceLineLabel('price_mchar_nanousd')).toBe('per 1M characters');
    expect(priceLineLabel('rate_in_nanousd@prompt_over=272000')).toBe('per 1M input tokens when prompt over 272,000 tokens');
    expect(priceLineLabel('price_video_second_nanousd@audio=yes,resolution=720p')).toBe(
      'per second of video when with sound, resolution 720p',
    );
    expect(priceLineLabel('price_image_nanousd@quality=low,resolution=1k')).toBe('per image when quality low, resolution 1k');
  });
  it('shows dollars with the decimals the price needs', () => {
    expect(formatNanoUSD(15e9)).toBe('$15.00');
    expect(formatNanoUSD(4_500_000)).toBe('$0.0045');
    expect(formatNanoUSD(1)).toBe('$0.000000001');
    expect(formatNanoUSD(0)).toBe('$0.00');
    expect(nanoToInput(2_500_000_000)).toBe('2.5');
    expect(nanoToInput(4_500_000)).toBe('0.0045');
    expect(nanoToInput(15e9)).toBe('15');
  });
});

describe('the builder rules', () => {
  it('reads dollars exactly and refuses negatives, junk, too many decimals and absurd prices', () => {
    expect(parseUSD('$2.50')).toEqual({ nano: 2_500_000_000, usd: 2.5 });
    expect(parseUSD('1,200')).toEqual({ nano: 1_200e9, usd: 1200 });
    expect(parseUSD('.000000001')).toEqual({ nano: 1, usd: 1e-9 });
    expect(parseUSD('0')).toEqual({ nano: 0, usd: 0 });
    expect(parseUSD('-1')).toEqual({ error: 'number' });
    expect(parseUSD('abc')).toEqual({ error: 'number' });
    expect(parseUSD(' ')).toEqual({ error: 'empty' });
    expect(parseUSD('0.0000000001')).toEqual({ error: 'decimals' });
    expect(parseUSD('10000.01')).toEqual({ error: 'max' });
  });
  it('builds canonical keys and reads the model type like the catalog does', () => {
    expect(lineKey('price_image_nanousd', { size: '1024x1024', quality: 'high', resolution: '' })).toBe(
      'price_image_nanousd@quality=high,size=1024x1024',
    );
    expect(lineKey('rate_in_nanousd', {})).toBe('rate_in_nanousd');
    expect(modelPriceType(['response', 'image'])).toBe('chat');
    expect(modelPriceType(['file', 'tts'])).toBe('tts');
    expect(relevantBases('video')).toEqual(['price_video_second_nanousd', 'price_request_nanousd']);
    expect(relevantBases('')).toContain('rate_out_nanousd');
  });
});
