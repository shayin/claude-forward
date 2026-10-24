// 全屏 TUI 不暴露内容总长度；这里只显示本手机成功发送的滚动净量，绝不伪装成真实进度。
export function createRelativeScrollHint(element) {
  let active = false;
  let netLines = 0;
  const render = () => {
    element.hidden = !active;
    if (!active) return;
    const direction = netLines < 0 ? "↑ " : netLines > 0 ? "↓ " : "";
    element.textContent = `本手机相对滚动：${direction}${Math.abs(netLines)} 行 · 非总进度`;
  };
  const reset = () => {
    active = false;
    netLines = 0;
    render();
  };
  return {
    reset,
    setRemoteMode(enabled) {
      if (!enabled) {
        reset();
      } else if (!active) {
        active = true;
        render();
      }
    },
    record(lines) {
      if (!active || !Number.isSafeInteger(lines) || lines === 0) return;
      netLines += lines;
      render();
    }
  };
}
