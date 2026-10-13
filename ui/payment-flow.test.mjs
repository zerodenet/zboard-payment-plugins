import assert from "node:assert/strict";
import test from "node:test";
import { createPaymentFlow } from "./payment-flow.mjs";

test("opens payment window before creating an order and confirms only from ZBoard order state", async () => {
  const calls = [];
  let orderStatus = "pending";
  const flow = createPaymentFlow({
    listMethods: async () => [{ id: "tdcloud", currency: "USD" }],
    openWindow: () => {
      calls.push("open");
      return { location: { replace: (url) => calls.push(`navigate:${url}`) }, close: () => calls.push("close") };
    },
    createOrder: async () => { calls.push("order"); return { id: "42" }; },
    startCheckout: async () => { calls.push("checkout"); return { redirect_url: "https://pay.example.test/42" }; },
    confirmPayment: async () => { calls.push("confirm"); return { state: "paid" }; },
    readOrder: async () => { calls.push("read"); return { status: orderStatus }; },
  });
  await flow.load("usd");
  const pending = flow.pay();
  assert.equal(calls[0], "open");
  await pending;
  assert.deepEqual(calls.slice(0, 4), ["open", "order", "checkout", "navigate:https://pay.example.test/42"]);
  assert.equal(flow.getState().phase, "awaiting");
  await flow.confirm();
  assert.equal(flow.getState().phase, "pending");
  orderStatus = "paid";
  await flow.confirm();
  assert.equal(flow.getState().phase, "paid");
});

test("retry keeps the same business order and a returned window joins active confirmation", async () => {
  let orders = 0;
  let checkouts = 0;
  let confirmations = 0;
  let release;
  const flow = createPaymentFlow({
    listMethods: async () => [{ id: "tdcloud", currency: "USD" }],
    openWindow: () => ({ location: { replace: () => {} }, close: () => {} }),
    createOrder: async () => ({ id: String(++orders) }),
    startCheckout: async () => {
      if (++checkouts === 1) throw new Error("network");
      return { redirect_url: "https://pay.example.test/1" };
    },
    confirmPayment: () => { confirmations++; return new Promise((resolve) => { release = resolve; }); },
    readOrder: async () => ({ status: "paid" }),
  });
  await flow.load("USD");
  await flow.pay();
  assert.equal(flow.getState().phase, "prepare_error");
  await flow.pay();
  assert.equal(orders, 1);
  assert.equal(flow.getState().phase, "awaiting");
  const original = flow.confirm();
  const returned = flow.checkoutReturned("1");
  assert.equal(confirmations, 1);
  release({ state: "paid" });
  await Promise.all([original, returned]);
  assert.equal(flow.getState().phase, "paid");
});

test("fresh returned page verifies owned order before joining confirmation", async () => {
  let confirmations = 0;
  const ports = {
    readOrder: async (id) => ({ id, status: "pending" }),
    confirmPayment: async () => { confirmations++; return { state: "paid" }; },
  };
  const flow = createPaymentFlow(ports);
  await flow.checkoutReturned("42");
  assert.equal(confirmations, 1);
  assert.equal(flow.getState().orderId, "42");
  assert.equal(flow.getState().phase, "pending");

  const denied = createPaymentFlow({ ...ports, readOrder: async () => { throw new Error("forbidden"); } });
  await denied.checkoutReturned("another-account-order");
  assert.equal(confirmations, 1);
  assert.equal(denied.getState().orderId, "");
  assert.equal(denied.getState().phase, "confirm_error");
});

test("blocked popup leaves business order untouched and offers retry", async () => {
  let orders = 0;
  const flow = createPaymentFlow({
    listMethods: async () => [{ id: "tdcloud", currency: "USD" }],
    openWindow: () => { throw new Error("blocked"); },
    createOrder: async () => { orders++; return { id: "42" }; },
  });
  await flow.load("USD");
  await flow.pay();
  assert.equal(orders, 0);
  assert.equal(flow.getState().phase, "prepare_error");
  assert.match(flow.getState().message, /拦截/);
});

test("a resumed provider checkout without a redirect proceeds to verification", async () => {
  let closed = false;
  const flow = createPaymentFlow({
    listMethods: async () => [{ id: "tdcloud", currency: "USD" }],
    openWindow: () => ({ close: () => { closed = true; } }),
    createOrder: async () => ({ id: "42" }),
    startCheckout: async () => ({ order_id: "42", state: "pending" }),
    confirmPayment: async () => ({ state: "paid" }),
    readOrder: async () => ({ id: "42", status: "paid" }),
  });
  await flow.load("USD");
  await flow.pay();
  assert.equal(closed, true);
  assert.equal(flow.getState().phase, "awaiting");
  await flow.confirm();
  assert.equal(flow.getState().phase, "paid");
});
