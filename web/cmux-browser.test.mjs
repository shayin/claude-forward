import assert from "node:assert/strict";
import test from "node:test";
import { previewPorts, previewPointer, previewScreenshotURL, sendBrowserText } from "./cmux-browser.mjs";

test("only offers bounded native loopback service ports", () => {
  assert.deepEqual(previewPorts({ ports: [3000, 8080, 3000, 0, 65536, "1234"] }), [3000, 8080]);
  assert.deepEqual(previewPorts(null), []);
});

test("keeps unsent draft when browser is busy or the RPC fails", async () => {
  assert.equal(await sendBrowserText("中文", async () => false), false);
  assert.equal(await sendBrowserText("中文", async () => { throw new Error("offline"); }).catch(() => false), false);
  assert.equal(await sendBrowserText("中文", async (action, details) =>
    action === "browser_type" && details.text === "中文"), true);
});

test("only embeds PNG screenshot data and maps bounded clicks", () => {
  const prefix = "iVBORw0KGgoAAA==";
  assert.equal(previewScreenshotURL({ data: prefix }), `data:image/png;base64,${prefix}`);
  assert.throws(() => previewScreenshotURL({ data: "javascript:alert(1)" }));
  const image = { getBoundingClientRect: () => ({ left: 10, top: 20, width: 100, height: 200 }) };
  assert.deepEqual(previewPointer({ clientX: 60, clientY: 120 }, image), { x: 0.5, y: 0.5 });
  assert.equal(previewPointer({ clientX: 0, clientY: 120 }, image), null);
});
