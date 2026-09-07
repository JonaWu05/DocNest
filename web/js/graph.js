// 結構圖：整個文件庫的連結關係。取代編輯/預覽區顯示，力導向自動排版（不引入外部套件），
// 支援滾輪縮放、拖曳平移；點節點開啟該文件並自動關閉圖檢視。不支援拖曳單一節點釘住位置。
import { state, API_BASE } from "./state.js";
import {
  graphBtn, graphPane, graphCanvas, graphEmpty,
  editorPane, previewPane, contentEl,
  modeButtons, saveBtn, attachBtn, exportBtn,
} from "./dom.js";
import { authFetch, ensureOk } from "./auth.js";
import { showToast } from "./ui.js";
import { openFileByPath, applyMode } from "./editor.js";
import { revealFolder } from "./fileTree.js";

// 節點形狀：檔案畫圓點，資料夾（含根目錄）畫方形——形狀本身就是「容器 vs 內容」的視覺區分，
// 不只靠大小/顏色，掃一眼就能分清楚點到的是檔案還是資料夾（畫面像素，固定不隨縮放變化）。
const FILE_RADIUS = 6;  // 檔案節點圓半徑
const DIR_HALF = 7;     // 一般資料夾節點方形半邊長
const ROOT_HALF = 9;    // 虛擬根目錄節點方形半邊長，比一般資料夾再大一點、且實心填色，作為圖上唯一的錨點

let isOpen = false;
let loading = false;
let ctx = null;

let nodes = [];   // { path, title, isDir, x, y }（x/y 為世界座標，由力導向排版算出）
let edges = [];   // { sourceNode, targetNode, type }（type: "contain" 資料夾歸屬 / "link" 站內連結）

// lastPositions 記住每個節點路徑上次收斂後的座標，讓同一批節點下次開圖時從原位置微調，
// 而不是整個重新亂數起跑——否則物理模擬沒有「正上方」的絕對方向，同樣的連結關係每次收斂完
// 朝向、鏡射都可能不同，使用者會覺得圖每次長得不一樣。僅存在本頁瀏覽期間（重新整理頁面即重置）。
let lastPositions = {};
let camera = { scale: 1, offsetX: 0, offsetY: 0 };
let hoveredNode = null;

let dragging = false;
let dragMoved = false;
let dragStartScreen = { x: 0, y: 0 };
let dragStartOffset = { x: 0, y: 0 };

// toggleGraphView 為工具列「結構圖」按鈕的點擊處理：尚未開啟則開啟並抓取最新資料，已開啟則關閉。
export async function toggleGraphView() {
  if (isOpen) closeGraphView();
  else await openGraphView();
}

async function openGraphView() {
  if (loading) return;
  isOpen = true;
  graphBtn.classList.add("active");
  editorPane.classList.add("hidden");
  previewPane.classList.add("hidden");
  graphPane.classList.remove("hidden");
  // 圖檢視期間，「預覽/編輯/分割/儲存/附件/PDF」都是對「目前檔案」的操作，與圖無關，先停用。
  modeButtons.forEach(b => b.disabled = true);
  saveBtn.disabled = true;
  attachBtn.disabled = true;
  exportBtn.disabled = true;

  ensureCanvasSize();
  await fetchAndLayout(); // 每次開啟都重抓，避免顯示過期的連結關係
  fitToView();
  render();
}

function closeGraphView() {
  isOpen = false;
  graphBtn.classList.remove("active");
  graphPane.classList.add("hidden");
  if (state.currentPath) {
    applyMode(state.currentMode); // 還原離開圖之前的檔案顯示模式
  } else {
    editorPane.classList.add("hidden");
    previewPane.classList.remove("hidden");
    contentEl.className = "preview";
  }
  modeButtons.forEach(b => b.disabled = !state.currentPath);
  saveBtn.disabled = !state.currentPath || !state.currentWritable;
  attachBtn.disabled = !state.currentPath || !state.currentWritable;
  exportBtn.disabled = !state.currentPath;
}

// fetchAndLayout 向後端取得（依讀取權過濾後的）節點與邊，並算好排版座標。
async function fetchAndLayout() {
  loading = true;
  graphEmpty.classList.remove("hidden");
  graphEmpty.innerHTML = '<i class="fa fa-spinner fa-spin"></i><p>載入中…</p>';
  try {
    const res = await authFetch(API_BASE + "/api/graph");
    await ensureOk(res);
    const data = await res.json();
    applyGraphData(data.nodes || [], data.edges || []);
  } catch (err) {
    showToast("結構圖載入失敗：" + err.message, "error");
    nodes = [];
    edges = [];
  } finally {
    loading = false;
  }
  graphEmpty.classList.toggle("hidden", nodes.length > 0);
  if (nodes.length === 0) {
    graphEmpty.innerHTML = '<i class="fa fa-sitemap"></i><p>目前沒有可顯示的文件連結</p>';
  }
}

// applyGraphData 把後端回傳的節點/邊轉成內部結構（邊直接持有節點物件參照，繪製與排版都方便），
// 並執行力導向排版。title 若為空（理論上只有無 index.md 的資料夾會遇到），退回去掉副檔名的檔名，
// 與檔案樹 displayNameOf 的退回規則一致。既有節點沿用 lastPositions 記住的上次座標當起跑點，
// 只有真正新出現的節點才給亂數起始位置。
function applyGraphData(rawNodes, rawEdges) {
  const byPath = {};
  nodes = rawNodes.map((n) => {
    const fallback = n.path.split("/").pop().replace(/\.[^.]+$/, "");
    const cached = lastPositions[n.path];
    const node = {
      path: n.path, title: n.title || fallback, isDir: !!n.isDir,
      x: cached ? cached.x : null, y: cached ? cached.y : null, // null 代表尚無座標，排版時才給亂數起點
    };
    byPath[n.path] = node;
    return node;
  });
  edges = rawEdges
    .map((e) => ({ sourceNode: byPath[e.source], targetNode: byPath[e.target], type: e.type }))
    .filter((e) => e.sourceNode && e.targetNode);

  if (nodes.length > 0) {
    const rect = graphPane.getBoundingClientRect();
    forceDirectedLayout(nodes, edges, rect.width || 800, rect.height || 600);
  }

  lastPositions = {}; // 排版完成後回存，供下次開圖沿用（含新節點這次剛算出的位置）
  nodes.forEach((n) => { lastPositions[n.path] = { x: n.x, y: n.y }; });
}

// forceDirectedLayout 為簡化版 Fruchterman-Reingold 力導向排版：節點互相排斥、有連結的節點互相
// 吸引，隨迭代降溫收斂到穩定位置。純手刻、零外部套件，小規模文件庫（數十~數百節點）足夠快。
// 已有座標（來自 lastPositions）的節點從原位置開始微調；沒有座標的（新節點）給亂數起點。
function forceDirectedLayout(nodes, edges, width, height, iterations = 300) {
  const area = width * height;
  const k = Math.sqrt(area / Math.max(nodes.length, 1));
  nodes.forEach((n) => {
    if (n.x === null || n.y === null) {
      n.x = Math.random() * width;
      n.y = Math.random() * height;
    }
  });

  let temperature = width / 10; // 控制單次迭代最大位移，隨迭代降溫幫助收斂而非持續震盪
  for (let iter = 0; iter < iterations; iter++) {
    nodes.forEach((n) => { n.dx = 0; n.dy = 0; });

    // 排斥力：每對節點互推（O(n²)，小規模文件庫可接受）
    for (let i = 0; i < nodes.length; i++) {
      for (let j = i + 1; j < nodes.length; j++) {
        const a = nodes[i], b = nodes[j];
        let dx = a.x - b.x, dy = a.y - b.y;
        const dist = Math.sqrt(dx * dx + dy * dy) || 0.01;
        const force = (k * k) / dist;
        dx = (dx / dist) * force; dy = (dy / dist) * force;
        a.dx += dx; a.dy += dy;
        b.dx -= dx; b.dy -= dy;
      }
    }

    // 吸引力：有連結的節點互拉
    edges.forEach((e) => {
      const a = e.sourceNode, b = e.targetNode;
      let dx = a.x - b.x, dy = a.y - b.y;
      const dist = Math.sqrt(dx * dx + dy * dy) || 0.01;
      const force = (dist * dist) / k;
      dx = (dx / dist) * force; dy = (dy / dist) * force;
      a.dx -= dx; a.dy -= dy;
      b.dx += dx; b.dy += dy;
    });

    nodes.forEach((n) => {
      const disp = Math.sqrt(n.dx * n.dx + n.dy * n.dy) || 0.01;
      const capped = Math.min(disp, temperature);
      n.x += (n.dx / disp) * capped;
      n.y += (n.dy / disp) * capped;
    });
    temperature *= 0.97;
  }
}

// ===== 畫布繪製 =====

function ensureCanvasSize() {
  const rect = graphPane.getBoundingClientRect();
  const dpr = window.devicePixelRatio || 1;
  graphCanvas.width = rect.width * dpr;
  graphCanvas.height = rect.height * dpr;
  graphCanvas.style.width = rect.width + "px";
  graphCanvas.style.height = rect.height + "px";
  ctx = graphCanvas.getContext("2d");
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0); // 之後繪製一律用 CSS px 座標，高解析度縮放交給這裡處理
}

// fitToView 依節點分佈範圍算出縮放與位移，讓整張圖第一次開啟時剛好置中顯示。
function fitToView() {
  const rect = graphPane.getBoundingClientRect();
  if (nodes.length === 0) {
    camera = { scale: 1, offsetX: rect.width / 2, offsetY: rect.height / 2 };
    return;
  }
  const xs = nodes.map((n) => n.x), ys = nodes.map((n) => n.y);
  const minX = Math.min(...xs), maxX = Math.max(...xs);
  const minY = Math.min(...ys), maxY = Math.max(...ys);
  const pad = 60;
  const contentW = Math.max(maxX - minX, 1);
  const contentH = Math.max(maxY - minY, 1);
  const scale = Math.min(
    (rect.width - pad * 2) / contentW,
    (rect.height - pad * 2) / contentH,
    2 // 節點很少、範圍很小時避免放大到誇張比例
  );
  const cx = (minX + maxX) / 2, cy = (minY + maxY) / 2;
  camera = {
    scale,
    offsetX: rect.width / 2 - cx * scale,
    offsetY: rect.height / 2 - cy * scale,
  };
}

function worldToScreen(x, y) {
  return { x: x * camera.scale + camera.offsetX, y: y * camera.scale + camera.offsetY };
}

function cssVar(name) {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

// render 重繪整張圖：讀取當下主題色（深色/淺色模式即時反映，不需另外監聽主題切換事件）。
// 邊分兩種畫法：contain（資料夾歸屬）淡而細，link（使用者主動建立的站內連結）粗而醒目——
// 讓「這篇雖然放在別的資料夾，但內容上有關」這種跨資料夾的連結一眼就看得出來，這是圖比純
// 資料夾樹多出來的資訊，值得特別標示。
function render() {
  if (!ctx || !isOpen) return;
  const rect = graphPane.getBoundingClientRect();
  ctx.clearRect(0, 0, rect.width, rect.height);

  const containColor = cssVar("--border");
  const linkColor = cssVar("--accent");
  const fileFill = cssVar("--bg-elev");
  const dirFill = cssVar("--hover");
  const nodeStroke = cssVar("--accent");
  const textColor = cssVar("--text-muted");

  edges.forEach((e) => {
    const a = worldToScreen(e.sourceNode.x, e.sourceNode.y);
    const b = worldToScreen(e.targetNode.x, e.targetNode.y);
    ctx.strokeStyle = e.type === "link" ? linkColor : containColor;
    ctx.lineWidth = e.type === "link" ? 2 : 1;
    ctx.beginPath();
    ctx.moveTo(a.x, a.y);
    ctx.lineTo(b.x, b.y);
    ctx.stroke();
  });

  ctx.font = "12px sans-serif";
  ctx.textAlign = "center";
  ctx.textBaseline = "top";
  nodes.forEach((n) => {
    const p = worldToScreen(n.x, n.y);
    const isRoot = n.path === "";
    const half = isRoot ? ROOT_HALF : DIR_HALF;
    const hovered = n === hoveredNode;

    ctx.beginPath();
    if (n.isDir) {
      ctx.rect(p.x - half, p.y - half, half * 2, half * 2);
    } else {
      ctx.arc(p.x, p.y, FILE_RADIUS, 0, Math.PI * 2);
    }
    // 根目錄實心填色作為圖上唯一的錨點；一般資料夾與檔案維持空心底色，hover 時都填強調色。
    ctx.fillStyle = hovered ? nodeStroke : (isRoot ? nodeStroke : (n.isDir ? dirFill : fileFill));
    ctx.fill();
    ctx.strokeStyle = nodeStroke;
    ctx.lineWidth = n.isDir ? 2 : 1.5;
    ctx.stroke();

    ctx.fillStyle = textColor;
    ctx.font = n.isDir ? "bold 12px sans-serif" : "12px sans-serif"; // 資料夾標籤加粗，呼應其樞紐角色
    const labelY = n.isDir ? p.y + half + 4 : p.y + FILE_RADIUS + 4;
    ctx.fillText(n.title, p.x, labelY);
  });
}

function hitTestNode(screenX, screenY) {
  for (const n of nodes) {
    const p = worldToScreen(n.x, n.y);
    const dx = screenX - p.x, dy = screenY - p.y;
    if (n.isDir) {
      const half = (n.path === "" ? ROOT_HALF : DIR_HALF) + 3; // 稍微加大點擊誤差容忍範圍
      if (Math.abs(dx) <= half && Math.abs(dy) <= half) return n;
    } else {
      const r = FILE_RADIUS + 3;
      if (dx * dx + dy * dy <= r * r) return n;
    }
  }
  return null;
}

// handleNodeClick：檔案節點直接開檔；資料夾節點重用麵包屑的「跳回檔案樹」機制（展開＋捲動＋
// 高亮），跟點麵包屑路徑段體驗一致。虛擬根目錄節點沒有對應的檔案樹節點可高亮，單純關閉圖。
function handleNodeClick(node) {
  closeGraphView();
  if (!node.isDir) {
    openFileByPath(node.path);
  } else if (node.path !== "") {
    revealFolder(node.path);
  }
}

// ===== 互動：拖曳平移、滾輪縮放、點節點開檔 =====

// initGraph 綁定結構圖按鈕與畫布互動事件，由 main.js 於啟動時呼叫一次。
export function initGraph() {
  graphBtn.addEventListener("click", toggleGraphView);

  graphCanvas.addEventListener("mousedown", (e) => {
    dragging = true;
    dragMoved = false;
    dragStartScreen = { x: e.clientX, y: e.clientY };
    dragStartOffset = { x: camera.offsetX, y: camera.offsetY };
  });

  window.addEventListener("mousemove", (e) => {
    if (!isOpen) return;
    const rect = graphCanvas.getBoundingClientRect();
    const mx = e.clientX - rect.left, my = e.clientY - rect.top;
    if (dragging) {
      const dx = e.clientX - dragStartScreen.x, dy = e.clientY - dragStartScreen.y;
      if (Math.abs(dx) > 3 || Math.abs(dy) > 3) dragMoved = true; // 超過門檻才視為拖曳（否則點擊會誤判成拖曳）
      camera.offsetX = dragStartOffset.x + dx;
      camera.offsetY = dragStartOffset.y + dy;
      render();
      return;
    }
    const hit = hitTestNode(mx, my);
    if (hit !== hoveredNode) {
      hoveredNode = hit;
      graphCanvas.style.cursor = hit ? "pointer" : "grab";
      render();
    }
  });

  window.addEventListener("mouseup", (e) => {
    if (!dragging) return;
    dragging = false;
    if (dragMoved || !isOpen) return; // 有明顯拖曳位移就不當成點擊
    const rect = graphCanvas.getBoundingClientRect();
    const hit = hitTestNode(e.clientX - rect.left, e.clientY - rect.top);
    if (hit) handleNodeClick(hit);
  });

  graphCanvas.addEventListener(
    "wheel",
    (e) => {
      if (!isOpen) return;
      e.preventDefault();
      const rect = graphCanvas.getBoundingClientRect();
      const mx = e.clientX - rect.left, my = e.clientY - rect.top;
      const factor = e.deltaY < 0 ? 1.1 : 1 / 1.1;
      const newScale = Math.min(Math.max(camera.scale * factor, 0.1), 5);
      // 縮放以滑鼠位置為中心：算出滑鼠當下對應的世界座標，縮放後平移讓同一世界座標仍對齊滑鼠
      const worldX = (mx - camera.offsetX) / camera.scale;
      const worldY = (my - camera.offsetY) / camera.scale;
      camera.scale = newScale;
      camera.offsetX = mx - worldX * newScale;
      camera.offsetY = my - worldY * newScale;
      render();
    },
    { passive: false }
  );

  window.addEventListener("resize", () => {
    if (!isOpen) return;
    ensureCanvasSize();
    render();
  });
}
