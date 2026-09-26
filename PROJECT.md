# Project: glm-aki-proxy Defense Standard Upgrade (GLM-ZAI-2API)

## Architecture
- **Upstream Network Layer (`internal/upstream`)**:
  - `tlsclient.go`: Chrome 146 TLS Client using `bogdanfinn/tls-client` and `bogdanfinn/fhttp` (pure Go, CGO-free, JA3/JA4 emulation, HTTP/2 settings, pseudo-header order, 120s timeout).
  - `proxy.go`: Proxy rotation pool with Direct-First policy, dead-proxy 10m TTL isolation, pre-check route `GET /api/models`.
  - `throttle.go`: Pacing gate enforcing `ANTHROPIC_ZAI_MIN_MS` (default 1500ms) with context cancellation awareness.
  - `chat.go`: Upstream chat completions with TLS client, `errors.Is(err, ErrUserBlocked)` account rotation, HTTP 529 `Retry-After: 15` error translation, atomic watchdog.
  - `vision.go`: Multi-part image processing, 50MB payload capacity, 401 upload retry.
- **Session & Token Management (`internal/session`, `internal/pool`)**:
  - `session.go`: Multi-account pool, synchronized `Pool.fe` under mutex, clean adapter for TLS Doer.
- **API & Protocol Layer (`internal/api`)**:
  - `anthropic.go`: Messages API, stream holdback buffer for `<`/`>` chunk edges, 5s SSE `: keep-alive\n\n` ping during thinking, 50MB MaxBytesReader, vision auto-route to `GLM-5v-Turbo`.
  - `responses.go`: OpenAI Responses API (`POST /v1/responses`) for Codex CLI support.
  - `models.go`: Live model catalog from Z.AI with 1h TTL cache and background refresh.
  - `admin.go`: Administrative endpoints (`/admin/health`, `/admin/stats`, `/admin/models`, `/admin/session/clear`).
  - `tools.go`: Preserved existing advanced tool calling contract and auto-repair engine.
- **Entrypoints (`cmd/server`, `api`)**:
  - `cmd/server/main.go`: Standalone HTTP server.
  - `api/index.go`: Vercel Serverless entrypoint (`package handler`) with `sync.Once` lazy initialization.

## Feature Inventory
| # | Feature | Description | Milestone | Source |
|---|---------|-------------|-----------|--------|
| 1 | Chrome 146 TLS Fingerprint | `tlsclient.go` with `profiles.Chrome_146`, HTTP/2 settings, JA3/JA4, 120s timeout | M1 | R1 |
| 2 | Pure Go & Vercel Compatibility | Zero CGO, runs cleanly on both Vercel Serverless and Standalone | M1 | R1 |
| 3 | Proxy Rotation Pool | `proxy.go` with `USE_PROXY`, `PROXY_FILE`, `PROXY_LIST` | M1 | R2 |
| 4 | Direct-First Proxy Routing | Attempt 0 direct, rotate to proxy on 403/405/WAF | M1 | R2 |
| 5 | Dead-Proxy 10m TTL | Isolate dead proxies for 10 minutes before retry | M1 | R2 |
| 6 | Pre-Check Route | Probe `GET /api/models` before burning single-use captcha device token | M1 | R2 |
| 7 | Request Throttle Gate | `throttle.go` pacing requests with `ANTHROPIC_ZAI_MIN_MS` (1500ms) | M1 | R3 |
| 8 | 403 USER_BLOCKED Handling | `errors.Is(err, ErrUserBlocked)` detection, instant token eviction & switch | M1 | R4 |
| 9 | HTTP 529 Retry-After | Emit HTTP 529 with `Retry-After: 15` on pool overload or cooldown | M1 | R4 |
| 10 | Stream Holdback Buffer | Holdback `<`/`>` on SSE chunk edges to prevent tag breaking or leak | M2 | R5 |
| 11 | SSE Keep-Alive Ping | Emit `: keep-alive\n\n` comment every 5s during thinking phase | M2 | R5 |
| 12 | Vision Auto-Route | Switch model to `GLM-5v-Turbo` on image blocks with 401 upload retry | M2 | R6 |
| 13 | 50MB Request Body | Increase `http.MaxBytesReader` to 50 MB for high-res images | M2 | R6 |
| 14 | Watchdog Race Fix | Replace `watchdogFired` bool with `atomic.Bool` | M2 | R6 |
| 15 | Pool.fe Race Fix | Synchronize `Pool.fe` under `p.mu` and propagate to active sessions | M2 | R6 |
| 16 | Script Security Fix | Remove plaintext `AUTH_TOKEN` leak in `install.ps1` and `api.go` | M2 | R6 |
| 17 | Codex POST /v1/responses | Implement `responses.go` with input polymorphism, tool contract, streaming | M3 | R7 |
| 18 | Live Model Catalog | `GET /v1/models`, `/models`, `/api/models` with 1h cache and background refresh | M3 | R7 |
| 19 | Admin Endpoints | `GET /admin/health`, `GET /admin/stats`, `GET /admin/models`, `POST /admin/session/clear` | M3 | R7 |
| 20 | Dual Route Registration | Wire all routes in both `cmd/server/main.go` and `api/index.go` | M3 | R7 |
| 21 | Zero-Regression Test Suite | `go test -v -count=1 ./...` 100% PASS across unit and benchmark tests | M4 | R8 |
| 22 | Dual Target Build Verification | Both `go build ./api/...` and `go build ./cmd/server/...` compile cleanly | M4 | R8 |
| 23 | Working Tree Documentation | Produce exhaustive working tree state and deliverable summary | M4 | R8 |

## Milestones
| # | Name | Scope | Dependencies | Status |
|---|------|-------|-------------|--------|
| 1 | Network & Upstream Security | R1, R2, R3, R4 (`go.mod`, `tlsclient.go`, `proxy.go`, `throttle.go`, `chat.go`, `session.go`) | Survey done | COMPLETED |
| 2 | Streaming Protocol & Concurrency Fixes | R5, R6 (`anthropic.go`, `vision.go`, race fixes, `install.ps1`) | M1 | COMPLETED |
| 3 | Extended Endpoints & Model Catalog | R7 (`responses.go`, `models.go`, `admin.go`, `api/index.go`, `main.go`) | M1, M2 | COMPLETED |
| 4 | Verification & Working Tree Output | R8 (Full test suite pass, dual target build, git working tree) | M1, M2, M3 | COMPLETED |

## Code Layout
- `internal/upstream/tlsclient.go`: Chrome 146 TLS client factory & cache
- `internal/upstream/proxy.go`: Proxy rotation pool & dead TTL tracking
- `internal/upstream/throttle.go`: Pacing gate
- `internal/upstream/chat.go`: Upstream chat completions logic
- `internal/upstream/vision.go`: Image uploads & retry logic
- `internal/session/session.go`: Account pool & HTTP client adapter
- `internal/api/anthropic.go`: Anthropic Messages handler, holdback buffer, keepalive
- `internal/api/responses.go`: Codex CLI responses handler
- `internal/api/models.go`: Model catalog & live cache
- `internal/api/admin.go`: Admin health, stats, models, session clear
- `internal/api/api.go`: Router registration & server struct
- `cmd/server/main.go`: Standalone server main
- `api/index.go`: Vercel serverless entrypoint
- `install.ps1`: Client installation script
