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

test("expired order shows an inspection control without creating or settling orders", async () => {
  const h = host({ orderId: 42 });
  const request = h.bridge.request;
  const sizes = [];
  h.bridge.resize = height => sizes.push(height);
  h.bridge.request = async (type, input) => {
    if (type === "checkout.context") return { currency: "USD", currency_precision: 2, order_id: 42, presentation: "host" };
    if (input?.action === "payments.methods") return { data: [{ id: "tdcloud", name: "TDCloud", currency: "USD", checkout_expires_at: new Date(Date.now() - 1000).toISOString() }] };
    return request(type, input);
  };
  const container = root();
  const entry = await mountPaymentEntry(container, { bridge: h.bridge });
  assert.equal(entry.getState().phase, "ready");
  assert.equal(container.hidden, false);
  assert.match(container.children[0].textContent, /窗口已超时.*确认过期后/);
  assert.ok(sizes.at(-1) > 0);
  assert.equal(container.find(node => node.tagName === "button").textContent, "检查原收款状态");
  assert.equal(h.calls.some(call => call.input?.action === "payments.prepare"), false);
  assert.equal(h.published[0][0].disabled, false);
  entry.destroy();
});

test("inspection displays authenticated review state without starting or confirming payment", async () => {
  const h = host({ orderId: 42 }); const request = h.bridge.request;
  const container = root();
  h.bridge.request = async (type, input) => {
    if (input?.action === "payments.inspect") {
      h.calls.push({ type, input });
      return { data: { order_id: 42, provider_order_id: "mp-42", provider_state: "review", state: "unknown", checked_at: "2026-10-14T06:00:00Z" } };
    }
    return request(type, input);
  };
  const entry = await mountPaymentEntry(container, { bridge: h.bridge });
  await entry.inspect();
  assert.match(container.children[0].textContent, /复核原收款/);
  assert.equal(container.children[0].attributes["data-provider-state"], "review");
  assert.equal(container.children[0].attributes["data-provider-order"], "mp-42");
  assert.equal(h.calls.filter(c => c.input?.action === "payments.inspect").length, 1);
  assert.equal(h.calls.some(c => ["payments.prepare", "payments.reconcile"].includes(c.input?.action)), false);
  entry.destroy();
});

test("inspection classifies access rejection and never labels it as expiry", async () => {
  const h = host({ orderId: 42 }); const request = h.bridge.request;
  h.bridge.request = async (type, input) => input?.action === "payments.inspect"
    ? { error: { code: "provider_access_denied" } } : request(type, input);
  const container = root(); const entry = await mountPaymentEntry(container, { bridge: h.bridge });
  await entry.inspect();
  assert.match(container.children[0].textContent, /拒绝查询/);
  assert.equal(container.children[0].attributes["data-inspection-error"], "provider_access_denied");
  assert.doesNotMatch(container.children[0].textContent, /已确认过期/);
  entry.destroy();
});

test("checkout deadline notice is scheduled locally and canceled on destruction", async () => {
  const h = host({ orderId: 42 }); const request = h.bridge.request;
  let timerCallback, canceled = false;
  const container = root();
  container.ownerDocument.defaultView = {
    addEventListener: () => {}, removeEventListener: () => {},
    setTimeout: (callback) => { timerCallback = callback; return 17; },
    clearTimeout: id => { canceled = id === 17; },
  };
  h.bridge.request = async (type, input) => {
    if (type === "checkout.context") return { currency: "USD", currency_precision: 2, order_id: 42, presentation: "host" };
    if (input?.action === "payments.methods") return { data: [{ id: "tdcloud", name: "TDCloud", currency: "USD", checkout_expires_at: new Date(Date.now() + 60_000).toISOString() }] };
    return request(type, input);
  };
  const entry = await mountPaymentEntry(container, { bridge: h.bridge });
  assert.equal(container.hidden, true);
  timerCallback();
  assert.equal(container.hidden, false);
  entry.destroy();
  assert.equal(canceled, true);
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
