// cmux 共享终端网格可能高于手机可视区；xterm 的备用屏无 scrollback，
// 但外层容器可以独立滚动整张已收到的画面。
export function shouldFollowLatest(element, alternateScreen, browsing) {
  return alternateScreen && !browsing &&
    element.scrollTop >= element.scrollHeight - element.clientHeight - 1;
}

export function showLatestScreen(element) {
  element.scrollTop = Math.max(0, element.scrollHeight - element.clientHeight);
}
