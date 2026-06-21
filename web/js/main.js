// 進入點：綁定所有靜態元素的事件，並完成初始化。
// 由 index.html 以 <script type="module"> 載入，間接帶入其餘模組。
import { state } from "./state.js";
import {
  modeButtons, saveBtn, exportBtn, themeBtn, attachBtn, attachInput,
  assetModal, assetTarget, autosaveToggle, tocHeader, tocToggle, tocSection, previewPane,
} from "./dom.js";
import { applyTheme } from "./theme.js";
import { buildTOC } from "./toc.js";
import { syncFromPreview } from "./scrollSync.js";
import { loadFileTree, createItem } from "./fileTree.js";
import { applyMode, saveFile, scheduleAutosave, openFileByPath } from "./editor.js";
import {
  openAssetModal, closeAssetModal, createTargetFolder, uploadToLibrary,
} from "./assets.js";
import { exportPDF } from "./exportPdf.js";
import { initShortcuts } from "./shortcuts.js";
import { initSidebarResize } from "./sidebar.js";
import { initSession, setEnterAppHandler } from "./session.js";
import { connectWS, onMessage } from "./ws.js";
import { handlePresenceUpdate } from "./presence.js";
import { handleFileUpdated, initSync } from "./sync.js";

// ===== 事件綁定 =====
modeButtons.forEach(btn => btn.addEventListener("click", () => applyMode(btn.dataset.mode)));
saveBtn.addEventListener("click", () => saveFile(false));
exportBtn.addEventListener("click", exportPDF);
themeBtn.addEventListener("click", () => {
  applyTheme(document.body.classList.contains("dark") ? "light" : "dark");
});
document.getElementById("refresh-btn").addEventListener("click", loadFileTree);

// 目錄區：點標題列摺疊 / 展開
tocHeader.addEventListener("click", () => {
  const collapsed = tocSection.classList.toggle("collapsed");
  tocToggle.textContent = collapsed ? "▸" : "▾";
});
// 分割模式：預覽區捲動時連動編輯區
previewPane.addEventListener("scroll", syncFromPreview);

document.getElementById("new-file-btn").addEventListener("click", () => createItem("file"));
document.getElementById("new-dir-btn").addEventListener("click", () => createItem("dir"));

// 📎 附件：開啟附件庫（可上傳/管理附件，或挑選既有附件插入）
attachBtn.addEventListener("click", openAssetModal);
document.getElementById("asset-close").addEventListener("click", closeAssetModal);
document.getElementById("asset-upload-btn").addEventListener("click", () => attachInput.click());
document.getElementById("asset-newdir-btn").addEventListener("click", createTargetFolder);
// 點對話框外的遮罩即關閉
assetModal.addEventListener("click", (e) => {
  if (e.target === assetModal) closeAssetModal();
});
// 上傳到所選目的地資料夾（上傳後留在附件庫，點縮圖才插入）
attachInput.addEventListener("change", async () => {
  if (attachInput.files.length) {
    await uploadToLibrary(Array.from(attachInput.files), assetTarget.value);
  }
  attachInput.value = ""; // 清空以便重複選同一檔案
});

autosaveToggle.addEventListener("change", () => {
  if (autosaveToggle.checked && state.isDirty) scheduleAutosave();
});

window.addEventListener("beforeunload", (e) => {
  if (state.isDirty) { e.preventDefault(); e.returnValue = ""; }
});

initShortcuts();
initSidebarResize(); // 側欄寬度拖曳調整
initSync(); // 綁定 file_updated 提示條的載入/忽略按鈕

// 註冊 WebSocket 訊息處理（連線在登入後才建立）
onMessage("presence_update", handlePresenceUpdate);
onMessage("file_updated", handleFileUpdated);

// ===== 初始化 =====
applyTheme(localStorage.getItem("theme") || "light"); // 還原使用者上次的主題（不需登入）

// 登入成功進入主介面後才載入檔案樹（避免未登入就打受保護的 API）。
// 若後端有設定首頁文件（DEFAULT_DOC），載入檔案樹後自動開啟它作為首頁。
setEnterAppHandler(async (defaultDoc) => {
  connectWS(); // 建立 WebSocket（presence / 即時同步）
  buildTOC();  // 先顯示「未開啟檔案」提示（開檔後會被覆蓋）
  await loadFileTree();
  if (defaultDoc) openFileByPath(defaultDoc);
});

// 啟動登入流程：依 token 來源決定顯示登入頁或主介面
initSession();
