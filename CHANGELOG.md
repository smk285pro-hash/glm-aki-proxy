# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
