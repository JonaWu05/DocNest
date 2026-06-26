// 資源回收筒：列出已刪除（軟刪除）的項目，可還原回原始位置或永久刪除。
// 後端只會回傳「使用者對其原始路徑有寫入權」的項目（見 trash.go ListTrash）。
import { API_BASE } from "./state.js";
import { authFetch, ensureOk } from "./auth.js";
import { showToast } from "./ui.js";
import { loadFileTree } from "./fileTree.js";
import { confirmModal } from "./modal.js";

const modal = document.getElementById("trash-modal");
const listEl = document.getElementById("trash-list");
const hintEl = document.getElementById("trash-hint");

// openTrash 開啟資源回收筒並載入項目。
export function openTrash() {
  modal.classList.remove("hidden");
  loadTrash();
}

// closeTrash 關閉資源回收筒。
export function closeTrash() {
  modal.classList.add("hidden");
}

// loadTrash 向後端取回收筒項目並渲染（後端只回傳對其原始路徑有寫入權者）。
async function loadTrash() {
  listEl.innerHTML = "";
  hintEl.textContent = "載入中…";
  try {
    const res = await authFetch(API_BASE + "/api/trash");
    await ensureOk(res);
    const items = (await res.json()).items || [];
    if (!items.length) {
      hintEl.textContent = "資源回收筒是空的。";
      return;
    }
    hintEl.textContent = "共 " + items.length + " 項，還原會放回原始位置。";
    items.forEach(it => listEl.appendChild(renderItem(it)));
  } catch (err) {
    hintEl.textContent = "載入失敗：" + err.message;
  }
}

// renderItem 建立單一回收筒項目列（名稱 / 原始路徑 / 刪除時間，附還原與永久刪除鈕）。
function renderItem(item) {
  const el = document.createElement("div");
  el.className = "trash-item";

  const meta = document.createElement("div");
  meta.className = "trash-item-meta";

  const name = document.createElement("div");
  name.className = "trash-item-name";
  name.innerHTML = '<i class="fa ' + (item.isDir ? "fa-folder-o" : "fa-file-text-o") + '"></i> ';
  name.append(item.name);

  const sub = document.createElement("div");
  sub.className = "trash-item-path";
  sub.textContent = item.original + "　·　" + formatTime(item.deletedAt);

  meta.appendChild(name);
  meta.appendChild(sub);

  const actions = document.createElement("div");
  actions.className = "trash-item-actions";
  const restoreBtn = document.createElement("button");
  restoreBtn.className = "tool-btn";
  restoreBtn.textContent = "還原";
  restoreBtn.addEventListener("click", () => restore(item));
  const purgeBtn = document.createElement("button");
  purgeBtn.className = "tool-btn";
  purgeBtn.textContent = "永久刪除";
  purgeBtn.addEventListener("click", () => purge(item));
  actions.appendChild(restoreBtn);
  actions.appendChild(purgeBtn);

  el.appendChild(meta);
  el.appendChild(actions);
  return el;
}

// 把 RFC3339 時間轉成在地可讀格式；解析失敗則原樣顯示
function formatTime(s) {
  const d = new Date(s);
  return isNaN(d.getTime()) ? s : d.toLocaleString();
}

// restore 還原項目回原始位置，並重載清單與檔案樹以反映變更。
async function restore(item) {
  try {
    const res = await authFetch(API_BASE + "/api/trash/restore?id=" + encodeURIComponent(item.id), { method: "POST" });
    await ensureOk(res);
    showToast("已還原：" + item.name, "success");
    await loadTrash();
    await loadFileTree(); // 還原後檔案樹需反映新項目
  } catch (err) {
    showToast("還原失敗：" + err.message, "error");
  }
}

// purge 永久刪除項目（先以對話框二次確認，無法復原）。
async function purge(item) {
  if (!(await confirmModal("永久刪除「" + item.name + "」？此動作無法復原。", { okText: "永久刪除" }))) return;
  try {
    const res = await authFetch(API_BASE + "/api/trash?id=" + encodeURIComponent(item.id), { method: "DELETE" });
    await ensureOk(res);
    showToast("已永久刪除：" + item.name, "success");
    await loadTrash();
  } catch (err) {
    showToast("永久刪除失敗：" + err.message, "error");
  }
}
