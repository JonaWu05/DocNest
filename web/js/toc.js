// 文件目錄（TOC）：依目前文件的標題建立大綱，點擊跳轉到對應段落。
import { state } from "./state.js";
import { tocList, previewPane } from "./dom.js";
import { applyMode } from "./editor.js";

// 索引與 renderPreview 為標題加的 id（toc-h-i）一一對應
export function buildTOC() {
  if (!state.currentPath) {
    tocList.innerHTML = '<div class="toc-empty">未開啟檔案</div>';
    return;
  }
  let tokens = [];
  try { tokens = marked.lexer(state.currentContent || ""); } catch (e) { tokens = []; }
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
  updateActiveHeading(); // 重建後立即標出目前所在段落
}

// ===== Scroll-spy：捲動預覽時高亮目前所在的標題 =====
// TOC 項目與預覽標題以相同索引一一對應（見 renderPreview 的 toc-h-i 與上方建立順序）。
let spyScheduled = false;

function updateActiveHeading() {
  const items = tocList.querySelectorAll(".toc-item");
  const headings = previewPane.querySelectorAll("h1,h2,h3,h4,h5,h6");
  if (!items.length || !headings.length) return;

  // 取「捲動位置之上、最接近頂端」的標題作為目前段落
  const baseTop = previewPane.getBoundingClientRect().top;
  let activeIdx = 0;
  for (let i = 0; i < headings.length; i++) {
    if (headings[i].getBoundingClientRect().top - baseTop <= 12) activeIdx = i;
    else break;
  }

  items.forEach((it, i) => it.classList.toggle("active", i === activeIdx));

  // 僅在 active 項超出 TOC 可視範圍時才捲動，避免干擾使用者
  const activeItem = items[activeIdx];
  if (activeItem) {
    const lr = tocList.getBoundingClientRect();
    const ar = activeItem.getBoundingClientRect();
    if (ar.top < lr.top || ar.bottom > lr.bottom) activeItem.scrollIntoView({ block: "nearest" });
  }
}

function onPreviewScroll() {
  if (spyScheduled) return;
  spyScheduled = true;
  requestAnimationFrame(() => { spyScheduled = false; updateActiveHeading(); });
}

// 啟動時綁定一次（previewPane 內容每次重繪後標題仍可即時讀取，不需重新綁定）
export function initScrollSpy() {
  previewPane.addEventListener("scroll", onPreviewScroll);
}

// 點 TOC 項目：捲動預覽到對應標題（編輯模式下先切到分割以顯示預覽）
export function gotoHeading(i) {
  if (state.currentMode === "edit") applyMode("split");
  const target = previewPane.querySelector("#toc-h-" + i);
  if (target) target.scrollIntoView({ behavior: "smooth", block: "start" });
}
