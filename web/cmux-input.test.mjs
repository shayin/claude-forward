import assert from "node:assert/strict";
import test from "node:test";
import { screenCoordinates, shouldSubmitComposer, submitTerminalDraft, terminalInput, wheelLines } from "./cmux-input.mjs";

test("maps native terminal approval keys without treating escape sequences as shell text", () => {
  assert.deepEqual(terminalInput("\r"), { action: "key", key: "return" });
  assert.deepEqual(terminalInput("\x03"), { action: "key", key: "ctrl+c" });
  assert.deepEqual(terminalInput("\x1b[B"), { action: "key", key: "down" });
  assert.deepEqual(terminalInput("你好"), { action: "text", text: "你好" });
  assert.equal(terminalInput("\x1b[5~"), null);
});

test("Enter submits only after composition; Shift+Enter stays in draft", () => {
  assert.equal(shouldSubmitComposer({ key: "Enter", shiftKey: false }), true);
  assert.equal(shouldSubmitComposer({ key: "Enter", isComposing: true }), false);
  assert.equal(shouldSubmitComposer({ key: "Enter", keyCode: 229 }), false);
  assert.equal(shouldSubmitComposer({ key: "Enter", shiftKey: true }), false);
});

test("composer pastes once, waits for TUI to settle, then submits exactly once", async () => {
  const calls = [];
  await submitTerminalDraft(
    async () => { calls.push("paste"); },
    async () => { calls.push("return"); },
    () => true, async () => { calls.push("settle"); }
  );
  assert.deepEqual(calls, ["paste", "settle", "return"]);
  calls.length = 0;
  await assert.rejects(submitTerminalDraft(async () => calls.push("paste"),
    async () => calls.push("return"), () => false, async () => {}), /未自动提交/);
  assert.deepEqual(calls, ["paste"]);
  await assert.rejects(submitTerminalDraft(async () => { throw Error("paste failed"); },
    async () => calls.push("return"), () => true, async () => {}), /paste failed/);
});

test("maps pointer and scroll to bounded native terminal coordinates", () => {
  const rect = { left: 20, top: 30, right: 820, bottom: 430, width: 800, height: 400 };
  assert.deepEqual(screenCoordinates(20, 30, rect, 80, 20), { column: 0, row: 0 });
  assert.deepEqual(screenCoordinates(819, 429, rect, 80, 20), { column: 79, row: 19 });
  assert.equal(screenCoordinates(820, 30, rect, 80, 20), null);
  assert.equal(wheelLines(36, 0, 18), 2);
  assert.equal(wheelLines(-200, 1), -100);
});
