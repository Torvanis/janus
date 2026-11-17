import { describe, expect, it } from 'vitest';
import { providerLabel } from './providerPresets';

describe('providerLabel', () => {
  it('names self-hosted engines instead of showing the adapter type', () => {
    expect(providerLabel('vlm', 'http://gpu1.example.com:8000/v1')).toBe('vLLM');
    expect(providerLabel('vlm')).toBe('vLLM');
    expect(providerLabel('llama_cpp', 'http://llama-host:8080/v1')).toBe('llama.cpp');
    expect(providerLabel('tei')).toBe('Hugging Face TEI');
  });

  it('names a cloud provider recognised by its base URL', () => {
    expect(providerLabel('openai_compatible', 'https://api.openai.com/v1')).toBe('OpenAI');
    expect(providerLabel('anthropic', 'https://api.anthropic.com/v1')).toBe('Anthropic');
  });

  it('falls back to a generic name for unknown OpenAI-compatible hosts and to the raw type when unknown', () => {
    expect(providerLabel('openai_compatible', 'https://proxy.internal.example/v1')).toBe('OpenAI-compatible');
    expect(providerLabel('openai_compatible')).toBe('OpenAI-compatible');
    expect(providerLabel('something_new')).toBe('something_new');
  });
});
