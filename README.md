# isann-servers

Open-source iSANN central servers you can self-host. Two independent servers:

- **market** — an **asset store**. Developers publish iSANN assets (presets,
  recipes, apps, models…) here, signed with their wallet; others browse, search,
  and install them. It stores small signed `install.ian` recipes + metadata, not
  the heavy payloads (those stay on GHCR/HF/URLs). Plain HTTP API on `:8820`.

- **rendezvous** (rv) — a **meeting point for P2P**. When two nodes are behind
  NAT/firewalls, they register with the RV, which introduces them and coordinates
  a hole-punch so they connect **directly**. Only the introduction goes through
  the RV — the actual data flows node-to-node. `:9100` (TCP + UDP).

  > ⚠️ **rv must run with `network_mode: host`** — hole-punching needs the RV to
  > see and advertise each peer's *real* public IP:port. Docker's bridge network
  > would hide that behind a `172.x` address and rewrite UDP source ports, adding
  > a second NAT layer in front of the one you're trying to punch through. market
  > has no such constraint (plain HTTP → bridged ports are fine).

> `gate` (public RV directory) is iSANN-operated central infrastructure and is **not** part of this repo.

Module: `github.com/isannai/isann-servers`. Shared crypto/wire code (`pkg/auth`) lives here once — both servers use it.

## Getting started

From a fresh Linux host to a running server. Everything builds **inside Docker**,
so you only need `git` and `docker` — no Go toolchain.

### 1. Install git + Docker

**Ubuntu / Debian**

```bash
sudo apt update
sudo apt install -y git

# Docker Engine + Compose plugin (official convenience script)
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker "$USER"      # run docker without sudo (re-login after)
```

Verify:

```bash
git --version
docker --version
docker compose version
```

> Other distros / macOS / Windows: install **Docker Desktop** (bundles Compose)
> from <https://docs.docker.com/get-docker/>, and `git` from your package manager.

### 2. Clone

```bash
git clone https://github.com/isannai/isann-servers.git
cd isann-servers
```

### 3. Start a server

Each server is a standalone compose under `deploy/<svc>/`. Pick what you need:

```bash
# market — asset registry (HTTP :8820)
cd deploy/market
docker compose up -d --build

# rendezvous — NAT coordination (:9100 TCP+UDP, host network)
cd ../rendezvous
docker compose up -d --build
```

First run downloads the Go build image and compiles inside the container
(a minute or two); later runs are cached. TLS certs are **auto-generated**
on first boot when `tls.enabled` and none are present.

### 4. Verify

```bash
curl http://localhost:8820/health          # market  → {"status":"ok",...}
docker compose logs -f                      # follow logs (run in the svc dir)
docker compose ps                           # status
```

### 5. Stop / update

```bash
docker compose down                         # stop (run in the svc dir)
git pull && docker compose up -d --build    # update to latest
```

Edit `deploy/<svc>/conf/*.json` to change ports, TLS, DB, etc. (see **Config**),
then `docker compose up -d --build` again.

## Build

Binaries land in `build/out/<os>/`. Version is stamped via `-ldflags` (2nd-arg optional).

```bash
./build/linux/build.sh 0.1.0            # linux/amd64 static → build/out/linux/{market,rendezvous}
build\windows\build.bat 0.1.0           # windows/amd64      → build/out/windows/{market.exe,rendezvous.exe}
# both scripts also take: market | rendezvous | all
```

## Run (deploy — Docker)

Each server has a standalone compose under `deploy/<svc>/`. It **builds inside
Docker** (multi-stage: `golang` build stage → tiny `distroless` runtime) — no
host Go toolchain, no prebuilt binary:

```bash
cd deploy/market      && docker compose up -d --build   # HTTP :8820 (bridged)
cd deploy/rendezvous  && docker compose up -d --build   # :9100 TCP+UDP (host network — required)
curl http://localhost:8820/health
```

Per-server config is in `deploy/<svc>/conf/`; the market SQLite DB persists in
`deploy/market/db/`, logs in `deploy/<svc>/logs/`, TLS certs in `deploy/<svc>/certs/`.

## Run (dev / verify — native)

Build for your OS and run the binary directly — no Docker:

```bash
build\windows\build.bat                 # → build\out\windows\market.exe
build\out\windows\market.exe --config deploy\market\conf\market.json
```

## Config

**conf/market.json**

| field | default | notes |
|---|---|---|
| `addr` | `:8820` | listen address |
| `db.driver` | `sqlite` | `sqlite` (dev) / `mysql` (prod) |
| `db.dsn` | `/db/market.db` | SQLite path or MySQL DSN |
| `tls.{enabled,cert,key}` | off | HTTPS |
| `replay_window_sec` | `300` | write-signature timestamp tolerance |

> `dev_insecure_skip_auth: true` disables write-signature checks — **local dev only**.

**conf/rendezvous.json**

| field | default | notes |
|---|---|---|
| `unified_addr` | `:9100` | control (TCP) + punch (UDP) on one port |
| `rest_addr` | — | optional REST API (e.g. `:9000`) |
| `tls.{enabled,cert,key}` | off | HTTPS |

## Layout

```
cmd/market/          market entrypoint
cmd/rendezvous/      rv entrypoint
pkg/market/          registry store + HTTP API
pkg/rendezvous/      RV server
pkg/auth/            EIP-191 signing / recover (shared)
pkg/tunnel/ signal/  RV networking
pkg/recipe/          install.ian parser (market metadata)
pkg/glog/ setup/     logging / version
build/{windows,linux}/  build scripts → build/out/<os>/
deploy/<svc>/        standalone docker-compose (mounts build/out/linux/<bin>) + conf/
```

## TLS

With `tls.enabled: true`, the server **auto-generates a self-signed cert on first
boot** if `certs/cert.pem` / `certs/key.pem` are missing (pure Go, no openssl) —
so `docker compose up` just works. They land in the mounted `deploy/<svc>/certs/`
and persist. Private keys are gitignored — never committed.

Production: drop your real certs (Let's Encrypt / your CA) into `deploy/<svc>/certs/`
and the auto-gen is skipped. `deploy/gen-certs.sh` can also pre-generate them.

## License

TODO
