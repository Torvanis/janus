package httpapi

import "strings"

// helpTopics lists the in-app help sections. These are short and task-shaped;
// the full reference lives in the documentation site under /docs.
func helpTopics() []map[string]string {
	return []map[string]string{
		{"id": "integration", "title": "Connect a tool", "summary": "Point any OpenAI-compatible client at Janus."},
		{"id": "tokens", "title": "API tokens", "summary": "Create, rotate, and revoke your credentials."},
		{"id": "models", "title": "Models", "summary": "See which models you can call and what they cost."},
		{"id": "quotas", "title": "Quotas", "summary": "Understand your limits and what happens when you reach them."},
		{"id": "troubleshooting", "title": "Troubleshooting", "summary": "Decode the error you just received."},
		{"id": "glossary", "title": "Glossary", "summary": "Terms used across the gateway."},
	}
}

// helpTopic renders one help topic with snippets pre-filled for this caller.
func helpTopic(topic, endpoint, token, model string) (map[string]any, bool) {
	base := strings.TrimRight(endpoint, "/") + "/v1"

	switch topic {
	case "integration":
		return map[string]any{
			"id":    "integration",
			"title": "Connect a tool",
			"body": "Janus speaks the OpenAI API. Point any compatible client at the gateway base URL below and " +
				"authenticate with one of your API tokens. Every call is metered against your quotas and appears " +
				"in your dashboard within a few seconds.",
			"docs_url": "/docs/getting-started",
			"fields": []map[string]string{
				{"label": "Base URL", "value": base},
				{"label": "Token", "value": token},
				{"label": "A model you can call", "value": model},
			},
			"snippets": []map[string]string{
				{
					"language": "bash", "label": "curl",
					"code": "curl " + base + "/chat/completions \\\n" +
						"  -H \"Authorization: Bearer $JANUS_API_KEY\" \\\n" +
						"  -H \"Content-Type: application/json\" \\\n" +
						"  -d '{\n" +
						"    \"model\": \"" + model + "\",\n" +
						"    \"messages\": [{\"role\": \"user\", \"content\": \"Hello from Janus\"}]\n" +
						"  }'",
				},
				{
					"language": "python", "label": "Python (OpenAI SDK)",
					"code": "from openai import OpenAI\n\n" +
						"client = OpenAI(\n" +
						"    base_url=\"" + base + "\",\n" +
						"    api_key=os.environ[\"JANUS_API_KEY\"],\n" +
						")\n\n" +
						"response = client.chat.completions.create(\n" +
						"    model=\"" + model + "\",\n" +
						"    messages=[{\"role\": \"user\", \"content\": \"Hello from Janus\"}],\n" +
						")\n" +
						"print(response.choices[0].message.content)",
				},
				{
					"language": "typescript", "label": "JavaScript / TypeScript",
					"code": "import OpenAI from 'openai';\n\n" +
						"const client = new OpenAI({\n" +
						"  baseURL: '" + base + "',\n" +
						"  apiKey: process.env.JANUS_API_KEY,\n" +
						"});\n\n" +
						"const response = await client.chat.completions.create({\n" +
						"  model: '" + model + "',\n" +
						"  messages: [{ role: 'user', content: 'Hello from Janus' }],\n" +
						"});",
				},
				{
					"language": "python", "label": "LangChain",
					"code": "from langchain_openai import ChatOpenAI\n\n" +
						"llm = ChatOpenAI(\n" +
						"    base_url=\"" + base + "\",\n" +
						"    api_key=os.environ[\"JANUS_API_KEY\"],\n" +
						"    model=\"" + model + "\",\n" +
						")\n" +
						"print(llm.invoke(\"Hello from Janus\").content)",
				},
				{
					"language": "bash", "label": "Hermes",
					"code": "# Point Hermes at Janus as an OpenAI-compatible provider.\n" +
						"# Run these three commands, then start a new session.\n\n" +
						"hermes config set providers.janus.base_url " + base + "\n" +
						"hermes config set providers.janus.api_key \"$JANUS_API_KEY\"\n" +
						"hermes config set providers.janus.model " + model + "\n\n" +
						"# Point Hermes at that provider and confirm it took:\n" +
						"hermes config set provider janus\n" +
						"hermes config get provider\n\n" +
						"# Verify end to end — this call appears in your Janus\n" +
						"# dashboard within a few seconds:\n" +
						"hermes --provider janus -p \"Hello from Janus\"\n\n" +
						"# Optional: identify Hermes in the Janus request log. Without\n" +
						"# this, every SDK-based client reports only \"OpenAI/Python\".\n" +
						"hermes config set providers.janus.extra_headers.X-Janus-Agent hermes\n\n" +
						"# base_url must be the FULL API root including /v1.\n" +
						"# If a call 404s, that is almost always the missing /v1.\n" +
						"# Alternatively, edit ~/.hermes/config.yaml directly:\n" +
						"#   provider: janus\n" +
						"#   providers:\n" +
						"#     janus:\n" +
						"#       base_url: " + base + "\n" +
						"#       api_key: <your Janus token, e.g. " + token + ">\n" +
						"#       model: " + model,
				},
				{
					"language": "bash", "label": "Desktop clients (LM Studio, GPT4All, Open WebUI)",
					"code": "# In the client's OpenAI-compatible provider settings:\n" +
						"Base URL : " + base + "\n" +
						"API key  : your Janus token\n" +
						"Model    : " + model + "\n\n" +
						"# Most clients call /v1/models on connect; Janus returns only the\n" +
						"# models you have been granted.",
				},
			},
		}, true

	case "tokens":
		return map[string]any{
			"id": "tokens", "title": "API tokens", "docs_url": "/docs/guides/tokens",
			"body": "Tokens identify you to the gateway. Create one per tool so you can revoke a single " +
				"integration without disrupting the rest. Janus stores only a hash of each token: the value is " +
				"shown once at creation and can never be retrieved again. Revocation takes effect immediately.",
			"snippets": []map[string]string{
				{"language": "bash", "label": "Use a token from the environment",
					"code": "export JANUS_API_KEY=\"janus_…\"   # the value you copied at creation\n" +
						"curl " + base + "/models -H \"Authorization: Bearer $JANUS_API_KEY\""},
			},
		}, true

	case "models":
		return map[string]any{
			"id": "models", "title": "Models", "docs_url": "/docs/guides/models",
			"body": "The model list is filtered to what you have been granted, and each entry appears under the " +
				"name your administrators chose for it. Use the model name shown in the catalog when making " +
				"requests — it is the name the gateway routes on. Administrators can rename a model at any " +
				"time; when that happens the original upstream name keeps working as a fallback, so existing " +
				"tool configurations never break. If a model you expect is missing, ask an administrator to " +
				"grant it to you, your group, or all users. Prices are shown per million tokens and are the " +
				"rates used to attribute your spend. A rate card carries up to five prices: " +
				"input, output, cached input, and — for providers that bill prompt-cache writes, such as " +
				"Anthropic — 5-minute and 1-hour cache-write rates. A price of $0 means the model is explicitly " +
				"free (typical for self-hosted models), not unpriced: administrators must save a rate card, even " +
				"an all-zero one, before a model can be enabled. The bundled reference prices are a snapshot at " +
				"the time they were authored — administrators should verify them against the provider's current " +
				"pricing before applying. Each model also shows its context window — the maximum number of " +
				"tokens it accepts as input (the catalog's context_window field, in tokens). Known models get " +
				"the value from the bundled reference seed; administrators can set or correct it on any model. " +
				"A dash means the context window is not recorded (context_window 0), not that the model has none.",
			"snippets": []map[string]string{
				{"language": "bash", "label": "List models you can call",
					"code": "# Each entry's \"id\" is the model name to use in requests. Renamed models\n" +
						"# also report their original upstream name as \"janus.native_name\".\n" +
						"curl " + base + "/models -H \"Authorization: Bearer $JANUS_API_KEY\""},
			},
		}, true

	case "quotas":
		return map[string]any{
			"id": "quotas", "title": "Quotas", "docs_url": "/docs/guides/quotas",
			"body": "Quotas cap input tokens, output tokens, spend, or request count over a window. Multiple " +
				"quotas combine, and the most restrictive one wins. You are warned at 80% and 95%. On breach the " +
				"next request is refused with HTTP 429 and a reset time. By default an in-flight request is " +
				"allowed to finish; administrators can switch a quota to cut streams immediately instead. " +
				"Prompt-cache writes (the 5-minute and 1-hour cache-write token counts some providers report) " +
				"are metered separately from input tokens: they count toward spend quotas at the model's " +
				"cache-write rates, but not toward an input-token quota.",
			"snippets": []map[string]string{
				{"language": "json", "label": "What a quota breach looks like",
					"code": "{\n  \"error\": {\n    \"message\": \"Daily input-token quota exceeded.\",\n" +
						"    \"code\": \"policy.quota_exceeded\",\n    \"type\": \"rate_limit_error\",\n" +
						"    \"reset_at\": \"2026-08-16T00:00:00Z\",\n    \"retry_after\": 3600\n  }\n}"},
			},
		}, true

	case "troubleshooting":
		return map[string]any{
			"id": "troubleshooting", "title": "Troubleshooting", "docs_url": "/docs/troubleshooting",
			"body": "Every gateway error carries a stable `code`. Look it up below, or open the error catalog for " +
				"the full list with causes and fixes.",
			"errors": []map[string]string{
				{"code": "policy.token_invalid", "status": "401", "fix": "The token is missing, revoked, or mistyped. Create a new one on the Tokens page."},
				{"code": "policy.user_disabled", "status": "403", "fix": "Your account is disabled. Contact an administrator."},
				{"code": "policy.model_not_granted", "status": "403", "fix": "You have not been granted this model. Call /v1/models to see what you can use."},
				{"code": "policy.endpoint_blocked", "status": "403", "fix": "A gateway policy rule matched this request. The reason field explains which signal fired."},
				{"code": "policy.quota_exceeded", "status": "429", "fix": "You reached a limit. Wait until reset_at, or ask for a higher quota."},
				{"code": "upstream.unavailable", "status": "503", "fix": "The provider could not be reached. Retry shortly."},
				{"code": "upstream.rate_limit", "status": "429", "fix": "The provider throttled the gateway. Back off and retry."},
			},
		}, true

	case "glossary":
		return map[string]any{
			"id": "glossary", "title": "Glossary", "docs_url": "/docs/glossary",
			"terms": []map[string]string{
				{"term": "Upstream", "definition": "A configured provider endpoint that Janus forwards requests to."},
				{"term": "Adapter", "definition": "The plug-in that translates between the OpenAI wire format and one provider's protocol."},
				{"term": "Grant", "definition": "Permission for a user, a group, or everyone to call a specific model."},
				{"term": "Rate card", "definition": "The per-million-token prices used to attribute spend: input, output, cached input, and 5-minute/1-hour cache-write rates. Versioned, so historical usage keeps its original rate. An explicitly saved $0 card means free, not unpriced."},
				{"term": "Context window", "definition": "The maximum number of tokens a model accepts as input, shown in the catalog (context_window). Seeded from the bundled reference data for well-known models and correctable by administrators; 0 renders as a dash meaning unknown."},
				{"term": "Modality", "definition": "What kind of work a request performs: chat, embedding, image, audio, and so on."},
				{"term": "Cached tokens", "definition": "Input tokens the provider served from its own prompt cache, usually billed at a lower rate."},
				{"term": "Cache-write tokens", "definition": "Tokens written to the provider's prompt cache (5-minute or 1-hour TTL). Anthropic bills these above the input rate; providers that do not report them meter zero."},
				{"term": "Quota window", "definition": "The period a limit is measured over: calendar day, week, month, or a rolling span."},
				{"term": "TTFB", "definition": "Time to first byte — how long the provider took to start responding."},
			},
		}, true
	}
	return nil, false
}
