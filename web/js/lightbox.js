// 預覽圖片點擊放大：點圖開啟全螢幕 lightbox，點背景或按 Esc 關閉。
// 以事件委派綁在 previewPane 上，跨每次重繪都有效，不需逐圖重綁。
import { previewPane } from "./dom.js";

let overlay = null;
let overlayImg = null;

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

function open(src, alt) {
  ensureOverlay();
  overlayImg.src = src;
  overlayImg.alt = alt || "";
  overlay.classList.remove("hidden");
  document.addEventListener("keydown", onEsc, true);
}

function close() {
  if (!overlay) return;
  overlay.classList.add("hidden");
  overlayImg.removeAttribute("src"); // 釋放，避免關閉後仍占用
  document.removeEventListener("keydown", onEsc, true);
}

// capture + stopPropagation：lightbox 開啟時 Esc 只用來關它，不波及其他 Esc 行為
function onEsc(e) {
  if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); close(); }
}

export function initLightbox() {
  previewPane.addEventListener("click", (e) => {
    const img = e.target.closest("img");
    // 被連結包住的圖（[![](img)](url)）維持原本的開連結行為，不攔截
    if (img && previewPane.contains(img) && !img.closest("a")) {
      open(img.currentSrc || img.src, img.alt);
    }
  });
}
