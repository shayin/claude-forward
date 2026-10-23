const rank = { blocked: 4, error: 3, working: 2, done: 1 };
const statusTTL = 5 * 60 * 1000;

export function applyProgramEvent(store, event, now = Date.now()) {
  if (event?.name !== "program_status.changed" || !event.workspace_id ||
      !event.surface_id || !Array.isArray(event.programs)) return false;
  const key = `${event.workspace_id}\x00${event.surface_id}`;
  const records = event.programs.slice(0, 16).filter(item =>
    item && Object.hasOwn(rank, item.state)).map(item => ({
    state: item.state, title: typeof item.title === "string" ? item.title.slice(0, 256) : "",
    app: typeof item.app === "string" ? item.app.slice(0, 128) : "",
    progress: Number.isInteger(item.progress) && item.progress >= 0 && item.progress <= 100
      ? item.progress : null
  }));
  store.set(key, { records, at: now });
  return true;
}

export function topProgram(store, workspaceId, surfaceId = "", now = Date.now()) {
  const prefix = `${workspaceId}\x00`;
  const records = [];
  for (const [key, entry] of store) {
    if (now - entry.at > statusTTL) {
      store.delete(key); // 事件丢失时不让“执行中”永久卡住。
    } else if (key.startsWith(prefix) && (!surfaceId || key === prefix + surfaceId)) {
      records.push(...entry.records);
    }
  }
  return records.sort((a, b) => rank[b.state] - rank[a.state])[0] || null;
}

export function programLabel(program) {
  if (!program) return "";
  const labels = { blocked: "等待处理", error: "执行失败", working: "执行中", done: "已完成" };
  return `${labels[program.state]}${program.progress !== null && program.state === "working"
    ? ` ${program.progress}%` : ""}${program.title ? ` · ${program.title}` : ""}`;
}

export function prunePrograms(store, workspaces) {
  const live = new Set();
  for (const workspace of workspaces) {
    for (const surface of workspace.terminals) live.add(`${workspace.id}\x00${surface.id}`);
  }
  for (const key of store.keys()) if (!live.has(key)) store.delete(key);
}
