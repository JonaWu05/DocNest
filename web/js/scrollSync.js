// 分割模式：編輯區與預覽區依「捲動比例」同步（兩側內容高度不同，用比例而非絕對位移才對得齊）。
// state.scrollSyncing 當作鎖：本端設定對方的捲動位置會再觸發對方的 scroll 事件，未上鎖會兩邊互相觸發成迴圈。
import { state } from "./state.js";
import { previewPane } from "./dom.js";

// syncFromEditor 由編輯區捲動時觸發：取編輯區目前的捲動比例，套用到預覽區。
// 僅於分割模式且未上鎖時作用；設定後以 requestAnimationFrame 等對方的 scroll 事件派發完再解鎖。
export function syncFromEditor() {
  if (state.currentMode !== "split" || state.scrollSyncing || !state.easyMDE) return;
  const info = state.easyMDE.codemirror.getScrollInfo();
  const denom = (info.height - info.clientHeight) || 1; // 可捲動範圍；內容未滿一頁時為 0，退而取 1 避免除以零
  const ratio = info.top / denom;
  state.scrollSyncing = true;
  previewPane.scrollTop = ratio * (previewPane.scrollHeight - previewPane.clientHeight);
  requestAnimationFrame(() => { state.scrollSyncing = false; });
}

// syncFromPreview 由預覽區捲動時觸發：與 syncFromEditor 反向，取預覽區的捲動比例套用到編輯區。
export function syncFromPreview() {
  if (state.currentMode !== "split" || state.scrollSyncing || !state.easyMDE) return;
  const denom = (previewPane.scrollHeight - previewPane.clientHeight) || 1; // 可捲動範圍；同上，防除以零
  const ratio = previewPane.scrollTop / denom;
  const info = state.easyMDE.codemirror.getScrollInfo();
  state.scrollSyncing = true;
  state.easyMDE.codemirror.scrollTo(null, ratio * (info.height - info.clientHeight));
  requestAnimationFrame(() => { state.scrollSyncing = false; });
}
