import assert from "node:assert/strict";
import test from "node:test";
import { readFile } from "node:fs/promises";
import { tdcloudNotificationPath } from "./tdcloud-config.mjs";

test("notification editor uses the actual registered route", async () => {
  const manifest = JSON.parse(await readFile(new URL("../manifest.json", import.meta.url), "utf8"));
  assert.equal(manifest.contributions.http_routes.find(route => route.id === "tdcloud-notification").path, tdcloudNotificationPath);
});
