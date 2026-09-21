#!/usr/bin/env python3
"""Checksum-verified Linux archive installer; never starts a service.

Online: python3 install.py --version 2026.9.2
Offline: python3 install.py --archive FILE --checksums SHA256SUMS
Checksums detect corruption, not a compromised release publisher.
"""
import argparse
import hashlib
import os
from pathlib import Path, PurePosixPath
import platform
import re
import shutil
import tarfile
import tempfile
import urllib.request


def verify(archive, checksums):
    matches = []
    for line in checksums.read_text().splitlines():
        parts = line.split()
        if len(parts) == 2 and parts[1].lstrip("*") == archive.name:
            matches.append(parts[0])
    if len(matches) != 1 or not re.fullmatch(r"[0-9a-fA-F]{64}", matches[0]):
        raise ValueError("Expected exactly one valid checksum for " + archive.name)
    digest = hashlib.sha256()
    with archive.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    actual = digest.hexdigest()
    if actual.lower() != matches[0].lower():
        raise ValueError("SHA-256 mismatch: " + archive.name)


def install(archive, checksums, prefix):
    verify(archive, checksums)
    with tempfile.TemporaryDirectory(prefix="janus-install-") as temp:
        staging = Path(temp)
        with tarfile.open(archive, "r:gz") as tar:
            members = tar.getmembers()
            roots = set()
            seen = set()
            total = 0
            for member in members:
                path = PurePosixPath(member.name)
                if path.is_absolute() or ".." in path.parts or not path.parts or "\\" in member.name:
                    raise ValueError("Unsafe archive path")
                if not member.isfile() or str(path) in seen:
                    raise ValueError("Archive must contain unique regular files only")
                seen.add(str(path))
                roots.add(path.parts[0])
                total += member.size
            if len(roots) != 1 or total > 1024 * 1024 * 1024:
                raise ValueError("Invalid archive layout or size")
            for member in members:
                dest = staging / member.name
                dest.parent.mkdir(parents=True, exist_ok=True)
                with tar.extractfile(member) as src, dest.open("wb") as out:
                    shutil.copyfileobj(src, out)
        root = staging / next(iter(roots))
        for name in ("janus", "LICENSE", "COMMERCIAL-TERMS.md", "NOTICE", "THIRD_PARTY_NOTICES.md", "BUILD", "run-local.py"):
            if not (root / name).is_file():
                raise ValueError("Missing archive member: " + name)
        with (root / "janus").open("rb") as binary:
            header = binary.read(20)
        machine = {"x86_64": 62, "aarch64": 183, "arm64": 183}.get(platform.machine())
        if len(header) != 20 or header[:6] != b"\x7fELF\x02\x01" or int.from_bytes(header[18:20], "little") != machine:
            raise ValueError("Expected a Linux ELF executable matching this machine")
        prefix = prefix.expanduser().resolve()
        bindir = prefix / "bin"
        share = prefix / "share/janus"
        bindir.mkdir(parents=True, exist_ok=True)
        share.mkdir(parents=True, exist_ok=True)
        # Replace the executable atomically so running instances are unaffected.
        fd, temporary = tempfile.mkstemp(prefix=".janus-", dir=bindir)
        try:
            with os.fdopen(fd, "wb") as dest, (root / "janus").open("rb") as src:
                shutil.copyfileobj(src, dest)
            os.chmod(temporary, 0o755)
            os.replace(temporary, bindir / "janus")
        finally:
            if os.path.exists(temporary):
                os.unlink(temporary)
        for path in root.iterdir():
            if path.name == "janus":
                continue
            if path.is_dir():
                shutil.copytree(path, share / path.name, dirs_exist_ok=True)
            else:
                shutil.copyfile(path, share / path.name)
        print(f"Installed {bindir / 'janus'}; legal notices and examples: {share}")
        print(f"Start locally: python3 {share / 'run-local.py'} --binary {bindir / 'janus'}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", default="2026.9.2")
    parser.add_argument("--prefix", type=Path, default=Path.home() / ".local")
    parser.add_argument("--archive", type=Path)
    parser.add_argument("--checksums", type=Path)
    args = parser.parse_args()
    if bool(args.archive) != bool(args.checksums):
        parser.error("--archive and --checksums must be provided together")
    if platform.system() != "Linux":
        parser.error("Only Linux binaries are provided")
    if args.archive:
        install(args.archive, args.checksums, args.prefix)
        return
    if not re.fullmatch(r"[0-9]{4}\.([1-9]|1[0-2])\.[1-9][0-9]*", args.version):
        parser.error("Invalid YEAR.MONTH.RELEASE_NUMBER")
    arch = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine())
    if not arch:
        parser.error("Only amd64 and arm64 are supported")
    base = f"https://github.com/Torvanis/janus/releases/download/v{args.version}"
    with tempfile.TemporaryDirectory(prefix="janus-download-") as temp:
        archive = Path(temp) / f"janus_{args.version}_linux_{arch}.tar.gz"
        checksums = Path(temp) / "SHA256SUMS"
        for path in (archive, checksums):
            with urllib.request.urlopen(base + "/" + path.name, timeout=60) as response, path.open("wb") as out:
                shutil.copyfileobj(response, out)
        install(archive, checksums, args.prefix)


if __name__ == "__main__":
    main()
