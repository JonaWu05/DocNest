// 側欄寬度調整：拖曳側欄右緣的把手即可改變寬度，並記憶於 localStorage。
const MIN_WIDTH = 200;
const MAX_WIDTH = 560;
const STORAGE_KEY = "sidebarWidth";

function clamp(w) {
  return Math.max(MIN_WIDTH, Math.min(MAX_WIDTH, w));
}

export function initSidebarResize() {
  const sidebar = document.getElementById("sidebar");
  const resizer = document.getElementById("sidebar-resizer");
  if (!sidebar || !resizer) return;

  // 還原上次拖曳的寬度
  const saved = parseInt(localStorage.getItem(STORAGE_KEY), 10);
  if (saved) sidebar.style.width = clamp(saved) + "px";

  let startX = 0;
  let startWidth = 0;

  const onMove = (e) => {
    sidebar.style.width = clamp(startWidth + (e.clientX - startX)) + "px";
  };
  const onUp = () => {
    document.removeEventListener("mousemove", onMove);
    document.removeEventListener("mouseup", onUp);
    document.body.classList.remove("resizing-sidebar");
    localStorage.setItem(STORAGE_KEY, parseInt(sidebar.style.width, 10));
    // 讓 CodeMirror 等依視窗尺寸佈局的元件重新計算
    window.dispatchEvent(new Event("resize"));
  };

  resizer.addEventListener("mousedown", (e) => {
    e.preventDefault();
    startX = e.clientX;
    startWidth = sidebar.getBoundingClientRect().width;
    document.body.classList.add("resizing-sidebar");
    document.addEventListener("mousemove", onMove);
    document.addEventListener("mouseup", onUp);
  });
}
