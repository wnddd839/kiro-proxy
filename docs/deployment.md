# Deployment

This page covers the three supported ways to run kiro.cache: Docker, a
prebuilt binary, and a source build. It also includes reverse proxy, systemd,
and outbound proxy notes.

The proxy is a local tool by default. It listens on `127.0.0.1:8787` and only
accepts admin requests from the same machine unless you configure
`admin_token`.

## Requirements

- One of: Docker 24+, a prebuilt binary, or Go 1.26.4+ for a source build.
- A Kiro account you are allowed to use.
- A client that speaks Anthropic Messages or OpenAI Chat Completions /
  Responses.

No runtime dependencies are required for the binary. The Docker image is
distroless and contains only the binary.

## Download a binary

Releases provide these assets. Older releases may contain only a subset.

| Platform | Asset |
|---|---|
| Windows x64 | `kiro-proxy-windows-amd64.exe` |
| Windows arm64 | `kiro-proxy-windows-arm64.exe` |
| Linux x64 | `kiro-proxy-linux-amd64` |
| Linux arm64 | `kiro-proxy-linux-arm64` |
| macOS Intel | `kiro-proxy-darwin-amd64` |
| macOS Apple Silicon | `kiro-proxy-darwin-arm64` |

Download the asset and `SHA256SUMS.txt` from the
[latest release](https://github.com/wnddd839/kiro.cache/releases/latest), then
verify:

```sh
sha256sum -c SHA256SUMS.txt
# macOS
shasum -a 256 -c SHA256SUMS.txt
```

On Linux and macOS:

```sh
chmod +x kiro-proxy-linux-amd64
./kiro-proxy-linux-amd64 -version
```

## Docker

The release workflow publishes `linux/amd64` and `linux/arm64` images to GHCR
when a `v*` tag is pushed. Until a tag with this workflow has been released,
build locally with compose:

```text
ghcr.io/wnddd839/kiro-cache:latest          # after the next v* tag
ghcr.io/wnddd839/kiro-cache:v0.5.0          # example tag
```

### Compose

```sh
git clone https://github.com/wnddd839/kiro.cache.git
cd kiro.cache

cat > .env <<EOF
KIRO_API_KEY=$(openssl rand -hex 24)
KIRO_ADMIN_TOKEN=$(openssl rand -hex 24)
EOF

docker compose up -d --build
docker compose logs -f
```

The compose file builds from the checkout by default. To use the published
image instead, replace `build:` with:

```yaml
image: ghcr.io/wnddd839/kiro-cache:latest
```

Open `http://127.0.0.1:8787/ui`.

`KIRO_API_KEY` is a full-access downstream key. Keep it secret. You can create
additional budgeted keys in the console later.

`KIRO_ADMIN_TOKEN` is required when the console is reached through Docker's
bridge network. Requests from the bridge are not loopback, so `/admin` would
otherwise reject them. The console asks for the token once and stores it in
`localStorage`.

### Docker run

Build or pull the image first. If the GHCR image for this tag does not exist
yet, use `kiro-cache:dev` built above.

```sh
docker run -d \
  --name kiro-cache \
  --restart unless-stopped \
  -p 127.0.0.1:8787:8787 \
  -e KIRO_API_KEY="$(openssl rand -hex 24)" \
  -e KIRO_ADMIN_TOKEN="$(openssl rand -hex 24)" \
  -v kiro-data:/data \
  kiro-cache:dev
```

The process runs as uid `65532` (`nonroot`). A named volume created by Docker
inherits the image directory ownership and works as-is. If you use a bind
mount, make it writable by that uid:

```sh
mkdir -p ./data
sudo chown -R 65532:65532 ./data
docker run ... -v "$PWD/data:/data" ...
```

### Sign in inside a container

The OAuth callback goes to `localhost` on the machine running the browser, not
into the container. Use the manual callback flow:

1. Open the console, go to **号池**, click **开始登录**.
2. Copy the sign-in URL shown on the page and open it in your host browser.
3. Complete sign-in. The browser stops on a URL ending in `oauth/callback`.
4. Paste that full URL into **粘贴回调地址** in the console.
5. For Builder ID / IAM Identity Center, the first step returns an AWS sign-in
   URL. Open it, complete sign-in, then paste the callback again.

`import-ide` is also possible if you mount the IDE token file. The proxy writes
refreshed tokens back to that file, so the mount must be writable:

```sh
-v "$HOME/.aws/sso/cache/kiro-auth-token.json:/data/ide-token.json"
```

Then import `/data/ide-token.json` from the console. Do not add the same Kiro
account both through `login` and `import-ide`: two refresh paths can invalidate
each other's refresh token.

### Health check

The image has a `HEALTHCHECK` that runs the binary with `-healthcheck`. You can
run it manually:

```sh
docker exec kiro-cache /usr/local/bin/kiro-proxy -healthcheck -listen 127.0.0.1:8787
```

`GET /health` returns `200` when the process is serving and the usage journal is
healthy. It returns `503` if the journal cannot accept writes.

### Building the image locally

```sh
docker build --build-arg VERSION=dev -t kiro-cache:dev .
docker run --rm -p 127.0.0.1:8787:8787 \
  -e KIRO_API_KEY=dev-key \
  -e KIRO_ADMIN_TOKEN=dev-admin \
  -v kiro-data:/data kiro-cache:dev
```

For a first run, set `KIRO_API_KEY` and `KIRO_ADMIN_TOKEN` to long random
values, not `dev-key` / `dev-admin`. The container refuses to start without a
downstream key because it listens on `0.0.0.0`.

## Binary

```sh
# Browser OAuth. On a server, paste the callback URL when prompted.
./kiro-proxy login

# Or import the account currently signed in to the Kiro IDE.
./kiro-proxy import-ide

# List the pool.
./kiro-proxy list

# Start. Default: 127.0.0.1:8787.
./kiro-proxy

# Print the version.
./kiro-proxy -version
```

`login` needs one of the IDE callback ports (`3128`, `4649`, `6588`, `8008`,
`9091`, `49153`, `50153`-`53153`) to be free. Close the Kiro IDE sign-in page
before starting.

On a server, the callback cannot reach the process. Both the CLI and the
console support pasting the callback URL:

- CLI: run `kiro-proxy login`; the terminal asks for a callback URL after the
  browser finishes.
- Console: **号池 → 开始登录 → 粘贴回调地址**.

### Useful flags

| Flag | Purpose |
|---|---|
| `-config PATH` | Config file. Missing file means defaults. |
| `-listen ADDR` | Override `listen`, for example `0.0.0.0:8787`. |
| `-allow-insecure-listen` | Allow a non-loopback listener without downstream keys. Dangerous. |
| `-healthcheck` | Call the local `/health` endpoint and exit. |
| `-version` | Print the version. |

### Environment variables

| Variable | Effect |
|---|---|
| `KIRO_ADMIN_TOKEN` | Overrides `admin_token`. Use it for containers or reverse proxies. |
| `KIRO_API_KEY` | Adds one full-access downstream key. Appended to `api_keys`. |

Environment variables are read at startup and are not written to disk.

## systemd

```ini
# /etc/systemd/system/kiro-proxy.service
[Unit]
Description=kiro.cache
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=kiro
Group=kiro
WorkingDirectory=/var/lib/kiro-proxy
ExecStart=/opt/kiro-proxy/kiro-proxy -config /etc/kiro-proxy/config.json
Restart=on-failure
RestartSec=5
StateDirectory=kiro-proxy
EnvironmentFile=-/etc/kiro-proxy/env

# Optional outbound proxy
# Environment=HTTPS_PROXY=http://127.0.0.1:7890
# Environment=HTTP_PROXY=http://127.0.0.1:7890
# Environment=NO_PROXY=127.0.0.1,localhost,::1

[Install]
WantedBy=multi-user.target
```

`/etc/kiro-proxy/env` can hold `KIRO_ADMIN_TOKEN` and `KIRO_API_KEY` with
`0600` permissions. Keep it out of the repository.

## Reverse proxy

If you mount the proxy under `/kiro/`, give the API its own location so Basic
Auth on the console does not intercept `Authorization: Bearer` used by
clients.

```nginx
location ^~ /kiro/v1/ {
    auth_basic off;
    proxy_pass http://127.0.0.1:8787/;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_read_timeout 600s;
    proxy_send_timeout 600s;
    proxy_buffering off;
    proxy_cache off;
}

location /kiro/ {
    auth_basic "kiro admin";
    auth_basic_user_file /etc/nginx/kiro-admin.htpasswd;
    proxy_pass http://127.0.0.1:8787/;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

The trailing slash on `proxy_pass` turns `/kiro/v1/messages` into
`/v1/messages`, which the server accepts. With a prefix, the console's
"接入" page still generates root-based URLs, so add `/kiro` manually when
copying them.

When the console is behind a reverse proxy, set `admin_token` or
`KIRO_ADMIN_TOKEN`. Requests arrive with forwarding headers, so the
"loopback only" rule will not apply.

## Outbound proxy

The upstream HTTP client honors Go's standard proxy environment variables:

- `HTTPS_PROXY`
- `HTTP_PROXY`
- `NO_PROXY`

`NO_PROXY=127.0.0.1,localhost,::1` keeps local health checks and the console
off the proxy. `socks5://user:pass@host:port` is supported.

There is no automatic fallback to a direct connection when the proxy fails.
Requests fail and the pool cools the account. After changing the proxy,
`systemctl daemon-reload` and restart the service.

## Upgrade and backup

Stop the process, replace the binary or pull the new image, then start it
again. Configuration and state files are not migrated by the binary.

Back up:

- `accounts.json` (contains refresh tokens)
- `accounts.json.state.json` (cooldowns and counters, non-secret)
- `keys.json` (only hashes, but keep it)
- `usage.jsonl`
- `promptcache.json` and `promptcache.json.sessions.json`
- `prices.json`
- `kiro-proxy.json`

Protect the backup like a password. The account file contains credentials.

## Next

- [Configuration](configuration.md)
- [API](api.md)
- [Operations and troubleshooting](operations.md)
- [Cache routing](cache.md)
