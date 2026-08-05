package server

import "testing"

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
