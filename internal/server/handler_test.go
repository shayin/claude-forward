package server

import (
	"encoding/json"
	"testing"

	"github.com/shayin/claude-forward/internal/protocol"
)

// TestHandleBackgroundResult_WithWeChatMgr 收到 BackgroundResult 后推送
func TestHandleBackgroundResult_WithWeChatMgr(t *testing.T) {
	hub := NewHub()

	client := &Connection{
		ID:   "test-client-1",
		Type: ConnTypeClient,
		Send: make(chan *protocol.Message, 256),
	}

	// 创建带 WeChatManager 的 Handler
	wechatMgr := newTestWechatManager()
	handler := NewHandler(hub, nil, nil)
	handler.SetWeChatManager(wechatMgr)

	// 构造 BackgroundResult 消息
	payload := protocol.BackgroundResultPayload{
		TaskID:   "task-001",
		WechatID: "wxid_test@im.wechat",
		FullText: "后台任务完成结果",
	}
	msg, err := protocol.NewMessage(protocol.TypeBackgroundResult, payload)
	if err != nil {
		t.Fatalf("NewMessage error: %v", err)
	}

	handler.handleBackgroundResult(client, msg)

	// 验证消息入了 PushQueue（因为 wechatMgr 的 bot 未登录，会走离线入队）
	wechatMgr.mu.Lock()
	user := wechatMgr.users["0"]
	queueLen := len(user.PushQueue)
	wechatMgr.mu.Unlock()

	if queueLen != 1 {
		t.Fatalf("expected 1 queued message, got %d", queueLen)
	}
	if user.PushQueue[0].Text != "后台任务完成结果" {
		t.Errorf("queued text = %q, want %q", user.PushQueue[0].Text, "后台任务完成结果")
	}
}

// TestHandleBackgroundResult_IsError 错误结果推送
func TestHandleBackgroundResult_IsError(t *testing.T) {
	hub := NewHub()
	wechatMgr := newTestWechatManager()
	handler := NewHandler(hub, nil, nil)
	handler.SetWeChatManager(wechatMgr)

	client := &Connection{
		ID:   "test-client-2",
		Type: ConnTypeClient,
		Send: make(chan *protocol.Message, 256),
	}

	payload := protocol.BackgroundResultPayload{
		TaskID:   "task-002",
		WechatID: "wxid_test@im.wechat",
		IsError:  true,
		ErrorMsg: "Claude process crashed",
	}
	msg, _ := protocol.NewMessage(protocol.TypeBackgroundResult, payload)

	handler.handleBackgroundResult(client, msg)

	wechatMgr.mu.Lock()
	text := wechatMgr.users["0"].PushQueue[0].Text
	wechatMgr.mu.Unlock()

	wantPrefix := "❌ 后台任务失败:"
	if len(text) < len(wantPrefix) || text[:len(wantPrefix)] != wantPrefix {
		t.Errorf("error text = %q, want prefix %q", text, wantPrefix)
	}
}

// TestHandleBackgroundResult_EmptyText 空文本兜底
func TestHandleBackgroundResult_EmptyText(t *testing.T) {
	hub := NewHub()
	wechatMgr := newTestWechatManager()
	handler := NewHandler(hub, nil, nil)
	handler.SetWeChatManager(wechatMgr)

	client := &Connection{
		ID:   "test-client-3",
		Type: ConnTypeClient,
		Send: make(chan *protocol.Message, 256),
	}

	payload := protocol.BackgroundResultPayload{
		TaskID:   "task-003",
		WechatID: "wxid_test@im.wechat",
		FullText: "",
	}
	msg, _ := protocol.NewMessage(protocol.TypeBackgroundResult, payload)

	handler.handleBackgroundResult(client, msg)

	wechatMgr.mu.Lock()
	text := wechatMgr.users["0"].PushQueue[0].Text
	wechatMgr.mu.Unlock()

	want := "（后台任务完成，无文本输出）"
	if text != want {
		t.Errorf("empty text fallback = %q, want %q", text, want)
	}
}

// TestHandleBackgroundResult_NoWeChatMgr 无 WeChatManager 时不崩溃
func TestHandleBackgroundResult_NoWeChatMgr(t *testing.T) {
	hub := NewHub()
	handler := NewHandler(hub, nil, nil)
	// 不设置 wechatMgr

	client := &Connection{
		ID:   "test-client-4",
		Type: ConnTypeClient,
		Send: make(chan *protocol.Message, 256),
	}

	payload := protocol.BackgroundResultPayload{
		TaskID:   "task-004",
		WechatID: "wxid_test@im.wechat",
		FullText: "should not crash",
	}
	msg, _ := protocol.NewMessage(protocol.TypeBackgroundResult, payload)

	// 不应 panic
	handler.handleBackgroundResult(client, msg)
}

// TestHandleBackgroundResult_InvalidPayload 无效载荷不崩溃
func TestHandleBackgroundResult_InvalidPayload(t *testing.T) {
	hub := NewHub()
	wechatMgr := newTestWechatManager()
	handler := NewHandler(hub, nil, nil)
	handler.SetWeChatManager(wechatMgr)

	client := &Connection{
		ID:   "test-client-5",
		Type: ConnTypeClient,
		Send: make(chan *protocol.Message, 256),
	}

	// 构造无效 payload
	msg := &protocol.Message{
		Type:    protocol.TypeBackgroundResult,
		Payload: json.RawMessage(`{invalid json`),
	}

	// 不应 panic
	handler.handleBackgroundResult(client, msg)

	wechatMgr.mu.Lock()
	queueLen := len(wechatMgr.users["0"].PushQueue)
	wechatMgr.mu.Unlock()

	if queueLen != 0 {
		t.Errorf("expected 0 queued messages for invalid payload, got %d", queueLen)
	}
}

// TestHandleBackgroundResult_NotInConfig wechatID 不在白名单不入队
func TestHandleBackgroundResult_NotInConfig(t *testing.T) {
	hub := NewHub()
	wechatMgr := newTestWechatManager()
	handler := NewHandler(hub, nil, nil)
	handler.SetWeChatManager(wechatMgr)

	client := &Connection{
		ID:   "test-client-6",
		Type: ConnTypeClient,
		Send: make(chan *protocol.Message, 256),
	}

	payload := protocol.BackgroundResultPayload{
		TaskID:   "task-006",
		WechatID: "wxid_unknown@im.wechat",
		FullText: "unknown user",
	}
	msg, _ := protocol.NewMessage(protocol.TypeBackgroundResult, payload)

	// PushMessage 对不在白名单的 wechatID 会返回错误，但 handleBackgroundResult 不应 panic
	handler.handleBackgroundResult(client, msg)
}

// TestSetWeChatManager 设置 WeChatManager
func TestSetWeChatManager(t *testing.T) {
	hub := NewHub()
	handler := NewHandler(hub, nil, nil)

	if handler.wechatMgr != nil {
		t.Error("expected nil wechatMgr initially")
	}

	mgr := newTestWechatManager()
	handler.SetWeChatManager(mgr)

	if handler.wechatMgr != mgr {
		t.Error("wechatMgr not set correctly")
	}
}
