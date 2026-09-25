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
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parent.parent
LEGAL = ("LICENSE", "COMMERCIAL-TERMS.md", "NOTICE", "THIRD_PARTY_NOTICES.md")


def archive(source, destination, epoch):
    with destination.open("wb") as raw, gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=epoch) as gz:
        with tarfile.open(fileobj=gz, mode="w") as tar:
            for path in sorted(source.rglob("*")):
                if path.is_file():
                    info = tar.gettarinfo(str(path), arcname=str(Path(source.name) / path.relative_to(source)))
                    info.uid = info.gid = 0
                    info.uname = info.gname = ""
                    info.mtime = epoch
                    info.mode = 0o755 if path.name == "janus" or path.suffix == ".py" else 0o644
                    with path.open("rb") as data:
                        tar.addfile(info, data)


def main():
    version = os.environ.get("VERSION", "2026.9.4")
    date = os.environ.get("BUILD_DATE", "2026-09-25")
    if not re.fullmatch(r"[0-9]{4}\.([1-9]|1[0-2])\.[1-9][0-9]*", version):
        raise SystemExit("Invalid YEAR.MONTH.RELEASE_NUMBER")
    epoch = int(datetime.datetime.strptime(date, "%Y-%m-%d").replace(tzinfo=datetime.timezone.utc).timestamp())
    for name in LEGAL:
        if not (ROOT / name).is_file():
            raise SystemExit("Missing required legal file: " + name)
    out = Path(os.environ.get("DIST_DIR", str(ROOT / "dist"))).resolve()
    out.mkdir(parents=True, exist_ok=True)
    subprocess.run(["make", "web"], cwd=ROOT, check=True)
    assets = []
    with tempfile.TemporaryDirectory(prefix="janus-release-") as temp:
        for arch in ("amd64", "arm64"):
            stage = Path(temp) / f"janus_{version}_linux_{arch}"
            stage.mkdir()
            env = dict(os.environ, GOOS="linux", GOARCH=arch, VERSION=version, BUILD_DATE=date, OUTPUT=str(stage / "janus"))
            subprocess.run(["bash", "scripts/build.sh"], cwd=ROOT, env=env, check=True)
            for name in LEGAL:
                shutil.copyfile(ROOT / name, stage / name)
            for path in ROOT.glob("THIRD_PARTY_NOTICES*"):
                if path.is_file():
                    shutil.copyfile(path, stage / path.name)
            shutil.copyfile(ROOT / "scripts/run-local.py", stage / "run-local.py")
            (stage / "deploy").mkdir()
            for name in ("README.md", "compose.yaml", "kubernetes.yaml"):
                shutil.copyfile(ROOT / "deploy" / name, stage / "deploy" / name)
            (stage / "scripts").mkdir()
            shutil.copyfile(ROOT / "scripts/init-env.py", stage / "scripts/init-env.py")
            (stage / "BUILD").write_text(f"version={version}\nbuild_date={date}\nos=linux\narch={arch}\n")
            artifact = out / (stage.name + ".tar.gz")
            archive(stage, artifact, epoch)
            assets.append(artifact)
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
