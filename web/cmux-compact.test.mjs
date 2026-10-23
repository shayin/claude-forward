import assert from "node:assert/strict";
import test from "node:test";
import { mountCmuxCompact } from "./cmux-compact.mjs";

class FakeElement {
  constructor() {
    this.listeners = new Map();
    this.classes = new Set();
    this.classList = {
      contains: name => this.classes.has(name),
      add: name => this.classes.add(name),
      remove: name => this.classes.delete(name),
      toggle: name => this.classes.has(name) ? (this.classes.delete(name), false) :
        (this.classes.add(name), true)
    };
    this.attributes = new Map();
    this.style = {};
    this.open = false;
  }
  addEventListener(type, handler) { this.listeners.set(type, handler); }
  dispatch(type, event = {}) { this.listeners.get(type)?.(event); }
  click() { this.dispatch("click"); }
  setAttribute(name, value) { this.attributes.set(name, value); }
  getAttribute(name) { return this.attributes.get(name); }
  contains(target) { return this === target; }
  querySelectorAll() { return this.keys || []; }
}

function setup() {
  const elements = new Map(["mobileFiles", "mobileBrowser", "mobileRefresh"]
    .map(id => [id, new FakeElement()]));
  const document = new FakeElement();
  document.getElementById = id => elements.get(id);
  globalThis.document = document;
  const media = new FakeElement();
  media.matches = true;
  const statusButton = new FakeElement();
  const statusElement = new FakeElement();
  const mobileTools = new FakeElement();
  const extraKeys = new FakeElement();
  extraKeys.keys = [new FakeElement()];
  const attachmentToggle = new FakeElement();
  const terminalPane = new FakeElement();
  const prompt = new FakeElement();
  prompt.scrollHeight = 44;
  const filesButton = new FakeElement();
  const browserButton = new FakeElement();
  const refreshButton = new FakeElement();
  const calls = [];
  for (const [button, name] of [
    [filesButton, "files"], [browserButton, "browser"], [refreshButton, "refresh"]
  ]) button.addEventListener("click", () => calls.push(name));
  const compact = mountCmuxCompact({
    media, statusButton, mobileTools, extraKeys, attachmentToggle, terminalPane,
    prompt, statusElement, filesButton, browserButton, refreshButton, statusDurationMs: 5
  });
  return {
    document, elements, media, statusButton, statusElement, mobileTools, extraKeys,
    attachmentToggle, terminalPane, prompt, calls, compact
  };
}

test("mobile actions reuse the original file, preview, and refresh handlers", () => {
  const ui = setup();
  for (const [id, name] of [
    ["mobileFiles", "files"], ["mobileBrowser", "browser"], ["mobileRefresh", "refresh"]
  ]) {
    ui.mobileTools.open = true;
    ui.elements.get(id).click();
    assert.equal(ui.mobileTools.open, false);
    assert.equal(ui.calls.at(-1), name);
  }
  assert.equal(ui.calls.length, 3);
});

test("attachment panel stays closed until requested and can be closed again", () => {
  const ui = setup();
  assert.equal(ui.terminalPane.classList.contains("attachments-open"), false);
  ui.attachmentToggle.click();
  assert.equal(ui.terminalPane.classList.contains("attachments-open"), true);
  assert.equal(ui.attachmentToggle.getAttribute("aria-expanded"), "true");
  ui.compact.setAttachmentOpen(false);
  assert.equal(ui.attachmentToggle.getAttribute("aria-expanded"), "false");
  assert.equal(ui.terminalPane.classList.contains("attachments-open"), false);
});

test("status and extra keys collapse on outside tap, Escape, or desktop switch", () => {
  const ui = setup();
  ui.statusButton.click();
  assert.equal(ui.statusButton.classList.contains("expanded"), true);
  ui.mobileTools.open = true;
  ui.extraKeys.open = true;
  ui.document.dispatch("pointerdown", { target: new FakeElement() });
  assert.equal(ui.statusButton.classList.contains("expanded"), false);
  assert.equal(ui.mobileTools.open, false);
  assert.equal(ui.extraKeys.open, false);
  ui.extraKeys.open = true;
  ui.extraKeys.keys[0].click();
  assert.equal(ui.extraKeys.open, false);
  ui.mobileTools.open = true;
  ui.document.dispatch("keydown", { key: "Escape" });
  assert.equal(ui.mobileTools.open, false);
  ui.statusButton.click();
  ui.media.matches = false;
  ui.media.dispatch("change");
  assert.equal(ui.statusButton.classList.contains("expanded"), false);
  ui.statusButton.click();
  assert.equal(ui.statusButton.classList.contains("expanded"), false);
});

test("composer starts at one line, grows to a cap, and resets on desktop", () => {
  const ui = setup();
  assert.equal(ui.prompt.style.height, "44px");
  ui.prompt.scrollHeight = 87;
  ui.compact.syncPromptHeight();
  assert.equal(ui.prompt.style.height, "87px");
  ui.prompt.scrollHeight = 500;
  ui.compact.syncPromptHeight();
  assert.equal(ui.prompt.style.height, "112px");
  ui.media.matches = false;
  ui.media.dispatch("change");
  assert.equal(ui.prompt.style.height, "");
});

test("operation feedback is visible briefly without permanently occupying terminal space", async () => {
  const ui = setup();
  ui.compact.showStatus("附件上传失败");
  assert.equal(ui.statusElement.textContent, "附件上传失败");
  assert.equal(ui.statusButton.getAttribute("aria-label"), "附件上传失败");
  assert.equal(ui.statusButton.classList.contains("expanded"), true);
  await new Promise(resolve => setTimeout(resolve, 15));
  assert.equal(ui.statusButton.classList.contains("expanded"), false);
  ui.compact.showStatus("终端模式已连接", false);
  assert.equal(ui.statusButton.classList.contains("expanded"), false);
  ui.compact.showStatus("再次失败");
  ui.statusButton.click(); // 用户主动关闭时不应再次弹出
  assert.equal(ui.statusButton.classList.contains("expanded"), false);
  await new Promise(resolve => setTimeout(resolve, 15));
  assert.equal(ui.statusButton.classList.contains("expanded"), false);
});
