# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.1.0] - 2026-09-27

### Added
- Automatic Cloudflare WARP IP rotation trigger on repeated WAF 405 blocks (`internal/upstream/warp.go`).
- Multi-proxy rotation pool with Direct-First policy and dead proxy isolation (`proxy.txt` and `USE_PROXY`).
- Dynamic token harvest quantity input prompt on the Web Chat UI supporting up to 2000 tokens per batch.

### Changed
- Increased default request pacing interval (`DefaultMinGap`) to 2800ms with random jitter to stay safely under Aliyun ESA WAF rate limits.
- Implemented progressive exponential backoff (up to 20s) on repeated WAF 405 blocks to allow Alibaba Cloud sliding penalty windows to clear.

### Fixed
- Eliminated 30s/45s account cooldown lock on WAF 405 error, enabling smooth cyclic rotation across all sessions without premature 529 aborts.
- Aligned Chrome 146 HTTP headers (`sec-ch-ua`, `origin`, `referer`, `accept`) and unescaped JSON encoding to prevent security challenges.
- Handled `USER_BLOCKED` via `errors.Is` comparison and improved proxy pool initialization on server startup.

## [1.0.0] - 2026-09-27

### Added
- Standalone 1-click launcher (`run.bat`) for Windows with automatic binary compilation and initialization.
- In-app device token harvester button on the Web Chat UI hitting `POST /api/harvest` to collect tokens in background.
- Dual API protocol support: Anthropic Messages API (`/v1/messages`) and OpenAI Chat Completions API (`/v1/chat/completions`).
- Full Anthropic Tool Calling contract with tool argument validation, auto-repair, and protocol leak prevention.
- Real-time deep reasoning stream (thinking phase) for `glm-5.2` and `glm-5.3` models with configurable effort levels.
- Multimodal vision pipeline supporting image blocks up to 50MB with automatic routing to `GLM-5v-Turbo`.
- Chrome 146 TLS fingerprint impersonation (`bogdanfinn/tls-client`) to prevent Aliyun WAF detection.
- Universal PowerShell 1-line installation script (`install.ps1`) configuring isolated `clglm` command for Claude Code CLI.
- Web Chat interface embedded into the binary at `http://127.0.0.1:5084/`.
- Trailing slash URL normalization in HTTP router to ensure seamless compatibility with Claude Desktop App.
- Intelligent WAF 405 auto-fallback to GLM-5.2 / GLM-5-Turbo with extended retries to prevent subagent loop failures.
- Increased default request pacing gap to 2000ms for robust resistance against Aliyun ESA WAF rate limits.
