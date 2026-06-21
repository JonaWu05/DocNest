// 跨模組共用的小型 UI 輔助：提示訊息與未儲存狀態。
import { state } from "./state.js";
import { toastEl, dirtyDotEl } from "./dom.js";
import { confirmModal } from "./modal.js";

// 提示訊息（toast）
export function showToast(message, type) {
  toastEl.textContent = message;
  toastEl.className = "show " + (type || "info");
  setTimeout(() => { toastEl.className = ""; }, 2000);
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
