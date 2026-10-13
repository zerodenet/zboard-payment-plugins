import assert from "node:assert/strict";
import test from "node:test";
import { mountPaymentWidget } from "./payment-widget.mjs";

class Node {
  constructor(tag, doc) {
    this.tagName = tag;
    this.ownerDocument = doc;
    this.children = [];
    this.listeners = {};
    this.attributes = {};
    this.textContent = "";
  }
  setAttribute(name, value) { this.attributes[name] = value; }
  addEventListener(name, handler) { this.listeners[name] = handler; }
  append(...nodes) { this.children.push(...nodes); }
  prepend(node) { this.children.unshift(node); }
  replaceChildren(...nodes) { this.children = nodes; }
  click() { this.listeners.click?.(); }
  find(predicate) {
    if (predicate(this)) return this;
    for (const child of this.children) {
      const result = child.find(predicate);
      if (result) return result;
    }
    return null;
  }
}

function fixture() {
  const doc = { createElement(tag) { return new Node(tag, doc); } };
  return new Node("root", doc);
}
const button = (root, label) => root.find((node) => node.tagName === "button" && node.textContent === label);
const tick = () => new Promise((resolve) => setImmediate(resolve));

test("payment slot renders loading, opens a window immediately, then confirms the business order", async () => {
  const root = fixture();
  let releaseMethods;
  let paid = false;
  const calls = [];
  const widget = mountPaymentWidget(root, {
    listMethods: () => new Promise((resolve) => { releaseMethods = resolve; }),
    openWindow: () => { calls.push("open"); return { location: { replace: () => calls.push("navigate") } }; },
    createOrder: async () => { calls.push("order"); return { id: "42" }; },
    startCheckout: async () => ({ redirect_url: "https://pay.example.test/42" }),
    confirmPayment: async () => ({ state: "paid" }),
    readOrder: async () => ({ status: paid ? "paid" : "pending" }),
  }, { currency: "USD" });
  assert.equal(widget.getState().phase, "loading");
  assert.equal(root.find((node) => node.attributes.role === "status")?.textContent, "正在加载支付方式");
  releaseMethods([{ id: "tdcloud", name: "TDCloud", currency: "USD" }]);
  await tick();
  assert.equal(widget.getState().phase, "ready");
  button(root, "去支付").click();
  assert.equal(calls[0], "open");
  await tick();
  assert.equal(widget.getState().phase, "awaiting");
  assert.deepEqual(calls, ["open", "order", "navigate"]);
  assert.ok(root.find((node) => node.attributes.role === "dialog"));
  button(root, "已支付，确认").click();
  await tick();
  assert.equal(widget.getState().phase, "pending");
  paid = true;
  button(root, "已支付，确认").click();
  await tick();
  assert.equal(widget.getState().phase, "paid");
  assert.equal(root.find((node) => node.attributes.role === "dialog"), null);
  widget.destroy();
  assert.equal(root.children.length, 0);
});

test("failed channel loading stays in the slot and can retry", async () => {
  const root = fixture();
  let loads = 0;
  const widget = mountPaymentWidget(root, {
    listMethods: async () => { if (++loads === 1) throw new Error("network"); return [{ id: "tdcloud", currency: "TDC" }]; },
  }, { currency: "TDC" });
  await tick();
  assert.equal(widget.getState().phase, "load_error");
  button(root, "重试加载").click();
  await tick();
  assert.equal(widget.getState().phase, "ready");
  assert.equal(loads, 2);
});

test("a return-page order read can retry without losing the returned identifier", async () => {
  const root = fixture();
  let reads = 0;
  const widget = mountPaymentWidget(root, {
    readOrder: async (id) => { if (++reads === 1) throw new Error("network"); return { id, status: "paid" }; },
    confirmPayment: async () => { throw new Error("already paid order needs no provider lookup"); },
  }, { returnedOrderId: "42" });
  await tick();
  assert.equal(widget.getState().phase, "confirm_error");
  button(root, "重新读取订单").click();
  await tick();
  assert.equal(reads, 2);
  assert.equal(widget.getState().phase, "paid");
  widget.destroy();
});
