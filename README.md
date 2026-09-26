# GLM-Aki-Proxy ⚡

A high-performance **OpenAI Chat Completions** and **Anthropic Messages API** proxy for Z.AI (GLM-4 / GLM-5 models), specifically optimized for **Claude Code CLI (`clglm`)**, **Cursor**, **Codex CLI**, and agentic AI workflows.

---

## ✨ Features

- **Dual API Support**: Seamless compatibility with both Anthropic `/v1/messages` and OpenAI `/v1/chat/completions`.
- **First-Class Tool Calling**: Full support for Anthropic `tool_use`/`tool_result` contracts and multi-step agentic loops without protocol leaking or syntax corruption.
- **Deep Reasoning (Thinking)**: Real-time thinking stream with `glm-5.2` and `glm-5.3` (configurable reasoning effort: `low`, `medium`, `high`, `max`).
- **Multimodal (Vision)**: Automatic image processing via `GLM-5v-Turbo` (supports up to 50MB per image).
- **Built-in Web Chat UI**: Lightweight chat interface at `http://127.0.0.1:5084/` with one-click token harvesting.
- **1-Click Launch**: Double-click `run.bat` on Windows to build, harvest initial tokens, and launch automatically.

---

## 🚀 Quick Start

### Windows (1-Click)

Prerequisites: [Go 1.24+](https://go.dev/dl/) and Google Chrome installed.

1. Clone the repository:
   ```powershell
   git clone https://github.com/smk285pro-hash/glm-aki-proxy.git
   cd glm-aki-proxy
   ```
2. Run **`run.bat`**:
   - Automatically compiles `glm-aki-proxy.exe` and `aki-collect.exe`.
   - Automatically harvests initial device tokens if `tokens.json` does not exist.
   - Opens `http://127.0.0.1:5084` in your browser and starts the server.

---

### Manual Launch (CLI)

```bash
# 1. Build binaries
go build -o glm-aki-proxy.exe ./cmd/server
go build -o aki-collect.exe ./cmd/collect

# 2. Harvest initial device tokens
.\aki-collect.exe --count 50 --out tokens.json

# 3. Start the proxy server
.\glm-aki-proxy.exe
```

- **API Base URL**: `http://127.0.0.1:5084/v1`
- **Web Chat UI**: `http://127.0.0.1:5084/`

---

## 🛠️ Claude Code CLI Setup (`clglm`)

Configure Claude Code CLI to connect to the local proxy with **a single PowerShell command**:

```powershell
irm http://127.0.0.1:5084/install.ps1 | iex
```

This creates an isolated profile in `~/.claude-glm` and adds the **`clglm`** alias to your PowerShell `$PROFILE`.

**Start coding in any project directory:**
```powershell
clglm
```

> **Tip:** Inside Claude Code CLI, type `/model` to quickly switch between GLM models.

---

## 💻 Cursor & OpenAI SDK Integration

In Cursor or any OpenAI-compatible client, configure:
- **Base URL**: `http://127.0.0.1:5084/v1`
- **API Key**: `aki-local-key` (or your custom `AUTH_TOKEN` from `.env`)
- **Model**: `glm-5.3`, `glm-5.2`, `glm-4.7`, `GLM-5-Turbo`, `GLM-5v-Turbo`

### Test with cURL:
```bash
curl http://127.0.0.1:5084/v1/chat/completions \
  -H "Authorization: Bearer aki-local-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"glm-5.3","messages":[{"role":"user","content":"Hello!"}],"stream":false}'
```

---

## 📊 Supported Models

| Model ID | Role | Thinking / Reasoning | Vision |
|---|---|:---:|:---:|
| `glm-5.3` / `claude-opus-gl-5.3` | Flagship Deep Reasoning & Architecture | ✅ Effort: Low / Med / High / Max | ❌ |
| `glm-5.2` / `claude-sonnet-gl-5.2` | Fast Coding & Agentic Loops | ✅ | ❌ |
| `glm-4.7` / `claude-haiku-gl-4.7` | High-speed, Lightweight & Tool Execution | ❌ | ❌ |
| `GLM-5-Turbo` | Ultra-low Latency Inference | ❌ | ❌ |
| `GLM-5v-Turbo` | Multimodal Vision & Code Analysis | ❌ | ✅ Up to 50MB/image |

---

## ⚡ Extra Parameters

| Field | Values | Description |
|---|---|---|
| `webSearch` / `search` | `true` / `false` | Enable/disable upstream web search |
| `deepThink` | `true` / `false` | Enable/disable deep reasoning (thinking) mode |
| `reasoning_effort` | `low` / `medium` / `high` / `max` | Control thinking depth (`glm-5.2`, `glm-5.3`) |

---

## 📁 Project Structure

```
├── run.bat                 # 1-Click launcher for Windows
├── install.ps1             # 1-Line installer for clglm command (PowerShell)
├── cmd/
│   ├── server/             # Standalone proxy HTTP server
│   └── collect/            # Headless Chrome device token harvester
├── internal/
│   ├── api/                # HTTP handlers (OpenAI, Anthropic, Admin, Harvest)
│   │   └── web/            # Embedded Web Chat UI & install scripts
│   ├── pool/               # Token pool manager (Disk file / Upstash / Redis)
│   ├── session/            # Z.AI session lifecycle & HMAC request signing
│   ├── upstream/           # Upstream chat request builder & SSE parser
│   └── captcha/            # Aliyun traceless captcha crypto engine
└── api/
    └── index.go            # Vercel Serverless entrypoint (optional)
```

---

## ⚖️ License

For personal, research, and educational purposes only.
