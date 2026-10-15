import assert from "node:assert/strict";
import test from "node:test";
import { mountPaymentReviews } from "./reviews-entry.mjs";

class Element {
  constructor(tag, doc) { this.tagName = tag; this.ownerDocument = doc; this.children = []; this.listeners = {}; }
  append(...nodes) { this.children.push(...nodes); }
  replaceChildren(...nodes) { this.children = nodes; }
  setAttribute() {}
  addEventListener(key, callback) { this.listeners[key] = callback; }
}
function fixture(page) {
  const doc = { createElement: tag => new Element(tag, doc) };
  const root = new Element("main", doc);
  const calls = [];
  const bridge = { resize() {}, async request(type, payload) { calls.push({ type, payload }); return { data: page }; } };
  return { root, bridge, calls };
}
const texts = el => [el.textContent || "", ...el.children.map(texts)].join(" ");

test("administrator worklist retains receipt details and has no settlement or refund action", async () => {
  const f = fixture({ offset: 0, total: 1, items: [{ order_id: 42, provider_order_id: "mp-old", reason: "additional_payment", fact: { amount_minor: 1050, currency: "USD", provider_event_id: "event-old", provider_trade_no: "trade-old", occurred_at: "2026-10-14T01:00:00Z" } }] });
  await mountPaymentReviews(f.root, { bridge: f.bridge });
  assert.match(texts(f.root), /额外收款/);
  assert.match(texts(f.root), /USD 10.50/);
  assert.match(texts(f.root), /event-old.*trade-old/);
  assert.deepEqual(f.calls, [{ type: "page.call", payload: { action: "payments.reviews", payload: { offset: 0, limit: 20 } } }]);
  assert.deepEqual(f.root.children.at(-1).children.map(e => e.textContent), ["刷新", "上一页", "下一页"]);
});

test("empty financial worklist is clear and paginated controls stay disabled", async () => {
  const f = fixture({ offset: 0, total: 0, items: [] });
  await mountPaymentReviews(f.root, { bridge: f.bridge });
  assert.match(texts(f.root), /暂无待核对收款/);
  assert.equal(f.root.children.at(-1).children[1].disabled, true);
  assert.equal(f.root.children.at(-1).children[2].disabled, true);
});
