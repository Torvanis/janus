# Changelog

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
