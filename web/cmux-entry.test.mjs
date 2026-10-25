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

test("terminal entry and dependencies use a fresh resource version", () => {
  const index = readFileSync(new URL("./index.html", import.meta.url), "utf8");
  const html = readFileSync(new URL("./cmux.html", import.meta.url), "utf8");
  const version = "20261025-return-bottom-down";
  assert.match(index, new RegExp(`location\\.assign\\('cmux\\.html\\?v=${version}'\\)`));
  for (const asset of ["cmux.mjs", "cmux.css", "lib/xterm.min.js",
    "lib/xterm-addon-fit.min.js", "lib/xterm.css"]) {
    assert.ok(html.includes(`${asset}?v=${version}`), `missing version on ${asset}`);
  }
  const pending = ["cmux.mjs"];
  const visited = new Set();
  while (pending.length) {
    const file = pending.pop();
    if (visited.has(file)) continue;
    visited.add(file);
    const moduleURL = new URL(`./${file}`, import.meta.url);
    const source = readFileSync(moduleURL, "utf8");
    for (const [, importPath] of source.matchAll(/from "(\.\/[^"]+)"/g)) {
      assert.ok(importPath.endsWith(`?v=${version}`),
        `missing version in ${file}: ${importPath}`);
      pending.push(new URL(importPath.split("?")[0], moduleURL).pathname
        .split("/web/")[1]);
    }
  }
  assert.ok(visited.has("cmux-preview.mjs"), "must validate transitive imports");
  assert.match(readFileSync(new URL("./cmux.mjs", import.meta.url), "utf8"),
    /document\.documentElement\.dataset\.cmuxStarted = "1"/);
});

test("module startup watchdog distinguishes script failure and slow connection", () => {
  const html = readFileSync(new URL("./cmux.html", import.meta.url), "utf8");
  const source = html.match(/<script>\s*([\s\S]*?)\s*<\/script>/)?.[1];
  assert.ok(source);
  for (const [started, expected] of [
    [false, "终端脚本未启动"],
    [true, "终端仍在连接"]
  ]) {
    const attributes = {};
    const button = {
      classList: { add: name => { attributes.className = name; } },
      setAttribute: (name, value) => { attributes[name] = value; }
    };
    const indicator = { dataset: { state: "connecting" } };
    const status = { textContent: "" };
    const context = {
      document: {
        documentElement: { dataset: { cmuxStarted: started ? "1" : "" } },
        getElementById: id => ({
          statusIndicator: indicator, connectionStatusButton: button, status
        })[id]
      },
      window: { setInterval: callback => { context.tick = callback; } }
    };
    vm.runInNewContext(source, context);
    context.tick();
    assert.equal(indicator.dataset.state, "unavailable");
    assert.match(status.textContent, new RegExp(expected));
    assert.equal(attributes["aria-label"], status.textContent);
    assert.equal(attributes.className, "expanded");
  }
  const indicator = { dataset: { state: "ready" } };
  const button = { setAttribute: () => {}, classList: { add: () => {} } };
  const status = { textContent: "" };
  const context = {
    document: {
      documentElement: { dataset: {} },
      getElementById: id => ({
        statusIndicator: indicator, connectionStatusButton: button, status
      })[id]
    },
    window: { setInterval: callback => { context.tick = callback; } }
  };
  vm.runInNewContext(source, context);
  context.tick();
  assert.equal(indicator.dataset.state, "ready", "watchdog must not overwrite a healthy connection");
  indicator.dataset.state = "connecting";
  context.tick();
  assert.equal(indicator.dataset.state, "unavailable",
    "watchdog must also diagnose a later stalled reconnection");
});

test("快捷菜单提供模型命令但不自动提交，按终端类型标注专属命令", () => {
  const html = readFileSync(new URL("./cmux.html", import.meta.url), "utf8");
  const source = readFileSync(new URL("./cmux.mjs", import.meta.url), "utf8");
  assert.match(html, /id="commandMenu"/);
  for (const command of ["/model", "/status", "/compact", "/review", "/permissions", "/clear"]) {
    assert.match(html, new RegExp(`data-command="${command}"`));
  }
  assert.match(html, /选中后填入草稿，不会自动发送/);
  assert.match(source, /const next = commandDraft\(prompt\.value, button\.dataset\.command\)/);
  assert.match(source, /prompt\.value = next;/);
  assert.doesNotMatch(source.slice(source.indexOf('commandMenu.querySelectorAll("[data-command]")'),
    source.indexOf('prompt.addEventListener("keydown"')), /requestSubmit|sendReturn/);
});

test("终端按键区提供一键回到底部，远端跳底请求向下且保持在协议上限内", () => {
  const html = readFileSync(new URL("./cmux.html", import.meta.url), "utf8");
  const source = readFileSync(new URL("./cmux.mjs", import.meta.url), "utf8");
  const button = html.match(/<button id="returnToBottomButton"[^>]*>/)?.[0];
  assert.ok(button);
  assert.match(button, /aria-label="回到底部"/);
  assert.doesNotMatch(button, /data-key=/);
  assert.match(source, /returnToBottomButton\.addEventListener\("click"/);
  assert.match(source, /scrollController\.goToBottom\(\)/);
  assert.match(source, /sendRemoteScrollRequest\(remoteBottomDeltaLines\)/);
  assert.match(source, /pendingPosition = false/);
  const delta = Number(source.match(/const remoteBottomDeltaLines = (-?\d+)/)?.[1]);
  assert.equal(delta, -1000);
});

test("滚动中途显示回放画面，空闲时补最终画面，切换终端不补旧快照", async () => {
  const source = readFileSync(new URL("./cmux.mjs", import.meta.url), "utf8");
  const begin = source.indexOf("async function sendRemoteScrollRequest");
  const end = source.indexOf("const fileView = mountCmuxFiles(", begin);
  assert.ok(begin > 0 && end > begin);
  let handlers;
  let pending = false;
  const snapshots = [];
  const frames = [];
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
    applySnapshot: result => { frames.push(result.render_grid); },
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
  context.request = async () => ({ render_grid: { rows: 20 } });
  pending = true;
  await handlers.sendRemoteScroll(-2);
  assert.equal(frames.length, 1, "排队期间也应显示已收到的中途画面");
  pending = false;
  handlers.onRemoteIdle();
  assert.deepEqual(snapshots, [true, true], "中途画面之后仍补读最终状态");
  context.request = async () => ({ scrolled: true });
  await handlers.sendRemoteScroll(-2);
  context.selectedSurface = "other";
  handlers.onRemoteIdle();
  assert.equal(snapshots.length, 2);
});

test("浏览器认证迁移长期 Token 且不再把 Token 放进 WebSocket URL", () => {
  const index = readFileSync(new URL("./index.html", import.meta.url), "utf8");
  const cmux = readFileSync(new URL("./cmux.mjs", import.meta.url), "utf8");
  const download = readFileSync(new URL("./cmux-download-http.mjs", import.meta.url), "utf8");
  assert.match(index, /\/api\/session\/login/);
  assert.match(index, /\/api\/session\/status/);
  assert.match(index, /\/api\/session\/logout/);
  assert.match(index, /localStorage\.removeItem\('cf_token'\)/);
  assert.match(index, /history\.replaceState/);
  assert.doesNotMatch(index, /localStorage\.setItem\('cf_token'/);
  assert.doesNotMatch(index, /\/ws\?token=/);
  assert.doesNotMatch(cmux, /cf_token|\/ws\?token=/);
  assert.match(download, /credentials: "same-origin"/);
  assert.doesNotMatch(download, /Authorization|cf_token/);
});

test("页面内联脚本保持语法有效", () => {
  for (const file of ["index.html", "cmux.html"]) {
    const html = readFileSync(new URL(`./${file}`, import.meta.url), "utf8");
    for (const match of html.matchAll(/<script(?![^>]*\bsrc=)[^>]*>([\s\S]*?)<\/script>/g)) {
      assert.doesNotThrow(() => new Function(match[1]), `invalid inline script in ${file}`);
    }
  }
});
