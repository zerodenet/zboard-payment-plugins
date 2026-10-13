import assert from "node:assert/strict";
import test from "node:test";
import { mountConfigEntry } from "./config-entry.mjs";
import { channelConfigProviders } from "./channel-config-providers.mjs";

class Element {
  constructor(tag, doc) { this.tagName = tag; this.ownerDocument = doc; this.children = []; this.listeners = {}; this.attributes = {}; this.value = ""; this.checked = false; }
  append(...nodes) { this.children.push(...nodes); }
  replaceChildren(...nodes) { this.children = nodes; }
  setAttribute(key, value) { this.attributes[key] = value; }
  addEventListener(key, callback) { this.listeners[key] = callback; }
  find(predicate) { if (predicate(this)) return this; for (const node of this.children) { const found = node.find(predicate); if (found) return found; } }
  reportValidity() {
    const invalid = this.find(el => ["input", "textarea", "select"].includes(el.tagName) && el.required && !el.value);
    return !invalid;
  }
}
function fixture(channels = []) {
  const doc = { createElement(tag) { return new Element(tag, doc); } };
  const root = new Element("main", doc);
  const calls = [], saves = [];
  let revision = 1, entries = structuredClone(channels), conflict = false;
  const bridge = {
    hostOrigin: () => "https://panel.example.test",
    request: async (type, payload) => {
      calls.push(type);
      if (type === "context.load") return { surface: "admin", plugin_id: "zboard.payments", page_id: "settings" };
      if (type === "config.load") return { revision, configured: true, config: { channels: structuredClone(entries) } };
      if (type === "config.save") {
        if (conflict) throw { code: "conflict" };
        assert.equal(payload.revision, revision);
        saves.push(structuredClone(payload)); entries = structuredClone(payload.config.channels);
        for (const item of entries) { delete item.settings.client_secret; delete item.settings.notify_secret; }
        revision++;
        return { revision, configured: true };
      }
      if (type === "config.test") return { healthy: true };
      throw Error("unexpected bridge type " + type);
    }, close() {},
  };
  return { root, bridge, calls, saves, input: key => root.find(el => el.id === key), conflict: value => { conflict = value; } };
}
const existing = {
  id: "tdcloud-main", name: "TDCloud", type: "tdcloud", enabled: true,
  settings: { api_url: "https://cloud.example.test", client_id: "merchant", notify_mode: "merchant_default", notify_url: "https://panel.example.test/.well-known/zboard-payments/v1/tdcloud-notify" },
};

test("failed host health exposes safe connection diagnostics without saving or creating orders", async () => {
  const f = fixture([existing]); const request = f.bridge.request;
  f.bridge.request = async (type, payload) => {
    if (type === "config.test") { f.calls.push(type); throw { code: "failed_precondition" }; }
    const result = await request(type, payload);
    if (type === "config.load") result.config.diagnostics = [{ id: existing.id, name: "TDCloud", connected: true, payment_ready: false, code: "provider_metadata_missing" }];
    return result;
  };
  const entry = await mountConfigEntry(f.root, { bridge: f.bridge });
  await entry.test();
  assert.ok(f.root.find(el => /商户连接已验证.*订单币种信息/.test(el.textContent || "")));
  assert.match(f.root.find(el => el.id === "notice").textContent, /接入尚未完成/);
  assert.equal(f.saves.length, 0);
  assert.equal(f.calls.some(type => type === "page.call"), false);
  entry.destroy();
});

test("admin configuration loads via host, saves a channel, and clears private inputs", async () => {
  const f = fixture(); const entry = await mountConfigEntry(f.root, { bridge: f.bridge });
  entry.edit(-1);
  for (const [key, value] of Object.entries(existing.settings)) {
    if (key === "notify_mode") continue;
    if (key === "notify_url") f.input("notify_origin").value = new URL(value).origin;
    else f.input(key).value = value;
  }
  f.input("client_secret").value = "private-test-secret";
  await entry.save();
  assert.equal(f.saves.length, 1);
  assert.equal(f.root.find(el => el.id === "payment_no"), undefined);
  assert.equal(f.input("currencies"), undefined);
  assert.equal(f.saves[0].config.channels[0].settings.currencies, undefined);
  assert.equal(f.saves[0].config.channels[0].settings.client_secret, "private-test-secret");
  assert.equal(f.input("client_secret").value, "");
  assert.equal(f.saves[0].config.channels[0].settings.notify_url, undefined);
  assert.equal(f.calls.some(type => type === "page.call" || type === "checkout.context"), false);
  await entry.test(); assert.deepEqual(f.calls.slice(-2), ["config.test", "config.load"]); entry.destroy();
});

test("editing retains omitted secrets and all other channels; merchant changes require a new secret", async () => {
  const other = { ...structuredClone(existing), id: "other", name: "Other" };
  const f = fixture([existing, other]); const entry = await mountConfigEntry(f.root, { bridge: f.bridge });
  entry.edit(0); f.input("name").value = "Edited"; await entry.save();
  const channels = f.saves[0].config.channels;
  assert.equal(channels.length, 2); assert.equal(channels[0].name, "Edited");
  assert.equal(channels[0].settings.client_secret, undefined);
  assert.equal(channels[0].settings.notify_secret, undefined);
  assert.deepEqual(channels[1], other);
  entry.edit(0); f.input("client_id").value = "changed-merchant"; await entry.save();
  assert.equal(f.saves.length, 1);
  f.input("client_secret").value = "new-test-secret";
  f.input("notify_secret").value = "new-platform-notification-key-123456789";
  await entry.save(); assert.equal(f.saves.length, 2); entry.destroy();
});

test("conflicted saves lock further writes until a refresh without replaying credentials", async () => {
  const f = fixture([existing]); const entry = await mountConfigEntry(f.root, { bridge: f.bridge });
  entry.edit(0); f.input("client_secret").value = "private-test-secret"; f.conflict(true);
  await entry.save(); const count = f.calls.length; await entry.save();
  assert.equal(f.calls.length, count); assert.equal(f.input("client_secret").value, "");
  assert.match(f.root.find(el => el.id === "notice").textContent, /其他页面修改/);
  f.conflict(false); await entry.reload(); entry.edit(0); await entry.save();
  assert.equal(f.saves.length, 1); entry.destroy();
});

test("channel removal has inline confirmation within the sandboxed iframe", async () => {
  const f = fixture([existing]); const entry = await mountConfigEntry(f.root, { bridge: f.bridge });
  entry.edit(0); await entry.remove(); assert.equal(f.saves.length, 0);
  await entry.remove(); assert.deepEqual(f.saves[0].config.channels, []); entry.destroy();
});

test("account sessions cannot load or save merchant configuration", async () => {
  const f = fixture(); const request = f.bridge.request;
  f.bridge.request = (type, payload) => type === "context.load" ? { surface: "account" } : request(type, payload);
  const entry = await mountConfigEntry(f.root, { bridge: f.bridge });
  entry.edit(-1); await entry.save();
  assert.deepEqual(f.calls, []); assert.match(f.root.find(el => el.id === "notice").textContent, /管理员/); entry.destroy();
});

test("channel manager presents a common add action and implemented provider types", async () => {
  const f = fixture([existing]); const entry = await mountConfigEntry(f.root, { bridge: f.bridge });
  assert.ok(f.root.find(el => el.tagName === "button" && el.textContent === "添加支付渠道"));
  assert.equal(f.root.find(el => el.textContent === "添加 TDCloud 渠道"), undefined);
  entry.edit(-1);
  assert.equal(f.input("type").value, "tdcloud");
  assert.deepEqual(f.input("type").children.map(option => option.textContent), ["TDCloud"]);
  assert.equal(f.input("id").value, "tdcloud-2");
  assert.equal(f.input("type").disabled, false);
  entry.edit(0); assert.equal(f.input("type").disabled, true);
  entry.destroy();
});

test("a registered provider owns its fields while generic switching clears previous secrets", async () => {
  const alternate = {
    type: "fixture", label: "Fixture", idPrefix: "fixture", defaultName: "Fixture",
    fields: [{ key: "access_key", label: "Access key", type: "password", required: true }],
    validate() {}, settings: inputs => ({ access_key: inputs.access_key.value }),
  };
  const f = fixture(); const entry = await mountConfigEntry(f.root, { bridge: f.bridge, providers: [...channelConfigProviders, alternate] });
  entry.edit(-1); const oldSecret = f.input("client_secret"); oldSecret.value = "must-discard";
  f.input("type").value = "fixture"; f.input("type").listeners.change();
  assert.equal(oldSecret.value, ""); assert.equal(f.input("client_secret"), undefined);
  f.input("access_key").value = "fixture-private-key"; await entry.save();
  assert.equal(f.saves[0].config.channels[0].type, "fixture");
  assert.deepEqual(f.saves[0].config.channels[0].settings, { access_key: "fixture-private-key" });
  assert.equal(f.input("access_key").value, "");
  entry.edit(0); assert.equal(f.input("access_key").value, "");
  entry.destroy();
});

test("notifications default to the authenticated host origin and serialize a fixed path with the platform signing key", async () => {
  const f = fixture(); const entry = await mountConfigEntry(f.root, { bridge: f.bridge });
  entry.edit(-1);
  assert.equal(f.input("notify_origin").value, "https://panel.example.test");
  assert.equal(f.input("notify_path").readOnly, true);
  assert.ok(f.input("notify_secret"));
  f.input("api_url").value = existing.settings.api_url;
  f.input("client_id").value = "merchant";
  f.input("client_secret").value = "private-test-secret";
  f.input("notifications").checked = true;
  f.input("notifications").listeners.change();
  f.input("notify_secret").value = "platform-signing-key-123456789";
  f.input("notify_origin").value = "https://callbacks.example.test/";
  f.input("notify_path").value = "/browser-tampered-path";
  await entry.save();
  assert.equal(f.saves[0].config.channels[0].settings.notify_url, "https://callbacks.example.test/.well-known/zboard-payments/v1/tdcloud-notify");
  assert.equal(f.saves[0].config.channels[0].settings.notify_mode, "merchant_default");
  assert.equal(f.saves[0].config.channels[0].settings.notify_secret, "platform-signing-key-123456789");
  assert.equal(f.input("notify_secret").value, "");
  entry.destroy();
});

test("editing notifications preserves the saved origin; invalid origins cannot save", async () => {
  const f = fixture([existing]); const entry = await mountConfigEntry(f.root, { bridge: f.bridge });
  entry.edit(0);
  assert.equal(f.input("notify_origin").value, "https://panel.example.test");
  for (const value of ["http://callbacks.example.test", "https://callbacks.example.test/custom", "https://user:password@callbacks.example.test", "https://callbacks.example.test/?x=1"]) {
    f.input("notify_origin").value = value; await entry.save();
    assert.equal(f.saves.length, 0);
  }
  f.input("notify_origin").value = "https://new.example.test";
  await entry.save(); assert.equal(f.saves.length, 1);
  entry.destroy();
});
