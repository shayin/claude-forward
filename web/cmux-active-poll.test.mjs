import assert from "node:assert/strict";
import test from "node:test";
import { createCmuxActivePoll } from "./cmux-active-poll.mjs";

test("滑动期间快速刷新，连续活动延长窗口，空闲后停止", () => {
  let time = 0, next = 0, count = 0;
  const timers = new Map();
  const poll = createCmuxActivePoll(() => count++, {
    now: () => time,
    schedule: (callback, delay) => { timers.set(++next, { callback, at: time + delay }); return next; },
    cancel: id => timers.delete(id),
    interval: 100, tail: 250
  });
  const advance = to => {
    while ([...timers.values()].some(item => item.at <= to)) {
      const [id, item] = [...timers].sort((a, b) => a[1].at - b[1].at)[0];
      time = item.at;
      timers.delete(id);
      item.callback();
    }
    time = to;
  };
  poll.activate();
  advance(100);
  assert.equal(count, 1);
  poll.activate();
  assert.equal(timers.size, 1);
  advance(300);
  assert.equal(count, 3);
  advance(500);
  assert.equal(count, 3);
  assert.equal(timers.size, 0);
  poll.activate();
  poll.stop();
  advance(1000);
  assert.equal(count, 3);
});
