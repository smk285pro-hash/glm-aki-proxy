document.addEventListener("DOMContentLoaded", async () => {
  const errorAlert = document.getElementById("errorAlert");
  const tokenCard = document.getElementById("tokenCard");
  const accEmail = document.getElementById("accEmail");
  const accID = document.getElementById("accID");
  const tokenPreview = document.getElementById("tokenPreview");
  const copyBtn = document.getElementById("copyBtn");
  const openZaiBtn = document.getElementById("openZaiBtn");

  function showError(msg) {
    errorAlert.textContent = msg;
    errorAlert.style.display = "block";
    tokenCard.style.display = "none";
    copyBtn.style.display = "none";
  }

  function decodeJWT(tok) {
    try {
      const parts = tok.split(".");
      if (parts.length < 2) return null;
      const b64 = parts[1].replace(/-/g, "+").replace(/_/g, "/");
      const json = decodeURIComponent(atob(b64).split("").map(c => "%" + ("00" + c.charCodeAt(0).toString(16)).slice(-2)).join(""));
      return JSON.parse(json);
    } catch (e) {
      return null;
    }
  }

  try {
    const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
    if (!tab || !tab.url || !tab.url.startsWith("https://chat.z.ai")) {
      showError("Bạn chưa ở trang chat.z.ai. Hãy mở và đăng nhập chat.z.ai trước!");
      openZaiBtn.style.display = "flex";
      openZaiBtn.addEventListener("click", () => {
        chrome.tabs.create({ url: "https://chat.z.ai" });
      });
      return;
    }

    const results = await chrome.scripting.executeScript({
      target: { tabId: tab.id },
      func: () => {
        return localStorage.getItem("token") || "";
      }
    });

    const token = results && results[0] && results[0].result ? results[0].result.trim() : "";
    if (!token) {
      showError("Không tìm thấy token. Hãy chắc chắn bạn đã đăng nhập tài khoản trên chat.z.ai!");
      return;
    }

    const claims = decodeJWT(token);
    accEmail.textContent = claims && claims.email ? claims.email : "Không xác định";
    accID.textContent = claims && claims.id ? claims.id : "Không xác định";
    tokenPreview.textContent = token;

    tokenCard.style.display = "block";
    copyBtn.style.display = "flex";

    copyBtn.addEventListener("click", async () => {
      await navigator.clipboard.writeText(token);
      const originalHTML = copyBtn.innerHTML;
      copyBtn.textContent = "✅ Đã copy token!";
      copyBtn.classList.add("btn-success");
      setTimeout(() => {
        copyBtn.innerHTML = originalHTML;
        copyBtn.classList.remove("btn-success");
      }, 2000);
    });
  } catch (err) {
    showError("Lỗi đọc token: " + err.message);
  }
});
