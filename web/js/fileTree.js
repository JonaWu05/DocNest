// 左側檔案樹：載入、渲染節點、新增 / 重新命名 / 刪除檔案與資料夾。
import { state, API_BASE } from "./state.js";
import { fileTreeEl, fileNameEl } from "./dom.js";
import { authFetch, ensureOk } from "./auth.js";
import { showToast } from "./ui.js";
import { openFile, openFileByPath, resetWorkspace } from "./editor.js";
import { updateTreeDots } from "./presence.js";
import { promptModal, confirmModal } from "./modal.js";

// 已展開的資料夾路徑集合：跨「重建檔案樹」保留，避免每次新增/改名/刪除後所有資料夾收合。
const expandedDirs = new Set();

// 建立一個 FontAwesome 圖示元素（樹狀的檔案 / 資料夾圖示）
function makeGlyph(faClass) {
  const i = document.createElement("i");
  i.className = "fa " + faClass + " tree-glyph";
  return i;
}

// 將某路徑的所有上層資料夾標記為展開（新增/改名後讓目標項目可見）
function expandAncestors(path) {
  const parts = path.split("/");
  parts.pop(); // 去掉最後一段（檔名或項目本身）
  let acc = "";
  for (const p of parts) {
    acc = acc ? acc + "/" + p : p;
    expandedDirs.add(acc);
  }
}

// ===== 載入檔案樹 =====
export async function loadFileTree() {
  const savedScroll = fileTreeEl.scrollTop; // 重建前記下捲動位置，重建後還原
  fileTreeEl.innerHTML = '<div class="empty-hint">載入中…</div>';
  try {
    const res = await authFetch(API_BASE + "/api/files");
    if (!res.ok) throw new Error("HTTP " + res.status);
    const data = await res.json();
    fileTreeEl.innerHTML = "";
    if (!data.files || data.files.length === 0) {
      fileTreeEl.innerHTML = '<div class="empty-hint">沒有任何檔案</div>';
      return;
    }
    data.files.forEach(node => fileTreeEl.appendChild(renderNode(node)));
    // 還原目前開啟檔案的高亮（重建後 DOM 是全新的，active 標示需重新套用）
    if (state.currentPath) {
      const active = fileTreeEl.querySelector('.tree-label[data-path="' + CSS.escape(state.currentPath) + '"]');
      if (active) active.classList.add("active");
    }
    updateTreeDots(); // 重新渲染後，依最新 presence 重新標示小圓點
    fileTreeEl.scrollTop = savedScroll;
  } catch (err) {
    fileTreeEl.innerHTML = '<div class="empty-hint">載入失敗：' + err.message + '</div>';
  }
}

// ===== 建立節點操作按鈕（重新命名 / 刪除）=====
function buildNodeActions(node) {
  const actions = document.createElement("span");
  actions.className = "node-actions";

  // 資料夾額外提供「在此新增子檔案 / 子資料夾」（可多層建立）
  if (node.isDir) {
    const newFileBtn = document.createElement("button");
    newFileBtn.innerHTML = '<i class="fa fa-file-o"></i>';
    newFileBtn.title = "在此資料夾新增檔案";
    newFileBtn.addEventListener("click", (e) => { e.stopPropagation(); createItem("file", node.path); });

    const newDirBtn = document.createElement("button");
    newDirBtn.innerHTML = '<i class="fa fa-folder-o"></i>';
    newDirBtn.title = "在此資料夾新增子資料夾";
    newDirBtn.addEventListener("click", (e) => { e.stopPropagation(); createItem("dir", node.path); });

    actions.appendChild(newFileBtn);
    actions.appendChild(newDirBtn);
  }

  const renameBtn = document.createElement("button");
  renameBtn.innerHTML = '<i class="fa fa-pencil"></i>';
  renameBtn.title = "重新命名";
  renameBtn.addEventListener("click", (e) => { e.stopPropagation(); renameItem(node); });

  const delBtn = document.createElement("button");
  delBtn.innerHTML = '<i class="fa fa-trash-o"></i>';
  delBtn.title = "刪除";
  delBtn.addEventListener("click", (e) => { e.stopPropagation(); deleteItem(node); });

  actions.appendChild(renameBtn);
  actions.appendChild(delBtn);
  return actions;
}

// ===== 遞迴渲染樹狀節點 =====
function renderNode(node) {
  const wrap = document.createElement("div");
  wrap.className = "tree-node";

  const label = document.createElement("div");
  label.className = "tree-label";

  const icon = document.createElement("span");
  icon.className = "toggle-icon";

  const name = document.createElement("span");
  name.className = "tree-name";

  if (node.isDir) {
    name.appendChild(makeGlyph("fa-folder-o"));
    name.append(node.name);
    label.appendChild(icon);
    label.appendChild(name);
    label.appendChild(buildNodeActions(node));
    label.dataset.path = node.path;

    const childrenWrap = document.createElement("div");
    childrenWrap.className = "tree-children";
    (node.children || []).forEach(child => childrenWrap.appendChild(renderNode(child)));

    // 依保留的展開狀態決定初始收合（預設收合）
    const expanded = expandedDirs.has(node.path);
    childrenWrap.classList.toggle("collapsed", !expanded);
    icon.textContent = expanded ? "▾" : "▸";

    label.addEventListener("click", () => {
      const collapsed = childrenWrap.classList.toggle("collapsed");
      icon.textContent = collapsed ? "▸" : "▾";
      if (collapsed) expandedDirs.delete(node.path);
      else expandedDirs.add(node.path);
    });

    wrap.appendChild(label);
    wrap.appendChild(childrenWrap);
  } else {
    icon.textContent = "";
    name.appendChild(makeGlyph("fa-file-text-o"));
    name.append(node.name);
    label.appendChild(icon);
    label.appendChild(name);
    label.appendChild(buildNodeActions(node));
    label.dataset.path = node.path;

    label.addEventListener("click", () => openFile(node.path, label));
    wrap.appendChild(label);
  }
  return wrap;
}

// ===== 檔案管理：新增（baseDir 為要建立在哪個資料夾底下，空值代表根目錄）=====
export async function createItem(type, baseDir) {
  const label = type === "dir" ? "資料夾" : "檔案";
  const where = baseDir ? "「" + baseDir + "」底下" : "根目錄";
  let name = await promptModal(
    "在" + where + "新增" + label + "，請輸入名稱" +
    (type === "file" ? "（需以 .md 或 .txt 結尾）：" : "：")
  );
  if (!name) return;
  name = name.trim();
  if (!name) return;

  // 組出完整相對路徑；名稱本身也允許再帶子層（例如 sub/a.md）
  const fullPath = baseDir ? baseDir + "/" + name : name;
  expandAncestors(fullPath); // 確保新項目所在的資料夾在重建後是展開的

  try {
    const res = await authFetch(
      API_BASE + "/api/create?path=" + encodeURIComponent(fullPath) + "&type=" + type,
      { method: "POST" }
    );
    await ensureOk(res);
    showToast("建立成功", "success");
    await loadFileTree();
    if (type === "file") openFileByPath(fullPath);
  } catch (err) {
    showToast("建立失敗：" + err.message, "error");
  }
}

// ===== 檔案管理：重新命名 / 移動 =====
export async function renameItem(node) {
  const newPath = await promptModal("請輸入新的路徑（相對於文件根目錄）：", node.path);
  if (!newPath || newPath.trim() === "" || newPath.trim() === node.path) return;
  expandAncestors(newPath.trim()); // 移動後讓目標所在資料夾在重建後可見

  try {
    const res = await authFetch(
      API_BASE + "/api/rename?path=" + encodeURIComponent(node.path) +
      "&newPath=" + encodeURIComponent(newPath.trim()),
      { method: "POST" }
    );
    await ensureOk(res);
    showToast("重新命名成功", "success");
    if (state.currentPath === node.path) {
      state.currentPath = newPath.trim();
      fileNameEl.textContent = state.currentPath;
    }
    await loadFileTree();
  } catch (err) {
    showToast("重新命名失敗：" + err.message, "error");
  }
}

// ===== 檔案管理：刪除 =====
export async function deleteItem(node) {
  const label = node.isDir ? "資料夾（含底下所有內容）" : "檔案";
  if (!(await confirmModal("確定要刪除此" + label + "嗎？\n" + node.path, { okText: "刪除" }))) return;

  try {
    const res = await authFetch(
      API_BASE + "/api/file?path=" + encodeURIComponent(node.path),
      { method: "DELETE" }
    );
    await ensureOk(res);
    showToast("刪除成功", "success");
    if (state.currentPath === node.path) resetWorkspace();
    await loadFileTree();
  } catch (err) {
    showToast("刪除失敗：" + err.message, "error");
  }
}
