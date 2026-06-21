// 匯出 PDF：渲染預覽後呼叫瀏覽器列印，由使用者於對話框另存為 PDF。
import { state } from "./state.js";
import { showToast } from "./ui.js";
import { renderPreview } from "./preview.js";

export function exportPDF() {
  if (!state.currentPath) { showToast("請先開啟檔案", "info"); return; }
  // 取得最新內容並渲染預覽（列印樣式會強制顯示預覽、隱藏其餘介面）
  if (state.currentMode !== "preview" && state.easyMDE) state.currentContent = state.easyMDE.value();
  renderPreview();
  // 以檔名作為列印對話框的預設檔名（去除路徑與副檔名）
  const prevTitle = document.title;
  document.title = state.currentPath.split("/").pop().replace(/\.(md|txt)$/i, "");
  const restore = () => { document.title = prevTitle; window.removeEventListener("afterprint", restore); };
  window.addEventListener("afterprint", restore);
  setTimeout(() => window.print(), 50); // 等 DOM 更新後再列印
}
