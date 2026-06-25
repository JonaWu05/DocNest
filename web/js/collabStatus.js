// 共編狀態列：顯示「即時共編中」、房內參與者（色點對齊各自的游標顏色）、
// 誰負責落檔（💾）與最近一次落檔時間。資料來自 collab.js 的 awareness（onPresenceChange 回呼），
// 與全域 presence（presence.js，含未開此檔者）互補：此列只反映「真的連進這個 Yjs 房」的人。
const bar = document.getElementById("collab-bar");

// fmtTime 把時戳格式化為 HH:MM:SS。
function fmtTime(ts) {
  const d = new Date(ts);
  const p = (n) => String(n).padStart(2, "0");
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

// renderCollabStatus 依參與者清單重繪狀態列；清單為空（未共編 / 已斷線）時隱藏。
export function renderCollabStatus(participants) {
  if (!bar) return;
  if (!participants || !participants.length) {
    bar.classList.add("hidden");
    bar.innerHTML = "";
    return;
  }
  bar.classList.remove("hidden");
  bar.innerHTML = "";

  const head = document.createElement("span");
  head.className = "collab-bar-head";
  head.innerHTML = `<i class="fa fa-bolt"></i> 即時共編 · ${participants.length} 人`;
  bar.appendChild(head);

  // 參與者 chip：色點對齊游標顏色，自己標「你」，落檔者標 💾。
  participants.forEach((p) => {
    const chip = document.createElement("span");
    chip.className = "collab-user" + (p.self ? " self" : "");

    const dot = document.createElement("span");
    dot.className = "collab-dot";
    dot.style.background = p.color || "#888";
    chip.appendChild(dot);

    const label = document.createElement("span");
    label.textContent = p.name + (p.self ? "（你）" : "");
    chip.appendChild(label);

    if (p.saver) {
      const badge = document.createElement("span");
      badge.className = "collab-saver";
      badge.title = "負責將共編內容落檔";
      badge.textContent = "💾";
      chip.appendChild(badge);
    }
    bar.appendChild(chip);
  });

  // 最近落檔時間：取落檔者經 awareness 帶出的 savedAt（多人時取最大值即最新一次）。
  const saved = participants.reduce((m, p) => Math.max(m, p.savedAt || 0), 0);
  if (saved) {
    const t = document.createElement("span");
    t.className = "collab-saved";
    t.textContent = "已儲存 " + fmtTime(saved);
    bar.appendChild(t);
  }
}
