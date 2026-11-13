#!/usr/bin/env python3
"""Build Linux release archives from this source tree; never publish."""
import datetime
import gzip
import hashlib
import io
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parent.parent
LEGAL = ("LICENSE", "COMMERCIAL-TERMS.md", "NOTICE", "THIRD_PARTY_NOTICES.md")
COMPOSE_FILES = ("compose.yaml", "setup.py", "backup.sh", "restore.sh", "Caddyfile", "README.md", "certs/README.txt")
DIGEST = r"@sha256:[0-9a-f]{64}"


def archive(source, destination, epoch):
    with destination.open("wb") as raw, gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=epoch) as gz:
        with tarfile.open(fileobj=gz, mode="w") as tar:
            for path in sorted(source.rglob("*")):
                if path.is_file():
                    info = tar.gettarinfo(str(path), arcname=str(Path(source.name) / path.relative_to(source)))
                    info.uid = info.gid = 0
                    info.uname = info.gname = ""
                    info.mtime = epoch
                    info.mode = 0o755 if path.name == "janus" or path.suffix in (".py", ".sh") else 0o644
                    with path.open("rb") as data:
                        tar.addfile(info, data)


def chart_digest(root=ROOT):
    """The image index digest the Helm chart is pinned to at release time."""
    values = (root / "charts/janus-helm-chart/values.yaml").read_text()
    m = re.search(r"^image:\n(?:  .*\n)*?  digest: (sha256:[0-9a-f]{64})$", values, re.M)
    if not m:
        raise SystemExit("charts/janus-helm-chart/values.yaml has no image digest")
    return m.group(1)


def check_compose(text, version, digest):
    """The bundle must run the released image and a fixed PostgreSQL, both by digest."""
    want = f"ghcr.io/torvanis/janus:{version}@{digest}"
    if "image: ${JANUS_IMAGE:-" + want + "}" not in text:
        raise SystemExit(f"deploy/compose/compose.yaml must pin {want}")
    if not re.search(r"image: \$\{POSTGRES_IMAGE:-postgres:16-alpine" + DIGEST + r"\}", text):
        raise SystemExit("deploy/compose/compose.yaml must pin postgres:16-alpine by digest")
    if not re.search(r"image: \$\{CADDY_IMAGE:-caddy:2-alpine" + DIGEST + r"\}", text):
        raise SystemExit("deploy/compose/compose.yaml must pin caddy:2-alpine by digest")
    if "build:" in text:
        raise SystemExit("deploy/compose/compose.yaml must not build from source")


def compose_bundle(out, version, epoch, root=ROOT, digest=None):
    """janus-compose-bundle-<version>.tar.gz: Compose + PostgreSQL for one server."""
    source = root / "deploy/compose"
    check_compose((source / "compose.yaml").read_text(), version, digest or chart_digest(root))
    with tempfile.TemporaryDirectory(prefix="janus-compose-") as temp:
        stage = Path(temp) / "janus-compose"
        for name in COMPOSE_FILES:
            (stage / name).parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(source / name, stage / name)
        for name in LEGAL:
            shutil.copyfile(root / name, stage / name)
        artifact = out / f"janus-compose-bundle-{version}.tar.gz"
        archive(stage, artifact, epoch)
    return artifact


def main():
    version = os.environ.get("VERSION", "2026.11.1")
    date = os.environ.get("BUILD_DATE", "2026-11-07")
    if not re.fullmatch(r"[0-9]{4}\.([1-9]|1[0-2])\.[1-9][0-9]*", version):
        raise SystemExit("Invalid YEAR.MONTH.RELEASE_NUMBER")
    epoch = int(datetime.datetime.strptime(date, "%Y-%m-%d").replace(tzinfo=datetime.timezone.utc).timestamp())
    for name in LEGAL:
        if not (ROOT / name).is_file():
            raise SystemExit("Missing required legal file: " + name)
    out = Path(os.environ.get("DIST_DIR", str(ROOT / "dist"))).resolve()
    out.mkdir(parents=True, exist_ok=True)
    if sys.argv[1:] == ["compose-bundle"]:
        # Only the Compose bundle (no Go/web build); for testing the bundle.
        bundle = compose_bundle(out, version, epoch)
        print(f"{hashlib.sha256(bundle.read_bytes()).hexdigest()}  {bundle.name}")
        return
    subprocess.run(["make", "web"], cwd=ROOT, check=True)
    assets = []
    with tempfile.TemporaryDirectory(prefix="janus-release-") as temp:
        for edition, prefix in (("default", "janus"), ("fips", "janus-fips")):
          for arch in ("amd64", "arm64"):
            stage = Path(temp) / f"{prefix}_{version}_linux_{arch}"
            stage.mkdir()
            env = dict(os.environ, GOOS="linux", GOARCH=arch, VERSION=version, BUILD_DATE=date, OUTPUT=str(stage / "janus"),
                       EDITION=edition)
            subprocess.run(["bash", "scripts/build.sh"], cwd=ROOT, env=env, check=True)
            for name in LEGAL:
                shutil.copyfile(ROOT / name, stage / name)
            for path in ROOT.glob("THIRD_PARTY_NOTICES*"):
                if path.is_file():
                    shutil.copyfile(path, stage / path.name)
            shutil.copyfile(ROOT / "scripts/run-local.py", stage / "run-local.py")
            (stage / "deploy").mkdir()
            for name in ("README.md", "kubernetes.yaml"):
                shutil.copyfile(ROOT / "deploy" / name, stage / "deploy" / name)
            (stage / "scripts").mkdir()
            shutil.copyfile(ROOT / "scripts/init-env.py", stage / "scripts/init-env.py")
            (stage / "BUILD").write_text(f"version={version}\nbuild_date={date}\nos=linux\narch={arch}\nedition={edition}\n")
            artifact = out / (stage.name + ".tar.gz")
            archive(stage, artifact, epoch)
            assets.append(artifact)
    assets.append(compose_bundle(out, version, epoch))
    installer = out / "install.py"
    shutil.copyfile(ROOT / "scripts/install.py", installer)
    assets.append(installer)
    checksums = out / "SHA256SUMS"
    checksums.write_text("".join(f"{hashlib.sha256(p.read_bytes()).hexdigest()}  {p.name}\n" for p in assets))
    print("Built release assets (not published):")
    for path in assets + [checksums]:
        print(path)


if __name__ == "__main__":
    main()
