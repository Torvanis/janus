/**
 * Provider presets for the add-upstream form. Choosing one sets the adapter
 * type and fills in the provider's documented base URL (and a suggested
 * name); both stay editable, so a proxy, a regional endpoint or a custom name
 * is one edit away. Self-hosted engines have no fixed address, so they only
 * suggest the engine's default port as a placeholder.
 *
 * Each URL is the base the provider documents for OpenAI-style clients. A
 * base that ends in a version path (/v1, /v1beta/openai) is the API root:
 * Janus appends /chat/completions or /models to it without another /v1.
 */
export interface ProviderPreset {
  id: string;
  label: string;
  adapterType: string;
  /** The provider's documented base URL, prefilled. Empty for self-hosted. */
  baseURL: string;
  /** Placeholder for the base URL field when nothing is prefilled. */
  placeholder?: string;
  /** Suggested upstream name. */
  name: string;
  group: 'cloud' | 'selfHosted' | 'other';
}

export const PROVIDER_PRESETS: ProviderPreset[] = [
  {
    id: 'openai',
    label: 'OpenAI',
    adapterType: 'openai_compatible',
    baseURL: 'https://api.openai.com/v1',
    name: 'OpenAI',
    group: 'cloud',
  },
  {
    id: 'anthropic',
    label: 'Anthropic',
    adapterType: 'anthropic',
    baseURL: 'https://api.anthropic.com/v1',
    name: 'Anthropic',
    group: 'cloud',
  },
  {
    id: 'gemini',
    label: 'Google Gemini (AI Studio key)',
    adapterType: 'gemini',
    baseURL: 'https://generativelanguage.googleapis.com/v1beta/openai',
    name: 'Google Gemini',
    group: 'cloud',
  },
  {
    id: 'vertex',
    label: 'Google Vertex AI (service account)',
    adapterType: 'vertex',
    // The project ID is the admin's own; suggest the shape, prefill nothing.
    baseURL: '',
    placeholder: 'https://us-central1-aiplatform.googleapis.com/v1/projects/PROJECT_ID/locations/us-central1',
    name: 'Google Vertex AI',
    group: 'cloud',
  },
  {
    id: 'bedrock',
    label: 'AWS Bedrock',
    adapterType: 'bedrock',
    baseURL: 'https://bedrock-runtime.us-east-1.amazonaws.com',
    name: 'AWS Bedrock',
    group: 'cloud',
  },
  {
    id: 'xai',
    label: 'xAI (Grok)',
    adapterType: 'openai_compatible',
    baseURL: 'https://api.x.ai/v1',
    name: 'xAI',
    group: 'cloud',
  },
  {
    id: 'mistral',
    label: 'Mistral',
    adapterType: 'openai_compatible',
    baseURL: 'https://api.mistral.ai/v1',
    name: 'Mistral',
    group: 'cloud',
  },
  {
    id: 'openrouter',
    label: 'OpenRouter',
    adapterType: 'openai_compatible',
    baseURL: 'https://openrouter.ai/api/v1',
    name: 'OpenRouter',
    group: 'cloud',
  },
  {
    id: 'groq',
    label: 'Groq',
    adapterType: 'openai_compatible',
    baseURL: 'https://api.groq.com/openai/v1',
    name: 'Groq',
    group: 'cloud',
  },
  {
    id: 'deepseek',
    label: 'DeepSeek',
    adapterType: 'openai_compatible',
    baseURL: 'https://api.deepseek.com/v1',
    name: 'DeepSeek',
    group: 'cloud',
  },
  {
    id: 'together',
    label: 'Together AI',
    adapterType: 'openai_compatible',
    baseURL: 'https://api.together.xyz/v1',
    name: 'Together AI',
    group: 'cloud',
  },
  {
    id: 'fireworks',
    label: 'Fireworks AI',
    adapterType: 'openai_compatible',
    baseURL: 'https://api.fireworks.ai/inference/v1',
    name: 'Fireworks AI',
    group: 'cloud',
  },
  {
    id: 'cerebras',
    label: 'Cerebras',
    adapterType: 'openai_compatible',
    baseURL: 'https://api.cerebras.ai/v1',
    name: 'Cerebras',
    group: 'cloud',
  },
  {
    id: 'moonshot',
    label: 'Moonshot AI (Kimi)',
    adapterType: 'openai_compatible',
    baseURL: 'https://api.moonshot.ai/v1',
    name: 'Moonshot AI',
    group: 'cloud',
  },
  {
    id: 'vlm',
    label: 'vLLM',
    adapterType: 'vlm',
    baseURL: '',
    placeholder: 'http://vllm-host:8000/v1',
    name: 'vLLM',
    group: 'selfHosted',
  },
  {
    id: 'llama_cpp',
    label: 'llama.cpp server',
    adapterType: 'llama_cpp',
    baseURL: '',
    placeholder: 'http://llama-host:8080/v1',
    name: 'llama.cpp',
    group: 'selfHosted',
  },
  {
    id: 'ollama',
    label: 'Ollama',
    adapterType: 'ollama',
    baseURL: '',
    placeholder: 'http://ollama-host:11434',
    name: 'Ollama',
    group: 'selfHosted',
  },
  {
    id: 'tei',
    label: 'Hugging Face TEI',
    adapterType: 'tei',
    baseURL: '',
    placeholder: 'http://tei-host:8080',
    name: 'TEI',
    group: 'selfHosted',
  },
  {
    id: 'openai_compatible',
    label: 'Other OpenAI-compatible endpoint',
    adapterType: 'openai_compatible',
    baseURL: '',
    placeholder: 'https://api.example.com/v1',
    name: '',
    group: 'other',
  },
];

/**
 * The presets this server can create: those whose adapter type the server
 * reports. An adapter type the server knows but no preset names is offered
 * under its own name, so a new adapter is never hidden by a stale list.
 */
export function availablePresets(adapterTypes: string[], titleCase: (s: string) => string): ProviderPreset[] {
  const known = new Set(adapterTypes);
  const out = PROVIDER_PRESETS.filter((p) => known.has(p.adapterType));
  for (const type of adapterTypes) {
    if (!PROVIDER_PRESETS.some((p) => p.adapterType === type)) {
      out.push({ id: type, label: titleCase(type), adapterType: type, baseURL: '', name: '', group: 'other' });
    }
  }
  return out;
}

/** The preset an existing upstream most likely came from, for display. */
export function presetForUpstream(adapterType: string, baseURL: string): ProviderPreset | undefined {
  let host = '';
  try {
    host = new URL(baseURL).hostname.toLowerCase();
  } catch {
    host = '';
  }
  const byHost = PROVIDER_PRESETS.find((p) => {
    if (p.adapterType !== adapterType || !p.baseURL) return false;
    try {
      return new URL(p.baseURL).hostname.toLowerCase() === host;
    } catch {
      return false;
    }
  });
  if (byHost) return byHost;
  if (adapterType === 'bedrock') return PROVIDER_PRESETS.find((p) => p.id === 'bedrock');
  return PROVIDER_PRESETS.find((p) => p.adapterType === adapterType && !p.baseURL);
}
