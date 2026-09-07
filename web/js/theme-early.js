// 提早套用深/淺色偏好，避免頁面先以淺色渲染、main.js 載入後才切深色造成的畫面閃爍（FOUC）。
// 此頁（admin.html）不載入 main.js，故獨立處理；原本是 inline <script>，現抽成外部檔，
// 讓 CSP 的 script-src 可拿掉 unsafe-inline。本檔須維持一般 <script>（非 module/defer），
// 並緊接在 <body> 開始標籤後載入，才能在其餘畫面繪出前完成套用。
if (localStorage.getItem("theme") === "dark") {
  document.body.classList.add("dark");
}
