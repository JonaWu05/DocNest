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
import { connectCollab, disconnectCollab, collabActive, collabManaged, collabIsSaver, collabCurrentPath, collabNoteSaved } from "./collab.js";
import { renderCollabStatus } from "./collabStatus.js";
import { setCollabExternal } from "./collabExternal.js";

// 打字時的預覽 / 目錄更新採 debounce：連續輸入停止約 150ms 後才重算一次，
// 避免每個按鍵都全量 marked.parse + 重建 DOM 造成卡頓。
// （切換模式等需即時呈現的場合仍直接呼叫 renderPreview / buildTOC，不走此路徑。）
const debouncedPreviewAndTOC = debounce(() => {
  if (state.currentMode === "split") renderPreview();
  buildTOC();
}, 150);

// ===== 確保 EasyMDE 已建立 =====
// EasyMDE 改為延遲載入（見 vendor.js），故本函式為 async：呼叫端需 await 後才可用 state.easyMDE。
async function ensureEditor() {
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
      // 已移除 preview / side-by-side / fullscreen（與上方 預覽 / 編輯 / 分割 重複）與 guide
    ],
  });
  // 編輯器內容變更：更新真實來源、標記未儲存、即時刷新分割預覽、排程自動儲存
  state.easyMDE.codemirror.on("change", () => {
    if (state.suppressChange) return;
    // 共編接管時：真實內容、預覽/目錄、落檔皆由 collab 的回呼處理（見 ensureCollab）。
    // 用 collabManaged（而非 collabActive）涵蓋連線空窗期，避免此時的編輯漏到舊版自動儲存而與他人分歧。
    if (collabManaged()) return;
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

// setEditorValue 以程式化方式設定編輯器內容；suppressChange 暫時忽略 change 事件，避免誤標未儲存。
export function setEditorValue(text) {
  state.suppressChange = true;
  state.easyMDE.value(text);
  state.suppressChange = false;
}

// 共編內容變動（本地或遠端）後的預覽 / 目錄刷新：debounce 避免逐則 update 全量重繪。
// 編輯模式不顯示預覽故略過 renderPreview；預覽 / 分割模式都需重繪。
const debouncedCollabRefresh = debounce(() => {
  if (state.currentMode !== "edit") renderPreview();
  buildTOC();
}, 150);

// ===== 啟用（或沿用）共編房間 =====
// 僅對可寫檔案於編輯 / 分割模式啟用：把底層 CodeMirror 綁到共享 Y.Text，
// 本地變更即時同步給協作者，落檔由 saver（房間第一個可寫者）debounce 後走既有存檔流程。
// 同一檔切換編輯/分割模式時沿用既有連線，不重連。
async function ensureCollab() {
  if (collabActive() && collabCurrentPath() === state.currentPath) return;
  await connectCollab(state.currentPath, state.easyMDE.codemirror, {
    canWrite: state.currentWritable,
    seedText: state.currentContent, // 若被指派為 seeder，用已讀入的 .md 內容初始化文件
    onContentChange: (txt) => {
      state.currentContent = txt; // 維持單一真實來源（供預覽 / 目錄 / 落檔）
      debouncedCollabRefresh();
    },
    onPresenceChange: renderCollabStatus, // 房內參與者 / 落檔者 / 落檔時間變動 → 重繪共編狀態列
    onExternalChange: setCollabExternal,  // 外部改檔（僅落檔者）→ 顯示 / 收掉協調橫幅

    onSaveRequest: (txt) => {
      state.currentContent = txt;
      // 由 saver 靜默落檔，且略過樂觀鎖（force）：共編內容為 CRDT 合併後的超集，已含磁碟上的版本，
      // saver 移交後新 saver 的版本號可能落後，若帶版本鎖會持續 409、檔案永遠存不進去。
      // （外部直接改檔的協調留待後續：此情境會以共編內容覆蓋外部變更。）
      saveFile(true, true);
    },
  });
}

// 進入編輯 / 分割模式時填入編輯器內容並設定可編輯狀態：
//   - 可寫檔：接上共編（綁定由 connectCollab 以 Y.Text 內容覆蓋編輯器，不可再 setEditorValue）。
//     連線/綁定完成前先設為唯讀，避免空窗期的編輯漏到舊版自動儲存、或被綁定的初始內容覆蓋。
//   - 唯讀檔：不參與共編，沿用既有靜態填入並維持唯讀。
async function prepareEditorContent() {
  const cm = state.easyMDE.codemirror;
  if (!state.currentWritable) {
    setEditorValue(state.currentContent);
    cm.setOption("readOnly", true);
    return;
  }
  const path = state.currentPath;
  cm.setOption("readOnly", true);
  clearTimeout(state.autosaveTimer); // 清掉可能殘留的舊版自動儲存排程
  try {
    await ensureCollab();
  } catch (e) {
    // 共編連線失敗（例如 bundle 載入失敗）：退回單機編輯，至少不卡在唯讀。
    showToast("即時共編連線失敗，改為單機編輯", "info");
    setEditorValue(state.currentContent);
  }
  if (state.currentPath === path) cm.setOption("readOnly", false); // 仍停在同一檔才解除唯讀
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
    await prepareEditorContent(); // 可寫檔接上共編並設定可編輯狀態；唯讀檔靜態填入
    setTimeout(() => state.easyMDE.codemirror.refresh(), 0);
  } else { // split
    editorPane.classList.remove("hidden");
    previewPane.classList.remove("hidden");
    await ensureEditor();
    await prepareEditorContent(); // 可寫檔接上共編並設定可編輯狀態；唯讀檔靜態填入
    renderPreview();
    setTimeout(() => state.easyMDE.codemirror.refresh(), 0);
  }
  buildTOC(); // 切換模式後同步更新目錄
  localStorage.setItem("lastMode", mode); // 記住模式，下次開啟同一檔時還原
  // 通知其他人我目前的狀態（編輯與分割皆視為編輯中）
  sendPresence(state.currentPath, mode !== "preview");
}

// ===== 自動儲存排程 =====
// scheduleAutosave 於開啟自動儲存時重設延遲計時器（停止輸入 AUTOSAVE_DELAY 後靜默存檔）。
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
  // 共編接管時：落檔集中由 saver 進行（避免多人併發或空窗期直接 POST 互踩版本鎖、造成分歧）。
  // 唯有「目前的 saver」會放行；其餘（非 saver、連線空窗期）一律不走舊版存檔，內容由 saver 自動落檔。
  if (collabManaged() && !collabIsSaver()) {
    if (!silent && !force) showToast("共編中：內容會由存檔者自動儲存", "info");
    return;
  }
  if (state.currentMode !== "preview" && state.easyMDE) state.currentContent = state.easyMDE.value();

  const savingPath = state.currentPath; // 存檔期間使用者可能切檔，回寫狀態前須比對
  saveBtn.disabled = true;
  saveBtn.textContent = "儲存中…"; // 慢網路時給明確進度回饋
  try {
    const url = API_BASE + "/api/file?path=" + encodeURIComponent(savingPath) + (force ? "&force=1" : "");
    const headers = { "Content-Type": "text/plain; charset=utf-8" };
    if (state.currentVersion) headers["X-File-Version"] = state.currentVersion; // 帶基準版本供後端比對
    const res = await authFetch(url, { method: "POST", headers, body: state.currentContent });

    // 409：編輯期間檔案已被他人更新，交由 sync 模組以提示條讓使用者選擇載入或覆蓋
    if (res.status === 409) {
      window.dispatchEvent(new CustomEvent("file:conflict", { detail: { path: savingPath } }));
      return;
    }
    await ensureOk(res);
    const byCollabSaver = collabManaged() && collabIsSaver(); // 共編 saver 的自動落檔：回饋改走狀態列
    // 存檔回應期間可能已切換到別的檔案；僅當仍停在同一檔時才回寫版本 / 未存標記，避免污染新檔狀態。
    if (state.currentPath === savingPath) {
      state.currentVersion = res.headers.get("X-File-Version") || state.currentVersion; // 更新基準版本
      setDirty(false);
    }
    if (byCollabSaver) {
      collabNoteSaved(); // 經 awareness 廣播落檔時間，由共編狀態列顯示「已儲存 …」
      if (!silent) showToast("儲存成功", "success"); // 手動存檔仍給回饋；自動落檔靜默（避免每 1.5s 跳一次提示）
    } else {
      showToast(silent ? "已自動儲存" : "儲存成功", silent ? "info" : "success");
    }
  } catch (err) {
    showToast("儲存失敗：" + err.message, "error");
  } finally {
    saveBtn.textContent = "儲存";
    // 仍停在同一檔才還原按鈕狀態；若已切檔，由 openFile 依新檔權限設定，不在此覆寫。
    if (state.currentPath === savingPath) saveBtn.disabled = !state.currentWritable;
  }
}

// ===== 將 Markdown 片段插入到編輯器游標處（必要時先切到編輯模式）=====
export async function insertIntoEditor(md) {
  if (state.currentMode === "preview") await applyMode("edit");
  await ensureEditor();
  state.easyMDE.codemirror.replaceSelection(md + "\n");
}

// ===== 開啟檔案 =====
// openFile 讀取指定檔案內容並切到預覽：先確認未存變更、收掉舊共編房間，再更新狀態與工具列。
// labelEl 為檔案樹節點，用於高亮與判斷可寫；無則預設可寫（伺服器端仍會擋）。
export async function openFile(path, labelEl) {
  // 點同一個已開啟且無未存變更的檔案：不需重抓（也避免無謂的 fetch）
  if (path === state.currentPath && !state.isDirty) return;
  // 有未存變更時一律先確認——含「重新載入目前正在編輯的同一個檔案」這個情況，
  // 否則會在無提示下以伺服器內容覆蓋掉使用者尚未儲存的變更（資料遺失）。
  if (!(await confirmDiscardIfDirty())) return;
  disconnectCollab(); // 切換檔案：先收掉舊檔的共編房間（會收尾未落檔內容）

  try {
    const res = await authFetch(API_BASE + "/api/file?path=" + encodeURIComponent(path));
    await ensureOk(res);
    const text = await res.text();

    state.currentPath = path;
    state.currentContent = text;
    state.currentVersion = res.headers.get("X-File-Version"); // 樂觀鎖基準版本
    setDirty(false);
    localStorage.setItem("lastFile", path); // 記住最後開啟的檔案，下次登入自動還原

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

// openFileByPath 以路徑開檔：先在檔案樹找對應節點（供高亮 / 權限判斷）再呼叫 openFile。
export function openFileByPath(path) {
  const label = document.querySelector('.tree-label[data-path="' + CSS.escape(path) + '"]');
  return openFile(path, label);
}

// resetWorkspace 清空工作區回到「未開啟檔案」：收掉共編房間、重設狀態、停用工具列、通知 presence 離開。
export function resetWorkspace() {
  disconnectCollab(); // 關閉工作區：收掉共編房間
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
