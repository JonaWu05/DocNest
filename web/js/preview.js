// Markdown 預覽渲染：將相對的圖片 / 附件連結改寫指向後端 raw 服務。
import { state } from "./state.js";
import { previewPane } from "./dom.js";
import { resolveAssetPath } from "./util.js";
import { rawUrl } from "./auth.js";
import { openFileByPath } from "./editor.js";
import { currentTokens } from "./markdown.js";
import { highlightWithin } from "./highlighter.js";

export function renderPreview() {
  // 先以 marked 轉成 HTML，再經 DOMPurify 消毒後才寫入 DOM。
  // 這是必要的防護：Markdown 允許內嵌原始 HTML，且協作模式下他人儲存的內容
  // 會在本機渲染，未消毒即等同儲存型 XSS（可竊取 token）。
  // 解析共用 currentTokens（與 TOC 共享同一次 lex），再以 marked.parser 產生 HTML。
  previewPane.innerHTML = DOMPurify.sanitize(marked.parser(currentTokens()));
  // 依文件順序為標題加上 id，與 TOC 項目的索引一一對應，供點擊跳轉
  previewPane.querySelectorAll("h1,h2,h3,h4,h5,h6").forEach((h, i) => { h.id = "toc-h-" + i; });
  // 圖片：相對路徑改指向 /api/raw 才能顯示（rawUrl 會夾帶 token）。
  // 帶上 from＝目前文件，讓無 asset 直接讀取權的閱讀者也能看本頁引用的圖（後端來源驗證）。
  // lazy + async：圖多的文件不必一次全載/全解碼，加快首屏與捲動順暢度。
  previewPane.querySelectorAll("img").forEach(img => {
    img.loading = "lazy";
    img.decoding = "async";
    const resolved = resolveAssetPath(img.getAttribute("src"));
    if (resolved) img.src = rawUrl(resolved, state.currentPath);
  });
  // 連結改寫：
  //   - 指向 .md / .txt 文件 → 站內開啟（點擊即在 app 內切換到該文件，不離開頁面）
  //   - 其餘相對連結（圖片/附件）→ 指向 raw，於新分頁開啟
  //   - 外部連結（http、mailto…）→ 不動
  previewPane.querySelectorAll("a").forEach(a => {
    const resolved = resolveAssetPath(a.getAttribute("href"));
    if (!resolved) return;
    if (/\.(md|txt)$/i.test(resolved)) {
      a.classList.add("doc-link");
      a.setAttribute("href", "#");
      a.addEventListener("click", (e) => {
        e.preventDefault();
        openFileByPath(resolved);
      });
    } else {
      a.href = rawUrl(resolved, state.currentPath); // from＝目前文件，供閱讀者下載本頁引用的附件
      a.target = "_blank";
      a.rel = "noopener noreferrer"; // 防 tabnabbing 與 referrer 外洩（連結含 token 的 raw URL）
    }
  });
  // 程式碼語法高亮（延遲載入 highlight.js；無 code block 則完全不載入）。
  // 在消毒後才呼叫，著色 span 由已消毒文字產生。
  highlightWithin(previewPane);
  addCopyButtons(previewPane);
}

// 為每個程式碼區塊加上右上角「複製」鈕（hover 顯示）。
function addCopyButtons(container) {
  container.querySelectorAll("pre").forEach(pre => {
    const code = pre.querySelector("code");
    if (!code) return;
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "code-copy-btn";
    btn.textContent = "複製";
    btn.addEventListener("click", async () => {
      const ok = await copyText(code.textContent);
      btn.textContent = ok ? "已複製" : "複製失敗";
      btn.classList.toggle("copied", ok);
      setTimeout(() => { btn.textContent = "複製"; btn.classList.remove("copied"); }, 1500);
    });
    pre.appendChild(btn);
  });
}

// 複製文字到剪貼簿：優先用 Clipboard API，非安全環境（http 非 localhost）則退回 execCommand。
async function copyText(text) {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch (e) { /* 落到下方備援 */ }
  try {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    const ok = document.execCommand("copy");
    document.body.removeChild(ta);
    return ok;
  } catch (e) {
    return false;
  }
}
