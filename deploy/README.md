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
image (preferably pinned by digest) before applying it. The registry reference
in the template is illustrative; this source release does not publish an image.

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

## Standalone archives

Python 3.9+ is needed for the installer and local launcher. The gateway binary
itself has no Python dependency. For this source release, build the Linux
archives and installer locally with `make release`, then install from `dist/`:

```sh
make release
python3 dist/install.py --archive dist/janus_2026.9.1_linux_amd64.tar.gz --checksums dist/SHA256SUMS
python3 ~/.local/share/janus/run-local.py --binary ~/.local/bin/janus
```

The installer also supports `--version` for releases that provide binary assets;
this initial source-only release does not. Inspect the installer before running it. Checksums detect
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
