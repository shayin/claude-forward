import assert from "node:assert/strict";
import test from "node:test";
import { createScrollDiagnostic } from "./cmux-scroll-diagnostic.mjs";

function fixture() {
  let time = 100;
  let callback;
  const reports = [];
  const diagnostic = createScrollDiagnostic({
    send: report => reports.push(report),
    now: () => time,
    setTimer: fn => { callback = fn; return 1; },
    clearTimer: () => { callback = null; }
  });
  return { diagnostic, reports, tick(ms) { time += ms; },
    flush() { const fn = callback; callback = null; fn?.(); } };
}

test("远程手势只输出一次排队、RPC、快照和写入的数值摘要", () => {
  const f = fixture();
  f.diagnostic.start("remote");
  f.diagnostic.move(-60);
  f.diagnostic.queue(-3, -3);
  f.diagnostic.queue(-4, -7);
  f.diagnostic.rpcStart();
  f.tick(180);
  f.diagnostic.rpc(160, false);
  f.diagnostic.snapshot(90);
  f.diagnostic.frame(12, 24, true);
  f.diagnostic.unchanged();
  f.diagnostic.end();
  f.tick(1500);
  f.flush();
  assert.deepEqual(f.reports, [{
    mode: "remote", duration_ms: 1680, moves: 1, delta_px: -60,
    queued_lines_peak: 7, batches: 1, rpc_ms_total: 160, rpc_ms_max: 160,
    failures: 0, snapshot_count: 1, snapshot_ms_max: 90,
    render_ms_max: 12, write_ms_max: 24, first_write_ms: 180,
    dropped_frames: 1, unchanged_frames: 1
  }]);
  f.flush();
  assert.equal(f.reports.length, 1);
});

test("本地滑动、无移动触碰、失败以及新手势的收尾", () => {
  const f = fixture();
  f.diagnostic.start("local");
  f.diagnostic.end();
  f.flush();
  assert.equal(f.reports.length, 0);
  f.diagnostic.start("remote");
  f.diagnostic.move(12);
  f.diagnostic.rpc(25, true);
  f.diagnostic.start("local");
  assert.equal(f.reports[0].failures, 1);
  f.diagnostic.move(40);
  f.diagnostic.end();
  f.flush();
  assert.equal(f.reports[1].mode, "local");
  assert.equal(f.reports[1].delta_px, 40);
});

test("RPC 未返回时不提前上报，周期快照不会无限延长手势", () => {
  const f = fixture();
  f.diagnostic.start("remote");
  f.diagnostic.move(-20);
  f.diagnostic.rpcStart();
  f.diagnostic.end();
  f.tick(1500);
  f.flush();
  assert.equal(f.reports.length, 0);
  f.tick(100);
  f.diagnostic.rpc(1600, false);
  f.diagnostic.snapshot(35);
  f.tick(1200);
  f.diagnostic.snapshot(20);
  f.tick(300);
  f.flush();
  assert.equal(f.reports.length, 1);
  assert.equal(f.reports[0].snapshot_count, 2);
});

test("前一滑动仍在等待 ACK 时连续滑动保留积压指标", () => {
  const f = fixture();
  f.diagnostic.start("remote");
  f.diagnostic.move(-30);
  f.diagnostic.rpcStart();
  f.diagnostic.start("remote");
  f.diagnostic.move(-40);
  f.diagnostic.queue(-2, -2);
  f.diagnostic.rpc(900, false);
  f.diagnostic.end();
  f.flush();
  assert.equal(f.reports.length, 1);
  assert.equal(f.reports[0].moves, 2);
  assert.equal(f.reports[0].rpc_ms_total, 900);
});
