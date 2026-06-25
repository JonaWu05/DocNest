// 即時共編真實量測:用真正的 yjs + WebSocket 連到執行中的 Go server(/ws/collab),
// 模擬多人打字,在線路上量測位元組數,驗證最佳化 A(log 壓縮)與 B(單人延後串流)。
// 為 headless 測試(不接 CodeMirror),直接以 Y.Text 模擬輸入。
//
// 用法:先啟動 server(PORT=18080),再 `node scripts/collab-bench.mjs 18080`
import * as Y from "yjs";
import { Awareness, encodeAwarenessUpdate, applyAwarenessUpdate } from "y-protocols/awareness";
import crypto from "crypto";
import fs from "fs";

const PORT = process.argv[2] || "18080";
const HOST = `ws://127.0.0.1:${PORT}`;

// ── 從 .env 取 JWT_SECRET(不印出),自簽一個對根目錄有寫權的身分(local:jonawuAdmin)──
function readEnv(key) {
  const txt = fs.readFileSync(".env", "utf8");
  for (const line of txt.split(/\r?\n/)) {
    const m = line.match(/^\s*([A-Z0-9_]+)\s*=\s*(.*)\s*$/);
    if (m && m[1] === key) {
      let v = m[2].trim();
      if ((v.startsWith('"') && v.endsWith('"')) || (v.startsWith("'") && v.endsWith("'"))) {
        v = v.slice(1, -1);
      }
      return v;
    }
  }
  throw new Error(`.env 缺少 ${key}`);
}
const SECRET = readEnv("JWT_SECRET");

function b64url(buf) {
  return Buffer.from(buf).toString("base64url");
}
function signJWT(sub, username) {
  const now = Math.floor(Date.now() / 1000);
  const header = b64url(JSON.stringify({ alg: "HS256", typ: "JWT" }));
  const payload = b64url(JSON.stringify({ username, login_type: "local", sub, exp: now + 3600, iat: now }));
  const data = `${header}.${payload}`;
  const sig = crypto.createHmac("sha256", SECRET).update(data).digest("base64url");
  return `${data}.${sig}`;
}
const TOKEN = signJWT("local:jonawuAdmin", "jonawuAdmin");

const TAG_UPDATE = 0x75, TAG_STATE = 0x73, TAG_CONTROL = 0x63, TAG_AWARENESS = 0x61;
const td = new TextDecoder();

function frame(tag, payload) {
  const out = new Uint8Array(1 + payload.length);
  out[0] = tag;
  out.set(payload, 1);
  return out;
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// headless 共編客戶端,鏡像 web/js/collab.js 的協定並累計量測數據。
class Client {
  constructor(path, name, opts = {}) {
    this.path = path;
    this.name = name;
    this.doc = new Y.Doc();
    this.text = this.doc.getText("content");
    this.isSaver = false;
    this.streaming = false;
    this.connectedOnce = false; // 分辨首次連線與重連(重連時補送完整狀態)
    this.seedText = "";
    // awareness（M3）僅在需要的情境啟用，避免干擾情境 1–4 的位元組量測。
    this.aw = opts.awareness ? new Awareness(this.doc) : null;
    if (this.aw) {
      this.aw.setLocalStateField("user", { name });
      this.aw.on("update", ({ added, updated, removed }, origin) => {
        if (origin === "local") this._sendAwareness([...added, ...updated, ...removed]);
        else if (added.length) this._sendAwareness([this.doc.clientID]);
      });
    }
    // 量測
    this.bytesSent = 0;
    this.bytesRecv = 0;
    this.updatesSent = 0;
    this.replayFrames = 0; // 加入後、開始打字前收到的 update frame 數(回放量)
    this.replayBytes = 0;
    this.typing = false;
    this.updateSizes = []; // 本端每次本地 update 的位元組大小(不論是否上傳)

    this.doc.on("update", (update, origin) => {
      if (origin !== "remote") {
        this.updateSizes.push(update.length);
        if (this.streaming) this._send(TAG_UPDATE, update);
      }
    });
  }

  _send(tag, payload) {
    if (this.ws && this.ws.readyState === 1) {
      const f = frame(tag, payload);
      this.ws.send(f);
      this.bytesSent += f.length;
      if (tag === TAG_UPDATE) this.updatesSent++;
    }
  }

  _sendAwareness(clients) {
    if (!this.aw || !this.streaming || !clients.length) return;
    this._send(TAG_AWARENESS, encodeAwarenessUpdate(this.aw, clients));
  }

  // 取得某 peer 的 awareness user.name（用於驗證傳遞）。
  peerName(clientID) {
    const st = this.aw && this.aw.getStates().get(clientID);
    return st && st.user ? st.user.name : null;
  }

  connect() {
    return new Promise((resolve, reject) => {
      const url = `${HOST}/ws/collab?path=${encodeURIComponent(this.path)}&token=${encodeURIComponent(TOKEN)}`;
      const ws = new WebSocket(url);
      ws.binaryType = "arraybuffer";
      this.ws = ws;
      this._inited = resolve;
      ws.addEventListener("message", (ev) => this._onMessage(new Uint8Array(ev.data)));
      ws.addEventListener("error", (e) => reject(new Error(`${this.name} WS error: ${e.message || e}`)));
      ws.addEventListener("close", (e) => {
        if (!this._inited_done) reject(new Error(`${this.name} 連線被關閉(code ${e.code}) — 可能是權限或 token 問題`));
      });
    });
  }

  _onMessage(data) {
    this.bytesRecv += data.length;
    const tag = data[0];
    const payload = data.subarray(1);
    if (tag === TAG_CONTROL) {
      const msg = JSON.parse(td.decode(payload));
      this._onControl(msg);
    } else if (tag === TAG_UPDATE) {
      if (!this.typing) {
        this.replayFrames++;
        this.replayBytes += data.length;
      }
      Y.applyUpdate(this.doc, payload, "remote");
    } else if (tag === TAG_AWARENESS && this.aw) {
      applyAwarenessUpdate(this.aw, payload, "remote");
    }
  }

  _onControl(msg) {
    if (msg.type === "init") {
      this.isSaver = !!msg.saver;
      this.streaming = !!msg.stream;
      this.lastInitSeed = !!msg.seed; // 記錄這次 init 是否被要求 seed(供測試驗證復現條件)
      // 只在本地還是空的時候才 seed(鏡像 collab.js 修正:避免 joiner/重連時重複插入內容)
      if (msg.seed && this.seedText && this.text.length === 0) this.doc.transact(() => this.text.insert(0, this.seedText), "seed");
      this._inited_done = true;
      this._inited();
      // 重連:把離線期間的本地編輯推回伺服器(鏡像 collab.js 的重連補齊)
      if (this.connectedOnce && this.streaming) this._send(TAG_UPDATE, Y.encodeStateAsUpdate(this.doc));
      this.connectedOnce = true;
    } else if (msg.type === "stream") {
      this.streaming = !!msg.stream;
      if (msg.stream && msg.sendState) this._send(TAG_UPDATE, Y.encodeStateAsUpdate(this.doc));
      if (msg.stream) this._sendAwareness([this.doc.clientID]);
    } else if (msg.type === "compact") {
      this.compactRequests = (this.compactRequests || 0) + 1;
      this._send(TAG_STATE, Y.encodeStateAsUpdate(this.doc));
    } else if (msg.type === "role") {
      this.isSaver = !!msg.saver;
    }
  }

  // 模擬連續打字:每字一次 insert(對應一個 Yjs update);ch 可指定字元以辨識來源。
  type(n, delayMs = 0, ch = "x") {
    this.typing = true;
    return (async () => {
      for (let i = 0; i < n; i++) {
        this.text.insert(this.text.length, ch);
        if (delayMs) await sleep(delayMs);
      }
    })();
  }

  // 模擬斷線重連:關掉現有 ws,重開連線(沿用同一 Y.Doc),鏡像 collab.js 行為。
  async reconnect() {
    if (this.ws) this.ws.close();
    await sleep(60);
    await this.connect();
  }

  close() {
    if (this.aw) this.aw.setLocalState(null); // 觸發送出移除(若 streaming),讓對端即時清掉我的游標
    if (this.ws) this.ws.close();
  }
}

function avg(arr) {
  return arr.length ? arr.reduce((a, b) => a + b, 0) / arr.length : 0;
}
function ok(cond) {
  return cond ? "✅" : "❌ 不符預期";
}

async function main() {
  console.log(`連線目標 ${HOST}/ws/collab  身分 local:jonawuAdmin\n`);

  // ── 量測1:每次打字產生的 Yjs update 大小 ──────────────────────────
  {
    const a = new Client("bench/sizes.md", "A");
    await a.connect();
    a.typing = true;
    await a.type(200);
    await sleep(100);
    const s = a.updateSizes;
    console.log("【1】每次打字的 Yjs update 大小(實測)");
    console.log(`    樣本=${s.length}  min=${Math.min(...s)}B  avg=${avg(s).toFixed(1)}B  max=${Math.max(...s)}B`);
    console.log(`    → 與規劃假設「約 20–50 bytes/則」對照\n`);
    a.close();
    await sleep(50);
  }

  // ── 量測2:B. 單人延後串流(獨自在房時不上傳)──────────────────────
  {
    const a = new Client("bench/solo.md", "A");
    await a.connect();
    await a.type(100); // 獨自在房打 100 字
    await sleep(100);
    const soloSent = a.bytesSent;

    const b = new Client("bench/solo.md", "B"); // 第二人加入
    await b.connect();
    await sleep(200); // 等 A 收到 stream 通知並補送完整狀態
    const converged = a.text.toString() === b.text.toString() && b.text.length === 100;

    console.log("【2】B. 單人延後串流");
    console.log(`    獨自打 100 字期間上傳位元組 = ${soloSent}B  ${ok(soloSent === 0)}（單人不上傳）`);
    console.log(`    第二人加入後 A 補送完整狀態 = ${a.bytesSent}B（一次,而非 100 則）`);
    console.log(`    第二人收到並收斂到 100 字 = ${b.text.length} 字  ${ok(converged)}\n`);
    a.close(); b.close();
    await sleep(50);
  }

  // ── 量測3:A. log 壓縮 + 晚加入回放量有界 ──────────────────────────
  {
    const a = new Client("bench/compact.md", "A");
    await a.connect();
    const b = new Client("bench/compact.md", "B");
    await b.connect();
    await sleep(150); // A 變為串流並補送(空)完整狀態

    await a.type(300, 1); // 串流中打 300 字 → 跨過壓縮門檻(256)
    await sleep(400);     // 等壓縮往返完成

    const c = new Client("bench/compact.md", "C"); // 晚加入者
    await c.connect();
    await sleep(300);
    const converged = c.text.toString() === a.text.toString() && c.text.length === 300;

    console.log("【3】A. log 壓縮（晚加入回放量有界）");
    console.log(`    A 打 300 則(>門檻 256) → saver 收到壓縮請求 = ${a.compactRequests || 0} 次  ${ok((a.compactRequests || 0) >= 1)}`);
    console.log(`    晚加入者 C 的回放 = ${c.replayFrames} 則 / ${c.replayBytes}B`);
    console.log(`    → 若無壓縮應約 300 則;壓縮後大幅變少  ${ok(c.replayFrames < 300)}`);
    console.log(`    C 收斂到 300 字 = ${c.text.length} 字  ${ok(converged)}\n`);
    a.close(); b.close(); c.close();
    await sleep(50);
  }

  // ── 量測4:三人同編的線路流量 ──────────────────────────────────────
  {
    const path = "bench/three.md";
    const a = new Client(path, "A"), b = new Client(path, "B"), c = new Client(path, "C");
    await a.connect(); await b.connect(); await c.connect();
    await sleep(150);
    a.typing = b.typing = c.typing = true;
    const t0 = Date.now();
    await Promise.all([a.type(100, 2), b.type(100, 2), c.type(100, 2)]);
    await sleep(400);
    const dt = Date.now() - t0;
    const totalSent = a.bytesSent + b.bytesSent + c.bytesSent;
    const totalRecv = a.bytesRecv + b.bytesRecv + c.bytesRecv;
    const allEq = a.text.toString() === b.text.toString() && b.text.toString() === c.text.toString();
    console.log("【4】三人同編(各打 100 字,共 300 次編輯)");
    console.log(`    上傳合計 = ${totalSent}B  伺服器轉發合計 = ${totalRecv}B  歷時 ${dt}ms`);
    console.log(`    平均每次編輯上行 ≈ ${(totalSent / 300).toFixed(1)}B`);
    console.log(`    三端內容一致 = ${a.text.length}/${b.text.length}/${c.text.length} 字  ${ok(allEq)}\n`);
    a.close(); b.close(); c.close();
    await sleep(50);
  }

  // ── 量測5:M3 awareness 即時游標傳遞與清除 ───────────────────────
  {
    const path = "bench/awareness.md";
    const a = new Client(path, "Alice", { awareness: true });
    await a.connect();
    const b = new Client(path, "Bob", { awareness: true });
    await b.connect();
    await sleep(300); // 等 stream 通知 + greet-back 往返

    const aSeesB = a.peerName(b.doc.clientID);
    const bSeesA = b.peerName(a.doc.clientID);
    console.log("【5】M3 awareness 游標傳遞");
    console.log(`    A 看到 B 的游標標籤 = ${JSON.stringify(aSeesB)}  ${ok(aSeesB === "Bob")}`);
    console.log(`    B 看到 A 的游標標籤 = ${JSON.stringify(bSeesA)}  ${ok(bSeesA === "Alice")}`);

    b.close(); // B 離開 → A 應即時移除 B 的游標
    await sleep(200);
    const removed = a.peerName(b.doc.clientID) == null;
    console.log(`    B 離開後 A 端移除 B 的游標 = ${removed}  ${ok(removed)}\n`);
    a.close();
    await sleep(50);
  }

  // ── 量測6:M4 斷線重連 + 雙向離線編輯補齊 ──────────────────────
  {
    const path = "bench/reconnect.md";
    const a = new Client(path, "A");
    await a.connect();
    const b = new Client(path, "B");
    await b.connect();
    await sleep(150);
    a.typing = b.typing = true;

    await a.type(5, 0, "x"); // A 打 5 個 x，B 應收到
    await sleep(150);

    b.ws.close(); // B 斷線
    await sleep(100);
    await a.type(5, 0, "x"); // 斷線期間：A 再打 5 個 x（B 收不到）
    await b.type(3, 0, "y"); // 斷線期間：B 離線打 3 個 y（A 收不到）
    await sleep(100);

    await b.reconnect(); // B 重連 → 應把離線的 y 推回、並補到 A 的 x
    await sleep(400);

    const sa = a.text.toString(), sb = b.text.toString();
    const converged = sa === sb;
    const full = sa.length === 13 && sa.includes("y") && (sa.match(/x/g) || []).length === 10;
    console.log("【6】M4 斷線重連 + 雙向離線編輯補齊");
    console.log(`    重連後 A/B 內容一致 = ${a.text.length}/${b.text.length} 字  ${ok(converged)}`);
    console.log(`    含雙方離線編輯(10 x + 3 y) = ${ok(full)}`);
    console.log(`    內容 = ${JSON.stringify(sa)}\n`);
    a.close(); b.close();
    await sleep(50);
  }

  // ── 量測7:重連不重複 seed(復現並驗證「文件被複製」的修正)──────────
  {
    const path = "bench/reseed.md";
    const c = new Client(path, "C");
    c.seedText = "hello world\n第二行內容"; // 模擬 .md 內容
    await c.connect(); // 首位寫入者 → server 要求 seed → 插入內容
    await sleep(150);
    const afterSeed = c.text.toString();

    // C 為單人,斷線後房間在 server 端回收;重連時又成為新房間首位寫入者 → server 再次要求 seed。
    // 修正前:會把 .md 內容再插一次 → 變兩份;修正後:本地已有內容 → 跳過,不重複。
    await c.reconnect();
    await sleep(300);
    const afterReconnect = c.text.toString();

    const noDup = afterReconnect === afterSeed;
    console.log("【7】M4 重連不重複 seed(修正「文件被複製」)");
    console.log(`    重連時 server 要求 seed = ${c.lastInitSeed}（true 才代表有復現到 bug 條件）`);
    console.log(`    seed 後 ${afterSeed.length} 字、重連後 ${afterReconnect.length} 字  ${ok(noDup)}`);
    console.log(`    內容 = ${JSON.stringify(afterReconnect)}\n`);
    c.close();
    await sleep(50);
  }

  console.log("量測完成。");
  process.exit(0);
}

main().catch((e) => {
  console.error("量測失敗:", e);
  process.exit(1);
});
