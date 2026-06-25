// 純工具函式：路徑換算與顯示格式化。
import { state } from "./state.js";

// 取得目前文件所在的目錄（相對於 DOC_ROOT）
function docDir() {
  if (!state.currentPath) return "";
  const i = state.currentPath.lastIndexOf("/");
  return i === -1 ? "" : state.currentPath.slice(0, i);
}

// 將「文件相對路徑」解析成「DOC_ROOT 相對路徑」；外部連結則回傳 null（不改寫）。
// 輸入來自 marked 的 img src / a href，marked 會將非 ASCII 字元 percent-encode（如中文 → %E6…），
// 故逐段 decodeURIComponent 還原成字面路徑，否則交給 rawUrl 的 encodeURIComponent 會雙重編碼而 404。
export function resolveAssetPath(src) {
  if (!src) return null;
  if (/^(https?:|data:|#|mailto:)/i.test(src) || src.startsWith("/")) return null;
  const decode = (s) => { try { return decodeURIComponent(s); } catch (e) { return s; } };
  const parts = (docDir() ? docDir().split("/") : []).concat(src.split("/"));
  const stack = [];
  for (const p of parts) {
    const seg = decode(p);
    if (seg === "" || seg === ".") continue;
    if (seg === "..") { stack.pop(); continue; }
    stack.push(seg);
  }
  return stack.join("/");
}

// 將「DOC_ROOT 相對路徑」換算成「相對於目前文件目錄」的路徑，確保連結可攜
export function relativeFromDocDir(targetRootRel) {
  const from = docDir() ? docDir().split("/") : [];
  const to = targetRootRel.split("/");
  let i = 0;
  while (i < from.length && i < to.length && from[i] === to[i]) i++;
  const ups = from.slice(i).map(() => "..");
  return ups.concat(to.slice(i)).join("/") || targetRootRel;
}

// 去除檔名前的時間戳記前綴，作為顯示與連結文字
export function assetDisplayName(name) {
  return name.replace(/^\d+_/, "");
}

export function formatSize(b) {
  if (b < 1024) return b + " B";
  if (b < 1048576) return (b / 1024).toFixed(1) + " KB";
  return (b / 1048576).toFixed(1) + " MB";
}

// debounce：在連續呼叫停止 ms 毫秒後才真正執行 fn，用來避免每次按鍵都全量重算。
export function debounce(fn, ms) {
  let timer = null;
  return function (...args) {
    if (timer) clearTimeout(timer);
    timer = setTimeout(() => { timer = null; fn.apply(this, args); }, ms);
  };
}
