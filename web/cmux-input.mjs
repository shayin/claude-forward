// 中文输入由浏览器 input/composition 事件管理；这里只转换真实终端键流。
export function terminalInput(data) {
  const keys = {
    "\r": "return", "\x1b": "escape", "\x03": "ctrl+c", "\x7f": "backspace",
    "\t": "tab", "\x1b[A": "up", "\x1b[B": "down", "\x1b[C": "right", "\x1b[D": "left"
  };
  if (keys[data]) return { action: "key", key: keys[data] };
  const control = data.length === 1 ? data.charCodeAt(0) : -1;
  if (control >= 1 && control <= 26) {
    return { action: "key", key: `ctrl+${String.fromCharCode(control + 96)}` };
  }
  if (!data || data.startsWith("\x1b") || (control >= 0 && control < 32)) return null;
  return { action: "text", text: data };
}

export function shouldSubmitComposer(event) {
  return event.key === "Enter" && !event.shiftKey && !event.isComposing &&
    event.keyCode !== 229 && !event.ctrlKey && !event.altKey && !event.metaKey;
}

// 粘贴和回车分开发送：旧版 cmux 在 Claude TUI 中偶尔过早投递 submit_key，
// 导致“发送”仅把文本留在输入框。这里恰好发送一次 Enter，不能盲目补发第二次。
export async function submitTerminalDraft(paste, sendReturn, stillCurrent, settle) {
  await paste();
  await settle();
  if (!stillCurrent()) throw new Error("已切换终端或失去输入权，粘贴内容未自动提交");
  await sendReturn();
}

export function screenCoordinates(x, y, bounds, columns, rows) {
  if (!bounds || !Number.isInteger(columns) || !Number.isInteger(rows) ||
      bounds.width <= 0 || bounds.height <= 0 || columns <= 0 || rows <= 0 ||
      x < bounds.left || x >= bounds.right || y < bounds.top || y >= bounds.bottom) {
    return null;
  }
  return {
    column: Math.min(columns - 1, Math.floor((x - bounds.left) / bounds.width * columns)),
    row: Math.min(rows - 1, Math.floor((y - bounds.top) / bounds.height * rows))
  };
}

export function wheelLines(deltaY, deltaMode = 0, rowHeight = 18) {
  const linePixels = deltaMode === 1 ? 1 : deltaMode === 2 ? 24 : Math.max(rowHeight, 1);
  const lines = Math.ceil(Math.abs(deltaY) / linePixels) * Math.sign(deltaY);
  return Math.max(-100, Math.min(100, lines));
}
