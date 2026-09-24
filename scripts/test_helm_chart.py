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
CHART = ROOT / "charts/janus"
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
        self.assertEqual(monitor["metadata"]["labels"]["release"], "metrics")

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
                    "JANUS_PUBLIC_URL", "JANUS_ENV", "JANUS_LICENSE_FILE", "PGHOST", "PGSERVICE"]:
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

    def test_image_digest_and_tag(self):
        image = deployment(self.render())["spec"]["template"]["spec"]["containers"][0]["image"]
        self.assertEqual(image, "ghcr.io/torvanis/janus@sha256:a4352c0fbde6e506d26be60714926e798ca36e223b124e404efd82a3e0f5a5ca")
        image = deployment(self.render({"image": {"digest": "", "tag": "2026.9.3"}}))["spec"]["template"]["spec"]["containers"][0]["image"]
        self.assertEqual(image, "ghcr.io/torvanis/janus:2026.9.3")

    def test_lint_variants(self):
        for value in [{}, {"database": {"type": "sqlite"}, "postgresql": {"enabled": False}},
                      {"postgresql": {"enabled": False},
                       "externalPostgresql": {"host": "db", "existingSecret": "db"}}]:
            with self.subTest(value=value):
                result = self.run_helm(value, command="lint", args=["--strict"])
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_package_roundtrip(self):
        with tempfile.TemporaryDirectory() as temp:
            result = subprocess.run([HELM, "package", str(CHART), "--destination", temp],
                                    capture_output=True, text=True, check=False)
            self.assertEqual(result.returncode, 0, result.stderr)
            package = Path(temp) / "janus-0.1.2.tgz"
            self.assertTrue(package.exists())
            self.assertEqual(self.render(), self.render(chart=package))
            if ARTIFACTS:
                shutil.copy2(package, Path(ARTIFACTS) / package.name)


if __name__ == "__main__":
    unittest.main(verbosity=2)
