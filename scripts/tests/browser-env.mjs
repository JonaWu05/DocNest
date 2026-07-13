import { JSDOM } from "jsdom";

// collab.js 會經 auth.js 在 import 期讀取 meta / localStorage，先建立最小瀏覽器環境。
const dom = new JSDOM(
  '<!doctype html><html><head><meta name="docnest-auth-mode" content="standalone"></head><body></body></html>',
  { url: "http://localhost/", pretendToBeVisual: true },
);

globalThis.window = dom.window;
globalThis.document = dom.window.document;
globalThis.location = dom.window.location;
globalThis.localStorage = dom.window.localStorage;
globalThis.Blob = dom.window.Blob;
globalThis.WebSocket = { OPEN: 1 };
Object.defineProperty(globalThis, "navigator", {
  value: dom.window.navigator,
  configurable: true,
});

export const collabModule = await import("../../web/js/collab.js");
export const { collabTestHooks } = collabModule;

export function resetCollabTestState() {
  collabTestHooks.clearSession();
  localStorage.clear();
}
