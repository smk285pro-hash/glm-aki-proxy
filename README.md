# GLM-Aki-Proxy ⚡

OpenAI & Anthropic Messages API Proxy chuyển đổi Z.AI (GLM-4 / GLM-5) phục vụ **Claude Code CLI (`clglm`)**, **Cursor**, **Codex CLI** và các Agentic AI tools.

---

## ✨ Tính năng chính

- **Tương thích chuẩn API kép**: Hỗ trợ đồng thời OpenAI `/v1/chat/completions` và Anthropic `/v1/messages`.
- **Hỗ trợ Agentic Loop & Tool Calling**: Chuyển đổi tool calls chuẩn Anthropic `tool_use`/`tool_result`, không rò rỉ cú pháp/marker ra văn bản hiển thị.
- **Hỗ trợ Deep Reasoning (Thinking)**: Stream thinking real-time với các model `glm-5.2` và `glm-5.3` (tùy chỉnh effort `low`/`medium`/`high`/`max`).
- **Hỗ trợ Vision (Multimodal)**: Tự động tiếp nhận ảnh qua `GLM-5v-Turbo` (tối đa 50MB/ảnh).
- **Web Chat UI tích hợp**: Giao diện chat trực tiếp tại `http://127.0.0.1:5084/` kèm nút nạp token 1-click.
- **Khởi chạy 1-click**: Tự động biên dịch và chạy bằng `run.bat` trên Windows.

---

## 🚀 Khởi động nhanh (Quick Start)

### Dành cho Windows (1-Click)

Yêu cầu: Đã cài [Go 1.24+](https://go.dev/dl/) và Google Chrome.

1. Clone repo về máy:
   ```powershell
   git clone https://github.com/smk285pro-hash/glm-aki-proxy.git
   cd glm-aki-proxy
   ```
2. Chạy file **`run.bat`**:
   - Tự động biên dịch binary (`glm-aki-proxy.exe` & `aki-collect.exe`).
   - Tự động thu hoạch 50 token ban đầu nếu chưa có `tokens.json`.
   - Tự động mở trình duyệt tại `http://127.0.0.1:5084` và chạy server.

---

### Khởi chạy thủ công (CLI)

```bash
# 1. Biên dịch binary
go build -o glm-aki-proxy.exe ./cmd/server
go build -o aki-collect.exe ./cmd/collect

# 2. Thu hoạch device token ban đầu
.\aki-collect.exe --count 50 --out tokens.json

# 3. Khởi chạy server
.\glm-aki-proxy.exe
```

- API Base URL: `http://127.0.0.1:5084/v1`
- Web Chat UI: `http://127.0.0.1:5084/`

---

## 🛠️ Cài đặt Claude Code CLI (`clglm`)

Cấu hình Claude Code CLI kết nối trực tiếp vào proxy chỉ với **1 lệnh trong PowerShell**:

```powershell
irm http://127.0.0.1:5084/install.ps1 | iex
```

Sau khi cài đặt, gõ lệnh sau ở bất kỳ thư mục nào để bắt đầu code:
```powershell
clglm
```

> **Gợi ý:** Trong giao diện Claude Code CLI, dùng `/model` để chuyển đổi giữa các model GLM.

---

## 💻 Tích hợp với Cursor & OpenAI SDK

Cấu hình trong Cursor hoặc ứng dụng sử dụng OpenAI SDK:
- **Base URL**: `http://127.0.0.1:5084/v1`
- **API Key**: `aki-local-key` (hoặc `AUTH_TOKEN` trong `.env` nếu có)
- **Model**: `glm-5.3`, `glm-5.2`, `glm-4.7`, `GLM-5-Turbo`, `GLM-5v-Turbo`

### Kiểm tra bằng cURL:
```bash
curl http://127.0.0.1:5084/v1/chat/completions \
  -H "Authorization: Bearer aki-local-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"glm-5.3","messages":[{"role":"user","content":"Xin chào!"}],"stream":false}'
```

---

## 📊 Danh sách Model hỗ trợ

| Model ID | Vai trò | Thinking / Reasoning | Vision |
|---|---|:---:|:---:|
| `glm-5.3` / `claude-opus-gl-5.3` | Flagship Deep Reasoning & Architecture | ✅ Hỗ trợ Effort Low/Med/High/Max | ❌ |
| `glm-5.2` / `claude-sonnet-gl-5.2` | Fast Coding & Agentic Loop | ✅ | ❌ |
| `glm-4.7` / `claude-haiku-gl-4.7` | High-speed, Lightweight & Tool Execution | ❌ | ❌ |
| `GLM-5-Turbo` | Ultra-low Latency Inference | ❌ | ❌ |
| `GLM-5v-Turbo` | Đọc hiểu hình ảnh + Code đa phương thức | ❌ | ✅ Hỗ trợ tối đa 50MB/ảnh |

---

## ⚡ Tham số mở rộng

| Field | Giá trị | Tác dụng |
|---|---|---|
| `webSearch` / `search` | `true` / `false` | Bật/tắt tính năng tìm kiếm web của Z.AI |
| `deepThink` | `true` / `false` | Bật/tắt chế độ suy nghĩ sâu (Thinking) |
| `reasoning_effort` | `low` / `medium` / `high` / `max` | Điều chỉnh độ sâu suy nghĩ (`glm-5.2`, `glm-5.3`) |

---

## 📁 Cấu trúc thư mục

```
├── run.bat                 # 1-Click launcher cho Windows
├── install.ps1             # Script cài đặt lệnh clglm cho PowerShell
├── cmd/
│   ├── server/             # HTTP server chính
│   └── collect/            # Công cụ thu hoạch device token (Headless Chrome)
├── internal/
│   ├── api/                # Handlers: OpenAI, Anthropic, Admin, Harvest
│   │   └── web/            # Embedded Web Chat UI & install script
│   ├── pool/               # Quản lý token pool (Disk / Upstash / Redis)
│   ├── session/            # Quản lý session Z.AI & request signing
│   ├── upstream/           # Upstream chat client & SSE streaming parser
│   └── captcha/            # Aliyun traceless captcha crypto
└── api/
    └── index.go            # Entrypoint Vercel Serverless (tùy chọn)
```

---

## ⚖️ Giấy phép

Dự án phục vụ mục đích nghiên cứu, học tập và sử dụng cá nhân.
