# Janus with Docker Compose and PostgreSQL

For one server and a small team: two containers, Janus and PostgreSQL 16.
Janus serves HTTPS itself. The database is never published outside the
Compose network. This is not a high-availability setup; use the Helm chart
for that.

## Requirements

- Linux with Docker Engine and the Docker Compose v2 plugin (`docker compose`).
  Tested on Ubuntu 24.04 with Docker's own packages.
- Python 3.8 or newer (for `setup.py`, standard library only).
- Outbound access to `ghcr.io` (Janus image) and Docker Hub (PostgreSQL image),
  or load both images with `docker load` beforehand.

## Install

```sh
tar xzf janus-compose-bundle-<version>.tar.gz
cd janus-compose
python3 setup.py --admin-email you@example.com --hostname ai.example.com
```

`setup.py`:

1. writes `.env` once with a new encryption key and database password
   (mode 0600; it never overwrites an existing `.env`),
2. pulls the images and starts PostgreSQL and Janus with the HTTPS port bound
   to 127.0.0.1 only,
3. creates the first administrator over loopback and prints the sign-in
   email and a generated password (shown only once),
4. publishes the HTTPS port (443 by default; `--https-port` changes it).

Without `--admin-email` it asks. `--skip-admin` leaves Janus reachable only
from the server: tunnel in with `ssh -L 443:127.0.0.1:443 <server>`, create the
administrator at `https://127.0.0.1`, then run `python3 setup.py open`.

If your user is not in the `docker` group, run the commands with `sudo`.
Docker publishes ports through its own firewall rules, so ufw/firewalld
rules do not restrict the published port.

## Certificates

Janus creates a self-signed certificate on first start (stored encrypted in
the database), so browsers warn until you install your own:

```sh
python3 setup.py cert --cert fullchain.pem --key privkey.pem   # your own
python3 setup.py cert --self-signed                             # back to self-signed
```

Run `cert` again with renewed files; Janus reloads them without a restart.

**Let's Encrypt (optional):** `python3 setup.py --caddy --admin-email ... --hostname ai.example.com`
on a fresh directory adds a Caddy container that obtains and renews the
certificate and redirects port 80 to HTTPS. It needs a public DNS name for the
host and ports 80 and 443 reachable from the internet. Janus then stays on
127.0.0.1:8443 behind Caddy.

## Everyday commands

```sh
docker compose ps                 # both containers healthy?
docker compose logs -f janus
docker compose restart janus
docker compose down               # stops; keeps data and .env
docker compose up -d
```

`docker compose down -v` **deletes the database**. Never edit
`JANUS_ENCRYPTION_KEY` or `POSTGRES_PASSWORD` in `.env`.

## Backup and restore

```sh
./backup.sh                        # backups/<UTC time>/janus.dump + env
./restore.sh backups/<UTC time>    # into this stack, or a fresh copy of the bundle
```

The backup holds a copy of `.env`, because the database cannot be read without
its encryption key. Keep backups private. To move to a new server, extract the
bundle there, copy the backup directory over and run `./restore.sh` before
`setup.py`.

## Upgrade

1. `./backup.sh`
2. From the directory above this one, extract the new bundle over it
   (`tar xzf janus-compose-bundle-<new version>.tar.gz`). That replaces
   `compose.yaml` and the scripts and keeps `.env`, `certs/` and `backups/`.
3. `docker compose pull && docker compose up -d`

Janus migrates the database on start. There is no downgrade after a migration;
restore the backup instead. A PostgreSQL major version change (16 to 17) is a
dump and restore, never just an image change.

## License key file

Licenses can be installed in the web UI. To provide one as a file instead,
copy it to `certs/janus.license`, make it readable by the container user
(`sudo chown 65532 certs/janus.license`), add
`JANUS_LICENSE_FILE=/certs/janus.license` to `.env` and run
`docker compose up -d`.

## FIPS edition

Add `JANUS_IMAGE=ghcr.io/torvanis/janus:<version>-fips` and the license key
file above to `.env` before the first start: the FIPS edition does not start
without a Business or Enterprise license key. When you upgrade with a newer
bundle, update the `JANUS_IMAGE` line to the new version as well.
