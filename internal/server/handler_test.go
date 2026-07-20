package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shayin/claude-forward/internal/protocol"
)

func newIsolatedTestWechatManager(t *testing.T) *WeChatManager {
	t.Helper()
	manager := newTestWechatManager()
	manager.dataDir = t.TempDir()
	return manager
}

// TestHandleBackgroundResult_WithWeChatMgr 收到 BackgroundResult 后推送
func TestHandleBackgroundResult_WithWeChatMgr(t *testing.T) {
	hub := NewHub()

	client := &Connection{
		ID:   "test-client-1",
		Type: ConnTypeClient,
		Send: make(chan *protocol.Message, 256),
	}

	// 创建带 WeChatManager 的 Handler
	wechatMgr := newIsolatedTestWechatManager(t)
	handler := NewHandler(hub, nil, nil)
	handler.SetWeChatManager(wechatMgr)

	// 构造 BackgroundResult 消息
	payload := protocol.BackgroundResultPayload{
		TaskID:    "task-001",
		WechatID:  "wxid_test@im.wechat",
		FullText:  "后台任务完成结果",
		CreatedAt: time.Now().UnixMilli(),
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

func TestHandleBackgroundResultAcknowledgesAcceptedAndDuplicateResult(t *testing.T) {
	wechatMgr := newIsolatedTestWechatManager(t)
	handler := NewHandler(NewHub(), nil, nil)
	handler.SetWeChatManager(wechatMgr)
	client := &Connection{ID: "ack-client", Type: ConnTypeClient, Send: make(chan *protocol.Message, 2)}
	payload := protocol.BackgroundResultPayload{TaskID: "ack-task", WechatID: "wxid_test@im.wechat", FullText: "done", CreatedAt: time.Now().UnixMilli()}
	message, _ := protocol.NewMessage(protocol.TypeBackgroundResult, payload)
	handler.handleBackgroundResult(client, message)
	handler.handleBackgroundResult(client, message)
	for i := 0; i < 2; i++ {
		ack := <-client.Send
		if ack.Type != protocol.TypeBackgroundAck {
			t.Fatalf("message type = %s, want background_ack", ack.Type)
		}
	}
}

// TestHandleBackgroundResult_IsError 错误结果推送
func TestHandleBackgroundResult_IsError(t *testing.T) {
	hub := NewHub()
	wechatMgr := newIsolatedTestWechatManager(t)
	handler := NewHandler(hub, nil, nil)
	handler.SetWeChatManager(wechatMgr)

	client := &Connection{
		ID:   "test-client-2",
		Type: ConnTypeClient,
		Send: make(chan *protocol.Message, 256),
	}

	payload := protocol.BackgroundResultPayload{
		TaskID:    "task-002",
		WechatID:  "wxid_test@im.wechat",
		IsError:   true,
		ErrorMsg:  "Claude process crashed",
		CreatedAt: time.Now().UnixMilli(),
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
	wechatMgr := newIsolatedTestWechatManager(t)
	handler := NewHandler(hub, nil, nil)
	handler.SetWeChatManager(wechatMgr)

	client := &Connection{
		ID:   "test-client-3",
		Type: ConnTypeClient,
		Send: make(chan *protocol.Message, 256),
	}

	payload := protocol.BackgroundResultPayload{
		TaskID:    "task-003",
		WechatID:  "wxid_test@im.wechat",
		FullText:  "",
		CreatedAt: time.Now().UnixMilli(),
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

func TestHandleBackgroundResult_DeduplicatesAcrossHandlerRestart(t *testing.T) {
	wechatMgr := newIsolatedTestWechatManager(t)
	client := &Connection{ID: "test-client-dedup", Type: ConnTypeClient, Send: make(chan *protocol.Message, 1)}
	payload := protocol.BackgroundResultPayload{
		TaskID:    "task-dedup",
		WechatID:  "wxid_test@im.wechat",
		FullText:  "只应推送一次",
		CreatedAt: time.Now().UnixMilli(),
	}
	msg, err := protocol.NewMessage(protocol.TypeBackgroundResult, payload)
	if err != nil {
		t.Fatalf("NewMessage error: %v", err)
	}

	firstHandler := NewHandler(NewHub(), nil, nil)
	firstHandler.SetWeChatManager(wechatMgr)
	firstHandler.handleBackgroundResult(client, msg)
	if _, err := os.Stat(filepath.Join(wechatMgr.dataDir, backgroundPushesStateFile)); err != nil {
		t.Fatalf("pushed-task state was not persisted: %v", err)
	}

	restartedHandler := NewHandler(NewHub(), nil, nil)
	restartedHandler.SetWeChatManager(wechatMgr)
	restartedHandler.handleBackgroundResult(client, msg)

	wechatMgr.mu.Lock()
	queueLen := len(wechatMgr.users["0"].PushQueue)
	wechatMgr.mu.Unlock()
	if queueLen != 1 {
		t.Fatalf("queued messages = %d, want 1 after restart", queueLen)
	}
}

func TestHandleBackgroundResult_RejectsStaleResend(t *testing.T) {
	wechatMgr := newIsolatedTestWechatManager(t)
	handler := NewHandler(NewHub(), nil, nil)
	handler.SetWeChatManager(wechatMgr)
	client := &Connection{ID: "test-client-stale", Type: ConnTypeClient, Send: make(chan *protocol.Message, 1)}
	payload := protocol.BackgroundResultPayload{
		TaskID:    "task-stale",
		WechatID:  "wxid_test@im.wechat",
		FullText:  "过期结果",
		CreatedAt: time.Now().Add(-backgroundResultTTL - time.Second).UnixMilli(),
		IsResend:  true,
	}
	msg, _ := protocol.NewMessage(protocol.TypeBackgroundResult, payload)
	handler.handleBackgroundResult(client, msg)

	wechatMgr.mu.Lock()
	queueLen := len(wechatMgr.users["0"].PushQueue)
	wechatMgr.mu.Unlock()
	if queueLen != 0 {
		t.Fatalf("queued messages = %d, want 0 for stale resend", queueLen)
	}
}

func TestHandleBackgroundResult_RejectsMissingOrFutureCreationTime(t *testing.T) {
	tests := []struct {
		name      string
		createdAt int64
	}{
		{name: "missing", createdAt: 0},
		{name: "future", createdAt: time.Now().Add(backgroundMaxClockSkew + time.Second).UnixMilli()},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wechatMgr := newIsolatedTestWechatManager(t)
			handler := NewHandler(NewHub(), nil, nil)
			handler.SetWeChatManager(wechatMgr)
			client := &Connection{ID: "test-client-time", Type: ConnTypeClient, Send: make(chan *protocol.Message, 1)}
			payload := protocol.BackgroundResultPayload{
				TaskID:    "task-" + test.name,
				WechatID:  "wxid_test@im.wechat",
				FullText:  "不应投递",
				CreatedAt: test.createdAt,
			}
			msg, _ := protocol.NewMessage(protocol.TypeBackgroundResult, payload)
			handler.handleBackgroundResult(client, msg)

			wechatMgr.mu.Lock()
			queueLen := len(wechatMgr.users["0"].PushQueue)
			wechatMgr.mu.Unlock()
			if queueLen != 0 {
				t.Fatalf("queued messages = %d, want 0", queueLen)
			}
		})
	}
}

func TestHandleBackgroundResult_ExpiresDedupRecord(t *testing.T) {
	wechatMgr := newIsolatedTestWechatManager(t)
	handler := NewHandler(NewHub(), nil, nil)
	handler.SetWeChatManager(wechatMgr)
	client := &Connection{ID: "test-client-retention", Type: ConnTypeClient, Send: make(chan *protocol.Message, 1)}
	handler.bgPushed["task-retention"] = time.Now().Add(-backgroundDedupRetention - time.Second).UnixMilli()
	payload := protocol.BackgroundResultPayload{
		TaskID:    "task-retention",
		WechatID:  "wxid_test@im.wechat",
		FullText:  "应作为新任务投递",
		CreatedAt: time.Now().UnixMilli(),
	}
	msg, _ := protocol.NewMessage(protocol.TypeBackgroundResult, payload)
	handler.handleBackgroundResult(client, msg)

	wechatMgr.mu.Lock()
	queueLen := len(wechatMgr.users["0"].PushQueue)
	wechatMgr.mu.Unlock()
	if queueLen != 1 {
		t.Fatalf("queued messages = %d, want 1 after dedup retention", queueLen)
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
		TaskID:    "task-004",
		WechatID:  "wxid_test@im.wechat",
		FullText:  "should not crash",
		CreatedAt: time.Now().UnixMilli(),
	}
	msg, _ := protocol.NewMessage(protocol.TypeBackgroundResult, payload)

	// 不应 panic
	handler.handleBackgroundResult(client, msg)
}

// TestHandleBackgroundResult_InvalidPayload 无效载荷不崩溃
func TestHandleBackgroundResult_InvalidPayload(t *testing.T) {
	hub := NewHub()
	wechatMgr := newIsolatedTestWechatManager(t)
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
	wechatMgr := newIsolatedTestWechatManager(t)
	handler := NewHandler(hub, nil, nil)
	handler.SetWeChatManager(wechatMgr)

	client := &Connection{
		ID:   "test-client-6",
		Type: ConnTypeClient,
		Send: make(chan *protocol.Message, 256),
	}

	payload := protocol.BackgroundResultPayload{
		TaskID:    "task-006",
		WechatID:  "wxid_unknown@im.wechat",
		FullText:  "unknown user",
		CreatedAt: time.Now().UnixMilli(),
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

	mgr := newIsolatedTestWechatManager(t)
	handler.SetWeChatManager(mgr)

	if handler.wechatMgr != mgr {
		t.Error("wechatMgr not set correctly")
	}
}
