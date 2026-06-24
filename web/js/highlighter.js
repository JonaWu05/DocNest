// 程式碼語法高亮：highlight.js 延遲載入——只有預覽含 code block 時才注入，
// 純預覽/無程式碼的文件完全不下載。執行期一律由本地 /static/vendor 提供，不依賴外部 CDN。
// common build 已含 go/js/ts/json/bash/yaml/sql/python/xml/css/markdown/diff/ini/c/cpp/makefile 等；
// 另外載入 common 未含的 dockerfile/powershell/nginx。

let hljsPromise = null;
let themeLinks = null;

// 注入單一腳本，回傳 Promise。
function loadScript(src) {
  return new Promise((resolve, reject) => {
    const s = document.createElement("script");
    s.src = src;
    s.onload = () => resolve();
    s.onerror = () => reject(new Error("載入失敗：" + src));
    document.head.appendChild(s);
  });
}

// 確保核心與額外語言已載入（單例 Promise；失敗不快取以便重試）。
function loadHljs() {
  if (window.hljs) return Promise.resolve();
  if (hljsPromise) return hljsPromise;
  const base = "/static/vendor/highlight";
  hljsPromise = loadScript(base + "/highlight.min.js")
    .then(() => Promise.all([
      loadScript(base + "/languages/dockerfile.min.js"),
      loadScript(base + "/languages/powershell.min.js"),
      loadScript(base + "/languages/nginx.min.js"),
    ]))
    .catch((err) => { hljsPromise = null; throw err; });
  return hljsPromise;
}

// 注入亮、暗兩份主題樣式（首次需要高亮時才注入）。
// 兩份都注入、用 disabled 切換，切主題時零延遲、不需重新下載。
function ensureThemeLinks() {
  if (themeLinks) return;
  const base = "/static/vendor/highlight/styles";
  const mk = (name) => {
    const l = document.createElement("link");
    l.rel = "stylesheet";
    l.href = base + "/" + name + ".min.css";
    document.head.appendChild(l);
    return l;
  };
  themeLinks = { light: mk("github"), dark: mk("github-dark") };
}

// 依目前 body.dark 啟用對應的高亮主題（由 applyTheme 與首次高亮時呼叫）。
// 尚未載入高亮（文件沒有 code block）時不做事。
export function applyHighlightTheme() {
  if (!themeLinks) return;
  const dark = document.body.classList.contains("dark");
  themeLinks.light.disabled = dark;
  themeLinks.dark.disabled = !dark;
}

// 對容器內所有 <pre><code> 套用語法高亮。無 code block 時直接返回，完全不載入任何資源。
// 高亮 span 由「已消毒的文字」產生（在 DOMPurify sanitize 之後才呼叫），不引入 XSS。
export function highlightWithin(container) {
  const blocks = container.querySelectorAll("pre code");
  if (!blocks.length) return;
  loadHljs().then(() => {
    ensureThemeLinks();
    applyHighlightTheme();
    blocks.forEach((el) => window.hljs.highlightElement(el));
  }).catch(() => { /* 載入失敗則維持純文字，不影響閱讀 */ });
}
