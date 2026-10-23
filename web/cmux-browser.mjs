export function previewPorts(result) {
  const ports = Array.isArray(result?.ports) ? result.ports : [];
  return [...new Set(ports.filter(port => Number.isInteger(port) && port > 0 && port <= 65535))].slice(0, 20);
}

export function previewScreenshotURL(result) {
  if (typeof result?.data !== "string" || result.data.length > 12 * 1024 * 1024 ||
      !result.data.startsWith("iVBORw0KGgo") || !/^[A-Za-z0-9+/]*={0,2}$/.test(result.data)) {
    throw new Error("cmux 网页截图格式错误");
  }
  return `data:image/png;base64,${result.data}`;
}

export function previewPointer(event, image) {
  const rect = image.getBoundingClientRect();
  if (!rect.width || !rect.height) return null;
  const x = (event.clientX - rect.left) / rect.width;
  const y = (event.clientY - rect.top) / rect.height;
  return x >= 0 && x <= 1 && y >= 0 && y <= 1 ? { x, y } : null;
}

export async function sendBrowserText(text, operation) {
  if (!text) return false;
  return (await operation("browser_type", { text })) === true;
}

export function mountCmuxBrowser({ request, currentClient, currentWorkspace, canManage, onShow, onHide }) {
  const pane = document.getElementById("browserPane");
  const terminalPane = document.getElementById("terminalPane");
  const filePane = document.getElementById("filePane");
  const status = document.getElementById("browserStatus");
  const portSelect = document.getElementById("browserPort");
  const pathInput = document.getElementById("browserPath");
  const image = document.getElementById("browserImage");
  const textInput = document.getElementById("browserText");
  const auto = document.getElementById("browserAuto");
  const actionButtons = [...pane.querySelectorAll("[data-browser-nav], [data-browser-key]"),
    document.getElementById("browserOpen"), document.getElementById("browserSendText")];
  let version = 0;
  let timer;
  let busy = false;
  let opened = "";
  let writable = false;
  let pointerStart = null;

  function setWritable(owned) {
    writable = owned;
    for (const button of actionButtons) button.disabled = !owned;
    pathInput.disabled = !owned;
    textInput.disabled = !owned;
    portSelect.disabled = !owned;
    document.getElementById("browserRefresh").disabled = !owned;
    if (!owned) {
      clearTimeout(timer);
      opened = "";
      image.removeAttribute("src");
    } else {
      schedule();
    }
  }

  function schedule() {
    clearTimeout(timer);
    if (!pane.hidden && writable && auto.checked && opened && !document.hidden) {
      timer = setTimeout(() => void snapshot(), 1600);
    }
  }

  async function operation(action, details = {}) {
    if (busy) {
      status.textContent = "上一操作尚未完成，请稍后重试";
      return false;
    }
    if (!writable || !currentClient() || !currentWorkspace()) return false;
    const client = currentClient();
    const workspace = currentWorkspace();
    const token = version;
    busy = true;
    status.textContent = `${action === "browser_navigate" ? "正在打开本机网页" : "正在刷新画面"}…`;
    try {
      const result = await request(action, { workspace_id: workspace, ...details });
      if (token !== version || !writable || client !== currentClient() ||
          workspace !== currentWorkspace() || pane.hidden) return false;
      image.src = previewScreenshotURL(result);
      opened = workspace;
      status.textContent = `来自本机 cmux 的网页截图 · ${new Date().toLocaleTimeString()}`;
      return true;
    } catch (error) {
      if (token === version && !pane.hidden) status.textContent = `网页预览失败：${error.message}`;
      return false;
    } finally {
      busy = false;
      schedule();
    }
  }

  async function snapshot() {
    if (!writable || !opened || opened !== currentWorkspace() || busy) {
      schedule();
      return;
    }
    await operation("browser_screenshot");
  }

  async function loadPorts() {
    const client = currentClient(), workspace = currentWorkspace(), token = ++version;
    opened = "";
    image.removeAttribute("src");
    portSelect.replaceChildren();
    clearTimeout(timer);
    if (!client || !workspace) {
      status.textContent = "先选择 cmux 工作区";
      return;
    }
    status.textContent = "查找当前工作区的本机网页服务…";
    try {
      const ports = previewPorts(await request("browser_ports", { workspace_id: workspace }));
      if (token !== version || pane.hidden || client !== currentClient() || workspace !== currentWorkspace()) return;
      for (const port of ports) portSelect.add(new Option(`本机 · ${port}`, String(port)));
      status.textContent = ports.length ? "选择本机端口打开；网页可自行跳转，仅管理设备可见截图" :
        "cmux 尚未在当前工作区侦测到本机网页服务；请先在电脑终端启动开发服务";
    } catch (error) {
      if (token === version && !pane.hidden) status.textContent = `端口读取失败：${error.message}`;
    }
  }

  function hide(backToTerminal = true) {
    version++;
    clearTimeout(timer);
    pane.hidden = true;
    opened = "";
    image.removeAttribute("src");
    if (backToTerminal && filePane.hidden) {
      terminalPane.hidden = false;
      onHide();
    }
  }

  function reset() {
    hide();
    portSelect.replaceChildren();
    pathInput.value = "/";
    textInput.value = "";
  }

  document.getElementById("browserToggleButton").addEventListener("click", () => {
    if (!currentClient()) {
      status.textContent = "请先连接 cmux Client";
      return;
    }
    onShow();
    filePane.hidden = true;
    terminalPane.hidden = true;
    pane.hidden = false;
    void loadPorts();
  });
  document.getElementById("browserBackToTerminal").addEventListener("click", hide);
  document.getElementById("browserOpen").addEventListener("click", () => {
    if (!writable || !portSelect.value) return;
    void operation("browser_navigate", { port: Number(portSelect.value), browser_path: pathInput.value || "/" });
  });
  document.getElementById("browserRefresh").addEventListener("click", () => void snapshot());
  for (const button of pane.querySelectorAll("[data-browser-nav]")) {
    button.addEventListener("click", () => {
      if (writable && opened === currentWorkspace()) void operation("browser_nav", { key: button.dataset.browserNav });
    });
  }
  for (const button of pane.querySelectorAll("[data-browser-key]")) {
    button.addEventListener("click", () => {
      if (writable && opened === currentWorkspace()) void operation("browser_key", { key: button.dataset.browserKey });
    });
  }
  document.getElementById("browserSendText").addEventListener("click", async () => {
    if (!writable || opened !== currentWorkspace() || !textInput.value) return;
    const text = textInput.value;
    if (await sendBrowserText(text, operation) && textInput.value === text) textInput.value = "";
  });
  auto.addEventListener("change", schedule);
  image.addEventListener("pointerdown", event => {
    pointerStart = { x: event.clientX, y: event.clientY, id: event.pointerId };
  });
  image.addEventListener("pointerup", event => {
    const start = pointerStart;
    pointerStart = null;
    if (!writable || opened !== currentWorkspace() || !start || start.id !== event.pointerId) return;
    const drag = Math.hypot(start.x - event.clientX, start.y - event.clientY);
    if (drag > 8) {
      void operation("browser_scroll", { scroll_y: Math.max(-2000, Math.min(2000, Math.round(start.y - event.clientY))) });
    } else {
      const point = previewPointer(event, image);
      if (point) void operation("browser_click", point);
    }
  });
  image.addEventListener("wheel", event => {
    if (!writable || opened !== currentWorkspace()) return;
    event.preventDefault();
    const delta = Math.max(-2000, Math.min(2000, Math.round(event.deltaY)));
    if (delta) void operation("browser_scroll", { scroll_y: delta });
  }, { passive: false });
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) clearTimeout(timer);
    else schedule();
  });
  setWritable(canManage());
  return { hide, reset, setWritable, workspaceChanged() { if (!pane.hidden) void loadPorts(); } };
}
