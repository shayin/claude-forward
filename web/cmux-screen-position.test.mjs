import assert from "node:assert/strict";
import test from "node:test";
import { shouldFollowLatest, showLatestScreen } from "./cmux-screen-position.mjs";

test("全屏网格比手机高时首次打开直接定位最新内容", () => {
  const element = { scrollTop: 0, scrollHeight: 850, clientHeight: 480 };
  showLatestScreen(element);
  assert.equal(element.scrollTop, 370);
  assert.equal(shouldFollowLatest(element, true, false), true);
});

test("手动阅读旧画面时不追随新快照，普通屏不使用容器滚动", () => {
  const element = { scrollTop: 50, scrollHeight: 850, clientHeight: 480 };
  assert.equal(shouldFollowLatest(element, true, true), false);
  assert.equal(shouldFollowLatest(element, true, false), false);
  assert.equal(shouldFollowLatest(element, false, false), false);
  showLatestScreen({ scrollTop: 0, scrollHeight: 300, clientHeight: 480 });
});
