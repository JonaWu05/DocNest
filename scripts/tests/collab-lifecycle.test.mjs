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

test("同步 saver callback 會取得目前內容並清除 pending", () => {
  let saved = "";
  const target = {
    isSaver: true,
    pendingSave: true,
    text: { toString: () => "latest draft" },
    onSaveRequest: (text) => { saved = text; },
  };
  collabTestHooks.setSession(target);

  collabTestHooks.runSaverSave();

  assert.equal(saved, "latest draft");
  assert.equal(target.pendingSave, false);
});

test("非 saver 不會觸發落檔 callback", () => {
  let calls = 0;
  const target = {
    isSaver: false,
    pendingSave: true,
    text: { toString: () => "draft" },
    onSaveRequest: () => { calls++; },
  };
  collabTestHooks.setSession(target);

  collabTestHooks.runSaverSave();

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
