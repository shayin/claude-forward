import assert from "node:assert/strict";
import test from "node:test";
import { renderGrid } from "./cmux-render.mjs";
import { alternateFramePatch, sameRenderedFrame } from "./cmux-frame-update.mjs";

const frame = (rows, cursor = null, alternate = true) => renderGrid({
  columns: 12, rows: 3, active_screen: alternate ? "alternate" : "primary",
  row_spans: rows.map((text, row) => ({ row, column: 0, text, cell_width: text.length })),
  cursor
});

test("全屏快照只更新变化行，复位光标，不清空或重进全屏", () => {
  const before = frame(["top", "middle", "bottom"], { row: 2, column: 3 });
  const after = frame(["top", "changed", "bottom"], { row: 1, column: 2 });
  const patch = alternateFramePatch(before, after);
  assert.match(patch, /^\x1b\[2;1H\x1b\[0m\x1b\[2K/);
  assert.match(patch, /\x1b\[2;3H\x1b\[\?25h$/);
  assert.ok(!patch.includes("top"));
  assert.ok(!patch.includes("\x1b[?1049h"));
  assert.ok(!patch.includes("\x1b[3J"));
});

test("清掉消失的末行并处理光标隐藏，控制符仍来自安全渲染帧", () => {
  const before = frame(["top", "middle", "bottom"], { row: 2, column: 3 });
  const after = frame(["top", "middle"]);
  const patch = alternateFramePatch(before, after);
  assert.match(patch, /\x1b\[3;1H\x1b\[0m\x1b\[2K/);
  assert.ok(!patch.includes("bottom"));
  assert.ok(patch.endsWith("\x1b[?25l"));
});

test("主屏、切屏、尺寸变化必须完整重建", () => {
  const alternate = frame(["top"]);
  assert.equal(alternateFramePatch(null, alternate), null);
  assert.equal(alternateFramePatch(alternate, frame(["top"], null, false)), null);
  assert.equal(alternateFramePatch(frame(["top"], null, false), alternate), null);
  assert.equal(alternateFramePatch(alternate, { ...alternate, columns: 13 }), null);
  assert.equal(alternateFramePatch(alternate, { ...alternate, rows: 4 }), null);
});

test("内容相同但程序切换鼠标模式时仍更新滚动策略", () => {
  const before = frame(["top"]);
  assert.equal(sameRenderedFrame(before, { ...before }, before.columns, before.rows), true);
  assert.equal(sameRenderedFrame(before, { ...before, mouseScroll: true },
    before.columns, before.rows), false);
  assert.equal(sameRenderedFrame(null, before, before.columns, before.rows), false);
  assert.equal(sameRenderedFrame(before, before, before.columns + 1, before.rows), false);
});
