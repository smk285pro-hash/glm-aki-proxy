# Z.AI Token Grabber Extension

Extension nhỏ gọn giúp lấy token JWT từ trang `chat.z.ai` chỉ với 1 click để nạp vào GLM-Aki-Proxy.

## Cách cài đặt vào Chrome / Edge / Brave (30 giây)

1. Mở trình duyệt Chrome/Edge và truy cập địa chỉ: `chrome://extensions` (hoặc `edge://extensions`).
2. Bật công tắc **Developer mode** (Chế độ dành cho nhà phát triển) ở góc trên bên phải.
3. Bấm nút **Load unpacked** (Tải tiện ích đã giải nén) ở góc trái.
4. Chọn thư mục này:
   `c:\Users\smk28\Desktop\glm-aki-proxy\extension`
5. Xong! Bạn ghim biểu tượng tiện ích lên thanh trình duyệt.

## Cách sử dụng

1. Vào trang [chat.z.ai](https://chat.z.ai) và đăng nhập tài khoản.
2. Bấm vào biểu tượng **Z.AI Token Grabber** trên thanh tiện ích.
3. Extension sẽ tự động hiển thị Email, User ID và nút **Copy Token vào Clipboard**.
4. Dán token vào biến `ZAI_TOKENS` trên Vercel hoặc file `.env`!
