// Presence 顯示：在線人數、誰在看/編輯哪個檔案、檔案樹小圓點標示。
import { state } from "./state.js";

let onlineUsers = []; // 最新一次 presence_update 的使用者清單（含自己）

const onlineCountEl = document.getElementById("online-count");
const presenceBar = document.getElementById("presence-bar");

let onlinePopover = null;   // 在線成員清單浮層（首次開啟時建立）
let popoverOpen = false;

// 收到後端 presence_update 時呼叫
export function handlePresenceUpdate(payload) {
  onlineUsers = (payload && payload.users) || [];
  renderPresence();
  updateTreeDots();
  if (popoverOpen) renderOnlinePopover(); // 浮層開著時即時更新內容
}

function chip(text, kind) {
  const el = document.createElement("span");
  el.className = "presence-chip " + kind;
  el.textContent = text;
  return el;
}

function renderPresence() {
  // 右上角在線人數（含自己）
  if (onlineCountEl) onlineCountEl.textContent = "在線 " + onlineUsers.length + " 人";
  if (!presenceBar) return;

  const me = state.username;
  const myFile = state.currentPath || "";
  const sameFile = []; // 與我看同一個檔案的人
  const others = [];   // 其他在線（看不同檔案或未選取）

  onlineUsers.forEach(u => {
    if (u.username === me) return; // 不顯示自己
    if (myFile && u.current_file === myFile) sameFile.push(u);
    else others.push(u);
  });

  presenceBar.innerHTML = "";
  if (!sameFile.length && !others.length) {
    presenceBar.classList.add("empty");
    return;
  }
  presenceBar.classList.remove("empty");

  // 查看同一檔案者：區分編輯中 / 預覽中
  sameFile.forEach(u => presenceBar.appendChild(chip(
    (u.is_editing ? "✏️ " : "👁 ") + u.username + (u.is_editing ? "（編輯中）" : "（預覽中）"),
    u.is_editing ? "editing" : "viewing"
  )));
  // 其他在線者
  others.forEach(u => presenceBar.appendChild(chip("🟢 " + u.username, "other")));
}

// ===== 在線成員清單浮層（點右上角「在線 N 人」開啟）=====

// 依使用者狀態決定圖示與說明
function statusOf(u) {
  if (u.is_editing) return { icon: "fa-pencil", cls: "editing", text: "編輯中" };
  if (u.current_file) return { icon: "fa-eye", cls: "viewing", text: "瀏覽中" };
  return { icon: "fa-circle", cls: "idle", text: "在線" };
}

function ensurePopover() {
  if (onlinePopover) return;
  onlinePopover = document.createElement("div");
  onlinePopover.className = "online-popover hidden";
  document.body.appendChild(onlinePopover);
}

function renderOnlinePopover() {
  if (!onlinePopover) return;
  const me = state.username;
  onlinePopover.innerHTML = "";

  const head = document.createElement("div");
  head.className = "online-popover-head";
  head.textContent = "在線 " + onlineUsers.length + " 人";
  onlinePopover.appendChild(head);

  // 自己排在最前
  const sorted = [...onlineUsers].sort((a, b) =>
    a.username === me ? -1 : b.username === me ? 1 : 0);

  sorted.forEach(u => {
    const s = statusOf(u);
    const row = document.createElement("div");
    row.className = "online-row";

    const ic = document.createElement("i");
    ic.className = "fa " + s.icon + " online-row-icon " + s.cls;

    const textCol = document.createElement("span");
    textCol.className = "online-row-text";

    const name = document.createElement("span");
    name.className = "online-row-name";
    name.textContent = u.username + (u.username === me ? "（你）" : "");

    const meta = document.createElement("span");
    meta.className = "online-row-meta";
    meta.textContent = u.current_file ? (s.text + " · " + u.current_file) : s.text;

    textCol.appendChild(name);
    textCol.appendChild(meta);
    row.appendChild(ic);
    row.appendChild(textCol);
    onlinePopover.appendChild(row);
  });
}

function openPopover() {
  ensurePopover();
  renderOnlinePopover();
  // 對齊「在線 N 人」下方、靠右
  const r = onlineCountEl.getBoundingClientRect();
  onlinePopover.style.top = (r.bottom + 6) + "px";
  onlinePopover.style.right = Math.max(8, window.innerWidth - r.right) + "px";
  onlinePopover.classList.remove("hidden");
  popoverOpen = true;
  document.addEventListener("mousedown", onOutside, true);
  document.addEventListener("keydown", onEsc, true);
}

function closePopover() {
  if (!popoverOpen) return;
  onlinePopover.classList.add("hidden");
  popoverOpen = false;
  document.removeEventListener("mousedown", onOutside, true);
  document.removeEventListener("keydown", onEsc, true);
}

function onOutside(e) {
  if (onlineCountEl.contains(e.target) || onlinePopover.contains(e.target)) return;
  closePopover();
}
function onEsc(e) { if (e.key === "Escape") closePopover(); }

if (onlineCountEl) {
  onlineCountEl.title = "點擊查看在線成員";
  onlineCountEl.addEventListener("click", () => (popoverOpen ? closePopover() : openPopover()));
}

// 檔案樹標示：有「其他人」正在查看/編輯的檔案旁加上小圓點。
// 檔案樹會被 loadFileTree 重新渲染，故重載後也需再呼叫一次（見 fileTree.js）。
export function updateTreeDots() {
  const me = state.username;
  const busy = new Set();
  onlineUsers.forEach(u => {
    if (u.username === me) return;
    if (u.current_file) busy.add(u.current_file);
  });

  document.querySelectorAll(".tree-label").forEach(label => {
    const path = label.dataset.path; // 僅檔案節點有 data-path
    const existing = label.querySelector(".tree-dot");
    if (path && busy.has(path)) {
      if (!existing) {
        const dot = document.createElement("span");
        dot.className = "tree-dot";
        dot.title = "有人正在此檔案";
        dot.textContent = "●";
        label.appendChild(dot);
      }
    } else if (existing) {
      existing.remove();
    }
  });
}
