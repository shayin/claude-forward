import { renderGrid } from "./cmux-render.mjs?v=20261024-browser-session";
import { alternateFramePatch, sameRenderedFrame } from "./cmux-frame-update.mjs?v=20261024-browser-session";
import { createCmuxFrameOrder } from "./cmux-frame-order.mjs?v=20261024-browser-session";
import { shouldFollowLatest, showLatestScreen } from "./cmux-screen-position.mjs?v=20261024-browser-session";
import { createScrollPositions } from "./cmux-scroll-position.mjs?v=20261024-browser-session";
import { chooseSurface, cmuxClients, cmuxGroups, cmuxWorkspaces, nativeAgentLabel,
  leaseForSelection, requiresCmuxInputClaim, requiresCmuxAdminClaim,
  visibleWorkspaces, groupMemberWorkspaces, nextUnreadWorkspace } from "./cmux-state.mjs?v=20261024-browser-session";
import { screenCoordinates, shouldSubmitComposer, submitTerminalDraft, terminalInput } from "./cmux-input.mjs?v=20261024-browser-session";
import { snapshotViewportPayload } from "./cmux-viewport.mjs?v=20261024-browser-session";
import { mountCmuxFiles } from "./cmux-file-view.mjs?v=20261024-browser-session";
import { mountCmuxBrowser } from "./cmux-browser.mjs?v=20261024-browser-session";
import { applyProgramEvent, programLabel, prunePrograms, topProgram } from "./cmux-programs.mjs?v=20261024-browser-session";
import { maxUploadFiles, uploadChunkedFile } from "./cmux-upload.mjs?v=20261024-browser-session";
import { bindMobileTerminalTextInput } from "./cmux-mobile-input.mjs?v=20261024-browser-session";
import { mountCmuxDrawer } from "./cmux-drawer.mjs?v=20261024-browser-session";
import { setConnectionIndicator } from "./cmux-status.mjs?v=20261024-browser-session";
import { mountCmuxCompact } from "./cmux-compact.mjs?v=20261024-browser-session";
import { mountCmuxScroll } from "./cmux-scroll.mjs?v=20261024-browser-session";
import { createScrollDiagnostic } from "./cmux-scroll-diagnostic.mjs?v=20261024-browser-session";
import { commandDraft } from "./cmux-commands.mjs?v=20261024-browser-session";

const status = document.getElementById("status");
const statusIndicator = document.getElementById("statusIndicator");
const connectionStatusButton = document.getElementById("connectionStatusButton");
const sidebarToggle = document.getElementById("sidebarToggle");
const workspaceShortcut = document.getElementById("workspaceShortcut");
const clientsSelect = document.getElementById("clientSelect");
const workspacesElement = document.getElementById("workspaces");
const unreadOnlyElement = document.getElementById("unreadOnly");
const nextUnreadButton = document.getElementById("nextUnreadButton");
const title = document.getElementById("terminalTitle");
const tabs = document.getElementById("terminalTabs");
const terminalPane = document.getElementById("terminalPane");
const attachmentToggle = document.getElementById("attachmentToggle");
const mobileMedia = window.matchMedia("(max-width: 680px)");
const promptSubmitButton = document.querySelector('#promptForm button[type="submit"]');
const notificationsElement = document.getElementById("notifications");
const prompt = document.getElementById("prompt");
const commandMenu = document.getElementById("commandMenu");
const inputStatus = document.getElementById("inputStatus");
const takeoverButton = document.getElementById("takeoverButton");
const adminStatus = document.getElementById("adminStatus");
const adminTakeoverButton = document.getElementById("adminTakeoverButton");
const terminalElement = document.getElementById("terminal");
const drawer = mountCmuxDrawer({
  button: sidebarToggle,
  aside: document.getElementById("cmuxSidebar"),
  backdrop: document.getElementById("sidebarScrim"),
  body: document.body,
  media: mobileMedia
});
const { syncPromptHeight, setAttachmentOpen, showStatus: presentStatus } = mountCmuxCompact({
  media: mobileMedia,
  statusButton: connectionStatusButton,
  mobileTools: document.getElementById("mobileTools"),
  extraKeys: document.querySelector(".mobile-extra-keys"),
  attachmentToggle,
  terminalPane,
  prompt,
  statusElement: status,
  filesButton: document.getElementById("filesToggleButton"),
  browserButton: document.getElementById("browserToggleButton"),
  refreshButton: document.getElementById("refreshButton")
});
const terminal = new window.Terminal({
  cursorBlink: true, fontSize: 14, fontFamily: "Menlo, monospace", scrollback: 3000,
  theme: { background: "#191d27", foreground: "#e6e9f2" }
});
terminal.open(terminalElement);
const viewportFit = new window.FitAddon.FitAddon();
terminal.loadAddon(viewportFit);

let socket;
let clientDiscoveryTimer;
let requestNumber = 0;
let listRequestVersion = 0;
let selectedClient = localStorage.getItem("cf_cmux_client") || "";
let selectedClientPath = "";
let selectedWorkspace = "";
let selectedSurface = "";
let availableClients = [];
let workspaces = [];
let groups = [];
const programs = new Map();
let unreadOnly = localStorage.getItem("cf_cmux_unread_only") === "1";
let pollTimer;
let listTimer;
let notificationTimer;
let claimTimer;
let adminTimer;
let eventTimer;
let eventRefresh = { list: false, terminal: false, notifications: false };
let activeClient = "";
let inputOwned = false;
let adminOwned = false;
let sessionEpoch = 0;
let snapshotInFlight = null;
let snapshotAfterFlight = false;
let lastFrame = "";
let lastGridFrame = null;
let remoteNeedsSnapshot = null;
const frameOrder = createCmuxFrameOrder();
const scrollPositions = createScrollPositions();
let pendingPosition = false;
let stopped = false;
let pointerStart = null;
let viewportTimer;
const drafts = new Map();
const selectedTabs = new Map();
let composerSending = false;
unreadOnlyElement.checked = unreadOnly;
const pending = new Map();
const scrollDiagnostic = createScrollDiagnostic({
  send: payload => {
    try {
      if (activeClient && document.visibilityState === "visible") {
        send({ type: "cmux_diagnostic", payload });
      }
    } catch { /* 诊断不得影响终端输入 */ }
  }
});
const terminalFiles = document.getElementById("terminalFiles");
const uploadAttachmentsButton = document.getElementById("uploadAttachmentsButton");
const attachmentPreview = document.getElementById("attachmentPreview");
let attachmentPreviewURLs = [];
const scrollController = mountCmuxScroll({
  element: terminalElement,
  terminal,
  isSelected: () => Boolean(selectedSurface && !terminalPane.hidden),
  canRemoteScroll: () => Boolean(inputOwned && selectedSurface && !terminalPane.hidden &&
    lastGridFrame?.mouseScroll),
  sendRemoteScroll: async lines => {
    const client = activeClient, workspace = selectedWorkspace, surface = selectedSurface;
    const epoch = sessionEpoch, sequence = frameOrder.issue();
    const result = await request("scroll", {
      workspace_id: workspace, surface_id: surface, delta_lines: lines
    });
    if (client === activeClient && workspace === selectedWorkspace &&
        surface === selectedSurface && epoch === sessionEpoch) {
      if (result?.render_grid) {
        // 中途帧也可见；序号检查会拒绝比新快照更旧的画面。
        applySnapshot(result, workspace, surface, epoch, sequence);
      }
      // 程序可能在滚动回执之后才完成重绘，空闲时补读一次最终状态。
      remoteNeedsSnapshot = { client, workspace, surface, epoch };
    }
  },
  onRemoteError: error => {
    remoteNeedsSnapshot = null;
    showStatus(`终端滚动失败：${error.message}`);
    void refreshSnapshot(true);
  },
  onRemoteIdle: () => {
    const target = remoteNeedsSnapshot;
    remoteNeedsSnapshot = null;
    if (target && target.client === activeClient &&
        target.workspace === selectedWorkspace && target.surface === selectedSurface &&
        target.epoch === sessionEpoch) void refreshSnapshot(true);
  },
  onGestureStart: mode => scrollDiagnostic.start(mode),
  onGestureMove: delta => scrollDiagnostic.move(delta),
  onGestureEnd: () => scrollDiagnostic.end(),
  onRemoteQueue: (lines, pending) => scrollDiagnostic.queue(lines, pending),
  onRemoteRequestStart: () => scrollDiagnostic.rpcStart(),
  onRemoteRequest: (ms, failed) => scrollDiagnostic.rpc(ms, failed),
  onReturnToBottom: () => void refreshSnapshot(true)
});
const fileView = mountCmuxFiles({
  request,
  currentClient: () => activeClient,
  canUpload: () => adminOwned,
  onShow: () => {
    browserView.hide(false);
    scrollController.reset();
    releaseInput(selectedSurface); setInputState(false);
  },
  onHide: () => { claimInput(); scheduleViewport(); }
});
const browserView = mountCmuxBrowser({
  request,
  currentClient: () => activeClient,
  currentWorkspace: () => selectedWorkspace,
  canManage: () => adminOwned,
  onShow: () => {
    fileView.reset();
    scrollController.reset();
    releaseInput(selectedSurface); setInputState(false);
  },
  onHide: () => { claimInput(); scheduleViewport(); }
});

function setInputState(owned, occupied = false) {
  const wasOwned = inputOwned;
  inputOwned = owned && Boolean(activeClient && selectedSurface);
  if (!inputOwned) {
    scrollController.cancelRemote();
    clearTimeout(viewportTimer);
    setAttachmentOpen(false);
  } else if (!wasOwned) {
    scheduleViewport();
  }
  inputStatus.textContent = !selectedSurface ? "选择终端后检查输入权" :
    inputOwned ? "此设备可输入" : occupied ? "另一设备正在输入 · 只读" : "等待获取输入权";
  takeoverButton.hidden = !selectedSurface || inputOwned || !activeClient;
  takeoverButton.textContent = occupied ? "接管输入" : "获取输入权";
  terminalPane.classList.toggle("input-owned", inputOwned);
  prompt.disabled = !inputOwned;
  commandMenu.classList.toggle("disabled", !inputOwned);
  if (!inputOwned) commandMenu.open = false;
  promptSubmitButton.disabled = !inputOwned || composerSending;
  attachmentToggle.disabled = !inputOwned;
  uploadAttachmentsButton.disabled = !inputOwned;
  document.querySelectorAll(".keys [data-key]").forEach(button => { button.disabled = !inputOwned; });
  document.querySelectorAll(".terminal-tab [data-close]").forEach(button => {
    button.disabled = !inputOwned || button.dataset.close !== selectedSurface;
  });
}

function setAdminState(owned, occupied = false) {
  adminOwned = owned && Boolean(activeClient);
  adminStatus.textContent = !activeClient ? "管理权未取得" :
    adminOwned ? "此设备可管理工作区/文件" :
    occupied ? "另一设备正在管理 · 只读" : "等待获取管理权";
  adminTakeoverButton.hidden = !activeClient || adminOwned;
  adminTakeoverButton.textContent = occupied ? "接管管理" : "获取管理权";
  document.querySelectorAll(".admin-action").forEach(button => { button.disabled = !adminOwned; });
  syncNewTabButtons();
  fileView.setUploadEnabled(adminOwned);
  browserView.setWritable(adminOwned);
}

function syncNewTabButtons() {
  for (const id of ["newTabButton", "mobileNewTabButton"]) {
    document.getElementById(id).disabled = !adminOwned || !selectedWorkspace;
  }
}

function claimAdmin(force = false) {
  if (activeClient && socket?.readyState === WebSocket.OPEN) {
    send({ type: "cmux_claim", payload: { surface_id: "__cmux_admin", force } });
  }
}

function releaseAdmin() {
  if (activeClient && socket?.readyState === WebSocket.OPEN) {
    send({ type: "cmux_release", payload: { surface_id: "__cmux_admin" } });
  }
}

function scheduleViewport() {
  clearTimeout(viewportTimer);
  if (!inputOwned || document.visibilityState !== "visible") return;
  viewportTimer = setTimeout(() => void refreshSnapshot(true), 180);
}

new ResizeObserver(() => {
  scheduleViewport();
  if (lastGridFrame?.alternateScreen && !pendingPosition && !scrollController.holdFrame()) {
    showLatestScreen(terminalElement);
  }
}).observe(terminalElement);
document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "hidden") {
    releaseInput(selectedSurface);
    releaseAdmin();
    setInputState(false);
    setAdminState(false);
  } else {
    claimAdmin();
    claimInput();
  }
});

function claimInput(force = false) {
  if (document.getElementById("terminalPane").hidden) return;
  if (activeClient && selectedSurface && socket?.readyState === WebSocket.OPEN) {
    send({ type: "cmux_claim", payload: { surface_id: selectedSurface, force } });
  }
}

function releaseInput(surfaceID) {
  if (activeClient && surfaceID && socket?.readyState === WebSocket.OPEN) {
    send({ type: "cmux_release", payload: { surface_id: surfaceID } });
  }
}

const draftKeyFor = (client, workspace, surface) => `${client}\x00${workspace}\x00${surface}`;
const draftKey = () => draftKeyFor(selectedClient, selectedWorkspace, selectedSurface);
const tabKey = workspaceID => `${selectedClient}\x00${workspaceID}`;
function saveDraft() {
  if (selectedSurface) drafts.set(draftKey(), prompt.value);
}
function loadDraft() {
  prompt.value = drafts.get(draftKey()) || "";
  syncPromptHeight();
}

function showStatus(message) {
  presentStatus(message);
}
function connectionState(state, message) {
  setConnectionIndicator(statusIndicator, state);
  presentStatus(message, state === "unavailable");
}

function updateWorkspaceShortcut() {
  const workspace = workspaces.find(item => item.id === selectedWorkspace);
  workspaceShortcut.textContent = workspace?.title || "工作区";
  sidebarToggle.setAttribute("aria-label", `切换工作区：${workspace?.title || "未选择"}`);
}

function send(message) {
  if (!socket || socket.readyState !== WebSocket.OPEN) {
    throw new Error("与 CF 的连接尚未就绪");
  }
  socket.send(JSON.stringify(message));
}

function request(action, details = {}) {
  if (!activeClient || activeClient !== selectedClient) {
    return Promise.reject(new Error("请等待 cmux Client 连接完成"));
  }
  if ((requiresCmuxInputClaim(action) ||
      action.startsWith("upload_") && action !== "upload_abort" &&
      details.upload_kind === "attachment") && !inputOwned) {
    return Promise.reject(new Error("此设备尚未取得终端输入权"));
  }
  if (requiresCmuxAdminClaim(action, details.upload_kind) && !adminOwned) {
    return Promise.reject(new Error("此设备尚未取得工作区/文件管理权"));
  }
  const requestId = `cmux-${Date.now()}-${++requestNumber}`;
  const epoch = sessionEpoch;
  const target = { workspace_id: selectedWorkspace, surface_id: selectedSurface, ...details };
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      pending.delete(requestId);
      reject(new Error(`${action} 超时`));
    }, action.startsWith("browser_") ? 45000 :
      action === "paste" ? 16000 : action === "workspace_create" ? 20000 : 10000);
    pending.set(requestId, { resolve, reject, timer, epoch });
    try {
      send({ type: "cmux_request", payload: {
        request_id: requestId, action, ...target
      } });
    } catch (error) {
      clearTimeout(timer);
      pending.delete(requestId);
      reject(error);
    }
  });
}

function clearPending(reason) {
  for (const entry of pending.values()) {
    clearTimeout(entry.timer);
    entry.reject(new Error(reason));
  }
  pending.clear();
}

function scheduleEventRefresh(category, force = false) {
  if (force || category !== "terminal") eventRefresh.list = true;
  if (force || category === "terminal" || category === "surface" ||
      category === "agent" || category === "program") {
    eventRefresh.terminal = true;
  }
  if (force || category === "notification" || category === "feed") eventRefresh.notifications = true;
  if (eventTimer) return;
  eventTimer = setTimeout(() => {
    eventTimer = null;
    const refresh = eventRefresh;
    eventRefresh = { list: false, terminal: false, notifications: false };
    if (refresh.list) void refreshWorkspaces().catch(error => showStatus(error.message));
    if (refresh.terminal) void refreshSnapshot();
    if (refresh.notifications) void refreshNotifications().catch(error => showStatus(error.message));
  }, 150);
}

function renderClients(clientList) {
  const available = cmuxClients(clientList);
  // 切换 Client 时不复用上一台电脑的工作目录。
  availableClients = available;
  clientsSelect.replaceChildren(new Option("选择 Client", ""));
  for (const client of available) {
    clientsSelect.add(new Option(client.name || client.id, client.id));
  }
  if (!available.length) {
    connectionState("unavailable", "终端模式未就绪，等待电脑连接");
    return;
  }
  const client = available.find(item => item.id === selectedClient) ||
    (selectedClient ? null : available[0]);
  if (!client) {
    connectionState("unavailable", "所选 Client 已离线，等待重新连接");
    return;
  }
  selectedClient = client.id;
  selectedClientPath = client.path || "";
  clientsSelect.value = selectedClient;
  attach();
}

function attach() {
  if (!selectedClient) return;
  scrollController.reset();
  fileView.reset();
  browserView.reset();
  clearAttachmentPreview();
  terminalFiles.value = "";
  releaseInput(selectedSurface);
  releaseAdmin();
  sessionEpoch += 1;
  activeClient = "";
  setAdminState(false);
  clearPending("已切换 cmux Client");
  clearInterval(pollTimer);
  clearInterval(listTimer);
  clearInterval(notificationTimer);
  clearInterval(claimTimer);
  clearInterval(adminTimer);
  clearTimeout(eventTimer);
  eventTimer = null;
  localStorage.setItem("cf_cmux_client", selectedClient);
  selectedWorkspace = "";
  selectedSurface = "";
  updateWorkspaceShortcut();
  setAttachmentOpen(false);
  setInputState(false);
  prompt.value = "";
  syncPromptHeight();
  workspaces = [];
  groups = [];
  programs.clear();
  snapshotInFlight = null;
  snapshotAfterFlight = false;
  lastFrame = "";
  lastGridFrame = null;
  workspacesElement.replaceChildren();
  notificationsElement.replaceChildren();
  tabs.replaceChildren();
  title.textContent = "连接电脑终端…";
  send({ type: "cmux_attach", payload: { client_id: selectedClient } });
}

function clearAttachmentPreview() {
  for (const url of attachmentPreviewURLs) URL.revokeObjectURL(url);
  attachmentPreviewURLs = [];
  attachmentPreview.replaceChildren();
}

terminalFiles.addEventListener("change", () => {
  clearAttachmentPreview();
  const files = Array.from(terminalFiles.files || []);
  if (files.length > maxUploadFiles) {
    showStatus(`每次最多上传 ${maxUploadFiles} 个附件`);
    return;
  }
  for (const file of files) {
    const item = document.createElement("span");
    item.textContent = file.name;
    attachmentPreview.append(item);
    if (file.type.startsWith("image/") && file.type !== "image/svg+xml") {
      const url = URL.createObjectURL(file);
      attachmentPreviewURLs.push(url);
      const image = document.createElement("img");
      image.src = url;
      image.alt = file.name;
      attachmentPreview.append(image);
    }
  }
});

uploadAttachmentsButton.addEventListener("click", async () => {
  const files = Array.from(terminalFiles.files || []);
  if (!files.length || files.length > maxUploadFiles || !inputOwned || !selectedSurface) {
    showStatus("请选择 1–8 个文件，并先取得当前终端输入权");
    return;
  }
  const client = activeClient, workspace = selectedWorkspace, surface = selectedSurface;
  const targetKey = draftKeyFor(client, workspace, surface);
  saveDraft();
  uploadAttachmentsButton.disabled = true;
  const stillCurrent = () => activeClient === client && selectedWorkspace === workspace &&
    selectedSurface === surface && inputOwned && !document.getElementById("terminalPane").hidden &&
    socket?.readyState === WebSocket.OPEN;
  try {
    for (const file of files) {
      const result = await uploadChunkedFile(file, {
        kind: "attachment", workspaceID: workspace, surfaceID: surface
      }, request, stillCurrent, (written, total) =>
        showStatus(`上传 ${file.name}：${written}/${total} B`));
      const currentDraft = drafts.get(targetKey) || "";
      drafts.set(targetKey, [currentDraft.trimEnd(), result.path].filter(Boolean).join("\n"));
      if (stillCurrent()) {
        prompt.value = drafts.get(targetKey);
        syncPromptHeight();
      }
    }
    showStatus("附件路径已加入当前终端草稿；确认内容后再发送");
    terminalFiles.value = "";
    clearAttachmentPreview();
    setAttachmentOpen(false);
  } catch (error) {
    showStatus(`附件上传中断：${error.message}；已成功的路径保留在原终端草稿`);
  } finally {
    uploadAttachmentsButton.disabled = !inputOwned;
  }
});

async function mutate(action, details, description, confirmText = "") {
  if (confirmText && !window.confirm(confirmText)) return;
  try {
    await request(action, details);
    showStatus(`${description}成功`);
    await refreshWorkspaces();
  } catch (error) {
    showStatus(`${description}失败：${error.message}`);
  }
}

function makeAction(label, handler) {
  const button = document.createElement("button");
  button.type = "button";
  button.textContent = label;
  button.addEventListener("click", event => { event.stopPropagation(); handler(); });
  return button;
}

function makeAdminAction(label, handler) {
  const button = makeAction(label, handler);
  button.classList.add("admin-action");
  button.disabled = !adminOwned;
  return button;
}

function renderWorkspace(workspace, container) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = `workspace${workspace.id === selectedWorkspace ? " active" : ""}`;
    const label = document.createElement("span");
    const state = nativeAgentLabel(workspace.terminals);
    label.textContent = `${workspace.title}${state ? ` · ${state}` : ""}` +
      `${workspace.unreadCount ? ` (${workspace.unreadCount} 未读)` : ""}`;
    const directory = document.createElement("small");
    directory.textContent = [workspace.directory, workspace.branch ? `分支 ${workspace.branch}` : "",
      workspace.listeningPorts.length ? `网页 ${workspace.listeningPorts.join(", ")}` : ""].filter(Boolean).join(" · ");
    const recent = document.createElement("small");
    recent.textContent = workspace.lastMessage;
    const activity = document.createElement("small");
    activity.textContent = programLabel(topProgram(programs, workspace.id));
    button.append(label, directory, recent, activity);
    button.addEventListener("click", () => selectWorkspace(workspace.id));
    container.append(button);

    const actions = document.createElement("div");
    actions.className = "workspace-actions";
    actions.append(
      makeAdminAction("改名", () => {
        const name = window.prompt("工作区新名称", workspace.title)?.trim();
        if (name && name !== workspace.title) void mutate("workspace_rename",
          { workspace_id: workspace.id, title: name }, "工作区改名");
      }),
      makeAdminAction("关闭", () => void mutate("workspace_close",
        { workspace_id: workspace.id }, "关闭工作区", `确定关闭「${workspace.title}」？`))
    );
    const index = workspaces.findIndex(item => item.id === workspace.id);
    if (index > 0) actions.append(makeAdminAction("↑", () => void mutate("workspace_reorder",
      { workspace_id: workspace.id, index: index - 1 }, "工作区上移")));
    if (index < workspaces.length - 1) actions.append(makeAdminAction("↓", () => void mutate("workspace_reorder",
      { workspace_id: workspace.id, index: index + 1 }, "工作区下移")));
    const move = document.createElement("select");
    move.classList.add("admin-action");
    move.disabled = !adminOwned;
    move.setAttribute("aria-label", `移动「${workspace.title}」到分组`);
    move.add(new Option("未分组", ""));
    for (const group of groups) move.add(new Option(group.title, group.id));
    move.value = workspace.groupId;
    move.addEventListener("change", () => {
      const action = move.value ? "group_add" : "group_remove";
      void mutate(action, { workspace_id: workspace.id, group_id: move.value }, "移动工作区");
    });
    actions.append(move);
    container.append(actions);
}

function renderWorkspaces() {
  workspacesElement.replaceChildren();
  const visible = visibleWorkspaces(workspaces, unreadOnly);
  const ungrouped = visible.filter(workspace => !groups.some(group => group.id === workspace.groupId));
  for (const group of groups) {
    const members = groupMemberWorkspaces(visible, group);
    if (unreadOnly && members.length === 0 &&
        !visible.some(workspace => workspace.id === group.anchorId)) continue;
    const section = document.createElement("section");
    section.className = "workspace-group";
    const header = document.createElement("div");
    header.className = "group-header";
    const collapsedKey = `cf_cmux_group_${group.id}`;
    const collapsed = localStorage.getItem(collapsedKey);
    const isCollapsed = !unreadOnly && (collapsed === null ? group.collapsed : collapsed === "closed");
    header.append(
      makeAction(`${isCollapsed ? "▸" : "▾"} ${group.title}${group.pinned ? " 📌" : ""}`, () => {
        localStorage.setItem(collapsedKey, isCollapsed ? "open" : "closed");
        renderWorkspaces();
      }),
      makeAdminAction("改名", () => {
        const name = window.prompt("分组新名称", group.title)?.trim();
        if (name && name !== group.title) void mutate("group_rename",
          { group_id: group.id, title: name }, "分组改名");
      })
    );
    if (group.anchorId) {
      header.append(makeAction("组终端", () => selectWorkspace(group.anchorId)));
    }
    section.append(header);
    if (!isCollapsed) {
      for (const workspace of members) {
        renderWorkspace(workspace, section);
      }
    }
    workspacesElement.append(section);
  }
  for (const workspace of ungrouped) {
    renderWorkspace(workspace, workspacesElement);
  }
}

function selectWorkspace(id) {
  const workspace = workspaces.find(item => item.id === id);
  if (!workspace) return;
  drawer.close();
  const previousSurface = selectedWorkspace === id ? selectedSurface :
    selectedTabs.get(tabKey(id)) || localStorage.getItem(`cf_cmux_tab_${tabKey(id)}`) || "";
  const nextSurface = chooseSurface(workspace, previousSurface);
  if (selectedWorkspace === id && selectedSurface === nextSurface) return;
  if (!pendingPosition) scrollPositions.save(activeClient, selectedWorkspace, selectedSurface,
    lastGridFrame?.alternateScreen === true, terminalElement, terminal);
  pendingPosition = true;
  scrollController.reset();
  saveDraft();
  const previousSelectedSurface = selectedSurface;
  const workspaceChanged = selectedWorkspace !== id;
  selectedWorkspace = id;
  updateWorkspaceShortcut();
  syncNewTabButtons();
  if (workspaceChanged) browserView.workspaceChanged();
  localStorage.setItem(`cf_cmux_workspace_${selectedClient}`, id);
  selectedSurface = nextSurface;
  if (previousSelectedSurface !== selectedSurface) {
    releaseInput(previousSelectedSurface);
    setInputState(false);
    claimInput();
  }
  selectedTabs.set(tabKey(id), selectedSurface);
  if (selectedSurface) localStorage.setItem(`cf_cmux_tab_${tabKey(id)}`, selectedSurface);
  loadDraft();
  title.textContent = `${workspace.title} · ${workspace.terminals.find(item => item.id === selectedSurface)?.title || "Terminal"}`;
  updateProgramStatus();
  terminal.reset();
  lastFrame = "";
  lastGridFrame = null;
  renderWorkspaces();
  renderTabs(workspace);
  void refreshSnapshot();
}

function renderTabs(workspace) {
  tabs.replaceChildren();
  for (const surface of workspace.terminals) {
    const button = document.createElement("button");
    button.type = "button";
    const agent = nativeAgentLabel([surface]);
    button.textContent = `${surface.title || surface.id}${agent ? ` · ${agent}` : ""}`;
    button.classList.toggle("active", surface.id === selectedSurface);
    button.addEventListener("click", () => {
      if (selectedSurface === surface.id && selectedWorkspace === workspace.id) return;
      if (!pendingPosition) scrollPositions.save(activeClient, selectedWorkspace, selectedSurface,
        lastGridFrame?.alternateScreen === true, terminalElement, terminal);
      pendingPosition = true;
      saveDraft();
      scrollController.reset();
      const oldSurface = selectedSurface;
      selectedSurface = surface.id;
      if (oldSurface !== selectedSurface) {
        releaseInput(oldSurface);
        setInputState(false);
        claimInput();
      }
      selectedTabs.set(tabKey(workspace.id), surface.id);
      localStorage.setItem(`cf_cmux_tab_${tabKey(workspace.id)}`, surface.id);
      loadDraft();
      title.textContent = `${workspace.title} · ${surface.title || "Terminal"}`;
      updateProgramStatus();
      terminal.reset();
      lastFrame = "";
      lastGridFrame = null;
      renderTabs(workspace);
      void refreshSnapshot();
    });
    const tab = document.createElement("div");
    tab.className = "terminal-tab";
    tab.append(button);
    if (workspace.terminals.length > 1) {
      const close = makeAction("×", () => void mutate("terminal_close",
        { workspace_id: workspace.id, surface_id: surface.id }, "关闭终端",
        `确定关闭「${surface.title || surface.id}」？终端内正在运行的程序会中断。`));
      close.dataset.close = surface.id;
      close.disabled = !inputOwned || surface.id !== selectedSurface;
      close.title = close.disabled ? "先切换到此终端并取得输入权" : "关闭当前终端";
      tab.append(close);
    }
    tabs.append(tab);
  }
}

function updateProgramStatus() {
  const element = document.getElementById("programStatus");
  const label = programLabel(topProgram(programs, selectedWorkspace, selectedSurface));
  element.textContent = label;
  element.hidden = !label;
}

async function refreshWorkspaces() {
  if (!activeClient || !socket || socket.readyState !== WebSocket.OPEN) return;
  const epoch = sessionEpoch;
  const version = ++listRequestVersion;
  const result = await request("list");
  if (epoch !== sessionEpoch || version !== listRequestVersion) return;
  workspaces = cmuxWorkspaces(result);
  groups = cmuxGroups(result);
  updateWorkspaceShortcut();
  prunePrograms(programs, workspaces);
  nextUnreadButton.disabled = !nextUnreadWorkspace(workspaces, selectedWorkspace);
  renderWorkspaces();
  const current = workspaces.find(item => item.id === selectedWorkspace);
  if (current) {
    const nextSurface = chooseSurface(current, selectedSurface);
    if (nextSurface !== selectedSurface) selectWorkspace(current.id);
    else renderTabs(current);
  }
  if (!workspaces.some(item => item.id === selectedWorkspace)) {
    if (workspaces.length) {
      const remembered = localStorage.getItem(`cf_cmux_workspace_${selectedClient}`);
      selectWorkspace(workspaces.find(item => item.id === remembered)?.id || workspaces[0].id);
    }
    else {
      selectedWorkspace = "";
      selectedSurface = "";
      updateWorkspaceShortcut();
      syncNewTabButtons();
      setInputState(false);
      updateProgramStatus();
      title.textContent = "cmux 中没有可用的终端工作区";
      terminal.reset();
      lastFrame = "";
      lastGridFrame = null;
    }
  }
  if (statusIndicator.dataset.state !== "ready") {
    connectionState("ready", "终端模式已连接");
  }
}

async function refreshSnapshot(urgent = false) {
  if (!selectedSurface || !activeClient) return;
  if (!urgent && scrollController.isRemoteActive()) return;
  if (scrollController.holdFrame()) return;
  const surface = selectedSurface;
  const workspace = selectedWorkspace;
  const epoch = sessionEpoch;
  if (snapshotInFlight?.surface === surface && snapshotInFlight.workspace === workspace &&
      snapshotInFlight.epoch === epoch) {
    if (urgent) snapshotAfterFlight = true;
    return;
  }
  const flight = { surface, workspace, epoch };
  snapshotInFlight = flight;
  const sequence = frameOrder.issue();
  const started = performance.now();
  try {
    const result = await request("snapshot", snapshotViewportPayload(
      workspace, surface, inputOwned, document.visibilityState === "visible",
      viewportFit.proposeDimensions()
    ));
    scrollDiagnostic.snapshot(performance.now() - started);
    // 请求发出后若又开始了滚动，旧快照不能覆盖正在进行的手势。
    if (!scrollController.isRemoteActive()) {
      applySnapshot(result, workspace, surface, epoch, sequence);
    } else {
      scrollDiagnostic.frame(0, 0, true);
    }
  } catch (error) {
    scrollDiagnostic.snapshot(performance.now() - started);
    showStatus(error.message);
  } finally {
    if (snapshotInFlight === flight) {
      snapshotInFlight = null;
      if (snapshotAfterFlight) {
        snapshotAfterFlight = false;
        if (!scrollController.holdFrame()) void refreshSnapshot(true);
      }
    }
  }
}

function applySnapshot(result, workspace, surface, epoch, sequence) {
  const renderStarted = performance.now();
  if (surface !== selectedSurface || workspace !== selectedWorkspace ||
      epoch !== sessionEpoch || scrollController.holdFrame() ||
      !frameOrder.accept(sequence)) {
    scrollDiagnostic.frame(0, 0, true);
    return;
  }
  if (result?.render_grid) {
    const frame = renderGrid(result.render_grid);
    scrollDiagnostic.frame(performance.now() - renderStarted);
    scrollController.setAlternateScreen(frame.alternateScreen);
    if (lastGridFrame?.mouseScroll && !frame.mouseScroll) scrollController.cancelRemote();
    if (sameRenderedFrame(lastGridFrame, frame, terminal.cols, terminal.rows)) {
      scrollDiagnostic.unchanged();
      return;
    }
    const scrollOffset = terminal.buffer.active.baseY - terminal.buffer.active.viewportY;
    const followLatest = shouldFollowLatest(terminalElement, frame.alternateScreen,
      scrollController.isBrowsing());
    const sizeChanged = terminal.cols !== frame.columns || terminal.rows !== frame.rows;
    if (sizeChanged) {
      terminal.resize(frame.columns, frame.rows);
    }
    if (terminal.hasSelection()) return;
    const patch = sizeChanged ? null : alternateFramePatch(lastGridFrame, frame);
    const writeStarted = performance.now();
    lastFrame = frame.content;
    lastGridFrame = frame;
    const showLatestIfCurrent = () => {
      if (surface === selectedSurface &&
          workspace === selectedWorkspace && epoch === sessionEpoch &&
          lastGridFrame === frame && !scrollController.holdFrame()) {
        if (pendingPosition) {
          pendingPosition = false;
          const restored = scrollPositions.restore(activeClient, workspace, surface,
            frame.alternateScreen, terminalElement, terminal);
          if (restored) scrollController.restoreBrowsing();
          else if (frame.alternateScreen) showLatestScreen(terminalElement);
        } else if (frame.alternateScreen) {
          showLatestScreen(terminalElement);
        }
      }
    };
    if (patch !== null) {
      if (patch) terminal.write(patch, () => {
        scrollDiagnostic.frame(0, performance.now() - writeStarted);
        if (followLatest) showLatestIfCurrent();
      });
    } else {
      // 主屏的历史缓冲与初始全屏帧仍需完整重建；全屏后续帧只更新变化行。
      terminal.reset();
      terminal.write(`\x1b[3J${frame.content}`, () => {
        scrollDiagnostic.frame(0, performance.now() - writeStarted);
        if (scrollOffset > 0 && !frame.alternateScreen) terminal.scrollLines(-scrollOffset);
        showLatestIfCurrent();
      });
    }
  } else {
    scrollController.setAlternateScreen(false);
    lastGridFrame = null;
    const plain = typeof result === "string" ? result : result?.text;
    const text = String(plain || "终端尚未就绪")
      .replace(/[\x00-\x08\x0b-\x1f\x7f-\x9f]/g, "");
    if (text !== lastFrame) {
      lastFrame = text;
      terminal.reset();
      terminal.write(text);
    }
    pendingPosition = false;
  }
}

async function refreshNotifications() {
  if (!activeClient) return;
  const epoch = sessionEpoch;
  const result = await request("notifications");
  if (epoch !== sessionEpoch) return;
  notificationsElement.replaceChildren();
  for (const item of (Array.isArray(result?.notifications) ? result.notifications : []).slice(0, 30)) {
    if (!item.id || item.is_read) continue;
    const button = makeAdminAction(item.title || item.body || "未读通知", () => {
      if (item.workspace_id) selectWorkspace(item.workspace_id);
      void request("notification_read", { notification_id: item.id })
        .then(() => refreshNotifications()).catch(error => showStatus(error.message));
    });
    button.classList.add("notification");
    notificationsElement.append(button);
  }
}

function connect() {
  const server = localStorage.getItem("cf_server") ||
    `${location.protocol === "https:" ? "wss:" : "ws:"}//${location.host}`;
  if (!server) {
    connectionState("unavailable", "请先返回原有页面登录");
    return;
  }
  let url;
  try {
    const parsed = new URL(server, location.href);
    parsed.protocol = parsed.protocol === "https:" || parsed.protocol === "wss:" ? "wss:" : "ws:";
    parsed.pathname = "/ws";
    parsed.search = "";
    parsed.hash = "";
    const httpOrigin = `${parsed.protocol === "wss:" ? "https:" : "http:"}//${parsed.host}`;
    if (httpOrigin !== location.origin) throw new Error("终端页面只能连接当前服务器");
    url = parsed.href;
  } catch (error) {
    connectionState("unavailable", error.message);
    return;
  }
  connectionState("connecting", "连接中…");
  const channel = new WebSocket(url);
  socket = channel;
  channel.onopen = () => {
    connectionState("connecting", "已连接，查找电脑终端");
    send({ type: "list" });
    clearInterval(clientDiscoveryTimer);
    clientDiscoveryTimer = setInterval(() => {
      if (!activeClient && socket === channel && channel.readyState === WebSocket.OPEN) {
        send({ type: "list" });
      }
    }, 5000);
  };
  channel.onmessage = event => {
    let message;
    try { message = JSON.parse(event.data); } catch { return; }
    switch (message.type) {
      case "list":
        renderClients(message.payload?.clients);
        break;
      case "cmux_attached":
        if (message.payload?.client_id !== selectedClient) break;
        activeClient = selectedClient;
        clearInterval(clientDiscoveryTimer);
        setInputState(false);
        setAdminState(false);
        claimAdmin();
        connectionState("connecting", "正在检查电脑终端");
        void refreshWorkspaces().catch(error => connectionState("unavailable", error.message));
        void refreshNotifications().catch(error => showStatus(error.message));
        void fileView.refreshRoots();
        clearInterval(pollTimer);
        clearInterval(listTimer);
        clearInterval(notificationTimer);
        clearInterval(claimTimer);
        clearInterval(adminTimer);
        pollTimer = setInterval(() => void refreshSnapshot(), 1200);
        listTimer = setInterval(() => void refreshWorkspaces()
          .catch(error => connectionState("unavailable", error.message)), 5000);
        notificationTimer = setInterval(() =>
          void refreshNotifications().catch(error => showStatus(error.message)), 8000);
        claimTimer = setInterval(() => {
          if (document.visibilityState === "visible") claimInput();
        }, 20000);
        adminTimer = setInterval(() => {
          if (document.visibilityState === "visible") claimAdmin();
        }, 20000);
        break;
      case "cmux_response": {
        const response = message.payload || {};
        const entry = pending.get(response.request_id);
        if (!entry || entry.epoch !== sessionEpoch) break;
        clearTimeout(entry.timer);
        pending.delete(response.request_id);
        if (response.error) entry.reject(new Error(response.error));
        else entry.resolve(response.result);
        break;
      }
      case "cmux_event":
        if (message.from !== activeClient) break;
        if (message.payload?.gap || message.payload?.resync) {
          programs.clear();
          updateProgramStatus();
          renderWorkspaces();
        }
        if (applyProgramEvent(programs, message.payload)) {
          updateProgramStatus();
          renderWorkspaces();
        }
        if (message.payload?.resync || message.payload?.gap) {
          scheduleEventRefresh("", true);
        } else if (message.payload?.surface_id && selectedSurface &&
            message.payload.surface_id !== selectedSurface && message.payload?.category === "terminal") {
          break;
        } else {
          scheduleEventRefresh(message.payload?.category || "");
        }
        break;
      case "cmux_claimed": {
        if (message.payload?.surface_id === "__cmux_admin" &&
            message.payload?.client_id === activeClient) {
          if (document.visibilityState === "hidden" && message.payload.owned) releaseAdmin();
          setAdminState(message.payload.owned === true && document.visibilityState === "visible",
            message.payload.occupied === true);
          break;
        }
        const claim = leaseForSelection(message.payload, activeClient, selectedSurface);
        if (claim) {
          if (document.getElementById("terminalPane").hidden && claim.owned) {
            releaseInput(selectedSurface);
          }
          setInputState(claim.owned && !document.getElementById("terminalPane").hidden, claim.occupied);
        }
        break;
      }
      case "detached":
        if (message.payload?.client_id !== activeClient) break;
        activeClient = "";
        setInputState(false);
        setAdminState(false);
        browserView.reset();
        sessionEpoch += 1;
        clearPending("Client 已离线");
        connectionState("unavailable", "Client 已离线，等待重新连接");
        clearInterval(pollTimer);
        clearInterval(listTimer);
        clearInterval(notificationTimer);
        clearInterval(claimTimer);
        clearInterval(adminTimer);
        clearTimeout(eventTimer);
        eventTimer = null;
        send({ type: "list" });
        clientDiscoveryTimer = setInterval(() => {
          if (!activeClient && channel.readyState === WebSocket.OPEN) send({ type: "list" });
        }, 5000);
        break;
      case "error":
        if (!activeClient) connectionState("unavailable", message.payload?.message || "连接错误");
        else showStatus(message.payload?.message || "连接错误");
        break;
    }
  };
  channel.onerror = () => connectionState("unavailable", "CF WebSocket 连接失败");
  channel.onclose = () => {
    if (socket !== channel || stopped) return;
    activeClient = "";
    setInputState(false);
    setAdminState(false);
    browserView.reset();
    sessionEpoch += 1;
    connectionState("connecting", "连接中断，正在重连…");
    clearInterval(clientDiscoveryTimer);
    clearInterval(pollTimer);
    clearInterval(listTimer);
    clearInterval(notificationTimer);
    clearInterval(claimTimer);
    clearInterval(adminTimer);
    clearTimeout(eventTimer);
    eventTimer = null;
    clearPending("连接已断开");
    setTimeout(connect, 3000);
  };
}

clientsSelect.addEventListener("change", () => {
  saveDraft();
  selectedClient = clientsSelect.value;
  selectedClientPath = availableClients.find(item => item.id === selectedClient)?.path || "";
  attach();
});
document.getElementById("refreshButton").addEventListener("click", () =>
  void refreshWorkspaces().catch(error => showStatus(error.message)));
unreadOnlyElement.addEventListener("change", () => {
  unreadOnly = unreadOnlyElement.checked;
  localStorage.setItem("cf_cmux_unread_only", unreadOnly ? "1" : "0");
  renderWorkspaces();
});
nextUnreadButton.addEventListener("click", () => {
  const next = nextUnreadWorkspace(workspaces, selectedWorkspace);
  if (next) selectWorkspace(next);
});
for (const id of ["workspaceButton", "groupButton", "newTabButton", "mobileNewTabButton"]) {
  document.getElementById(id).classList.add("admin-action");
}
adminTakeoverButton.addEventListener("click", () => claimAdmin(true));
document.getElementById("workspaceButton").addEventListener("click", async () => {
  const name = window.prompt("新工作区名称")?.trim();
  if (!name) return;
  const directory = window.prompt("电脑上的绝对工作目录",
    workspaces.find(item => item.id === selectedWorkspace)?.directory || selectedClientPath)?.trim();
  if (!directory) return;
  try {
    const result = await request("workspace_create", { title: name, directory, surface_id: "",
      profile: document.getElementById("workspaceProfile").value });
    await refreshWorkspaces();
    if (result?.workspace_id && workspaces.some(item => item.id === result.workspace_id)) {
      selectWorkspace(result.workspace_id);
    }
    showStatus(`工作区「${name}」已创建`);
  } catch (error) {
    showStatus(`创建工作区失败：${error.message}`);
  }
});
document.getElementById("groupButton").addEventListener("click", () => {
  const name = window.prompt("新分组名称")?.trim();
  if (!name) return;
  const directory = workspaces.find(item => item.id === selectedWorkspace)?.directory || "";
  void mutate("group_create", { title: name, directory }, "创建分组");
});
for (const id of ["newTabButton", "mobileNewTabButton"]) {
  document.getElementById(id).addEventListener("click", () => {
    if (selectedWorkspace && adminOwned) void mutate("terminal_create",
      { workspace_id: selectedWorkspace, surface_id: "" }, "新建终端");
  });
}
document.getElementById("filesToggleButton").addEventListener("click", drawer.close);
document.getElementById("browserToggleButton").addEventListener("click", drawer.close);
takeoverButton.addEventListener("click", () => claimInput(true));
document.getElementById("promptForm").addEventListener("submit", event => {
  event.preventDefault();
  if (!selectedSurface || !inputOwned || !prompt.value || composerSending) return;
  composerSending = true;
  promptSubmitButton.disabled = true;
  const text = prompt.value;
  const targetSurface = selectedSurface;
  const targetWorkspace = selectedWorkspace;
  const client = activeClient, epoch = sessionEpoch;
  const stillCurrent = () => client === activeClient && epoch === sessionEpoch &&
    targetSurface === selectedSurface && targetWorkspace === selectedWorkspace &&
    inputOwned && socket?.readyState === WebSocket.OPEN;
  void submitTerminalDraft(
    () => request("paste", { workspace_id: targetWorkspace, surface_id: targetSurface, text }),
    () => request("key", { workspace_id: targetWorkspace, surface_id: targetSurface, key: "return" }),
    stillCurrent, () => new Promise(resolve => setTimeout(resolve, 250))
  ).then(() => {
    if (targetSurface === selectedSurface && targetWorkspace === selectedWorkspace) {
      if (prompt.value === text) prompt.value = "";
      drafts.set(draftKey(), prompt.value);
      syncPromptHeight();
      void refreshSnapshot();
    }
  }).catch(error => showStatus(error.message)).finally(() => {
    composerSending = false;
    promptSubmitButton.disabled = !inputOwned;
  });
});
prompt.addEventListener("input", () => { saveDraft(); syncPromptHeight(); });
commandMenu.addEventListener("toggle", () => {
  if (!inputOwned) commandMenu.open = false;
});
commandMenu.querySelectorAll("[data-command]").forEach(button => button.addEventListener("click", () => {
  if (!inputOwned || !selectedSurface) return;
  const next = commandDraft(prompt.value, button.dataset.command);
  if (next === null) {
    showStatus("输入框已有内容，请先发送或清空草稿");
    return;
  }
  prompt.value = next;
  saveDraft();
  syncPromptHeight();
  commandMenu.open = false;
  prompt.focus();
}));
prompt.addEventListener("keydown", event => {
  if (!shouldSubmitComposer(event)) return;
  event.preventDefault();
  document.getElementById("promptForm").requestSubmit();
});
document.querySelectorAll("[data-key]").forEach(button => button.addEventListener("click", () => {
  if (!selectedSurface) return;
  void request("key", { surface_id: selectedSurface, key: button.dataset.key })
    .then(() => void refreshSnapshot()).catch(error => showStatus(error.message));
}));
terminal.onData(data => {
  if (!selectedSurface || !inputOwned) return;
  const input = terminalInput(data);
  if (!input) {
    showStatus("当前快捷键暂不支持，请使用下方按钮");
    return;
  }
  const { action, ...details } = input;
  void request(action, { ...details, workspace_id: selectedWorkspace, surface_id: selectedSurface })
    .catch(error => showStatus(error.message));
});

terminalElement.addEventListener("pointerdown", event => {
  if (event.button !== 0 || !selectedSurface) return;
  pointerStart = {
    x: event.clientX, y: event.clientY, pointerId: event.pointerId,
    workspace: selectedWorkspace, surface: selectedSurface, pointerType: event.pointerType
  };
});
terminalElement.addEventListener("pointerup", event => {
  const start = pointerStart;
  pointerStart = null;
  if (!inputOwned || !start || start.pointerId !== event.pointerId || start.surface !== selectedSurface ||
      start.workspace !== selectedWorkspace) return;
  const distance = Math.hypot(event.clientX - start.x, event.clientY - start.y);
  if (start.pointerType === "touch" && distance > 12) return;
  if (distance > 5 || terminal.hasSelection()) return;
  const screen = terminalElement.querySelector(".xterm-screen");
  const position = screenCoordinates(event.clientX, event.clientY,
    screen?.getBoundingClientRect(), terminal.cols, terminal.rows);
  if (position) void request("mouse", {
    workspace_id: start.workspace, surface_id: start.surface, ...position
  }).then(() => refreshSnapshot()).catch(error => showStatus(error.message));
});
const xtermTextarea = terminalElement.querySelector(".xterm-helper-textarea");
if (xtermTextarea) bindMobileTerminalTextInput({
  textarea: xtermTextarea,
  isActive: () => inputOwned && window.matchMedia("(pointer: coarse)").matches,
  sendText: text => {
    if (!selectedSurface || !text) return;
    void request("text", {
      workspace_id: selectedWorkspace, surface_id: selectedSurface, text
    }).catch(error => showStatus(error.message));
  }
});

window.addEventListener("pagehide", () => {
  scrollController.reset();
  releaseInput(selectedSurface);
  releaseAdmin();
  stopped = true;
  fileView.reset();
  browserView.reset();
  clearAttachmentPreview();
  clearTimeout(viewportTimer);
  clearInterval(pollTimer);
  clearInterval(clientDiscoveryTimer);
  clearInterval(listTimer);
  clearInterval(notificationTimer);
  clearInterval(claimTimer);
  clearInterval(adminTimer);
  clearTimeout(eventTimer);
  clearPending("页面已关闭");
  socket?.close();
});
window.addEventListener("pageshow", event => {
  if (event.persisted) {
    stopped = false;
    connect();
  }
});

setInputState(false);
setAdminState(false);
connect();
document.documentElement.dataset.cmuxStarted = "1";
