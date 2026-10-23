import assert from "node:assert/strict";
import test from "node:test";
import { applyProgramEvent, programLabel, prunePrograms, topProgram } from "./cmux-programs.mjs";

test("uses bounded progress metadata and prioritizes blocked over working", () => {
  const store = new Map();
  assert.equal(applyProgramEvent(store, { name: "terminal.changed", programs: [] }), false);
  assert.equal(applyProgramEvent(store, {
    name: "program_status.changed", workspace_id: "w", surface_id: "s1",
    programs: [{ state: "working", title: "构建", progress: 20 }, { state: "wrong" }]
  }), true);
  applyProgramEvent(store, {
    name: "program_status.changed", workspace_id: "w", surface_id: "s2",
    programs: [{ state: "blocked", title: "用户确认", progress: 150 }]
  });
  assert.equal(programLabel(topProgram(store, "w")), "等待处理 · 用户确认");
  assert.equal(programLabel(topProgram(store, "w", "s1")), "执行中 20% · 构建");
  prunePrograms(store, [{ id: "w", terminals: [{ id: "s2" }] }]);
  assert.equal(topProgram(store, "w", "s1"), null);
  assert.equal(topProgram(store, "w", "s2").progress, null);
  store.clear();
  assert.equal(topProgram(store, "w"), null);
});

test("drops stale progress when the finishing event was lost", () => {
  const store = new Map();
  applyProgramEvent(store, {
    name: "program_status.changed", workspace_id: "w", surface_id: "s",
    programs: [{ state: "working", title: "building" }]
  }, 1000);
  assert.ok(topProgram(store, "w", "s", 1000 + 299999));
  assert.equal(topProgram(store, "w", "s", 1000 + 300001), null);
});
