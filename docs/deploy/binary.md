# Go binary + systemd

Run CrashCart as a plain service on a Linux host, with Postgres installed
on the same machine or provided by a managed service, and an
S3-compatible bucket for payloads and symbol files. Prefer containers?
See [Docker Compose on a VPS](./docker).

::: info Storage
Any Postgres 14+ does. For the bucket, use your cloud's (S3, R2, B2, …)
or run [MinIO](https://min.io/docs/minio/linux/index.html) next to the
service. See [The database and the object store](./postgres).
:::

## 1. Get the binary

Download the latest release for your platform — a single static binary
with the web assets embedded, nothing else to install:

```sh
curl -fsSL https://github.com/crashcartapp/crashcart/releases/latest/download/crashcart_linux_amd64.tar.gz | tar xz crashcart
sudo install -m 755 crashcart /usr/local/bin/crashcart
crashcart version
```

Builds are published for `linux_amd64`, `linux_arm64`, `darwin_amd64` and
`darwin_arm64`; see the
[releases page](https://github.com/crashcartapp/crashcart/releases) for
checksums.

::: details Building from source instead
Needs Go 1.24 or newer.

```sh
git clone https://github.com/crashcartapp/crashcart
cd crashcart
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o crashcart ./cmd/crashcart
sudo install -m 755 crashcart /usr/local/bin/crashcart
```
:::

## 2. Set up Postgres

Either a local Postgres:

```sh
sudo apt install postgresql          # or your distro's package
sudo -u postgres psql <<'SQL'
CREATE USER crashcart WITH PASSWORD 'change-me';
CREATE DATABASE crashcart OWNER crashcart;
SQL
```

or a managed one — take its connection URL. Then create a bucket for
CrashCart (dedicated: it manages the bucket's lifecycle rules) and an
access key for it. See [The database and the object store](./postgres).

## 3. Configure

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin crashcart
sudo tee /etc/crashcart.env >/dev/null <<'EOF2'
DATABASE_URL=postgres://crashcart:change-me@localhost:5432/crashcart?sslmode=disable
S3_ENDPOINT=https://<account>.r2.cloudflarestorage.com   # empty for AWS S3; http://localhost:9000 for a local MinIO
S3_REGION=auto
S3_BUCKET=crashcart
S3_ACCESS_KEY=change-me
S3_SECRET_KEY=change-me
PUBLIC_URL=https://crashcart.example.com
RETENTION_DAYS=30
# Only if you use a browser SDK and want to restrict which sites may send events.
# CORS_ORIGIN=https://shop.example.com
EOF2
sudo chmod 600 /etc/crashcart.env
```

All settings are listed in [Configuration](./configuration).

## 4. Install the service

```sh
sudo tee /etc/systemd/system/crashcart.service >/dev/null <<'EOF2'
[Unit]
Description=CrashCart
After=network-online.target postgresql.service
Wants=network-online.target

[Service]
User=crashcart
EnvironmentFile=/etc/crashcart.env
ExecStart=/usr/local/bin/crashcart serve
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF2
sudo systemctl daemon-reload
sudo systemctl enable --now crashcart
sudo systemctl status crashcart
```

CrashCart is listening on port 8080, has created its tables and set the
bucket's lifecycle rules (the log says if it could not).

## 5. Put a reverse proxy in front

CrashCart does not terminate TLS. With Caddy, this is the whole config:

```
crashcart.example.com {
    reverse_proxy localhost:8080
}
```

For nginx, proxy `/` to `http://127.0.0.1:8080` and set
`client_max_body_size 25m` so large crash reports get through.

## 6. Create a project

```sh
sudo -u crashcart env $(sudo cat /etc/crashcart.env | xargs) crashcart project shop-ios "Shop app (iOS)" ios
```

or open `https://crashcart.example.com` and create it in the viewer. Then
follow [Connect an SDK](/guide/sdks), and go through
[Before going live](./checklist) once.

## Upgrading

Download the new release, replace the binary, restart:

```sh
curl -fsSL https://github.com/crashcartapp/crashcart/releases/latest/download/crashcart_linux_amd64.tar.gz | tar xz crashcart
sudo install -m 755 crashcart /usr/local/bin/crashcart
sudo systemctl restart crashcart
```

The schema is created on start.

## iOS crashes

dSYM symbolication runs in a separate container
(`container/symbolicate` in the repository). Run it with Docker or Podman
next to the service and set `SYMBOLICATE_URL=http://localhost:<port>` in
`/etc/crashcart.env`. Android and JavaScript need nothing extra.
