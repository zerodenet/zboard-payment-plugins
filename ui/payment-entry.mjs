import { callPaymentAction, createPluginBridge } from "./plugin-bridge.mjs";

const paymentSlots = new Set(["account.checkout.payment", "account.orders.payment"]);

// Register methods with the formal host checkout. The sandboxed iframe never
// creates a business order, opens a window, or reports an order as fulfilled.
export async function mountPaymentEntry(root, { bridge } = {}) {
  bridge ||= createPluginBridge(root.ownerDocument.defaultView);
  const view = root.ownerDocument.defaultView;
  let destroyed = false;
  let loading = false;
  let hostPresentation = false;
  let state = { phase: "loading", methods: [] };
  const show = (message) => {
    if (destroyed) return;
    const node = root.ownerDocument.createElement("p");
    node.setAttribute("role", "status");
    node.setAttribute("aria-live", "polite");
    node.textContent = message;
    root.replaceChildren(node);
    root.hidden = hostPresentation;
  };
  const resize = () => { if (!destroyed) bridge.resize?.(hostPresentation ? 0 : root.scrollHeight || 40); };
  const observer = view?.ResizeObserver ? new view.ResizeObserver(resize) : null;
  observer?.observe(root);
  const hideMethods = () => { if (!destroyed) bridge.publishOptions([]); };
  const entry = {
    getState: () => ({ ...state, methods: [...state.methods] }),
    retry: () => initialize(),
    destroy() {
      if (destroyed) return;
      destroyed = true;
      observer?.disconnect();
      view?.removeEventListener("pagehide", onHide);
      bridge.close?.();
      root.replaceChildren();
    },
  };
  const onHide = () => entry.destroy();
  view?.addEventListener("pagehide", onHide, { once: true });
  async function initialize() {
    if (loading || destroyed) return;
    loading = true;
    state = { phase: "loading", methods: [] };
    show("正在加载支付方式");
    bridge.publishStatus?.("loading");
    try {
      const context = await bridge.request("context.load");
      if (context?.surface !== "account" || !paymentSlots.has(context.slot)) {
        throw { code: "permission_denied" };
      }
      const checkout = await bridge.request("checkout.context");
      hostPresentation = checkout.presentation === "host";
      const readiness = await callPaymentAction(bridge, "payments.status");
      if (!readiness.checkout_ready) throw { code: "host_capability_missing" };
      if (!/^[A-Z]{3}$/.test(checkout.currency || "") || checkout.currency_precision !== 2) throw { code: "invalid_argument" };
      const payload = { currency: checkout.currency };
      if (checkout.order_id != null) {
        if (!Number.isSafeInteger(checkout.order_id) || checkout.order_id <= 0) throw { code: "invalid_argument" };
        payload.order_id = String(checkout.order_id);
      }
      const methods = await callPaymentAction(bridge, "payments.methods", payload);
      if (destroyed) return;
      if (!Array.isArray(methods) || methods.length > 16) throw { code: "invalid_response" };
      const options = methods.map(method => ({
        id: method.id, title: method.name, disabled: !!method.disabled,
        prepare_action: "payments.prepare", reconcile_action: "payments.reconcile",
      }));
      bridge.publishOptions(options);
      state = { phase: options.length ? "ready" : "empty", methods: options };
      show(options.length ? "请在订单页选择支付方式" : "暂无可用支付方式");
      bridge.publishStatus?.(options.length ? "ready" : "empty");
    } catch (error) {
      if (destroyed) return;
      hideMethods();
      state = { phase: "error", methods: [], code: error?.code || "temporarily_unavailable" };
      bridge.publishStatus?.("error", state.code);
      if (["session_unavailable", "bridge_unavailable", "session_expired", "permission_denied"].includes(error?.code)) {
        show("支付会话不可用，请重新进入订单页");
      } else if (error?.code === "provider_metadata_missing") {
        show("支付渠道接入尚未完成，请联系管理员；订单币种信息暂不可用");
      } else if (error?.code === "host_capability_missing") {
        show("支付功能暂不可用，请联系管理员检查插件授权");
      } else {
        show(["bridge_timeout", "deadline_exceeded"].includes(error?.code) ? "支付方式加载超时，请重试" : ["invalid_argument", "invalid_response"].includes(error?.code) ? "支付接口返回无效数据，请联系管理员" : "暂时无法加载支付方式，请重试");
        const retry = root.ownerDocument.createElement("button");
        retry.type = "button";
        retry.textContent = "重试加载";
        retry.addEventListener("click", () => { void initialize(); });
        if (!hostPresentation) root.append(retry);
      }
    } finally { loading = false; resize(); }
  }
  await initialize();
  return entry;
}
