# Changelog

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
