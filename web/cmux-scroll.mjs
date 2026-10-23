// 普通屏历史只在浏览器中滚动；全屏 TUI 的滚轮输入才交给本机 cmux。
export function mountCmuxScroll({
  element, terminal, isSelected, canRemoteScroll, sendRemoteScroll, onRemoteError,
  onReturnToBottom,
  raf = requestAnimationFrame, cancelRaf = cancelAnimationFrame, now = performance.now.bind(performance)
}) {
  let alternateScreen = false;
  let browsing = false;
  let touching = false;
  let dragging = false;
  let startY = 0;
  let startLine = 0;
  let lastY = 0;
  let lastAt = 0;
  let velocity = 0;
  let pendingLine = 0;
  let dragFrame = 0;
  let momentumFrame = 0;
  let lastGestureAt = 0;
  let wheelIntent = false;
  let remotePixels = 0;
  let remoteLines = 0;
  let remoteBusy = false;
  let generation = 0;

  const buffer = () => terminal.buffer.active;
  const lineHeight = () => Math.max(1,
    (element.querySelector(".xterm-screen")?.getBoundingClientRect().height || 0) /
    Math.max(1, terminal.rows) || 18);
  const atBottom = () => buffer().viewportY >= buffer().baseY;
  const holdFrame = () => !alternateScreen && (browsing || touching || momentumFrame !== 0);
  const finishLocal = () => {
    touching = false;
    dragging = false;
    if (atBottom()) {
      if (browsing) {
        browsing = false;
        onReturnToBottom();
      }
    } else {
      browsing = true;
    }
  };
  const paintLine = () => {
    dragFrame = 0;
    const target = Math.max(0, Math.min(buffer().baseY, pendingLine));
    terminal.scrollToLine(target);
    if (!atBottom()) browsing = true;
  };
  const scheduleLine = line => {
    pendingLine = Math.round(line);
    if (!dragFrame) dragFrame = raf(paintLine);
  };
  const cancelMotion = () => {
    if (dragFrame) cancelRaf(dragFrame);
    if (momentumFrame) cancelRaf(momentumFrame);
    dragFrame = 0;
    momentumFrame = 0;
  };
  const queueRemote = pixels => {
    if (!alternateScreen || !canRemoteScroll() || !Number.isFinite(pixels)) return;
    if (remotePixels * pixels < 0) remotePixels = 0;
    remotePixels = Math.max(-32 * lineHeight(), Math.min(32 * lineHeight(), remotePixels + pixels));
    const lines = Math.trunc(remotePixels / lineHeight());
    if (!lines) return;
    remotePixels -= lines * lineHeight();
    if (remoteLines * lines < 0) remoteLines = 0;
    remoteLines = Math.max(-32, Math.min(32, remoteLines + lines));
    void drainRemote();
  };
  async function drainRemote() {
    if (remoteBusy) return;
    remoteBusy = true;
    const current = generation;
    try {
      while (current === generation && alternateScreen && canRemoteScroll() && remoteLines) {
        const lines = Math.max(-8, Math.min(8, remoteLines));
        remoteLines -= lines;
        await sendRemoteScroll(lines);
      }
    } catch (error) {
      if (current === generation) {
        remoteLines = 0;
        onRemoteError(error);
      }
    } finally {
      remoteBusy = false;
      if (remoteLines && alternateScreen && canRemoteScroll()) void drainRemote();
    }
  }
  const cancelRemote = () => {
    generation++;
    remotePixels = 0;
    remoteLines = 0;
  };
  const reset = () => {
    cancelRemote();
    cancelMotion();
    alternateScreen = false;
    browsing = false;
    touching = false;
    dragging = false;
    wheelIntent = false;
    lastGestureAt = 0;
  };
  const setAlternateScreen = alternate => {
    if (alternateScreen === alternate) return;
    reset();
    alternateScreen = alternate;
  };
  element.addEventListener("touchstart", event => {
    if (!isSelected() || event.touches.length !== 1) return;
    if (alternateScreen && !canRemoteScroll()) return;
    cancelMotion();
    touching = true;
    dragging = false;
    const touch = event.touches[0];
    startY = lastY = touch.clientY;
    startLine = buffer().viewportY;
    lastAt = now();
    velocity = 0;
  }, { capture: true, passive: true });
  element.addEventListener("touchmove", event => {
    if (!touching || event.touches.length !== 1) return;
    if (alternateScreen && !canRemoteScroll()) {
      touching = false;
      dragging = false;
      cancelRemote();
      return;
    }
    const touch = event.touches[0];
    const distance = touch.clientY - startY;
    if (!dragging && Math.abs(distance) < 4) return;
    dragging = true;
    if (event.cancelable) event.preventDefault();
    event.stopImmediatePropagation();
    const time = now();
    const elapsed = Math.max(1, time - lastAt);
    velocity = Math.max(-2.5, Math.min(2.5, (lastY - touch.clientY) / elapsed));
    if (alternateScreen) {
      queueRemote(lastY - touch.clientY);
    } else {
      scheduleLine(startLine - distance / lineHeight());
    }
    lastY = touch.clientY;
    lastAt = time;
    lastGestureAt = time;
  }, { capture: true, passive: false });
  element.addEventListener("touchend", event => {
    if (!touching) return;
    if (dragging && event.cancelable) event.preventDefault();
    if (dragging) event.stopImmediatePropagation();
    if (dragFrame) {
      cancelRaf(dragFrame);
      paintLine();
    }
    if (alternateScreen || !dragging || Math.abs(velocity) < .12) {
      finishLocal();
      return;
    }
    let previous = now();
    const glide = time => {
      const elapsed = Math.min(32, Math.max(1, time - previous));
      previous = time;
      velocity *= Math.exp(-elapsed / 190);
      if (Math.abs(velocity) < .12) {
        momentumFrame = 0;
        finishLocal();
        return;
      }
      const before = buffer().viewportY;
      terminal.scrollToLine(Math.max(0, Math.min(buffer().baseY,
        Math.round(before + velocity * elapsed / lineHeight()))));
      if (!atBottom()) browsing = true;
      if (buffer().viewportY === before) {
        momentumFrame = 0;
        finishLocal();
      } else {
        momentumFrame = raf(glide);
      }
    };
    momentumFrame = raf(glide);
  }, { capture: true, passive: false });
  element.addEventListener("touchcancel", () => {
    cancelMotion();
    remoteLines = 0;
    remotePixels = 0;
    finishLocal();
  }, { capture: true });
  element.addEventListener("wheel", event => {
    if (!isSelected() || !event.deltaY || event.altKey || event.ctrlKey || event.metaKey) return;
    if (alternateScreen) {
      if (!canRemoteScroll()) return;
      const pixels = event.deltaMode === 1 ? event.deltaY * lineHeight() :
        event.deltaMode === 2 ? event.deltaY * lineHeight() * terminal.rows : event.deltaY;
      queueRemote(pixels);
      event.preventDefault();
      event.stopImmediatePropagation();
      return;
    }
    wheelIntent = true;
    lastGestureAt = now();
    raf(() => {
      if (wheelIntent) {
        const wasBrowsing = browsing;
        browsing = !atBottom();
        wheelIntent = false;
        if (wasBrowsing && !browsing) onReturnToBottom();
      }
    });
  }, { capture: true, passive: false });
  terminal.onScroll(() => {
    if (alternateScreen || touching || momentumFrame) return;
    if (browsing && atBottom()) {
      browsing = false;
      onReturnToBottom();
    } else if (wheelIntent || now() - lastGestureAt < 500 && !atBottom()) {
      browsing = !atBottom();
    }
  });
  return { holdFrame, reset, cancelRemote, setAlternateScreen, isBrowsing: () => browsing };
}
