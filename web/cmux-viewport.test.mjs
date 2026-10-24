import assert from "node:assert/strict";
import test from "node:test";
import { snapshotViewportPayload, viewportDimensions } from "./cmux-viewport.mjs";

test("accepts only measurable viewport dimensions within cmux limits", () => {
  assert.deepEqual(viewportDimensions({ cols: 80, rows: 25 }), { columns: 80, rows: 25 });
  assert.deepEqual(viewportDimensions({ cols: 500, rows: 200 }), { columns: 300, rows: 120 });
  assert.equal(viewportDimensions(null), null);
  assert.equal(viewportDimensions({ cols: 10, rows: 25 }), null);
  assert.equal(viewportDimensions({ cols: 80.5, rows: 25 }), null);
  assert.equal(viewportDimensions({ cols: 80, rows: 0 }), null);
});

test("snapshot只在可见且拥有输入权时携带非粘性视口", () => {
  assert.deepEqual(snapshotViewportPayload("w", "s", true, true, { cols: 90, rows: 28 }),
    { workspace_id: "w", surface_id: "s", columns: 90, rows: 28 });
  assert.deepEqual(snapshotViewportPayload("w", "s", false, true, { cols: 90, rows: 28 }),
    { workspace_id: "w", surface_id: "s" });
  assert.deepEqual(snapshotViewportPayload("w", "s", true, false, { cols: 90, rows: 28 }),
    { workspace_id: "w", surface_id: "s" });
  assert.deepEqual(snapshotViewportPayload("w", "s", true, true, null),
    { workspace_id: "w", surface_id: "s" });
});
