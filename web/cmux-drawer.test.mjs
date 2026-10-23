import assert from "node:assert/strict";
import test from "node:test";
import { mountCmuxDrawer } from "./cmux-drawer.mjs";

function fakeElement() {
  const listeners = new Map();
  const attributes = new Map();
  return {
    hidden: false,
    attributes,
    setAttribute(key, value) { attributes.set(key, value); },
    addEventListener(key, callback) { listeners.set(key, callback); },
    click() { listeners.get("click")?.(); },
    change() { listeners.get("change")?.(); }
  };
}

test("mobile workspace drawer opens, closes on backdrop, and restores desktop sidebar", () => {
  const button = fakeElement(), aside = fakeElement(), backdrop = fakeElement();
  const classes = new Set();
  const body = { classList: { toggle(name, active) {
    if (active) classes.add(name);
    else classes.delete(name);
  } } };
  const media = fakeElement();
  media.matches = true;
  const drawer = mountCmuxDrawer({ button, aside, backdrop, body, media });
  assert.equal(button.attributes.get("aria-expanded"), "false");
  assert.equal(aside.attributes.get("aria-hidden"), "true");
  assert.equal(backdrop.hidden, true);
  button.click();
  assert.equal(button.attributes.get("aria-expanded"), "true");
  assert.equal(backdrop.hidden, false);
  assert.ok(classes.has("cmux-drawer-open"));
  backdrop.click();
  assert.equal(backdrop.hidden, true);
  button.click();
  media.matches = false;
  media.change();
  assert.equal(aside.attributes.get("aria-hidden"), "false");
  assert.equal(button.attributes.get("aria-expanded"), "false");
  drawer.close();
});
