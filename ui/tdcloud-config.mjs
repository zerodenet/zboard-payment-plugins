// Provider-owned fields and validation. The callback path matches the manifest
// route; operators can change only the receiving site's HTTPS origin.
export const tdcloudNotificationPath = "/.well-known/zboard-payments/v1/tdcloud-notify";
function notificationOrigin(settings, context) {
  if (settings.notify_url) {
    try { return new URL(settings.notify_url).origin; } catch { return ""; }
  }
  return context.hostOrigin || "";
}
function endpoint(input) {
  try {
    const url = new URL(input.trim());
    if (url.protocol !== "https:" || url.username || url.password || url.pathname !== "/" || url.search || url.hash) return null;
    return url.origin;
  } catch { return null; }
}
export const tdcloudConfig = Object.freeze({
  type: "tdcloud", label: "TDCloud", idPrefix: "tdcloud", defaultName: "TDCloud",
  privateSettings: ["client_secret", "notify_secret"],
  fields: [
    { key: "api_url", label: "API 地址", type: "url", required: true, hint: "填写 TDCloud API 的 HTTPS 源地址，不含路径。" },
    { key: "client_id", label: "商户 Client ID", required: true },
    { key: "client_secret", label: "商户 Client Secret", type: "password", hint: "已有密钥留空保留；更换 API 地址或 Client ID 时需重新填写。" },
    { key: "notifications", label: "接收支付状态通知", type: "checkbox", checked: settings => !!settings.notify_url },
    { key: "notify_origin", label: "通知域名", type: "url", value: notificationOrigin, visible: inputs => inputs.notifications.checked, hint: "默认使用当前管理页域名。可修改为接收本站通知的公网 HTTPS 域名，不含路径。" },
    { key: "notify_path", label: "通知路径", readOnly: true, value: () => tdcloudNotificationPath, visible: inputs => inputs.notifications.checked },
    { key: "notify_secret", label: "TDCloud 通知签名密钥", type: "password", visible: inputs => inputs.notifications.checked, hint: "填写 TDCloud 收款状态通知生成的签名密钥，用于插件验签；与 Client Secret 不同。已保存的平台密钥留空保留。" },
    { key: "notify_key", type: "info", hint: "请将此域名加固定路径填写到 TDCloud 的默认通知地址。插件使用平台默认通知，不重新生成密钥或覆盖平台地址。", visible: inputs => inputs.notifications.checked },
  ],
  validate(inputs, previous) {
    const changedMerchant = !previous || previous.api_url !== inputs.api_url.value.trim() || previous.client_id !== inputs.client_id.value.trim();
    inputs.client_secret.required = changedMerchant;
    inputs.notify_origin.required = inputs.notifications.checked;
    inputs.notify_secret.required = inputs.notifications.checked && (!previous || previous.notify_mode !== "merchant_default" || changedMerchant);
    const invalid = inputs.notifications.checked && !endpoint(inputs.notify_origin.value);
    inputs.notify_origin.setCustomValidity?.(invalid ? "请填写公网 HTTPS 域名，不含路径、账号或查询参数。" : "");
    return !invalid;
  },
  settings(inputs) {
    const settings = { api_url: inputs.api_url.value.trim(), client_id: inputs.client_id.value.trim() };
    if (inputs.client_secret.value) settings.client_secret = inputs.client_secret.value;
    if (inputs.notifications.checked) {
      settings.notify_mode = "merchant_default";
      settings.notify_url = endpoint(inputs.notify_origin.value) + tdcloudNotificationPath;
      if (inputs.notify_secret.value) settings.notify_secret = inputs.notify_secret.value;
    }
    return settings;
  },
});
