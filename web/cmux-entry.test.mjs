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
  assert.equal(list.children[0].children[1].children[3].textContent,
    "客户端在线 · 未提供终端模式");
  assert.equal(list.children[1].children.length, 4);
  assert.equal(list.children[1].children[1].children[3].textContent, "终端模式入口可用");
  assert.equal(list.children[1].children[3].textContent, "终端模式");
  list.children[0].handlers.click();
  let stopped = false;
  list.children[1].children[3].handlers.click({ stopPropagation: () => { stopped = true; } });
  assert.deepEqual(actions, [["attach", hostile], ["cmux", 1]]);
  assert.equal(stopped, true);
  context.showClients([]);
  assert.match(list.innerHTML, /客户端未连接，终端模式暂不可用/);
});

test("client list refreshes while visible and stops after leaving the view", () => {
  const html = readFileSync(new URL("./index.html", import.meta.url), "utf8");
  const begin = html.indexOf("function startPollingForClient() {");
  const end = html.indexOf("function showClients(clients) {", begin);
  const actions = [];
  const view = { classList: { contains: () => true } };
  const context = {
    pollTimer: null, waitingForClient: null, ws: { readyState: 1 },
    WebSocket: { OPEN: 1 },
    document: { getElementById: () => view },
    setInterval: fn => { context.tick = fn; return 42; },
    clearInterval: id => actions.push(["clear", id]),
    sendMessage: msg => actions.push(["send", msg.type])
  };
  vm.runInNewContext(`${html.slice(begin, end)}; this.start = startPollingForClient;`, context);
  context.start();
  context.start();
  context.tick();
  assert.deepEqual(actions, [["send", "list"]]);
  view.classList.contains = () => false;
  context.tick();
  assert.deepEqual(actions, [["send", "list"], ["clear", 42]]);
});

test("cmux page title and header use terminal mode name", () => {
  const html = readFileSync(new URL("./cmux.html", import.meta.url), "utf8");
  assert.match(html, /<title>Claude Forward · 终端模式<\/title>/);
  assert.match(html, /<strong>Claude Forward · 终端模式<\/strong>/);
  assert.match(html, /id="statusIndicator".*data-state="connecting"/);
  assert.match(html, /rel="icon".*href="favicon\.svg"/);
  assert.doesNotMatch(html, /云桥终端/);
});

test("滚动 ACK 只在队列空闲后取一次最终画面，切换终端后不补旧快照", async () => {
  const source = readFileSync(new URL("./cmux.mjs", import.meta.url), "utf8");
  const begin = source.indexOf("const scrollController = mountCmuxScroll({");
  const end = source.indexOf("const fileView = mountCmuxFiles(", begin);
  assert.ok(begin > 0 && end > begin);
  let handlers;
  let pending = false;
  const snapshots = [];
  const context = {
    terminalElement: {}, terminal: {}, terminalPane: { hidden: false },
    selectedSurface: "surface", selectedWorkspace: "workspace", activeClient: "client",
    inputOwned: true, lastGridFrame: { mouseScroll: true }, sessionEpoch: 1,
    remoteNeedsSnapshot: null,
    frameOrder: { issue: () => 1 },
    mountCmuxScroll: options => {
      handlers = options;
      return { hasPendingRemote: () => pending };
    },
    request: async () => ({ scrolled: true }),
    refreshSnapshot: urgent => { snapshots.push(urgent); },
    applySnapshot: () => { throw new Error("ACK 不应当作快照"); },
    showStatus: () => {}
  };
  vm.runInNewContext(source.slice(begin, end), context);
  pending = true;
  await handlers.sendRemoteScroll(-4);
  await handlers.sendRemoteScroll(-6);
  assert.equal(snapshots.length, 0);
  pending = false;
  handlers.onRemoteIdle();
  assert.deepEqual(snapshots, [true]);
  handlers.onRemoteIdle();
  assert.equal(snapshots.length, 1);
  await handlers.sendRemoteScroll(-2);
  context.selectedSurface = "other";
  handlers.onRemoteIdle();
  assert.equal(snapshots.length, 1);
});
