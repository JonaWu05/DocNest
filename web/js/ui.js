// 跨模組共用的小型 UI 輔助：提示訊息與未儲存狀態。
import { state } from "./state.js";
import { toastEl, dirtyDotEl } from "./dom.js";
import { confirmModal } from "./modal.js";

// 提示訊息（toast）
// 用單一計時器：連續觸發時先清掉前一個，否則前一則的計時器到點會把「後來才顯示」
// 的這一則一起清掉，造成新 toast 只閃一下就消失。
let toastTimer = null;
export function showToast(message, type) {
  if (toastTimer) clearTimeout(toastTimer);
  toastEl.textContent = message;
  toastEl.className = "show " + (type || "info");
  // 錯誤訊息較重要（如儲存/上傳失敗），停留久一點避免被錯過
  const duration = type === "error" ? 5000 : 2000;
  toastTimer = setTimeout(() => { toastEl.className = ""; toastTimer = null; }, duration);
}

// 標記未儲存狀態
export function setDirty(dirty) {
  state.isDirty = dirty;
  dirtyDotEl.classList.toggle("hidden", !dirty);
}

// 切換檔案前的未儲存確認（改用站內對話框，回傳 Promise<boolean>）
export async function confirmDiscardIfDirty() {
  if (!state.isDirty) return true;
  return confirmModal("目前檔案有未儲存的變更，確定要放棄並切換嗎？", { okText: "放棄變更" });
}
