import assert from "node:assert/strict";
import test from "node:test";
import { createScrollPositions } from "./cmux-scroll-position.mjs";

function view() {
  const buffer = { baseY: 100, viewportY: 100 };
  const terminal = {
    buffer: { active: buffer },
    scrollToLine(line) { buffer.viewportY = Math.min(buffer.baseY, Math.max(0, line)); }
  };
  return { terminal, buffer, element: { scrollTop: 370, scrollHeight: 850, clientHeight: 480 } };
}

test("工作区和标签分别记住全屏本地阅读位置，不复用其他 Client", () => {
  const positions = createScrollPositions();
  const { terminal, element } = view();
  element.scrollTop = 120;
  positions.save("client", "workspace1", "tab1", true, element, terminal);
  element.scrollTop = 240;
  positions.save("client", "workspace2", "tab1", true, element, terminal);
  element.scrollTop = 0;
  assert.equal(positions.restore("client", "workspace1", "tab1", true, element, terminal), true);
  assert.equal(element.scrollTop, 120);
  assert.equal(positions.restore("client", "workspace1", "tab2", true, element, terminal), false);
  assert.equal(positions.restore("other", "workspace1", "tab1", true, element, terminal), false);
  assert.equal(positions.restore("client", "workspace2", "tab1", true, element, terminal), true);
  assert.equal(element.scrollTop, 240);
});

test("普通屏保留距底部行数，历史缩短时自动钳制；屏幕模式变化不误恢复", () => {
  const positions = createScrollPositions();
  const { terminal, buffer, element } = view();
  buffer.viewportY = 70;
  positions.save("client", "workspace", "tab", false, element, terminal);
  buffer.baseY = 120;
  buffer.viewportY = 120;
  assert.equal(positions.restore("client", "workspace", "tab", false, element, terminal), true);
  assert.equal(buffer.viewportY, 90);
  buffer.baseY = 15;
  assert.equal(positions.restore("client", "workspace", "tab", false, element, terminal), true);
  assert.equal(buffer.viewportY, 0);
  assert.equal(positions.restore("client", "workspace", "tab", true, element, terminal), false);
  positions.save("", "workspace", "tab", false, element, terminal);
  assert.equal(positions.restore("", "workspace", "tab", false, element, terminal), false);
});
