import assert from "node:assert/strict";
import test from "node:test";

import { collabTestHooks, resetCollabTestState } from "../browser-env.mjs";

test.afterEach(resetCollabTestState);

test("KNOWN: init timeout 應 reject，讓呼叫端安全退回單機模式", async () => {
  const target = {};
  await assert.rejects(
    collabTestHooks.createReadyPromise(target, 5),
    /timeout|逾時/i,
  );
});

test("KNOWN: 非同步存檔尚未成功前必須保留 pendingSave", () => {
  const neverSettles = new Promise(() => {});
  const target = {
    isSaver: true,
    pendingSave: true,
    text: { toString: () => "unsaved draft" },
    onSaveRequest: () => neverSettles,
  };
  collabTestHooks.setSession(target);

  collabTestHooks.runSaverSave();

  assert.equal(target.pendingSave, true);
});
