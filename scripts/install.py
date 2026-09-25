#!/usr/bin/env python3
"""Janus Edge installer and server control tool.

Server install (systemd, HTTPS on 443, HTTP on 80 redirects to HTTPS):
    sudo python3 install.py --admin-email you@example.com [--hostname ai.example.com]

Bring your own certificate, or use Let's Encrypt, after installing:
    sudo janus-ctl cert install --cert fullchain.pem --key privkey.pem
    sudo janus-ctl cert acme --domain ai.example.com --email you@example.com

Other commands: janus-ctl status | upgrade [--version V] | cert self-signed | uninstall [--purge]
No-sudo trial in your home directory (loopback only, foreground):
    python3 install.py --user

Every download is checked against the release SHA256SUMS. Checksums detect
corruption, not a compromised release publisher.
"""
import argparse
import datetime
import getpass
import grp
import hashlib
import http.server
import ipaddress
import json
import os
from pathlib import Path, PurePosixPath
import platform
import pwd
import re
import secrets
import shutil
import socket
import socketserver
import ssl
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.request

DEFAULT_VERSION = "2026.9.4"
RELEASES = "https://github.com/Torvanis/janus/releases/download"

# Server layout (FHS). Configuration and data survive upgrade and uninstall.
BIN = Path("/usr/local/bin/janus")
CTL = Path("/usr/local/sbin/janus-ctl")
# RHEL's sudo secure_path omits /usr/local/*, so `sudo janus-ctl` needs a link in /usr/bin.
CTL_LINK = Path("/usr/bin/janus-ctl")
SHARE = Path("/usr/local/share/janus")
LIBEXEC = Path("/usr/local/lib/janus")
ETC = Path("/etc/janus")
ENV_FILE = ETC / "janus.env"
TLS_DIR = ETC / "tls"
CERT = TLS_DIR / "fullchain.pem"
KEY = TLS_DIR / "privkey.pem"
ACME_DIR = ETC / "acme"
ACME_CONF = ETC / "acme.json"
DATA = Path("/var/lib/janus")
WEBROOT = DATA / "acme-webroot"
BACKUPS = DATA / "backups"
UNIT_DIR = Path("/etc/systemd/system")
SERVICE_USER = "janus"
UNITS = ("janus.service", "janus-redirect.service", "janus-acme-renew.service", "janus-acme-renew.timer")

# lego (ACME client) is only downloaded when `cert acme` is used.
LEGO_VERSION = "5.5.2"
LEGO_SHA256 = {
    "amd64": "2a35505089e7772c92e1e9ac144df91151ef2eca8568630db0ff91fca06d9bef",
    "arm64": "15b14ec2ab14fde69cc8396eb0204c5ce4327e31a486953225a6059b26db3e8c",
}


class InstallError(Exception):
    """A problem the operator can act on; printed without a traceback."""


# --------------------------------------------------------------------------
# Download and archive verification (shared by --user and server installs)
# --------------------------------------------------------------------------

def arch():
    a = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine())
    if not a:
        raise InstallError("Only amd64 and arm64 Linux are supported")
    return a


def sha256_file(path):
    digest = hashlib.sha256()
    with Path(path).open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def verify(archive, checksums):
    matches = []
    for line in checksums.read_text().splitlines():
        parts = line.split()
        if len(parts) == 2 and parts[1].lstrip("*") == archive.name:
            matches.append(parts[0])
    if len(matches) != 1 or not re.fullmatch(r"[0-9a-fA-F]{64}", matches[0]):
        raise ValueError("Expected exactly one valid checksum for " + archive.name)
    if sha256_file(archive).lower() != matches[0].lower():
        raise ValueError("SHA-256 mismatch: " + archive.name)


def extract(archive, checksums, staging):
    """Verify and unpack a release archive; return its root directory."""
    verify(archive, checksums)
    staging.mkdir(parents=True, exist_ok=True)
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
    for name in ("janus", "LICENSE", "COMMERCIAL-TERMS.md", "NOTICE", "THIRD_PARTY_NOTICES.md", "BUILD"):
        if not (root / name).is_file():
            raise ValueError("Missing archive member: " + name)
    with (root / "janus").open("rb") as binary:
        header = binary.read(20)
    machine = {"x86_64": 62, "aarch64": 183, "arm64": 183}.get(platform.machine())
    if len(header) != 20 or header[:6] != b"\x7fELF\x02\x01" or int.from_bytes(header[18:20], "little") != machine:
        raise ValueError("Expected a Linux ELF executable matching this machine")
    return root


def fetch(url, path):
    try:
        with urllib.request.urlopen(url, timeout=120) as response, Path(path).open("wb") as out:
            shutil.copyfileobj(response, out)
    except urllib.error.HTTPError as err:
        raise InstallError(f"Download failed ({err.code}): {url}") from None
    except urllib.error.URLError as err:
        raise InstallError(f"Download failed: {url}: {err.reason}") from None


def download_release(version, temp):
    if not re.fullmatch(r"[0-9]{4}\.([1-9]|1[0-2])\.[1-9][0-9]*", version):
        raise InstallError("Invalid version; expected YEAR.MONTH.RELEASE_NUMBER, for example " + DEFAULT_VERSION)
    base = f"{RELEASES}/v{version}"
    archive = temp / f"janus_{version}_linux_{arch()}.tar.gz"
    checksums = temp / "SHA256SUMS"
    for path in (archive, checksums):
        fetch(base + "/" + path.name, path)
    return archive, checksums


def replace_file(source, dest, mode):
    """Atomically replace dest so a running copy is never half-written."""
    dest.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix="." + dest.name + "-", dir=dest.parent)
    try:
        with os.fdopen(fd, "wb") as out, open(source, "rb") as src:
            shutil.copyfileobj(src, out)
        os.chmod(temporary, mode)
        os.replace(temporary, dest)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def copy_share(root, share):
    share.mkdir(parents=True, exist_ok=True)
    for path in root.iterdir():
        if path.name == "janus":
            continue
        if path.is_dir():
            shutil.copytree(path, share / path.name, dirs_exist_ok=True)
        else:
            shutil.copyfile(path, share / path.name)


def install(archive, checksums, prefix):
    """No-sudo install into a prefix (default ~/.local): the laptop trial."""
    with tempfile.TemporaryDirectory(prefix="janus-install-") as temp:
        root = extract(archive, checksums, Path(temp))
        if not (root / "run-local.py").is_file():
            raise ValueError("Missing archive member: run-local.py")
        prefix = prefix.expanduser().resolve()
        bindir = prefix / "bin"
        share = prefix / "share/janus"
        replace_file(root / "janus", bindir / "janus", 0o755)
        copy_share(root, share)
        print(f"Installed {bindir / 'janus'}; legal notices and examples: {share}")
        print(f"Start locally: python3 {share / 'run-local.py'} --binary {bindir / 'janus'}")


# --------------------------------------------------------------------------
# Small helpers
# --------------------------------------------------------------------------

def run(cmd, check=True, capture=False, env=None):
    result = subprocess.run(cmd, check=False, text=True, env=env,
                            stdout=subprocess.PIPE if capture else None,
                            stderr=subprocess.PIPE if capture else None)
    if check and result.returncode != 0:
        detail = (result.stderr or result.stdout or "").strip() if capture else ""
        raise InstallError(f"Command failed ({result.returncode}): {' '.join(cmd)}" + (f"\n{detail}" if detail else ""))
    return result


def need_root(what):
    if os.geteuid() != 0:
        raise InstallError(f"{what} needs root. Run it with sudo.")


def need_systemd():
    if not Path("/run/systemd/system").is_dir():
        raise InstallError("This server install needs systemd. For a no-sudo trial use: python3 install.py --user")


def need_openssl():
    if not shutil.which("openssl"):
        raise InstallError("openssl is required. Install it (apt install openssl / dnf install openssl) and retry.")


def read_env():
    values = {}
    if ENV_FILE.exists():
        for line in ENV_FILE.read_text().splitlines():
            line = line.strip()
            if line and not line.startswith("#") and "=" in line:
                k, v = line.split("=", 1)
                values[k.strip()] = v.strip()
    return values


def write_env(values):
    """Rewrite janus.env with keys in a stable order; mode 0640 root:janus."""
    order = ["JANUS_ENCRYPTION_KEY", "JANUS_DATABASE_URL", "JANUS_PUBLIC_URL", "JANUS_LISTEN_ADDR",
             "JANUS_TLS_CERT_PATH", "JANUS_TLS_KEY_PATH", "JANUS_ENV", "JANUS_TROUBLESHOOT_DIR"]
    lines = ["# Janus Edge configuration. Keep JANUS_ENCRYPTION_KEY with your backups:",
             "# a database cannot be read without it. Apply edits with: sudo systemctl restart janus"]
    for k in order + sorted(k for k in values if k not in order):
        if k in values:
            if "\n" in values[k]:
                raise InstallError(f"Invalid value for {k}")
            lines.append(f"{k}={values[k]}")
    fd, temporary = tempfile.mkstemp(prefix=".janus.env-", dir=ETC)
    with os.fdopen(fd, "w") as out:
        out.write("\n".join(lines) + "\n")
    os.chmod(temporary, 0o640)
    os.chown(temporary, 0, service_gid())
    os.replace(temporary, ENV_FILE)


def service_uid():
    return pwd.getpwnam(SERVICE_USER).pw_uid


def service_gid():
    return grp.getgrnam(SERVICE_USER).gr_gid


def https_port_of(env):
    addr = env.get("JANUS_LISTEN_ADDR", ":443")
    try:
        return int(addr.rsplit(":", 1)[1])
    except (IndexError, ValueError):
        return 443


def host_from_url(url):
    m = re.match(r"https?://(\[[^\]]+\]|[^/:]+)", url or "")
    return m.group(1) if m else ""


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
    ips = set()
    out = run(["hostname", "-I"], check=False, capture=True).stdout if shutil.which("hostname") else ""
    ips.update((out or "").split())
    if not ips:
        try:
            with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as s:
                s.connect(("192.0.2.1", 9))
                ips.add(s.getsockname()[0])
        except OSError:
            pass
    good = []
    for ip in sorted(ips):
        try:
            addr = ipaddress.ip_address(ip)
        except ValueError:
            continue
        if not (addr.is_loopback or addr.is_link_local):
            good.append(ip)
    return good


def version_of(path):
    build = Path(path) / "BUILD"
    if build.exists():
        m = re.search(r"\d{4}\.\d+\.\d+", build.read_text())
        if m:
            return m.group(0)
    return "unknown"


# --------------------------------------------------------------------------
# Certificates
# --------------------------------------------------------------------------

def cert_info(path):
    """Subject, issuer, names, expiry, and whether the cert is self-signed."""
    out = run(["openssl", "x509", "-in", str(path), "-noout", "-subject", "-issuer", "-enddate", "-text"],
              capture=True).stdout
    subject = re.search(r"^subject=\s*(.*)$", out, re.M)
    issuer = re.search(r"^issuer=\s*(.*)$", out, re.M)
    end = re.search(r"^notAfter=(.*)$", out, re.M)
    san_block = re.search(r"X509v3 Subject Alternative Name:.*?\n\s*(.*)\n", out)
    names = re.findall(r"(?:DNS|IP Address):([^,\s]+)", san_block.group(1)) if san_block else []
    return {
        "subject": subject.group(1).strip() if subject else "",
        "issuer": issuer.group(1).strip() if issuer else "",
        "not_after": end.group(1).strip() if end else "",
        "names": names,
        "self_signed": bool(subject and issuer and subject.group(1).strip() == issuer.group(1).strip()),
    }


def name_matches(host, names):
    host = host.lower().rstrip(".")
    for n in names:
        n = n.lower().rstrip(".")
        if n == host or (n.startswith("*.") and host.count(".") == n.count(".") and host.endswith(n[1:])):
            return True
    return False


def check_cert_pair(cert, key, hostname=None):
    """Refuse a certificate that won't work: unreadable, expired, or not matching the key."""
    need_openssl()
    for path, what in ((cert, "certificate"), (key, "private key")):
        if not Path(path).is_file():
            raise InstallError(f"{what} not found: {path}")
    if "BEGIN CERTIFICATE" not in Path(cert).read_text(errors="replace"):
        raise InstallError(f"{cert} is not a PEM certificate (expected '-----BEGIN CERTIFICATE-----')")
    key_text = Path(key).read_text(errors="replace")
    if "ENCRYPTED" in key_text:
        raise InstallError(f"{key} is password-protected; Janus needs an unencrypted key file")
    if "PRIVATE KEY" not in key_text:
        raise InstallError(f"{key} is not a PEM private key")
    cert_pub = run(["openssl", "x509", "-in", str(cert), "-noout", "-pubkey"], capture=True).stdout
    key_pub = run(["openssl", "pkey", "-in", str(key), "-pubout"], capture=True).stdout
    if cert_pub.strip() != key_pub.strip():
        raise InstallError("The private key does not match the certificate. Check you have the right pair.")
    if run(["openssl", "x509", "-in", str(cert), "-noout", "-checkend", "0"], check=False, capture=True).returncode:
        raise InstallError("The certificate has expired.")
    info = cert_info(cert)
    if hostname and info["names"] and not name_matches(hostname, info["names"]):
        print(f"Warning: {hostname} is not one of the certificate's names ({', '.join(info['names'])}). "
              "Browsers will warn unless people use one of those names.", file=sys.stderr)
    return info


def install_cert_files(cert, key):
    TLS_DIR.mkdir(parents=True, exist_ok=True)
    os.chmod(TLS_DIR, 0o750)
    os.chown(TLS_DIR, 0, service_gid())
    for src, dest, mode in ((cert, CERT, 0o644), (key, KEY, 0o640)):
        fd, temporary = tempfile.mkstemp(prefix="." + dest.name + "-", dir=TLS_DIR)
        with os.fdopen(fd, "wb") as out, open(src, "rb") as data:
            shutil.copyfileobj(data, out)
        os.chmod(temporary, mode)
        os.chown(temporary, 0, service_gid())
        os.replace(temporary, dest)


def make_self_signed(hostname):
    need_openssl()
    names = []
    for n in [hostname, socket.gethostname(), "localhost"] + local_ips() + ["127.0.0.1"]:
        if n and n not in names and valid_hostname(n):
            names.append(n)
    san = ",".join(("IP:" if is_ip(n) else "DNS:") + n for n in names)
    with tempfile.TemporaryDirectory(prefix="janus-cert-") as temp:
        c, k = Path(temp) / "cert.pem", Path(temp) / "key.pem"
        run(["openssl", "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1",
             "-nodes", "-days", "825", "-subj", "/CN=" + hostname[:64], "-addext", "subjectAltName=" + san,
             "-addext", "basicConstraints=critical,CA:FALSE", "-addext", "extendedKeyUsage=serverAuth",
             "-keyout", str(k), "-out", str(c)], capture=True)
        install_cert_files(c, k)
    return names


# --------------------------------------------------------------------------
# systemd units
# --------------------------------------------------------------------------

HARDENING = """NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
LockPersonality=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE"""


def unit_files(https_port):
    return {
        "janus.service": f"""[Unit]
Description=Janus Edge AI gateway (HTTPS)
Documentation=https://janusedge.com/docs/install
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User={SERVICE_USER}
Group={SERVICE_USER}
EnvironmentFile={ENV_FILE}
ExecStart={BIN}
WorkingDirectory={DATA}
Restart=on-failure
RestartSec=3
TimeoutStopSec=90
LimitNOFILE=65536
ReadWritePaths={DATA}
{HARDENING}

[Install]
WantedBy=multi-user.target
""",
        "janus-redirect.service": f"""[Unit]
Description=Janus Edge HTTP to HTTPS redirect (port 80)
Documentation=https://janusedge.com/docs/install
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User={SERVICE_USER}
Group={SERVICE_USER}
ExecStart=/usr/bin/python3 {CTL} redirect --port 80 --https-port {https_port} --webroot {WEBROOT}
Restart=on-failure
RestartSec=3
{HARDENING}

[Install]
WantedBy=multi-user.target
""",
        "janus-acme-renew.service": f"""[Unit]
Description=Renew the Janus Edge Let's Encrypt certificate
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/bin/python3 {CTL} cert renew
""",
        "janus-acme-renew.timer": """[Unit]
Description=Check the Janus Edge certificate for renewal twice a day

[Timer]
OnCalendar=*-*-* 03,15:17:00
RandomizedDelaySec=45min
Persistent=true

[Install]
WantedBy=timers.target
""",
    }


def write_units(https_port):
    changed = False
    for name, text in unit_files(https_port).items():
        path = UNIT_DIR / name
        if not path.exists() or path.read_text() != text:
            path.write_text(text)
            os.chmod(path, 0o644)
            changed = True
    if changed:
        run(["systemctl", "daemon-reload"])


def systemctl(*args, check=True):
    return run(["systemctl", *args], check=check, capture=True)


# --------------------------------------------------------------------------
# Readiness and first administrator
# --------------------------------------------------------------------------

def loopback_context():
    # Only ever used for our own listener on 127.0.0.1, whose certificate names
    # the public host (or is self-signed), so verification would prove nothing.
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    return ctx


def local_get(port, path, timeout=5):
    with urllib.request.urlopen(f"https://127.0.0.1:{port}{path}", timeout=timeout, context=loopback_context()) as r:
        return r.status, r.read()


def wait_ready(port, timeout=60):
    deadline = time.time() + timeout
    last = ""
    while time.time() < deadline:
        try:
            status, _ = local_get(port, "/readyz")
            if status == 200:
                return
        except Exception as err:  # noqa: BLE001 - reported below
            last = str(err)
        time.sleep(1)
    logs = run(["journalctl", "-u", "janus", "-n", "25", "--no-pager"], check=False, capture=True).stdout
    raise InstallError(f"Janus did not become ready on port {port}: {last}\n{logs}")


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
        raise InstallError(f"Could not create the administrator: {detail}") from None


def prompt_admin(args):
    email = args.admin_email
    interactive = sys.stdin.isatty()
    if not email and interactive:
        email = input("Administrator email: ").strip()
    if not email:
        return None
    if not re.fullmatch(r"[^@\s]+@[^@\s]+\.[^@\s]+", email):
        raise InstallError("That doesn't look like an email address: " + email)
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


# --------------------------------------------------------------------------
# Commands
# --------------------------------------------------------------------------

def ensure_user():
    try:
        pwd.getpwnam(SERVICE_USER)
        return
    except KeyError:
        pass
    shell = "/usr/sbin/nologin" if Path("/usr/sbin/nologin").exists() else "/sbin/nologin"
    run(["useradd", "--system", "--home-dir", str(DATA), "--no-create-home", "--shell", shell,
         "--user-group", "--comment", "Janus Edge", SERVICE_USER], capture=True)


def backup_database(env, version):
    url = env.get("JANUS_DATABASE_URL", "")
    if not url.startswith("sqlite://"):
        print("PostgreSQL database: take your usual backup before upgrading if you haven't.")
        return
    src = Path(url[len("sqlite://"):].split("?")[0])
    if not src.exists():
        return
    import sqlite3
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    label = f"before-{version}" if version != "unknown" else "backup"
    dest = BACKUPS / f"janus-{label}-{stamp}.db"
    source = sqlite3.connect(f"file:{src}?mode=ro", uri=True)
    target = sqlite3.connect(dest)
    try:
        source.backup(target)
    finally:
        source.close()
        target.close()
    os.chmod(dest, 0o600)
    os.chown(dest, service_uid(), service_gid())
    print(f"Backed up the database to {dest}")


def open_firewall(https_port):
    if shutil.which("firewall-cmd") and run(["firewall-cmd", "--state"], check=False, capture=True).returncode == 0:
        run(["firewall-cmd", "--permanent", "--add-service=http"], check=False, capture=True)
        extra = ["--add-service=https"] if https_port == 443 else [f"--add-port={https_port}/tcp"]
        run(["firewall-cmd", "--permanent"] + extra, check=False, capture=True)
        run(["firewall-cmd", "--reload"], check=False, capture=True)
        print(f"Opened ports 80 and {https_port} in firewalld.")
    elif shutil.which("ufw"):
        status = run(["ufw", "status"], check=False, capture=True).stdout or ""
        if "Status: active" in status:
            run(["ufw", "allow", "80/tcp"], check=False, capture=True)
            run(["ufw", "allow", f"{https_port}/tcp"], check=False, capture=True)
            print(f"Opened ports 80 and {https_port} in ufw.")


def link_ctl():
    if CTL_LINK.is_symlink() or not CTL_LINK.exists():
        if CTL_LINK.is_symlink():
            CTL_LINK.unlink()
        CTL_LINK.symlink_to(CTL)


def label_selinux():
    """On SELinux systems let systemd execute the binaries we placed."""
    if shutil.which("restorecon") and Path("/sys/fs/selinux/enforce").exists():
        run(["restorecon", "-R", str(BIN), str(CTL), str(LIBEXEC)], check=False, capture=True)


def cmd_install(args, upgrade=False):
    need_root("The server install")
    need_systemd()
    need_openssl()
    if not shutil.which("python3") and not Path("/usr/bin/python3").exists():
        raise InstallError("python3 is required at /usr/bin/python3")
    if args.database_url and not re.match(r"(postgres|postgresql|sqlite)://", args.database_url):
        raise InstallError("--database-url must start with postgres:// or sqlite://")
    first_install = not ENV_FILE.exists()
    if upgrade and first_install:
        raise InstallError("Janus is not installed as a service here; run: sudo python3 install.py")
    env = read_env()
    https_port = https_port_of(env) if not first_install else args.https_port
    hostname = args.hostname or host_from_url(env.get("JANUS_PUBLIC_URL", "")) or socket.getfqdn()
    if not valid_hostname(hostname):
        raise InstallError(f"Invalid hostname {hostname!r}; pass --hostname ai.example.com")

    admin = None
    if first_install and not args.skip_admin:
        admin = prompt_admin(args)
        if admin is None:
            raise InstallError("An administrator email is needed so nobody else can claim this server first.\n"
                               "  sudo python3 install.py --admin-email you@example.com\n"
                               "or add --skip-admin to create the administrator in the browser while the "
                               "server is still only reachable from this machine.")

    with tempfile.TemporaryDirectory(prefix="janus-install-") as temp:
        temp = Path(temp)
        if args.archive:
            archive, checksums = args.archive, args.checksums
        else:
            print(f"Downloading Janus Edge {args.version} ({arch()}) ...")
            archive, checksums = download_release(args.version, temp)
        root = extract(archive, checksums, temp / "x")
        version = version_of(root)

        ensure_user()
        for d, mode, owner in ((ETC, 0o750, (0, service_gid())),
                               (DATA, 0o750, (service_uid(), service_gid())),
                               (WEBROOT / ".well-known/acme-challenge", 0o755, (0, 0)),
                               (BACKUPS, 0o700, (service_uid(), service_gid()))):
            d.mkdir(parents=True, exist_ok=True)
            os.chmod(d, mode)
            os.chown(d, *owner)
        for d in (WEBROOT, WEBROOT / ".well-known"):
            os.chmod(d, 0o755)

        if not first_install:
            backup_database(env, version_of(SHARE))

        replace_file(root / "janus", BIN, 0o755)
        replace_file(Path(__file__).resolve(), CTL, 0o755)
        link_ctl()
        copy_share(root, SHARE)
        label_selinux()

    # Configuration: created once. The encryption key is never rotated.
    if first_install:
        env = {
            "JANUS_ENCRYPTION_KEY": secrets.token_hex(32),
            "JANUS_DATABASE_URL": args.database_url or f"sqlite://{DATA / 'janus.db'}",
            "JANUS_ENV": "production",
            "JANUS_TROUBLESHOOT_DIR": str(DATA / "captures"),
        }
    elif args.database_url and args.database_url != env.get("JANUS_DATABASE_URL"):
        raise InstallError("Changing the database of an existing install is a migration, not an upgrade. "
                           f"Edit {ENV_FILE} deliberately if you really mean it.")
    env["JANUS_PUBLIC_URL"] = "https://" + hostname + ("" if https_port == 443 else f":{https_port}")
    env["JANUS_TLS_CERT_PATH"] = str(CERT)
    env["JANUS_TLS_KEY_PATH"] = str(KEY)

    if not CERT.exists() or not KEY.exists():
        names = make_self_signed(hostname)
        print(f"Created a self-signed certificate for {', '.join(names)}")

    write_units(https_port)
    open_firewall(https_port)

    if first_install:
        # Bring Janus up on loopback first so nobody else can reach the
        # "create the first administrator" page before the installer does.
        env["JANUS_LISTEN_ADDR"] = f"127.0.0.1:{https_port}"
        write_env(env)
        systemctl("enable", "janus.service")
        systemctl("restart", "janus.service")
        wait_ready(https_port)
        if admin and needs_setup(https_port):
            create_admin(https_port, admin[0], admin[1], admin[2])
        if args.skip_admin:
            systemctl("enable", "--now", "janus-redirect.service")
            print(f"\nJanus Edge {version} is installed, but only reachable from this machine until you "
                  "create the first administrator.\n"
                  f"  From your computer: ssh -L {https_port}:127.0.0.1:{https_port} <this-server>\n"
                  f"  Then open https://127.0.0.1:{https_port}, create the administrator, and run:\n"
                  "    sudo janus-ctl open")
            return
    env["JANUS_LISTEN_ADDR"] = f":{https_port}"
    write_env(env)
    systemctl("enable", "janus.service", "janus-redirect.service")
    systemctl("restart", "janus.service", "janus-redirect.service")
    if ACME_CONF.exists():
        systemctl("enable", "--now", "janus-acme-renew.timer")
    wait_ready(https_port)

    info = cert_info(CERT)
    print()
    print(f"Janus Edge {version} is {'upgraded' if upgrade else 'running as a system service'}.")
    print(f"  Open:        {env['JANUS_PUBLIC_URL']}")
    ips = [ip for ip in local_ips() if ip != hostname]
    if ips and not is_ip(hostname):
        print(f"               or https://{ips[0]}" + ("" if https_port == 443 else f":{https_port}")
              + " until DNS points here")
    print("  HTTP:        port 80 redirects to HTTPS")
    if admin:
        print(f"  Sign in as:  {admin[0]}")
        if admin[3]:
            print(f"  Password:    {admin[2]}")
            print("               (shown only now; change it under your profile after signing in)")
    if info["self_signed"]:
        print("  Certificate: self-signed, so browsers will warn until you install a real one:")
        print("                 sudo janus-ctl cert install --cert fullchain.pem --key privkey.pem")
        print("               or get a free one from Let's Encrypt (needs a public DNS name and port 80):")
        print("                 sudo janus-ctl cert acme --domain "
              + (hostname if "." in hostname and not is_ip(hostname) else "ai.example.com")
              + " --email you@example.com")
    else:
        print(f"  Certificate: {', '.join(info['names']) or info['subject']}, expires {info['not_after']}")
    print(f"  Config:      {ENV_FILE}  (back it up: it holds the encryption key)")
    print(f"  Data:        {DATA}")
    print("  Manage:      sudo janus-ctl status | upgrade | cert ... | uninstall")


def cmd_upgrade(args):
    args.hostname = None
    args.admin_email = None
    args.admin_name = None
    args.skip_admin = True
    args.database_url = None
    args.https_port = 443
    cmd_install(args, upgrade=True)


def cmd_open(_args):
    """Finish a --skip-admin install: listen on all interfaces."""
    need_root("Opening the server")
    env = read_env()
    port = https_port_of(env)
    if needs_setup(port):
        raise InstallError("Create the first administrator first (see the install output), then run this again.")
    env["JANUS_LISTEN_ADDR"] = f":{port}"
    write_env(env)
    systemctl("restart", "janus.service")
    wait_ready(port)
    print(f"Janus is now reachable at {env.get('JANUS_PUBLIC_URL')}")


def cmd_status(_args):
    if not ENV_FILE.exists():
        raise InstallError(f"Janus is not installed as a service here ({ENV_FILE} not found).")
    if not os.access(ENV_FILE, os.R_OK):
        raise InstallError("Run it with sudo to read the configuration: sudo janus-ctl status")
    env = read_env()
    port = https_port_of(env)
    print(f"Version:     {version_of(SHARE)}")
    print(f"URL:         {env.get('JANUS_PUBLIC_URL')}")
    for unit in ("janus", "janus-redirect", "janus-acme-renew.timer"):
        state = systemctl("is-active", unit, check=False).stdout.strip()
        if unit.endswith(".timer") and not ACME_CONF.exists():
            continue
        print(f"{unit + ':':<13}{state}")
    try:
        _, body = local_get(port, "/readyz")
        print(f"Ready:       {'yes' if json.loads(body).get('ready') else 'no'}")
    except Exception as err:  # noqa: BLE001
        print(f"Ready:       no ({err})")
    db = env.get("JANUS_DATABASE_URL", "")
    print("Database:    " + ("SQLite " + db[len("sqlite://"):] if db.startswith("sqlite://") else "PostgreSQL"))
    if CERT.exists():
        info = cert_info(CERT)
        kind = "self-signed" if info["self_signed"] else (
            "Let's Encrypt, renews automatically" if ACME_CONF.exists() else "installed")
        print(f"Certificate: {kind}; expires {info['not_after']}")
        print(f"             names: {', '.join(info['names']) or info['subject']}")
    if env.get("JANUS_LISTEN_ADDR", "").startswith("127.0.0.1"):
        print("Note:        only reachable from this machine; run `sudo janus-ctl open` after creating the administrator.")


def set_public_host(env, host):
    port = https_port_of(env)
    env["JANUS_PUBLIC_URL"] = "https://" + host + ("" if port == 443 else f":{port}")
    write_env(env)
    systemctl("restart", "janus.service")
    wait_ready(port)


def drop_acme():
    if ACME_CONF.exists():
        ACME_CONF.unlink()
    systemctl("disable", "--now", "janus-acme-renew.timer", check=False)


def cmd_cert(args):
    need_root("Changing the certificate")
    env = read_env()
    if not env:
        raise InstallError("Install Janus first: sudo python3 install.py")
    hostname = host_from_url(env.get("JANUS_PUBLIC_URL", ""))
    if args.cert_cmd == "install":
        cert = Path(args.cert)
        combined = None
        if args.chain:
            fd, combined = tempfile.mkstemp(prefix="janus-chain-")
            with os.fdopen(fd, "w") as out:
                out.write(cert.read_text().rstrip() + "\n" + Path(args.chain).read_text())
            cert = Path(combined)
        try:
            info = check_cert_pair(cert, Path(args.key), args.hostname)
            new_host = args.hostname or hostname
            if not args.hostname and info["names"] and not name_matches(hostname, info["names"]):
                dns = [n for n in info["names"] if not is_ip(n) and not n.startswith("*.")]
                if dns:
                    new_host = dns[0]
                    print(f"Using {new_host} as the public address, from the certificate.")
            install_cert_files(cert, Path(args.key))
        finally:
            if combined:
                os.unlink(combined)
        drop_acme()
        set_public_host(env, new_host)
        print(f"Installed the certificate for {', '.join(info['names']) or info['subject']} "
              f"(expires {info['not_after']}). Janus has restarted with it.")
    elif args.cert_cmd == "self-signed":
        host = args.hostname or hostname
        names = make_self_signed(host)
        drop_acme()
        set_public_host(env, host)
        print(f"Created a new self-signed certificate for {', '.join(names)}.")
    elif args.cert_cmd == "acme":
        if not valid_hostname(args.domain) or is_ip(args.domain) or "." not in args.domain:
            raise InstallError("--domain must be a public DNS name that points at this server")
        conf = {"domain": args.domain, "email": args.email or "",
                "server": args.server or ("https://acme-staging-v02.api.letsencrypt.org/directory" if args.staging
                                          else "https://acme-v02.api.letsencrypt.org/directory")}
        if args.ca_bundle:
            conf["ca_bundle"] = str(Path(args.ca_bundle).resolve())
        obtain_acme(conf, force=True)
        ACME_CONF.write_text(json.dumps(conf, indent=2) + "\n")
        os.chmod(ACME_CONF, 0o600)
        systemctl("enable", "--now", "janus-acme-renew.timer")
        set_public_host(env, args.domain)
        print(f"Installed a Let's Encrypt certificate for {args.domain}. It renews automatically.")
    elif args.cert_cmd == "renew":
        if not ACME_CONF.exists():
            print("No Let's Encrypt certificate is configured; nothing to renew.")
            return
        if obtain_acme(json.loads(ACME_CONF.read_text()), force=args.force):
            systemctl("restart", "janus.service")
            print("Certificate renewed; Janus restarted.")
        else:
            print("Certificate is not due for renewal.")


def lego_binary():
    path = LIBEXEC / f"lego-{LEGO_VERSION}"
    if path.exists():
        return path
    a = arch()
    name = f"lego_v{LEGO_VERSION}_linux_{a}.tar.gz"
    with tempfile.TemporaryDirectory(prefix="janus-lego-") as temp:
        tgz = Path(temp) / name
        print(f"Downloading the lego ACME client {LEGO_VERSION} ...")
        fetch(f"https://github.com/go-acme/lego/releases/download/v{LEGO_VERSION}/{name}", tgz)
        if sha256_file(tgz) != LEGO_SHA256[a]:
            raise InstallError("The lego download failed its checksum; not using it.")
        with tarfile.open(tgz, "r:gz") as tar:
            member = tar.getmember("lego")
            if not member.isfile():
                raise InstallError("Unexpected lego archive layout")
            with tar.extractfile(member) as src, (Path(temp) / "lego").open("wb") as out:
                shutil.copyfileobj(src, out)
        replace_file(Path(temp) / "lego", path, 0o755)
    label_selinux()
    return path


def obtain_acme(conf, force):
    """Get or renew over HTTP-01, served from the port-80 redirect service's webroot."""
    lego = lego_binary()
    ACME_DIR.mkdir(parents=True, exist_ok=True)
    os.chmod(ACME_DIR, 0o700)
    if systemctl("is-active", "janus-redirect", check=False).stdout.strip() != "active":
        systemctl("start", "janus-redirect")
    crt = ACME_DIR / "certificates" / f"{conf['domain']}.crt"
    key = ACME_DIR / "certificates" / f"{conf['domain']}.key"
    before = sha256_file(crt) if crt.exists() else ""
    cmd = [str(lego), "--log.format", "text", "run", "--accept-tos", "--path", str(ACME_DIR),
           "--server", conf["server"], "-d", conf["domain"], "--http", "--http.webroot", str(WEBROOT)]
    if conf.get("email"):
        cmd += ["-m", conf["email"]]
    if force and crt.exists():
        cmd += ["--renew-force"]
    # The systemd timer already spreads renewals (RandomizedDelaySec), and a
    # manual renewal should not sit silently for minutes.
    cmd += ["--no-random-sleep"]
    env = dict(os.environ)
    if conf.get("ca_bundle"):
        env["LEGO_CA_CERTIFICATES"] = conf["ca_bundle"]
    result = run(cmd, check=False, capture=True, env=env)
    if result.returncode != 0:
        tail = "\n".join((result.stderr or result.stdout or "").strip().splitlines()[-8:])
        raise InstallError("The certificate authority could not issue the certificate. Check that "
                           f"{conf['domain']} resolves to this server and that port 80 is reachable "
                           f"from the internet.\n{tail}")
    if not crt.exists() or not key.exists():
        raise InstallError("lego finished but no certificate was written")
    changed = force or sha256_file(crt) != before
    if changed:
        check_cert_pair(crt, key, conf["domain"])
        install_cert_files(crt, key)
    return changed


def cmd_uninstall(args):
    need_root("Uninstalling")
    for unit in UNITS:
        run(["systemctl", "disable", "--now", unit], check=False, capture=True)
        (UNIT_DIR / unit).unlink(missing_ok=True)
    run(["systemctl", "daemon-reload"], check=False)
    if CTL_LINK.is_symlink():
        CTL_LINK.unlink()
    for path in (BIN, CTL):
        path.unlink(missing_ok=True)
    for path in (SHARE, LIBEXEC):
        shutil.rmtree(path, ignore_errors=True)
    if args.purge:
        shutil.rmtree(ETC, ignore_errors=True)
        shutil.rmtree(DATA, ignore_errors=True)
        run(["userdel", SERVICE_USER], check=False, capture=True)
        print("Removed Janus Edge with its configuration, certificates and data.")
    else:
        print("Removed the Janus Edge service and program files.\n"
              f"Kept your configuration ({ETC}) and data ({DATA}); reinstalling picks them up again.\n"
              "To delete those too: sudo python3 install.py uninstall --purge")


# --------------------------------------------------------------------------
# Port 80: redirect to HTTPS and answer ACME HTTP-01 challenges
# --------------------------------------------------------------------------

HOST_RE = re.compile(r"^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9.-]{1,253})(?::[0-9]{1,5})?$")
TOKEN_RE = re.compile(r"^[A-Za-z0-9_-]{1,256}$")
CHALLENGE_PREFIX = "/.well-known/acme-challenge/"


def redirect_target(host_header, path, https_port, fallback_host):
    m = HOST_RE.match((host_header or "").strip())
    host = m.group(1) if m else fallback_host
    if not path.startswith("/") or path.startswith("//"):
        path = "/"
    return f"https://{host}{'' if https_port == 443 else ':' + str(https_port)}{path}"


class RedirectHandler(http.server.BaseHTTPRequestHandler):
    server_version = "janus-redirect"
    sys_version = ""

    def _challenge(self):
        if not self.path.startswith(CHALLENGE_PREFIX):
            return False
        token = self.path[len(CHALLENGE_PREFIX):].split("?", 1)[0]
        body = None
        if TOKEN_RE.match(token):
            try:
                body = (Path(self.server.webroot) / ".well-known/acme-challenge" / token).read_bytes()
            except OSError:
                body = None
        self.send_response(200 if body is not None else 404)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body or b"")))
        self.end_headers()
        if body and self.command != "HEAD":
            self.wfile.write(body)
        return True

    def _redirect(self):
        if self.command in ("GET", "HEAD") and self._challenge():
            return
        target = redirect_target(self.headers.get("Host"), self.path, self.server.https_port,
                                 self.server.fallback_host)
        # 308 keeps the method and body for API clients that POST to http://.
        self.send_response(301 if self.command in ("GET", "HEAD") else 308)
        self.send_header("Location", target)
        self.send_header("Content-Length", "0")
        self.send_header("Connection", "close")
        self.end_headers()

    do_GET = do_HEAD = do_POST = do_PUT = do_PATCH = do_DELETE = do_OPTIONS = _redirect

    def log_message(self, fmt, *args):  # one log line per redirect is noise
        pass


class RedirectServer(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True
    allow_reuse_address = True


def cmd_redirect(args):
    fallback = ""
    if os.access(ENV_FILE, os.R_OK):
        fallback = host_from_url(read_env().get("JANUS_PUBLIC_URL", ""))
    try:
        RedirectServer.address_family = socket.AF_INET6

        class V6(RedirectServer):
            def server_bind(self):
                self.socket.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 0)
                super().server_bind()
        server = V6(("::", args.port), RedirectHandler)
    except OSError:
        RedirectServer.address_family = socket.AF_INET
        server = RedirectServer(("0.0.0.0", args.port), RedirectHandler)
    server.https_port = args.https_port
    server.webroot = args.webroot
    server.fallback_host = fallback or "localhost"
    print(f"Redirecting http port {args.port} to https port {args.https_port}", flush=True)
    server.serve_forever()


# --------------------------------------------------------------------------
# Entry point
# --------------------------------------------------------------------------

COMMANDS = {"install", "upgrade", "status", "open", "cert", "uninstall", "redirect"}


def build_parser():
    parser = argparse.ArgumentParser(prog="janus-ctl", description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command")

    def add_source(p):
        p.add_argument("--version", default=DEFAULT_VERSION, help="release to install (default %(default)s)")
        p.add_argument("--archive", type=Path, help="install from a downloaded archive (offline)")
        p.add_argument("--checksums", type=Path, help="the release SHA256SUMS, required with --archive")

    p = sub.add_parser("install", help="install the system service (default command)")
    add_source(p)
    p.add_argument("--hostname", help="name people will use to reach it (default: this machine's FQDN)")
    p.add_argument("--admin-email", help="create the first administrator with this email")
    p.add_argument("--admin-name", help="administrator display name")
    p.add_argument("--skip-admin", action="store_true",
                   help="create the administrator in the browser instead; Janus stays loopback-only until then")
    p.add_argument("--database-url", help="postgres://... to use PostgreSQL (default: SQLite in /var/lib/janus)")
    p.add_argument("--https-port", type=int, default=443, help=argparse.SUPPRESS)
    p.add_argument("--user", action="store_true", help="no-sudo trial install into ~/.local (no service)")
    p.add_argument("--prefix", type=Path, default=Path.home() / ".local", help=argparse.SUPPRESS)

    p = sub.add_parser("upgrade", help="install another release, keeping configuration and data")
    add_source(p)
    sub.add_parser("status", help="show service, readiness and certificate")
    sub.add_parser("open", help="after a --skip-admin install, listen on all interfaces")

    p = sub.add_parser("cert", help="manage the HTTPS certificate")
    cs = p.add_subparsers(dest="cert_cmd", required=True)
    c = cs.add_parser("install", help="use your own certificate and key")
    c.add_argument("--cert", required=True, help="PEM certificate, ideally the full chain (fullchain.pem)")
    c.add_argument("--key", required=True, help="PEM private key, unencrypted (privkey.pem)")
    c.add_argument("--chain", help="intermediate certificates, if they are not already in --cert")
    c.add_argument("--hostname", help="public name (default: taken from the certificate)")
    c = cs.add_parser("self-signed", help="replace the certificate with a new self-signed one")
    c.add_argument("--hostname", help="name to put on the certificate")
    c = cs.add_parser("acme", help="get a Let's Encrypt certificate over port 80 and renew it automatically")
    c.add_argument("--domain", required=True, help="public DNS name that points at this server")
    c.add_argument("--email", help="contact email for expiry notices")
    c.add_argument("--staging", action="store_true", help="use Let's Encrypt staging, for testing")
    c.add_argument("--server", help="another ACME directory URL (default Let's Encrypt)")
    c.add_argument("--ca-bundle", help=argparse.SUPPRESS)
    c = cs.add_parser("renew", help="renew the Let's Encrypt certificate when due (run by a timer)")
    c.add_argument("--force", action="store_true", help=argparse.SUPPRESS)

    p = sub.add_parser("uninstall", help="remove the service; keeps configuration and data unless --purge")
    p.add_argument("--purge", action="store_true", help="also delete configuration, certificates and data")

    p = sub.add_parser("redirect")  # internal: run by janus-redirect.service
    p.add_argument("--port", type=int, default=80)
    p.add_argument("--https-port", type=int, default=443)
    p.add_argument("--webroot", default=str(WEBROOT))
    return parser


def main(argv=None):
    argv = list(sys.argv[1:] if argv is None else argv)
    if not argv or argv[0] not in COMMANDS | {"-h", "--help"}:
        argv = ["install"] + argv
    args = build_parser().parse_args(argv)
    if platform.system() != "Linux":
        raise InstallError("Only Linux is supported")
    if bool(getattr(args, "archive", None)) != bool(getattr(args, "checksums", None)):
        raise InstallError("--archive and --checksums must be provided together")
    if args.command == "install" and args.user:
        if args.archive:
            install(args.archive, args.checksums, args.prefix)
            return
        with tempfile.TemporaryDirectory(prefix="janus-download-") as temp:
            archive, checksums = download_release(args.version, Path(temp))
            install(archive, checksums, args.prefix)
        return
    if args.command in ("install", "upgrade") and os.geteuid() != 0:
        raise InstallError("The server install needs root:\n"
                           "  sudo python3 install.py --admin-email you@example.com\n"
                           "For a no-sudo trial in your home directory instead:\n"
                           "  python3 install.py --user")
    {"install": cmd_install, "upgrade": cmd_upgrade, "status": cmd_status, "open": cmd_open,
     "cert": cmd_cert, "uninstall": cmd_uninstall, "redirect": cmd_redirect}[args.command](args)


if __name__ == "__main__":
    try:
        main()
    except InstallError as err:
        print(f"janus: {err}", file=sys.stderr)
        sys.exit(1)
    except (ValueError, OSError) as err:
        print(f"janus: {err}", file=sys.stderr)
        sys.exit(1)
    except KeyboardInterrupt:
        sys.exit(130)
