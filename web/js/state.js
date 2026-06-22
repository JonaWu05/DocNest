// 全域共享狀態：各功能模組透過此單一物件讀寫同一份狀態，
// 避免散落的全域變數（ES 模組之間無法共享裸變數，故集中於此）。
export const API_BASE = "";
export const AUTOSAVE_DELAY = 1500;

export const state = {
  username: "",           // 目前登入者名稱（由 session 設定，用於 presence 自我辨識）
  currentPath: null,      // 目前開啟的檔案相對路徑
  currentMode: "preview", // "preview" / "edit" / "split"
  easyMDE: null,
  currentContent: "",     // 內容的單一真實來源
  currentVersion: null,   // 目前內容對應的伺服器版本（樂觀鎖基準，由 X-File-Version 標頭取得）
  isDirty: false,
  autosaveTimer: null,
  suppressChange: false,  // 程式化設定編輯器內容時暫時忽略 change 事件
  scrollSyncing: false,   // 程式化捲動時上鎖，避免兩邊互相觸發形成迴圈
  // 權限分組相關（由 /api/me 與檔案樹節點提供；伺服器端仍為真正防線，前端僅控制呈現）
  hasAccess: true,        // 是否有任何可讀內容；否則登入後顯示歡迎頁
  canWriteRoot: true,     // 是否可在根目錄新增檔案/資料夾
  currentWritable: true,  // 目前開啟的檔案是否可寫（決定能否編輯/儲存）
};
