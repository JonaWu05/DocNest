// 用 jsdom + 真正的 CodeMirror 載入「實際打包的 bundle」,模擬遠端游標,
// 檢查 y-codemirror 綁定是否真的把遠端游標渲染成 .remote-caret DOM 元素。
// 目的:把「資料有傳到 / 畫面沒渲染」這件事在無瀏覽器下定論。
import { JSDOM } from "jsdom";
import fs from "fs";
import path from "path";
import url from "url";

const dom = new JSDOM('<!DOCTYPE html><body><div id="ed"></div></body>', { pretendToBeVisual: true });
globalThis.window = dom.window;
globalThis.document = dom.window.document;
globalThis.getComputedStyle = dom.window.getComputedStyle;
globalThis.DOMParser = dom.window.DOMParser;
dom.window.document.hasFocus = () => true;

// jsdom 沒有排版引擎,CodeMirror 量測時會呼叫 getBoundingClientRect / getClientRects;補上 stub。
const rect = () => ({ left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0, x: 0, y: 0 });
const emptyRects = () => Object.assign([], { item: () => null });
dom.window.Range.prototype.getBoundingClientRect = rect;
dom.window.Range.prototype.getClientRects = emptyRects;
dom.window.Element.prototype.getBoundingClientRect = rect;
dom.window.Element.prototype.getClientRects = emptyRects;

// CodeMirror 需在 import 期就有 document;且 cm-shim 會用 window.CodeMirror。
const CodeMirror = (await import("codemirror")).default;
globalThis.CodeMirror = CodeMirror;
dom.window.CodeMirror = CodeMirror;

// bundle 為 ESM 但副檔名 .js,在 Node(無 type:module)會被當 CJS;複製成 .mjs 再載入。
const tmp = path.resolve("scripts/.yjs-bundle.tmp.mjs");
fs.copyFileSync("web/vendor/yjs/yjs-bundle.js", tmp);
const B = await import(url.pathToFileURL(tmp).href);
fs.unlinkSync(tmp);
const { Y, CodemirrorBinding, Awareness, encodeAwarenessUpdate, applyAwarenessUpdate } = B;

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// ── peer B:真正的編輯器 + 綁定(含 awareness)──
const ed = document.getElementById("ed");
const cmB = CodeMirror(ed, { value: "" });
const docB = new Y.Doc();
const textB = docB.getText("content");
const awB = new Awareness(docB);
new CodemirrorBinding(textB, cmB, awB);

// ── peer A:只有文件 + awareness(模擬另一個人)──
const docA = new Y.Doc();
const textA = docA.getText("content");
const awA = new Awareness(docA);
awA.setLocalStateField("user", { name: "Alice", color: "#d1495b" });
textA.insert(0, "hello world");

// 同步 A 的文件內容到 B
Y.applyUpdate(docB, Y.encodeStateAsUpdate(docA), "remote");
await sleep(20);
console.log("B 編輯器內容 =", JSON.stringify(cmB.getValue()));

// A 在 index 5 放游標(相對位置),並把 awareness 傳給 B
const rel = Y.createRelativePositionFromTypeIndex(textA, 5);
awA.setLocalStateField("cursor", { anchor: JSON.stringify(rel), head: JSON.stringify(rel) });
applyAwarenessUpdate(awB, encodeAwarenessUpdate(awA, [docA.clientID]), "remote");

await sleep(80); // 等綁定的游標渲染 debounce(約 10ms)

const carets = ed.querySelectorAll(".remote-caret");
console.log("B 端 awareness 狀態數(含自己)=", awB.getStates().size);
console.log("B 端 .remote-caret 元素數 =", carets.length);
if (carets.length > 0) {
  console.log("caret outerHTML =", carets[0].outerHTML);
  console.log("\n✅ 綁定有把遠端游標渲染成 DOM → 問題在 CSS/焦點,不是渲染邏輯");
} else {
  console.log("\n❌ 綁定沒有產生 .remote-caret → 渲染路徑本身有問題");
}
process.exit(0);
