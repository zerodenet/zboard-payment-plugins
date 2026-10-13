import assert from "node:assert/strict";
import test from "node:test";
import { mountPaymentEntry } from "./payment-entry.mjs";
class Element {
  constructor(tag, doc) { this.tagName = tag; this.ownerDocument = doc; this.children = []; this.attributes = {}; this.listeners = {}; }
  setAttribute(key, value) { this.attributes[key] = value; }
  addEventListener(key, callback) { this.listeners[key] = callback; }
  replaceChildren(...nodes) { this.children = nodes; }
  append(...nodes) { this.children.push(...nodes); }
  find(predicate) { if (predicate(this)) return this; for (const node of this.children) { const found = node.find(predicate); if (found) return found; } }
}
function root() { const doc = { createElement(tag) { return new Element(tag, doc); } }; return new Element("main", doc); }
function host({ orderId = null, failure = false, ready = true } = {}) {
  const calls = [], published = [];
  return { calls, published, bridge: {
    request: async (type, input) => {
      calls.push({ type, input });
      if (type === "context.load") return { surface: "account", slot: "account.checkout.payment" };
      if (type === "checkout.context") return { currency: "USD", currency_precision: 2, order_id: orderId };
      if (input.action === "payments.status") return { data: { checkout_ready: ready } };
      if (failure) throw { code: "deadline_exceeded" };
      return { data: [{ id: "tdcloud", name: "TDCloud", currency: "USD" }] };
    }, publishOptions: options => published.push(options),
  } };
}
test("entry registers options through the formal host checkout without creating orders or windows", async () => {
  const h = host();
  const container = root();
  const entry = await mountPaymentEntry(container, { bridge: h.bridge });
  assert.equal(entry.getState().phase, "ready");
  assert.deepEqual(h.published, [[{ id: "tdcloud", title: "TDCloud", disabled: false, prepare_action: "payments.prepare", reconcile_action: "payments.reconcile" }]]);
  assert.deepEqual(h.calls.map(call => call.type), ["context.load", "checkout.context", "page.call", "page.call"]);
  assert.equal(container.find(node => node.textContent === "去支付"), undefined);
  entry.destroy();
});
test("order-page discovery uses only the host-selected order and preserves disabled historical options", async () => {
  const h = host({ orderId: 42 });
  const request = h.bridge.request;
  h.bridge.request = async (type, input) => input?.action === "payments.methods"
    ? { data: [{ id: "old-method", name: "原订单支付方式", currency: "USD", disabled: true }] }
    : request(type, input);
  const entry = await mountPaymentEntry(root(), { bridge: h.bridge });
  assert.equal(h.published[0][0].disabled, true);
  entry.destroy();
});
test("missing commerce wiring clears host options and does not discover provider methods", async () => {
  const h = host({ ready: false });
  const container = root();
  const entry = await mountPaymentEntry(container, { bridge: h.bridge });
  assert.match(container.children[0].textContent, /授权/);
  assert.deepEqual(h.published, [[]]);
  assert.equal(h.calls.some(call => call.input?.action === "payments.methods"), false);
  entry.destroy();
});
test("transient discovery can retry in place and only publishes fresh methods", async () => {
  const h = host({ failure: true });
  const container = root();
  const entry = await mountPaymentEntry(container, { bridge: h.bridge });
  assert.equal(entry.getState().phase, "error");
  assert.ok(container.find(node => node.textContent === "重试加载"));
  h.bridge.request = host().bridge.request;
  await entry.retry();
  assert.equal(entry.getState().phase, "ready");
  assert.equal(h.published.length, 2);
  entry.destroy();
});

test("missing provider invoice metadata reports a permanent setup error without a retry button", async () => {
  const h = host(); const request = h.bridge.request;
  h.bridge.request = async (type, input) => input?.action === "payments.methods"
    ? Promise.reject({ code: "provider_metadata_missing" }) : request(type, input);
  const container = root();
  const entry = await mountPaymentEntry(container, { bridge: h.bridge });
  assert.equal(entry.getState().code, "provider_metadata_missing");
  assert.deepEqual(h.published, [[]]);
  assert.match(container.children[0].textContent, /尚未完成.*管理员/);
  assert.equal(container.find(node => node.tagName === "button"), undefined);
  entry.destroy();
});
test("host presentation reports errors and collapses the iframe without a second retry control", async () => {
  const h = host({ failure: true });
  const request = h.bridge.request;
  const statuses = [], sizes = [];
  h.bridge.request = async (type, input) => type === "checkout.context"
    ? { currency: "USD", currency_precision: 2, order_id: null, presentation: "host" }
    : request(type, input);
  h.bridge.publishStatus = (state, code) => statuses.push({ state, code });
  h.bridge.resize = height => sizes.push(height);
  const container = root();
  const entry = await mountPaymentEntry(container, { bridge: h.bridge });
  assert.equal(entry.getState().code, "deadline_exceeded");
  assert.deepEqual(statuses.at(-1), { state: "error", code: "deadline_exceeded" });
  assert.equal(container.hidden, true);
  assert.equal(container.find(node => node.tagName === "button"), undefined);
  assert.deepEqual(sizes, [0]);
  entry.destroy();
});
test("missing provider metadata remains a specific unavailable reason and publishes no payment options", async () => {
  const h = host(); const request = h.bridge.request;
  const statuses = [];
  h.bridge.request = async (type, input) => input?.action === "payments.methods"
    ? { error: { code: "provider_metadata_missing" } } : request(type, input);
  h.bridge.publishStatus = (state, code) => statuses.push({ state, code });
  const container = root(); const entry = await mountPaymentEntry(container, { bridge: h.bridge });
  assert.equal(entry.getState().code, "provider_metadata_missing");
  assert.match(container.children[0].textContent, /订单币种信息暂不可用/);
  assert.deepEqual(h.published, [[]]);
  assert.deepEqual(statuses.at(-1), { state: "error", code: "provider_metadata_missing" });
  entry.destroy();
});
