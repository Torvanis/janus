#!/usr/bin/env python3
"""Set up Janus with PostgreSQL under Docker Compose on this server.

  python3 setup.py --admin-email you@example.com [--hostname ai.example.com]
  python3 setup.py --skip-admin          # create the administrator in the browser
  python3 setup.py open                  # after --skip-admin: publish the port
  python3 setup.py cert --cert fullchain.pem --key privkey.pem
  python3 setup.py cert --self-signed

The first run writes .env once (encryption key, database password, hostname;
mode 0600) and never overwrites it. Janus first starts reachable from this
machine only; the administrator is created over loopback before the HTTPS
port is published, so nobody else can claim the server first.
"""
import argparse
import getpass
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import socket
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.request

HERE = Path(__file__).resolve().parent
ENV_FILE = HERE / ".env"
CERTS = HERE / "certs"
CONTAINER_UID = 65532
CADDY_JANUS_PORT = 8443
DEFAULT_SUBNET = "172.31.250.0/24"


class SetupError(Exception):
    pass


# --------------------------------------------------------------------------
# .env handling: created once, secrets never rewritten
# --------------------------------------------------------------------------

def read_env(path=ENV_FILE):
    env = {}
    for line in path.read_text().splitlines():
        line = line.strip()
        if line and not line.startswith("#") and "=" in line:
            key, value = line.split("=", 1)
            env[key.strip()] = value.strip()
    return env


def new_env(hostname, https_port, caddy=False, postgres_image=None, names=()):
    """The initial .env content. Secrets are hex so they never need escaping in the database URL."""
    port = CADDY_JANUS_PORT if caddy else https_port
    public = "https://" + (f"[{hostname}]" if ":" in hostname else hostname)
    if not caddy and https_port != 443:
        public += f":{https_port}"
    lines = [
        "# Written once by setup.py. Back it up together with the database: the",
        "# database is unreadable without JANUS_ENCRYPTION_KEY. Never regenerate it.",
        "JANUS_ENCRYPTION_KEY=" + secrets.token_hex(32),
        "POSTGRES_PASSWORD=" + secrets.token_hex(32),
        "JANUS_HOSTNAME=" + hostname,
        "JANUS_PUBLIC_URL=" + public,
        "JANUS_TLS_NAMES=" + ",".join(dict.fromkeys([hostname, "localhost", "127.0.0.1", *names])),
        f"JANUS_HTTPS_PORT={port}",
        "JANUS_BIND=127.0.0.1",
    ]
    if caddy:
        # COMPOSE_PROFILES=caddy is added only once the administrator exists,
        # so Caddy never forwards the internet to an unclaimed instance.
        lines += ["JANUS_CADDY=true", "JANUS_TRUSTED_PROXIES=" + DEFAULT_SUBNET]
    if postgres_image:
        lines.append("POSTGRES_IMAGE=" + postgres_image)
    return "\n".join(lines) + "\n"


def create_env_file(content, path=ENV_FILE):
    """Create .env with mode 0600; refuse to touch an existing one."""
    try:
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    except FileExistsError:
        raise SetupError(f"{path} already exists; it is never overwritten") from None
    with os.fdopen(fd, "w") as out:
        out.write(content)


def set_env_values(values, path=ENV_FILE):
    """Change non-secret settings in place, keeping every other line and the file mode."""
    for key in values:
        if key in ("JANUS_ENCRYPTION_KEY", "POSTGRES_PASSWORD"):
            raise SetupError(f"refusing to change {key}")
    lines, seen = [], set()
    for line in path.read_text().splitlines():
        key = line.split("=", 1)[0].strip() if "=" in line and not line.lstrip().startswith("#") else None
        if key in values:
            seen.add(key)
            if values[key] is None:
                continue
            line = f"{key}={values[key]}"
        lines.append(line)
    lines += [f"{k}={v}" for k, v in values.items() if k not in seen and v is not None]
    tmp = path.with_name(path.name + ".tmp")
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as out:
        out.write("\n".join(lines) + "\n")
    os.replace(tmp, path)


# --------------------------------------------------------------------------
# Host facts
# --------------------------------------------------------------------------

def is_ip(name):
    try:
        ipaddress.ip_address(name.strip("[]"))
        return True
    except ValueError:
        return False


def valid_hostname(name):
    if not name or len(name) > 253:
        return False
    if is_ip(name):
        return True
    return all(re.fullmatch(r"(?!-)[A-Za-z0-9-]{1,63}(?<!-)", p) for p in name.rstrip(".").split("."))


def local_ips():
    """Global addresses of this host's own interfaces, without container bridges."""
    ips = []
    try:
        out = subprocess.run(["ip", "-o", "addr", "show", "scope", "global"], capture_output=True, text=True,
                             check=False).stdout
    except OSError:
        out = ""
    for line in out.splitlines():
        parts = line.split()
        if len(parts) < 4 or parts[1].startswith(("docker", "br-", "veth", "cni", "podman", "virbr")):
            continue
        ip = parts[3].split("/")[0]
        try:
            addr = ipaddress.ip_address(ip)
        except ValueError:
            continue
        if not (addr.is_loopback or addr.is_link_local) and ip not in ips:
            ips.append(ip)
    return ips


# --------------------------------------------------------------------------
# Docker Compose and Janus over loopback
# --------------------------------------------------------------------------

def compose(*args, capture=False):
    cmd = ["docker", "compose", *args]
    try:
        result = subprocess.run(cmd, cwd=HERE, capture_output=capture, text=True)
    except FileNotFoundError:
        raise SetupError("docker was not found. Install Docker Engine with the Compose plugin first.") from None
    if result.returncode != 0:
        detail = (result.stderr or "").strip() if capture else ""
        hint = ""
        if "permission denied" in detail.lower() and "docker" in detail.lower():
            hint = "\nRun it with sudo, or add your user to the docker group and log in again."
        raise SetupError(f"`{' '.join(cmd)}` failed{': ' + detail if detail else ''}{hint}")
    return result.stdout if capture else ""


def check_docker():
    try:
        version = subprocess.run(["docker", "compose", "version", "--short"], capture_output=True, text=True)
    except FileNotFoundError:
        raise SetupError("docker was not found. Install Docker Engine with the Compose plugin first.") from None
    if version.returncode != 0:
        raise SetupError("`docker compose` is not available. Install the Docker Compose v2 plugin.")
    info = subprocess.run(["docker", "info", "--format", "{{.ServerVersion}}"], capture_output=True, text=True)
    if info.returncode != 0:
        msg = info.stderr.strip()
        if "permission denied" in msg.lower():
            msg += "\nRun it with sudo, or add your user to the docker group and log in again."
        raise SetupError("Docker is not reachable: " + msg)


def loopback_context():
    # Only used for our own listener on 127.0.0.1, whose self-signed
    # certificate would prove nothing if verified.
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    return ctx


def local_get(port, path, timeout=5):
    with urllib.request.urlopen(f"https://127.0.0.1:{port}{path}", timeout=timeout, context=loopback_context()) as r:
        return r.status, r.read()


def wait_ready(port, timeout=240):
    deadline = time.time() + timeout
    last = ""
    while time.time() < deadline:
        try:
            status, _ = local_get(port, "/readyz")
            if status == 200:
                return
        except Exception as err:  # noqa: BLE001 - reported below
            last = str(err)
        time.sleep(2)
    logs = subprocess.run(["docker", "compose", "logs", "--tail", "30", "janus"], cwd=HERE, capture_output=True,
                          text=True).stdout
    raise SetupError(f"Janus did not become ready on 127.0.0.1:{port}: {last}\n{logs}")


def needs_setup(port):
    try:
        _, body = local_get(port, "/auth/local/status")
        return bool(json.loads(body).get("needs_setup"))
    except Exception:  # noqa: BLE001 - an IdP-configured gateway has no local setup
        return False


def create_admin(port, email, name, password):
    body = json.dumps({"email": email, "name": name, "password": password}).encode()
    req = urllib.request.Request(f"https://127.0.0.1:{port}/auth/local/setup", data=body, method="POST",
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=15, context=loopback_context()) as r:
            return r.status < 400
    except urllib.error.HTTPError as err:
        detail = err.read().decode(errors="replace")
        try:
            detail = json.loads(detail)["error"]["message"]
        except Exception:  # noqa: BLE001
            pass
        raise SetupError(f"Could not create the administrator: {detail}") from None


def prompt_admin(args):
    email = args.admin_email
    interactive = sys.stdin.isatty()
    if not email and interactive:
        email = input("Administrator email: ").strip()
    if not email:
        return None
    if not re.fullmatch(r"[^@\s]+@[^@\s]+\.[^@\s]+", email):
        raise SetupError("That doesn't look like an email address: " + email)
    name = args.admin_name or email.split("@")[0]
    password, generated = None, False
    if interactive and not args.admin_email:
        while True:
            password = getpass.getpass("Administrator password (12+ characters, blank to generate one): ")
            if not password:
                break
            if len(password) < 12:
                print("Use at least 12 characters.")
                continue
            if getpass.getpass("Repeat password: ") == password:
                break
            print("The passwords didn't match.")
    if not password:
        password = "-".join(secrets.token_urlsafe(6) for _ in range(4))
        generated = True
    return email, name, password, generated


def https_port(env):
    return int(env.get("JANUS_HTTPS_PORT", "443"))


def uses_caddy(env):
    return env.get("JANUS_CADDY") == "true"


def publish(env):
    """Listen beyond loopback. With Caddy, Janus stays on loopback and Caddy owns 80/443."""
    if uses_caddy(env):
        set_env_values({"COMPOSE_PROFILES": "caddy"})
        compose("pull", "--quiet", "caddy")
        compose("up", "-d")
        return read_env()
    set_env_values({"JANUS_BIND": "0.0.0.0"})
    compose("up", "-d")
    env = read_env()
    wait_ready(https_port(env))
    return env


# --------------------------------------------------------------------------
# Commands
# --------------------------------------------------------------------------

def cmd_setup(args):
    check_docker()
    admin = None
    first = not ENV_FILE.exists()
    if first:
        if not args.skip_admin:
            admin = prompt_admin(args)
            if admin is None:
                raise SetupError("An administrator email is needed so nobody else can claim this server first.\n"
                                 "  python3 setup.py --admin-email you@example.com\n"
                                 "or add --skip-admin to create the administrator in the browser while Janus is "
                                 "still only reachable from this machine.")
        hostname = args.hostname or socket.getfqdn()
        if not valid_hostname(hostname):
            raise SetupError(f"Invalid hostname {hostname!r}; pass --hostname ai.example.com")
        if args.caddy and (is_ip(hostname) or "." not in hostname):
            raise SetupError("--caddy needs the public DNS name of this server: --hostname ai.example.com")
        create_env_file(new_env(hostname, args.https_port, caddy=args.caddy, postgres_image=args.postgres_image,
                                names=local_ips()))
        print(f"Created {ENV_FILE.name} (encryption key and database password; back it up).")
    else:
        print(f"Using the existing {ENV_FILE.name}; it is never overwritten.")
        if args.admin_email and not args.skip_admin:
            admin = prompt_admin(args)
    env = read_env()
    port = https_port(env)

    print("Pulling images ...")
    compose("pull", "--quiet")
    print("Starting PostgreSQL and Janus (reachable from this machine only) ...")
    compose("up", "-d")
    wait_ready(port)

    if needs_setup(port):
        if admin:
            create_admin(port, admin[0], admin[1], admin[2])
            print(f"Created the administrator {admin[0]}.")
        else:
            local = "https://127.0.0.1" + ("" if port == 443 else f":{port}")
            print(f"\nJanus is running, but only reachable from this machine until you create the first "
                  "administrator.\n"
                  f"  From your computer: ssh -L {port}:127.0.0.1:{port} <this-server>\n"
                  f"  Then open {local}, create the administrator, and run:\n"
                  "    python3 setup.py open")
            return
    elif admin:
        print("An administrator already exists; nothing to create.")
        admin = None

    env = publish(env)
    summary(env, admin)


def cmd_open(_args):
    check_docker()
    env = read_env()
    port = https_port(env)
    wait_ready(port, timeout=60)
    if needs_setup(port):
        raise SetupError("Create the first administrator first (see the setup output), then run this again.")
    env = publish(env)
    print(f"Janus is now reachable at {env['JANUS_PUBLIC_URL']}")


def check_cert_pair(cert, key):
    if not shutil.which("openssl"):
        return
    def pub(cmd):
        out = subprocess.run(cmd, capture_output=True, text=True)
        if out.returncode != 0:
            raise SetupError(out.stderr.strip() or "openssl failed")
        return out.stdout
    if pub(["openssl", "x509", "-in", str(cert), "-noout", "-pubkey"]) != pub(
            ["openssl", "pkey", "-in", str(key), "-pubout", "-passin", "pass:"]):
        raise SetupError("The key does not match the certificate.")
    if subprocess.run(["openssl", "x509", "-in", str(cert), "-noout", "-checkend", "0"],
                      capture_output=True).returncode != 0:
        raise SetupError("The certificate has expired.")


def cmd_cert(args):
    check_docker()
    env = read_env()
    if args.self_signed:
        set_env_values({"JANUS_TLS_SELF_SIGNED": "true", "JANUS_TLS_CERT_PATH": None, "JANUS_TLS_KEY_PATH": None})
    else:
        if not (args.cert and args.key):
            raise SetupError("Pass --cert fullchain.pem --key privkey.pem, or --self-signed.")
        check_cert_pair(args.cert, args.key)
        CERTS.mkdir(exist_ok=True)
        os.chmod(CERTS, 0o755)
        # Key first, certificate last: Janus reloads when the certificate file
        # changes, and the key must already match by then.
        for src, name, mode in ((args.key, "privkey.pem", 0o600), (args.cert, "fullchain.pem", 0o644)):
            tmp = CERTS / (name + ".new")
            tmp.unlink(missing_ok=True)
            shutil.copyfile(src, tmp)
            os.chmod(tmp, mode)
            if name == "privkey.pem":
                # The janus container runs as uid 65532 and must read the key.
                if os.geteuid() == 0:
                    os.chown(tmp, CONTAINER_UID, CONTAINER_UID)
                else:
                    compose("run", "--rm", "--no-deps", "-T", "--entrypoint", "chown", "-v", f"{CERTS}:/fix",
                            "postgres", f"{CONTAINER_UID}:{CONTAINER_UID}", "/fix/privkey.pem.new", capture=True)
            os.replace(tmp, CERTS / name)
        set_env_values({"JANUS_TLS_SELF_SIGNED": "false", "JANUS_TLS_CERT_PATH": "/certs/fullchain.pem",
                        "JANUS_TLS_KEY_PATH": "/certs/privkey.pem"})
    compose("up", "-d", "janus")
    wait_ready(https_port(env))
    print("Janus now serves " + ("its self-signed certificate." if args.self_signed else
                                 "your certificate. To renew, run this again with the new files; Janus picks "
                                 "them up within about 10 seconds, without a restart."))


def summary(env, admin):
    port = https_port(env)
    caddy = uses_caddy(env)
    print()
    print("Janus is running with PostgreSQL.")
    print(f"  Open:        {env['JANUS_PUBLIC_URL']}")
    if not caddy:
        ips = [ip for ip in local_ips() if ip != env.get("JANUS_HOSTNAME")]
        if ips:
            host = f"[{ips[0]}]" if ":" in ips[0] else ips[0]
            print(f"               or https://{host}" + ("" if port == 443 else f":{port}") + " until DNS points here")
    if admin:
        print(f"  Sign in as:  {admin[0]}")
        if admin[3]:
            print(f"  Password:    {admin[2]}")
            print("               (shown only now; change it under your profile after signing in)")
    if caddy:
        print("  Certificate: Let's Encrypt via Caddy (needs public DNS and ports 80/443 reachable)")
    elif env.get("JANUS_TLS_SELF_SIGNED", "true") != "false":
        print("  Certificate: self-signed, so browsers will warn until you install your own:")
        print("                 python3 setup.py cert --cert fullchain.pem --key privkey.pem")
    print(f"  Config:      {ENV_FILE}  (back it up: it holds the encryption key)")
    print("  Manage:      docker compose ps | logs janus | ./backup.sh")


def main(argv=None):
    # Keep our lines in order with docker compose's own output, even when piped.
    getattr(sys.stdout, "reconfigure", lambda **_: None)(line_buffering=True)
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--admin-email", help="email of the first administrator (password generated and shown once)")
    parser.add_argument("--admin-name", help="display name of the first administrator")
    parser.add_argument("--hostname", help="DNS name clients use (default: this host's FQDN)")
    parser.add_argument("--https-port", type=int, default=443, help="HTTPS port to publish (default 443)")
    parser.add_argument("--skip-admin", action="store_true",
                        help="create the administrator in the browser; Janus stays loopback-only until `open`")
    parser.add_argument("--caddy", action="store_true",
                        help="put Caddy in front for Let's Encrypt and a port 80 redirect (needs public DNS)")
    parser.add_argument("--postgres-image", help=argparse.SUPPRESS)
    sub = parser.add_subparsers(dest="command")
    sub.add_parser("open", help="after --skip-admin, publish the HTTPS port")
    cert = sub.add_parser("cert", help="serve your own certificate, or go back to self-signed")
    cert.add_argument("--cert", type=Path, help="certificate chain (PEM, leaf first)")
    cert.add_argument("--key", type=Path, help="unencrypted private key (PEM)")
    cert.add_argument("--self-signed", action="store_true", help="go back to the self-signed certificate")
    args = parser.parse_args(argv)
    if not 1 <= args.https_port <= 65535:
        parser.error("--https-port must be 1-65535")
    try:
        if args.command == "open":
            cmd_open(args)
        elif args.command == "cert":
            cmd_cert(args)
        else:
            cmd_setup(args)
    except SetupError as err:
        print(f"setup.py: {err}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        return 130
    return 0


if __name__ == "__main__":
    sys.exit(main())
