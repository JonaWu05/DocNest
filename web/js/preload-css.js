// 將 rel=preload 的樣式表下載完成後升級為 rel=stylesheet（避免阻擋首屏渲染）。
// 原本以 inline onload="" 屬性完成，現抽成外部檔，讓 CSP 的 script-src 可拿掉 unsafe-inline。
// 本檔為一般 <script>（非 module），插在對應的 <link rel="preload"> 之後即同步執行；
// load 事件一律非同步觸發，即使樣式表已在瀏覽器快取中也不會搶在這裡註冊監聽器之前發生。
document.querySelectorAll('link[rel="preload"][as="style"]').forEach((link) => {
  link.addEventListener("load", () => {
    link.rel = "stylesheet";
  });
});
