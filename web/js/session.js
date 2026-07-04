// 登入流程與登入頁／主介面切換。
import { API_BASE, state } from "./state.js";
import { getToken, setToken, clearToken, authFetch, ensureOk } from "./auth.js";
import { disconnectWS } from "./ws.js";

// 進入主介面時要執行的初始化（由 main.js 注入，例如載入檔案樹）
let onEnterApp = () => {};
export function setEnterAppHandler(fn) { onEnterApp = fn; }

const loginView = document.getElementById("login-view");
const appView = document.getElementById("app");
const userNameEl = document.getElementById("user-name");
const userTypeEl = document.getElementById("user-type");
const loginError = document.getElementById("login-error");
const loginForm = document.getElementById("login-form");
const loginUser = document.getElementById("login-username");
const loginPass = document.getElementById("login-password");
const discordBtn = document.getElementById("discord-login-btn");
const logoutBtn = document.getElementById("logout-btn");
const adminLink = document.getElementById("admin-link");
const changePwBtn = document.getElementById("change-pw-btn");

// showLogin / showApp 切換登入頁與主介面的顯示。
function showLogin() {
  appView.classList.add("hidden");
  loginView.classList.remove("hidden");
}
function showApp() {
  loginView.classList.add("hidden");
  appView.classList.remove("hidden");
}

// 顯示登入者名稱與登入方式標籤
function setUser(username, loginType) {
  state.username = username; // 供 presence 自我辨識
  userNameEl.textContent = username;
  userTypeEl.textContent = loginType === "discord" ? "Discord" : "本地帳號";
  userTypeEl.className = "user-type-tag " + (loginType === "discord" ? "is-discord" : "is-local");
}

// 用目前的 token 呼叫 /api/me 驗證並取得登入者資訊；成功進入主介面，失敗回登入頁。
async function enterAppWithMe() {
  try {
    const res = await authFetch(API_BASE + "/api/me");
    if (!res.ok) throw new Error("驗證失敗");
    const me = await res.json();
    setUser(me.username, me.login_type);
    // 權限摘要（未啟用權限分組或舊後端時欄位可能不存在，預設給最寬鬆值以維持相容）
    state.hasAccess = me.has_access !== false;
    state.canWriteRoot = me.can_write_root !== false;
    state.isAdmin = me.is_admin === true;
    adminLink.classList.toggle("hidden", !state.isAdmin);              // 僅管理員可見「管理」入口
    changePwBtn.classList.toggle("hidden", me.login_type !== "local"); // 僅本地帳號可改密碼
    showApp();
    onEnterApp(me.default_doc); // 把首頁文件設定一併交給主介面初始化
  } catch (e) {
    clearToken();
    showLogin();
  }
}

// logout 主動登出：關閉 WebSocket、清除認證 cookie 與 token、回到登入頁。
async function logout() {
  disconnectWS(); // 主動關閉 WebSocket，不再自動重連
  // 清除頁面守門用的 HttpOnly cookie（前端 JS 無法自行刪除，需伺服器清）；失敗不阻斷登出。
  try { await authFetch(API_BASE + "/api/logout", { method: "POST" }); } catch (e) { /* 忽略 */ }
  clearToken();
  showLogin();
}

// Local Account 登入
async function doLocalLogin(e) {
  e.preventDefault();
  loginError.textContent = "";
  try {
    const res = await fetch(API_BASE + "/api/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username: loginUser.value, password: loginPass.value }),
    });
    const data = await (await ensureOk(res)).json();
    setToken(data.token);
    loginPass.value = ""; // 清掉密碼欄
    await enterAppWithMe();
  } catch (err) {
    loginError.textContent = err.message;
  }
}

// ===== 修改密碼小彈窗（本地帳號自助）=====
const pwModal = document.getElementById("pw-modal");
const pwOld = document.getElementById("pw-old");
const pwNew = document.getElementById("pw-new");
const pwError = document.getElementById("pw-error");
const pwSubmit = document.getElementById("pw-submit");

function openPwModal() {
  pwOld.value = ""; pwNew.value = ""; pwError.textContent = "";
  pwModal.classList.remove("hidden");
  pwOld.focus();
}
function closePwModal() { pwModal.classList.add("hidden"); }

// doChangePassword 呼叫 /api/me/password 修改自己的密碼（需驗舊密碼）。
async function doChangePassword() {
  pwError.textContent = "";
  if (!pwNew.value) { pwError.textContent = "請輸入新密碼"; return; }
  pwSubmit.disabled = true;
  try {
    const res = await authFetch(API_BASE + "/api/me/password", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ old_password: pwOld.value, new_password: pwNew.value }),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || "修改失敗");
    closePwModal();
  } catch (err) {
    pwError.textContent = err.message;
  } finally {
    pwSubmit.disabled = false;
  }
}

// 初始化登入流程：綁定事件，並依 token 來源決定顯示登入頁或主介面。
export function initSession() {
  loginForm.addEventListener("submit", doLocalLogin);
  discordBtn.addEventListener("click", () => { window.location.href = "/auth/discord"; });
  logoutBtn.addEventListener("click", logout);
  // 「管理」為 <a href="/admin"> 原生導覽，無需綁定；修改密碼彈窗：
  changePwBtn.addEventListener("click", openPwModal);
  document.getElementById("pw-close").addEventListener("click", closePwModal);
  pwSubmit.addEventListener("click", doChangePassword);
  pwModal.addEventListener("click", (e) => { if (e.target === pwModal) closePwModal(); });
  pwNew.addEventListener("keydown", (e) => { if (e.key === "Enter") doChangePassword(); });
  // 任何受保護請求遇 401：自動登出回登入頁
  window.addEventListener("auth:unauthorized", logout);

  // 1) 先檢查 URL fragment 是否帶 token（Discord callback 導回時）
  const m = window.location.hash.match(/(?:^|[#&])token=([^&]+)/);
  if (m) {
    setToken(decodeURIComponent(m[1]));
    // 清除 fragment，避免 token 殘留在網址列與瀏覽記錄
    history.replaceState(null, "", window.location.pathname + window.location.search);
    enterAppWithMe();
    return;
  }
  // 2) 否則檢查 localStorage 是否已有 token（跨瀏覽器重啟保留）
  if (getToken()) {
    enterAppWithMe();
  } else {
    showLogin();
  }
}
