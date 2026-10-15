import { createPluginBridge, callPaymentAction } from "./plugin-bridge.mjs";

const reasons = { additional_payment: "同一订单存在额外收款", host_payment_conflict: "付款与本站已记账结果冲突", closed_business_order: "订单关闭后收到付款", provider_reversal: "渠道出现退款或拒付", settlement_pending: "付款已确认，本站记账待重试", historical_credentials_missing: "旧支付尝试缺少历史凭据，需人工核对" };

// Read-only administrator worklist. Refunds and entitlement decisions belong
// to the operator and the owning platform; there is no force-paid button.
export async function mountPaymentReviews(root, { bridge } = {}) {
  bridge ||= createPluginBridge(root.ownerDocument.defaultView);
  const doc = root.ownerDocument;
  const node = (tag, text) => { const el = doc.createElement(tag); el.textContent = text; return el; };
  const title = node("h2", "付款核对");
  const help = node("p", "以下收款需要人工核对。请在收款平台核实并按退款或人工处理流程处置；插件不自动退款或重复开通权益。");
  const status = node("p", "正在读取付款记录"); status.setAttribute("role", "status");
  const list = node("div", "");
  const actions = node("div", ""); actions.className = "actions";
  let offset = 0, total = 0, busy = false;
  const button = (label, fn) => { const el = node("button", label); el.type = "button"; el.addEventListener("click", () => { void fn(); }); actions.append(el); return el; };
  const refresh = button("刷新", () => load());
  const previous = button("上一页", () => { offset = Math.max(0, offset - 20); return load(); });
  const next = button("下一页", () => { offset += 20; return load(); });
  root.replaceChildren(title, help, status, list, actions);
  function lock() { refresh.disabled = busy; previous.disabled = busy || offset === 0; next.disabled = busy || offset + 20 >= total; }
  async function load() {
    if (busy) return;
    busy = true; lock();
    try {
      const page = await callPaymentAction(bridge, "payments.reviews", { offset, limit: 20 });
      if (!Array.isArray(page.items) || page.items.length > 20 || !Number.isSafeInteger(page.total) || page.total < 0) throw new Error("invalid_response");
      offset = page.offset; total = page.total; list.replaceChildren();
      status.textContent = total ? `待核对 ${total} 项 · 当前 ${offset + 1}–${offset + page.items.length}` : "暂无待核对收款";
      for (const item of page.items) {
        const card = node("section", "");
        card.append(node("h3", `订单 #${item.order_id} · ${reasons[item.reason] || "需要进一步核对"}`));
        card.append(node("p", `收款单 ${item.provider_order_id}`));
        if (item.fact) {
          const f = item.fact;
          const minor = BigInt(f.amount_minor);
          card.append(node("p", `${f.currency} ${minor / 100n}.${String(minor % 100n).padStart(2, "0")} · ${f.occurred_at}`));
          card.append(node("p", `支付事件 ${f.provider_event_id} · 交易号 ${f.provider_trade_no}`));
        }
        list.append(card);
      }
    } catch { status.textContent = "暂时无法读取付款记录，请刷新重试。"; }
    finally { busy = false; lock(); bridge.resize?.(Math.min(1000, root.scrollHeight || 500)); }
  }
  await load();
  return { reload: load };
}
