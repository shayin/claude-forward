import assert from "node:assert/strict";
import test from "node:test";
import { chooseSurface, cmuxClients, cmuxGroups, cmuxWorkspaces, nativeAgentLabel,
  leaseForSelection, requiresCmuxInputClaim, requiresCmuxAdminClaim,
  visibleWorkspaces, groupMemberWorkspaces, nextUnreadWorkspace } from "./cmux-state.mjs";

test("lists only clients with an explicit cmux capability", () => {
  assert.deepEqual(cmuxClients([
    { id: "old" }, { id: "new", features: ["cmux"] }, { id: "other", features: ["chat"] }
  ]).map(client => client.id), ["new"]);
  assert.deepEqual(cmuxClients(null), []);
});

test("retains native workspace order, including not-yet-ready terminals", () => {
  assert.deepEqual(cmuxWorkspaces({ workspaces: [
    { id: "w1", title: "甲", terminals: [{ id: "s1" }] },
    { id: "empty", terminals: [] },
    { id: "invalid", terminals: [{}] },
    { id: "w2", current_directory: "/tmp", terminals: [{ id: "s2" }, {}] }
  ] }), [
    { id: "w1", title: "甲", directory: "", groupId: "", unreadCount: 0, branch: "",
      lastMessage: "", listeningPorts: [], terminals: [{ id: "s1" }] },
    { id: "empty", title: "empty", directory: "", groupId: "", unreadCount: 0, branch: "",
      lastMessage: "", listeningPorts: [], terminals: [] },
    { id: "invalid", title: "invalid", directory: "", groupId: "", unreadCount: 0, branch: "",
      lastMessage: "", listeningPorts: [], terminals: [] },
    { id: "w2", title: "/tmp", directory: "/tmp", groupId: "", unreadCount: 0, branch: "",
      lastMessage: "", listeningPorts: [], terminals: [{ id: "s2" }] }
  ]);
  assert.deepEqual(cmuxWorkspaces({}), []);
});

test("retains only safe typed sidebar hints for unread and preview", () => {
  const [item] = cmuxWorkspaces({ workspaces: [{
    id: "w", unread_count: 3, branch_summary: { branch: "main" },
    latest_conversation_message: "<script>untrusted</script>",
    listening_ports: [3000, { port: 5173 }, 65536, "9999"]
  }] });
  assert.equal(item.unreadCount, 3);
  assert.equal(item.branch, "main");
  assert.equal(item.lastMessage, "<script>untrusted</script>");
  assert.deepEqual(item.listeningPorts, [3000, 5173]);
});

test("preserves cmux native groups and flags", () => {
  assert.deepEqual(cmuxGroups({ groups: [
    { id: "g1", title: "开发", is_pinned: true, anchor_workspace_id: "w1",
      anchor_workspace_is_generated: true },
    { group_id: "g2", name: "临时", is_collapsed: true }, {}
  ] }), [
    { id: "g1", title: "开发", pinned: true, collapsed: false, anchorId: "w1", generatedAnchor: true },
    { id: "g2", title: "临时", pinned: false, collapsed: true, anchorId: "", generatedAnchor: false }
  ]);
});

test("group anchor is accessed through group terminal, not repeated as a member", () => {
  const workspaces = [
    { id: "anchor", groupId: "g" },
    { id: "member", groupId: "g" },
    { id: "elsewhere", groupId: "other" }
  ];
  for (const generatedAnchor of [true, false]) {
    assert.deepEqual(groupMemberWorkspaces(workspaces,
      { id: "g", anchorId: "anchor", generatedAnchor }), [workspaces[1]]);
  }
  assert.deepEqual(groupMemberWorkspaces(workspaces, { id: "g", anchorId: "" }),
    workspaces.slice(0, 2));
  assert.deepEqual(groupMemberWorkspaces(workspaces, { id: "g", anchorId: "missing" }),
    workspaces.slice(0, 2));
  assert.deepEqual(groupMemberWorkspaces(visibleWorkspaces([
    { ...workspaces[0], unreadCount: 1 }, { ...workspaces[1], unreadCount: 0 }
  ], true), { id: "g", anchorId: "anchor" }), []);
});

test("keeps selected tab when possible, otherwise uses native focus", () => {
  const workspace = { terminals: [{ id: "a" }, { id: "b", is_focused: true }] };
  assert.equal(chooseSurface(workspace, "a"), "a");
  assert.equal(chooseSurface(workspace, "missing"), "b");
  assert.equal(chooseSurface({ terminals: [] }), "");
});

test("native cmux agent status wins over idle and does not invent unknown states", () => {
  assert.equal(nativeAgentLabel([{ agent_state: "idle" }, { agent_state: "needs_input" }]), "待输入");
  assert.equal(nativeAgentLabel([{ agent_state: "running" }]), "运行中");
  assert.equal(nativeAgentLabel([{ agent_state: "unknown" }]), "");
});

test("input ownership is scoped to the exact client and surface", () => {
  assert.equal(requiresCmuxInputClaim("paste"), true);
  assert.equal(requiresCmuxInputClaim("terminal_close"), true);
  assert.equal(requiresCmuxInputClaim("viewport_report"), true);
  assert.equal(requiresCmuxInputClaim("viewport_clear"), true);
  assert.equal(requiresCmuxInputClaim("snapshot"), false);
  const claim = { client_id: "computer-a", surface_id: "s1", occupied: true, owned: false };
  assert.deepEqual(leaseForSelection(claim, "computer-a", "s1"), { owned: false, occupied: true });
  assert.equal(leaseForSelection(claim, "computer-a", "s2"), null);
  assert.equal(leaseForSelection(claim, "computer-b", "s1"), null);
});

test("metadata writes and file uploads require explicit management ownership", () => {
  assert.equal(requiresCmuxAdminClaim("workspace_close"), true);
  assert.equal(requiresCmuxAdminClaim("terminal_create"), true);
  assert.equal(requiresCmuxAdminClaim("upload_chunk", "file"), true);
  assert.equal(requiresCmuxAdminClaim("upload_abort", "file"), false);
  assert.equal(requiresCmuxAdminClaim("upload_begin", "attachment"), false);
  assert.equal(requiresCmuxAdminClaim("file_list"), false);
  assert.equal(requiresCmuxAdminClaim("browser_navigate"), true);
  assert.equal(requiresCmuxAdminClaim("browser_click"), true);
  assert.equal(requiresCmuxAdminClaim("browser_screenshot"), true);
});

test("unread filter and next unread keep native workspace order", () => {
  const workspaces = [
    { id: "a", unreadCount: 0 }, { id: "b", unreadCount: 2 },
    { id: "c", unreadCount: 1 }
  ];
  assert.deepEqual(visibleWorkspaces(workspaces, true).map(item => item.id), ["b", "c"]);
  assert.equal(nextUnreadWorkspace(workspaces, "a"), "b");
  assert.equal(nextUnreadWorkspace(workspaces, "b"), "c");
  assert.equal(nextUnreadWorkspace(workspaces, "c"), "b");
  assert.equal(nextUnreadWorkspace([], "c"), "");
});
