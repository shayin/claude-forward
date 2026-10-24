// 每次手势只上报一次纯数值摘要；不采集终端内容、输入、路径或身份。
export function createScrollDiagnostic({ send, now = () => performance.now(),
  setTimer = setTimeout, clearTimer = clearTimeout }) {
  let sample = null;
  let timer = null;
  const flush = () => {
    if (timer !== null) clearTimer(timer);
    timer = null;
    if (!sample) return;
    if (sample.inFlight) {
      schedule();
      return;
    }
    const finished = sample;
    sample = null;
    if (!finished.moves && !finished.batches) return;
    send({
      mode: finished.mode, duration_ms: Math.round(now() - finished.started),
      moves: finished.moves, delta_px: Math.round(finished.delta),
      queued_lines_peak: finished.peak, batches: finished.batches,
      rpc_ms_total: Math.round(finished.rpc), rpc_ms_max: Math.round(finished.rpcMax),
      failures: finished.failures, snapshot_count: finished.snapshots,
      snapshot_ms_max: Math.round(finished.snapshotMax),
      render_ms_max: Math.round(finished.renderMax),
      write_ms_max: Math.round(finished.writeMax),
      first_write_ms: finished.firstWrite, dropped_frames: finished.dropped
    });
  };
  const schedule = () => {
    if (timer !== null) clearTimer(timer);
    timer = setTimer(flush, 1500);
  };
  const active = () => {
    if (!sample) {
      sample = { mode: "remote", started: now(), moves: 0, delta: 0,
      peak: 0, batches: 0, rpc: 0, rpcMax: 0, failures: 0, snapshots: 0,
      snapshotMax: 0, renderMax: 0, writeMax: 0, firstWrite: 0, dropped: 0, inFlight: 0 };
      schedule();
    }
    return sample;
  };
  return {
    start(mode) {
      // 旧请求尚未回执时把连续手势视为同一轮积压，保留慢请求证据。
      if (sample?.inFlight) return;
      flush();
      active().mode = mode;
    },
    move(delta) { const s = active(); s.moves++; s.delta += delta; },
    end() { if (sample) schedule(); },
    queue(_lines, pending) { const s = active(); s.peak = Math.max(s.peak, Math.abs(pending)); },
    rpcStart() { active().inFlight++; },
    rpc(ms, failed) {
      const s = active();
      s.inFlight = Math.max(0, s.inFlight - 1);
      s.batches++; s.rpc += ms; s.rpcMax = Math.max(s.rpcMax, ms);
      if (failed) s.failures++;
      schedule();
    },
    snapshot(ms) {
      if (!sample) return;
      sample.snapshots++; sample.snapshotMax = Math.max(sample.snapshotMax, ms);
    },
    frame(renderMs, writeMs = 0, dropped = false) {
      if (!sample) return;
      sample.renderMax = Math.max(sample.renderMax, renderMs);
      sample.writeMax = Math.max(sample.writeMax, writeMs);
      if (writeMs > 0 && !sample.firstWrite) sample.firstWrite = Math.round(now() - sample.started);
      if (dropped) sample.dropped++;
    }
  };
}
