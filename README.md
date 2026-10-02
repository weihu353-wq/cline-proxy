# Cline Proxy

## Reliability fork

This fork keeps upstream history on `main` and the reviewed r3 changes on
[`codex/cline-reliability-r3`](https://github.com/weihu353-wq/cline-proxy/tree/codex/cline-reliability-r3).
See [fork maintenance](docs/fork-maintenance.md) for the source baseline,
error/streaming changes, validation commands and `Dockerfile.reliability`.

A single Go binary that turns free upstream LLM quotas (Cline accounts + opencode zen free models) into one clean **OpenAI-compatible `/v1` gateway** for coding IDEs — Cursor, ZCode, Cline, Claude Code, OpenClaw — with account pooling, per-request proxy rotation, and an English admin panel.

Fork-in-progress of [YuJunZhiXue/Cline-proxy](https://github.com/YuJunZhiXue/Cline-proxy), reworked for solo public deployment. This project is not affiliated with Cline or opencode; use their free tiers respectfully.

## What it does

```
Cursor / ZCode / Cline / Claude Code ──►  cline-proxy  ──►  cline account pool (round-robin)
   /v1/chat/completions                        │
   /v1/messages (Anthropic)                    ├─►  opencode zen free models (multi-key, proxy pool)
   /v1/responses (OpenAI Responses)            │
   /v1/models                                  └─►  egress socks5/http proxy pool (per-request rotation)
```

- **One endpoint, both upstreams.** The gateway inspects the requested `model` and routes it: zen free models (`mimo-v2.5-free`, `nemotron-3-ultra-free`, …) go to opencode zen, paid zen models are rejected with a clear 400, everything else goes to the Cline account pool. Combos let you define your own alias model IDs on either platform.
- **Three client dialects, one upstream language.** Both upstreams are OpenAI chat-completions servers. `/v1/chat/completions` is a near-passthrough; `/v1/responses` and `/v1/messages` (Anthropic) are translated in and converted back, including streaming events, tool calls, and usage.
- **IDE-controlled parameters are respected.** `max_tokens`, `temperature`, `top_p`, `stop`, `tools`, `tool_choice`, `reasoning_effort`, … all pass through from the client. The gateway only fills gaps (128k default output budget when the client sends none) and enforces semantics upstreams get wrong: `tool_choice: "none"` is enforced by stripping tools, unknown model names return a clean 400 (`STRICT_MODEL_MATCH=false` restores the old catch-all), and `stop` sequences are deterministically truncated client-side for non-stream responses.
- **Pool everything.** Cline accounts round-robin with automatic 429 cooldown ("Try again in 17h 59m" parsing) and auto-recovery. Zen keys rotate the same way. Egress proxies rotate per request so IP-based rate limits don't bottleneck one address; proxy failures never poison account state.
- **Function calling that actually works.** Battle-tested against live upstreams across chat stream/non-stream, parallel tool calls, `/v1/responses` and Anthropic streaming, with repair logic for upstream tool-argument quirks. See the test matrix below.
- **English admin panel** at `/admin/`: Dashboard, Accounts, Add accounts (OAuth / refreshToken / static `sk_` API key — auto-detected, also batch), Gateway settings (API keys, models, headers), **Proxy pool** (one shared socks5/http egress list with per-upstream toggles for Cline and zen), opencode free models, Combos, request logs, live stats.

## Quick start (Docker)

```bash
docker run -d --name cline-proxy \
  -p 3457:3457 \
  -v cline-proxy-data:/app/data \
  -e PORT=3457 \
  -e API_KEY=change-me \
  -e ADMIN_PASSWORD=change-me-too \
  ghcr.io/foxy1402/cline-proxy:latest

curl http://127.0.0.1:3457/health
```

Then open `http://127.0.0.1:3457/admin/`, log in with `ADMIN_PASSWORD`, and add accounts or zen keys. The image is multi-arch (`linux/amd64`, `linux/arm64`); `:latest` tracks `main`, version tags (`vX.Y.Z`) and `:sha-xxxxxx` tags are also published.

> The image name follows the GitHub repository name. If your repo is named differently, replace `cline-proxy` with the repo name (GHCR images are always lowercase).

## Portainer stack template

Paste into Portainer → **Stacks → Add stack**, fill in the environment variables in the UI (Portainer interpolates them), then deploy:

```yaml
services:
  cline-proxy:
    image: ghcr.io/foxy1402/cline-proxy:latest
    container_name: cline-proxy
    restart: unless-stopped
    ports:
      - "${PROXY_PORT:-3457}:${PROXY_PORT:-3457}"
    volumes:
      - cline-proxy-data:/app/data
    environment:
      - PORT=${PROXY_PORT:-3457}
      - API_KEY=${API_KEY:?set API_KEY in the stack environment}
      - ADMIN_PASSWORD=${ADMIN_PASSWORD:?set ADMIN_PASSWORD in the stack environment}
      - ZEN_KEYS=${ZEN_KEYS:-}
      - CLINE_ACCOUNTS_SEED_FILE=/app/data/cline-seed.json
      - STRICT_MODEL_MATCH=true
      # optional — session-remint interval in hours (default 4; must stay under the 5h quota window)
      - ZEN_HARVEST_INTERVAL_HOURS=${ZEN_HARVEST_INTERVAL_HOURS:-4}
      # optional — keep at 1 unless the box has spare cores (the CLI bursts CPU per run)
      - ZEN_HARVEST_CONCURRENCY=${ZEN_HARVEST_CONCURRENCY:-1}
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:${PROXY_PORT:-3457}/health"]
      interval: 30s
      timeout: 5s
      start_period: 10s
      retries: 3

volumes:
  cline-proxy-data:
```

Environment variables to define in the Portainer stack UI:

| Variable | Required | Example |
|---|---|---|
| `API_KEY` | yes | any long random string — the only key accepted on `/v1/*` |
| `ADMIN_PASSWORD` | yes | admin panel login password |
| `PROXY_PORT` | no | defaults to `3457`; changes both the container listen port and the host mapping |
| `ZEN_KEYS` | no | comma-separated opencode zen keys; leave empty to use the anonymous `public` key or configure in the admin panel |
| `ZEN_HARVEST_INTERVAL_HOURS` | no | how often zen sessions are re-minted, in hours; defaults to `4`. Keep it **below 5** (see [session harvester](#opencode-zen-session-harvester)) |
| `ZEN_HARVEST_CONCURRENCY` | no | how many keys mint at once; defaults to `1` (serial), which is what small instances want |

To seed Cline accounts on first boot, drop a `cline-seed.json` file into the volume (see [Seeding accounts](#seeding-accounts)).

> **Bind-mount note (Portainer/NAS users):** the container runs as a non-root user, so a **named volume** (`-v cline-proxy-data:/app/data`) is preferred. If you bind-mount a host directory (e.g. `-v /opt/cline-proxy-data:/app/data`), pre-create it and chown it to UID 1000 (`chown -R 1000:1000 /opt/cline-proxy-data`) or the gateway cannot write its state files and will fail to persist accounts.

## Configuration

All state lives in the `/app/data` volume (`cline-accounts.json`, `zen-config.json`, `combos.json`, `requests.jsonl`). Backup = copy the volume.

| Variable | Default | Description |
|---|---|---|
| `PORT` | `3457` | Listen port (`-port` flag wins over the env var) |
| `DATA_DIR` | `/app/data` | State directory inside the container |
| `API_KEY` / `API_KEY_FILE` | empty | The single valid `/v1` key (via `Authorization: Bearer` or `x-api-key`). When set, admin-panel-generated keys are ignored for `/v1` |
| `ADMIN_PASSWORD` / `ADMIN_PASSWORD_FILE` | empty | Admin panel password; when set, every `/admin/*` route requires login |
| `REQUIRE_ADMIN_AUTH` | `true` | Set `false` only when a reverse proxy already handles auth |
| `PROXY_PORT` | — | Compose/Portainer convenience: sets host mapping + `PORT` together |
| `STRICT_MODEL_MATCH` | `true` | `400` for unknown model names instead of silently serving the default model |
| `POOL_STRATEGY` | `round_robin` | Cline account strategy: `round_robin` / `fill` / `random` (env wins over panel config) |
| `ZEN_KEYS` | empty | opencode zen keys, comma-separated; panel config is not overwritten when it already has keys |
| `CLINE_ACCOUNTS_SEED_FILE` | empty | Seed JSON imported at boot when the pool is empty |
| `CLINE_USE_PROXIES` | `false` | Route the Cline upstream through the egress proxy pool |
| `LOG_REQUESTS` | `true` | Request logging (metadata only: IP, path, model, status, duration — never conversation content) |
| `LOG_FILE_MAX_MB` | `10` | `requests.jsonl` size cap; wiped when exceeded |
| `MAX_BODY_MB` | `32` | Request body limit; larger bodies get `413` |
| `APPLY_SYSTEM_PROMPT_OVERRIDE` | `false` | `true` enables replacing client system prompts with `override.md` |
| `STREAM_LOG` | `false` | Dump raw Anthropic-path SSE to disk (full conversations — debugging only) |
| `CLIENT_IP_HEADER` | empty | Trust this header for client IP behind a reverse proxy (e.g. `X-Real-IP`); by default only `RemoteAddr` is used |

Fail-closed startup: binding a non-loopback address without `API_KEY` and `ADMIN_PASSWORD` refuses to start.

### opencode zen session harvester

The zen free tier only accepts session IDs the upstream has actually seen, minted by the bundled `opencode` CLI. The gateway mints them for you at boot, on repeated `403`s, and on a timer — all of it configurable:

| Variable | Default | Description |
|---|---|---|
| `ZEN_HARVEST` | `1` | `0` disables the harvester entirely (gateway-only mode). It also self-disables when the CLI binary is absent |
| `ZEN_HARVEST_INTERVAL_HOURS` | `4` | **How old a session may get before the periodic pass re-mints it.** Integer ≥ 1. The pass runs every 10 minutes and mints any key whose session is older than this |
| `ZEN_HARVEST_CONCURRENCY` | `1` | How many keys may mint at the same time (1–8). Keep at `1` on small instances: the CLI is a Bun binary that bursts CPU and memory per run, and concurrent runs starve each other past the budget — the symptom is *every* key logging `no session minted`. Raise to `2`–`3` only with spare cores |
| `ZEN_HARVEST_KEY_TIMEOUT_SECONDS` | `150` | Total budget for one key (minimum 30s). Bounds the failure path, and feeds the batch-timeout calculation |
| `ZEN_HARVEST_BIN` | `/app/bin/opencode` | CLI binary path (point it at your own install if you don't use the bundled one) |
| `ZEN_HARVEST_HOME` | `/app/.opencode-home` | CLI `HOME`; each key gets its own hashed subdirectory under it |
| `ZEN_PIN_KEY` | empty | Troubleshooting: pin all upstream attempts to key number *n* (1-based) instead of rotating, so one key can be tested in isolation |

**Setting your own remint interval.** The default is 4 hours, chosen to stay inside zen's ~5-hour quota window. To remint every 2 hours:

```yaml
    environment:
      - ZEN_HARVEST_INTERVAL_HOURS=2      # any integer >= 1
```

or on the command line:

```bash
docker run -d --name cline-proxy -p 3457:3457\
  -v cline-proxy-data:/app/data\
  -e API_KEY=... -e ADMIN_PASSWORD=... -e ZEN_KEYS=...\
  -e ZEN_HARVEST_INTERVAL_HOURS=2\
  ghcr.io/foxy1402/cline-proxy:latest
```

Two things to know before you change it:

- **Keep it under 5 hours.** zen's free quota (~200 requests / 5h) is accounted per **egress IP**, and re-minting does **not** refill it. A longer interval buys no capacity; it only risks a stretch where every session has expired — the refresh interval must stay below the quota window, not above it.
- **Anything invalid falls back to 4** — empty, non-numeric, `0`, or negative. The effective floor is 1 hour, and the parser is lenient about trailing text: `2h` is read as `2`, not rejected.

The admin panel's opencode tab shows the current interval, per-key session age and liveness, and has *Mint missing sessions* / *Force mint / refresh all* buttons plus a per-key **Test** button with a probe-model picker (default **auto**: big-pickle first, then live-synced models; any free zen model can be chosen): it sends one real probe request pinned to that key, and on success clears the key's cooldown immediately (a rate-limited answer — 429, or a 403/503 whose body zen words as a limit — reports the upstream's expected recovery time instead). Note a successful probe consumes one request of that key's quota — the same trade-off as the cline Test button. Full mechanics — quota model, per-key CLI isolation, batch timeouts — are in [docs/zen-harvester.md](docs/zen-harvester.md).

## Connecting your IDE

**Cursor / ZCode / anything OpenAI-flavored** — `POST /v1/chat/completions`:

```
Base URL:  http://<host>:3457/v1
API Key:   <API_KEY>
Model:     cline-free/deepseek-v4.1-flash   (cline pool)
           mimo-v2.5-free                    (opencode zen)
```

**Claude Code / Cline** — Anthropic dialect `POST /v1/messages`, same base URL and key.

**OpenAI Responses dialect** — `POST /v1/responses` (tool calls, streaming events, and usage are fully translated).

`GET /v1/models` lists every model the gateway can serve, including your Combos. Free-model feeds sync automatically (cline official feed every 60s, zen catalog every 10min): the live list is authoritative and the built-in seeds only cover a cold start / offline boot, so delisted or newly-freed models appear or disappear on their own.

### Seeding accounts

Two account kinds are supported and can be mixed:

```json
[
  {"refreshToken": "workos-oauth-refresh-token", "email": "acc1@example.com"},
  {"apiToken": "sk_...", "email": "acc2@example.com"}
]
```

Mount the file anywhere in the container and point `CLINE_ACCOUNTS_SEED_FILE` at it; it's imported when the pool is empty. Static `sk_` API keys are used directly as Bearer tokens (no refresh; a 401 marks them expired). OAuth accounts can also be added via the panel's browser login flow, manual token paste, or batch import.

### Combos (alias models)

Create user-defined alias IDs in the panel (e.g. `my-cline-flash` → `cline-free/deepseek-v4.1-flash` on the cline platform, or any zen free model). Strictly same-platform targets; aliases show up in `/v1/models` so IDEs can pick them directly.

## Battle-tested

Verified against live upstreams with real free-tier credentials (16-probe chat matrix + 12-probe function-calling matrix, all passing):

- Streaming shape (role chunk, `[DONE]`, `finish_reason` `tool_calls` vs `stop`), full tool round-trips, parallel tool calls with distinct indices/ids
- `stream_options.include_usage`, array content parts, long multi-turn histories, parameter passthrough, clean 4xx errors
- Client aborts propagate (no account/key cooldown pollution), 8-way parallel load, container healthcheck, seed import, key rotation

Known upstream quirks (not gateway bugs): zen's `muse-spark-*-free` models only work on the native `/v1/responses` endpoint (the gateway routes them there automatically and re-emits chat/Anthropic shapes); `deepseek-v4-flash-free` was removed from the catalog as deprecated; cline's `stop` handling wipes content when the model's reasoning echoes the stop word (gateway truncates non-stream output as compensation); some reasoning-heavy models eat small `max_tokens` budgets before producing visible text. For zen deployment details (session harvester, env vars) see [docs/zen-harvester.md](docs/zen-harvester.md).

## Development

```bash
go build ./... && go vet ./...
./cline-proxy -host 127.0.0.1 -port 3457   # or: go run . 
./start.sh                                  # build-or-docker wrapper
docker compose up -d --build                # build from source (PROXY_PORT to change the port)
```

One more env var is dev-only and deliberately off by default: `KILL_PORT_ON_START=true` makes the gateway force-kill whatever holds the listen port before binding (a Windows convenience, implemented with `Stop-Process`). Leave it unset in production — it kills a process it did not start, and that may be a legitimate service.

CI: every push to `main` runs `go build` + `go vet`, then publishes the multi-arch image to GHCR via Buildx (amd64 compiled natively, arm64 cross-compiled via `TARGETARCH`; only the embedded opencode CLI stage runs under QEMU for arm64, at build time — the images themselves are native on both arches). Tag a release with `v*` to publish `:vX.Y.Z` alongside `:latest`.

Project layout:

```
├── main.go                  entry point, CLI flags, running detection
├── internal/app/
│   ├── proxy.go             /v1 routing, chat handler, upstream calls, aggregation
│   ├── responses.go         /v1/responses dialect translation
│   ├── zen.go               opencode zen upstream, routing, rate-limit defense
│   ├── zen_session.go       sticky zen sessions (CLI-minted sess_, per-key identity)
│   ├── zen_harvest.go       session harvester (embedded opencode CLI, self-maintaining)
│   ├── zen_endpoint.go      endpoint auto-learn (chat vs /v1/responses per model)
│   ├── tls_bun.go           uTLS ClientHello mimicry for the zen upstream
│   ├── compact.go           opencode-style context compaction for zen free models
│   ├── proxy_pool.go        egress proxy pool (uTLS/h1 client cache, per-request rotation)
│   ├── models.go            cline free-model feed sync + default model
│   ├── pool.go              cline account pool (OAuth refresh, static API keys, cooldowns)
│   ├── seed.go              boot-time account seeding
│   ├── combos.go            alias model IDs
│   ├── admin.go/_auth/_html/_zen  admin panel: API, session auth, UI, zen page
│   ├── config.go            env-driven configuration (fail-closed checks)
│   ├── logs.go / stats.go   request logging + token statistics
│   └── types.go             shared data structures
├── internal/cline/          cline upstream auth (WorkOS OAuth refresh)
├── internal/kit/            HTTP client, random IDs, data paths
├── Dockerfile               multi-arch (amd64 native + arm64 cross-compile)
└── docker-compose.yml       source build, PROXY_PORT-parameterized
```

## Credits

Built on [YuJunZhiXue/Cline-proxy](https://github.com/YuJunZhiXue/Cline-proxy). Thanks to the [LINUX DO](https://linux.do) community.
