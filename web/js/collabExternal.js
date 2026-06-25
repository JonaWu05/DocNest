// 共編外部改檔協調橫幅：當伺服器偵測到共編中的 .md 被外部改寫（git pull、腳本、伺服器直接編輯…），
// 僅向落檔者顯示，讓其二選一：保留目前共編版本（覆蓋磁碟），或改用磁碟版本（覆蓋在途共編內容）。
// 偵測與暫停自動落檔的邏輯在 collab.js；本模組只負責橫幅 UI 與抓取磁碟內容。
import { API_BASE, state } from "./state.js";
import { authFetch, ensureOk } from "./auth.js";
import { showToast } from "./ui.js";
import { collabResolveKeepMine, collabResolveUseDisk } from "./collab.js";

const bar = document.getElementById("collab-external-bar");
const keepBtn = document.getElementById("collab-ext-keep");
const diskBtn = document.getElementById("collab-ext-disk");

function hide() {
  if (bar) bar.classList.add("hidden");
}

// setCollabExternal 由 collab.js 註冊為 onExternalChange：active=true 顯示橫幅、false 收掉（房間切換 / 斷線）。
export function setCollabExternal(active) {
  if (!bar) return;
  bar.classList.toggle("hidden", !active);
}

// 保留共編版本：恢復落檔，以目前共編內容覆蓋磁碟上的外部變更。
if (keepBtn) {
  keepBtn.addEventListener("click", () => {
    collabResolveKeepMine();
    hide();
    showToast("已保留共編版本，將覆蓋磁碟上的外部變更", "info");
  });
}

// 改用磁碟版本：抓取磁碟最新內容灌回共享文件（經 CRDT 傳播給全房），覆蓋目前在途的共編編輯。
if (diskBtn) {
  diskBtn.addEventListener("click", async () => {
    const path = state.currentPath;
    diskBtn.disabled = true;
    try {
      const res = await authFetch(API_BASE + "/api/file?path=" + encodeURIComponent(path));
      await ensureOk(res);
      const text = await res.text();
      collabResolveUseDisk(text);
      hide();
      showToast("已改用磁碟版本（原共編內容已被覆蓋）", "info");
    } catch (err) {
      showToast("讀取磁碟版本失敗：" + err.message, "error");
    } finally {
      diskBtn.disabled = false;
    }
  });
}
