package client

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/shayin/claude-forward/internal/protocol"
)

// 本测试只能从用户正常启动的 cmux 终端执行，不更改 cmux socket 安全设置。
// 默认跳过，CI 仍由假 socket 单测覆盖，人工验收须显式设置 CF_CMUX_LIVE_READONLY=1。
func TestCmuxLiveReadOnly(t *testing.T) {
	if os.Getenv("CF_CMUX_LIVE_READONLY") != "1" {
		t.Skip("requires legitimate cmux terminal context")
	}
	manager := NewCmuxManager(CmuxConfig{Enabled: true})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raw, err := manager.Execute(ctx, protocol.CmuxRequestPayload{
		RequestID: "live-list", Action: protocol.CmuxList,
	})
	if err != nil {
		t.Fatal(err)
	}
	var listing struct {
		Workspaces []struct {
			ID         string `json:"id"`
			IsSelected bool   `json:"is_selected"`
			Terminals  []struct {
				ID      string `json:"id"`
				IsReady bool   `json:"is_ready"`
			} `json:"terminals"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil || len(listing.Workspaces) == 0 {
		t.Fatalf("cmux workspaces unavailable: count=%d err=%v", len(listing.Workspaces), err)
	}
	surfaceID := ""
	for _, workspace := range listing.Workspaces {
		if !workspace.IsSelected {
			continue
		}
		for _, terminal := range workspace.Terminals {
			if terminal.IsReady {
				surfaceID = terminal.ID
				break
			}
		}
	}
	if surfaceID == "" {
		t.Skip("selected workspace has no ready terminal")
	}
	raw, err = manager.Execute(ctx, protocol.CmuxRequestPayload{
		RequestID: "live-snapshot", Action: protocol.CmuxSnapshot, SurfaceID: surfaceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		RenderGrid struct {
			Columns int `json:"columns"`
			Rows    int `json:"rows"`
		} `json:"render_grid"`
	}
	if err := json.Unmarshal(raw, &snapshot); err != nil ||
		snapshot.RenderGrid.Columns < 20 || snapshot.RenderGrid.Rows < 5 {
		t.Fatalf("invalid native render grid: cols=%d rows=%d err=%v",
			snapshot.RenderGrid.Columns, snapshot.RenderGrid.Rows, err)
	}
	_, err = manager.Execute(ctx, protocol.CmuxRequestPayload{
		RequestID: "live-notifications", Action: protocol.CmuxNotifications,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("native cmux read-only RPC verified: %d workspaces, terminal grid %dx%d",
		len(listing.Workspaces), snapshot.RenderGrid.Columns, snapshot.RenderGrid.Rows)
}
