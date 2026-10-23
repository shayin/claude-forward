import assert from "node:assert/strict";
import test from "node:test";
import { readFileSync } from "node:fs";
import vm from "node:vm";
import { setConnectionIndicator } from "./cmux-status.mjs";

test("connection indicator distinguishes ready, connecting, and unavailable", () => {
  const indicator = { dataset: {}, title: "" };
  setConnectionIndicator(indicator, "connecting");
  assert.equal(indicator.dataset.state, "connecting");
  setConnectionIndicator(indicator, "ready");
  assert.equal(indicator.title, "终端模式可用");
  setConnectionIndicator(indicator, "unavailable");
  assert.equal(indicator.dataset.state, "unavailable");
  assert.throws(() => setConnectionIndicator(indicator, "unknown"), RangeError);
  assert.equal(indicator.dataset.state, "unavailable");
});

test("discovery waits for the selected Client instead of switching to another machine", () => {
  const source = readFileSync(new URL("./cmux.mjs", import.meta.url), "utf8");
  const start = source.indexOf("function renderClients(clientList) {");
  const end = source.indexOf("function attach() {", start);
  assert.ok(start >= 0 && end > start);
  const state = [];
  const select = { options: [], replaceChildren() { this.options = []; },
    add(option) { this.options.push(option); }, value: "" };
  const context = {
    availableClients: [], clientsSelect: select, selectedClient: "mind",
    selectedClientPath: "", cmuxClients: clients => clients,
    Option: function(label, value) { this.label = label; this.value = value; },
    connectionState: (...args) => state.push(args),
    attach: () => state.push(["attach"])
  };
  vm.runInNewContext(`${source.slice(start, end)}; this.renderClients = renderClients;`, context);
  context.renderClients([{ id: "other", name: "其他电脑" }]);
  assert.deepEqual(state, [["unavailable", "所选 Client 已离线，等待重新连接"]]);
  assert.equal(context.selectedClient, "mind");
  context.renderClients([{ id: "mind", path: "/project" }]);
  assert.equal(context.selectedClientPath, "/project");
  assert.deepEqual(state.at(-1), ["attach"]);
});
