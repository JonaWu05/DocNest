// 編輯器核心：EasyMDE 建立、模式切換、預覽、儲存、自動儲存、開檔 / 重設工作區。
import { state, API_BASE, AUTOSAVE_DELAY } from "./state.js";
import {
  editorPane, previewPane, contentEl, modeButtons,
  fileNameEl, saveBtn, attachBtn, exportBtn, autosaveToggle,
} from "./dom.js";
import { showToast, setDirty, confirmDiscardIfDirty } from "./ui.js";
import { renderPreview } from "./preview.js";
import { buildTOC } from "./toc.js";
import { syncFromEditor } from "./scrollSync.js";
import { uploadFile } from "./api.js";
import { authFetch, ensureOk } from "./auth.js";
import { relativeFromDocDir, debounce } from "./util.js";
import { sendPresence } from "./ws.js";
import { loadEasyMDE } from "./vendor.js";
import { openAssetModal } from "./assets.js";
import { openDocPicker } from "./docPicker.js";

// 打字時的預覽 / 目錄更新採 debounce：連續輸入停止約 150ms 後才重算一次，
// 避免每個按鍵都全量 marked.parse + 重建 DOM 造成卡頓。
// （切換模式等需即時呈現的場合仍直接呼叫 renderPreview / buildTOC，不走此路徑。）
const debouncedPreviewAndTOC = debounce(() => {
  if (state.currentMode === "split") renderPreview();
  buildTOC();
}, 150);

// ===== 確保 EasyMDE 已建立 =====
// EasyMDE 改為延遲載入（見 vendor.js），故本函式為 async：呼叫端需 await 後才可用 state.easyMDE。
export async function ensureEditor() {
  if (state.easyMDE) return;
  await loadEasyMDE();
  state.easyMDE = new EasyMDE({
    element: document.getElementById("editor"),
    autoDownloadFontAwesome: false, // FontAwesome 由 index.html 以本地 /static/vendor 載入，不從 CDN 抓

    spellChecker: false,
    status: ["lines", "words"],
    placeholder: "在此輸入 Markdown…（可直接拖放或貼上圖片）",
    lineNumbers: true, // 編輯區左側顯示行號（如 VSCode）
    // 啟用 EasyMDE 內建的圖片上傳（支援拖放與貼上），改用我們的後端
    uploadImage: true,
    imageUploadFunction: function (file, onSuccess, onError) {
      // 拖放/貼上：就近存到目前文件的 assets/，回傳相對於文件的連結
      uploadFile(file)
        .then(data => onSuccess(relativeFromDocDir(data.path)))
        .catch(err => onError(err.message));
    },
    // 自訂工具列：沿用 EasyMDE 預設按鈕，但「圖片」鈕改為開啟附件庫，
    // 並新增「插入分頁連結」鈕（挑選工作區內其他文件，插入可站內轉跳的連結）。
    toolbar: [
      "bold", "italic", "heading", "|",
      "quote", "unordered-list", "ordered-list", "|",
      "link",
      {
        name: "image",
        action: () => openAssetModal(),
        className: "fa fa-picture-o",
        title: "插入圖片 / 附件",
      },
      {
        name: "doc-link",
        action: () => openDocPicker(),
        className: "fa fa-file-text-o",
        title: "插入分頁連結（連到其他文件，點擊可站內轉跳）",
      },
      "|",
      "preview", "side-by-side", "fullscreen", "|",
      "guide",
    ],
  });
  // 編輯器內容變更：更新真實來源、標記未儲存、即時刷新分割預覽、排程自動儲存
  state.easyMDE.codemirror.on("change", () => {
    if (state.suppressChange) return;
    // 即時：真實內容、未儲存標記、自動儲存排程都不可延遲
    state.currentContent = state.easyMDE.value();
    setDirty(true);
    scheduleAutosave();
    // 延遲：分割預覽與目錄重算（debounce，避免逐鍵全量重繪）
    debouncedPreviewAndTOC();
  });
  // 分割模式：編輯區捲動時連動預覽
  state.easyMDE.codemirror.on("scroll", syncFromEditor);
}

export function setEditorValue(text) {
  state.suppressChange = true;
  state.easyMDE.value(text);
  state.suppressChange = false;
}

// ===== 套用模式（preview / edit / split）=====
// edit / split 需先 await ensureEditor()（EasyMDE 延遲載入），故本函式為 async。
export async function applyMode(mode) {
  if (!state.currentPath) return;
  if (state.currentMode !== "preview" && state.easyMDE) state.currentContent = state.easyMDE.value();

  state.currentMode = mode;
  modeButtons.forEach(b => b.classList.toggle("active", b.dataset.mode === mode));
  contentEl.className = mode;

  if (mode === "preview") {
    editorPane.classList.add("hidden");
    previewPane.classList.remove("hidden");
    renderPreview();
  } else if (mode === "edit") {
    editorPane.classList.remove("hidden");
    previewPane.classList.add("hidden");
    await ensureEditor();
    setEditorValue(state.currentContent);
    state.easyMDE.codemirror.setOption("readOnly", !state.currentWritable); // 無寫入權則編輯器唯讀
    setTimeout(() => state.easyMDE.codemirror.refresh(), 0);
  } else { // split
    editorPane.classList.remove("hidden");
    previewPane.classList.remove("hidden");
    await ensureEditor();
    setEditorValue(state.currentContent);
    state.easyMDE.codemirror.setOption("readOnly", !state.currentWritable); // 無寫入權則編輯器唯讀
    renderPreview();
    setTimeout(() => state.easyMDE.codemirror.refresh(), 0);
  }
  buildTOC(); // 切換模式後同步更新目錄
  // 通知其他人我目前的狀態（編輯與分割皆視為編輯中）
  sendPresence(state.currentPath, mode !== "preview");
}

// ===== 自動儲存排程 =====
export function scheduleAutosave() {
  if (!autosaveToggle.checked) return;
  if (state.autosaveTimer) clearTimeout(state.autosaveTimer);
  state.autosaveTimer = setTimeout(() => saveFile(true), AUTOSAVE_DELAY);
}

// ===== 儲存檔案 =====
// force=true 時略過樂觀鎖檢查（使用者在衝突提示中明確選擇覆蓋）。
export async function saveFile(silent, force) {
  if (!state.currentPath) return;
  // 唯讀檔案不送出儲存（伺服器端仍會擋；這裡提前攔截避免無謂的 403 與閃爍）
  if (!state.currentWritable) {
    if (!silent) showToast("此檔案為唯讀，您沒有編輯權限", "info");
    return;
  }
  if (state.currentMode !== "preview" && state.easyMDE) state.currentContent = state.easyMDE.value();

  saveBtn.disabled = true;
  try {
    const url = API_BASE + "/api/file?path=" + encodeURIComponent(state.currentPath) + (force ? "&force=1" : "");
    const headers = { "Content-Type": "text/plain; charset=utf-8" };
    if (state.currentVersion) headers["X-File-Version"] = state.currentVersion; // 帶基準版本供後端比對
    const res = await authFetch(url, { method: "POST", headers, body: state.currentContent });

    // 409：編輯期間檔案已被他人更新，交由 sync 模組以提示條讓使用者選擇載入或覆蓋
    if (res.status === 409) {
      window.dispatchEvent(new CustomEvent("file:conflict", { detail: { path: state.currentPath } }));
      return;
    }
    await ensureOk(res);
    state.currentVersion = res.headers.get("X-File-Version") || state.currentVersion; // 更新基準版本
    setDirty(false);
    showToast(silent ? "已自動儲存" : "儲存成功", silent ? "info" : "success");
  } catch (err) {
    showToast("儲存失敗：" + err.message, "error");
  } finally {
    saveBtn.disabled = false;
  }
}

// ===== 將 Markdown 片段插入到編輯器游標處（必要時先切到編輯模式）=====
export async function insertIntoEditor(md) {
  if (state.currentMode === "preview") await applyMode("edit");
  await ensureEditor();
  state.easyMDE.codemirror.replaceSelection(md + "\n");
}

// ===== 開啟檔案 =====
export async function openFile(path, labelEl) {
  if (path !== state.currentPath && !(await confirmDiscardIfDirty())) return;

  try {
    const res = await authFetch(API_BASE + "/api/file?path=" + encodeURIComponent(path));
    await ensureOk(res);
    const text = await res.text();

    state.currentPath = path;
    state.currentContent = text;
    state.currentVersion = res.headers.get("X-File-Version"); // 樂觀鎖基準版本
    setDirty(false);

    // 若編輯器已建立，先把內容同步到新檔。否則切換檔案時 applyMode 仍處於上一個檔的
    // 編輯模式，會把編輯器內殘留的舊檔內容誤撈回 currentContent，導致新檔顯示／存成舊檔內容。
    if (state.easyMDE) setEditorValue(text);

    document.querySelectorAll(".tree-label.active").forEach(el => el.classList.remove("active"));
    if (labelEl) labelEl.classList.add("active");

    // 此檔是否可寫：由檔案樹節點標記（找不到標記時預設可寫，伺服器端仍會擋無權限的儲存）
    state.currentWritable = !labelEl || labelEl.dataset.writable !== "";

    fileNameEl.textContent = path;
    modeButtons.forEach(b => b.disabled = false);
    // 唯讀檔案：停用儲存與附件上傳（編輯器本身也會設為唯讀，見 applyMode）
    saveBtn.disabled = !state.currentWritable;
    attachBtn.disabled = !state.currentWritable;
    exportBtn.disabled = false;

    applyMode("preview");
  } catch (err) {
    showToast("開啟失敗：" + err.message, "error");
  }
}

export function openFileByPath(path) {
  const label = document.querySelector('.tree-label[data-path="' + CSS.escape(path) + '"]');
  openFile(path, label);
}

export function resetWorkspace() {
  state.currentPath = null;
  state.currentContent = "";
  state.currentVersion = null;
  state.currentWritable = true;
  setDirty(false);
  fileNameEl.textContent = "尚未開啟檔案";
  modeButtons.forEach(b => b.disabled = true);
  saveBtn.disabled = true;
  attachBtn.disabled = true;
  exportBtn.disabled = true;
  buildTOC(); // 清空目錄（顯示未開啟提示）
  previewPane.innerHTML = '<div class="empty-state"><i class="fa fa-file-text-o"></i><p>從左側選擇一個檔案開始閱讀或編輯</p></div>';
  editorPane.classList.add("hidden");
  previewPane.classList.remove("hidden");
  contentEl.className = "preview";
  state.currentMode = "preview";
  // 回到未選取狀態：通知其他人我已不在任何檔案上
  sendPresence("", false);
}
