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
  let remoteEnabled = true;
  let remoteTouchEnabled = false;
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
  let scrollTop = 370;
  const element = {
    scrollHeight: 850, clientHeight: 480,
    get scrollTop() { return scrollTop; },
    set scrollTop(value) {
      scrollTop = Math.max(0, Math.min(this.scrollHeight - this.clientHeight, value));
    },
    addEventListener(name, handler) { listeners.set(name, handler); },
    querySelector() { return { getBoundingClientRect: () => ({ height: 360 }) }; }
  };
  const controller = mountCmuxScroll({
    element, terminal, isSelected: () => selected,
    canRemoteScroll: () => writable && remoteEnabled,
    remoteTouchEnabled: () => remoteTouchEnabled,
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
    controller, buffer, terminal, element, remote, errors, listeners, touch, flush,
    setTime(value) { clock = value; },
    setWritable(value) { writable = value; },
    setRemoteEnabled(value) {
      remoteEnabled = value;
      remoteTouchEnabled = value;
      if (!value) controller.cancelRemote();
    },
    setRemoteTouchEnabled(value) { remoteTouchEnabled = value; },
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

test("全屏手机只滚动已收到的画面，失去输入权仍可阅读", () => {
  const f = fixture();
  f.controller.setAlternateScreen(true);
  f.touch("touchstart", 100);
  f.setTime(2000);
  assert.equal(f.touch("touchmove", 220).prevented, true);
  f.flush();
  assert.equal(f.element.scrollTop, 250);
  f.touch("touchend", 220);
  assert.equal(f.controller.holdFrame(), true);
  assert.deepEqual(f.remote, []);
  f.setWritable(true);
  f.touch("touchstart", 220);
  f.setTime(4000);
  f.touch("touchmove", 50);
  f.flush();
  f.touch("touchend", 50);
  assert.equal(f.element.scrollTop, 370);
  assert.equal(f.controller.holdFrame(), false);
  assert.equal(f.resumed, 1);
  assert.deepEqual(f.remote, []);
});

test("普通屏滚轮交给 xterm，全屏已有画面在本机滚动", async () => {
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
  assert.equal(wheel(-54), true);
  assert.equal(f.element.scrollTop, 316);
  await Promise.resolve();
  assert.deepEqual(f.remote, []);
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

test("无本地溢出时保留桌面滚轮远端队列，切换终端丢弃旧队列", async () => {
  let resolve;
  const f = fixture(() => new Promise(done => { resolve = done; }));
  f.controller.setAlternateScreen(true);
  f.setWritable(true);
  f.element.scrollHeight = f.element.clientHeight;
  const wheel = deltaY => f.listeners.get("wheel")({
    deltaY, deltaMode: 0, altKey: false, ctrlKey: false, metaKey: false,
    preventDefault() {}, stopImmediatePropagation() {}
  });
  wheel(-180);
  wheel(-70);
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

test("桌面滚轮失去输入权后不补发旧位移，手机触摸仍可本地查看", async () => {
  let resolve;
  const f = fixture(() => new Promise(done => { resolve = done; }));
  f.controller.setAlternateScreen(true);
  f.setWritable(true);
  f.element.scrollHeight = f.element.clientHeight;
  const wheel = deltaY => f.listeners.get("wheel")({
    deltaY, deltaMode: 0, altKey: false, ctrlKey: false, metaKey: false,
    preventDefault() {}, stopImmediatePropagation() {}
  });
  wheel(-180);
  wheel(-70);
  assert.deepEqual(f.remote, [-8]);
  f.setWritable(false);
  f.controller.cancelRemote();
  wheel(-70);
  f.element.scrollHeight = 850;
  f.element.scrollTop = 370;
  f.touch("touchstart", 100);
  f.setTime(2000);
  assert.equal(f.touch("touchmove", 220).prevented, true);
  f.flush();
  assert.equal(f.element.scrollTop, 250);
  f.touch("touchend", 220);
  f.setWritable(true);
  f.element.scrollHeight = f.element.clientHeight;
  wheel(-18);
  resolve();
  await Promise.resolve();
  await Promise.resolve();
  assert.deepEqual(f.remote, [-8, -1]);
  resolve();
  await Promise.resolve();
});

test("全屏手机的惯性只移动本地画面，再次触摸停止旧惯性", () => {
  const f = fixture();
  f.controller.setAlternateScreen(true);
  f.setWritable(true);
  f.touch("touchstart", 100);
  f.setTime(20);
  f.touch("touchmove", 140);
  f.flush(20);
  f.touch("touchend", 140);
  const before = f.element.scrollTop;
  f.flush(36);
  assert.ok(f.element.scrollTop < before);
  f.touch("touchstart", 140);
  const stopped = f.element.scrollTop;
  f.flush(52);
  assert.equal(f.element.scrollTop, stopped);
  f.setWritable(false);
  assert.deepEqual(f.remote, []);
  assert.equal(f.activities, 0);
});

test("关闭远程滚动后不再发送桌面滚轮位移，也不补发排队位移", async () => {
  let resolve;
  const f = fixture(() => new Promise(done => { resolve = done; }));
  f.controller.setAlternateScreen(true);
  f.setWritable(true);
  f.element.scrollHeight = f.element.clientHeight;
  const wheel = deltaY => f.listeners.get("wheel")({
    deltaY, deltaMode: 0, altKey: false, ctrlKey: false, metaKey: false,
    preventDefault() {}, stopImmediatePropagation() {}
  });
  wheel(-180);
  wheel(-70);
  assert.deepEqual(f.remote, [-8]);
  f.setRemoteEnabled(false);
  wheel(-70);
  resolve();
  await Promise.resolve();
  await Promise.resolve();
  assert.deepEqual(f.remote, [-8]);
  f.element.scrollHeight = 850;
  f.element.scrollTop = 370;
  wheel(-36);
  assert.equal(f.element.scrollTop, 334);
  assert.deepEqual(f.remote, [-8]);
  f.setRemoteEnabled(true);
  f.element.scrollHeight = f.element.clientHeight;
  wheel(-18);
  assert.deepEqual(f.remote, [-8, -1]);
  resolve();
  await Promise.resolve();
});

test("恢复非底部阅读位置后暂停新快照，回到底部才恢复", () => {
  const f = fixture();
  f.controller.setAlternateScreen(true);
  f.element.scrollTop = 180;
  f.controller.restoreBrowsing();
  assert.equal(f.controller.holdFrame(), true);
  f.element.scrollTop = 370;
  f.controller.restoreBrowsing();
  assert.equal(f.controller.holdFrame(), false);
});

test("开关可对比手机全屏远程滚动与本地滚动，远端不改变本地容器位置", async () => {
  const f = fixture();
  f.controller.setAlternateScreen(true);
  f.setWritable(true);
  f.setRemoteTouchEnabled(true);
  f.touch("touchstart", 100);
  f.setTime(2000);
  f.touch("touchmove", 220);
  assert.equal(f.controller.holdFrame(), false);
  assert.equal(f.element.scrollTop, 370);
  assert.deepEqual(f.remote, [-6]);
  f.touch("touchend", 220);
  f.setRemoteTouchEnabled(false);
  f.controller.reset();
  f.controller.setAlternateScreen(true);
  f.touch("touchstart", 100);
  f.setTime(4000);
  f.touch("touchmove", 220);
  f.flush();
  assert.equal(f.element.scrollTop, 250);
  assert.deepEqual(f.remote, [-6]);
  await Promise.resolve();
});
