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
    action: 'Create a new token on the API Tokens page and update the tool that failed.',
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
    when: 'A Security Gateway policy refused the request or withheld the response. error.param names the check kind (secrets, pii, terms, shape, prompt_injection); the rule and the matched text are never returned. On a stream this arrives as a final in-band error frame followed by [DONE]. If the message says the policy fails closed, the classifier behind a check was unreachable — nothing matched. Child Safety uses the same code when a model or endpoint is blocked for a child account.',
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
    code: 'policy.response_too_large',
    status: 500,
    when: "A streamed response grew past the gateway's response size cap (JANUS_MAX_RESPONSE_BYTES). The text before this point was delivered and this arrives as a final in-band error frame (the HTTP status was already 200). A non-streamed response over the cap is delivered whole.",
    action:
      'Ask for a shorter answer (lower max_tokens, or split the task). If large responses are expected, an administrator can raise JANUS_MAX_RESPONSE_BYTES.',
    retryable: false,
  },
  {
    code: 'policy.service_token_scope',
    status: 403,
    when: 'A service token was used outside the /v1 inference API, for example against a dashboard or admin endpoint. Service tokens only authenticate model calls.',
    action: 'Use the service token only for /v1 requests. For the web API, sign in with a user account.',
    retryable: false,
  },
  {
    code: 'policy.service_token_expired',
    status: 401,
    when: 'The service token has passed the expiry date set when it was created. This is different from a revocation: nobody turned it off, it aged out.',
    action: 'Ask a Janus administrator to issue a replacement under Admin → Service tokens and update the integration.',
    retryable: false,
  },
  {
    code: 'policy.managed_model_unavailable',
    status: 503,
    when: 'The managed model (alias) you called points at a model that is missing or disabled. Your request is correct; the alias needs repairing.',
    action: 'Contact a Janus administrator. Until it is fixed, call another model from GET /v1/models directly.',
    retryable: true,
  },
  {
    code: 'policy.managed_model_fallback_exhausted',
    status: 503,
    when: "The managed model's target is unavailable and the fallback configured for exactly that case cannot take over either. The message names both reasons.",
    action:
      'Retry in a few minutes; if it persists, contact a Janus administrator with the request_id so they can fix the target or the fallback.',
    retryable: true,
  },
  {
    code: 'policy.subscription_reauth_required',
    status: 403,
    when: 'The call used one of your personal provider subscriptions (my/… models) and its stored sign-in no longer works: it expired, was revoked at the provider, or the password changed.',
    action: 'Open the Subscriptions page and connect that subscription again, then retry.',
    retryable: false,
  },
  {
    code: 'upstream.unavailable',
    status: 503,
    when: 'The provider behind the requested model could not be reached, or its credentials could not be decrypted.',
    action: 'Retry shortly. If it persists, an administrator should check Admin → Settings → System status for that upstream.',
    retryable: true,
  },
  {
    code: 'upstream.stream_interrupted',
    status: 502,
    when: 'The provider ended a streaming response early: its connection dropped, or it closed the stream without a [DONE], finish_reason or usage block. The HTTP status was already 200, so this arrives as a final in-band error frame (502 is the status inside that frame) and the text before it is incomplete.',
    action:
      'Retry the request. If it keeps happening, an administrator should check the upstream server (restarts, out-of-memory kills, proxies with short idle timeouts). The request log shows these with this code.',
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
    when: 'The request body was malformed, or a required field such as `model` was missing. The same code is used, with HTTP 413, when a body is too large for the provider adapter to rewrite, and with HTTP 409 when an admin change conflicts with things that depend on it.',
    action:
      'Read the message and param field, fix the request, and send it again. For 413, send a smaller payload; for 409, deal with the dependents the message names first.',
    retryable: false,
  },
  {
    code: 'not_found_error',
    status: 404,
    when: 'The path does not exist on this gateway, or the item it names (a token, model, request, team…) was not found or is not visible to you.',
    action: 'Check the URL and the id. For model calls the base URL must end in /v1; for model names call GET /v1/models.',
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
    code: 'license.required',
    status: 402,
    when: 'An administrator tried to create something new (an upstream, token, model…) while the gateway has no valid license. Everything already configured keeps working.',
    action: 'An administrator installs or renews the license key under Administration → Settings → License & updates.',
    retryable: false,
  },
  {
    code: 'license.feature_not_licensed',
    status: 402,
    when: 'An administrator tried to switch on a feature the installed edition does not include (for example load balancing or guardrail enforcement).',
    action:
      'Install a key for an edition that includes the feature under Administration → Settings → License & updates, or leave the feature off.',
    retryable: false,
  },
  {
    code: 'license.seats_exhausted',
    status: 402,
    when: 'An administrator tried to create a new local account, or turn a disabled account back on, while every licensed seat is in use. A seat is a user active in the last 30 days.',
    action:
      'Add seats to the license under Administration → Settings → License & updates, or wait for an inactive user to free a seat.',
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
      'The plug-in that translates between the OpenAI wire format and one provider’s protocol. Janus ships adapters for OpenAI-compatible endpoints, Google Gemini, Anthropic, AWS Bedrock, Google Vertex, Ollama, vLLM, llama.cpp, and Hugging Face TEI.',
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
      'Permission for a user, a group, all users, a team, all teams, or a service token to call a model. Personal access combines direct, group and all-users grants — the most permissive match wins. Working under a team uses only that team’s grants and all-teams grants.',
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
      'Input tokens the provider served from its own prompt cache, billed at the cached rate. OpenAI-style providers count them inside tokens_in; Anthropic and Bedrock report them separately from tokens_in.',
  },
  {
    term: 'Cache-write tokens',
    definition:
      'Tokens written to the provider’s prompt cache (5-minute or 1-hour). They are billed at the model’s cache-write rates; providers that do not report them meter zero.',
  },
  {
    term: 'Context window',
    definition: 'The maximum number of tokens a model accepts as input (context_window). 0 is shown as a dash, meaning unknown.',
  },
  {
    term: 'Quota window',
    definition:
      'The period a limit is measured over. Calendar windows (day, week starting Monday, month) snap to UTC boundaries; rolling windows (24 hours, 7 days, 30 days) slide continuously.',
  },
  {
    term: 'Breach behaviour',
    definition:
      'What happens when a limit is crossed mid-request. The default lets the in-flight request finish and refuses the next one; hard-kill cuts a stream within a fraction of a second of the limit being crossed and ends it with a policy.quota_exceeded error frame. Non-streaming requests always finish.',
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
          'Open Janus. With SSO configured, choose “Continue with corporate SSO” (or “Continue with” your identity provider): your account is created automatically on first sign-in, and your group memberships are read from the identity token each time. Without SSO, sign in with the Janus account your administrator created for you (you will be asked to choose your own password the first time).',
          'On a brand-new gateway without SSO, the first visit asks you to create the administrator account; with SSO, administrators come from the configured admin emails or admin groups. If every licensed seat is in use, a new user’s first sign-in is refused until an administrator adds seats.',
          'You do not need to be added to an allowlist to sign in. What you can *do* is controlled by model grants.',
        ],
      },
      {
        heading: '2. Create a token',
        body: [
          'Go to API Tokens and choose “Create token”. Name it after the tool that will use it — that is what makes revoking a single integration safe later. Pick its Token context — Personal or one of your teams — which decides the models and quotas the token uses.',
          'The value is displayed once. Janus stores only a SHA-256 digest and genuinely cannot show it again.',
        ],
      },
      {
        heading: '3. Point your tool at the gateway',
        body: [
          'Janus speaks the OpenAI API. Set the base URL to your gateway’s API root, ending in /v1 (Help shows the exact value), and use your token as the API key. Everything else in your client stays the same.',
        ],
        code: {
          language: 'bash',
          code: `export JANUS_BASE_URL="https://your-gateway"
export JANUS_API_KEY="janus_…"

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
          'Create the replacement first, update the tool, confirm traffic is flowing on the new token in the request log, and only then revoke the old one. Revocation takes effect at once on every gateway replica.',
        ],
      },
      {
        heading: 'Token context',
        body: [
          'Each token works either as Personal or as one of your teams, chosen when you create it. The context decides which models and quotas the token uses, whatever team you have selected in the browser.',
          'Use Change team on the API Tokens page to move a token to another context without replacing the key; you can choose to move its existing usage too, which also moves it between quotas. If you leave the team a token is bound to, the token stops working until you change its context.',
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
          'GET /v1/models returns the enabled models and managed models granted to you — directly, through one of your groups, or through an all-users grant — plus the models of your own connected subscriptions (my/…), which need no grant. Without grants you can only use your own subscriptions.',
          'When you work under a team — through a team token or the Working under selector — only that team’s grants and all-teams grants count, so you see only that team’s models.',
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
          'Every model carries up to five rates: input, output, cached input, and — for providers that bill prompt-cache writes, such as Anthropic — 5-minute and 1-hour cache-write rates. Cached input tokens are billed at the cached rate — OpenAI-style providers count them inside the input total, Anthropic and Bedrock report them separately; cache-write tokens are metered separately and billed at the matching cache-write rate.',
          'A model cannot be enabled without a rate card (except in local-only mode, where costs are not tracked). Automatic metadata can supply supported prices; unknown prices are distinct from explicitly free prices. An explicitly saved $0 rate card counts: self-hosted models can be enabled free of charge, and their usage is attributed at $0.00.',
        ],
      },
      {
        heading: 'Context windows',
        body: [
          'Each model in the catalog shows its context window — the maximum number of tokens it accepts as input, reported as context_window (in tokens) in the API. Discovery refreshes it from the upstream, then an exact reference for that provider. In Admin → Models → Edit, administrators can override context and each price independently, or choose Use automatic to reset one field.',
          'A dash means the context window is not recorded (context_window is 0), not that the model has none. Direct API calls are never rejected or truncated on this value, but auto-routing skips a model a request would not fit, and Janus Chat drops the oldest messages of a long chat to fit it.',
        ],
      },
      {
        heading: 'Health and error rate',
        body: [
          'Each model card on the Models page carries live health. The badge mirrors what administrators see under Admin → Upstreams: Healthy with the last probe latency when the provider answered, Down when the last probe failed (the card is tinted red and names the error) or when every one of at least 3 requests in the last ten minutes failed upstream, Degraded when at least 10% of recent requests failed, and No data when the upstream has not been probed and nothing has been requested yet.',
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
          'A quota caps one of: input tokens, output tokens, spend, or request count. It applies to a person, a team, or a service token, optionally narrowed to one model or managed model. A team’s quota counts only requests made while working under that team (a team token or the Working under selector).',
          'Prompt-cache writes count toward spend quotas at the cache-write rates, but not toward input-token quotas.',
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
          'You are warned at 80% and again at 95% by default; an administrator can configure different warning percentages per quota rule. Warnings for a personal quota go to you; warnings for team and service-token quotas, and every breach, go to administrators. When the limit is reached, the next request is refused with HTTP 429 and a reset_at timestamp. For rolling windows reset_at is the latest time; capacity comes back gradually as older usage leaves the window.',
          'By default a request already in flight is allowed to finish, and the overage is recorded — which is why administrators are advised to set limits with headroom. A quota can instead be set to hard-kill; while any hard-kill quota applies to a request, its stream is cut as soon as any of your limits is crossed.',
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
          'Set "stream": true exactly as you would against the provider. Janus relays each event as it arrives, without buffering, so time-to-first-token is unchanged apart from the gateway’s own overhead. When a security policy checks responses, a small trailing window of text is held back until it has been scanned, and for child accounts Child Safety releases replies a paragraph at a time.',
          'Token counts come from the provider’s usage frames. For OpenAI-compatible, Ollama and Vertex upstreams Janus adds stream_options.include_usage to streamed chat and completions requests that do not set stream_options, so the counts are present. If a stream ends without them, Janus estimates output from the text it relayed.',
          'Other body changes — a renamed model’s native name, security redactions, routing or Child Safety instructions — are described on their own pages.',
        ],
      },
      {
        heading: 'Quotas during a stream',
        body: [
          'By default a stream that crosses a limit mid-flight is allowed to finish and the next request is refused. If an administrator set the quota to hard-kill, the stream is cut as soon as the limit is crossed.',
          'Because the HTTP status is already 200, a stream that is cut ends with a final data: {"error": …} frame carrying policy.quota_exceeded, policy.security_blocked, policy.response_too_large or upstream.stream_interrupted. Check the last frame for an error before treating the answer as complete.',
        ],
      },
    ],
  },
  {
    slug: 'guides/chat',
    title: 'Janus Chat',
    summary: 'Chat with every model you can use, in the browser, metered and protected like API traffic.',
    group: 'User guide',
    sections: [
      {
        heading: 'Starting a chat',
        body: [
          'Open Chat in the sidebar. If it is not there, your administrator has turned Chat off or limited it to certain groups, teams or people; ask them for access.',
          'The model picker in the message box lists the models you can use while working under the current “Working under” selection, plus the chat models from your own connected plans under “Your subscriptions” (unless your administrator turned personal plans off for Chat). Your administrator decides how the picker is grouped and may give models friendly names, a short description and a Recommended badge. “Billed to” says who pays: the team or personal account you work under, or your own plan for a subscription model. New chats start on the model you last started a chat with, or on your organization’s default model if that one is no longer offered.',
          'Press Enter to send and Shift+Enter for a new line. Replies keep going if you close the tab; reopen the chat to see them. Esc stops a reply, Ctrl/Cmd+Shift+O starts a new chat, and / jumps to the message box. An unsent message is kept per chat while the tab is open.',
          'While a reply streams, the thread follows it until you scroll up to read; Jump to latest follows it again. Edit a message you sent, or regenerate a reply, to make a new version; the arrows under it switch between versions.',
          'After the first reply, the chat’s model gives the conversation a short title. This is a separate request, metered like the chat and shown in the request log as janus-chat-title. Its cost is added to the first reply’s footer, and it does not count as a request toward rate limits or request quotas.',
        ],
      },
      {
        heading: 'Your models',
        body: [
          'Tools in the message box opens Tools & models. Under Your models you choose your own Chat, Images, Video and Music models (the last three appear when you have such models). They are your defaults in every chat for as long as they stay available to you. Automatic uses your plan’s model when you chat on a plan, otherwise the organization’s.',
        ],
      },
      {
        heading: 'Files, sound and video',
        body: [
          'Attach files with the paperclip, by dropping them on the message box, or by pasting an image. You can attach up to 10 files per message, each up to 20 MiB unless your administrator changed the limit. Files are checked by their contents: programs and archives are refused even when renamed, and so are images over 40 megapixels. Text files must be UTF-8. Files you attach but never send are removed after a day.',
          'Documents (PDF, Word and text files) are converted to text and sent in full with every message of the chat, so the box shows about how many tokens they add before you send.',
          'Images (PNG, JPEG, GIF, WebP) go to models that see images. Other models are told an image was left out, and the chat says so.',
          'You can also attach sound (MP3, WAV, M4A, OGG, FLAC, AAC, Opus) and video (MP4, MOV, WebM) and play them in the thread. A model that hears sound or watches video gets the file itself. Any other model gets a transcript of the sound, or a description of the video written by a model that watches video. This text is made once per file, and the reply shows Transcribed or Watched the video. Your administrator decides which models see images, hear sound and see video.',
        ],
      },
      {
        heading: 'Instructions and thinking',
        body: [
          'The settings button in the chat header holds instructions for this chat, your own instructions for every chat (your name, tone, language, format), a Thinking level and your Memory. Your organization’s instructions, set by an administrator, come first in every request, then yours, then this chat’s. The model sees them; the thread does not show them. They are stored encrypted.',
          'Thinking (model default, low, medium or high) is sent as reasoning_effort. Janus fits it to what each model takes, and the note under a reply says when it was adjusted. Models that do not reason ignore it.',
        ],
      },
      {
        heading: 'Tools',
        body: [
          'Tools for this chat, in Tools & models, lists what this chat’s model may use, with a switch for each: Web search, X search, Read web pages, Run code, Create images, Create files, Create videos, Create music and Memory. A model is offered only the tools its provider or Janus can actually run, and it decides when to use them. Every tool is on unless you switch it off for this chat or your administrator turned it off for everyone. If the model cannot use tools, the tray says so. If Child Safety is on for your account, some tools may be missing; a note under Tools for this chat says which.',
          'Web search and Run code run on the provider’s own servers for OpenAI, xAI, Claude and Gemini models; code never runs on Janus. X search searches posts on X and is offered to Grok models. Any extra fee the provider charges for a tool is included in the reply’s cost. Self-hosted and other models without a search of their own can search the web when your administrator has set up a search service. The reply shows what was searched and lists the sources it cited.',
          'Read web pages works with every chat model: give it a link and it reads the page. Claude and Gemini use their provider’s reader. Every other model uses Janus’s own reader, which opens only pages on the public internet and reads the first 30,000 characters. The page is listed under the reply.',
          'Pages, search results and the text made from an attached sound or video file are checked when Chat reads them, against your Security Gateway policy. One that contains instructions aimed at the assistant is withheld from the model, the reply says “Withheld something it read”, and the reply goes on.',
        ],
      },
      {
        heading: 'Making images, files, videos and music',
        body: [
          'Ask a chat model for a picture and it makes one in the same reply, up to four per reply. It uses your own image model if you chose one under Your models, otherwise your plan’s image model on a subscription chat, otherwise the organization’s.',
          'You can also pick a model from “Make images” in the model picker (it lists the image models you are granted under the current “Working under”). The message box then asks for a description; send it and the reply is the image. Generation takes from a few seconds to about a minute. Under the image are its size, a download link, the description the provider actually used, and what it cost.',
          'Images are kept in Janus, encrypted, so they stay after the provider’s own link expires; only you can open them, and deleting the chat deletes them (for good 30 days later, like the chat). You can switch back to a chat model in the same chat to talk about the image; the chat model is told an image was made, but does not see it. Image models make new images from a description; editing an attached image is not supported yet.',
          'A model can hand you whole files, such as code, web pages, games, documents or data, up to 10 per reply and 2 MiB each. A complete HTML or SVG page in a reply becomes a file card too. A file card has Preview (in a sandbox with no access to Janus), Download and Copy, and code blocks have Download.',
          'With Create videos and Create music, a model makes short videos or music clips with the organization’s video or music model, or your own under Your models, at most two per reply. They play in the thread and can be downloaded. Each one is its own metered request, billed like an image, and a video can take a few minutes.',
        ],
      },
      {
        heading: 'Memory',
        body: [
          'When you ask the assistant to remember something, it saves it to your memory and reads it in later chats; ask it to forget something and it removes it. A Memory updated note under the reply lists what changed, with a Manage memory button.',
          'Memory in the settings drawer lists everything saved. You can add, edit or delete entries, delete them all, or switch off Remember things across chats. Memories are encrypted and only you can see them; administrators cannot read them. Temporary chats never use or update memory, and an administrator can turn Memory off for everyone.',
        ],
      },
      {
        heading: 'Deep research',
        body: [
          'Deep research, under This message in Tools & models, has the model run many searches (up to 20), read the most relevant pages and write a cited report. It takes a few minutes. The report is also saved on the reply as a Markdown file you can download.',
          'It applies to the next message only and shows as a chip above the message box, which you can remove before sending. It needs a chat model and cannot be combined with Compare.',
        ],
      },
      {
        heading: 'Comparing models',
        body: [
          'Compare, under This message in Tools & models, sends your message to two or three chat models at once and shows their answers side by side, each with its output tokens, cost and time. Each model is billed for its own reply, and Stop stops all of them. Image models cannot be compared.',
          'The first model you pick is marked Preferred, and the chat goes on from its answer. Choose Prefer this answer under another reply to continue from that one, with its model, instead; the other answers stay as versions you can switch to. Compare stays on until you remove its chip above the message box.',
        ],
      },
      {
        heading: 'History, pinning and temporary chats',
        body: [
          'Chats are listed by date with pinned chats at the top. Each row has a menu to rename, pin or delete; double-click a title to rename it. When you delete a chat it disappears at once; Janus removes it and its files for good 30 days later. Search finds chats by whole words in titles, messages and attached documents.',
          'A Temporary chat (the toggle in the header of a new chat) is never listed or searchable, and is removed once it has been idle for as long as your administrator set (a day by default; hover the toggle to see the current setting). It never uses or updates memory, and starting one does not change the model new chats start on. It still goes through the gateway and is metered like any other request.',
        ],
      },
      {
        heading: 'Saved prompts',
        body: [
          'Type / in the message box to pick a saved prompt; Enter or Tab inserts it, arrows move between them. A prompt can ask for values with {{name}}: you fill them in before it is inserted, and blank ones stay in the text to fill by hand. Manage your prompts from the prompts button in the chat header, or turn a message you sent into one with “Save as prompt”. Prompts are yours only and stored encrypted.',
        ],
      },
      {
        heading: 'Sharing and exporting',
        body: [
          'Share & export in the chat header makes a read-only link that anyone signed in to this Janus who may use Chat can open. The link shows the chat as it was when you made it; your instructions, tokens and costs are left out. Stop sharing removes the copy straight away, and deleting the chat ends all of its links. Temporary chats cannot be shared.',
          'Export downloads the messages you see as Markdown or JSON (with the model, tokens and cost of each reply, this chat’s instructions, and files by name), or opens the print dialog so you can save a PDF. Your administrator can turn share links or export off; printing is always available.',
        ],
      },
      {
        heading: 'Cost before you send',
        body: [
          'While you type, the message box shows about how many input tokens the next message sends and, for priced models, what that input costs. Every message re-sends the conversation, which is why long chats and large files cost more per message. The reply is billed on top at the model’s output rate; models from your own plans show tokens only. If a quota you are under is close to its limit or used up, a line above the box says so and when it resets.',
          'The tokens and cost under a reply cover the whole reply: every tool and research round, images, video, music, transcripts and descriptions it made, and the chat’s title. The chat header shows what this chat has cost so far — every reply, including other branches and Compare answers — and how much of the model’s context window it fills. When the conversation no longer fits, the oldest messages are left out of the request, and the thread says how many earlier messages were not sent.',
        ],
      },
      {
        heading: 'What the notes under a reply mean',
        body: [
          'Each reply shows the model that answered, tokens in and out, cost, speed and a link to its row in the request log. A short note appears when something worth knowing happened: an auto-routing router or alias answered with a different model (and why), a fallback model answered, Janus adjusted the thinking level, a security policy flagged, redacted or stopped the reply, or Child Safety’s tutor held back the answer. A reply a policy declines is shown as a calm notice, not an error.',
          'In the request log, a reply’s first call has the client janus-chat; its extra calls are janus-chat-tool (tool rounds), janus-chat-research (deep-research rounds), janus-chat-media (images made by a tool, video, music, transcripts and descriptions) and janus-chat-title. All calls of one Chat conversation carry the conversation as X-Janus-Session, so auto-routing keeps the conversation on one route.',
          'Tool use is shown with the reply: what was searched and the sources, pages read, code that ran, images, files, videos and music made, sound transcribed or video watched, and memory changes.',
          'Administrators cannot read chats. Messages, instructions and files are encrypted at rest. Administrators set up Chat in Administration → Chat; see Setting up Janus Chat.',
        ],
      },
    ],
  },
  {
    slug: 'admin/chat',
    title: 'Setting up Janus Chat',
    summary: 'Who can use Chat, which models it offers, its tools, and the instructions every chat starts with.',
    group: 'Admin guide',
    sections: [
      {
        heading: 'Turning Chat on',
        body: [
          'Janus Chat runs only when the gateway starts with JANUS_CHAT_PREVIEW=true (false by default). Without it, Chat is hidden from everyone and Administration → Chat shows a banner saying so; settings you save there apply once it is on.',
          'Administration → Chat → General → “Janus Chat is on” turns Chat off for everyone without a restart. Nothing is deleted: turning it back on restores every conversation. Administrators configure Chat but can never read anyone’s chats, memories or files.',
        ],
      },
      {
        heading: 'General',
        body: [
          'Default model is what a person starts with until they pick their own; left on First available model, it is the first model in the picker. Instructions for every chat (up to 8,000 characters) are sent first in every conversation, before each person’s own instructions. Suggestions on the empty chat screen are up to 8 starter prompts of up to 200 characters each.',
          'Under Features, Share links lets people share a read-only copy of a chat with others signed in to this instance; turning it off stops new links, and links already made keep working until their owner stops sharing or deletes the chat. Export lets people download chats as Markdown or JSON; printing stays available either way. Personal plans in Chat lets people chat with their own connected vendor plans (ChatGPT, Grok…); those are paid by their owner, show $0, and still count in usage and reports. Turned off, plan models leave the picker and are refused.',
          'Keep temporary chats for sets how long an idle temporary chat is kept before it is deleted: 1 hour, 6 hours, 1 day (the default), 3 days, 7 days or 30 days.',
        ],
      },
      {
        heading: 'Models',
        body: [
          'The Models tab lists every model granted to someone, and how it appears in the Chat picker. People only ever see the models they are granted. In Chat hides a model from the picker and Chat refuses it; it stays usable through the API. Category (General, Fast, Reasoning, Coding, Vision, Local & private, Creative) becomes the model’s group in the picker, Order (0–999) sorts it, lower first, and Recommended adds a badge.',
          'Sees images, Hears sound and Sees video decide whether Chat sends the model the images, sound and video people attach. Automatic uses the model’s listed modalities, or guesses from its name. Set Yes for a self-hosted model served under your own name, for example a vision model on vLLM, so attachments reach it. A model set to No gets a note, a transcript or a description instead (see Tools).',
          'The friendly chat name (up to 60 characters) and description (up to 200) are set per grant, with Chat name and description on a grant row in Administration → Gateway → Grants, so the same model can read differently for different teams. When several grants apply, the most specific one with a name or description wins; reports keep the catalog name.',
        ],
      },
      {
        heading: 'Tools',
        body: [
          'Tools from the model’s provider (Web search, X search, Read web pages, Run code) are on by default. Web search, X search and Run code run on the provider’s own servers, and only for models whose provider offers them; any extra provider fee is included in each reply’s cost. Read web pages works with every chat model: Claude and Gemini use their provider’s reader, and every other model uses Janus’s own, which opens only public internet addresses, fetches up to 5 MB and reads the first 30,000 characters. Switching a tool off here turns it off for everyone; people can also turn any tool off for one chat.',
          'Create images (Let chat models make images) lets any chat model make pictures. Image model for Create images is used when the chat is not on a personal plan with its own image model; left empty, it is the first image model the person may use, billed to their Working under.',
          'Create files, Memory, Create videos and Create music are run by Janus for any chat model that can call tools. Video model and Music model choose the models for videos (xAI Grok Imagine, Google Veo, OpenAI Sora) and music (Google Lyria); left empty, the first one the person may use. Videos and music are their own metered requests, billed like images; files and memory cost nothing extra. The video and music settings need a video or music model that is enabled and granted.',
          'Transcribe sound with picks the speech-to-text model that turns attached sound into a transcript for models that do not hear it. Describe videos with picks a model that watches video (set Sees video on the Models tab) to describe attached videos for models that do not watch them; without one, a video’s sound track is transcribed. Both are made once per file.',
        ],
      },
      {
        heading: 'Web search for self-hosted models',
        body: [
          'Models whose provider runs no search (vLLM, llama.cpp, Ollama…) can search through a service you set up under Web search for other models. It is off by default; providers with their own search keep using it. Choose a search service: SearXNG (self-hosted and free; add json under search → formats in its settings.yml and enter its address), Brave Search, Exa or Tavily (paste an API key; it is stored encrypted and never shown again, and Remove key deletes it).',
          'Results per search is 3, 5 (the default), 8 or 10; more results give the model more to work with but cost more tokens. Test search runs a sample search with the values on screen before you save. Then turn on Use this search for models without their own. The status beside the heading shows whether search is in use, and with which service.',
          'To set search up with the deployment instead, run SearXNG next to Janus and pass its address in JANUS_CHAT_SEARCH_SEARXNG_URL (in the Helm chart, under extraEnv). At startup Janus turns search on with it once and shows it as Installed with Janus. It never replaces a search service you already use, and your later changes are kept across restarts.',
        ],
      },
      {
        heading: 'Who can use Chat',
        body: [
          'On Janus Business, Who can use Chat limits Chat to Everyone or to Only these groups, teams and people. Message for people without access (up to 300 characters) is shown instead of Chat; without one, they are told to ask an administrator. Administrators can always use Chat. People outside the restriction can still open share links others send them (read-only). Someone who loses access can still list and revoke the links they made, but cannot make new ones.',
          'On Community, everyone with an account can use Chat, and only the instance-wide on/off switch applies. A restriction saved under a Business license keeps applying after the license expires or the gateway is downgraded to Community; the Who can use Chat tab then shows a warning with Let everyone use Chat, which lifts it. Saving a new or changed restriction needs Business. Chat being turned off is always honoured.',
        ],
      },
      {
        heading: 'Search index, audit and environment',
        body: [
          'Chat search stores keyed hashes of words, never the words. After changing JANUS_ENCRYPTION_KEY, rebuild the index with POST /api/v1/admin/chat/reindex. Every change to these settings is recorded in the audit log as chat_settings_updated; search API keys are never written there.',
          'JANUS_CHAT_PREVIEW (default false) must be true for Chat to run. JANUS_CHAT_AUTO_TITLE (default true) names each chat after its first reply with a separate, metered janus-chat-title request; set false to turn it off. JANUS_CHAT_MAX_UPLOAD_BYTES (default 20 MiB) caps one attachment, and never exceeds JANUS_STORAGE_MAX_OBJECT_BYTES. JANUS_CHAT_SEARCH_SEARXNG_URL is the address of a SearXNG installed with the deployment. Attachments and made files are kept in Administration → Settings → General → File storage.',
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
          'Choosing a provider fills in its documented base URL and a suggested name. Both stay editable: change the URL for a proxy or a regional endpoint, and name the upstream whatever you like. Self-hosted engines have no fixed address, so their field shows the engine’s default port as an example.',
          'A base URL that ends in an API version (…/v1, Google’s …/v1beta/openai) is used as the API root, the way the provider documents it for OpenAI clients; Janus appends /chat/completions or /models without adding another /v1. A base without one, such as https://openrouter.ai/api, gets /v1 added.',
        ],
      },
      {
        heading: 'Procedure',
        body: [
          '1. Administration → Gateway → Upstreams → Add upstream. Choose the provider type: it is immutable afterwards, because it determines how every stored model is addressed.',
          '2. Choose Create and discover models. Janus starts discovery right away and records what the provider reports. For a self-hosted engine (vLLM, llama.cpp, Ollama, TEI), check the server flags in “Self-hosted engines” first: they decide whether speed, cache savings and the context window are exact.',
          '3. Administration → Gateway → Models. New models arrive as “Pending approval” and are not served until you enable them. A model the upstream stops listing is marked stale. When a stale model is listed again it returns to the state it had before — enabled, disabled or awaiting approval — so a short gap in the listing never switches a model off for good. If an upstream that has enabled models answers with an empty list, Janus treats the check as failed and changes nothing; three failed checks in a row raise an “unreachable” alert. When enabled models stop being listed, administrators get a notification naming them.',
          '4. Open Edit to review automatic metadata and pricing warnings, set any overrides, then enable. A model cannot be enabled until it has a rate card ($0 is allowed for self-hosted models); unsupported tiers and per-image rates are never flattened into token prices.',
          '5. Administration → Gateway → Grants. Grant the model to a person, a group, a team, a service token, or everyone.',
        ],
      },
      {
        heading: 'Pricing from the bundled reference seed',
        body: [
          'Rate cards are always per million tokens; see “Pricing by modality” for how speech, transcription, image and realtime models — which providers quote per minute, per character or per tier — translate into that unit. Janus ships a reference rate-card seed for common OpenAI, Anthropic, Mistral and Google Gemini models so a fresh install does not need every price typed by hand. Discovery prices a model from this seed automatically when the upstream is the provider’s official endpoint (api.openai.com, api.anthropic.com or generativelanguage.googleapis.com); xAI and OpenRouter publish their own prices, which discovery reads too. GET /api/v1/admin/ratecards/reference lists the seed; POST /api/v1/admin/ratecards/apply prices every discovered model that matches the seed by name and has no rate card yet — use it for models reached some other way, such as Mistral or a proxy. It is available through the API only. Models you already priced are never touched, and re-applying is a no-op.',
          'The seed reflects public list prices at the time it was authored — it is a starting point, not a live price feed. Verify against the provider’s current pricing before relying on cost attribution, and adjust any figure per model afterwards.',
          'The seed also carries each model’s context window (context_window_tokens). Applying it fills in the context window for every matching model that does not have one yet — including models you already priced by hand — but never overwrites a value that is already set.',
        ],
        code: {
          language: 'bash',
          code: 'curl -X POST ${JANUS_PUBLIC_URL}/api/v1/admin/ratecards/apply \\\n  -H "Cookie: janus_session=…" -H "X-Janus-CSRF: …"',
        },
      },
      {
        heading: 'Google Gemini',
        body: [
          'Choose Google Gemini (AI Studio key) and paste an API key from Google AI Studio. Janus talks to Google’s OpenAI-compatible endpoint, https://generativelanguage.googleapis.com/v1beta/openai, so chat, streaming, function calling, image input and embeddings use the OpenAI request format unchanged.',
          'Discovery lists the models the key can call, with their context windows. Live-API (realtime voice) models are left out because the OpenAI-compatible endpoint cannot call them.',
          'Gemini reports thinking tokens only in its total, and Google bills them at the output rate, so Janus meters output as total minus prompt: the request log and quotas match the bill. Implicit prompt-cache hits are recorded as cached input.',
          'Prices for Gemini 2.5 Flash and Flash-Lite, 3 Flash Preview, 3.1 Flash-Lite, 3.5 Flash and Flash-Lite and Embedding 2 come from the bundled reference. Models priced by prompt size (2.5 Pro, 3.1 Pro) or with a scheduled price change (3.6 to 3.8 Flash, which double on 1 January 2027) arrive unpriced: enter the price yourself.',
          'For a Google Cloud project with a service account instead of an AI Studio key, use Google Vertex AI.',
        ],
      },
      {
        heading: 'Thinking settings and provider limits',
        body: [
          'Chat clients such as Open WebUI send the same settings to every model: a reasoning effort (none, minimal, low, medium, high, xhigh), a temperature from a preset, and tools. Providers each refuse a different part of that. GPT-6 Sol and Astra take low to xhigh only; Gemini takes none to high but not minimal or xhigh; Grok refuses none; Claude 5 refuses any temperature or top_p; GPT-5 and GPT-6 take only the default temperature and refuse function tools with a reasoning effort on chat completions.',
          'Janus fits the request instead of returning the provider’s error. When a provider refuses a setting and says what it refused, Janus changes it and sends the request again before anything reaches the caller (a refused request is not billed): a reasoning level becomes the nearest level the model takes, a refused sampling setting is left out, and a GPT-5 or GPT-6 request with tools goes through OpenAI’s Responses API and comes back as a normal chat completion. What it learns is kept per model for a day on each gateway replica, so later requests go out fitted without a refusal.',
          'Every change is shown on the response in the X-Janus-Reasoning-Adjusted header and on the request in the request log, for example “minimal->low temperature->omitted tools->responses”. An error Janus cannot fix is passed through unchanged.',
          'Anthropic: reasoning_effort is translated into the thinking form each Claude model takes. Claude 4.6 and later (all of Claude 5) get adaptive thinking with that effort and a summary of the thinking, which Janus returns as reasoning_content (none and minimal ask for the lowest effort, low; adaptive models cannot switch thinking off), and the output cap is raised to 32,000 tokens when the caller set none. Earlier Claude models get a thinking budget (1,024 to 63,999 tokens by level, kept within the model’s output limit) and none leaves thinking off. Thinking needs the default temperature, so a temperature is left out when thinking is on. A request that sends Anthropic’s own thinking object is passed through as it is.',
          'Gemini: with thinking on, Gemini signs each tool call and refuses the next turn when the signature is missing, which most chat clients drop. Janus remembers each signature as it relays the tool call and puts it back when the call is replayed; a call it did not see (another replica, an older conversation) gets the placeholder Google documents for that case.',
        ],
      },
      {
        heading: 'The same model on more than one upstream',
        body: [
          'Two upstreams can serve a model under the same name — the same vLLM model on two nodes, or an old and a new endpoint while a pod moves. Each copy is its own catalog entry with its own rate card and grants, and the Models page marks it with a “+N upstream(s)” badge. A request for the name goes to the most recently discovered copy the caller is granted, so granting the new copy is enough to move traffic; the old copy’s grants never apply to the new one, and an ungranted copy is never used as a fallback.',
          'If only the address changed, edit the existing upstream’s base URL instead of adding a new one: the upstream keeps its models, rate cards and grants, and traffic follows the new address immediately.',
          'For one stable name users never have to change, create a managed model on the Managed models page, grant it, and repoint it whenever the model moves. Grants on a managed model are independent of the model it points at, so repointing moves every caller without touching a single grant.',
          'To use several copies at once, make the managed model a load-balanced pool (below).',
        ],
      },
      {
        heading: 'Load-balanced pools (Business)',
        body: [
          'A managed model can list several models — typically the same model served by several vLLM or llama.cpp servers — and Janus spreads its traffic across them. Open Managed models → Edit, and under Load balancing use Add a model. Callers keep using the one alias name; grants, quotas and the fallback stay on the alias, and the request log records which server actually answered.',
          'Balancing: Failover sends traffic to the servers with the lowest priority number and keeps higher numbers as standbys; servers that share a priority also share conversations. A server added under Failover gets the next number, so it starts as a standby; under the other policies it joins the serving tier. Round robin takes turns by weight. Least loaded picks the server with the shortest queue (waiting requests count double). Context aware compares how much of each server’s context capacity is taken by the conversations it is running, so one 100,000-token conversation counts for more than several short chats — set a server’s Context capacity to override what it reports. Whatever the policy, each server’s Priority (0–100) groups servers into tiers: lower numbers serve first and higher numbers only take traffic when every lower-priority server is out.',
          'Keep conversations together: every turn of a conversation goes to the server that already holds its prompt cache, which makes follow-up turns much faster. Janus recognises a conversation from an X-Janus-Session, X-Session-Id or X-Conversation-Id header, the OpenAI prompt_cache_key field, or — when a client sends none — from its tools, system prompt and first message, which stay the same on every turn. Bounded (the default) moves a conversation to its second-choice server only while its own server is busier than the pool average by the chosen percentage and by a meaningful absolute amount; Strict moves it only when its server is out of service; Off balances every request separately.',
          'Every gateway replica makes the same choice for the same conversation without sharing any state on the request path, so pools work the same with one replica or many. Live load is read every 2 seconds from each server’s /metrics endpoint (vLLM: requests running and waiting, KV-cache use and size; llama.cpp: run llama-server with --metrics, and --slots for context figures). Servers without metrics (cloud APIs, Ollama) are balanced on Janus’s own in-flight counts and marked “No live metrics”. The full flag list per engine is in “Self-hosted engines”.',
          'Failures: a server that refuses a request before sending anything (connection error, 502, 503, 504, 429) is skipped and the request goes to the next server in the same call, so the caller does not see the error; nothing is retried once a response has started. Three consecutive failures (connection errors, 502, 503, 504 — not 429, which means the server is busy) take a server out for 5 seconds, doubling up to 2 minutes, and every replica takes it out together. When every server is out, the alias’s fallback model serves (response header X-Janus-Fallback-Reason: pool_exhausted); without a fallback the request fails with a message naming the pool.',
          'When a member fails before answering, Janus retries on the next member only if that member gets the same Security Gateway checks, isn’t blocked by Child Safety for the caller, and the caller’s quota on it isn’t used up; a member that fails any of these is skipped. Bind a stricter policy to the pool’s name rather than to one member if every member should be checked the same way. Security Gateway classifiers are never pool members.',
          'When a server is taken out of rotation, administrators get one “Pool member taken out of rotation” alert per ejection.',
          'Pooled responses carry X-Janus-Pool-Member (model@upstream) and X-Janus-Pool-Reason (policy, affinity, spill, moved, retry or only). The editor’s Servers right now panel shows each server’s state, queue, context use and prompt-cache hit rate, refreshed every few seconds.',
          'Licensing: a pool of more than one model, or any policy other than Failover, needs a Business license with load balancing. When the license lapses, existing pools keep balancing and can still be shrunk, re-weighted or have servers switched off; adding or re-enabling servers and changing the policy are blocked.',
        ],
      },
      {
        heading: 'Verification',
        body: [
          'Administration → Settings → System status shows each upstream’s last check, latency, and last error. A user should now see the model in GET /v1/models within seconds.',
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
    slug: 'admin/self-hosted-engines',
    title: 'Self-hosted engines',
    summary:
      'Server flags that give Janus exact usage, real speed, the right context size and live load for vLLM, llama.cpp, Ollama and TEI.',
    group: 'Admin guide',
    sections: [
      {
        heading: 'Why engine settings matter',
        body: [
          'Janus works with any of these engines out of the box, but several figures are only as good as what the engine reports. Exact token counts and cost, the speed users see on the Models page, the context window on each model, prompt-cache savings and live load for pools all come from the engine. Without them Janus falls back to its own estimates, which are marked as such but are less accurate.',
          'Each engine section below lists what to turn on, what Janus reads, and what you lose without it. After changing flags, restart the server and run discovery again from Administration → Gateway → Upstreams so the context window is refreshed.',
        ],
      },
      {
        heading: 'vLLM',
        body: [
          'Provider type: vLLM. Base URL: the server root with /v1, for example http://vllm.example:8000/v1.',
          '`--enable-per-request-metrics` — returns each request’s timing (prefill time and decode time) with its usage. Janus records decode speed as reported by the provider; without it, speed is measured on the gateway’s clock and includes prompt processing, so long prompts make the model look slow. Requires engine statistics: do not also pass `--disable-log-stats`.',
          '`--enable-force-include-usage` — puts a usage block on every stream, even when a client does not ask for one. Janus already asks on the requests it forwards, so this is a safety net for exact metering.',
          '`--enable-prompt-tokens-details` — reports cached prompt tokens, so prompt-cache savings are billed at the cached rate and shown on the request log. Pair it with `--enable-prefix-caching`.',
          '`--max-model-len` — the context window Janus reads from /v1/models (max_model_len). Set it to what the server can actually hold; discovery copies it onto the model.',
          'Live load for pools: /metrics is on by default. Janus reads requests running and waiting, KV-cache use and prefix-cache hit rate from it every 2 seconds.',
          '`--served-model-name` — the model name users call. Keep it identical on every server of a pool so they appear as the same model.',
        ],
        code: {
          language: 'bash',
          code: 'vllm serve /models/qwen3-32b \\\n  --served-model-name qwen3-32b --max-model-len 32768 \\\n  --enable-prefix-caching --enable-prompt-tokens-details \\\n  --enable-force-include-usage --enable-per-request-metrics',
        },
      },
      {
        heading: 'llama.cpp (llama-server)',
        body: [
          'Provider type: llama.cpp server. Base URL: the server root with /v1, for example http://llama.example:8080/v1.',
          'Speed and token counts: llama-server returns its own timings (prompt and generation tokens per second) with every response. Janus records them as reported by the provider with no extra flag.',
          '`--metrics` — enables /metrics, which pools use for live queue length and KV-cache use. Without it the server is balanced on Janus’s own in-flight count, and the Servers right now panel marks its load “estimated”.',
          '`--slots` — enables /slots, which gives pools each server’s real context capacity and current use for context-aware balancing.',
          '`--ctx-size` and `--parallel` — the served context is ctx-size, which /v1/models reports as meta.n_ctx and Janus copies onto the model. With `--parallel N` and a non-unified KV cache (`--no-kv-unified`), each request gets ctx-size ÷ N, so set ctx-size to N × the context you want per request.',
          '`--alias` — the model name users call; without it the name is the GGUF file path. Keep it identical across a pool.',
          'Prompt caching is on by default (cache_prompt), and cached tokens are reported as prompt_tokens_details.cached_tokens.',
        ],
        code: {
          language: 'bash',
          code: 'llama-server -m /models/Qwen3-32B-Q4_K_M.gguf --alias qwen3-32b \\\n  --ctx-size 65536 --parallel 2 --no-kv-unified \\\n  --jinja --metrics --slots',
        },
      },
      {
        heading: 'Ollama',
        body: [
          'Provider type: Ollama. Base URL: the server root without /v1, for example http://ollama.example:11434. Discovery reads /api/tags and /api/show; traffic goes through Ollama’s OpenAI-compatible endpoint.',
          'Speed: the OpenAI-compatible endpoint returns no timings, so speed is measured on the gateway’s clock (marked Calculated by gateway in the request log). Treat it as a lower bound.',
          'Context window: discovery reads the model’s trained context length from /api/show, but Ollama serves only `num_ctx` (by default a few thousand tokens) unless told otherwise. Set `OLLAMA_CONTEXT_LENGTH` on the server, or a `PARAMETER num_ctx` in the Modelfile, and override the model’s context window in Administration → Gateway → Models to match. Otherwise long prompts are silently truncated by Ollama.',
          'Pools: Ollama has no /metrics endpoint, so pooled Ollama servers are balanced on Janus’s own in-flight counts. Set `OLLAMA_NUM_PARALLEL` to how many requests each server should run at once.',
          '`OLLAMA_KEEP_ALIVE` — keep models loaded (for example 24h). A model that has to load first makes the first request after idle very slow, which shows up in the speed and latency figures.',
        ],
      },
      {
        heading: 'Hugging Face TEI',
        body: [
          'Provider type: Hugging Face TEI. Base URL: the server root, for example http://tei.example:8080. TEI serves one embedding, reranker or classifier model per process; discovery reads /info and fills in the model name, its input cap (max_input_length) and, for a guard classifier, its security role.',
          '`--served-model-name` — the model name users call; without it Janus uses the last part of the model id.',
          'TEI has no chat endpoint: a TEI upstream only proxies /v1/embeddings. Use it for embeddings and for the Prompt Guard classifier (see Security gateway).',
        ],
      },
      {
        heading: 'Other OpenAI-compatible servers',
        body: [
          'SGLang, LM Studio, LocalAI, TGI’s OpenAI endpoint and similar servers work with the Other OpenAI-compatible endpoint provider type. Check three things on the first requests in the request log: the counting method should say Reported by upstream (not Estimated from byte count), streamed requests should carry token counts (the server must honour stream_options.include_usage), and the model’s context window should be filled in after discovery. Set any missing context window by hand in Administration → Gateway → Models.',
        ],
      },
      {
        heading: 'Checking an engine is set up well',
        body: [
          'Open the request log, filter by the model, and open a recent request. Counting method should be Reported by upstream. Throughput should say Measured by upstream for vLLM (with per-request metrics) and llama.cpp; Calculated by gateway means Janus timed it. Cached input tokens should appear on repeat prompts if prefix caching is on.',
          'For pools, Managed models → Edit → Servers right now should mark each vLLM or llama.cpp server’s load “live” and show its queue and context use; “estimated” means its metrics are not being read.',
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
        heading: 'What a quota measures',
        body: [
          'Administration → People & Usage → Quotas. Each quota caps input tokens, output tokens, spend (USD) or requests over a calendar day, week or month, or a rolling 24 hours, 7 days or 30 days. It applies to a person or a team, and covers all models or one model, managed model or router; a managed model or router counts everything sent to it, whichever model served.',
          'Warning alerts go out at 80% and 95% of the limit unless you set your own percentages (up to 10); a breach alert always fires at 100%.',
          'A window that needs more usage history than Janus keeps (JANUS_USAGE_RETENTION_DAYS, 365 days by default) is refused, because it would undercount. Team quotas need that history even for calendar windows.',
          'Service tokens can be capped too, from the Quotas page or with subject_type service_token in POST /api/v1/admin/quotas.',
          'Rate limits (Quotas → Rate limits → New rate limit) cap requests per minute on an endpoint. Applies to: people (everyone or one person) or a service token; a service-token rule throttles only that integration. Rate limits are enforced while the per-endpoint rate limits feature flag is on.',
          'A Janus Chat reply counts as one request toward rate limits and request-count quotas, however many calls it makes (tool rounds, deep research, media, title). Tokens and cost count for every call. With Compare, each model’s answer counts as one request.',
        ],
      },
      {
        heading: 'Set limits with headroom',
        body: [
          'The default breach behaviour lets an in-flight request finish, so a single large streaming completion can overshoot the limit. Choose a limit that tolerates one maximum-size request beyond it.',
          '“Cut the stream immediately” stops a stream as soon as the limit is reached, counting the stream’s own tokens, so the overshoot is at most the last chunk — but it cuts users off mid-answer. Reach for it only when a hard ceiling matters more than the experience.',
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
          'Each team’s page in Administration → People & Usage → People → Teams has a “Team leads may edit quotas” checkbox under Administrator controls. When an administrator ticks it, that team’s lead gets a quota-management panel on their team page and can add, edit (limit and whether to cut streams) and remove quotas for that team only — never for other teams, individuals, or global settings. Leads cannot create quotas while license creation is paused, and cannot create or change quotas of a deleted team.',
          'Every change a lead makes is audit-logged with their identity, exactly like an admin edit. Untick the checkbox to withdraw the delegation at any time; existing quotas keep enforcing either way.',
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
          'Speech models are quoted two ways. Token-native models (gpt-4o-mini-tts) bill text input and audio output per million tokens, which the provider also expresses as an approximate per-minute cost. Janus can meter them only when the caller streams with stream_format: sse, whose final event reports the tokens; a plain audio response carries no usage.',
          'A speech response with no usage, which includes every response from character-billed models (tts-1, tts-1-hd), is recorded as not metered (unmetered_modality) with zero tokens and zero cost, so a rate card has no effect on it; attribution for those requests depends on the provider’s invoice.',
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
              cells: ['tts-1', 'Input text', 'per 1M characters', '15.00', 'not metered'],
              highlight: 4,
            },
            {
              group: 'tts-1-hd',
              tag: 'Chars',
              cells: ['tts-1-hd', 'Input text', 'per 1M characters', '30.00', 'not metered'],
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
          'Image models bill several modalities on one request: text input (the prompt), image input (edits and references) and image output. The provider publishes each as its own per-1M-token rate, so a model appears as a block with one sub-row per modality. Batch pricing is a separate tier at half price — use the tab that matches how your callers submit work. A Janus rate card has one input rate, so text and image input are priced at the same rate: enter the image-input rate if most requests are edits, otherwise the text rate. Image output is priced as output tokens. When a provider reports the request cost directly, that figure is used instead (accounting method upstream_reported_cost).',
          'An image response that reports neither token usage nor a cost is recorded as not metered (unmetered_modality); check the provider’s invoice for those requests.',
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
          'Transcription is quoted per minute of audio. Token-native models (gpt-4o-transcribe) also publish per-1M-token rates for audio input and text output, which map directly onto a Janus rate card. Minute-billed models (whisper-1) report seconds of audio, which Janus records as one input unit per started second: enter the per-second price × 1,000,000 as the input rate (0.006 per minute = 100.00). Response formats that return plain text (text, srt, vtt) carry no usage, so Janus estimates output from the transcript’s length and records no input.',
        ],
        pricing: {
          variant: 'transcription',
          caption: 'Speech-to-text',
          unit: 'USD; per-minute is the provider’s estimate, audio-in and text-out are per 1M tokens',
          columns: ['Model', 'Use case', 'Per minute', 'Audio in', 'Text out'],
          rows: [
            { cells: ['gpt-4o-transcribe', 'Accuracy', '0.006', '2.50', '10.00'], highlight: 3 },
            { cells: ['gpt-4o-mini-transcribe', 'Cost', '0.003', '1.25', '5.00'], highlight: 3 },
            { cells: ['whisper-1', 'Per second of audio', '0.006', '100.00', '—'], highlight: 3 },
          ],
        },
      },
      {
        heading: 'Realtime (speech-to-speech)',
        body: [
          'Janus does not proxy Realtime sessions. They run over WebSocket or WebRTC, which the gateway does not carry, so realtime models cannot be used through Janus and their usage never reaches the request log. The provider’s rates are shown below for reference only.',
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
          'Rules are evaluated on ingress, before grants and quotas, so a blocked request never reaches a provider and never costs anything. They apply to the Decisions API too, and to Janus Chat: Chat’s requests carry the browser’s address, its X-Forwarded-For (from trusted proxies only) and its headers.',
          'Give each rule a Reason shown to the caller: a blocked caller receives it in the error response. Without one, the error names the rule and the condition that matched. The list shows how many requests each rule has blocked and when it last fired.',
        ],
      },
      {
        heading: 'What they are not',
        body: [
          'Apart from the token and the source IP (the connecting address, or the forwarded address when the connection comes from a trusted proxy in JANUS_TRUSTED_PROXIES), every signal is supplied by the client and can be forged. The X-Forwarded-For chain condition reads the header only when the connection comes from a trusted proxy (JANUS_TRUSTED_PROXIES); a client talking to Janus directly cannot set it, and without trusted proxies the condition never matches. The proxy itself adds to whatever the client sent, so match the addresses your proxy appends. A rule that blocks a user agent stops a tool that identifies itself honestly; it does not stop someone who changes the string.',
          'Use rules to keep sanctioned tooling on the rails. Do not treat them as a containment boundary.',
        ],
      },
      {
        heading: 'Test before you ship',
        body: [
          'Administration → Safety & Security → Policy rules → Test a request: fill in a sample user agent, source IP, path, method, model and token prefix, and see which saved rule and which condition would fire, before any real traffic is affected. Header and X-Forwarded-For conditions can only be tested through POST /api/v1/admin/rules/blocking/test.',
        ],
      },
    ],
  },
  {
    slug: 'admin/insights',
    title: 'Usage insights',
    summary:
      'Label requests by the kind of work they ask for (coding, legal, writing, …) and see the mix by team, model and person.',
    group: 'Admin guide',
    sections: [
      {
        heading: 'What it does',
        body: [
          "A classifier reads each sampled chat request and picks a category from your taxonomy, with a subcategory when that category has any. Administration → Usage insights shows the result: categories over time, each category's share and confidence, the subcategories, models and people behind it, the category mix per team, and the classifier's own speed and failures.",
          "Only the label, its confidence and the classifier's timing are stored. The prompt is sent to the classifier and never written anywhere by Janus. Results are deleted after the retention you set (180 days by default), and never kept longer than the request log (JANUS_USAGE_RETENTION_DAYS, 365 days by default).",
        ],
      },
      {
        heading: 'Choosing a classifier',
        body: [
          'Decision model (System One API). A decision model scores every category you offer and returns calibrated probabilities without generating text, so it is fast and its confidence means something. Janus speaks the System One API used by TypeSafe Jev, Bespoke Nimble, Laya and Cloudflare Clef, wherever they are served: TypeSafe (api.typesafe.ai), OpenRouter (/api/alpha/decisions, model ~typesafe/jev-latest), Ollama 0.35 or later (/v1/systemone, model nimble), or llama.cpp. It also speaks the OpenAI Decisions API (/v1/decisions, model gpt-6-luna): Janus picks the format from the provider, and Usage insights → Performance can override it per model. Add the server as an upstream like any other, then pick it as the provider. A hosted classifier receives your prompts; a self-hosted one keeps them inside your network.',
          'Chat model. Any chat model Janus can reach is shown the taxonomy and asked to answer in JSON. It needs no extra service, which suits small deployments, but it is slower than a decision model, costs generation tokens, and its confidence is its own estimate. A small, fast model is usually enough. An answer that is not one of your categories is recorded as an error, never guessed.',
          'Use Classifier → Try it to classify a sample with unsaved settings and compare classifiers and their speed before switching. Nothing is stored. Try it reads the text as a single message; to see how a follow-up is read with its conversation, use Auto-routing → Classifier → Try routing, which takes an earlier conversation (it uses the routing categories).',
          'One decision model can serve Usage insights and the Security Gateway (prompt injection and content safety) at once. See Classifiers: speed and accuracy for how Janus combines their questions and how to measure what your choices cost.',
        ],
      },
      {
        heading: 'Background or inline',
        body: [
          'Background (the default) labels requests after they are sent on, from a queue on each replica, so reporting adds no latency. When the queue is full a sample is skipped and counted as dropped.',
          'Inline labels the request before it is forwarded and returns the label in the X-Janus-Category response header (category/subcategory). The request waits at most the classifier timeout; a timeout or error never fails the request, it proceeds unlabelled and the miss is recorded. When more inline calls are in flight than the parallel limit, extra requests are labelled in the background instead, so a slow classifier cannot pile up latency. Auto-routing does not use this label; it asks its own questions (see Auto-routing).',
          'When a Security Gateway check already calls the same decision model about the same text, and that model takes questions together, the Usage insights question is added to that call in either mode. That happens for a first message (or for any message with Latest user message only), with Include the system prompt off, and when the label is a single question (subcategories off, or category and subcategory asked together). A follow-up read with its conversation is asked on its own, because the guards judge the new turn alone. A shared label costs no extra call or wait, is recorded as inline, and an answer Janus cannot use is recorded as a classifier error without affecting the check.',
        ],
      },
      {
        heading: 'Categories',
        body: [
          'Janus ships 22 general categories (Coding, Technical help, Data analysis, Extraction and formatting, Writing and editing, Creative writing, Summarization, Translation and language, Research, Learning and tutoring, Science and math, Business and strategy, Marketing and sales, Customer communication, Finance and accounting, Legal, Medical and health, HR and people, Security, Personal and lifestyle, General chat, Other). Built-ins can be renamed, re-described or disabled, not deleted. Other is always offered so a request that fits nothing is not forced into a wrong category.',
          "Add your own top-level categories for your industry, and subcategories one level deep: languages under Coding, Reverse engineering under Security. The description is what the classifier reads, so say what belongs and, when two categories are easy to confuse, what does not. A category's subcategories are also named in its description when the classifier picks the top level, so it knows that work belongs there.",
          'At most 26 top-level categories and 25 subcategories per category can be enabled at once. Deleting a category stops new labels; requests already labelled keep theirs.',
          'When the classifier accepts enough choices in one question, Janus asks for the category and subcategory together, offering every category and every subcategory as its own answer ("Coding", "Coding › Rust", …). That is one call instead of two and was the most accurate way to ask on every decision model we tested. A server with a lower limit (Ollama accepts 26 choices) is asked the category first and the subcategory second. Usage insights → Performance → Decision models shows the limit Janus detected for each classifier (Most choices per question) and lets you change it.',
        ],
      },
      {
        heading: 'Sampling and what is sent',
        body: [
          'Requests to classify sets the share of chat requests labelled. Text to classify chooses the latest message with the recent conversation as context (default), the latest user message only, every user message, or the whole conversation. With context, a follow-up such as “What about Ohio?” is labelled by what the conversation is about rather than read on its own; the last six earlier messages are sent (each user message cut to its first 1,000 characters and each assistant reply to its first 300, at most 4,000 characters in all, newest kept) with the latest message marked as the one to judge, and a first message is classified exactly as it is. Installs that had saved the old default (latest user message) were moved to the new one; Include the system prompt helps when applications put the task there. Only the most recent characters up to the limit are sent (Nimble reads about 8,000 tokens). Embeddings, images and audio are not classified, and neither are requests refused by quota, rate limits or the Security Gateway. Usage insights reads the caller’s own text (after any Security Gateway redaction). Instructions Janus adds — a router’s instructions and Child Safety’s instructions — are never classified, even with Include the system prompt on. Janus Chat messages are classified once; Chat’s tool rounds, research rounds, media calls and titles are not classified.',
          "Classifier calls are reported on the Usage insights page with their tokens and cost (from the provider's reported price, or the model's rate card for chat models). They never count against the caller's quota. Prometheus: janus_classifications_total{status,mode}, janus_classification_latency_seconds{mode,protocol} and janus_classification_queue.",
        ],
      },
      {
        heading: 'In the request log',
        body: [
          "Administration → Requests shows each request's label in the Classified as column and can filter by category, by category › subcategory, or by whether a request was classified at all. The request drawer says whether the label was produced inline or in the background, how confident the classifier was, and how long it took.",
          'Every request also records how long it waited on classifiers before it was forwarded and how many classifier calls that took (the Classifier wait column, hidden by default). That covers the Security Gateway guards and an inline label. Background labelling never delays a request, so it is not in that number; its own time, queue wait and call count are shown separately in the drawer.',
        ],
      },
    ],
  },
  {
    slug: 'admin/auto-routing',
    title: 'Auto-routing',
    summary:
      'Let Janus pick the model for each request: a classifier decides what kind of work it is and how hard, and a router sends it to the model you chose for that work.',
    group: 'Admin guide',
    sections: [
      {
        heading: 'What it does',
        body: [
          'A router is a model name callers use, like auto/legal or auto/smart. For each request, a decision model answers which category of work it is (from your routing categories) and, when a route needs it, how difficult it is on a 1–5 scale. The router then tries its routes top to bottom and sends the request to the first one that fits. Anything else goes to the router’s default model.',
          'Routing never fails a request. If the classifier is unsure, slow, busy or down, no route fits, or the chosen route cannot serve (its model was removed, or every server in its pool is out of service), the default serves it and the request log says why. For someone covered by Child Safety, a route whose model their age range blocks is skipped, like a route they aren’t granted. If the default is blocked, the first route they may use serves; if every model is blocked, they’re told the model isn’t available. Grants, quotas and security policies still apply to the model that serves, as below.',
          'Typical setups: keep legal and other sensitive specialist work on a self-hosted or fine-tuned model and send everything else to a frontier model; send easy everyday work to a cheaper model; or use one frontier model for everything and let Janus raise or lower how hard it thinks.',
          'New router offers four templates — Cost saver (auto/cost-saver), Keep specialist work on-prem (auto/legal), Effort scaler (auto/smart) and Coding by difficulty (auto/code) — and a Blank router. A template fills in categories, difficulty, thinking and instructions; you choose the models. New routers start as drafts.',
        ],
      },
      {
        heading: 'Routers are managed models',
        body: [
          'A router is a managed model of kind router, so the rest of Janus treats it like an alias. Callers need a grant on the router (Grants). Security policies bound to the router apply to every request sent through it, and policies bound to the model a route picks apply too. A quota on the router counts everything sent to it, whichever model served; a quota on a route’s model (or on an alias the route serves through) still applies and counts the traffic the route sends it. By default a caller over that quota is refused; tick “Skip this route when the caller is over a quota on its model” and the next route or the default serves instead. A caller already over a quota on the router is refused without a classifier call.',
          'A route can be limited to callers who are also granted its model (“only for callers also granted this model”), for example to keep an expensive model to one team while everyone else gets the default. When the router’s configuration cannot be read, the default serves and the policies bound to the default model and its upstream apply.',
          'When the default or a route is an alias or a pool, the router uses that alias as it is at the moment of each request — its current target, its fallback and its pool members — so repointing the alias needs no change to the router. The router is shown with a problem, and leaves the model list and the Chat picker, only when the alias itself cannot serve.',
          'The request log shows the router name the caller asked for, the model that served, and the reason (matched a route, same conversation, no route fit, classifier unsure, classifier too slow, …). With “Tell callers which route served” on, responses carry X-Janus-Route, X-Janus-Route-Reason and X-Janus-Route-Model (and X-Janus-Route-Instructions when instructions were added).',
        ],
      },
      {
        heading: 'Draft, shadow and live',
        body: [
          'Draft: not callable. Use the builder’s Test this router panel to see where prompts would go.',
          'Shadow: callable, but always served by the default. Janus classifies in the background (from the text the model receives, after any redaction) and records which route would have served, so the route mix and the classifier’s speed are visible before anything changes for callers. No added latency.',
          'Live: requests are routed. The decision waits at most the router’s time budget (800 ms by default); past it the default serves.',
        ],
      },
      {
        heading: 'Routing categories',
        body: [
          'Routing categories are what the classifier chooses between. Fifteen are built in, including Legal, which is kept apart from Specialist so legal work can go to its own model; you can add your own. Each has a name, what it includes, what it does not include, a minimum confidence, and test examples. The descriptions are what the classifier reads: say what belongs, and name the neighbours that do not and where they go. That is what stops misfiling; bare names were measurably worse in testing.',
          'Every enabled category is offered on every request, plus “none of these”. Offering only the categories a router uses pulls borderline requests into them; offering the near neighbours lets the classifier say “this is planning advice, not research”.',
          'At most 25 routing categories can be enabled at once. A category a router uses cannot be disabled or deleted until it is removed from that router’s routes. Built-in categories can be disabled or reset to their shipped wording, not deleted.',
          'A routing category can follow a Usage insights category instead of having its own description, so one definition serves both.',
          'Below a category’s minimum confidence, the answer does not count: the request is not routed by that category.',
        ],
      },
      {
        heading: 'The classifier',
        body: [
          'Any decision model can classify (Administration → Models, role “decision”). In testing, hosted Jev answered in a few hundred milliseconds and was as accurate as the larger self-hosted Clef; Clef-Flash and Nimble are faster self-hosted choices that misfile more borderline requests. A hosted classifier reads every prompt sent to a router, including ones you route to self-hosted models: use a self-hosted classifier if that matters. The builder warns when this is the case.',
          'When a security policy already calls the same decision model and it takes questions together, the routing questions join that call: no extra call, no extra wait. Otherwise they get a call of their own. That call starts in parallel with the Security Gateway checks only when no ingress check can block or redact the message and the caller is not covered by Child Safety; if not, the router asks after those checks pass, about the text that is actually forwarded, so a blocked message never reaches the routing classifier. A shadow router also decides from the forwarded (redacted) text.',
          'Each routing decision call is recorded as a classification usage event with client janus-auto-routing and the caller’s request id; it counts as Auto-routing cost, not Security Gateway cost. When a routing question rides along on a Security Gateway call, that one call is recorded as Security Gateway cost. A request’s classifier calls and wait include the checks added by the model the router chose.',
          'Classifier → Try routing classifies a prompt with any classifier (saved or not) and shows where every router would send it. The batch test runs every category’s test examples and lists the misfiles.',
          'The routing classifier reads the latest message with the recent conversation as context, so a short follow-up is routed by the task it belongs to: “Keep going” on a hard coding task stays hard coding, and “What about Ohio?” after a legal question stays legal. The last six earlier messages are included (user messages cut to their first 1,000 characters and assistant replies to their first 300, at most 4,000 characters in all, newest kept) with the latest message marked as the one to judge, which keeps a real change of topic (“Unrelated quick one: …”) from being dragged to the earlier subject. A first message is classified on its own, exactly as before, and can share a Security Gateway guard’s call; a follow-up is asked in a call of its own, in parallel with the guards, because the guards still judge the new turn alone. Try routing takes an optional earlier conversation to test follow-ups.',
          'On a live router, conversations stick to the route of their first decided turn for 30 minutes by default (1 minute to 24 hours; each turn starts the window again), so the model does not change mid-task: switching models makes the new one re-read the whole conversation, and the old one’s cached context is thrown away. Only the model sticks. When the route’s thinking scales with difficulty, each turn’s thinking still follows that turn: Janus asks the classifier about difficulty only (with the conversation as context) and sets the level, so an easy step of a hard task does not pay for high thinking, and a hard turn in an easy conversation gets it. If that question fails or times out, the level from the last judged turn is used. A sticky turn with fixed or passthrough thinking makes no classifier call. Either way the route must still fit the turn: a conversation that outgrows the route model’s context window, loses the grant a restricted route needs, or goes over a quota the route skips on is decided afresh. Revoking a grant therefore takes effect on the next turn.',
          'Janus recognises a conversation by an X-Janus-Session header (or X-Session-Id, X-Conversation-Id or X-Correlation-Id), a prompt_cache_key or session_id in the body, or otherwise by its opening (tools, system prompt and first user message), always per caller. A turn the default served because the classifier was too slow, failed or was busy is not remembered, so the next turn is decided afresh.',
        ],
      },
      {
        heading: 'Difficulty and thinking',
        body: [
          'A route can require a difficulty band: Easy (1–2), Medium (3), Hard (4–5), Expert (5), or a custom range. The classifier’s rating is a weighted average, so 2.5 sits between easy and moderate.',
          'Each route and the default set thinking: As requested (the caller’s setting passes through), a fixed level (none, low, medium, high, …), or Scale with difficulty (low, medium or high from the rating). Janus fits the level to what the target model supports, as it does for any request. OpenAI-compatible upstreams (including Vertex) get reasoning_effort; Anthropic gets the thinking form the Claude model takes (adaptive thinking with that effort on Claude 4.6 and later, a thinking budget before), see Thinking settings and provider limits. Bedrock’s Converse format has no thinking setting Janus can send, so on Bedrock a route’s thinking level is not applied and the request is sent as the caller made it.',
        ],
      },
      {
        heading: 'Instructions',
        body: [
          'A router can add text to the system prompt of the requests it serves. Router instructions are added to every request the router serves, whichever route serves it; each route, and the default, can add its own instructions after them. For example, a router says “Answer in British English.” for everything, and its Legal route adds “End every answer with a short note that this is general information, not legal advice.” Each field is plain text of up to 8,000 characters; surrounding blank space is trimmed.',
          'The router text and the route text are joined with a blank line and added before the caller’s own system prompt: on chat completions, at the start of the first system or developer message (or as a new first text part when that message is a list of parts, or as a new system message when there is none); on Anthropic messages, at the start of system (or as a first text block, or as system when there is none); on responses, at the start of instructions. Completions, embeddings and other endpoints have no system prompt, so instructions are not applied there, and the request log says so.',
          'Instructions are sent to the model and counted in its input tokens, so they cost what any prompt text costs. Callers cannot see or remove them, and they are not returned in responses. They are added on every turn, sticky turns included, because callers resend the conversation without them.',
          'The Security Gateway and the routing classifier read only the caller’s text: instructions are added after both have run, and they are not scanned (an administrator wrote them). The text Janus classifies is unchanged.',
          'A shadow router always serves its default, so its requests get the router instructions and the default’s instructions, never a route’s: shadow mode is observably the default plus those instructions. When a route’s model cannot serve and the default takes over, the default’s instructions apply instead of the route’s; instructions are added once, to the request that is finally sent.',
          'Evaluation compares like with like: the routed answer was produced with the route’s instructions, and the default’s replay carries the router instructions and the default’s.',
          'The request log detail for a routed request shows “Router instructions added: router”, “route” or “router + route”. With “Tell callers which route served” on, responses also carry X-Janus-Route-Instructions with the same value.',
        ],
      },
      {
        heading: 'Evaluating a route',
        body: [
          'Turn on “Evaluate against the default” on a route to find out whether it is as good as the default for the requests it gets. For a sample of the route’s requests (5% by default), after the caller has their answer, Janus asks the router’s default model the same question, exactly as the default would have been asked (the caller’s own request with the default’s thinking setting, not the route’s), then a judge model compares the two answers without knowing which is which, twice with the order swapped. If the two verdicts disagree, the comparison counts as a tie.',
          'The copies sent to the default and to the judge go through the Security Gateway policies that would apply to the caller sending them there (policies on the router, the model, its provider, the caller and their groups). If a policy would block or redact a copy, the sample is skipped and counted as “not evaluated”; nothing is sent. A route that keeps work on a self-hosted model and is evaluated against an externally hosted default or judge still sends a sample of those prompts off site, so the builder and the Evaluations tab warn about it.',
          'The Evaluations tab shows, per route: comparisons, better / tie / worse, “at least as good” (wins plus half of ties) with a 95% range, the judge’s consistency, and cost against the default. After at least 10 comparisons, a route is marked Losing when even the top of the range is below 50%, Keep when the bottom of the range is above 45% (a near-tie that saves money is worth keeping), and Needs more data otherwise.',
          'A route’s tally covers only comparisons between the models it uses now: its model, the router’s default and the judge. Change any of them and the tally starts again; earlier comparisons stay listed as history under the card and never count towards the new verdict.',
          'Only the verdict, scores, the judge’s one-line reason, cost and timing are stored, never the prompt or either answer. Evaluation covers chat completions without tools or images, up to 64 KB of request; a streamed answer too long to keep in full is not judged. It stops at the end date (14 days by default), at its daily count and daily budget, and pauses while the gateway is busy. The count and budget are reserved before anything is spent, at a deliberately high estimate, so a burst or several replicas cannot overshoot them. Evaluations at once per replica, the busy threshold and the default answer’s token cap are set on the Evaluations tab. Evaluation calls are platform overhead: they never count against the caller’s quota and are hidden from the request log. Requests from people covered by Child Safety are never sampled for evaluation.',
          'Auto-routing → Classifier → Advanced → Keep decisions (days) (90 by default; 0 keeps them) deletes decisions and evaluation verdicts older than that in the daily retention job. Remembered conversations are deleted once they expire. A decision kept longer than usage events (JANUS_USAGE_RETENTION_DAYS) no longer names the caller: its user and service token are cleared when the matching usage events are purged.',
          'Pick a judge that is neither the route’s model nor the default: judges favour their own answers. The judge must be an enabled model that is not a classifier; a disabled default or judge is not called.',
        ],
      },
    ],
  },
  {
    slug: 'admin/classifiers',
    title: 'Classifiers: speed and accuracy',
    summary:
      'How Janus asks decision models several questions at once, which models suit which job, and how to benchmark what your classifier choices cost each request.',
    group: 'Admin guide',
    sections: [
      {
        heading: 'One classifier, several questions',
        body: [
          'A request can need several answers before it is forwarded: is it a prompt injection, is it harmful, and what kind of work is it. With a decision model (TypeSafe Jev, Bespoke Nimble) all three can come from the same classifier. Janus collects the questions bound for each classifier and asks them in the way that classifier answers best.',
          'All checks that apply to a request run at the same time, so a request waits for its slowest classifier, not for the sum of them.',
          "Usage insights, in background or inline mode, adds its question to the guards' call when they use the same classifier about the same text and the classifier takes questions together (see Usage insights → Background or inline): the label then costs no extra call and no extra wait. Otherwise it is asked separately, before the request is forwarded (inline) or afterwards (background).",
        ],
      },
      {
        heading: 'Together or one at a time',
        body: [
          'Hosted decision APIs (TypeSafe, OpenRouter) answer each question in a request independently and in parallel. Several questions in one request cost about the same time as one and give the same answers as asking separately, so Janus sends them together.',
          'Self-hosted Nimble (llama.cpp or Ollama) reads every question in the request while answering each one, so related questions influence each other: a coding request asked alongside a language question can come back labelled as data analysis. Janus therefore sends each question in its own request, all at the same time. The server reads the prompt once per question, which costs some throughput but keeps every answer as accurate as asking alone.',
          'Janus picks the style from the provider automatically. Administration → Usage insights → Performance lists every decision classifier with the style in use, and lets you override it; run a benchmark before and after so the change is measured, not guessed.',
        ],
      },
      {
        heading: 'Which classifier for which job',
        body: [
          'Prompt injection: a decision model is a good alternative to Llama Prompt Guard 2. In our evaluation on public injection and jailbreak sets plus ordinary work requests, both Nimble and Jev caught more attacks than Prompt Guard 2 with fewer false alarms. Jev was the most accurate; Nimble is the one you can run yourself.',
          'Harmful content: a decision model can replace Llama Guard 4. Both Jev and Nimble flagged more harmful requests than Llama Guard 4 while naming the right hazard category about as often. Llama Guard is more conservative: it rarely flags something harmless, but it misses more.',
          'Security categories (the Categories method of content safety) need a decision model. Asking every category in one question is the fastest and works well on hosted Jev. Self-hosted Nimble and Clef judge categories more accurately when each is its own yes/no question, at one call per category, sent in parallel: pick One yes/no question per category on the check, and keep the category list short for those models. The larger Clef is more accurate than Clef-Flash and Nimble but slower.',
          'Usage insights: both decision models label requests accurately. Write category descriptions that say what belongs and what does not; short descriptions and bare names were measurably worse.',
          'Asking a classifier directly whether a request needs a stronger model was close to chance in our tests, so Janus does not ask it. Auto-routing instead routes by category and difficulty, which classifiers judge well.',
        ],
      },
      {
        heading: 'Thresholds',
        body: [
          'A decision model answers "is this an injection?" with a probability. Its probabilities are calibrated differently from Prompt Guard\'s scores: at Prompt Guard\'s usual 0.9 a decision model lets most attacks through. Thresholds between about 0.1 and 0.3 catch most attacks with few false alarms; Janus uses 0.3 when you bind a decision model and leave the threshold empty, and suggests it in the policy editor.',
          'Start in observe mode, review Security → Violations for a week of real traffic, then move the threshold: lower catches more and flags more ordinary requests; higher the reverse.',
          'Content safety with a decision model counts a request as harmful when the probability of "unsafe" is at least one half; the hazard categories you select decide which of those block. On responses (direction egress or both) the decision model is asked the same question about the assistant\'s reply; like every response verdict it is observed and logged, never blocked.',
          'With the Categories method each category has its own minimum confidence (0.5 unless you change it). Lower catches more and raises more false alarms. Run Test all examples on Security → Categories after changing wording or a threshold.',
        ],
      },
      {
        heading: 'What a decision model reads',
        body: [
          'Prompt Guard scans every message in overlapping windows. A decision model is asked once per check, and by default reads the new turn (what the caller added since the last assistant reply; a trailing assistant prefill is skipped) and at most its last 12,000 characters. That keeps each check to one fast call, and earlier turns were judged when they were new, provided they came through Janus.',
          "Two options on each prompt-injection and content-safety check bound to a decision model widen this. What to judge → Whole conversation also judges the earlier user, system and tool messages the caller sends, so made-up history cannot hide an attack. Long text → Whole text, in windows judges text longer than 12,000 characters in overlapping windows, one call per window in parallel, and takes the worst answer, so an attack placed before a long filler is still seen. Text longer than 16 windows (about 180,000 characters) counts as a classifier failure and the check's fail setting applies.",
          "Both cost time and money in proportion to the text: more characters per call, or more calls per request. On per-token hosted classifiers that is a direct cost; on a self-hosted one it is GPU time, and the slowest window decides the wait. Turn them on for blocking checks that guard sensitive applications, and keep the timeout large enough for the longest text you expect. A mandatory policy's wider setting cannot be narrowed by a more specific one.",
          "Questions from other features (Usage insights, Auto-routing) join a security check's call only when they are about the same text and may wait as long as the check does; a check never waits longer than its own timeout because another feature shares its call.",
        ],
      },
      {
        heading: 'Hosting Nimble yourself',
        body: [
          "llama.cpp (a release with System One support) serves Nimble with the same accuracy as Ollama and without Ollama's limit of 26 choices per question, so a large category list is still asked in one question. Prefer it for new installations.",
          'Cloudflare Clef and the smaller Clef-Flash (Apache 2.0) run on llama.cpp releases that include Clef support. Clef reads the whole request in one pass, so start llama-server with a physical batch at least as large as the context per slot (--batch-size and --ubatch-size); otherwise longer texts fail with "input is too large to process". In our tests Clef was as accurate as Jev on injection and work categories but is the slowest to self-host; Clef-Flash was about as accurate as Nimble and somewhat faster. Clef answers are far less affected by sibling questions than Nimble\'s, but Janus still asks them separately by default.',
          'On consumer and workstation GPUs the Vulkan build of llama.cpp was faster than the ROCm build in our tests. A 4-bit quantisation was no faster than 8-bit: the work is reading the prompt, not generating, so keep the 8-bit model for its accuracy.',
          'Give the server several parallel slots (llama.cpp --parallel, Ollama OLLAMA_NUM_PARALLEL). With a single slot, simultaneous requests wait in line and the wait grows with every concurrent user; with several, the GPU reads them side by side. Keep the model loaded permanently (Ollama keep-alive -1) so the first request after a quiet period does not pay for loading it.',
          'Give each slot enough context for your longest classified text plus the questions; Usage insights sends only the most recent characters up to its limit, and the guards judge the newest turn of the conversation.',
        ],
      },
      {
        heading: 'Benchmarking your choices',
        body: [
          'Administration → Usage insights → Performance → Benchmark measures what your classifier choices cost: pick the prompt-injection and content-safety classifiers and whether Usage insights runs inline or in the background, set the wait you will accept, and run it. Janus sends a built-in set of sample prompts through the real classifiers exactly as live traffic would, at increasing numbers of simultaneous requests.',
          'The result shows the wait one request pays with nothing else running, the typical and 95th-percentile wait at each level, how many classifier calls each request needs, and the highest number of simultaneous requests whose 95th-percentile wait stays within your budget. Compare runs with different classifiers or grouping to see the trade-off before you change a policy.',
          'Nothing is forwarded to a model, nothing is written to the request log, and the benchmark uses its own circuit breakers so overloading a classifier cannot trip the breaker live traffic depends on. It does share the classifier servers with live requests, so run it when traffic is light. One benchmark runs at a time.',
        ],
      },
      {
        heading: 'Two decision formats',
        body: [
          'Decision models come in two wire formats. System One is spoken by TypeSafe (api.typesafe.ai), OpenRouter (/api/alpha/decisions), llama.cpp and Ollama. OpenAI Decisions (/v1/decisions) is spoken by OpenAI, for example gpt-6-luna. Janus uses either one everywhere a decision model can be used: security checks, Usage insights, Auto-routing, the Try box, benchmarks and troubleshooting captures.',
          "Janus picks the format from the provider's address: OpenAI (api.openai.com) and Azure OpenAI speak OpenAI Decisions, everything else System One. Override it per model under Administration → Usage insights → Performance → Decision models, for a proxy or a self-hosted server that speaks the other format.",
          'OpenAI lists its decision models as chat models. Discovery adds a separate catalog entry for each one, named with :decisions (gpt-6-luna:decisions), because the decision API is priced differently from chat. OpenRouter decision models are found automatically and marked as decision models, with their prices.',
        ],
      },
      {
        heading: 'What decision calls cost',
        body: [
          'Each decision model has a rate card like any other model and, under Usage insights → Performance → Decision models, a Cost of each call setting and an optional Fee per call (USD). Automatic (the default) uses the charge the provider reports with each answer when it sends one (OpenRouter does), and the rate card otherwise. What the provider reports uses only the reported charge; The rate card always uses the card; Free records no cost, for a model you host yourself. The fee is added to every call, for example to recover hosting costs.',
          'Typical prices: OpenAI charges for decision input only ($0.10 per million tokens for gpt-6-luna); TypeSafe Jev is $0.042 per million; OpenRouter passes on the provider price. The same figure is used whether Janus calls the model for itself (security checks, insights, routing) or a person calls it, so the two always agree: cached input is charged at the cached rate, and a charge the provider reports as zero is recorded as zero. A decision model with no rate card and no reported charge is shown as unpriced in reports, not as free.',
        ],
      },
      {
        heading: 'Serving decisions to people and apps',
        body: [
          'People can call decision models through Janus with their own token, in either format, at /v1/decisions (OpenAI SDKs), /v1/systemone (TypeSafe SDK) or /api/alpha/decisions (OpenRouter SDK). Janus translates when the model speaks the other format and passes same-format requests through unchanged; the X-Janus-Decision-Translated header says when it translated. GET /v1/models lists decision models with janus.kind set to decision.',
          'Calls are granted, rate-limited, checked by security policies, counted against quotas, priced and logged like any other request, with the kind of request shown as Decision. Security policies read everything the caller wrote: the evidence (input or state) and the question text (instructions, yes/no boundaries, choices and their descriptions, score levels, question names). A finding in text Janus can rewrite (instructions, descriptions, level labels) is redacted where the policy redacts; one in a question name or a choice value, which the answers refer to, blocks the request instead. Answers are probabilities, so egress checks do not apply to them. A block always answers HTTP 403 with the error object (policy.security_blocked), even when the policy is set to return a synthetic refusal.',
          'Rate limits and blocking rules written for /v1/* or /v1/systemone also apply to /api/alpha/decisions, which takes the same requests. A managed model with a pool of decision models picks its member per request, like a chat pool.',
          "A question the model's format cannot express is refused rather than changed: images to a text-only model, or System One criteria for a choice the OpenAI format has no place for. System One yes/no criteria are kept, written into the OpenAI question's instructions.",
          'A decision model that blocks requests in a security policy cannot be granted: anyone who can call the judge can test what gets past it. That includes a model named in observe mode by a policy layered on a mandatory organization policy that blocks the same check, because the mandatory policy keeps the check blocking with that model as its judge. Existing grants stop working while it guards, the model is no longer listed in GET /v1/models, and callers are told only that it is reserved for security checks. An administrator can allow it per model.',
          "If a provider's decision model list cannot be read during discovery, the decision models already known are kept as they are. Any model that drops out of a provider's list and comes back returns in the state it had (an enabled model stays enabled), decision and chat models alike.",
          'A managed model (alias) can point at a decision model, with a fallback; for example jev falling back to gpt-6-luna:decisions. A chat request to a decision model, or a decision request to a chat model, is refused with a message that says which endpoint to use.',
        ],
        code: {
          language: 'python',
          code: 'from openai import OpenAI\n\nclient = OpenAI(base_url="${JANUS_PUBLIC_URL}/v1", api_key="<your Janus token>")\nresult = client.decisions.create(\n    model="typesafe/jev-router",  # a System One model: Janus translates\n    input="I was charged twice for my subscription.",\n    questions=[{"type": "predicate", "name": "urgent", "instructions": "Is this urgent?"}],\n)\nprint(result.answers[0].probability)',
        },
      },
      {
        heading: 'Troubleshooting classifier decisions',
        body: [
          'While troubleshooting mode is capturing a request, every classifier call the request caused is captured with it: the feature that asked, the classifier, inline or background, timing, status, and, when the session captures request bodies, the exact questions sent and answers returned. A background label that finishes after the request is attached when it lands.',
          "Open the request in Administration → Requests to read them under Classifier calls, or download the capture: classifiers.json sits next to the request and response. Captured classifier calls follow the session's encryption and retention and are deleted with their capture. Viewing them is written to the audit log.",
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
          'Two tiers exist. The deterministic tier (secrets, PII, term lists, request shape) needs no model and no GPU, observes on every edition (redact and block need Business), runs in the gateway process in well under a millisecond, and produces span-level evidence an auditor accepts. The classifier tier (prompt injection in this version) needs a guard model and adds latency; it is the only way to catch things defined by intent rather than structure.',
        ],
      },
      {
        heading: 'Policies and bindings',
        body: [
          'A policy is a named bundle of checks. Each check has a kind, a mode (observe / redact / block), a direction (ingress, egress, or both), and for model-backed checks a fail stance (closed or open) and the classifier that backs it.',
          'A policy does nothing until it is bound to a scope: Everyone, a group, an upstream provider, personal subscriptions (people’s own connected plans, all providers or one), a model, a managed model, or a service token. One binding per scope. Resolution is most-specific-wins: Everyone → group → upstream provider or personal subscriptions → model → managed model → service token.',
          'A policy bound to Everyone may be marked mandatory. Its checks then become a floor: a more specific binding may add checks or tighten a mode (observe → redact → block), fail stance (open → closed), direction, or hold window — never relax or remove. Administration → Safety & Security → Security → Bindings → "Explain effective policy" shows the resolved checks for any caller and model, and names every relaxation that was refused. Precedence you cannot see is precedence you cannot audit.',
          'Bindings are deliberately not part of grants. Grants answer "may this caller use this model"; policies answer "what rules apply to this traffic". Keeping them apart is what makes "block secrets org-wide" one binding instead of one edit per model per group — and is what stops a service token from being a bypass.',
        ],
      },
      {
        heading: 'Rollout: observe first',
        body: [
          'Every check defaults to observe. In observe mode nothing is blocked or rewritten; matches are recorded exactly as they would be in block mode. Run a new policy in observe for long enough to see what it would have done to real traffic, review the Violations tab, then switch individual checks to redact or block.',
          'Save a new policy without binding it (an unbound policy affects nothing), then use Security → Policies → Test to dry-run a sample request against it. Test runs the saved policy, not unsaved edits, and saving a policy that is already bound changes live traffic. A dry run makes no upstream call, writes no usage event and records no violation.',
          'On Community every check runs in observe mode; redact and block need Business. Policies that already redact or block keep doing so after a downgrade, but a policy with a redact or block check cannot be saved again until the licence allows it.',
        ],
      },
      {
        heading: 'The checks',
        body: [
          'Secrets: gitleaks-style rules for API keys, tokens, private keys, JWTs, and credentials embedded in connection strings. Structured rules (AWS, GitHub, Slack, Stripe, private-key blocks, …) are enabled by default; the generic high-entropy rule is off because it fires on base64 images, hashes and tool-call payloads in prose. Patterns are Go RE2 — linear time, so an administrator-authored rule can never take the proxy down.',
          'PII: payment cards (Luhn-checked), IBANs (mod-97), US Social Security numbers (hyphenated by default, structural checks), NPIs, e-mail addresses and phone numbers. Redact rather than block for most of these: blocking "summarise this complaint" over an e-mail address is the false positive that gets the feature switched off.',
          'Term lists: your own words — project codenames, client names, classification markings. Modes: exact (whole words), substring, regex, or fuzzy (catches Pr0j3ct H4lb3rd and confusable characters). Lists are encrypted at rest and never shown in listings; viewing or exporting a list is written to the audit log. Import a text or CSV file rather than typing four hundred names into a form.',
          'Shape: limits on message count, body size, image parts and tools, and a rule that refuses system-role messages from service tokens.',
          'Prompt injection: a text-classification guard model (Llama Prompt Guard 2 is the reference) scores every message in overlapping windows, or a decision model (Jev, Nimble) judges the newest turn (see What a decision model reads). Unlike harmful-content filtering, nothing upstream protects you from injection — it targets the application, not the model.',
          'Tool output: messages with role tool (or function) are tool output, not the person’s words. Violations found in them are recorded as tool output, and Child Safety judges them as content shown to the child, never as the child’s request. Pages, search results and file transcripts that Janus Chat reads for a reply are checked when Chat reads them, against the person’s policy. One that trips prompt injection or content safety is withheld from the model (the reply says “Withheld something it read”) and the reply goes on. The violation is listed under Security → Violations as “in a page Chat read”, not as the person’s words. Pages read by the provider’s own tools (hosted web search and page reading) are not checked when read; only the reply is.',
        ],
      },
      {
        heading: 'What a block looks like',
        body: [
          'By default a blocked request receives HTTP 403 with an OpenAI-shaped error, code policy.security_blocked, and the check kind in error.param. The rule id and the matched text are never returned to the caller.',
          'A policy may instead answer with a synthetic refusal: HTTP 200 and a well-formed completion whose finish_reason is content_filter, for clients that render errors badly. Streaming callers receive it as one chunk followed by [DONE]. The usage event still records the block either way. The synthetic refusal applies to chat completions only: a block on a decisions call (/v1/decisions, /v1/systemone, /api/alpha/decisions) always returns HTTP 403 with the error object (code policy.security_blocked).',
          'A redacted request sets X-Janus-Redactions on the response. The upstream saw different text than the user sent; that must never be silent.',
        ],
      },
      {
        heading: 'Responses and streaming',
        body: [
          "Checks with direction egress or both also scan the model's output — message content, reasoning content, and tool-call arguments. On a buffered response a block replaces the body with the error shape; a redaction rewrites the spans in place.",
          'On a stream the gateway withholds a small trailing window of text (hold_bytes, default 256) so a match can be caught before the bytes leave. Frames are released once their text is older than the window and scanned clean. A blocking match drops everything still held, emits a final in-band error frame and [DONE], and records stream_cut on the usage event. Text already flushed cannot be recalled: the window is the entire protection, and 256 bytes is enough for every deterministic rule at a latency no reader can perceive.',
          'Non-streaming replies larger than the response size limit (JANUS_MAX_RESPONSE_BYTES, 100 MiB by default) are never passed through unchecked when a reply check applies. For a child covered by Child Safety the reply is withheld and the child sees the stop notice. With a Security Gateway reply policy, the first part of the reply (up to the limit) is scanned by the deterministic checks; a match in block or redact mode withholds the whole reply (it cannot be rewritten), and observe mode records it. Replies with no reply check (for example large images) are passed through as before.',
          'Tokens the upstream generated before a cut are metered normally. The spend happened; hiding it would misstate what the filter costs.',
        ],
      },
      {
        heading: 'What is recorded — capture the attack, never the asset',
        body: [
          'Every violation row carries the request id, caller, model, policy, binding, check kind, rule id, action, offset/length and a hash of the matched span. What else is kept depends on the kind:',
          'Secrets: hash only. Storing the match would move a live credential into a second at-rest location. Term lists: hash only — the log must not leak the codename it protects. PII: the redaction marker, never the value; a security log must not become a new regulated data store. Prompt injection and content safety: the judged message, encrypted, when the policy’s Keep prompt-injection request bodies for analysis is on (off for a new policy, from the editor or the API; otherwise hash only) — the attack text is the intelligence, and it is what tunes the classifier. Child Safety never keeps what a child wrote.',
          'Reading captured text (Violations → Reveal) is itself written to the audit log. Retention is per kind: prompt-injection captures 90 days / 50k rows; secrets, PII, term-list and shape rows 30 days / 100k rows. Content-safety rows, Child Safety events included, are kept 90 days / 100k rows. Violations are never kept longer than the request log (JANUS_USAGE_RETENTION_DAYS, 365 days by default). Nothing captured ever leaves the gateway.',
        ],
      },
      {
        heading: 'Classifier models',
        body: [
          "Add the guard model's server as an upstream of type TEI (Hugging Face Text Embeddings Inference, the standard host for Llama Prompt Guard 2 — an encoder model that generative engines such as vLLM cannot load). Discovery reads the server's /info, finds the one model it serves, and assigns the classifier role automatically; confirm it under Administration → Safety & Security → Security → Classifiers before referencing it in a policy.",
          'A decision model (TypeSafe Jev, Bespoke Nimble) takes the Decision model role and can back prompt injection, content safety, or both. When both checks use the same decision model Janus asks them together; see Classifiers: speed and accuracy.',
          'A model with a classifier role is removed from /v1/models, cannot be granted, and is called only by the gateway. Classifier models (guard and text-classification roles) only check traffic: they can’t be an alias’s target, fallback or pool member, and a model a managed model uses can’t be made a classifier until it is removed from that managed model. A request that still resolves to one is refused. A caller who could invoke the judge directly would have free, unlimited oracle access to the thing judging them.',
          "Every classifier call is metered as a usage event on a platform-overhead subject with no user or service token attached: finance can see what the security feature costs and no user's quota moves because of it. The classifier is wrapped in a circuit breaker: three consecutive failures of the security checks' own calls open it for thirty seconds and raise a critical admin alert. Calls made by other features on the same decision model (Auto-routing, Usage insights, policy dry runs, Try boxes) never open it, and neither does a call the caller cancelled. While it is open, blocking checks with fail=closed refuse requests and say so in the error message; checks with fail=open pass and log. Observe-mode checks never block, whatever their fail setting. Choose fail=closed for security; choose fail=open only where availability matters more, and pair it with the alert. “Security classifier unavailable” is sent once per outage for the whole cluster (again after 6 hours if it is still down), followed by “Security classifier recovered” when it answers again.",
          'A classifier whose model is disabled or stale in Models is not called. Checks that use it follow their fail setting: fail closed refuses requests, fail open lets them through. Child Safety always fails closed. Rotating the API key or base URL of a classifier’s upstream takes effect on every replica within JANUS_CONFIG_CACHE_TTL_SECONDS (5 seconds by default).',
          'Generative guard calls that report token usage (for example Llama Guard on a paid API) are priced from the model’s rate card, like decision-model calls; encoder guards on TEI report no usage and cost nothing.',
          'Llama Prompt Guard 2 is distributed under the Llama 4 Community License: the download is gated, "Built with Llama" attribution applies, and any fine-tune must carry a name beginning with "Llama". If those terms are unacceptable to your organisation, any model served in the same text-classification shape can back the check instead.',
        ],
      },
      {
        heading: 'Content safety: security categories',
        body: [
          'Content safety asks a classifier whether a request (and, if you choose, the reply) falls under categories you care about. The default method is Categories: a decision model checks the categories the policy selects. The fixed hazard list Llama Guard uses is still available as Llama Guard compatible, a legacy method for Llama Guard models; checks saved before categories existed keep using it and behave exactly as before.',
          'Security → Categories lists two groups. Safety: the fourteen standard hazard categories under plain names (Violent crimes, Child sexual exploitation, Suicide and self-harm and the rest), built in; their wording can be edited and reset. Your categories: ones you add. The children’s categories (Feeling low or unsafe, Homework done for them and the rest) belong to Child Safety and are not offered to policies; a policy that still selects one shows it only so you can remove it.',
          "Each category has a name, what it includes and what it does not include (the text the classifier reads), a minimum confidence, test examples and an on/off switch. Instead of writing a description, a category can follow a Usage insights category, reading that category's name and description; editing the Usage insights category changes it too. Say what counts and name the near neighbors that do not: that is what stops false alarms.",
          "Try a request at the top of the tab shows the probability for every category that is on and which ones would count. Test all examples runs every category's examples through the chosen decision model and reports which it missed and what it said instead. Neither stores anything.",
          "In the policy editor, the content safety check's Method picks Categories or Llama Guard compatible. A new check starts on Categories with Violent crimes, Sex-related crimes, Child sexual exploitation, Indiscriminate weapons and Suicide and self-harm selected, and lists only decision models. Ask is either together (one question listing every selected category, the default) or one yes/no question per category, which is more accurate on self-hosted Nimble and Clef but makes one call per category; the editor shows the number of calls. A check takes at most 25 categories.",
          'A category counts when the classifier is at least as sure as its minimum confidence. One violation records every category that counted, by name, ordered by confidence. In block mode a request is refused; replies are always observed and logged, never withheld. A category a policy uses cannot be deleted or turned off until it is removed from that policy. Under a mandatory policy the selected categories are a floor: a more specific policy may add categories, never remove one, and cannot switch the method.',
        ],
      },
      {
        heading: 'Child Safety (every edition)',
        body: [
          'Child Safety is one set of rules for the children on a gateway, free on every edition. Open Administration → Safety & Security → Child Safety, switch it on, choose a safety model and add the children. The rules follow each child to every model they can use — current and future ones, organization and personal-plan models alike — in Janus Chat and through the API. They go by who the child is, not by the model they pick or the workspace they work under, so a grant to everyone or a newly added model is covered without any upkeep. Child Safety is not a policy you bind: it applies to the children in addition to, and after, any Security policies that apply to them.',
          'Add a child directly, or a group (team) of children, and give each an age range: Little kids (8 and under), Kids (9-12) or Teens (13-17), or your own range copied from one of them. A child’s own age range wins over their groups’; a child in several groups gets the strictest setting of each category.',
          'Each age range sets six categories — Feeling low or unsafe, Not suitable for children, Getting around parental controls, Sharing personal information, Homework done for them, and Homework learning — to Off, Answer appropriately or Block. Answer appropriately sends the message to the model with that category’s instructions added to the system prompt (editable, with Reset to built-in). Block answers with a short, kind refusal that names no category; the message is not sent to the model, and administrators are notified. For Feeling low, Block means Janus answers itself with a kind message pointing to a trusted adult and the 988 Suicide & Crisis Lifeline (911 in an emergency, Childhelp 1-800-422-4453 if home does not feel safe), and every administrator is told the child may need support — never what they wrote.',
          'Out of the box, Little kids blocks Feeling low or unsafe, Not suitable for children and Getting around parental controls; Kids blocks Feeling low or unsafe; Teens blocks none of them and gets a milder Not suitable for children text. Every other category is set to Answer appropriately. A new range starts as a copy of the one selected.',
          'Each message is judged with the recent conversation as context, so a follow-up is read for what it refers to: “how do I do that without my parents knowing?” right after asking where to buy a vape is treated as the same request. The last six earlier messages are included (user messages cut to their first 1,000 characters and assistant replies to their first 300) with the latest message marked as the one to judge. A message long enough to be checked in several windows is checked window by window, without the context.',
          'Every message from a child also gets the age range’s “every message” text. While either homework category is set to Answer appropriately, every message also gets the tutor rule: never give the final answer, the result or a worked solution, even after the child tries — when they share an answer, say only whether it is right or wrong and which step to look at again. Replies are checked for an answer given away; one that does stops there and ends with a short note encouraging the child to work it out. That is the tutor working, not an incident, so it does not notify anyone.',
          'A fixed set of safety categories is blocked for every child in every age range, in what they send and what the model answers: child sexual exploitation, sexual content, sex-related crimes, violent crimes, indiscriminate weapons, suicide and self-harm (which always gets the kind answer), hate, and non-violent crimes such as drugs. Replies are also checked for the categories the age range blocks, except Feeling low or unsafe and the homework categories, so a kind reply to a sad child is never cut. A reply is checked a paragraph at a time: each paragraph is held until the safety model clears it; a reply that is stopped ends with a short notice. Children can use chat completions, completions and embeddings. Each age range also has What they can make: four switches — Make pictures, Make videos, Make music, Turn speech into text — all off by default. When one is on, children in that range can use it, in Chat and on its endpoint (/v1/images/generations, /v1/videos, music models, /v1/audio/transcriptions); what they ask for is checked first, like any message. When it is off, Chat does not offer it and the endpoint is refused. With Turn speech into text on, sound and video files a child attaches in Chat are always turned into text and checked before the model reads them; with it off, those files are refused. Pictures a child attaches themselves are passed to the model and are not checked. Every other endpoint (the Responses and Messages APIs, speech, decisions) is refused for them.',
          'What Chat reads for a child (a web page, search results, the text of an attached sound or video file) is judged like a reply, as content shown to the child. Flagged content is withheld from the model and never treated as something the child asked: no support message and no parent notice.',
          'Block these models is optional: the rules already apply to every model, so use it only to keep an age range off particular models. Aliases and routers can be blocked too. A blocked model is hidden from the child’s model list, refused, and never picked by Chat for images, video, music, transcripts or video descriptions. It stays blocked when it or its alias is renamed.',
          'A child cannot leave a team that keeps Child Safety on for them. If a team is deleted, a member is removed, or a directory sync takes someone out of the team, administrators get a “Child Safety no longer covers …” notification. Deleting a team removes its Child Safety assignment.',
          "Any decision model works as the safety model: hosted Jev is recommended as the fastest and most accurate in our tests, but hosted means the messages and replies are sent to that provider to be checked. For fully local checking use Clef; for lower delay on local hardware use Nimble or Clef-Flash, ideally asking one question per category. A Llama Guard model is also listed (marked legacy), but it checks only the always-blocked safety categories, not the six age-range categories; use a decision model. With Child Safety on, the page shows a red Not protecting state, with the reason, until a model is chosen and at least one child is added; nothing is filtered without a model; once one is chosen, a model that is missing or not answering refuses the children's messages (fail closed) and touches no one else.",
          'Events are listed on the page and under Security → Violations. Administrators get an in-app notification when a child’s message or a reply to them is blocked (at most one every 15 minutes per child, across all replicas) and every time a child may need support; none is sent when the safety model was unavailable. Child Safety itself never keeps what a child wrote. Janus Chat keeps the child’s conversation like anyone else’s, and a running troubleshooting session with body capture on also captures it. Alert rules can subscribe to the Child Safety trigger (family_safety in the API) for webhooks (and email, on Business). When a category was answered appropriately, the request’s details in the request log show a Child Safety line naming it (for example “Answered appropriately: Homework learning”), on auto-routed requests too. Other Security policies work on every edition in observe mode; redacting or blocking with them (secrets, personal data, term lists, shape, prompt injection, content safety) needs Business.',
        ],
      },
      {
        heading: 'Content safety: Llama Guard compatible',
        body: [
          'Content safety is a second, separate check backed by a different kind of classifier. Llama Guard is a generative model served on a chat engine such as vLLM: add that server as an ordinary OpenAI-compatible upstream, let discovery find the model, and mark it as a generative guard on Security → Classifiers. The two roles are not interchangeable — a Prompt Guard encoder cannot judge hazard categories and a Llama Guard cannot score injection — so a policy that binds the wrong role is refused when saved, with the reason.',
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
          'Without an identity provider configured, the very first visit to a new gateway shows “Create the first administrator”. That endpoint disappears once any account exists; every later account is created by an administrator under Administration → People → Add user.',
        ],
      },
      {
        heading: 'Passwords and lockout',
        body: [
          'Passwords are 12–128 characters with no composition rules (a sentence is a good password) and are stored as bcrypt hashes. Ten wrong attempts in a row lock the account for 15 minutes, and the form then says so. Otherwise a wrong email and a wrong password give the same answer, so the sign-in form cannot be used to discover accounts. Each network address may try to sign in 20 times a minute. Changing a password signs out every other session.',
          'Administrators create accounts and reset passwords with a temporary password the person must replace at their next sign-in. Administrators never see or set a permanent password.',
        ],
      },
      {
        heading: 'Two-factor authentication',
        body: [
          'Two-factor protects a Janus password, so set a password first. Sign-in through an identity provider relies on that provider\u2019s own two-factor, and directory passwords are not covered. Personal settings → Security → Set up shows a QR code for any TOTP app. Enrollment completes only after a correct code, and issues ten single-use recovery codes shown once. Secrets are encrypted at rest with the gateway key. Turning two-factor off yourself asks for your password. An administrator can remove a user\u2019s second factor from Administration → People when a device is lost.',
        ],
      },
      {
        heading: 'Several identity providers (Business)',
        body: [
          'Administration → Settings → Sign-in & provisioning → Identity providers adds OIDC providers beside the environment-configured one: a contractor tenant in Entra ID next to the corporate Okta, or Google Workspace for a subsidiary. Each provider has a slug that becomes its callback URL (`/auth/callback/<slug>`) — register that URL at the provider — and its own sign-in button.',
          'Test checks discovery before you save. The client secret is encrypted at rest and never shown again. ID tokens must be signed with RS256/384/512 or ES256/384/512 — Authentik in particular defaults new providers to HS256 until a signing key is selected; Janus refuses HS256 with an explicit message. Per-provider admin groups are re-evaluated at every sign-in and combine with JANUS_ADMIN_GROUPS. Subjects are namespaced by slug, so two providers can never collide on the same user id. Deleting a provider keeps the accounts it created; those people need another way in.',
        ],
      },
      {
        heading: 'Directory sign-in — LDAP and Active Directory (Business)',
        body: [
          'Administration → Settings → Sign-in & provisioning → Directory (LDAP / Active Directory) connects one LDAP directory. People type their directory email and password into the same sign-in form Janus accounts use; Janus looks the person up with the service account, binds as them to prove the password, reads their groups, and creates the account on first sign-in. Admin groups are re-evaluated at every sign-in and combine with JANUS_ADMIN_GROUPS.',
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
          'JANUS_DEV_AUTH=true (any email signs in, no password) remains for laptops and demos only. The gateway refuses to start with it when JANUS_ENV=production is set explicitly, or when JANUS_PUBLIC_URL is not localhost or a loopback address (the check that protects a deployment left with JANUS_ENV unset). With local accounts available there is no longer a reason to run it on a shared gateway.',
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
          'Every Business feature is a control that is already in the product; Community shows it greyed out with a one-line reason, and the API answers 402 feature_not_licensed if it is called anyway. Reads are never gated, and most things configured before a downgrade keep running — only the enabling write is refused. The exception is sign-in: without a valid key that includes them, sign-in through additional identity providers and LDAP stops (Janus-account passwords and the environment-configured provider keep working). A saved Chat access restriction keeps applying on every edition; only saving a new or changed one needs Business.',
          'Guardrails enforce: redact and block modes on security policies (observe is Community; Child Safety blocks on every edition). Request capture: turning troubleshooting recording on. Email alerts: the email channel on an alert (in-app and webhook are Community). Model fallbacks: a fallback model on a managed model. Load balancing: a managed model with more than one model, or a balancing policy other than failover. Audit export: the CSV download on the audit log. Chat access: limiting Janus Chat to chosen users, groups or teams (Administration → Chat → Who can use Chat). Scheduled reports, SCIM provisioning, multiple identity providers, LDAP directory sign-in and more than one node are described on their own pages.',
        ],
      },
      {
        heading: 'Seats',
        body: [
          'A seat is an active user: anyone who signed in or made a request in the last 30 days. Disabled accounts do not hold a seat. A new person takes a seat at their first sign-in — including accounts created in advance by SCIM, the directory or Administration → People. When every seat is used, a new person cannot complete their first sign-in; everyone already active is unaffected, and administrators can always sign in. Adding a Janus account under Administration → People → Add user, or turning a disabled account back on (on its profile or through SCIM), is refused the same way (402 license.seats_exhausted; SCIM answers 403). SCIM provisioning itself never fails on seats.',
          'Business seats are bought as a pool at janusedge.com and split into one key per gateway or site from the portal; each gateway enforces only its own key.',
        ],
      },
      {
        heading: 'Expiry never disrupts work',
        body: [
          'Thirty days before expiry the shell shows a notice. After expiry there is a grace period (normally 30 days) in which nothing changes. After the grace period the gateway is restricted: everything already configured keeps running — proxying, every configured sign-in (SSO providers, LDAP, Janus accounts), SCIM sync, guardrails, quotas, a Chat access restriction, reports, HA — but creating new items is paused: tokens, service tokens, upstreams, managed models, routers, quotas, rate limits, policy rules, teams, groups, grants, security policies, policy bindings, term lists, alerts, reports, report schedules, SCIM tokens and Janus accounts. Edits and deletes still work: deleting is never paused, so you can remove an LDAP directory or an extra sign-in provider after a downgrade. Rotating a SCIM token counts as creating one. The API returns 402 with code license.required. An installed key that fails verification is treated the same way.',
          'Enabling a feature the edition does not include returns 402 with code license.feature_not_licensed. A feature that is already on keeps working when the key lapses, including sign-in through additional identity providers, LDAP directory sign-in and a saved Chat access restriction. After the grace period you cannot add a provider or change who may use Chat until a key is installed. A key that fails verification includes no features, so additional providers and LDAP stop until a valid key is installed: keep a Janus-account password for at least one administrator. A saved Chat access restriction keeps running after a downgrade; you can lift it on Administration → Chat → Who can use Chat (Let everyone use Chat).',
        ],
      },
      {
        heading: 'Installing a key',
        body: [
          'Admin → Settings → License & updates: paste the key exactly as downloaded from the portal and choose Install. The key is verified before it is stored and takes effect immediately on this node; other nodes pick it up within an hour or on restart.',
          'Automatic renewal and sync is off by default. The simplest setup: paste the activation code from the janusedge.com portal (it starts with JANUS-ACTIVATION-1 and carries both the license and its sync token) into Admin → Settings → License & updates → Install a key, tick Automatically renew and sync, and install. Lost the code? Use New activation code on the key in the portal; the key and seats stay the same. Alternatively enter the raw sync token under Automatic renewal and sync, save, and use Sync now. Saving a token or installing an online key never enables sync by itself. This retrieves renewed signed licenses, not software updates. Blank tokens preserve the stored credential; Clear stored sync token removes it. JANUS_LICENSE_SYNC and JANUS_LICENSE_SYNC_TOKEN pin their respective settings when supplied in the environment. Offline mode prevents remote sync. File-backed keys are not synchronized or overwritten; deliberately remove the file configuration, restart and install a database key before opting in. Renewal warnings are suppressed only while a successful renewal confirmation is fresh; expiry and grace details remain visible.',
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
          'Off by default. Set JANUS_UPDATE_CHECK=true and the gateway asks janusedge.com once a day for the latest release, sending only the running version, the edition and this gateway\u2019s instance id. The answer (a newer version, release notes, or an advisory) is shown under Administration \u2192 Settings \u2192 License & updates \u2192 Updates and nowhere else; nothing is downloaded or changed. JANUS_OFFLINE=true forces it off for air-gapped deployments.',
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
          'Configure and test OIDC or direct LDAP sign-in under Settings → Sign-in & provisioning; SCIM 2.0 provisioning is separate. Administration → People has Users, Groups, and Teams. People → Teams owns organization-wide creation, CSV import, membership roles, join-request decisions, group mappings, quota delegation, and deletion. Deleting a team switches API keys, Chat conversations and “Working under” selections that use it to personal context; their owners get a notification. Usage history keeps the team name. The top-level Team workspace is informational: it shows only your actual memberships, members, usage, and quotas, even for administrators. Administrator permissions alone do not make a team one of your memberships. Model access still requires Grants. CSV imports validate the complete upload and apply all rows or none.',
          'Deleting a person disables the account and revokes their API tokens at once; their share links stop working. Their chats, files, memories, saved prompts, chat settings, share links and personal subscriptions are kept for 30 days — the list shows “Deleted — held until <date>”. Choose Restore to bring the person and everything back, or open the person and choose Delete content now (POST /api/v1/admin/users/{id}/purge) to delete it straight away. After 30 days it is deleted automatically. Usage history and the audit log are kept as before. If your directory deletes someone through SCIM and provisions them again within 30 days (same user name or email, or the same external ID), they get their account and content back.',
          'Deployment settings come from environment variables; nothing is hardcoded. Gateway behaviour that administrators change at runtime — file storage, additional sign-in providers and the directory, feature flags, Chat, troubleshooting — is set in the app and stored in the database. JANUS_ENCRYPTION_KEY is always required. An identity provider is optional: without one, people sign in with Janus accounts. If you set JANUS_OIDC_PROVIDER_URL you must also set JANUS_OIDC_CLIENT_ID and JANUS_OIDC_CLIENT_SECRET.',
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
          'GET /healthz reports liveness with no dependency checks. GET /readyz returns 503 unless the database and the environment-configured identity provider (if any) are reachable; admin-added providers and the directory are not checked — wire it to your readiness probe so a replica never receives traffic it cannot serve.',
          'The identity-provider check tolerates transient faults: a single failed discovery probe (for example a connection refused while the provider restarts) is retried immediately and does not make the replica unready. Two consecutive failed probe rounds mark the provider unreachable, so a real outage flips /readyz to 503 within about 40 seconds, and a recovered provider clears it within about 5 seconds. Each failed probe is logged at WARN level with the discovery URL and the underlying error.',
        ],
      },
      {
        heading: 'Retention',
        body: [
          'Usage events are kept for JANUS_USAGE_RETENTION_DAYS (365 by default) and audit entries for JANUS_AUDIT_RETENTION_DAYS (730). A purge job runs daily at JANUS_PURGE_JOB_TIME_UTC and reconciles quota ledgers afterwards. The same job deletes in-app notifications after 90 days, chats a person deleted 30 days after the delete, and the content of deleted people once their 30-day hold ends. Violations and Usage insights results are never kept longer than the request log (JANUS_USAGE_RETENTION_DAYS); within that, violations keep their own 30-day (90-day for prompt injection and content safety) limit and Usage insights its own setting.',
          'The gateway never persists prompt or response bodies of API calls, including in debug logs — with one deliberate exception: troubleshooting mode, described below, which an administrator must switch on. Janus Chat is different by design: it keeps each person’s conversations and attachments, encrypted with the gateway key, until they are deleted. When someone deletes a chat it disappears at once and is removed for good, with its files, 30 days later.',
        ],
      },
      {
        heading: 'Troubleshooting mode',
        body: [
          'When a client keeps failing and the request log’s metadata is not enough, Administration → Settings → Troubleshooting starts a capture session (24 hours by default, up to one week; No expiry is also offered, with a warning, and runs until you disable it). Starting a session needs a Business key. While it is active, requests matching its filter have their headers — Authorization, Cookie and API-key headers always redacted — and, optionally, their request and response bodies captured. Captured bodies are exactly what the client sent and received: they are taken before any Security Gateway redaction and regardless of a policy’s Keep prompt-injection request bodies setting, and they include messages from people covered by Child Safety. Leave Request bodies and Response bodies off under What to capture when you only need headers and metadata. The filter combines models, users or service tokens, groups, upstreams, error codes, HTTP statuses, success/failure and token-size thresholds on each direction with AND or OR; optional Require (every rule must match) and Exclude (any match keeps a request out) gates narrow it further. An empty filter captures everything and is warned about.',
          'Every session declares a retention policy — maximum age, maximum number of captures, maximum total size, at least one required — enforced hourly and on demand with Run cleanup now. Bodies are stored in the database by default or, when JANUS_TROUBLESHOOT_DIR is set on the gateway, as files under that directory; either way they can be encrypted at rest with the gateway key, and the panel warns prominently when they are not. When File storage (below) is set to S3-compatible, a third choice, Object storage, writes the bodies to that bucket under troubleshoot/<date>/<capture>/, always encrypted by Janus before upload. Purge all captured data removes everything at once.',
          'Captured rows in Admin → Requests gain a Download control (also in the detail drawer) serving a tar.gz with manifest.json, request/headers.json, request/body.*, response/headers.json and response/body.*. Export all streams every capture in one archive with a captures.jsonl index — one JSON document per request — and an Athena table definition in README.txt for bulk analysis or replay validation. Enabling, updating, disabling, purging and exporting are all audit-logged.',
        ],
      },
      {
        heading: 'File storage',
        body: [
          'Files Janus keeps — chat attachments and, optionally, troubleshooting capture bodies — live in one admin-chosen place: Admin → Settings → General → File storage. Database (the default) keeps them in the Janus database and needs nothing else. S3-compatible sends them to a bucket on AWS S3, SeaweedFS, Ceph or any other S3-compatible service: enter the endpoint, bucket, region, path-style addressing (needed by most self-hosted servers), an optional key prefix, the access key ID and secret, and an optional CA bundle for a private CA. Test connection writes, reads back and deletes a probe object with the values on screen before you save; it reports the provider’s own error, such as SignatureDoesNotMatch for a wrong secret.',
          'Every file is encrypted by Janus with the gateway key (AES-256-GCM) before it is stored, so neither the database nor the storage provider ever sees its contents. The secret access key is stored encrypted and never shown again; leave the field blank to keep it. Janus creates the bucket on first use when it does not exist. Single files are limited to JANUS_STORAGE_MAX_OBJECT_BYTES (25 MiB by default).',
          'Switching backends does not move existing files: they stay where they are and remain readable, because every file records which backend holds it. Only new files go to the new backend.',
          'A deployment can fix the choice instead with JANUS_STORAGE_MODE (database or s3) and JANUS_S3_ENDPOINT, JANUS_S3_BUCKET, JANUS_S3_REGION (default us-east-1), JANUS_S3_PATH_STYLE, JANUS_S3_PREFIX, JANUS_S3_ACCESS_KEY_ID, JANUS_S3_SECRET_ACCESS_KEY and JANUS_S3_CA_FILE. The card then shows those values as Set by the deployment and only offers Test connection. With the Helm chart, pass them under extraEnv, with the secret access key from a Secret.',
        ],
      },
      {
        heading: 'Observability',
        body: [
          'GET /metrics exposes Prometheus metrics. Deliberately, no metric carries a user or token label: per-person analytics come from the usage-event table instead, which keeps active series bounded.',
          'Logs are structured JSON on stdout. The Authorization header, API keys, and request bodies are excluded by construction.',
          'Alert delivery is split in two on purpose. Alerts the gateway can observe itself (quota thresholds, upstream down) are sent by the Janus process using JANUS_SMTP_* and the alert rules configured in Admin \u2192 Alerts. Infrastructure alerts \u2014 gateway down, database replication lag over ten seconds \u2014 are evaluated by Prometheus (deploy/compose/prometheus/alert-rules.yaml) and must not depend on the gateway being healthy, so they are delivered by Alertmanager: enable the compose monitoring profile and set the ALERTMANAGER_* variables (SMTP for email to the admin, ALERTMANAGER_WEBHOOK_URL for a webhook). Running Prometheus without Alertmanager leaves those alerts visible in the Prometheus UI but delivered nowhere. Outside the reference compose stack, attach your own Alertmanager and point it at the same rules.',
          'Alert rules (Administration → Safety & Security → Alerts) pick a trigger. Besides quota warnings and breaches, Upstream unreachable, System events and Child Safety, there are: Pool member taken out of rotation (a load-balanced alias stopped sending requests to one member after repeated failures; sent once per ejection); Personal subscription needs reconnecting (sent to the person whose subscription it is, when Janus can no longer renew their sign-in); License expiring or expired (weekly in the last 30 days, then daily in the grace period and after expiry); and Chat replies failing (at most once per model every 15 minutes).',
          'Severity is the lowest severity this rule sends by email or webhook. Critical sends only critical alerts and Info sends everything. Recovered notices are always sent. The in-app inbox receives every alert regardless.',
          'With several replicas, Upstream unreachable, Security classifier unavailable, pool ejection, license, Chat failure and Child Safety blocked notices are deduplicated in the database, so the cluster sends each once.',
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
          'Once a real administrator exists you can unset the variable and restart: roles are stored in the database and persist. Further promotions happen in Administration → People.',
          'Alternatively, JANUS_ADMIN_GROUPS grants the admin capability to members of the listed identity-provider groups (comma- or space-separated names, matched case-insensitively against the groups the person signs in with — the JANUS_OIDC_GROUPS_CLAIM claim, an added provider’s groups claim, or directory groups — on every sign-in). Unlike the bootstrap list this grant is not sticky: leaving the group, or unsetting the variable, revokes it at the next sign-in, while bootstrap- and UI-granted admin persist.',
          'Removing an administrator group, or removing someone from an admin group (in Janus or through SCIM), takes group-granted administrator access away at once; so does changing an identity provider’s or the directory’s admin groups. Explicit and bootstrap administrators keep it. A person who leaves a group only in the identity provider keeps group-granted access until their next sign-in, because Janus learns those groups at sign-in.',
          'Administrator groups can also be managed without a redeploy under Administration → People → Groups → Administrator groups, and per identity provider or directory (Admin groups). All of them combine with JANUS_ADMIN_GROUPS and are re-checked at every sign-in, and again whenever one of these lists or a group’s membership changes in Janus.',
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
          'Three consequences to know. First, quota enforcement reads the database ledger: concurrent requests can each pass an almost-exhausted check, so a cap can be overshot by the amount in flight; the reconciliation job trues the ledger up. Second, revoking an API token or a service token, or moving its expiry into the past, takes effect immediately on every replica: the 60-second credential cache only saves the digest lookup, and each cached hit re-reads the token row. Third, requests-per-minute rate-limit buckets live in process memory per replica, but each replica divides the limit by the number of live replicas, so the configured rate holds across the cluster. For up to 45 seconds after a replica stops, the survivors still divide by the old count and admit slightly less than the limit.',
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
    summary: 'Symptom-first diagnosis, and what every error code you can receive means.',
    group: 'Reference',
    sections: [
      {
        heading: 'Start with the error code',
        body: [
          'Every error Janus itself returns has a stable `code` in the error body (and a request_id to quote). Find the code in the sections below; the error catalog has the full cause and fix for each. Errors that come from the provider are passed through unchanged and do not use these codes.',
          'Streaming requests already returned HTTP 200 before anything could go wrong, so a failure mid-stream arrives as a final `data:` frame with an `error` object instead of an HTTP status. Check the last frame when a stream ends without [DONE].',
        ],
      },
      {
        heading: 'I got a 401: my key is not accepted',
        body: [
          '`policy.token_invalid` — the token is missing, mistyped, revoked, or belongs to a deleted account. Check the API Tokens page: a revoked token shows as revoked. Confirm your client sends `Authorization: Bearer <token>` and not the token alone.',
          '`policy.service_token_expired` — a service token reached the expiry set when it was created. An administrator issues a replacement under Administration → Service tokens.',
          '`authentication_error` — a browser call to the web API had no valid session; it expired after 24 hours or 4 idle hours. Sign in again.',
        ],
      },
      {
        heading: 'I got a 403: I am not allowed',
        body: [
          '`policy.model_not_granted` — the model is not enabled, or you hold no grant for it. Call GET /v1/models to see what you can use; ask an administrator for a grant.',
          '`policy.user_disabled` — an administrator deactivated your account. Every call is refused until it is re-enabled.',
          '`policy.endpoint_blocked` — a policy rule matched the request. The reason field gives the administrator’s explanation, or names the rule and the clause that fired.',
          '`policy.security_blocked` — a Security Gateway check refused the request or withheld the response; error.param names the check (secrets, pii, terms, shape, prompt_injection, content_safety); Child Safety refusals carry no param. Remove the flagged material and retry, or give the request_id to an administrator. If the message says the policy fails closed, the classifier was unreachable and nothing matched.',
          '`policy.subscription_reauth_required` — a my/… model uses your personal provider subscription and its sign-in stopped working. Reconnect it on the Subscriptions page.',
          '`policy.service_token_scope` — a service token was used outside /v1. Service tokens only make model calls.',
          '`permission_error` — your account lacks the role for an administrative action.',
        ],
      },
      {
        heading: 'I got a 429: too many requests or quota used up',
        body: [
          '`policy.quota_exceeded` — a token, spend, or request quota is used up. Retrying before reset_at fails the same way; ask for a higher limit if you need one.',
          '`policy.rate_limit` — a per-minute rate limit was exceeded. Wait retry_after seconds.',
          '`upstream.rate_limit` — the provider throttled the gateway itself. You receive the provider’s own 429 body; the request log records it under this code. Back off and retry. Janus does not retry generative calls for you, because a retry is a second charge — except that a load-balanced managed model hands the request to its next model before anything is generated.',
        ],
      },
      {
        heading: 'I got a 5xx: the model did not answer',
        body: [
          '`upstream.unavailable` (503) — the provider could not be reached or timed out. Retry shortly; an administrator can check the upstream under Administration → Settings → System status.',
          '`policy.managed_model_unavailable` (503) — the managed model (alias) you called points at a missing or disabled model. Your request is fine; call another model until an administrator repairs the alias.',
          '`policy.managed_model_fallback_exhausted` (503) — the alias target is down and its configured fallback cannot take over either. Retry later or report the request_id.',
          '`server_error` (500) — an unexpected gateway fault. Retry once; if it recurs, quote the request_id to whoever runs the gateway.',
        ],
      },
      {
        heading: 'My stream stopped early',
        body: [
          'Read the last frame. `upstream.stream_interrupted` means the provider dropped the connection or closed the stream without finishing; the text you received is incomplete, so retry. `policy.response_too_large` means the stream passed the gateway response cap (JANUS_MAX_RESPONSE_BYTES); ask for a shorter answer. A non-streaming answer larger than the cap is delivered in full, with its tokens estimated from its size. `policy.security_blocked` means a guardrail withheld the rest. `policy.quota_exceeded` means a hard-kill quota ran out mid-answer.',
          'If there is no error frame at all, check your own client and proxies for read timeouts shorter than the answer takes.',
        ],
      },
      {
        heading: 'I got a 400, 404, 409 or 413',
        body: [
          '`invalid_request_error` — the body was malformed or a required field such as model was missing (400); the body is too large for the provider adapter to rewrite (413); or an admin change conflicts with things that depend on it (409). The message and param field say what to fix.',
          '`not_found_error` (404) — the path or the item does not exist, or is not visible to you. For model calls the base URL must end in /v1.',
        ],
      },
      {
        heading: 'Administrators: I got a 402',
        body: [
          '`license.required` — creating something new needs a valid license; existing configuration keeps running. Install or renew the key under Administration → Settings → License & updates.',
          '`license.feature_not_licensed` — the feature is not in the installed edition. Install a key that includes it, or leave it off.',
          '`license.seats_exhausted` — every licensed seat is in use, so a new Janus account cannot be added or a disabled one turned back on. Free a seat by disabling an account, or add seats to the key.',
        ],
      },
      {
        heading: 'My model is missing from /v1/models',
        body: [
          'Either the model is not enabled, or you hold no grant for it. Both are administrator actions. A model that has never been priced cannot be enabled at all — an administrator must save a rate card first ($0 rates are allowed for self-hosted models).',
        ],
      },
      {
        heading: 'A model is slow',
        body: [
          'The Models page shows each model’s typical speed (tokens per second over the last 7 days) and how busy it is (jobs per day, with a trend arrow). Pick a faster model for interactive work. A long wait before the first word usually means a long prompt or a busy server rather than a slow model.',
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
          'Open the request log and look at token_accounting_method. Requests marked “estimated from byte count” were priced from the generated text because the provider reported no usage block — most often on the catch-all route or on a stream the provider cut off. Cached input tokens are also counted in tokens_in.',
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
          'Proxy — PUBLIC_URL/v1/* speaks the OpenAI API and authenticates with a bearer token from the API Tokens page. This is the base URL to configure in SDKs. See “Proxy endpoints” for details.',
          'Application — /api/v1/* serves the web UI: profile, tokens, request log, dashboards, and the team-lead quota endpoints. It authenticates with the browser session cookie or a personal API token (service tokens are refused). Mutating calls made with the session cookie must echo the janus_csrf cookie value in the X-Janus-CSRF header; calls made with a token need no CSRF header.',
          'Admin — /api/v1/admin/* requires the admin role on top of a session. Every mutation is audit-logged with actor and before/after values.',
          'Outside these three: POST /v1/decisions, /v1/systemone and /api/alpha/decisions call decision models with the same bearer token as the proxy. /scim/v2/* takes a SCIM provisioning token from Administration → Settings → Sign-in & provisioning. /auth/* handles sign-in. /healthz (liveness), /readyz (readiness: database and identity provider) and /metrics (Prometheus) need no credentials.',
        ],
      },
      {
        heading: 'Error envelope',
        body: [
          'Every error Janus itself returns uses the OpenAI-compatible error shape: an error object with message, type, code, and — where useful — param, reason, retry_after, reset_at, request_id, and docs_url, a link to the code’s entry in the error catalog. The catalog documents every code with its cause and fix. SCIM provisioning returns SCIM error bodies instead, and errors from a provider are passed through unchanged.',
        ],
        code: {
          language: 'json',
          code: '{\n  "error": {\n    "message": "Quota exceeded: 1,000,000 input tokens per day.",\n    "type": "rate_limit_error",\n    "code": "policy.quota_exceeded",\n    "reset_at": "2025-01-02T00:00:00Z",\n    "request_id": "req_8f3a"\n  }\n}',
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
          'The primary endpoint. For OpenAI-compatible upstreams the body is forwarded as sent, with a few exceptions: a display name in model is replaced by the model’s native name, streaming requests get stream_options.include_usage so usage can be metered, and Security Gateway redactions or router instructions are applied when configured. Requests to other providers (Anthropic, Gemini, Bedrock) are translated, and so are their responses. Set stream:true for server-sent events — frames are relayed as they arrive and usage is read from the final frame.',
          'Successful buffered (non-streaming) responses carry X-Janus-Cost-USD with the cost attributed to the call under the model’s current rate card, and X-Janus-Token-Accounting naming how the figures were obtained (upstream_reported, upstream_reported_cost, byte_count_fallback, unmetered_modality). Streaming responses do not carry these headers — the cost is only known once the final frame has arrived — but the call is metered identically and appears in the request log and dashboards. Calls billed to a personal subscription report a cost of 0, and error responses are recorded as not_billable.',
          'Throughput is returned too. When the provider measures it (Groq, llama.cpp, Ollama) the gateway forwards it as X-Janus-Tokens-In-Per-Second and X-Janus-Tokens-Out-Per-Second with X-Janus-Throughput-Source: upstream. When it does not, the gateway derives it from the token counts and its own clock and returns it under DIFFERENT names — X-Janus-Calculated-Tokens-In-Per-Second and X-Janus-Calculated-Tokens-Out-Per-Second with X-Janus-Throughput-Source: calculated — so a network-inclusive estimate can never be mistaken for a provider measurement. On streams the same fields arrive as HTTP trailers after the final frame (prompt throughput uses time-to-first-byte as its window, generation throughput the remainder). Input throughput counts only the prompt the provider actually processed: tokens served from its prompt cache are excluded (and cache writes included), so a mostly-cached request reports the model’s real prefill speed rather than a multiple of it.',
        ],
        code: {
          language: 'bash',
          code: 'curl ${JANUS_PUBLIC_URL}/v1/chat/completions \\\n  -H "Authorization: Bearer $JANUS_TOKEN" \\\n  -H "Content-Type: application/json" \\\n  -d \'{"model": "gpt-4o-mini", "messages": [{"role": "user", "content": "Hello"}]}\'',
        },
      },
      {
        heading: 'POST /v1/completions and POST /v1/embeddings',
        body: [
          'Legacy completions and embeddings work the same way: the same forwarding, grant and quota checks on the model, full metering. Embedding requests are recorded with the embedding modality so dashboards can separate them from chat.',
        ],
      },
      {
        heading: 'GET /v1/models',
        body: [
          'Returns the OpenAI-shaped model list filtered to what your token may use. A model appears only when it is enabled and covered by a grant that matches you (outside local-only mode, a model cannot be enabled until it has a rate card). If a model you expect is missing, ask an administrator for a grant.',
          'The list also includes managed models, your connected personal subscriptions, and decision models (marked kind: decision, with the endpoints that serve them). Each entry has a janus object with the upstream, modalities, price per million tokens, how you were granted it, and the native model name. Models blocked for a child account by Child Safety are left out.',
        ],
      },
      {
        heading: 'Everything else under /v1',
        body: [
          'Paths without a dedicated route (audio, images, moderations, files, and so on) pass through to the upstream when policy allows, and are metered by modality. When a provider reports a cost directly (X.ai cost_in_usd_ticks) that exact figure is recorded. When a TEXT-shaped response returns no usage block, tokens are estimated from byte counts and the request log marks the accounting method byte_count_fallback. Media responses (audio, images, video) are never byte-estimated — a 30 KB MP3 is not 7,680 tokens — so one that carries no usage is recorded with zero tokens and zero cost under unmetered_modality, logged as a configuration gap, and counted in janus_unmetered_requests_total. Streamed TTS (stream_format: sse) reports exact usage on its final frame and is metered normally.',
          'This includes /v1/responses and /v1/messages (Anthropic Messages), which get the same policy, routing and metering as chat completions. A request without a body (such as GET /v1/videos/{id}) names its model in the X-Janus-Model header or a ?model= query parameter.',
        ],
      },
      {
        heading: 'POST /v1/decisions, /v1/systemone, /api/alpha/decisions',
        body: [
          'Call a decision model in the OpenAI Decisions format (/v1/decisions) or the System One format (the other two). Any decision model can be reached from any of the three: when the model speaks the other format, Janus translates both ways and says so in X-Janus-Decision-Translated; X-Janus-Decision-Dialect names the format the model was called in. Grants, policy rules, rate limits, the Security Gateway, quotas and metering apply as for chat.',
        ],
      },
      {
        heading: 'Other headers',
        body: [
          'Responses also carry X-Janus-Request-ID (quote it when reporting a problem); X-Janus-Route, X-Janus-Route-Reason and X-Janus-Route-Model when an auto-router chose the model; X-Janus-Fallback-Reason when a managed model was served by its fallback; X-Janus-Security-Action and X-Janus-Redactions when the Security Gateway observed or redacted the request; and X-Janus-Reasoning-Adjusted when Janus changed the requested reasoning effort to fit the model.',
          'Callers can label requests for reports with X-Janus-Project and X-Janus-Cost-Center, and name the calling application with X-Janus-Agent (X-OpenRouter-Title and X-Title are honored too).',
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
          'JANUS_ENCRYPTION_KEY is the only required variable — 32 bytes, given as 64 hex characters (openssl rand -hex 32) or base64, used to encrypt upstream credentials at rest; changing it orphans stored credentials.',
          'JANUS_PUBLIC_URL (default http://localhost:8080) — the externally reachable URL; set it for any deployment reached from another machine. It forms the OIDC redirect URI and the SDK base URL shown in the UI.',
        ],
      },
      {
        heading: 'Database',
        body: [
          'JANUS_DATABASE_URL — postgres:// (or postgresql://) for production, or sqlite:///absolute/path.db for a single-node deployment. Optional: when unset, the gateway defaults to embedded SQLite at /data/janus.db (single-node evaluation mode) and logs a prominent startup warning. The Administration → Settings → System status page reports the backend and sanitized location in force.',
        ],
      },
      {
        heading: 'Identity',
        body: [
          'All identity variables are optional: without an identity provider Janus uses local accounts, and the first sign-in creates the administrator. JANUS_OIDC_PROVIDER_URL, JANUS_OIDC_CLIENT_ID and JANUS_OIDC_CLIENT_SECRET configure the identity provider; if the provider URL is set, the other two must be set too. JANUS_OIDC_EMAIL_CLAIM (default email), JANUS_OIDC_NAME_CLAIM (name) and JANUS_OIDC_GROUPS_CLAIM (groups) override claim names for non-standard providers; JANUS_OIDC_SCOPES (default openid,profile,email,groups) sets the requested scopes. JANUS_BOOTSTRAP_ADMIN_EMAILS grants the admin role on first sign-in (sticky). JANUS_ADMIN_GROUPS — a comma- or space-separated list of IdP group names, matched case-insensitively against the groups claim on every sign-in — grants the admin capability while membership lasts; leaving the group in the identity provider revokes it at the next sign-in, and removing someone from the group in Janus or through SCIM, or removing the group from an admin-group list, revokes it at once. Administrators can add more admin groups under Administration → People → Groups; both lists apply.',
          'JANUS_DEV_AUTH enables the local evaluation sign-in (no identity provider, any email). The gateway refuses to boot with it when JANUS_ENV=production is set explicitly (an unset JANUS_ENV does not count, so the local quick start works) or the public URL is not loopback; JANUS_DEV_AUTH_ALLOW_UNSAFE=true overrides that refusal for isolated demos only.',
        ],
      },
      {
        heading: 'Serving and sessions',
        body: [
          'JANUS_LISTEN_ADDR (default :8080), JANUS_TLS_CERT_PATH / JANUS_TLS_KEY_PATH for direct TLS (set both or neither), JANUS_COOKIE_SECURE to force secure cookies (default: on when the public URL is https), JANUS_TRUSTED_PROXIES as a comma- or space-separated list of IP addresses or CIDR blocks gating X-Forwarded-For, JANUS_CA_BUNDLE for private upstream CAs. Outbound calls honor the standard HTTPS_PROXY, HTTP_PROXY and NO_PROXY variables.',
          'JANUS_SESSION_TTL_HOURS (default 24) and JANUS_SESSION_IDLE_TIMEOUT_HOURS (default 4) bound browser sessions.',
        ],
      },
      {
        heading: 'Proxy behaviour and retention',
        body: [
          'JANUS_UPSTREAM_CONNECT_TIMEOUT_SECONDS (default 10), JANUS_UPSTREAM_TTFB_TIMEOUT_SECONDS (30), and JANUS_UPSTREAM_TOTAL_TIMEOUT_SECONDS (600) set the default bound for each hop. An administrator can override any of them at runtime from Administration → Settings → General → Upstream timeouts (PATCH /api/v1/admin/system/upstream-timeouts): the override is stored in the database, survives restarts, applies to the next request on every replica without a redeploy, and is audit-logged. Raise the time-to-first-byte bound when a busy provider queues requests and callers see 503s. A streamed reply is timed out on silence, not on its total length. The same limits apply when Janus resends a request it fitted to a model’s limits. JANUS_MAX_RESPONSE_BYTES (default 104857600, 100 MiB) caps buffered responses. JANUS_DISCOVERY_INTERVAL_MINUTES (default 2) sets the model-discovery cadence — each run also records whether every upstream is reachable, so it doubles as the availability probe — and an administrator can override it at runtime from Administration → Settings → General → Model discovery (PATCH /api/v1/admin/system/discovery-interval), stored in the database, applied immediately without a restart, and audit-logged.',
          'JANUS_DB_MAX_CONNS (default 25) caps each replica\u2019s PostgreSQL connection pool. Keep replicas \u00d7 this value well under the database\u2019s max_connections, with headroom for maintenance tools: when the database runs out of connections, requests fail.',
          'JANUS_PERFORMANCE_MODE (default false; Business edition) raises the request rate one replica can meter. Usage records are written in batches every 100 ms and committed without waiting for the database\u2019s disk flush. The trade: a database crash can lose about the last 0.5 s of usage records, and a replica that is killed without a graceful shutdown (out of memory, SIGKILL) can lose up to 100 ms of them. A graceful shutdown or rollout loses nothing. Quota counters trail by at most one batch. The mode is decided at startup: without a valid Business or Enterprise license the gateway logs why and runs normally, and Administration \u2192 Settings \u2192 System status shows the mode in force. Leave it off unless one replica is approaching its request ceiling (several hundred requests per second); adding replicas is the other answer.',
          'Usage figures on console lists and dashboards come from hourly totals that each replica\u2019s background job keeps up to date about once a minute (one replica at a time), combined with the last few minutes of the request log, so they always match the request log exactly and stay fast however much history the gateway holds. Purging usage and moving a token\u2019s history to another team update the totals in the same step. After an upgrade the first start fills them from existing history in the background, at roughly 4 seconds per million requests.',
          'JANUS_USAGE_RETENTION_DAYS (default 365) and JANUS_AUDIT_RETENTION_DAYS (730) control purging, run daily at JANUS_PURGE_JOB_TIME_UTC (default 02:00). Keep usage retention at least as long as your longest rolling quota window; a quota with a longer window is refused. JANUS_QUOTA_CHECKPOINT_INTERVAL_HOURS (default 24) bounds quota-counter recovery time. JANUS_SMTP_HOST/PORT/USER/PASSWORD/FROM configure alert email (port defaults to 587, sender to janus@localhost). JANUS_ENV (default production) labels the deployment; JANUS_LOG_LEVEL (default info) is debug, info, warn, or error.',
        ],
      },
      {
        heading: 'Chat and files',
        body: [
          'JANUS_CHAT_PREVIEW (default false) must be true for Janus Chat to be available at all; while it is false, Chat stays off whatever Administration → Chat says. JANUS_CHAT_AUTO_TITLE (default true) names each chat with a short model-written title; the call is metered and shows in the request log as janus-chat-title. JANUS_CHAT_MAX_UPLOAD_BYTES (default 20 MiB) caps one chat attachment; the smaller of this and the storage cap applies. JANUS_CHAT_SEARCH_SEARXNG_URL (unset) points at a SearXNG instance, such as http://searxng:8080, and turns Chat web search on with it once at startup; later admin changes stick.',
          'JANUS_STORAGE_MODE (database or s3; unset lets Administration → Settings → General → File storage decide) fixes where chat attachments and troubleshooting captures are stored. With s3, JANUS_S3_ENDPOINT, JANUS_S3_BUCKET, JANUS_S3_ACCESS_KEY_ID and JANUS_S3_SECRET_ACCESS_KEY are required; JANUS_S3_REGION (default us-east-1), JANUS_S3_PATH_STYLE (default false), JANUS_S3_PREFIX (key prefix) and JANUS_S3_CA_FILE (PEM file of extra CAs for the endpoint) are optional. JANUS_STORAGE_MAX_OBJECT_BYTES (default 25 MiB) caps one stored file.',
        ],
      },
      {
        heading: 'Licensing and updates',
        body: [
          'JANUS_LICENSE_FILE (unset) — path to a license key file; when present it takes precedence over a key installed from the UI. JANUS_LICENSE_SYNC (unset: the UI decides) pins automatic license renewal and sync on or off, and JANUS_LICENSE_SYNC_TOKEN pins its sync token. JANUS_UPDATE_CHECK (default false) turns on the daily check for a newer release. JANUS_OFFLINE (default false) declares an air-gapped deployment: no update check, no license sync, and no personal subscriptions.',
        ],
      },
      {
        heading: 'Telemetry',
        body: [
          'OTEL_EXPORTER_OTLP_ENDPOINT (or the alias JANUS_OTEL_EXPORTER_OTLP_ENDPOINT; unset means no OTLP export, /metrics only) sends metrics to an OpenTelemetry collector. JANUS_OTEL_EXPORT_INTERVAL_SECONDS (default 30) sets how often.',
        ],
      },
      {
        heading: 'Other',
        body: [
          'JANUS_LOCAL_ONLY (default false) is for deployments with no billing: cost tracking is off, USD quotas stop applying and cannot be created, and all pricing is hidden in the UI and in Chat exports. Token, request and latency figures are unaffected.',
          'JANUS_TROUBLESHOOT_DIR (unset) lets troubleshooting sessions store captured bodies as files under that directory instead of in the database. JANUS_CONFIG_CACHE_TTL_SECONDS (default 5) is how long other replicas may serve model, grant, rule, quota rule, Security Gateway policy, Child Safety, router and rate-card settings — and classifier credentials and base URLs — before they see an admin change. Chat administration settings and quota counters refresh every 5 seconds and the file storage backend every 15 seconds, whatever this value is. A license change reaches a replica that serves no license page within an hour.',
          'JANUS_GITHUB_OAUTH_CLIENT_ID, JANUS_OPENAI_OAUTH_CLIENT_ID and JANUS_XAI_OAUTH_CLIENT_ID (unset: built-in IDs) replace the OAuth client IDs used to connect personal subscriptions. JANUS_BUILD_VERSION (default dev), JANUS_BUILD_SHA (unknown) and JANUS_BUILD_DATE are build details set by the image; leave them alone.',
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
          'Not for API traffic: on /v1 calls only request metadata is recorded — who called which model, when, token counts, cost, latency, and status. There are three exceptions. Janus Chat keeps your conversations, encrypted, so you can return to them. A troubleshooting session that an administrator switches on captures the bodies of matching requests for a limited time. A Security Gateway violation keeps the matched text, encrypted, for review.',
        ],
      },
      {
        heading: 'Why does my bill differ from the provider’s invoice?',
        body: [
          'Janus attributes cost from resolved rate cards: admin overrides take priority over upstream metadata, then exact provider references. Discovery refreshes automatic prices; administrators can set per-field overrides with effective dates. If a rate card lags a provider price change, attribution drifts until the card is updated. Text responses where the provider returned no usage block are estimated from byte counts and marked as such in the log; media responses with no usage are recorded at zero and flagged as a metering gap. Calls on your own personal subscription are not charged to the organization.',
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
          'Yes, if an administrator switches on “Team leads may edit quotas” for that team. The lead then manages that team’s quotas from the team page — scoped to that team only, and audit-logged like any admin change. Leads can manage who is on their team either way.',
        ],
      },
      {
        heading: 'Is a token tied to my account?',
        body: [
          'Yes. Every personal API token belongs to one person; its usage, grants, and quotas are that person’s. (Service tokens, which administrators create for unattended integrations, belong to no person.) Only the SHA-256 digest of a token is stored, so a lost token cannot be recovered — revoke it and create a new one.',
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
