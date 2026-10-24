// 快捷菜单只填草稿，由用户明确点击发送；远端 TUI 自行处理交互式选择器。
const commands = new Set(["/model", "/status", "/compact", "/review", "/permissions", "/clear"]);

export function commandDraft(current, command) {
  if (!commands.has(command)) return null;
  if (current.trim() && !/^\/[a-z]*$/.test(current.trim())) return null;
  return command;
}
