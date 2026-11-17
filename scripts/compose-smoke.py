#!/usr/bin/env python3
"""Smoke-test an extracted janus-compose bundle after `setup.py` ran (used by CI).

  python3 scripts/compose-smoke.py <bundle dir> <setup output file> <hostname> <port>

Checks: the database port is not published, Janus serves HTTPS with a
self-signed certificate for the hostname, the administrator printed by
setup.py can sign in, an API token works, and the token still works after a
restart and after backup.sh -> `docker compose down -v` -> restore.sh.
"""
import http.cookiejar
import json
from pathlib import Path
import re
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.request

BUNDLE, SETUP_LOG, HOST, PORT = Path(sys.argv[1]).resolve(), Path(sys.argv[2]), sys.argv[3], int(sys.argv[4])
BASE = f"https://{HOST}:{PORT}"


def sh(*cmd, check=True):
    print("+", " ".join(cmd), flush=True)
    return subprocess.run(cmd, cwd=BUNDLE, check=check, capture_output=True, text=True).stdout


def verified_context():
    pem = ssl.get_server_certificate(("127.0.0.1", PORT))
    ca = BUNDLE.parent / "janus-ca.pem"
    ca.write_text(pem)
    return ssl.create_default_context(cafile=str(ca))  # hostname checked against HOST


def wait_healthy():
    for _ in range(90):
        if "(healthy)" in sh("docker", "compose", "ps", "janus", "--format", "{{.Status}}"):
            return
        time.sleep(2)
    raise SystemExit(sh("docker", "compose", "logs", "--tail", "40"))


def request(opener, method, path, body=None, headers=None):
    req = urllib.request.Request(BASE + path, method=method, headers={"Content-Type": "application/json", **(headers or {})},
                                 data=json.dumps(body).encode() if body is not None else None)
    try:
        with opener.open(req, timeout=30) as r:
            return r.status, json.loads(r.read() or b"null")
    except urllib.error.HTTPError as err:
        return err.code, err.read().decode(errors="replace")


def main():
    ports = sh("docker", "compose", "ps", "postgres", "--format", "{{.Ports}}")
    assert "->" not in ports, f"PostgreSQL port is published: {ports}"
    ctx = verified_context()
    jar = http.cookiejar.CookieJar()
    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar), urllib.request.HTTPSHandler(context=ctx))
    found = re.search(r"Password:\s+(\S+)", SETUP_LOG.read_text())
    assert found, "setup.py printed no administrator password"
    password = found.group(1)
    status, _ = request(opener, "POST", "/auth/local/login", {"email": "admin@example.test", "password": password})
    assert status == 200, f"login {status}"
    csrf = next(c.value for c in jar if c.name == "janus_csrf")
    status, body = request(opener, "POST", "/api/v1/tokens", {"description": "ci"}, {"X-Janus-CSRF": csrf})
    assert status == 201 and isinstance(body, dict), f"token {status} {body}"
    token = body["value"]
    plain = urllib.request.build_opener(urllib.request.HTTPSHandler(context=ctx))

    def models():
        status, body = request(plain, "GET", "/v1/models", headers={"Authorization": "Bearer " + token})
        assert status == 200, f"/v1/models {status} {body}"
        print("token works: GET /v1/models 200", flush=True)

    models()
    sh("docker", "compose", "restart")
    wait_healthy()
    models()
    print(sh("./backup.sh"))
    backup = sorted((BUNDLE / "backups").iterdir())[-1]
    sh("docker", "compose", "down", "-v")
    (BUNDLE / ".env").unlink()
    print(sh("./restore.sh", str(backup.relative_to(BUNDLE))))
    wait_healthy()
    models()
    print("compose bundle smoke test passed")


if __name__ == "__main__":
    main()
