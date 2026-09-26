# GLM-Aki-Proxy ⚡

**Standalone Open-Source Local AI Router** chuyển đổi Z.AI (GLM-4 / GLM-5) thành chuẩn **Anthropic Messages API** & **OpenAI Chat Completions API**, tối ưu chuyên biệt cho **Claude Code CLI (`clglm`)**, **Cursor**, **Codex CLI** và các Agentic AI tools.

Mô hình hoạt động độc lập 100% trên máy cá nhân (theo phong cách **9Router**): **Không cần server cloud, không cần Redis, không tốn chi phí hạ tầng**.

---

## 🌟 Vì sao nên chạy Local (Phong cách 9Router)?

1. **Bảo mật & Tránh WAF 100% (Clean Residential IP)**:
   - Cơ chế Aliyun Traceless Captcha của Z.AI kiểm tra và ràng buộc IP thu hoạch token với IP gửi request chat.
   - Khi chạy proxy trực tiếp trên máy của bạn, IP sinh device token trùng khớp 100% với IP gửi request chat. Không bao giờ bị chặn 403 hoặc dính cờ botnet như khi chạy trên datacenter (Vercel/AWS).
2. **Không chi phí - Không phụ thuộc Cloud**:
   - Lưu trữ token trực tiếp qua file `tokens.json` cục bộ.
   - Không cần trả phí server, không cần cấu hình Upstash Redis.
3. **1-Click Khởi chạy (`run.bat`)**:
   - Tự động biên dịch, tự động thu hoạch token ban đầu, tự động mở Web Chat UI và khởi động proxy.
4. **Nạp thêm token ngay trên Web UI**:
   - Giao diện Web có sẵn nút **⚡ Nạp thêm Token** để kích hoạt trình duyệt ngầm thu hoạch thêm token chỉ với 1 click.
5. **Full Tool Calling & Multi-step Agentic Loop**:
   - Hệ thống Tool Contract chuẩn hóa Anthropic `tool_use`/`tool_result`, hỗ trợ stream thinking, auto-repair JSON và triệt tiêu 100% leak protocol ra giao diện chat.

---

## 🚀 Khởi động nhanh (Quick Start)

### Cách 1: 1-Click trên Windows (Khuyên dùng)

Yêu cầu: Đã cài [Go 1.24+](https://go.dev/dl/) và Google Chrome.

1. Clone repo về máy:
   ```powershell
   git clone https://github.com/smk285pro-hash/glm-aki-proxy.git
   cd glm-aki-proxy
   ```
2. Double-click file **`run.bat`**:
   - Script sẽ tự động build binary `glm-aki-proxy.exe` và `aki-collect.exe`.
   - Tự động thu hoạch 50 token ban đầu nếu chưa có `tokens.json`.
   - Tự động mở trình duyệt tại `http://127.0.0.1:5084` và khởi động server.

---

### Cách 2: Khởi chạy thủ công (CLI)

```bash
# 1. Biên dịch binary
go build -o glm-aki-proxy.exe ./cmd/server
go build -o aki-collect.exe ./cmd/collect

# 2. Thu hoạch 50 device token ban đầu
.\aki-collect.exe --count 50 --out tokens.json

# 3. Chạy proxy server
.\glm-aki-proxy.exe
```

Proxy lắng nghe tại: `http://127.0.0.1:5084`  
Web Chat UI tích hợp sẵn tại: `http://127.0.0.1:5084/`

---

## 🛠️ Tích hợp với Claude Code CLI (`clglm`)

Cài đặt cấu hình Claude Code CLI kết nối trực tiếp vào proxy local chỉ với **1 lệnh duy nhất trong PowerShell**:

```powershell
irm http://127.0.0.1:5084/install.ps1 | iex
```

Lệnh trên sẽ:
- Tạo profile độc lập tại `~/.claude-glm` (không ảnh hưởng đến Claude Code gốc của bạn).
- Cấu hình endpoint Anthropic trỏ về `http://127.0.0.1:5084`.
- Tự động thêm alias hàm **`clglm`** vào PowerShell `$PROFILE`.

**Sử dụng ngay trong bất kỳ project nào:**
```powershell
clglm
```

> **Mẹo:** Trong Claude Code CLI, bạn có thể gõ `/model` để đổi linh hoạt giữa `GLM-5.3` (Opus Thinking), `GLM-5.2` (Sonnet), `GLM-4.7` (Haiku) hoặc `GLM-5v-Turbo` (Vision).

---

## 💻 Tích hợp với Cursor / OpenAI SDK

Trong Cursor hoặc bất kỳ ứng dụng nào hỗ trợ OpenAI Base URL:
- **Base URL**: `http://127.0.0.1:5084/v1`
- **API Key**: `aki-local-key` (hoặc giá trị `AUTH_TOKEN` nếu bạn tự đặt trong `.env`)
- **Model**: `glm-5.3`, `glm-5.2`, `glm-4.7`, `GLM-5-Turbo`, `GLM-5v-Turbo`

### Test thử bằng cURL:
```bash
curl http://127.0.0.1:5084/v1/chat/completions \
  -H "Authorization: Bearer aki-local-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"glm-5.3","messages":[{"role":"user","content":"Xin chào!"}],"stream":false}'
```

---

## 📊 Bảng ánh xạ Model & Tính năng

| Model ID | Vai trò | Thinking / Reasoning | Vision |
|---|---|:---:|:---:|
| `glm-5.3` / `claude-opus-gl-5.3` | Flagship Deep Reasoning & Architecture | ✅ Hỗ trợ Effort Low/Med/High/Max | ❌ |
| `glm-5.2` / `claude-sonnet-gl-5.2` | Fast Coding & Agentic Loop | ✅ | ❌ |
| `glm-4.7` / `claude-haiku-gl-4.7` | High-speed, Lightweight & Tool Execution | ❌ | ❌ |
| `GLM-5-Turbo` | Ultra-low Latency Inference | ❌ | ❌ |
| `GLM-5v-Turbo` | Đọc hiểu hình ảnh + Code đa phương thức | ❌ | ✅ Hỗ trợ tối đa 50MB/ảnh |

---

## ⚡ Các tham số mở rộng (Extra Request Fields)

| Field | Giá trị | Tác dụng |
|---|---|---|
| `webSearch` / `search` | `true` / `false` | Bật/tắt tính năng tìm kiếm web trực tiếp của Z.AI |
| `deepThink` | `true` / `false` | Bật/tắt chế độ suy nghĩ sâu (Thinking) |
| `reasoning_effort` | `low` / `medium` / `high` / `max` | Điều chỉnh độ sâu suy nghĩ của `glm-5.2` và `glm-5.3` |

---

## 🌐 Triển khai Cloud (Tùy chọn - Vercel & Upstash Redis)

Nếu bạn vẫn muốn deploy lên Vercel để chia sẻ cho nhóm qua internet:

1. Tạo database Redis miễn phí tại [Upstash Redis](https://console.upstash.com).
2. Lấy `UPSTASH_REDIS_REST_URL` và `UPSTASH_REDIS_REST_TOKEN`.
3. Đẩy token từ máy local lên Redis:
   ```powershell
   .\aki-collect.exe -push-only -file tokens.json
   ```
4. Cấu hình biến môi trường trên Vercel:
   - `AUTH_TOKEN`: Key bảo vệ API
   - `UPSTASH_REDIS_REST_URL`: URL Upstash
   - `UPSTASH_REDIS_REST_TOKEN`: Token Upstash
   - `ZAI_TOKEN`: JWT tài khoản Z.AI (nếu có, để trống sẽ dùng tài khoản khách)
5. Deploy repo lên Vercel (`api/index.go` là serverless handler).

---

## 📁 Cấu trúc thư mục (Architecture)

```
├── run.bat                 # 1-Click launcher cho Windows (9Router style)
├── install.ps1             # Script 1-line cài đặt clglm cho PowerShell
├── cmd/
│   ├── server/             # Standalone proxy HTTP server
│   └── collect/            # Harvester thu hoạch device token (Headless Chrome)
├── internal/
│   ├── api/                # OpenAI / Anthropic / Admin / Harvest HTTP handlers
│   │   └── web/            # Embedded Web Chat UI & install scripts
│   ├── pool/               # Token pool (Disk file / Upstash REST / Redis)
│   ├── session/            # Z.AI session lifecycle & request signing
│   ├── upstream/           # Upstream chat request & SSE streaming parser
│   └── captcha/            # Aliyun traceless captcha crypto
└── api/
    └── index.go            # Entrypoint tương thích Vercel Serverless
```

---

## ⚖️ Giấy phép & Tuyên bố miễn trừ

- Dự án phục vụ mục đích nghiên cứu, học tập và sử dụng cá nhân.
- Vui lòng tuân thủ điều khoản dịch vụ của Z.AI.
