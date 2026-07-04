// 使用者管理頁（/admin）。伺服器端已用認證 cookie 守門；此處再以 token 呼叫受保護 API，
// 任何未授權（401/403）一律導回首頁。所有變動後重新抓一次總覽並重繪（簡單且正確）。
import { API_BASE } from "./state.js";
import { getToken, authFetch } from "./auth.js";
import { confirmModal, promptModal } from "./modal.js";

const reloadBtn = document.getElementById("reload-btn");
const notice = document.getElementById("admin-notice");
const localList = document.getElementById("local-list");
const discordList = document.getElementById("discord-list");
const addUserForm = document.getElementById("add-user-form");
const addDiscordForm = document.getElementById("add-discord-form");

let overview = null; // 最近一次的總覽（含 groups / admin_group）

// 無 token 直接回首頁（守門的第一道；伺服器端仍是真正防線）
if (!getToken()) location.href = "/";

// bail 在遇到未授權時導回首頁。
function bail() { location.href = "/"; }

// api 呼叫受保護端點，回傳解析後的 JSON；未授權導回首頁，其他錯誤丟出訊息。
async function api(path, opts = {}) {
  let res;
  try {
    res = await authFetch(API_BASE + path, opts);
  } catch (e) {
    bail(); // authFetch 於 401 會清 token 並丟出
    throw e;
  }
  if (res.status === 401 || res.status === 403) { bail(); throw new Error("未授權"); }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || ("HTTP " + res.status));
  return data;
}

// load 抓取總覽並重繪。
async function load() {
  overview = await api("/api/admin/overview");
  render();
}

// render 依 overview 重繪本地帳號與 Discord 兩區。
function render() {
  // 未啟用權限分組時提示，並停用群組指派（accounts 管理仍可用）。
  if (!overview.perms_enabled) {
    notice.textContent = "未啟用權限分組（找不到 permissions.json），群組指派已停用；帳號管理仍可使用。";
    notice.classList.remove("hidden");
  } else {
    notice.classList.add("hidden");
  }

  localList.innerHTML = "";
  overview.local.forEach((u) => {
    localList.appendChild(row("local:" + u.username, u.username, u.groups, [
      btn("重設密碼", () => resetPassword(u.username)),
      btn("刪除", () => deleteUser(u.username), "danger"),
    ]));
  });
  if (overview.local.length === 0) localList.appendChild(emptyHint("尚無本地帳號"));

  discordList.innerHTML = "";
  overview.discord.forEach((d) => {
    // 顯示備註（沒有則顯示 ID）；有備註時把 ID 當副標，方便對照
    discordList.appendChild(row("discord:" + d.id, d.label || d.id, d.groups, [
      btn("重新命名", () => renameDiscord(d.id, d.label)),
      btn("移除", () => removeDiscord(d.id), "danger"),
    ], d.label ? d.id : ""));
  });
  if (overview.discord.length === 0) discordList.appendChild(emptyHint("尚無 Discord 白名單"));
}

// row 建立一列：名稱（可帶副標）+ 群組切換 chips + 操作按鈕。
function row(subject, name, groups, actions, subtitle) {
  const el = document.createElement("div");
  el.className = "admin-row";

  const nameEl = document.createElement("div");
  nameEl.className = "admin-row-name";
  nameEl.textContent = name;
  if (subtitle) {
    const sub = document.createElement("span");
    sub.className = "admin-row-sub";
    sub.textContent = subtitle;
    nameEl.appendChild(sub);
  }
  el.appendChild(nameEl);

  const groupsEl = document.createElement("div");
  groupsEl.className = "admin-row-groups";
  if (overview.perms_enabled) {
    const memberOf = groups || []; // 後端對無群組者可能回 null，防呆
    overview.groups.forEach((g) => {
      const on = memberOf.includes(g);
      const chip = document.createElement("button");
      chip.type = "button";
      chip.className = "group-chip" + (on ? " on" : "");
      chip.textContent = g;
      chip.title = on ? "點擊移出「" + g + "」" : "點擊加入「" + g + "」";
      chip.addEventListener("click", () => toggleGroup(g, subject, !on));
      groupsEl.appendChild(chip);
    });
  }
  el.appendChild(groupsEl);

  const actionsEl = document.createElement("div");
  actionsEl.className = "admin-row-actions";
  actions.forEach((b) => actionsEl.appendChild(b));
  el.appendChild(actionsEl);
  return el;
}

function btn(label, onClick, extra) {
  const b = document.createElement("button");
  b.type = "button";
  b.className = "tool-btn" + (extra ? " " + extra : "");
  b.textContent = label;
  b.addEventListener("click", onClick);
  return b;
}

function emptyHint(text) {
  const el = document.createElement("div");
  el.className = "admin-empty";
  el.textContent = text;
  return el;
}

// ===== 動作 =====

async function withReload(fn) {
  try { await fn(); await load(); }
  catch (err) { alert(err.message); }
}

function toggleGroup(group, subject, add) {
  return withReload(() => api("/api/admin/group/member", jsonBody({ group, subject, add })));
}

function deleteUser(username) {
  return withReload(async () => {
    if (!(await confirmModal("確定刪除帳號「" + username + "」？"))) return;
    await api("/api/admin/user/delete", jsonBody({ username }));
  });
}

function resetPassword(username) {
  return withReload(async () => {
    const pw = await promptModal("為「" + username + "」設定新密碼：");
    if (pw === null || pw === "") return;
    await api("/api/admin/user/password", jsonBody({ username, password: pw }));
  });
}

function removeDiscord(id) {
  return withReload(async () => {
    if (!(await confirmModal("確定移除 Discord ID「" + id + "」？"))) return;
    await api("/api/admin/discord/remove", jsonBody({ id }));
  });
}

function renameDiscord(id, current) {
  return withReload(async () => {
    const label = await promptModal("為此 Discord 成員設定備註（ID：" + id + "）：", current || "");
    if (label === null) return; // 取消（空字串代表清除備註，允許）
    await api("/api/admin/discord/label", jsonBody({ id, label }));
  });
}

addUserForm.addEventListener("submit", (e) => {
  e.preventDefault();
  const username = document.getElementById("new-username").value.trim();
  const password = document.getElementById("new-password").value;
  withReload(async () => {
    await api("/api/admin/user/create", jsonBody({ username, password }));
    addUserForm.reset();
  });
});

addDiscordForm.addEventListener("submit", (e) => {
  e.preventDefault();
  const id = document.getElementById("new-discord-id").value.trim();
  const label = document.getElementById("new-discord-label").value.trim();
  withReload(async () => {
    await api("/api/admin/discord/add", jsonBody({ id, label }));
    addDiscordForm.reset();
  });
});

reloadBtn.addEventListener("click", () => {
  reloadBtn.disabled = true;
  withReload(async () => {
    await api("/api/admin/reload", { method: "POST" });
    reloadBtn.textContent = "已重載 ✓";
    setTimeout(() => { reloadBtn.textContent = "重載設定"; }, 2000);
  }).finally(() => { reloadBtn.disabled = false; });
});

// jsonBody 組出帶 JSON 標頭與內容的 fetch options。
function jsonBody(obj) {
  return { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(obj) };
}

load().catch((err) => alert(err.message));
