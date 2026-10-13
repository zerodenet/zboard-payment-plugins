export class PaymentEntryError extends Error {
  constructor(code) { super(code); this.code = code; }
}

// The existing ZBoard postMessage bridge authenticates the exact parent and
// per-frame token. No account token or merchant credential enters this module.
export function createPluginBridge(win = window, { timeoutMs = 35000 } = {}) {
  const token = new URLSearchParams(win.location.hash.slice(1)).get("bridge_token");
  const pending = new Map();
  let sequence = 0;
  const requestPrefix = `payment_${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 10)}`;
  let closed = false;
  let hostOrigin = "";
  function receive(event) {
    const message = event.data;
    if (event.source !== win.parent || !message || message.source !== "zboard-plugin-host" || message.bridge_token !== token) return;
    const call = pending.get(message.request_id);
    if (!call) return;
    // Browser-supplied origin of the authenticated parent response provides
    // a form default without accessing the sandboxed parent's location.
    try {
      const origin = new URL(event.origin);
      if (origin.protocol === "https:" && origin.origin === event.origin) hostOrigin = origin.origin;
    } catch { /* Opaque/unavailable origins leave the default empty. */ }
    pending.delete(message.request_id);
    clearTimeout(call.timer);
    if (message.ok === true) call.resolve(message.result);
    else call.reject(new PaymentEntryError(message.code || "bridge_unavailable"));
  }
  win.addEventListener("message", receive);
  function send(type, payload = {}) {
    if (closed || !token || win.parent === win) throw new PaymentEntryError("session_unavailable");
    win.parent.postMessage({ ...payload, source: "zboard-plugin-ui", bridge_token: token, type }, "*");
  }
  return {
    hostOrigin: () => hostOrigin,
    request(type, payload = {}) {
      if (closed || !token || win.parent === win) return Promise.reject(new PaymentEntryError("session_unavailable"));
      if (pending.size >= 8) return Promise.reject(new PaymentEntryError("temporarily_unavailable"));
      const requestID = `${requestPrefix}_${++sequence}`;
      return new Promise((resolve, reject) => {
        const timer = setTimeout(() => { pending.delete(requestID); reject(new PaymentEntryError("bridge_timeout")); }, timeoutMs);
        pending.set(requestID, { resolve, reject, timer });
        try { send(type, { ...payload, request_id: requestID }); }
        catch (error) { clearTimeout(timer); pending.delete(requestID); reject(error); }
      });
    },
    resize(height) { if (Number.isFinite(height)) send("ui.resize", { height: Math.max(0, Math.min(1000, Math.ceil(height))) }); },
    publishOptions(options) { send("checkout.options", { options }); },
    publishStatus(state, code) { send("checkout.status", { state, ...(code ? { code } : {}) }); },
    close() {
      closed = true;
      win.removeEventListener("message", receive);
      for (const call of pending.values()) { clearTimeout(call.timer); call.reject(new PaymentEntryError("session_unavailable")); }
      pending.clear();
    },
  };
}

export async function callPaymentAction(bridge, action, payload = {}) {
  const result = await bridge.request("page.call", { action, payload });
  if (result?.error?.code) throw new PaymentEntryError(result.error.code);
  if (!result || !("data" in result)) throw new PaymentEntryError("invalid_response");
  return result.data;
}
