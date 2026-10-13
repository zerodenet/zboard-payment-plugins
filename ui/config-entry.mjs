import { createPluginBridge } from "./plugin-bridge.mjs";
import { channelConfigProviders } from "./channel-config-providers.mjs";

const errors = {
  conflict: "配置已被其他页面修改，请刷新后再保存。",
  invalid_argument: "配置未通过校验，请检查渠道参数及密钥。",
  permission_denied: "需要管理员配置会话，请重新打开插件配置页。",
  session_expired: "配置会话已过期，请重新打开插件配置页。",
  bridge_timeout: "请求超时；若刚保存，请先刷新确认结果。",
};

// Only the authenticated host configuration bridge persists merchant settings.
// This page never calls TDCloud, keeps credentials in local storage, or enables a plugin.
export async function mountConfigEntry(root, { bridge, providers = channelConfigProviders } = {}) {
  const doc = root.ownerDocument;
  const win = doc.defaultView;
  bridge ||= createPluginBridge(win);
  let entries = [], revision = null, selected = -1, busy = false, destroyed = false, removeConfirmed = false, adapter = null;
  const providerFor = type => providers.find(provider => provider.type === type);
  let providerContext = { hostOrigin: "" };
  const node = (tag, text = "") => { const el = doc.createElement(tag); el.textContent = text; return el; };
  const heading = node("h2", "支付渠道");
  const explanation = node("p", "管理本站支付渠道。添加渠道时选择类型，再填写该渠道所需的连接参数。");
  const list = node("div"); list.className = "channels";
  const diagnostics = node("div"); diagnostics.setAttribute("role", "status");
  const state = node("p"); state.setAttribute("role", "status");
  const notice = node("p"); notice.id = "notice"; notice.setAttribute("role", "status"); notice.setAttribute("aria-live", "polite");
  const toolbar = node("div"); toolbar.className = "actions";
  const button = (label, callback, parent = toolbar) => {
    const el = node("button", label); el.type = "button";
    el.addEventListener("click", () => { void callback(); }); parent.append(el); return el;
  };
  const form = node("form"), fields = node("fieldset"), legend = node("legend", "渠道设置");
  fields.append(legend); form.append(fields); fields.hidden = true;
  const inputs = {}, labels = {};
  const providerFields = node("div");
  function redact(item) {
    const provider = providerFor(item.type);
    for (const field of provider?.fields || []) if (field.type === "password") delete item.settings[field.key];
    for (const key of provider?.privateSettings || []) delete item.settings[key];
  }
  function input(key, label, type = "text", hint = "", parent = fields) {
    const wrapper = node("label", label), el = node("input");
    el.type = type; el.id = key; el.name = key;
    el.autocomplete = type === "password" ? "new-password" : "off";
    wrapper.append(el); parent.append(wrapper); labels[key] = wrapper;
    if (hint) wrapper.append(node("small", hint));
    inputs[key] = el; return el;
  }
  input("id", "渠道标识", "text", "区分本站的渠道配置。2～32 位小写字母、数字或连字符，保存后保持不变。");
  inputs.id.pattern = "[a-z][a-z0-9-]{1,31}"; inputs.id.maxLength = 32;
  input("name", "显示名称"); inputs.name.maxLength = 80;
  input("enabled", "启用此渠道", "checkbox");
  const typeLabel = node("label", "渠道类型"), typeSelect = node("select");
  typeSelect.id = typeSelect.name = "type"; typeSelect.required = true;
  for (const provider of providers) { const option = node("option", provider.label); option.value = provider.type; typeSelect.append(option); }
  typeLabel.append(typeSelect); fields.append(typeLabel); inputs.type = typeSelect;
  fields.append(providerFields);
  function visibility() {
    for (const field of adapter?.fields || []) labels[field.key].hidden = field.visible ? !field.visible(inputs) : false;
    resize();
  }
  function renderProvider(type, settings = {}) {
    for (const field of adapter?.fields || []) { if (inputs[field.key]) inputs[field.key].value = ""; delete inputs[field.key]; delete labels[field.key]; }
    providerFields.replaceChildren(); adapter = providerFor(type);
    if (!adapter) return;
    for (const field of adapter.fields) {
      if (field.type === "info") {
        labels[field.key] = node("p", field.hint); providerFields.append(labels[field.key]); continue;
      }
      const el = input(field.key, field.label, field.type || "text", field.hint || "", providerFields);
      el.required = !!field.required;
      el.readOnly = !!field.readOnly;
      el.value = field.type === "password" ? "" : field.value ? field.value(settings, providerContext) : settings[field.key] ?? "";
      el.checked = field.checked ? field.checked(settings) : !!settings[field.key];
      el.addEventListener("change", visibility);
    }
    visibility();
  }
  typeSelect.addEventListener("change", () => {
    if (selected >= 0 || busy) return;
    renderProvider(typeSelect.value);
    if (adapter) { inputs.name.value = adapter.defaultName; inputs.id.value = availableID(adapter.idPrefix); }
  });
  function availableID(prefix) { let id = `${prefix}-main`, n = 2; while (entries.some(item => item.id === id)) id = `${prefix}-${n++}`; return id; }
  const actions = node("div"); actions.className = "actions"; fields.append(actions);
  const saveButton = button("保存渠道", save, actions); saveButton.className = "primary";
  const removeButton = button("移除此渠道", remove, actions);
  const cancelButton = button("取消编辑", () => { clearEditor(); lock(false); }, actions);
  const reloadButton = button("刷新配置", reload);
  const addButton = button("添加支付渠道", () => edit(-1));
  const testButton = button("检测已保存渠道", test);
  form.addEventListener("submit", event => { event.preventDefault(); void save(); });
  root.replaceChildren(heading, explanation, state, list, toolbar, diagnostics, form, notice);
  function message(text, error = false) {
    if (destroyed) return;
    notice.textContent = text; notice.setAttribute("data-error", String(error)); resize();
  }
  function resize() { if (!destroyed) bridge.resize?.(doc.documentElement?.scrollHeight || 500); }
  function lock(value) {
    busy = value; fields.disabled = busy || revision === null;
    reloadButton.disabled = busy; addButton.disabled = busy || revision === null || entries.length >= 16;
    testButton.disabled = busy || revision === null || !entries.some(item => item.enabled);
    for (const el of list.children) el.disabled = busy;
    removeButton.disabled = busy || selected < 0; cancelButton.disabled = busy;
  }
  function clearEditor() {
    selected = -1; fields.hidden = true;
    removeConfirmed = false; removeButton.textContent = "移除此渠道";
    for (const el of Object.values(inputs)) { el.value = ""; el.checked = false; }
  }
  function show(view) {
    if (!view || !Number.isSafeInteger(view.revision) || view.revision < 0 || typeof view.configured !== "boolean") throw { code: "invalid_response" };
    const channels = view.config?.channels ?? [];
    if (!Array.isArray(channels) || channels.length > 16) throw { code: "invalid_response" };
    revision = view.revision; entries = structuredClone(channels);
    diagnostics.replaceChildren();
    for (const report of view.config?.diagnostics || []) {
      const reason = report.code === "provider_metadata_missing" ? "支付接入未完成：订单币种信息自动读取尚未接通。"
        : report.code === "connection_failed" ? "连接检测未通过，请检查渠道接口、商户授权或网络。"
        : report.code === "configuration_invalid" ? "渠道配置未通过校验，请检查参数。"
        : report.code === "check_timeout" ? "渠道检测超时。"
        : report.code === "disabled" ? "渠道已停用。"
        : report.payment_ready ? "支付渠道已就绪。" : "尚未检测支付就绪状态。";
      diagnostics.append(node("p", `${report.name}：${report.connected === true ? "最近检测商户连接已验证；" : ""}${reason}`));
    }
    // The host projection must never put saved credentials into controls/state.
    for (const item of entries) { redact(item); }
    state.textContent = `已配置 ${entries.length} 个支付渠道`;
    list.replaceChildren();
    entries.forEach((item, index) => button(`${item.name} · ${providerFor(item.type)?.label || item.type} · ${item.enabled ? "已启用" : "已停用"}`, () => edit(index), list));
    if (!entries.length) list.append(node("p", "尚未配置支付渠道，点击“添加支付渠道”开始设置。"));
  }
  function edit(index) {
    if (busy || revision === null || destroyed) return;
    const provider = providerFor(index < 0 ? providers[0]?.type : entries[index]?.type);
    if (!provider) { clearEditor(); lock(false); message("此渠道类型暂不支持表单编辑。", true); return; }
    selected = index;
    removeConfirmed = false; removeButton.textContent = "移除此渠道";
    const item = index < 0 ? { id: availableID(provider.idPrefix), name: provider.defaultName, type: provider.type, enabled: true, settings: {} } : entries[index];
    renderProvider(item.type, item.settings);
    for (const key of ["id", "name", "type"]) { inputs[key].value = item[key]; inputs[key].required = true; }
    inputs.id.readOnly = index >= 0; inputs.type.disabled = index >= 0; inputs.enabled.checked = item.enabled;
    legend.textContent = index < 0 ? "添加支付渠道" : "编辑支付渠道";
    fields.hidden = false; visibility(); lock(false); message("填写渠道参数后保存。已有密钥不会回显。");
  }
  async function reload() {
    if (busy || destroyed) return;
    lock(true); state.textContent = "正在加载支付渠道配置";
    try {
      const context = await bridge.request("context.load");
      if (context?.surface !== "admin" || context.plugin_id !== "zboard.payments" || context.page_id !== "settings") throw { code: "permission_denied" };
      providerContext = { hostOrigin: bridge.hostOrigin?.() || "" };
      const view = await bridge.request("config.load");
      if (destroyed) return;
      show(view); clearEditor(); message("选择已有渠道编辑，或添加新渠道。");
    } catch (error) { revision = null; state.textContent = "配置加载失败"; message(errors[error.code] || "无法读取配置，请重试。", true); }
    finally { if (!destroyed) lock(false); }
  }
  async function persist(next) {
    lock(true); message("正在保存渠道配置");
    try {
      await bridge.request("config.save", { revision, config: { channels: next } });
      if (destroyed) return;
      // Reload the redacted view and new revision; never replay uncertain saves.
      const view = await bridge.request("config.load");
      if (destroyed) return;
      show(view); clearEditor(); message("渠道已保存。检测通过后可在插件详情页启用插件。");
    } catch (error) { revision = null; message(errors[error.code] || "保存失败，请刷新状态后重试。", true); }
    finally {
      for (const item of next) { redact(item); }
      for (const field of adapter?.fields || []) if (field.type === "password") inputs[field.key].value = "";
      if (!destroyed) lock(false);
    }
  }
  async function save() {
    if (busy || destroyed || revision === null || fields.hidden) return;
    const previous = selected < 0 ? null : entries[selected];
    if (!adapter || adapter.type !== inputs.type.value) { message("请选择可用的渠道类型。", true); return; }
    if (adapter.validate(inputs, previous?.settings) === false) { message("请检查渠道参数，通知域名须为公网 HTTPS 源地址。", true); return; }
    if (!form.reportValidity()) return;
    const value = { id: inputs.id.value.trim(), name: inputs.name.value.trim(), type: adapter.type, enabled: inputs.enabled.checked, settings: adapter.settings(inputs) };
    if (selected < 0 && entries.some(item => item.id === value.id)) { message("渠道标识已存在，请使用其他标识。", true); return; }
    const next = structuredClone(entries);
    if (selected < 0) next.push(value); else next[selected] = value;
    await persist(next);
  }
  async function remove() {
    if (busy || destroyed || revision === null || selected < 0) return;
    const target = selected;
    if (!removeConfirmed) {
      removeConfirmed = true; removeButton.textContent = "确认移除";
      message("再次点击确认移除；已有支付尝试仍使用原渠道快照核对。取消编辑可返回。");
      return;
    }
    await persist(entries.filter((_, index) => index !== target).map(item => structuredClone(item)));
  }
  async function test() {
    if (busy || destroyed || revision === null) return;
    lock(true); message("正在检测已保存渠道，最长约 12 秒");
    let failure = null;
    try { await bridge.request("config.test"); }
    catch (error) { failure = error; }
    try {
      const view = await bridge.request("config.load");
      if (destroyed) return;
      show(view);
      const reports = view.config?.diagnostics || [];
      if (reports.some(report => report.code === "provider_metadata_missing")) message("渠道接入尚未完成，具体原因见检测结果。", true);
      else if (failure) message(errors[failure.code] || "渠道检测未通过，具体原因见检测结果。", true);
      else message("检测通过，可回到插件详情页启用插件。");
    } catch { message("无法读取检测结果，请刷新配置后重试。", true); }
    finally { if (!destroyed) lock(false); }
  }
  function destroy() {
    destroyed = true; clearEditor(); entries = []; revision = null;
    observer?.disconnect(); bridge.close?.(); win?.removeEventListener("pagehide", destroy);
    root.replaceChildren();
  }
  const observer = win?.ResizeObserver ? new win.ResizeObserver(resize) : null;
  observer?.observe(root); win?.addEventListener("pagehide", destroy, { once: true });
  await reload();
  return { reload, edit, save, remove, test, destroy };
}
