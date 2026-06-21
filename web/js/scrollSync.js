// 分割模式：編輯區與預覽區依比例同步捲動（state.scrollSyncing 上鎖避免迴圈）。
import { state } from "./state.js";
import { previewPane } from "./dom.js";

export function syncFromEditor() {
  if (state.currentMode !== "split" || state.scrollSyncing || !state.easyMDE) return;
  const info = state.easyMDE.codemirror.getScrollInfo();
  const denom = (info.height - info.clientHeight) || 1;
  const ratio = info.top / denom;
  state.scrollSyncing = true;
  previewPane.scrollTop = ratio * (previewPane.scrollHeight - previewPane.clientHeight);
  requestAnimationFrame(() => { state.scrollSyncing = false; });
}

export function syncFromPreview() {
  if (state.currentMode !== "split" || state.scrollSyncing || !state.easyMDE) return;
  const denom = (previewPane.scrollHeight - previewPane.clientHeight) || 1;
  const ratio = previewPane.scrollTop / denom;
  const info = state.easyMDE.codemirror.getScrollInfo();
  state.scrollSyncing = true;
  state.easyMDE.codemirror.scrollTo(null, ratio * (info.height - info.clientHeight));
  requestAnimationFrame(() => { state.scrollSyncing = false; });
}
