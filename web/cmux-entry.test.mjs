import assert from "node:assert/strict";
import test from "node:test";
import { readFileSync } from "node:fs";
import vm from "node:vm";

class Element {
  constructor(tag) { this.tag = tag; this.children = []; this.handlers = {}; this.textContent = ""; }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this.children = children; }
  addEventListener(type, handler) { this.handlers[type] = handler; }
}

test("legacy client selection and independent cmux entry coexist without interpreting client HTML", () => {
  const html = readFileSync(new URL("./index.html", import.meta.url), "utf8");
  const begin = html.indexOf("function showClients(clients) {");
  const end = html.indexOf("function createMessageEl", begin);
  assert.ok(begin > 0 && end > begin);
  const list = new Element("div");
  const actions = [];
  const context = {
    availableClients: [], waitingForClient: "", attachedClient: "",
    document: { getElementById: () => list, createElement: tag => new Element(tag) },
    attach: id => actions.push(["attach", id]),
    openCmuxPage: index => actions.push(["cmux", index])
  };
  vm.runInNewContext(`${html.slice(begin, end)}; this.showClients = showClients;`, context);
  const hostile = `<img src=x onerror=alert(1)>`;
  context.showClients([
    { id: hostile, name: hostile, path: hostile, pid: 123 },
    { id: "new", name: "新版", path: "/home", features: ["cmux"] }
  ]);
  assert.equal(list.children.length, 2);
  assert.equal(list.children[0].children[1].children[0].textContent, hostile);
  assert.equal(list.children[0].children.length, 3); // 旧 Client 不显示新入口
  assert.equal(list.children[1].children.length, 4);
  list.children[0].handlers.click();
  let stopped = false;
  list.children[1].children[3].handlers.click({ stopPropagation: () => { stopped = true; } });
  assert.deepEqual(actions, [["attach", hostile], ["cmux", 1]]);
  assert.equal(stopped, true);
});
