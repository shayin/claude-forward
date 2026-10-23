// 全屏 TUI 只有当前网格，没有可供本地预读的历史；相邻快照只重画变化行。
// 非全屏/切屏/尺寸变化仍交给调用方完整重建，避免污染普通屏的历史缓冲。
export function sameRenderedFrame(previous, next, columns, rows) {
  return previous?.content === next.content && previous?.mouseScroll === next.mouseScroll &&
    columns === next.columns && rows === next.rows;
}

export function alternateFramePatch(previous, next) {
  if (!previous?.alternateScreen || !next?.alternateScreen ||
      previous.columns !== next.columns || previous.rows !== next.rows ||
      !Array.isArray(previous.screenLines) || !Array.isArray(next.screenLines)) return null;
  let patch = "";
  for (let row = 0; row < next.rows; row++) {
    const before = previous.screenLines[row] || "";
    const after = next.screenLines[row] || "";
    if (before !== after) patch += `\x1b[${row + 1};1H\x1b[0m\x1b[2K${after}`;
  }
  if (next.cursor) {
    patch += `\x1b[${next.cursor.row + 1};${next.cursor.column + 1}H\x1b[?25h`;
  } else if (patch || previous.cursor) {
    patch += "\x1b[?25l";
  }
  return patch;
}
