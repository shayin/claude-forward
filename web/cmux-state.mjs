export function cmuxClients(clients) {
  return (Array.isArray(clients) ? clients : []).filter(client =>
    Array.isArray(client.features) && client.features.includes("cmux"));
}

export function cmuxWorkspaces(result) {
  return (Array.isArray(result?.workspaces) ? result.workspaces : [])
    .filter(workspace => typeof workspace.id === "string" && workspace.id)
    .map(workspace => ({
      id: workspace.id,
      title: workspace.title || workspace.current_directory || workspace.id,
      directory: workspace.current_directory || "",
      groupId: workspace.group_id || "",
      unreadCount: Number(workspace.unread_count) || 0,
      branch: typeof workspace.branch_summary === "string" ? workspace.branch_summary :
        String(workspace.branch_summary?.branch || workspace.git_branches?.[0]?.branch || ""),
      lastMessage: typeof (workspace.latest_conversation_message || workspace.preview) === "string"
        ? (workspace.latest_conversation_message || workspace.preview).slice(0, 240) : "",
      listeningPorts: (Array.isArray(workspace.listening_ports) ? workspace.listening_ports : [])
        .map(item => typeof item === "object" && item ? item.port : item)
        .filter(port => Number.isInteger(port) && port > 0 && port <= 65535).slice(0, 20),
      terminals: (Array.isArray(workspace.terminals) ? workspace.terminals : [])
        .filter(terminal => typeof terminal.id === "string" && terminal.id)
    }));
}

export function cmuxGroups(result) {
  return (Array.isArray(result?.groups) ? result.groups : [])
    .filter(group => typeof (group.id || group.group_id) === "string")
    .map(group => ({
      id: group.id || group.group_id,
      title: group.title || group.name || group.id || group.group_id,
      pinned: group.is_pinned === true,
      collapsed: group.is_collapsed === true,
      anchorId: group.anchor_workspace_id || "",
      generatedAnchor: group.anchor_workspace_is_generated === true
    }));
}

export function chooseSurface(workspace, currentId = "") {
  const terminals = workspace?.terminals || [];
  return terminals.find(terminal => terminal.id === currentId)?.id ||
    terminals.find(terminal => terminal.is_focused)?.id ||
    terminals[0]?.id || "";
}

export function nativeAgentLabel(terminals) {
  const states = (Array.isArray(terminals) ? terminals : [])
    .map(terminal => String(terminal.agent_state || "").toLowerCase().replace(/[\s_-]/g, ""));
  if (states.includes("needsinput")) return "待输入";
  if (states.includes("error")) return "异常";
  if (states.includes("running")) return "运行中";
  if (states.includes("idle")) return "空闲";
  return "";
}

export function visibleWorkspaces(workspaces, unreadOnly) {
  const list = Array.isArray(workspaces) ? workspaces : [];
  return unreadOnly ? list.filter(workspace => workspace.unreadCount > 0) : list;
}

export function groupMemberWorkspaces(workspaces, group) {
  return workspaces.filter(workspace =>
    workspace.groupId === group.id && workspace.id !== group.anchorId);
}

export function nextUnreadWorkspace(workspaces, currentId) {
  const unread = visibleWorkspaces(workspaces, true);
  if (!unread.length) return "";
  const index = unread.findIndex(workspace => workspace.id === currentId);
  return unread[(index + 1) % unread.length].id;
}

export function requiresCmuxInputClaim(action) {
  return ["text", "key", "paste", "scroll", "mouse", "terminal_close",
    "viewport_report", "viewport_clear"].includes(action);
}

export function requiresCmuxAdminClaim(action, uploadKind = "") {
  return [
    "workspace_create", "workspace_rename", "workspace_close", "workspace_reorder",
    "group_create", "group_rename", "group_add", "group_remove",
    "terminal_create", "notification_read",
    "browser_navigate", "browser_click", "browser_type", "browser_key",
    "browser_scroll", "browser_nav", "browser_screenshot"
  ].includes(action) || action.startsWith("upload_") &&
    action !== "upload_abort" && uploadKind === "file";
}

export function leaseForSelection(claim, clientId, surfaceId) {
  if (!claim || claim.client_id !== clientId || claim.surface_id !== surfaceId) return null;
  return { owned: claim.owned === true, occupied: claim.occupied === true };
}
