// 附件庫對話框：上傳、瀏覽、刪除附件，並挑選插入目前文件。
import { state, API_BASE } from "./state.js";
import { assetModal, assetGrid, assetHint, assetTarget } from "./dom.js";
import { showToast } from "./ui.js";
import { uploadFile } from "./api.js";
import { authFetch, rawUrl, ensureOk } from "./auth.js";
import { assetDisplayName, relativeFromDocDir, formatSize } from "./util.js";
import { insertIntoEditor } from "./editor.js";
import { promptModal, confirmModal } from "./modal.js";

// ===== 上傳新檔到附件庫（上傳後加入清單，不直接插入；點縮圖才插入）=====
export async function uploadToLibrary(files, dir) {
  for (const file of files) {
    try {
      await uploadFile(file, dir);
      showToast("已上傳：" + file.name, "success");
    } catch (err) {
      showToast("上傳失敗：" + err.message, "error");
    }
  }
  await loadAssets(); // 重新整理附件清單，新檔即出現
}

// ===== 附件庫：目的地資料夾下拉（僅限 assets 樹底下）=====
async function populateTargetFolders() {
  const prev = assetTarget.value;
  assetTarget.innerHTML = "";
  let folders = ["assets"];
  try {
    const res = await authFetch(API_BASE + "/api/asset-folders");
    const data = await res.json();
    if (data.folders && data.folders.length) folders = data.folders;
  } catch (e) { /* 忽略，至少有 assets 根可選 */ }
  folders.forEach(f => {
    const o = document.createElement("option");
    o.value = f;
    o.textContent = (f === "assets") ? "assets（根）" : f;
    assetTarget.appendChild(o);
  });
  // 維持先前選取，否則預設根 assets
  assetTarget.value = (prev && folders.includes(prev)) ? prev : "assets";
}

// 在附件庫的 assets 樹底下新增子資料夾，建立後選為目的地
export async function createTargetFolder() {
  const base = assetTarget.value || "assets";
  let name = await promptModal("在「" + base + "」底下新增資料夾，請輸入名稱：");
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
    await populateTargetFolders();
    assetTarget.value = full;
  } catch (err) {
    showToast("建立失敗：" + err.message, "error");
  }
}

// ===== 附件庫：刪除附件 =====
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

// ===== 附件庫：開關與載入 =====
// openAssetModal 開啟附件庫：載入目的地資料夾下拉與附件清單。
export function openAssetModal() {
  if (!state.currentPath) return;
  assetModal.classList.remove("hidden");
  populateTargetFolders();
  loadAssets();
}
// closeAssetModal 關閉附件庫。
export function closeAssetModal() {
  assetModal.classList.add("hidden");
}

// loadAssets 向 /api/assets 取附件清單並渲染為縮圖格。
async function loadAssets() {
  assetGrid.innerHTML = "";
  assetHint.textContent = "載入中…";
  try {
    const res = await authFetch(API_BASE + "/api/assets");
    if (!res.ok) throw new Error("HTTP " + res.status);
    const data = await res.json();
    const assets = data.assets || [];
    if (!assets.length) {
      assetHint.textContent = "尚無已上傳的附件，點上方按鈕上傳。";
      return;
    }
    assetHint.textContent = "共 " + assets.length + " 個附件，點選即插入目前文件。";
    assets.forEach(item => assetGrid.appendChild(renderAssetItem(item)));
  } catch (err) {
    assetHint.textContent = "載入失敗：" + err.message;
  }
}

// renderAssetItem 建立單一附件項目（圖片顯示縮圖、其餘顯示檔案圖示；附刪除鈕，點選即插入）。
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

  // 刪除鈕（hover 時顯示）
  const del = document.createElement("button");
  del.className = "asset-del";
  del.innerHTML = '<i class="fa fa-trash-o"></i>';
  del.title = "刪除附件";
  del.addEventListener("click", (e) => deleteAsset(item, e));
  el.appendChild(del);

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
