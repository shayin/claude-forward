package client

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/shayin/claude-forward/internal/protocol"
)

func TestCmuxListMergesSidebarWithoutChangingNativeOrderOrTerminals(t *testing.T) {
	socket := fakeCmuxSocket(t, func(request cmuxRPCRequest) string {
		switch request.Method {
		case "mobile.workspace.list":
			return `{"ok":true,"result":{"version":9,"groups":[{"id":"g"}],"workspaces":[` +
				`{"id":"w1","unread_count":0,"terminals":[{"id":"s1","agent_state":"Running"}]},` +
				`{"id":"w2","terminals":[{"id":"s2","agent_state":"Idle"}]}]}}`
		case "extension.sidebar.snapshot":
			return `{"ok":true,"result":{"workspaces":[` +
				`{"id":"w2","unread_count":3,"listening_ports":[3000],"current_directory":"/tmp","secret":"not-transferred"},` +
				`{"id":"w1","unread_count":2,"latest_conversation_message":"hello"}]}}`
		}
		return `{"ok":false,"error":{"code":"method_not_found"}}`
	})
	manager := NewCmuxManager(CmuxConfig{Enabled: true, SocketPath: socket})
	raw, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{RequestID: "list", Action: protocol.CmuxList})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Version int `json:"version"`
		Groups  []struct {
			ID string `json:"id"`
		} `json:"groups"`
		Workspaces []struct {
			ID        string `json:"id"`
			Unread    int    `json:"unread_count"`
			Directory string `json:"current_directory"`
			Ports     []int  `json:"listening_ports"`
			Terminals []struct {
				State string `json:"agent_state"`
			} `json:"terminals"`
			Secret string `json:"secret"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Version != 9 || len(result.Groups) != 1 || len(result.Workspaces) != 2 ||
		result.Workspaces[0].ID != "w1" || result.Workspaces[0].Unread != 2 ||
		result.Workspaces[0].Terminals[0].State != "Running" ||
		result.Workspaces[1].ID != "w2" || result.Workspaces[1].Unread != 3 ||
		result.Workspaces[1].Directory != "/tmp" || result.Workspaces[1].Ports[0] != 3000 ||
		result.Workspaces[1].Secret != "" {
		t.Fatalf("native/sidebar merge changed semantics: %+v", result)
	}
}

func TestCmuxListFallsBackWhenSidebarUnavailable(t *testing.T) {
	socket := fakeCmuxSocket(t, func(request cmuxRPCRequest) string {
		if request.Method == "mobile.workspace.list" {
			return `{"ok":true,"result":{"workspaces":[{"id":"w"}]}}`
		}
		return `{"ok":false,"error":{"code":"method_not_found"}}`
	})
	manager := NewCmuxManager(CmuxConfig{Enabled: true, SocketPath: socket})
	raw, err := manager.Execute(context.Background(), protocol.CmuxRequestPayload{RequestID: "list", Action: protocol.CmuxList})
	if err != nil || string(raw) != `{"workspaces":[{"id":"w"}]}` {
		t.Fatalf("old cmux list fallback = %s, %v", raw, err)
	}
}
