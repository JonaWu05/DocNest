// 站內對話框：以 Promise 取代原生 confirm / prompt，統一樣式、配合深淺色主題，
// 且不像原生對話框那樣凍結整個分頁（背景的 autosave / 即時同步仍照常運作）。
//
// confirmModal(message)        → Promise<boolean>          按確定為 true、取消為 false
// promptModal(message, value)  → Promise<string|null>      按確定回傳輸入字串、取消回 null

let overlay = null;        // 共用的遮罩 + 對話框 DOM（首次使用時建立）
let cancelActive = null;   // 取消目前開著的對話框（用於「同時只開一個」）

function ensureOverlay() {
  if (overlay) return;
  overlay = document.createElement("div");
  overlay.className = "modal-overlay hidden";
  overlay.innerHTML =
    '<div class="dialog" role="dialog" aria-modal="true">' +
    '  <div class="dialog-msg"></div>' +
    '  <input type="text" class="dialog-input hidden" />' +
    '  <div class="dialog-actions">' +
    '    <button class="dialog-btn dialog-cancel" type="button"></button>' +
    '    <button class="dialog-btn primary dialog-ok" type="button"></button>' +
    '  </div>' +
    '</div>';
  document.body.appendChild(overlay);
}

// 共用實作：input=true 時帶輸入框（prompt），否則為確認框（confirm）
function showDialog({ message, input, defaultValue, okText, cancelText }) {
  ensureOverlay();
  // 同時只允許一個對話框：若已有，先以「取消」收掉舊的
  if (cancelActive) cancelActive();

  return new Promise((resolve) => {
    const msgEl = overlay.querySelector(".dialog-msg");
    const inputEl = overlay.querySelector(".dialog-input");
    const okBtn = overlay.querySelector(".dialog-ok");
    const cancelBtn = overlay.querySelector(".dialog-cancel");

    msgEl.textContent = message;          // textContent：訊息含使用者檔名，不可當 HTML（防 XSS）
    okBtn.textContent = okText || "確定";
    cancelBtn.textContent = cancelText || "取消";
    inputEl.classList.toggle("hidden", !input);
    if (input) inputEl.value = defaultValue || "";

    const cleanup = () => {
      overlay.classList.add("hidden");
      okBtn.removeEventListener("click", onOk);
      cancelBtn.removeEventListener("click", onCancel);
      overlay.removeEventListener("mousedown", onBackdrop);
      document.removeEventListener("keydown", onKey, true);
      cancelActive = null;
    };
    const settle = (val) => { cleanup(); resolve(val); };
    const onOk = () => settle(input ? inputEl.value : true);
    const onCancel = () => settle(input ? null : false);
    const onBackdrop = (e) => { if (e.target === overlay) onCancel(); };
    const onKey = (e) => {
      if (e.key === "Escape") { e.preventDefault(); onCancel(); }
      else if (e.key === "Enter" && (!input || document.activeElement === inputEl)) { e.preventDefault(); onOk(); }
    };

    cancelActive = onCancel; // 若被新的對話框取代，視為取消
    okBtn.addEventListener("click", onOk);
    cancelBtn.addEventListener("click", onCancel);
    overlay.addEventListener("mousedown", onBackdrop);
    document.addEventListener("keydown", onKey, true); // capture：搶在編輯器之前處理 Enter/Esc

    overlay.classList.remove("hidden");
    if (input) { inputEl.focus(); inputEl.select(); } else { okBtn.focus(); }
  });
}

export function confirmModal(message, opts = {}) {
  return showDialog({ message, input: false, okText: opts.okText, cancelText: opts.cancelText });
}

export function promptModal(message, defaultValue = "", opts = {}) {
  return showDialog({ message, input: true, defaultValue, okText: opts.okText, cancelText: opts.cancelText });
}
