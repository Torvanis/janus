# Deployment examples

These examples use one replica and persistent SQLite storage for evaluation or
small single-node installations. Production deployments should use PostgreSQL,
backups, TLS, and an appropriate licensed configuration. Do not scale a SQLite
instance above one replica or mount its database into multiple writers.

## Compose (Docker Compose v2)

From the repository root:

```sh
python3 scripts/init-env.py
docker compose --env-file deploy/.env -f deploy/compose.yaml up --build -d
```

Open <http://127.0.0.1:8080> and create the first administrator. There is no
default password and development authentication is disabled. The published port
is bound only to loopback. Keep it that way until administrator setup is complete.
Back up `deploy/.env` and the `janus-data` volume together; never generate a new
key for an existing database. The initializer refuses to overwrite a key.
`docker compose ... down` retains the data; **do not use `down -v`** unless you
intend to delete it. Compose builds the image locally from this source checkout.

## Kubernetes

Build the Dockerfile, push the resulting image to a registry your cluster can
access, and replace the `image:` value in `deploy/kubernetes.yaml` with that
image (preferably pinned by digest) before applying it. The template references the
public image `ghcr.io/torvanis/janus:2026.9.3`; alternatively build and push
your own image from this checkout.

Requires a default StorageClass supporting filesystem volumes and `fsGroup`,
and permission to create resources in your chosen namespace. This example does
not create an Ingress or LoadBalancer. Anyone with network access to an unclaimed
instance can become its first administrator: isolate the namespace/network until
setup is complete, and do not expose it publicly before setup.

```sh
kubectl create namespace janus
python3 scripts/init-env.py --output ./janus-kubernetes.env
kubectl -n janus create secret generic janus --from-env-file=./janus-kubernetes.env
kubectl -n janus apply -f deploy/kubernetes.yaml
kubectl -n janus rollout status deployment/janus
kubectl -n janus port-forward --address 127.0.0.1 service/janus 8080:8080
```

Keep the generated env file private and backed up; never commit it. Complete the
first-administrator form at <http://127.0.0.1:8080>. `Recreate` prevents overlapping
SQLite writers during upgrades. For remote access, configure your own HTTPS
Ingress/reverse proxy and set `JANUS_PUBLIC_URL` to its HTTPS origin and
`JANUS_COOKIE_SECURE=true`. Set trusted proxy CIDRs deliberately, not globally.
Use a secret-provided `JANUS_DATABASE_URL` for PostgreSQL and remove the SQLite
volume only after a separately planned data migration. Keep the encryption key
stable across upgrades, replicas, restores, and database migrations.

## Linux server (systemd)

Python 3.9+ and openssl are needed for the installer. The gateway binary itself
has no Python dependency. Prebuilt Linux amd64/arm64 archives, `install.py` and
`SHA256SUMS` are attached to each GitHub release.

```sh
curl -fLO https://github.com/Torvanis/janus/releases/download/v2026.9.3/install.py
sudo python3 install.py --admin-email you@example.com --hostname ai.example.com
```

This installs a `janus` system service on any systemd distribution (tested on
Ubuntu 24.04 and Rocky Linux 9 with SELinux enforcing):

- HTTPS on port 443; port 80 answers with a 301/308 redirect to HTTPS.
- A self-signed certificate for the hostname and the machine's addresses is
  created on first install.
- The first administrator is created before Janus listens beyond loopback, so
  nobody else can claim the server. Without `--admin-email` the installer asks;
  `--skip-admin` keeps it loopback-only until you create the administrator in
  the browser and run `sudo janus-ctl open`.
- Runs as the unprivileged `janus` user with `CAP_NET_BIND_SERVICE` only.
- Configuration and the encryption key: `/etc/janus/janus.env` (back it up).
  Data: `/var/lib/janus` (SQLite by default; `--database-url postgres://...`
  selects PostgreSQL). SQLite suits a single server for a small team; use
  PostgreSQL for larger or highly available deployments.
- firewalld or an active ufw gets ports 80 and 443 opened.

Certificates:

```sh
sudo janus-ctl cert install --cert fullchain.pem --key privkey.pem   # your own (add --chain if separate)
sudo janus-ctl cert acme --domain ai.example.com --email you@example.com  # Let's Encrypt, auto-renewed
sudo janus-ctl cert self-signed                                        # back to self-signed
```

`cert install` refuses a key that doesn't match the certificate, an expired
certificate or an encrypted key. `cert acme` needs a public DNS name pointing at
the server and port 80 reachable from the internet (HTTP-01); a systemd timer
renews it. Other commands: `sudo janus-ctl status`, `sudo janus-ctl upgrade
[--version V]` (backs up a SQLite database first and never rotates the key), and
`sudo janus-ctl uninstall [--purge]` (keeps configuration and data unless
`--purge`). Offline: `sudo python3 install.py --archive FILE --checksums SHA256SUMS`.

## No-sudo trial

`python3 install.py --user` installs into `~/.local` with no service, for a quick
look on a laptop. It runs in the foreground on loopback only:

```sh
python3 install.py --user
python3 ~/.local/share/janus/run-local.py --binary ~/.local/bin/janus
```

To build the archives and installer locally instead, run `make release` and
install from `dist/` with `--archive dist/janus_2026.9.3_linux_amd64.tar.gz
--checksums dist/SHA256SUMS`.

Inspect the installer before running it. Checksums detect
corruption, not compromise of the release publisher. Installation defaults to
`~/.local`; `--prefix /chosen/path` changes it. No sudo, service creation, or
automatic startup occurs. Legal notices and examples are installed under
`PREFIX/share/janus`. The launcher uses `~/.local/share/janus` (or
`$XDG_DATA_HOME/janus`) for the database and a private persistent encryption key;
`--data-dir` and `--port` override those local choices. Back up both database and
key while the instance is stopped. The launcher intentionally ignores ambient
`JANUS_*` variables to preserve its loopback-only, local-account configuration.
For custom production configuration run the binary directly with explicit
`JANUS_*` environment variables and a stable secret-managed encryption key.
