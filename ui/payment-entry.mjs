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
  let expiryNotice = false;
  let expiryTimer;
  let orderId;
  let inspecting = false;
  let state = { phase: "loading", methods: [] };
  const show = (message) => {
    if (destroyed) return;
    const node = root.ownerDocument.createElement("p");
    node.setAttribute("role", "status");
    node.setAttribute("aria-live", "polite");
    node.textContent = message;
    root.replaceChildren(node);
    root.hidden = hostPresentation && !expiryNotice;
  };
  const resize = () => { if (!destroyed) bridge.resize?.(hostPresentation && !expiryNotice ? 0 : root.scrollHeight || 40); };
  const observer = view?.ResizeObserver ? new view.ResizeObserver(resize) : null;
  observer?.observe(root);
  const hideMethods = () => { if (!destroyed) bridge.publishOptions([]); };
  const entry = {
    getState: () => ({ ...state, methods: [...state.methods] }),
    retry: () => initialize(),
    inspect: () => inspectCurrent(),
    destroy() {
      if (destroyed) return;
      destroyed = true;
      if (expiryTimer != null) view?.clearTimeout?.(expiryTimer);
      observer?.disconnect();
      view?.removeEventListener("pagehide", onHide);
      bridge.close?.();
      root.replaceChildren();
    },
  };
  const onHide = () => entry.destroy();
  view?.addEventListener("pagehide", onHide, { once: true });
  function inspectButton() {
    if (orderId == null || destroyed) return;
    const button = root.ownerDocument.createElement("button");
    button.type = "button";
    button.textContent = inspecting ? "正在检查原收款状态…" : "检查原收款状态";
    button.disabled = inspecting;
    button.addEventListener("click", () => { void inspectCurrent(); });
    root.append(button);
  }
  async function inspectCurrent() {
    if (orderId == null || inspecting || destroyed) return;
    inspecting = true; expiryNotice = true;
    show("正在查询原收款单，不会创建新订单或确认付款。");
    inspectButton(); resize();
    try {
      const result = await callPaymentAction(bridge, "payments.inspect", { order_id: String(orderId) });
      if (destroyed) return;
      if (result?.order_id !== orderId || !/^[a-z][a-z0-9_]{0,63}$/.test(result.provider_state || "") ||
        !["pending", "unknown", "failed", "paid", "reversed"].includes(result.state)) throw { code: "invalid_response" };
      state.inspection = result;
      show(result.state === "failed" ? "原收款单已确认过期。点击“继续支付”将在此订单上重新发起。"
        : result.state === "paid" ? "渠道返回已付款。请点击“已支付，确认”核对本站订单。"
        : result.state === "reversed" ? "原收款出现退款或拒付，需要进一步核对。"
        : result.provider_state === "review" ? "渠道仍在复核原收款，尚不能重新发起支付。"
        : "原收款尚未确认结束，请稍后检查。");
      const node = root.children[0];
      node.setAttribute("data-provider-state", result.provider_state);
      node.setAttribute("data-provider-order", result.provider_order_id);
      node.setAttribute("data-checked-at", result.checked_at);
    } catch (error) {
      if (destroyed) return;
      const messages = {
        provider_oauth_failed: "原收款渠道的商户授权失败，请联系管理员。",
        provider_access_denied: "渠道拒绝查询原收款单，请联系管理员核对权限。",
        provider_order_not_found: "渠道未找到原收款单，请联系管理员核对。",
        provider_order_mismatch: "原收款单信息与本站订单不一致，已停止重试。",
        provider_configuration_invalid: "原收款渠道配置无法恢复，请联系管理员。",
        conflict: "原支付尝试与本站订单不一致，请联系管理员核对。",
        not_found: "本站尚未保存此订单的收款单，可点击继续支付。",
        pending: "原收款单仍在创建，请使用同一订单重试。",
      };
      show(messages[error?.code] || "暂时无法查询原收款单，请稍后重试。");
      root.children[0].setAttribute("data-inspection-error", error?.code || "temporarily_unavailable");
    } finally {
      inspecting = false;
      if (!destroyed) { inspectButton(); resize(); }
    }
  }
  async function initialize() {
    if (loading || destroyed) return;
    loading = true;
    expiryNotice = false;
    if (expiryTimer != null) view?.clearTimeout?.(expiryTimer);
    state = { phase: "loading", methods: [] };
    show("正在加载支付方式");
    bridge.publishStatus?.("loading");
    try {
      const context = await bridge.request("context.load");
      if (context?.surface !== "account" || !paymentSlots.has(context.slot)) {
        throw { code: "permission_denied" };
      }
      const checkout = await bridge.request("checkout.context");
      orderId = checkout.order_id ?? null;
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
      // The slot owns this notice. The host still owns its buttons/window.
      // No polling or provider request is needed to display a known deadline.
      const deadlines = methods.map(method => Date.parse(method.checkout_expires_at || "")).filter(Number.isFinite);
      if (deadlines.length) {
        const remaining = Math.min(...deadlines) - Date.now();
        const notice = () => {
          if (destroyed) return;
          expiryNotice = true;
          show("原支付窗口已超时。继续支付会先核对原收款结果；确认过期后自动创建新的支付尝试。结果未决时请稍后重试。");
          inspectButton();
          resize();
        };
        if (remaining <= 0) notice();
        else if (remaining < 2147483647) expiryTimer = view?.setTimeout?.(notice, remaining);
      }
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
