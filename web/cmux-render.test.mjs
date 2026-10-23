import assert from "node:assert/strict";
import test from "node:test";
import { renderGrid } from "./cmux-render.mjs";

test("renders colored grid with scrollback and native cursor", () => {
  const frame = renderGrid({
    columns: 10, rows: 2, active_screen: "normal",
    styles: [{ id: 1, foreground: "#ff8000", bold: true }],
    scrollback_spans: [{ row: 0, column: 0, text: "old", cell_width: 3 }],
    row_spans: [{ row: 0, column: 2, text: "你好", cell_width: 4, style_id: 1 }],
    cursor: { row: 0, column: 6 }
  });
  assert.equal(frame.alternateScreen, false);
  assert.match(frame.content, /old.*\r\n/);
  assert.match(frame.content, /\x1b\[38;2;255;128;0m你好/);
  assert.match(frame.content, /\x1b\[7G\x1b\[\?25h/);
});

test("alternate screen keeps only the TUI viewport, not primary-screen history", () => {
  const frame = renderGrid({
    columns: 12, rows: 2, active_screen: "alternate",
    scrollback_spans: [{ row: 0, column: 0, text: "old history", cell_width: 11 }],
    row_spans: [{ row: 0, column: 0, text: "TUI", cell_width: 3 }],
    cursor: { row: 0, column: 3 }
  });
  assert.equal(frame.alternateScreen, true);
  assert.ok(frame.content.startsWith("\x1b[?1049h"));
  assert.ok(!frame.content.includes("old history"));
  assert.match(frame.content, /\x1b\[4G\x1b\[\?25h/);
  assert.equal(frame.mouseScroll, false);
});

test("only alternate-screen programs with SGR mouse tracking use remote scrolling", () => {
  const grid = {
    columns: 80, rows: 25, row_spans: [],
    modes: [{ code: 1006, ansi: false, on: true }, { code: 1002, ansi: false, on: true }]
  };
  assert.equal(renderGrid({ ...grid, active_screen: "alternate" }).mouseScroll, true);
  assert.equal(renderGrid({ ...grid, active_screen: "normal" }).mouseScroll, false);
  assert.equal(renderGrid({ ...grid, active_screen: "alternate", modes: [grid.modes[0]] }).mouseScroll, false);
  assert.equal(renderGrid({ ...grid, active_screen: "alternate",
    modes: [{ ...grid.modes[0], on: false }, grid.modes[1]] }).mouseScroll, false);
  assert.equal(renderGrid({ ...grid, active_screen: "alternate",
    modes: [{ ...grid.modes[0], ansi: true }, grid.modes[1]] }).mouseScroll, false);
  assert.equal(renderGrid({ ...grid, active_screen: "alternate", modes: null }).mouseScroll, false);
});

test("rejects invalid grids and strips embedded terminal controls", () => {
  assert.throws(() => renderGrid({}), /缺少网格/);
  assert.throws(() => renderGrid({ columns: 2000, row_spans: [] }), /尺寸无效/);
  const frame = renderGrid({ columns: 80, rows: 1, row_spans: [
    { row: 0, column: 0, text: "a\x1b[2Jb", cell_width: 2 }
  ] });
  assert.ok(frame.content.includes("a[2Jb"));
  assert.ok(!frame.content.includes("a\x1b[2Jb"));
});

test("clips wide right-edge cells and keeps cursor within the visible screen after scrollback", () => {
  const frame = renderGrid({ columns: 4, rows: 2,
    row_spans: [{ row: 0, column: 3, text: "你好", cell_width: 4 },
      { row: 1, column: 0, text: "x", cell_width: 1 }],
    scrollback_spans: [{ row: 0, column: 0, text: "old" }, { row: 1, column: 0, text: "older" }],
    cursor: { row: 0, column: 0 }
  });
  assert.ok(!frame.content.includes("你好"), "a wide character cannot occupy one remaining cell");
  assert.match(frame.content, /\x1b\[1A\x1b\[1G/);
  assert.ok(!frame.content.includes("\x1b[3;1H"), "absolute position includes scrollback incorrectly");
});
