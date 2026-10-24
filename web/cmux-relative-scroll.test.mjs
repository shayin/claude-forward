import assert from "node:assert/strict";
import test from "node:test";
import { readFileSync } from "node:fs";
import { createRelativeScrollHint } from "./cmux-relative-scroll.mjs";

test("只在远程鼠标滚动模式显示本手机净操作量，上下抵消且不显示百分比", () => {
  const element = { hidden: true, textContent: "" };
  const hint = createRelativeScrollHint(element);
  hint.record(-8);
  assert.equal(element.hidden, true);
  hint.setRemoteMode(true);
  assert.equal(element.textContent, "本手机相对滚动：0 行 · 非总进度");
  hint.record(-12);
  assert.equal(element.textContent, "本手机相对滚动：↑ 12 行 · 非总进度");
  hint.setRemoteMode(true);
  hint.record(5);
  assert.equal(element.textContent, "本手机相对滚动：↑ 7 行 · 非总进度");
  hint.record(7);
  assert.equal(element.textContent, "本手机相对滚动：0 行 · 非总进度");
  hint.record(3);
  assert.equal(element.textContent, "本手机相对滚动：↓ 3 行 · 非总进度");
  assert.equal(element.textContent.includes("%"), false);
});

test("模式关闭或切换终端归零，不累计非法位移及失败请求", () => {
  const element = { hidden: true, textContent: "" };
  const hint = createRelativeScrollHint(element);
  hint.setRemoteMode(true);
  hint.record(-4);
  hint.record(Number.NaN);
  hint.record(1.5);
  assert.equal(element.textContent, "本手机相对滚动：↑ 4 行 · 非总进度");
  hint.reset();
  assert.equal(element.hidden, true);
  hint.record(-8);
  hint.setRemoteMode(true);
  assert.equal(element.textContent, "本手机相对滚动：0 行 · 非总进度");
  hint.record(2);
  hint.setRemoteMode(false);
  assert.equal(element.hidden, true);
  hint.setRemoteMode(true);
  assert.equal(element.textContent, "本手机相对滚动：0 行 · 非总进度");
});

test("网页入口明确说明相对指标不是 Codex 真实位置", () => {
  const html = readFileSync(new URL("./cmux.html", import.meta.url), "utf8");
  assert.match(html, /id="terminalScrollHint"[^>]*title="[^"]*不代表 Codex 真实位置[^"]*"/);
});
