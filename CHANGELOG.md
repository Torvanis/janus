# Changelog

## [2026.10.5] — 2026-10-30

### Added

- **Janus Chat (preview; turn on with `JANUS_CHAT_PREVIEW=true`).** Signed-in users get a Chat page. Every message goes through the normal gateway path, so grants, quotas, security policies, Child Safety and routing apply, and it is metered like API traffic (request log client `janus-chat`, with tokens, cost and speed on each reply). Chat bills to the "Working under" selection. Messages, instructions and files are stored encrypted, and administrators cannot read anyone's chats.
  - **Conversations:** a history rail grouped by Today / Yesterday / Previous 7 and 30 days, with rename, pin and delete; streaming replies with Stop, edit and regenerate as branches; replies keep generating if the tab closes (a reply cut off by a restart is marked interrupted). The thread follows a reply only while you are at the bottom; **Jump to latest** brings you back. Shortcuts: Ctrl/Cmd+Shift+O new chat, Esc stop, / jump to the message box.
  - **Formatted replies:** markdown with tables, task lists and footnotes; highlighted code with Copy, wrap, line numbers and Download; math and mermaid diagrams. HTML in a reply is shown as text and remote images load only on click. A complete HTML page or SVG shows as a file card with Preview (sandboxed, no access to Janus), Download and Copy.
  - **Message bar and Tools & models tray:** the bar is attach · model · **Tools** · send; who pays is under the box. The tray holds Deep research and Compare for the next message, this chat's tool switches, who pays, and **Your models** (your own chat, image, video and music models, used as defaults in every chat). The model picker lists only chat models, grouped by the categories, chat names, descriptions and Recommended badges administrators set, with a search box once there are many.
  - **Instructions and thinking:** the chat settings hold instructions for this chat, your own instructions for every chat, a Thinking choice (sent as `reasoning_effort` and fitted to the model), and Memory. Temporary chats are never listed or searched and are removed once idle (a day by default; Administration → Chat sets 1 hour–30 days). Short notes under a reply say when auto-routing or a fallback answered, when Janus adjusted the thinking level, and when a security policy flagged, redacted or stopped it.
  - **Saved prompts, sharing and export:** type `/` to insert a saved prompt (with `{{variables}}`); share a read-only snapshot with anyone signed in to this Janus (audited, revocable); export as Markdown, JSON or PDF. The box estimates the next message's input tokens and cost, and warns when a quota is close to its limit.
  - **Search and titles:** search your chats by whole words (only keyed, per-user word hashes are stored; rebuild with `POST /api/v1/admin/chat/reindex` after changing the encryption key). The chat's model names each conversation (`janus-chat-title` request; turn off with `JANUS_CHAT_AUTO_TITLE=false`). When a conversation outgrows the context window the oldest messages are left out and the thread says how many.
  - **Attachments:** images (PNG, JPEG, GIF, WebP), documents (PDF, Word, text, Markdown, CSV, JSON, YAML, HTML, logs, code — converted to text), sound (MP3, WAV, M4A, OGG, FLAC, AAC, Opus) and video (MP4, MOV, WebM). Files are checked by their bytes; up to 10 per message, each up to `JANUS_CHAT_MAX_UPLOAD_BYTES` (20 MiB, never more than the file storage limit). A model that cannot hear or watch gets a transcript or a video description instead, made once and reused. Unsent files are removed after a day.
  - **Files from the model:** any tool-calling model can hand you a real file (code, pages, documents, CSV/JSON), shown as a file card with Preview, Download and Copy and deleted with the chat.
  - **Compare** (in the tray): up to two more models answer the same message side by side with their own time and cost. The first model picked is **Preferred** and the conversation continues from its answer; **Prefer this answer** under another reply continues from that one instead.
  - **Memory:** tell the assistant to remember something and later chats use it (encrypted, owner-only). The chat settings list, edit and delete memories and have an off switch; temporary chats never use it.
  - **Deep research** (in the tray): the model plans, searches up to 20 rounds, reads the most relevant pages and writes a cited report, saved on the reply as a Markdown file.
  - **Images, video and music:** ask any chat model for a picture, video or song. Images use the organization's image model or your plan's; video uses Grok Imagine, Veo or Sora and music Google Lyria. Each is its own metered request. Results are kept in Janus's encrypted file storage, visible only to their owner, and deleted with the chat.
  - **Personal subscriptions:** models from your own plans appear under "Your subscriptions" and are paid by that plan (recorded at $0 and still metered). `GET /api/v1/models` returns them as `personal_models`, with `janus.modalities`.
- **Administration → Chat.** **General:** turn Chat off for everyone (nothing is deleted), default model, instructions sent first in every chat, empty-screen suggestions, temporary-chat retention, and switches for share links, export and personal plans. **Models:** which granted models show in Chat, with category, order and Recommended badge, and **Sees images**, **Hears sound** and **Sees video** per model. **Tools:** tool switches, search service, image, video, music, transcription and video-description models, Memory. **Who can use Chat** (Business): everyone or chosen groups, teams and people; administrators always keep access.
- **Chat names on grants.** A grant can give a model a chat name and description; the most specific grant wins and reports keep the real name.
- **Reports by who paid.** Reports can group by `billing_source` (the organization or a personal plan).
- **Provider-run tools, in Chat and through the gateway.** On by default; off for everyone in Administration → Chat → Tools, or per chat in the Tools tray. A model gets only the tools its provider really runs. Janus never runs code itself.
  - **Claude:** web search, reading web pages and running code, citations kept ($10 per 1,000 searches counted).
  - **Gemini 3 and newer:** Google Search grounding, reading web pages and running code through Gemini's native API ($14 per 1,000 searches).
  - **OpenAI and xAI:** web search (and X search for Grok) and running code in the provider's sandbox; xAI's exact cost, search fees included, is metered.
  - `/v1/chat/completions` accepts the generic tool types `web_search`, `x_search`, `web_fetch` and `code_execution`; activity and citations come back as `janus_activity` and `annotations`. A model whose provider runs none is refused with a clear 400.
- **Read web pages with any chat model.** Models without their provider's reader use Janus's own. Public internet only (private, loopback, link-local and cluster addresses are refused by name, after DNS and on every redirect); up to 5 MB fetched and the first 30,000 characters read; the page is cited under the reply and marked to the model as information, not instructions.
- **Web search for every other model** (vLLM, llama.cpp, Ollama…) through SearXNG, Brave Search, Exa or Tavily, set up in Administration → Chat → Tools with a **Test search** button (off by default; keys stored encrypted and never shown again). `JANUS_CHAT_SEARCH_SEARXNG_URL` turns it on once at startup with a SearXNG you run next to Janus.
- **File storage** (Administration → Settings → General → File storage): the database (default) or any S3-compatible bucket, with Test connection. Files are encrypted by Janus before they are stored. Fix the choice with `JANUS_STORAGE_MODE` and `JANUS_S3_*` (in the Helm chart, under `extraEnv`). Troubleshooting captures go to object storage when file storage is S3.
- **Child Safety (every edition).** Administration → Safety & Security → Child Safety protects children on the gateway. Rules attach to the child (directly or through a team) and cover every model they can use, personal plans and future models included.
  - Age ranges Little kids, Kids and Teens, plus your own copies. Six categories (feeling low, not suitable for children, getting around parental controls, personal information, homework done for them, homework learning) are each Off, Answer appropriately or Block, with editable instructions. The critical safety categories always block.
  - Tutor rule: while homework is answered appropriately the model never gives the final answer and only says whether the child's own answer is right; a reply that gives it away stops with a short note.
  - Feeling low set to Block (and self-harm, always) answers with a kind message pointing to a trusted adult and the 988 Suicide & Crisis Lifeline, and administrators are notified.
  - Children can use chat (chat completions, completions, embeddings). Each age range has **What they can make** switches (Make pictures, Make videos, Make music, Turn speech into text), all off by default; when one is on, the child's request is checked first. Replies are checked paragraph by paragraph as they stream. An optional per-range model block-list hides models from the child.
  - The safety model is any decision model (hosted Jev recommended; Clef, Clef-Flash or Nimble for local) or a Llama Guard model. Without a working one, Child Safety fails closed and shows **Not protecting**.
- **Classifiers read follow-ups in context.** Auto-routing, Usage insights and Child Safety judge the latest message with the recent conversation as context (the last six messages, assistant replies cut to 300 characters). "Keep going" on a hard coding task stays hard coding; first messages are classified as before. On hosted Jev with 20 follow-up and topic-switch conversations, the category was right 17 times (was 8) and the difficulty 10 of 11 (was 4). Security guards still judge the new turn alone.
  - Sticky routers keep the conversation's model, but thinking that scales with difficulty is set from each turn (the last level if that fails).
  - Usage insights' default text is "Latest message, with recent conversation"; settings on the old default move to it.
  - Try routing takes an optional earlier conversation.
- **New alert triggers:** "Pool member taken out of rotation", "Personal subscription needs reconnecting" (sent to the subscription's owner), "License expiring or expired" and "Chat replies failing" (at most once per model every 15 minutes).
- **Rate limits for one service token.** A rate limit can apply to a single service token, so a looping integration can be throttled without affecting people.
- **Deleted people are held for 30 days.** Deleting a person (in People or from your directory) keeps their chats, files, memories, saved prompts, chat settings, share links and personal subscriptions for 30 days, then deletes them. People shows "Deleted — held until <date>", with **Restore** and **Delete content now**. Restoring the person, or your directory provisioning them again with the same user name, email or external ID within the 30 days, brings everything back.

### Changed

- Helm chart 0.2.2: the image pin moves to 2026.10.5; values and rendered names are unchanged.
- **Upgrading from 2026.10.4 runs migrations 0054–0070.** Migrations 0065 and 0069 add indexes to the usage log; on PostgreSQL each takes a few seconds per million rows, and request-log writes wait while it builds.
- **Administration menu regrouped:** Overview and Chat at the top; Gateway (Upstreams, Models, Managed models, Auto-routing, Grants, Service tokens); People & Usage (People, Quotas, Usage insights); Safety & Security (Security, Child Safety, Policy rules, Alerts); Activity (Requests, Audit log); Settings. The brand and live readout stay in place while the menu scrolls; sections fold and remember your choice.
- **Dashboards load progressively:** the page shows at once and each chart fills in as its data arrives.
- **Removing an administrator group takes access away at once.** Removing an administrator group, or taking someone out of one, removes group-granted administrator access immediately instead of at the person's next sign-in.
- **Alert severity works:** a rule's Severity is the lowest severity it sends by email or webhook ("Critical" sends only critical alerts, "Info" sends everything; recovery notices always go). The in-app inbox still receives every alert.
- **Child Safety blocks models by model, not by name**, so renaming an alias or a model's display name keeps it blocked. Blocks saved by name are converted automatically.
- **A Chat reply counts once** toward per-minute rate limits and request-count quotas, however many calls it makes (tool rounds, deep research, images, video, music, titles). Tokens and cost still count for every call. Each Compare model's answer counts once.
- **Chat requests carry the browser's address.** Chat's requests carry the browser's address, forwarded address (through trusted proxies only), user agent and headers, so policy rules on IP, forwarded address or headers apply to Chat and the request log shows where Chat traffic came from.
- **The request log names Chat's extra calls:** `janus-chat-tool` (tool rounds), `janus-chat-research` (deep-research rounds), `janus-chat-media` (images, video, music, transcripts, descriptions) and `janus-chat-title`. Usage insights classifies only the message you sent, not these extra calls.
- **The docs say plainly what troubleshooting captures keep:** requests exactly as sent, before Security Gateway redaction and including messages from people covered by Child Safety, and that body capture can be left off.

### Removed

- **Parental controls policy template and Family router template:** Child Safety replaces them. Existing `auto/family` routers keep working as ordinary routers, and Security → Categories no longer lists the Family categories (a policy that already selects one keeps it).

### Fixed

- **Recent requests load fast.** The personal dashboard, a person's page, a service token's page and the token request lists no longer count the whole request history to show a few recent requests. The first load on a cold database took 5–9 s and now takes about 0.1 s.
- **Policy rules read X-Forwarded-For only from trusted proxies.** The X-Forwarded-For condition used to match the header as any client sent it, so a caller could forge an address to dodge or trigger a rule. It now reads the chain only when the connection comes from `JANUS_TRUSTED_PROXIES`, as the request log already did; without trusted proxies it never matches.
- **License expiry no longer locks people out.** Past the grace period, sign-in through additional identity providers and LDAP kept by the key stopped. It now keeps working, as the "expiry never disrupts work" rule says; adding a provider waits for a renewed key.
- **Deleting is never paused by the license.** An LDAP directory or an extra sign-in provider can be removed after a license downgrade. Rotating a SCIM token now needs a valid Business license, like creating one.
- **A saved Chat access restriction keeps applying** after the license expires or is downgraded to Community, instead of opening Chat to everyone; administrators can still lift it.
- **Seats are counted at first sign-in.** People added by SCIM, the directory or an administrator now need a free licensed seat at their first sign-in, and turning a disabled account back on (on its profile or through SCIM) needs a free seat too. Administrators are exempt; SCIM provisioning itself never fails on seats.
- **Security policies created through the API keep no matched text unless asked**, as in the editor (the API used to default capture on). Existing policies are unchanged.
- **Content-safety violations are pruned** after 90 days or 100,000 rows. They were never pruned.
- **Upgrading a database first migrated by 1.2.0 works.** 1.2.0 recorded the money widening as migration 0011, which was later renumbered 0013; a 1.2.0 database failed on upgrade with "duplicate column". The old name is now recognised.
- **Load-balanced pools: a server added under Failover is a standby.** It gets the next priority number instead of 0, so it no longer shares traffic with the server already there.
- **Pool retries keep the same checks.** When a pool member fails and Janus retries on the next member, it now only moves to a member the same Security Gateway checks, Child Safety model blocks and quotas allow; a member that would need different checks, is blocked for the caller, or is over the caller's quota is skipped.
- **Classifiers can't answer requests.** A Security Gateway classifier can no longer be an alias's target, fallback or pool member, a model a managed model serves through can't be given a classifier role, and a request that still resolves to one is refused.
- **Quotas for service tokens in the admin UI** (the API already took them), and **team leads can edit a quota's limit and breach behaviour** in place.
- **Team lead quotas follow the license and the team.** Team leads can no longer create quotas while license creation is paused, and can no longer create or change quotas for a deleted team.
- **Deleting a team keeps its keys working.** API keys, Chat conversations and "Working under" selections that used the team switch to personal context, and their owners get a notification.
- **Deleting a team removes its Child Safety assignment.**
- **Revoking a service token** (or moving its expiry into the past) takes effect on every replica immediately.
- **Model discovery:**
  - When an upstream briefly lists no models, for example while a vLLM or llama.cpp server restarts, Janus counts that as a failed check and keeps its enabled models as they are, instead of marking every one of them stale.
  - A model that stops being listed and comes back returns to the state it had (enabled, disabled or awaiting approval), so aliases, pools, routers, grants and Chat defaults that use it keep working without re-approval.
  - Administrators get a notification when enabled models stop being listed by their upstream and are marked stale.
- **Auto-routing:**
  - A router whose default is an alias or a pool follows that alias as it is today: its fallback, a repoint and the pool's other members work exactly as when the alias is called directly. The router stays in the model list and the Chat model picker while the alias can serve.
  - The routing decision model no longer sees a message a Security Gateway policy blocks. With a blocking or redacting policy, or for a child covered by Child Safety, the router decides only after those checks pass.
  - A shadow router decides from the redacted text, the same text the model receives.
  - Routing no longer picks, or stays on, a route whose model (or alias) a child's age range blocks; it uses the next route the child may use or the default. A router whose default is blocked for a child no longer refuses the child before routing.
  - Routing evaluation never replays a conversation of someone covered by Child Safety to the default or the judge model.
  - Chat sends the conversation as the routing session, so turning a tool on or off or saving a memory no longer moves a conversation to another model mid-reply.
- **Classifiers:**
  - Rotating the API key or base URL of an upstream that hosts a Security Gateway, Auto-routing or Child Safety classifier takes effect on every replica within a few seconds. Before, it took effect only after a restart.
  - A classifier whose model is disabled or marked stale in the model catalog is no longer called. Each check's fail setting (open or closed) decides what happens instead.
  - Generative guard classifiers (for example Llama Guard on a paid API) are priced from the model's rate card in usage and cost reports, like decision classifiers.
  - Auto-routing's own decision calls are recorded as Auto-routing cost (client `janus-auto-routing`), not Security Gateway cost.
  - Shadow routing decisions and evaluation guard checks are tied to the request that caused them, so their cost shows against that request.
  - A request's classifier call count and wait include the checks added by the model a router chose.
  - Usage insights classifies what the caller wrote, not the instructions Janus adds for routers or Child Safety.
- **Alerts:**
  - "Security classifier unavailable" is sent once per outage for the whole cluster, instead of every 30 seconds from every replica. A "Security classifier recovered" notice follows when the classifier answers again.
  - "Upstream unreachable" and Child Safety "blocked" notices are sent once by the whole cluster, not once per replica.
  - The `janus_license_expiry_timestamp_seconds` metric updates as soon as a license is installed or removed.
- **Security Gateway:**
  - A Security Gateway block on the decisions API (`/v1/decisions`, `/v1/systemone`) always answers with the 403 error object, even when the policy uses a synthetic refusal; decisions clients could not read a chat-style refusal.
  - Non-streaming replies larger than the response size limit are no longer passed through unchecked when a reply check applies: a child covered by Child Safety gets the stop notice instead, and a reply policy in block or redact mode checks the first part of the reply and withholds it on a match.
  - Security → Violations says "in a page Chat read" for a violation found in a page, search result or file text rather than in what the person wrote.
- **Child Safety:**
  - Pages and search results Chat reads for a child are judged as content shown to the child, not as the child's own message. A flagged page is withheld from the model, and the child is never shown the support message or reported to a parent because of it.
  - A child's sound and video files are always turned into text and checked before a model reads them. If their age range does not allow it, the files are refused when attached.
  - Chat no longer offers children picture, video or music tools that Child Safety would refuse, and says why under Tools & models.
  - Chat no longer picks an image, video, music, speech-to-text or video-watching model that a child's age range blocks.
  - A child can no longer leave the team that keeps Child Safety on for them.
  - Administrators are notified when someone stops being covered by Child Safety because a team was deleted, they were removed from it, or a directory sync changed their teams.
- **Chat:**
  - Chat no longer treats a web page, search results or the text made of an attached sound or video file as something the person wrote. When one contains instructions aimed at the assistant (prompt injection), Chat withholds it from the model, shows "Withheld something it read" in the reply and still answers, instead of blocking the whole reply.
  - The tokens and cost under a reply cover the whole reply: every tool and research round, retries, images, video, music, transcripts and descriptions, and the chat's automatic title. The "this chat" total includes every branch and every Compare answer.
  - A reply's notes and footer no longer show an auto-routing evaluation (judge) request in place of the reply's own.
  - People outside the Chat access restriction can still open a chat someone shared with them, and someone who loses Chat access can still see and revoke the share links they made.
  - In local-only mode, Chat exports no longer show cost.
- **People and retention:**
  - A deleted or disabled person's chat share links stop opening at once.
  - Personal subscription tokens of a deleted or disabled person are no longer renewed in the background.
  - Chats a person deletes are removed for good, with their messages and files, 30 days after the delete.
  - In-app notifications are deleted after 90 days.
  - Security Gateway and Child Safety violations, and Usage insights results, are never kept longer than the request log (`JANUS_USAGE_RETENTION_DAYS`).
- **Proxy and streaming:**
  - Streamed `/v1/responses` calls no longer fail with 400 (`stream_options` is sent to Chat Completions and Completions only).
  - Streamed Claude 4+ replies without an output limit are no longer cut off at 4,096 tokens.
  - Long streamed replies are no longer cut off by the total timeout while they keep streaming; they time out on silence.
  - When Janus fits a request to a model's limits and sends it again, the resend gets the same timeouts as the first try, so long streams and requests with provider-run tools are no longer cut off early.
  - Image, video and speech requests are no longer cut off by the time-to-first-byte limit.
  - Downloading a finished Veo video no longer fails with 401.
  - Video status checks and other requests without a body are routed when they name a personal-plan or renamed model, instead of being refused.
- **Metering:**
  - Speech-to-text is metered by seconds of audio, not upload size.
  - Embeddings and moderation responses without usage no longer count output tokens.
  - Transcription and realtime models are no longer listed as chat models.

### Security

- **Built with Go 1.26.10**, which fixes crypto/x509 trusting the compromised eMudhra intermediate certificates (CVE-2026-111943, GO-2026-6809).
- **golang.org/x/image 0.47.0** fixes three WebP decoder issues reachable from Chat image uploads (GO-2026-6222, GO-2026-5061, GO-2026-4961).
- **KaTeX 0.19.1** for math in Chat replies fixes a prototype-pollution bypass of KaTeX's trust restrictions (GHSA-238p-pmpm-9mq7).

## [2026.10.4] — 2026-10-18

### Added

- **Security categories for content safety** (Security → Categories). Content safety checks now default to a new **Categories** method: a decision model checks the categories a policy selects, and each one counts when the classifier is at least as sure as that category's minimum confidence.
  - Built in: 14 **Safety** categories (the standard hazard categories under plain names) and 5 **Family** categories for parental controls: Homework done for them, Not suitable for children, Getting around parental controls, Sharing personal information, Feeling low or unsafe. Add your own, edit or reset the built-in wording, or follow a Usage insights category instead of writing one.
  - Each category has what it includes and does not include, a minimum confidence and test examples. **Try a request** shows the probability for every category that is on; **Test all examples** runs every example at once.
  - A check can ask about all its categories in one question, or one yes/no question per category; the policy editor shows the resulting classifier calls.
  - The fixed hazard list stays available as **Llama Guard compatible**; existing checks keep it unchanged. A category an enabled policy uses can't be deleted or turned off.
  - Blocking stays a Business feature; observing works on every edition.
- **Parental controls policy template** (Security → Policies). Opens a new policy with content safety on the Family and critical Safety categories (requests and replies), plus secrets and personal data checks, ready to bind to a group or a family router.
- **Violations by category.** Content safety violations show category names, the Violations tab filters by category, and the Security overview lists the top categories of the last seven days.
- **Router instructions** (Auto-routing). A router can add text to the system prompt of the requests it serves: router-wide instructions for every request, plus instructions per route and for the default, up to 8,000 characters each.
  - Added before the caller's own system prompt on chat completions, Anthropic messages and Responses requests, on every turn including sticky ones, after the Security Gateway and the classifier have read the caller's text (instructions are not scanned). They count as input tokens and callers can't see or remove them.
  - Shadow routers apply the router and default instructions; evaluations replay the default with its instructions.
  - The request log shows which instructions were added (router, route, default), and routers that disclose routing send `X-Janus-Route-Instructions`.
- **Family router template** (Auto-routing → New router). Six routes for children: a supportive listener for feeling low or unsafe (with a pointer to a trusted adult and the 988 Lifeline), kind refusals for content not suitable for children and for getting around parental controls, a privacy coach for personal information, a tutor that gives hints instead of answers for homework shortcuts, and normal help for homework learning. Router-wide instructions keep every answer age-appropriate, teach rather than hand over homework, and never repeat personal details a child shares. Sticky is off, so every message is judged; missing Family categories are created with the router.

### Fixed

- **Classifier benchmark:** starting a benchmark could race with the run that had just begun and return a half-written result.

## [2026.10.3] — 2026-10-16

### Added

- **Auto-routing** (Administration → Auto-routing). Let Janus pick the model for each request. A router is a model name callers use (`auto/legal`, `auto/smart`): a decision model classifies each request by kind of work and difficulty, and the router sends it to the first route that fits, or to its default. Routing never fails a request; an unsure, slow or failed classifier, or no fitting route, serves the default.
  - Routes choose a model or managed model by routing category, difficulty band, context fit, grant and quota, and set thinking (as requested, fixed, or scaled with difficulty). Templates: Cost saver, Keep specialist work on-prem, Effort scaler, Coding by difficulty.
  - 15 built-in routing categories plus a Legal example, each with includes/does-not-include descriptions, a minimum confidence and test examples; or follow a Usage insights category.
  - Draft, Shadow (served by the default while Janus records what it would have done) and Live states. Conversations stick to their route (30 minutes by default) without another classifier call, and later turns are held to the same rules as the first.
  - Routers are managed models: grant them, bind security policies and scope quotas to them.
  - Try routing, and a batch test of each category's examples with any classifier; a warning when a hosted classifier would read prompts that are routed to self-hosted models.
  - **Evaluation**: a sample of a route's requests is also answered by the default, out of band, and a judge model compares the two blind, twice with the order swapped. Verdicts, a 95% range and cost against the default per route; prompts and answers are never stored. A daily count, budget, end date and a busy-gateway pause bound it, and it never counts against the caller's quota.
  - The request log shows the router asked for, the model that served and why; optional `X-Janus-Route`, `X-Janus-Route-Reason` and `X-Janus-Route-Model` response headers.
- **Usage insights** (Administration → Usage insights). See what kinds of work people use AI for. A classifier labels sampled chat requests from a built-in taxonomy of 22 categories (Coding, Technical help, Legal, Medical and health, Writing and editing, and so on), plus your own categories and subcategories. Reports show categories over time, share, confidence, subcategories, models and people, the mix per team, and the classifier's own latency, failures, tokens and cost. Only labels are stored, never prompts.
  - Any decision model can classify (System One models such as TypeSafe Jev, Bespoke Nimble or Laya, served by TypeSafe, OpenRouter, Ollama or llama.cpp; or OpenAI Decisions models), or any chat model Janus can reach, prompted to answer in JSON.
  - Runs in the background (no added latency) or inline, where the label is returned in `X-Janus-Category` within a time budget; a classifier failure never fails the request.
- **Decision models.** A new model role for classifiers that answer typed questions (yes/no, choice, score) in the System One format (`/v1/systemone`) or the OpenAI Decisions format (`/v1/decisions`). Janus picks the format from the provider and translates between them.
  - **As security guards**: a decision model can back prompt-injection checks, content-safety checks (requests and, observed, responses), or both, as an alternative to Llama Prompt Guard 2 and Llama Guard 4. Model-backed checks run at the same time, so a request waits for its slowest guard, not all of them in turn. Checks bound to the same decision model are asked together when the model can, or in parallel when it cannot. Two options per check: **What to judge** (the new turn, or the whole conversation) and **Long text** (the last 12,000 characters, or the whole text in windows).
  - **For people and apps**: call any granted decision model at `/v1/decisions` (OpenAI SDKs), `/v1/systemone` (TypeSafe SDK) or `/api/alpha/decisions` (OpenRouter SDK). Calls are granted, checked by security policies (questions and evidence both), counted against quotas, priced and logged like any other request. A decision model that blocks requests in a security policy can't be granted unless an administrator allows it. `/v1/models` lists decision models with `janus.kind: "decision"`.
  - **Pricing per provider**: each decision model has a cost source (automatic, the provider's reported charge, the rate card, or free) and an optional fee per call; the same figure prices Janus's own classifier calls.
- **Classifier benchmark** (Usage insights → Performance). Measure what your classifier choices cost each request: the wait with nothing else running, typical and 95th-percentile wait at rising concurrency, classifier calls per request, and the most simultaneous requests that stay within a latency budget you set. Sample prompts run through the real classifiers; nothing is forwarded or logged.
- The request log records how long each request waited on classifiers and how many calls that took, shows how Usage insights labelled it (Classified as, with a filter), and troubleshooting captures include every classifier call a request caused.
- **Models page**: filter a large catalog by kind, by what a model is used as, by price and by access, with counts on every choice; every column sorts, and unpriced models sort last, never as free. **Requests page**: filter and sort by kind, model, status, time to first byte and classifier wait.

### Changed

- **Thinking settings work on every provider.** Chat clients such as Open WebUI send one reasoning effort, temperature and set of tools to every model, and providers each refused part of that. When a provider refuses a setting and names it, Janus now fits the request and sends it again before anything reaches the caller, and remembers the fix per model for a day: a refused reasoning level becomes the nearest one the model takes, a refused temperature or top_p is left out, and GPT-5/GPT-6 tool calls go through OpenAI's Responses API and come back as a normal chat completion. Changes are shown in `X-Janus-Reasoning-Adjusted` and the request log.
- The navigation item **Tokens** is now **API Tokens**.
- **Helm chart renamed to `janus-helm-chart`** (chart 0.2.0, file `janus-helm-chart-0.2.0.tgz`, source in `charts/janus-helm-chart`). Rendered resource names and labels are unchanged, so `helm upgrade` of an existing release works as before; point it at the new file.
- **Offline container images** are attached to the release as `janus-container-image-2026.10.3-linux-amd64.tar` and `janus-container-image-2026.10.3-linux-arm64.tar`, for installing without registry access (`docker load -i …` or `podman load -i …`).
- Built with Go 1.26.9.

### Fixed

- **Claude now thinks when asked.** `reasoning_effort` was dropped on the way to Anthropic. Claude 4.6 and later (all of Claude 5) get adaptive thinking with that effort and return a summary of the thinking as `reasoning_content`; earlier models get a thinking budget kept within the model's output limit. Auto-routing's forced levels use the same translation.
- Gemini tool calls no longer fail on the second turn when the client drops the thought signature: Janus restores it.
- An observe-mode security check no longer blocks requests when its classifier is down; fail closed applies to blocking checks only.
- A failed decision-model catalog read no longer switches decision models off.
- Classifier benchmarks interrupted by a restart no longer show as running forever, and only one runs at a time across replicas.

## [2026.10.2] — 2026-10-04

### Added

- **Google Gemini** as a provider: choose Google Gemini (AI Studio key) and paste an API key from Google AI Studio. Janus uses Google's OpenAI-compatible endpoint, lists the models the key can call with their context windows (Live-API models, which that endpoint cannot call, are left out), meters thinking tokens as output as Google bills them, and records implicit prompt-cache hits as cached input. The bundled reference prices Gemini 2.5 Flash and Flash-Lite, 3 Flash Preview, 3.1 Flash-Lite, 3.5 Flash and Flash-Lite, and Embedding 2.
- Adding an upstream starts from a provider list: OpenAI, Anthropic, Google Gemini, Google Vertex AI, AWS Bedrock, xAI, Mistral, OpenRouter, Groq, DeepSeek, Together AI, Fireworks AI, Cerebras, Moonshot AI, the self-hosted engines, and any other OpenAI-compatible endpoint. Choosing one fills in its documented base URL and a suggested name. Both stay editable, and a value you typed is kept when you switch provider.

### Fixed

- A base URL that ends in an API version (`…/v1`, `…/openai/v1`, Google's `…/v1beta/openai`) is used as the API root, the way providers document it for OpenAI clients. Before, only a trailing `/v1` was recognised, so `…/v1beta/openai` was called as `…/v1beta/openai/v1/…` and failed.
- Streamed output speed for providers that generate reasoning without streaming it (Gemini) is measured over the whole call; it was divided by the few milliseconds after the first byte and reported thousands of tokens per second.
- On a phone, the Upstreams page no longer scrolls sideways when an upstream has a discovery error.

## [2026.10.1] — 2026-10-04

### Added

- **Load-balanced model pools** (Business). A managed model can put several servers running the same model behind one name. Balancing can be failover, round robin by weight, least loaded (queue length read from each vLLM or llama.cpp server's `/metrics` every 2 seconds) or context aware (share of each server's context capacity in use, from vLLM KV-cache figures or llama.cpp `/slots`). Every turn of a conversation goes to the server that already holds its prompt cache, recognised from a session header, `prompt_cache_key`, or the conversation's opening when the client sends neither. A server that refuses a request before answering is skipped within the same call; repeated failures take it out on every replica together. Responses carry `X-Janus-Pool-Member` and `X-Janus-Pool-Reason`, and the editor shows each server's queue, context use and prompt-cache hit rate live.
- **Personal subscriptions.** People can connect a provider plan they already pay for (OpenAI ChatGPT Plus or Pro, GitHub Copilot, xAI SuperGrok or X Premium+, Mistral) on the new Subscriptions page and call its models as `my/<provider>/<model>` with their own Janus token. Calls are metered, logged and covered by security policies like any other. Administrators turn the feature on organization-wide; it is off in offline mode. Reasoning effort is fitted to what each plan's model accepts, and a sign-in that stops working returns `policy.subscription_reauth_required`.
- **Model speed and activity.** Model cards show tokens per second and jobs per day with a trend; the admin Overview has a Model performance card charting each model's concurrency, speed and input size over time; the live readout adds a per-second line. Engine-measured speeds (including vLLM per-request metrics) are preferred over Janus's own estimates.
- A **Self-hosted engines** guide covering the vLLM, llama.cpp, Ollama and TEI flags that give Janus exact token counts, speeds, context windows and live pool load.
- **Performance mode** (Business, `JANUS_PERFORMANCE_MODE=true`) writes usage in 100 ms batches and commits without waiting for the database's disk flush, for the busiest gateways. A database crash can lose about the last 0.5 s of usage records and a replica killed without a graceful shutdown up to 100 ms of them; graceful shutdowns lose nothing. The state is shown on Admin → System.
- `JANUS_DB_MAX_CONNS` (default 25) caps each replica's PostgreSQL connection pool. The previous fixed ceiling was 50.

### Changed

- **Built for large networks.** Usage is recorded without locking shared rows, which roughly tripled per-replica throughput in testing and lets throughput grow with replicas. Console pages that show 30-day usage per row (API tokens, Service tokens, Managed models, the admin Overview) read hourly totals that a background job keeps current, so they load in well under a second after millions of requests instead of tens of seconds. The figures still match the request log exactly. The first start after upgrading fills the totals from existing history in the background (about 4 seconds per million requests); pages work normally meanwhile.
- **Usage keeps the team it was admitted with.** Joining, leaving or switching teams no longer moves recorded usage, including a person's first team. To move a token's history, use **Change team** on the token and choose to move its recorded usage.
- The Subscriptions page has a tab per provider, lists the models you use first, and adds models from a side panel; removing a model can be undone.
- Managed models open in a side panel, and Model grants is one dense table like the other admin pages.
- The Troubleshooting page covers every error code Janus can return.
- Quota rules are cached for the configuration cache interval and refreshed on every quota change, and `last_used_at` on tokens is written at most every 30 seconds per token.

### Fixed

- A stream the provider ends early is recorded as a failure, and streams without a usage block are estimated from the text they carried instead of being metered as zero.
- Writing a usage record no longer clears the model and rate-card caches.

## [2026.9.5] — 2026-09-26

### Added

- One-paste activation: install the activation code from the license portal and tick **Automatically renew and sync** in the same step. The gateway installs the key, stores the sync token, and reports any sync problem immediately.
- License & updates opens with a single verdict: a green check when the gateway is licensed and healthy, a yellow **Attention needed** listing each reason (expiring without confirmed renewal, grace period, over seats or nodes, renewal sync stale or failing, cancellation scheduled, clock skew, unsupported version), and a red **Not properly licensed** when the key is expired or invalid. The page fits on one screen at common desktop sizes.

### Changed

- Reports: a redesigned result view, library actions, and cleaner PDF, CSV and spreadsheet exports.
- My teams is one compact workspace per team: header, a summary strip (members, requests, tokens, spend, error rate), an activity chart, usage by model and the member roster side by side.
- Admin tables act from one line per row: row actions are icon buttons whose label appears on hover and keyboard focus, and the actions column stays pinned on narrow windows. People uses tighter rows and a Rows per page choice (25–200).
- License sync token and key fields use the standard themed inputs.

### Fixed

- The same model name served by more than one upstream no longer breaks grants: a request goes to the most recently discovered copy the caller is granted and is billed at that copy's rate card. The Models and Grants pages label copies by upstream.
- Input tokens-per-second no longer counts prompt-cache hits as processed input.
- An upstream can be created with the name of a deleted one (deleted upstreams keep usage history but no longer reserve their name).
- The container image no longer declares `VOLUME /data`, so PostgreSQL deployments do not get an anonymous volume. SQLite users still mount `/data` as documented.
- "Rows per page" no longer wraps on narrow tables; Help's topic list and snippet tabs render correctly.

## [2026.9.4] — 2026-09-25

### Fixed

- Local-only mode (`JANUS_LOCAL_ONLY=true`) can enable newly discovered models again. The mode hides all pricing, but enabling a model that had never been priced was still refused with "Save a rate card first", leaving no way to enable it. A rate card is now required only when cost tracking is on, and the Models, Upstreams and Administration pages no longer ask local-only administrators to price models.

## [2026.9.3] — 2026-09-24

### Added

- Linux server installation: `sudo python3 install.py` installs Janus Edge as a systemd service running as an unprivileged `janus` user, with HTTPS on port 443 and port 80 redirecting to HTTPS. The first administrator is created while the gateway still listens only on loopback, so nobody else can claim a new server.
- Certificates: a self-signed certificate is created on first install; `janus-ctl cert install` installs your own certificate after checking that the key matches and it has not expired, and `janus-ctl cert acme` obtains a Let's Encrypt certificate over HTTP-01 and renews it automatically with a systemd timer.
- `janus-ctl status`, `upgrade` and `uninstall` manage the service. Upgrades back up a SQLite database first and never rotate the encryption key; uninstall keeps configuration and data unless `--purge` is given.

### Changed

- The previous no-sudo installation into `~/.local` with the foreground loopback launcher remains available as `python3 install.py --user`.
## [2026.9.2] — 2026-09-21

### Added

- Opt-in automatic license renewal sync for subscription licenses, off by default, under Admin → Settings → License & updates. Enable it explicitly in the UI or with `JANUS_LICENSE_SYNC=true`; storing a sync token or installing an online key never enables it on its own, and offline mode always prevents remote sync.
- Sync token onboarding: paste the raw sync token from the license portal, save, and use Sync now. Blank saves preserve the stored credential, and a clear action removes it. `JANUS_LICENSE_SYNC_TOKEN` pins the credential from the environment.
- Renewal attention notices in the shell banner and the license card that are independent of signed license validity: cancellation, payment failure, stale or failed sync, and exhausted grace stay visible while routine expiry warnings are suppressed only during a fresh, confirmed automatic renewal.

### Changed

- Revocation recovery is replay-safe: after an advisory revocation the gateway keeps syncing but clears the warning only for a newer same-identity signed license accompanied by fresh subscription evidence, so an old revoked response cannot restore a stale state.

## [2026.9.1] — 2026-09-20

### Added

- Initial public release of Janus Edge, a self-hosted AI gateway with provider routing, model discovery, and OpenAI-compatible APIs.
- User and service tokens, identity-provider integration, local authentication, team membership, quotas, and usage reporting.
- Security policies, guard classifiers, audit logging, and administrative troubleshooting.
- Separate personal Team workspaces and organization-wide team management under Administration → People → Teams.
- Offline license verification and self-hosted installation support.
