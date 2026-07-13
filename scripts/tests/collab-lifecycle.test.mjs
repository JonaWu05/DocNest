import assert from "node:assert/strict";
import test from "node:test";

import { collabModule, collabTestHooks, resetCollabTestState } from "./browser-env.mjs";

test.afterEach(resetCollabTestState);

test("收到 init 時 ready promise 會立即完成", async () => {
  const target = {};
  const ready = collabTestHooks.createReadyPromise(target, 50);

  assert.equal(typeof target.markReady, "function");
  target.markReady();
  await ready;
});

test("init timeout 會 reject，讓呼叫端安全退回單機模式", async () => {
  const target = {};

  await assert.rejects(
    collabTestHooks.createReadyPromise(target, 5),
    /timeout|逾時/i,
  );
  assert.equal(target.markReady, null);
});

test("切檔可立即取消等待中的 ready promise", async () => {
  const target = {};
  const ready = collabTestHooks.createReadyPromise(target, 1000);

  target.cancelReady();

  await assert.rejects(ready, /取消/);
  assert.equal(target.markReady, null);
  assert.equal(target.cancelReady, null);
});

test("同步 saver callback 會取得目前內容並清除 pending", async () => {
  let saved = "";
  const target = {
    isSaver: true,
    pendingSave: true,
    saveRevision: 1,
    text: { toString: () => "latest draft" },
    onSaveRequest: (text) => { saved = text; },
  };
  collabTestHooks.setSession(target);

  await collabTestHooks.runSaverSave();

  assert.equal(saved, "latest draft");
  assert.equal(target.pendingSave, false);
});

test("非同步存檔尚未成功前會保留 pending", async () => {
  let finishSave;
  const target = {
    isSaver: true,
    pendingSave: true,
    saveRevision: 1,
    text: { toString: () => "unsaved draft" },
    onSaveRequest: () => new Promise((resolve) => { finishSave = resolve; }),
  };
  collabTestHooks.setSession(target);

  const saving = collabTestHooks.runSaverSave();
  assert.equal(target.pendingSave, true);
  assert.equal(target.saveInFlight, true);

  finishSave(true);
  assert.equal(await saving, true);
  assert.equal(target.pendingSave, false);
  assert.equal(target.saveInFlight, false);
});

test("存檔途中有新 revision 時不會誤清 pending", async () => {
  let finishSave;
  const target = {
    isSaver: true,
    pendingSave: true,
    saveRevision: 1,
    text: { toString: () => "first revision" },
    onSaveRequest: () => new Promise((resolve) => { finishSave = resolve; }),
  };
  collabTestHooks.setSession(target);

  const saving = collabTestHooks.runSaverSave();
  target.saveRevision = 2;
  target.pendingSave = true;
  finishSave(true);

  assert.equal(await saving, true);
  assert.equal(target.pendingSave, true);
  assert.ok(target.saveTimer);
});

test("存檔失敗會保留 pending 並排程退避重試", async () => {
  const target = {
    isSaver: true,
    pendingSave: true,
    saveRevision: 1,
    text: { toString: () => "retry draft" },
    onSaveRequest: async () => false,
  };
  collabTestHooks.setSession(target);

  assert.equal(await collabTestHooks.runSaverSave(), false);
  assert.equal(target.pendingSave, true);
  assert.equal(target.saveRetryAttempts, 1);
  assert.ok(target.saveTimer);
});

test("非 saver 不會觸發落檔 callback", async () => {
  let calls = 0;
  const target = {
    isSaver: false,
    pendingSave: true,
    text: { toString: () => "draft" },
    onSaveRequest: () => { calls++; },
  };
  collabTestHooks.setSession(target);

  await collabTestHooks.runSaverSave();

  assert.equal(calls, 0);
  assert.equal(target.pendingSave, true);
});

test("pagehide 補存只處理 saver 的 pending 內容", () => {
  let beaconURL = "";
  let beaconBody = null;
  Object.defineProperty(globalThis.navigator, "sendBeacon", {
    value: (url, body) => {
      beaconURL = url;
      beaconBody = body;
      return true;
    },
    configurable: true,
  });
  localStorage.setItem("auth_token", "test-token");
  const target = {
    isSaver: true,
    pendingSave: true,
    externalChanged: false,
    path: "notes/a.md",
    text: { toString: () => "last draft" },
  };
  collabTestHooks.setSession(target);

  collabTestHooks.flushBeacon();

  assert.match(beaconURL, /\/api\/file\?path=notes%2Fa\.md/);
  assert.match(beaconURL, /token=test-token/);
  assert.match(beaconURL, /force=1/);
  assert.ok(beaconBody instanceof Blob);
  assert.equal(target.pendingSave, false);
});

test("sendBeacon 未接受請求時會保留 pending", () => {
  Object.defineProperty(globalThis.navigator, "sendBeacon", {
    value: () => false,
    configurable: true,
  });
  const target = {
    isSaver: true,
    pendingSave: true,
    externalChanged: false,
    path: "notes/a.md",
    text: { toString: () => "last draft" },
  };
  collabTestHooks.setSession(target);

  assert.equal(collabTestHooks.flushBeacon(), false);
  assert.equal(target.pendingSave, true);
});

test("切檔斷線時會要求 saver 收尾 pending 內容", () => {
  let saved = "";
  let destroyed = false;
  collabTestHooks.setSession({
    saveTimer: null,
    reconnectTimer: null,
    heartbeatTimer: null,
    pendingSave: true,
    isSaver: true,
    externalChanged: false,
    onSaveRequest: (text) => { saved = text; },
    text: { toString: () => "final before switching" },
    awareness: null,
    binding: null,
    ws: null,
    doc: { destroy: () => { destroyed = true; } },
  });

  collabModule.disconnectCollab();

  assert.equal(saved, "final before switching");
  assert.equal(destroyed, true);
});

test("切檔會等待在途存檔完成，再補送較新的 revision", async () => {
  let finishFirst;
  let content = "old content";
  const saved = [];
  const firstRequest = new Promise((resolve) => { finishFirst = resolve; });
  const target = {
    path: "notes/a.md",
    saveTimer: null,
    reconnectTimer: null,
    heartbeatTimer: null,
    pendingSave: true,
    saveRevision: 1,
    isSaver: true,
    externalChanged: false,
    onSaveRequest: (text) => {
      saved.push(text);
      return saved.length === 1 ? firstRequest : true;
    },
    text: { toString: () => content },
    awareness: null,
    binding: null,
    ws: null,
    doc: { destroy: () => {} },
  };
  collabTestHooks.setSession(target);

  const saving = collabTestHooks.runSaverSave();
  content = "new content";
  target.saveRevision = 2;
  target.pendingSave = true;
  collabModule.disconnectCollab();

  assert.deepEqual(saved, ["old content"]);
  finishFirst(true);
  await saving;
  await new Promise((resolve) => setImmediate(resolve));

  assert.deepEqual(saved, ["old content", "new content"]);
  assert.equal(target.pendingSave, false);
});

test("切檔收尾存檔失敗時會改用 beacon 補送原文件", async () => {
  let beaconURL = "";
  let beaconBody = null;
  Object.defineProperty(globalThis.navigator, "sendBeacon", {
    value: (url, body) => {
      beaconURL = url;
      beaconBody = body;
      return true;
    },
    configurable: true,
  });
  collabTestHooks.setSession({
    path: "notes/old.md",
    saveTimer: null,
    reconnectTimer: null,
    heartbeatTimer: null,
    pendingSave: true,
    isSaver: true,
    externalChanged: false,
    onSaveRequest: async () => false,
    text: { toString: () => "final old content" },
    awareness: null,
    binding: null,
    ws: null,
    doc: { destroy: () => {} },
  });

  collabModule.disconnectCollab();
  await new Promise((resolve) => setImmediate(resolve));

  assert.match(beaconURL, /path=notes%2Fold\.md/);
  assert.ok(beaconBody instanceof Blob);
  assert.equal(await beaconBody.text(), "final old content");
});
