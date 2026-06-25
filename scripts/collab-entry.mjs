// 即時共編前端相依的打包進入點:把 Yjs、y-codemirror(CM5 綁定)、awareness 匯整為單一 ESM bundle,
// 供瀏覽器以 import() 延遲載入(見 web/js/collab.js)。輸出至 web/vendor/yjs/yjs-bundle.js。
//
// 重建指令(node/npm 為開發相依,不進版控):
//   npm install --save-dev yjs y-codemirror y-protocols esbuild
//   npx esbuild scripts/collab-entry.mjs --bundle --format=esm --minify \
//     --alias:codemirror=./scripts/cm-shim.mjs --outfile=web/vendor/yjs/yjs-bundle.js
import * as Y from "yjs";
import { CodemirrorBinding } from "y-codemirror";
import {
  Awareness,
  encodeAwarenessUpdate,
  applyAwarenessUpdate,
  removeAwarenessStates,
} from "y-protocols/awareness";

export {
  Y,
  CodemirrorBinding,
  Awareness,
  encodeAwarenessUpdate,
  applyAwarenessUpdate,
  removeAwarenessStates,
};
