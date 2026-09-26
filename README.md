# glm-aki-proxy

Local OpenAI-compatible proxy in front of Z.AI web chat (GLM models).
Single Go binary + a small device-token collector. No copied code:
every file in this repo was written fresh for this project.

## How it works

```
client (OpenAI SDK / Claude Code)
  -> GET  /v1/models
  -> POST /v1/chat/completions   (stream or not, tool calling)
  -> POST /v1/messages           (Anthropic shim for Claude Code:
     text + thinking + tool_use/tool_result; the CLI runs the
     agentic loop, the server translates)
  -> GET  /status
        |
        v
upstream https://chat.z.ai/api/v2/chat/completions
  + X-Signature (HMAC-SHA256 per request)
  + X-FE-Version (scraped from homepage)
  + captcha_verify_param (Aliyun traceless verification,
    minted in-process from one single-use device token)
```

Device tokens come from a real desktop Chrome session, collected by
`cmd/collect`: it opens chat.z.ai, pokes the chat box once so the page
injects its captcha SDK, then calls the SDK's token minter N times and
saves the pool to `tokens.json`. Each token is burned after one use.

## Quick start

Requires Go 1.24+ and desktop Google Chrome (collector only).

```bash
# 1. build
go build -o glm-aki-proxy.exe ./cmd/server
go build -o aki-collect.exe ./cmd/collect

# 2. collect device tokens (first run fetches the playwright driver once)
.\aki-collect.exe --count 100 --out tokens.json

# 3. configure
copy .env.example .env
# edit .env: AUTH_TOKEN, ZAI_TOKEN (login JWT; empty = guest)

# 4. run
.\glm-aki-proxy.exe
# API at http://127.0.0.1:5084
# Web chat UI at http://127.0.0.1:5084/ (same origin, no CORS issues)
```

```bash
curl http://127.0.0.1:5084/v1/chat/completions \
  -H "Authorization: Bearer aki-local-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}],"stream":false}'
```

## Claude Code CLI

The server speaks enough of the Anthropic Messages API for the CLI
to connect, including tool calling. Z.AI has no native function
calling, so `tools` become a strict marker contract in the prompt
(see `internal/api/tools.go` — fresh implementation following the
GLM-ZAI-2API approach); marker blocks in the reply are parsed back
into `tool_use`, with `stop_reason: tool_use`. The CLI runs the
agentic loop itself; the server only translates.
Reasoning streams as Anthropic `thinking` blocks with an unsigned
placeholder signature (`unsigned-glm-aki-proxy`); echoed thinking
blocks are flattened back to text on the next turn.
Answer text and tool calls follow after the upstream turn completes so
the Anthropic SSE blocks remain in order even when Z.AI sends late
reasoning or malformed text-format calls.
In Claude Desktop Code, the Normal transcript view hides thinking even
when the proxy sends it. Choose **Thinking** or **Verbose** in the
Transcript view dropdown near Send, or press `Ctrl+O` to cycle modes.
The Thinking option appears once the session has produced thinking
([Claude Desktop docs](https://code.claude.com/docs/en/desktop#switch-view-modes)).
Any model id starting with `glm` passes through, anything else
falls back to `glm-4.7`. `system`-role messages are accepted.

NOTE: Claude Code reads `env` from `~/.claude/settings.json`, which
wins over shell environment. Point it at this proxy there:

```json
"env": {
  "ANTHROPIC_BASE_URL": "http://127.0.0.1:5084",
  "ANTHROPIC_AUTH_TOKEN": "aki-local-key"
}
```

## Extra request fields

| Field | Effect |
|---|---|
| `webSearch` / `search` | `true` — upstream web search on; `false` — forced off |
| `deepThink` | Override the thinking toggle (`true`/`false`) |
| `reasoning_effort` (`low`/`high`/`max`) | Effort level on `glm-5.2`/`glm-5.3` only (ignored elsewhere, forces thinking on) |

Accepted on both `/v1/chat/completions` and `/v1/messages`.

## Vision (image input)

OpenAI `image_url` parts (URL or `data:` base64) — on `/v1/messages`
use Anthropic `image` blocks, translated automatically. Each image is
uploaded to Z.AI and referenced as a file, mirroring the web client.
Requires a logged-in `ZAI_TOKEN` (guest upload → 401); works best with
`GLM-5v-Turbo`. Max 10 images/request, 50 MB each. Note: image URLs are
fetched server-side — fine for local personal use, reconsider before
exposing the server to a network.

```bash
curl http://127.0.0.1:5084/v1/chat/completions \
  -H "Authorization: Bearer aki-local-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"GLM-5v-Turbo","messages":[{"role":"user","content":[
        {"type":"text","text":"What is on this image?"},
        {"type":"image_url","image_url":{"url":"data:image/png;base64,..."}}]}]}'
```

## Deploy to Vercel (Upstash Redis)

Triển khai serverless lên Vercel sử dụng Upstash Redis làm kho lưu trữ token tập trung:

1. **Tạo Upstash Redis database (miễn phí)**:
   - Truy cập [console.upstash.com](https://console.upstash.com) và tạo một database Redis (chọn vùng gần nhất, ví dụ Singapore).
   - Trong mục **REST API**, sao chép `UPSTASH_REDIS_REST_URL` và `UPSTASH_REDIS_REST_TOKEN`.

2. **Nạp token thiết bị lên Redis từ máy cá nhân**:
   - Thêm `UPSTASH_REDIS_REST_URL` và `UPSTASH_REDIS_REST_TOKEN` vào file `.env` ở máy local.
   - Đẩy các token có sẵn lên Redis:
     ```powershell
     .\aki-collect.exe -push-only -file tokens.json
     ```
   - Hoặc thu thập thêm token mới và tự động đẩy thẳng lên Redis:
     ```powershell
     .\aki-collect.exe -count 100 -push-redis
     ```

3. **Deploy lên Vercel**:
   - Đẩy repo lên GitHub (hoặc dùng Vercel CLI: `vercel`).
   - Trên Vercel Project Settings > **Environment Variables**, cấu hình các biến sau:
     - `AUTH_TOKEN`: API key bảo vệ server (ví dụ `your-secret-key`)
     - `ZAI_TOKEN`: JWT đăng nhập Z.AI (tùy chọn; để trống sẽ dùng guest mode)
     - `UPSTASH_REDIS_REST_URL`: URL Upstash của bạn
     - `UPSTASH_REDIS_REST_TOKEN`: Token Upstash của bạn
   - Bấm **Redeploy**. API proxy sẽ sẵn sàng tại `https://your-project.vercel.app/v1/chat/completions`.

## Layout

```
api/index.go    Vercel Serverless entrypoint
cmd/server      HTTP entry point (standalone)
cmd/collect     device-token collector (headless Chrome)
internal/config env + .env loading
internal/util   uuid, base64, small helpers
internal/session  Z.AI session: guest/login init, JWT decode,
                  homepage version scrape, per-request signature
internal/captcha  Aliyun verification: device-data AES,
                  init/verify RPC signing, track payload cipher
internal/pool   token pool (Upstash REST / Redis / JSON file fallback)
internal/upstream chat request builder + SSE response parser,
                  per-request options, vision upload pipeline
internal/api    OpenAI-compatible HTTP handlers
            (+ embedded web chat UI at GET /,
             Anthropic shim, tool-call adapter)
```

## Notes

- Web chat history on chat.z.ai stays empty by design: every session
  uses a random chat id and lives only in this server's memory.
- Conversation memory: `/v1/messages` is stateless (Anthropic clients
  resend full history; server-side accumulation would duplicate it and
  explode input size). `/v1/chat/completions` keeps server memory only
  when you send `X-Session-Id` (the web UI always does); without it the
  endpoint is stateless too. Idle sessions are reaped after 30 minutes.
- `tokens.json` holds single-use secrets, keep it out of git.
- For study/personal use. Respect Z.AI's terms of service.
