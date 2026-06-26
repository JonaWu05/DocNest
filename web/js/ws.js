// WebSocket 連線管理：建立連線、斷線自動重連、收送統一格式訊息。
import { getToken, authFetch } from "./auth.js";
import { API_BASE } from "./state.js";

let socket = null;
let reconnectTimer = null;
let reconnectAttempts = 0;          // 連續重連失敗次數，用於指數退避（連線成功後歸零）
let intentionalClose = false;       // 主動登出/關閉時不要自動重連
const handlers = {};                // type -> handler(payload)

const BASE_RECONNECT_DELAY = 1000;  // 退避起始延遲
const MAX_RECONNECT_DELAY = 30000;  // 退避上限（避免無止盡拉長）

const reconnectToast = document.getElementById("reconnect-toast");

// 註冊某類型訊息的處理函式（由 main.js 在登入後設定）
export function onMessage(type, fn) {
  handlers[type] = fn;
}

// showReconnecting / hideReconnecting 顯示 / 隱藏「連線中斷，嘗試重新連線…」提示。
function showReconnecting() {
  if (reconnectToast) reconnectToast.classList.remove("hidden");
}
function hideReconnecting() {
  if (reconnectToast) reconnectToast.classList.add("hidden");
}

// 建立 WebSocket 連線（token 以 query 夾帶，因 WS 無法自訂標頭）
export function connectWS() {
  const token = getToken();
  if (!token) return;
  intentionalClose = false;

  const proto = location.protocol === "https:" ? "wss" : "ws";
  socket = new WebSocket(`${proto}://${location.host}/ws?token=${encodeURIComponent(token)}`);

  socket.addEventListener("open", () => {
    hideReconnecting();
    reconnectAttempts = 0; // 連線成功，重置退避
  });

  socket.addEventListener("message", (ev) => {
    let msg;
    try { msg = JSON.parse(ev.data); } catch (e) { return; }
    const fn = handlers[msg.type];
    if (fn) fn(msg.payload);
  });

  socket.addEventListener("close", () => {
    socket = null;
    if (intentionalClose) return;
    showReconnecting();   // 非侵入式提示「連線中斷，嘗試重新連線…」
    scheduleReconnect();
  });

  // error 之後一定會觸發 close，重連邏輯統一放在 close
  socket.addEventListener("error", () => {});
}

// scheduleReconnect 以指數退避排程下一次重連（已排程則略過，避免疊加）。
function scheduleReconnect() {
  if (reconnectTimer) return;
  // 指數退避：1s, 2s, 4s … 上限 30s，避免斷線時固定頻率猛撞伺服器
  const delay = Math.min(MAX_RECONNECT_DELAY, BASE_RECONNECT_DELAY * 2 ** reconnectAttempts);
  reconnectAttempts++;
  reconnectTimer = setTimeout(attemptReconnect, delay);
}

// 重連前先探測 token 是否仍有效，避免 token 過期後無止盡撞 401 握手。
// WebSocket 握手失敗的 close 事件取不到 HTTP 狀態碼，故改用一個 HTTP 請求判別：
//   - 401：authFetch 會清除 token 並派發 auth:unauthorized（由 session 接手登出）→ 停止重連
//   - 網路錯誤（token 還在）：稍後再以更長的退避重試
async function attemptReconnect() {
  reconnectTimer = null;
  if (intentionalClose || !getToken()) return;
  try {
    const res = await authFetch(API_BASE + "/api/me");
    if (res.ok) connectWS();
    else scheduleReconnect(); // 非 401 的伺服器錯誤，稍後再試
  } catch (e) {
    if (getToken()) scheduleReconnect(); // token 還在 → 純網路錯誤，繼續退避重試
    // token 已被 authFetch 因 401 清除 → 不再重連（登出流程已啟動）
  }
}

// 主動關閉連線（登出時呼叫），不再自動重連
export function disconnectWS() {
  intentionalClose = true;
  reconnectAttempts = 0;
  if (reconnectTimer) { clearTimeout(reconnectTimer); reconnectTimer = null; }
  if (socket) { socket.close(); socket = null; }
  hideReconnecting();
}

// 送出一則訊息（連線未開時靜默忽略）
function sendWS(type, payload) {
  if (socket && socket.readyState === WebSocket.OPEN) {
    socket.send(JSON.stringify({ type, payload }));
  }
}

// 送出自己的 presence 狀態（開檔、切換模式、關檔時呼叫）
export function sendPresence(currentFile, isEditing) {
  sendWS("client_presence", {
    current_file: currentFile || "",
    is_editing: !!isEditing,
  });
}
