#!/usr/bin/env python3
"""Offline Helm contract tests. Requires Helm 3 and PyYAML, never a cluster.

HELM selects the binary. HELM_TEST_ARTIFACTS optionally saves command outputs
and rendered manifests to a directory outside the source tree.
"""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[1]
CHART = ROOT / "charts/janus-helm-chart"
HELM = os.environ.get("HELM", "helm")
ARTIFACTS = os.environ.get("HELM_TEST_ARTIFACTS")
BASE = {"encryptionKey": {"existingSecret": "test-key"},
        "postgresql": {"existingSecret": "test-db"}}


def merged(left, right):
    result = dict(left)
    for key, value in right.items():
        result[key] = merged(result.get(key, {}), value) if isinstance(value, dict) else value
    return result


def objects(text, kind):
    return [doc for doc in yaml.safe_load_all(text) if doc and doc["kind"] == kind]


def deployment(text):
    return objects(text, "Deployment")[0]


def env(text):
    container = deployment(text)["spec"]["template"]["spec"]["containers"][0]
    return {item["name"]: item for item in container["env"]}


class ChartTest(unittest.TestCase):
    counter = 0

    def run_helm(self, values=None, args=(), command="template", chart=CHART, base=True):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "values.yaml"
            path.write_text(yaml.safe_dump(merged(BASE if base else {}, values or {})))
            cmd = [HELM, command]
            if command == "template":
                cmd += ["test"]
            cmd += [str(chart), "-f", str(path), *args]
            result = subprocess.run(cmd, text=True, capture_output=True, check=False)
        if ARTIFACTS:
            target = Path(ARTIFACTS)
            target.mkdir(parents=True, exist_ok=True)
            ChartTest.counter += 1
            stem = f"{self._testMethodName}-{ChartTest.counter}"
            (target / f"{stem}.log").write_text(
                f"command: {' '.join(cmd)}\nexit: {result.returncode}\n"
                f"{result.stderr}\n{result.stdout}")
            if result.returncode == 0 and command == "template":
                (target / f"{stem}.yaml").write_text(result.stdout)
        return result

    def render(self, values=None, **kwargs):
        result = self.run_helm(values, **kwargs)
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        return result.stdout

    def reject(self, values, message, **kwargs):
        result = self.run_helm(values, **kwargs)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn(message, result.stderr)

    def test_default_single_postgres(self):
        text = self.render()
        self.assertEqual(len(list(yaml.safe_load_all(text))), 4)
        self.assertEqual(deployment(text)["spec"]["replicas"], 1)
        self.assertEqual(deployment(text)["spec"]["strategy"]["type"], "Recreate")
        pg = objects(text, "StatefulSet")[0]
        self.assertEqual(pg["spec"]["replicas"], 1)
        claim = pg["spec"]["volumeClaimTemplates"][0]["spec"]
        self.assertEqual(claim["accessModes"], ["ReadWriteOnce"])
        self.assertNotIn("storageClassName", claim)
        self.assertEqual(env(text)["PGHOST"]["value"], "test-janus-postgresql")
        self.assertEqual(env(text)["JANUS_DATABASE_URL"]["value"], "postgres://")
        self.assertEqual(env(text)["PGSSLMODE"]["value"], "disable")
        self.assertFalse(objects(text, "Secret"))
        self.assertFalse(objects(text, "Ingress"))
        self.assertFalse(objects(text, "ServiceMonitor"))

    def notes(self, values=None):
        result = self.run_helm(values, command="install",
                               args=("--generate-name", "--dry-run=client"))
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        return result.stdout.split("NOTES:", 1)[1]

    def test_notes_first_administrator(self):
        private = "Complete the first-administrator form PRIVATELY before enabling ingress:"
        text = self.notes()
        self.assertIn(private, text)
        self.assertNotIn("UNCLAIMED", text)
        self.assertIn("port-forward --address 127.0.0.1 service/", text)
        text = self.notes({"publicURL": "https://janus.example.com",
                           "ingress": {"enabled": True, "host": "janus.example.com"}})
        self.assertNotIn(private, text)
        self.assertIn("UNCLAIMED until the first administrator exists", text)
        self.assertIn("Create the administrator NOW, privately", text)
        self.assertIn("port-forward --address 127.0.0.1 service/", text)
        self.assertIn("If janus.example.com is already publicly reachable, "
                      "install with ingress.enabled=false first", text)

    def test_security_and_probes(self):
        text = self.render()
        for kind, uid in [("Deployment", 65532), ("StatefulSet", 70)]:
            spec = objects(text, kind)[0]["spec"]["template"]["spec"]
            self.assertFalse(spec["automountServiceAccountToken"])
            self.assertEqual(spec["securityContext"]["runAsUser"], uid)
            self.assertEqual(spec["securityContext"]["fsGroup"], uid)
            container = spec["containers"][0]
            self.assertTrue(container["securityContext"]["readOnlyRootFilesystem"])
            self.assertFalse(container["securityContext"]["allowPrivilegeEscalation"])
            self.assertEqual(container["securityContext"]["capabilities"]["drop"], ["ALL"])
        pod = deployment(text)["spec"]["template"]["spec"]
        self.assertEqual(pod["terminationGracePeriodSeconds"], 45)
        container = pod["containers"][0]
        self.assertEqual(container["readinessProbe"]["httpGet"]["path"], "/readyz")
        self.assertEqual(container["livenessProbe"]["httpGet"]["path"], "/healthz")
        self.assertEqual(container["startupProbe"]["failureThreshold"], 60)
        for name in ["JANUS_DEV_AUTH", "JANUS_DEV_AUTH_ALLOW_UNSAFE", "JANUS_COOKIE_SECURE"]:
            self.assertEqual(env(text)[name]["value"], "false")

    def test_required_encryption_secret(self):
        self.reject({}, "encryptionKey.existingSecret is required", base=False)

    def test_required_database_secret(self):
        self.reject({"encryptionKey": {"existingSecret": "key"}},
                    "postgresql.existingSecret is required", base=False)

    def test_secret_references_upgrade_determinism(self):
        first = self.render()
        self.assertEqual(first, self.render())
        self.assertEqual(first, self.render(args=["--is-upgrade"]))
        self.assertEqual(env(first)["PGPASSWORD"]["valueFrom"]["secretKeyRef"],
                         {"name": "test-db", "key": "password"})
        pg_env = objects(first, "StatefulSet")[0]["spec"]["template"]["spec"]["containers"][0]["env"]
        self.assertEqual(next(x for x in pg_env if x["name"] == "POSTGRES_PASSWORD")["valueFrom"],
                         env(first)["PGPASSWORD"]["valueFrom"])
        for path in (CHART / "templates").iterdir():
            self.assertNotIn("randAlpha", path.read_text())
            self.assertNotIn("lookup ", path.read_text())

    def test_sqlite(self):
        text = self.render({"database": {"type": "sqlite"}, "postgresql": {"enabled": False}})
        self.assertFalse(objects(text, "StatefulSet"))
        self.assertEqual(env(text)["JANUS_DATABASE_URL"]["value"], "sqlite:///data/janus.db")
        self.assertFalse(any(name.startswith("PG") for name in env(text)))
        self.assertEqual(deployment(text)["spec"]["strategy"]["type"], "Recreate")
        pvc = objects(text, "PersistentVolumeClaim")[0]
        self.assertEqual(pvc["metadata"]["annotations"]["helm.sh/resource-policy"], "keep")
        self.assertNotIn("storageClassName", pvc["spec"])

    def test_sqlite_existing_claim(self):
        text = self.render({"database": {"type": "sqlite"}, "postgresql": {"enabled": False},
                            "sqlite": {"persistence": {"existingClaim": "restored-data"}}})
        self.assertFalse(objects(text, "PersistentVolumeClaim"))
        volumes = deployment(text)["spec"]["template"]["spec"]["volumes"]
        self.assertEqual(next(v for v in volumes if v["name"] == "data")["persistentVolumeClaim"]["claimName"],
                         "restored-data")

    def test_sqlite_ha_rejected(self):
        self.reject({"database": {"type": "sqlite"}, "postgresql": {"enabled": False},
                     "replicaCount": 2}, "SQLite requires replicaCount=1")

    def test_sqlite_bundled_conflict(self):
        self.reject({"database": {"type": "sqlite"}}, "SQLite requires postgresql.enabled=false")

    def test_external_postgres(self):
        text = self.render({"postgresql": {"enabled": False}, "replicaCount": 3,
                            "externalPostgresql": {"host": "db.example.com", "port": 6432,
                            "database": "janus-special db", "username": "janus@role",
                            "existingSecret": "external-db", "passwordKey": "db-pass",
                            "caSecret": "pg-ca", "caKey": "root.pem"}})
        self.assertFalse(objects(text, "StatefulSet"))
        self.assertFalse(objects(text, "PersistentVolumeClaim"))
        self.assertEqual(env(text)["PGHOST"]["value"], "db.example.com")
        self.assertEqual(env(text)["PGPORT"]["value"], "6432")
        self.assertEqual(env(text)["PGDATABASE"]["value"], "janus-special db")
        self.assertEqual(env(text)["PGUSER"]["value"], "janus@role")
        self.assertEqual(env(text)["PGSSLMODE"]["value"], "verify-full")
        self.assertEqual(env(text)["PGSSLROOTCERT"]["value"], "/etc/janus/postgresql/ca.crt")
        self.assertEqual(env(text)["PGPASSWORD"]["valueFrom"]["secretKeyRef"],
                         {"name": "external-db", "key": "db-pass"})
        self.assertEqual(deployment(text)["spec"]["strategy"]["rollingUpdate"],
                         {"maxSurge": 0, "maxUnavailable": 1})

    def test_external_missing_parameters(self):
        for pg in [{}, {"host": "db"}, {"existingSecret": "db"}]:
            with self.subTest(pg=pg):
                self.reject({"postgresql": {"enabled": False}, "externalPostgresql": pg},
                             "external PostgreSQL requires")

    def test_external_and_bundled_conflict(self):
        self.reject({"externalPostgresql": {"host": "db"}}, "mutually exclusive")

    def test_storage_classes(self):
        for storage in ["", "fast-ssd"]:
            with self.subTest(storage=storage):
                pg = objects(self.render({"postgresql": {"persistence": {"storageClass": storage}}}),
                             "StatefulSet")[0]
                self.assertEqual(pg["spec"]["volumeClaimTemplates"][0]["spec"]["storageClassName"], storage)
                sqlite = self.render({"database": {"type": "sqlite"}, "postgresql": {"enabled": False},
                                      "sqlite": {"persistence": {"storageClass": storage}}})
                self.assertEqual(objects(sqlite, "PersistentVolumeClaim")[0]["spec"]["storageClassName"], storage)

    def test_ingress_and_cookies(self):
        text = self.render({"publicURL": "https://janus.example.com", "ingress": {
            "enabled": True, "host": "janus.example.com", "className": "nginx",
            "annotations": {"example.com/test": "yes"},
            "tls": [{"secretName": "janus-tls", "hosts": ["janus.example.com"]}]}})
        ing = objects(text, "Ingress")[0]
        self.assertEqual(ing["spec"]["ingressClassName"], "nginx")
        self.assertEqual(ing["spec"]["tls"][0]["secretName"], "janus-tls")
        self.assertEqual(env(text)["JANUS_COOKIE_SECURE"]["value"], "true")

    def test_ingress_guard(self):
        self.reject({"ingress": {"enabled": True}}, "ingress.host is required")
        self.reject({"ingress": {"enabled": True, "host": "janus.example.com"}},
                    "Ingress requires an HTTPS publicURL")

    def test_monitor_crd_guard(self):
        self.reject({"serviceMonitor": {"enabled": True}}, "ServiceMonitor CRD")
        text = self.render({"serviceMonitor": {"enabled": True, "labels": {"release": "metrics"}}},
                           args=["--api-versions", "monitoring.coreos.com/v1/ServiceMonitor"])
        monitor = objects(text, "ServiceMonitor")[0]
        self.assertEqual(monitor["spec"]["endpoints"][0]["path"], "/metrics")
        self.assertEqual(monitor["spec"]["endpoints"][0]["port"], "metrics")
        self.assertEqual(monitor["metadata"]["labels"]["release"], "metrics")
        self.reject({"serviceMonitor": {"enabled": True}, "metrics": {"enabled": False}},
                    "requires metrics.enabled", args=["--api-versions", "monitoring.coreos.com/v1/ServiceMonitor"])

    def test_metrics_port(self):
        # Janus serves /metrics only on JANUS_METRICS_ADDR, never on :8080.
        text = self.render({})
        self.assertEqual(env(text)["JANUS_METRICS_ADDR"]["value"], ":9464")
        deploy = objects(text, "Deployment")[0]
        ports = {p["name"]: p["containerPort"] for p in deploy["spec"]["template"]["spec"]["containers"][0]["ports"]}
        self.assertEqual(ports, {"http": 8080, "metrics": 9464})
        service = [s for s in objects(text, "Service") if s["metadata"]["name"] == deploy["metadata"]["name"]][0]
        self.assertEqual({p["name"]: p["targetPort"] for p in service["spec"]["ports"]}, {"http": "http", "metrics": "metrics"})
        off = self.render({"metrics": {"enabled": False}})
        self.assertEqual(env(off)["JANUS_METRICS_ADDR"]["value"], "off")
        deploy = objects(off, "Deployment")[0]
        self.assertEqual([p["name"] for p in deploy["spec"]["template"]["spec"]["containers"][0]["ports"]], ["http"])

    def test_license_extra_env_and_secret_env(self):
        text = self.render({"license": {"existingSecret": "license", "key": "license.key"},
                            "extraEnv": {"JANUS_LOG_LEVEL": "debug", "JANUS_OFFLINE": "true"},
                            "secretEnv": {"JANUS_SMTP_PASSWORD": {"name": "mail", "key": "pass"}}})
        self.assertEqual(env(text)["JANUS_LICENSE_FILE"]["value"], "/etc/janus/license/license")
        self.assertEqual(env(text)["JANUS_LOG_LEVEL"]["value"], "debug")
        self.assertEqual(env(text)["JANUS_SMTP_PASSWORD"]["valueFrom"]["secretKeyRef"]["name"], "mail")
        self.assertIsInstance(env(text)["JANUS_OFFLINE"]["value"], str)

    def test_env_override_guards(self):
        for key in ["JANUS_DATABASE_URL", "JANUS_ENCRYPTION_KEY", "JANUS_DEV_AUTH",
                    "JANUS_DEV_AUTH_ALLOW_UNSAFE", "JANUS_COOKIE_SECURE", "JANUS_LISTEN_ADDR",
                    "JANUS_PUBLIC_URL", "JANUS_ENV", "JANUS_LICENSE_FILE", "JANUS_METRICS_ADDR", "PGHOST", "PGSERVICE"]:
            for field in ["extraEnv", "secretEnv"]:
                with self.subTest(key=key, field=field):
                    val = "unsafe" if field == "extraEnv" else {"name": "unsafe", "key": "value"}
                    self.reject({field: {key: val}}, "chart-managed or reserved")
        self.reject({"extraEnv": {"JANUS_LOG_LEVEL": "debug"},
                     "secretEnv": {"JANUS_LOG_LEVEL": {"name": "secret", "key": "value"}}},
                     "appears in both")

    def test_schema_guards(self):
        for value in [{"replicaCount": 0}, {"replicaCount": 1.5}, {"database": {"type": "mysql"}},
                      {"publicURL": "http://public.example.com"}, {"publicURL": "https://example.com/path"},
                      {"image": {"digest": "sha256:bad"}}, {"service": {"port": 65536}},
                      {"externalPostgresql": {"sslMode": "prefer"}}, {"extraEnv": {"BAD-NAME": "x"}},
                      {"extraEnv": {"JANUS_OFFLINE": True}}, {"postgresql": {"enabled": "false"}},
                      {"postgressql": {}}, {"secretEnv": {"JANUS_LOG_LEVEL": {"name": "x"}}}]:
            with self.subTest(value=value):
                self.reject(value, "values don't meet the specifications")

    def test_name_length_selectors_and_services(self):
        text = self.render({"fullnameOverride": "a" * 100})
        docs = [doc for doc in yaml.safe_load_all(text) if doc]
        workloads = [doc for doc in docs if doc["kind"] in ["Deployment", "StatefulSet"]]
        for doc in docs:
            self.assertLessEqual(len(doc["metadata"]["name"]), 63)
        for workload in workloads:
            self.assertEqual(workload["spec"]["selector"]["matchLabels"],
                             workload["spec"]["template"]["metadata"]["labels"])
        for service in objects(text, "Service"):
            matches = [work for work in workloads if service["spec"]["selector"] ==
                       work["spec"]["template"]["metadata"]["labels"]]
            self.assertEqual(len(matches), 1)
            names = {p["name"] for p in matches[0]["spec"]["template"]["spec"]["containers"][0]["ports"]}
            self.assertIn(service["spec"]["ports"][0]["targetPort"], names)

    def test_rename_keeps_resource_names_for_upgrades(self):
        # The chart was renamed janus -> janus-helm-chart in 0.2.0. Deployment and
        # StatefulSet selectors are immutable, so rendered names and labels must
        # stay exactly what chart 0.1.x produced or `helm upgrade` fails.
        docs = [doc for doc in yaml.safe_load_all(self.render()) if doc]
        names = sorted({doc["metadata"]["name"] for doc in docs})
        self.assertIn("test-janus", names)
        labelled = [doc for doc in docs if "helm.sh/chart" in doc["metadata"].get("labels", {})]
        self.assertTrue(labelled)
        for doc in labelled:
            self.assertEqual(doc["metadata"]["labels"]["app.kubernetes.io/name"], "janus")
            self.assertEqual(doc["metadata"]["labels"]["helm.sh/chart"], "janus-helm-chart-2026.11.2")
        for work in (d for d in docs if d["kind"] in ["Deployment", "StatefulSet"]):
            self.assertEqual(work["spec"]["selector"]["matchLabels"]["app.kubernetes.io/name"], "janus")

    def test_image_digest_and_tag(self):
        image = deployment(self.render())["spec"]["template"]["spec"]["containers"][0]["image"]
        self.assertEqual(image, "ghcr.io/torvanis/janus@sha256:ec74d8c442c564a04f9cb791458f7fe28bc5c73045a6bec5d35235d614bf4bba")
        image = deployment(self.render({"image": {"digest": "", "tag": "2026.11.2"}}))["spec"]["template"]["spec"]["containers"][0]["image"]
        self.assertEqual(image, "ghcr.io/torvanis/janus:2026.11.2")

    def test_lint_variants(self):
        for value in [{}, {"database": {"type": "sqlite"}, "postgresql": {"enabled": False}},
                      {"postgresql": {"enabled": False},
                       "externalPostgresql": {"host": "db", "existingSecret": "db"}}]:
            with self.subTest(value=value):
                result = self.run_helm(value, command="lint", args=["--strict"])
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def pod(self, text):
        return deployment(text)["spec"]["template"]["spec"]

    def test_ha_defaults_unchanged_for_one_replica(self):
        text = self.render()
        self.assertFalse(objects(text, "PodDisruptionBudget"))
        pod = self.pod(text)
        self.assertNotIn("topologySpreadConstraints", pod)
        self.assertNotIn("lifecycle", pod["containers"][0])
        self.assertEqual(pod["terminationGracePeriodSeconds"], 45)
        self.assertNotIn("JANUS_TRUSTED_PROXIES", env(text))

    def test_ha_auto_with_replicas(self):
        text = self.render({"replicaCount": 3, "postgresql": {"enabled": False},
                            "externalPostgresql": {"host": "db", "existingSecret": "db"}})
        pdb = objects(text, "PodDisruptionBudget")[0]
        selector = deployment(text)["spec"]["selector"]["matchLabels"]
        self.assertEqual(pdb["spec"], {"maxUnavailable": 1, "selector": {"matchLabels": selector}})
        self.assertEqual(self.pod(text)["topologySpreadConstraints"], [
            {"maxSkew": 1, "topologyKey": "kubernetes.io/hostname",
             "whenUnsatisfiable": "ScheduleAnyway", "labelSelector": {"matchLabels": selector}}])

    def test_ha_forced_and_overridden(self):
        text = self.render({"podDisruptionBudget": {"enabled": True, "maxUnavailable": "50%"},
                            "topologySpread": {"enabled": True, "hostname": "DoNotSchedule",
                                               "zone": "ScheduleAnyway"}})
        self.assertEqual(objects(text, "PodDisruptionBudget")[0]["spec"]["maxUnavailable"], "50%")
        spread = self.pod(text)["topologySpreadConstraints"]
        self.assertEqual([(c["topologyKey"], c["whenUnsatisfiable"]) for c in spread],
                         [("kubernetes.io/hostname", "DoNotSchedule"),
                          ("topology.kubernetes.io/zone", "ScheduleAnyway")])
        off = self.render({"replicaCount": 2, "podDisruptionBudget": {"enabled": False},
                           "topologySpread": {"enabled": False}})
        self.assertFalse(objects(off, "PodDisruptionBudget"))
        self.assertNotIn("topologySpreadConstraints", self.pod(off))
        raw = [{"maxSkew": 2, "topologyKey": "example.com/rack", "whenUnsatisfiable": "DoNotSchedule",
                "labelSelector": {"matchLabels": {"app.kubernetes.io/component": "gateway"}}}]
        text = self.render({"replicaCount": 2, "topologySpreadConstraints": raw})
        self.assertEqual(self.pod(text)["topologySpreadConstraints"], raw)

    def test_prestop_sleep_needs_kubernetes_130(self):
        values = {"lifecycle": {"preStopSleepSeconds": 10}, "terminationGracePeriodSeconds": 50}
        new = self.pod(self.render(values, args=["--kube-version", "1.31.0"]))
        self.assertEqual(new["terminationGracePeriodSeconds"], 50)
        self.assertEqual(new["containers"][0]["lifecycle"], {"preStop": {"sleep": {"seconds": 10}}})
        self.assertIn("lifecycle", self.pod(self.render(values, args=["--kube-version", "1.30.0"]))["containers"][0])
        old = self.pod(self.render(values, args=["--kube-version", "1.29.0"]))
        self.assertNotIn("lifecycle", old["containers"][0])
        self.assertEqual(old["terminationGracePeriodSeconds"], 50)
        self.reject({"lifecycle": {"preStopSleepSeconds": 45}}, "must exceed lifecycle.preStopSleepSeconds")
        # 45 is not enough for a 10 s sleep plus the 30 s default drain plus 5 s.
        self.reject({"lifecycle": {"preStopSleepSeconds": 10}}, "= 45")

    def test_shutdown_and_connect_timeout_unset_by_default(self):
        for values in [{}, {"database": {"type": "sqlite"}, "postgresql": {"enabled": False}},
                       {"postgresql": {"enabled": False},
                        "externalPostgresql": {"host": "db", "existingSecret": "db"}}]:
            with self.subTest(values=values):
                names = env(self.render(values))
                for name in ["JANUS_SHUTDOWN_DELAY", "JANUS_SHUTDOWN_GRACE", "JANUS_DB_CONNECT_TIMEOUT"]:
                    self.assertNotIn(name, names)

    def test_shutdown_and_connect_timeout(self):
        text = self.render({"shutdown": {"delaySeconds": 10, "graceSeconds": 120},
                            "database": {"connectTimeoutSeconds": 7},
                            "terminationGracePeriodSeconds": 140})
        self.assertEqual(env(text)["JANUS_SHUTDOWN_DELAY"]["value"], "10s")
        self.assertEqual(env(text)["JANUS_SHUTDOWN_GRACE"]["value"], "120s")
        self.assertEqual(env(text)["JANUS_DB_CONNECT_TIMEOUT"]["value"], "7s")
        self.assertEqual(self.pod(text)["terminationGracePeriodSeconds"], 140)
        external = self.render({"postgresql": {"enabled": False}, "database": {"connectTimeoutSeconds": 5},
                                "externalPostgresql": {"host": "db", "existingSecret": "db"}})
        self.assertEqual(env(external)["JANUS_DB_CONNECT_TIMEOUT"]["value"], "5s")
        # An explicit 0 delay is rendered (it is a choice, not an absence).
        zero = self.render({"shutdown": {"delaySeconds": 0}})
        self.assertEqual(env(zero)["JANUS_SHUTDOWN_DELAY"]["value"], "0s")
        # SQLite has no PostgreSQL connection to time out.
        sqlite = self.render({"database": {"type": "sqlite", "connectTimeoutSeconds": 5},
                              "postgresql": {"enabled": False}})
        self.assertNotIn("JANUS_DB_CONNECT_TIMEOUT", env(sqlite))

    def test_termination_grace_covers_shutdown(self):
        # 45 > 0 + 0 + 30 + 5 (defaults) renders; each part counts.
        self.render({"terminationGracePeriodSeconds": 36})
        self.reject({"terminationGracePeriodSeconds": 35}, "= 35")
        self.reject({"shutdown": {"delaySeconds": 10}}, "= 45")
        self.render({"shutdown": {"delaySeconds": 10}, "terminationGracePeriodSeconds": 46})
        self.reject({"shutdown": {"graceSeconds": 60}}, "= 65")
        self.reject({"shutdown": {"delaySeconds": 10, "graceSeconds": 30},
                     "lifecycle": {"preStopSleepSeconds": 5}, "terminationGracePeriodSeconds": 50}, "= 50")
        self.render({"shutdown": {"graceSeconds": 10}, "terminationGracePeriodSeconds": 16})

    def test_shutdown_values_and_env_not_both(self):
        for value, env_name in [({"shutdown": {"delaySeconds": 5}}, "JANUS_SHUTDOWN_DELAY"),
                                ({"shutdown": {"graceSeconds": 5}}, "JANUS_SHUTDOWN_GRACE"),
                                ({"database": {"connectTimeoutSeconds": 5}}, "JANUS_DB_CONNECT_TIMEOUT")]:
            with self.subTest(env=env_name):
                self.reject(merged(value, {"extraEnv": {env_name: "5s"}, "terminationGracePeriodSeconds": 60}),
                            "not both")
                self.reject(merged(value, {"secretEnv": {env_name: {"name": "s", "key": "k"}},
                                           "terminationGracePeriodSeconds": 60}), "not both")
        # Without the dedicated value, extraEnv keeps working as before.
        legacy = self.render({"extraEnv": {"JANUS_SHUTDOWN_DELAY": "10s"}})
        self.assertEqual(env(legacy)["JANUS_SHUTDOWN_DELAY"]["value"], "10s")

    def test_trusted_proxies(self):
        text = self.render({"trustedProxies": ["10.42.0.0/16", "fd00::/8", "192.0.2.10"]})
        self.assertEqual(env(text)["JANUS_TRUSTED_PROXIES"]["value"], "10.42.0.0/16,fd00::/8,192.0.2.10")
        legacy = self.render({"extraEnv": {"JANUS_TRUSTED_PROXIES": "10.0.0.0/8"}})
        self.assertEqual(env(legacy)["JANUS_TRUSTED_PROXIES"]["value"], "10.0.0.0/8")
        self.reject({"trustedProxies": ["10.0.0.0/8"], "extraEnv": {"JANUS_TRUSTED_PROXIES": "10.0.0.0/8"}},
                    "not both")

    def test_ha_schema_guards(self):
        for value in [{"podDisruptionBudget": {"enabled": "yes"}}, {"podDisruptionBudget": {"maxUnavailable": 0}},
                      {"podDisruptionBudget": {"minAvailable": 1}}, {"topologySpread": {"hostname": "Maybe"}},
                      {"topologySpreadConstraints": [{"maxSkew": 1}]}, {"lifecycle": {"preStopSleepSeconds": -1}},
                      {"terminationGracePeriodSeconds": 0}, {"trustedProxies": ["0.0.0.0/0"]},
                      {"trustedProxies": ["proxy.example.com"]}, {"trustedProxies": "10.0.0.0/8"},
                      {"shutdown": {"delaySeconds": -1}}, {"shutdown": {"delaySeconds": 301}},
                      {"shutdown": {"graceSeconds": 0}}, {"shutdown": {"graceSeconds": 3601}},
                      {"shutdown": {"graceSeconds": "30s"}}, {"shutdown": {"drainSeconds": 5}},
                      {"database": {"connectTimeoutSeconds": 0}}, {"database": {"connectTimeoutSeconds": 121}},
                      {"database": {"connectTimeoutSeconds": "5"}}]:
            with self.subTest(value=value):
                self.reject(value, "values don't meet the specifications")

    def test_ha_example_values(self):
        example = CHART / "examples/values-ha-cloudnativepg.yaml"
        text = self.render(args=["-f", str(example), "--kube-version", "1.31.0"], base=False)
        self.assertEqual(deployment(text)["spec"]["replicas"], 3)
        self.assertFalse(objects(text, "StatefulSet"))
        self.assertEqual(objects(text, "PodDisruptionBudget")[0]["spec"]["maxUnavailable"], 1)
        pod = self.pod(text)
        self.assertEqual(pod["terminationGracePeriodSeconds"], 50)
        # The gateway's own shutdown delay replaces a preStop sleep.
        self.assertNotIn("lifecycle", pod["containers"][0])
        self.assertEqual(env(text)["JANUS_SHUTDOWN_DELAY"]["value"], "10s")
        self.assertEqual(env(text)["JANUS_SHUTDOWN_GRACE"]["value"], "30s")
        self.assertEqual(env(text)["JANUS_DB_CONNECT_TIMEOUT"]["value"], "5s")
        self.assertEqual(pod["topologySpreadConstraints"][0]["topologyKey"], "kubernetes.io/hostname")
        self.assertEqual(env(text)["PGHOST"]["value"], "janus-db-rw")
        self.assertEqual(env(text)["PGSSLMODE"]["value"], "verify-full")
        self.assertEqual(env(text)["PGSSLROOTCERT"]["value"], "/etc/janus/postgresql/ca.crt")
        self.assertEqual(objects(text, "Ingress")[0]["spec"]["ingressClassName"], "traefik")
        result = self.run_helm(command="lint", args=["--strict", "-f", str(example)], base=False)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_package_roundtrip(self):
        with tempfile.TemporaryDirectory() as temp:
            result = subprocess.run([HELM, "package", str(CHART), "--destination", temp],
                                    capture_output=True, text=True, check=False)
            self.assertEqual(result.returncode, 0, result.stderr)
            package = Path(temp) / "janus-helm-chart-2026.11.2.tgz"
            self.assertTrue(package.exists())
            self.assertEqual(self.render(), self.render(chart=package))
            if ARTIFACTS:
                shutil.copy2(package, Path(ARTIFACTS) / package.name)


if __name__ == "__main__":
    unittest.main(verbosity=2)
