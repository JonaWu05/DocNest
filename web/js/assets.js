// 附件庫對話框：左側 assets 資料夾樹選目前資料夾，右側為該資料夾的直層附件；
// 支援上傳到目前資料夾、新增子資料夾、附件重新命名 / 刪除，點縮圖插入目前文件。
import { state, API_BASE } from "./state.js";
import {
  assetModal, assetGrid, assetHint, assetFolderTree, assetCurrentDir,
  assetUploadBtn, assetNewdirBtn,
} from "./dom.js";
import { showToast } from "./ui.js";
import { uploadFile } from "./api.js";
import { authFetch, rawUrl, ensureOk } from "./auth.js";
import { assetDisplayName, relativeFromDocDir, formatSize } from "./util.js";
import { insertIntoEditor } from "./editor.js";
import { promptModal, confirmModal } from "./modal.js";

// 目前選取的資料夾與各資料夾的可寫狀態（開關對話框之間保留選取）
let currentDir = "assets";
let folderWritable = new Map(); // path -> 是否可寫

// ===== 上傳到附件庫（上傳後加入清單，不直接插入；點縮圖才插入）=====
// dir 未給時上傳到目前選取的資料夾。
export async function uploadToLibrary(files, dir) {
  const target = dir || currentDir;
  for (const file of files) {
    try {
      const item = await uploadFile(file, target);
      showToast("已上傳：" + item.name, "success");
    } catch (err) {
      showToast("上傳失敗：" + err.message, "error");
    }
  }
  await loadAssets(); // 重新整理附件清單，新檔即出現
}

// ===== 左側資料夾樹 =====
// loadFolders 向 /api/asset-folders 取可見資料夾（扁平清單，含可寫標記）並渲染為樹。
async function loadFolders() {
  assetFolderTree.innerHTML = "";
  let folders = [];
  try {
    const res = await authFetch(API_BASE + "/api/asset-folders");
    const data = await res.json();
    folders = data.folders || [];
  } catch (e) { /* 載入失敗時視同無可見資料夾，底下給提示 */ }

  folderWritable = new Map(folders.map(f => [f.path, !!f.writable]));
  if (!folderWritable.has(currentDir)) currentDir = "assets"; // 先前選取的資料夾已不可見時回到根

  if (!folders.length) {
    assetFolderTree.innerHTML = '<div class="empty-hint">沒有可存取的附件資料夾</div>';
    updateCurrentDirUI();
    return;
  }

  // 回應已依路徑排序，父層必在子層之前；用 map 對應各資料夾的子節點容器
  const containers = new Map(); // path -> 子節點容器元素
  folders.forEach(f => {
    const node = document.createElement("div");
    node.className = "asset-tree-node";

    const label = document.createElement("div");
    label.className = "asset-tree-label";
    label.dataset.path = f.path;
    // 依層級縮排（assets 根為 0 層）
    const depth = f.path.split("/").length - 1;
    label.style.paddingLeft = (8 + depth * 14) + "px";

    const glyph = document.createElement("i");
    glyph.className = "fa fa-folder-o";
    label.appendChild(glyph);

    const name = document.createElement("span");
    name.className = "asset-tree-name";
    name.textContent = f.path === "assets" ? "assets（根）" : f.path.split("/").pop();
    label.appendChild(name);

    if (!f.writable) {
      // 唯讀資料夾：可瀏覽挑選，但不能上傳 / 改名 / 新增子資料夾
      const lock = document.createElement("i");
      lock.className = "fa fa-lock asset-tree-lock";
      lock.title = "唯讀";
      label.appendChild(lock);
    }

    label.addEventListener("click", () => selectFolder(f.path));
    node.appendChild(label);

    const childWrap = document.createElement("div");
    node.appendChild(childWrap);
    containers.set(f.path, childWrap);

    const parent = f.path.includes("/") ? f.path.slice(0, f.path.lastIndexOf("/")) : null;
    (parent && containers.has(parent) ? containers.get(parent) : assetFolderTree).appendChild(node);
  });

  updateCurrentDirUI();
}

// selectFolder 切換目前資料夾：更新 active 標示與提示，並載入該資料夾的附件。
async function selectFolder(path) {
  currentDir = path;
  updateCurrentDirUI();
  await loadAssets();
}

// updateCurrentDirUI 同步目前資料夾的 active 標示、路徑顯示與上傳 / 新增資料夾按鈕的可用狀態。
function updateCurrentDirUI() {
  assetFolderTree.querySelectorAll(".asset-tree-label").forEach(el => {
    el.classList.toggle("active", el.dataset.path === currentDir);
  });
  const writable = folderWritable.get(currentDir) === true;
  assetUploadBtn.disabled = !writable;
  assetNewdirBtn.disabled = !writable;
  assetUploadBtn.title = writable ? "上傳到 " + currentDir : "此資料夾唯讀，無法上傳";
  assetCurrentDir.textContent = currentDir + (writable ? "" : "（唯讀）");
}

// createAssetFolder 在目前選取的資料夾底下新增子資料夾，建立後選為目前資料夾。
export async function createAssetFolder() {
  const base = currentDir;
  let name = await promptModal(
    "在「" + base + "」底下新增資料夾，請輸入名稱\n（僅限英文字母、數字、-、_、.）："
  );
  if (!name || !name.trim()) return;
  name = name.trim().replace(/^\/+|\/+$/g, "");
  const full = base + "/" + name;
  try {
    const res = await authFetch(
      API_BASE + "/api/create?path=" + encodeURIComponent(full) + "&type=dir",
      { method: "POST" }
    );
    await ensureOk(res);
    showToast("資料夾已建立", "success");
    currentDir = full;
    await loadFolders();
    await loadAssets();
  } catch (err) {
    showToast("建立失敗：" + err.message, "error");
  }
}

// ===== 附件操作：刪除 / 重新命名 =====
async function deleteAsset(item, ev) {
  ev.stopPropagation(); // 避免觸發插入
  const label = assetDisplayName(item.name);
  if (!(await confirmModal("確定刪除附件「" + label + "」？\n若有文件引用此附件，連結將會失效。", { okText: "刪除" }))) return;
  try {
    const res = await authFetch(
      API_BASE + "/api/file?path=" + encodeURIComponent(item.path),
      { method: "DELETE" }
    );
    await ensureOk(res);
    showToast("已刪除：" + label, "success");
    await loadAssets();
  } catch (err) {
    showToast("刪除失敗：" + err.message, "error");
  }
}

// renameAsset 重新命名附件：只改使用者可見的檔名（時間戳記前綴由後端保留），
// 副檔名不可變更（未輸入時自動補回原本的副檔名）。
async function renameAsset(item, ev) {
  ev.stopPropagation(); // 避免觸發插入
  const cur = assetDisplayName(item.name);
  const input = await promptModal(
    "重新命名附件（僅限英文字母、數字、-、_、.，副檔名不可變更）。\n" +
    "注意：若已有文件引用此附件，既有連結不會自動更新、將會失效。",
    cur
  );
  if (!input || !input.trim() || input.trim() === cur) return;

  let newName = input.trim();
  // 未輸入副檔名時自動補回原本的副檔名（僅為輸入輔助，副檔名限制由後端強制）
  const dot = cur.lastIndexOf(".");
  const ext = dot >= 0 ? cur.slice(dot) : "";
  if (ext && !newName.toLowerCase().endsWith(ext.toLowerCase())) newName += ext;

  try {
    const res = await authFetch(
      API_BASE + "/api/asset/rename?path=" + encodeURIComponent(item.path) +
      "&newName=" + encodeURIComponent(newName),
      { method: "POST" }
    );
    await ensureOk(res);
    showToast("已重新命名：" + assetDisplayName(newName), "success");
    await loadAssets();
  } catch (err) {
    showToast("重新命名失敗：" + err.message, "error");
  }
}

// ===== 附件庫：開關與載入 =====
// openAssetModal 開啟附件庫：載入左側資料夾樹與目前資料夾的附件清單。
export function openAssetModal() {
  if (!state.currentPath) return;
  assetModal.classList.remove("hidden");
  (async () => {
    await loadFolders();
    await loadAssets();
  })();
}
// closeAssetModal 關閉附件庫。
export function closeAssetModal() {
  assetModal.classList.add("hidden");
}

// loadAssets 向 /api/assets 取目前資料夾的直層附件並渲染為縮圖格。
async function loadAssets() {
  assetGrid.innerHTML = "";
  assetHint.textContent = "載入中…";
  try {
    const res = await authFetch(API_BASE + "/api/assets?dir=" + encodeURIComponent(currentDir));
    if (!res.ok) throw new Error("HTTP " + res.status);
    const data = await res.json();
    const assets = data.assets || [];
    if (!assets.length) {
      assetHint.textContent = "「" + currentDir + "」內沒有附件，可切換左側資料夾或上傳新檔。";
      return;
    }
    assetHint.textContent = "「" + currentDir + "」內共 " + assets.length + " 個附件，點選即插入目前文件。";
    assets.forEach(item => assetGrid.appendChild(renderAssetItem(item)));
  } catch (err) {
    assetHint.textContent = "載入失敗：" + err.message;
  }
}

// renderAssetItem 建立單一附件項目（圖片顯示縮圖、其餘顯示檔案圖示；點選即插入）。
// 目前資料夾可寫時提供重新命名與刪除鈕（hover 時顯示；伺服器端仍會驗證權限）。
function renderAssetItem(item) {
  const el = document.createElement("div");
  el.className = "asset-item";
  el.title = item.path;

  if (item.isImage) {
    const img = document.createElement("img");
    img.className = "asset-thumb";
    img.loading = "lazy";
    img.src = rawUrl(item.path);
    el.appendChild(img);
  } else {
    const ic = document.createElement("div");
    ic.className = "asset-thumb-file";
    ic.innerHTML = '<i class="fa fa-file-o"></i>';
    el.appendChild(ic);
  }

  const name = document.createElement("div");
  name.className = "asset-name";
  name.textContent = assetDisplayName(item.name);
  el.appendChild(name);

  const size = document.createElement("div");
  size.className = "asset-size";
  size.textContent = formatSize(item.size);
  el.appendChild(size);

  if (folderWritable.get(currentDir) === true) {
    const ren = document.createElement("button");
    ren.className = "asset-act asset-ren";
    ren.innerHTML = '<i class="fa fa-pencil"></i>';
    ren.title = "重新命名";
    ren.addEventListener("click", (e) => renameAsset(item, e));
    el.appendChild(ren);

    const del = document.createElement("button");
    del.className = "asset-act asset-del";
    del.innerHTML = '<i class="fa fa-trash-o"></i>';
    del.title = "刪除附件";
    del.addEventListener("click", (e) => deleteAsset(item, e));
    el.appendChild(del);
  }

  el.addEventListener("click", () => insertAsset(item));
  return el;
}

// 插入已上傳的附件（換算成相對於目前文件的路徑）
function insertAsset(item) {
  if (!state.currentPath) return;
  const rel = relativeFromDocDir(item.path);
  const label = assetDisplayName(item.name);
  // 圖片與 PDF 用圖片語法 ![]()：圖片直接顯示、PDF 由預覽改寫成內嵌 viewer（見 preview.js embedPdf）。
  // 其餘附件用一般連結 []()（點擊於新分頁開啟 / 下載）。
  const isPdf = /\.pdf$/i.test(item.name);
  const md = (item.isImage || isPdf ? "![" : "[") + label + "](" + rel + ")";
  insertIntoEditor(md);
  showToast("已插入：" + label, "success");
  closeAssetModal();
}
