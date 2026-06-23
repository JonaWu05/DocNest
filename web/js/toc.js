// 文件目錄（TOC）：依目前文件的標題建立大綱，點擊跳轉到對應段落。
import { state } from "./state.js";
import { tocList, previewPane } from "./dom.js";
import { applyMode } from "./editor.js";
import { currentTokens } from "./markdown.js";

// 索引與 renderPreview 為標題加的 id（toc-h-i）一一對應
export function buildTOC() {
  if (!state.currentPath) {
    tocList.innerHTML = '<div class="toc-empty">未開啟檔案</div>';
    return;
  }
  let tokens = [];
  try { tokens = currentTokens(); } catch (e) { tokens = []; } // 與預覽共用同一次 lex
  const headings = tokens.filter(t => t.type === "heading");
  tocList.innerHTML = "";
  if (!headings.length) {
    tocList.innerHTML = '<div class="toc-empty">（此文件沒有標題）</div>';
    return;
  }
  headings.forEach((h, i) => {
    const item = document.createElement("div");
    item.className = "toc-item";
    // 依標題層級（depth 1~6）縮排
    item.style.paddingLeft = (8 + (h.depth - 1) * 14) + "px";
    item.textContent = h.text;
    item.title = h.text;
    item.addEventListener("click", () => gotoHeading(i));
    tocList.appendChild(item);
  });
  refreshScrollSpy(); // 重建後依新標題重設觀察並標出目前所在段落
}

// ===== Scroll-spy：以 IntersectionObserver 高亮目前所在的標題 =====
// 在預覽頂端設一條「啟用區帶」（rootMargin 把底部裁掉 80%，只留頂端 20%）；
// 標題捲入此區帶即視為目前段落。改用 IO 後不必每次捲動 frame 讀全部標題位置。
// TOC 項目與預覽標題以相同索引一一對應（見 renderPreview 的 toc-h-i 與上方建立順序）。
let spyObserver = null;

function setActiveTocItem(idx) {
  const items = tocList.querySelectorAll(".toc-item");
  items.forEach((it, i) => it.classList.toggle("active", i === idx));
  // 僅在 active 項超出 TOC 可視範圍時才捲動，避免干擾使用者
  const activeItem = items[idx];
  if (activeItem) {
    const lr = tocList.getBoundingClientRect();
    const ar = activeItem.getBoundingClientRect();
    if (ar.top < lr.top || ar.bottom > lr.bottom) activeItem.scrollIntoView({ block: "nearest" });
  }
}

// 依目前預覽中的標題重建觀察器（內容/標題改變或切換檔案後呼叫）。
function refreshScrollSpy() {
  if (spyObserver) { spyObserver.disconnect(); spyObserver = null; }
  const headings = previewPane.querySelectorAll("h1,h2,h3,h4,h5,h6");
  if (!headings.length) return;

  spyObserver = new IntersectionObserver((entries) => {
    // 同一批可能多個標題進出區帶，取進入區帶中索引最大者（最接近區帶底＝最新跨入）為 active
    let bestIdx = -1;
    for (const e of entries) {
      if (e.isIntersecting) {
        const idx = Number(e.target.dataset.tocIndex);
        if (idx > bestIdx) bestIdx = idx;
      }
    }
    if (bestIdx >= 0) setActiveTocItem(bestIdx);
  }, {
    root: previewPane,
    rootMargin: "0px 0px -80% 0px", // 只在預覽頂端 20% 區帶內觸發
    threshold: 0,
  });

  headings.forEach((h, i) => {
    h.dataset.tocIndex = i;
    spyObserver.observe(h);
  });
}

// 點 TOC 項目：捲動預覽到對應標題（編輯模式下先切到分割以顯示預覽）
export function gotoHeading(i) {
  if (state.currentMode === "edit") applyMode("split");
  const target = previewPane.querySelector("#toc-h-" + i);
  if (target) target.scrollIntoView({ behavior: "smooth", block: "start" });
}
