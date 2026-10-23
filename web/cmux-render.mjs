// 将 cmux 的终端网格转成 xterm 可显示的 ANSI 帧；不执行终端文本中的控制符。
const graphemeSegmenter = typeof Intl.Segmenter === "function"
  ? new Intl.Segmenter(undefined, { granularity: "grapheme" }) : null;
const colorSequence = (value, background) => {
  if (typeof value !== "string" || !/^#[0-9a-f]{6}$/i.test(value)) return "";
  const channels = [1, 3, 5].map(index => Number.parseInt(value.slice(index, index + 2), 16));
  return `\x1b[${background ? 48 : 38};2;${channels.join(";")}m`;
};

function styleSequence(style = {}) {
  let sequence = "\x1b[0m";
  if (style.bold) sequence += "\x1b[1m";
  if (style.italic) sequence += "\x1b[3m";
  if (style.underline) sequence += "\x1b[4m";
  if (style.inverse) sequence += "\x1b[7m";
  sequence += colorSequence(style.foreground, false);
  sequence += colorSequence(style.background, true);
  return sequence;
}

function addSpans(rows, spans, offset = 0) {
  for (const span of (Array.isArray(spans) ? spans : [])) {
    const row = Number(span.row) + offset;
    if (!Number.isInteger(row) || row < -10000 || row > 10000) continue;
    if (!rows.has(row)) rows.set(row, []);
    rows.get(row).push(span);
  }
}

function clipTextToCells(value, available) {
  const segments = graphemeSegmenter ? graphemeSegmenter.segment(value) : value;
  let used = 0;
  let clipped = "";
  for (const item of segments) {
    const segment = typeof item === "string" ? item : item.segment;
    const cp = segment.codePointAt(0);
    const width = (cp >= 0x1100 && cp <= 0x115f) ||
      (cp >= 0x2e80 && cp <= 0xa4cf) ||
      (cp >= 0xac00 && cp <= 0xd7a3) ||
      (cp >= 0xf900 && cp <= 0xfaff) || cp >= 0x1f000 ? 2 : 1;
    if (used + width > available || clipped.length + segment.length > available * 8) break;
    clipped += segment;
    used += width;
  }
  return clipped;
}

export function renderGrid(grid) {
  if (!grid || !Array.isArray(grid.row_spans)) {
    throw new Error("终端快照缺少网格数据");
  }
  const columns = Number(grid.columns) || 80;
  const height = Number(grid.rows) || 24;
  if (columns < 1 || columns > 1000 || height < 1 || height > 1000) {
    throw new Error("终端尺寸无效");
  }
  const styleMap = new Map((Array.isArray(grid.styles) ? grid.styles : []).map(style => [style.id, style]));
  const rows = new Map();
  const alternateScreen = grid.active_screen === "alternate";
  const activeModes = new Set((Array.isArray(grid.modes) ? grid.modes : [])
    .filter(mode => mode?.ansi === false && mode.on === true).map(mode => mode.code));
  const mouseScroll = alternateScreen && activeModes.has(1006) &&
    [1000, 1002, 1003].some(code => activeModes.has(code));
  // alternate screen 没有可回滚历史；将主屏 scrollback 写进去会把 TUI 的顶行挤出视口。
  const scrollback = !alternateScreen && Array.isArray(grid.scrollback_spans)
    ? grid.scrollback_spans : [];
  const maxScrollback = scrollback.reduce((max, span) => Math.max(max, Number(span.row) || 0), -1);
  addSpans(rows, scrollback, -(maxScrollback + 1));
  addSpans(rows, grid.row_spans);
  let minimumRow = 0;
  for (const row of rows.keys()) minimumRow = Math.min(minimumRow, row);
  minimumRow = Math.max(-2000, minimumRow);
  const lines = [];
  for (let row = minimumRow; row < height; row += 1) {
    let line = "";
    let column = 0;
    const spans = (rows.get(row) || []).sort((a, b) => a.column - b.column);
    for (const span of spans) {
      const start = Number(span.column);
      if (!Number.isInteger(start) || start < column || start >= columns) continue;
      line += "\x1b[0m" + " ".repeat(start - column);
      const rawText = String(span.text || "").replace(/[\x00-\x1f\x7f-\x9f]/g, "");
      const available = columns - start;
      const cellWidth = Number(span.cell_width);
      const text = clipTextToCells(rawText, available);
      line += styleSequence(styleMap.get(span.style_id)) + text;
      column = Math.min(columns, start + (Number.isInteger(cellWidth) && cellWidth >= 0
        ? cellWidth : Array.from(text).length));
    }
    lines.push(line + "\x1b[0m");
  }
  while (lines.length > 1 && lines.at(-1) === "\x1b[0m") lines.pop();
  let content = (alternateScreen ? "\x1b[?1049h" : "") +
    "\x1b[H\x1b[2J" + lines.join("\r\n");
  const cursor = grid.cursor;
  if (cursor && Number.isInteger(cursor.row) && Number.isInteger(cursor.column) &&
      cursor.row >= 0 && cursor.row < height && cursor.column >= 0 && cursor.column < columns) {
    const finalRow = minimumRow + lines.length - 1;
    const delta = finalRow - cursor.row;
    if (delta > 0) content += `\x1b[${Math.min(delta, height - 1)}A`;
    if (delta < 0) content += `\x1b[${Math.min(-delta, height - 1)}B`;
    content += `\x1b[${cursor.column + 1}G\x1b[?25h`;
  } else {
    content += "\x1b[?25l";
  }
  return { content, columns, rows: height, alternateScreen, mouseScroll,
    screenLines: alternateScreen ? lines : null,
    cursor: cursor && Number.isInteger(cursor.row) && Number.isInteger(cursor.column) &&
      cursor.row >= 0 && cursor.row < height && cursor.column >= 0 && cursor.column < columns
      ? { row: cursor.row, column: cursor.column } : null };
}
