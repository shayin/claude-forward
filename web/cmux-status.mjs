const labels = {
  ready: "终端模式可用",
  connecting: "正在连接终端模式",
  unavailable: "终端模式不可用"
};

export function setConnectionIndicator(indicator, state) {
  if (!Object.prototype.hasOwnProperty.call(labels, state)) {
    throw new RangeError("invalid connection state");
  }
  indicator.dataset.state = state;
  indicator.title = labels[state];
}
