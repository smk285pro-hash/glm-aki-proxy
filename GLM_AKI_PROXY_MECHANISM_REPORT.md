# BÁO CÁO KỸ THUẬT: CƠ CHẾ HOẠT ĐỘNG TOÀN DIỆN CỦA HỆ THỐNG GLM-AKI-PROXY

---

## 1. Bản chất hệ thống: Đây là gì?

Hệ thống **không phải** là một API gọi tin nhắn thông thường (như OpenAI hay Anthropic B2B API dùng API key trả phí). 

Thực chất, đây là một **Hạ tầng Reverse Proxy giả lập trình duyệt nâng cao**. Hệ thống đứng làm cầu nối trung gian: nhận lệnh từ Claude Code CLI (`clglm`) theo chuẩn Anthropic Messages API, sau đó "đóng vai" một trình duyệt người dùng thực thụ để gửi request vào backend web nội bộ của **`chat.z.ai`**, vượt qua mọi lớp bảo mật/Captcha, rồi dịch ngược kết quả về cho CLI.

```
┌────────────────────────┐
│  Claude Code CLI       │ (Giao thức Anthropic Messages API)
│  (clglm)               │
└───────────┬────────────┘
            │ POST /v1/messages (tool_use, messages, thinking)
            ▼
┌────────────────────────┐
│  GLM-AKI-PROXY         │
│  (Go Server / Vercel)  │
└───────────┬────────────┘
            │ 1. Giả lập chữ ký HMAC-SHA256 (x-signature)
            │ 2. Lấy Captcha Device Token từ Pool (Upstash Redis)
            │ 3. Đính kèm Cookie WAF (acw_tc, cdn_sec_tc)
            │ 4. Chuyển đổi Function Calling thành Text Contract
            ▼
┌────────────────────────┐
│  Aliyun ESA & WAF      │ (Hệ thống tường lửa Alibaba Cloud)
└───────────┬────────────┘
            ▼
┌────────────────────────┐
│  Backend chat.z.ai     │ (POST /api/v2/chat/completions)
└────────────────────────┘
```

---

## 2. Trọng tâm: Cơ chế Pool Token Captcha hoạt động thế nào?

### 2.1. Tại sao bắt buộc phải có Captcha Token?
Khi người dùng bấm gửi tin nhắn trên giao diện web `chat.z.ai`, hệ thống của họ kích hoạt cơ chế nhận diện thiết bị và chống bot ngầm của **Alibaba Cloud Captcha (Aliyun)**.
Mỗi request gửi tới API backend `/api/v2/chat/completions` bắt buộc phải kèm theo tham số:
```json
"captcha_verify_param": "{\"sessionId\":\"...\",\"sig\":\"...\",\"token\":\"...\",\"scene\":\"...\"}"
```
Nếu thiếu tham số này hoặc gửi token giả/hết hạn, server Z.AI sẽ lập tức từ chối request với mã lỗi Captcha Failed hoặc trả về thách thức trượt mảnh ghép.

### 2.2. Vòng đời của một Captcha Token trong hệ thống
Hệ thống sử dụng mô hình **Thu hoạch trước (Harvest) — Lưu trữ trung gian (Redis Pool) — Tiêu thụ theo nhu cầu (Consume)**:

```
┌─────────────────────────────────┐
│     aki-collect.exe             │ (Tool thu thập chạy Headless Chromium)
│  - Giải ngầm SDK Aliyun Captcha │
│  - Trích xuất Device Token sạch │
└────────────────┬────────────────┘
                 │ Đẩy hàng loạt (Push 1.000 tokens)
                 ▼
┌─────────────────────────────────┐
│     Upstash Redis / Memory      │
│  - Key: glm_device_tokens       │
│  - Cơ chế hàng đợi (List)       │
│  - Gắn nhãn thời gian (TTL 3h)  │
└────────────────┬────────────────┘
                 │ Lấy 1 token cho mỗi request chat (RPOP / LPOP)
                 ▼
┌─────────────────────────────────┐
│     glm-aki-proxy               │
│  - Gắn vào captcha_verify_param │
│  - Tự động bỏ qua token hết hạn │
└─────────────────────────────────┘
```

#### Bước 1: Thu thập tự động (`aki-collect.exe`)
* Chạy một phiên Chromium ngầm kết nối trực tiếp vào module xác thực của Aliyun.
* Thu thập chữ ký thiết bị phần cứng thật (Canvas fingerprint, WebGL, AudioContext, Screen resolution).
* Nhận về chuỗi device token hợp lệ từ server Aliyun và đẩy lên **Upstash Redis** qua REST API hoặc lưu vào file `tokens.json`.

#### Bước 2: Quản lý và chống "Token chết" (Auto-Discard TTL)
* Aliyun Device Token có tuổi thọ giới hạn (thường từ **2 đến 3 tiếng**). Sau thời gian này, Aliyun đánh dấu token hết hạn và trả về mã lỗi rủi ro `F001` (`VerifyResult: false`).
* Proxy có cơ chế **TTL 3 giờ**: Khi lấy token ra khỏi pool, proxy kiểm tra thời điểm token được thu thập. Nếu quá 3 tiếng, proxy **tự động loại bỏ (discard)** và lấy tiếp token mới, đảm bảo 100% token gửi lên Z.AI đều còn "sống".

#### Bước 3: Tiêu thụ (Single-Use per Request)
* Mỗi lượt gọi chat của Claude Code chỉ tốn đúng **1 token**.
* Proxy lấy token ra khỏi pool và nhúng thẳng vào payload trước khi băm chữ ký và gửi đi.

---

## 3. Các cơ chế bảo mật và giả lập khác của Proxy

### 3.1. Cơ chế bẻ khóa chữ ký động HMAC-SHA256 (`x-signature`)
Mọi request gửi lên Z.AI đều phải có header `x-signature`. Nếu sai chữ ký dù chỉ 1 ký tự, server trả lỗi `403 Forbidden` hoặc `400 Bad Request`.

Proxy thực hiện thuật toán ký 2 tầng độc quyền của Z.AI theo thời gian thực:
1. **Tầng 1 (Round Key)**: Lấy thời gian epoch chia cho 300,000 (gom cụm mỗi 5 phút) và băm HMAC-SHA256 với chuỗi secret salt ẩn của Z.AI:
   $$\text{RoundKey} = \text{HMAC-SHA256}(\text{signSalt}, \lfloor\text{timestamp} / 300000\rfloor)$$
2. **Tầng 2 (Request Signature)**: Tạo UUIDv4 `requestId`, nối với timestamp mili-giây, `user_id` và chuỗi Base64 của toàn bộ nội dung prompt:
   $$\text{x-signature} = \text{HMAC-SHA256}(\text{RoundKey}, \text{info} + "|" + \text{Base64(Prompt)} + "|" + \text{timestamp})$$

### 3.2. Cơ chế vượt rào Aliyun WAF / ESA (CookieJar)
* Hệ thống Z.AI được bảo vệ bởi mạng phân phối Alibaba Cloud ESA. WAF của họ cấp 2 cookie bảo mật bắt buộc: `acw_tc` (WAF Traffic Controller) và `cdn_sec_tc`.
* Nếu client gửi request mà không có các cookie này (đặc biệt là từ các dải IP Datacenter của AWS/Vercel), Aliyun WAF sẽ coi đó là cuộc tấn công và chặn ngay lập tức bằng mã lỗi **`405 Method Not Allowed`**.
* Proxy tích hợp bộ quản lý **`CookieJar`**: Ngay khi khởi chạy, proxy thực hiện truy cập giả lập trang chủ `chat.z.ai`, thu nhận đầy đủ các cookie bảo mật và header trình duyệt (`Accept-Language`, `Sec-Ch-Ua`), sau đó tự động tái sử dụng cho mọi request chat tiếp theo.

### 3.3. Cơ chế giả lập Tool Calling (Function Calling Adapter)
Giao diện chat của Z.AI không hỗ trợ Function Calling trực tiếp như Claude/OpenAI. Proxy giải quyết bài toán này qua cơ chế **Text-Protocol Adapter**:
1. **Prompt Framing**: Khi Claude Code gửi danh sách tool (Read, Write, Bash...), proxy tự động dịch các tool này thành một "bản hợp đồng văn bản" đính kèm vào tin nhắn cuối của user, ra lệnh cho model GLM: *"Khi cần gọi tool, hãy xuất đúng cú pháp `<<<TOOL_CALL>>>{"name": "...", "arguments": {...}}<<<END_TOOL_CALL>>>`"*.
2. **Stream Sanitizer & Parser**: Khi model trả về dòng text, proxy lắng nghe theo thời gian thực, bóc tách các thẻ marker ra khỏi luồng hiển thị của người dùng, làm sạch các lỗi ảo giác (như đường dẫn chứa dấu `|`, thiếu tham số `command`), rồi đóng gói thành đối tượng `tool_use` chuẩn của Anthropic.
3. **Agentic Loop**: Claude Code thực thi tool trên máy tính, trả kết quả `tool_result` về proxy, và proxy tiếp tục chuyển thành ngữ cảnh người dùng để model phân tích tiếp tục.

### 3.4. Pool tài khoản Z.AI & Tự động chuyển vùng (Auto-Failover)
* Proxy hỗ trợ nạp nhiều tài khoản Z.AI cùng lúc (`ZAI_TOKENS`).
* Cơ chế Round-robin xoay vòng giữa các tài khoản để dàn đều hạn ngạch sử dụng.
* Khi một tài khoản gặp lỗi 429 (quá tải) hoặc hết hạn session, proxy tự động đưa tài khoản đó vào chế độ Cooldown (nghỉ ngơi) và tự động chuyển ngay lập tức sang tài khoản tiếp theo mà không làm gián đoạn công việc của người dùng.

---

## 4. Tóm tắt nhanh

* **`glm-aki-proxy`** biến tài khoản Web miễn phí/Pro của Z.AI thành một endpoint Anthropic Messages API mạnh mẽ cho Claude Code.
* **Captcha Pool** là "nguồn nhiên liệu": Mỗi tin nhắn cần 1 token xác thực từ Chromium headless để chứng minh với Z.AI rằng đây là người dùng thật.
* **Chữ ký `x-signature` + CookieJar**: Hai chìa khóa kỹ thuật giúp request đi xuyên qua hệ thống bảo mật của Alibaba Cloud mà không bị WAF chặn (405) hay từ chối chữ ký (400).
* **Text-Protocol**: Bộ dịch thuật giúp model web thuần text có thể lập luận và chạy code/tool calling đa bước như Claude 3.7 Sonnet xịn.
