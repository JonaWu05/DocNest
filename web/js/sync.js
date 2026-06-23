// 即時同步：處理後端廣播的 file_updated 通知（其他人儲存了檔案），
// 以及自己儲存被樂觀鎖擋下（409）時的衝突提示。
// 通知不含內容，需要時才向 /api/file 取回最新內容與版本（節省廣播頻寬）。
import { state, API_BASE } from "./state.js";
import { previewPane } from "./dom.js";
import { renderPreview } from "./preview.js";
import { buildTOC } from "./toc.js";
import { showToast, setDirty } from "./ui.js";
import { setEditorValue, saveFile } from "./editor.js";
import { authFetch, ensureOk } from "./auth.js";

const updateBar = document.getElementById("update-bar");
const updateMsg = document.getElementById("update-bar-msg");
const loadBtn = document.getElementById("update-bar-load");
const ignoreBtn = document.getElementById("update-bar-ignore");
const overwriteBtn = document.getElementById("update-bar-overwrite");

let pendingPath = null;     // 待使用者決定的檔案路徑（內容於按下「載入」時才抓）
let conflictMode = false;   // true 代表是「自己儲存被擋（409）」的衝突，需多給「覆蓋」選項

// 向後端取回指定檔案的最新文字內容與版本（比照 editor.js 的讀檔流程）
async function fetchLatest(path) {
  const res = await authFetch(API_BASE + "/api/file?path=" + encodeURIComponent(path));
  await ensureOk(res);
  return { content: await res.text(), version: res.headers.get("X-File-Version") };
}

export async function handleFileUpdated(payload) {
  if (!payload) return;
  const { path, saved_by } = payload;

  if (saved_by === state.username) return; // 自己儲存的，忽略（不跳提示）
  if (path !== state.currentPath) return;  // 不是目前開啟的檔案，忽略

  if (state.currentMode === "preview") {
    // 預覽模式：直接抓最新內容並套用
    try {
      const { content, version } = await fetchLatest(path);
      if (path !== state.currentPath) return; // 抓取期間可能已切換檔案
      state.currentContent = content;
      state.currentVersion = version;
      // 重繪會重設 innerHTML 進而把捲動歸零，正在閱讀長文者會被彈回頂端。
      // 以重繪前的捲動比例近似還原（內容長度可能改變，用比例而非絕對位移）。
      const denom = (previewPane.scrollHeight - previewPane.clientHeight) || 1;
      const ratio = previewPane.scrollTop / denom;
      renderPreview();
      buildTOC();
      previewPane.scrollTop = ratio * (previewPane.scrollHeight - previewPane.clientHeight);
      showToast(saved_by + " 更新了此文件", "info");
    } catch (err) {
      showToast("同步最新內容失敗：" + err.message, "error");
    }
  } else {
    // 編輯 / 分割模式：可能正在編輯，不可直接覆蓋，改用提示條讓使用者選擇
    showBar(path, saved_by + " 剛剛儲存了此文件，是否載入最新版本？", false);
  }
}

// 自己儲存被樂觀鎖擋下（editor.saveFile 收到 409 後派發 file:conflict 事件）
function handleSaveConflict(path) {
  if (path !== state.currentPath) return;
  if (conflictMode && pendingPath === path) return; // 已在顯示同一檔的衝突提示，不重覆彈
  showBar(path, "此文件已被他人更新，你的變更尚未儲存：要載入最新版本，還是以你的版本覆蓋？", true);
}

// 顯示提示條。conflict=true 時額外露出「覆蓋」按鈕
function showBar(path, message, conflict) {
  pendingPath = path;
  conflictMode = conflict;
  updateMsg.textContent = message;
  if (overwriteBtn) overwriteBtn.classList.toggle("hidden", !conflict);
  updateBar.classList.remove("hidden");
}

// 使用者按「載入」：抓取最新內容覆蓋編輯器（放棄自己未存的變更）
async function applyPending() {
  if (pendingPath === null) return;
  const path = pendingPath;
  try {
    const { content, version } = await fetchLatest(path);
    if (path !== state.currentPath) { hideBar(); return; } // 期間已切換檔案
    state.currentContent = content;
    state.currentVersion = version;
    if (state.easyMDE) setEditorValue(content);
    renderPreview();
    buildTOC();
    setDirty(false); // 與伺服器一致，標記為已同步
    hideBar();
  } catch (err) {
    showToast("載入最新內容失敗：" + err.message, "error");
  }
}

// 使用者按「覆蓋」：以自己的版本強制儲存（略過樂觀鎖）
async function overwritePending() {
  if (pendingPath === null || pendingPath !== state.currentPath) { hideBar(); return; }
  hideBar();
  await saveFile(false, true); // force=1
}

function hideBar() {
  pendingPath = null;
  conflictMode = false;
  if (overwriteBtn) overwriteBtn.classList.add("hidden");
  updateBar.classList.add("hidden");
}

// 綁定提示條按鈕與衝突事件（啟動時呼叫一次）
export function initSync() {
  if (loadBtn) loadBtn.addEventListener("click", applyPending);
  if (ignoreBtn) ignoreBtn.addEventListener("click", hideBar);
  if (overwriteBtn) overwriteBtn.addEventListener("click", overwritePending);
  window.addEventListener("file:conflict", (e) => handleSaveConflict(e.detail && e.detail.path));
}
