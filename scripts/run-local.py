#!/usr/bin/env python3
"""Run a loopback-only instance with persistent private storage and key."""
import argparse
import fcntl
import os
from pathlib import Path
import re
import secrets
import signal
import subprocess


def encryption_key(path, database):
    if path.is_symlink():
        raise ValueError("Refusing symlink encryption key")
    if not path.exists():
        if database.exists():
            raise ValueError("Database exists but encryption key is missing; restore the original key from backup")
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "w") as out:
            out.write(secrets.token_hex(32) + "\n")
    if path.stat().st_mode & 0o077:
        raise ValueError("Encryption key must be private (chmod 600)")
    value = path.read_text().strip()
    if not re.fullmatch("[0-9a-f]{64}", value):
        raise ValueError("Invalid encryption key file")
    return value


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, default=Path(__file__).resolve().parent / "janus")
    parser.add_argument("--data-dir", type=Path, default=Path(os.environ.get("XDG_DATA_HOME", str(Path.home() / ".local/share"))) / "janus")
    parser.add_argument("--port", type=int, default=8080)
    args = parser.parse_args()
    if not 1024 <= args.port <= 65535:
        parser.error("Use an unprivileged port between 1024 and 65535")
    binary = args.binary.expanduser().resolve(strict=True)
    data = args.data_dir.expanduser().resolve()
    os.umask(0o077)
    data.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.chmod(data, 0o700)
    with (data / "run.lock").open("w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        database = data / "janus.db"
        key = encryption_key(data / "encryption.key", database)
        # Ambient auth/DB settings cannot turn the local launcher into an unsafe instance.
        env = {k: v for k, v in os.environ.items() if not k.startswith("JANUS_")}
        env.update(JANUS_ENCRYPTION_KEY=key, JANUS_DATABASE_URL="sqlite://" + str(database),
                   JANUS_LISTEN_ADDR=f"127.0.0.1:{args.port}", JANUS_PUBLIC_URL=f"http://127.0.0.1:{args.port}",
                   JANUS_DEV_AUTH="false", JANUS_ENV="production", JANUS_COOKIE_SECURE="false")
        print(f"Open http://127.0.0.1:{args.port} and create the first administrator. Back up {data} securely.", flush=True)
        child = subprocess.Popen([str(binary)], env=env)
        def forward(signum, _frame):
            if child.poll() is None:
                child.send_signal(signum)
        signal.signal(signal.SIGTERM, forward)
        signal.signal(signal.SIGINT, forward)
        raise SystemExit(child.wait())


if __name__ == "__main__":
    main()
