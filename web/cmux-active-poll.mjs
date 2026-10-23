// 正常闲置时仍走原有轮询；只有全屏远程滚动期间临时快速取帧。
export function createCmuxActivePoll(refresh, {
  now = Date.now, schedule = setTimeout, cancel = clearTimeout,
  interval = 120, tail = 600
} = {}) {
  let timer = 0;
  let activeUntil = 0;
  const tick = () => {
    timer = 0;
    if (now() >= activeUntil) return;
    refresh();
    timer = schedule(tick, interval);
  };
  return {
    activate() {
      activeUntil = now() + tail;
      if (!timer) timer = schedule(tick, interval);
    },
    stop() {
      activeUntil = 0;
      if (timer) cancel(timer);
      timer = 0;
    }
  };
}
