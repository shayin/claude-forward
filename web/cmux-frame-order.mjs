// 滚动响应自带快照，可能与独立轮询乱序到达；旧画面不能覆盖新画面。
export function createCmuxFrameOrder() {
  let issued = 0;
  let applied = 0;
  return {
    issue: () => ++issued,
    accept(sequence) {
      if (sequence < applied) return false;
      applied = sequence;
      return true;
    }
  };
}
