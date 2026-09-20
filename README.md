# Janus Edge

**A self-hosted gateway for organizational AI access, policy, and visibility.**

Janus provides a shared API and web workspace for accessing AI providers and
self-hosted models. Manage identities, model access, team usage, quotas,
security policies, and reporting without distributing upstream credentials
to every client.

- **Provider and model management:** discover upstream models, manage access
  grants, and expose managed model names with routing and fallback policies.
- **Personal and team workspaces:** users see their own access and team
  memberships; administrators manage teams and CSV imports under
  **Administration → People → Teams**, alongside Users and Groups.
- **Usage and reporting:** request, token, cost, latency, and quota views, with
  saved reports and exportable results.
- **Security controls:** configurable policies and classifier integrations,
  findings, and explicitly enabled troubleshooting captures.
- **Identity:** local accounts with optional TOTP, OIDC, directory integration,
  and provisioning. Availability of advanced features depends on the edition.
- **Self-hosting:** one executable with an embedded web UI, container images,
  SQLite for evaluation, and PostgreSQL for production deployments.

Product information and licensing: [janusedge.com](https://janusedge.com).

## Release 2026.9.1

Versions use **YEAR.MONTH.RELEASE_NUMBER**. The release tag is `v2026.9.1`.

- [Releases and downloads](https://github.com/torvanis/janus/releases)
- Container: build locally from this release using the Dockerfile (see below).
- [Release notes](CHANGELOG.md)

Use a versioned image or an immutable digest rather than an unpinned tag.

## Quick start with a container

Requires Docker and OpenSSL, and a checkout of this release (see Build from source).
No prebuilt registry image is promised by this source release. Build the image:

```sh
docker build -t janus:2026.9.1 .
```

This starts a **local evaluation** instance;
complete initial administrator setup before exposing it to other machines.

Generate a persistent encryption key once. Keep this file private and back it
up with the database. Do not regenerate the key for an existing installation.

```sh
umask 077
printf 'JANUS_ENCRYPTION_KEY=%s\n' "$(openssl rand -hex 32)" > janus.env

docker run -d --name janus \
  --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  --env-file janus.env \
  -e JANUS_PUBLIC_URL=http://localhost:8080 \
  -v janus-data:/data \
  janus:2026.9.1
```

Open **http://localhost:8080** and create the first administrator. There is no
shared default administrator password. After setup:

1. Add an upstream and its credentials.
2. Discover and enable the models you want to expose.
3. Grant access to users, groups, or teams.
4. Create a client API token and follow the in-product connection instructions.
5. Set quotas and security policies as appropriate for your deployment.

The named volume preserves the evaluation database across container restarts.
Use the same encryption key on every restart and on every replica.

## Configuration

Janus reads configuration from environment variables. Common settings:

| Variable | Purpose |
| --- | --- |
| `JANUS_ENCRYPTION_KEY` | Required 32-byte encryption key, encoded as 64 hexadecimal characters. |
| `JANUS_PUBLIC_URL` | Browser-facing URL, including scheme. Use HTTPS for shared installations. |
| `JANUS_LISTEN_ADDR` | HTTP bind address; the container listens on port 8080. |
| `JANUS_DATABASE_URL` | Database connection string; defaults to `sqlite:///data/janus.db`. |
| `JANUS_OIDC_PROVIDER_URL` | Optional OIDC issuer URL. |
| `JANUS_OIDC_CLIENT_ID` | OIDC client identifier; required when configuring the environment-based provider. |
| `JANUS_OIDC_CLIENT_SECRET` | OIDC client secret; required with that provider. |
| `JANUS_BOOTSTRAP_ADMIN_EMAILS` | Optional comma-separated administrator emails for external identity sign-in. |
| `JANUS_LICENSE_FILE` | Optional path to a signed license file. |
| `JANUS_OFFLINE` | Disables update checks when set to `true`. |
| `JANUS_UPDATE_CHECK` | Opt-in version checking; disabled by default. |

See the built-in documentation for additional identity, policy, retention,
notification, and operational settings. Never place credentials in source
control or distribute them inside an image.

## Production deployment

- Use PostgreSQL and a tested backup/restore procedure. The default SQLite
  configuration is intended for evaluation and low-scale single-node use.
- Set the correct HTTPS public URL and terminate TLS at a trusted reverse
  proxy, or configure the gateway's TLS certificate and key.
- Restrict first-run setup to an administrator-controlled network until an
  administrator account exists. Configure OIDC completely if using it.
- Persist and protect the encryption key, database, and any configured capture
  storage. Database backups without the corresponding key cannot restore
  encrypted upstream credentials.
- Keep development authentication disabled. Restrict monitoring endpoints and
  configure trusted proxies only for proxies you actually operate.
- Apply the edition's node allowance and use PostgreSQL for multiple replicas.
- Back up before upgrading; retain the prior image digest and configuration.
  Database schema changes can require a restore rather than simply running an
  older executable.

`/healthz` is the liveness endpoint. `/readyz` checks readiness. A successful
health check does not replace testing administrator sign-in and a real request
through a configured upstream.

## Build from source

Requires Git, Make, Bash, Python **3**, Go **1.26.7**, and Node.js **22**
with npm. The verification suite also requires a C compiler for Go race tests.
These commands target a Unix-like build environment. Dependency versions are
recorded in the Go module files and `web/package-lock.json`.

```sh
git clone https://github.com/torvanis/janus.git
cd janus
git checkout v2026.9.1
make web-deps
make verify
make build
```

Build the web UI before compiling the gateway: the executable embeds the
resulting bundle. Release builds also record their version and release date.
A rebuild must retain its original release date because perpetual licenses
are checked against that date.

## License

Janus Edge is **source-available**, not OSI-approved open-source software. It is
licensed under the [Elastic License 2.0](LICENSE), with the accompanying
[Commercial Terms](COMMERCIAL-TERMS.md).

ELv2 restricts offering the software as a hosted or managed service, removing
or bypassing license-key functionality, and removing licensing notices.
Consult the full terms for the controlling language, edition eligibility,
and paid licensing requirements.

The gateway verifies signed licenses using a public verification key. License
issuance and private signing credentials are not part of this distribution.
Third-party components retain their own licenses; see
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) and [NOTICE](NOTICE).
