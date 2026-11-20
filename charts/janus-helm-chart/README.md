# Janus Edge Helm chart

Chart **2026.11.2**, application **2026.11.2**. From 2026.11.2 the chart is
versioned with Janus (`YEAR.MONTH.RELEASE_NUMBER`; earlier charts were
`0.1.x`/`0.2.x`). The default public gateway
image is pinned to the multiarchitecture digest of `ghcr.io/torvanis/janus:2026.11.2`.
A nonempty `image.digest` takes precedence over `image.tag`; clear the digest
explicitly when selecting a different tag.

The chart is published as `janus-helm-chart-<version>.tgz` on each GitHub
release (it was named `janus` up to chart 0.1.6). The rename changes only the
chart's name: rendered resource names and labels are still `janus`, so
`helm upgrade` of a release installed from the old chart keeps working when you
point it at the new file.

## FIPS edition

The Janus FIPS edition image is `ghcr.io/torvanis/janus:<version>-fips`
(Business; PostgreSQL only). Select it with
`--set image.tag=2026.11.2-fips --set image.digest=`, or set `image.digest` to
that image's digest. Its offline archives are
`janus-fips-container-image-<version>-linux-<arch>.tar`.

## Offline (air-gapped) installs

Each release also attaches the container image as a file per architecture:
`janus-container-image-<version>-linux-amd64.tar` and
`janus-container-image-<version>-linux-arm64.tar`. Load it on a machine that
can reach your private registry, push it there, and set `image.repository`
(and clear `image.digest`, or set it to the digest your registry reports):

```bash
podman load -i janus-container-image-2026.11.2-linux-amd64.tar   # or: docker load -i …
podman tag ghcr.io/torvanis/janus:2026.11.2 registry.example.internal/janus:2026.11.2
podman push registry.example.internal/janus:2026.11.2
helm upgrade --install janus ./janus-helm-chart-2026.11.2.tgz --namespace janus \
  --set image.repository=registry.example.internal/janus --set image.tag=2026.11.2 \
  --set image.digest= …
```

The bundled PostgreSQL uses `postgres:16-alpine`; mirror it the same way and
set `postgresql.image`, or use an existing PostgreSQL.

## kind, k3d or a cluster without registry access (evaluation)

A nonempty `image.digest` (this chart pins one) makes the kubelet resolve the
image by digest, and that needs DNS and HTTPS to the registry (`ghcr.io`) from
the node. It does so **even when an image with the same tag is already on the
node**: `kind load`, `k3d image import` and `ctr images import` keep the tag,
and a tag does not satisfy a digest reference. When the node cannot reach the
registry (for example `lookup ghcr.io on 172.18.0.1:53: server misbehaving`
inside kind), pods stay in `ImagePullBackOff`.

Preload the image and select it by tag instead:

```bash
podman pull ghcr.io/torvanis/janus:2026.11.2       # or: docker pull …
kind load docker-image ghcr.io/torvanis/janus:2026.11.2 --name <cluster>
# podman: podman save ghcr.io/torvanis/janus:2026.11.2 -o janus.tar && kind load image-archive janus.tar --name <cluster>
# or use the release's janus-container-image-2026.11.2-linux-<arch>.tar: kind load image-archive <file> --name <cluster>
```

```yaml
# values-kind.yaml (evaluation only)
image:
  repository: ghcr.io/torvanis/janus
  tag: "2026.11.2"
  digest: ""              # select by tag: no registry lookup needed
  pullPolicy: Never       # or IfNotPresent: use the preloaded image, never pull
```

```bash
helm upgrade --install janus ./janus-helm-chart-2026.11.2.tgz --namespace janus --create-namespace \
  -f values-kind.yaml …   # plus the Secrets from "First installation" below
```

`pullPolicy: IfNotPresent` also works once the image is on every node; `Never`
fails fast with `ErrImageNeverPull` if it is missing, which is easier to read.
Mirror `postgres:16-alpine` the same way (or use SQLite evaluation, below).
For a real air-gapped install, push to your own registry as described above
and keep a digest pin to that registry.

## Architecture and requirements

- Kubernetes 1.25+, Helm 3, namespace creation/installation permissions, and a
  filesystem StorageClass that implements `fsGroup` permissions.
- **Default: one Janus replica and one PostgreSQL 16 StatefulSet with a PVC.**
  This database is **not HA**. There is no database operator, external chart
  dependency, fixed storage backend, or implicit replication. Increasing Janus
  replicas does not make the bundled database highly available.
- Alternatively, use existing external PostgreSQL (managed HA is recommended for
  critical production systems), or explicitly select single-replica SQLite for
  evaluation. Database selection is not a data migration mechanism.
- Existing Secrets are required; this chart never generates or rotates keys or
  passwords, and never embeds credentials in Helm values or release manifests.
- All Services are ClusterIP. Ingress and ServiceMonitor are disabled by default;
  enable them only with the corresponding controller/CRD installed.
- Janus runs as nonroot UID/GID 65532; the official Alpine PostgreSQL image runs
  as UID/GID 70. Both use read-only root filesystems, writable data/tmp mounts,
  dropped capabilities, RuntimeDefault seccomp, and no service-account token.
  Alternate PostgreSQL images must support this UID/layout (Debian variants do not).

## First installation: bundled PostgreSQL

Keep the namespace/network private during setup. ClusterIP does **not** isolate
an application from other cluster workloads: use your CNI/network controls and
restrict access until the first administrator is claimed. Anyone who can reach
an unclaimed instance can create that administrator. OIDC is optional; development
authentication is always disabled. There is no default administrator password.

Generate each secret **once**, store it in your secret manager, and back it up.
The following commands are for a new installation only; they deliberately fail
if the named Secret already exists. Run from a secure workstation without shell
tracing. Temporary files are created with private permissions and deleted after
use; save the generated values in your approved secret manager before deletion.
Do not rerun key generation for an existing database.

```sh
kubectl create namespace janus
umask 077
secret_dir=$(mktemp -d)
trap 'rm -rf "$secret_dir"' EXIT
openssl rand -hex 32 | tr -d '\n' > "$secret_dir/encryption-key"
openssl rand -base64 48 | tr -d '\n' > "$secret_dir/password"
# Back up these files securely before continuing.
kubectl -n janus create secret generic janus-key \
  --from-file=JANUS_ENCRYPTION_KEY="$secret_dir/encryption-key"
kubectl -n janus create secret generic janus-db \
  --from-file=password="$secret_dir/password"
helm upgrade --install janus ./charts/janus-helm-chart --namespace janus \
  --set encryptionKey.existingSecret=janus-key \
  --set postgresql.existingSecret=janus-db \
  --wait --timeout 10m
kubectl -n janus port-forward --address 127.0.0.1 service/janus-janus 8080:8080
```

Visit <http://127.0.0.1:8080> and create the first administrator. The default
`publicURL` and non-secure cookie are **only** for this loopback access path.
HTTP public origins other than loopback are rejected by the values schema.
The encryption Secret must contain exactly 64 hex characters (32 bytes).
Secret contents are validated by Janus at runtime, not by Helm's offline render.
A missing Secret/key causes a Kubernetes configuration error; fix it, never
replace an existing encryption key to make a startup error disappear.

Gateway startup may initially retry while PostgreSQL initializes; the gateway
checks its database and runs migrations at startup. Probes use `/healthz` and
`/readyz`, with a five-minute startup allowance and 45-second termination grace
(`terminationGracePeriodSeconds`).
Single-replica upgrades use `Recreate` (brief downtime). Multi-replica PostgreSQL
configurations use rolling updates with no surge, so upgrades do not temporarily
exceed the configured gateway replica count.

## Production HTTPS / ingress

After private bootstrap, persist non-secret settings in a values file and use
that file on every upgrade. Configure the public origin and actual ingress host
consistently. TLS can terminate at your ingress or a trusted upstream proxy;
providing a TLS Secret below is the usual ingress termination setup.

```yaml
encryptionKey:
  existingSecret: janus-key
postgresql:
  existingSecret: janus-db
publicURL: https://janus.example.com
ingress:
  enabled: true
  className: nginx
  host: janus.example.com
  tls:
    - secretName: janus-tls
      hosts: [janus.example.com]
```

`publicURL` must be HTTPS when ingress is enabled. The chart sets secure cookies
from the URL. No certificate issuer is installed. List the proxies in front of
Janus (usually the ingress-controller pods) in `trustedProxies` so client IPs are
read from `X-Forwarded-For`; do not trust every source. Existing installs that set
`JANUS_TRUSTED_PROXIES` through `extraEnv` keep working; set one or the other.
Set ingress-controller-specific streaming timeouts/body limits in
`ingress.annotations` for your workload. Prometheus `/metrics` is served only on
its own port (`metrics.port`, default 9464; the Service port named `metrics`),
never through the Ingress; it is unauthenticated, so keep it inside the cluster.

## Existing external PostgreSQL

Provision a database/user with permission to run Janus schema migrations, plus
an existing password Secret in the release namespace. Credentials are passed
through separate standard `PGHOST`, `PGPORT`, `PGDATABASE`, `PGUSER`, and
`PGPASSWORD` variables understood by Janus's pgx driver, with
`JANUS_DATABASE_URL=postgres://`. Password punctuation is never interpolated into
a connection URI. The default external TLS mode is `verify-full`.

```yaml
encryptionKey:
  existingSecret: janus-key
postgresql:
  enabled: false
externalPostgresql:
  host: postgres.example.com
  port: 5432
  database: janus
  username: janus
  existingSecret: janus-external-db
  passwordKey: password
  sslMode: verify-full
  caSecret: janus-postgres-ca
  caKey: ca.crt
publicURL: https://janus.example.com
```

CA Secrets contain a PEM CA bundle. Omit `caSecret` only when the server's chain
is already trusted by the image. `require` encrypts without verifying server
identity; `disable` is for explicitly trusted networks. Bundled PostgreSQL uses
unencrypted in-cluster connections: use external PostgreSQL with verified TLS
when your threat model requires transport encryption. The bundled database
initializes its named user as a PostgreSQL superuser; use a separately provisioned
least-privilege external database role for stricter isolation.

## High availability with CloudNativePG

Several gateway replicas are only as available as the database behind them, and
the bundled PostgreSQL is a single instance. For HA, run Janus against an HA
PostgreSQL such as a [CloudNativePG](https://cloudnative-pg.io) cluster (the
operator is installed separately; this chart does not depend on it). Janus keeps
sessions and state in the database, so no sticky sessions or shared cache are
needed. A complete values file is in
[`examples/values-ha-cloudnativepg.yaml`](examples/values-ha-cloudnativepg.yaml).

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: janus-db
  namespace: janus
spec:
  instances: 3
  imageName: ghcr.io/cloudnative-pg/postgresql:16
  storage:
    size: 10Gi
    # storageClass: a block- or local-backed class suitable for databases
  bootstrap:
    initdb:
      database: janus
      owner: janus
  affinity:
    enablePodAntiAffinity: true
    topologyKey: kubernetes.io/hostname
    podAntiAffinityType: required   # never two instances on one node
  enablePDB: true
  postgresql:
    synchronous:
      method: any
      number: 1
      dataDurability: preferred     # keep accepting writes if no standby is up
```

CloudNativePG creates `janus-db-rw` (the read-write Service that follows the
primary), `janus-db-app` (key `password`) and `janus-db-ca` (key `ca.crt`), which
the example consumes with `sslMode: verify-full`. Then:

```sh
helm install janus <chart> -n janus -f examples/values-ha-cloudnativepg.yaml
```

What the HA values do (all default to off for one replica, so single-replica
installs render exactly as before):

| Value | Default | Effect |
|---|---|---|
| `podDisruptionBudget.enabled`, `.maxUnavailable` | `auto`, `1` | PodDisruptionBudget when `replicaCount > 1` (`true`/`false` force it). `maxUnavailable` rather than `minAvailable`, so a drain still proceeds when a pod is already Pending |
| `topologySpread.enabled`, `.hostname`, `.zone` | `auto`, `ScheduleAnyway`, `""` | Spread gateway pods across nodes (and zones when `zone` is set) when `replicaCount > 1`. Soft by default: on a three-node cluster a drained node's pod can still run on a survivor |
| `topologySpreadConstraints` | `[]` | Raw constraints; when non-empty they replace the generated ones |
| `lifecycle.preStopSleepSeconds` | `0` | Native `preStop` `sleep` (the image has no shell) so endpoints and the ingress stop routing to a terminating pod before it drains. Rendered only on Kubernetes 1.30 or later; NOTES warns when it is skipped |
| `terminationGracePeriodSeconds` | `45` | Must exceed the preStop sleep plus Janus' 30-second request drain (render fails otherwise) |
| `trustedProxies` | `[]` | `JANUS_TRUSTED_PROXIES` (comma-joined IPs/CIDRs) |

Rolling updates stay `maxSurge: 0` / `maxUnavailable: 1`, so a rollout never runs
more gateway pods than `replicaCount`; two of three serve during a rollout.

Be honest about the remaining single points of failure:

- **The entry point must be redundant too.** Three gateway pods do not help if
  clients reach them through one node's IP. Use a load balancer, a VIP (for
  example kube-vip or MetalLB), or DNS that covers every ingress node, and run
  the ingress controller on more than one node.
- A primary failover takes a few seconds; requests that write to the database in
  that window can fail and should be retried by the client.
- Each gateway replica counts as a licensed node: Community covers one node,
  Business up to three nodes per instance (see COMMERCIAL-TERMS.md).
- With `replicaCount > 1` and bundled PostgreSQL, the chart renders but NOTES
  warns that the gateway is redundant and the database is not.

## SQLite evaluation

```yaml
encryptionKey:
  existingSecret: janus-key
database:
  type: sqlite
postgresql:
  enabled: false
replicaCount: 1
sqlite:
  persistence:
    size: 10Gi
    # existingClaim: previously-created-janus-data
```

SQLite uses `/data/janus.db`, `ReadWriteOnce`, and `Recreate` upgrades. The chart
rejects replicas other than one and rejects bundled PostgreSQL being enabled at
the same time. Never share this claim with another release or writer. RWO alone
is not a multiwriter guard. There is no autoscaler in this chart.

## Storage, backups, upgrades and uninstall

- `postgresql.persistence.storageClass` and `sqlite.persistence.storageClass`:
  `null`/omitted selects the cluster default; `""` explicitly selects no class;
  any other string names a class. Select a block-backed or local filesystem
  StorageClass suitable for databases; do not assume the cluster default is
  appropriate. Avoid NFS for PostgreSQL or SQLite database files unless its
  locking, durability, and failure semantics have been explicitly validated.
- Back up PostgreSQL (consistent database backup/WAL or quiesced snapshots), its
  password, and the unchanged Janus encryption key **together**. For SQLite,
  stop Janus and back up the database/WAL state consistently. Test restores.
  Licensing Secrets should also be preserved. The PostgreSQL-mode `/data`
  directory is scratch space, not a persistent store; export diagnostic bundles
  before replacing a pod.
- The StatefulSet's generated PostgreSQL PVC survives ordinary uninstall;
  SQLite's chart-created PVC has `helm.sh/resource-policy: keep`. Existing
  Secrets are not owned/deleted by Helm. Your StorageClass/PV reclaim policy
  still controls what happens if an operator explicitly deletes a PVC.
- Do not rename the Helm release/fullname or database/username/storageClass on
  an existing bundled install. StatefulSet volume templates are immutable;
  expansion and migration require a separately planned storage operation.
- Changing `postgresql.existingSecret` or its password does **not** change the
  password inside an initialized PostgreSQL PVC. Coordinate SQL password
  rotation, Secret changes, and pod restarts separately. External Secret value
  changes do not automatically roll Janus: restart deliberately after changes.
- Do not switch database modes or PostgreSQL major versions with a casual Helm
  upgrade. Back up first; rehearse migrations/restores. Helm rollback does not
  reverse application schema migrations. Disabling bundled PostgreSQL stops
  its StatefulSet but retains the PVC; it does not copy data to an external DB.
- Reinstall with the same release identity and Secrets for PostgreSQL PVC reuse.
  To reuse retained SQLite storage, set `sqlite.persistence.existingClaim` to
  the retained claim (normally `janus-janus-data`) instead of asking Helm to
  create it again. Review resource ownership before any adoption.

## Other values

See `values.yaml` and `values.schema.json` for the complete interface.

| Value | Purpose |
|---|---|
| `fullnameOverride`, `nameOverride` | Stable resource naming; set before first install |
| `imagePullSecrets` | Existing registry credential Secret references |
| `replicaCount` | Gateway replicas; default 1; check license entitlement before scaling |
| `license.existingSecret`, `license.key` | Optional license file mounted read-only; no key supplied by chart |
| `resources`, `postgresql.resources` | Separate gateway/database sizing |
| `nodeSelector`, `tolerations`, `affinity` | Gateway scheduling; these do not configure PostgreSQL scheduling |
| `podDisruptionBudget`, `topologySpread`, `topologySpreadConstraints`, `lifecycle.preStopSleepSeconds`, `terminationGracePeriodSeconds` | Gateway HA settings; see [High availability with CloudNativePG](#high-availability-with-cloudnativepg) |
| `trustedProxies` | Reverse-proxy IPs/CIDRs trusted for `X-Forwarded-For` (`JANUS_TRUSTED_PROXIES`) |
| `podAnnotations` | Gateway pod annotations; change a rollout annotation to restart after Secret changes |
| `extraEnv` | Map of additional **string** application env values |
| `secretEnv` | Map of env names to `{name: existing-secret, key: secret-key}` |
| `metrics.enabled`, `metrics.port` | Separate Prometheus metrics listener (`JANUS_METRICS_ADDR`), default on at 9464 and published on the ClusterIP Service as port `metrics`; `false` turns it off |
| `serviceMonitor.enabled` | Requires Prometheus Operator ServiceMonitor CRD and `metrics.enabled`, otherwise render fails; scrapes the `metrics` port |
| `serviceMonitor.labels` | Labels matching your Prometheus serviceMonitorSelector |

Chart-managed database/encryption/listen/public URL/auth-safety variables and
all `PG*` variables cannot be overridden by extra settings. Use `secretEnv` for
OIDC client secrets, SMTP passwords, etc.; avoid credentials in literal values.
OIDC can also be configured through the administrator UI after local bootstrap.
The chart neither issues licenses nor bypasses feature gates. Additional gateway
replicas require the appropriate license; Community defaults to one node. See
the repository's LICENSE and COMMERCIAL-TERMS.md for authoritative terms.

## Validation and packaging

No running cluster is required for the focused static test suite:

```sh
# Python 3 + PyYAML; Helm 3 on PATH (or set HELM=/path/to/helm).
python3 scripts/test_helm_chart.py
helm lint charts/janus-helm-chart --strict \
  --set encryptionKey.existingSecret=test-key --set postgresql.existingSecret=test-db
helm template janus charts/janus-helm-chart \
  --set encryptionKey.existingSecret=test-key --set postgresql.existingSecret=test-db
helm package charts/janus-helm-chart --destination /chosen/artifact/directory
```

Bare `helm template` intentionally fails until existing Secret names are supplied.
`helm lint` may treat template `required`/`fail` as informational; rely on the
negative render tests, not lint alone, for enforcement. No cluster lookups or
random generators are used, making offline rendering and GitOps deterministic.
For offline rendering of ServiceMonitor, pass
`--api-versions monitoring.coreos.com/v1/ServiceMonitor` only if the target cluster
actually has that CRD.

## Runtime acceptance for 0.1.0

Disposable Kubernetes installations of the pinned application image passed:

- Bundled PostgreSQL, single-replica SQLite, and independently provisioned
  PostgreSQL: initial migrations, first-administrator setup, authenticated admin
  API access, pod replacement, and a Helm upgrade that replaces the gateway pod.
- The same administrator could sign in after each replacement and upgrade;
  database and encryption Secrets remained byte-for-byte unchanged. PostgreSQL
  instances were also restarted, and database PVCs used block-backed storage.
- External PostgreSQL with a punctuation-bearing password and `verify-full`
  TLS using a mounted CA; active TLS connections were confirmed at the server.

These tests used an authenticated registry pull. They do not establish anonymous
registry availability, HA, backup/restore, ingress-controller compatibility, or
Prometheus discovery. Validate those separately for your deployment.

Before publishing deployment claims, exercise on a disposable cluster: default
PostgreSQL initialization/migrations and first-admin login without OIDC; restart,
upgrade and backup/restore with unchanged Secrets; external PostgreSQL TLS and
punctuated passwords; SQLite PVC permissions/restart; ingress/TLS and a real
ServiceMonitor discovery if used. Static template checks do not prove runtime
storage permissions, image pulls, migrations, ingress-controller behavior, or
cluster compatibility.
