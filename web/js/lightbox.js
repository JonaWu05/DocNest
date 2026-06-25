// 預覽圖片點擊放大：點圖開啟全螢幕 lightbox，點背景或按 Esc 關閉。
// 以事件委派綁在 previewPane 上，跨每次重繪都有效，不需逐圖重綁。
import { previewPane } from "./dom.js";

let overlay = null;
let overlayImg = null;

// ensureOverlay 首次使用時建立 lightbox 的遮罩與圖片元素（之後重用同一份）。
function ensureOverlay() {
  if (overlay) return;
  overlay = document.createElement("div");
  overlay.id = "lightbox";
  overlay.className = "hidden";
  overlayImg = document.createElement("img");
  overlay.appendChild(overlayImg);
  overlay.addEventListener("click", close);
  document.body.appendChild(overlay);
}

// open 以指定圖片開啟全螢幕 lightbox，並掛上 Esc 關閉監聽。
function open(src, alt) {
  ensureOverlay();
  overlayImg.src = src;
  overlayImg.alt = alt || "";
  overlay.classList.remove("hidden");
  document.addEventListener("keydown", onEsc, true);
}

// close 關閉 lightbox 並釋放圖片來源（避免關閉後仍占用記憶體）。
function close() {
  if (!overlay) return;
  overlay.classList.add("hidden");
  overlayImg.removeAttribute("src"); // 釋放，避免關閉後仍占用
  document.removeEventListener("keydown", onEsc, true);
}

// onEsc 處理 lightbox 開啟時的 Esc：capture + stopPropagation，只用來關它、不波及其他 Esc 行為。
function onEsc(e) {
  if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); close(); }
}

// initLightbox 以事件委派在 previewPane 綁定「點圖放大」；委派故跨每次重繪都有效，不需逐圖重綁。
export function initLightbox() {
  previewPane.addEventListener("click", (e) => {
    const img = e.target.closest("img");
    // 被連結包住的圖（[![](img)](url)）維持原本的開連結行為，不攔截
    if (img && previewPane.contains(img) && !img.closest("a")) {
      open(img.currentSrc || img.src, img.alt);
    }
  });
}
