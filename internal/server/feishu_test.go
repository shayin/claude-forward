package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFeishuChatResponseCollectText_PrefersFinalResult(t *testing.T) {
	r := &feishuChatResponse{}
	r.collectText("text", "我先读取材料。")
	r.collectText("text", "我继续处理。")
	r.collectText("result", "最终结果。")

	if got, want := r.FullText, "最终结果。"; got != want {
		t.Fatalf("FullText = %q, want %q", got, want)
	}
}

func TestFeishuChatResponseCollectText_EmptyResultKeepsFallback(t *testing.T) {
	r := &feishuChatResponse{}
	r.collectText("text", "助手最终回复")
	r.collectText("result", "")

	if got, want := r.FullText, "助手最终回复"; got != want {
		t.Fatalf("FullText = %q, want %q", got, want)
	}
}

func TestFeishuChatResponseCollectText_WhitespaceResultKeepsFallback(t *testing.T) {
	r := &feishuChatResponse{}
	r.collectText("text", "已有有效文本")
	r.collectText("result", " \n\t ")

	if got, want := r.FullText, "已有有效文本"; got != want {
		t.Fatalf("FullText = %q, want %q", got, want)
	}
}

func TestFeishuChatResponseCollectText_StreamDeltaAccumulatesThenResultWins(t *testing.T) {
	r := &feishuChatResponse{}
	r.collectText("stream_delta", "你好")
	r.collectText("stream_delta", "世界")

	if got, want := r.FullText, "你好世界"; got != want {
		t.Fatalf("FullText = %q, want %q", got, want)
	}
	// result 仍优先于累积的 stream_delta
	r.collectText("result", "最终结果")
	if got, want := r.FullText, "最终结果"; got != want {
		t.Fatalf("FullText = %q, want %q", got, want)
	}
}

// TestFeishuManager_BindingsPersistRoundTrip 验证 bindings 持久化往返不丢、不串。
func TestFeishuManager_BindingsPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := &FeishuManager{
		bindings: make(map[string]string),
		dataDir:  dir,
	}
	m.setBinding("ou_abc", "server-1")
	m.setBinding("ou_def", "server-2")

	// 用同一 dataDir 重新加载，模拟 server 重启
	m2 := &FeishuManager{
		bindings: make(map[string]string),
		dataDir:  dir,
	}
	m2.loadBindings()

	if got, want := m2.bindings["ou_abc"], "server-1"; got != want {
		t.Fatalf("ou_abc = %q, want %q", got, want)
	}
	if got, want := m2.bindings["ou_def"], "server-2"; got != want {
		t.Fatalf("ou_def = %q, want %q", got, want)
	}
}

// TestFeishuManager_PushMessage_NotInConfig 验证非白名单 open_id 被拒（不触网络）。
func TestFeishuManager_PushMessage_NotInConfig(t *testing.T) {
	m := &FeishuManager{
		openMap:  map[string]*FeishuUserRoute{},
		bindings: make(map[string]string),
	}
	if _, err := m.PushMessage("ou_unknown", "hello"); err == nil {
		t.Fatal("expected error for unknown open_id, got nil")
	}
}

// TestFeishuManager_OpenMapWhitelist 验证白名单构造：仅配置的 open_id 可命中 route。
func TestFeishuManager_OpenMapWhitelist(t *testing.T) {
	cfg := FeishuConfig{
		Enabled: true,
		Users: []FeishuUserRoute{
			{FeishuID: "ou_allowed", ClawbotID: "server"},
		},
	}
	m := NewFeishuManager(nil, nil, cfg)

	if _, ok := m.openMap["ou_allowed"]; !ok {
		t.Fatal("白名单 open_id 未命中")
	}
	if _, ok := m.openMap["ou_blocked"]; ok {
		t.Fatal("非白名单 open_id 不应命中")
	}
	if route := m.openMap["ou_allowed"]; route.ClawbotID != "server" {
		t.Fatalf("ClawbotID = %q, want %q", route.ClawbotID, "server")
	}
}

// TestLoadConfig_MergesSingleFeishuIntoApps 验证单条 feishu（向后兼容）合并进 feishu_apps 列表头部。
func TestLoadConfig_MergesSingleFeishuIntoApps(t *testing.T) {
	yamlContent := `
feishu:
  enabled: true
  app_id: "cli_single"
  app_secret: "secret-single"
  users:
    - feishu_id: "ou_a"
      clawbot_id: "server"

feishu_apps:
  - enabled: true
    app_id: "cli_txy"
    app_secret: "secret-txy"
    data_dir: "feishu-data-txy"
    users:
      - feishu_id: "ou_b"
        clawbot_id: "txy"
  - enabled: false
    app_id: "cli_disabled"
    app_secret: "secret-off"
`
	path := filepath.Join(t.TempDir(), "server.yaml")
	if err := os.WriteFile(path, []byte(yamlContent), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig error: %v", err)
	}

	if len(cfg.FeishuApps) != 3 {
		t.Fatalf("FeishuApps len = %d, want 3 (单条 + 2 条列表)", len(cfg.FeishuApps))
	}
	// 单条配置必须排首位（保持原有 app 先初始化的行为可预期）
	if cfg.FeishuApps[0].AppID != "cli_single" {
		t.Errorf("FeishuApps[0].AppID = %q, want %q", cfg.FeishuApps[0].AppID, "cli_single")
	}
	if cfg.FeishuApps[1].AppID != "cli_txy" || cfg.FeishuApps[1].DataDir != "feishu-data-txy" {
		t.Errorf("FeishuApps[1] = %+v, want cli_txy + feishu-data-txy", cfg.FeishuApps[1])
	}
	if cfg.FeishuApps[1].Users[0].ClawbotID != "txy" {
		t.Errorf("FeishuApps[1].Users[0].ClawbotID = %q, want %q", cfg.FeishuApps[1].Users[0].ClawbotID, "txy")
	}
}

// TestLoadConfig_DisabledSingleFeishuNotMerged 验证单条 feishu 未启用时不混入列表。
func TestLoadConfig_DisabledSingleFeishuNotMerged(t *testing.T) {
	yamlContent := `
feishu:
  enabled: false
  app_id: "cli_single"

feishu_apps:
  - enabled: true
    app_id: "cli_txy"
    app_secret: "secret-txy"
`
	path := filepath.Join(t.TempDir(), "server.yaml")
	if err := os.WriteFile(path, []byte(yamlContent), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig error: %v", err)
	}

	if len(cfg.FeishuApps) != 1 {
		t.Fatalf("FeishuApps len = %d, want 1", len(cfg.FeishuApps))
	}
	if cfg.FeishuApps[0].AppID != "cli_txy" {
		t.Errorf("FeishuApps[0].AppID = %q, want %q", cfg.FeishuApps[0].AppID, "cli_txy")
	}
}

// TestAddFeishuManager 验证多管理器注册与 nil 忽略。
func TestAddFeishuManager(t *testing.T) {
	h := NewHandler(NewHub(), nil, nil)
	if len(h.feishuMgrs) != 0 {
		t.Fatalf("初始 feishuMgrs len = %d, want 0", len(h.feishuMgrs))
	}

	h.AddFeishuManager(nil)
	if len(h.feishuMgrs) != 0 {
		t.Fatalf("nil 注册后 feishuMgrs len = %d, want 0", len(h.feishuMgrs))
	}

	m1 := NewFeishuManager(nil, nil, FeishuConfig{Enabled: true, AppID: "cli_1"})
	m2 := NewFeishuManager(nil, nil, FeishuConfig{Enabled: true, AppID: "cli_2"})
	h.AddFeishuManager(m1)
	h.AddFeishuManager(m2)
	if len(h.feishuMgrs) != 2 {
		t.Fatalf("feishuMgrs len = %d, want 2", len(h.feishuMgrs))
	}
}
