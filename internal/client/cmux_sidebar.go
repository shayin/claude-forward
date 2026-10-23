package client

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/shayin/claude-forward/internal/protocol"
)

var errInvalidCmuxWorkspaces = errors.New("cmux 工作区列表格式错误")

// mobile API 是工作区、分组和 tab 的真实顺序；sidebar 补齐原生未读、
// 工作目录和服务端口。只合并明确的字段，不把整个 sidebar 快照透传到 Web。
func (m *CmuxManager) listCmuxWorkspaces(ctx context.Context) (json.RawMessage, error) {
	raw, err := m.rpc(ctx, "mobile.workspace.list", cmuxRPCParams{Password: cmuxPassword()})
	if err != nil {
		return nil, err
	}
	var mobile map[string]json.RawMessage
	if json.Unmarshal(raw, &mobile) != nil {
		return nil, errInvalidCmuxWorkspaces
	}
	var workspaces []map[string]json.RawMessage
	if json.Unmarshal(mobile["workspaces"], &workspaces) != nil || workspaces == nil {
		return nil, errInvalidCmuxWorkspaces
	}
	sidebar, err := m.rpc(ctx, "extension.sidebar.snapshot", cmuxRPCParams{Password: cmuxPassword()})
	if err != nil {
		return raw, nil // 旧版 cmux 没有 sidebar API 时仍保留原始工作区。
	}
	var snapshot struct {
		Workspaces []map[string]json.RawMessage `json:"workspaces"`
	}
	if json.Unmarshal(sidebar, &snapshot) != nil {
		return raw, nil
	}
	byID := make(map[string]map[string]json.RawMessage, len(snapshot.Workspaces))
	for _, entry := range snapshot.Workspaces {
		var id string
		if json.Unmarshal(entry["id"], &id) == nil && id != "" {
			byID[id] = entry
		}
	}
	for _, workspace := range workspaces {
		var id string
		if json.Unmarshal(workspace["id"], &id) != nil {
			continue
		}
		side := byID[id]
		for _, field := range []string{"unread_count", "listening_ports", "git_branches",
			"branch_summary", "latest_conversation_message", "latest_submitted_message",
			"last_submitted_at", "custom_color", "pull_request_urls", "current_directory"} {
			if value := side[field]; len(value) > 0 && string(value) != "null" {
				workspace[field] = value
			}
		}
	}
	mobile["workspaces"] = protocol.MustMarshal(workspaces)
	return protocol.MustMarshal(mobile), nil
}
