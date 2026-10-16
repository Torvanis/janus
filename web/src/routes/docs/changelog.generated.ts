// GENERATED FILE — DO NOT EDIT BY HAND.
// Source of truth: CHANGELOG.md at the repository root.
// Regenerate with: cd web && node scripts/gen-docs.mjs
// Drift-checked by src/docs/gen.test.ts.

export interface ChangelogCategory {
  name: string;
  items: string[];
}

export interface ChangelogRelease {
  version: string;
  /** Release date as written in CHANGELOG.md; empty when undated. */
  date: string;
  categories: ChangelogCategory[];
}

export const CHANGELOG: ChangelogRelease[] = [
  {
    version: '2026.10.3',
    date: '2026-10-16',
    categories: [
      {
        name: 'Added',
        items: [
          'Auto-routing (Administration → Auto-routing). Let Janus pick the model for each request. A router is a model name callers use (auto/legal, auto/smart): a decision model classifies each request by kind of work and difficulty, and the router sends it to the first route that fits, or to its default. Routing never fails a request; an unsure, slow or failed classifier, or no fitting route, serves the default. - Routes choose a model or managed model by routing category, difficulty band, context fit, grant and quota, and set thinking (as requested, fixed, or scaled with difficulty). Templates: Cost saver, Keep specialist work on-prem, Effort scaler, Coding by difficulty. - 15 built-in routing categories plus a Legal example, each with includes/does-not-include descriptions, a minimum confidence and test examples; or follow a Usage insights category. - Draft, Shadow (served by the default while Janus records what it would have done) and Live states. Conversations stick to their route (30 minutes by default) without another classifier call, and later turns are held to the same rules as the first. - Routers are managed models: grant them, bind security policies and scope quotas to them. - Try routing, and a batch test of each category\'s examples with any classifier; a warning when a hosted classifier would read prompts that are routed to self-hosted models. - Evaluation: a sample of a route\'s requests is also answered by the default, out of band, and a judge model compares the two blind, twice with the order swapped. Verdicts, a 95% range and cost against the default per route; prompts and answers are never stored. A daily count, budget, end date and a busy-gateway pause bound it, and it never counts against the caller\'s quota. - The request log shows the router asked for, the model that served and why; optional X-Janus-Route, X-Janus-Route-Reason and X-Janus-Route-Model response headers.',
          'Usage insights (Administration → Usage insights). See what kinds of work people use AI for. A classifier labels sampled chat requests from a built-in taxonomy of 22 categories (Coding, Technical help, Legal, Medical and health, Writing and editing, and so on), plus your own categories and subcategories. Reports show categories over time, share, confidence, subcategories, models and people, the mix per team, and the classifier\'s own latency, failures, tokens and cost. Only labels are stored, never prompts. - Any decision model can classify (System One models such as TypeSafe Jev, Bespoke Nimble or Laya, served by TypeSafe, OpenRouter, Ollama or llama.cpp; or OpenAI Decisions models), or any chat model Janus can reach, prompted to answer in JSON. - Runs in the background (no added latency) or inline, where the label is returned in X-Janus-Category within a time budget; a classifier failure never fails the request.',
          'Decision models. A new model role for classifiers that answer typed questions (yes/no, choice, score) in the System One format (/v1/systemone) or the OpenAI Decisions format (/v1/decisions). Janus picks the format from the provider and translates between them. - As security guards: a decision model can back prompt-injection checks, content-safety checks (requests and, observed, responses), or both, as an alternative to Llama Prompt Guard 2 and Llama Guard 4. Model-backed checks run at the same time, so a request waits for its slowest guard, not all of them in turn. Checks bound to the same decision model are asked together when the model can, or in parallel when it cannot. Two options per check: What to judge (the new turn, or the whole conversation) and Long text (the last 12,000 characters, or the whole text in windows). - For people and apps: call any granted decision model at /v1/decisions (OpenAI SDKs), /v1/systemone (TypeSafe SDK) or /api/alpha/decisions (OpenRouter SDK). Calls are granted, checked by security policies (questions and evidence both), counted against quotas, priced and logged like any other request. A decision model that blocks requests in a security policy can\'t be granted unless an administrator allows it. /v1/models lists decision models with janus.kind: "decision". - Pricing per provider: each decision model has a cost source (automatic, the provider\'s reported charge, the rate card, or free) and an optional fee per call; the same figure prices Janus\'s own classifier calls.',
          'Classifier benchmark (Usage insights → Performance). Measure what your classifier choices cost each request: the wait with nothing else running, typical and 95th-percentile wait at rising concurrency, classifier calls per request, and the most simultaneous requests that stay within a latency budget you set. Sample prompts run through the real classifiers; nothing is forwarded or logged.',
          'The request log records how long each request waited on classifiers and how many calls that took, shows how Usage insights labelled it (Classified as, with a filter), and troubleshooting captures include every classifier call a request caused.',
          'Models page: filter a large catalog by kind, by what a model is used as, by price and by access, with counts on every choice; every column sorts, and unpriced models sort last, never as free. Requests page: filter and sort by kind, model, status, time to first byte and classifier wait.',
        ],
      },
      {
        name: 'Changed',
        items: [
          'Thinking settings work on every provider. Chat clients such as Open WebUI send one reasoning effort, temperature and set of tools to every model, and providers each refused part of that. When a provider refuses a setting and names it, Janus now fits the request and sends it again before anything reaches the caller, and remembers the fix per model for a day: a refused reasoning level becomes the nearest one the model takes, a refused temperature or top_p is left out, and GPT-5/GPT-6 tool calls go through OpenAI\'s Responses API and come back as a normal chat completion. Changes are shown in X-Janus-Reasoning-Adjusted and the request log.',
          'The navigation item Tokens is now API Tokens.',
          'Helm chart renamed to janus-helm-chart (chart 0.2.0, file janus-helm-chart-0.2.0.tgz, source in charts/janus-helm-chart). Rendered resource names and labels are unchanged, so helm upgrade of an existing release works as before; point it at the new file.',
          'Offline container images are attached to the release as janus-container-image-2026.10.3-linux-amd64.tar and janus-container-image-2026.10.3-linux-arm64.tar, for installing without registry access (docker load -i … or podman load -i …).',
          'Built with Go 1.26.9.',
        ],
      },
      {
        name: 'Fixed',
        items: [
          'Claude now thinks when asked. reasoning_effort was dropped on the way to Anthropic. Claude 4.6 and later (all of Claude 5) get adaptive thinking with that effort and return a summary of the thinking as reasoning_content; earlier models get a thinking budget kept within the model\'s output limit. Auto-routing\'s forced levels use the same translation.',
          'Gemini tool calls no longer fail on the second turn when the client drops the thought signature: Janus restores it.',
          'An observe-mode security check no longer blocks requests when its classifier is down; fail closed applies to blocking checks only.',
          'A failed decision-model catalog read no longer switches decision models off.',
          'Classifier benchmarks interrupted by a restart no longer show as running forever, and only one runs at a time across replicas.',
        ],
      },
    ],
  },
  {
    version: '2026.10.2',
    date: '2026-10-04',
    categories: [
      {
        name: 'Added',
        items: [
          'Google Gemini as a provider: choose Google Gemini (AI Studio key) and paste an API key from Google AI Studio. Janus uses Google\'s OpenAI-compatible endpoint, lists the models the key can call with their context windows (Live-API models, which that endpoint cannot call, are left out), meters thinking tokens as output as Google bills them, and records implicit prompt-cache hits as cached input. The bundled reference prices Gemini 2.5 Flash and Flash-Lite, 3 Flash Preview, 3.1 Flash-Lite, 3.5 Flash and Flash-Lite, and Embedding 2.',
          'Adding an upstream starts from a provider list: OpenAI, Anthropic, Google Gemini, Google Vertex AI, AWS Bedrock, xAI, Mistral, OpenRouter, Groq, DeepSeek, Together AI, Fireworks AI, Cerebras, Moonshot AI, the self-hosted engines, and any other OpenAI-compatible endpoint. Choosing one fills in its documented base URL and a suggested name. Both stay editable, and a value you typed is kept when you switch provider.',
        ],
      },
      {
        name: 'Fixed',
        items: [
          'A base URL that ends in an API version (…/v1, …/openai/v1, Google\'s …/v1beta/openai) is used as the API root, the way providers document it for OpenAI clients. Before, only a trailing /v1 was recognised, so …/v1beta/openai was called as …/v1beta/openai/v1/… and failed.',
          'Streamed output speed for providers that generate reasoning without streaming it (Gemini) is measured over the whole call; it was divided by the few milliseconds after the first byte and reported thousands of tokens per second.',
          'On a phone, the Upstreams page no longer scrolls sideways when an upstream has a discovery error.',
        ],
      },
    ],
  },
  {
    version: '2026.10.1',
    date: '2026-10-04',
    categories: [
      {
        name: 'Added',
        items: [
          'Load-balanced model pools (Business). A managed model can put several servers running the same model behind one name. Balancing can be failover, round robin by weight, least loaded (queue length read from each vLLM or llama.cpp server\'s /metrics every 2 seconds) or context aware (share of each server\'s context capacity in use, from vLLM KV-cache figures or llama.cpp /slots). Every turn of a conversation goes to the server that already holds its prompt cache, recognised from a session header, prompt_cache_key, or the conversation\'s opening when the client sends neither. A server that refuses a request before answering is skipped within the same call; repeated failures take it out on every replica together. Responses carry X-Janus-Pool-Member and X-Janus-Pool-Reason, and the editor shows each server\'s queue, context use and prompt-cache hit rate live.',
          'Personal subscriptions. People can connect a provider plan they already pay for (OpenAI ChatGPT Plus or Pro, GitHub Copilot, xAI SuperGrok or X Premium+, Mistral) on the new Subscriptions page and call its models as my/<provider>/<model> with their own Janus token. Calls are metered, logged and covered by security policies like any other. Administrators turn the feature on organization-wide; it is off in offline mode. Reasoning effort is fitted to what each plan\'s model accepts, and a sign-in that stops working returns policy.subscription_reauth_required.',
          'Model speed and activity. Model cards show tokens per second and jobs per day with a trend; the admin Overview has a Model performance card charting each model\'s concurrency, speed and input size over time; the live readout adds a per-second line. Engine-measured speeds (including vLLM per-request metrics) are preferred over Janus\'s own estimates.',
          'A Self-hosted engines guide covering the vLLM, llama.cpp, Ollama and TEI flags that give Janus exact token counts, speeds, context windows and live pool load.',
          'Performance mode (Business, JANUS_PERFORMANCE_MODE=true) writes usage in 100 ms batches and commits without waiting for the database\'s disk flush, for the busiest gateways. A database crash can lose about the last 0.5 s of usage records and a replica killed without a graceful shutdown up to 100 ms of them; graceful shutdowns lose nothing. The state is shown on Admin → System.',
          'JANUS_DB_MAX_CONNS (default 25) caps each replica\'s PostgreSQL connection pool. The previous fixed ceiling was 50.',
        ],
      },
      {
        name: 'Changed',
        items: [
          'Built for large networks. Usage is recorded without locking shared rows, which roughly tripled per-replica throughput in testing and lets throughput grow with replicas. Console pages that show 30-day usage per row (API tokens, Service tokens, Managed models, the admin Overview) read hourly totals that a background job keeps current, so they load in well under a second after millions of requests instead of tens of seconds. The figures still match the request log exactly. The first start after upgrading fills the totals from existing history in the background (about 4 seconds per million requests); pages work normally meanwhile.',
          'Usage keeps the team it was admitted with. Joining, leaving or switching teams no longer moves recorded usage, including a person\'s first team. To move a token\'s history, use Change team on the token and choose to move its recorded usage.',
          'The Subscriptions page has a tab per provider, lists the models you use first, and adds models from a side panel; removing a model can be undone.',
          'Managed models open in a side panel, and Model grants is one dense table like the other admin pages.',
          'The Troubleshooting page covers every error code Janus can return.',
          'Quota rules are cached for the configuration cache interval and refreshed on every quota change, and last_used_at on tokens is written at most every 30 seconds per token.',
        ],
      },
      {
        name: 'Fixed',
        items: [
          'A stream the provider ends early is recorded as a failure, and streams without a usage block are estimated from the text they carried instead of being metered as zero.',
          'Writing a usage record no longer clears the model and rate-card caches.',
        ],
      },
    ],
  },
  {
    version: '2026.9.5',
    date: '2026-09-26',
    categories: [
      {
        name: 'Added',
        items: [
          'One-paste activation: install the activation code from the license portal and tick Automatically renew and sync in the same step. The gateway installs the key, stores the sync token, and reports any sync problem immediately.',
          'License & updates opens with a single verdict: a green check when the gateway is licensed and healthy, a yellow Attention needed listing each reason (expiring without confirmed renewal, grace period, over seats or nodes, renewal sync stale or failing, cancellation scheduled, clock skew, unsupported version), and a red Not properly licensed when the key is expired or invalid. The page fits on one screen at common desktop sizes.',
        ],
      },
      {
        name: 'Changed',
        items: [
          'Reports: a redesigned result view, library actions, and cleaner PDF, CSV and spreadsheet exports.',
          'My teams is one compact workspace per team: header, a summary strip (members, requests, tokens, spend, error rate), an activity chart, usage by model and the member roster side by side.',
          'Admin tables act from one line per row: row actions are icon buttons whose label appears on hover and keyboard focus, and the actions column stays pinned on narrow windows. People uses tighter rows and a Rows per page choice (25–200).',
          'License sync token and key fields use the standard themed inputs.',
        ],
      },
      {
        name: 'Fixed',
        items: [
          'The same model name served by more than one upstream no longer breaks grants: a request goes to the most recently discovered copy the caller is granted and is billed at that copy\'s rate card. The Models and Grants pages label copies by upstream.',
          'Input tokens-per-second no longer counts prompt-cache hits as processed input.',
          'An upstream can be created with the name of a deleted one (deleted upstreams keep usage history but no longer reserve their name).',
          'The container image no longer declares VOLUME /data, so PostgreSQL deployments do not get an anonymous volume. SQLite users still mount /data as documented.',
          '"Rows per page" no longer wraps on narrow tables; Help\'s topic list and snippet tabs render correctly.',
        ],
      },
    ],
  },
  {
    version: '2026.9.4',
    date: '2026-09-25',
    categories: [
      {
        name: 'Fixed',
        items: [
          'Local-only mode (JANUS_LOCAL_ONLY=true) can enable newly discovered models again. The mode hides all pricing, but enabling a model that had never been priced was still refused with "Save a rate card first", leaving no way to enable it. A rate card is now required only when cost tracking is on, and the Models, Upstreams and Administration pages no longer ask local-only administrators to price models.',
        ],
      },
    ],
  },
  {
    version: '2026.9.3',
    date: '2026-09-24',
    categories: [
      {
        name: 'Added',
        items: [
          'Linux server installation: sudo python3 install.py installs Janus Edge as a systemd service running as an unprivileged janus user, with HTTPS on port 443 and port 80 redirecting to HTTPS. The first administrator is created while the gateway still listens only on loopback, so nobody else can claim a new server.',
          'Certificates: a self-signed certificate is created on first install; janus-ctl cert install installs your own certificate after checking that the key matches and it has not expired, and janus-ctl cert acme obtains a Let\'s Encrypt certificate over HTTP-01 and renews it automatically with a systemd timer.',
          'janus-ctl status, upgrade and uninstall manage the service. Upgrades back up a SQLite database first and never rotate the encryption key; uninstall keeps configuration and data unless --purge is given.',
        ],
      },
      {
        name: 'Changed',
        items: [
          'The previous no-sudo installation into ~/.local with the foreground loopback launcher remains available as python3 install.py --user.',
        ],
      },
    ],
  },
  {
    version: '2026.9.2',
    date: '2026-09-21',
    categories: [
      {
        name: 'Added',
        items: [
          'Opt-in automatic license renewal sync for subscription licenses, off by default, under Admin → Settings → License & updates. Enable it explicitly in the UI or with JANUS_LICENSE_SYNC=true; storing a sync token or installing an online key never enables it on its own, and offline mode always prevents remote sync.',
          'Sync token onboarding: paste the raw sync token from the license portal, save, and use Sync now. Blank saves preserve the stored credential, and a clear action removes it. JANUS_LICENSE_SYNC_TOKEN pins the credential from the environment.',
          'Renewal attention notices in the shell banner and the license card that are independent of signed license validity: cancellation, payment failure, stale or failed sync, and exhausted grace stay visible while routine expiry warnings are suppressed only during a fresh, confirmed automatic renewal.',
        ],
      },
      {
        name: 'Changed',
        items: [
          'Revocation recovery is replay-safe: after an advisory revocation the gateway keeps syncing but clears the warning only for a newer same-identity signed license accompanied by fresh subscription evidence, so an old revoked response cannot restore a stale state.',
        ],
      },
    ],
  },
  {
    version: '2026.9.1',
    date: '2026-09-20',
    categories: [
      {
        name: 'Added',
        items: [
          'Initial public release of Janus Edge, a self-hosted AI gateway with provider routing, model discovery, and OpenAI-compatible APIs.',
          'User and service tokens, identity-provider integration, local authentication, team membership, quotas, and usage reporting.',
          'Security policies, guard classifiers, audit logging, and administrative troubleshooting.',
          'Separate personal Team workspaces and organization-wide team management under Administration → People → Teams.',
          'Offline license verification and self-hosted installation support.',
        ],
      },
    ],
  },
];
