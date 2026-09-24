/**
 * Documentation content.
 *
 * The error catalog and glossary are the authoritative in-product reference —
 * every error surface in the app deep-links into `#<code>` anchors here, so
 * these entries must stay in step with internal/httpapi/errors.go.
 *
 * The changelog page is not authored here: its sections come from
 * changelog.generated.ts, which `web/scripts/gen-docs.mjs` derives from the
 * repository-root CHANGELOG.md at build time (drift-checked in CI).
 */
import { CHANGELOG } from './changelog.generated';

/**
 * How a pricing table is laid out. Each provider modality bills differently,
 * and the table shape follows the billing model rather than forcing every
 * rate into one grid:
 * - `tokens`: the classic per-1M-token grid (chat, embeddings).
 * - `voice`: text-to-speech — providers quote per minute or per 1M
 *   characters; one column carries the provider's own unit and the last
 *   column the per-1M-token equivalent to type into a Janus rate card.
 * - `image`: per-1M-token rates with Image/Text sub-rows under each model,
 *   plus tabs (Standard / Batch) when the provider prices tiers separately.
 * - `transcription`: per-minute rates with a use-case column and separate
 *   audio-input and text-output columns.
 * - `realtime`: per-1M-token rates with Audio/Text/Image sub-rows under
 *   each model.
 */
type PricingVariant = 'tokens' | 'voice' | 'image' | 'transcription' | 'realtime';

/** One line of a pricing table. `group` names the model a sub-row belongs to; `tag` is its modality pill. */
export interface PricingRow {
  cells: string[];
  /** Model this row belongs to; consecutive rows sharing a group render as one block. */
  group?: string;
  /** Modality/kind pill shown in the first cell of a sub-row (Image, Text, Audio, per minute…). */
  tag?: string;
  /** Emphasise the cell at this index (the figure an admin should copy into Janus). */
  highlight?: number;
}

interface PricingTab {
  label: string;
  rows: PricingRow[];
}

export interface PricingTable {
  variant: PricingVariant;
  caption: string;
  /** The unit every numeric cell is quoted in, shown under the caption. */
  unit: string;
  columns: string[];
  /** Either a single body of rows or several tabs (e.g. Standard / Batch). */
  rows?: PricingRow[];
  tabs?: PricingTab[];
  footnote?: string;
}

interface DocSection {
  heading: string;
  body: string[];
  code?: { language: string; code: string };
  pricing?: PricingTable;
}

export interface DocPage {
  slug: string;
  title: string;
  summary: string;
  group: 'Get started' | 'User guide' | 'Admin guide' | 'Reference';
  sections: DocSection[];
}

export interface ErrorEntry {
  code: string;
  status: number;
  when: string;
  action: string;
  retryable: boolean;
}

export const ERROR_CATALOG: ErrorEntry[] = [
  {
    code: 'policy.token_invalid',
    status: 401,
    when: 'The bearer token is absent, malformed, revoked, or belongs to a deleted account.',
    action: 'Create a new token on the Tokens page and update the tool that failed.',
    retryable: false,
  },
  {
    code: 'policy.user_disabled',
    status: 403,
    when: 'The account behind the token has been deactivated by an administrator.',
    action: 'Contact a Janus administrator. Tokens stay valid but every call is refused while the account is disabled.',
    retryable: false,
  },
  {
    code: 'policy.model_not_granted',
    status: 403,
    when: 'The caller asked for a model that is not enabled, or that they hold no grant for.',
    action: 'Call GET /v1/models to see what you can use, and ask an administrator for a grant if the model is missing.',
    retryable: false,
  },
  {
    code: 'policy.endpoint_blocked',
    status: 403,
    when: 'A policy blocking rule matched the request. The reason field names the rule and the clause that fired.',
    action: 'Read the reason. If the block is wrong, an administrator can adjust the rule in Admin → Policy rules.',
    retryable: false,
  },
  {
    code: 'policy.security_blocked',
    status: 403,
    when: 'A Security Gateway policy refused the request or withheld the response. error.param names the check kind (secrets, pii, terms, shape, prompt_injection); the rule and the matched text are never returned. On a stream this arrives as a final in-band error frame followed by [DONE]. If the message says the policy fails closed, the classifier behind a check was unreachable — nothing matched.',
    action:
      'Remove the flagged material and retry. If you believe this is a mistake, an administrator can look the request up by request_id under Admin → Security → Violations.',
    retryable: false,
  },
  {
    code: 'policy.quota_exceeded',
    status: 429,
    when: 'A tokens, spend, or request quota is used up for the current window.',
    action: 'Wait until reset_at, or ask an administrator to raise the limit. Retrying earlier will fail the same way.',
    retryable: true,
  },
  {
    code: 'policy.rate_limit',
    status: 429,
    when: 'A per-endpoint rate limit was exceeded.',
    action: 'Back off for the number of seconds in retry_after and try again.',
    retryable: true,
  },
  {
    code: 'upstream.unavailable',
    status: 503,
    when: 'The provider behind the requested model could not be reached, or its credentials could not be decrypted.',
    action: 'Retry shortly. If it persists, an administrator should check Admin → Settings → System status for that upstream.',
    retryable: true,
  },
  {
    code: 'upstream.rate_limit',
    status: 429,
    when: 'The provider throttled the gateway itself.',
    action: 'Back off and retry. Janus never retries generative calls for you, because a retry is a second charge.',
    retryable: true,
  },
  {
    code: 'invalid_request_error',
    status: 400,
    when: 'The request body was malformed, or a required field such as `model` was missing.',
    action: 'Read the param field, fix the request, and send it again.',
    retryable: false,
  },
  {
    code: 'authentication_error',
    status: 401,
    when: 'A browser request reached an API route without a valid session, or the session expired or idled out.',
    action: 'Sign in again. Sessions last 24 hours, or 4 hours of inactivity, whichever comes first.',
    retryable: false,
  },
  {
    code: 'permission_error',
    status: 403,
    when: 'The signed-in account lacks the role required for an administrative action.',
    action: 'Ask an administrator to perform the action or to grant you the role.',
    retryable: false,
  },
  {
    code: 'server_error',
    status: 500,
    when: 'The gateway hit an unexpected internal failure.',
    action: 'Retry once. If it recurs, quote the request_id from the response to whoever operates the gateway.',
    retryable: true,
  },
];

export const GLOSSARY: Array<{ term: string; definition: string }> = [
  {
    term: 'Adapter',
    definition:
      'The plug-in that translates between the OpenAI wire format and one provider’s protocol. Janus ships adapters for OpenAI-compatible endpoints, Anthropic, AWS Bedrock, Google Vertex, Ollama, vLLM, and llama.cpp.',
  },
  { term: 'Upstream', definition: 'A configured provider endpoint: a base URL, an adapter type, and an encrypted credential.' },
  {
    term: 'Model',
    definition:
      'One model discovered on an upstream. Discovered models stay disabled until an administrator approves and prices them.',
  },
  {
    term: 'Grant',
    definition:
      'Permission for a user, a group, or every authenticated user to call a model. Grants combine with OR — the most permissive match wins.',
  },
  {
    term: 'Rate card',
    definition:
      'The per-million-token prices used to attribute spend. Rate cards are versioned with an effective-from date so historical usage keeps its original price.',
  },
  {
    term: 'Modality',
    definition:
      'What kind of work a request performs: chat, embedding, image, audio, tts, stt, video, function, response, assistant, thread, file, moderation, or fine_tune.',
  },
  {
    term: 'Cached tokens',
    definition:
      'Input tokens the provider served from its own prompt cache. They are a subset of tokens_in and are billed at the cached rate.',
  },
  {
    term: 'Quota window',
    definition:
      'The period a limit is measured over. Calendar windows (day, week, month) snap to UTC boundaries; rolling windows slide continuously.',
  },
  {
    term: 'Breach behaviour',
    definition:
      'What happens when a limit is crossed mid-request. The default lets the in-flight request finish and refuses the next one; hard-kill cuts the stream immediately.',
  },
  {
    term: 'TTFB',
    definition: 'Time to first byte — how long the provider took to start responding, separate from total latency.',
  },
  {
    term: 'Policy rule',
    definition:
      'A composable block rule over client-supplied signals. A policy control, not a security control: every signal but the token can be spoofed.',
  },
];

export const PAGES: DocPage[] = [
  {
    slug: 'getting-started',
    title: 'Quick start',
    summary: 'From first sign-in to a metered request in about five minutes.',
    group: 'Get started',
    sections: [
      {
        heading: '1. Sign in',
        body: [
          'Open Janus. With corporate SSO configured, choose “Continue with corporate SSO”: your account is created automatically on first sign-in, and your group memberships are read from the identity token each time. Without SSO, sign in with the Janus account your administrator created for you (you will be asked to choose your own password the first time). On a brand-new gateway the first visit creates the administrator account.',
          'You do not need to be added to an allowlist to sign in. What you can *do* is controlled by model grants.',
        ],
      },
      {
        heading: '2. Create a token',
        body: [
          'Go to Tokens and choose “Create token”. Name it after the tool that will use it — that is what makes revoking a single integration safe later.',
          'The value is displayed once. Janus stores only a SHA-256 digest and genuinely cannot show it again.',
        ],
      },
      {
        heading: '3. Point your tool at the gateway',
        body: [
          'Janus speaks the OpenAI API. Set the base URL to your gateway and use your token as the API key. Everything else in your client stays the same.',
        ],
        code: {
          language: 'bash',
          code: `export JANUS_API_KEY="janus_…"

curl "$JANUS_BASE_URL/v1/chat/completions" \\
  -H "Authorization: Bearer $JANUS_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "MODEL_NAME",
    "messages": [{"role": "user", "content": "Hello from Janus"}]
  }'`,
        },
      },
      {
        heading: '4. Watch it land',
        body: [
          'Open your dashboard. The request appears within a few seconds with its token counts, latency, and attributed cost.',
          'If the call was refused, the error carries a stable code — look it up in the error catalog.',
        ],
      },
    ],
  },
  {
    slug: 'guides/tokens',
    title: 'Working with tokens',
    summary: 'Create, rotate, and revoke credentials safely.',
    group: 'User guide',
    sections: [
      {
        heading: 'One token per tool',
        body: [
          'A token identifies you, not a device. Issuing one per tool means you can revoke a single integration — a laptop that was lost, a CI runner that was retired — without breaking everything else you use.',
        ],
      },
      {
        heading: 'Rotating safely',
        body: [
          'Create the replacement first, update the tool, confirm traffic is flowing on the new token in the request log, and only then revoke the old one. Revocation takes effect immediately, with no grace period.',
        ],
      },
      {
        heading: 'What is stored',
        body: [
          'Janus stores a SHA-256 digest of the token plus its first characters for display. The value itself is never written to the database, the logs, or any metric.',
        ],
      },
    ],
  },
  {
    slug: 'guides/models',
    title: 'Choosing a model',
    summary: 'How the model list is filtered and priced.',
    group: 'User guide',
    sections: [
      {
        heading: 'You see what you are granted',
        body: [
          'GET /v1/models returns only models that are enabled and granted to you — directly, through one of your groups, or through an all-users grant. A user with no grants gets an empty list and cannot proxy anything.',
          'If a model you expect is missing, ask an administrator to grant it. Nothing about your account needs to change.',
        ],
      },
      {
        heading: 'Model names can be curated',
        body: [
          'Administrators can edit a model’s display name, context window and prices in Admin → Models → Edit. The catalog then lists the model under that name, and it becomes the name to use in the model field of your requests; the raw upstream identifier is reported alongside it as janus.native_name. Use the model name shown in the catalog — not the provider\u2019s own catalog — when configuring a tool.',
          'Renames are backward compatible: the original upstream name keeps resolving forever, so a tool configured before a rename never breaks. One limitation: a request that carries the display name in a body the gateway cannot rewrite (file uploads and other non-JSON payloads) is refused with an error naming the native model name to use instead.',
        ],
      },
      {
        heading: 'Prices are per million tokens',
        body: [
          'Every model carries up to five rates: input, output, cached input, and — for providers that bill prompt-cache writes, such as Anthropic — 5-minute and 1-hour cache-write rates. Cached input tokens are a subset of the input count and are billed at the cached rate; cache-write tokens are metered separately and billed at the matching cache-write rate.',
          'A model cannot be enabled without a rate card. Automatic metadata can supply supported prices; unknown prices are distinct from explicitly free prices. An explicitly saved $0 rate card counts: self-hosted models can be enabled free of charge, and their usage is attributed at $0.00.',
        ],
      },
      {
        heading: 'Context windows',
        body: [
          'Each model in the catalog shows its context window — the maximum number of tokens it accepts as input, reported as context_window (in tokens) in the API. Discovery refreshes it from the upstream, then an exact reference for that provider. In Admin → Models → Edit, administrators can override context and each price independently, or choose Use automatic to reset one field.',
          'A dash means the context window is not recorded (context_window is 0), not that the model has none. Janus displays the value for information only — it does not reject or truncate requests based on it.',
        ],
      },
      {
        heading: 'Health and error rate',
        body: [
          'Each model card on the Models page carries live health. The badge mirrors what administrators see under Admin → Upstreams: Healthy with the last probe latency when the provider answered, Down when the last probe failed (the card is tinted red and names the error), Degraded when at least 10% of recent requests failed, and No data when the upstream has not been probed and nothing has been requested yet.',
          'Errors (10 min) is the share of requests to that model over the trailing ten minutes that failed on the provider side — a 5xx response or the provider being unreachable or rate-limiting the gateway. Requests Janus refused on policy grounds (quota, grant, blocking rule) are not counted on either side of the ratio, so your own quota does not make a model look unhealthy. The API reports the same figures on GET /api/v1/models as request_count_10m, error_count_10m, error_rate_percent, upstream_reachable, upstream_last_latency_ms, and health.',
          'A model marked Down will fail requests until the upstream recovers; if it stays down, contact an administrator rather than retrying in a loop.',
        ],
      },
    ],
  },
  {
    slug: 'guides/quotas',
    title: 'Quotas and limits',
    summary: 'What a quota measures, when it resets, and what happens on breach.',
    group: 'User guide',
    sections: [
      {
        heading: 'What is measured',
        body: [
          'A quota caps one of: input tokens, output tokens, spend, or request count. It applies to a person or a team, optionally narrowed to a single model.',
          'When several quotas apply at once they combine with AND — the most restrictive one decides.',
        ],
      },
      {
        heading: 'Windows',
        body: [
          'Calendar windows (per day, per week, per month) reset on UTC boundaries. Rolling windows (24 hours, 7 days, 30 days) slide continuously, so the oldest usage drops out as time passes.',
        ],
      },
      {
        heading: 'On breach',
        body: [
          'You are warned at 80% and again at 95% by default; an administrator can configure different warning percentages per quota rule. When the limit is reached, the next request is refused with HTTP 429 and a reset_at timestamp.',
          'By default a request already in flight is allowed to finish, and the overage is recorded — which is why administrators are advised to set limits with headroom. A quota can instead be configured to cut streams immediately.',
        ],
        code: {
          language: 'json',
          code: `{
  "error": {
    "message": "Your per day quota of 1000000 input tokens has been used up. Access resumes at 2026-08-16T00:00:00Z.",
    "code": "policy.quota_exceeded",
    "type": "rate_limit_error",
    "reset_at": "2026-08-16T00:00:00Z",
    "retry_after": 34200
  }
}`,
        },
      },
    ],
  },
  {
    slug: 'guides/streaming',
    title: 'Streaming responses',
    summary: 'How Janus relays Server-Sent Events and still counts tokens.',
    group: 'User guide',
    sections: [
      {
        heading: 'Transparent relay',
        body: [
          'Set "stream": true exactly as you would against the provider. Janus relays each event as it arrives, without buffering, so time-to-first-token is unchanged apart from the gateway’s own overhead.',
          'Token counts are read from the final frame. For OpenAI-compatible upstreams Janus adds stream_options.include_usage so the counts are present — this is the only edit it makes to a request body, and it is additive.',
        ],
      },
      {
        heading: 'Quotas during a stream',
        body: [
          'By default a stream that crosses a limit mid-flight is allowed to finish and the next request is refused. If an administrator set the quota to hard-kill, the stream is cut as soon as the limit is crossed.',
        ],
      },
    ],
  },
  {
    slug: 'admin/upstreams',
    title: 'Configuring upstreams',
    summary: 'Add a provider, discover its models, and price them.',
    group: 'Admin guide',
    sections: [
      {
        heading: 'Before you start',
        body: [
          'You need the provider base URL and a credential. Credentials are encrypted with AES-256-GCM using JANUS_ENCRYPTION_KEY before they touch the database, and only the first and last four characters are ever displayed again.',
          'AWS Bedrock and Google Vertex are paid cloud services: you supply your own account. Bedrock credentials are stored as ACCESS_KEY_ID:SECRET_ACCESS_KEY; Vertex takes the service-account JSON key.',
        ],
      },
      {
        heading: 'Procedure',
        body: [
          '1. Admin → Upstreams → Add upstream. Choose the provider type: it is immutable afterwards, because it determines how every stored model is addressed.',
          '2. Save. Janus immediately runs discovery and records what the provider reports.',
          '3. Admin → Models. New models arrive as “pending approval” and disabled.',
          '4. Open Edit to review automatic metadata and pricing warnings, set any overrides, then enable. Unknown prices require review; unsupported tiers and per-image rates are never flattened into token prices.',
          '5. Admin → Grants. Grant the model to a user, a group, or all users.',
        ],
      },
      {
        heading: 'Pricing from the bundled reference seed',
        body: [
          'Rate cards are always per million tokens; see “Pricing by modality” for how speech, transcription, image and realtime models — which providers quote per minute, per character or per tier — translate into that unit. Janus ships a reference rate-card seed for common OpenAI, Anthropic, and Mistral models so a fresh install does not need every price typed by hand. GET /api/v1/admin/ratecards/reference lists it; POST /api/v1/admin/ratecards/apply prices every discovered model that matches the seed by name and has no rate card yet. Models you already priced are never touched, and re-applying is a no-op.',
          'The seed reflects public list prices at the time it was authored — it is a starting point, not a live price feed. Verify against the provider’s current pricing before relying on cost attribution, and adjust any figure per model afterwards.',
          'The seed also carries each model’s context window (context_window_tokens). Applying it fills in the context window for every matching model that does not have one yet — including models you already priced by hand — but never overwrites a value that is already set.',
        ],
        code: {
          language: 'bash',
          code: 'curl -X POST ${JANUS_PUBLIC_URL}/api/v1/admin/ratecards/apply \\\n  -H "Cookie: janus_session=…" -H "X-Janus-CSRF: …"',
        },
      },
      {
        heading: 'Verification',
        body: [
          'Admin → Settings → System status shows each upstream’s last check, latency, and last error. A user should now see the model in GET /v1/models within seconds.',
        ],
      },
      {
        heading: 'Rollback',
        body: [
          'Disabling an upstream stops all traffic through it and disables its models, while retaining every historical usage event that references it.',
          'Deleting an upstream first shows what depends on it: the models it hosts (all disabled by the delete), the managed models whose target lives there (they keep resolving by name but can no longer be served unless they have a fallback), and the direct grants on those models. While an enabled managed model still targets the upstream the delete is blocked — repoint or disable the alias on the Managed models page, or tick the explicit “delete anyway” acknowledgement (DELETE …?force=true) and repoint afterwards. An opt-in also removes the direct grants on the upstream’s models (?purge_grants=true) rather than leaving them attached to models nobody can call.',
        ],
      },
    ],
  },
  {
    slug: 'admin/quotas',
    title: 'Setting quotas',
    summary: 'Choose a metric, a window, and a breach behaviour.',
    group: 'Admin guide',
    sections: [
      {
        heading: 'Set limits with headroom',
        body: [
          'The default breach behaviour lets an in-flight request finish, so a single large streaming completion can overshoot the limit. Choose a limit that tolerates one maximum-size request beyond it.',
          'Hard-kill removes the overshoot but cuts users off mid-answer. Reach for it only when a hard ceiling matters more than the experience.',
        ],
      },
      {
        heading: 'Team quotas',
        body: [
          'A team quota measures every member’s usage together. Combined with a per-user quota it produces a shared budget with an individual cap inside it.',
        ],
      },
      {
        heading: 'Delegating to team leads',
        body: [
          'Each team has a “lead can edit quotas” toggle. When an administrator switches it on, that team’s lead gets a quota-management panel on their team page and can create, adjust, and delete quotas for that team only — never for other teams, individuals, or global settings.',
          'Every change a lead makes is audit-logged with their identity, exactly like an admin edit. Switch the toggle off to withdraw the delegation at any time; existing quotas keep enforcing either way.',
        ],
      },
    ],
  },
  {
    slug: 'admin/pricing',
    title: 'Pricing by modality',
    summary: 'Every rate card is per million tokens — here is how each kind of model actually bills, and what to type in.',
    group: 'Admin guide',
    sections: [
      {
        heading: 'One rate card, many billing models',
        body: [
          'Janus prices every request from its rate card in USD per million tokens, using the token counts the upstream reports. That is the native unit for chat and embeddings, but providers quote speech, transcription, image and realtime models in their own units — per minute, per character, per image tier. The tables below show each modality in the shape its provider publishes, with the per-1M-token figure to enter in Janus highlighted.',
          'Figures are illustrative public list prices at the time of writing; check the provider’s pricing page before relying on attribution. A rate card with the wrong denominator is the most common cause of a bill that does not reconcile.',
        ],
      },
      {
        heading: 'Chat and embeddings',
        body: [
          'Text models are the straightforward case: the provider bills the same units the rate card uses. Enter input, output and cached-input rates as published; leave cache-write rates empty unless the provider bills them (Anthropic does).',
        ],
        pricing: {
          variant: 'tokens',
          caption: 'Text models',
          unit: 'USD per 1M tokens',
          columns: ['Model', 'Input', 'Cached input', 'Output'],
          rows: [
            { cells: ['gpt-4o', '2.50', '1.25', '10.00'] },
            { cells: ['gpt-4o-mini', '0.15', '0.075', '0.60'] },
            { cells: ['claude-sonnet-4', '3.00', '0.30', '15.00'] },
            { cells: ['text-embedding-3-small', '0.02', '—', '—'] },
          ],
        },
      },
      {
        heading: 'Voice (text-to-speech)',
        body: [
          'Speech models are quoted two ways. Token-native models (gpt-4o-mini-tts) bill text input and audio output per million tokens, which the provider also expresses as an approximate per-minute cost. Character-billed models (tts-1, tts-1-hd) are quoted per million characters; since the upstream reports characters in the usage block’s input count, the per-1M-character rate is entered as the input rate and output is left at zero.',
        ],
        pricing: {
          variant: 'voice',
          caption: 'Text-to-speech',
          unit: 'USD, in the unit the provider quotes; the last column is what to enter, per 1M tokens',
          columns: ['Model', 'Billed', 'Provider unit', 'Rate', 'Janus rate card'],
          rows: [
            {
              group: 'gpt-4o-mini-tts',
              tag: 'Text',
              cells: ['gpt-4o-mini-tts', 'Text input', 'per 1M tokens', '0.60', '0.60 in'],
              highlight: 4,
            },
            {
              group: 'gpt-4o-mini-tts',
              tag: 'Audio',
              cells: ['', 'Audio output', 'per 1M tokens (≈ 0.015 / min)', '12.00', '12.00 out'],
              highlight: 4,
            },
            {
              group: 'tts-1',
              tag: 'Chars',
              cells: ['tts-1', 'Input text', 'per 1M characters', '15.00', '15.00 in · 0 out'],
              highlight: 4,
            },
            {
              group: 'tts-1-hd',
              tag: 'Chars',
              cells: ['tts-1-hd', 'Input text', 'per 1M characters', '30.00', '30.00 in · 0 out'],
              highlight: 4,
            },
          ],
          footnote:
            'Audio-token models: roughly 1 minute of speech ≈ 1,250 audio tokens, so a per-minute quote × 800 gives the per-1M-token rate.',
        },
      },
      {
        heading: 'Image generation',
        body: [
          'Image models bill several modalities on one request: text input (the prompt), image input (edits and references) and image output. Each is its own per-1M-token rate, so a model appears as a block with one sub-row per modality. Batch pricing is a separate tier at half price — use the tab that matches how your callers submit work. Janus prices text and image input together as input tokens and image output as output tokens; when a provider reports the request cost directly, that figure is used instead (accounting method upstream_reported_cost).',
        ],
        pricing: {
          variant: 'image',
          caption: 'Image generation',
          unit: 'USD per 1M tokens',
          columns: ['Model / modality', 'Input', 'Cached input', 'Output'],
          tabs: [
            {
              label: 'Standard',
              rows: [
                { group: 'gpt-image-1', tag: 'Text', cells: ['gpt-image-1', '5.00', '1.25', '—'] },
                { group: 'gpt-image-1', tag: 'Image', cells: ['', '10.00', '2.50', '40.00'], highlight: 3 },
                { group: 'gpt-image-1-mini', tag: 'Text', cells: ['gpt-image-1-mini', '2.00', '0.20', '—'] },
                { group: 'gpt-image-1-mini', tag: 'Image', cells: ['', '2.50', '0.25', '8.00'], highlight: 3 },
              ],
            },
            {
              label: 'Batch',
              rows: [
                { group: 'gpt-image-1', tag: 'Text', cells: ['gpt-image-1', '2.50', '—', '—'] },
                { group: 'gpt-image-1', tag: 'Image', cells: ['', '5.00', '—', '20.00'], highlight: 3 },
                { group: 'gpt-image-1-mini', tag: 'Text', cells: ['gpt-image-1-mini', '1.00', '—', '—'] },
                { group: 'gpt-image-1-mini', tag: 'Image', cells: ['', '1.25', '—', '4.00'], highlight: 3 },
              ],
            },
          ],
          footnote:
            'Per-image list prices (e.g. 0.04 for a 1024×1024 medium-quality image) are what these token rates work out to.',
        },
      },
      {
        heading: 'Transcription (speech-to-text)',
        body: [
          'Transcription is quoted per minute of audio. Token-native models (gpt-4o-transcribe) also publish per-1M-token rates for audio input and text output, which map directly onto a Janus rate card. Minute-billed models (whisper-1) report no token usage; Janus records those requests as unmetered_modality unless the upstream returns a cost, so attribution for them depends on the provider’s own invoice.',
        ],
        pricing: {
          variant: 'transcription',
          caption: 'Speech-to-text',
          unit: 'USD; per-minute is the provider’s estimate, audio-in and text-out are per 1M tokens',
          columns: ['Model', 'Use case', 'Per minute', 'Audio in', 'Text out'],
          rows: [
            { cells: ['gpt-4o-transcribe', 'Accuracy', '0.006', '2.50', '10.00'], highlight: 3 },
            { cells: ['gpt-4o-mini-transcribe', 'Cost', '0.003', '1.25', '5.00'], highlight: 3 },
            { cells: ['whisper-1', 'Per-minute only', '0.006', '—', '—'], highlight: 2 },
          ],
        },
      },
      {
        heading: 'Realtime (speech-to-speech)',
        body: [
          'Realtime sessions stream audio in and out while also exchanging text, so a single model publishes a rate per modality: audio, text and — on newer models — image input. Audio tokens are far more expensive than text tokens for the same model, which is why the sub-rows matter. Janus sums each modality’s tokens as reported by the session’s usage events and prices them at the rate card; enter the audio rates unless your callers are text-only.',
        ],
        pricing: {
          variant: 'realtime',
          caption: 'Realtime API',
          unit: 'USD per 1M tokens',
          columns: ['Model / modality', 'Input', 'Cached input', 'Output'],
          rows: [
            { group: 'gpt-realtime', tag: 'Text', cells: ['gpt-realtime', '4.00', '0.40', '16.00'] },
            { group: 'gpt-realtime', tag: 'Audio', cells: ['', '32.00', '0.40', '64.00'], highlight: 1 },
            { group: 'gpt-realtime', tag: 'Image', cells: ['', '5.00', '0.50', '—'] },
            { group: 'gpt-realtime-mini', tag: 'Text', cells: ['gpt-realtime-mini', '0.60', '0.06', '2.40'] },
            { group: 'gpt-realtime-mini', tag: 'Audio', cells: ['', '10.00', '0.30', '20.00'], highlight: 1 },
          ],
        },
      },
    ],
  },
  {
    slug: 'admin/rules',
    title: 'Policy blocking rules',
    summary: 'Steer well-behaved clients — and understand what these rules cannot do.',
    group: 'Admin guide',
    sections: [
      {
        heading: 'What they are',
        body: [
          'A rule is a set of clauses over eight signals: user agent, source IP, X-Forwarded-For chain, endpoint path, HTTP method, a named header, the token prefix, and the model name. Clauses combine with AND or OR, and any clause can be negated to build an allowlist.',
          'Rules are evaluated on ingress, before grants and quotas, so a blocked request never reaches a provider and never costs anything.',
        ],
      },
      {
        heading: 'What they are not',
        body: [
          'Every signal except the bearer token is supplied by the client and can be forged. A rule that blocks a user agent stops a tool that identifies itself honestly; it does not stop someone who changes the string.',
          'Use rules to keep sanctioned tooling on the rails. Do not treat them as a containment boundary.',
        ],
      },
      {
        heading: 'Test before you ship',
        body: [
          'Admin → Policy rules includes a tester: paste a sample request and see which rule and which clause would fire, before any real traffic is affected.',
        ],
      },
    ],
  },
  {
    slug: 'admin/security',
    title: 'Security Gateway',
    summary: 'Policy-driven filtering of prompts and responses: secrets, PII, codenames, request shape, and prompt injection.',
    group: 'Admin guide',
    sections: [
      {
        heading: 'What it is — and what it is not',
        body: [
          'The Security Gateway evaluates every proxied request and response against policies you define. It can observe, redact, or block. Every match is recorded with the request id, the caller, the model, the rule that fired, and the action taken, so the outcome is attributable and reportable.',
          'It is an accident-prevention and audit control, not a security boundary. A determined insider can rephrase, encode, or split material across turns to get past any filter. What the gateway promises is that every attempt is policy-evaluated, blocked where configured, logged and attributable — which is what compliance actually buys.',
          'Two tiers exist. The deterministic tier (secrets, PII, term lists, request shape) needs no model, no GPU and no licence, runs in the gateway process in well under a millisecond, and produces span-level evidence an auditor accepts. The classifier tier (prompt injection in this version) needs a guard model and adds latency; it is the only way to catch things defined by intent rather than structure.',
        ],
      },
      {
        heading: 'Policies and bindings',
        body: [
          'A policy is a named bundle of checks. Each check has a kind, a mode (observe / redact / block), a direction (ingress, egress, or both), and for model-backed checks a fail stance (closed or open) and the classifier that backs it.',
          'A policy does nothing until it is bound to a scope: Everyone, a group, a service token, a model, or a managed model. One binding per scope. Resolution is most-specific-wins: Everyone → group → model → managed model → service token.',
          'A policy bound to Everyone may be marked mandatory. Its checks then become a floor: a more specific binding may add checks or tighten a mode (observe → redact → block), fail stance (open → closed), direction, or hold window — never relax or remove. Admin → Security → Bindings → "Explain effective policy" shows the resolved checks for any caller and model, and names every relaxation that was refused. Precedence you cannot see is precedence you cannot audit.',
          'Bindings are deliberately not part of grants. Grants answer "may this caller use this model"; policies answer "what rules apply to this traffic". Keeping them apart is what makes "block secrets org-wide" one binding instead of one edit per model per group — and is what stops a service token from being a bypass.',
        ],
      },
      {
        heading: 'Rollout: observe first',
        body: [
          'Every check defaults to observe. In observe mode nothing is blocked or rewritten; matches are recorded exactly as they would be in block mode. Run a new policy in observe for long enough to see what it would have done to real traffic, review the Violations tab, then switch individual checks to redact or block.',
          'Use Admin → Security → Policies → Test to dry-run a sample request against a draft policy before binding it. A dry run makes no upstream call, writes no usage event and records no violation.',
        ],
      },
      {
        heading: 'The checks',
        body: [
          'Secrets: gitleaks-style rules for API keys, tokens, private keys, JWTs, and credentials embedded in connection strings. Structured rules (AWS, GitHub, Slack, Stripe, private-key blocks, …) are enabled by default; the generic high-entropy rule is off because it fires on base64 images, hashes and tool-call payloads in prose. Patterns are Go RE2 — linear time, so an administrator-authored rule can never take the proxy down.',
          'PII: payment cards (Luhn-checked), IBANs (mod-97), US Social Security numbers (hyphenated by default, structural checks), NPIs, e-mail addresses and phone numbers. Redact rather than block for most of these: blocking "summarise this complaint" over an e-mail address is the false positive that gets the feature switched off.',
          'Term lists: your own words — project codenames, client names, classification markings. Modes: exact (whole words), substring, regex, or fuzzy (catches Pr0j3ct H4lb3rd and confusable characters). Lists are encrypted at rest and never shown in listings; viewing or exporting a list is written to the audit log. Import a text or CSV file rather than typing four hundred names into a form.',
          'Shape: limits on message count, body size, image parts and tools, and a rule that refuses system-role messages from service tokens.',
          'Prompt injection: a text-classification guard model (Llama Prompt Guard 2 is the reference) scores every message in overlapping windows. Unlike harmful-content filtering, nothing upstream protects you from injection — it targets the application, not the model.',
        ],
      },
      {
        heading: 'What a block looks like',
        body: [
          'By default a blocked request receives HTTP 403 with an OpenAI-shaped error, code policy.security_blocked, and the check kind in error.param. The rule id and the matched text are never returned to the caller.',
          'A policy may instead answer with a synthetic refusal: HTTP 200 and a well-formed completion whose finish_reason is content_filter, for clients that render errors badly. Streaming callers receive it as one chunk followed by [DONE]. The usage event still records the block either way.',
          'A redacted request sets X-Janus-Redactions on the response. The upstream saw different text than the user sent; that must never be silent.',
        ],
      },
      {
        heading: 'Responses and streaming',
        body: [
          "Checks with direction egress or both also scan the model's output — message content, reasoning content, and tool-call arguments. On a buffered response a block replaces the body with the error shape; a redaction rewrites the spans in place.",
          'On a stream the gateway withholds a small trailing window of text (hold_bytes, default 256) so a match can be caught before the bytes leave. Frames are released once their text is older than the window and scanned clean. A blocking match drops everything still held, emits a final in-band error frame and [DONE], and records stream_cut on the usage event. Text already flushed cannot be recalled: the window is the entire protection, and 256 bytes is enough for every deterministic rule at a latency no reader can perceive.',
          'Tokens the upstream generated before a cut are metered normally. The spend happened; hiding it would misstate what the filter costs.',
        ],
      },
      {
        heading: 'What is recorded — capture the attack, never the asset',
        body: [
          'Every violation row carries the request id, caller, model, policy, binding, check kind, rule id, action, offset/length and a hash of the matched span. What else is kept depends on the kind, and the rule is fixed rather than configurable:',
          'Secrets: hash only. Storing the match would move a live credential into a second at-rest location. Term lists: hash only — the log must not leak the codename it protects. PII: the redaction marker, never the value; a security log must not become a new regulated data store. Prompt injection: the full matched message, encrypted — the attack text is the intelligence, and it is what tunes the classifier.',
          'Reading captured text (Violations → Reveal) is itself written to the audit log. Retention is per kind: prompt-injection captures 90 days / 50k rows, everything else 30 days / 100k rows. Nothing captured ever leaves the gateway.',
        ],
      },
      {
        heading: 'Classifier models',
        body: [
          "Add the guard model's server as an upstream of type TEI (Hugging Face Text Embeddings Inference, the standard host for Llama Prompt Guard 2 — an encoder model that generative engines such as vLLM cannot load). Discovery reads the server's /info, finds the one model it serves, and assigns the classifier role automatically; confirm it under Admin → Security → Classifiers before referencing it in a policy.",
          'A model with a classifier role is removed from /v1/models, cannot be granted, cannot be the target of a managed model, and is called only by the gateway. A caller who could invoke the judge directly would have free, unlimited oracle access to the thing judging them.',
          "Every classifier call is metered as a usage event on a platform-overhead subject with no user or service token attached: finance can see what the security feature costs and no user's quota moves because of it. The classifier is wrapped in a circuit breaker: three consecutive failures open it for thirty seconds and raise a critical admin alert. While it is open, checks with fail=closed refuse requests and say so in the error message; checks with fail=open pass and log. Choose fail=closed for security; choose fail=open only where availability matters more, and pair it with the alert.",
          'Llama Prompt Guard 2 is distributed under the Llama 4 Community License: the download is gated, "Built with Llama" attribution applies, and any fine-tune must carry a name beginning with "Llama". If those terms are unacceptable to your organisation, any model served in the same text-classification shape can back the check instead.',
        ],
      },
      {
        heading: 'Content safety (Llama Guard)',
        body: [
          'Content safety is a second, separate check backed by a different kind of classifier. Llama Guard is a generative model served on a chat engine such as vLLM: add that server as an ordinary OpenAI-compatible upstream, let discovery find the model, and mark it as a generative guard under Admin → Security → Classifiers. The two roles are not interchangeable — a Prompt Guard encoder cannot judge hazard categories and a Llama Guard cannot score injection — so a policy that binds the wrong role is refused when saved, with the reason.',
          'The guard judges each conversation turn against the MLCommons hazard taxonomy (S1 Violent Crimes through S14 Code Interpreter Abuse) and answers safe or unsafe with the categories that apply. The policy names which categories to enforce. A verdict blocks only when it names a selected category; anything the guard flags outside the selection is still recorded as observed, so you can see what a category would catch before enforcing it. An empty selection enforces every category. Under a mandatory policy the selection is a floor: a more specific binding may add categories, never remove one.',
          'On requests the check can block. On responses, in this version, it observes and logs only: the guard judges the answer in the context of the prompt it replies to, and the verdict lands in Violations with its categories, but the response is never withheld. A generative verdict cannot be produced inside a streaming hold-back window, and blocking buffered responses while streamed ones pass would be a control that only works when the caller happens not to stream. The editor refuses block on egress for that reason. Streamed responses are not judged at all.',
        ],
      },
      {
        heading: 'Limitations to state plainly',
        body: [
          "Text only: images, audio and file attachments are metered but not scanned. Per-message: material split across turns of a conversation is not correlated. Non-English and encoded text (base64, leetspeak beyond the fuzzy term folding) materially reduce catch rates for the classifier. Streaming classifier-backed egress checks are not in this version: a content classifier needs a sentence or two, by which time that text is on the user's screen.",
        ],
      },
    ],
  },
  {
    slug: 'admin/accounts',
    title: 'Accounts and sign-in',
    summary: 'Janus accounts (email + password + two-factor), SSO, first-run setup and break-glass.',
    group: 'Admin guide',
    sections: [
      {
        heading: 'Two ways in',
        body: [
          'Janus accepts sign-in from a corporate identity provider (OIDC — Entra ID, Okta, Google, Authentik, Keycloak…) and from Janus accounts held by the gateway itself: email, password and an optional authenticator-app second factor. Both create the same kind of user, so seats, roles, teams, grants and audit are identical.',
          'Without an identity provider configured, the very first visit to a new gateway shows “Create the first administrator”. That endpoint disappears once any account exists; every later account is created by an administrator under Admin → People → Add user.',
        ],
      },
      {
        heading: 'Passwords and lockout',
        body: [
          'Passwords are 12–128 characters with no composition rules (a sentence is a good password) and are stored as bcrypt hashes. Ten wrong attempts lock the account for 15 minutes; wrong email and wrong password give the same answer so the sign-in form cannot be used to discover accounts. Changing a password signs out every other session.',
          'Administrators create accounts and reset passwords with a temporary password the person must replace at their next sign-in. Administrators never see or set a permanent password.',
        ],
      },
      {
        heading: 'Two-factor authentication',
        body: [
          'Personal settings → Security → Set up shows a QR code for any TOTP app. Enrollment completes only after a correct code, and issues ten single-use recovery codes shown once. Secrets are encrypted at rest with the gateway key. An administrator can remove a user\u2019s second factor from Admin → People when a device is lost.',
        ],
      },
      {
        heading: 'Several identity providers (Business)',
        body: [
          'Admin → Provisioning → Identity providers adds OIDC providers beside the environment-configured one: a contractor tenant in Entra ID next to the corporate Okta, or Google Workspace for a subsidiary. Each provider has a slug that becomes its callback URL (`/auth/callback/<slug>`) — register that URL at the provider — and its own sign-in button.',
          'Test checks discovery before you save. The client secret is encrypted at rest and never shown again. ID tokens must be signed with RS256/384/512 or ES256/384/512 — Authentik in particular defaults new providers to HS256 until a signing key is selected; Janus refuses HS256 with an explicit message. Per-provider admin groups are re-evaluated at every sign-in and combine with JANUS_ADMIN_GROUPS. Subjects are namespaced by slug, so two providers can never collide on the same user id. Deleting a provider keeps the accounts it created; those people need another way in.',
        ],
      },
      {
        heading: 'Directory sign-in — LDAP and Active Directory (Business)',
        body: [
          'Admin → Provisioning → Directory connects one LDAP directory. People type their directory email and password into the same sign-in form Janus accounts use; Janus looks the person up with the service account, binds as them to prove the password, reads their groups, and creates the account on first sign-in. Admin groups are re-evaluated at every sign-in and combine with JANUS_ADMIN_GROUPS.',
          'Use ldaps:// wherever possible. The service-account password is encrypted at rest and never shown again. The user filter receives the typed login (escaped, so nothing a user types can widen the search); the Active Directory default matches mail, UPN or sAMAccountName. Test resolves a login without its password and shows the DN, groups and whether that person would be an administrator — check this before enabling.',
          'A wrong directory password answers exactly like a wrong Janus-account password. If the directory is unreachable the form says so (HTTP 503) instead of pretending the password was wrong, and Janus-account passwords keep working — an admin with a break-glass password can always get in. Local accounts are checked first, so a person can hold both.',
        ],
      },
      {
        heading: 'Break-glass',
        body: [
          'An SSO user — typically an administrator — can add a password under Personal settings → Security. When the identity provider is unreachable the sign-in page still offers the password form, so the gateway can be administered during an IdP outage. Protect such accounts with two-factor authentication.',
        ],
      },
      {
        heading: 'Evaluation sign-in',
        body: [
          'JANUS_DEV_AUTH=true (any email signs in, no password) remains for laptops and demos only and is refused in production. With local accounts available there is no longer a reason to run it on a shared gateway.',
        ],
      },
    ],
  },
  {
    slug: 'admin/licensing',
    title: 'Licensing',
    summary: 'Editions, seats, what happens when a key expires, and how to install one.',
    group: 'Admin guide',
    sections: [
      {
        heading: 'Editions',
        body: [
          'Janus Edge runs without a key as the Community edition: 25 active users, one node, guardrails observe-only, one identity provider. Business keys add seats, high availability, SCIM, LDAP, multiple identity providers, guardrail enforcement, scheduled reports, troubleshooting captures, audit export, model fallbacks and email alerting. Enterprise keys are issued per agreement and may be perpetual or air-gapped.',
          'A key is a signed JANUS-LICENSE-1 string verified against public keys embedded in the binary. Nothing phones home: an air-gapped gateway is fully licensed with just the file.',
        ],
      },
      {
        heading: 'What a Business key unlocks',
        body: [
          'Every Business feature is a control that is already in the product; Community shows it greyed out with a one-line reason, and the API answers 402 feature_not_licensed if it is called anyway. Reads are never gated, and anything configured before a downgrade keeps running — only the enabling write is refused.',
          'Guardrails enforce: redact and block modes on security policies (observe is Community). Request capture: turning troubleshooting recording on. Email alerts: the email channel on an alert (in-app and webhook are Community). Model fallbacks: a fallback model on a managed model. Audit export: the CSV download on the audit log. Scheduled reports, SCIM provisioning, multiple identity providers, LDAP directory sign-in and more than one node are described on their own pages.',
        ],
      },
      {
        heading: 'Seats',
        body: [
          'A seat is an active user: anyone who signed in or made a request in the last 30 days. Disabled accounts do not hold a seat. When every seat is used, a new person cannot complete their first sign-in; everyone already active is unaffected, and administrators can always sign in.',
          'Business seats are bought as a pool at janusedge.com and split into one key per gateway or site from the portal; each gateway enforces only its own key.',
        ],
      },
      {
        heading: 'Expiry never disrupts work',
        body: [
          'Thirty days before expiry the shell shows a notice. After expiry there is a grace period (normally 30 days) in which nothing changes. After the grace period the gateway is restricted: everything already configured keeps running — proxying, SSO and SCIM sync, guardrails, quotas, reports, HA — but creating new items is paused: tokens, upstreams, managed models, quotas, rate limits, policy rules, teams, groups, grants, security policies, alerts, report schedules and SCIM tokens. Edits and deletes still work. The API returns 402 with code license.required.',
          'Enabling a feature the edition does not include returns 402 with code license.feature_not_licensed. A feature that is already on keeps working when the key lapses.',
        ],
      },
      {
        heading: 'Installing a key',
        body: [
          'Admin → Settings → License & updates: paste the key exactly as downloaded from the portal and choose Install. The key is verified before it is stored and takes effect immediately on this node; other nodes pick it up within an hour or on restart.',
          'Automatic license sync is off by default. For monthly or annual subscriptions, optionally enable it under Admin → Settings → License & updates, enter the sync token, save, and use Sync now. Saving a token or installing an online key never enables sync by itself. This retrieves renewed signed licenses, not software updates. Blank tokens preserve the stored credential; Clear stored sync token removes it. JANUS_LICENSE_SYNC and JANUS_LICENSE_SYNC_TOKEN pin their respective settings when supplied in the environment. Offline mode prevents remote sync. File-backed keys are not synchronized or overwritten; deliberately remove the file configuration, restart and install a database key before opting in. Renewal warnings are suppressed only while a successful renewal confirmation is fresh; expiry and grace details remain visible.',
          'Copy the raw Sync token from the portal. A complete license_id.token credential is also accepted only for the installed license; do not paste Authorization or Bearer header text. A revoked service response preserves signed rights and keeps a warning visible while sync continues. Only a newer same-identity signed license plus fresh higher-generation subscription evidence can automatically recover. For legacy servers without revocation freshness, obtain and manually install a verified replacement.',
          'For deployments managed as configuration, set JANUS_LICENSE_FILE to a path containing the key. A file, when present, takes precedence over a key installed from the UI. Removing the UI key returns the gateway to Community limits without deleting anything.',
        ],
      },
      {
        heading: 'Nodes and high availability',
        body: [
          'A node is one running gateway process. Every process heartbeats a row in the database; Admin → Settings → License & updates shows how many are live against what the key covers. Community covers one node. Running more than the key covers is a warning in the log and on the card — nothing is refused and no traffic is dropped, because a scale-up during an incident is exactly when you do not want licensing in the way. Add nodes at janusedge.com or scale down to clear it.',
          'Everything a sign-in needs lives in the database: sessions, OIDC state, the pending step between password and authenticator code. Two or more replicas behind one Service work without sticky sessions; a person can enter their password on one pod and their code on another. Rate limits divide by the live node count automatically. The Helm chart defaults to two replicas with a PodDisruptionBudget and anti-affinity; SQLite mode is single-writer and stays at one.',
        ],
      },
      {
        heading: 'Update check',
        body: [
          'Off by default. Set JANUS_UPDATE_CHECK=true and the gateway asks janusedge.com once a day for the latest release, sending only the running version, the edition and this gateway\u2019s instance id. The answer (a newer version, release notes, or an advisory) is shown under Admin \u2192 System \u2192 License \u2192 Updates and nowhere else; nothing is downloaded or changed. JANUS_OFFLINE=true forces it off for air-gapped deployments.',
        ],
      },
      {
        heading: 'Perpetual keys and upgrades',
        body: [
          'A perpetual key carries a maintenance date. Any release published on or before that date runs forever. A newer release checks the key before touching the database and exits with a clear message, so the previous version restarts cleanly. Renewing maintenance at the portal issues a key with a later date.',
        ],
      },
    ],
  },
  {
    slug: 'admin/operations',
    title: 'Operating Janus',
    summary: 'Configuration, retention, health checks, and observability.',
    group: 'Admin guide',
    sections: [
      {
        heading: 'Configuration',
        body: [
          'Administration → Settings owns gateway configuration: General, Sign-in & provisioning, License & updates, Troubleshooting, and System status. Personal settings remain under the account menu, with Account, Security, Appearance, and Regional sections. Each card keeps its own save controls; switching workspace sections retains unsaved drafts in memory, not across reloads.',
          'Configure and test OIDC or direct LDAP sign-in under Settings → Sign-in & provisioning; SCIM 2.0 provisioning is separate. Administration → People has Users, Groups, and Teams. People → Teams owns organization-wide creation, CSV import, membership roles, join-request decisions, group mappings, quota delegation, and deletion. The top-level Team workspace is informational: it shows only your actual memberships, members, usage, and quotas, even for administrators. Administrator permissions alone do not make a team one of your memberships. Model access still requires Grants. CSV imports validate the complete upload and apply all rows or none.',
          'Everything is configured by environment variable; nothing is hardcoded. JANUS_ENCRYPTION_KEY is always required. With JANUS_DEV_AUTH unset you must also supply JANUS_OIDC_PROVIDER_URL, JANUS_OIDC_CLIENT_ID, and JANUS_OIDC_CLIENT_SECRET.',
          'JANUS_DATABASE_URL selects the database: a postgres:// URL for production, or a sqlite:// URL. When it is unset the gateway defaults to embedded SQLite at /data/janus.db — single-node evaluation mode — and logs a prominent startup warning; Settings → System status shows which backend is in force.',
          'Startup validates every required variable and dependency, and exits with a message naming what is missing rather than failing later under load.',
          'Feature flags are the exception to environment-variable configuration: they are runtime database settings toggled on Admin → Settings → General, audit-logged, and applied without a restart. The spend_emphasis flag chooses whether dashboard graphs open on the Spend metric (on) or the Tokens metric (off, the shipped usage-emphasis default); viewers can still switch metrics per graph, and links with an explicit metric keep it.',
        ],
        code: {
          language: 'bash',
          code: `JANUS_DATABASE_URL=postgres://janus:…@postgres:5432/janus
JANUS_ENCRYPTION_KEY=$(openssl rand -hex 32)
JANUS_OIDC_PROVIDER_URL=https://id.example.com/application/o/janus
JANUS_OIDC_CLIENT_ID=janus
JANUS_OIDC_CLIENT_SECRET=…
JANUS_BOOTSTRAP_ADMIN_EMAILS=alice@example.com,bob@example.com
JANUS_PUBLIC_URL=https://janus.example.com
JANUS_TRUSTED_PROXIES=10.0.0.0/8`,
        },
      },
      {
        heading: 'Linux server install',
        body: [
          'On a Linux server with systemd, the release installer sets Janus up as a service: sudo python3 install.py --admin-email you@example.com --hostname ai.example.com. It runs as the unprivileged janus user with HTTPS on port 443, redirects port 80 to HTTPS, creates a self-signed certificate on first install, and creates the first administrator before the gateway listens beyond loopback.',
          'Configuration, including the encryption key, lives in /etc/janus/janus.env; back it up with the database in /var/lib/janus. Apply edits with sudo systemctl restart janus. SQLite is the default and suits a single server for a small team; pass --database-url postgres://… on first install for PostgreSQL.',
          'Replace the certificate with sudo janus-ctl cert install --cert fullchain.pem --key privkey.pem, which checks that the key matches and that the certificate has not expired, or with sudo janus-ctl cert acme --domain ai.example.com for a Let’s Encrypt certificate that renews automatically (it needs a public DNS name and port 80 reachable from the internet). sudo janus-ctl status shows the service, readiness and certificate expiry; sudo janus-ctl upgrade --version V installs another release after backing up a SQLite database.',
        ],
        code: {
          language: 'bash',
          code: `curl -fLO https://github.com/Torvanis/janus/releases/latest/download/install.py
sudo python3 install.py --admin-email you@example.com --hostname ai.example.com
sudo janus-ctl cert install --cert fullchain.pem --key privkey.pem
sudo janus-ctl status`,
        },
      },
      {
        heading: 'Health and readiness',
        body: [
          'GET /healthz reports liveness with no dependency checks. GET /readyz returns 503 unless the database and identity provider are both reachable — wire it to your readiness probe so a replica never receives traffic it cannot serve.',
          'The identity-provider check tolerates transient faults: a single failed discovery probe (for example a connection refused while the provider restarts) is retried immediately and does not make the replica unready. Two consecutive failed probe rounds mark the provider unreachable, so a real outage flips /readyz to 503 within about 40 seconds, and a recovered provider clears it within about 5 seconds. Each failed probe is logged at WARN level with the discovery URL and the underlying error.',
        ],
      },
      {
        heading: 'Retention',
        body: [
          'Usage events are kept for JANUS_USAGE_RETENTION_DAYS (365 by default) and audit entries for JANUS_AUDIT_RETENTION_DAYS (730). A purge job runs daily at JANUS_PURGE_JOB_TIME_UTC and reconciles quota ledgers afterwards.',
          'Prompt and response bodies are never persisted anywhere, including debug logs — with one deliberate exception: troubleshooting mode, described below, which an administrator must switch on.',
        ],
      },
      {
        heading: 'Troubleshooting mode',
        body: [
          'When a client keeps failing and the request log’s metadata is not enough, Admin → Settings → Troubleshooting starts a time-boxed capture session (24 hours by default, one week at most). While it is active, requests matching its filter have their headers — Authorization, Cookie and API-key headers always redacted — and, optionally, their request and response bodies captured. The filter combines models, users or service tokens, upstreams, error codes, HTTP statuses, success/failure and token-size thresholds on each direction with AND or OR; an empty filter captures everything and is warned about.',
          'Every session declares a retention policy — maximum age, maximum number of captures, maximum total size, at least one required — enforced hourly and on demand with Run cleanup now. Bodies are stored in the database by default or, when JANUS_TROUBLESHOOT_DIR is set on the gateway, as files under that directory; either way they can be encrypted at rest with the gateway key, and the panel warns prominently when they are not. Purge all captured data removes everything at once.',
          'Captured rows in Admin → Requests gain a Download control (also in the detail drawer) serving a tar.gz with manifest.json, request/headers.json, request/body.*, response/headers.json and response/body.*. Export all streams every capture in one archive with a captures.jsonl index — one JSON document per request — and an Athena table definition in README.txt for bulk analysis or replay validation. Enabling, updating, disabling, purging and exporting are all audit-logged.',
        ],
      },
      {
        heading: 'Observability',
        body: [
          'GET /metrics exposes Prometheus metrics. Deliberately, no metric carries a user or token label: per-person analytics come from the usage-event table instead, which keeps active series bounded.',
          'Logs are structured JSON on stdout. The Authorization header, API keys, and request bodies are excluded by construction.',
          'Alert delivery is split in two on purpose. Alerts the gateway can observe itself (quota thresholds, upstream down) are sent by the Janus process using JANUS_SMTP_* and the alert rules configured in Admin \u2192 Alerts. Infrastructure alerts \u2014 gateway down, database replication lag over ten seconds \u2014 are evaluated by Prometheus (deploy/compose/prometheus/alert-rules.yaml) and must not depend on the gateway being healthy, so they are delivered by Alertmanager: enable the compose monitoring profile and set the ALERTMANAGER_* variables (SMTP for email to the admin, ALERTMANAGER_WEBHOOK_URL for a webhook). Running Prometheus without Alertmanager leaves those alerts visible in the Prometheus UI but delivered nowhere. Outside the reference compose stack, attach your own Alertmanager and point it at the same rules.',
        ],
      },
    ],
  },
  {
    slug: 'admin/bootstrap',
    title: 'Bootstrapping the first administrator',
    summary: 'Break-glass access for a fresh deployment.',
    group: 'Admin guide',
    sections: [
      {
        heading: 'Procedure',
        body: [
          'Set JANUS_BOOTSTRAP_ADMIN_EMAILS to a comma-separated list before first start. Any listed address is granted the admin role when it signs in — on first sign-in, or on the next one if the account already exists.',
          'Once a real administrator exists you can unset the variable and restart: roles are stored in the database and persist. Further promotions happen in Admin → People.',
          'Alternatively, JANUS_ADMIN_GROUPS grants the admin capability to members of the listed identity-provider groups (comma- or space-separated names, matched case-insensitively against the JANUS_OIDC_GROUPS_CLAIM claim on every sign-in). Unlike the bootstrap list this grant is not sticky: leaving the group, or unsetting the variable, revokes it at the next sign-in, while bootstrap- and UI-granted admin persist.',
        ],
      },
      {
        heading: 'Guard rail',
        body: [
          'Janus refuses to demote or disable the last active administrator, so a deployment can never be locked out of its own admin console.',
        ],
      },
    ],
  },
  {
    slug: 'admin/test-idp',
    title: 'Test identity provider (Authentik)',
    summary: 'Run a local Authentik to exercise real OIDC sign-in, group sync, and auto-grants.',
    group: 'Admin guide',
    sections: [
      {
        heading: 'Why',
        body: [
          'JANUS_DEV_AUTH bypasses the identity provider entirely, which means the OIDC group-sync and group-based auto-grant paths never run. To exercise them without an enterprise IdP, the reference compose file ships an optional Authentik instance behind the idp profile. It is an evaluation aid: keep it on localhost and never treat it as production identity.',
        ],
        code: {
          language: 'bash',
          code: `cd deploy/compose
# .env additions (see .env.example): AUTHENTIK_SECRET_KEY and
# AUTHENTIK_POSTGRES_PASSWORD are required, both via openssl rand -hex 32.
docker compose --profile idp up -d
# Authentik UI: http://localhost:9000 (akadmin / AUTHENTIK_BOOTSTRAP_PASSWORD)`,
        },
      },
      {
        heading: 'Create the OIDC application in Authentik',
        body: [
          'Sign in as akadmin, then under Applications → Providers create an OAuth2/OpenID provider: authorization code flow, confidential client, redirect URI set to JANUS_PUBLIC_URL + /auth/callback (for the default compose setup that is http://localhost:8080/auth/callback). Leave the default scope mappings — Authentik includes group membership in the standard profile scope.',
          'Then under Applications → Applications create an application named janus with slug janus and attach the provider. The issuer URL Janus needs is the provider\u2019s "OpenID configuration issuer", which for slug janus is http://localhost:9000/application/o/janus/.',
          'Create a few groups (Admin → Directory → Groups) and users, and add the users to groups. On sign-in Janus syncs each user\u2019s group list from the ID token, creates missing groups, and applies any model grants an administrator attached to those groups — this is the auto-grant path.',
        ],
      },
      {
        heading: 'Point Janus at it',
        body: [
          'Set the JANUS_OIDC_* variables and unset JANUS_DEV_AUTH. Authentik has no separate groups scope (groups ride in profile), so narrow JANUS_OIDC_SCOPES accordingly; the groups claim name is the default "groups", so no claim override is needed.',
          'Janus verifies ID-token signatures itself and accepts RS256/RS384/RS512 (RSA) and ES256/ES384/ES512 (ECDSA) signing algorithms. EdDSA-only providers are rejected fail-closed; if sign-in fails with an unsupported-algorithm error, switch the identity provider\u2019s signing key to RS256 (the OIDC default).',
          'Restart Janus and open the sign-in page: you are redirected to Authentik, and after authenticating, your Janus profile shows the synced groups. Removing a user from a group in Authentik is reflected on their next sign-in.',
        ],
        code: {
          language: 'bash',
          code: `JANUS_DEV_AUTH=false
JANUS_OIDC_PROVIDER_URL=http://localhost:9000/application/o/janus/
JANUS_OIDC_CLIENT_ID=<client id from the provider page>
JANUS_OIDC_CLIENT_SECRET=<client secret from the provider page>
JANUS_OIDC_SCOPES=openid,profile,email`,
        },
      },
      {
        heading: 'What to verify',
        body: [
          'Sign-in round-trip: an Authentik user lands in Janus with their email, name, and groups populated (Admin → People shows the synced groups).',
          'Group sync: change a user\u2019s groups in Authentik, sign them out and back in, and confirm the Janus group list follows.',
          'Auto-grants: attach a model grant to a group in Janus (Admin → Grants), then confirm a group member\u2019s GET /v1/models includes the model with grant_source group.',
        ],
      },
    ],
  },
  {
    slug: 'reference/divergences',
    title: 'Divergences from the original decisions',
    summary: 'Where the shipped system departs from the decision record, why, and what it costs you.',
    group: 'Reference',
    sections: [
      {
        heading: 'TimescaleDB is not used',
        body: [
          'The decision record specified PostgreSQL 16 with TimescaleDB. Usage events are stored in plain PostgreSQL tables with composite time indexes instead. At the specified scale this answers every dashboard query comfortably, and it keeps managed PostgreSQL offerings viable.',
          'If event volume grows an order of magnitude, TimescaleDB can be added additively (CREATE EXTENSION plus converting the usage-event table to a hypertable); every usage query is isolated in one store file.',
        ],
      },
      {
        heading: 'Redis is not used',
        body: [
          'Sessions, OIDC sign-in state, and quota ledgers live in PostgreSQL; credentials are cached per replica in process memory for 60 seconds. This removes a stateful service from the operational surface.',
          'Three consequences to know. First, quota enforcement reads the database ledger: concurrent requests can each pass an almost-exhausted check, so a cap can be overshot by the amount in flight; the reconciliation job trues the ledger up. Second, revoking an API token takes effect immediately on the replica that processes the revocation but up to 60 seconds on the others, because each replica expires its credential cache independently. Third, requests-per-minute rate-limit buckets live in process memory per replica: an N-replica deployment enforces up to N\u00d7 the configured limit for a client whose requests spread across replicas, so scaling the replica count makes rate limits look looser than configured.',
        ],
      },
      {
        heading: 'OTLP export is built in, not the OTel SDK',
        body: [
          'The decision record called for OpenTelemetry export via the OTel Go SDK. Janus ships a dependency-free OTLP/HTTP exporter instead: when OTEL_EXPORTER_OTLP_ENDPOINT is set, each proxied request produces a root span with quota-check, upstream-call, and usage-write children, and metric snapshots are exported on a configurable interval (default 30s). Export is strictly non-blocking — an unreachable collector yields a rate-limited warning log and nothing else — and the otel_enabled feature flag pauses it at runtime.',
          'The exporter speaks OTLP/HTTP with the JSON encoding, so point the endpoint at a collector\u2019s 4318 port, not the gRPC 4317 one. Without the environment variable there is no export at all; Prometheus /metrics remains the primary surface.',
        ],
      },
      {
        heading: 'SQLite and dev-auth were added',
        body: [
          'Two evaluation conveniences exist that the decision record never asked for: an embedded SQLite backend (JANUS_DATABASE_URL=sqlite://…) and a no-IdP sign-in mode (JANUS_DEV_AUTH=true).',
          'Dev-auth is off by default, requires an explicit opt-in, and logs a startup warning — but it accepts any email address without a password. Never enable it, or SQLite, on a deployment anyone else can reach. Startup enforces this: the process refuses to boot with dev-auth when JANUS_ENV=production or when JANUS_PUBLIC_URL is not loopback/localhost, unless JANUS_DEV_AUTH_ALLOW_UNSAFE=true explicitly overrides it for an isolated demo. Production remains PostgreSQL plus a real identity provider, exactly as specified.',
        ],
      },
      {
        heading: 'URL path shapes differ from the requirement text',
        body: [
          'The requirement text named /api/v1/* as an OpenAI-compatible proxy path and placed admin endpoints under /admin/v1/*. The shipped routing uses a single canonical proxy namespace instead: the OpenAI-compatible surface is PUBLIC_URL/v1/* (this is the base URL to configure in SDKs), the first-party application API is /api/v1/*, and admin endpoints live at /api/v1/admin/* where they share the app API session auth and CSRF protection.',
          'A client that follows the requirement text literally and points an SDK at PUBLIC_URL/api/v1 will reach the application API and get 404/401 JSON errors, not proxy responses. Always use PUBLIC_URL/v1 as the SDK base URL — the /api/v1/me response and the in-app help snippets report exactly this value.',
        ],
      },
    ],
  },
  {
    slug: 'troubleshooting',
    title: 'Troubleshooting',
    summary: 'Symptom-first diagnosis.',
    group: 'Reference',
    sections: [
      {
        heading: 'I got a 401',
        body: [
          'The token is missing, revoked, mistyped, or belongs to a disabled account. Check the Tokens page: a revoked token shows as revoked. Confirm your client sends `Authorization: Bearer <token>` and not the token alone.',
        ],
      },
      {
        heading: 'My model is missing from /v1/models',
        body: [
          'Either the model is not enabled, or you hold no grant for it. Both are administrator actions. A model that has never been priced cannot be enabled at all — an administrator must save a rate card first ($0 rates are allowed for self-hosted models).',
        ],
      },
      {
        heading: 'My stream stopped early',
        body: [
          'Check the request in the log. If quota_violated is set and a hard-kill quota applies, the stream was cut on purpose. Otherwise look at the response size against JANUS_MAX_RESPONSE_BYTES, and at the upstream timeout settings.',
        ],
      },
      {
        heading: 'My dashboard shows zero',
        body: [
          'Metering is written just after the response is delivered, so allow a few seconds. Confirm the time range covers the request. If the call was refused before reaching a provider, it is still recorded, but with zero tokens and its error code set.',
        ],
      },
      {
        heading: 'I am burning quota faster than expected',
        body: [
          'Open the request log and look at token_accounting_method. Requests marked “estimated from byte count” were priced from a byte-length approximation because the provider reported no usage block — most often on the catch-all route. Cached input tokens are also counted in tokens_in.',
        ],
      },
    ],
  },
  {
    slug: 'reference/api',
    title: 'API reference',
    summary: 'The three API surfaces, how to authenticate against each, and the machine-readable OpenAPI 3.1 description.',
    group: 'Reference',
    sections: [
      {
        heading: 'Download the OpenAPI description',
        body: [
          'Every endpoint, parameter, and error envelope is described in an OpenAPI 3.1 document served by this gateway at /openapi.json. Because it ships inside the binary, it always matches the running build. Import it into Postman, Insomnia, or a code generator:',
        ],
        code: {
          language: 'bash',
          code: 'curl -O ${JANUS_PUBLIC_URL}/openapi.json',
        },
      },
      {
        heading: 'The three surfaces',
        body: [
          'Proxy — PUBLIC_URL/v1/* speaks the OpenAI API and authenticates with a bearer token from the Tokens page. This is the base URL to configure in SDKs. See “Proxy endpoints” for details.',
          'Application — /api/v1/* serves the web UI: profile, tokens, request log, dashboards, and the team-lead quota endpoints. It authenticates with the browser session cookie, and mutating calls must echo the janus_csrf cookie value in the X-Janus-CSRF header.',
          'Admin — /api/v1/admin/* requires the admin role on top of a session. Every mutation is audit-logged with actor and before/after values.',
        ],
      },
      {
        heading: 'Error envelope',
        body: [
          'All surfaces return the OpenAI-compatible error shape: an error object with message, type, code, and — where useful — param, reason, retry_after, reset_at, and request_id. The error catalog documents every code with its cause and fix.',
        ],
        code: {
          language: 'json',
          code: '{\n  "error": {\n    "message": "Quota exceeded: 1,000,000 input tokens per day.",\n    "type": "policy.quota_exceeded",\n    "code": "policy.quota_exceeded",\n    "reset_at": "2025-01-02T00:00:00Z",\n    "request_id": "req_8f3a"\n  }\n}',
        },
      },
    ],
  },
  {
    slug: 'reference/proxy-endpoints',
    title: 'Proxy endpoints',
    summary: 'Per-endpoint reference for the OpenAI-compatible surface at PUBLIC_URL/v1.',
    group: 'Reference',
    sections: [
      {
        heading: 'POST /v1/chat/completions',
        body: [
          'The primary endpoint. The body is forwarded verbatim to the upstream serving the requested model; only the model field is inspected, for routing, grants, and policy. Set stream:true for server-sent events — frames are relayed as they arrive and usage is read from the final frame.',
          'Successful buffered (non-streaming) responses carry X-Janus-Cost-USD with the cost attributed to the call under the model’s current rate card, and X-Janus-Token-Accounting naming how the figures were obtained (upstream_reported, upstream_reported_cost, byte_count_fallback, unmetered_modality). Streaming responses do not carry these headers — the cost is only known once the final frame has arrived — but the call is metered identically and appears in the request log and dashboards.',
          'Throughput is returned too. When the provider measures it (Groq, llama.cpp, Ollama) the gateway forwards it as X-Janus-Tokens-In-Per-Second and X-Janus-Tokens-Out-Per-Second with X-Janus-Throughput-Source: upstream. When it does not, the gateway derives it from the token counts and its own clock and returns it under DIFFERENT names — X-Janus-Calculated-Tokens-In-Per-Second and X-Janus-Calculated-Tokens-Out-Per-Second with X-Janus-Throughput-Source: calculated — so a network-inclusive estimate can never be mistaken for a provider measurement. On streams the same fields arrive as HTTP trailers after the final frame (prompt throughput uses time-to-first-byte as its window, generation throughput the remainder).',
        ],
        code: {
          language: 'bash',
          code: 'curl ${JANUS_PUBLIC_URL}/v1/chat/completions \\\n  -H "Authorization: Bearer $JANUS_TOKEN" \\\n  -H "Content-Type: application/json" \\\n  -d \'{"model": "gpt-4o-mini", "messages": [{"role": "user", "content": "Hello"}]}\'',
        },
      },
      {
        heading: 'POST /v1/completions and POST /v1/embeddings',
        body: [
          'Legacy completions and embeddings work the same way: verbatim forwarding, grant and quota checks on the model, full metering. Embedding requests are recorded with the embedding modality so dashboards can separate them from chat.',
        ],
      },
      {
        heading: 'GET /v1/models',
        body: [
          'Returns the OpenAI-shaped model list filtered to what your token may use. A model appears only when it is enabled, priced, and covered by a grant that matches you. If a model you expect is missing, ask an administrator for a grant.',
        ],
      },
      {
        heading: 'Everything else under /v1',
        body: [
          'Paths without a dedicated route (audio, images, moderations, files, and so on) pass through to the upstream when policy allows, and are metered by modality. When a provider reports a cost directly (X.ai cost_in_usd_ticks) that exact figure is recorded. When a TEXT-shaped response returns no usage block, tokens are estimated from byte counts and the request log marks the accounting method byte_count_fallback. Media responses (audio, images, video) are never byte-estimated — a 30 KB MP3 is not 7,680 tokens — so one that carries no usage is recorded with zero tokens and zero cost under unmetered_modality, logged as a configuration gap, and counted in janus_unmetered_requests_total. Streamed TTS (stream_format: sse) reports exact usage on its final frame and is metered normally.',
        ],
      },
    ],
  },
  {
    slug: 'reference/environment',
    title: 'Environment variables',
    summary: 'Every configuration variable the gateway reads, with defaults and constraints.',
    group: 'Reference',
    sections: [
      {
        heading: 'Required',
        body: [
          'JANUS_ENCRYPTION_KEY — 64 hex characters (32 bytes) used to encrypt upstream credentials at rest; changing it orphans stored credentials. JANUS_PUBLIC_URL — the externally reachable URL; forms the OIDC redirect URI and the SDK base URL shown in the UI.',
        ],
      },
      {
        heading: 'Database',
        body: [
          'JANUS_DATABASE_URL — postgres://… for production, or sqlite://path for a single-node deployment. Optional: when unset, the gateway defaults to embedded SQLite at /data/janus.db (single-node evaluation mode) and logs a prominent startup warning. The Admin → Settings → System status page reports the backend and sanitized location in force.',
        ],
      },
      {
        heading: 'Identity',
        body: [
          'JANUS_OIDC_PROVIDER_URL, JANUS_OIDC_CLIENT_ID, JANUS_OIDC_CLIENT_SECRET configure the identity provider; JANUS_OIDC_EMAIL_CLAIM, JANUS_OIDC_NAME_CLAIM, JANUS_OIDC_GROUPS_CLAIM, and JANUS_OIDC_SCOPES override claim names for non-standard providers. JANUS_BOOTSTRAP_ADMIN_EMAILS grants the admin role on first sign-in (sticky). JANUS_ADMIN_GROUPS — a comma- or space-separated list of IdP group names, matched case-insensitively against the groups claim on every sign-in — grants the admin capability while membership lasts; leaving the group revokes it at the next sign-in.',
          'JANUS_DEV_AUTH enables the local evaluation sign-in (no identity provider, any email). The gateway refuses to boot with it when JANUS_ENV=production or the public URL is not loopback; JANUS_DEV_AUTH_ALLOW_UNSAFE=true overrides that refusal for isolated demos only.',
        ],
      },
      {
        heading: 'Serving and sessions',
        body: [
          'JANUS_LISTEN_ADDR (default :8080), JANUS_TLS_CERT_PATH / JANUS_TLS_KEY_PATH for direct TLS, JANUS_COOKIE_SECURE to force secure cookies, JANUS_TRUSTED_PROXIES as a comma-separated CIDR list gating X-Forwarded-For, JANUS_CA_BUNDLE for private upstream CAs.',
          'JANUS_SESSION_TTL_HOURS (default 24) and JANUS_SESSION_IDLE_TIMEOUT_HOURS (default 4) bound browser sessions.',
        ],
      },
      {
        heading: 'Proxy behaviour and retention',
        body: [
          'JANUS_UPSTREAM_CONNECT_TIMEOUT_SECONDS, JANUS_UPSTREAM_TTFB_TIMEOUT_SECONDS, and JANUS_UPSTREAM_TOTAL_TIMEOUT_SECONDS set the default bound for each hop. An administrator can override any of them at runtime from Admin → Settings → General → Upstream timeouts (PATCH /api/v1/admin/system/upstream-timeouts): the override is stored in the database, survives restarts, applies to the next request on every replica without a redeploy, and is audit-logged. Raise the time-to-first-byte bound when a busy provider queues requests and callers see 503s. JANUS_MAX_RESPONSE_BYTES caps buffered responses. JANUS_DISCOVERY_INTERVAL_MINUTES (default 2) sets the model-discovery cadence — each run also records whether every upstream is reachable, so it doubles as the availability probe — and an administrator can override it at runtime from Admin → Settings → General → Model discovery (PATCH /api/v1/admin/system/discovery-interval), stored in the database, applied immediately without a restart, and audit-logged.',
          'JANUS_USAGE_RETENTION_DAYS and JANUS_AUDIT_RETENTION_DAYS control purging, run daily at JANUS_PURGE_JOB_TIME_UTC. JANUS_QUOTA_CHECKPOINT_INTERVAL_HOURS bounds quota-counter recovery time. JANUS_SMTP_HOST/PORT/USER/PASSWORD/FROM configure alert email. JANUS_ENV labels the deployment; JANUS_LOG_LEVEL is debug, info, warn, or error.',
        ],
      },
    ],
  },
  {
    slug: 'faq',
    title: 'FAQ',
    summary: 'Short answers to the questions new users and administrators ask first.',
    group: 'Reference',
    sections: [
      {
        heading: 'Which SDKs work with Janus?',
        body: [
          'Anything that speaks the OpenAI API: the official openai Python and JavaScript SDKs, LangChain, LiteLLM, continue.dev, and most IDE assistants. Point the SDK’s base URL at PUBLIC_URL/v1 and use a Janus token as the API key. No other change is needed.',
        ],
      },
      {
        heading: 'Does Janus store my prompts?',
        body: [
          'No. Only request metadata is recorded: who called which model, when, token counts, cost, latency, and status. Request and response bodies are never persisted. The request-log columns are the complete list of what is kept.',
        ],
      },
      {
        heading: 'Why does my bill differ from the provider’s invoice?',
        body: [
          'Janus attributes cost from resolved rate cards: admin overrides take priority over upstream metadata, then exact provider references. Discovery refreshes automatic prices; administrators can set per-field overrides with effective dates. If a rate card lags a provider price change, attribution drifts until the card is updated. Requests where the provider returned no usage block are estimated from byte counts and marked as such in the log.',
        ],
      },
      {
        heading: 'What happens when I hit a quota?',
        body: [
          'The request is refused with policy.quota_exceeded and a reset_at timestamp. By default an in-flight request is allowed to finish; hard-kill quotas cut streams immediately. Retrying before reset_at fails the same way.',
        ],
      },
      {
        heading: 'Can a team lead manage quotas?',
        body: [
          'Yes, if an administrator switches on “lead can edit quotas” for that team. The lead then manages that team’s quotas from the team page — scoped to that team only, and audit-logged like any admin change.',
        ],
      },
      {
        heading: 'Is a token tied to my account?',
        body: [
          'Yes. Every token belongs to one person; its usage, grants, and quotas are that person’s. Only the SHA-256 digest of a token is stored, so a lost token cannot be recovered — revoke it and create a new one.',
        ],
      },
    ],
  },
  {
    slug: 'changelog',
    title: 'Changelog',
    summary:
      'What changed in each release of this gateway. Generated at build time from the repository CHANGELOG.md, the authoritative copy of this history.',
    group: 'Reference',
    sections: CHANGELOG.flatMap((release) =>
      release.categories.map((category) => ({
        heading: `${release.version}${release.date ? ` (${release.date})` : ''} — ${category.name}`,
        body: category.items,
      })),
    ),
  },
  {
    slug: 'legal/notices',
    title: 'Third-party notices',
    summary: 'Attribution for the open-source software this product includes.',
    group: 'Reference',
    sections: [
      {
        heading: 'Backend (Go)',
        body: [
          'github.com/go-chi/chi/v5 — MIT. github.com/jackc/pgx/v5 — MIT. github.com/prometheus/client_golang — Apache-2.0. modernc.org/sqlite — BSD-3-Clause. golang.org/x/* — BSD-3-Clause.',
        ],
      },
      {
        heading: 'Frontend (npm)',
        body: [
          'react and react-dom — MIT. react-router-dom — MIT. @tanstack/react-query — MIT. vite and @vitejs/plugin-react — MIT. typescript — Apache-2.0. vitest — MIT.',
        ],
      },
      {
        heading: 'Full licence texts',
        body: [
          'Each component remains under its own licence. Full texts ship in each dependency’s source distribution: `go list -m all` enumerates the Go module graph, and `npm ls --all` under web/ enumerates the JavaScript tree. The repository’s NOTICE file is the authoritative copy of this attribution list.',
        ],
      },
    ],
  },
];

export function pageBySlug(slug: string): DocPage | undefined {
  return PAGES.find((page) => page.slug === slug);
}
