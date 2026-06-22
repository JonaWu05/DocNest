// 動態載入大型第三方相依：EasyMDE（約 326KB JS + CSS）只在進入編輯 / 分割模式時才需要，
// 登入頁與純預覽完全用不到。改成需要時才注入，可把它移出首屏關鍵路徑、縮短 LCP。
let easyMDEPromise = null;

// loadEasyMDE 確保 EasyMDE 的 JS 與 CSS 已載入，回傳一個 Promise。
// 以單例 Promise 避免重複注入；載入失敗時清空快取以便下次重試。
export function loadEasyMDE() {
  if (window.EasyMDE) return Promise.resolve();
  if (easyMDEPromise) return easyMDEPromise;

  easyMDEPromise = new Promise((resolve, reject) => {
    // 樣式：與 JS 一起延後載入（原本在 index.html 的 <head> 同步引入，會阻擋首屏渲染）
    const css = document.createElement("link");
    css.rel = "stylesheet";
    css.href = "/static/vendor/easymde.min.css";
    document.head.appendChild(css);

    const script = document.createElement("script");
    script.src = "/static/vendor/easymde.min.js";
    script.onload = () => resolve();
    script.onerror = () => {
      easyMDEPromise = null; // 失敗不快取，允許重試
      reject(new Error("EasyMDE 載入失敗"));
    };
    document.head.appendChild(script);
  });

  return easyMDEPromise;
}
