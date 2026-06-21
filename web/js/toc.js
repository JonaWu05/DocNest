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
}

// 點 TOC 項目：捲動預覽到對應標題（編輯模式下先切到分割以顯示預覽）
export function gotoHeading(i) {
  if (state.currentMode === "edit") applyMode("split");
  const target = previewPane.querySelector("#toc-h-" + i);
  if (target) target.scrollIntoView({ behavior: "smooth", block: "start" });
}
