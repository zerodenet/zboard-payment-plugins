import assert from "node:assert/strict";
import test from "node:test";
import { createPluginBridge, callPaymentAction } from "./plugin-bridge.mjs";

function frame() {
  const listeners = new Map();
  const messages = [];
  const win = { location: { hash: "#bridge_token=frame-token" },
    parent: { postMessage: (message) => messages.push(message) },
    addEventListener: (name, callback) => listeners.set(name, callback),
    removeEventListener: (name) => listeners.delete(name) };
  return { win, messages, reply(data, source = win.parent, origin = "https://panel.example.test") { listeners.get("message")?.({ source, data, origin }); } };
}

test("bridge authenticates the parent and token and matches each outstanding request", async () => {
  const f = frame();
  const bridge = createPluginBridge(f.win);
  const pending = bridge.request("context.load");
  const request = f.messages[0];
  const response = { source: "zboard-plugin-host", bridge_token: "frame-token", request_id: request.request_id, ok: true, result: { surface: "account" } };
  let settled = false;
  void pending.then(() => { settled = true; });
  f.reply(response, {});
  f.reply({ ...response, bridge_token: "wrong" });
  f.reply({ ...response, request_id: "unknown" });
  await Promise.resolve();
  assert.equal(settled, false);
  assert.equal(bridge.hostOrigin(), "");
  f.reply(response);
  assert.deepEqual(await pending, { surface: "account" });
  assert.equal(bridge.hostOrigin(), "https://panel.example.test");
  bridge.close();
});

test("opaque parent origins do not become notification defaults", async () => {
  const f = frame(); const bridge = createPluginBridge(f.win);
  const pending = bridge.request("context.load");
  f.reply({ source: "zboard-plugin-host", bridge_token: "frame-token", request_id: f.messages[0].request_id, ok: true, result: {} }, f.win.parent, "null");
  await pending; assert.equal(bridge.hostOrigin(), ""); bridge.close();
});

test("timeouts and teardown reject outstanding work without leaking timers", async () => {
  const f = frame();
  const bridge = createPluginBridge(f.win, { timeoutMs: 5 });
  await assert.rejects(bridge.request("context.load"), { code: "bridge_timeout" });
  const pending = bridge.request("context.load");
  bridge.close();
  await assert.rejects(pending, { code: "session_unavailable" });
  await assert.rejects(bridge.request("context.load"), { code: "session_unavailable" });
});

test("bridge publishes formal options and preserves structured host authorization errors", async () => {
  const f = frame();
  const bridge = createPluginBridge(f.win);
  bridge.publishOptions([{ id: "tdcloud-main", title: "TDCloud", prepare_action: "payments.prepare", reconcile_action: "payments.reconcile" }]);
  assert.equal(f.messages[0].type, "checkout.options");
  const pending = bridge.request("checkout.context");
  const id = f.messages[1].request_id;
  f.reply({ source: "zboard-plugin-host", bridge_token: "frame-token", request_id: id, ok: false, code: "session_expired" });
  await assert.rejects(pending, { code: "session_expired" });
  await assert.rejects(callPaymentAction({ request: async () => ({ error: { code: "host_capability_missing" } }) }, "payments.status"), { code: "host_capability_missing" });
  bridge.close();
});
test("payment status and zero-height slots retain the authenticated bridge contract", () => {
  const f = frame(); const bridge = createPluginBridge(f.win);
  bridge.resize(0); bridge.publishStatus("error", "deadline_exceeded");
  assert.equal(f.messages[0].height, 0);
  assert.equal(f.messages[1].type, "checkout.status");
  assert.equal(f.messages[1].bridge_token, "frame-token");
  bridge.close();
});
