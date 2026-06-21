// 快捷鍵：Ctrl/Cmd+S 儲存、Ctrl/Cmd+1/2/3 切換預覽/編輯/分割、Esc 關閉附件庫。
import { state } from "./state.js";
import { assetModal } from "./dom.js";
import { applyMode, saveFile } from "./editor.js";
import { closeAssetModal } from "./assets.js";

// 用 capture 階段攔截，確保比 CodeMirror 與瀏覽器預設行為先處理
export function initShortcuts() {
  document.addEventListener("keydown", (e) => {
    const mod = e.ctrlKey || e.metaKey;
    if (mod && (e.key === "s" || e.key === "S")) {
      e.preventDefault();
      if (state.currentPath) saveFile(false);
      return;
    }
    if (mod && (e.key === "1" || e.key === "2" || e.key === "3")) {
      if (!state.currentPath) return;
      e.preventDefault();
      applyMode(e.key === "1" ? "preview" : e.key === "2" ? "edit" : "split");
      return;
    }
    if (e.key === "Escape" && !assetModal.classList.contains("hidden")) {
      closeAssetModal();
    }
  }, true);
}
