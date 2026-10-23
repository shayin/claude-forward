import assert from "node:assert/strict";
import test from "node:test";
import { bindMobileTerminalTextInput } from "./cmux-mobile-input.mjs";

class EventTargetStub {
  listeners = new Map();
  addEventListener(type, handler) {
    const handlers = this.listeners.get(type) || [];
    handlers.push(handler);
    this.listeners.set(type, handlers);
  }
  removeEventListener(type, handler) {
    this.listeners.set(type, (this.listeners.get(type) || []).filter(item => item !== handler));
  }
  dispatch(type, target = this, options = {}) {
    const event = {
      target, inputType: options.inputType, data: options.data,
      isComposing: options.isComposing || false, cancelable: options.cancelable || false,
      defaultPrevented: false, stopped: false,
      preventDefault() { this.defaultPrevented = true; },
      stopImmediatePropagation() { this.stopped = true; }
    };
    for (const handler of this.listeners.get(type) || []) {
      handler(event);
      if (event.stopped) break;
    }
    return event;
  }
}

function createInput() {
  const parent = new EventTargetStub();
  const textarea = new EventTargetStub();
  textarea.parentElement = parent;
  textarea.value = "";
  const sent = [];
  let active = true;
  const dispose = bindMobileTerminalTextInput({
    textarea, isActive: () => active, sendText: text => sent.push(text)
  });
  return { parent, textarea, sent, dispose, setActive(value) { active = value; } };
}

test("mobile input sends committed text once before xterm consumes cancelable input", () => {
  const { parent, textarea, sent, dispose } = createInput();
  const event = parent.dispatch("beforeinput", textarea,
    { inputType: "insertText", data: "1", cancelable: true });
  assert.equal(event.defaultPrevented, true);
  assert.equal(event.stopped, true);
  assert.deepEqual(sent, ["1"]);
  assert.equal(textarea.value, "");
  dispose();
  assert.equal(parent.dispatch("beforeinput", textarea,
    { inputType: "insertText", data: "2", cancelable: true }).defaultPrevented, false);
  assert.deepEqual(sent, ["1"]);
});

test("non-cancelable input-only keyboard restores xterm textarea to avoid duplicate output", () => {
  const { parent, textarea, sent, dispose } = createInput();
  assert.equal(parent.dispatch("beforeinput", textarea,
    { inputType: "insertText", data: "好", cancelable: false }).defaultPrevented, false);
  textarea.value = "好";
  const event = parent.dispatch("input", textarea, { inputType: "insertText", data: "好" });
  assert.equal(event.stopped, true);
  assert.equal(textarea.value, "");
  assert.deepEqual(sent, ["好"]);
  dispose();
});

test("IME composition, inactive terminal, and paste remain on xterm's existing path", async () => {
  const { parent, textarea, sent, dispose, setActive } = createInput();
  textarea.dispatch("compositionstart");
  assert.equal(parent.dispatch("beforeinput", textarea,
    { inputType: "insertText", data: "中", cancelable: true }).defaultPrevented, false);
  assert.equal(parent.dispatch("input", textarea,
    { inputType: "insertText", data: "中", isComposing: true }).stopped, true);
  textarea.dispatch("compositionend");
  assert.equal(parent.dispatch("input", textarea,
    { inputType: "insertText", data: "中文" }).stopped, true);
  assert.deepEqual(sent, []);
  await new Promise(resolve => setTimeout(resolve, 5));
  setActive(false);
  assert.equal(parent.dispatch("beforeinput", textarea,
    { inputType: "insertText", data: "x", cancelable: true }).stopped, false);
  setActive(true);
  assert.equal(parent.dispatch("beforeinput", textarea,
    { inputType: "insertFromPaste", data: "粘贴", cancelable: true }).stopped, false);
  assert.deepEqual(sent, []);
  dispose();
});
