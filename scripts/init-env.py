#!/usr/bin/env python3
"""Create a private Compose env file once. Never rotate an existing key."""
import argparse
import os
from pathlib import Path
import secrets


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, default=Path("deploy/.env"))
    args = parser.parse_args()
    args.output.parent.mkdir(parents=True, exist_ok=True)
    try:
        fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    except FileExistsError:
        raise SystemExit("Refusing to overwrite an existing env file; retain and back up the original key")
    with os.fdopen(fd, "w") as out:
        out.write("JANUS_ENCRYPTION_KEY=" + secrets.token_hex(32) + "\n")
    print(f"Created {args.output}; back it up securely together with the database.")


if __name__ == "__main__":
    main()
