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

## Release 2026.11.1

Versions use **YEAR.MONTH.RELEASE_NUMBER**. The release tag is `v2026.11.1`.

- [Releases and downloads](https://github.com/torvanis/janus/releases)
- Container: `ghcr.io/torvanis/janus:2026.11.1` (public, Linux amd64/arm64); alternatively build locally using the Dockerfile below.
- [Linux server installer and architecture archives](https://github.com/Torvanis/janus/releases/tag/v2026.11.1): see [Install on a Linux server](#install-on-a-linux-server)
- [Helm chart installation guide](charts/janus-helm-chart/README.md): bundled single-instance PostgreSQL by default, existing PostgreSQL, or SQLite **for evaluation only**.
- [Download Helm chart 2026.11.1](https://github.com/Torvanis/janus/releases/download/v2026.11.1/janus-helm-chart-2026.11.1.tgz)
- Offline container images for air-gapped installs: `janus-container-image-2026.11.1-linux-amd64.tar` and `janus-container-image-2026.11.1-linux-arm64.tar` on the [release page](https://github.com/Torvanis/janus/releases/tag/v2026.11.1) (`docker load -i` or `podman load -i`).
- **Janus FIPS edition** (Business): `ghcr.io/torvanis/janus:2026.11.1-fips`, plus `janus-fips_2026.11.1_linux_<arch>.tar.gz` and `janus-fips-container-image-2026.11.1-linux-<arch>.tar` on the release page. See [FIPS edition and STIG profile](#fips-edition-and-stig-profile).
- [Release notes](CHANGELOG.md)

Use a versioned image or an immutable digest rather than an unpinned tag.

## Install on a Linux server

On any systemd distribution (tested on Ubuntu 24.04 and Rocky Linux 9) with
Python 3.9+ and openssl:

```sh
curl -fLO https://github.com/Torvanis/janus/releases/download/v2026.11.1/install.py
sudo python3 install.py --admin-email you@example.com --hostname ai.example.com
```

Janus runs as a `janus` system service with HTTPS on port 443; port 80
redirects to HTTPS. A self-signed certificate is created on first install.
Replace it with your own, or with Let's Encrypt:

```sh
sudo janus-ctl cert install --cert fullchain.pem --key privkey.pem
sudo janus-ctl cert acme --domain ai.example.com --email you@example.com
```

`sudo janus-ctl status | upgrade | uninstall` manage it afterwards. Details,
PostgreSQL and offline installation: [deploy/README.md](deploy/README.md#linux-server-systemd).

## Quick start with a container

Requires Docker and OpenSSL. Pull `ghcr.io/torvanis/janus:2026.11.1` or build
the image from a checkout of this release (see Build from source):

```sh
docker build -t janus:2026.11.1 .
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
  janus:2026.11.1
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

## Docker Compose with PostgreSQL

For one server and a small team, the release attaches
`janus-compose-bundle-<version>.tar.gz` (listed in `SHA256SUMS`): Janus and
PostgreSQL 16, both pinned by digest, HTTPS with a self-signed certificate
(or your own, or Let's Encrypt through an optional Caddy container), and a
`setup.py` that creates the first administrator before the port is published.

```sh
sha256sum -c --ignore-missing SHA256SUMS
tar xzf janus-compose-bundle-<version>.tar.gz && cd janus-compose
python3 setup.py --admin-email you@example.com --hostname ai.example.com
```

Backup, restore, certificates and upgrades: [deploy/compose/README.md](deploy/compose/README.md).

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
| `JANUS_PERFORMANCE_MODE` | Business: batch usage writes for the busiest gateways (see below). Defaults to `false`. |
| `JANUS_DB_MAX_CONNS` | PostgreSQL connections per replica; defaults to 25. Keep replicas × this well under the server's `max_connections`. |
| `JANUS_CHAT_PREVIEW` | Turns on Janus Chat (preview). Defaults to `false`. |
| `JANUS_CHAT_AUTO_TITLE` | Lets the chat's model name each conversation (a metered `janus-chat-title` request). Defaults to `true`. |
| `JANUS_CHAT_MAX_UPLOAD_BYTES` | Largest chat attachment; defaults to 20 MiB and never exceeds the storage limit. |
| `JANUS_CHAT_SEARCH_SEARXNG_URL` | Address of a SearXNG instance; Chat web search is turned on with it once at startup. |
| `JANUS_STORAGE_MODE` | `database` or `s3` fixes where chat files and troubleshooting captures are kept; unset lets Administration → Settings → General → File storage decide. |
| `JANUS_S3_ENDPOINT`, `JANUS_S3_BUCKET`, `JANUS_S3_ACCESS_KEY_ID`, `JANUS_S3_SECRET_ACCESS_KEY` | Required with `JANUS_STORAGE_MODE=s3`. Optional: `JANUS_S3_REGION` (default `us-east-1`), `JANUS_S3_PATH_STYLE`, `JANUS_S3_PREFIX`, `JANUS_S3_CA_FILE`. Files are encrypted by Janus before upload. |
| `JANUS_STORAGE_MAX_OBJECT_BYTES` | Largest stored file; defaults to 25 MiB. |
| `JANUS_STIG_PROFILE` | Business: `true` puts Janus into its documented configuration for the DISA Application Security and Development STIG and refuses unsafe settings at startup (see below). |
| `JANUS_TLS_CERT_PATH`, `JANUS_TLS_KEY_PATH` | Serve TLS directly with an issued certificate; reloaded when the files change. |
| `JANUS_TLS_SELF_SIGNED`, `JANUS_TLS_REQUIRE` | Generate a self-signed listener certificate shared by every replica; refuse to start unless Janus serves TLS itself. |
| `JANUS_TRUSTED_PROXIES` | Comma-separated addresses or CIDR blocks of proxies whose `X-Forwarded-For` (and smart-card certificate header) Janus trusts. |
| `JANUS_CA_BUNDLE`, `JANUS_CA_DIR`, `JANUS_TRUST_DOD_ROOTS` | Extra certificate authorities for every outbound TLS connection: one PEM file, a directory rescanned every 30 seconds, and the built-in DoD PKI roots (on by default in the FIPS edition). |
| `JANUS_TLS_REVOCATION`, `JANUS_OCSP_RESPONDER` | `soft` or `hard` OCSP/CRL revocation checking of outbound certificates (hard in the STIG profile), and an enclave OCSP responder. |
| `JANUS_WEB_ALLOW_PRIVATE`, `JANUS_WEB_ALLOW_DOMAINS`, `JANUS_WEB_DENY_DOMAINS`, `JANUS_WEB_PROXY`, `JANUS_WEB_TLS_TRUST` | Which web pages chat models may open (all off by default): private networks for offline sites, domain allow/deny lists, a forward proxy for every read, and which certificate authorities those reads trust. |

See the built-in documentation for additional identity, policy, retention,
notification, and operational settings. Never place credentials in source
control or distribute them inside an image.

**Performance mode (Business).** One replica meters several hundred requests
per second in normal mode. `JANUS_PERFORMANCE_MODE=true` raises that by
writing usage in 100 ms batches and committing without waiting for the
database's disk flush. The trade is a bounded loss window: a database crash
can lose about the last 0.5 s of usage records, and a replica killed without a
graceful shutdown up to 100 ms of them. Graceful shutdowns and rollouts lose
nothing. It needs a valid Business or Enterprise license (otherwise the
gateway logs why and runs normally) and is shown on Administration → Settings → System status.

## FIPS edition and STIG profile

The **Janus FIPS edition** is the same source built with the Go Cryptographic
Module v1.0.0 (CMVP certificate #5247) in FIPS 140-3 approved mode. It refuses
to start unless that module is linked with FIPS mode on; `janus --fips-status`
prints the edition, the module and the features it restricts. It needs a
Business, Enterprise or trial license and PostgreSQL (it refuses SQLite).

```bash
docker run --rm ghcr.io/torvanis/janus:2026.11.1-fips --fips-status
make build-fips            # from source: GOFIPS140=v1.0.0, -tags fips
```

With Helm: `--set image.tag=2026.11.1-fips --set image.digest=` (or that
image's digest).

`JANUS_STIG_PROFILE=true` (either edition, Business) applies the documented
configuration for the DISA Application Security and Development STIG: offline,
913-day audit retention, hard revocation checking, the STIG security settings
as a floor, and administrative changes that fail closed when their audit record
cannot be written. Plain HTTP, SQLite, the evaluation sign-in, LDAP
skip-verify, untrusted upstream TLS, phoning home and a shorter audit retention
stop startup with a named error. It is designed to support your ATO; it is a
configuration, not a certification. The in-product documentation
(`/docs`, Administration → FIPS edition and STIG profile) lists every setting.

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

Requires Git, Make, Bash, Python **3**, Go **1.26.10**, and Node.js **22**
with npm. The verification suite also requires a C compiler for Go race tests.
These commands target a Unix-like build environment. Dependency versions are
recorded in the Go module files and `web/package-lock.json`.

```sh
git clone https://github.com/torvanis/janus.git
cd janus
git checkout v2026.11.1
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
