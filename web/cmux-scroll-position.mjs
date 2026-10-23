// 阅读位置仅保存在当前页面内，按 Client / 工作区 / 终端隔离。
export function createScrollPositions() {
  const positions = new Map();
  const key = (client, workspace, surface) => JSON.stringify([client, workspace, surface]);
  return {
    save(client, workspace, surface, alternateScreen, element, terminal) {
      if (!client || !workspace || !surface) return;
      const buffer = terminal.buffer.active;
      positions.set(key(client, workspace, surface), alternateScreen
        ? { alternateScreen, top: element.scrollTop }
        : { alternateScreen, bottomOffset: buffer.baseY - buffer.viewportY });
    },
    restore(client, workspace, surface, alternateScreen, element, terminal) {
      const position = positions.get(key(client, workspace, surface));
      if (!position || position.alternateScreen !== alternateScreen) return false;
      if (alternateScreen) element.scrollTop = position.top;
      else terminal.scrollToLine(Math.max(0, terminal.buffer.active.baseY - position.bottomOffset));
      return true;
    }
  };
}
