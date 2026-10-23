import assert from "node:assert/strict";
import test from "node:test";
import { viewportDimensions, viewportReportKey } from "./cmux-viewport.mjs";

test("accepts only measurable viewport dimensions within cmux limits", () => {
  assert.deepEqual(viewportDimensions({ cols: 80, rows: 25 }), { columns: 80, rows: 25 });
  assert.deepEqual(viewportDimensions({ cols: 500, rows: 200 }), { columns: 300, rows: 120 });
  assert.equal(viewportDimensions(null), null);
  assert.equal(viewportDimensions({ cols: 10, rows: 25 }), null);
  assert.equal(viewportDimensions({ cols: 80.5, rows: 25 }), null);
  assert.equal(viewportDimensions({ cols: 80, rows: 0 }), null);
});

test("a new owner or surface requires its own viewport report", () => {
  const size = viewportDimensions({ cols: 90, rows: 28 });
  assert.notEqual(viewportReportKey("client-a", "surface-a", size),
    viewportReportKey("client-b", "surface-a", size));
  assert.notEqual(viewportReportKey("client-a", "surface-a", size),
    viewportReportKey("client-a", "surface-b", size));
});
