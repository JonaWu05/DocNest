// 即時共編(M2/M3):以 Yjs CRDT 讓多人同時逐字編輯、自動合併,並顯示彼此的即時游標 / 選取(awareness)。
// 連到後端 dumb relay(/ws/collab,以文件路徑分房),把底層 CodeMirror 5 綁到 Y.Text,
// 本地變更轉成 Yjs update 上傳、收到的 update 套用回文件。落檔由被指派為 saver 的客戶端
// debounce 後走既有的存檔流程(onSaveRequest callback)完成,單一 saver 避免併發互踩版本鎖。
//
// 邊界(本期):僅對「可寫檔案」於編輯/分割模式啟用;唯讀檔仍走既有靜態讀檔路徑。
import { getToken } from "./auth.js";
import { state, API_BASE } from "./state.js";

// 二進位 frame 第 1 個位元組為 tag,其後為負載(與後端 internal/collab 對齊)。
const TAG_UPDATE = 0x75; // 'u' Yjs 文件 update
const TAG_AWARENESS = 0x61; // 'a' awareness(游標 / 選取)
const TAG_STATE = 0x73; // 's' 完整狀態快照(本端→伺服器,供 log 壓縮)
const TAG_CONTROL = 0x63; // 'c' 控制訊息(JSON)

const SAVE_DELAY = 1500; // saver 落檔的 debounce 間隔(毫秒),比照 autosave
const AWARENESS_HEARTBEAT = 10000; // awareness 心跳間隔(毫秒):多人時定期重送,維持存活並讓晚加入者看到游標
const RECONNECT_BASE = 1000; // 共編 WS 重連退避起始延遲(毫秒)
const RECONNECT_MAX = 30000; // 共編 WS 重連退避上限(毫秒)

// 游標配色:依使用者名稱雜湊到固定色盤,讓同一人每次都同色、彼此易辨識。
const USER_COLORS = ["#1f8a70", "#d1495b", "#3d7ea6", "#b8860b", "#7b5cd6", "#c2410c", "#0e7490", "#9d174d"];
function colorFor(name) {
  let h = 0;
  for (let i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) >>> 0;
  return USER_COLORS[h % USER_COLORS.length];
}

const textDecoder = new TextDecoder();

// 延遲載入打包好的 Yjs 相依(約 100KB);僅在首次啟用共編時注入,不影響首屏。
// 版本 query:vendor 走 immutable 長快取,重建 bundle 後必須遞增此值,否則瀏覽器會供應舊檔。
const BUNDLE_VERSION = 2;
let mod = null;
function loadYjs() {
  if (mod) return Promise.resolve(mod);
  return import(`/static/vendor/yjs/yjs-bundle.js?v=${BUNDLE_VERSION}`).then((m) => (mod = m));
}

// 目前的共編工作階段;null 代表未啟用。
let session = null;
let connectGen = 0; // 連線世代:每次 connect/disconnect 遞增,讓進行中的 async 連線辨識自己是否已被取代
let connectFailed = false; // 最近一次連線是否失敗(供 collabManaged 在失敗時退回單機編輯)
let presenceHandler = null; // 房內參與者變動時的 UI 回呼(由 connectCollab 的 opts 註冊;斷線時以空清單收尾)
let presenceEmitTimer = null; // emitPresence 合併計時器:游標頻繁變動時避免逐次重建 UI
let externalHandler = null; // 外部改檔時的 UI 回呼(active:bool):僅落檔者需出橫幅二選一

// collabActive 回報目前是否已建立共編連線。
export function collabActive() {
  return !!session;
}

// collabManaged 回報「目前檔案是否由共編接管」:已連線(任何模式),或可寫檔處於編輯/分割模式
// (含連線尚未完成的空窗期)。據此讓編輯/存檔/同步提示一律走共編路徑,不會在空窗期漏到舊版
// 自動儲存或樂觀鎖而與他人產生分歧。連線失敗時退回單機編輯(回 false)。
export function collabManaged() {
  if (collabActive()) return true;
  return !connectFailed && state.currentWritable && state.currentMode !== "preview";
}

// collabCurrentPath 回報目前共編的文件路徑(無則 null)。
export function collabCurrentPath() {
  return session ? session.path : null;
}

// collabIsSaver 回報本客戶端是否為目前房間的落檔者。
export function collabIsSaver() {
  return !!session && session.isSaver;
}

// frameBytes 在負載前加上 1 byte tag。
function frameBytes(tag, payload) {
  const out = new Uint8Array(1 + payload.length);
  out[0] = tag;
  out.set(payload, 1);
  return out;
}

// sendFrame 在連線開啟時送出一個 frame;未開啟則靜默忽略。
function sendFrame(tag, payload) {
  const ws = session && session.ws;
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(frameBytes(tag, payload));
}

const textEncoder = new TextEncoder();

// sendControl 送出客戶端→伺服器的控制訊息(JSON)。目前僅用於 hello(登記 Yjs clientID)。
function sendControl(obj) {
  sendFrame(TAG_CONTROL, textEncoder.encode(JSON.stringify(obj)));
}

// scheduleSaverSave 排程一次落檔(僅 saver),連續變更時 debounce 成一次。
// 外部改檔待決期間暫停,避免 saver 以共編內容靜默覆蓋磁碟上的外部變更(由橫幅讓使用者選擇後才恢復)。
function scheduleSaverSave() {
  if (session.externalChanged) return;
  session.pendingSave = true;
  clearTimeout(session.saveTimer);
  session.saveTimer = setTimeout(() => {
    if (session && session.isSaver && session.onSaveRequest) {
      session.onSaveRequest(session.text.toString());
      session.pendingSave = false;
    }
  }, SAVE_DELAY);
}

// sendFullState 送出目前文件的完整狀態(Y.encodeStateAsUpdate)。
//   tag=TAG_UPDATE：單人→多人時補餵新加入者(最佳化 B,會被廣播)。
//   tag=TAG_STATE ：回應伺服器壓縮請求(最佳化 A,僅用於取代 log,不廣播)。
function sendFullState(tag) {
  if (!session) return;
  sendFrame(tag, mod.Y.encodeStateAsUpdate(session.doc));
}

// ensureBinding 建立 CodeMirror↔Y.Text 綁定(僅一次)。於收到 init(seed 完成)後才綁,
// 讓 seeder 的編輯器直接顯示已 seed 的內容,避免「內容→空白→內容」的閃爍。
// 傳入 awareness 後,綁定會自動同步本端游標並渲染其他人的游標 / 選取。
function ensureBinding() {
  if (session.binding) return;
  const b = new mod.CodemirrorBinding(session.text, session.cm, session.awareness);
  session.binding = b;
  // y-codemirror 預設在編輯器失焦時清掉自己的游標(blur → cursor=null),
  // 會造成「對方視窗在背景 / 單機多視窗測試」時看不到游標。移除此行為,讓游標位置跨視窗保留
  // （離線/關閉時仍會在 disconnectCollab 主動送出移除）。屬性名於 pinned 版本穩定。
  if (b._blurListeer) {
    session.cm.off("blur", b._blurListeer);
    session.cm.off("swapDoc", b._blurListeer);
  }
}

// sendAwareness 送出指定 client 的 awareness 狀態(游標 / 名稱顏色);僅在串流中(有他人)才送。
function sendAwareness(clients) {
  if (!session.streaming || !clients.length) return;
  sendFrame(TAG_AWARENESS, mod.encodeAwarenessUpdate(session.awareness, clients));
}

// updateLocalUser 設定本端 awareness 的 user 欄位(名稱 / 顏色 / 是否落檔者 / 最近落檔時間)。
// y-codemirror 只讀 name/color 渲染游標標籤,額外欄位供房內參與者清單(collabStatus)顯示。
// 變更會觸發 awareness "update"(origin local),連帶廣播給他人並重新 emitPresence。
function updateLocalUser() {
  if (!session) return;
  const name = state.username || "匿名";
  session.awareness.setLocalStateField("user", {
    name,
    color: colorFor(name),
    saver: !!session.isSaver,
    savedAt: session.savedAt || 0,
  });
}

// participantsList 由 awareness 各端狀態組出房內參與者(名稱 / 顏色 / 落檔者 / 落檔時間 / 是否自己)。
function participantsList() {
  const states = session.awareness.getStates();
  const selfId = session.doc.clientID;
  const out = [];
  states.forEach((st, id) => {
    const u = st && st.user;
    if (!u || !u.name) return;
    out.push({ name: u.name, color: u.color, saver: !!u.saver, savedAt: u.savedAt || 0, self: id === selfId });
  });
  return out;
}

// emitPresence 把最新房內參與者清單交給已註冊的 UI 回呼;以短延遲合併連續變動(游標移動頻繁)。
function emitPresence() {
  if (!session || !presenceHandler || presenceEmitTimer) return;
  presenceEmitTimer = setTimeout(() => {
    presenceEmitTimer = null;
    if (session && presenceHandler) presenceHandler(participantsList());
  }, 120);
}

// collabNoteSaved 由 saver 落檔成功後呼叫:記錄落檔時間並經 awareness 廣播,
// 讓房內所有人看到「已儲存 …」,同時取代 saver 每次自動落檔的提示噪音。
export function collabNoteSaved() {
  if (!session || !session.isSaver) return;
  session.savedAt = Date.now();
  updateLocalUser();
}

// seedFromText 由本客戶端以 .md 內容初始化共享文件(僅伺服器指派的 seeder 執行)。
// 以非 "remote" 的 origin 進行,使其產生的 update 會被上傳給伺服器存入 log 並廣播給他人。
//
// 關鍵:只在「本地文件還是空的」時才插入。若文件已有內容(晚加入者已透過回放/同步取得內容,
// 或斷線重連後本地 Y.Doc 仍保有內容),再插入會與既有內容形成不同 lineage 而重複(文字接在底下)。
// 不可改用「我有沒有 seed 過」判斷:joiner 從沒呼叫過本函式,卻已持有內容。
function seedFromText(seedText) {
  session.seeded = true;
  if (session.text.length > 0 || !seedText) return;
  session.doc.transact(() => session.text.insert(0, seedText), "seed");
}

// handleControl 處理伺服器送來的控制訊息(init / role)。
function handleControl(msg) {
  if (msg.type === "init") {
    session.isSaver = !!msg.saver;
    session.streaming = !!msg.stream;
    if (msg.seed) seedFromText(session.seedText);
    ensureBinding();
    // 登記本端 Yjs clientID:伺服器於本連線關閉時據此廣播 peerLeft,讓他人即時移除我的殘留游標。
    // 每次(重)連都重送:重連在伺服器端為新連線,需重新登記。
    sendControl({ type: "hello", clientId: session.doc.clientID });
    updateLocalUser(); // 反映落檔者身分到 awareness,並觸發房內參與者清單更新
    emitPresence();
    if (msg.external) enterExternalState(); // 接手 / 重連即為 saver,且房間有未處理的外部改檔
    if (session.markReady) session.markReady(); // 綁定完成,通知 connectCollab 可解除唯讀
    // 重連:把離線期間的本地編輯推回伺服器(server 視為新加入,log 可能落後或為空),並重新宣告游標。
    if (session.connectedOnce && session.streaming) {
      sendFullState(TAG_UPDATE);
      sendAwareness([session.doc.clientID]);
    }
    session.connectedOnce = true;
  } else if (msg.type === "role") {
    // saver 移交:成為新 saver,必要時補做 seed
    session.isSaver = !!msg.saver;
    if (msg.seed) seedFromText(session.seedText);
    updateLocalUser(); // 落檔者身分變更,同步到 awareness 讓全房可見
    if (msg.external) enterExternalState(); // 接手成為 saver 且房間有未處理的外部改檔 → 出橫幅(下方 scheduleSaverSave 因待決而成 no-op)
    if (session.isSaver) scheduleSaverSave(); // 接手後盡快把目前狀態落檔
  } else if (msg.type === "stream") {
    // 串流切換(最佳化 B):多人時開始上傳;單人時停止(本機累積)。
    session.streaming = !!msg.stream;
    if (msg.stream && msg.sendState) sendFullState(TAG_UPDATE); // 補餵剛加入者
    if (msg.stream) sendAwareness([session.doc.clientID]); // 由獨自在房變為多人:宣告自己的游標
  } else if (msg.type === "compact") {
    sendFullState(TAG_STATE); // 以完整狀態壓縮伺服器端 log(最佳化 A)
  } else if (msg.type === "peerLeft") {
    // 某連線離線:伺服器以其 Yjs clientID 通知,立即移除該人殘留的 awareness(游標 / 選取),
    // 不必枯等 y-protocols 30s 逾時(否則快速 F5 會在房內堆積 ghost)。origin "remote" 避免回送。
    if (msg.clientId) mod.removeAwarenessStates(session.awareness, [msg.clientId], "remote");
  } else if (msg.type === "external") {
    enterExternalState();
  }
}

// enterExternalState 進入「外部改檔待決」:磁碟上的 .md 被外部改寫,僅落檔者需處理
// (其餘人的編輯本就走 CRDT,由 saver 決定如何與磁碟協調)。暫停自動落檔避免靜默覆蓋,出橫幅讓 saver 二選一。
// 由三處觸發:external 控制訊息(當下的 saver)、init / role 帶 external 旗標(接手 / 重連 / 加入成為 saver)。
function enterExternalState() {
  if (!session || !session.isSaver) return;
  session.externalChanged = true;
  clearTimeout(session.saveTimer); // 取消已排程的落檔
  if (externalHandler) externalHandler(true);
}

// collabResolveKeepMine 由橫幅「保留共編版本」呼叫:恢復落檔,以目前共編內容覆蓋磁碟上的外部變更。
export function collabResolveKeepMine() {
  if (!session || !session.isSaver) return;
  session.externalChanged = false;
  sendControl({ type: "extResolved" }); // 告知伺服器已處理,後續 saver 移交不再重複出橫幅
  scheduleSaverSave(); // 立即(debounce)落檔
}

// collabResolveUseDisk 由橫幅「改用磁碟版本」呼叫:以磁碟內容取代共享 Y.Text,經 CRDT 傳播給全房,
// 落檔者隨後存回。會覆蓋目前在途的共編編輯(已於橫幅警告);僅由落檔者執行,避免多端重複重置。
export function collabResolveUseDisk(diskText) {
  if (!session || !session.isSaver) return;
  session.externalChanged = false;
  sendControl({ type: "extResolved" }); // 告知伺服器已處理
  session.doc.transact(() => {
    session.text.delete(0, session.text.length);
    if (diskText) session.text.insert(0, diskText);
  }, "external-reset"); // 非 "remote" origin:會被上傳廣播,並觸發 saver 落檔
}

// handleMessage 解析並分派一個收到的 frame。
function handleMessage(data) {
  if (data.length < 1) return;
  const tag = data[0];
  const payload = data.subarray(1);
  if (tag === TAG_CONTROL) {
    let msg;
    try {
      msg = JSON.parse(textDecoder.decode(payload));
    } catch (e) {
      return;
    }
    handleControl(msg);
  } else if (tag === TAG_UPDATE) {
    // origin "remote":避免在 doc.update 處理器中又把它回送伺服器(造成迴圈)
    mod.Y.applyUpdate(session.doc, payload, "remote");
  } else if (tag === TAG_AWARENESS) {
    // origin "remote":避免 awareness 變更處理器把它再回送(造成迴圈)
    mod.applyAwarenessUpdate(session.awareness, payload, "remote");
  }
}

// connectCollab 對 path 建立共編房間並把 cm 綁到共享文件。
//   opts.canWrite      此檔是否可寫(唯讀者不會走到這裡,保留旗標供日後使用)
//   opts.seedText      若被指派為 seeder,用來初始化文件的 .md 內容
//   opts.onContentChange(text)  文件內容變動時呼叫(供更新真實來源與預覽/目錄)
//   opts.onSaveRequest(text)    身為 saver 需落檔時呼叫(走既有存檔流程)
export async function connectCollab(path, cm, opts) {
  disconnectCollab(); // 切檔前先收掉舊房間(會遞增 connectGen)
  const myGen = connectGen;
  connectFailed = false;
  let m;
  try {
    m = await loadYjs();
  } catch (e) {
    connectFailed = true; // 讓 collabManaged 退回單機編輯,避免可寫檔卡在唯讀
    throw e;
  }
  if (myGen !== connectGen) return; // 載入期間又切換/斷線 → 放棄,避免綁到錯的文件

  const doc = new m.Y.Doc();
  const text = doc.getText("content");
  const awareness = new m.Awareness(doc);
  session = {
    path,
    cm,
    doc,
    text,
    awareness,
    ws: null,
    binding: null,
    isSaver: false,
    seeded: false,
    streaming: false, // 最佳化 B：是否上傳本地 update（由伺服器 init / stream 控制訊息決定）
    savedAt: 0, // saver 最近一次落檔成功的時戳（經 awareness 廣播，供全房顯示「已儲存 …」）
    externalChanged: false, // 磁碟被外部改寫且尚未由 saver 決定如何處理：暫停自動落檔，避免靜默覆蓋
    saveTimer: null,
    pendingSave: false,
    heartbeatTimer: null,
    reconnectTimer: null,
    reconnectAttempts: 0,
    connectedOnce: false, // 是否已成功連過(用於分辨首次連線與重連)
    canWrite: !!opts.canWrite,
    seedText: opts.seedText || "",
    onContentChange: opts.onContentChange,
    onSaveRequest: opts.onSaveRequest,
    markReady: null,
  };
  const mySession = session; // 關閉後辨識自身,避免遲到的回呼污染新房間
  presenceHandler = opts.onPresenceChange || null; // 註冊房內參與者變動的 UI 回呼
  externalHandler = opts.onExternalChange || null; // 註冊外部改檔的 UI 回呼

  // ready:收到 init 並完成綁定後 resolve;呼叫端據此才解除編輯器唯讀(避免綁定前的編輯被覆蓋)。
  // 加逾時保險,避免 init 一直沒到(例如 WS 卡住)時編輯器永遠卡在唯讀。
  const ready = new Promise((resolve) => {
    session.markReady = resolve;
    setTimeout(resolve, 5000);
  });

  // 本端身分(名稱 / 顏色 / 落檔者 / 落檔時間):供其他人的編輯器渲染我的游標標籤,並組成房內參與者清單。
  updateLocalUser();

  // 文件變更:本地變更上傳、同步真實來源、必要時(saver)排程落檔。
  doc.on("update", (update, origin) => {
    if (session !== mySession) return;
    // 本地變更才上傳,且僅在串流中(最佳化 B:單人時本機累積不上傳)。落檔與預覽不受串流影響。
    if (origin !== "remote" && session.streaming) sendFrame(TAG_UPDATE, update);
    if (session.onContentChange) session.onContentChange(text.toString());
    if (session.isSaver) scheduleSaverSave();
  });

  // awareness 變更:本端游標移動即廣播;收到遠端新加入者時回敬一次,讓對方立即看到我的游標。
  awareness.on("update", ({ added, updated, removed }, origin) => {
    if (session !== mySession) return;
    if (origin === "local") {
      sendAwareness([...added, ...updated, ...removed]);
    } else if (added.length) {
      sendAwareness([doc.clientID]);
    }
    emitPresence(); // 房內成員 / 落檔者 / 落檔時間任一變動 → 更新狀態列
  });

  // 心跳:多人時定期重送本端 awareness,避免被對端的逾時機制清除,也補強晚加入者的初次同步。
  session.heartbeatTimer = setInterval(() => {
    if (session === mySession) sendAwareness([doc.clientID]);
  }, AWARENESS_HEARTBEAT);

  // 綁定延後到收到 init 後(見 ensureBinding),避免可寫檔進編輯時的內容閃爍。
  openSocket(mySession);

  await ready; // 等綁定完成再回傳,呼叫端才解除唯讀
}

// openSocket 建立(或重建)房間的 WebSocket。斷線時以指數退避自動重連;
// 重連後伺服器重新送 init,由 handleControl 把離線期間的本地編輯推回(見上)。
function openSocket(s) {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  const url = `${proto}://${location.host}/ws/collab?path=${encodeURIComponent(s.path)}&token=${encodeURIComponent(getToken())}`;
  const ws = new WebSocket(url);
  ws.binaryType = "arraybuffer";
  s.ws = ws;
  ws.addEventListener("open", () => {
    if (session === s) s.reconnectAttempts = 0; // 連上即重置退避
  });
  ws.addEventListener("message", (ev) => {
    if (session === s) handleMessage(new Uint8Array(ev.data));
  });
  ws.addEventListener("close", () => {
    if (session === s) scheduleReconnect(s); // 仍是目前房間才重連(已切換/關閉則作罷)
  });
}

// scheduleReconnect 以指數退避(1s、2s、4s…上限 30s)排程重連。
function scheduleReconnect(s) {
  if (s.reconnectTimer) return;
  const delay = Math.min(RECONNECT_MAX, RECONNECT_BASE * 2 ** s.reconnectAttempts);
  s.reconnectAttempts++;
  s.reconnectTimer = setTimeout(() => {
    s.reconnectTimer = null;
    if (session === s) openSocket(s);
  }, delay);
}

// flushBeacon 在分頁關閉 / 隱藏時,以 sendBeacon 把 saver 尚未落檔的內容送出(unload 期間 fetch 不可靠)。
// 帶 force 略過樂觀鎖(與 saver 正常落檔一致),token 走 query(beacon 無法設標頭)。
function flushBeacon() {
  if (!session || !session.isSaver || !session.pendingSave) return;
  if (session.externalChanged) return; // 外部改檔待決:不在關閉時靜默覆蓋磁碟
  const url = `${API_BASE}/api/file?path=${encodeURIComponent(session.path)}&token=${encodeURIComponent(getToken())}&force=1`;
  navigator.sendBeacon(url, new Blob([session.text.toString()], { type: "text/plain; charset=utf-8" }));
  session.pendingSave = false;
}
if (typeof window !== "undefined") window.addEventListener("pagehide", flushBeacon);

// disconnectCollab 收掉目前的共編房間(切檔、關檔、登出時呼叫)。
export function disconnectCollab() {
  connectGen++; // 使任何進行中的 connectCollab 失效(快速切換 TAB 時避免綁到舊文件)
  // 通知 UI 收掉共編狀態列(無論是否有 session:可能在連線空窗期切檔)。
  clearTimeout(presenceEmitTimer);
  presenceEmitTimer = null;
  if (presenceHandler) presenceHandler([]);
  presenceHandler = null;
  if (externalHandler) externalHandler(false); // 收掉外部改檔橫幅
  externalHandler = null;
  if (!session) return;
  const s = session;
  session = null; // 先清空,讓後續遲到的回呼/事件變成 no-op
  clearTimeout(s.saveTimer);
  clearTimeout(s.reconnectTimer);
  clearInterval(s.heartbeatTimer);
  // 收尾前把尚未落檔的內容存下(僅 saver 且確有待存),避免切檔/關檔遺失最後 1.5 秒的編輯。
  // 外部改檔待決時不收尾落檔,避免以共編內容靜默覆蓋磁碟上的外部變更。
  if (s.pendingSave && s.isSaver && !s.externalChanged && s.onSaveRequest) s.onSaveRequest(s.text.toString());
  // 通知他人移除我的游標(趁連線還開著;否則對端要等逾時才清掉殘留的游標)。
  if (s.awareness && s.streaming && s.ws && s.ws.readyState === WebSocket.OPEN) {
    try {
      s.awareness.setLocalState(null);
      s.ws.send(frameBytes(TAG_AWARENESS, mod.encodeAwarenessUpdate(s.awareness, [s.doc.clientID])));
    } catch (e) {
      /* 忽略 */
    }
  }
  if (s.awareness) {
    try {
      s.awareness.destroy();
    } catch (e) {
      /* 忽略 */
    }
  }
  if (s.binding) {
    try {
      s.binding.destroy();
    } catch (e) {
      /* 忽略 */
    }
  }
  if (s.ws) {
    try {
      s.ws.close();
    } catch (e) {
      /* 忽略 */
    }
  }
  if (s.doc) s.doc.destroy();
}
