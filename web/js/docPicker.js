// 分頁連結選擇器：列出工作區內所有文件（.md / .txt），挑選後在游標處插入
// 連到該文件的 Markdown 連結。預覽時 preview.js 會把 .md/.txt 連結轉成站內轉跳。
import { state, API_BASE } from "./state.js";
import { docModal, docList, docSearch } from "./dom.js";
import { showToast } from "./ui.js";
import { authFetch } from "./auth.js";
import { relativeFromDocDir } from "./util.js";
import { insertIntoEditor } from "./editor.js";

let allDocs = []; // 攤平後的文件清單 [{ name, path }]

// 將 /api/files 的巢狀樹攤平成文件清單（只取檔案、排除目前開啟的文件本身）
function flatten(nodes, acc) {
  for (const node of nodes) {
    if (node.isDir) {
      flatten(node.children || [], acc);
    } else if (/\.(md|txt)$/i.test(node.name)) {
      acc.push({ name: node.name, path: node.path });
    }
  }
  return acc;
}

export async function openDocPicker() {
  if (!state.currentPath) return;
  docModal.classList.remove("hidden");
  docSearch.value = "";
  docList.innerHTML = '<div class="doc-hint">載入中…</div>';
  try {
    const res = await authFetch(API_BASE + "/api/files");
    if (!res.ok) throw new Error("HTTP " + res.status);
    const data = await res.json();
    allDocs = flatten(data.files || [], []).filter(d => d.path !== state.currentPath);
    renderDocList("");
    docSearch.focus();
  } catch (err) {
    docList.innerHTML = '<div class="doc-hint">載入失敗：' + err.message + '</div>';
  }
}

export function closeDocPicker() {
  docModal.classList.add("hidden");
}

export function renderDocList(keyword) {
  const kw = (keyword || "").trim().toLowerCase();
  const docs = kw ? allDocs.filter(d => d.path.toLowerCase().includes(kw)) : allDocs;
  docList.innerHTML = "";
  if (!docs.length) {
    docList.innerHTML = '<div class="doc-hint">' + (allDocs.length ? "沒有符合的文件" : "工作區沒有其他文件") + "</div>";
    return;
  }
  docs.forEach(d => docList.appendChild(renderDocItem(d)));
}

function renderDocItem(doc) {
  const el = document.createElement("div");
  el.className = "doc-item";
  el.title = doc.path;
  el.innerHTML = '<i class="fa fa-file-text-o doc-item-icon"></i>';

  const meta = document.createElement("div");
  meta.className = "doc-item-meta";
  const name = document.createElement("div");
  name.className = "doc-item-name";
  name.textContent = doc.name;
  const path = document.createElement("div");
  path.className = "doc-item-path";
  path.textContent = doc.path;
  meta.appendChild(name);
  meta.appendChild(path);
  el.appendChild(meta);

  el.addEventListener("click", () => insertDocLink(doc));
  return el;
}

// 插入連到所選文件的連結（換算成相對於目前文件目錄的路徑，確保可攜）
function insertDocLink(doc) {
  if (!state.currentPath) return;
  const rel = relativeFromDocDir(doc.path);
  const label = doc.name.replace(/\.(md|txt)$/i, "");
  insertIntoEditor("[" + label + "](" + rel + ")");
  showToast("已插入分頁連結：" + label, "success");
  closeDocPicker();
}
