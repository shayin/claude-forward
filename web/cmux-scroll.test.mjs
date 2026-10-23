import assert from "node:assert/strict";
import test from "node:test";
import { mountCmuxScroll } from "./cmux-scroll.mjs";

function fixture(send = () => Promise.resolve()) {
  const listeners = new Map();
  const callbacks = new Map();
  let frameID = 0;
  let clock = 0;
  let selected = true;
  let writable = false;
  const remote = [];
  const errors = [];
  let activities = 0;
  let resumed = 0;
  let scrollListener = () => {};
  const buffer = { baseY: 100, viewportY: 100 };
  const terminal = {
    rows: 20,
    buffer: { active: buffer },
    scrollToLine(line) {
      buffer.viewportY = Math.max(0, Math.min(buffer.baseY, line));
      scrollListener();
    },
    onScroll(fn) { scrollListener = fn; }
  };
  const element = {
    addEventListener(name, handler) { listeners.set(name, handler); },
    querySelector() { return { getBoundingClientRect: () => ({ height: 360 }) }; }
  };
  const controller = mountCmuxScroll({
    element, terminal, isSelected: () => selected, canRemoteScroll: () => writable,
    sendRemoteScroll: lines => { remote.push(lines); return send(lines); },
    onRemoteError: error => errors.push(error),
    onRemoteActivity: () => { activities++; },
    onReturnToBottom: () => { resumed++; },
    raf: callback => { callbacks.set(++frameID, callback); return frameID; },
    cancelRaf: id => callbacks.delete(id),
    now: () => clock
  });
  const touch = (name, y) => {
    let prevented = false, stopped = false;
    const event = {
      touches: name === "touchend" ? [] : [{ clientY: y }],
      cancelable: true,
      preventDefault() { prevented = true; },
      stopImmediatePropagation() { stopped = true; }
    };
    listeners.get(name)(event);
    return { prevented, stopped };
  };
  const flush = (time = clock) => {
    const pending = [...callbacks.values()];
    callbacks.clear();
    for (const callback of pending) callback(time);
  };
  return {
    controller, buffer, terminal, remote, errors, listeners, touch, flush,
    setTime(value) { clock = value; },
    setWritable(value) { writable = value; },
    setSelected(value) { selected = value; },
    get resumed() { return resumed; },
    get activities() { return activities; }
  };
}

test("普通屏只在本机跟手滚动，失去输入权也能查看历史，回底才继续刷新", () => {
  const f = fixture();
  f.touch("touchstart", 120);
  f.setTime(2000);
  const movement = f.touch("touchmove", 300);
  assert.equal(movement.prevented, true);
  assert.equal(movement.stopped, true);
  assert.equal(f.controller.holdFrame(), true);
  f.flush();
  assert.equal(f.buffer.viewportY, 90);
  f.touch("touchend", 300);
  assert.equal(f.controller.isBrowsing(), true);
  assert.equal(f.controller.holdFrame(), true);
  assert.deepEqual(f.remote, []);
  f.touch("touchstart", 300);
  f.setTime(4000);
  f.touch("touchmove", 120);
  f.flush();
  f.touch("touchend", 120);
  assert.equal(f.buffer.viewportY, 100);
  assert.equal(f.controller.holdFrame(), false);
  assert.equal(f.resumed, 1);
});

test("轻点、切换终端、取消手势不会误触发滚动", () => {
  const f = fixture();
  f.touch("touchstart", 100);
  assert.equal(f.touch("touchmove", 102).prevented, false);
  f.touch("touchend", 102);
  assert.equal(f.buffer.viewportY, 100);
  f.setSelected(false);
  f.touch("touchstart", 100);
  f.touch("touchmove", 300);
  f.flush();
  assert.equal(f.buffer.viewportY, 100);
  f.setSelected(true);
  f.touch("touchstart", 120);
  f.touch("touchmove", 300);
  f.controller.reset();
  f.flush();
  assert.equal(f.buffer.viewportY, 100);
  assert.equal(f.controller.holdFrame(), false);
  f.touch("touchstart", 120);
  f.touch("touchmove", 300);
  f.touch("touchcancel", 300);
  f.flush();
  assert.equal(f.controller.holdFrame(), false);
});

test("全屏模式只有持有输入权才合并远程滚动，且不会冻结画面", async () => {
  const f = fixture();
  f.controller.setAlternateScreen(true);
  f.touch("touchstart", 100);
  f.setTime(20);
  assert.equal(f.touch("touchmove", 220).prevented, false);
  f.touch("touchend", 220);
  assert.deepEqual(f.remote, []);
  f.setWritable(true);
  f.touch("touchstart", 100);
  f.setTime(40);
  f.touch("touchmove", 220);
  f.touch("touchend", 220);
  await Promise.resolve();
  assert.deepEqual(f.remote, [-6]);
  assert.equal(f.controller.holdFrame(), false);
  assert.equal(f.buffer.viewportY, 100);
});

test("正常滚轮交给 xterm，只有全屏模式才拦截并送远程", async () => {
  const f = fixture();
  const wheel = deltaY => {
    let prevented = false;
    f.listeners.get("wheel")({
      deltaY, deltaMode: 0, altKey: false, ctrlKey: false, metaKey: false,
      preventDefault() { prevented = true; }, stopImmediatePropagation() {}
    });
    return prevented;
  };
  assert.equal(wheel(-36), false);
  f.terminal.scrollToLine(98);
  f.flush();
  assert.equal(f.controller.isBrowsing(), true);
  f.terminal.scrollToLine(100);
  assert.equal(f.resumed, 1);
  f.controller.setAlternateScreen(true);
  f.setWritable(true);
  assert.equal(wheel(54), true);
  await Promise.resolve();
  assert.deepEqual(f.remote, [3]);
});

test("松手后本地惯性继续滚动，再次触摸能取消旧手势", () => {
  const f = fixture();
  f.touch("touchstart", 120);
  f.setTime(16);
  f.touch("touchmove", 300);
  f.flush(16);
  const atRelease = f.buffer.viewportY;
  f.touch("touchend", 300);
  f.flush(32);
  assert.ok(f.buffer.viewportY < atRelease);
  f.touch("touchstart", 300);
  const stopped = f.buffer.viewportY;
  f.flush(48);
  assert.equal(f.buffer.viewportY, stopped);
});

test("远程 RPC 未完成时合并后续位移，切换终端丢弃旧队列", async () => {
  let resolve;
  const f = fixture(() => new Promise(done => { resolve = done; }));
  f.controller.setAlternateScreen(true);
  f.setWritable(true);
  f.touch("touchstart", 100);
  f.setTime(20);
  f.touch("touchmove", 280);
  f.setTime(40);
  f.touch("touchmove", 350);
  assert.deepEqual(f.remote, [-8]);
  resolve();
  await Promise.resolve();
  await Promise.resolve();
  assert.deepEqual(f.remote, [-8, -5]);
  f.controller.reset();
  resolve();
  await Promise.resolve();
  assert.deepEqual(f.remote, [-8, -5]);
});

test("输入权丢失后不补发旧位移，恢复后只处理新手势", async () => {
  let resolve;
  const f = fixture(() => new Promise(done => { resolve = done; }));
  f.controller.setAlternateScreen(true);
  f.setWritable(true);
  f.touch("touchstart", 100);
  f.setTime(20);
  f.touch("touchmove", 280);
  f.setTime(40);
  f.touch("touchmove", 350);
  assert.deepEqual(f.remote, [-8]);
  f.setWritable(false);
  f.controller.cancelRemote();
  assert.equal(f.touch("touchmove", 370).prevented, false);
  f.setWritable(true);
  f.touch("touchstart", 100);
  f.setTime(60);
  f.touch("touchmove", 118);
  resolve();
  await Promise.resolve();
  await Promise.resolve();
  assert.deepEqual(f.remote, [-8, -1]);
  resolve();
  await Promise.resolve();
});

test("全屏松手后短暂惯性合并远程位移，再次触摸或失权即停止", async () => {
  const f = fixture();
  f.controller.setAlternateScreen(true);
  f.setWritable(true);
  f.touch("touchstart", 100);
  f.setTime(20);
  f.touch("touchmove", 140);
  f.touch("touchend", 140);
  const before = f.activities;
  f.flush(36);
  assert.ok(f.activities > before);
  f.touch("touchstart", 140);
  const stopped = f.activities;
  f.flush(52);
  assert.equal(f.activities, stopped);
  f.setTime(60);
  f.touch("touchmove", 180);
  f.touch("touchend", 180);
  f.setWritable(false);
  f.flush(76);
  assert.equal(f.activities, stopped + 1);
  await Promise.resolve();
});
