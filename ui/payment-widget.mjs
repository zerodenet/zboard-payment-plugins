import { createPaymentFlow } from "./payment-flow.mjs";

const css = `
.zbpay { color: inherit; font: inherit; }
.zbpay__status { min-height: 2.5rem; display: flex; align-items: center; gap: .6rem; }
.zbpay__spinner { width: 1rem; height: 1rem; border: 2px solid currentColor; border-right-color: transparent; border-radius: 50%; animation: zbpay-spin .8s linear infinite; }
@keyframes zbpay-spin { to { transform: rotate(360deg); } }
.zbpay__choices { display: grid; gap: .5rem; margin: .75rem 0; }
.zbpay__choice { display: flex; align-items: center; gap: .65rem; padding: .7rem .8rem; border: 1px solid currentColor; border-radius: .6rem; cursor: pointer; }
.zbpay__choice:has(input:checked) { outline: 2px solid currentColor; outline-offset: 1px; }
.zbpay__actions { display: flex; flex-wrap: wrap; align-items: center; gap: .65rem; }
.zbpay button { min-height: 2.5rem; padding: .4rem .85rem; border-radius: .5rem; cursor: pointer; }
.zbpay button:disabled { cursor: wait; opacity: .65; }
.zbpay__backdrop { position: fixed; inset: 0; z-index: 1000; background: rgba(0,0,0,.48); display: grid; place-items: center; padding: 1rem; }
.zbpay__dialog { box-sizing: border-box; width: min(100%, 26rem); padding: 1.25rem; border-radius: .8rem; background: Canvas; color: CanvasText; box-shadow: 0 1rem 3rem rgba(0,0,0,.25); }
.zbpay__dialog h2 { margin: 0 0 .65rem; font-size: 1.15rem; }
.zbpay__dialog p { line-height: 1.5; }
@media (prefers-reduced-motion: reduce) { .zbpay__spinner { animation: none; } }
`;

function element(doc, tag, className, text) {
  const node = doc.createElement(tag);
  if (className) node.className = className;
  if (text != null) node.textContent = text;
  return node;
}

// Mount only inside an authenticated payment slot. The host supplies scoped
// ports; the widget never receives merchant credentials or trusts URL status.
export function mountPaymentWidget(root, ports, { currency, returnedOrderId = "" }) {
  if (!root?.ownerDocument) throw new TypeError("payment slot root is required");
  const doc = root.ownerDocument;
  const style = element(doc, "style", "", css);
  const host = element(doc, "section", "zbpay");
  host.setAttribute("aria-label", "支付方式");
  root.replaceChildren(style, host);
  let dismissed = false;
  let destroyed = false;

  const flow = createPaymentFlow(ports, render);
  function button(label, action, disabled = false) {
    const node = element(doc, "button", "", label);
    node.type = "button";
    node.disabled = disabled;
    node.addEventListener("click", action);
    return node;
  }

  function render(state) {
    if (destroyed) return;
    host.replaceChildren();
    const status = element(doc, "div", "zbpay__status", state.message);
    status.setAttribute("role", "status");
    status.setAttribute("aria-live", "polite");
    if (["loading", "preparing", "confirming"].includes(state.phase)) {
      status.prepend(element(doc, "span", "zbpay__spinner"));
    }
    host.append(status);

    if (state.phase === "ready") {
      const choices = element(doc, "div", "zbpay__choices");
      for (const method of state.methods) {
        const label = element(doc, "label", "zbpay__choice");
        const radio = element(doc, "input");
        radio.type = "radio";
        radio.name = "zbpay-method";
        radio.value = method.id;
        radio.checked = state.methodId === method.id;
        radio.addEventListener("change", () => flow.select(method.id));
        label.append(radio, element(doc, "span", "", method.name || method.id));
        choices.append(label);
      }
      host.append(choices);
    }

    const actions = element(doc, "div", "zbpay__actions");
    if (["ready", "prepare_error", "failed"].includes(state.phase)) {
      actions.append(button(state.orderId ? "重新打开支付" : "去支付", () => { dismissed = false; void flow.pay(); }, !state.methodId));
    }
    if (["load_error", "empty"].includes(state.phase)) {
      actions.append(button("重试加载", () => { void flow.load(state.currency); }));
    }
    if (state.phase === "confirm_error" && !state.orderId && returnedOrderId) {
      actions.append(button("重新读取订单", () => { void flow.checkoutReturned(returnedOrderId); }));
    }
    const hasDialog = state.orderId && ["preparing", "awaiting", "confirming", "pending", "confirm_error"].includes(state.phase);
    if (hasDialog && dismissed) {
      actions.append(button("查看支付状态", () => { dismissed = false; render(flow.getState()); }));
    }
    host.append(actions);

    if (hasDialog && !dismissed) {
      const backdrop = element(doc, "div", "zbpay__backdrop");
      const dialog = element(doc, "div", "zbpay__dialog");
      dialog.setAttribute("role", "dialog");
      dialog.setAttribute("aria-modal", "true");
      dialog.setAttribute("aria-label", "支付确认");
      dialog.append(element(doc, "h2", "", "确认支付结果"), element(doc, "p", "", state.message));
      const dialogActions = element(doc, "div", "zbpay__actions");
      dialogActions.append(button(state.phase === "confirming" ? "正在核对…" : "已支付，确认",
        () => { void flow.confirm(); }, !["awaiting", "pending", "confirm_error"].includes(state.phase)));
      dialogActions.append(button("稍后再试", () => { dismissed = true; render(flow.getState()); }));
      dialog.append(dialogActions);
      backdrop.append(dialog);
      host.append(backdrop);
    }
  }

  render(flow.getState());
  if (returnedOrderId) void flow.checkoutReturned(returnedOrderId);
  else void flow.load(currency);
  return {
    getState: flow.getState,
    confirm: flow.confirm,
    checkoutReturned: flow.checkoutReturned,
    destroy() { destroyed = true; root.replaceChildren(); },
  };
}
