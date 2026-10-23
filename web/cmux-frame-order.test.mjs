import assert from "node:assert/strict";
import test from "node:test";
import { createCmuxFrameOrder } from "./cmux-frame-order.mjs";

test("滚动后的新帧先到时丢弃迟到的旧轮询帧", () => {
  const frames = createCmuxFrameOrder();
  const oldPoll = frames.issue();
  const scroll = frames.issue();
  assert.equal(frames.accept(scroll), true);
  assert.equal(frames.accept(oldPoll), false);
  assert.equal(frames.accept(frames.issue()), true);
});
