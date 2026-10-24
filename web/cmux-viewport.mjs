// 本地视口只参与 cmux 的共享尺寸协商；真实网格尺寸始终来自 cmux 快照。
export function viewportDimensions(proposal) {
  if (!proposal || !Number.isInteger(proposal.cols) || !Number.isInteger(proposal.rows) ||
      proposal.cols < 20 || proposal.rows < 5) return null;
  return { columns: Math.min(300, proposal.cols), rows: Math.min(120, proposal.rows) };
}

export function snapshotViewportPayload(workspaceId, surfaceId, owned, visible, proposal) {
  const payload = { workspace_id: workspaceId, surface_id: surfaceId };
  if (!owned || !visible) return payload;
  const dimensions = viewportDimensions(proposal);
  return dimensions ? { ...payload, ...dimensions } : payload;
}
