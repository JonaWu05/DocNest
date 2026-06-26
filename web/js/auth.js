// 前端登入狀態與請求封裝：token 儲存、帶 Authorization 的 fetch、raw 連結。
import { API_BASE } from "./state.js";

const TOKEN_KEY = "auth_token";

// getToken / setToken / clearToken 讀寫 / 清除 token。改用 localStorage：token 會保留到
// JWT 過期（JWT_EXPIRE_HOURS）或登出為止，關閉瀏覽器後重開不需重新登入
// （sessionStorage 則會在關閉分頁時清除）。
export function getToken() {
  return localStorage.getItem(TOKEN_KEY);
}
export function setToken(t) {
  localStorage.setItem(TOKEN_KEY, t);
}
export function clearToken() {
  localStorage.removeItem(TOKEN_KEY);
}

// 包裝 fetch：自動帶上 Authorization: Bearer <token>。
// 任一請求回 401 時，清除 token 並廣播事件（由 session 模組接手導回登入頁）。
export async function authFetch(input, init = {}) {
  const token = getToken();
  const headers = new Headers(init.headers || {});
  if (token) headers.set("Authorization", "Bearer " + token);

  const res = await fetch(input, { ...init, headers });
  if (res.status === 401) {
    clearToken();
    window.dispatchEvent(new CustomEvent("auth:unauthorized"));
    throw new Error("未授權，請重新登入");
  }
  return res;
}

// 回應非 2xx 時，解析後端回傳的錯誤訊息並丟出 Error；成功則原樣回傳 res。
// 注意：成功路徑不讀取 body，呼叫端仍可自由 res.text() / res.json()。
export async function ensureOk(res) {
  if (!res.ok) {
    const data = await res.json().catch(() => ({}));
    throw new Error(data.error || ("HTTP " + res.status));
  }
  return res;
}

// 原始檔案 URL（供 <img>/<a> 使用）。
// 瀏覽器無法為 <img> 設定 Authorization 標頭，故把 token 以 query 參數夾帶，
// 後端 middleware 會接受 ?token= 作為備援（僅本機開發場景）。
// from：來源文件路徑（選填）。讓只有「該頁讀取權」而無 asset 直接讀取權的閱讀者，
// 仍能檢視自己有權讀的頁面所引用的圖片／附件（後端 Raw 的來源驗證，見 files.go）。
export function rawUrl(path, from) {
  const t = getToken();
  return API_BASE + "/api/raw?path=" + encodeURIComponent(path) +
    (t ? "&token=" + encodeURIComponent(t) : "") +
    (from ? "&from=" + encodeURIComponent(from) : "");
}
