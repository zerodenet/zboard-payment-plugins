// Plugin-owned checkout interaction state. The eventual ZBoard slot supplies
// authenticated ports; this module never handles a ZBoard session or TDCloud
// merchant credential in the browser.
export function createPaymentFlow(ports, onChange = () => {}) {
  let state = { phase: "idle", currency: "", methods: [], methodId: "", orderId: "", message: "" };
  let loadVersion = 0;
  let preparation = null;
  let confirmation = null;

  const snapshot = () => ({ ...state, methods: [...state.methods] });
  const update = (change) => {
    state = { ...state, ...change };
    onChange(snapshot());
    return snapshot();
  };

  const failureMessage = (error, fallback) => {
    if (error?.code === "checkout_expiry_pending") return "支付窗口已超时，渠道仍在核对原收款结果；请稍后重试";
    if (error?.code === "provider_metadata_missing") return "支付渠道接入尚未完成，请联系管理员";
    if (error?.code === "host_capability_missing") return "支付功能尚未开放，请稍后从订单页重试";
    if (["session_unavailable", "bridge_unavailable", "permission_denied"].includes(error?.code)) return "支付会话不可用，请重新进入订单页";
    if (error?.code === "not_found") return "订单不存在或无权访问，请返回订单页";
    return fallback;
  };

  async function load(currency) {
    if (state.orderId) return snapshot();
    const version = ++loadVersion;
    const normalized = String(currency || "").trim().toUpperCase();
    update({ phase: "loading", currency: normalized, methods: [], methodId: "", message: "正在加载支付方式" });
    try {
      const methods = await ports.listMethods(normalized);
      if (version !== loadVersion) return snapshot();
      const available = Array.isArray(methods) ? methods.filter((item) => item?.id && item.currency === normalized) : [];
      return update({ phase: available.length ? "ready" : "empty", methods: available,
        methodId: available.length === 1 ? available[0].id : "",
        message: available.length ? "请选择支付方式" : "暂无可用支付方式" });
    } catch (error) {
      if (version !== loadVersion) return snapshot();
      return update({ phase: "load_error", message: failureMessage(error, "暂时无法加载支付方式，请重试") });
    }
  }

  function select(methodId) {
    if (state.phase !== "ready" || !state.methods.some((item) => item.id === methodId)) return snapshot();
    return update({ methodId, message: "" });
  }

  // Called directly inside the user's click handler. Reserve the window
  // before any asynchronous order or checkout request to avoid popup blocking.
  function pay() {
    if (preparation) return preparation;
    if (!["ready", "prepare_error", "failed"].includes(state.phase) || !state.methodId) return Promise.resolve(snapshot());
    let popup;
    try { popup = ports.openWindow(); } catch { popup = null; }
    if (!popup) return Promise.resolve(update({ phase: "prepare_error", message: "浏览器拦截了支付窗口，请允许弹窗后重试" }));
    const closePopup = () => { try { popup.close?.(); } catch { /* window may already be gone */ } };
    update({ phase: "preparing", message: "正在准备支付，请稍候" });
    preparation = (async () => {
      try {
        const orderId = state.orderId || String((await ports.createOrder(state.methodId)).id);
        if (!orderId || orderId === "undefined") throw new Error("missing order");
        update({ orderId });
        const checkout = await ports.startCheckout(orderId, state.methodId);
        if (!checkout.redirect_url && ["paid", "review", "pending", "creating"].includes(checkout.state)) {
          closePopup();
          return update({ phase: "awaiting", message: "支付状态待确认，请点击“已支付，确认”" });
        }
        const target = new URL(checkout.redirect_url);
        if (target.protocol !== "https:" || target.username || target.password || target.hash) throw new Error("invalid checkout URL");
        popup.location.replace(target.href);
        return update({ phase: "awaiting", message: "请在新窗口完成支付，然后点击“已支付，确认”" });
      } catch (error) {
        closePopup();
        return update({ phase: "prepare_error", message: failureMessage(error, "支付暂时无法发起，请重试；已有订单会继续使用") });
      } finally {
        preparation = null;
      }
    })();
    return preparation;
  }

  function confirm() {
    if (confirmation) return confirmation;
    if (!state.orderId || !["awaiting", "confirm_error", "pending"].includes(state.phase)) return Promise.resolve(snapshot());
    update({ phase: "confirming", message: "正在核对支付结果" });
    confirmation = (async () => {
      try {
        const result = await ports.confirmPayment(state.orderId);
        if (result.state !== "paid") {
          return update({ phase: result.state === "failed" ? "failed" : "pending", message: result.state === "failed"
            ? "本次支付未完成，可重新发起支付"
            : result.state === "review" ? "支付需要进一步核对，请稍后从订单页确认"
            : "暂未查到支付结果，请稍后再确认" });
        }
        const order = await ports.readOrder(state.orderId);
        return order.status === "paid"
          ? update({ phase: "paid", message: "支付成功，订单已完成" })
          : update({ phase: "pending", message: "支付已确认，订单仍在处理，请稍后再确认" });
      } catch (error) {
        return update({ phase: "confirm_error", message: failureMessage(error, "核对暂时失败，请稍后再确认") });
      } finally {
        confirmation = null;
      }
    })();
    return confirmation;
  }

  async function checkoutReturned(orderId) {
    const id = String(orderId);
    if (!id || id === "undefined" || (state.orderId && state.orderId !== id) || state.phase === "paid") return snapshot();
    if (!state.orderId) {
      try {
        // The returned page is a fresh instance. Its authenticated order read
        // establishes ownership; the URL parameter alone is not authority.
        const order = await ports.readOrder(id);
        if (order.id != null && String(order.id) !== id) throw new Error("order mismatch");
        if (order.status === "paid") return update({ orderId: id, phase: "paid", message: "支付成功，订单已完成" });
        update({ orderId: id, phase: "awaiting", message: "正在核对支付结果" });
      } catch (error) {
        return update({ phase: "confirm_error", message: failureMessage(error, "暂时无法读取订单，请重试") });
      }
    }
    return confirm();
  }

  return { getState: snapshot, load, select, pay, confirm, checkoutReturned };
}
